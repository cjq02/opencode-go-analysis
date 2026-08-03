// Package export 将用量数据导出为 CSV
package export

import (
	"encoding/csv"
	"fmt"
	"io"
	"sort"
	"time"

	"opencode-go-analysis/internal/model"
)

// WriteUsageCSV 按时间升序将记录写入 CSV（标题 + 数据行）
func WriteUsageCSV(w io.Writer, records []model.UsageRecord) error {
	sorted := make([]model.UsageRecord, len(records))
	copy(sorted, records)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].TimeCreated < sorted[j].TimeCreated
	})

	cw := csv.NewWriter(w)
	defer cw.Flush()
	if err := cw.Write([]string{"date", "model", "input_tokens", "output_tokens", "cost_usd", "session_id"}); err != nil {
		return err
	}
	for _, r := range sorted {
		date := time.UnixMilli(r.TimeCreated).Format("2006-01-02 15:04:05")
		row := []string{
			date,
			r.Model,
			fmt.Sprintf("%d", r.TotalInputTokens()),
			fmt.Sprintf("%d", r.OutputTokens),
			fmt.Sprintf("%.4f", r.CostUSD()),
			r.SessionID,
		}
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	return cw.Error()
}
