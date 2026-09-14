package quota

import "testing"

// 跨行毒化回归：旧正则的 .*? 会跨 </tr> 抓到别行的值
//（曾把 DeepSeek V4.1 Flash 解析成 Peak 行输入价 0.3）。
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
