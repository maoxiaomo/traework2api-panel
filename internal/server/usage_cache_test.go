package server

// issue #3：/v1/chat/completions 的 usage 必须带 OpenAI 兼容层的缓存字段，
// 否则上游明明命中缓存（cache_read_input_tokens），客户端一律读成 0。

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"traework2api/internal/auth"
)

const cacheUsageSSE = "event:output\ndata:{\"response\":\"你好\"}\n\n" +
	"event:token_usage\ndata:{\"prompt_tokens\":4387,\"completion_tokens\":73,\"total_tokens\":4460," +
	"\"reasoning_tokens\":71,\"cache_read_input_tokens\":4352,\"cache_creation_input_tokens\":0}\n\n" +
	"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n"

func chatWithSSE(t *testing.T, sse, body string) *httptest.ResponseRecorder {
	t.Helper()
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sse, true })
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	return rec
}

func usageOfBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var resp struct {
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("非流式响应不是 JSON: %s", body)
	}
	if resp.Usage == nil {
		t.Fatalf("响应没有 usage: %s", body)
	}
	return resp.Usage
}

func nestedNum(t *testing.T, u map[string]any, section, key string) float64 {
	t.Helper()
	d, _ := u[section].(map[string]any)
	if d == nil {
		t.Fatalf("usage 缺 %s: %v", section, u)
	}
	n, ok := d[key].(float64)
	if !ok {
		t.Fatalf("%s 缺 %s: %v", section, key, d)
	}
	return n
}

func TestChatUsageMirrorsCachedTokens(t *testing.T) {
	// 非流式：客户端读的是嵌套 details。
	u := usageOfBody(t, chatWithSSE(t, cacheUsageSSE, `{"model":"glm-5.2","messages":[]}`).Body.Bytes())
	if got := nestedNum(t, u, "prompt_tokens_details", "cached_tokens"); got != 4352 {
		t.Errorf("prompt_tokens_details.cached_tokens=%v want 4352", got)
	}
	if got := nestedNum(t, u, "completion_tokens_details", "reasoning_tokens"); got != 71 {
		t.Errorf("completion_tokens_details.reasoning_tokens=%v want 71", got)
	}
	for _, k := range []string{"cached_tokens", "prompt_cache_hit_tokens"} {
		if u[k] != float64(4352) {
			t.Errorf("顶层 %s=%v want 4352", k, u[k])
		}
	}
	// 原有字段不能少：Anthropic 那条路和用量台账都还按老名字读。
	if u["cache_read_input_tokens"] != float64(4352) {
		t.Errorf("cache_read_input_tokens 被改坏了: %v", u)
	}

	// 流式：usage 挂在某一帧上，同样要有嵌套字段。
	body := chatWithSSE(t, cacheUsageSSE, `{"model":"glm-5.2","stream":true,"messages":[]}`).Body.String()
	for _, want := range []string{`"prompt_tokens_details":{"cached_tokens":4352}`,
		`"completion_tokens_details":{"reasoning_tokens":71}`, `"cached_tokens":4352`} {
		if !strings.Contains(body, want) {
			t.Errorf("流式缺少 %s\n%s", want, body)
		}
	}

	// 上游报了 0（量过、没命中）也要照写：缺字段会被读成「上游没量」。
	miss := strings.Replace(cacheUsageSSE, `"cache_read_input_tokens":4352`, `"cache_read_input_tokens":0`, 1)
	u = usageOfBody(t, chatWithSSE(t, miss, `{"model":"glm-5.2","messages":[]}`).Body.Bytes())
	if got := nestedNum(t, u, "prompt_tokens_details", "cached_tokens"); got != 0 {
		t.Errorf("缓存未命中应报 0，实得 %v", got)
	}
}
