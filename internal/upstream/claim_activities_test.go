package upstream

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"traework2api/internal/auth"
)

// TestClaimActivitiesRoutes 活动领取的两条链路与「领不到不是失败」的判定。
// 钉住三件会静默出错的事：103 必须带 activity_id 走 fission；其余走 benefit；
// 9069/9090/9070 是「现在领不到」而不是错误（记成错误会让调度器每天报警）。
func TestClaimActivitiesRoutes(t *testing.T) {
	var paths, bodies []string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		paths = append(paths, r.URL.Path)
		bodies = append(bodies, string(b))
		switch r.URL.Path {
		case EpActivityInfo:
			// 三条：两条 103（一条过期）、一条 104、一条 103 但 Enabled=false
			return jsonResp(200, `{"commercial_activities":[
				{"activity_id":"fission_live","activity_type":103,"Enabled":true,"start_time_ms":1,"end_time_ms":99999999999999},
				{"activity_id":"fission_dead","activity_type":103,"Enabled":true,"start_time_ms":1,"end_time_ms":2},
				{"activity_id":"checkin_x","activity_type":104,"Enabled":true},
				{"activity_id":"fission_off","activity_type":103,"Enabled":false}]}`), nil
		case EpFissionGrant:
			return jsonResp(200, `{"code":0}`), nil
		case EpActivityAction:
			// 券类活动：action 是唯一回 work_extra 的接口（granted/prize_text）
			return jsonResp(200, `{"work_extra":{"grantable":false,"granted":true,"prize_text":"测试券"}}`), nil
		case EpGrantBenefit:
			return jsonResp(200, `{"code":9069,"message":"活动暂不可用"}`), nil
		}
		t.Fatalf("意外的上游路径 %s", r.URL.Path)
		return nil, nil
	})

	notes, err := c.ClaimActivities(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("9069 是「现在领不到」，不该报错：%v", err)
	}
	// 只打四条：清单 + 一条 fission（另一条过期、一条 Enabled=false）+ 一条 action + 权益
	if len(paths) != 4 {
		t.Fatalf("应只请求 4 次（清单 + 1 条裂变 + 1 条 action + 权益），实得 %v", paths)
	}
	if !strings.Contains(bodies[1], `"activity_id":"fission_live"`) {
		t.Errorf("裂变领取必须带 activity_id，实得 %s", bodies[1])
	}
	if !strings.Contains(bodies[2], `"activity_id":"checkin_x"`) {
		t.Errorf("action 也要带 activity_id，实得 %s", bodies[2])
	}
	if !strings.Contains(bodies[3], `"trae_client":"Lite"`) || !strings.Contains(bodies[3], `"device_id":"`) {
		t.Errorf("权益领取的 body 形状不对：%s", bodies[3])
	}
	// notes 只写领到的（含上游给的 prize_text）；9069 不进 notes（否则每天刷同样的噪音）
	if len(notes) != 2 || !strings.Contains(notes[0], "fission_live") || !strings.Contains(notes[1], "测试券") {
		t.Errorf("notes 应含 fission_live 与带券名的 action 各一条，实得 %v", notes)
	}
}

// TestClaimActivitiesIntlSkipped 国际域没有 ug 接口（同签到），不该白打上游。
func TestClaimActivitiesIntlSkipped(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("国际号不该请求上游，实打 %s", r.URL.Path)
		return nil, nil
	})
	if _, err := c.ClaimActivities(&auth.Auth{Domain: "trae.ai"}); err != ErrNoIntlUG {
		t.Fatalf("国际版应直接返回 ErrNoIntlUG，实得 %v", err)
	}
}
