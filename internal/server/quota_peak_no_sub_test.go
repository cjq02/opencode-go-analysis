package server

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"opencode-go-analysis/internal/model"
	"opencode-go-analysis/internal/quota"
	"opencode-go-analysis/internal/store"
	"opencode-go-analysis/internal/subscription"
)

// 订阅缺失时周期窗口必须回退 trailing 30d，否则峰谷拆分取不到数。
func TestCycleWindowFallsBackToTrailing30DWithoutSubscription(t *testing.T) {
	start, end := cycleWindow(nil)
	if end <= start {
		t.Fatalf("start=%d end=%d, want end > start", start, end)
	}
	want := int64(subscription.PeriodMonthly) * 1000
	if delta := end - start; delta != want {
		t.Errorf("window = %dms, want %dms (trailing 30d)", delta, want)
	}
}

func TestCycleWindowUsesSubscriptionPeriod(t *testing.T) {
	start := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	gotStart, gotEnd := cycleWindow(&subscription.Info{PeriodStart: start, PeriodEnd: end})
	if gotStart != start.UnixMilli() || gotEnd != end.UnixMilli() {
		t.Errorf("cycleWindow = (%d, %d), want (%d, %d)",
			gotStart, gotEnd, start.UnixMilli(), end.UnixMilli())
	}
}

// 回归：/go/status 返回 access:null → currentSub == nil 时，
// 满额预估不得退回不拆分的 calendar-month 路径，DeepSeek 峰/谷两行必须还在。
//
// 触发链：subscription.Fetch 因 st.Access == nil 报错 → currentSub 保持 nil →
// buildData 曾被 subForTpl.Access() 门控挡住整个周期聚合块 →
// quotaEstimate(monthRows, monthModelRows) 不做峰谷拆分。
func TestQuotaPeakSplitSurvivesMissingSubscription(t *testing.T) {
	savedQuota, savedSub, savedBD := currentQuota, currentSub, currentMonthlyBD
	t.Cleanup(func() { currentQuota, currentSub, currentMonthlyBD = savedQuota, savedSub, savedBD })
	currentQuota = quota.Default
	currentSub = nil // 复现 access 取不到时的状态
	currentMonthlyBD = nil

	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// 峰谷判定基于北京时间（SQL 里 time_created + 8h），因此按北京时区构造。
	beijing := time.FixedZone("CST", 8*3600)
	now := time.Now().In(beijing)
	var day time.Time
	for i := 1; i <= 7; i++ {
		d := now.AddDate(0, 0, -i)
		if w := int(d.Weekday()); w >= 1 && w <= 5 { // 北京时间周一至周五
			day = d
			break
		}
	}
	if day.IsZero() {
		t.Fatal("7 天内找不到工作日")
	}
	peakAt := time.Date(day.Year(), day.Month(), day.Day(), 10, 0, 0, 0, beijing) // 峰时 09-12/14-18
	offAt := time.Date(day.Year(), day.Month(), day.Day(), 3, 0, 0, 0, beijing)   // 谷时

	ws := "wrk_test"
	records := []model.UsageRecord{
		{ID: "peak-1", WorkspaceID: ws, TimeCreated: peakAt.UnixMilli(),
			Model: "deepseek-v4.1-flash", Provider: "deepseek",
			InputTokens: 1_000_000, Cost: 44_000_000},
		{ID: "off-1", WorkspaceID: ws, TimeCreated: offAt.UnixMilli(),
			Model: "deepseek-v4.1-flash", Provider: "deepseek",
			InputTokens: 1_000_000, Cost: 22_000_000},
	}
	for _, r := range records {
		if err := st.Upsert(ctx, r); err != nil {
			t.Fatalf("upsert %s: %v", r.ID, err)
		}
	}

	s := &Server{st: st, ws: ws, dailyMonDefault: time.Now().Format("2006-01"), peakStartDefault: "2026-08-17"}
	data := s.buildData(ctx, time.Now().Format("2006-01"), "2026-08-17")

	rows, ok := data["QuotaRows"].([]QuotaRow)
	if !ok {
		t.Fatalf("QuotaRows 类型为 %T, want []QuotaRow", data["QuotaRows"])
	}
	got := map[string]QuotaRow{}
	for _, r := range rows {
		got[r.Model] = r
	}
	for _, want := range []string{"deepseek-v4.1-flash (Peak)", "deepseek-v4.1-flash (Off-Peak)"} {
		if _, ok := got[want]; !ok {
			names := make([]string, 0, len(rows))
			for _, r := range rows {
				names = append(names, r.Model)
			}
			t.Fatalf("订阅缺失时缺少拆分行 %q，实际 QuotaRows = %v", want, names)
		}
	}
	// 拆分后两行各自保留自己的量，不得互相串数
	if got["deepseek-v4.1-flash (Peak)"].Count != 1 || got["deepseek-v4.1-flash (Off-Peak)"].Count != 1 {
		t.Errorf("Peak.Count=%d Off.Count=%d, want 1/1",
			got["deepseek-v4.1-flash (Peak)"].Count, got["deepseek-v4.1-flash (Off-Peak)"].Count)
	}
	// 峰单价约为谷的 2 倍（同 tokens 成本 0.44 vs 0.22）
	if got["deepseek-v4.1-flash (Peak)"].Per100M <= got["deepseek-v4.1-flash (Off-Peak)"].Per100M {
		t.Errorf("Peak.Per100M=%v 应高于 Off.Per100M=%v",
			got["deepseek-v4.1-flash (Peak)"].Per100M, got["deepseek-v4.1-flash (Off-Peak)"].Per100M)
	}
	if data["QuotaMonth"] == "" {
		t.Error("QuotaMonth 不应为空（应为周期窗口标签）")
	}
}
