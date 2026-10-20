package upstream

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// batchFixture 真实抓包片段：2026-10-15 从本仓 CN 账号的客户端模型选择器视图
// （batch_get_detail_param）响应里抽出的 3 个面，**字节原样**落盘，不是手写的。
//
//	internal/upstream/testdata/batch_max_variants.json ← capture/batch/batch_*.json
//
// 挑这三个面是因为它们正好覆盖三种情况：
//
//	solo_work_lite 面：glm-5.2 只有 __dev、没有 context_window_tokens.max（本面板默认面，没有 Max）
//	chat_v3 面：      glm-5.2 有 __dev + __max，116000 → 1000000（Max 在这里）
//	solo_agent 面：   同上有 Max，且 dev 是 200000
//
// 夹具里还带两条「变体名不能拼」的实证：glm-5.2_advisor_doubao → glm-5.2__dev、
// search_agent_qwen_fast → search_agent_model__dev/__max，以及 search_agent_qwen_fast
// 那种「有 __max 名但窗口没变大（184000 == 184000）」的假 Max。
const batchFixture = "testdata/batch_max_variants.json"

// loadBatchFixture 读夹具，返回 function → paramConfig 列表。
func loadBatchFixture(t *testing.T) map[string][]paramConfig {
	t.Helper()
	raw, err := os.ReadFile(batchFixture)
	if err != nil {
		t.Fatalf("读夹具失败: %v", err)
	}
	var resp struct {
		FunctionConfigs []struct {
			Function       string        `json:"function"`
			ConfigInfoList []paramConfig `json:"config_info_list"`
		} `json:"function_configs"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("夹具不是合法 JSON: %v", err)
	}
	out := map[string][]paramConfig{}
	for _, g := range resp.FunctionConfigs {
		out[g.Function] = g.ConfigInfoList
	}
	return out
}

// findCfg 在某个面的配置列表里按 config_name 找一条。
func findCfg(t *testing.T, list []paramConfig, name string) paramConfig {
	t.Helper()
	for _, c := range list {
		if c.ConfigName == name {
			return c
		}
	}
	t.Fatalf("夹具里没有 %s", name)
	return paramConfig{}
}

// TestVariantNamesMustBeReadNotDerived 变体名必须原样读出来，不能按 ID 拼。
//
// 这是本次改动最容易写错的一处：想当然地写 model = config_name + "__max"，
// 在这 99 条上会拼出一个上游认不出的名字，而失败形态是「静默按 dev 行为跑」——
// 客户端以为开了 1M，实际上还是 116k，很难从表象看出来。
func TestVariantNamesMustBeReadNotDerived(t *testing.T) {
	fx := loadBatchFixture(t)

	// glm-5.2_advisor_doubao 的变体名是 glm-5.2__dev（复用基础模型的名字）
	advisor := findCfg(t, fx["chat_v3"], "glm-5.2_advisor_doubao")
	dev, mx := variantNames(advisor)
	if dev != "glm-5.2__dev" {
		t.Errorf("advisor 槽位的 dev 变体名 = %q，期望 glm-5.2__dev（夹具原文）", dev)
	}
	if want := advisor.ConfigName + "__dev"; dev == want {
		t.Errorf("变体名竟然是拼出来的（%q）——夹具里它不是这个值", want)
	}
	if mx != "" {
		t.Errorf("advisor 槽位不该有 max 变体名，得到 %q", mx)
	}

	// search_agent_qwen_fast 的变体名是 search_agent_model__max（多个 config 共用一个）
	shared := findCfg(t, fx["solo_agent"], "search_agent_qwen_fast")
	dev2, mx2 := variantNames(shared)
	if dev2 != "search_agent_model__dev" || mx2 != "search_agent_model__max" {
		t.Errorf("共用变体名读成了 dev=%q max=%q，期望 search_agent_model__dev/__max", dev2, mx2)
	}
	if shared.ConfigName+"__max" == mx2 {
		t.Error("共用变体名被拼成了 ID+后缀，说明没读表")
	}
}

// TestMaxAvailableNeedsBiggerWindow 「有 __max 名」不等于「窗口更大」。
//
// 夹具里 search_agent_qwen_fast 的 context_window_tokens 是 dev=184000 且 max 为空，
// 也就是说这类条目开了 Max 也不增长。报 true 就是骗客户端。
func TestMaxAvailableNeedsBiggerWindow(t *testing.T) {
	fx := loadBatchFixture(t)

	// 真 1M：chat_v3 面的 glm-5.2（116000 → 1000000）
	g := findCfg(t, fx["chat_v3"], "glm-5.2")
	dev, mx := variantNames(g)
	mi := ModelInfo{ID: g.ConfigName, ContextWindow: g.ContextWindowTokens.Dev,
		ContextMax: g.ContextWindowTokens.Max, DevModelName: dev, MaxModelName: mx}
	if !mi.MaxAvailable() {
		t.Errorf("chat_v3 的 glm-5.2 应可用 Max：dev=%d max=%d 变体=%q",
			mi.ContextWindow, mi.ContextMax, mi.MaxModelName)
	}
	if mi.ContextWindow != 116000 || mi.ContextMax != 1000000 {
		t.Errorf("夹具口径变了：dev=%d max=%d（期望 116000/1000000）", mi.ContextWindow, mi.ContextMax)
	}

	// 面板默认面：solo_work_lite 的 glm-5.2 **只有 __dev**，上游根本没给 Max
	w := findCfg(t, fx["solo_work_lite"], "glm-5.2")
	wdev, wmx := variantNames(w)
	wm := ModelInfo{ID: w.ConfigName, ContextWindow: w.ContextWindowTokens.Dev,
		ContextMax: w.ContextWindowTokens.Max, DevModelName: wdev, MaxModelName: wmx}
	if wm.MaxAvailable() {
		t.Errorf("solo_work_lite 的 glm-5.2 不该报 Max 可用（变体=%q dev=%d max=%d）",
			wm.MaxModelName, wm.ContextWindow, wm.ContextMax)
	}
	if wmx != "" {
		t.Errorf("solo_work_lite 面不该读到 __max 变体名，得到 %q", wmx)
	}

	// 有 __max 名但窗口没变大 → 仍然 false
	s := findCfg(t, fx["solo_agent"], "search_agent_qwen_fast")
	sdev, smx := variantNames(s)
	sm := ModelInfo{ID: s.ConfigName, ContextWindow: s.ContextWindowTokens.Dev,
		ContextMax: s.ContextWindowTokens.Max, DevModelName: sdev, MaxModelName: smx}
	if smx == "" {
		t.Fatal("夹具该有 search_agent_model__max")
	}
	if sm.MaxAvailable() {
		t.Errorf("窗口没变大却报可用：dev=%d max=%d", sm.ContextWindow, sm.ContextMax)
	}
	// 只有名、没有更大窗口 → 不给名字（调用方据此判定「没有 Max」）
	onlyName := ModelInfo{ContextWindow: 200000, ContextMax: 200000, MaxModelName: "x__max"}
	if onlyName.MaxAvailable() {
		t.Error("ctx_max == ctx_dev 时不该报可用")
	}
	noName := ModelInfo{ContextWindow: 116000, ContextMax: 1000000}
	if noName.MaxAvailable() {
		t.Error("没有 __max 变体名时不该报可用（变体名不能拼，取不到就是没有）")
	}
}

// TestMetaReadsVariantsFromRealTable 单条配置进 meta() 后两种变体名与双口径都要在。
func TestMetaReadsVariantsFromRealTable(t *testing.T) {
	fx := loadBatchFixture(t)
	g := findCfg(t, fx["solo_agent"], "deepseek-v4.1-flash")
	m := g.meta()
	if m.DevName != "deepseek-v4.1-flash__dev" || m.MaxName != "deepseek-v4.1-flash__max" {
		t.Errorf("meta 没读出变体名: dev=%q max=%q", m.DevName, m.MaxName)
	}
	if m.Dev != 200000 || m.Max != 1000000 {
		t.Errorf("meta 双口径不对: dev=%d max=%d", m.Dev, m.Max)
	}
	if !m.HasDevEntry {
		t.Error("有 __dev 条目却没标记可调用实证")
	}
}

// TestMergeCatalogCarriesVariantNames 合并后 ModelInfo 上要带上变体名（出站靠它）。
func TestMergeCatalogCarriesVariantNames(t *testing.T) {
	fx := loadBatchFixture(t)
	cat := map[string]catalogMeta{}
	for _, g := range fx["chat_v3"] {
		cat[strings.ToLower(g.ConfigName)] = g.meta()
	}
	list := []ModelInfo{
		{ID: "glm-5.2", Name: "GLM-5.2", Function: Function},
		{ID: "kimi-k3", Name: "Kimi-K3", Function: Function},
	}
	got := mergeCatalog(list, cat, nil)
	byID := map[string]ModelInfo{}
	for _, m := range got {
		byID[m.ID] = m
	}
	if v := byID["glm-5.2"]; v.MaxModelName != "glm-5.2__max" || !v.MaxAvailable() {
		t.Errorf("glm-5.2 合并后应可 Max: max=%q avail=%v dev=%d max=%d",
			v.MaxModelName, v.MaxAvailable(), v.ContextWindow, v.ContextMax)
	}
	if v := byID["kimi-k3"]; v.MaxModelName != "kimi-k3__max" || !v.MaxAvailable() {
		t.Errorf("kimi-k3 合并后应可 Max: max=%q avail=%v", v.MaxModelName, v.MaxAvailable())
	}
}

// TestMergeCatalogDoesNotEraseSingleViewMax 批量视图那条目「没这份数据」时不能把单视图的 Max 清掉。
//
// 真实成因（2026-10-20 现场）：国服批量报文本体 1.56MB，超出当时 1MB 的读上限，整表退回
// 单 function 视图（那条路自己会带 context_window_tokens.max 与 __max 名）。批量视图里一部分
// 面的条目（solo_work_lite / solo_coder 全表 max==dev）本身就不声明更大的 max、也没有 __max
// 名——无条件覆盖会把「这条没有」当成「这里没有」，MaxAvailable() 从 true 翻 false，
// 表象是某些模型的 Max 开关凭空消失，而且只按模型丢，不是整表丢。
func TestMergeCatalogDoesNotEraseSingleViewMax(t *testing.T) {
	// 单视图：上游老接口如实给了 max 口径与 __max 变体名。
	list := []ModelInfo{{
		ID: "glm-5.2", Name: "GLM-5.2", Function: Function,
		ContextWindow: 200000, ContextMax: 1000000,
		DevModelName: "glm-5.2__dev", MaxModelName: "glm-5.2__max",
	}}
	// 批量视图：这条只有 dev 口径，没有 max、没有变体名（solo_work_lite 面的真实形状）。
	cat := map[string]catalogMeta{
		"glm-5.2": {ID: "glm-5.2", Visible: true, Label: "GLM-5.2", Dev: 200000, Max: 0},
	}
	got := mergeCatalog(list, cat, nil)
	m := got[0]
	if m.ContextMax != 1000000 {
		t.Errorf("批量视图没给 max 却把单视图的 Max 清成了 %d（应保留 1000000）", m.ContextMax)
	}
	if m.MaxModelName != "glm-5.2__max" {
		t.Errorf("批量视图没给变体名却清空了 MaxModelName（得到 %q）", m.MaxModelName)
	}
	if !m.MaxAvailable() {
		t.Error("合并后 MaxAvailable() 应为 true——这正是「某些模型 Max 开关消失」的形态")
	}
	if m.ContextWindow != 200000 {
		t.Errorf("dev 口径被改成了 %d", m.ContextWindow)
	}
}
