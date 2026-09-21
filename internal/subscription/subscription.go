package subscription

import (
	"context"
	"fmt"
	"time"

	"opencode-go-analysis/internal/api"
)

// 旧版订阅页（workspace/<id>/go）已下线，滚动 5h/周/月 token 窗口不复存在。
// 新版 console API 仅提供 24h/7d/30d/all 四档用量汇总，订阅窗口改为：
// Day=24h / Weekly=7d / Monthly=30d 的官方用量（tokens + cost），不再含限额与重置倒计时。

// Window 某时间档的官方用量
type Window struct {
	Range           string  `json:"range"`
	Requests        int64   `json:"requests"`
	InputTokens     int64   `json:"inputTokens"`
	OutputTokens    int64   `json:"outputTokens"`
	CacheReadTokens int64   `json:"cacheReadTokens"`
	CacheWriteTokens int64  `json:"cacheWriteTokens"`
	CostMicroCents  int64   `json:"costMicroCents"` // 1e-8 美元，与 DB cost_raw 同单位
	TotalTokens     int64   `json:"totalTokens"`
	CostUSD         float64 `json:"costUSD"`
}

// Info 完整订阅用量信息
type Info struct {
	Day      Window    `json:"day"`
	Weekly   Window    `json:"weekly"`
	Monthly  Window    `json:"monthly"`
	FetchedAt time.Time `json:"fetchedAt"`
	Source   string    `json:"source"`
}

func toWindow(timeRange string, s api.Summary) Window {
	w := Window{
		Range:           timeRange,
		Requests:        int64(s.TotalRequests),
		InputTokens:     int64(s.TotalInputTokens),
		OutputTokens:    int64(s.TotalOutputTokens),
		CacheReadTokens: int64(s.TotalCacheRead),
		CacheWriteTokens: int64(s.TotalCacheWrite5m) + int64(s.TotalCacheWrite1h),
		CostMicroCents:  int64(s.TotalCostMicroCents),
	}
	w.TotalTokens = w.InputTokens + w.OutputTokens + w.CacheReadTokens + w.CacheWriteTokens
	w.CostUSD = float64(w.CostMicroCents) / 1e8
	return w
}

// Fetch 抓取 24h/7d/30d 三档官方用量
func Fetch(ctx context.Context, cookie, orgID string) (Info, error) {
	if cookie == "" {
		return Info{}, fmt.Errorf("cookie empty")
	}
	c := api.New(cookie, orgID)
	c.HTTP.Timeout = 12 * time.Second
	day, err := c.FetchSummary("24h")
	if err != nil {
		return Info{}, fmt.Errorf("summary 24h: %w", err)
	}
	week, err := c.FetchSummary("7d")
	if err != nil {
		return Info{}, fmt.Errorf("summary 7d: %w", err)
	}
	month, err := c.FetchSummary("30d")
	if err != nil {
		return Info{}, fmt.Errorf("summary 30d: %w", err)
	}
	return Info{
		Day:       toWindow("24h", day),
		Weekly:    toWindow("7d", week),
		Monthly:   toWindow("30d", month),
		FetchedAt: time.Now(),
		Source:    "https://opencode.ai/console/api/usage/summary",
	}, nil
}

// 周期长度（秒），用于订阅周期起止展示（新版为自然 trailing 30d）
const PeriodMonthly = 30 * 86400

// ModelBreakdown 单模型在某周期内的配额消耗
type ModelBreakdown struct {
	Model               string  `json:"model"`
	Name                string  `json:"name"`
	Cost                int64   `json:"cost"`      // 原始 cost（1e-8 美元）
	QuotaCost           int64   `json:"quotaCost"` // 计入配额的 cost（新版无 multiplier，等于 cost）
	Multiplier          float64 `json:"multiplier"`
	Estimated           bool    `json:"estimated"`
	ContributionPercent float64 `json:"contributionPercent"`
}

// Breakdown 某周期（monthly=30d）的模型明细
type Breakdown struct {
	Usage        int64            `json:"usage"` // sum quotaCost
	Limit        int64            `json:"limit"` // 新版无总额度，恒为 0
	UsagePercent float64          `json:"usagePercent"`
	Rows         []ModelBreakdown `json:"rows"`
	FetchedAt    time.Time        `json:"fetchedAt"`
	Period       string           `json:"period"`
}

// FetchBreakdown 抓取某周期（"monthly"→30d，也支持 "weekly"→7d、"rolling"→24h）的模型配额明细
func FetchBreakdown(ctx context.Context, cookie, orgID, period string) (Breakdown, error) {
	if cookie == "" {
		return Breakdown{}, fmt.Errorf("cookie empty")
	}
	timeRange := "30d"
	switch period {
	case "rolling", "24h":
		timeRange = "24h"
	case "weekly", "7d":
		timeRange = "7d"
	}
	c := api.New(cookie, orgID)
	c.HTTP.Timeout = 12 * time.Second
	stats, err := c.FetchUsageModels(timeRange)
	if err != nil {
		return Breakdown{}, err
	}
	var bd Breakdown
	bd.Period = period
	bd.FetchedAt = time.Now()
	for _, st := range stats {
		mb := ModelBreakdown{
			Model:      st.Model,
			Name:       st.Model,
			Cost:       int64(st.TotalCostMicroCents),
			QuotaCost:  int64(st.TotalCostMicroCents),
			Multiplier: 1,
		}
		bd.Rows = append(bd.Rows, mb)
		bd.Usage += mb.QuotaCost
	}
	if len(bd.Rows) == 0 {
		return bd, fmt.Errorf("no rows parsed")
	}
	for i := range bd.Rows {
		if bd.Usage > 0 {
			bd.Rows[i].ContributionPercent = float64(bd.Rows[i].QuotaCost) / float64(bd.Usage) * 100
		}
	}
	return bd, nil
}
