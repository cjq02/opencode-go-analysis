// Package api 封装 opencode.ai 新版 console API（https://opencode.ai/console/api）。
//
// 旧版 SolidStart 应用（/_server + workspace/<id>/go 页面）已下线，
// 用量数据改走 console REST 接口，需 Cookie + x-org-id 请求头。
package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"opencode-go-analysis/internal/model"
)

const (
	DefaultBaseURL = "https://opencode.ai/console/api"
	PageSize       = 100 // 服务端 pageSize 上限（qo maximum=100）
)

// Client 调用 opencode.ai console API
type Client struct {
	BaseURL string
	Cookie  string // 浏览器会话 cookie（Cookie 头原文）
	OrgID   string // 组织 id（x-org-id 头；Go workspace 下与 workspace id 相同）
	HTTP    *http.Client

	mu       sync.Mutex // 限速锁
	minGap   time.Duration
	lastCall time.Time
}

// New 创建 API client
func New(cookie, orgID string) *Client {
	return &Client{
		BaseURL: DefaultBaseURL,
		Cookie:  cookie,
		OrgID:   orgID,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
		minGap: 1 * time.Second,
	}
}

// pace 全局限速
func (c *Client) pace() {
	if c.minGap <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		now := time.Now()
		if now.Sub(c.lastCall) >= c.minGap {
			c.lastCall = now
			return
		}
		time.Sleep(c.lastCall.Add(c.minGap).Sub(now))
	}
}

// doGet 发起带鉴权的 GET 请求；401/403 视为 cookie 失效直接报错，
// 不再当作“限流空页”重试。
func (c *Client) doGet(path string, query map[string]string) ([]byte, error) {
	u, err := url.Parse(c.BaseURL + path)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	for k, v := range query {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	c.pace()
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cookie", c.Cookie)
	req.Header.Set("x-org-id", c.OrgID)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("unauthorized: cookie expired (console api %d %s)", resp.StatusCode, truncate(data, 160))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("console api %s: HTTP %d: %s", path, resp.StatusCode, truncate(data, 200))
	}
	return data, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

// ---- 新版 console API 类型 ----

// flexInt 兼容“数字或数字字符串”两种 JSON 表示（大整数常以字符串返回）。
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*f = 0
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		if s == "" {
			*f = 0
			return nil
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return err
		}
		*f = flexInt(n)
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*f = flexInt(n)
	return nil
}

// UsageItem 对应 /api/usage/rows 的单条记录
type UsageItem struct {
	ID               flexInt `json:"id"`
	OrgID            string  `json:"orgId"`
	Model            string  `json:"model"`
	Provider         string  `json:"provider"`
	InputTokens      flexInt `json:"inputTokens"`
	OutputTokens     flexInt `json:"outputTokens"`
	ReasoningTokens  flexInt `json:"reasoningTokens"`
	CacheReadTokens  flexInt `json:"cacheReadTokens"`
	CacheWrite5m     flexInt `json:"cacheWrite5mTokens"`
	CacheWrite1h     flexInt `json:"cacheWrite1hTokens"`
	CostMicroCents   flexInt `json:"costMicroCents"`
	BillingSource    string  `json:"billingSource"`
	CreatedAt        string  `json:"createdAt"`
}

type rowsResponse struct {
	Items      []UsageItem `json:"items"`
	NextCursor *string     `json:"nextCursor"`
}

// ModelStat 对应 /api/usage/models 的单模型聚合
type ModelStat struct {
	Model            string  `json:"model"`
	Provider         string  `json:"provider"`
	TotalRequests    flexInt `json:"totalRequests"`
	TotalInputTokens flexInt `json:"totalInputTokens"`
	TotalOutputTokens flexInt `json:"totalOutputTokens"`
	TotalCacheRead   flexInt `json:"totalCacheReadTokens"`
	TotalCacheWrite5m flexInt `json:"totalCacheWrite5mTokens"`
	TotalCacheWrite1h flexInt `json:"totalCacheWrite1hTokens"`
	TotalCostMicroCents flexInt `json:"totalCostMicroCents"`
}

