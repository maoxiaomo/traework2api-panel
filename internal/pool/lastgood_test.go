package pool

import (
	"os"
	"path/filepath"
	"testing"

	"traework2api/internal/auth"
)

// 状态盘坏掉不该把「禁用/积分」清零：上一代 .bak 还在就按它恢复。
func TestLoadFallsBackToBak(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")

	p := New(fp)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Disable("u1", "手工禁用")
	p.SetCredits("u1", 4242) // 第二次保存 → 上一代进 .bak

	if _, err := os.Stat(fp + ".bak"); err != nil {
		t.Fatalf("保存两次后应有 .bak: %v", err)
	}
	// 模拟断电：主文件全长零字节
	if err := os.WriteFile(fp, make([]byte, 256), 0o600); err != nil {
		t.Fatal(err)
	}

	p2 := New(fp)
	st, ok := p2.Status("u1")
	if !ok {
		t.Fatal("坏状态盘应回落到 .bak，账号不该消失")
	}
	if !st.Disabled || st.Reason != "手工禁用" {
		t.Errorf("禁用标记应随上一代恢复，实得 disabled=%v reason=%q", st.Disabled, st.Reason)
	}
	// .bak 只往回一代：它保存的是最后一次覆盖**之前**的那份，所以上面那次 SetCredits
	// 不在里面（credits 为 0 是正常的）。挡的是「一次坏盘让池整个归零」，不是全部历史。
}
