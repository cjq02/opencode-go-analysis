package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

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
	// 第一条：正常 inference/succeeded 行，需要入库
	good := `{"id":"2c0a9c0e-0000-4000-8000-000000000001","workspaceID":"wrk_x","startedAt":1758444521000,` +
		`"category":"inference","outcome":"succeeded","model":"deepseek-v4.1-flash","provider":"opencode",` +
		`"sessionID":"ses_1","inputTokens":229,"outputTokens":65,"reasoningTokens":0,` +
		`"cacheReadTokens":14464,"cacheWriteTokens":7,"cost":0.00023348}`
	// 第二条：非 inference 行，必须被丢弃（与旧 /usage/rows 语义一致）
	bad := `{"id":"d","workspaceID":"wrk_x","startedAt":1758444521000,"category":"api","outcome":"succeeded",` +
		`"model":"x","provider":"opencode","inputTokens":1,"outputTokens":2}`
	body := `{"items":[` + good + `,` + bad + `],"nextCursor":"n2","until":1758444521000,"retentionDays":30}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-org-id") != "wrk_x" {
			t.Errorf("x-org-id = %q, want wrk_x", r.Header.Get("x-org-id"))
		}
		if r.URL.Path != "/request-logs" {
			t.Errorf("path = %q, want /request-logs", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("cursor") != "abc" {
			t.Errorf("cursor = %q", q.Get("cursor"))
		}
		if q.Get("limit") != "100" {
			t.Errorf("limit = %q, want 100", q.Get("limit"))
		}
		if q.Get("category") != "inference" || q.Get("outcome") != "succeeded" {
			t.Errorf("filters = %q/%q", q.Get("category"), q.Get("outcome"))
		}
		w.Write([]byte(body))
	}))
	defer srv.Close()

	c := New("ck", "wrk_x")
	c.BaseURL = srv.URL
	c.minGap = 0
	recs, next, err := c.FetchUsageRowsPage("abc", 100)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d recs, want 1 (api-category row must be filtered out)", len(recs))
	}
	r := recs[0]
	if r.ID != "2c0a9c0e-0000-4000-8000-000000000001" {
		t.Errorf("ID = %q", r.ID)
	}
	if r.Model != "deepseek-v4.1-flash" || r.Provider != "opencode" || r.SessionID != "ses_1" {
		t.Errorf("bad record: %+v", r)
	}
	if r.InputTokens != 229 || r.CacheReadTokens != 14464 || r.CacheWrite5m != 7 {
		t.Errorf("bad tokens: %+v", r)
	}
	if r.Cost != 23348 {
		t.Errorf("Cost = %d, want 23348 (0.0023348 USD -> 1e-8 美元整数)", r.Cost)
	}
	if r.TimeCreated != 1758444521000 {
		t.Errorf("TimeCreated = %d, want startedAt 毫秒", r.TimeCreated)
	}
	if next != "n2" {
		t.Errorf("next = %q, want n2", next)
	}
}

// cost 缺失 / cacheWriteTokens 为 null 时不能 panic，按 0 处理。
func TestFetchUsageRowsPageNullCostAndCacheWrite(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"items":[{"id":"n1","workspaceID":"wrk_x","startedAt":9,` +
			`"category":"inference","outcome":"succeeded","model":"m","provider":"opencode",` +
			`"cacheWriteTokens":null}],"nextCursor":null,"until":9,"retentionDays":30}`))
	}))
	defer srv.Close()

	c := New("ck", "wrk_x")
	c.BaseURL = srv.URL
	c.minGap = 0
	recs, next, err := c.FetchUsageRowsPage("", 100)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d recs", len(recs))
	}
	if recs[0].Cost != 0 || recs[0].CacheWrite5m != 0 || recs[0].CacheWrite1h != 0 {
		t.Errorf("want zero values, got %+v", recs[0])
	}
	if next != "" {
		t.Errorf("next = %q, want empty", next)
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

// 回归：某页被 category/outcome 过滤成 0 条、但 nextCursor 仍有值时
// 不能当作“数据末尾”，否则会漏抓后续页。
func TestFetchUsagePagesSkipsEmptyFilteredPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cursor := r.URL.Query().Get("cursor")
		switch cursor {
		case "":
			// 第一页全是非 inference 行，被本地过滤后为空，但还有下一页
			w.Write([]byte(`{"items":[{"id":"x","workspaceID":"wrk_x","startedAt":9,` +
				`"category":"api","outcome":"failed","model":"m","provider":"opencode"}],` +
				`"nextCursor":"p2","until":9,"retentionDays":30}`))
		case "p2":
			w.Write([]byte(`{"items":[{"id":"y","workspaceID":"wrk_x","startedAt":10,` +
				`"category":"inference","outcome":"succeeded","model":"m","provider":"opencode",` +
				`"inputTokens":1,"outputTokens":2,"cost":0.00000001}],` +
				`"nextCursor":null,"until":10,"retentionDays":30}`))
		default:
			t.Errorf("unexpected cursor %q", cursor)
		}
	}))
	defer srv.Close()

	c := New("ck", "wrk_x")
	c.BaseURL = srv.URL
	c.minGap = 0

	var got []model.UsageRecord
	pages := 0
	err := c.FetchUsagePages(t.Context(), "wrk_x", func(p int, recs []model.UsageRecord) bool {
		pages++
		got = append(got, recs...)
		return false
	})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if pages != 2 {
		t.Errorf("pages = %d, want 2 (空过滤页不能提前终止)", pages)
	}
	if len(got) != 1 || got[0].ID != "y" {
		t.Fatalf("got %+v, want the record from page 2", got)
	}
	if got[0].Cost != 1 {
		t.Errorf("Cost = %d, want 1 (0.00000001 USD)", got[0].Cost)
	}
}

// 回归：水位掉出 request-logs 的 30 天保留窗口时必须给出警告，
// 否则数据缺口会静默产生且事后无法补回。
func TestCheckRetentionGap(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name      string
		watermark int64
		wantWarn  bool
	}{
		{"空库不告警", 0, false},
		{"水位在窗口内", now.Add(-6 * 24 * time.Hour).UnixMilli(), false},
		// 加 1 分钟余量：函数内部用 time.Now() 重算边界，直接取边界值会被时钟漂移误判
		{"水位紧贴窗口内侧", now.AddDate(0, 0, -RetentionDays).Add(time.Minute).UnixMilli(), false},
		{"水位掉出窗口", now.AddDate(0, 0, -RetentionDays-1).UnixMilli(), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckRetentionGap(tc.watermark)
			if tc.wantWarn && got == "" {
				t.Errorf("want warning, got empty")
			}
			if !tc.wantWarn && got != "" {
				t.Errorf("want no warning, got %q", got)
			}
			if tc.wantWarn && !strings.Contains(got, strconv.Itoa(RetentionDays)) {
				t.Errorf("warning should mention retention days: %q", got)
			}
		})
	}
}
