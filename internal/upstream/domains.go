// domains.go agent host 的动态发现（TNC get_domains/v5）。
//
// 背景（2026-10-15 抓包实测）：官方客户端**不写死** agent 网关。它请求一个逻辑主机名
// （国际版 referer 是 coresg-normal.trae.ai，国服是 trae-api-cn.mchost.guru），由 TTNet
// 的 dispatcher 按 TNC 下发的域名表在传输层改写成具体后端再发出去。所以同一个客户端，
// referer 里写着 trae-api-cn，实际 TCP/TLS 打的是 api5-normal.mchost.guru。
//
// 本仓是纯 Go HTTP，不走 TTNet，只能自己把具体后端当基址 —— 这就是 2026-10-15 那次
// 「国服换域名」的根因，也是同类改动以后还会再发生的原因。这里把那张表拉回来自己解析，
// 让基址跟着上游走，而不是靠人肉盯梢。
//
// 实测证据（两区各一组，2026-10-15；注意 cronet_version/ttnet_version 不能空，
// 空值返回 0 条规则）：
//
//	国服 GET https://tnc0-aliec2.zijieapi.com/get_domains/v5/?aid=787976&...
//	     → host_group 含 api3-normal.mchost.guru / api5-normal.mchost.guru
//	国际 GET https://tnc-boot.trae.ai/get_domains/v5/?aid=677332&...
//	     → host_group 含 api16/api22-normal-alisg.mchost.guru、
//	       api16/api22-normal-useast1a.mchost.guru，**也含 api5-normal.mchost.guru**
//	       （所以一张表可覆盖两区，但两区的 boot 主机不同，见 tncBootHosts）
//
// 判活与回退：先按地区过滤候选（国服 = 无区域段，国际 = 带区域段），再逐个探活，取第一个
// 有 HTTP 响应的；401 也算活（它证明路由存在，只是没带 token）。全部失败就保留原值、
// 最终回落到 constants.go 的常量 —— 发现机制自己坏掉不能影响面板出流。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"traework2api/internal/auth"
)

// EpDomains TNC 域名下发路径（GET，无需鉴权）。
const EpDomains = "/get_domains/v5/"

// tncBootHosts 两区的 TNC boot 主机，按顺序试。实测来源：国服客户端打
// tnc0-aliec2/tnc3-bjlgy.zijieapi.com，国际客户端打 tnc-boot.trae.ai /
// tnc16-normal-sg.trae.ai。
var tncBootHosts = map[string][]string{
	auth.RealmCN: {
		"https://tnc0-aliec2.zijieapi.com",
		"https://tnc3-bjlgy.zijieapi.com",
		"https://tnc-boot.trae.ai", // 兜底：国际 boot 的表里也带国服主机
	},
	auth.RealmIntl: {
		"https://tnc-boot.trae.ai",
		"https://tnc16-normal-sg.trae.ai",
	},
}

// domainQuery 下发请求的固定参数。aid 是**两区不同的应用号**（国服 787976 = 客户端
// x-lscbd-aid；国际 677332 = x-ss-dp），它决定返回哪一区的表。
// cronet/ttnet 版本是客户端构建信息，实测清空会让响应退化成 0 条规则，所以照抄实测值。
var domainQuery = map[string]string{
	auth.RealmCN:   "aid=787976&device_platform=windows&version_code=3402",
	auth.RealmIntl: "aid=677332&device_platform=windows&version_code=362",
}

const (
	domainAppName     = "Trae"
	domainCronetVer   = "a61b3479_2026-08-04"
	domainTTNetVer    = "4.2.243.20-trae"
	domainProbeBudget = 12 * time.Second // 单次发现的总预算（两区并行共用，含候选探活）
	domainHTTPTimeout = 8 * time.Second
)

// agentHostRE 具体 agent 后端的命名：api<N>-normal[-<region>].mchost.guru
// 国服无区域段（api5-normal.mchost.guru），国际带区域段（api22-normal-alisg.mchost.guru）。
var agentHostRE = regexp.MustCompile(`^api(\d+)-normal(-[a-z0-9]+)?\.mchost\.guru$`)

// dispatchDoc 只解我们需要的字段，别的一律忽略（表很大，32 个顶层键）。
type dispatchDoc struct {
	Data struct {
		Actions []struct {
			Param struct {
				HostGroup []string `json:"host_group"`
			} `json:"param"`
		} `json:"ttnet_dispatch_actions"`
	} `json:"data"`
}

