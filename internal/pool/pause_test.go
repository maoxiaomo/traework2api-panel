package pool

import (
	"testing"
	"time"

	"traework2api/internal/auth"
)

// TestPauseOnlyBlocksPick 暂停选号只挡 Pick，不动冷却/禁用字段；Unpause 恢复。
func TestPauseOnlyBlocksPick(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 100)

	if !p.Pause("u1", "test") {
		t.Fatal("Pause 应成功")
	}
	// 选号应只返回 u2（u1 已暂停）
	for i := 0; i < 20; i++ {
		a := p.Pick()
		if a == nil || a.UID != "u2" {
			t.Fatalf("paused 号被选中: %+v", a)
		}
		p.Release("u2")
	}
	// Status 透出
	st, ok := p.Status("u1")
	if !ok || !st.Paused {
		t.Fatalf("Status.Paused=%v ok=%v", st.Paused, ok)
	}
	if !p.Unpause("u1") {
		t.Fatal("Unpause 应成功")
	}
	// 恢复后能被选中
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		a := p.Pick()
		if a != nil {
			seen[a.UID] = true
			p.Release(a.UID)
		}
	}
	if !seen["u1"] {
		t.Fatal("Unpause 后 u1 仍未被选中")
	}
	_ = time.Now // 占位，防误删 import
}
