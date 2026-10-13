package quota

import "testing"

// 跨行毒化回归：旧正则的 .*? 会跨 </tr> 抓到别行的值
// （曾把 DeepSeek V4.1 Flash 解析成 Peak 行输入价 0.3）。
// 逐行解析后，普通行与加成行相邻也不得互相污染。
func TestParseNoCrossRowPoison(t *testing.T) {
	html := `<table><tbody>` +
		`<tr><td>Foo Model</td><td>$2.00</td><td>$6.00</td><td>$0.50</td><td>-</td><td>$15</td></tr>` +
		`<tr><td>DeepSeek V4.1 Flash (Off-Peak)</td><td>$0.15</td><td>$0.60</td><td>$0.003</td><td>-</td><td><del>$15</del> <strong>$60</strong><br><small>4x · 9 月 20 日结束</small></td></tr>` +
		`<tr><td>DeepSeek V4.1 Flash (Peak)</td><td>$0.30</td><td>$1.20</td><td>$0.006</td><td>-</td><td><del>$15</del> <strong>$60</strong><br><small>4x · 9 月 20 日结束</small></td></tr>` +
		`</tbody></table>`
	got := parsePerModelQuotas(html)
	if got["foo-model"] != 15 {
		t.Errorf("foo-model = %v, want 15", got["foo-model"])
	}
	if got["deepseek-v4.1-flash"] != 60 {
		t.Errorf("deepseek-v4.1-flash = %v, want 60", got["deepseek-v4.1-flash"])
	}
}

// 限时加成行必须取 <strong> 中的生效值，而非 <del> 中的原价。
func TestParsePromoQuotaStrongValue(t *testing.T) {
	html := `<ul><li><strong>5 小时限制</strong> — $12 的使用额度</li>` +
		`<li><strong>每周限制</strong> — $30 的使用额度</li>` +
		`<li><strong>每月限制</strong> — $60 的使用额度</li></ul>` +
		`<table><thead><tr><th>模型</th><th>输入</th><th>输出</th><th>缓存读取</th><th>缓存写入</th><th>每月限制</th></tr></thead><tbody>` +
		`<tr><td>DeepSeek V4.1 Flash (Off-Peak)</td><td>$0.15</td><td>$0.60</td><td>$0.003</td><td>-</td><td><del>$15</del> <strong>$60</strong><br><small>4x · 9 月 20 日结束</small></td></tr>` +
		`<tr><td>DeepSeek V4.1 Flash (Peak)</td><td>$0.30</td><td>$1.20</td><td>$0.006</td><td>-</td><td><del>$15</del> <strong>$60</strong><br><small>4x · 9 月 20 日结束</small></td></tr>` +
		`<tr><td>Omen Alpha</td><td>$0.20</td><td>$0.66</td><td>$0.04</td><td>-</td><td><strong>$100</strong></td></tr>` +
		`<tr><td>GLM-5.3-Flash</td><td>$0.15</td><td>$0.50</td><td>$0.03</td><td>-</td><td><strong>$60</strong></td></tr>` +
		`</tbody></table>`
	q, err := Parse(html)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	// 旧格式解析到全局 $ 金额，百分比字段保持默认 20/50
	if q.FiveHour != 12 || q.Weekly != 30 || q.Monthly != 60 {
		t.Errorf("global = 5h %v / week %v / month %v, want 12/30/60", q.FiveHour, q.Weekly, q.Monthly)
	}
	for model, want := range map[string]float64{
		"deepseek-v4.1-flash": 60, // 生效值 $60，而非原价 $15
		"omen-alpha":          100,
		"glm-5.3-flash":       60,
	} {
		if got := q.GetPerModel(model); got != want {
			t.Errorf("GetPerModel(%q) = %v, want %v (perModel=%v)", model, got, want, q.PerModel)
		}
	}
}

