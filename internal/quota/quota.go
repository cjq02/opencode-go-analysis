package quota

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
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
}

var Default = Quota{FiveHour: 12, Weekly: 30, Monthly: 60, Source: "hardcoded"}

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
	return q, nil
}
