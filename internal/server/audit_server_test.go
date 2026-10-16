//go:build audit

// audit_server_test.go 只读审计的「证明测试」：断言**当前（有问题）**的行为，
// 全部用 httptest / 构造数据，不碰真实网络（无 TW2A_PROBE_*、无线上地址）。
//
// 跑法：go test -tags audit -count=1 -run TestAudit -v ./internal/server/
// 说明：这些用例在缺陷被修好后会失败 —— 那是设计意图（断言的是「缺陷存在」）。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"traework2api/internal/auth"
	"traework2api/internal/pool"
	"traework2api/internal/reqlog"
	"traework2api/internal/session"
	"traework2api/internal/upstream"
	"traework2api/internal/usage"
)

// auditModelProbeBody 模型表探测（get_detail_param）的假响应：形状不可解析 = 拉不到动态表，
// mapModel / modelList 回落到静态表 —— 与生产里「上游拉表失败」同一条路径。
const auditModelProbeBody = `{"user_info":{"config_info_list":[]}}`

// auditUpstream 假上游：非对话路径一律回模型探测体，对话路径由 chat 决定。
func auditUpstream(calls *[]string, chat func(authz string) (int, string, bool)) *upstream.Client {
	return fakeWithPaths(calls, func(path, authz string) (int, string, bool) {
		if path != upstream.EpChat {
			return 200, auditModelProbeBody, false
		}
		return chat(authz)
	})
}

func auditPost(h *Handler, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader(body)))
	return rec
}

// ---------------------------------------------------------------------------
// 1. 流式入站协议的「首字节前错误」丢掉 HTTP 状态码（200 + 错误帧）
// ---------------------------------------------------------------------------

// TestAuditResponsesStreamPreStreamErrorIsHTTP200：/v1/responses + stream=true 时，
// 请求在**写任何字节之前**就失败了（模型名未知），但客户端拿到的是 HTTP 200 + SSE
// response.failed —— 状态码不再表达失败；请求记录（reqlog）也因此记成 success。
func TestAuditResponsesStreamPreStreamErrorIsHTTP200(t *testing.T) {
	var calls []string
	up := auditUpstream(&calls, func(string) (int, string, bool) {
		return 500, "对话不该被调到", false
	})
	rl := reqlog.New(reqlog.Config{})
	h := NewHandler(Config{
		Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:   up,
		RequestLog: rl,
	})
	rec := auditPost(h, "/v1/responses", `{"model":"audit-nope","stream":true,"input":"hi"}`)

	snap := rl.Snapshot()
	t.Logf("HTTP status=%d content-type=%q", rec.Code, rec.Header().Get("Content-Type"))
	t.Logf("body=%s", strings.TrimSpace(rec.Body.String()))
	t.Logf("请求记录: status=%d ok=%v outcome=%q",
		snap.Recent[0].Status, snap.Recent[0].OK, snap.Recent[0].Outcome)

	// 首字节还没写给客户端时，错误要用真实状态码回，并且请求记录不能记成功。
	if rec.Code == http.StatusOK {
		t.Fatalf("首字节前的错误被当成 200 回了: body=%s", strings.TrimSpace(rec.Body.String()))
	}
	if snap.Recent[0].Status == 200 || snap.Recent[0].OK || snap.Recent[0].Outcome == reqlog.OutcomeSuccess {
		t.Fatalf("一次失败被记成成功: %+v", snap.Recent[0])
	}
	if strings.Contains(strings.Join(calls, ","), upstream.EpChat) {
		t.Fatalf("模型名就该在本地被拒，不该打上游：%v", calls)
	}
}

