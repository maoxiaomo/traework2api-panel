package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"traework2api/internal/auth"
)

// 固件是 2026-10-15 从真实 TNC 接口抓下的原始响应（未改动）：
//
//	testdata/get_domains_cn.json    https://tnc0-aliec2.zijieapi.com/get_domains/v5/?aid=787976&...
//	testdata/get_domains_intl.json  https://tnc-boot.trae.ai/get_domains/v5/?aid=677332&...
//
// 用真响应而不是手写 JSON：候选列表来自上游真实命名，手写的话考不出「排除非 agent 主机」
// 这类回归（国服表里就有 trae-api-cn.mchost.guru，它**不是**分片后端）。
func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("读固件 %s: %v", name, err)
	}
	return raw
}

func TestAgentHostsFromRealFixtures(t *testing.T) {
	cn := loadFixture(t, "get_domains_cn.json")
	intl := loadFixture(t, "get_domains_intl.json")

	gotCN, err := agentHostsFrom(cn, auth.RealmCN)
	if err != nil {
		t.Fatalf("国服表解析失败: %v", err)
	}
	// 分片号从大到小：api5 在 api3 前。trae-api-cn.mchost.guru 必须被排除（不是分片后端）。
	wantCN := "api5-normal.mchost.guru,api3-normal.mchost.guru"
	if strings.Join(gotCN, ",") != wantCN {
		t.Errorf("国服候选=%v want %v", gotCN, wantCN)
	}
	// 国服表里没有带区域段的主机。
	if gotIntl, _ := agentHostsFrom(cn, auth.RealmIntl); len(gotIntl) != 0 {
		t.Errorf("国服表不该给出国际候选，得到 %v", gotIntl)
	}

	gotIntl, err := agentHostsFrom(intl, auth.RealmIntl)
	if err != nil {
		t.Fatalf("国际表解析失败: %v", err)
	}
	// 同号按字母序，api22 全在 api16 之前。
	wantIntl := "api22-normal-alisg.mchost.guru,api22-normal-useast1a.mchost.guru," +
		"api16-normal-alisg.mchost.guru,api16-normal-useast1a.mchost.guru"
	if strings.Join(gotIntl, ",") != wantIntl {
		t.Errorf("国际候选=%v want %v", gotIntl, wantIntl)
	}
	// 国际表里含国服主机（api5-normal），且它**必须**只出现在国服候选里。
	gotCNFromIntl, _ := agentHostsFrom(intl, auth.RealmCN)
	if strings.Join(gotCNFromIntl, ",") != "api5-normal.mchost.guru" {
		t.Errorf("国际表里的国服候选=%v want [api5-normal.mchost.guru]", gotCNFromIntl)
	}
}

func TestAgentHostsFromEmptyOrGarbage(t *testing.T) {
	if h, err := agentHostsFrom([]byte(`{"data":{"ttnet_dispatch_actions":[]}}`), auth.RealmCN); err != nil || len(h) != 0 {
		t.Errorf("空表应返回空候选且不报错，得到 %v %v", h, err)
	}
	if _, err := agentHostsFrom([]byte("not json"), auth.RealmCN); err == nil {
		t.Error("非 JSON 应报错")
	}
	// 只有非 agent 主机时不能瞎凑候选。
	raw := []byte(`{"data":{"ttnet_dispatch_actions":[{"param":{"host_group":["trae-api-cn.mchost.guru","core-normal.trae.ai"]}}]}}`)
	if h, err := agentHostsFrom(raw, auth.RealmCN); err != nil || len(h) != 0 {
		t.Errorf("非 agent 主机应被排除，得到 %v %v", h, err)
	}
}

// fakeRT 按 host 给固定响应：boot 主机返回固件，候选探活按 wantAlive 决定 401/404。
type fakeRT struct {
	fixture   []byte
	bootHosts []string
	alive     map[string]bool

	mu    sync.Mutex // 两区现在是并行发现的，calls 会被两个 goroutine 同时追加
	calls []string
}

