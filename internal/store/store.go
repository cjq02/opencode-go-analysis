// Package store 用量记录的 SQLite 持久化
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"opencode-go-analysis/internal/model"
)

const schema = `
CREATE TABLE IF NOT EXISTS usage_records (
	id              TEXT PRIMARY KEY,
	workspace_id    TEXT NOT NULL,
	time_created    INTEGER NOT NULL,
	model           TEXT NOT NULL,
	provider        TEXT,
	session_id      TEXT,
	input_tokens    INTEGER NOT NULL DEFAULT 0,
	output_tokens   INTEGER NOT NULL DEFAULT 0,
	reasoning_tokens INTEGER NOT NULL DEFAULT 0,
	cache_read_tokens INTEGER NOT NULL DEFAULT 0,
	cache_write_5m  INTEGER NOT NULL DEFAULT 0,
	cache_write_1h  INTEGER NOT NULL DEFAULT 0,
	cost_raw        INTEGER NOT NULL DEFAULT 0,
	created_at      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_usage_workspace_time
	ON usage_records (workspace_id, time_created);
CREATE TABLE IF NOT EXISTS fetch_meta (
	workspace_id TEXT PRIMARY KEY,
	last_page    INTEGER NOT NULL DEFAULT 0,
	updated_at   INTEGER NOT NULL
);
`

// Store 封装 SQLite 访问
type Store struct {
	db *sql.DB
}

// Open 打开（或创建）数据库文件
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite 单写者，串行更稳
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("初始化表结构: %w", err)
	}
	return &Store{db: db}, nil
}

// Close 关闭数据库
func (s *Store) Close() error { return s.db.Close() }