// agentHostsFrom 从下发文档里挑出某地区可用的 agent 后端。
// 国服 = 无区域段；国际 = 带区域段。排序按分片序号**从大到小**（实测客户端用 api5/api22，
// 即偏新的分片），同号按字母序稳定，保证同一份输入每次选出同一个首选。
func agentHostsFrom(raw []byte, realm string) ([]string, error) {
	var doc dispatchDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("get_domains parse: %w", err)
	}
	seen := map[string]bool{}
	var out []string
	for _, act := range doc.Data.Actions {
		for _, h := range act.Param.HostGroup {
			h = strings.TrimSpace(h)
			if seen[h] {
				continue
			}
			m := agentHostRE.FindStringSubmatch(h)
			if m == nil {
				continue
			}
			hasRegion := m[2] != ""
			if (realm == auth.RealmIntl) != hasRegion {
				continue
			}
			seen[h] = true
			out = append(out, h)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		ni, _ := strconv.Atoi(agentHostRE.FindStringSubmatch(out[i])[1])
		nj, _ := strconv.Atoi(agentHostRE.FindStringSubmatch(out[j])[1])
		if ni != nj {
			return ni > nj
		}
		return out[i] < out[j]
	})
	return out, nil
}

// fetchDispatch 依次试该地区的 boot 主机，取第一份能解析出**候选非空**的响应。
func (c *Client) fetchDispatch(ctx context.Context, realm string) ([]string, string, error) {
	var lastErr error
	for _, base := range tncBootHosts[realm] {
		u := base + EpDomains + "?" + domainQuery[realm] +
			"&app_name=" + url.QueryEscape(domainAppName) +
			"&cronet_version=" + url.QueryEscape(domainCronetVer) +
			"&ttnet_version=" + url.QueryEscape(domainTTNetVer) +
			"&tnc_src=1&delay=0"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("User-Agent", "TraeClient/TTNet")
		resp, err := c.discoveryHTTP().Do(req)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", base, err)
			continue
		}
		raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<22))
		resp.Body.Close()
		if rerr != nil {
			lastErr = fmt.Errorf("%s: %w", base, rerr)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("%s: http %d", base, resp.StatusCode)
			continue
		}
		hosts, perr := agentHostsFrom(raw, realm)
		if perr != nil {
			lastErr = fmt.Errorf("%s: %w", base, perr)
			continue
		}
		if len(hosts) == 0 {
			lastErr = fmt.Errorf("%s: 表里没有 %s 区的 agent 候选", base, realm)
			continue
		}
		return hosts, base, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("没有可用的 TNC boot 主机")
	}
	return nil, "", lastErr
}

// probeAgentBase 判候选是否可用：POST 模型表端点发最小报文。
// 判据是**拿到 HTTP 响应**即算活 —— 401/400 都说明路由存在、只是没带凭据；
// 只有传输层错误与 404 判不可用（404 = 这台没有那个路由）。
func (c *Client) probeAgentBase(ctx context.Context, base string) (bool, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+EpModels,
		strings.NewReader(`{"function":"solo_work_lite","need_prompt":false}`))
	if err != nil {
		return false, err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "TraeClient/TTNet")
	resp, err := c.discoveryHTTP().Do(req)
	if err != nil {
		return false, err.Error()
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false, "http 404"
	case resp.StatusCode >= 500:
		// 网关/机房坏了不等于「这台在路由」：502/503 的候选必须如实判死，否则它会顶掉
		// 后面真正健康的候选（DiscoverHosts 在首个「活」候选上就 break）。
		return false, fmt.Sprintf("http %d", resp.StatusCode)
	case resp.StatusCode < 300 && !json.Valid(bytes.TrimSpace(body)):
		// 200 却不给 JSON = 停车场/边缘 HTML 页，不是 Trae 的分片。
		return false, fmt.Sprintf("http %d 非 JSON 响应", resp.StatusCode)
	}
	return true, fmt.Sprintf("http %d", resp.StatusCode)
}

// DiscoverHosts 拉两区的域名表、挑候选、探活，更新生效的 agent 基址。
// 任何一步失败都保留现有值（Discover 不返回错误地清空），调用方只需记日志。
func (c *Client) DiscoverHosts(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, domainProbeBudget)
	defer cancel()

	type realmResult struct {
		realm, src string
		hosts      []string
		picked     string
		err        error
	}
	realms := []string{auth.RealmCN, auth.RealmIntl}
	results := make([]realmResult, len(realms))
	var wg sync.WaitGroup
	// 两区并行，共用一份预算。串行时前一个 realm 最慢能吃满整个额度（单 realm 最坏
	// 3 个 boot × 8s + 候选 × 8s 就超过一份），后一个 realm 就拿着已死的 ctx 发请求 →
	// 那一区永远发现不出来，而两区的发现过程互不依赖。
	for i, realm := range realms {
		wg.Add(1)
		go func(i int, realm string) {
			defer wg.Done()
			r := realmResult{realm: realm}
			r.hosts, r.src, r.err = c.fetchDispatch(ctx, realm)
			if r.err != nil {
				results[i] = r
				return
			}
			for _, h := range r.hosts {
				ok, why := c.probeAgentBase(ctx, "https://"+h)
				if !ok {
					continue
				}
				r.picked = "https://" + h
				log.Printf("[domains] %s 候选 %s 探活通过（%s）", realm, h, why)
				break
			}
			results[i] = r
		}(i, realm)
	}
	wg.Wait()

	// 写回与日志放在并行段之后：两区各自的结果一个人处理，日志顺序稳定。
	var firstErr error
	changed := false
	for _, r := range results {
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			log.Printf("[domains] %s 域名表获取失败，沿用现有基址 %s: %v", r.realm, c.effectiveAgentBase(r.realm), r.err)
			continue
		}
		if r.picked == "" {
			log.Printf("[domains] %s 的 %d 个候选全部不可用，沿用现有基址 %s（来源 %s）",
				r.realm, len(r.hosts), c.effectiveAgentBase(r.realm), r.src)
			continue
		}
		if old := c.setDiscovered(r.realm, r.picked); old != r.picked {
			changed = true
			log.Printf("[domains] %s agent 基址 %s → %s（来自 %s，候选 %v）", r.realm, old, r.picked, r.src, r.hosts)
		}
	}
	if changed {
		log.Printf("[domains] 生效基址：国服=%s 国际=%s", c.effectiveAgentBase(auth.RealmCN), c.effectiveAgentBase(auth.RealmIntl))
	}
	return firstErr
}

