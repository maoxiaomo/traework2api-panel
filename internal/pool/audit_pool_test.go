//go:build audit

// 只读审计用的「证明测试」：断言的是**当前**（有缺陷的）行为，不是期望行为。
// 仅在 `go test -tags audit` 下编译运行，用来钉住审计报告里的每条结论：
// 一旦某条被修好，对应用例会失败，报告需要同步更新。
package pool

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"traework2api/internal/auth"
)

// P-1 CooldownCheckin 只要「还在冷却中」就整段早退：长冷却装不进去。
// 1 分钟的软限流冷却在场时，随后 12 小时的 plan 冷却（上游 1005）被丢弃，
// 账号 1 分钟后又被签到轮次打一次 —— 与 setCooldownLocked「不叠加：取更长者」相反。
func TestAuditCooldownCheckinCannotLengthen(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	p.CooldownCheckin("u1", time.Minute, "soft_rate")
	p.CooldownCheckin("u1", 12*time.Hour, "plan 权益不足")

	st, _ := p.Status("u1")
	t.Logf("CheckinUntil=%v reason=%q", st.CheckinUntil.Format(time.RFC3339), st.CheckinReason)
	// 更重的判罚必须能加长冷却（否则 12h 的「plan 权益不足」被 1min 的 soft_rate 吃掉）。
	if time.Until(st.CheckinUntil) < 11*time.Hour || st.CheckinReason != "plan 权益不足" {
		t.Fatalf("长冷却没生效（应加长到 12h）: %+v", st)
	}
	// 反向：已经在长冷却里，轻的判罚不许把它缩短。
	p.CooldownCheckin("u1", time.Minute, "soft_rate")
	st2, _ := p.Status("u1")
	if time.Until(st2.CheckinUntil) < 11*time.Hour || st2.CheckinReason != "plan 权益不足" {
		t.Fatalf("轻判罚把长冷却缩短了: %+v", st2)
	}
}

// P-2 AcquireIfHealthy 写 lastUsed 却不推进 usedSeq：会话粘住的号在「全部候选都在
// minPickGap 内」的 LRU 兜底里仍被判成「最久未被选中」，于是刚粘住的号被立刻再选中。
// 期望：粘住也是一次选中，应推进 usedSeq（否则兜底顺序与该路径的语义分裂）。
func TestAuditAcquireIfHealthyLeavesUsedSeqStale(t *testing.T) {
	oldGap := minPickGap
	minPickGap = time.Hour // 让两个候选都落在窗口内，强制走 LRU 兜底
	defer func() { minPickGap = oldGap }()

	p := New("")
	p.ApplyLimits(Limits{MaxInFlight: 2})
	p.SetRandInt64N(func(int64) int64 { return 0 }) // 抽签退化成「权重最高者」
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})

	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("前提不成立：首次抽签应中 u1，实得 %+v", got)
	}
	if _, ok := p.AcquireIfHealthy("u2"); !ok {
		t.Fatal("前提不成立：u2 应能粘住")
	}
	// u2 刚被粘住（时间上最新），u1 是上一轮的选中；LRU 兜底却选 u2。
	got := p.Pick()
	if got == nil {
		t.Fatal("没有候选")
	}
	t.Logf("LRU 兜底选中 %s（u2 刚被 AcquireIfHealthy 粘住）", got.UID)
	// 粘住也要算「刚用过」：LRU 兜底该选 u1，不该把刚粘住的 u2 再给你一次。
	if got.UID != "u1" {
		t.Fatalf("LRU 兜底选中了刚被粘住的 %s：AcquireIfHealthy 没推进 usedSeq", got.UID)
	}
}

