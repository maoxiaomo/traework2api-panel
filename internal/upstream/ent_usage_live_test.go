package upstream

// ent_usage_live_test.go 只读诊断：把 ide_user_ent_usage 的**原始响应**打出来，
// 用于确认国内/国际两套计费口径的字段与标记（尤其是 is_dollar_usage_billing）。
//
// 只读查询，不签到、不改本地状态、不打印凭据。
// 手动跑：TW2A_PROBE=1 TW2A_AUTH_DIR=<含 token 的目录> go test ./internal/upstream -run TestProbeEntUsageRaw -v

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"

	"traework2api/internal/auth"
)

func TestProbeEntUsageRaw(t *testing.T) {
	if os.Getenv("TW2A_PROBE") == "" {
		t.Skip("置 TW2A_PROBE=1 才跑（需要真实凭据与网络）")
	}
	dir := os.Getenv("TW2A_AUTH_DIR")
	if dir == "" {
		dir = "../../auths"
	}
	as, err := auth.LoadDir(dir)
	if err != nil || len(as) == 0 {
		t.Fatalf("加载账号失败: %v (dir=%s)", err, dir)
	}
	c := New()
	for _, a := range as {
		endpoint := c.ugBaseFor(a) + entUsagePath(a)
		req, rerr := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader([]byte(entUsageBody)))
		if rerr != nil {
			t.Fatal(rerr)
		}
		UgHeaders(req, a)
		data, derr := c.doJSON(req)
		fmt.Printf("\n================ realm=%s uid=%s ================\n%s\n", a.Realm(), a.UID, endpoint)
		if derr != nil {
			fmt.Printf("  错误: %v\n", derr)
			continue
		}
		// TW2A_PROBE_SAVE=<文件> 时把**原始响应字节**存成回归夹具（两地区各存一份，
		// 见 ent_usage_unit_test.go）。脱敏：报文里本来只有额度/套餐字段，没有凭据。
		if out := os.Getenv("TW2A_PROBE_SAVE"); out != "" {
			fp := out + "-" + a.Realm() + ".json"
			if werr := os.WriteFile(fp, data, 0o644); werr != nil {
				fmt.Printf("  夹具写入失败: %v\n", werr)
			} else {
				fmt.Printf("  夹具已写: %s（%d 字节）\n", fp, len(data))
			}
		}
		// 只打印计费口径相关的键，token 之类的业务字段一并原样列出（响应里本来就没有凭据）。
		var m map[string]any
		if json.Unmarshal(data, &m) != nil {
			fmt.Printf("  原文: %s\n", string(data))
			continue
		}
		for _, k := range []string{"billing_version", "is_dollar_usage_billing", "is_credits_billing",
			"is_pay_freshman", "trial_status"} {
			if v, ok := m[k]; ok {
				fmt.Printf("  %s = %v\n", k, v)
			}
		}
		packs, _ := m["user_entitlement_pack_list"].([]any)
		for i, p := range packs {
			pm, _ := p.(map[string]any)
			ebi, _ := pm["entitlement_base_info"].(map[string]any)
			q, _ := ebi["quota"].(map[string]any)
			u, _ := pm["usage"].(map[string]any)
			fmt.Printf("  [包 %d] display_desc=%v\n", i, pm["display_desc"])
			fmt.Printf("         quota.credits_limit=%v basic_usage_limit=%v bonus_usage_limit=%v\n",
				q["credits_limit"], q["basic_usage_limit"], q["bonus_usage_limit"])
			fmt.Printf("         usage.credits_amount=%v basic_usage_amount=%v bonus_usage_amount=%v is_flash_consuming=%v\n",
				u["credits_amount"], u["basic_usage_amount"], u["bonus_usage_amount"], u["is_flash_consuming"])
			fmt.Printf("         charge_amount=%v currency=%v end_time=%v\n",
				ebi["charge_amount"], ebi["currency"], ebi["end_time"])
		}
		// 顺带看看有没有别的金额口径字段被漏掉
		var keys []string
		for k := range m {
			keys = append(keys, k)
		}
		fmt.Printf("  顶层键: %v\n", keys)
	}
}
