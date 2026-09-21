package server

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"

	"opencode-go-analysis/internal/api"
	"opencode-go-analysis/internal/envfile"
	"opencode-go-analysis/internal/format"
	"opencode-go-analysis/internal/model"
	"opencode-go-analysis/internal/quota"
	"opencode-go-analysis/internal/store"
	"opencode-go-analysis/internal/subscription"
	"opencode-go-analysis/web"
	"strings"
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
	// 启动时异步预取额度与订阅周期
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		if q, err := quota.Fetch(ctx); err == nil {
			quotaMu.Lock()
			currentQuota = q
			quotaMu.Unlock()
			log.Printf("quota prefetch: 5h $%.0f / weekly $%.0f / monthly $%.0f, perModel %d", q.FiveHour, q.Weekly, q.Monthly, len(q.PerModel))
		}
		if ws != "" {
			_ = envfile.Load(".env")
			cookie := os.Getenv("OPENCODE_COOKIE")
			if cookie != "" {
				if sub, err := subscription.Fetch(ctx, cookie, ws); err == nil {
					subMu.Lock()
					currentSub = &sub
					subMu.Unlock()
					log.Printf("subscription prefetch: 5h $%.2f/$%.2f (%.1f%%), week $%.2f/$%.2f (%.1f%%), month $%.2f/$%.2f (%.1f%%)",
						sub.FiveHour.UsedUSD, sub.FiveHour.LimitUSD, sub.FiveHour.UsedPercent,
						sub.Weekly.UsedUSD, sub.Weekly.LimitUSD, sub.Weekly.UsedPercent,
						sub.Monthly.UsedUSD, sub.Monthly.LimitUSD, sub.Monthly.UsedPercent)
				} else {
					log.Printf("subscription prefetch failed: %v", err)
				}
				if bd, err := subscription.FetchBreakdown(ctx, cookie, ws, "monthly"); err == nil {
					monthlyBDMu.Lock()
					currentMonthlyBD = &bd
					monthlyBDMu.Unlock()
					log.Printf("subscription breakdown prefetch monthly: usage $%.2f rows %d", float64(bd.Usage)/1e8, len(bd.Rows))
				} else {
					log.Printf("subscription breakdown prefetch failed: %v", err)
				}
			}
		}
	}()
	tmpl := template.Must(template.New("site").Funcs(template.FuncMap{
		"thousands":  format.Thousands,
		"thousandsF": format.ThousandsF,
		"per100MIn":  format.Per100MIn,
		"compact":    format.CompactTokens,
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
	mux.HandleFunc("/api/subscription", s.handleSubscription)
	mux.HandleFunc("/api/subscription/refresh", s.handleSubscriptionRefresh)
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
	// 增量抓取时同步爬取最新额度与订阅周期
	if q, err := quota.Fetch(ctx); err == nil {
		quotaMu.Lock()
		currentQuota = q
		quotaMu.Unlock()
		log.Printf("quota refreshed on fetch: 5h $%.0f / weekly $%.0f / monthly $%.0f", q.FiveHour, q.Weekly, q.Monthly)
	} else {
		log.Printf("quota fetch on incremental failed, keep current: %v", err)
	}
	if sub, err := subscription.Fetch(ctx, cookie, s.ws); err == nil {
		subMu.Lock()
		currentSub = &sub
		subMu.Unlock()
		log.Printf("subscription refreshed on fetch: month $%.2f/$%.2f (%.1f%%)", sub.Monthly.UsedUSD, sub.Monthly.LimitUSD, sub.Monthly.UsedPercent)
	} else {
		log.Printf("subscription fetch on incremental failed, keep current: %v", err)
	}
	if bd, err := subscription.FetchBreakdown(ctx, cookie, s.ws, "monthly"); err == nil {
		monthlyBDMu.Lock()
		currentMonthlyBD = &bd
		monthlyBDMu.Unlock()
		log.Printf("subscription breakdown refreshed: monthly $%.2f rows %d", float64(bd.Usage)/1e8, len(bd.Rows))
	} else {
		log.Printf("subscription breakdown refresh failed: %v", err)
	}
	known, err := s.st.AllIDs(ctx, s.ws)
	if err != nil {
		http.Error(w, `{"error":"read db failed"}`, http.StatusInternalServerError)
		return
	}
	before := int64(len(known))
	// 新版 console API 记录 id 与旧 id 不同源，按时间戳水位做增量：
	// 记录按 createdAt 倒序返回，遇到不晚于水位的记录即可停止。
	watermark, err := s.st.LatestTimeCreated(ctx, s.ws)
	if err != nil {
		http.Error(w, `{"error":"read db failed"}`, http.StatusInternalServerError)
		return
	}
	client := api.New(cookie, s.ws)
	err = client.FetchUsagePages(ctx, s.ws, func(page int, recs []model.UsageRecord) (stop bool) {
		var fresh []model.UsageRecord
		for _, rec := range recs {
			if watermark > 0 && rec.TimeCreated <= watermark {
				return true // 已追平历史，后续更旧，无需再翻页
			}
			if _, dup := known[rec.ID]; dup {
				continue
			}
			known[rec.ID] = struct{}{}
			fresh = append(fresh, rec)
		}
		if len(fresh) > 0 {
			_ = s.st.BulkUpsert(ctx, fresh)
		}
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

var currentSub *subscription.Info
var subMu sync.RWMutex

var currentMonthlyBD *subscription.Breakdown
var monthlyBDMu sync.RWMutex

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
	RemainingTokens  int64
	RemainingUSD     float64
	TokensPer1USD    int64 // $1 可使用 tokens
	TokensPer100Calls int64 // 每百次消耗 tokens = Input / Count *100
}

type QuotaSummary struct {
	TotalCount          int64
	TotalCost           float64
	TotalQuotaUSD       float64
	RemainingUSD        float64
	TotalInput          int64
	TotalMaxMonthly     int64
	RemainingTokens     int64
	UsedPercent         float64
	TokensPer100Calls int64
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
	dailyTableRows := store.WithDailySubtotals(dailyRows)
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
	subMu.RLock()
	subForTpl := currentSub
	subMu.RUnlock()
	var quotaMonth string
	var quotaRows []QuotaRow
	var quotaCycleLabel string
	var subCycleStart, subCycleEnd string
	monthlyBDMu.RLock()
	bdForTpl := currentMonthlyBD
	monthlyBDMu.RUnlock()
	if subForTpl != nil && subForTpl.Access() {
		// 订阅月周期用官方 access.startsAt/endsAt；缺失时回退 trailing 30d
		monthlyStart := time.Now().UnixMilli() - int64(subscription.PeriodMonthly)*1000
		endMs := time.Now().UnixMilli()
		if !subForTpl.PeriodStart.IsZero() {
			monthlyStart = subForTpl.PeriodStart.UnixMilli()
		}
		if !subForTpl.PeriodEnd.IsZero() {
			endMs = subForTpl.PeriodEnd.UnixMilli()
		}
		subCycleStart = time.UnixMilli(monthlyStart).Format("2006-01-02")
		subCycleEnd = time.UnixMilli(endMs).Format("2006-01-02")
		quotaCycleLabel = fmt.Sprintf("%s ~ %s 订阅周期", subCycleStart, subCycleEnd)
		if cycleRows, err := s.st.CycleModelStats(ctx, s.ws, monthlyStart); err == nil && len(cycleRows) > 0 {
			// 同一模型不同 ID 先归一（如 deepseek-flash → deepseek-v4.1-flash），
			// 否则峰谷拆分时查不到对应项会漏拆，且明细表会出现两行。
			cycleRows = mergeCycleRowsByCanonical(cycleRows)
			// DeepSeek 按峰/谷拆分为两行（文档峰时：周一至周五 北京时间 09-12 / 14-18）
			expandedCycleRows := cycleRows
			if splitRows, err := s.st.CycleDeepseekSplit(ctx, s.ws, monthlyStart); err == nil && len(splitRows) > 0 {
				expandedCycleRows = expandCycleRowsWithPeak(cycleRows, mergeSplitRowsByCanonical(splitRows))
			}
			if bdForTpl != nil && len(bdForTpl.Rows) > 0 {
				quotaMonth, quotaRows = quotaEstimateFromBreakdown(bdForTpl, expandedCycleRows, quotaCycleLabel)
			} else {
				quotaMonth, quotaRows = quotaEstimateCycle(expandedCycleRows, quotaCycleLabel)
			}
		} else {
			quotaMonth, quotaRows = quotaEstimate(monthRows, monthModelRows)
			quotaCycleLabel = ""
		}
	} else {
		quotaMonth, quotaRows = quotaEstimate(monthRows, monthModelRows)
	}
	qLabels, q5h, qWeekly, qMonthly := quotaChartData(quotaRows)
	quotaSummary := buildQuotaSummary(quotaRows, qForTpl)
	if subForTpl != nil && subForTpl.Access() {
		// 已用占比以官方 Go 套餐月用量为准（与 /console/wrk_.../go 页一致）：
		// usedMicroCents/limitMicroCents，均为 1e-8 美元。
		var totalInput int64
		for _, r := range quotaRows {
			totalInput += r.InputTokens
		}
		quotaSummary.TotalInput = totalInput
		quotaSummary.TotalQuotaUSD = subForTpl.Monthly.LimitUSD
		quotaSummary.TotalCost = subForTpl.Monthly.UsedUSD
		quotaSummary.UsedPercent = subForTpl.Monthly.UsedPercent
		quotaSummary.RemainingUSD = subForTpl.Monthly.LimitUSD - subForTpl.Monthly.UsedUSD
		if subForTpl.Monthly.UsedUSD > 0 && totalInput > 0 {
			quotaSummary.TotalMaxMonthly = int64(float64(totalInput) / subForTpl.Monthly.UsedUSD * subForTpl.Monthly.LimitUSD)
		}
		quotaSummary.RemainingTokens = quotaSummary.TotalMaxMonthly - totalInput
		if quotaSummary.TotalCount > 0 {
			quotaSummary.TokensPer100Calls = quotaSummary.TotalInput * 100 / quotaSummary.TotalCount
		}
	} else if bdForTpl != nil && len(bdForTpl.Rows) > 0 {
		// 无订阅状态时回退到官方 30d 明细成本 / 月额度
		var totalInput int64
		for _, r := range quotaRows {
			totalInput += r.InputTokens
		}
		quotaSummary.TotalInput = totalInput
		quotaSummary.TotalQuotaUSD = qForTpl.Monthly
		officialCost := float64(bdForTpl.Usage) / 1e8
		quotaSummary.TotalCost = officialCost
		if quotaSummary.TotalQuotaUSD > 0 {
			quotaSummary.UsedPercent = officialCost / quotaSummary.TotalQuotaUSD * 100
		}
		quotaSummary.RemainingUSD = quotaSummary.TotalQuotaUSD - officialCost
		if officialCost > 0 && totalInput > 0 {
			quotaSummary.TotalMaxMonthly = int64(float64(totalInput) / officialCost * quotaSummary.TotalQuotaUSD)
		}
		quotaSummary.RemainingTokens = quotaSummary.TotalMaxMonthly - totalInput
		if quotaSummary.TotalCount > 0 {
			quotaSummary.TokensPer100Calls = quotaSummary.TotalInput * 100 / quotaSummary.TotalCount
		}
	}
	shareTop3, shareCheapest := buildShareData(quotaRows)
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
		"DailyRows":       dailyTableRows,
		"PeakRows":        peakRows,
		"Quota":           qForTpl,
		"QuotaMonth":      quotaMonth,
		"QuotaRows":       quotaRows,
		"QuotaSummary":    quotaSummary,
		"ShareTop3":       shareTop3,
		"ShareCheapest":   shareCheapest,
		"QuotaCycleLabel": quotaCycleLabel,
		"Subscription":    subForTpl,
		"SubCycleStart":   subCycleStart,
		"SubCycleEnd":     subCycleEnd,
		"MonthlyBD":       bdForTpl,
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

func (s *Server) handleSubscription(w http.ResponseWriter, r *http.Request) {
	subMu.RLock()
	sub := currentSub
	subMu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	if sub == nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "not fetched"})
		return
	}
	_ = json.NewEncoder(w).Encode(sub)
}

func (s *Server) handleSubscriptionRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	_ = envfile.Load(".env")
	cookie := os.Getenv("OPENCODE_COOKIE")
	if cookie == "" {
		http.Error(w, `{"error":"OPENCODE_COOKIE not set"}`, http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	sub, err := subscription.Fetch(ctx, cookie, s.ws)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadGateway)
		return
	}
	subMu.Lock()
	currentSub = &sub
	subMu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sub)
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
	// 同一模型不同 ID 先归一合并（如 deepseek-flash → deepseek-v4.1-flash）
	type mmRow = struct {
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
	}
	mergedMonth := make([]mmRow, 0, len(monthModelRows))
	mmIdx := make(map[string]int, len(monthModelRows))
	for _, r := range monthModelRows {
		if r.Month != latest {
			continue
		}
		key := canonicalModel(r.Model)
		if i, ok := mmIdx[key]; ok {
			m := &mergedMonth[i]
			m.Count += r.Count
			m.CostUSD += r.CostUSD
			m.InputTokens += r.InputTokens
			m.OutputTokens += r.OutputTokens
			m.ReasoningTokens += r.ReasoningTokens
			m.CacheRead += r.CacheRead
			m.CacheWrite5m += r.CacheWrite5m
			m.CacheWrite1h += r.CacheWrite1h
			continue
		}
		mmIdx[key] = len(mergedMonth)
		r.Model = key
		mergedMonth = append(mergedMonth, r)
	}
	var out []QuotaRow
	for _, r := range mergedMonth {
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
		tokensPer1USD := int64(float64(r.InputTokens) / r.CostUSD)
		tokensPer100Calls := int64(0)
		if r.Count > 0 {
			tokensPer100Calls = r.InputTokens * 100 / r.Count
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
			RemainingTokens:  maxMonthly - r.InputTokens,
			RemainingUSD:     monthlyQuota - r.CostUSD,
			TokensPer1USD:    tokensPer1USD,
			TokensPer100Calls: tokensPer100Calls,
		})
	}
	return latest, out
}

