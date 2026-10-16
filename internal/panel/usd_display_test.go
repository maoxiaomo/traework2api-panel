package panel

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// usd_display_test.go 国际版「美元额度」显示层的断言。
//
// 背景：国际版是**美元**计费（上游 is_dollar_usage_billing=true，Free plan 是
// basic_usage_limit=1 即 $1），而积分字段恒 0。旧代码拿积分字段渲染，面板显示成
// 「0 / 无额度」——用户看到的就是那个 0。这里把显示层的纯函数抠出来喂 node 断言：
//
//	① 金额格式化：$0.00059 这种 5 位小数不许被砍成 0（砍了就回到「没花钱」的错觉）
//	② 额度单元格：美元号只给一行「剩余 / 额度」（已用量与百分比在悬停提示里），
//	   且**不能再出现裸 0**
//	③ 国内号渲染逐字不变（零回归）
//
// 汇总卡（`renderPackages`）碰 DOM，没法直接跑，只能做源码级守卫。
func TestUSDCreditDisplay(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("无 node，跳过纯函数断言")
	}
	raw, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)

	driver := `
const assert = require('assert');
` + extractJSFunc(t, src, "esc") + "\n" +
		extractJSFunc(t, src, "fmtTok") + "\n" +
		extractJSFunc(t, src, "isUSD") + "\n" +
		extractJSFunc(t, src, "fmtUSD") + "\n" +
		extractJSFunc(t, src, "usdRemain") + "\n" +
		extractJSFunc(t, src, "credTitle") + "\n" +
		extractJSFunc(t, src, "credCell") + `
// ① 美元金额：小数点后 5 位是真实存在的量级（实测已用 $0.00059）
assert.strictEqual(fmtUSD(1), '$1.00', '整美元要带两位小数，别显示成裸 1');
assert.strictEqual(fmtUSD(0.99941), '$0.99941');
assert.strictEqual(fmtUSD(0.00059), '$0.00059', '5 位小数不许被砍成 $0.00');
assert.strictEqual(fmtUSD(0), '$0.00');
assert.strictEqual(fmtUSD(undefined), '$—', '缺值要显示成未知，不能显示成 $0.00');
assert.strictEqual(fmtUSD('abc'), '$—');
assert.strictEqual(fmtUSD(20), '$20.00', 'Pro 档额度也是这套格式化');

// ② 剩余美元
assert.strictEqual(usdRemain({ limit_usd: 1, used_usd: 0.00059 }), 1 - 0.00059);
assert.strictEqual(usdRemain({ limit_usd: 1, used_usd: 2 }), 0, '超支要钳 0，不许出负数');
assert.strictEqual(usdRemain({}), 0);

// ③ 额度单元格：美元号
const intl = { credit_unit: 'usd', credits: 0, used_usd: 0.00059, limit_usd: 1 };
const intlCell = credCell(intl);
assert.ok(intlCell.includes('$0.99941'), '美元号要显示剩余美元，实得: ' + intlCell);
assert.ok(intlCell.includes('/$1.00'), '美元号要显示额度分母，实得: ' + intlCell);
// 只许一行：单元格里再压「已用 … · 剩 …%」是与「剩余/总额」重复的小字（用户 2026-10-16 要求去掉），
// 这两项信息仍在悬停提示 credTitle 里。
assert.ok(!/class="note"/.test(intlCell), '美元单元格不该再有第二行小字，实得: ' + intlCell);
assert.ok(!intlCell.includes('已用') && !/%/.test(intlCell), '已用与百分比只留在提示里，实得: ' + intlCell);
// 这是用户实际看到的那个 bug：拿积分字段渲染成裸 0
assert.ok(!/>0<\/div>/.test(intlCell.replace(/<span[^>]*>[\s\S]*?<\/span>/g, '')),
  '美元号不能再出现裸 0（那正是「国际版显示 0」的观感）: ' + intlCell);

// ④ 国内号渲染逐字不变
const cnCell = credCell({ credits: 5466, credits_total: 5900 });
assert.ok(cnCell.includes('>5466<') && cnCell.includes('/5900'), '国内号要照旧显示 剩余/总额，实得: ' + cnCell);
assert.ok(!cnCell.includes('$'), '国内号不该出现美元符号，实得: ' + cnCell);

// ⑤ 坏输入不许渲染出 NaN/undefined
for (const v of [undefined, null, 'abc', NaN, {}, { credit_unit: 'usd' }]) {
  const s = credCell(v);
  assert.ok(!/NaN|undefined|Infinity/.test(s), '坏渲染: ' + JSON.stringify(v) + ' → ' + s);
}

// ⑥ 国际号还没取到美元数（刷新前/新导入）：不许退回积分口径的裸 0——0 会被读成「无限量」。
//    列头叫「额度」，所以这一格要么是美元额度，要么是「—待刷新」。
const intlNoData = { realm: 'intl', credits: 0 };
const intlNoDataCell = credCell(intlNoData);
assert.ok(!/>0<\//.test(intlNoDataCell), '国际号没取到美元数时不许显示裸 0: ' + intlNoDataCell);
assert.ok(intlNoDataCell.includes('待刷新'), '国际号没取到美元数要提示待刷新: ' + intlNoDataCell);
assert.ok(!intlNoDataCell.includes('$'), '没有美元数时不许编出金额: ' + intlNoDataCell);
assert.ok(!/积分/.test(intlNoDataCell), '国际号的格子不许出现「积分」: ' + intlNoDataCell);
assert.ok(/额度/.test(credTitle(intlNoData)) && /不是无限量/.test(credTitle(intlNoData)),
  '国际号提示要说清「额度」并且别让人读成无限量: ' + credTitle(intlNoData));
`
	out, err := exec.Command(node, "-e", driver).CombinedOutput()
	if err != nil {
		t.Fatalf("node 断言失败: %v\n%s", err, out)
	}
}

