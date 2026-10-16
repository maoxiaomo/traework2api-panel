//go:build audit

// 只读审计用的「证明测试」：断言的是**当前**（有缺陷的）行为，不是期望行为。
// 仅在 `go test -tags audit` 下编译运行；某条被修好后对应用例会失败，提示更新审计报告。
// 全部走 httptest 假上游 + t.TempDir()，不打真实上游、不用真实凭据。
package scheduler

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"traework2api/internal/auth"
	"traework2api/internal/pool"
)

func auditReadAuth(t *testing.T, path string) *auth.Auth {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 auth 文件: %v", err)
	}
	a, err := auth.Parse(raw)
	if err != nil {
		t.Fatalf("解析 auth 文件: %v", err)
	}
	return a
}

// S-1 CheckinUID 的「签到前预刷新」丢了 RefreshTokenIfNeeded 的返回值、也不落盘：
// refreshToken 在上游已轮换，磁盘上还是被消费掉的旧值。重启后拿旧值换票 → 上游判失效 →
// 连续 session dead → 账号被自动禁用（对照 RunRefreshNow/handler 都成对 SaveAtomic）。
func TestAuditCheckinPreRefreshNotPersisted(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500, resourceUsed: 100}
	srv := f.server()
	defer srv.Close()

	a := &auth.Auth{
		UID: "u1", Domain: "trae.cn",
		AccessToken: "at-old", RefreshToken: "rt-old",
		// 落在 checkinRefreshSkew(2h) 内 → 走签到前预刷新
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
		FilePath:  filepath.Join(t.TempDir(), "trae-u1.json"),
	}
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	p := pool.New("")
	p.Add(a)
	s := newTestScheduler(f, p, srv)

	if _, err := s.CheckinUID("u1"); err != nil {
		t.Fatalf("前提不成立：该用例要的是签到成功路径: %v", err)
	}
	if f.refreshCalls.Load() == 0 {
		t.Fatal("前提不成立：预刷新没触发")
	}
	mem := a.RefreshTokenValue()
	disk := auditReadAuth(t, a.FilePath).RefreshToken
	t.Logf("内存 refreshToken=%q（新）/ 磁盘 refreshToken=%q", mem, disk)
	// 签到前的预刷新只要真刷了就必须落盘：上游已轮换，盘上留旧票的话进程重启即被判
	// session 失效并自动禁用（要人工重登）。
	if disk != "newrt" || mem != "newrt" {
		t.Fatalf("签到路径的预刷新没落盘（票会被判失效）: mem=%q disk=%q", mem, disk)
	}
}

