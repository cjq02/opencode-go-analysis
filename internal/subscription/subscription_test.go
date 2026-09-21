package subscription

import (
	"testing"

	"opencode-go-analysis/internal/api"
)

func TestToWindowComputesTotals(t *testing.T) {
	w := toWindow("30d", api.Summary{
		TotalRequests:       18826,
		TotalInputTokens:    82131551,
		TotalOutputTokens:   10950807,
		TotalCacheRead:      2378367718,
		TotalCacheWrite5m:   2382815,
		TotalCacheWrite1h:   0,
		TotalCostMicroCents: 4360152185,
	})
	if w.TotalTokens != 82131551+10950807+2378367718+2382815 {
		t.Errorf("TotalTokens = %d", w.TotalTokens)
	}
	if w.CacheWriteTokens != 2382815 {
		t.Errorf("CacheWriteTokens = %d", w.CacheWriteTokens)
	}
	if w.CostUSD < 43.6 || w.CostUSD > 43.7 {
		t.Errorf("CostUSD = %v, want ~43.60", w.CostUSD)
	}
}

func TestFetchBreakdownEmptyCookie(t *testing.T) {
	if _, err := FetchBreakdown(t.Context(), "", "wrk_x", "monthly"); err == nil {
		t.Errorf("expected error for empty cookie")
	}
}
