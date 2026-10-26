//go:build audit

// audit_upstream_test.go 只读缺陷审计的**证明测试**：断言的是当前（错误/可疑）行为。
//
//	跑法：go test -tags audit -count=1 -run TestAudit ./internal/upstream/
//	竞态：go test -tags audit -race -count=1 -run TestAuditRace ./internal/upstream/
//
// 每条断言旁注明「修好之后应改成什么」。为了让它继续绿而放宽断言 = 把证据抹掉。
// 不写真网络：全部 httptest / 内存 reader。
package upstream

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"traework2api/internal/auth"
)

func auditAuth() *auth.Auth {
	return &auth.Auth{AccessToken: "at-audit", UID: "1234567890123456"}
}

// ── 1. 错误分类：1005/plan 排在传输层之后（已修，回归网）─────────────────────

func TestAuditClassifyPlanBranchBeatsTransport(t *testing.T) {
	// 传输层的信号优先：429/404/5xx 的 body 里带 1005 文案时按传输层算。
	// 曾是「plan 分支在最前面」→ 一次软限流被判 ErrPlanLimit = 12h 硬冷却。
	got := Classify(http.StatusTooManyRequests, `{"code":1005,"message":"too many requests"}`)
	t.Logf("429 + {\"code\":1005} → %v（按注释应是 ErrSoftRate）", got)
	if got != ErrSoftRate {
		t.Fatalf("429 被业务码抢走了判定: %v（应为 ErrSoftRate）", got)
	}

	// 同一个业务码，只差 JSON 里一个空格：现在两种写法同结果（都是 429 → ErrSoftRate）。
	noSpace := Classify(http.StatusTooManyRequests, `{"code":1005}`)
	withSpace := Classify(http.StatusTooManyRequests, `{"code": 1005}`)
	t.Logf("\"code\":1005 → %v ; \"code\": 1005 → %v", noSpace, withSpace)
	if noSpace != withSpace || noSpace != ErrSoftRate {
		t.Fatalf("同一业务码两种写法结果不一致: %v / %v（都应为 ErrSoftRate）", noSpace, withSpace)
	}

	// 5xx 是上游故障（该 NoteError 熔断计数），body 里带 1005 也一样。
	got5 := Classify(http.StatusInternalServerError, `{"code":1005,"plan":"free"}`)
	t.Logf("500 + 1005 → %v（应为 ErrServer）", got5)
	if got5 != ErrServer {
		t.Fatalf("500 被业务码抢走了判定: %v（应为 ErrServer）", got5)
	}
	// 认不出的业务码交给 HTTP 状态判定（「认不出的业务码不算」）。
	unknown := Classify(http.StatusBadRequest, `{"code":111005,"message":"plan misuse"}`)
	t.Logf("400 + 认不出的 111005 → %v（应为 ErrClient）", unknown)
	if unknown != ErrClient {
		t.Fatalf("未识别业务码不该按 plan 处理: %v（应为 ErrClient）", unknown)
	}
}

// ── 2. HTTP 级与流内**不是**同一张判据（client.go:136 vs solosse.go:84）────────

