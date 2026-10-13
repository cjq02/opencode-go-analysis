package quota

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// 旧版中文文档 /docs/zh-cn/go/ 已 302 到新版英文页（2026-10 实测）。
const docsURL = "https://opencode.ai/docs/zh-cn/go/"

// Quota 来自文档的额度（USD）。
// 新版文档（/v2/docs/console/go/）不再给出全局 5h/周/月 $ 金额，
// 而是「每模型各有月限额；5h = 月限额 20%、周 = 50%、月 = 100%」。
// FiveHour/Weekly/Monthly 为典型单模型口径（$60 × 百分比）供展示与汇总回退，
// 真正参与满额估算的是 PerModel + FiveHourPct/WeeklyPct。
type Quota struct {
	FiveHour float64 `json:"fiveHour"`
	Weekly   float64 `json:"weekly"`
	Monthly  float64 `json:"monthly"`
	// FiveHourPct/WeeklyPct 为 5h/周窗口占月限额的百分比（新版文档 20/50）
	FiveHourPct float64   `json:"fiveHourPct"`
	WeeklyPct   float64   `json:"weeklyPct"`
	Source      string    `json:"source"`
	FetchedAt   time.Time `json:"fetchedAt"`
	// PerModel 按模型归一化 key -> 月度使用额度 USD（来自“Monthly limit”列）
	PerModel map[string]float64 `json:"perModel"`
}

var Default = Quota{
	FiveHour: 12, Weekly: 30, Monthly: 60,
	FiveHourPct: 20, WeeklyPct: 50,
	Source: "hardcoded", PerModel: defaultPerModelQuotas,
}

// defaultPerModelQuotas 文档 Go 套餐表「Monthly limit」列的硬编码兜底
// （2026-10-13 与 /v2/docs/console/go/ 一致），避免文档抓取失败时
// GetPerModel 回退到 $60 导致“额度都变成 60”。
var defaultPerModelQuotas = map[string]float64{
	"omen-alpha":                 100,
	"glm-5.3-flash":              60,
	"glm-5.3":                    15,
	"glm-5.2":                    60,
	"kimi-k3":                    15,
	"kimi-k2.7-code":             60,
	"kimi-k2.6":                  60,
	"longcat-2.0":                60,
	"mimo-v2.6-flash":            60,
	"mimo-v2.6-pro":              15,
	"mimo-v2.5":                  60,
	"mimo-v2.5-pro":              15,
	"minimax-m3":                 60,
	"minimax-m2.7":               60,
	"muse-spark-1.3-contributor": 60,
	"muse-spark-1.2-contributor": 60,
	"qwen3.8-max":                15,
	"qwen3.8-flash":              30,
	"qwen3.7-plus":               60,
	"deepseek-v4-pro":            15,
	"deepseek-v4-flash":          30,
	// 限时 4x 提升后文档现为 $80（2026-10-13 实测）
	"deepseek-v4.1-flash":          80,
	"deepseek-v4-flash-vision-exp": 15,
	"hy4-preview":                  30,
	"hy3":                          60,
	"space-bunny":                  30,
	"grok-4.7":                     15,
	"grok-4.6":                     15,
	"gpt-6-luna":                   15,
	"gpt-5.6-luna":                 15,
	"claude-haiku-5-5":             15,
	// 文档显示名归一化产物（点保留），与上面的官方 ID 双写兜底
	"claude-haiku-5.5": 15,
}

