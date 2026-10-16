//go:build audit

// 只读审计用的「证明测试」：断言的是**当前**（有缺陷的）行为，不是期望行为。
// 仅在 `go test -tags audit` 下编译运行；全部只用 t.TempDir()，不碰真实 auths/。
package auth

import (
	"os"
	"path/filepath"
	"testing"
)

// A-1 LoadDir 用 filepath.Glob 拼路径：目录名里带 glob 元字符（[ ] * ?）时
// Glob 返回空且 err == nil —— 一个账号都扫不到，调用方只看到「loaded 0 account(s)」，
// 池子空转（对话全 503）。同一目录用 os.ReadDir 是能读到的。
func TestAuditLoadDirGlobMetacharacterYieldsNoAccounts(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "auths[1]")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw := `{"account":{"uid":"u1"},"auth":{"accessToken":"at","refreshToken":"rt"}}`
	if err := os.WriteFile(filepath.Join(dir, "trae-u1.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	as, err := LoadDir(dir)
	t.Logf("LoadDir(%q) → %d 个账号, err=%v", dir, len(as), err)
	if err != nil || len(as) != 1 {
		t.Fatalf("带 [ ] 的目录名也必须正常读出账号（不能当 Glob 模式）: n=%d err=%v", len(as), err)
	}
}

// A-2 Parse 只要求 accessToken 非空：一个 accessToken 被清空（或从来没写过）但
// refreshToken 完好的文件被整体判无效，LoadDir 静默跳过 → 该账号从面板消失，
// 而它本可以靠 refreshToken 自助换票回来。
func TestAuditParseRejectsUsableRefreshTokenOnlyFile(t *testing.T) {
	dir := t.TempDir()
	raw := `{"account":{"uid":"u1"},"auth":{"accessToken":"","refreshToken":"rt-good"}}`
	if err := os.WriteFile(filepath.Join(dir, "trae-u1.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	as, err := LoadDir(dir)
	t.Logf("只有 refreshToken 的文件 → LoadDir 返回 %d 个账号, err=%v", len(as), err)
	// 这种账号是能救回来的（签到/保活用 refreshToken 换 access token），不该被静默丢掉。
	if err != nil || len(as) != 1 || as[0].UID != "u1" {
		t.Fatalf("只含 refreshToken 的账号应被载入: n=%d err=%v", len(as), err)
	}
	if _, perr := Parse([]byte(raw)); perr != nil {
		t.Fatalf("Parse 应接受只含 refreshToken 的文件: %v", perr)
	}
	// 两个都没有才是无效。
	if _, perr := Parse([]byte(`{"account":{"uid":"u2"},"auth":{}}`)); perr == nil {
		t.Fatal("无 accessToken 也无 refreshToken 的文件应判无效")
	}
}
