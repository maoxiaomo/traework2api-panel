//go:build audit

// audit_panel_test.go 只读审计的「证明测试」：断言**当前（有问题）**的行为。
// 全部用 httptest / 构造数据 / t.TempDir()，不碰真实网络（无 127.0.0.1:7864、无真实 auths、
// 无真实 config.json），也不改任何已存在文件。
//
// 跑法：go test -tags audit -count=1 -run TestAudit -v ./internal/panel/
// 说明：这些用例在缺陷被修好后会失败 —— 那是设计意图（断言的是「缺陷存在」）。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"traework2api/internal/auth"
	"traework2api/internal/pool"
)

// auditPanelReq 发一个面板请求，默认把来源设成本机（浏览器就在面板这台机器上）。
func auditPanelReq(p *Panel, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.RemoteAddr = "127.0.0.1:53124"
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, r)
	return rec
}

// ---------------------------------------------------------------------------
// 1. 本机免鉴权形态下的跨站请求：已修（回归网）
// ---------------------------------------------------------------------------

// TestAuditCrossSiteRequestExecutesOnKeylessPanel：面板密钥留空时只信本机；**写操作**再加一道
// 同源（panel.go authorized → sameOrigin）。浏览器发起的跨站请求来源 IP 也是 127.0.0.1，
// 但它一定带自己的 Origin/Referer —— 光看 IP 等于对「本机上的任何页面」全放行。
//
// 这条原本复现的是最重的后果：用 `Content-Type: text/plain` 的**简单请求**（浏览器不预检）
// 跨站 POST「移除账号」，凭证文件被真删掉。现在期望被拒且文件还在；别把断言改回去。
func TestAuditCrossSiteRequestExecutesOnKeylessPanel(t *testing.T) {
	dir := t.TempDir()
	victimFile := filepath.Join(dir, "trae-victim1.json")
	if err := os.WriteFile(victimFile, []byte(`{"auth":{"accessToken":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "victim1", FilePath: victimFile})
	p := New(Config{Pool: pl, AuthDir: dir}) // APIKey 空 = 「本机免鉴权」形态

	rec := auditPanelReq(p, "POST", "/panel/api/accounts/victim1/remove", "{}", map[string]string{
		"Origin":         "https://evil.example",
		"Sec-Fetch-Site": "cross-site",
		"Sec-Fetch-Mode": "no-cors",
		"Content-Type":   "text/plain;charset=UTF-8",
	})
	_, statErr := os.Stat(victimFile)
	t.Logf("跨站 POST /panel/api/accounts/victim1/remove: code=%d body=%s", rec.Code, strings.TrimSpace(rec.Body.String()))
	t.Logf("凭证文件 %s 还在吗: %v", filepath.Base(victimFile), statErr == nil)

	if rec.Code == http.StatusOK {
		t.Fatalf("跨站写请求被放行了（CSRF 回归）: code=%d", rec.Code)
	}
	if statErr != nil {
		t.Fatal("凭证文件被跨站请求删掉了（CSRF 回归）")
	}
	// 同源的写请求（面板自己的页面）必须照旧放行，否则面板就用不了了。
	// httptest.NewRequest 的 Host 是 example.com，同源就是 Origin 与它一致。
	sameRec := auditPanelReq(p, "POST", "/panel/api/accounts/victim1/remove", "{}", map[string]string{
		"Origin": "http://example.com",
	})
	if sameRec.Code != http.StatusOK {
		t.Errorf("同源写请求被误拒: code=%d body=%s", sameRec.Code, sameRec.Body)
	}

	t.Run("配了密钥的同样请求被挡", func(t *testing.T) {
		pl2 := pool.New("")
		pl2.Add(&auth.Auth{UID: "victim2", FilePath: filepath.Join(dir, "trae-victim2.json")})
		p2 := New(Config{Pool: pl2, AuthDir: dir, APIKey: "k"})
		rec2 := auditPanelReq(p2, "POST", "/panel/api/accounts/victim2/remove", "{}", map[string]string{
			"Origin":         "https://evil.example",
			"Sec-Fetch-Site": "cross-site",
			"Content-Type":   "text/plain;charset=UTF-8",
		})
		t.Logf("有密钥时同一跨站请求: code=%d", rec2.Code)
		if rec2.Code != http.StatusUnauthorized {
			t.Fatalf("期望 401（有密钥时没有 Authorization 就进不来）：code=%d", rec2.Code)
		}
	})

	t.Run("跨站改写配置被挡（含清空 api_key 这条路）", func(t *testing.T) {
		// saveConfig 不看 Content-Type，text/plain 的简单请求带 JSON 文本也能落进去；
		// 密钥清空 = 关掉鉴权，这正是「一台网关自己把自己打开」的那一步。现在被同源判据挡住。
		var got string
		p3 := New(Config{
			Pool:       pool.New(""),
			SaveConfig: func(raw []byte) ([]string, error) { got = string(raw); return nil, nil },
		})
		rec3 := auditPanelReq(p3, "POST", "/panel/api/config", `{"api_key":""}`, map[string]string{
			"Origin":         "https://evil.example",
			"Sec-Fetch-Site": "cross-site",
			"Content-Type":   "text/plain;charset=UTF-8",
		})
		t.Logf("跨站 POST /panel/api/config: code=%d，服务端收到的原文=%q", rec3.Code, got)
		if rec3.Code == http.StatusOK || got != "" {
			t.Fatalf("跨站改写配置被放行了（CSRF 回归）: code=%d raw=%q", rec3.Code, got)
		}
	})
}

// TestAuditPanelAPIsAllRequireKey 反证用：逐条打**全部** /panel/api/* 路由（无 Authorization、
// 带转发头 = 反代/隧道来源），期望一律 401。当前实现全部命中，故「有接口漏挂 withAuth」不成立；
// 这条同时是那批路由的回归网（新增端点忘了包 withAuth 时会红）。
func TestAuditPanelAPIsAllRequireKey(t *testing.T) {
	routes := []struct{ method, path string }{
		{"GET", "/panel/api/overview"},
		{"GET", "/panel/api/logs"},
		{"GET", "/panel/api/request_logs"},
		{"GET", "/panel/api/models"},
		{"GET", "/panel/api/packages"},
		{"GET", "/panel/api/usage"},
		{"POST", "/panel/api/usage/save"},
		{"GET", "/panel/api/config"},
		{"POST", "/panel/api/config"},
		{"POST", "/panel/api/login/start"},
		{"POST", "/panel/api/login/finish"},
		{"POST", "/panel/api/login/refresh"},
		{"POST", "/panel/api/checkin"},
		{"POST", "/panel/api/balance"},
		{"POST", "/panel/api/keepalive"},
		{"POST", "/panel/api/accounts/u1/checkin"},
		{"POST", "/panel/api/accounts/u1/balance"},
		{"POST", "/panel/api/accounts/u1/disable"},
		{"POST", "/panel/api/accounts/u1/enable"},
		{"POST", "/panel/api/accounts/u1/clear-cooldown"},
		{"POST", "/panel/api/accounts/u1/rotate-device"},
		{"POST", "/panel/api/accounts/rotate-device-all"},
		{"POST", "/panel/api/accounts/u1/remove"},
	}
	p := New(Config{Pool: pool.New(""), APIKey: "k", AuthDir: t.TempDir()})
	bad := 0
	for _, rt := range routes {
		rec := auditPanelReq(p, rt.method, rt.path, "{}", map[string]string{"X-Forwarded-For": "10.0.0.9"})
		if rec.Code != http.StatusUnauthorized {
			bad++
			t.Errorf("%s %s → %d（期望 401；404 = 路由或 withAuth 漏了）", rt.method, rt.path, rec.Code)
		}
	}
	t.Logf("逐条无密钥访问 %d 个面板 API 路由：非 401 的有 %d 个", len(routes), bad)
	// 静态路径（页面/脚本）本来就不鉴权，但必须有 no-store，避免缓存把密钥页发出去。
	page := auditPanelReq(p, "GET", "/panel/", "", nil)
	if cc := page.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("/panel/ Cache-Control=%q", cc)
	}
}

// ---------------------------------------------------------------------------
// 2. 「空密钥」被显示成「本机免鉴权」：与真实绑定面无关的安全指示
// ---------------------------------------------------------------------------

// TestAuditOverviewReportsAuthScope：overview 只透出
// auth_required（panel.go:228），app.js:265 由它单独决定文案「本机免鉴权」。
// 但绑定地址来自 config.listen，默认 ":7864"（= 0.0.0.0，全接口）—— 同一份 overview 在
// 「只监听回环」和「监听全部网卡」两种配置下**完全相同**，前端那句「本机」在后者上是错的，
// 而它正是运维判断"这个网关有没有暴露"的唯一界面依据。
func TestAuditOverviewReportsAuthScope(t *testing.T) {
	p := New(Config{Pool: pool.New(""), Version: "audit"}) // APIKey 空
	rec := auditPanelReq(p, "GET", "/panel/api/overview", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	t.Logf("overview 字段: %s", strings.Join(keys, ", "))
	t.Logf("auth_required=%v auth_scope=%v", body["auth_required"], body["auth_scope"])
	if body["auth_required"] != false {
		t.Fatalf("期望复现「空密钥 → 免密钥访问」：auth_required=%v", body["auth_required"])
	}
	// 没有 BindAll（测试直连 New，不知道 listen）时必须是 loopback：不能凭空说「对全网开放」。
	if body["auth_scope"] != "loopback" {
		t.Fatalf("空密钥 + 未告知绑定范围时应报 loopback，实得 %v", body["auth_scope"])
	}
	app, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`d\.auth_scope === 'open'`).Match(app) {
		t.Fatal("app.js 的免鉴权提示没有按 auth_scope 分叉：绑全接口时仍会显示「本机免鉴权」")
	}
	// 绑全接口这一支单独验一次：BindAll 为真时必须报 open（"本机免鉴权" 的说明此时是错的）。
	p2 := New(Config{Pool: pool.New(""), Version: "audit", BindAll: true})
	rec2 := auditPanelReq(p2, "GET", "/panel/api/overview", "", nil)
	var body2 map[string]any
	if err := json.Unmarshal(rec2.Body.Bytes(), &body2); err != nil {
		t.Fatal(err)
	}
	if body2["auth_scope"] != "open" {
		t.Fatalf("空密钥 + 绑全接口应报 open，实得 %v", body2["auth_scope"])
	}
	t.Logf("→ 绑全接口时会显示「免密钥访问（已绑全接口）」，不再谎称「本机免鉴权」")
}

// ---------------------------------------------------------------------------
// 3. 面板 API 响应（含明文 api_key）没有 no-store：与 /panel/ 页面不一致
// ---------------------------------------------------------------------------

// TestAuditConfigAPIHasNoCacheControl：index.go:31 / index.go:39 给页面与脚本加了
// Cache-Control: no-store，但 writeJSON（panel.go:733-742）没有 —— /panel/api/config
// 的响应体里**就是明文 api_key**（main.go:195 的 LoadConfig 如实回显整份配置）。
// 请求带 Authorization，所以共享缓存理论上不存；但浏览器私有缓存与「离开页面/登出后
// 回退」这类场景没有任何响应头拦着，同一份 JSON 也是唯一带密钥的响应。
func TestAuditConfigAPIHasNoCacheControl(t *testing.T) {
	p := New(Config{
		Pool: pool.New(""), APIKey: "k", ConfigPath: "/tmp/audit-config.json",
		LoadConfig: func() (any, error) { return map[string]any{"api_key": "sk-audit-secret"}, nil },
	})
	page := auditPanelReq(p, "GET", "/panel/", "", nil)
	api := auditPanelReq(p, "GET", "/panel/api/config", "", map[string]string{"Authorization": "Bearer k"})
	t.Logf("/panel/             Cache-Control=%q", page.Header().Get("Cache-Control"))
	t.Logf("/panel/api/config   Cache-Control=%q body=%s", api.Header().Get("Cache-Control"), strings.TrimSpace(api.Body.String()))
	if page.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("页面本来就有 no-store，这条断言的前提变了")
	}
	if !strings.Contains(api.Body.String(), "sk-audit-secret") {
		t.Fatal("这个响应本该带明文密钥，前提变了")
	}
	// 面板 API 一律 no-store（响应体里有明文密钥/账号状态，浏览器缓存住就是外泄面）。
	if cc := api.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("API 响应缺 no-store: Cache-Control=%q", cc)
	}
}

// ---------------------------------------------------------------------------
// 4. 前端死开关 / 死属性：rolling 参数只被传 false，trangeLabel 无调用点
// ---------------------------------------------------------------------------

// TestAuditFrontendHasNoDeadSwitchOrAttribute：三处「写了没读」已删干净（rolling 参数 /
// trangeLabel / data-lock），改成守住「不再长出来」的回归网。
func TestAuditFrontendHasNoDeadSwitchOrAttribute(t *testing.T) {
	app, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	page, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	src := string(app)

	calls := regexp.MustCompile(`trangeQuery\('([^']+)'`).FindAllStringSubmatch(src, -1)
	t.Logf("trangeQuery 调用点: %v", calls)
	if len(calls) == 0 {
		t.Fatal("没扫到 trangeQuery 的调用点，正则失效")
	}
	if regexp.MustCompile(`trangeQuery\('[^']+',`).MatchString(src) {
		t.Fatal("trangeQuery 又出现了第二个参数（死开关回来了）")
	}
	if n := strings.Count(src, "trangeLabel"); n != 0 {
		t.Fatalf("trangeLabel 又出现了（%d 次）：没人调的函数别留在文件里", n)
	}
	if n := strings.Count(src, "data-lock") + strings.Count(string(page), "data-lock"); n != 0 {
		t.Fatalf("data-lock 又出现了（%d 次）：没人读的属性别留在文件里", n)
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("无 node，跳过区间参数语义断言")
	}
	driver := `const assert = require('assert');
let auditPreset = '0';
function trangeState(){ return { preset: auditPreset, from: null, to: null }; }
function trangeMidnight(){ return new Date(2026, 0, 1, 0, 0, 0); }
` + extractJSFunc(t, src, "trangeQuery") + `
for (const preset of ['0', '1', '24', 'today', 'custom']) {
  auditPreset = preset;
  console.log('preset=' + preset + ' → "' + trangeQuery('x').toString() + '"');
}
auditPreset = '0';
assert.strictEqual(trangeQuery('x').get('hours'), null, '「全部历史」不该发 hours');
auditPreset = '24';
assert.ok(trangeQuery('x').get('from'), '滚动预设要发 from');
assert.strictEqual(trangeQuery('x').get('hours'), null, '一律走 from/to，不发 hours');
`
	out, err := exec.Command(node, "-e", driver).CombinedOutput()
	t.Logf("node 输出:\n%s", strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("node 断言失败: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 5. 反证：上游/客户端可控字符串进 innerHTML 的路径都已转义（XSS 不成立）
// ---------------------------------------------------------------------------

// TestAuditFrontendEscapesUpstreamStrings 反证用：把 HTML/属性注入载荷塞进**服务端可控**
// 字段（模型 id/展示名/能力/上代原因 —— 这些来自上游模型表；请求记录里的 model/UA/IP
// 来自任意客户端，走的是同一套 esc+innerHTML 拼装），断言渲染结果里没有裸标签、没有属性
// 逃逸。当前实现全部转义（mdRowHtml → esc/tag），故「前端把用户可控数据直接塞 innerHTML」
// 这条不成立。
func TestAuditFrontendEscapesUpstreamStrings(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("无 node，跳过转义断言")
	}
	raw, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	// fmtNum / fmtRate / fnLabel 是单行函数，extractJSFunc 抠不出来，这里按 app.js 逐字复刻。
	driver := `const assert = require('assert');
function fmtNum(n) { return Number(n || 0).toLocaleString('en-US'); }
const FN_LABEL = { solo_work_lite: 'Work', solo_coder: 'Coder' };
function fnLabel(fn) { return FN_LABEL[fn] || fn || '—'; }
` + extractJSFunc(t, string(raw), "esc") + "\n" +
		extractJSFunc(t, string(raw), "tag") + "\n" +
		extractJSFunc(t, string(raw), "switchLabel") + "\n" +
		extractJSFunc(t, string(raw), "thinkCell") + "\n" +
		extractJSFunc(t, string(raw), "mdRowHtml") + `
const evil = {
  id: 'cn:<img src=x onerror=alert(1)>',
  name: '" onmouseover="alert(2)" x="',
  capability: '<svg/onload=alert(3)>',
  function: 'solo_coder',
  thinking: '<script>alert(4)</script>',
  reasoning_effort_config: '{"support_thinking":false,"x":"<b>raw</b>"}',
  credit_rate: 1.5, context_length: 200000, context_length_max: 262144, max_output_tokens: 32000,
  legacy: true, legacy_reason: '"><img src=y onerror=alert(5)>',
};
const html = mdRowHtml(evil);
console.log(html.slice(0, 400));
assert.ok(!/<img|<svg|<script|<b>/i.test(html), '渲染结果里出现裸标签: ' + html);
assert.ok(!/" onmouseover=/.test(html), '属性被逃逸: ' + html);
assert.ok(html.includes('&lt;img'), '期望载荷被转义成实体');
assert.ok(html.includes('&quot;'), '期望引号被转义成实体');
`
	out, err := exec.Command(node, "-e", driver).CombinedOutput()
	t.Logf("node 输出:\n%s", strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("期望「都已转义」（反证失败 = 真找到了 XSS）: %v", err)
	}
}