type modelsResponse struct {
	Items []ModelStat `json:"items"`
	PageInfo struct {
		Page      int `json:"page"`
		PageSize  int `json:"pageSize"`
		Total     int `json:"total"`
		PageCount int `json:"pageCount"`
	} `json:"pageInfo"`
}

// Summary 对应 /api/usage/summary 的聚合
type Summary struct {
	TotalRequests       flexInt `json:"totalRequests"`
	TotalInputTokens    flexInt `json:"totalInputTokens"`
	TotalOutputTokens   flexInt `json:"totalOutputTokens"`
	TotalCacheRead      flexInt `json:"totalCacheReadTokens"`
	TotalCacheWrite5m   flexInt `json:"totalCacheWrite5mTokens"`
	TotalCacheWrite1h   flexInt `json:"totalCacheWrite1hTokens"`
	TotalCostMicroCents flexInt `json:"totalCostMicroCents"`
}

// FetchUsageRowsPage 拉取用量明细的一页（cursor 分页；cursor 为 "" 取首页）。
// 返回记录、下一页 cursor（"" 表示末尾）。
func (c *Client) FetchUsageRowsPage(timeRange, cursor string, pageSize int) ([]model.UsageRecord, string, error) {
	if pageSize <= 0 || pageSize > PageSize {
		pageSize = PageSize
	}
	query := map[string]string{"range": timeRange, "pageSize": strconv.Itoa(pageSize)}
	if cursor != "" {
		query["cursor"] = cursor
	}
	data, err := c.doGet("/usage/rows", query)
	if err != nil {
		return nil, "", err
	}
	var rr rowsResponse
	if err := json.Unmarshal(data, &rr); err != nil {
		return nil, "", fmt.Errorf("decode usage rows: %w", err)
	}
	recs := make([]model.UsageRecord, 0, len(rr.Items))
	for _, it := range rr.Items {
		rec := model.UsageRecord{
			ID:              strconv.FormatInt(int64(it.ID), 10),
			WorkspaceID:     it.OrgID,
			Model:           it.Model,
			Provider:        it.Provider,
			InputTokens:     int64(it.InputTokens),
			OutputTokens:    int64(it.OutputTokens),
			ReasoningTokens: int64(it.ReasoningTokens),
			CacheReadTokens: int64(it.CacheReadTokens),
			CacheWrite5m:    int64(it.CacheWrite5m),
			CacheWrite1h:    int64(it.CacheWrite1h),
			Cost:            int64(it.CostMicroCents), // 与旧 cost_raw 同单位：1e-8 美元
		}
		if it.CreatedAt != "" {
			if ts, err := time.Parse(time.RFC3339, it.CreatedAt); err == nil {
				rec.TimeCreated = ts.UnixMilli()
			}
		}
		recs = append(recs, rec)
	}
	next := ""
	if rr.NextCursor != nil {
		next = *rr.NextCursor
	}
	return recs, next, nil
}

// FetchUsageModels 拉取某 range 下全量模型聚合（自动翻页）。
func (c *Client) FetchUsageModels(timeRange string) ([]ModelStat, error) {
	var all []ModelStat
	for page := 1; ; page++ {
		data, err := c.doGet("/usage/models", map[string]string{
			"range":    timeRange,
			"page":     strconv.Itoa(page),
			"pageSize": strconv.Itoa(PageSize),
		})
		if err != nil {
			return nil, err
		}
		var mr modelsResponse
		if err := json.Unmarshal(data, &mr); err != nil {
			return nil, fmt.Errorf("decode usage models: %w", err)
		}
		all = append(all, mr.Items...)
		if mr.PageInfo.PageCount == 0 || page >= mr.PageInfo.PageCount {
			break
		}
	}
	return all, nil
}

// FetchSummary 拉取某 range 下的用量汇总。
func (c *Client) FetchSummary(timeRange string) (Summary, error) {
	data, err := c.doGet("/usage/summary", map[string]string{"range": timeRange})
	if err != nil {
		return Summary{}, err
	}
	var s Summary
	if err := json.Unmarshal(data, &s); err != nil {
		return Summary{}, fmt.Errorf("decode usage summary: %w", err)
	}
	return s, nil
}
