package server

import "testing"

// TestApplyRateFree 限免判定：上游用基准倍率 0 表示限时免费。
//
// 面板用真值判断倍率（0 在 JS 里是假值）会把限免行渲染成「—」，所以载荷里必须
// 有独立的 free 标记。nil（上游没给倍率）与 0（明确限免）是两件事，不能一起判。
func TestApplyRateFree(t *testing.T) {
	zero, half, normal := 0.0, 0.5, 1.5

	entry := map[string]any{}
	applyRate(entry, &zero)
	if entry["free"] != true {
		t.Errorf("倍率 0 应标 free: %v", entry)
	}
	if r, ok := entry["credit_rate"].(float64); !ok || r != 0 {
		t.Errorf("倍率 0 必须原样保留（前端据此显示 0.00×）: %v", entry["credit_rate"])
	}

	// 未给倍率：既不能编一个数，更不能当限免。
	entry = map[string]any{}
	applyRate(entry, nil)
	if _, ok := entry["credit_rate"]; ok {
		t.Errorf("上游没给倍率时不该写 credit_rate: %v", entry)
	}
	if _, ok := entry["free"]; ok {
		t.Errorf("上游没给倍率时不该标 free: %v", entry)
	}

	for _, rate := range []*float64{&half, &normal} {
		entry = map[string]any{}
		applyRate(entry, rate)
		if _, ok := entry["free"]; ok {
			t.Errorf("倍率 %v 不该标 free: %v", *rate, entry)
		}
		if r, ok := entry["credit_rate"].(float64); !ok || r != *rate {
			t.Errorf("倍率未透传: %v", entry)
		}
	}
}
