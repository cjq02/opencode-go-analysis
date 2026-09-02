package server

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"opencode-go-analysis/internal/api"
	"opencode-go-analysis/internal/envfile"
	"opencode-go-analysis/internal/format"
	"opencode-go-analysis/internal/model"
	"opencode-go-analysis/internal/quota"
	"opencode-go-analysis/internal/store"
	"opencode-go-analysis/web"
)

// TABLE_OFFSET 表格高度偏移常量：表格高度 = 100vh - TABLE_OFFSET
const TABLE_OFFSET = "280px"

// Server 封装本地动态服务的全部状态与路由
type Server struct {
	st                *store.Store
	tmpl              *template.Template
	ws                string
	dailyMonDefault   string
	peakStartDefault  string
	fetchMu           sync.Mutex
}

// New 创建 Server，打开数据库并解析模板
func New(dbPath, ws, dailyMon, peakStart string) (*Server, error) {
	if dailyMon == "" {
		dailyMon = time.Now().Format("2006-01")
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return nil, err
	}
	// 启动时异步预取额度，保证首屏即可显示按模型额度；增量抓取时也会同步刷新
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		if q, err := quota.Fetch(ctx); err == nil {
			quotaMu.Lock()
			currentQuota = q
			quotaMu.Unlock()
			log.Printf("quota prefetch: 5h $%.0f / weekly $%.0f / monthly $%.0f, perModel %d", q.FiveHour, q.Weekly, q.Monthly, len(q.PerModel))
		}
	}()
	tmpl := template.Must(template.New("site").Funcs(template.FuncMap{
		"thousands":  format.Thousands,
		"thousandsF": format.ThousandsF,
		"per100MIn":  format.Per100MIn,
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
	return &Server{st: st, tmpl: tmpl, ws: ws, dailyMonDefault: dailyMon, peakStartDefault: peakStart}, nil
}

// Close 关闭数据库
func (s *Server) Close() error { return s.st.Close() }

// Handler 返回已注册路由的 http.Handler
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/api/meta", s.handleMeta)
	mux.HandleFunc("/api/month", s.handleMonth)
	mux.HandleFunc("/api/model", s.handleModel)
	mux.HandleFunc("/api/fetch", s.handleFetch)
	mux.HandleFunc("/api/quota", s.handleQuota)
	mux.HandleFunc("/api/quota/refresh", s.handleQuotaRefresh)
	return mux
}

// Run 启动监听
func (s *Server) Run(addr string) error {
	log.Printf("listening on %s", addr)
	return http.ListenAndServe(addr, s.Handler())
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	dm := r.URL.Query().Get("month")
	if dm == "" {
		dm = s.dailyMonDefault
	}
	ps := r.URL.Query().Get("peak-start")
	if ps == "" {
		ps = s.peakStartDefault
	}
	data := s.buildData(r.Context(), dm, ps)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.Execute(w, data); err != nil {
		log.Printf("render: %v", err)
	}
}

func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) { s.serveJSON(w, r, "meta") }
func (s *Server) handleMonth(w http.ResponseWriter, r *http.Request) { s.serveJSON(w, r, "month") }
func (s *Server) handleModel(w http.ResponseWriter, r *http.Request) { s.serveJSON(w, r, "model") }