func (f *fakeRT) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Host
	f.mu.Lock()
	f.calls = append(f.calls, req.Method+" "+req.URL.Scheme+"://"+host+req.URL.Path)
	f.mu.Unlock()
	mk := func(code int, body string) *http.Response {
		return &http.Response{
			StatusCode: code,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}
	}
	for _, b := range f.bootHosts {
		if strings.TrimPrefix(b, "https://") == host {
			return mk(http.StatusOK, string(f.fixture)), nil
		}
	}
	if strings.HasSuffix(req.URL.Path, EpModels) {
		if f.alive[host] {
			return mk(http.StatusUnauthorized, `{"code":1001}`), nil
		}
		return mk(http.StatusNotFound, "404 page not found"), nil
	}
	return mk(http.StatusNotFound, "404 page not found"), nil
}

func withFakeDiscovery(t *testing.T, f *fakeRT) *Client {
	t.Helper()
	c := New()
	c.discoveryCli = &http.Client{Transport: f}
	t.Cleanup(func() { c.discoveryCli = nil })
	return c
}

func TestDiscoverHostsPicksFirstAliveCandidate(t *testing.T) {
	f := &fakeRT{
		fixture:   loadFixture(t, "get_domains_intl.json"),
		bootHosts: tncBootHosts[auth.RealmIntl],
		// 首选 api22-normal-alisg 故意判死，考验「跳过死候选取下一个」。
		alive: map[string]bool{
			"api22-normal-useast1a.mchost.guru": true,
			"api5-normal.mchost.guru":           true,
		},
	}
	c := withFakeDiscovery(t, f)
	if err := c.DiscoverHosts(context.Background()); err != nil {
		t.Fatalf("DiscoverHosts: %v", err)
	}
	if got := c.agentBaseFor(nil); got != "https://api5-normal.mchost.guru" {
		t.Errorf("国服基址=%q want api5-normal（api22-normal-alisg 已判死）", got)
	}
	intlAuth := &auth.Auth{Domain: "trae.ai", ApiHost: "https://api-sg-central.trae.ai"}
	if got := c.agentBaseFor(intlAuth); got != "https://api22-normal-useast1a.mchost.guru" {
		t.Errorf("国际基址=%q want api22-normal-useast1a（首选 alisg 已判死）", got)
	}
	cn, intl, source, at := c.AgentHosts()
	if source != "自动发现" {
		t.Errorf("source=%q want 自动发现", source)
	}
	if cn == "" || intl == "" || at.IsZero() {
		t.Errorf("快照不完整: cn=%q intl=%q at=%v", cn, intl, at)
	}
}

func TestDiscoverHostsKeepsFallbackWhenAllCandidatesDead(t *testing.T) {
	f := &fakeRT{
		fixture:   loadFixture(t, "get_domains_intl.json"),
		bootHosts: tncBootHosts[auth.RealmIntl],
		alive:     map[string]bool{}, // 全死
	}
	c := withFakeDiscovery(t, f)
	if err := c.DiscoverHosts(context.Background()); err != nil {
		t.Fatalf("DiscoverHosts 不该返回错误（候选全死只记日志）: %v", err)
	}
	// 全死 → 一个都没写，回落常量。
	if got := c.agentBaseFor(nil); got != AgentHost {
		t.Errorf("国服应回落常量，得到 %q", got)
	}
	if got := c.agentBaseFor(&auth.Auth{Domain: "trae.ai"}); got != AgentHostIntl {
		t.Errorf("国际应回落常量，得到 %q", got)
	}
	_, _, source, at := c.AgentHosts()
	if source != "内置常量/显式配置" || !at.IsZero() {
		t.Errorf("source=%q at=%v want 内置常量/显式配置 + 零值时间", source, at)
	}
}

