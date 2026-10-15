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

// TestPackagesUnitKeyMatchesOverview 账号级口径键名在两个接口上必须一致。
//
// 这是一个**真实发生过**的静默失效：`/panel/api/overview` 发 `credit_unit`，
// 而 `/panel/api/packages` 发 `unit`；前端 `isUSD()` 只认 `credit_unit`，
// 于是在「积分构成」视图里美元号被判成积分号，整张卡显示 0 ——
// 不报错、不写日志，单元测试也全绿（它们喂的账号对象带的是 credit_unit）。
// 只有在真浏览器里看渲染结果才能发现。
//
// 所以这条测试锁的是「两个接口对同一概念用同一个键名」，而不是某个具体值。
func TestPackagesUnitKeyMatchesOverview(t *testing.T) {
	// 上游打桩：给一个美元计费的账号
	c := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{"is_dollar_usage_billing":true,
				  "user_entitlement_pack_list":[{"display_desc":"Free plan",
				    "entitlement_base_info":{"quota":{"basic_usage_limit":1,"credits_limit":0}},
				    "usage":{"basic_usage_amount":0.00059}}]}`)),
			}, nil
		})},
		AgentHost: "https://agent.example", UgHost: "https://ug.example",
		OAuthHost: "https://oauth.example", ClientID: upstream.ClientID,
	}
	p := pool.New("")
	p.Add(&auth.Auth{UID: "intl1", Nickname: "国际号", Domain: "trae.ai"})
	p.SetBalance("intl1", pool.Balance{Unit: "usd", UsedUSD: 0.00059, LimitUSD: 1}, 1793491199)

	pn := New(Config{Pool: p, Upstream: c, Version: "test"})

	// overview 里的键。空 api_key 只放行本机来源，httptest 默认 RemoteAddr 不是本机 → 401，
	// 所以显式设成本机（与 panel_test.go 里那几条鉴权测试同一做法）。
	localReq := func(path string) *http.Request {
		req := httptest.NewRequest("GET", path, nil)
		req.RemoteAddr = "127.0.0.1:9"
		return req
	}

	rec := httptest.NewRecorder()
	pn.mux.ServeHTTP(rec, localReq("/panel/api/overview"))
	var ov struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ov); err != nil {
		t.Fatalf("overview 解析失败: %v body=%s", err, rec.Body)
	}
	if len(ov.Accounts) != 1 {
		t.Fatalf("overview 账号数 = %d", len(ov.Accounts))
	}
	ovUnit, ok := ov.Accounts[0]["credit_unit"]
	if !ok {
		t.Fatalf("overview 没有 credit_unit 键: %v", ov.Accounts[0])
	}

	// packages 里的键
	rec2 := httptest.NewRecorder()
	pn.mux.ServeHTTP(rec2, localReq("/panel/api/packages"))
	var pks struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &pks); err != nil {
		t.Fatalf("packages 解析失败: %v body=%s", err, rec2.Body)
	}
	if len(pks.Accounts) != 1 {
		t.Fatalf("packages 账号数 = %d", len(pks.Accounts))
	}
	pkUnit, ok := pks.Accounts[0]["credit_unit"]
	if !ok {
		t.Fatalf("packages 没有 credit_unit 键（键名漂移回来了？）: %v", pks.Accounts[0])
	}

	if ovUnit != pkUnit {
		t.Errorf("同一账号在两个接口里的口径不一致: overview=%v packages=%v", ovUnit, pkUnit)
	}
	if pkUnit != "usd" {
		t.Errorf("美元号应报 usd，实得 %v", pkUnit)
	}
	if pks.Accounts[0]["used_usd"] != 0.00059 || pks.Accounts[0]["limit_usd"] != 1.0 {
		t.Errorf("美元金额没透出: %v", pks.Accounts[0])
	}
}

// TestJSIsUSDAcceptsBothKeys 前端 isUSD 对两个键名都要认（接口已统一，但留容错）。
func TestJSIsUSDAcceptsBothKeys(t *testing.T) {
	raw, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := strings.ReplaceAll(string(raw), string(rune(13))+string(rune(10)), string(rune(10)))
	body := extractJSFunc(t, src, "isUSD")
	for _, want := range []string{"credit_unit", "unit"} {
		if !strings.Contains(body, want) {
			t.Errorf("isUSD 没认 %q —— 键名一旦漂移，美元号会静默退回积分口径显示 0", want)
		}
	}
}