func TestAuditHTTPAndStreamKindDiverge(t *testing.T) {
	// client.go:133-140 声称「同一个码走 HTTP 状态与走 SSE 流内事件是同一套判据」，
	// 但 HTTP 侧把整张表锁在「body 顶层有非零数字业务码」后面（bodyBusinessCode != 0），
	// 流内侧（SOLOStreamError.Kind）无条件用码 + 文案。结果：同一种上游语义落进
	// 不同的池动作（ErrClient = NoteDegrade 降权计数；ErrSoftRate = 60s 软冷却）。
	cases := []struct {
		name       string
		status     int
		body       string
		stream     *SOLOStreamError
		wantHTTP   ErrKind
		wantStream ErrKind
	}{
		{"quota 文案、无数字业务码", 400, `{"message":"quota exceeded"}`,
			&SOLOStreamError{Code: 4008, Msg: "quota exceeded"}, ErrSoftRate, ErrSoftRate},
		{"业务码嵌在 error 里（OpenAI 信封）", 400, `{"error":{"code":4008,"message":"quota exceeded"}}`,
			&SOLOStreamError{Code: 4008, Msg: "quota exceeded"}, ErrSoftRate, ErrSoftRate},
		{"顶层 code 为 0 + 文案", 400, `{"code":0,"message":"quota exceeded"}`,
			&SOLOStreamError{Code: 4008, Msg: "quota exceeded"}, ErrSoftRate, ErrSoftRate},
		{"模型配置为空（不该罚号）", 400, `{"message":"model config is empty"}`,
			&SOLOStreamError{Code: 4001, Msg: "the param is invalid"}, ErrNone, ErrNone},
	}
	for _, tc := range cases {
		h, s := Classify(tc.status, tc.body), tc.stream.Kind()
		t.Logf("%-32s HTTP=%v 流内=%v", tc.name, h, s)
		// 同一段文案走 HTTP 状态与走流内事件必须落进同一个池动作（ErrClient 会降权、
		// ErrSoftRate 只软冷却，两边不一致时同一个上游语义会得到两种惩罚）。
		if h != s {
			t.Fatalf("%s：两种到达方式结果不一致 HTTP=%v 流内=%v", tc.name, h, s)
		}
		if h != tc.wantHTTP || s != tc.wantStream {
			t.Fatalf("%s：实际 HTTP=%v 流内=%v，与预期不符", tc.name, h, s)
		}
	}
}

// ── 3. 流内 event:error 之后仍然续写（errSeen 从不赋值，continue.go:65/249）────

func TestAuditContinueAfterStreamError(t *testing.T) {
	const seg1 = "event:output\ndata:{\"response\":\"AAA\",\"tool_calls\":null}\n\n" +
		"event:token_usage\ndata:{\"prompt_tokens\":10,\"completion_tokens\":32000,\"total_tokens\":32010}\n\n" +
		"event:error\ndata:{\"code\":1005,\"message\":\"plan exhausted\"}\n\n"
	const seg2 = "event:output\ndata:{\"response\":\"BBB\"}\n\n" +
		"event:token_usage\ndata:{\"prompt_tokens\":1,\"completion_tokens\":3,\"total_tokens\":4}\n\n" +
		"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n"

	var mu sync.Mutex
	var reqs int
	var secondBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs++
		n := reqs
		if n == 2 {
			secondBody = string(b)
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			io.WriteString(w, seg1)
		} else {
			io.WriteString(w, seg2)
		}
		w.(http.Flusher).Flush()
	}))
	defer ts.Close()

	c := &Client{HTTP: ts.Client(), AgentHost: ts.URL}
	a := auditAuth()
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	rc, status, _, err := c.ChatStream(a, body)
	if err != nil || rc == nil {
		t.Fatalf("ChatStream: status=%d err=%v", status, err)
	}
	defer rc.Close()

	cont := NewContinueReader(c, a, body, rc)
	rr := httptest.NewRecorder()
	var onErr []*SOLOStreamError
	_, serr := StreamWithError(rr, cont, func(se *SOLOStreamError) { onErr = append(onErr, se) }, nil)
	cont.Close()
	out := rr.Body.String()

	mu.Lock()
	n, sb := reqs, secondBody
	mu.Unlock()
	t.Logf("上游请求数=%d（=1 无续写，=2 说明 error 之后又发了一次请求）", n)
	t.Logf("流内回调次数=%d；StreamWithError err=%v", len(onErr), serr)
	// 流内 error 之后不许再续写：客户端已经收到 error + [DONE]，后面再冒内容就是协议错乱
	// （errSeen 由 flushEvent 在 error 事件上置位）。
	if n != 1 {
		t.Fatalf("error 之后又续写了一轮（上游请求数=%d）: %s", n, truncate(sb, 160))
	}
	if len(onErr) == 0 {
		t.Fatalf("流内错误回调没触发：%v", serr)
	}

	// 客户端侧协议：收到 error 事件 + [DONE]，且**没有**第二段正文。
	if !strings.Contains(out, "event: error") {
		t.Fatalf("客户端没收到 error 事件：%s", truncate(out, 200))
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("缺 [DONE]：%s", truncate(out, 240))
	}
	if strings.Contains(out, "BBB") {
		t.Fatalf("error 之后仍下发了续写正文：%s", truncate(out, 240))
	}
	t.Logf("客户端字节序：error@%d < [DONE]@%d（[DONE] 之后不该再有正文）",
		strings.Index(out, "event: error"), strings.Index(out, "data: [DONE]"))
}

