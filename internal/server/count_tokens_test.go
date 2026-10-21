package server

// count_tokens 端点：Claude Code 每轮问一次上下文预算，上游没有对应端点。
// 这条只做本地估算，不碰账号池、不花额度。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"traework2api/internal/auth"
)

func TestCountTokensEndpoint(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		t.Error("count_tokens 不该打到上游")
		return 500, "", false
	})
	h := NewHandler(Config{APIKey: "k", Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1"}), Upstream: up})

	post := func(body string, hdr http.Header) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(body))
		for k, vs := range hdr {
			req.Header[k] = vs
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// 鉴权和 /v1/messages 一致：x-api-key 也算。
	if rec := post(`{"model":"glm-5.2","messages":[]}`, http.Header{}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("无 key 应 401，实得 %d", rec.Code)
	}
	rec := post(`{"model":"glm-5.2","messages":[{"role":"user","content":"你好 world"}]}`,
		http.Header{"X-Api-Key": []string{"k"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var got struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 {input_tokens:N}: %s", rec.Body)
	}
	// 两个汉字 3 + " world" 6 字符 1.5 → 约 4~5；口径宽松，只要求落在这个量级。
	if got.InputTokens < 3 || got.InputTokens > 12 {
		t.Fatalf("input_tokens=%d 偏离量级（正文很少）", got.InputTokens)
	}

	// 路径别名 /messages/count_tokens 同一条。
	req := httptest.NewRequest("POST", "/messages/count_tokens",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer k")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("/messages/count_tokens code=%d", rec2.Code)
	}
}

func TestEstimateChatTokens(t *testing.T) {
	// 纯 ASCII 与纯中文的长度口径不同：4 个 ASCII 字符≈1 token，1 个汉字≈1.5。
	ascii := estimateChatTokens([]byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("a", 400) + `"}]}`))
	cjk := estimateChatTokens([]byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("字", 100) + `"}]}`))
	if ascii < 90 || ascii > 130 {
		t.Fatalf("400 个 ASCII 字符应≈100 token，实得 %d", ascii)
	}
	if cjk < 140 || cjk > 180 {
		t.Fatalf("100 个汉字应≈150 token，实得 %d", cjk)
	}

	// base64 图片按 1000 计，不能按字符数计（100KB 的图会算出几万 token）。
	img := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` +
		strings.Repeat("A", 100000) + `"}}]}]}`
	if n := estimateChatTokens([]byte(img)); n < 990 || n > 1100 {
		t.Fatalf("图片应≈1000 token，实得 %d", n)
	}

	// 空请求也返回正数：Anthropic 语义里 input_tokens 是下限 1。
	if n := estimateChatTokens([]byte(`{}`)); n != 1 {
		t.Fatalf("空请求=%d 应为 1", n)
	}
}
