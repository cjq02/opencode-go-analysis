package subscription

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"time"
)

const workspaceGoURL = "https://opencode.ai/workspace/%s/go"

// Usage 表示 Go 订阅的滚动/周/月用量
type Usage struct {
	Status       string  `json:"status"`
	ResetInSec   int64   `json:"resetInSec"`
	Usage        int64   `json:"usage"`
	Limit        int64   `json:"limit"`
	UsagePercent float64 `json:"usagePercent"`
}

// Info 完整订阅周期信息
type Info struct {
	Rolling Usage    `json:"rolling"`
	Weekly  Usage    `json:"weekly"`
	Monthly Usage    `json:"monthly"`
	FetchedAt time.Time `json:"fetchedAt"`
	Source    string    `json:"source"`
}

var (
	reRolling = regexp.MustCompile(`rollingUsage:[^{]*\{[^}]*resetInSec:(\d+)[^}]*usagePercent:([0-9.]+)[^}]*usage:(\d+)[^}]*limit:(\d+)`)
	reWeekly  = regexp.MustCompile(`weeklyUsage:[^{]*\{[^}]*resetInSec:(\d+)[^}]*usagePercent:([0-9.]+)[^}]*usage:(\d+)[^}]*limit:(\d+)`)
	reMonthly = regexp.MustCompile(`monthlyUsage:[^{]*\{[^}]*status:"[^"]*"[^}]*resetInSec:(\d+)[^}]*usagePercent:([0-9.]+)[^}]*usage:(\d+)[^}]*limit:(\d+)`)
	// 兼容 monthlyUsage 顺序不同：兜底匹配
	reMonthlyAlt = regexp.MustCompile(`monthlyUsage:[^{]*\{[^}]*resetInSec:(\d+)[^}]*usage:(\d+)[^}]*limit:(\d+)`)
)

func parseUsage(re *regexp.Regexp, html string) (Usage, bool) {
	m := re.FindStringSubmatch(html)
	if m == nil {
		return Usage{}, false
	}
	// groups: 1=reset,2=percent,3=usage,4=limit (monthlyAlt has 1=reset,2=usage,3=limit)
	if len(m) == 5 {
		ri, _ := strconv.ParseInt(m[1], 10, 64)
		pct, _ := strconv.ParseFloat(m[2], 64)
		us, _ := strconv.ParseInt(m[3], 10, 64)
		lim, _ := strconv.ParseInt(m[4], 10, 64)
		return Usage{Status: "ok", ResetInSec: ri, UsagePercent: pct, Usage: us, Limit: lim}, true
	}
	if len(m) == 4 {
		ri, _ := strconv.ParseInt(m[1], 10, 64)
		us, _ := strconv.ParseInt(m[2], 10, 64)
		lim, _ := strconv.ParseInt(m[3], 10, 64)
		return Usage{ResetInSec: ri, Usage: us, Limit: lim}, true
	}
	return Usage{}, false
}