// isDeepSeekModel 是否为 DeepSeek 系模型（需区分峰/谷定价）
func isDeepSeekModel(m string) bool {
	return strings.HasPrefix(strings.ToLower(m), "deepseek")
}

// modelAliases 将已知的同一模型不同 ID 归一到 canonical ID。
// 如订阅明细把 deepseek-flash 标为 "DeepSeek V4.1 Flash"，与 deepseek-v4.1-flash 实为同一模型。
var modelAliases = map[string]string{
	"deepseek-flash": "deepseek-v4.1-flash",
}

// splitPeakSuffix 拆分 DeepSeek 峰谷显示后缀 " (Peak)" / " (Off-Peak)"。
func splitPeakSuffix(m string) (base, suffix string) {
	if i := strings.LastIndex(m, " ("); i >= 0 && strings.HasSuffix(m, ")") {
		return m[:i], m[i:]
	}
	return m, ""
}

// canonicalModel 返回归一后的模型 ID（保留峰谷后缀，匹配大小写不敏感）。
func canonicalModel(m string) string {
	base, suffix := splitPeakSuffix(m)
	if alias, ok := modelAliases[strings.ToLower(strings.TrimSpace(base))]; ok {
		return alias + suffix
	}
	return m
}

// mergeCycleRowsByCanonical 将周期聚合按 canonicalModel() 归一并合并（各项求和，保持首次出现顺序）。
func mergeCycleRowsByCanonical(rows []struct {
	Model           string
	Count           int64
	CostUSD         float64
	InputTokens     int64
	OutputTokens    int64
	ReasoningTokens int64
	CacheRead       int64
	CacheWrite5m    int64
	CacheWrite1h    int64
}) []struct {
	Model           string
	Count           int64
	CostUSD         float64
	InputTokens     int64
	OutputTokens    int64
	ReasoningTokens int64
	CacheRead       int64
	CacheWrite5m    int64
	CacheWrite1h    int64
} {
	type row = struct {
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
	merged := make([]row, 0, len(rows))
	idx := make(map[string]int, len(rows))
	for _, r := range rows {
		key := canonicalModel(r.Model)
		if i, ok := idx[key]; ok {
			m := &merged[i]
			m.Count += r.Count
			m.CostUSD += r.CostUSD
			m.InputTokens += r.InputTokens
			m.OutputTokens += r.OutputTokens
			m.ReasoningTokens += r.ReasoningTokens
			m.CacheRead += r.CacheRead
			m.CacheWrite5m += r.CacheWrite5m
			m.CacheWrite1h += r.CacheWrite1h
			continue
		}
		idx[key] = len(merged)
		r.Model = key
		merged = append(merged, r)
	}
	return merged
}

// mergeSplitRowsByCanonical 将峰谷拆分行按 canonicalModel() 归一并合并（同一模型+峰谷项求和）。
func mergeSplitRowsByCanonical(rows []struct {
	Model       string
	IsPeak      int64
	Count       int64
	CostUSD     float64
	InputTokens int64
}) []struct {
	Model       string
	IsPeak      int64
	Count       int64
	CostUSD     float64
	InputTokens int64
} {
	type row = struct {
		Model       string
		IsPeak      int64
		Count       int64
		CostUSD     float64
		InputTokens int64
	}
	merged := make([]row, 0, len(rows))
	idx := make(map[string]int, len(rows))
	for _, r := range rows {
		key := canonicalModel(r.Model) + "\x00" + string(rune('0'+r.IsPeak))
		if i, ok := idx[key]; ok {
			m := &merged[i]
			m.Count += r.Count
			m.CostUSD += r.CostUSD
			m.InputTokens += r.InputTokens
			continue
		}
		idx[key] = len(merged)
		r.Model = canonicalModel(r.Model)
		merged = append(merged, r)
	}
	return merged
}

// expandCycleRowsWithPeak 将周期聚合中的 deepseek 行按峰/谷拆分为两行。
// 显示名沿用文档口径 "model (Peak)" / "model (Off-Peak)"，quota.GetPerModel 会去掉括号取同一额度。
func expandCycleRowsWithPeak(cycleRows []struct {
	Model           string
	Count           int64
	CostUSD         float64
	InputTokens     int64
	OutputTokens    int64
	ReasoningTokens int64
	CacheRead       int64
	CacheWrite5m    int64
	CacheWrite1h    int64
}, splitRows []struct {
	Model       string
	IsPeak      int64
	Count       int64
	CostUSD     float64
	InputTokens int64
}) []struct {
	Model           string
	Count           int64
	CostUSD         float64
	InputTokens     int64
	OutputTokens    int64
	ReasoningTokens int64
	CacheRead       int64
	CacheWrite5m    int64
	CacheWrite1h    int64
} {
	type splitKey struct {
		peakCount int64
		peakCost  float64
		peakInput int64
		offCount  int64
		offCost   float64
		offInput  int64
		hasPeak   bool
		hasOff    bool
	}
	byModel := map[string]*splitKey{}
	for _, s := range splitRows {
		k, ok := byModel[s.Model]
		if !ok {
			k = &splitKey{}
			byModel[s.Model] = k
		}
		if s.IsPeak == 1 {
			k.peakCount, k.peakCost, k.peakInput = s.Count, s.CostUSD, s.InputTokens
			k.hasPeak = true
		} else {
			k.offCount, k.offCost, k.offInput = s.Count, s.CostUSD, s.InputTokens
			k.hasOff = true
		}
	}
	var out []struct {
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
	for _, r := range cycleRows {
		if !isDeepSeekModel(r.Model) {
			out = append(out, r)
			continue
		}
		sp, ok := byModel[r.Model]
		if !ok || !(sp.hasPeak && sp.hasOff) {
			// 只有单边数据时仍保留原行，避免拆分后丢失总量
			out = append(out, r)
			continue
		}
		out = append(out, struct {
			Model           string
			Count           int64
			CostUSD         float64
			InputTokens     int64
			OutputTokens    int64
			ReasoningTokens int64
			CacheRead       int64
			CacheWrite5m    int64
			CacheWrite1h    int64
		}{Model: r.Model + " (Peak)", Count: sp.peakCount, CostUSD: sp.peakCost, InputTokens: sp.peakInput})
		out = append(out, struct {
			Model           string
			Count           int64
			CostUSD         float64
			InputTokens     int64
			OutputTokens    int64
			ReasoningTokens int64
			CacheRead       int64
			CacheWrite5m    int64
			CacheWrite1h    int64
		}{Model: r.Model + " (Off-Peak)", Count: sp.offCount, CostUSD: sp.offCost, InputTokens: sp.offInput})
	}
	return out
}

func quotaEstimateCycle(cycleRows []struct {
	Model           string
	Count           int64
	CostUSD         float64
	InputTokens     int64
	OutputTokens    int64
	ReasoningTokens int64
	CacheRead       int64
	CacheWrite5m    int64
	CacheWrite1h    int64
}, label string) (string, []QuotaRow) {
	cycleRows = mergeCycleRowsByCanonical(cycleRows)
	var out []QuotaRow
	for _, r := range cycleRows {
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
		tokensPer1USD := int64(float64(r.InputTokens) / r.CostUSD)
		tokensPer100Calls := int64(0)
		if r.Count > 0 {
			tokensPer100Calls = r.InputTokens * 100 / r.Count
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
			RemainingTokens:  maxMonthly - r.InputTokens,
			RemainingUSD:     monthlyQuota - r.CostUSD,
			TokensPer1USD:    tokensPer1USD,
			TokensPer100Calls: tokensPer100Calls,
		})
	}
	return label, out
}

func quotaEstimateFromBreakdown(bd *subscription.Breakdown, cycleRows []struct {
	Model           string
	Count           int64
	CostUSD         float64
	InputTokens     int64
	OutputTokens    int64
	ReasoningTokens int64
	CacheRead       int64
	CacheWrite5m    int64
	CacheWrite1h    int64
}, label string) (string, []QuotaRow) {
	if bd == nil || len(bd.Rows) == 0 {
		return quotaEstimateCycle(cycleRows, label)
	}
	// 防御性归一：调用方一般已在 buildData 归一，直接调用时也保证一致。
	cycleRows = mergeCycleRowsByCanonical(cycleRows)
	// 建 DB cycle 映射以取 InputTokens/Count（订阅侧已给出 cost/quotaCost）
	dbMap := map[string]struct {
		Count       int64
		InputTokens int64
		CostUSD     float64
	}{}
	for _, r := range cycleRows {
		dbMap[r.Model] = struct {
			Count       int64
			InputTokens int64
			CostUSD     float64
		}{r.Count, r.InputTokens, r.CostUSD}
	}
	// 5h/周/月限额比例固定为文档口径：月 100%、周 50%、5 小时 20%。
	// 新版 console API 不再返回 token 限额，用标称月限额 6e9 换算比例即可。
	var limit5h, limitWeekly, limitMonthly int64 = 1200000000, 3000000000, 6000000000
	if bd.Limit > 0 {
		limitMonthly = bd.Limit
	}
	quotaMu.RLock()
	q := currentQuota
	quotaMu.RUnlock()
	if q.FiveHour == 0 {
		q = quota.Default
	}
	buildRow := func(displayModel string, count int64, costUSD float64, inputTokens int64) (QuotaRow, bool) {
		if inputTokens <= 0 || costUSD <= 0 {
			return QuotaRow{}, false
		}
		per100M := costUSD / float64(inputTokens) * 1e8
		// 周期额度取文档的每模型每月配额（GetPerModel 会去掉 " (Peak)" 后缀，峰/谷同额度，如 flash $30）
		monthlyQuotaUSD := q.GetPerModel(displayModel)
		var maxMonthly, maxWeekly, max5h int64
		if per100M > 0 {
			maxMonthly = int64(monthlyQuotaUSD / per100M * 1e8)
			if limitMonthly > 0 {
				maxWeekly = int64(float64(maxMonthly) * float64(limitWeekly) / float64(limitMonthly))
				max5h = int64(float64(maxMonthly) * float64(limit5h) / float64(limitMonthly))
			} else {
				maxWeekly = int64(q.Weekly / per100M * 1e8)
				max5h = int64(q.FiveHour / per100M * 1e8)
			}
		}
		usedPct := float64(costUSD) / monthlyQuotaUSD * 100
		tokensPer1USD := int64(float64(inputTokens) / costUSD)
		var tokensPer100Calls int64
		if count > 0 {
			tokensPer100Calls = inputTokens * 100 / count
		}
		return QuotaRow{
			Model:             displayModel,
			Count:             count,
			CostUSD:           costUSD,
			InputTokens:       inputTokens,
			Per100M:           per100M,
			QuotaUSD:          monthlyQuotaUSD,
			MaxTokens5h:       max5h,
			MaxTokensWeekly:   maxWeekly,
			MaxTokensMonthly:  maxMonthly,
			UsedPercent:       usedPct,
			RemainingTokens:   maxMonthly - inputTokens,
			RemainingUSD:      monthlyQuotaUSD - costUSD,
			TokensPer1USD:     tokensPer1USD,
			TokensPer100Calls: tokensPer100Calls,
		}, true
	}
	var out []QuotaRow
	// 订阅明细可能对同一模型返回多行（如不同 multiplier 桶：
	// glm-5.3-flash 曾同时出现 mult=2.0 与 mult=1.0 两行）。
	// 先按 canonicalModel() 合并，否则满额预估表会出现两行完全相同的行
	//（两行取同一份 DB 数据计算），deepseek 甚至会翻倍拆出 4 行。
	merged := make([]subscription.ModelBreakdown, 0, len(bd.Rows))
	idxByModel := make(map[string]int, len(bd.Rows))
	for _, mb := range bd.Rows {
		mb.Model = canonicalModel(mb.Model)
		if i, ok := idxByModel[mb.Model]; ok {
			m := &merged[i]
			m.Cost += mb.Cost
			m.QuotaCost += mb.QuotaCost
			m.ContributionPercent += mb.ContributionPercent
			if mb.Multiplier > m.Multiplier {
				m.Multiplier = mb.Multiplier
			}
			continue
		}
		idxByModel[mb.Model] = len(merged)
		merged = append(merged, mb)
	}
	for _, mb := range merged {
		// DeepSeek 在 cycleRows 中已拆为 "(Peak)" / "(Off-Peak)" 两行，此处分别建行
		if isDeepSeekModel(mb.Model) {
			peakEntry, hasPeak := dbMap[mb.Model+" (Peak)"]
			offEntry, hasOff := dbMap[mb.Model+" (Off-Peak)"]
			if hasPeak || hasOff {
				if hasPeak {
					if row, ok := buildRow(mb.Model+" (Peak)", peakEntry.Count, peakEntry.CostUSD, peakEntry.InputTokens); ok {
						out = append(out, row)
					}
				}
				if hasOff {
					if row, ok := buildRow(mb.Model+" (Off-Peak)", offEntry.Count, offEntry.CostUSD, offEntry.InputTokens); ok {
						out = append(out, row)
					}
				}
				continue
			}
		}
		dbEntry, hasDB := dbMap[mb.Model]
		var inputTokens int64
		var count int64
		var costUSD float64
		if hasDB {
			inputTokens = dbEntry.InputTokens
			count = dbEntry.Count
			costUSD = dbEntry.CostUSD
		} else {
			// 无 DB 时用订阅 cost 估算（cost/1e8 为 USD，无法得 tokens，仅作展示）
			costUSD = float64(mb.Cost) / 1e8
		}
		if inputTokens == 0 {
			continue
		}
		if row, ok := buildRow(mb.Model, count, costUSD, inputTokens); ok {
			out = append(out, row)
		}
	}
	// 按已用占比降序，便于表格突出高消耗模型
	sort.Slice(out, func(i, j int) bool { return out[i].UsedPercent > out[j].UsedPercent })
	return label, out
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

func buildShareData(rows []QuotaRow) ([]QuotaRow, *QuotaRow) {
	if len(rows) == 0 {
		return nil, nil
	}
	// Top3 已用占比最高
	cp := append([]QuotaRow(nil), rows...)
	sort.Slice(cp, func(i, j int) bool { return cp[i].UsedPercent > cp[j].UsedPercent })
	top3 := cp
	if len(top3) > 3 {
		top3 = top3[:3]
	}
	// 最省：每亿成本最低
	cp2 := append([]QuotaRow(nil), rows...)
	sort.Slice(cp2, func(i, j int) bool { return cp2[i].Per100M < cp2[j].Per100M })
	var cheapest *QuotaRow
	if len(cp2) > 0 {
		cheapest = &cp2[0]
	}
	return top3, cheapest
}

func buildQuotaSummary(rows []QuotaRow, q quota.Quota) QuotaSummary {
	var s QuotaSummary
	if q.Monthly == 0 {
		q = quota.Default
	}
	s.TotalQuotaUSD = q.Monthly // 总额度固定 $60
	var weightedCost float64
	for _, r := range rows {
		s.TotalCount += r.Count
		s.TotalInput += r.InputTokens
		// 按权重换算到总额度：cost * 60 / perModelQuota（如 glm $1 / $15 *60 = $4）
		w := r.CostUSD
		if r.QuotaUSD > 0 {
			w = r.CostUSD * q.Monthly / r.QuotaUSD
		}
		weightedCost += w
	}
	s.TotalCost = weightedCost // 汇总为加权后等效成本
	if s.TotalCost > 0 && s.TotalInput > 0 {
		// 需结合加权成本反推等效 tokens？保持按加权成本换算
		// 等效单价 = 加权成本 / 总输入，则满额 tokens = 60 / 单价
		s.TotalMaxMonthly = int64(float64(s.TotalInput) / s.TotalCost * q.Monthly)
		s.UsedPercent = s.TotalCost / q.Monthly * 100
	} else {
		s.TotalMaxMonthly = 0
		s.UsedPercent = 0
	}
	s.RemainingUSD = s.TotalQuotaUSD - s.TotalCost
	s.RemainingTokens = s.TotalMaxMonthly - s.TotalInput
	if s.TotalCount > 0 {
		s.TokensPer100Calls = s.TotalInput * 100 / s.TotalCount
	}
	return s
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

func dailyChartData(rows []store.DailyModelRow) ([]string, []float64) {
	m := map[string]float64{}
	var order []string
	for _, r := range rows {
		if r.IsSubtotal {
			continue
		}
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