// TestAuditMessagesStreamPreStreamErrorIsHTTP200 同一缺陷的 Anthropic 侧。
func TestAuditMessagesStreamPreStreamErrorIsHTTP200(t *testing.T) {
	var calls []string
	up := auditUpstream(&calls, func(string) (int, string, bool) {
		return 500, "对话不该被调到", false
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := auditPost(h, "/v1/messages",
		`{"model":"audit-nope","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)

	t.Logf("HTTP status=%d content-type=%q", rec.Code, rec.Header().Get("Content-Type"))
	t.Logf("body=%s", strings.TrimSpace(rec.Body.String()))
	if rec.Code != http.StatusOK {
		t.Fatalf("期望复现「错误也被当成 200 回」：实际 %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "event: error") || !strings.Contains(body, "unknown model") {
		t.Fatalf("期望 SSE error 帧带原始错误文案，实得 %q", body)
	}
}

// ---------------------------------------------------------------------------
// 2. 无 token_usage 的成功回复被判成失败：台账记错 + 会话粘性被解绑
// ---------------------------------------------------------------------------

// auditNoUsageSSE 有正文、有正常收尾（done），但**没有** token_usage 帧。
// 上游某些模型/某些时段确实不回报用量（SOLO 只在有 usage 时才发该帧）。
const auditNoUsageSSE = "event:metadata\ndata:{\"model\":\"\",\"session_id\":\"s1\"}\n\n" +
	"event:output\ndata:{\"response\":\"你好\",\"reasoning_content\":\"\",\"tool_calls\":null}\n\n" +
	"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n"

func TestAuditSuccessWithoutUsageRecordedAsFailureAndUnbindsSession(t *testing.T) {
	up := auditUpstream(new([]string), func(string) (int, string, bool) { return 200, auditNoUsageSSE, true })
	ledger := usage.New("") // 不落盘
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	sess := session.New(session.Config{Enabled: true, TTL: time.Minute, GCInterval: time.Minute})
	defer sess.Stop()
	h := NewHandler(Config{Pool: p, Upstream: up, Usage: ledger, Session: sess})

	body := `{"model":"glm-5.2","stream":true,"conversation_id":"c-audit","messages":[{"role":"user","content":"hi"}]}`
	key := session.ExtractKey([]byte(body))
	sess.Bind(key, "u1") // 上一条请求已经把这个会话粘到 u1

	rr := auditPost(h, "/v1/chat/completions", body)
	snap := ledger.Snapshot(72, nil)
	t.Logf("HTTP status=%d body=%q", rr.Code, strings.TrimSpace(rr.Body.String()))
	t.Logf("用量台账: requests=%d errors=%d", snap.Totals.Requests, snap.Totals.Errors)
	_, stillBound := sess.Resolve(key)
	t.Logf("会话绑定还在吗: %v (bindings=%d)", stillBound, sess.Count())

	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "你好") {
		t.Fatalf("这次请求其实是成功的（正文已下发）：code=%d body=%s", rr.Code, rr.Body)
	}
	// 修好后的行为：正文下发即成功（上游某些段不给 token_usage，不能拿它当成功判据）；
	// 用量台账记 ok，会话粘性保持绑定。
	if snap.Totals.Requests != 1 || snap.Totals.Errors != 0 {
		t.Fatalf("完整回复被记成失败（回归）：%+v", snap.Totals)
	}
	if !stillBound {
		t.Fatalf("成功请求不该解绑会话粘性：Resolve=%v", stillBound)
	}
}

// ---------------------------------------------------------------------------
// 3. attempt 内部 panic → 在途名额永久泄漏（release 不是 defer）
// ---------------------------------------------------------------------------

// TestAuditInFlightLeakWhenAttemptPanics attempt 内部 panic 之后在途名额必须被释放。
//
// 曾经的缺陷：`release()` 是顺序调用，panic 直接跳过它 → 这一号的在途名额永久占住，
// 该号此后再也选不中（整池可用性逐次塌）。现在 release 走 defer，panic 也放得掉。
func TestAuditInFlightLeakWhenAttemptPanics(t *testing.T) {
	// 假上游在「换票」这一步 panic：等价于 attempt 内部（上游客户端/后续新增代码）任何一次 panic。
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			panic("audit: 模拟 attempt 内部 panic")
		})},
		AgentHost: "https://fake.example", UgHost: "https://fake.example",
		OAuthHost: "https://fake.example", ClientID: upstream.ClientID,
	}
	// ExpiresAt=0 → NeedsRefresh 恒真，于是会真的走网络（被上面的 panic 拦住）。
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", RefreshToken: "rt1", ExpiresAt: 0})
	p.ApplyLimits(pool.Limits{MaxInFlight: 1})
	h := NewHandler(Config{Pool: p, Upstream: up, RequestLog: reqlog.New(reqlog.Config{})})

	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`
	first := auditPost(h, "/v1/chat/completions", body)
	st, _ := p.Status("u1")
	t.Logf("panic 请求: code=%d；panic 后池状态 in_flight=%d cooling=%v",
		first.Code, st.InFlight, st.Cooling)
	if st.InFlight != 0 {
		t.Fatalf("panic 后名额没释放（回归）：in_flight=%d", st.InFlight)
	}
	second := auditPost(h, "/v1/chat/completions", body)
	t.Logf("随后一次正常请求: code=%d body=%s", second.Code, strings.TrimSpace(second.Body.String()))
	// 假上游对每次请求都 panic，所以这一次也必然失败——但**不能是 503「号都不可用」**：
	// 那才说明名额真的漏了、该号再也选不中。失败原因应当是 panic 本身（500）。
	if second.Code == http.StatusServiceUnavailable {
		t.Fatalf("panic 之后该号已选不中（名额泄漏回归）: code=%d body=%s", second.Code, second.Body)
	}
}

// ---------------------------------------------------------------------------
// 4. 上游 4xx（调用方错误）：如实回上游状态码，且不罚号
// ---------------------------------------------------------------------------

func TestAuditUpstreamClientErrorBecomes503AndDegradesAccounts(t *testing.T) {
	var calls []string
	up := auditUpstream(&calls, func(string) (int, string, bool) {
		// Classify(400, 认不出的业务码) = ErrClient（调用方参数问题，与账号无关）。
		return 400, `{"code":11101,"message":"param is invalid"}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := auditPost(h, "/v1/chat/completions", `{"model":"glm-5.2","messages":[]}`)

	chats := len(chatCalls(calls))
	t.Logf("HTTP status=%d body=%s", rec.Code, strings.TrimSpace(rec.Body.String()))
	for _, st := range p.List() {
		t.Logf("账号 %s: degraded=%d cooling=%v", st.UID, st.Degraded, st.Cooling)
	}
	t.Logf("被打到上游的对话次数=%d", chats)

	// 修好后的行为：① 如实回上游的 400（不再谎报 503「号都不可用」）
	// ② 只打一次上游（调用方错换号没意义）③ 一个号都不记降权。
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("调用方 400 应如实回 400：code=%d body=%s", rec.Code, rec.Body)
	}
	if chats != 1 {
		t.Fatalf("调用方错误不该换号重试：chats=%d", chats)
	}
	for _, st := range p.List() {
		if st.Degraded != 0 {
			t.Fatalf("与账号无关的 4xx 不该记降权：%s degraded=%d", st.UID, st.Degraded)
		}
	}
}

// ---------------------------------------------------------------------------
// 5. 并行 tool_call 以 index 为唯一键：缺 index / 同 index 会被合并成一个 item
// ---------------------------------------------------------------------------

// auditToolDeltaSSE index 相同的两次工具调用增量（上游在缺 index 时两边都是 0）。
const auditToolDeltaSSE = `data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"p\":\"/a\""}}]}}]}` + "\n\n" +
	`data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_2","type":"function","function":{"name":"write_file","arguments":"}"}}]}}]}` + "\n\n" +
	`data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
	"data: [DONE]\n\n"

// auditSSEEvents 把 SSE 响应体拆成事件对象（只用来看结构，不做协议断言）。
func auditSSEEvents(body string) []map[string]any {
	var out []map[string]any
	for _, frame := range strings.Split(body, "\n\n") {
		for _, line := range strings.Split(frame, "\n") {
			payload, ok := strings.CutPrefix(strings.TrimSpace(line), "data: ")
			if !ok {
				continue
			}
			var obj map[string]any
			if json.Unmarshal([]byte(payload), &obj) == nil {
				out = append(out, obj)
			}
		}
	}
	return out
}

func TestAuditParallelToolCallsCollapseOnSameIndex(t *testing.T) {
	t.Run("responses", func(t *testing.T) {
		rec := httptest.NewRecorder()
		w := &responsesWriter{ResponseWriter: rec, stream: true}
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := w.Write([]byte(auditToolDeltaSSE)); err != nil {
			t.Fatal(err)
		}
		w.finish()
		body := rec.Body.String()

		opened := 0
		for _, ev := range auditSSEEvents(body) {
			item, _ := ev["item"].(map[string]any)
			if ev["type"] == "response.output_item.added" && item != nil && item["type"] == "function_call" {
				opened++
			}
		}
		t.Logf("客户端该拿到 2 个 function_call item；实际下发 output_item.added(function_call)=%d", opened)
		if opened != 2 {
			t.Fatalf("同 index 不同 id 的两次调用必须各成一个 item：added 数=%d", opened)
		}
		// 两次调用各自完整：read_file 拿到自己的 /a，write_file 拿到自己的 }（不再被拼在一起）。
		if !strings.Contains(body, `"call_id":"call_1"`) || !strings.Contains(body, `name":"read_file"`) ||
			!strings.Contains(body, `"delta":"{\"p\":\"/a\""`) {
			t.Fatalf("第一次调用（read_file 拿自己的参数）不完整：%s", body)
		}
		if !strings.Contains(body, `"call_id":"call_2"`) || !strings.Contains(body, `name":"write_file"`) ||
			!strings.Contains(body, `"delta":"}"`) {
			t.Fatalf("第二次调用（write_file 拿 }）不完整：%s", body)
		}
		// 合并的老行为：一次调用把 {"p":"/a"} 整段拿走、名字变成最后一个。
		if strings.Contains(body, `"arguments":"{\"p\":\"/a\"}"`) {
			t.Fatalf("两次调用又被拼成一个完整参数：%s", body)
		}
	})

	t.Run("messages", func(t *testing.T) {
		rec := httptest.NewRecorder()
		w := &messagesWriter{ResponseWriter: rec, stream: true}
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := w.Write([]byte(auditToolDeltaSSE)); err != nil {
			t.Fatal(err)
		}
		w.finish()
		body := rec.Body.String()
		blocks := strings.Count(body, `"type":"tool_use"`)
		t.Logf("content_block_start(tool_use) 数量=%d", blocks)
		if blocks != 2 {
			t.Fatalf("同 index 不同 id 的两次调用必须各起一个 tool_use 块：块数=%d", blocks)
		}
	})

	// 对照组：index 互不相同时（index:0 / index:1）两次调用各成一个 item ——
	// 说明合并的唯一原因就是「以 index 为键」，而不是别的地方丢了调用。
	t.Run("responses-distinct-index", func(t *testing.T) {
		frames := strings.NewReplacer(`"index":0,"id":"call_2"`, `"index":1,"id":"call_2"`).Replace(auditToolDeltaSSE)
		rec := httptest.NewRecorder()
		w := &responsesWriter{ResponseWriter: rec, stream: true}
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := w.Write([]byte(frames)); err != nil {
			t.Fatal(err)
		}
		w.finish()
		opened := 0
		for _, ev := range auditSSEEvents(rec.Body.String()) {
			item, _ := ev["item"].(map[string]any)
			if ev["type"] == "response.output_item.added" && item != nil && item["type"] == "function_call" {
				opened++
			}
		}
		t.Logf("index 不同时 output_item.added(function_call)=%d", opened)
		if opened != 2 {
			t.Fatalf("对照组应得 2 个 item：%d", opened)
		}
	})
}

