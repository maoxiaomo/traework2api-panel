// pricing.go TRAE 单请求积分推算（**估算**，不是上游回报值）。
//
// 上游 token_usage 帧里没有积分字段（实测只有 token 数），所以单请求积分只能按官方计费
// 公式推算，展示层一律带「≈」。推算值与单价表的出处、实测方法见下，改价先读这一段。
//
// 公式（官方 docs.trae.cn/enterprise_billing-items）：
//
//	积分 = (输入 token − 缓存命中 token) × 输入单价
//	     + 输出 token × 输出单价
//	     + 缓存命中 token × 缓存单价
//
// 单价单位「积分 / 百万 token」，由官方刊例价（元 / 百万）乘 40 得积分；部分模型有账号
// 身份补贴 / 限时活动 / 闲时折扣，刊例价会高估，走 discount/override 两档修正。
//
// 数据来源：RobbsLuo/Coding2API 的 src/provider/trae/pricing.py（MIT），由其
// scripts/probe_trae_credit_rate.py 实测反推（探额度 → 发一次最小对话 → 再探额度，
// 差值 / token 数 = 该模型积分/百万；多模型多轮拟合）。**那是他家账号的实测值**，
// 账号身份不同（会员补贴 / 闲时折扣）会变 → 本仓用 TestProbeLiveCreditRate 自查，
// 对不上就改本文件的表，不要改公式。
package usage

import "strings"

// listPricesCNY 官方刊例价（元 / 百万 token）：(输入, 输出, 缓存命中)。
// key = TRAE 模型 id 小写。
var listPricesCNY = map[string][3]float64{
	"doubao-seed-evolving":       {6.0, 30.0, 1.2},
	"doubao-seed-2.1-pro":        {6.0, 30.0, 1.2},
	"doubao-seed-2.1-turbo":      {3.0, 15.0, 0.6},
	"doubao-seed-2.0-code":       {3.2, 16.0, 0.64},
	"step-5-preview":             {7.0, 20.0, 0.35},
	"glm-5.3-flash":              {0.8, 2.8, 0.23},
	"glm-5.3":                    {8.0, 28.0, 2.0},
	"glm-5.2":                    {8.0, 28.0, 2.0},
	"glm-5":                      {4.0, 18.0, 1.0},
	"minimax-m3":                 {2.1, 8.4, 0.42},
	"qwen3.8-max":                {12.0, 36.0, 2.4},
	"qwen-3.7-plus":              {2.0, 8.0, 0.4},
	"kimi-k3":                    {20.0, 100.0, 2.0},
	"kimi-k2.8-preview":          {6.5, 27.0, 1.7},
	"kimi-k2.7-code":             {6.5, 27.0, 1.3},
	"kimi-k2.6":                  {6.5, 27.0, 1.3}, // 官方未单列，实测系数同 k2.7-code
	"deepseek-v4.1-flash":        {2.0, 8.0, 0.04},
	"deepseek-v4-pro":            {4.8, 9.6, 0.4}, // 活动价
	"deepseek-v4-pro-official":   {9.0, 27.0, 0.3},
	"deepseek-v4-flash":          {3.0, 9.0, 0.1},
	"deepseek-v4-flash-official": {3.0, 9.0, 0.1},
	// ── 以下 2026-10-27 补齐：上游 /v1/models 有、本表缺失的模型 ──
	// 无官方刊例，按 credit_rate / glm-5.3(0.78) 比例从 glm-5.3 刊例(8/28/2)推算；
	// 与上游 credit_rate 同源（都是 Trae 定价的相对系数），误差待实测校准。
	"glm-5.1":            {8.5, 29.8, 2.13},  // rate 0.83
	"glm-5.3-flashx":     {3.2, 7.1, 0.8},    // rate 0.31；输出 7.1 为探针实测（两次独立一致 284积分/M），含 -max 同价
	"glm-5v-turbo":       {4.0, 18.0, 1.0},   // rate 缺失，按 glm-5 近似
	"kimi-k2.5":          {6.5, 27.0, 1.3},   // rate 缺失，按 k2.6 近似
	"kimi-k3.1-pro":      {12.3, 43.1, 3.08}, // rate 1.2（含 -max 同价）
	"mimo-v2.6-flash":    {0.6, 2.1, 0.15},   // rate 0.06（含 -max 同价）
	"mimo-v2.6-pro":      {1.5, 5.4, 0.38},   // rate 0.15（含 -max 同价）
	"minimax-m2.7":       {2.8, 9.7, 0.69},   // rate 0.27（含 -max 同价）
	"minimax-m3.1-flash": {0.9, 3.2, 0.23},   // rate 0.09（含 -max 同价）
	"qwen-3.5":           {2.7, 9.3, 0.67},   // rate 0.26
	"qwen-3.6-plus":      {2.7, 9.3, 0.67},   // rate 0.26
	"qwen3.8-flash":      {0.8, 2.9, 0.21},   // rate 0.08（含 -max 同价）
	"doubao-seed-code":   {0.6, 2.1, 0.15},   // rate 0.06
}

