package upstream

// agent2api_borrow_test.go 从 wocaoniubi123/agent2api 借来的两处修正的回归（见 solosse.go errSent、
// client.go msField）。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"traework2api/internal/auth"
)

// 上游「流内 error 之后再补一个 done」的形状：error 分支自己已经写过 [DONE]，
// 后面的 done 不能再写第二套收尾帧（客户端会收到两个 [DONE] + 一个多余的 finish chunk）。
func TestStreamErrorThenDoneSingleTerminator(t *testing.T) {
	raw := "event:output\ndata:{\"response\":\"半截\"}\n\n" +
		"event:error\ndata:{\"code\":4008,\"message\":\"流的调用错误\"}\n\n" +
		"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n"
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	if n := strings.Count(body, "[DONE]"); n != 1 {
		t.Errorf("[DONE] 应只有 1 个，实得 %d: %q", n, body)
	}
	if strings.Count(body, `"finish_reason"`) != 0 {
		t.Errorf("error 之后不该再出 finish chunk: %q", body)
	}
}

// 换票响应里 TokenExpireAt 是字符串时，不能把整条响应判成解析失败（否则票不落盘、
// 连续 401 后账号被判失效）。
func TestRefreshTokenExchangeStringExpiry(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"Result":{"Token":"newat","RefreshToken":"newrt","TokenExpireAt":"1786847930141"}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "oldrt", ApiHost: "https://oauth.example"}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("字符串过期时间应照常换票: %v", err)
	}
	if a.AccessToken != "newat" || a.ExpiresAt != 1786847930 {
		t.Errorf("token=%q expiresAt=%d", a.AccessToken, a.ExpiresAt)
	}
}
