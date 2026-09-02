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

var Default = Quota{FiveHour: 12, Weekly: 30, Monthly: 60, Source: "hardcoded", PerModel: map[string]float64{}}

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
	reMonthly= regexp.MustCompile(`每月限制[^0-9]*([0-9]+(?:\.[0-9]+)?)\s*美元`)
)

func Parse(html string) (Quota, error) {
	m5 := re5h.FindStringSubmatch(html)
	mW := reWeekly.FindStringSubmatch(html)
	mM := reMonthly.FindStringSubmatch(html)
	if m5 == nil || mW == nil || mM == nil {
		return Quota{}, fmt.Errorf("parse quota failed")
	}
	var q Quota
	fmt.Sscan(m5[1], &q.FiveHour)
	fmt.Sscan(mW[1], &q.Weekly)
	fmt.Sscan(mM[1], &q.Monthly)
	q.Source = docsURL
	q.FetchedAt = time.Now()
	q.PerModel = parsePerModelQuotas(html)
	return q, nil
}

func parsePerModelQuotas(html string) map[string]float64 {
	// 匹配“使用额度”列所在表格：提取所有 <tr><td>模型</td>...<td>$XX</td></tr>
	// 简化：全局匹配 <td>模型名</td> ... <td>$数字</td> 连续 6 列的表格
	reRow := regexp.MustCompile(`<tr>\s*<td[^>]*>(.*?)</td>\s*<td[^>]*>.*?</td>\s*<td[^>]*>.*?</td>\s*<td[^>]*>.*?</td>\s*<td[^>]*>.*?</td>\s*<td[^>]*>\$([0-9]+(?:\.[0-9]+)?)\s*</td>`)
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