// creditsPerYuan 元 → 积分的换算（官方元价 × 40 = 积分 / 百万）。
const creditsPerYuan = 40.0

// measuredDiscount 实测有效折扣（官方价 × 40 × 折扣）。
// glm-5.2/5.3：会员专属补贴，实测 0.675（输入/输出/缓存一致）。
var measuredDiscount = map[string]float64{
	"glm-5.2": 0.675,
	"glm-5.3": 0.675,
}

// measuredOverride 实测有效价（积分 / 百万），优先于上面两档——刊例 × 折扣与实际对不上时用。
//   - deepseek-v4.1-flash：缓存命中价是刊例的 ~1.75 倍；该模型缓存占输入 ≈99%，
//     按刊例算会把整体积分低估约一半（实测全量 229 → 449）。
//   - doubao-seed-2.1-pro：实测三档同比例约为刊例的 0.104，数值可疑但稳定，先按实测。
var measuredOverride = map[string][3]float64{
	"deepseek-v4.1-flash": {28.0, 112.0, 2.8},
	"doubao-seed-2.1-pro": {24.87, 124.4, 4.93},
}

// modelKey 把网关的模型标识归一成价目表的键：去空白、转小写，并剥掉 CN 域前缀。
//
// 为什么要剥 "cn:"：同一条请求在网关里有两种入账形态——resolveModel 之后是裸名
// （glm-5.2），而按入站原样记录时带域前缀（cn:glm-5.2）。不归一就会出现「同一个模型
// 一半请求有积分估算、一半没有」的错觉。**intl: 不剥**：国际版与 CN 的计费口径不同，
// 价目表里只有人民币价，拿 CN 价去估国际版等于编数（宁可不估）。
func modelKey(model string) string {
	key := strings.ToLower(strings.TrimSpace(model))
	key = strings.TrimPrefix(key, "cn:")
	// -max 变体与基础版同价（上游 credit_rate 相同，max 只扩上下文不改单价）：
	// 剥掉后缀让 kimi-k3-max 等落到基础版价目上，避免整类请求积分显示 —。
	// qwen3.8-max 本名以 -max 结尾，不能误剥——只剥「基名也是表内模型」的后缀。
	if strings.HasSuffix(key, "-max") {
		base := strings.TrimSuffix(key, "-max")
		if _, ok := listPricesCNY[base]; ok {
			key = base
		}
	}
	return key
}

// EffectivePrices 模型 → 积分单价 (输入, 输出, 缓存命中) / 百万 token；未收录返回 false。
// 优先级：实测覆盖 > 刊例 × 40 × 折扣 > 刊例 × 40。
func EffectivePrices(model string) ([3]float64, bool) {
	key := modelKey(model)
	if v, ok := measuredOverride[key]; ok {
		return v, true
	}
	entry, ok := listPricesCNY[key]
	if !ok {
		return [3]float64{}, false
	}
	f := creditsPerYuan * discountOf(key)
	return [3]float64{entry[0] * f, entry[1] * f, entry[2] * f}, true
}

func discountOf(key string) float64 {
	if d, ok := measuredDiscount[key]; ok {
		return d
	}
	return 1.0
}

// EstimateCredit 按官方公式推算一组 token 的积分（≈，4 位小数）。
// 模型未收录或没给输入 token 时返回 false —— 宁可不显示，也不拿错单价误导。
// cached 会被夹进 [0, prompt]：上游没报时传 0。
func EstimateCredit(model string, prompt, completion, cached int64) (float64, bool) {
	prices, ok := EffectivePrices(model)
	if !ok || prompt < 0 {
		return 0, false
	}
	if cached < 0 {
		cached = 0
	}
	if cached > prompt {
		cached = prompt
	}
	if completion < 0 {
		completion = 0
	}
	total := float64(prompt-cached)*prices[0] + float64(completion)*prices[1] + float64(cached)*prices[2]
	return round4(total / 1e6), true
}

// round4 保留 4 位小数：计费量子为 0.0004，4 位足够，也避免浮点长尾。
func round4(v float64) float64 {
	return float64(int64(v*10000+0.5)) / 10000
}
