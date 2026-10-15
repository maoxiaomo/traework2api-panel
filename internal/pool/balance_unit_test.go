package pool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"traework2api/internal/auth"
)

// balance_unit_test.go 额度口径（国内积分 / 国际美元）在池子里的行为。
//
// 关键约束：两种单位不可换算，任何把它们相加/比较的地方都是 bug。
// 选号按 realm 分组（见 pickLocked 的 realm 过滤），所以同组内单位一致，
// 各自按自己的口径归一即可。

// TestSetBalanceKeepsUnitsApart 写余额时两种口径各自落到自己的字段，
// 且**不互相写入**——把美元塞进 credits 会让「积分」列显示一个不是积分的数。
func TestSetBalanceKeepsUnitsApart(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "cn1", Nickname: "国内号"})
	p.Add(&auth.Auth{UID: "intl1", Nickname: "国际号", Domain: "trae.ai"})

	p.SetBalance("cn1", Balance{Unit: "credit", Remain: 5466, Total: 5900}, 1792968453)
	p.SetBalance("intl1", Balance{Unit: "usd", UsedUSD: 0.00059, LimitUSD: 1}, 1793491199)

	st := map[string]Status{}
	for _, s := range p.List() {
		st[s.UID] = s
	}

	cn := st["cn1"]
	if cn.Credits != 5466 || cn.CreditsTotal != 5900 {
		t.Errorf("国内号积分口径被改坏: %d/%d", cn.Credits, cn.CreditsTotal)
	}
	if cn.UsedUSD != 0 || cn.LimitUSD != 0 {
		t.Errorf("国内号不该有美元字段: %v/%v", cn.UsedUSD, cn.LimitUSD)
	}
	if cn.CreditUnit != "credit" {
		t.Errorf("国内号 unit=%q，期望 credit", cn.CreditUnit)
	}

	in := st["intl1"]
	if in.CreditUnit != "usd" {
		t.Fatalf("国际号 unit=%q，期望 usd", in.CreditUnit)
	}
	if in.LimitUSD != 1 || in.UsedUSD != 0.00059 {
		t.Errorf("国际号美元额度不对: limit=%v used=%v", in.LimitUSD, in.UsedUSD)
	}
	if in.Credits != 0 || in.CreditsTotal != 0 {
		t.Errorf("美元号不该往积分字段写数: credits=%d total=%d", in.Credits, in.CreditsTotal)
	}
	if in.Expire != 1793491199 {
		t.Errorf("到期时间没落: %d", in.Expire)
	}

	// 反向：先美元后积分，必须把美元字段清掉（否则切口径后会残留旧数字）
	p.SetBalance("intl1", Balance{Unit: "credit", Remain: 100, Total: 200}, 1)
	for _, s := range p.List() {
		if s.UID != "intl1" {
			continue
		}
		if s.Credits != 100 || s.CreditsTotal != 200 {
			t.Errorf("切回积分口径没生效: %d/%d", s.Credits, s.CreditsTotal)
		}
		if s.UsedUSD != 0 || s.LimitUSD != 0 {
			t.Errorf("切回积分口径后美元字段应清空: %v/%v", s.UsedUSD, s.LimitUSD)
		}
	}
}