func TestFetchDispatchTriesNextBootHost(t *testing.T) {
	// 第一个 boot 主机 500，第二个给固件 —— 验证按顺序回落。
	bad := &fakeRT{bootHosts: nil, alive: nil} // 不认识任何 host → 一律 404
	good := &fakeRT{
		fixture:   loadFixture(t, "get_domains_cn.json"),
		bootHosts: []string{"https://boot2.test"},
		alive:     nil,
	}
	rt := rtSwitch{first: bad, second: good}
	c := New()
	c.discoveryCli = &http.Client{Transport: rt}
	t.Cleanup(func() { c.discoveryCli = nil })

	old := tncBootHosts
	tncBootHosts = map[string][]string{auth.RealmCN: {"https://boot1.test", "https://boot2.test"}}
	t.Cleanup(func() { tncBootHosts = old })

	hosts, src, err := c.fetchDispatch(context.Background(), auth.RealmCN)
	if err != nil {
		t.Fatalf("fetchDispatch: %v", err)
	}
	if src != "https://boot2.test" {
		t.Errorf("用了 %q，应回落到第二个 boot 主机", src)
	}
	if len(hosts) == 0 || hosts[0] != "api5-normal.mchost.guru" {
		t.Errorf("候选=%v want 首个 api5-normal.mchost.guru", hosts)
	}
}

// rtSwitch 第一个 boot 主机一律 404，其余交给 second。
type rtSwitch struct{ first, second *fakeRT }

func (s rtSwitch) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == "boot1.test" {
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader("boom")),
			Request:    req,
		}, nil
	}
	return s.second.RoundTrip(req)
}

func TestProbeAgentBaseJudgements(t *testing.T) {
	c := withFakeDiscovery(t, &fakeRT{alive: map[string]bool{"alive.test": true}})
	if ok, why := c.probeAgentBase(context.Background(), "https://alive.test"); !ok {
		t.Errorf("401 应判活，得到 %v %s", ok, why)
	}
	if ok, why := c.probeAgentBase(context.Background(), "https://dead.test"); ok {
		t.Errorf("404 应判不可用，得到 %v %s", ok, why)
	}
	// 传输层错误（这里给一个必然连不上的地址）也要判不可用而不是 panic。
	c2 := New()
	if ok, _ := c2.probeAgentBase(context.Background(), "https://127.0.0.1:1"); ok {
		t.Error("连不上应判不可用")
	}
}

func TestEffectiveAgentBasePrecedence(t *testing.T) {
	c := New()
	// 1) 默认：常量。
	if got := c.effectiveAgentBase(auth.RealmCN); got != AgentHost {
		t.Errorf("默认为 %q want %q", got, AgentHost)
	}
	if got := c.effectiveAgentBase(auth.RealmIntl); got != AgentHostIntl {
		t.Errorf("默认为 %q want %q", got, AgentHostIntl)
	}
	// 2) 显式配置覆盖常量。
	c.AgentHost = "https://explicit.test"
	c.AgentHostIntl = "https://explicit-intl.test"
	if got := c.effectiveAgentBase(auth.RealmCN); got != "https://explicit.test" {
		t.Errorf("显式配置未生效：%q", got)
	}
	// 3) 自动发现覆盖显式配置。
	c.setDiscovered(auth.RealmCN, "https://discovered.test")
	c.setDiscovered(auth.RealmIntl, "https://discovered-intl.test")
	if got := c.effectiveAgentBase(auth.RealmCN); got != "https://discovered.test" {
		t.Errorf("自动发现应优先：%q", got)
	}
	// 4) 只发现了一边：另一边仍是显式配置（“部分自动发现”）。
	cn, intl, source, _ := c.AgentHosts()
	if cn != "https://discovered.test" || intl != "https://discovered-intl.test" {
		t.Errorf("快照 cn=%q intl=%q", cn, intl)
	}
	if source != "自动发现" {
		t.Errorf("source=%q want 自动发现", source)
	}
	c2 := New()
	c2.AgentHost = "https://only-cn.test"
	c2.setDiscovered(auth.RealmCN, "https://discovered.test")
	if _, _, s, _ := c2.AgentHosts(); s != "部分自动发现" {
		t.Errorf("source=%q want 部分自动发现", s)
	}
}

func TestStartHostDiscoveryDisabledKeepsConstants(t *testing.T) {
	c := New()
	c.StartHostDiscovery(context.Background(), false, 0)
	time.Sleep(50 * time.Millisecond)
	if got := c.agentBaseFor(nil); got != AgentHost {
		t.Errorf("关掉自动跟进后应仍是常量，得到 %q", got)
	}
}

var errFake = errors.New("fake")