// ---------------------------------------------------------------------------
// 6. reqlog：一个打不开的归档文件让整次读盘失败（且证明 limit 不省 IO）
// ---------------------------------------------------------------------------

// TestAuditReadArchiveAbortsEverythingOnOneBadFile：read() 把所有 requests-*.jsonl
// 都打开一遍，limit 只管截断；任何**单个**文件打开失败（实测触发：prune 的 1s ticker
// 与面板读并发删同一个文件）都让 ReadArchive 直接返回 err，面板「运行日志」页整页 500。
// 这里用一个悬空符号链接稳定复现「文件打不开」这一步。
func TestAuditReadArchiveAbortsEverythingOnOneBadFile(t *testing.T) {
	dir := t.TempDir()
	r := reqlog.New(reqlog.Config{Dir: dir, Enabled: true, QueueSize: 8})
	r.Record(reqlog.Event{
		Time: time.Now(), RequestID: "req-audit-1", Path: "/v1/chat/completions",
		Status: 200, OK: true, Outcome: reqlog.OutcomeSuccess, DurationMs: 1,
	})
	r.Close() // 停机刷盘，保证最新一条真的在盘上

	bad := filepath.Join(dir, "requests-2000-01-01.jsonl")
	if err := os.Symlink(filepath.Join(dir, "已经没了的文件"), bad); err != nil {
		t.Fatal(err)
	}
	events, err := r.ReadArchive(1, reqlog.Filter{})
	t.Logf("ReadArchive(limit=1)：events=%d err=%v", len(events), err)
	if err == nil {
		t.Fatal("期望复现「单个归档文件打不开 → 整次读盘失败」")
	}
	if !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("期望是打开那个旧文件失败：%v", err)
	}
	t.Logf("→ limit=1 也照样打开了全部归档文件（否则不会碰到坏文件）；面板拿到 err 后整页 500")
}

