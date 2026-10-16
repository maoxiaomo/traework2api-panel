//go:build audit

// audit_delta_server_test.go 只读审计（增量 6188638..436b0df）的证明测试：
// 断言的是**当前**行为，全部走 httptest + 假上游，不碰真实网络/凭据。
//
// 跑法：go test -tags audit -count=1 -run TestAuditDelta -v ./internal/server/
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"traework2api/internal/auth"
	"traework2api/internal/upstream"
)

// auditDeltaWire 发一次任意入站路径的请求，返回假上游收到的出站报文（改写之后的那份）。
func auditDeltaWire(t *testing.T, path, body string) map[string]any {
	t.Helper()
	var captured map[string]any
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &captured)
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(soloSSE)),
			}, nil
		})},
		AgentHost: "https://fake.example", UgHost: "https://fake.example",
		OAuthHost: "https://fake.example", ClientID: upstream.ClientID,
	}
	h := NewHandler(Config{
		Pool:        testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:    up,
		MaxFunction: DefaultMaxFunction,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s 入站失败: code=%d body=%s", path, rec.Code, rec.Body)
	}
	if captured == nil {
		t.Fatalf("%s: 假上游没收到请求，出站报文无从断言", path)
	}
	return captured
}

// TestAuditDeltaMaxContextFieldDroppedOnMessagesAndResponses：
// handler.go 新增的 `max_context:true` 开法只在 /v1/chat/completions 生效 ——
// /v1/messages 与 /v1/responses 的适配层（messages.go:66 / responses.go:50 的键白名单）
// 不转发 max_context，字段在翻译时被静默丢掉，客户端拿到的是 **dev 档窗口**
// （上游对带 __max 名的请求才给大窗口），而 /v1/models 对三个面都播报 max_available。
//
// 同一份 handler 的注释把两种开法写成「等价」（handler.go:640-643、674-675），
// 所以这是新增契约在两个入站面上不成立，且不报错、不写日志（WARN 也不会打，
// 因为 maxOn 压根没被置起来）。
func TestAuditDeltaMaxContextFieldDroppedOnMessagesAndResponses(t *testing.T) {
	installCatalog(t, maxModel())

	cases := []struct{ path, body string }{
		{"/v1/chat/completions", `{"model":"glm-5.2","stream":true,"max_context":true,"messages":[{"role":"user","content":"hi"}]}`},
		{"/v1/messages", `{"model":"glm-5.2","stream":true,"max_tokens":16,"max_context":true,"messages":[{"role":"user","content":"hi"}]}`},
		{"/v1/responses", `{"model":"glm-5.2","stream":true,"max_context":true,"input":"hi"}`},
	}
	// 修好后的行为：三个入站面都认 max_context（以前只有 chat/completions 认，
	// messages/responses 的键白名单把它静默吃掉，客户端以为开了大窗口实际拿 dev 档）。
	want := map[string]bool{
		"/v1/chat/completions": true,
		"/v1/messages":         true,
		"/v1/responses":        true,
	}
	for _, c := range cases {
		got := auditDeltaWire(t, c.path, c.body)
		maxOn := got["model"] == "glm-5.2__max" && got["function"] == DefaultMaxFunction
		t.Logf("%-20s max_context:true → 出站 model=%v function=%v（Max 生效=%v）",
			c.path, got["model"], got["function"], maxOn)
		if maxOn != want[c.path] {
			t.Fatalf("%s 的当前行为变了（期望 Max 生效=%v，实得 %v）—— 本条缺陷或被修掉，请更新报告",
				c.path, want[c.path], maxOn)
		}
	}

	// 对照组：**模型名后缀**那种开法三个面都生效（说明丢的确实只有请求体字段，
	// 不是「这两个面整体不支持 Max」）。
	got := auditDeltaWire(t, "/v1/messages",
		`{"model":"glm-5.2__max","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	t.Logf("/v1/messages 模型名带 __max → 出站 model=%v function=%v", got["model"], got["function"])
	if got["model"] != "glm-5.2__max" || got["function"] != DefaultMaxFunction {
		t.Fatalf("后缀开法在 /v1/messages 上也失效了: %v", got)
	}
}

// TestAuditDeltaMaxContextTypeMismatchKeepsOtherFields：反证（期望为真，即缺陷不成立）——
// 新增的 chatRequest.MaxContext 会不会因为类型不符把整份 peek 清零、把模型静默换成默认模型？
// encoding/json 遇到类型不符只跳过该字段并继续填其它字段，所以 model/stream 照旧解析出来。
func TestAuditDeltaMaxContextTypeMismatchKeepsOtherFields(t *testing.T) {
	for _, body := range []string{
		`{"model":"glm-5.2","stream":true,"max_context":"yes"}`,
		`{"model":"glm-5.2","stream":true,"max_context":1}`,
		`{"model":"glm-5.2","stream":true,"max_context":{"on":true}}`,
	} {
		var peek chatRequest
		err := json.Unmarshal([]byte(body), &peek)
		t.Logf("%s → err=%v model=%q stream=%v max_on=%v", body, err, peek.Model, peek.Stream, peek.MaxContext)
		if peek.Model != "glm-5.2" || !peek.Stream {
			t.Fatalf("类型不符把整份 peek 打坏了（模型会被换成默认档）: %+v", peek)
		}
	}
}
