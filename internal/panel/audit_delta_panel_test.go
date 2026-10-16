//go:build audit

// audit_delta_panel_test.go 只读审计（增量 6188638..436b0df）的证明测试：
// 面板侧的新增输出与「额度口径」在接口间的一致性。全部构造数据，不碰真实网络。
//
// 跑法：go test -tags audit -count=1 -run TestAuditDelta -v ./internal/panel/
package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"traework2api/internal/auth"
	"traework2api/internal/pool"
	"traework2api/internal/upstream"
)

// auditDeltaPanel 一个只带池子 + 假上游的面板（AuthDir 空、无密钥 → 只放行本机来源）。
func auditDeltaPanel(p *pool.Pool, c *upstream.Client) *Panel {
	return New(Config{Pool: p, Upstream: c, Version: "audit"})
}

func auditDeltaLocal(pn *Panel, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	req.RemoteAddr = "127.0.0.1:9" // 本机来源，空密钥形态下才放行
	rec := httptest.NewRecorder()
	pn.mux.ServeHTTP(rec, req)
	return rec
}

// auditDeltaUpstream 假上游：任何请求都回同一份 ent-usage 报文（口径由 body 决定）。
func auditDeltaUpstream(body string) *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		AgentHost: "https://agent.example", UgHost: "https://ug.example",
		OAuthHost: "https://oauth.example", ClientID: upstream.ClientID,
	}
}

func auditDeltaAccounts(t *testing.T, pn *Panel, path string) []map[string]any {
	t.Helper()
	rec := auditDeltaLocal(pn, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s code=%d body=%s", path, rec.Code, rec.Body)
	}
	var d struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("%s 解析失败: %v", path, err)
	}
	return d.Accounts
}

// TestAuditDeltaAgentHostOverviewFieldsAreDead：overview 新增的四个 agent_host* 键
// **没有任何前端读者** —— commit caa7c62 的变更说明写着「agent_host*（面板可见）」，
// panel.go:225-235 的注释也把它当成「上游换域名时的第一现场」，但 app.js / index.html
// 里搜不到 agent_host：运维在面板上看不到当前生效的基址与来源，只能翻进程日志的 [domains] 行。
// 这条属于「写了没读」的死输出（同 audit_panel_test.go 的 TestAuditDeadFrontendSwitchAndAttribute 一类）。
func TestAuditDeltaAgentHostOverviewFieldsAreDead(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "号1"})
	pn := auditDeltaPanel(p, auditDeltaUpstream(`{"user_entitlement_pack_list":[{"entitlement_base_info":{"quota":{"credits_limit":1}}}]}`))

	rec := auditDeltaLocal(pn, "/panel/api/overview")
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("overview 解析失败: %v", err)
	}
	var present []string
	for k := range body {
		if strings.HasPrefix(k, "agent_host") {
			present = append(present, k)
		}
	}
	t.Logf("overview 新增键: %v", present)
	if len(present) == 0 {
		t.Fatalf("overview 不再返回 agent_host* —— 本条缺陷已被修掉（或字段被删），请更新报告")
	}

	for _, f := range []string{"app.js", "index.html"} {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读 %s: %v", f, err)
		}
		if !strings.Contains(string(raw), "agent_host") {
			t.Errorf("%s 仍无 agent_host* 读者：运维在面板上看不到生效基址与来源", f)
		} else {
			t.Logf("%s: 有 agent_host* 读者（%d 字节）", f, len(raw))
		}
	}
	// 渲染那一行要真的用上四个键里的基址（只有 CN 基址时也不该整行空着）。
	app, _ := os.ReadFile("app.js")
	if !strings.Contains(string(app), "$('agHostNote')") {
		t.Error("app.js 没有把 agent_host* 写进面板（缺 agHostNote 渲染）")
	}
}

// TestAuditDeltaNewUpstreamConfigItemsAbsentFromPanelForm：增量新增的 5 个 upstream 配置项
// 在面板上**既看不到也改不了** —— 配置表单的字段映射 CFG_MAP（app.js:1134-1161）与
// cfgForm（index.html）里都没有它们，而 commit caa7c62 的变更说明写着「agent_host*（面板可见）」
// （config.go:70-72 的注释同调）。
//
// 注意这不是「值会被覆盖」：collectConfig() 返回的是整份 cfgLoaded（不在 CFG_MAP 里的键
// 原样带回，ParseBody 也是 clone 后覆盖式 Unmarshal），所以保存不会丢这些键。
// 缺的是**可见性与可操作性**：要关掉域名自动跟进、钉死 agent_host、或换 max_function，
// 面板上没有入口，只能手改 config.json（host_discovery* 改完还要重启，见 RestartFields）。
func TestAuditDeltaNewUpstreamConfigItemsAbsentFromPanelForm(t *testing.T) {
	keys := []string{"host_discovery", "host_discovery_hours", "agent_host", "agent_host_intl", "max_function"}
	app, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if !strings.Contains(string(app), k) {
			t.Errorf("app.js 里没有 %q：配置页看不到也改不了", k)
		}
		if !strings.Contains(string(html), k) {
			t.Errorf("index.html 里没有 %q：表单没有入口", k)
		}
	}
	// host_discovery 是布尔项：必须走 BOOL_FIELDS，否则字符串 "true" 会让后端 Unmarshal 失败。
	if !strings.Contains(string(app), "BOOL_FIELDS = [") || !strings.Contains(string(app), "'host_discovery'") {
		t.Error("host_discovery 不在 BOOL_FIELDS 里：会以字符串提交，后端 Unmarshal 失败")
	}
	t.Logf("面板里有 %v 的读者（app.js %d 字节 / index.html %d 字节）→ 可见且可改",
		keys, len(app), len(html))
}