// StartHostDiscovery 后台周期刷新。enabled 决定是否真的跑；interval<=0 用内置下限。
// 首轮立刻跑（并发，不阻塞启动），之后按 interval 循环，直到 ctx 结束。
func (c *Client) StartHostDiscovery(ctx context.Context, enabled bool, interval time.Duration) {
	if !enabled {
		log.Printf("[domains] 自动跟进已关闭（upstream.host_discovery=false），agent 基址用常量/显式配置")
		return
	}
	if interval < time.Minute {
		interval = time.Minute
	}
	go func() {
		if err := c.DiscoverHosts(ctx); err != nil {
			log.Printf("[domains] 首轮发现未完全成功（已回退常量）：%v", err)
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := c.DiscoverHosts(ctx); err != nil {
					log.Printf("[domains] 周期刷新未完全成功（沿用现有基址）：%v", err)
				}
			}
		}
	}()
}

// discoveryHTTP 发现用的客户端：短超时，不跟着运行时的长超时走（发现是辅助路径，
// 不该拖住启动，也不该被热改的 120s 超时放大）。
func (c *Client) discoveryHTTP() *http.Client {
	if c.discoveryCli != nil {
		return c.discoveryCli
	}
	return &http.Client{Timeout: domainHTTPTimeout}
}

// AgentHosts 面板/运维展示用：两区**生效**的 agent 基址 + 来源 + 最近一次发现时刻。
// 返回值保证非空（最次是内置常量），调用方不必再兜底。
func (c *Client) AgentHosts() (cn, intl, source string, at time.Time) {
	c.hostMu.RLock()
	dynCN, dynIntl, at := c.dynAgentCN, c.dynAgentIntl, c.dynAgentAt
	c.hostMu.RUnlock()
	cn = c.effectiveAgentBase(auth.RealmCN)
	intl = c.effectiveAgentBase(auth.RealmIntl)
	switch {
	case dynCN != "" && dynIntl != "":
		source = "自动发现"
	case dynCN != "" || dynIntl != "":
		source = "部分自动发现"
	default:
		source = "内置常量/显式配置"
	}
	return cn, intl, source, at
}

func (c *Client) setDiscovered(realm, base string) string {
	c.hostMu.Lock()
	defer c.hostMu.Unlock()
	old := ""
	switch realm {
	case auth.RealmIntl:
		old = c.dynAgentIntl
		c.dynAgentIntl = base
	default:
		old = c.dynAgentCN
		c.dynAgentCN = base
	}
	c.dynAgentAt = time.Now()
	if old == "" {
		old = "(未发现)"
	}
	return old
}

// dynAgentBase 取某区已发现的具体基址；空 = 还没发现出来。
func (c *Client) dynAgentBase(realm string) string {
	c.hostMu.RLock()
	defer c.hostMu.RUnlock()
	if realm == auth.RealmIntl {
		return c.dynAgentIntl
	}
	return c.dynAgentCN
}

// effectiveAgentBase 当前**实际会用**的基址（已发现 > 显式配置 > 内置常量）。
func (c *Client) effectiveAgentBase(realm string) string {
	if h := c.dynAgentBase(realm); h != "" {
		return h
	}
	if realm == auth.RealmIntl {
		if h := c.agentBaseIntl(); h != "" {
			return h
		}
		return AgentHostIntl
	}
	return c.agentBase()
}

// agentBaseIntl 显式配置的国际版基址（同 agentBase：写侧可能热改，读要走锁）。
func (c *Client) agentBaseIntl() string {
	c.hostMu.RLock()
	defer c.hostMu.RUnlock()
	return c.AgentHostIntl
}