// Upsert 插入或更新一条记录（按 id 幂等）
func (s *Store) Upsert(ctx context.Context, r model.UsageRecord) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO usage_records (
	id, workspace_id, time_created, model, provider, session_id,
	input_tokens, output_tokens, reasoning_tokens,
	cache_read_tokens, cache_write_5m, cache_write_1h, cost_raw, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	workspace_id    = excluded.workspace_id,
	time_created    = excluded.time_created,
	model           = excluded.model,
	provider        = excluded.provider,
	session_id      = excluded.session_id,
	input_tokens    = excluded.input_tokens,
	output_tokens   = excluded.output_tokens,
	reasoning_tokens= excluded.reasoning_tokens,
	cache_read_tokens = excluded.cache_read_tokens,
	cache_write_5m  = excluded.cache_write_5m,
	cache_write_1h  = excluded.cache_write_1h,
	cost_raw        = excluded.cost_raw`,
		r.ID, r.WorkspaceID, r.TimeCreated, r.Model, r.Provider, r.SessionID,
		r.InputTokens, r.OutputTokens, r.ReasoningTokens,
		r.CacheReadTokens, r.CacheWrite5m, r.CacheWrite1h, r.Cost,
		time.Now().UnixMilli(),
	)
	return err
}

// BulkUpsert 批量写入（单事务）
func (s *Store) BulkUpsert(ctx context.Context, records []model.UsageRecord) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO usage_records (
	id, workspace_id, time_created, model, provider, session_id,
	input_tokens, output_tokens, reasoning_tokens,
	cache_read_tokens, cache_write_5m, cache_write_1h, cost_raw, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	model = excluded.model, cost_raw = excluded.cost_raw`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, r := range records {
		if _, err := stmt.ExecContext(ctx,
			r.ID, r.WorkspaceID, r.TimeCreated, r.Model, r.Provider, r.SessionID,
			r.InputTokens, r.OutputTokens, r.ReasoningTokens,
			r.CacheReadTokens, r.CacheWrite5m, r.CacheWrite1h, r.Cost,
			time.Now().UnixMilli()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Count 统计某 workspace 的记录数
func (s *Store) Count(ctx context.Context, workspaceID string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM usage_records WHERE workspace_id = ?`, workspaceID).Scan(&n)
	return n, err
}

// All 查询某 workspace 全部记录（按时间升序）
func (s *Store) All(ctx context.Context, workspaceID string) ([]model.UsageRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, workspace_id, time_created, model, provider, session_id,
	input_tokens, output_tokens, reasoning_tokens,
	cache_read_tokens, cache_write_5m, cache_write_1h, cost_raw
FROM usage_records WHERE workspace_id = ? ORDER BY time_created ASC`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []model.UsageRecord
	for rows.Next() {
		var r model.UsageRecord
		if err := rows.Scan(&r.ID, &r.WorkspaceID, &r.TimeCreated, &r.Model, &r.Provider,
			&r.SessionID, &r.InputTokens, &r.OutputTokens, &r.ReasoningTokens,
			&r.CacheReadTokens, &r.CacheWrite5m, &r.CacheWrite1h, &r.Cost); err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

// AllIDs 查询某 workspace 已入库的全部记录 id（用于增量去重判断）
func (s *Store) AllIDs(ctx context.Context, workspaceID string) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM usage_records WHERE workspace_id = ?`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = struct{}{}
	}
	return ids, rows.Err()
}

// Query 按月x模型分组统计
func (s *Store) Query(ctx context.Context, workspaceID string) ([]struct {
	Month, Model     string
	Count            int64
	CostUSD          float64
	InputTokens      int64
	OutputTokens     int64
	ReasoningTokens  int64
	CacheRead        int64
	CacheWrite5m     int64
	CacheWrite1h     int64
}, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT strftime('%Y-%m', time_created/1000, 'unixepoch') AS month,
       model,
       COUNT(*),
       SUM(cost_raw)/1e8,
       SUM(input_tokens + cache_read_tokens + cache_write_5m + cache_write_1h),
       SUM(output_tokens),
       SUM(reasoning_tokens),
       SUM(cache_read_tokens),
       SUM(cache_write_5m),
       SUM(cache_write_1h)
 FROM usage_records WHERE workspace_id = ?
GROUP BY month, model ORDER BY month DESC, 3 DESC`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []struct {
		Month, Model    string
		Count           int64
		CostUSD         float64
		InputTokens     int64
		OutputTokens    int64
		ReasoningTokens int64
		CacheRead       int64
		CacheWrite5m    int64
		CacheWrite1h    int64
	}
	for rows.Next() {
		var r struct {
			Month, Model    string
			Count           int64
			CostUSD         float64
			InputTokens     int64
			OutputTokens    int64
			ReasoningTokens int64
			CacheRead       int64
			CacheWrite5m    int64
			CacheWrite1h    int64
		}
		if err := rows.Scan(&r.Month, &r.Model, &r.Count, &r.CostUSD,
			&r.InputTokens, &r.OutputTokens, &r.ReasoningTokens,
			&r.CacheRead, &r.CacheWrite5m, &r.CacheWrite1h); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CycleModelStats 按订阅周期窗口（sinceMs 毫秒）分组统计 per-model
func (s *Store) CycleModelStats(ctx context.Context, workspaceID string, sinceMs int64) ([]struct {
	Model           string
	Count           int64
	CostUSD         float64
	InputTokens     int64
	OutputTokens    int64
	ReasoningTokens int64
	CacheRead       int64
	CacheWrite5m    int64
	CacheWrite1h    int64
}, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT model,
       COUNT(*),
       SUM(cost_raw)/1e8,
       SUM(input_tokens + cache_read_tokens + cache_write_5m + cache_write_1h),
       SUM(output_tokens),
       SUM(reasoning_tokens),
       SUM(cache_read_tokens),
       SUM(cache_write_5m),
       SUM(cache_write_1h)
 FROM usage_records WHERE workspace_id = ? AND time_created >= ?
 GROUP BY model ORDER BY 2 DESC`, workspaceID, sinceMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []struct {
		Model           string
		Count           int64
		CostUSD         float64
		InputTokens     int64
		OutputTokens    int64
		ReasoningTokens int64
		CacheRead       int64
		CacheWrite5m    int64
		CacheWrite1h    int64
	}
	for rows.Next() {
		var r struct {
			Model           string
			Count           int64
			CostUSD         float64
			InputTokens     int64
			OutputTokens    int64
			ReasoningTokens int64
			CacheRead       int64
			CacheWrite5m    int64
			CacheWrite1h    int64
		}
		if err := rows.Scan(&r.Model, &r.Count, &r.CostUSD, &r.InputTokens, &r.OutputTokens, &r.ReasoningTokens, &r.CacheRead, &r.CacheWrite5m, &r.CacheWrite1h); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CycleDeepseekSplit 按订阅周期窗口拆分 deepseek 各模型的峰/谷用量。
// 峰时定义同 DeepseekPeak：北京时间周一至周五 09-12 / 14-18。
// 返回每行一个 (model, isPeak) 组合，非 deepseek 模型 isPeak 恒为 0。
func (s *Store) CycleDeepseekSplit(ctx context.Context, workspaceID string, sinceMs int64) ([]struct {
	Model       string
	IsPeak      int64
	Count       int64
	CostUSD     float64
	InputTokens int64
}, error) {
	const tzShift = 28800000 // +8h(ms)，换算北京时间
	rows, err := s.db.QueryContext(ctx, `
SELECT model,
  (CAST(strftime('%w', (time_created+?)/1000, 'unixepoch') AS INTEGER) IN (1,2,3,4,5)
    AND CAST(strftime('%H', (time_created+?)/1000, 'unixepoch') AS INTEGER)
      IN (9,10,11,14,15,16,17)) AS is_peak,
  COUNT(*),
  SUM(cost_raw)/1e8,
  SUM(input_tokens + cache_read_tokens + cache_write_5m + cache_write_1h)
 FROM usage_records WHERE workspace_id = ? AND time_created >= ? AND model LIKE 'deepseek%'
 GROUP BY model, is_peak`, tzShift, tzShift, workspaceID, sinceMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []struct {
		Model       string
		IsPeak      int64
		Count       int64
		CostUSD     float64
		InputTokens int64
	}
	for rows.Next() {
		var r struct {
			Model       string
			IsPeak      int64
			Count       int64
			CostUSD     float64
			InputTokens int64
		}
		if err := rows.Scan(&r.Model, &r.IsPeak, &r.Count, &r.CostUSD, &r.InputTokens); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// QueryRow 执行单值聚合查询（agg 为 MIN/MAX 等），写入 dest
func (s *Store) QueryRow(ctx context.Context, workspaceID, agg string, dest *int64) error {
	return s.db.QueryRowContext(ctx,
		`SELECT `+agg+`(time_created) FROM usage_records WHERE workspace_id = ?`, workspaceID).Scan(dest)
}

// CountAll 统计某 workspace 的记录数（写入 dest）
func (s *Store) CountAll(ctx context.Context, workspaceID string, dest *int64) error {
	return s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM usage_records WHERE workspace_id = ?`, workspaceID).Scan(dest)
}

// MonthStats 按月分组统计
func (s *Store) MonthStats(ctx context.Context, workspaceID string) ([]struct {
	Month           string
	Count           int64
	CostUSD         float64
	InputTokens     int64
	OutputTokens    int64
	ReasoningTokens int64
}, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT strftime('%Y-%m', time_created/1000, 'unixepoch') AS month,
       COUNT(*),
       SUM(cost_raw)/1e8,
       SUM(input_tokens + cache_read_tokens + cache_write_5m + cache_write_1h),
       SUM(output_tokens),
       SUM(reasoning_tokens)
 FROM usage_records WHERE workspace_id = ?
GROUP BY month ORDER BY month DESC`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []struct {
		Month           string
		Count           int64
		CostUSD         float64
		InputTokens     int64
		OutputTokens    int64
		ReasoningTokens int64
	}
	for rows.Next() {
		var r struct {
			Month           string
			Count           int64
			CostUSD         float64
			InputTokens     int64
			OutputTokens    int64
			ReasoningTokens int64
		}
		if err := rows.Scan(&r.Month, &r.Count, &r.CostUSD,
			&r.InputTokens, &r.OutputTokens, &r.ReasoningTokens); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ModelStats 按模型分组统计（调用次数降序）
func (s *Store) ModelStats(ctx context.Context, workspaceID string) ([]struct {
	Model           string
	Count           int64
	CostUSD         float64
	InputTokens     int64
	OutputTokens    int64
	ReasoningTokens int64
}, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT model,
       COUNT(*),
       SUM(cost_raw)/1e8,
       SUM(input_tokens + cache_read_tokens + cache_write_5m + cache_write_1h),
       SUM(output_tokens),
       SUM(reasoning_tokens)
FROM usage_records WHERE workspace_id = ?
GROUP BY model ORDER BY 2 DESC`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []struct {
		Model           string
		Count           int64
		CostUSD         float64
		InputTokens     int64
		OutputTokens    int64
		ReasoningTokens int64
	}
	for rows.Next() {
		var r struct {
			Model           string
			Count           int64
			CostUSD         float64
			InputTokens     int64
			OutputTokens    int64
			ReasoningTokens int64
		}
		if err := rows.Scan(&r.Model, &r.Count, &r.CostUSD,
			&r.InputTokens, &r.OutputTokens, &r.ReasoningTokens); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CacheStats 按月统计缓存读写 tokens
func (s *Store) CacheStats(ctx context.Context, workspaceID string) ([]struct {
	Month          string
	Count          int64
	InputTokens    int64
	CacheRead      int64
	CacheWrite5m   int64
	CacheWrite1h   int64
}, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT strftime('%Y-%m', time_created/1000, 'unixepoch') AS month,
       COUNT(*),
       SUM(input_tokens),
       SUM(cache_read_tokens),
       SUM(cache_write_5m),
       SUM(cache_write_1h)
FROM usage_records WHERE workspace_id = ?
GROUP BY month ORDER BY month DESC`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []struct {
		Month        string
		Count        int64
		InputTokens  int64
		CacheRead    int64
		CacheWrite5m int64
		CacheWrite1h int64
	}
	for rows.Next() {
		var r struct {
			Month        string
			Count        int64
			InputTokens  int64
			CacheRead    int64
			CacheWrite5m int64
			CacheWrite1h int64
		}
		if err := rows.Scan(&r.Month, &r.Count, &r.InputTokens,
			&r.CacheRead, &r.CacheWrite5m, &r.CacheWrite1h); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DailyModelRow 每天x模型分组统计行，IsSubtotal 为 true 时表示某日小计行
// （Model 为空，各项为当日汇总）。
type DailyModelRow struct {
	Day, Model       string
	Count            int64
	CostUSD          float64
	InputTokens      int64
	OutputTokens     int64
	ReasoningTokens  int64
	CacheRead        int64
	CacheWrite5m     int64
	CacheWrite1h     int64
	IsSubtotal       bool
}

// DailyModelStats 按指定月份(YYYY-MM)的每天x模型分组统计
func (s *Store) DailyModelStats(ctx context.Context, workspaceID, monthPrefix string) ([]DailyModelRow, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT strftime('%Y-%m-%d', time_created/1000, 'unixepoch') AS day,
       model,
       COUNT(*),
       SUM(cost_raw)/1e8,
       SUM(input_tokens + cache_read_tokens + cache_write_5m + cache_write_1h),
       SUM(output_tokens),
       SUM(reasoning_tokens),
       SUM(cache_read_tokens),
       SUM(cache_write_5m),
       SUM(cache_write_1h)
 FROM usage_records WHERE workspace_id = ? AND day LIKE ? || '%'
GROUP BY day, model ORDER BY day DESC, 3 DESC`, workspaceID, monthPrefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DailyModelRow
	for rows.Next() {
		var r DailyModelRow
		if err := rows.Scan(&r.Day, &r.Model, &r.Count, &r.CostUSD,
			&r.InputTokens, &r.OutputTokens, &r.ReasoningTokens,
			&r.CacheRead, &r.CacheWrite5m, &r.CacheWrite1h); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// WithDailySubtotals 在每天x模型行之间插入每日小计行（IsSubtotal=true，紧随当日各模型行之后）。
// 输入须按 day 聚合（如 DailyModelStats 的 day DESC 输出），空输入返回空。
func WithDailySubtotals(rows []DailyModelRow) []DailyModelRow {
	if len(rows) == 0 {
		return rows
	}
	out := make([]DailyModelRow, 0, len(rows)+len(rows)/3+1)
	var curDay string
	var sCnt, sIn, sOut, sReason, sCacheR, sCacheW5, sCacheW1 int64
	var sCost float64
	flush := func() {
		if curDay == "" {
			return
		}
		out = append(out, DailyModelRow{
			Day: curDay, Count: sCnt, CostUSD: sCost,
			InputTokens: sIn, OutputTokens: sOut, ReasoningTokens: sReason,
			CacheRead: sCacheR, CacheWrite5m: sCacheW5, CacheWrite1h: sCacheW1,
			IsSubtotal: true,
		})
	}
	for _, r := range rows {
		if r.IsSubtotal {
			continue
		}
		if r.Day != curDay {
			flush()
			curDay = r.Day
			sCnt, sCost, sIn, sOut, sReason, sCacheR, sCacheW5, sCacheW1 = 0, 0, 0, 0, 0, 0, 0, 0
		}
		sCnt += r.Count
		sCost += r.CostUSD
		sIn += r.InputTokens
		sOut += r.OutputTokens
		sReason += r.ReasoningTokens
		sCacheR += r.CacheRead
		sCacheW5 += r.CacheWrite5m
		sCacheW1 += r.CacheWrite1h
		out = append(out, r)
	}
	flush()
	return out
}

// DeepseekPeak 统计 deepseek 模型自 startDay(北京时间 YYYY-MM-DD) 起，
// 每天按峰时段与谷时段拆分。
// 峰时定义（文档 https://opencode.ai/docs/zh-cn/go/）：周一至周五 01:00-04:00 / 06:00-10:00 UTC，
// 即北京时间 09:00-12:00 / 14:00-18:00；周末全天为谷时。
func (s *Store) DeepseekPeak(ctx context.Context, workspaceID, startDay string) ([]struct {
	Day                                 string
	Total                               int64
	PeakCalls                           int64
	PeakCost                            float64
	PeakInput, PeakOutput               int64
	OffCalls                            int64
	OffCost                             float64
	OffInput, OffOutput                 int64
}, error) {
	const tzShift = 28800000 // +8h(ms)，换算北京时间
	rows, err := s.db.QueryContext(ctx, `
WITH t AS (
  SELECT
    strftime('%Y-%m-%d', (time_created+?)/1000, 'unixepoch') AS day,
    (CAST(strftime('%w', (time_created+?)/1000, 'unixepoch') AS INTEGER) IN (1,2,3,4,5)
      AND CAST(strftime('%H', (time_created+?)/1000, 'unixepoch') AS INTEGER)
        IN (9,10,11,14,15,16,17)) AS is_peak,
    cost_raw,
    input_tokens + cache_read_tokens + cache_write_5m + cache_write_1h AS input_all,
    output_tokens
  FROM usage_records
  WHERE workspace_id = ? AND model LIKE 'deepseek%'
    AND strftime('%Y-%m-%d', (time_created+?)/1000, 'unixepoch') >= ?
)
SELECT day,
  COUNT(*),
  SUM(CASE WHEN is_peak THEN 1 ELSE 0 END),
  SUM(CASE WHEN is_peak THEN cost_raw ELSE 0 END)/1e8,
  SUM(CASE WHEN is_peak THEN input_all ELSE 0 END),
  SUM(CASE WHEN is_peak THEN output_tokens ELSE 0 END),
  SUM(CASE WHEN is_peak THEN 0 ELSE 1 END),
  SUM(CASE WHEN is_peak THEN 0 ELSE cost_raw END)/1e8,
  SUM(CASE WHEN is_peak THEN 0 ELSE input_all END),
  SUM(CASE WHEN is_peak THEN 0 ELSE output_tokens END)
 FROM t GROUP BY day ORDER BY day DESC`, tzShift, tzShift, tzShift, workspaceID, tzShift, startDay)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []struct {
		Day                                 string
		Total                               int64
		PeakCalls                           int64
		PeakCost                            float64
		PeakInput, PeakOutput               int64
		OffCalls                            int64
		OffCost                             float64
		OffInput, OffOutput                 int64
	}
	for rows.Next() {
		var r struct {
			Day                                 string
			Total                               int64
			PeakCalls                           int64
			PeakCost                            float64
			PeakInput, PeakOutput               int64
			OffCalls                            int64
			OffCost                             float64
			OffInput, OffOutput                 int64
		}
		if err := rows.Scan(&r.Day, &r.Total, &r.PeakCalls, &r.PeakCost,
			&r.PeakInput, &r.PeakOutput, &r.OffCalls, &r.OffCost,
			&r.OffInput, &r.OffOutput); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetLastPage 读取上次抓到的页数（断点续传）
func (s *Store) GetLastPage(ctx context.Context, workspaceID string) (int, error) {
	var page int
	err := s.db.QueryRowContext(ctx,
		`SELECT last_page FROM fetch_meta WHERE workspace_id = ?`, workspaceID).Scan(&page)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return page, err
}

// SetLastPage 记录已抓页数
func (s *Store) SetLastPage(ctx context.Context, workspaceID string, page int) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO fetch_meta (workspace_id, last_page, updated_at) VALUES (?, ?, ?)
ON CONFLICT(workspace_id) DO UPDATE SET last_page = excluded.last_page, updated_at = excluded.updated_at`,
		workspaceID, page, time.Now().UnixMilli())
	return err
}