// TestAuditRecordAfterCloseIsSwallowedSilently：Close 之后 Record 的事件既进不了盘，
// 也不计 dropped_writes —— 「队列满时丢弃并计数」的约定在停机后失效，归档缺口无声。
func TestAuditRecordAfterCloseIsSwallowedSilently(t *testing.T) {
	dir := t.TempDir()
	r := reqlog.New(reqlog.Config{Dir: dir, Enabled: true, QueueSize: 8})
	r.Record(reqlog.Event{
		Time: time.Now(), RequestID: "req-before-close", Path: "/v1/models",
		Status: 200, OK: true, Outcome: reqlog.OutcomeSuccess,
	})
	r.Close()
	r.Record(reqlog.Event{
		Time: time.Now(), RequestID: "req-after-close", Path: "/v1/models",
		Status: 200, OK: true, Outcome: reqlog.OutcomeSuccess,
	})
	r.Record(reqlog.Event{
		Time: time.Now(), RequestID: "req-after-close-2", Path: "/v1/models",
		Status: 200, OK: true, Outcome: reqlog.OutcomeSuccess,
	})

	stats := r.Snapshot().Archive
	events, err := r.ReadArchive(10, reqlog.Filter{})
	ids := make([]string, 0, len(events))
	for _, e := range events {
		ids = append(ids, e.RequestID)
	}
	t.Logf("归档: enabled=%v dropped_writes=%d last_error=%q", stats.Enabled, stats.DroppedWrites, stats.LastError)
	t.Logf("盘上事件: %v err=%v", ids, err)

	if err != nil {
		t.Fatalf("读归档不该出错: %v", err)
	}
	if len(events) != 1 || events[0].RequestID != "req-before-close" {
		t.Fatalf("期望复现「Close 后的事件无声消失」：%v", ids)
	}
	if stats.DroppedWrites != 0 {
		t.Fatalf("期望复现「丢了却不算 dropped」：%d", stats.DroppedWrites)
	}
}

