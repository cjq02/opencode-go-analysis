package subscription

import (
	"testing"
	"time"

	"opencode-go-analysis/internal/api"
)

func TestToMeterComputesPercentAndUSD(t *testing.T) {
	m := toMeter(api.GoMeter{LimitMicroCents: 6000000000, UsedMicroCents: 3295866115}, time.Time{})
	if m.LimitUSD != 60 {
		t.Errorf("LimitUSD = %v, want 60", m.LimitUSD)
	}
	if m.UsedUSD < 32.95 || m.UsedUSD > 32.96 {
		t.Errorf("UsedUSD = %v, want ~32.96", m.UsedUSD)
	}
	if m.UsedPercent < 54.9 || m.UsedPercent > 55.0 {
		t.Errorf("UsedPercent = %v, want ~54.93", m.UsedPercent)
	}
}

func TestToMeterParsesResetsAt(t *testing.T) {
	m := toMeter(api.GoMeter{
		ResetsAt:        "2026-09-21T11:14:11.441Z",
		LimitMicroCents: 1200000000,
		UsedMicroCents:  39230754,
	}, time.Time{})
	if m.ResetsAt.IsZero() {
		t.Fatalf("ResetsAt not parsed")
	}
	if m.ResetInSec <= 0 {
		t.Errorf("ResetInSec = %d, want > 0", m.ResetInSec)
	}
}

func TestFetchEmptyCookie(t *testing.T) {
	if _, err := Fetch(t.Context(), "", "wrk_x"); err == nil {
		t.Errorf("expected error for empty cookie")
	}
	if _, err := FetchBreakdown(t.Context(), "", "wrk_x", "monthly"); err == nil {
		t.Errorf("expected error for empty cookie")
	}
}
