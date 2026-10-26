package upstream

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// 首包门（照 agent2api forward.rs 的 prefetch_head）：上游的额度/凭证失败是
// HTTP 200 + 流内 event:error。落在第一帧、且一个字节都还没写给客户端时，
// 这帧错误必须**交回调用方换号重试**，而不是写成一次「成功但报错」的答复。
func TestStreamFirstFrameAccountErrorNotWritten(t *testing.T) {
	const raw = "event:error\ndata:{\"code\":1005,\"message\":\"plan limit\"}\n\n"
	rr := httptest.NewRecorder()
	var seen []*SOLOStreamError
	_, err := StreamWithError(rr, strings.NewReader(raw), func(se *SOLOStreamError) {
		seen = append(seen, se)
	}, nil)
	if err == nil {
		t.Fatalf("首帧账号级错误应返回给调用方，实得 nil；body=%q", rr.Body.String())
	}
	se, ok := err.(*SOLOStreamError)
	if !ok || se.Code != 1005 {
		t.Fatalf("err=%#v，want *SOLOStreamError code=1005", err)
	}
	if rr.Body.Len() != 0 {
		t.Errorf("首包门不该写任何字节（否则调用方已提交 event-stream，不能再换号）: %q", rr.Body.String())
	}
	if len(seen) != 1 {
		t.Errorf("onErr 仍要回调一次（调用方据此罚号）: %d", len(seen))
	}
}

// 请求级错误（4001 参数 / 4026 上下文超长）不换号：换几个号都是同一个答复，
// 只会白打上游。这类仍按今天的形状下发 error 事件 + [DONE]。
func TestStreamFirstFrameRequestErrorStillWritten(t *testing.T) {
	const raw = "event:error\ndata:{\"code\":4001,\"message\":\"the param is invalid\"}\n\n"
	rr := httptest.NewRecorder()
	_, err := StreamWithError(rr, strings.NewReader(raw), nil, nil)
	if err != nil {
		t.Fatalf("请求级错误不该交回调用方换号: %v", err)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "event: error") || !strings.Contains(body, "data: [DONE]") {
		t.Errorf("请求级错误要照常下发 error 事件: %q", body)
	}
}

// 已写过字节（正文先到）之后再来的 error 仍按现状收尾：此时换号会让客户端看到
// 两条互相矛盾的回复，只能把错误追加在同一条流里。
func TestStreamErrorAfterOutputStillWritten(t *testing.T) {
	const raw = "event:output\ndata:{\"response\":\"AAA\"}\n\n" +
		"event:error\ndata:{\"code\":1005,\"message\":\"plan limit\"}\n\n"
	rr := httptest.NewRecorder()
	_, err := StreamWithError(rr, strings.NewReader(raw), nil, nil)
	if err != nil {
		t.Fatalf("已提交的流不能再交回换号: %v", err)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "AAA") || !strings.Contains(body, "event: error") {
		t.Errorf("body=%q want 正文 + error 事件", body)
	}
}
