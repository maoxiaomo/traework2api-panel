package durable

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validJSON(b []byte) bool { return json.Valid(b) }

// 断电留下的全长零字节文件是最常见的坏法：必须回落到上一代，不能当成空。
func TestReadFallsBackToBak(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	good := `{"accounts":{"u1":{"disabled":true}}}`
	if err := os.WriteFile(Bak(p), []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, 64), 0o600); err != nil { // 64 字节全零
		t.Fatal(err)
	}
	raw, note, err := Read(p, validJSON)
	if err != nil {
		t.Fatalf("有可用上一代时不该报错: %v", err)
	}
	if string(raw) != good {
		t.Errorf("应拿到上一代内容，实得 %q", raw)
	}
	if !strings.Contains(note, "全是零") {
		t.Errorf("note 要说清坏在哪，实得 %q", note)
	}
}

// 两边都坏：必须是错误，不是空内容——调用方据此决定「备份现场后空启动」。
func TestReadBothBadIsError(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	_ = os.WriteFile(p, []byte(`{"a":`), 0o600)
	_ = os.WriteFile(Bak(p), []byte(`{`), 0o600)
	if _, _, err := Read(p, validJSON); err == nil {
		t.Fatal("两边都坏应报错")
	}
}

// 文件不存在就报错，不回看 .bak（不存在的文件是被有意删掉的）。
func TestReadMissingDoesNotUseBak(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	_ = os.WriteFile(Bak(p), []byte(`{}`), 0o600)
	if _, _, err := Read(p, validJSON); !os.IsNotExist(err) {
		t.Fatalf("应报 NotExist，实得 %v", err)
	}
}

// Keep：留下现在这份；当前坏时**不动** .bak（否则一次断电会盖掉唯一的好副本）。
func TestKeep(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	good := `{"gen":1}`
	_ = os.WriteFile(p, []byte(good), 0o600)
	if err := Keep(p, validJSON); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(Bak(p)); string(b) != good {
		t.Fatalf(".bak=%q", b)
	}
	// 主文件坏了：.bak 必须保持上一代好内容
	_ = os.WriteFile(p, make([]byte, 8), 0o600)
	if err := Keep(p, validJSON); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(Bak(p)); string(b) != good {
		t.Fatalf("坏文件不该覆盖好副本，.bak=%q", b)
	}
	// 首次保存（文件不存在）不报错
	_ = os.Remove(p)
	if err := Keep(p, validJSON); err != nil {
		t.Fatalf("文件不存在应返回 nil: %v", err)
	}
}
