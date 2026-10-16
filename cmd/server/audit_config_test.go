//go:build audit

// audit_config_test.go 只读审计的「证明测试」：断言**当前（有问题）**的行为。
// 全部用 t.TempDir() 里的临时配置 + t.Setenv（只管 TW2A_* 配置项，不涉及任何探测/网络环境变量）。
//
// 跑法：go test -tags audit -count=1 -run TestAudit -v ./cmd/server/
// 说明：这些用例在缺陷被修好后会失败 —— 那是设计意图（断言的是「缺陷存在」）。
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// auditWriteConfig 往临时目录落一份配置（用 json.Marshal 拼，避免路径转义问题）。
func auditWriteConfig(t *testing.T, cfg map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	fp := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(fp, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return fp
}

// TestAuditEmptyPromptFileAcceptedAsCustomPrompt：normalizePrompt（config.go:432-452）只在
// 提示词**不可读**时报错（prompt.Load 的 err 分支），可读但为空的文件一路放行 →
// cfg.PromptText=""。而 prompt.Rewrite 对空文本直接原样返回（internal/prompt/prompt.go:44-46），
// 于是「配置里写着 custom + 指定了文件」与实际行为（完全不改写）相反，配置侧不报错、
// 面板保存也返回 200 且不带 restart_required —— 运维看不出这条改写其实没生效。
func TestAuditEmptyPromptFileAcceptedAsCustomPrompt(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(empty, nil, 0o600); err != nil { // 0 字节（明确的「空」）
		t.Fatal(err)
	}
	blank := filepath.Join(dir, "blank.md")
	if err := os.WriteFile(blank, []byte("\n   \n"), 0o600); err != nil { // 只有空白：另一种形态
		t.Fatal(err)
	}
	fp := auditWriteConfig(t, map[string]any{
		"prompt": map[string]any{"mode": "custom", "file": empty},
	})
	c, err := Load(fp)
	t.Logf("Load(mode=custom, 0 字节文件) → err=%v", err)
	// 空提示词文件必须在加载时报错：否则 custom/append 会退化成静默不改写，而面板一路报成功。
	if err == nil {
		t.Fatalf("空提示词文件被接受了: Mode=%q PromptText=%q", c.Prompt.Mode, c.PromptText)
	}
	// 另一种形态：只含空白 —— 同样要报错。
	if _, err := Load(auditWriteConfig(t, map[string]any{
		"prompt": map[string]any{"mode": "custom", "file": blank},
	})); err == nil {
		t.Fatal("全空白提示词文件被接受了（同样会静默不改写）")
	}
	t.Log("空/全空白提示词文件在加载时都被拒")
}

// TestAuditNegativeMaxInFlightBecomesUnlimited：normalize 把 max_in_flight 的非法负值
// **静默夹成 0**（config.go:378-380），而 pool 的判据是 `MaxInFlight > 0` 才限流
// （internal/pool/pool.go:383/528/545）→ 0 = **不限在途**。于是「非法值」落到风险最大的那一档：
// 单账号并发上限从默认 3 变成无限（写配置手抖、env 传 -1 都会命中），且没有任何日志或报错。
// 合法值 0 本来就表示「不限」，非法值却得到同一个语义 —— 越非法越宽松。
func TestAuditNegativeMaxInFlightBecomesUnlimited(t *testing.T) {
	fp := auditWriteConfig(t, map[string]any{"pool": map[string]any{"max_in_flight": -5}})
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("期望复现「负值被接受」：err=%v", err)
	}
	t.Logf("配置文件 pool.max_in_flight=-5 → 生效值 %d（默认是 %d）",
		c.Pool.MaxInFlight, Default().Pool.MaxInFlight)
	// 负数是非法的：不许变成「0 = 不限」那一档，必须回退默认（并记一行日志）。
	if c.Pool.MaxInFlight == 0 {
		t.Fatal("负数被夹成了 0（= 不限在途），非法值落到了最宽松档")
	}
	if c.Pool.MaxInFlight != Default().Pool.MaxInFlight {
		t.Fatalf("负值应回退默认 %d，实得 %d", Default().Pool.MaxInFlight, c.Pool.MaxInFlight)
	}

	t.Setenv("TW2A_MAX_IN_FLIGHT", "-5")
	c2, err := Load("") // 纯默认 + env
	if err != nil {
		t.Fatalf("env 路径不应报错（只记日志 + 回退默认）：%v", err)
	}
	t.Logf("TW2A_MAX_IN_FLIGHT=-5 → 生效值 %d", c2.Pool.MaxInFlight)
	// env 与配置文件同一条 normalize 路径：负数一律回退默认，不落进「0 = 不限」。
	if c2.Pool.MaxInFlight == 0 || c2.Pool.MaxInFlight != Default().Pool.MaxInFlight {
		t.Fatalf("env 负数应回退默认 %d（不能变 0=不限），实得 %d",
			Default().Pool.MaxInFlight, c2.Pool.MaxInFlight)
	}
}

// TestAuditBarePortListenSilentlyBindsAllInterfaces：listen 只写端口号（"7864"）时
// normalize 补成 ":7864"（config.go:405-407）= 监听**所有网卡**。Default() 本身就是
// listen=":7864" + api_key=""（config.go:132-133），而随机生成密钥的 WriteDefault 只在
// config.json **不存在**时跑；老配置/手工清空 api_key 的进程一启动就是「全接口 + 无鉴权」，
// 面板却把它显示成「本机免鉴权」（见 internal/panel/audit_panel_test.go）。
func TestAuditBarePortListenSilentlyBindsAllInterfaces(t *testing.T) {
	fp := auditWriteConfig(t, map[string]any{"listen": "7864"})
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf(`配置文件 listen="7864" → 生效值 %q（= 所有网卡:7864）`, c.Listen)
	if c.Listen != ":7864" {
		t.Fatalf("期望复现「裸端口被静默补成全接口」：%q", c.Listen)
	}
	d := Default()
	t.Logf("Default(): listen=%q api_key=%q → 默认形态即「全接口 + 空密钥」", d.Listen, d.APIKey)
	if d.Listen != ":7864" || d.APIKey != "" {
		t.Fatal("前提变了：默认配置的绑定/密钥形态与断言不符")
	}
}

// TestAuditEnvInvalidValueKeepsDefault 反证用：env 数值非法时**不** fail-open ——
// envInt 解析失败只记一行日志并保留默认（config.go:297-304），字符串类 env 则由
// normalize 在启动阶段 fail fast。故「env 非法值静默改变行为」这条不成立。
func TestAuditEnvInvalidValueKeepsDefault(t *testing.T) {
	t.Setenv("TW2A_TIMEOUT_SECONDS", "abc")
	t.Setenv("TW2A_CHECKIN_HOURS", "9,,x")
	c, err := Load("")
	if err != nil {
		t.Fatalf("非法 env 不该阻断启动（当前行为）：%v", err)
	}
	t.Logf("TW2A_TIMEOUT_SECONDS=abc → %d（保留默认 120）", c.Upstream.TimeoutSeconds)
	t.Logf("TW2A_CHECKIN_HOURS=9,,x → %v（保留默认 [9]）", c.Schedule.CheckinHours)
	if c.Upstream.TimeoutSeconds != 120 {
		t.Fatalf("期望保留默认 120：%d", c.Upstream.TimeoutSeconds)
	}
	if len(c.Schedule.CheckinHours) != 1 || c.Schedule.CheckinHours[0] != 9 {
		t.Fatalf("期望保留默认 [9]：%v", c.Schedule.CheckinHours)
	}
}
