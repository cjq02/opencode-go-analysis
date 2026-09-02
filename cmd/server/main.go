// opencode-server 本地动态服务：每次请求实时读 usage.db 渲染页面
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"
	"time"

	"opencode-go-analysis/internal/store"
	"opencode-go-analysis/web"
)

// 表格高度偏移常量：表格高度 = 100vh - TABLE_OFFSET，改这里即可（当前 280px）
const TABLE_OFFSET = "280px"

func main() {
	var (
		dbPath    string
		addr      string
		ws        string
		peakStart string
		dailyMon  string
	)
	flag.StringVar(&dbPath, "db", "usage.db", "SQLite 路径")
	flag.StringVar(&addr, "addr", ":18080", "监听地址")
	flag.StringVar(&ws, "workspace", "wrk_01KQE6ZT476376EYCQDQ0AMC28", "workspace id")
	flag.StringVar(&peakStart, "peak-start", "2026-08-17", "deepseek 峰谷起始日")
	flag.StringVar(&dailyMon, "daily-month", "", "每日表月份(默认当月 YYYY-MM)")
	flag.Parse()
	if dailyMon == "" {
		dailyMon = time.Now().Format("2006-01")
	}

	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer st.Close()

	tmpl := template.Must(template.New("site").Funcs(template.FuncMap{
		"thousands":  thousands,
		"thousandsF": thousandsF,
		"per100MIn":  per100MIn,
		"add":        func(a, b int64) int64 { return a + b },
		"cachePct": func(inp, rd, w5, w1 int64) float64 {
			c := rd + w5 + w1
			sum := inp + c
			if sum == 0 {
				return 0
			}
			return float64(c) / float64(sum) * 100
		},
		"peakPct": func(total, peak int64) float64 {
			if total == 0 {
				return 0
			}
			return float64(peak) / float64(total) * 100
		},
	}).Parse(web.IndexHTML))

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		ctx := r.Context()
		// 支持 ?month=YYYY-MM 覆盖每日视图
		dm := r.URL.Query().Get("month")
		if dm == "" {
			dm = dailyMon
		}
		ps := r.URL.Query().Get("peak-start")
		if ps == "" {
			ps = peakStart
		}
		data := buildData(ctx, st, ws, dm, ps)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.Execute(w, data); err != nil {
			log.Printf("render: %v", err)
		}
	})
	// JSON API 供按需刷新
	http.HandleFunc("/api/meta", func(w http.ResponseWriter, r *http.Request) { serveJSON(w, r, st, ws, dailyMon, peakStart, "meta") })
	http.HandleFunc("/api/month", func(w http.ResponseWriter, r *http.Request) { serveJSON(w, r, st, ws, dailyMon, peakStart, "month") })
	http.HandleFunc("/api/model", func(w http.ResponseWriter, r *http.Request) { serveJSON(w, r, st, ws, dailyMon, peakStart, "model") })

	log.Printf("listening on %s (db=%s)", addr, dbPath)
	log.Fatal(http.ListenAndServe(addr, nil))
}

func availableMonths(ctx context.Context, st *store.Store, ws string) []string {
	// 复用 MonthStats 的月份列表，避免新增 SQL
	rows, _ := st.MonthStats(ctx, ws)
	var out []string
	for _, r := range rows {
		out = append(out, r.Month)
	}
	return out
}

func buildData(ctx context.Context, st *store.Store, ws, dailyMon, peakStart string) map[string]any {
	var total int64
	_ = st.CountAll(ctx, ws, &total)
	minTS, maxTS := aggTS(ctx, st, ws, "MIN"), aggTS(ctx, st, ws, "MAX")
	rangeStr := fmt.Sprintf("%s ~ %s", time.UnixMilli(minTS).Format("2006-01-02"), time.UnixMilli(maxTS).Format("2006-01-02"))
	monthRows, _ := st.MonthStats(ctx, ws)
	modelRows, _ := st.ModelStats(ctx, ws)
	monthModelRows, _ := st.Query(ctx, ws)
	cacheRows, _ := st.CacheStats(ctx, ws)
	dailyRows, _ := st.DailyModelStats(ctx, ws, dailyMon)
	peakRows, _ := st.DeepseekPeak(ctx, ws, peakStart)
	monthLabels, monthCosts, monthCounts := labelsCosts(monthRows)
	modelLabels, modelCosts := modelLabelsCosts(modelRows)
	cacheLabels, cacheInput, cacheRead, cacheWrite := cacheChartData(cacheRows)
	dailyLabels, dailyCosts := dailyChartData(dailyRows)
	peakLabels, peakCosts, offCosts := peakChartData(peakRows)
	return map[string]any{
		"GeneratedAt":     time.Now().Format("2006-01-02 15:04:05"),
		"Range":           rangeStr,
		"Total":           thousands(total),
		"PeakStart":       peakStart,
		"DailyMonth":      dailyMon,
		"AvailableMonths": availableMonths(ctx, st, ws),
		"TableOffset":     TABLE_OFFSET,
		"MonthRows":       monthRows,
		"ModelRows":       modelRows,
		"MonthModelRows":  monthModelRows,
		"CacheRows":       cacheRows,
		"DailyRows":       dailyRows,
		"PeakRows":        peakRows,
		"MonthLabelsJSON": mustJSON(monthLabels),
		"MonthCostsJSON":  mustJSON(monthCosts),
		"MonthCountsJSON": mustJSON(monthCounts),
		"ModelLabelsJSON": mustJSON(modelLabels),
		"ModelCostsJSON":  mustJSON(modelCosts),
		"CacheLabelsJSON": mustJSON(cacheLabels),
		"CacheInputJSON":  mustJSON(cacheInput),
		"CacheReadJSON":   mustJSON(cacheRead),
		"CacheWriteJSON":  mustJSON(cacheWrite),
		"DailyLabelsJSON": mustJSON(dailyLabels),
		"DailyCostsJSON":  mustJSON(dailyCosts),
		"PeakLabelsJSON":  mustJSON(peakLabels),
		"PeakCostsJSON":   mustJSON(peakCosts),
		"OffCostsJSON":    mustJSON(offCosts),
	}
}

