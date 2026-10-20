package auth

import (
	"os"
	"path/filepath"
	"testing"
)

// 坏掉的凭证文件不能让这个号从池里消失：上一代 .bak 还在，就该从它把凭证捞回来。
func TestLoadDirFallsBackToBak(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "trae-u1.json")
	a := &Auth{
		AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1786805537,
		Domain: "trae.cn", ApiHost: "https://api.trae.com.cn",
		UID: "u1", Nickname: "n1", FilePath: fp,
	}
	if err := a.SaveAtomic(); err != nil { // 第一次：还没有 .bak
		t.Fatal(err)
	}
	if err := a.SaveAtomic(); err != nil { // 第二次：上一代进 .bak
		t.Fatal(err)
	}
	if _, err := os.Stat(fp + ".bak"); err != nil {
		t.Fatalf("写两次后应有 .bak: %v", err)
	}
	// 模拟断电：主文件全长零字节
	if err := os.WriteFile(fp, make([]byte, 128), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].UID != "u1" || got[0].RefreshTokenValue() != "rt" {
		t.Fatalf("坏文件应回落到 .bak，实得 %d 个: %+v", len(got), got)
	}
}
