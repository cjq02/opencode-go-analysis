package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"opencode-go-analysis/internal/model"
)

func TestFlexIntNumberOrString(t *testing.T) {
	var v struct {
		A flexInt `json:"a"`
		B flexInt `json:"b"`
		C flexInt `json:"c"`
	}
	raw := `{"a":2313409390,"b":"82131551","c":null}`
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v.A != 2313409390 || v.B != 82131551 || v.C != 0 {
		t.Errorf("got a=%d b=%d c=%d", v.A, v.B, v.C)
	}
}

func TestFetchUsageRowsPageMapsAndCursor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-org-id") != "wrk_x" {
			t.Errorf("x-org-id = %q, want wrk_x", r.Header.Get("x-org-id"))
		}
		if r.URL.Path != "/usage/rows" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("cursor") != "abc" {
			t.Errorf("cursor = %q", r.URL.Query().Get("cursor"))
		}
		w.Write([]byte(`{"items":[{"id":2313409390,"orgId":"wrk_x","model":"deepseek-v4.1-flash","provider":"opencode-go","inputTokens":229,"outputTokens":65,"reasoningTokens":0,"cacheReadTokens":14464,"cacheWrite5mTokens":0,"cacheWrite1hTokens":0,"costMicroCents":"23348","createdAt":"2026-09-21T08:48:41.000Z"}],"nextCursor":"n2"}`))
	}))
	defer srv.Close()

	c := New("ck", "wrk_x")
	c.BaseURL = srv.URL
	c.minGap = 0
	recs, next, err := c.FetchUsageRowsPage("all", "abc", 100)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d recs", len(recs))
	}
	r := recs[0]
	if r.Model != "deepseek-v4.1-flash" || r.InputTokens != 229 || r.CacheReadTokens != 14464 || r.Cost != 23348 {
		t.Errorf("bad record: %+v", r)
	}
	if r.TimeCreated == 0 {
		t.Errorf("TimeCreated not parsed")
	}
	if next != "n2" {
		t.Errorf("next = %q, want n2", next)
	}
}

func TestDoGetUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"_tag":"Unauthorized"}`))
	}))
	defer srv.Close()

	c := New("dead", "wrk_x")
	c.BaseURL = srv.URL
	c.minGap = 0
	if _, err := c.FetchSummary("30d"); err == nil {
		t.Fatalf("expected unauthorized error, got nil")
	}
}

// 回归：混合页（新记录在前、旧记录在后）必须既返回新记录、又标记 stop。
// 早前实现遇到旧记录就 return true，跳过了写库，导致增量永远 added:0。
func TestSplitFreshPageMixedPageKeepsNewRecords(t *testing.T) {
	const watermark = 2000
	recs := []model.UsageRecord{
		{ID: "new3", TimeCreated: 3000},
		{ID: "new2", TimeCreated: 2500},
		{ID: "new1", TimeCreated: 2100},
		{ID: "old2", TimeCreated: 1500},
		{ID: "old1", TimeCreated: 1000},
	}
	known := map[string]struct{}{}
	fresh, stop := SplitFreshPage(watermark, known, recs)
	if !stop {
		t.Errorf("stop = false, want true (hit records <= watermark)")
	}
	if len(fresh) != 3 {
		t.Fatalf("fresh = %d, want 3 (newer ones must not be dropped)", len(fresh))
	}
	for i, want := range []string{"new3", "new2", "new1"} {
		if fresh[i].ID != want {
			t.Errorf("fresh[%d] = %q, want %q", i, fresh[i].ID, want)
		}
	}
	if _, ok := known["old1"]; ok {
		t.Errorf("old record should not enter known set")
	}
}

func TestSplitFreshPageAllNewAndAllOld(t *testing.T) {
	// 全新：不停止，全部返回
	known := map[string]struct{}{}
	fresh, stop := SplitFreshPage(1000, known, []model.UsageRecord{
		{ID: "a", TimeCreated: 3000},
		{ID: "b", TimeCreated: 2000},
	})
	if stop || len(fresh) != 2 {
		t.Errorf("all-new: stop=%v len=%d, want false/2", stop, len(fresh))
	}

	// 全旧：停止，且一条都不写
	fresh, stop = SplitFreshPage(5000, map[string]struct{}{}, []model.UsageRecord{
		{ID: "c", TimeCreated: 3000},
	})
	if !stop || len(fresh) != 0 {
		t.Errorf("all-old: stop=%v len=%d, want true/0", stop, len(fresh))
	}
}