// ── 4. readErr 跨段粘滞（continue.go:144 / 234）──────────────────────────────

func TestAuditContinueStaleReadError(t *testing.T) {
	const seg1 = "event:output\ndata:{\"response\":\"AAA\",\"tool_calls\":null}\n\n" +
		"event:token_usage\ndata:{\"prompt_tokens\":10,\"completion_tokens\":32000,\"total_tokens\":32010}\n\n"
	// 第二段干净收尾（无 done 事件，只有 EOF）—— 正常上游形态。
	const seg2 = "event:output\ndata:{\"response\":\"BBB\"}\n\n" +
		"event:token_usage\ndata:{\"prompt_tokens\":1,\"completion_tokens\":8,\"total_tokens\":9}\n\n"

	var mu sync.Mutex
	var reqs int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs++
		n := reqs
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			// 声明 1MB 只写一小段 → 客户端后续 Read 拿到 io.ErrUnexpectedEOF（非 EOF 的传输错误）
			w.Header().Set("Content-Length", "1000000")
			io.WriteString(w, seg1)
			w.(http.Flusher).Flush()
			return
		}
		io.WriteString(w, seg2)
		w.(http.Flusher).Flush()
	}))
	defer ts.Close()

	c := &Client{HTTP: ts.Client(), AgentHost: ts.URL}
	a := auditAuth()
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	rc, status, _, err := c.ChatStream(a, body)
	if err != nil || rc == nil {
		t.Fatalf("ChatStream: status=%d err=%v", status, err)
	}
	cont := NewContinueReader(c, a, body, rc)
	defer cont.Close()

	all, err := io.ReadAll(cont)
	t.Logf("已交付字节=%q；ReadAll 最终错误=%v", truncate(string(all), 40), err)

	mu.Lock()
	n := reqs
	mu.Unlock()
	if n != 2 {
		t.Fatalf("没有发生续写（上游请求数=%d），本测试的前提不成立", n)
	}
	if !bytes.Contains(all, []byte("BBB")) {
		t.Fatalf("第二段正文没有交付：%q", truncate(string(all), 80))
	}
	// 上一段的读错误到换段时该清掉：续写把它救回来了，整体就是完整的，Read 不该再把它
	// 当作最终错误抛出去（那会让用量记失败 + 解绑会话粘性）。
	if err != nil {
		t.Fatalf("续写救回的完整回复仍带最终错误 %v（粘滞读错误未清）", err)
	}
}

// ── 5. 单条 SSE 行没有上限（solosse.go:222 / 445，continue.go:121）────────────

// auditNoNewline 只吐 'a'、永不吐换行：模拟上游/中间盒的一帧无换行长 data。
type auditNoNewline struct{ left int64 }

func (r *auditNoNewline) Read(p []byte) (int, error) {
	if r.left <= 0 {
		return 0, io.EOF
	}
	n := len(p)
	if int64(n) > r.left {
		n = int(r.left)
	}
	for i := 0; i < n; i++ {
		p[i] = 'a'
	}
	r.left -= int64(n)
	return n, nil
}

