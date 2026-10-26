package server

// 观测面：声明了 tools 的回合要把「声明数 / 实际调用数」落进请求记录。
// 两个数一起才判得出「模型停摆」——声明了工具、上游却正常 stop 且零 tool_calls，
// 客户端看到的就是一次什么都没做的回合，而归档里它与正常收尾逐字节相同。
// 反例：没声明 tools 的纯聊天两个字段保持零值，不进分母。

import (
	"net/http/httptest"
	"strings"
	"testing"

	"traework2api/internal/auth"
	"traework2api/internal/reqlog"
)

const toolsBody = `"tools":[{"type":"function","function":{"name":"exec_command","parameters":{"type":"object"}}}]`

// 上游真的调了工具（首帧带 name/id，后两帧是参数分片）。
const toolSSE = "event:output\ndata:{\"tool_calls\":[{\"function\":{\"arguments\":\"\",\"name\":\"get_weather\"},\"id\":\"call_1\",\"index\":0,\"type\":\"function\"}]}\n\n" +
	"event:output\ndata:{\"tool_calls\":[{\"function\":{\"arguments\":\"{}\",\"name\":\"\"},\"id\":\"\",\"index\":0}]}\n\n" +
	"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n"

func toolsSurfaceHandler(t *testing.T, sse string) *Handler {
	t.Helper()
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sse, true })
	return NewHandler(Config{
		Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:   up,
		RequestLog: reqlog.New(reqlog.Config{}),
	})
}

func TestRequestLogRecordsToolsSurface(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		sse      string
		declared int
		calls    int
	}{
		{"流式：声明了 tools 且真调了工具", `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"查天气"}],` + toolsBody + `}`, toolSSE, 1, 1},
		{"流式：声明了 tools 但零工具调用收尾", `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"查天气"}],` + toolsBody + `}`, soloSSE, 1, 0},
		{"非流式：声明了 tools 但零工具调用收尾", `{"model":"glm-5.2","messages":[{"role":"user","content":"查天气"}],` + toolsBody + `}`, soloSSE, 1, 0},
		{"纯聊天：没声明 tools", `{"model":"glm-5.2","messages":[{"role":"user","content":"在吗"}]}`, soloSSE, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := toolsSurfaceHandler(t, tc.sse)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(tc.body)))
			if rec.Code != 200 {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
			}
			ev := h.cfg.RequestLog.Snapshot().Recent
			if len(ev) != 1 {
				t.Fatalf("recent=%+v", ev)
			}
			if ev[0].ToolsDeclared != tc.declared || ev[0].ToolCalls != tc.calls {
				t.Fatalf("tools_declared=%d tool_calls=%d, want %d/%d",
					ev[0].ToolsDeclared, ev[0].ToolCalls, tc.declared, tc.calls)
			}
		})
	}
}

// 声明数要在请求失败时也留下：这里模型名未知 → 400，但「这轮本来要用工具」得记着。
func TestRequestLogKeepsToolsDeclaredOnFailure(t *testing.T) {
	h := toolsSurfaceHandler(t, soloSSE)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"no-such-model","messages":[],`+toolsBody+`}`)))
	if rec.Code != 400 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	ev := h.cfg.RequestLog.Snapshot().Recent
	if len(ev) != 1 || ev[0].ToolsDeclared != 1 || ev[0].ToolCalls != 0 {
		t.Fatalf("recent=%+v", ev)
	}
}

// 非流式的计数要认两种组包形态：Aggregate 自己组的是 []map[string]any，
// 直解上游 JSON 是 []any。只认一种会让这条路径恒记 0（线上冒烟就是这么发现的）。
func TestToolCallCountBothShapes(t *testing.T) {
	resp := func(tcs any) map[string]any {
		return map[string]any{"choices": []any{map[string]any{
			"message": map[string]any{"tool_calls": tcs},
		}}}
	}
	if n := toolCallCount(resp([]any{map[string]any{"id": "a"}, map[string]any{"id": "b"}})); n != 2 {
		t.Fatalf("[]any: n=%d, want 2", n)
	}
	if n := toolCallCount(resp([]map[string]any{{"id": "a"}})); n != 1 {
		t.Fatalf("[]map[string]any: n=%d, want 1", n)
	}
	if n := toolCallCount(map[string]any{}); n != 0 {
		t.Fatalf("空响应: n=%d, want 0", n)
	}
}
