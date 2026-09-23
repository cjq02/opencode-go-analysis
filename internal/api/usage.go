package api

import (
	"context"
	"fmt"
	"os"

	"opencode-go-analysis/internal/model"
)

// FetchUsagePage 抓取用量明细的一页（兼容旧签名：page 仅用于展示/日志，内部走 cursor）。
// range 固定为 all（全量历史），由上层按已知 ID 去重实现增量。
func (c *Client) FetchUsagePage(workspaceID string, page int) ([]model.UsageRecord, error) {
	recs, _, err := c.FetchUsageRowsPage("all", "", PageSize)
	if err != nil {
		return nil, err
	}
	return recs, nil
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
// 新版 console API 为 cursor 分页：nextCursor 为空即数据末尾。
// 鉴权失败（cookie 过期）直接返回 error，不再按“限流”等待重试。
func (c *Client) FetchUsagePages(ctx context.Context, workspaceID string, onPage func(page int, recs []model.UsageRecord) (stop bool)) error {
	const pageSize = PageSize
	page := 0
	cursor := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		recs, next, err := c.FetchUsageRowsPage("all", cursor, pageSize)
		if err != nil {
			return fmt.Errorf("page %d: %w", page, err)
		}
		if len(recs) == 0 {
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
