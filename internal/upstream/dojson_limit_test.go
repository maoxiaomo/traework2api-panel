package upstream

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// chunkedErrReader 读到一半报错，模拟连接被掐（上游发大 body 时最常见）。
type chunkedErrReader struct{ sent bool }

func (r *chunkedErrReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, `{"a":1`), nil
	}
	return 0, errors.New("connection reset by peer")
}

// TestDoJSONRejectsOversizeExplicitly 超上限要报「超过 N 字节」，不能把半截 body 丢给
// json.Unmarshal——那只会得到 "unexpected end of JSON input"，分不清是截断还是上游坏了。
func TestDoJSONRejectsOversizeExplicitly(t *testing.T) {
	big := strings.Repeat("x", maxJSONBody+16)
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(big))}, nil
	})
	req, _ := http.NewRequest("POST", "https://ug.example/x", nil)
	_, err := c.doJSON(req)
	if err == nil || !strings.Contains(err.Error(), "字节上限") {
		t.Fatalf("超限应显式报错，实得 %v", err)
	}
}

// TestDoJSONReportsReadInterrupt 读取中断同样要如实报，不能当成坏 JSON。
func TestDoJSONReportsReadInterrupt(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(&chunkedErrReader{})}, nil
	})
	req, _ := http.NewRequest("POST", "https://ug.example/x", nil)
	_, err := c.doJSON(req)
	if err == nil || !strings.Contains(err.Error(), "中断") {
		t.Fatalf("读取中断应显式报错，实得 %v", err)
	}
}

// TestDoJSONNormalBody 正常 body 不受影响（守住上面两条改动的边界）。
func TestDoJSONNormalBody(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"ok":true}`), nil
	})
	req, _ := http.NewRequest("POST", "https://ug.example/x", nil)
	raw, err := c.doJSON(req)
	if err != nil || string(raw) != `{"ok":true}` {
		t.Fatalf("raw=%s err=%v", raw, err)
	}
}
