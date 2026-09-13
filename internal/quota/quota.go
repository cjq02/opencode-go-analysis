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

const docsURL = "https://opencode.ai/docs/zh-cn/go/"

// Quota 来自文档的额度（USD）
type Quota struct {
	FiveHour float64 `json:"fiveHour"`
	Weekly   float64 `json:"weekly"`
	Monthly  float64 `json:"monthly"`
	Source   string  `json:"source"`
	FetchedAt time.Time `json:"fetchedAt"`
	// PerModel 按模型归一化 key -> 月度使用额度 USD（来自“使用额度”列）
	PerModel map[string]float64 `json:"perModel"`
}

var Default = Quota{FiveHour: 12, Weekly: 30, Monthly: 60, Source: "hardcoded", PerModel: defaultPerModelQuotas}

// defaultPerModelQuotas 文档表格的硬编码兜底（与 docs/zh-cn/go/ 每月限制列一致），
// 避免文档抓取失败时 GetPerModel 回退到 $60 导致“额度都变成 60”。
var defaultPerModelQuotas = map[string]float64{
	"omen-alpha":                 100,
	"glm-5.3-flash":              60,
	"glm-5.3":                    15,
	"glm-5.2":                    60,
	"glm-5.1":                    60,
	"kimi-k3":                    15,
	"kimi-k2.7-code":             60,
	"kimi-k2.6":                  60,
	"longcat-2.0":                60,
	"mimo-v2.5":                  60,
	"mimo-v2.5-pro":              15,
	"minimax-m3":                 60,
	"minimax-m2.7":               60,
	"minimax-m2.5":               60,
	"muse-spark-1.3-contributor": 60,
	"muse-spark-1.2-contributor": 60,
	"qwen3.8-max":                15,
	"qwen3.8-flash":              30,
	"qwen3.7-max":                30,
	"qwen3.7-plus":               60,
	"qwen3.6-plus":               60,
	"deepseek-v4-pro":            15,
	"deepseek-v4-flash":          30,
	"deepseek-v4-flash-vision-exp": 15,
	"hy4-preview":                30,
	"hy3":                        60,
	"grok-4.6":                   15,
	"gpt-5.6-luna":               15,
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
	re5h     = regexp.MustCompile(`5\s*小时限制[^0-9]*([0-9]+(?:\.[0-9]+)?)\s*美元`)
	reWeekly = regexp.MustCompile(`每周限制[^0-9]*([0-9]+(?:\.[0-9]+)?)\s*美元`)
	reMonthly = regexp.MustCompile(`每月限制[^0-9]*([0-9]+(?:\.[0-9]+)?)\s*美元`)
	// 新版文档格式：<li><strong>5 小时限制</strong> — $12 的使用额度</li>
	// 必须要求 </strong> 避免误匹配表头 <th>每月限制</th> 到首个价格 $0.20
	re5hNew     = regexp.MustCompile(`5\s*小时限制\s*</strong>[^$0-9]{0,20}\$([0-9]+(?:\.[0-9]+)?)`)
	reWeeklyNew = regexp.MustCompile(`每周限制\s*</strong>[^$0-9]{0,20}\$([0-9]+(?:\.[0-9]+)?)`)
	reMonthlyNew = regexp.MustCompile(`每月限制\s*</strong>[^$0-9]{0,20}\$([0-9]+(?:\.[0-9]+)?)`)
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

func Parse(html string) (Quota, error) {
	s5 := findQuota(re5hNew, re5h, html)
	sW := findQuota(reWeeklyNew, reWeekly, html)
	sM := findQuota(reMonthlyNew, reMonthly, html)
	if s5 == "" || sW == "" || sM == "" {
		return Quota{}, fmt.Errorf("parse quota failed")
	}
	var q Quota
	fmt.Sscan(s5, &q.FiveHour)
	fmt.Sscan(sW, &q.Weekly)
	fmt.Sscan(sM, &q.Monthly)
	q.Source = docsURL
	q.FetchedAt = time.Now()
	q.PerModel = parsePerModelQuotas(html)
	if len(q.PerModel) == 0 {
		q.PerModel = defaultPerModelQuotas
	}
	return q, nil
}

func parsePerModelQuotas(html string) map[string]float64 {
	// 匹配“使用额度”列所在表格：提取所有 <tr><td>模型</td>...<td>$XX</td></tr>
	// 简化：全局匹配 <td>模型名</td> ... <td>$数字</td> 连续 6 列的表格
	// 末列兼容 <td><strong>$60</strong></td> 新版格式
	reRow := regexp.MustCompile(`<tr>\s*<td[^>]*>(.*?)</td>\s*<td[^>]*>.*?</td>\s*<td[^>]*>.*?</td>\s*<td[^>]*>.*?</td>\s*<td[^>]*>.*?</td>\s*<td[^>]*>(?:<[^>]+>)*\$([0-9]+(?:\.[0-9]+)?)(?:<[^>]+>)*\s*</td>`)
	matches := reRow.FindAllStringSubmatch(html, -1)
	out := map[string]float64{}
	for _, m := range matches {
		rawModel := stripTags(m[1])
		val := m[2]
		var usd float64
		fmt.Sscan(val, &usd)
		key := normalizeModel(rawModel)
		if key == "" {
			continue
		}
		// 同一模型多行（如 Off-Peak/Peak）取最大值或首值，这里取首值一致则覆盖无影响
		if _, exists := out[key]; !exists {
			out[key] = usd
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
	if q.Monthly != 0 {
		return q.Monthly
	}
	return Default.Monthly
}
