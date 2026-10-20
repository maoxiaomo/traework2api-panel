// Package durable 读/写「坏了就别当成空」的 JSON 文件。
//
// 起因（借鉴 yetone/magpie 的 internal/lastgood）：断电/掉盘会把文件留下全长零字节，
// 解析失败的一方此前只有两条路——当成空，或者备份现场后空启动。当空的后果是真丢数据：
// 账号文件读不出来 = 这个号从池里消失；状态文件读不出来 = 禁用/冷却/积分全清零。
//
// 这里只加一件事：写之前把现在这份好文件留成 <path>.bak，读不出来时回落到它。
package durable

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

// Bak 是 path 上一代好副本的位置。
func Bak(path string) string { return path + ".bak" }

// Read 读 path；内容不合法时回落到 path.bak，note 非空说明回落了什么（给日志用）。
//
// 文件不存在直接报 fs.ErrNotExist（不回看 .bak：rename 只会让文件是旧的或新的，
// 不会凭空消失，不存在的就是被有意删掉了）。两边都读不出来才返回错误——
// 「读不出来」是未知，不是空，调用方不该把它当空处理。
func Read(path string, valid func([]byte) bool) (data []byte, note string, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	if valid(b) {
		return b, "", nil
	}
	why := describe(b)
	if bak, berr := os.ReadFile(Bak(path)); berr == nil && valid(bak) {
		return bak, path + " " + why + "，已回落到上一代 " + Bak(path), nil
	}
	return nil, "", errors.New(path + " " + why + "，且没有可用的上一代副本")
}

// Keep 在 path 即将被替换前调用：现在这份合法的文件留成 path.bak。
//
// 当前文件不合法时**不动 .bak**——否则一次断电会把唯一的好副本也盖掉。
// 与已有 .bak 逐字节相同时不重写：状态文件每改一次冷却就会保存一次，没必要反复写盘。
// 文件不存在返回 nil（首次保存）。
func Keep(path string, valid func([]byte) bool) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !valid(b) {
		return nil
	}
	if old, err := os.ReadFile(Bak(path)); err == nil && bytes.Equal(old, b) {
		return nil
	}
	return os.WriteFile(Bak(path), b, 0o600)
}

// describe 说清坏在哪：日志里「0 字节」和「全长零」完全是两回事。
func describe(b []byte) string {
	switch {
	case len(b) == 0:
		return "是空文件"
	case len(bytes.Trim(b, "\x00")) == 0:
		return strconv.Itoa(len(b)) + " 字节全是零（断电/掉盘的特征）"
	case len(bytes.TrimSpace(b)) == 0:
		return "只有空白"
	}
	return "解析失败（" + strconv.Itoa(len(b)) + " 字节，开头 " +
		strconv.Quote(strings.ToValidUTF8(string(trunc(b)), "?")) + "）"
}

func trunc(b []byte) []byte {
	if len(b) > 40 {
		return b[:40]
	}
	return b
}