// ---------------------------------------------------------------------------
// 8. 并发对照（已证伪方向）：混合路径 + 会话 + 台账 + 请求记录并发压一遍
// ---------------------------------------------------------------------------

// TestAuditConcurrentMixedPathsNoRace 并发跑 chat/responses/messages/models，
// 共享 session / usage / reqlog —— 用 -race 跑，用来证伪「共享状态有数据竞争」。
func TestAuditConcurrentMixedPathsNoRace(t *testing.T) {
	mkResp := func(status int, body string, stream bool) *http.Response {
		ct := "application/json"
		if stream {
			ct = "text/event-stream"
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{ct}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}
	}
	var chats atomic.Int64
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != upstream.EpChat {
				return mkResp(200, auditModelProbeBody, false), nil
			}
			chats.Add(1)
			return mkResp(200, soloSSE, true), nil
		})},
		AgentHost: "https://fake.example", UgHost: "https://fake.example",
		OAuthHost: "https://fake.example", ClientID: upstream.ClientID,
	}
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	sess := session.New(session.Config{Enabled: true, TTL: time.Minute, GCInterval: time.Minute})
	defer sess.Stop()
	rl := reqlog.New(reqlog.Config{})
	ledger := usage.New("")
	h := NewHandler(Config{
		Pool: p, Upstream: up, Session: sess, Usage: ledger,
		RequestLog: rl, RequestClientInfo: true,
	})

	reqs := []struct{ path, body string }{
		{"/v1/chat/completions", `{"model":"glm-5.2","stream":true,"conversation_id":"c-x","messages":[{"role":"user","content":"hi"}]}`},
		{"/v1/chat/completions", `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`},
		{"/v1/responses", `{"model":"glm-5.2","stream":true,"input":"hi","prompt_cache_key":"pc-1"}`},
		{"/v1/responses", `{"model":"glm-5.2","input":"hi"}`},
		{"/v1/messages", `{"model":"glm-5.2","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`},
		{"/v1/messages", `{"model":"glm-5.2","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`},
	}
	const rounds = 4
	for r := 0; r < rounds; r++ {
		var wg sync.WaitGroup
		for _, q := range reqs {
			wg.Add(1)
			go func(path, body string) {
				defer wg.Done()
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader(body)))
				if rec.Code != http.StatusOK {
					t.Errorf("%s code=%d body=%s", path, rec.Code, rec.Body)
				}
			}(q.path, q.body)
		}
		wg.Wait()
	}
	// 顺带把面板侧的读路径也压一下（与落盘并发）。
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = rl.Snapshot()
			_, _ = rl.ReadArchive(10, reqlog.Filter{})
		}()
	}
	wg.Wait()

	snap := rl.Snapshot()
	t.Logf("并发 %d 次混合请求：reqlog completed=%d（期望 %d）chats=%d 会话绑定=%d",
		rounds*len(reqs), snap.Completed, rounds*len(reqs), chats.Load(), sess.Count())
	t.Logf("台账: requests=%d errors=%d",
		ledger.Snapshot(72, nil).Totals.Requests, ledger.Snapshot(72, nil).Totals.Errors)
	if got, want := snap.Completed, int64(rounds*len(reqs)); got != want {
		t.Fatalf("请求记录条数=%d，期望 %d", got, want)
	}
	if snap.InFlight != 0 {
		t.Fatalf("在途未归零：%d", snap.InFlight)
	}
}