func serveJSON(w http.ResponseWriter, r *http.Request, st *store.Store, ws, dailyMon, peakStart, kind string) {
	ctx := r.Context()
	w.Header().Set("Content-Type", "application/json")
	var v any
	switch kind {
	case "meta":
		var total int64
		_ = st.CountAll(ctx, ws, &total)
		v = map[string]any{"total": total, "total_fmt": thousands(total)}
	case "month":
		rows, _ := st.MonthStats(ctx, ws)
		v = rows
	case "model":
		rows, _ := st.ModelStats(ctx, ws)
		v = rows
	default:
		v = map[string]string{"ok": "true"}
	}
	_ = json.NewEncoder(w).Encode(v)
}

func mustJSON(v any) template.JS {
	b, _ := json.Marshal(v)
	return template.JS(b)
}
func aggTS(ctx context.Context, st *store.Store, ws, agg string) int64 {
	var ts int64
	_ = st.QueryRow(ctx, ws, agg, &ts)
	return ts
}
func labelsCosts(rows []struct {
	Month           string
	Count           int64
	CostUSD         float64
	InputTokens     int64
	OutputTokens    int64
	ReasoningTokens int64
}) ([]string, []float64, []int64) {
	var l []string
	var c []float64
	var n []int64
	for _, r := range rows {
		l = append(l, r.Month)
		c = append(c, r.CostUSD)
		n = append(n, r.Count)
	}
	return l, c, n
}
func modelLabelsCosts(rows []struct {
	Model           string
	Count           int64
	CostUSD         float64
	InputTokens     int64
	OutputTokens    int64
	ReasoningTokens int64
}) ([]string, []float64) {
	var l []string
	var c []float64
	for _, r := range rows {
		l = append(l, r.Model)
		c = append(c, r.CostUSD)
	}
	return l, c
}
func cacheChartData(rows []struct {
	Month        string
	Count        int64
	InputTokens  int64
	CacheRead    int64
	CacheWrite5m int64
	CacheWrite1h int64
}) ([]string, []int64, []int64, []int64) {
	var l []string
	var inp, rd, wr []int64
	for _, r := range rows {
		l = append(l, r.Month)
		inp = append(inp, r.InputTokens)
		rd = append(rd, r.CacheRead)
		wr = append(wr, r.CacheWrite5m+r.CacheWrite1h)
	}
	return l, inp, rd, wr
}
func dailyChartData(rows []struct {
	Day, Model      string
	Count           int64
	CostUSD         float64
	InputTokens     int64
	OutputTokens    int64
	ReasoningTokens int64
	CacheRead       int64
	CacheWrite5m    int64
	CacheWrite1h    int64
}) ([]string, []float64) {
	m := map[string]float64{}
	var order []string
	for _, r := range rows {
		if _, ok := m[r.Day]; !ok {
			order = append(order, r.Day)
		}
		m[r.Day] += r.CostUSD
	}
	var costs []float64
	for _, d := range order {
		costs = append(costs, m[d])
	}
	return order, costs
}
func peakChartData(rows []struct {
	Day                       string
	Total                     int64
	PeakCalls                 int64
	PeakCost                  float64
	PeakInput, PeakOutput     int64
	OffCalls                  int64
	OffCost                   float64
	OffInput, OffOutput       int64
}) ([]string, []float64, []float64) {
	var l []string
	var p, o []float64
	for _, r := range rows {
		l = append(l, r.Day)
		p = append(p, r.PeakCost)
		o = append(o, r.OffCost)
	}
	return l, p, o
}
func thousands(n int64) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
func thousandsF(f float64) string {
	s := fmt.Sprintf("%.2f", f)
	ip := strings.IndexByte(s, '.')
	for i := ip - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return "$" + s
}
func per100MIn(cost float64, tokens int64) string {
	if tokens <= 0 {
		return "—"
	}
	return fmt.Sprintf("$%.2f", cost/float64(tokens)*1e8)
}