// Fetch 爬取文档解析额度，失败返回 error，调用方可回退 Default
func Fetch(ctx context.Context) (Quota, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", docsURL, nil)
	req.Header.Set("User-Agent", "opencode-go-analysis/1.0")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return Quota{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return Quota{}, fmt.Errorf("status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return Quota{}, err
	}
	return Parse(string(b))
}

var (
	re5h      = regexp.MustCompile(`5\s*小时限制[^0-9]*([0-9]+(?:\.[0-9]+)?)\s*美元`)
	reWeekly  = regexp.MustCompile(`每周限制[^0-9]*([0-9]+(?:\.[0-9]+)?)\s*美元`)
	reMonthly = regexp.MustCompile(`每月限制[^0-9]*([0-9]+(?:\.[0-9]+)?)\s*美元`)
	// 旧版中文列表格式：<li><strong>5 小时限制</strong> — $12 的使用额度</li>
	// 必须要求 </strong> 避免误匹配表头 <th>每月限制</th> 到首个价格 $0.20
	re5hNew      = regexp.MustCompile(`5\s*小时限制\s*</strong>[^$0-9]{0,20}\$([0-9]+(?:\.[0-9]+)?)`)
	reWeeklyNew  = regexp.MustCompile(`每周限制\s*</strong>[^$0-9]{0,20}\$([0-9]+(?:\.[0-9]+)?)`)
	reMonthlyNew = regexp.MustCompile(`每月限制\s*</strong>[^$0-9]{0,20}\$([0-9]+(?:\.[0-9]+)?)`)
	// 新版英文文档（2026-10 起）：无全局 $ 金额，只有占月限额的百分比
	// "Each model has the following usage limits: 5-hour — 20% of the monthly limit;
	//  weekly — 50%; and monthly — 100%."
	re5hPct     = regexp.MustCompile(`5-hour\s*[—–-]\s*(\d+)\s*%`)
	reWeeklyPct = regexp.MustCompile(`weekly\s*[—–-]\s*(\d+)\s*%`)
)

func findQuota(reNew, reOld *regexp.Regexp, html string) string {
	if m := reNew.FindStringSubmatch(html); m != nil {
		return m[1]
	}
	if m := reOld.FindStringSubmatch(html); m != nil {
		return m[1]
	}
	return ""
}

// Parse 解析文档额度。新版文档只有「每模型月限额 + 5h/周百分比」，
// 旧版中文文档还有全局 $12/$30/$60 —— 两者兼容：
//   - 全局 $ 金额可选：旧格式直接取；新格式由 Monthly × 百分比派生
//   - 百分比可选：未解析到时按文档默认 20/50
//   - 只要 per-model 表有结果即成功；两者皆空才报错
func Parse(html string) (Quota, error) {
	perModel := parsePerModelQuotas(html)
	// 全局 $ 金额（旧版中文文档）
	s5 := findQuota(re5hNew, re5h, html)
	sW := findQuota(reWeeklyNew, reWeekly, html)
	sM := findQuota(reMonthlyNew, reMonthly, html)
	// 5h/周占月限额百分比（新版英文文档；旧版无此文案时用默认）
	pct5, pctW := 20.0, 50.0
	if m := re5hPct.FindStringSubmatch(html); m != nil {
		fmt.Sscan(m[1], &pct5)
	}
	if m := reWeeklyPct.FindStringSubmatch(html); m != nil {
		fmt.Sscan(m[1], &pctW)
	}
	q := Quota{
		FiveHourPct: pct5,
		WeeklyPct:   pctW,
		Monthly:     60, // 典型单模型月限额（Go 套餐多数模型 $60），仅作展示/汇总回退
		Source:      docsURL,
		FetchedAt:   time.Now(),
	}
	// 旧格式解析到的全局金额优先（语义即全局口径）；新格式按 $60 × 百分比派生
	if s5 != "" && sW != "" && sM != "" {
		fmt.Sscan(s5, &q.FiveHour)
		fmt.Sscan(sW, &q.Weekly)
		fmt.Sscan(sM, &q.Monthly)
	} else {
		q.FiveHour = q.Monthly * pct5 / 100
		q.Weekly = q.Monthly * pctW / 100
	}
	q.PerModel = perModel
	if len(q.PerModel) == 0 {
		if s5 == "" && sW == "" && sM == "" {
			return Quota{}, fmt.Errorf("parse quota failed: no per-model table and no global limits")
		}
		q.PerModel = defaultPerModelQuotas
	}
	return q, nil
}

func parsePerModelQuotas(html string) map[string]float64 {
	// 先按行切分再逐行匹配：单元格里的 .*? 绝不能跨 </tr>，
	// 否则遇到异形行（如 V4.1 的 <del>/<strong> 行）会跨行抓到别行的值毒化结果。
	reTR := regexp.MustCompile(`(?s)<tr>(.*?)</tr>`)
	// 限时加成行：末列形如 <del>$15</del> <strong>$60</strong>（如 DeepSeek V4.1 Flash 4x），
	// 取 <strong> 中的当前生效值，优先于常规解析。
	// 中间价格列用 [^<]*（纯文本），杜绝跨单元格/跨行。
	reRowStrong := regexp.MustCompile(`\A\s*<td[^>]*>(.*?)</td>(?:\s*<td[^>]*>[^<]*</td>){4}\s*<td[^>]*>.*?<strong>\$([0-9]+(?:\.[0-9]+)?)</strong>.*?</td>`)
	// 常规行：末列兼容 <td><strong>$60</strong></td> 新版格式。
	reRow := regexp.MustCompile(`\A\s*<td[^>]*>(.*?)</td>(?:\s*<td[^>]*>[^<]*</td>){4}\s*<td[^>]*>(?:<[^>]+>)*\$([0-9]+(?:\.[0-9]+)?)(?:<[^>]+>)*\s*</td>`)
	out := map[string]float64{}
	collect := func(m []string) {
		rawModel := stripTags(m[1])
		val := m[2]
		var usd float64
		fmt.Sscan(val, &usd)
		key := normalizeModel(rawModel)
		if key == "" {
			return
		}
		// 同一模型多行（如 Off-Peak/Peak）取首值；加成行的生效值已优先写入，不被覆盖
		if _, exists := out[key]; !exists {
			out[key] = usd
		}
		// digit.digit ↔ digit-digit 变体一并登记（如 claude-haiku-5.5 ↔ claude-haiku-5-5），
		// 使 DB 里的官方模型 ID 无需依赖 GetPerModel 运行时兜底也能命中。
		if alt := digitSepVariant(key); alt != key {
			if _, exists := out[alt]; !exists {
				out[alt] = usd
			}
		}
	}
	for _, m := range reTR.FindAllStringSubmatch(html, -1) {
		tr := m[1]
		if mm := reRowStrong.FindStringSubmatch(tr); mm != nil {
			collect(mm)
			continue
		}
		if mm := reRow.FindStringSubmatch(tr); mm != nil {
			collect(mm)
		}
	}
	return out
}

func stripTags(s string) string {
	re := regexp.MustCompile(`<[^>]+>`)
	return strings.TrimSpace(re.ReplaceAllString(s, ""))
}

func normalizeModel(s string) string {
	// 去掉括号及内容，如 "DeepSeek V4 Flash (Off-Peak)" -> "DeepSeek V4 Flash"
	if idx := strings.Index(s, "("); idx >= 0 {
		s = s[:idx]
	}
	s = strings.TrimSpace(s)
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, " ", "-")
	s = strings.ReplaceAll(s, "_", "-")
	// 合并多余 -
	reDash := regexp.MustCompile(`-+`)
	s = reDash.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	return s
}