func TestAuditSSESingleLineUnbounded(t *testing.T) {
	const total = 32 << 20
	feed := &auditNoNewline{left: total}
	_, err := Aggregate(feed)
	t.Logf("无换行的 %d MB 单帧 → Aggregate 只读掉 %d MB 就返回 err=%v",
		total>>20, (total-feed.left)>>20, err)
	if feed.left <= 0 {
		t.Fatalf("32 MB 无换行单帧被整段读掉：单行仍无上限")
	}
	if err == nil {
		t.Fatalf("单行超过上限却没报错：应判流坏")
	}
}

// ── 6. UserEntUsage 的 remain 不夹 0（client.go:1029 vs 1101）─────────────────

func TestAuditUserEntUsageNegativeRemain(t *testing.T) {
	const pack = `{"user_entitlement_pack_list":[{"display_desc":"over",
		"entitlement_base_info":{"quota":{"credits_limit":100},"end_time":4102444800},
		"usage":{"credits_amount":150}}]}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, pack)
	}))
	defer ts.Close()

	c := &Client{HTTP: ts.Client(), UgHost: ts.URL}
	a := auditAuth()
	remain, total, _, err := c.UserEntUsage(a)
	if err != nil {
		t.Fatalf("UserEntUsage: %v", err)
	}
	pkgs, sumRemain, sumSize, err := c.CreditPackages(a)
	if err != nil {
		t.Fatalf("CreditPackages: %v", err)
	}
	t.Logf("同一份上游数据：UserEntUsage remain=%d total=%d / CreditPackages remain=%d size=%d (%d 包)",
		remain, total, sumRemain, sumSize, len(pkgs))
	// 上游给的 used(150) > limit(100)：两条路都必须夹 0，不能报剩余 -50
	// （否则超支的包会把别的包的余额抵掉，选号/面板都跟着错）。
	if remain != 0 {
		t.Fatalf("UserEntUsage 该夹 0，实得 remain=%d", remain)
	}
	if sumRemain != 0 {
		t.Fatalf("CreditPackages remain=%d 该夹 0", sumRemain)
	}
	if total != 100 || sumSize != 100 {
		t.Fatalf("总额口径应一致：total=%d size=%d", total, sumSize)
	}
}

// ── 7. identity 覆盖对国际版版本头 / GetUserInfo 请求体无效（client.go:252/1181）─

func TestAuditIdentityOverrideNotAppliedIntl(t *testing.T) {
	SetIdentity("AuditUA/9.9.9", "9.9.9")
	t.Cleanup(func() { SetIdentity("", "") })

	cn, cnCode := identFor(&auth.Auth{Domain: "trae.cn"})
	intl, intlCode := identFor(&auth.Auth{Domain: "trae.ai"})
	t.Logf("CN=(%s,%s) Intl=(%s,%s) currentUA=%s", cn, cnCode, intl, intlCode, currentUA())
	if cn != "9.9.9" {
		t.Fatalf("国内版没吃到覆盖：%q", cn)
	}
	// 覆盖是运维显式钉死的：国际版也要吃，否则「设了覆盖但国际流量还是老版本」没人看得出。
	if intl != "9.9.9" || intlCode != IdeVersionCodeIntl {
		t.Fatalf("国际版没吃到版本覆盖或动了版本码：(%q,%q)", intl, intlCode)
	}

	var body string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		io.WriteString(w, `{"Result":{"UserID":"u","ScreenName":"n"}}`)
	}))
	defer ts.Close()
	a := auditAuth()
	a.ApiHost = ts.URL
	if _, _, _, err := (&Client{HTTP: ts.Client()}).GetUserInfo(a); err != nil {
		t.Fatalf("GetUserInfo: %v", err)
	}
	t.Logf("GetUserInfo 请求体=%s", body)
	// 请求体里必须带上覆盖版本（国际版同样生效）。
	if !strings.Contains(body, "9.9.9") {
		t.Fatalf("GetUserInfo 请求体没带上覆盖版本: %s", body)
	}
}

// ── 8. nil *auth.Auth 在头部构造函数里 panic（headers.go:27 / 131）────────────

func auditPanics(f func()) (r any) {
	defer func() { r = recover() }()
	f()
	return nil
}

func TestAuditNilAuthPanicsInHeaderBuilders(t *testing.T) {
	// 同一条调用链的两半口径不一致：base-URL / 分类 / 版本函数都显式支持 nil
	//（client.go:240「Auth 为 nil（如配置校验路径）按国内版」），头部构造函数直接解引用。
	if got := (&Client{AgentHost: "http://x"}).agentBaseFor(nil); got != "http://x" {
		t.Fatalf("agentBaseFor(nil)=%q，nil 容忍性已变", got)
	}
	if got := (&Client{UgHost: "http://y"}).ugBaseFor(nil); got != "http://y" {
		t.Fatalf("ugBaseFor(nil)=%q", got)
	}
	cases := []struct {
		name string
		fn   func()
	}{
		{"SOLOHeaders(nil)", func() {
			req, _ := http.NewRequest(http.MethodPost, "http://x", nil)
			SOLOHeaders(req, nil, false)
		}},
		{"UgHeaders(nil)", func() {
			req, _ := http.NewRequest(http.MethodPost, "http://x", nil)
			UgHeaders(req, nil)
		}},
	}
	for _, tc := range cases {
		if r := auditPanics(tc.fn); r == nil {
			t.Fatalf("%s 不再 panic（已加 nil 保护？）—— 删除本条断言", tc.name)
		} else {
			t.Logf("%s → panic: %v", tc.name, r)
		}
	}
}

// ── 9. normalizeTools 对非 map 工具条目静默剔除，且 tool_choice 残留 ───────────

func TestAuditToolsNonMapDropIsSilent(t *testing.T) {
	// payload.go:171-173 承诺「剔除**必须留痕**」，但 payload.go:186-189 对非 map 的条目
	// 直接 continue，不留痕也不进 droppedNames（于是 tool_choice 残留在没有 tools 的报文里）。
	var buf bytes.Buffer
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(os.Stderr); log.SetFlags(log.LstdFlags) })

	out := PrepareBody([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"tools":["web_search"],"tool_choice":"web_search"}`))
	t.Logf("出站 body=%s", out)
	t.Logf("剔除日志=%q", buf.String())
	if strings.Contains(string(out), `"tools"`) {
		t.Fatalf("非 map 工具条目该被剔除（上游反序列化不了）: %s", out)
	}
	// 剔除必须留痕：静默丢 = 客户端以为自己还能调这个工具。
	if !strings.Contains(buf.String(), "web_search") || !strings.Contains(buf.String(), "剔除") {
		t.Fatalf("剔除非 map 条目没留痕: %q", buf.String())
	}
}

// ── 竞态：超时字段热改 + 在途请求 + 包级身份（期望**无**竞争）─────────────────

func TestAuditRaceTimeoutAndIdentity(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer ts.Close()

	c := &Client{HTTP: ts.Client(), StreamHTTP: ts.Client(), AgentHost: ts.URL, UgHost: ts.URL}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(3)
	go func() { // 面板热改配置
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			c.SetTimeouts(time.Second, time.Second, time.Second)
			c.SetTimeout(time.Second)
		}
	}()
	go func() { // 在途短 RPC
		defer wg.Done()
		for i := 0; i < 300; i++ {
			req, _ := http.NewRequest(http.MethodPost, ts.URL, nil)
			_, _ = c.doJSON(req)
		}
	}()
	go func() { // 出站身份 + 头部构造
		defer wg.Done()
		a := auditAuth()
		for i := 0; i < 300; i++ {
			SetIdentity("AuditUA/x", "1.2.3")
			_ = currentUA()
			_, _ = identFor(a)
			req, _ := http.NewRequest(http.MethodPost, ts.URL, nil)
			SOLOHeaders(req, a, true)
		}
	}()
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
	SetIdentity("", "")
	t.Log("无报告竞态（-race）：写锁与读锁跨 Do 的互斥成立")
}
