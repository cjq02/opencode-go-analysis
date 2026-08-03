package api

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"opencode-go-analysis/internal/model"
)

// FetchUsagePage 抓取 workspace 用量的一页数据（每页 PageSize 条）。
// 服务端限流时返回空 seroval 响应，视为空页（len==0, err==nil），由上层处理。
func (c *Client) FetchUsagePage(workspaceID string, page int) ([]model.UsageRecord, error) {
	data, err := c.call(usageFnID, workspaceID, page)
	if err != nil {
		return nil, err
	}
	return parseUsageResponse(data)
}

// FetchUsagePages 从 page 0 开始顺序抓取，每页通过 onPage 回调交付；
// 回调返回 stop=true 时停止抓取（用于增量：已抓完所有新数据）。
// 服务端对请求频率敏感：过密时会返回空数组（限流），
// 因此遇到空页会等待后重试，连续 emptyRetryLimit 次仍空才视为数据末尾。
func (c *Client) FetchUsagePages(ctx context.Context, workspaceID string, onPage func(page int, recs []model.UsageRecord) (stop bool)) error {
	const (
		emptyRetryLimit = 3
		emptyRetryWait  = 60 * time.Second
	)
	page := 0
	emptyRetries := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		recs, err := c.FetchUsagePage(workspaceID, page)
		if err != nil {
			return fmt.Errorf("page %d: %w", page, err)
		}
		if len(recs) == 0 {
			emptyRetries++
			if emptyRetries >= emptyRetryLimit {
				return nil // 数据末尾
			}
			// 疑似限流，等待后重试同一页
			fmt.Fprintf(os.Stderr, "\rpage %d: 空响应(疑似限流), 等待 %s 重试(%d/%d)", page, emptyRetryWait, emptyRetries, emptyRetryLimit)
			select {
			case <-time.After(emptyRetryWait):
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		}
		emptyRetries = 0
		if onPage(page, recs) {
			return nil
		}
		page++
	}
}

// ---- seroval 流响应解析 ----

// 响应格式: ;0x<hex长度>;<seroval JS 代码>
// 记录形如: $R[1]={id:"usg_...",timeCreated:$R[2]=new Date("..."),model:"...",
//           inputTokens:123,outputTokens:456,cost:789,enrichment:$R[4]={plan:"lite"}}
var (
	reRecord    = regexp.MustCompile(`\{id:"usg_`)
	reID        = regexp.MustCompile(`id:"(usg_[0-9A-Z]+)"`)
	reWSID      = regexp.MustCompile(`workspaceID:"([^"]*)"`)
	reCreated   = regexp.MustCompile(`timeCreated:\$?R?\[\d+\]?=\s*new Date\("([^"]+)"\)|timeCreated:new Date\("([^"]+)"\)`)
	reModel     = regexp.MustCompile(`model:"([^"]*)"`)
	reProvider  = regexp.MustCompile(`provider:"([^"]*)"`)
	reSession   = regexp.MustCompile(`sessionID:"([^"]*)"`)
	reInput     = regexp.MustCompile(`inputTokens:(\d+)`)
	reOutput    = regexp.MustCompile(`outputTokens:(\d+)`)
	reReasoning = regexp.MustCompile(`reasoningTokens:(?:null|(\d+))`)
	reCacheR    = regexp.MustCompile(`cacheReadTokens:(?:null|(\d+))`)
	reCache5m   = regexp.MustCompile(`cacheWrite5mTokens:(?:null|(\d+))`)
	reCache1h   = regexp.MustCompile(`cacheWrite1hTokens:(?:null|(\d+))`)
	reCost      = regexp.MustCompile(`cost:(\d+)`)
)

func parseUsageResponse(data []byte) ([]model.UsageRecord, error) {
	text := string(data)
	if i := strings.IndexByte(text, ';'); i >= 0 && i+1 < len(text) && strings.HasPrefix(text[i+1:], "0x") {
		// 跳过 ;0x<hex>; 头
		if j := strings.IndexByte(text[i+1:], ';'); j >= 0 {
			text = text[i+1+j+1:]
		}
	}

	var records []model.UsageRecord
	starts := reRecord.FindAllStringIndex(text, -1)
	for n, m := range starts {
		start := m[0]
		end := len(text)
		if n+1 < len(starts) {
			end = starts[n+1][0]
		}
		seg := text[start:end]

		rec := model.UsageRecord{
			ID:              reID.FindStringSubmatch(seg)[1],
			Model:           first(reModel.FindStringSubmatch(seg)),
			Provider:        first(reProvider.FindStringSubmatch(seg)),
			SessionID:       first(reSession.FindStringSubmatch(seg)),
			InputTokens:     atoi(reInput.FindStringSubmatch(seg)),
			OutputTokens:    atoi(reOutput.FindStringSubmatch(seg)),
			ReasoningTokens: atoi(reReasoning.FindStringSubmatch(seg)),
			CacheReadTokens: atoi(reCacheR.FindStringSubmatch(seg)),
			CacheWrite5m:    atoi(reCache5m.FindStringSubmatch(seg)),
			CacheWrite1h:    atoi(reCache1h.FindStringSubmatch(seg)),
			Cost:            atoi(reCost.FindStringSubmatch(seg)),
		}
		if m := reWSID.FindStringSubmatch(seg); len(m) > 1 {
			rec.WorkspaceID = m[1]
		}
		if m := reCreated.FindStringSubmatch(seg); len(m) > 1 {
			ts, err := time.Parse(time.RFC3339Nano, pick(m[1], m[2]))
			if err == nil {
				rec.TimeCreated = ts.UnixMilli()
			}
		}
		records = append(records, rec)
	}

	if len(records) == 0 && strings.TrimSpace(text) != "" && strings.TrimSpace(text) != "[]" {
		// 空 seroval 响应（如限流空页）不报错，交给上层按空页处理
		return nil, nil
	}
	return records, nil
}

func first(m []string) string {
	if len(m) > 1 {
		return m[1]
	}
	return ""
}

func pick(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func atoi(m []string) int64 {
	if len(m) > 1 && m[1] != "" {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		return n
	}
	return 0
}