// Fetch 抓取 workspace Go 订阅页并解析用量
func Fetch(ctx context.Context, cookie, workspaceID string) (Info, error) {
	url := fmt.Sprintf(workspaceGoURL, workspaceID)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("Cookie", cookie)
	req.Header.Set("User-Agent", "opencode-go-analysis/1.0")
	if cookie == "" {
		return Info{}, fmt.Errorf("cookie empty")
	}
	client := &http.Client{Timeout: 12 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return Info{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return Info{}, fmt.Errorf("status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return Info{}, err
	}
	return Parse(string(b))
}

func Parse(html string) (Info, error) {
	r, ok1 := parseUsage(reRolling, html)
	w, ok2 := parseUsage(reWeekly, html)
	m, ok3 := parseUsage(reMonthly, html)
	if !ok3 {
		m, ok3 = parseUsage(reMonthlyAlt, html)
	}
	if !ok1 || !ok2 || !ok3 {
		return Info{}, fmt.Errorf("parse subscription failed rolling=%v weekly=%v monthly=%v", ok1, ok2, ok3)
	}
	return Info{Rolling: r, Weekly: w, Monthly: m, FetchedAt: time.Now(), Source: "https://opencode.ai/workspace/.../go"}, nil
}

// CycleStart 计算周期开始时间（毫秒），period 为周期总时长（秒）
func CycleStart(fetchedAt time.Time, periodSec, resetInSec int64) int64 {
	elapsed := periodSec - resetInSec
	if elapsed < 0 {
		elapsed = 0
	}
	return fetchedAt.UnixMilli() - elapsed*1000
}

const (
	PeriodRolling = 5 * 3600
	PeriodWeekly  = 7 * 86400
	PeriodMonthly = 30 * 86400
)

// ModelBreakdown 单模型在某周期内的配额消耗
type ModelBreakdown struct {
	Model               string `json:"model"`
	Name                string `json:"name"`
	Cost                int64  `json:"cost"`      // 原始 cost_raw
	QuotaCost           int64  `json:"quotaCost"` // 计入配额的 cost（含 multiplier）
	Multiplier          float64 `json:"multiplier"`
	Estimated           bool   `json:"estimated"`
	ContributionPercent float64 `json:"contributionPercent"`
}

// Breakdown 某周期（rolling/weekly/monthly）的模型明细
type Breakdown struct {
	Usage        int64            `json:"usage"` // sum quotaCost
	Limit        int64            `json:"limit"` // token limit
	UsagePercent float64          `json:"usagePercent"`
	Rows         []ModelBreakdown `json:"rows"`
	FetchedAt    time.Time        `json:"fetchedAt"`
	Period       string           `json:"period"`
}

// breakdownFnID 为 workspace/go 模型明细的 server function id（通过抓包 workspace/go 页获得）
const breakdownFnID = "ba154d05c4028a885b8c753f9def7e45d87eb982e65fa8b14254cbe636168914"

var (
	reCost        = regexp.MustCompile(`cost:(\d+)`)
	reQuotaCost   = regexp.MustCompile(`quotaCost:(\d+)`)
	reMultiplier  = regexp.MustCompile(`multiplier:([0-9.]+)`)
	reModel       = regexp.MustCompile(`model:"([^"]+)"`)
	reName        = regexp.MustCompile(`name:"([^"]+)"`)
	reEstimated   = regexp.MustCompile(`estimated:(!0|!1|true|false)`)
	reContrib     = regexp.MustCompile(`contributionPercent:([0-9.]+)`)
	reUsage       = regexp.MustCompile(`usage:(\d+)`)
	reLimit       = regexp.MustCompile(`limit:(\d+)`)
	reUsagePct    = regexp.MustCompile(`usagePercent:([0-9.]+)`)
	reRowStart    = regexp.MustCompile(`\{model:"`)
)

// FetchBreakdown 抓取某周期（rolling/weekly/monthly）的模型配额明细，period 取值 "rolling"|"weekly"|"monthly"
func FetchBreakdown(ctx context.Context, cookie, workspaceID, period string) (Breakdown, error) {
	if cookie == "" {
		cookie = os.Getenv("OPENCODE_COOKIE")
	}
	if cookie == "" {
		return Breakdown{}, fmt.Errorf("cookie empty")
	}
	// seroval: workspaceID string, period string
	body, _ := json.Marshal(map[string]any{
		"t": map[string]any{"t": 9, "i": 0, "l": 2, "a": []map[string]any{{"t": 1, "s": workspaceID}, {"t": 1, "s": period}}, "o": 0},
		"f": 31,
		"m": []any{},
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://opencode.ai/_server", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Server-Id", breakdownFnID)
	req.Header.Set("X-Server-Instance", "server-fn:1")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Origin", "https://opencode.ai")
	req.Header.Set("Referer", "https://opencode.ai/workspace/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	client := &http.Client{Timeout: 12 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return Breakdown{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return Breakdown{}, err
	}
	if resp.StatusCode != 200 {
		return Breakdown{}, fmt.Errorf("status %d: %s", resp.StatusCode, string(b[:200]))
	}
	return parseBreakdown(string(b), period)
}

func parseBreakdown(js, period string) (Breakdown, error) {
	// 粗解析：先找 usage/limit/usagePercent，再按 {model:" 拆段解析每行
	text := js
	// 跳过 ;0x...; 头
	if i := bytes.Index([]byte(text), []byte(";0x")); i >= 0 {
		if j := bytes.Index([]byte(text[i+2:]), []byte(";")); j >= 0 {
			text = text[i+2+j+1:]
		}
	}
	var bd Breakdown
	bd.Period = period
	bd.FetchedAt = time.Now()
	if m := reUsage.FindStringSubmatch(text); len(m) > 1 {
		bd.Usage, _ = strconv.ParseInt(m[1], 10, 64)
	}
	if m := reLimit.FindStringSubmatch(text); len(m) > 1 {
		bd.Limit, _ = strconv.ParseInt(m[1], 10, 64)
	}
	if m := reUsagePct.FindStringSubmatch(text); len(m) > 1 {
		bd.UsagePercent, _ = strconv.ParseFloat(m[1], 64)
	}
	// 按模型分段
	starts := reRowStart.FindAllStringIndex(text, -1)
	for idx, pos := range starts {
		end := len(text)
		if idx+1 < len(starts) {
			end = starts[idx+1][0]
		}
		seg := text[pos[0]:end]
		var mb ModelBreakdown
		if m := reModel.FindStringSubmatch(seg); len(m) > 1 {
			mb.Model = m[1]
		}
		if m := reName.FindStringSubmatch(seg); len(m) > 1 {
			mb.Name = m[1]
		}
		if m := reCost.FindStringSubmatch(seg); len(m) > 1 {
			mb.Cost, _ = strconv.ParseInt(m[1], 10, 64)
		}
		if m := reQuotaCost.FindStringSubmatch(seg); len(m) > 1 {
			mb.QuotaCost, _ = strconv.ParseInt(m[1], 10, 64)
		}
		if m := reMultiplier.FindStringSubmatch(seg); len(m) > 1 {
			mb.Multiplier, _ = strconv.ParseFloat(m[1], 64)
		}
		if m := reEstimated.FindStringSubmatch(seg); len(m) > 1 {
			mb.Estimated = (m[1] == "!0" || m[1] == "true")
		}
		if m := reContrib.FindStringSubmatch(seg); len(m) > 1 {
			mb.ContributionPercent, _ = strconv.ParseFloat(m[1], 64)
		}
		if mb.Model != "" {
			bd.Rows = append(bd.Rows, mb)
		}
	}
	if len(bd.Rows) == 0 {
		return bd, fmt.Errorf("no rows parsed")
	}
	return bd, nil
}
