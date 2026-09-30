package api

import (
	"context"
	"fmt"
	"os"
	"time"

	"opencode-go-analysis/internal/model"
)

// RetentionDays 是 /console/api/request-logs 的数据保留期（响应里的 retentionDays）。
// 早于该窗口的记录已经拉不到，因此这是增量抓取能覆盖的最大历史跨度。
const RetentionDays = 30

// CheckRetentionGap 用已入库的时间水位判断是否已掉出保留窗口。
// 超出时返回可读的警告文案，否则返回 ""。
// 调用方应在抓取前打印它 —— 否则数据缺口会静默产生，事后无法察觉也无法补救。
func CheckRetentionGap(watermark int64) string {
	if watermark <= 0 {
		return "" // 空库 / 首次抓取，不适用
	}
	oldest := time.Now().AddDate(0, 0, -RetentionDays).UnixMilli()
	if watermark >= oldest {
		return ""
	}
	return fmt.Sprintf("usage 水位 %s 已早于 request-logs 的 %d 天保留期（%s），之前的缺口无法补回",
		time.UnixMilli(watermark).Format("2006-01-02"), RetentionDays,
		time.UnixMilli(oldest).Format("2006-01-02"))
}

// SplitFreshPage 按时间水位把一页记录分成「需要写库的新记录」，并给出是否该停止翻页。
//
// 记录按 createdAt 倒序返回：一旦遇到不晚于水位的记录，之后的都更旧，可停止翻页。
// 关键约定：**即使 stop=true，返回的 fresh 也必须写库** —— 否则半页新记录会被丢弃，
// 表现为增量抓取永远 added:0（只有整页全为新记录时才写得进去）。
func SplitFreshPage(watermark int64, known map[string]struct{}, recs []model.UsageRecord) (fresh []model.UsageRecord, stop bool) {
	for _, rec := range recs {
		if watermark > 0 && rec.TimeCreated <= watermark {
			return fresh, true
		}
		if _, dup := known[rec.ID]; dup {
			continue
		}
		known[rec.ID] = struct{}{}
		fresh = append(fresh, rec)
	}
	return fresh, false
}

// FetchUsagePages 从最新记录开始顺序抓取，每页通过 onPage 回调交付；
// 回调返回 stop=true 时停止抓取（用于增量：已抓完所有新数据）。
// 数据源是 /console/api/request-logs，cursor 分页：nextCursor 为空即数据末尾。
// 注意：单页可能被 category/outcome 过滤成 0 条但仍有 nextCursor，
// 此时不能当作数据末尾，必须继续翻页（否则会漏抓）。
// 鉴权失败（cookie 过期）直接返回 error，不再按“限流”等待重试。
func (c *Client) FetchUsagePages(ctx context.Context, workspaceID string, onPage func(page int, recs []model.UsageRecord) (stop bool)) error {
	const pageSize = PageSize
	page := 0
	cursor := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		recs, next, err := c.FetchUsageRowsPage(cursor, pageSize)
		if err != nil {
			return fmt.Errorf("page %d: %w", page, err)
		}
		if len(recs) == 0 && next == "" {
			return nil // 数据末尾
		}
		if onPage(page, recs) {
			return nil
		}
		page++
		if next == "" || next == cursor {
			return nil // 数据末尾（防呆：cursor 不再推进也停）
		}
		cursor = next
		if os.Getenv("USAGE_DEBUG") != "" {
			fmt.Fprintf(os.Stderr, "[debug] page %d done, cursor=%.24s...\n", page, next)
		}
	}
}