// 新版（2026-10 起）英文文档：无全局 $ 金额，只有百分比 + PlanTabs 双表
// （Go 套餐表在前，Go Plus 表在后不得污染）。
// fixture 结构与 https://opencode.ai/v2/docs/console/go/ 实测 HTML 一致。
func TestParseNewEnglishDocs(t *testing.T) {
	html := `<p>Each model has the following usage limits: 5-hour — 20% of the monthly limit;
weekly — 50%; and monthly — 100%.</p>` +
		`<div class="docs-plan-tabs"><div slot="go">` +
		`<table><thead><tr><th>Model</th><th>Input</th><th>Output</th><th>Cached Read</th><th>Cached Write</th><th>Monthly limit</th></tr></thead><tbody>` +
		`<tr><td>GLM-5.3-Flash</td><td>$0.15</td><td>$0.50</td><td>$0.03</td><td>-</td><td><strong>$60</strong></td></tr>` +
		`<tr><td>LongCat 2.5 Preview Free</td><td>Free</td><td>Free</td><td>Free</td><td>-</td><td><strong>Unlimited</strong><br/><small>limited time</small></td></tr>` +
		`<tr><td>DeepSeek V4.1 Flash (Off-Peak)</td><td>$0.15</td><td>$0.60</td><td>$0.003</td><td>-</td><td><strong>$80</strong></td></tr>` +
		`<tr><td>DeepSeek V4.1 Flash (Peak)</td><td>$0.30</td><td>$1.20</td><td>$0.006</td><td>-</td><td><strong>$80</strong></td></tr>` +
		`<tr><td>Grok 4.7 (≤ 200K tokens)</td><td>$2.00</td><td>$6.00</td><td>$0.50</td><td>-</td><td><strong>$15</strong></td></tr>` +
		`<tr><td>Grok 4.7 (&gt; 200K tokens)</td><td>$4.00</td><td>$12.00</td><td>$1.00</td><td>-</td><td><strong>$15</strong></td></tr>` +
		`<tr><td>MiMo-V2.6-Flash</td><td>$0.14</td><td>$0.28</td><td>$0.0028</td><td>-</td><td><strong>$60</strong></td></tr>` +
		`<tr><td>Claude Haiku 5.5 (≤ 100K tokens)</td><td>$0.10</td><td>$0.50</td><td>$0.01</td><td>$0.125</td><td><strong>$15</strong></td></tr>` +
		`</tbody></table></div>` +
		`<div slot="go-plus">` +
		`<table><tbody>` +
		`<tr><td>GLM-5.3-Flash</td><td>$0.15</td><td>$0.50</td><td>$0.03</td><td>-</td><td><strong>$180</strong></td></tr>` +
		`<tr><td>DeepSeek V4.1 Flash (Off-Peak)</td><td>$0.15</td><td>$0.60</td><td>$0.003</td><td>-</td><td><strong>$160</strong></td></tr>` +
		`</tbody></table></div></div>`
	q, err := Parse(html)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if q.FiveHourPct != 20 || q.WeeklyPct != 50 {
		t.Errorf("pct = 5h %v / week %v, want 20/50", q.FiveHourPct, q.WeeklyPct)
	}
	// 全局金额按 $60 × 百分比派生
	if q.Monthly != 60 || q.FiveHour != 12 || q.Weekly != 30 {
		t.Errorf("derived global = 5h %v / week %v / month %v, want 12/30/60", q.FiveHour, q.Weekly, q.Monthly)
	}
	for model, want := range map[string]float64{
		"glm-5.3-flash":       60, // Go 表，非 Go Plus 的 180
		"deepseek-v4.1-flash": 80, // 非 Go Plus 的 160
		"grok-4.7":            15, // 多档定价行归一，取首值
		"mimo-v2.6-flash":     60,
		"claude-haiku-5-5":    15,
	} {
		if got := q.GetPerModel(model); got != want {
			t.Errorf("GetPerModel(%q) = %v, want %v (perModel=%v)", model, got, want, q.PerModel)
		}
	}
	// Unlimited 行跳过，GetPerModel 回退 Monthly
	if got := q.GetPerModel("longcat-2.5-preview-free"); got != 60 {
		t.Errorf("Unlimited model fallback = %v, want 60", got)
	}
}
