package upstream

// claim_live_test.go 真号验领取链路（会**真的领取**，会改上游状态）。
// 默认跳过，手动跑：TW2A_CLAIM=1 TW2A_AUTH_DIR=<auths 目录> go test ./internal/upstream -run TestClaimActivitiesLive -v
// 只打第一个账号；不打印任何凭据。

import (
	"os"
	"testing"

	"traework2api/internal/auth"
)

func TestClaimActivitiesLive(t *testing.T) {
	if os.Getenv("TW2A_CLAIM") == "" {
		t.Skip("置 TW2A_CLAIM=1 才跑（会真的领取活动/权益）")
	}
	dir := os.Getenv("TW2A_AUTH_DIR")
	if dir == "" {
		t.Fatalf("需要 TW2A_AUTH_DIR 指向 auths 目录")
	}
	as, err := auth.LoadDir(dir)
	if err != nil || len(as) == 0 {
		t.Fatalf("加载账号失败: %v (dir=%s)", err, dir)
	}
	var failed int
	for _, a := range as {
		if a.Realm() != auth.RealmCN {
			t.Logf("%s 国际号，跳过（没有 ug 接口）", a.UID)
			continue
		}
		notes, err := New().ClaimActivities(a)
		t.Logf("uid=%s notes=%v err=%v", a.UID, notes, err)
		if err != nil {
			failed++
		}
	}
	if failed > 0 {
		t.Fatalf("%d 个账号领取失败", failed)
	}
}