// ---------------------------------------------------------------------------
// 7. 会话粘性键：派生键只吃 (system + 首条 user)，不同会话会撞同一个键
// ---------------------------------------------------------------------------

// TestAuditDerivedSessionKeyCollidesAcrossConversations：客户端固定 system 提示词 +
// 首条用户消息相同（同一个 CLI 的两次会话很常见）时，两次**不同**的会话派生出同一个
// 键 —— 它们会被粘到同一个账号（互相排队，且不再轮换）。
func TestAuditDerivedSessionKeyCollidesAcrossConversations(t *testing.T) {
	convA := `{"model":"glm-5.2","messages":[
		{"role":"system","content":"You are a coding agent."},
		{"role":"user","content":"继续"},
		{"role":"assistant","content":"上一次的答案 A"},
		{"role":"user","content":"改成 B 方案"}]}`
	convB := `{"model":"glm-5.2","messages":[
		{"role":"system","content":"You are a coding agent."},
		{"role":"user","content":"继续"},
		{"role":"assistant","content":"完全不同的上下文"},
		{"role":"user","content":"帮我改个 bug"}]}`
	ka, kb := session.ExtractKey([]byte(convA)), session.ExtractKey([]byte(convB))
	t.Logf("会话 A 派生键=%s", ka)
	t.Logf("会话 B 派生键=%s", kb)
	if ka == "" || ka != kb {
		t.Fatalf("期望复现「不同会话同一个键」：%q vs %q", ka, kb)
	}
}