// DeepSeek 文档定价（https://opencode.ai/docs/zh-cn/go/，每 1M tokens 美元）。
// 峰时：周一至周五 01:00-04:00 / 06:00-10:00 UTC（即北京时间 09:00-12:00 / 14:00-18:00），
// 其余时段（含周末）为谷时；峰价约为谷价 2 倍。
type DeepSeekPrice struct {
	Input     float64
	Output    float64
	CacheRead float64
	QuotaUSD  float64
}

var DeepSeekDocs = map[string]DeepSeekPrice{
	"deepseek-v4-flash (Off-Peak)":            {Input: 0.22, Output: 0.66, CacheRead: 0.007, QuotaUSD: 30},
	"deepseek-v4-flash (Peak)":                {Input: 0.44, Output: 1.32, CacheRead: 0.014, QuotaUSD: 30},
	"deepseek-v4-flash-vision-exp (Off-Peak)": {Input: 0.22, Output: 0.66, CacheRead: 0.007, QuotaUSD: 15},
	"deepseek-v4-flash-vision-exp (Peak)":     {Input: 0.44, Output: 1.32, CacheRead: 0.014, QuotaUSD: 15},
	"deepseek-v4-pro (Off-Peak)":              {Input: 0.66, Output: 1.98, CacheRead: 0.022, QuotaUSD: 15},
	"deepseek-v4-pro (Peak)":                  {Input: 1.32, Output: 3.96, CacheRead: 0.044, QuotaUSD: 15},
}

// PeakHoursNote 峰时段说明（北京时间）
const PeakHoursNote = "峰时：周一至周五 北京时间 09:00-12:00 / 14:00-18:00（UTC 01:00-04:00 / 06:00-10:00），其余为谷时"

var (
	reDigitDot  = regexp.MustCompile(`(\d)\.(\d)`)
	reDigitDash = regexp.MustCompile(`(\d)-(\d)`)
)

// digitSepVariant 在 digit.digit 与 digit-digit 之间互换。
// 官方模型 ID 有时代号用 - 代 .（claude-haiku-5-5），文档显示名则保留点
// （"Claude Haiku 5.5" → claude-haiku-5.5），需双向兜底。
func digitSepVariant(s string) string {
	if reDigitDot.MatchString(s) {
		return reDigitDot.ReplaceAllString(s, "$1-$2")
	}
	return reDigitDash.ReplaceAllString(s, "$1.$2")
}

// GetPerModel 返回归一化后的按模型额度，未命中则回退 monthly 通用额度
func (q Quota) GetPerModel(model string) float64 {
	key := normalizeModel(model)
	if v, ok := q.PerModel[key]; ok {
		return v
	}
	// 尝试去掉版本号后的匹配，如 deepseek-v4-flash
	if v, ok := q.PerModel[strings.ToLower(model)]; ok {
		return v
	}
	// digit.digit ↔ digit-digit 变体（claude-haiku-5-5 ↔ claude-haiku-5.5）
	if v, ok := q.PerModel[digitSepVariant(key)]; ok {
		return v
	}
	if q.Monthly != 0 {
		return q.Monthly
	}
	return Default.Monthly
}