// TestAuditDeltaPanelCaliberAgreesAcrossEndpoints：反证（期望为真）——
// 「新额度口径在面板与后端分歧」在本仓当前代码里不成立：
// overview（pool.Status 快照）与 packages（实时查上游）对同一账号报同一个口径键
// credit_unit，且美元数/积分数逐字段一致；美元号两个接口都**不**出现「0 积分」，
// 国内号逐字保持积分口径（零回归）。
func TestAuditDeltaPanelCaliberAgreesAcrossEndpoints(t *testing.T) {
	const intlBody = `{"is_dollar_usage_billing":true,"user_entitlement_pack_list":[
		{"display_desc":"Free plan","entitlement_base_info":{"quota":{"basic_usage_limit":1,"credits_limit":0}},
		 "usage":{"basic_usage_amount":0.00059}}]}`
	const cnBody = `{"user_entitlement_pack_list":[
		{"display_desc":"免费","entitlement_base_info":{"quota":{"credits_limit":5900}},
		 "usage":{"credits_amount":434}}]}`

	t.Run("usd", func(t *testing.T) {
		p := pool.New("")
		p.Add(&auth.Auth{UID: "intl1", Nickname: "国际号", Domain: "trae.ai"})
		p.SetBalance("intl1", pool.Balance{Unit: "usd", UsedUSD: 0.00059, LimitUSD: 1}, 1793491199)
		pn := auditDeltaPanel(p, auditDeltaUpstream(intlBody))

		ov := auditDeltaAccounts(t, pn, "/panel/api/overview")[0]
		pk := auditDeltaAccounts(t, pn, "/panel/api/packages")[0]
		t.Logf("overview: credit_unit=%v credits=%v used_usd=%v limit_usd=%v", ov["credit_unit"], ov["credits"], ov["used_usd"], ov["limit_usd"])
		t.Logf("packages: credit_unit=%v remain=%v used_usd=%v limit_usd=%v", pk["credit_unit"], pk["remain"], pk["used_usd"], pk["limit_usd"])

		if ov["credit_unit"] != "usd" || pk["credit_unit"] != "usd" {
			t.Fatalf("口径键不一致/不是 usd: overview=%v packages=%v", ov["credit_unit"], pk["credit_unit"])
		}
		if ov["used_usd"] != pk["used_usd"] || ov["limit_usd"] != pk["limit_usd"] {
			t.Fatalf("同一账号两个接口的美元数不一致: overview(%v/%v) packages(%v/%v)",
				ov["used_usd"], ov["limit_usd"], pk["used_usd"], pk["limit_usd"])
		}
		if ov["credits"] != 0.0 {
			t.Fatalf("美元号不该在积分位上报数: %v", ov["credits"])
		}
	})

	t.Run("credit", func(t *testing.T) {
		p := pool.New("")
		p.Add(&auth.Auth{UID: "cn1", Nickname: "国内号"})
		p.SetBalance("cn1", pool.Balance{Unit: "credit", Remain: 5466, Total: 5900}, 1792968453)
		pn := auditDeltaPanel(p, auditDeltaUpstream(cnBody))

		ov := auditDeltaAccounts(t, pn, "/panel/api/overview")[0]
		pk := auditDeltaAccounts(t, pn, "/panel/api/packages")[0]
		t.Logf("overview: credit_unit=%v credits=%v/%v", ov["credit_unit"], ov["credits"], ov["credits_total"])
		t.Logf("packages: credit_unit=%v remain=%v size=%v", pk["credit_unit"], pk["remain"], pk["size"])

		if ov["credit_unit"] != "credit" || pk["credit_unit"] != "credit" {
			t.Fatalf("国内号口径键不对: overview=%v packages=%v", ov["credit_unit"], pk["credit_unit"])
		}
		if ov["credits"] != pk["remain"] || ov["credits_total"] != pk["size"] {
			t.Fatalf("国内号积分两边不一致（零回归被打破）: overview(%v/%v) packages(%v/%v)",
				ov["credits"], ov["credits_total"], pk["remain"], pk["size"])
		}
	})
}