// S-2 同一条缺口出现在 session-dead 补救路径：RefreshToken(a) 换了票，落盘还是旧的。
func TestAuditCheckinSessionDeadRefreshNotPersisted(t *testing.T) {
	var statusCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/checkin_credits/status"):
			if statusCalls.Add(1) == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"code":1001,"message":"login required"}`))
				return
			}
			w.Write([]byte(`{"checked_in":true,"credits":200,"enable":true}`))
		case strings.HasSuffix(r.URL.Path, "/ExchangeToken"):
			w.Write([]byte(`{"Result":{"Token":"newat","RefreshToken":"newrt","TokenExpireAt":1786805537}}`))
		case strings.HasSuffix(r.URL.Path, "/ide_user_ent_usage"), strings.HasSuffix(r.URL.Path, "/web_user_ent_usage"):
			w.Write([]byte(`{"user_entitlement_pack_list":[{"entitlement_base_info":{"quota":{"credits_limit":500}},"usage":{"credits_amount":100}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	a := &auth.Auth{
		UID: "u1", Domain: "trae.cn",
		AccessToken: "at-old", RefreshToken: "rt-old",
		ExpiresAt: 9999999999, // 不走预刷新，只验 session-dead 补救那条
		FilePath:  filepath.Join(t.TempDir(), "trae-u1.json"),
	}
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	p := pool.New("")
	p.Add(a)
	s := newTestScheduler(&fakeUpstream{}, p, srv)

	if _, err := s.CheckinUID("u1"); err != nil {
		t.Fatalf("前提不成立：换票后应签到成功: %v", err)
	}
	mem := a.RefreshTokenValue()
	disk := auditReadAuth(t, a.FilePath).RefreshToken
	t.Logf("内存 refreshToken=%q（新）/ 磁盘 refreshToken=%q", mem, disk)
	if disk != "newrt" || mem != "newrt" {
		t.Fatalf("session 失效补救路径换到的票没落盘: mem=%q disk=%q", mem, disk)
	}
}

// S-3 签到**失败**也走 ReenableIfCredits（只要余额 > 0 就清网关冷却）：
// 12h 计划硬冷却 / 熔断 / 降权冷却被一次失败的签到冲掉，而这条路径被
// runCheckinRetryLoop 每小时自动跑一遍（不只用户手点）。
func TestAuditCheckinFailStillUnfreezesGatewayCooldown(t *testing.T) {
	f := &fakeUpstream{
		claimResponse:  `{"code":9074,"message":"当前参与用户太多，请稍后再试"}`,
		resourceRemain: 500,
		resourceUsed:   100,
	}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Domain: "trae.cn", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.CooldownPlan("u1") // 1005 硬冷却 12h
	before, _ := p.Status("u1")
	if !before.Cooling || time.Until(before.Until) < 11*time.Hour {
		t.Fatalf("前提不成立：计划冷却没生效: %+v", before)
	}
	s := newTestScheduler(f, p, srv)

	res, err := s.CheckinUID("u1")
	after, _ := p.Status("u1")
	t.Logf("签到结果 status=%q err=%v；网关冷却 before=%v(%.1fh) → after=%v",
		res.Status, err, before.Cooling, time.Until(before.Until).Hours(), after.Cooling)
	if err == nil {
		t.Fatal("前提不成立：9074 应判签到失败")
	}
	// 失败路径不许解冻：签到失败跟「号恢复了」是两件事，把 12h 计划冷却冲掉等于
	// 一次失败顺手把风控惩罚作废（runCheckinRetryLoop 每小时自动跑一遍）。
	if !after.Cooling {
		t.Fatalf("签到失败把网关冷却解冻了（惩罚被冲掉）: %+v", after)
	}
}

// S-4 定时整点、每小时补签 ticker、面板「立即签到」按钮、启动 CatchUp 之间没有任何
// 串行化：同一账号可以被两路同时送进 CheckinUID（本身就是秒级二次 claim，而上游原注释说
// 秒级内第二次 claim 会延长限流窗口），并发的 BumpDeviceSeed 还会互相覆盖（见 pool P-3）。
func TestAuditConcurrentRunCheckinNotSerialized(t *testing.T) {
	var cur, peak atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/checkin_credits/status"):
			n := cur.Add(1)
			for {
				m := peak.Load()
				if n <= m || peak.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(150 * time.Millisecond) // 撑开重叠窗口，让并发可见
			cur.Add(-1)
			w.Write([]byte(`{"checked_in":false,"credits":200,"enable":true}`))
		case strings.HasSuffix(r.URL.Path, "/checkin_credits/claim"):
			w.Write([]byte(`{"code":0,"message":"success"}`))
		case strings.HasSuffix(r.URL.Path, "/ide_user_ent_usage"):
			w.Write([]byte(`{"user_entitlement_pack_list":[{"entitlement_base_info":{"quota":{"credits_limit":500}},"usage":{"credits_amount":100}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Domain: "trae.cn", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	s := newTestScheduler(&fakeUpstream{}, p, srv)

	run := func() {
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); s.RunCheckinNow() }()
		}
		wg.Wait()
	}
	// 一次调度偶然错开不算证据：三轮都只能见到 1 才判「已串行化」。
	for attempt := 1; attempt <= 3 && peak.Load() < 2; attempt++ {
		run()
	}
	t.Logf("同时进入 CheckinUID 的并发峰值=%d（有串行化时只可能为 1）", peak.Load())
	if peak.Load() < 2 {
		t.Fatalf("缺陷行为已消失（出现串行化了？请更新审计报告）")
	}
}

// S-5 nextFire 的「已过今天 → 加 24h 整」用的是绝对时长而不是本地日期重算：
// 跨 DST 跳变（美东 2026-03-08 02:00 EST→03:00 EDT）后算出的时点本地小时变成 4，
// 主循环的 contains(KeepaliveHours, fired.Hour()) 不匹配 → 当天 03:00 那档整个被吞，
// 两次真实保活之间隔 47 小时（保活没有补跑循环兜底，只有签到有）。
func TestAuditNextFireSkipsSlotAcrossDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("本机无 tzdata，无法构造 DST 场景: %v", err)
	}
	hours := []int{3}
	now := time.Date(2026, 3, 7, 0, 30, 0, 0, ny) // 3/8 02:00 EST → 03:00 EDT

	fired := nextFire(now, hours)  // 3/7 03:00 EST —— 正常触发
	next := nextFire(fired, hours) // fired.Add(24h) → 3/8 04:00 EDT
	next2 := nextFire(next, hours) // → 3/9 03:00 EDT

	t.Logf("fired=%v(h=%d) next=%v(h=%d) next2=%v", fired.Format(time.RFC3339), fired.Hour(),
		next.Format(time.RFC3339), next.Hour(), next2.Format(time.RFC3339))
	if fired.Hour() != 3 {
		t.Fatalf("前提不成立: %v", fired)
	}
	if contains(hours, next.Hour()) {
		t.Fatalf("缺陷行为已消失（跨 DST 的时点仍然对得上？请更新审计报告）")
	}
	if gap := next2.Sub(fired); gap < 40*time.Hour {
		t.Fatalf("缺陷行为已消失（间隔只有 %v）", gap)
	}
	t.Logf("两次真实保活间隔 %v（3/8 那档被静默跳过）", next2.Sub(fired))
}
