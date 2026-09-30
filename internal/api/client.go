// Package api 封装 opencode.ai 新版 console API（https://opencode.ai/console/api）。
//
// 旧版 SolidStart 应用（/_server + workspace/<id>/go 页面）已下线，
// 用量数据改走 console REST 接口，需 Cookie + x-org-id 请求头。
package api

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
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

// RequestLog 对应 /console/api/request-logs 的单条请求日志。
// 旧的 /console/api/usage/rows 已于 2026-09-30 下线（HTTP 404，console 前端
// 的 usage 资源只剩 summary/cost-by-day/models/users），改用 Logs 页的
// request-logs 接口按相同语义重建 usage_records：
//   - 过滤 category=inference 且 outcome=succeeded 即与旧 usage/rows 完全等价
//     （2026-09-20 实测 269 条 / $0.204903，与库内 269 条 / $0.20490322 一致）
//   - cost 以「美元浮点数」返回，需 ×1e8 转成本库的 1e-8 美元整数
//   - cacheWrite1hTokens 恒不返回，只有 cacheWriteTokens（可为 null）
//   - provider 对 go/standard/go-plus 被上游脱敏为 "opencode"（分析查询只按
//     model 聚合，故不影响结果）
//   - 只保留 30 天（响应里的 retentionDays），更早的历史拉不到
type RequestLog struct {
	ID                string   `json:"id"`
	WorkspaceID       string   `json:"workspaceID"`
	StartedAt         int64    `json:"startedAt"`
	Category          string   `json:"category"`
	Outcome           string   `json:"outcome"`
	Model             string   `json:"model"`
	Provider          string   `json:"provider"`
	SessionID         string   `json:"sessionID"`
	InputTokens       flexInt  `json:"inputTokens"`
	OutputTokens      flexInt  `json:"outputTokens"`
	ReasoningTokens   flexInt  `json:"reasoningTokens"`
	CacheReadTokens   flexInt  `json:"cacheReadTokens"`
	CacheWriteTokens  flexInt  `json:"cacheWriteTokens"`
	CacheWrite1h      flexInt  `json:"cacheWrite1hTokens"`
	Cost              *float64 `json:"cost"`
}

type requestLogsResponse struct {
	Items         []RequestLog `json:"items"`
	NextCursor    *string      `json:"nextCursor"`
	Until         int64        `json:"until"`
	RetentionDays int          `json:"retentionDays"`
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
// 实际走 /console/api/request-logs：limit 上限 100，cursor 是服务端给的
// 原样字符串（内容为 JSON，不要再二次编码），按 startedAt 倒序返回。
// pageSize 之外还会带上 category=inference&outcome=succeeded 的服务端过滤，
// 映射时再本地过滤一次，避免服务端忽略过滤条件时语义跑偏。
func (c *Client) FetchUsageRowsPage(cursor string, pageSize int) ([]model.UsageRecord, string, error) {
	if pageSize <= 0 || pageSize > PageSize {
		pageSize = PageSize
	}
	query := map[string]string{
		"limit":    strconv.Itoa(pageSize),
		"category": "inference",
		"outcome":  "succeeded",
	}
	if cursor != "" {
		query["cursor"] = cursor
	}
	data, err := c.doGet("/request-logs", query)
	if err != nil {
		return nil, "", err
	}
	var rr requestLogsResponse
	if err := json.Unmarshal(data, &rr); err != nil {
		return nil, "", fmt.Errorf("decode request logs: %w", err)
	}
	recs := make([]model.UsageRecord, 0, len(rr.Items))
	for _, it := range rr.Items {
		if it.Category != "inference" || it.Outcome != "succeeded" {
			continue
		}
		recs = append(recs, toUsageRecord(it))
	}
	next := ""
	if rr.NextCursor != nil {
		next = *rr.NextCursor
	}
	return recs, next, nil
}

// toUsageRecord 把一条 request-log 记录映射成本库的 UsageRecord。
// cost 单位由「美元浮点」转成 1e-8 美元整数，与旧 cost_raw 口径一致。
func toUsageRecord(it RequestLog) model.UsageRecord {
	rec := model.UsageRecord{
		ID:              it.ID,
		WorkspaceID:     it.WorkspaceID,
		TimeCreated:     it.StartedAt,
		Model:           it.Model,
		Provider:        it.Provider,
		SessionID:       it.SessionID,
		InputTokens:     int64(it.InputTokens),
		OutputTokens:    int64(it.OutputTokens),
		ReasoningTokens: int64(it.ReasoningTokens),
		CacheReadTokens: int64(it.CacheReadTokens),
		CacheWrite5m:    int64(it.CacheWriteTokens),
		CacheWrite1h:    int64(it.CacheWrite1h),
	}
	if it.Cost != nil {
		rec.Cost = int64(math.Round(*it.Cost * 1e8))
	}
	return rec
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

// ---- Go 订阅状态（Go 页面顶部 5h/周/月 用量表）----

// GoMeter 单个用量窗口。金额单位为 microcents（1e-8 美元），与 cost_raw 同单位。
// month 窗口没有 startsAt/resetsAt（月周期取 Access.StartsAt/EndsAt）。
type GoMeter struct {
	StartsAt        string  `json:"startsAt"`
	ResetsAt        string  `json:"resetsAt"`
	LimitMicroCents flexInt `json:"limitMicroCents"`
	UsedMicroCents  flexInt `json:"usedMicroCents"`
}

// GoMeters 三档用量窗口
type GoMeters struct {
	FiveHour GoMeter `json:"fiveHour"`
	Week     GoMeter `json:"week"`
	Month    GoMeter `json:"month"`
}

// GoAccess 订阅有效期与各窗口用量
type GoAccess struct {
	StartsAt          string   `json:"startsAt"`
	EndsAt            string   `json:"endsAt"`
	CancelAtPeriodEnd bool     `json:"cancelAtPeriodEnd"`
	Meters            GoMeters `json:"meters"`
}

// GoStatus 对应 GET /console/api/go/status 的返回。
// 注意：普通组织走 /go/status；/internal/.../go/status 仅用于客服代登录（会 403）。
type GoStatus struct {
	SubscriberUserID  string    `json:"subscriberUserId"`
	UseBalance        bool      `json:"useBalance"`
	CancelAtPeriodEnd bool      `json:"cancelAtPeriodEnd"`
	RenewalPending    bool      `json:"renewalPending"`
	Access            *GoAccess `json:"access"`
}

// FetchGoStatus 拉取 Go 订阅状态（含 5h/周/月 官方已用与额度）。
func (c *Client) FetchGoStatus() (GoStatus, error) {
	data, err := c.doGet("/go/status", nil)
	if err != nil {
		return GoStatus{}, err
	}
	var st GoStatus
	if err := json.Unmarshal(data, &st); err != nil {
		return GoStatus{}, fmt.Errorf("decode go status: %w", err)
	}
	return st, nil
}
