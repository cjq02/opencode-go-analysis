package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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