// P-3 BumpDeviceSeed 的读-改-写（DeviceSeedValue()+1 → SetDeviceSeed）不是原子的：
// 两个 goroutine 同时进（调度器补签轮次与面板「换指纹」按钮/整点轮次都会调），
// 各自读到同一个旧值 → 写回同一个新值 → 两次「换指纹」只前进了 1 格，
// 而 9074/9095 正是靠「换一套设备身份」脱开旧标记的。
func TestAuditBumpDeviceSeedLostUpdateOnConcurrentCalls(t *testing.T) {
	dir := t.TempDir()
	a := &auth.Auth{UID: "u1", FilePath: filepath.Join(dir, "trae-u1.json")}
	p := New("")
	p.Add(a)

	const (
		rounds   = 20
		perRound = 4
	)
	lost := 0
	for r := 0; r < rounds; r++ {
		p.AuthByUID("u1").SetDeviceSeed(int64(r * 100)) // 每轮从干净值出发
		var wg sync.WaitGroup
		ready := make(chan struct{}, perRound)
		for i := 0; i < perRound; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ready <- struct{}{} // 四路都就绪，尽量同一瞬间进 BumpDeviceSeed
				_, _, _ = p.BumpDeviceSeed("u1")
			}()
		}
		for i := 0; i < perRound; i++ {
			<-ready
		}
		wg.Wait()
		if got := p.AuthByUID("u1").DeviceSeedValue(); got != int64(r*100+perRound) {
			lost++
			t.Logf("第 %d 轮：4 次 BumpDeviceSeed 后 seed=%d（应为 %d，丢了 %d 次）",
				r, got, r*100+perRound, int64(r*100+perRound)-got)
		}
	}
	if lost == 0 {
		t.Fatalf("%d 轮都没复现丢更新（请更新审计报告为「未证实」）", rounds)
	}
	t.Logf("%d/%d 轮出现丢更新", lost, rounds)
}

// P-4 state.json 损坏（截断/半截 JSON）时 load() 静默整包丢弃：
// 不禁用任何号、不打一行日志，禁用/冷却/熔断档位全部归零 —— 死号重新入池。
func TestAuditCorruptStateFileSilentlyDropsAllState(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")

	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")
	p.CooldownPlan("u1")

	if _, ok := New(fp).Status("u1"); !ok {
		t.Fatal("前提不成立：正常 state.json 未能加载")
	}
	if st, _ := New(fp).Status("u1"); !st.Disabled || !st.Cooling {
		t.Fatalf("前提不成立：应有的禁用/冷却没落盘: %+v", st)
	}

	// 模拟崩溃/半截写入留下的坏文件（load 的 json.Unmarshal 失败分支）
	if err := os.WriteFile(fp, []byte(`{"accounts":{"u1":{"credits":1`), 0o600); err != nil {
		t.Fatal(err)
	}
	q := New(fp)
	st, ok := q.Status("u1")
	t.Logf("坏 state.json → List=%d, Status(u1) ok=%v st=%+v", len(q.List()), ok, st)
	if ok {
		t.Fatal("坏文件不该被当成正常状态载入")
	}
	// 现场必须留一份：不然下一次保存就把坏文件覆盖掉了，想查都查不到。
	if _, err := os.Stat(fp + ".corrupt"); err != nil {
		t.Fatalf("坏 state.json 没被备份成 .corrupt: %v", err)
	}
}

// P-5 auth.Parse 不校验 uid：两个「没写 uid」的 auth 文件（扁平形手建，文档明确支持）
// 在池里按 uid 作 key，于是后一个静默顶掉前一个 —— 账号无声消失，只剩一个可用。
func TestAuditLoadDirAccountsWithoutUIDCollapseIntoOne(t *testing.T) {
	dir := t.TempDir()
	for name, tok := range map[string]string{"trae-a.json": "at-a", "trae-b.json": "at-b"} {
		raw := `{"accessToken":"` + tok + `","refreshToken":"rt-` + tok + `"}`
		if err := os.WriteFile(filepath.Join(dir, name), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	as, err := auth.LoadDir(dir)
	if err != nil || len(as) != 2 {
		t.Fatalf("前提不成立：应扫到 2 个文件，实得 %d (%v)", len(as), err)
	}
	p := New("")
	p.SyncToDir(as)
	l := p.List()
	t.Logf("2 个无 uid 的 auth 文件 → 池里 %d 个条目", len(l))
	// 两个号都得在：uid 为空时会互相覆盖，只剩一个。
	if len(l) != 2 {
		t.Fatalf("同为空 uid 的账号被合并了: %+v", l)
	}
	for _, a := range as {
		if a.UID == "" {
			t.Fatalf("uid 没按文件名兜底: %+v", as)
		}
	}
}