func (s *Server) serveJSON(w http.ResponseWriter, r *http.Request, kind string) {
	ctx := r.Context()
	w.Header().Set("Content-Type", "application/json")
	var v any
	switch kind {
	case "meta":
		var total int64
		_ = s.st.CountAll(ctx, s.ws, &total)
		v = map[string]any{"total": total, "total_fmt": format.Thousands(total)}
	case "month":
		rows, _ := s.st.MonthStats(ctx, s.ws)
		v = rows
	case "model":
		rows, _ := s.st.ModelStats(ctx, s.ws)
		v = rows
	default:
		v = map[string]string{"ok": "true"}
	}
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handleFetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.fetchMu.TryLock() {
		http.Error(w, `{"error":"fetch already running"}`, http.StatusTooManyRequests)
		return
	}
	defer s.fetchMu.Unlock()
	_ = envfile.Load(".env")
	cookie := os.Getenv("OPENCODE_COOKIE")
	if cookie == "" {
		http.Error(w, `{"error":"OPENCODE_COOKIE not set"}`, http.StatusInternalServerError)
		return
	}
	ctx := r.Context()
	// 增量抓取时同步爬取最新额度
	if q, err := quota.Fetch(ctx); err == nil {
		quotaMu.Lock()
		currentQuota = q
		quotaMu.Unlock()
		log.Printf("quota refreshed on fetch: 5h $%.0f / weekly $%.0f / monthly $%.0f", q.FiveHour, q.Weekly, q.Monthly)
	} else {
		log.Printf("quota fetch on incremental failed, keep current: %v", err)
	}
	known, err := s.st.AllIDs(ctx, s.ws)
	if err != nil {
		http.Error(w, `{"error":"read db failed"}`, http.StatusInternalServerError)
		return
	}
	before := int64(len(known))
	client := api.New(cookie)
	const staleStopLimit = 3
	stalePages := 0
	err = client.FetchUsagePages(ctx, s.ws, func(page int, recs []model.UsageRecord) (stop bool) {
		var fresh []model.UsageRecord
		for _, rec := range recs {
			if _, dup := known[rec.ID]; dup {
				continue
			}
			known[rec.ID] = struct{}{}
			fresh = append(fresh, rec)
		}
		if len(fresh) == 0 {
			stalePages++
			return stalePages >= staleStopLimit
		}
		stalePages = 0
		_ = s.st.BulkUpsert(ctx, fresh)
		return false
	})
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadGateway)
		return
	}
	var total int64
	_ = s.st.CountAll(ctx, s.ws, &total)
	added := total - before
	if added < 0 {
		added = 0
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"added": added, "total": total})
}

// 额度默认来自 https://opencode.ai/docs/zh-cn/go/#使用限制，启动时尝试爬取刷新
var currentQuota = quota.Default
var quotaMu sync.RWMutex

type QuotaRow struct {
	Model            string
	Count            int64
	CostUSD          float64
	InputTokens      int64
	Per100M          float64 // $ per 100M input tokens
	QuotaUSD         float64 // 当月按模型额度（来自文档“使用额度”列）
	MaxTokens5h      int64
	MaxTokensWeekly  int64
	MaxTokensMonthly int64
	UsedPercent      float64 // 已用 / 月满额 *100
}

// buildData 组装模板数据（表格降序，图表升序）
func (s *Server) buildData(ctx context.Context, dailyMon, peakStart string) map[string]any {
	var total int64
	_ = s.st.CountAll(ctx, s.ws, &total)
	minTS, maxTS := s.aggTS(ctx, "MIN"), s.aggTS(ctx, "MAX")
	rangeStr := fmt.Sprintf("%s ~ %s", time.UnixMilli(minTS).Format("2006-01-02"), time.UnixMilli(maxTS).Format("2006-01-02"))
	monthRows, _ := s.st.MonthStats(ctx, s.ws)
	modelRows, _ := s.st.ModelStats(ctx, s.ws)
	monthModelRows, _ := s.st.Query(ctx, s.ws)
	cacheRows, _ := s.st.CacheStats(ctx, s.ws)
	dailyRows, _ := s.st.DailyModelStats(ctx, s.ws, dailyMon)
	peakRows, _ := s.st.DeepseekPeak(ctx, s.ws, peakStart)
	monthLabels, monthCosts, monthCounts := labelsCosts(monthRows)
	modelLabels, modelCosts := modelLabelsCosts(modelRows)
	cacheLabels, cacheInput, cacheRead, cacheWrite := cacheChartData(cacheRows)
	dailyLabels, dailyCosts := dailyChartData(dailyRows)
	peakLabels, peakCosts, offCosts := peakChartData(peakRows)
	quotaMu.RLock()
	qForTpl := currentQuota
	quotaMu.RUnlock()
	if qForTpl.FiveHour == 0 {
		qForTpl = quota.Default
	}
	quotaMonth, quotaRows := quotaEstimate(monthRows, monthModelRows)
	qLabels, q5h, qWeekly, qMonthly := quotaChartData(quotaRows)
	return map[string]any{
		"GeneratedAt":     time.Now().Format("2006-01-02 15:04:05"),
		"Range":           rangeStr,
		"Total":           format.Thousands(total),
		"PeakStart":       peakStart,
		"DailyMonth":      dailyMon,
		"AvailableMonths": s.availableMonths(ctx),
		"TableOffset":     TABLE_OFFSET,
		"MonthRows":       monthRows,
		"ModelRows":       modelRows,
		"MonthModelRows":  monthModelRows,
		"CacheRows":       cacheRows,
		"DailyRows":       dailyRows,
		"PeakRows":        peakRows,
		"Quota":           qForTpl,
		"QuotaMonth":      quotaMonth,
		"QuotaRows":       quotaRows,
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
		"QuotaLabelsJSON": mustJSON(qLabels),
		"Quota5hJSON":     mustJSON(q5h),
		"QuotaWeeklyJSON": mustJSON(qWeekly),
		"QuotaMonthlyJSON": mustJSON(qMonthly),
	}
}