// TestWeightUSDUsesRemainFraction 美元号按**剩余占比**给分。
//
// 不能沿用积分那条「占池内最大积分的比例」：美元数与积分数不可比。
// 用剩余占比还顺手修掉一个老问题——国际版免费号 credits 恒 0，在乘性权重下权重恒 0、
// 会被永久饿死（drawLocked 的注释记着这件事）。
func TestWeightUSDUsesRemainFraction(t *testing.T) {
	p := New("")
	now := time.Now()
	// 闲置加成会在线性项之外再加分（ApplyLimits 故意忽略零值，改不动它），
	// 所以直接把 lastUsed 设成现在，让闲置项为 0，断言才干净。
	fresh := now

	full := &entry{unit: "usd", limitUSD: 1, usedUSD: 0, lastUsed: fresh}  // 剩 100%
	low := &entry{unit: "usd", limitUSD: 1, usedUSD: 0.9, lastUsed: fresh} // 剩 10%
	spent := &entry{unit: "usd", limitUSD: 1, usedUSD: 1, lastUsed: fresh} // 用光
	credit := &entry{credits: 5466, creditsTotal: 5900, lastUsed: fresh}   // 国内号（unit 空 = credit）

	// maxCredits 只对积分号有意义；给 5466 模拟「池里最大的国内号」
	wFull := p.weight(full, 5466, now)
	wLow := p.weight(low, 5466, now)
	wSpent := p.weight(spent, 5466, now)
	wCredit := p.weight(credit, 5466, now)

	if !(wFull > wLow) {
		t.Errorf("剩余多的美元号权重应更高: full=%.3f low=%.3f", wFull, wLow)
	}
	if wFull <= 1.0 {
		t.Errorf("剩 100%% 的美元号权重应大于基准 1.0，实得 %.3f", wFull)
	}
	// 用光的号只拿到基准分，不该因为「积分 0」被额外惩罚或奖励
	if wSpent < 1.0 || wSpent > 1.001 {
		t.Errorf("用光的美元号权重应为基准 1.0，实得 %.3f", wSpent)
	}
	// 国内号行为逐字不变：5466/5900 → 1 + (5466/5466)*10 = 11
	if wCredit < 10.99 || wCredit > 11.01 {
		t.Errorf("国内号权重公式被改了: %.3f（期望 11）", wCredit)
	}
	// 两个口径的上限都是 11，美元号不该超过满额积分号
	if wFull > 11.0 {
		t.Errorf("美元号权重不该超过满额积分号: %.3f", wFull)
	}
}

// TestBalanceUSDSurvivesRestart 美元额度必须持久化：重启后凭 0 积分恢复不出任何东西，
// 面板会退回「无额度」（这正是改动前国际号的样子）。
func TestBalanceUSDSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")

	p := New(fp)
	p.Add(&auth.Auth{UID: "intl1", Domain: "trae.ai"})
	p.SetBalance("intl1", Balance{Unit: "usd", UsedUSD: 0.00059, LimitUSD: 1}, 1793491199)
	if _, err := os.Stat(fp); err != nil {
		t.Fatalf("状态没落盘: %v", err)
	}

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "intl1", Domain: "trae.ai"})
	var got *Status
	for _, s := range p2.List() {
		if s.UID == "intl1" {
			v := s
			got = &v
		}
	}
	if got == nil {
		t.Fatal("重启后账号没了")
	}
	if got.CreditUnit != "usd" || got.LimitUSD != 1 || got.UsedUSD != 0.00059 || got.Expire != 1793491199 {
		t.Errorf("重启后美元额度丢失: unit=%q limit=%v used=%v expire=%d",
			got.CreditUnit, got.LimitUSD, got.UsedUSD, got.Expire)
	}
}

// TestBalanceFileAddsOnlyIgnorableKeys 落盘新增的键必须是**老版本能忽略**的。
//
// 老版本 stateFile 是匿名 struct，encoding/json 遇到未知键会跳过，所以多写
// `credit_unit` 不会把老二进制读崩。真正要保证的是：老版本依赖的那几个键与取值不变。
func TestBalanceFileAddsOnlyIgnorableKeys(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "cn1"})
	p.SetBalance("cn1", Balance{Unit: "credit", Remain: 5466, Total: 5900}, 1792968453)

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	// 老版本要读的键一个都不能少、值不能变
	for _, want := range []string{
		`"credits": 5466`, `"credits_total": 5900`, `"expire": 1792968453`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("落盘缺少老版本依赖的字段 %s：\n%s", want, s)
		}
	}
	// 新增键只能是「有就写、没有也能跑」的那种，且必须能被老版本忽略
	if !strings.Contains(s, `"credit_unit": "credit"`) {
		t.Errorf("国内号也该写明口径（前端按 credit_unit 分支），实得：\n%s", s)
	}
	if strings.Contains(s, `"used_usd"`) || strings.Contains(s, `"limit_usd"`) {
		t.Errorf("国内号不该落美元字段（omitempty 没生效）：\n%s", s)
	}
}
