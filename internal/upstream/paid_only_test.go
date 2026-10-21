package upstream

import "testing"

// contactFree 是「免费 / 订阅专享」的唯一判据：上游 identity_list 里有没有 Free(0)。
//
// 样例取自 2026-10-21 的真实响应：国内版 47 条全是 [0 5 1 2 3 100]；
// 国际版 gpt-6.1-sol / gpt-6-sol / gpt-6-luna 是 [4 1 2 3]，其余是 [0 5 4 1 2 3]。
func TestContactFree(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		free  bool
		known bool
	}{
		{"国内版全表", `{"access":{"data":{"identity_list":[0,5,1,2,3,100]}}}`, true, true},
		{"国际版免费可用", `{"access":{"data":{"identity_list":[0,5,4,1,2,3]}}}`, true, true},
		{"国际版订阅专享", `{"access":{"data":{"identity_list":[4,1,2,3]}}}`, false, true},
		{"空串=不知道", ``, false, false},
		{"没有 access 块", `{"consumption_rate":{"data":{"rate":0.2}}}`, false, false},
		{"identity_list 为空=不知道", `{"access":{"data":{"identity_list":[]}}}`, false, false},
		{"坏 JSON=不知道", `{"access":`, false, false},
	}
	for _, c := range cases {
		free, known := contactFree(c.raw)
		if free != c.free || known != c.known {
			t.Errorf("%s: 得到 free=%v known=%v，期望 free=%v known=%v", c.name, free, known, c.free, c.known)
		}
	}
}

// 上游没给 identity_list 时不能把模型标成付费——「不知道」按可用处理，
// 否则一次 JSON 变更就会让整张表挂上「需订阅」。
func TestPaidOnlyNeedsIdentityList(t *testing.T) {
	cfg := paramConfig{ConfigName: "glm-5.2", Usage: usageChat}
	if _, known := contactFree(cfg.DisplayContactConfig); known {
		t.Fatal("空 display_contact_config 不该判成 known")
	}
	if (&cfg).meta().PaidOnly {
		t.Error("identity_list 缺失时 PaidOnly 必须为 false")
	}

	cfg.DisplayContactConfig = `{"access":{"data":{"identity_list":[4,1,2,3]}}}`
	if !(&cfg).meta().PaidOnly {
		t.Error("identity_list 缺 Free(0) 时 PaidOnly 必须为 true")
	}
}