// TestPackagesSummaryIsUnitAware 汇总卡（截图里那张「0 · 共 0 · 1 个包 · 占最高 0%」的卡）
// 必须走口径分支。
//
// 它碰 DOM 所以跑不起来，只能做源码级守卫——上一轮就是漏了这张卡：明细表、KPI、到期图都改了，
// 汇总卡还在读积分的 remain/size，于是国际号大数字仍是 0。
func TestPackagesSummaryIsUnitAware(t *testing.T) {
	raw, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := strings.ReplaceAll(string(raw), string(rune(13))+string(rune(10)), string(rune(10)))
	body := extractJSFunc(t, src, "renderPackages")

	for _, want := range []string{
		"isUSD(a)",     // 按账号判口径
		"fmtUSD",       // 美元号的分支里要用美元格式化
		"usdRemain(a)", // 主数字取剩余美元
		"limit_usd",    // 分母取美元额度
		"used_usd",     // 已用量取美元已用
		"maxRemainOf",  // 「占最高 N%」只在同口径内比较
		"按量计费",         // 卡片上标明这不是积分
	} {
		if !strings.Contains(body, want) {
			t.Errorf("汇总卡 renderPackages 里找不到 %q —— 美元号会退回积分口径（会显示成 0）", want)
		}
	}

	// 分组折叠按钮的重排文本也要按口径格式化（否则展开时美元金额被 fmtTok 印成 0）
	toggle := src[strings.Index(src, "pk-group-toggle"):]
	if !strings.Contains(toggle, "data-unit") {
		t.Error("分组按钮没带 data-unit，展开时的金额会用积分格式重排")
	}
	if !strings.Contains(toggle, "btn.dataset.unit === 'usd' ? fmtUSD : fmtTok") {
		t.Error("分组按钮的重排没按 data-unit 选格式化函数")
	}
}

// TestAccountsColumnHeaderSaysQuota 账号表列头必须是「额度」。
//
// 国际版那一格是**美元额度**（积分字段恒 0），列头写「积分」时那个 0 会被读成
// 「没额度 / 无限量」——用户就是这么读错的。列头叫额度，两种口径都成立。
func TestAccountsColumnHeaderSaysQuota(t *testing.T) {
	raw, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	if !strings.Contains(src, "<th>额度</th>") {
		t.Error("账号表列头里找不到 <th>额度</th> —— 国际版那一列要按额度显示")
	}
	if strings.Contains(src, "<th>积分</th>") {
		t.Error("账号表列头还写着 <th>积分</th> —— 国际号的 0 会被读成无限量")
	}
}