func (s *Server) handleQuota(w http.ResponseWriter, r *http.Request) {
	quotaMu.RLock()
	q := currentQuota
	quotaMu.RUnlock()
	if q.Source == "" {
		q = quota.Default
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(q)
}

func (s *Server) handleQuotaRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	q, err := quota.Fetch(ctx)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadGateway)
		return
	}
	quotaMu.Lock()
	currentQuota = q
	quotaMu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(q)
}

func quotaEstimate(monthRows []struct {
	Month           string
	Count           int64
	CostUSD         float64
	InputTokens     int64
	OutputTokens    int64
	ReasoningTokens int64
}, monthModelRows []struct {
	Month           string
	Model           string
	Count           int64
	CostUSD         float64
	InputTokens     int64
	OutputTokens    int64
	ReasoningTokens int64
	CacheRead       int64
	CacheWrite5m    int64
	CacheWrite1h    int64
}) (string, []QuotaRow) {
	if len(monthRows) == 0 {
		return "", nil
	}
	latest := monthRows[0].Month
	var out []QuotaRow
	for _, r := range monthModelRows {
		if r.Month != latest {
			continue
		}
		if r.InputTokens <= 0 || r.CostUSD <= 0 {
			continue
		}
		per100M := r.CostUSD / float64(r.InputTokens) * 1e8
		quotaMu.RLock()
		q := currentQuota
		quotaMu.RUnlock()
		if q.FiveHour == 0 {
			q = quota.Default
		}
		monthlyQuota := q.GetPerModel(r.Model)
		max5h := int64(float64(r.InputTokens) / r.CostUSD * q.FiveHour)
		maxWeekly := int64(float64(r.InputTokens) / r.CostUSD * q.Weekly)
		maxMonthly := int64(float64(r.InputTokens) / r.CostUSD * monthlyQuota)
		usedPct := 0.0
		if maxMonthly > 0 {
			usedPct = float64(r.InputTokens) / float64(maxMonthly) * 100
		}
		out = append(out, QuotaRow{
			Model:            r.Model,
			Count:            r.Count,
			CostUSD:          r.CostUSD,
			InputTokens:      r.InputTokens,
			Per100M:          per100M,
			QuotaUSD:         monthlyQuota,
			MaxTokens5h:      max5h,
			MaxTokensWeekly:  maxWeekly,
			MaxTokensMonthly: maxMonthly,
			UsedPercent:      usedPct,
		})
	}
	return latest, out
}

func quotaChartData(rows []QuotaRow) ([]string, []int64, []int64, []int64) {
	var l []string
	var a, b, c []int64
	for _, r := range rows {
		l = append(l, r.Model)
		a = append(a, r.MaxTokens5h)
		b = append(b, r.MaxTokensWeekly)
		c = append(c, r.MaxTokensMonthly)
	}
	return l, a, b, c
}

func (s *Server) availableMonths(ctx context.Context) []string {
	rows, _ := s.st.MonthStats(ctx, s.ws)
	var out []string
	for _, r := range rows {
		out = append(out, r.Month)
	}
	return out
}

func (s *Server) aggTS(ctx context.Context, agg string) int64 {
	var ts int64
	_ = s.st.QueryRow(ctx, s.ws, agg, &ts)
	return ts
}

func mustJSON(v any) template.JS {
	b, _ := json.Marshal(v)
	return template.JS(b)
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
	for i, j := 0, len(l)-1; i < j; i, j = i+1, j-1 {
		l[i], l[j] = l[j], l[i]
		c[i], c[j] = c[j], c[i]
		n[i], n[j] = n[j], n[i]
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
	for i, j := 0, len(l)-1; i < j; i, j = i+1, j-1 {
		l[i], l[j] = l[j], l[i]
		inp[i], inp[j] = inp[j], inp[i]
		rd[i], rd[j] = rd[j], rd[i]
		wr[i], wr[j] = wr[j], wr[i]
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
	for i, j := 0, len(order)-1; i < j; i, j = i+1, j-1 {
		order[i], order[j] = order[j], order[i]
		costs[i], costs[j] = costs[j], costs[i]
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
	for i, j := 0, len(l)-1; i < j; i, j = i+1, j-1 {
		l[i], l[j] = l[j], l[i]
		p[i], p[j] = p[j], p[i]
		o[i], o[j] = o[j], o[i]
	}
	return l, p, o
}
