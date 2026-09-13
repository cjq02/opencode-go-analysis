package server

import (
	"testing"

	"opencode-go-analysis/internal/quota"
	"opencode-go-analysis/internal/subscription"
)

type cycleRow = struct {
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

// 订阅明细对同一模型返回多行（不同 multiplier 桶）时，满额预估表不得出现重复行。
func TestQuotaEstimateFromBreakdownMergesDuplicateModels(t *testing.T) {
	currentQuota = quota.Default
	currentSub = nil

	bd := &subscription.Breakdown{
		Limit: 6000000000,
		Rows: []subscription.ModelBreakdown{
			{Model: "glm-5.3-flash", Name: "GLM 5.3 Flash", Cost: 222252144, QuotaCost: 444504288, Multiplier: 2, ContributionPercent: 7.4},
			{Model: "glm-5.3-flash", Name: "GLM 5.3 Flash", Cost: 77275183, QuotaCost: 77275183, Multiplier: 1, ContributionPercent: 1.3},
			{Model: "omen-alpha", Name: "Omen Alpha", Cost: 701706892, QuotaCost: 421024152, Multiplier: 0.6, ContributionPercent: 7},
		},
	}
	cycleRows := []cycleRow{
		{Model: "glm-5.3-flash", Count: 918, CostUSD: 2.99527327, InputTokens: 64830109},
		{Model: "omen-alpha", Count: 1447, CostUSD: 7.01706892, InputTokens: 133407933},
	}

	_, rows := quotaEstimateFromBreakdown(bd, cycleRows, "label")
	if len(rows) != 2 {
		t.Fatalf("want 2 rows after merge, got %d: %+v", len(rows), rows)
	}
	seen := map[string]int{}
	for _, r := range rows {
		seen[r.Model]++
	}
	for m, n := range seen {
		if n > 1 {
			t.Fatalf("duplicate row for %q x%d", m, n)
		}
	}
}

// deepseek 明细行重复时，峰/谷拆分不得翻倍（2 行明细 × 拆分 ≠ 4 行）。
func TestQuotaEstimateFromBreakdownDeepSeekDupNoQuadruple(t *testing.T) {
	currentQuota = quota.Default
	currentSub = nil

	bd := &subscription.Breakdown{
		Limit: 6000000000,
		Rows: []subscription.ModelBreakdown{
			{Model: "deepseek-v4-flash", Cost: 100, QuotaCost: 200, Multiplier: 2},
			{Model: "deepseek-v4-flash", Cost: 100, QuotaCost: 200, Multiplier: 2},
		},
	}
	cycleRows := []cycleRow{
		{Model: "deepseek-v4-flash (Peak)", Count: 10, CostUSD: 1.0, InputTokens: 1000},
		{Model: "deepseek-v4-flash (Off-Peak)", Count: 20, CostUSD: 1.0, InputTokens: 2000},
	}

	_, rows := quotaEstimateFromBreakdown(bd, cycleRows, "label")
	if len(rows) != 2 {
		t.Fatalf("want 2 rows (Peak + Off-Peak), got %d: %+v", len(rows), rows)
	}
}

func TestCanonicalModel(t *testing.T) {
	for in, want := range map[string]string{
		"deepseek-flash":            "deepseek-v4.1-flash",
		"DeepSeek-Flash":            "deepseek-v4.1-flash",
		"deepseek-flash (Peak)":     "deepseek-v4.1-flash (Peak)",
		"deepseek-flash (Off-Peak)": "deepseek-v4.1-flash (Off-Peak)",
		"deepseek-v4.1-flash":       "deepseek-v4.1-flash",
		"deepseek-v4-flash":         "deepseek-v4-flash",
		"glm-5.3-flash":             "glm-5.3-flash",
		"qwen3.7-plus":              "qwen3.7-plus",
	} {
		if got := canonicalModel(in); got != want {
			t.Errorf("canonicalModel(%q) = %q, want %q", in, got, want)
		}
	}
}

// deepseek-flash 应归属到 deepseek-v4.1-flash：明细与 DB 两侧合并为一行。
func TestQuotaEstimateDeepseekFlashMerged(t *testing.T) {
	currentQuota = quota.Default
	currentSub = nil

	bd := &subscription.Breakdown{
		Limit: 6000000000,
		Rows: []subscription.ModelBreakdown{
			{Model: "deepseek-v4.1-flash", Cost: 53741819, QuotaCost: 53741819, Multiplier: 1},
			{Model: "deepseek-flash", Cost: 26657672, QuotaCost: 26657672, Multiplier: 1},
		},
	}
	cycleRows := []cycleRow{
		{Model: "deepseek-v4.1-flash", Count: 455, CostUSD: 0.46793874, InputTokens: 46269196},
		{Model: "deepseek-flash", Count: 312, CostUSD: 0.26657672, InputTokens: 34767624},
	}

	_, rows := quotaEstimateFromBreakdown(bd, cycleRows, "label")
	if len(rows) != 1 {
		t.Fatalf("want 1 merged row, got %d: %+v", len(rows), rows)
	}
	got := rows[0]
	if got.Model != "deepseek-v4.1-flash" {
		t.Fatalf("want model deepseek-v4.1-flash, got %q", got.Model)
	}
	if got.Count != 767 {
		t.Errorf("want count 767, got %d", got.Count)
	}
	if got.InputTokens != 46269196+34767624 {
		t.Errorf("want input %d, got %d", 46269196+34767624, got.InputTokens)
	}
}

type splitRow = struct {
	Model       string
	IsPeak      int64
	Count       int64
	CostUSD     float64
	InputTokens int64
}

// 端到端：归一 → 峰谷拆分 → 明细建表，deepseek-flash 的峰谷量应并入 v4.1 的对应桶。
func TestDeepseekFlashPeakSplitMerged(t *testing.T) {
	currentQuota = quota.Default
	currentSub = nil

	cycle := mergeCycleRowsByCanonical([]cycleRow{
		{Model: "deepseek-v4.1-flash", Count: 455, CostUSD: 0.46, InputTokens: 4600},
		{Model: "deepseek-flash", Count: 312, CostUSD: 0.26, InputTokens: 3400},
	})
	split := mergeSplitRowsByCanonical([]splitRow{
		{Model: "deepseek-v4.1-flash", IsPeak: 1, Count: 34, CostUSD: 0.03, InputTokens: 300},
		{Model: "deepseek-v4.1-flash", IsPeak: 0, Count: 421, CostUSD: 0.43, InputTokens: 4300},
		{Model: "deepseek-flash", IsPeak: 0, Count: 312, CostUSD: 0.26, InputTokens: 3400},
	})
	expanded := expandCycleRowsWithPeak(cycle, split)
	bd := &subscription.Breakdown{
		Limit: 6000000000,
		Rows: []subscription.ModelBreakdown{
			{Model: "deepseek-v4.1-flash", Cost: 1, QuotaCost: 1},
			{Model: "deepseek-flash", Cost: 1, QuotaCost: 1},
		},
	}
	_, rows := quotaEstimateFromBreakdown(bd, expanded, "label")
	if len(rows) != 2 {
		t.Fatalf("want 2 rows (Peak + Off-Peak), got %d: %+v", len(rows), rows)
	}
	byModel := map[string]QuotaRow{}
	for _, r := range rows {
		byModel[r.Model] = r
	}
	peak, ok := byModel["deepseek-v4.1-flash (Peak)"]
	if !ok {
		t.Fatalf("missing Peak row: %+v", rows)
	}
	off, ok := byModel["deepseek-v4.1-flash (Off-Peak)"]
	if !ok {
		t.Fatalf("missing Off-Peak row: %+v", rows)
	}
	if peak.Count != 34 {
		t.Errorf("want peak count 34, got %d", peak.Count)
	}
	if off.Count != 421+312 {
		t.Errorf("want off-peak count 733, got %d", off.Count)
	}
	if _, dup := byModel["deepseek-flash"]; dup {
		t.Errorf("deepseek-flash should be merged, got extra row: %+v", rows)
	}
}
