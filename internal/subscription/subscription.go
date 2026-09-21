package subscription

import (
	"context"
	"fmt"
	"time"

	"opencode-go-analysis/internal/api"
)

// 新版 console API 的 Go 订阅状态（GET /console/api/go/status）给出官方的
// 5 小时 / 周 / 月用量窗口：已用与额度均以 microcents（1e-8 美元）计。
// 这与 Go 页面顶部三条进度条同源，是“已用百分比”的权威口径。
// （旧版 workspace/<id>/go 页面与 /_server server function 已下线。）

// Meter 某个用量窗口的官方数据
type Meter struct {
	StartsAt        time.Time `json:"startsAt"`
	ResetsAt        time.Time `json:"resetsAt"`
	LimitMicroCents int64     `json:"limitMicroCents"`
	UsedMicroCents  int64     `json:"usedMicroCents"`
	UsedPercent     float64   `json:"usedPercent"`
	LimitUSD        float64   `json:"limitUSD"`
	UsedUSD         float64   `json:"usedUSD"`
	ResetInSec      int64     `json:"resetInSec"`
}

// Info 完整订阅信息
type Info struct {
	FiveHour    Meter     `json:"fiveHour"`
	Weekly      Meter     `json:"weekly"`
	Monthly     Meter     `json:"monthly"`
	PeriodStart time.Time `json:"periodStart"` // 订阅月周期开始（access.startsAt）
	PeriodEnd   time.Time `json:"periodEnd"`   // 订阅月周期结束（access.endsAt）
	UseBalance  bool      `json:"useBalance"`
	FetchedAt   time.Time `json:"fetchedAt"`
	Source      string    `json:"source"`
}

// Access 订阅是否有效
func (i Info) Access() bool { return i.Monthly.LimitMicroCents > 0 }

func toMeter(m api.GoMeter, resetsFallback time.Time) Meter {
	limit := int64(m.LimitMicroCents)
	used := int64(m.UsedMicroCents)
	me := Meter{
		LimitMicroCents: limit,
		UsedMicroCents:  used,
		LimitUSD:        float64(limit) / 1e8,
		UsedUSD:         float64(used) / 1e8,
	}
	if limit > 0 {
		me.UsedPercent = float64(used) / float64(limit) * 100
	}
	if ts, err := time.Parse(time.RFC3339, m.StartsAt); err == nil {
		me.StartsAt = ts
	}
	if ts, err := time.Parse(time.RFC3339, m.ResetsAt); err == nil {
		me.ResetsAt = ts
	} else {
		me.ResetsAt = resetsFallback
	}
	if !me.ResetsAt.IsZero() {
		me.ResetInSec = int64(time.Until(me.ResetsAt).Seconds())
		if me.ResetInSec < 0 {
			me.ResetInSec = 0
		}
	}
	return me
}

// Fetch 抓取 Go 订阅状态（5h/周/月官方用量）
func Fetch(ctx context.Context, cookie, orgID string) (Info, error) {
	if cookie == "" {
		return Info{}, fmt.Errorf("cookie empty")
	}
	c := api.New(cookie, orgID)
	c.HTTP.Timeout = 12 * time.Second
	st, err := c.FetchGoStatus()
	if err != nil {
		return Info{}, err
	}
	if st.Access == nil {
		return Info{}, fmt.Errorf("no go subscription access")
	}
	var periodStart, periodEnd time.Time
	if ts, err := time.Parse(time.RFC3339, st.Access.StartsAt); err == nil {
		periodStart = ts
	}
	if ts, err := time.Parse(time.RFC3339, st.Access.EndsAt); err == nil {
		periodEnd = ts
	}
	return Info{
		FiveHour:    toMeter(st.Access.Meters.FiveHour, time.Time{}),
		Weekly:      toMeter(st.Access.Meters.Week, time.Time{}),
		Monthly:     toMeter(st.Access.Meters.Month, periodEnd),
		PeriodStart: periodStart,
		PeriodEnd:   periodEnd,
		UseBalance:  st.UseBalance,
		FetchedAt:   time.Now(),
		Source:      "https://opencode.ai/console/api/go/status",
	}, nil
}

// 标称月周期长度（秒），仅供无 access 时的周期起止回退
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
	Usage        int64            `json:"usage"` // sum quotaCost（1e-8 美元）
	Limit        int64            `json:"limit"` // 由调用方按官方额度填充，默认 0
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
