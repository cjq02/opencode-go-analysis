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
)

func main() {
	var (
		dbPath    string
		addr      string
		ws        string
		peakStart string
		dailyMon  string
	)
	flag.StringVar(&dbPath, "db", "usage.db", "SQLite 路径")
	flag.StringVar(&addr, "addr", ":8080", "监听地址")
	flag.StringVar(&ws, "workspace", "wrk_01KQE6ZT476376EYCQDQ0AMC28", "workspace id")
	flag.StringVar(&peakStart, "peak-start", "2026-08-17", "deepseek 峰谷起始日")
	flag.StringVar(&dailyMon, "daily-month", "2026-08", "每日表月份")
	flag.Parse()

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
	}).Parse(htmlTmpl))

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

const htmlTmpl = `<!doctype html>
<html lang="zh-CN"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>opencode.ai 用量分析</title>
<script src="https://cdn.jsdelivr.net/npm/chart.js@4.4.7/dist/chart.umd.min.js"></script>
<style>
*{box-sizing:border-box}body{font-family:-apple-system,BlinkMacSystemFont,Segoe UI,Roboto,Helvetica,Arial,sans-serif;margin:0;background:#f6f7f9;color:#1a1a1a}
header{background:#111827;color:#fff;padding:20px 24px}header h1{margin:0;font-size:20px}header p{margin:6px 0 0;color:#9ca3af;font-size:13px}
nav{display:flex;gap:6px;padding:12px 16px;background:#fff;border-bottom:1px solid #e5e7eb;position:sticky;top:0;z-index:10;overflow-x:auto}
nav button{white-space:nowrap;padding:8px 14px;border:1px solid #e5e7eb;border-radius:999px;background:#fff;cursor:pointer;font-size:13px}
nav button.active{background:#111827;color:#fff;border-color:#111827}
main{max-width:1280px;margin:0 auto;padding:16px}
.card{background:#fff;border:1px solid #e5e7eb;border-radius:12px;padding:16px;margin-bottom:16px}
.card h2{margin:0 0 12px;font-size:15px}
canvas{max-height:320px}
table{width:100%;border-collapse:collapse;font-size:13px}
th,td{padding:8px 10px;border-bottom:1px solid #f0f0f0;text-align:right;white-space:nowrap}
th{position:sticky;top:0;background:#fafafa;cursor:pointer;user-select:none;text-align:right}
th:first-child,td:first-child{text-align:left}
tr:hover td{background:#fafafa}
.badge{display:inline-block;padding:2px 8px;border-radius:999px;background:#eef2ff;color:#3730a3;font-size:11px}
.tab{display:none}.tab.active{display:block}
.note{font-size:12px;color:#6b7280;margin-top:8px}
</style>
</head><body>
<header>
<h1>opencode.ai 用量分析</h1>
<p>生成时间 {{.GeneratedAt}} · 数据范围 {{.Range}} · 共 {{.Total}} 条 · 输入tokens = input + cache_read + cache_write · 本地实时</p>
</header>
<nav>
<button class="active" onclick="openTab(event,'month')">按月</button>
<button onclick="openTab(event,'model')">按模型</button>
<button onclick="openTab(event,'monthModel')">按月×模型</button>
<button onclick="openTab(event,'cache')">缓存</button>
<button onclick="openTab(event,'daily')">每日 ({{.DailyMonth}})</button>
<button onclick="openTab(event,'peak')">DeepSeek 峰谷 ({{.PeakStart}}起)</button>
</nav>
<main>
<div id="tab-month" class="tab active"><div class="card"><h2>按月成本与调用次数</h2><canvas id="chart-month"></canvas></div>
<div class="card"><h2>按月统计 <span class="badge">{{.Total}} 条</span></h2>
<table><thead><tr><th onclick="sortTable(this)">月份</th><th onclick="sortTable(this)">调用次数</th><th onclick="sortTable(this)">成本(USD)</th><th onclick="sortTable(this)">输入tokens</th><th onclick="sortTable(this)">输出tokens</th><th onclick="sortTable(this)">推理tokens</th></tr></thead><tbody>
{{range .MonthRows}}<tr><td>{{.Month}}</td><td data-sort="{{.Count}}">{{thousands .Count}}</td><td data-sort="{{.CostUSD}}">{{thousandsF .CostUSD}}</td><td data-sort="{{.InputTokens}}">{{thousands .InputTokens}}</td><td data-sort="{{.OutputTokens}}">{{thousands .OutputTokens}}</td><td data-sort="{{.ReasoningTokens}}">{{thousands .ReasoningTokens}}</td></tr>{{end}}
</tbody></table></div></div>
<div id="tab-model" class="tab"><div class="card"><h2>按模型成本</h2><canvas id="chart-model"></canvas></div>
<div class="card"><h2>按模型统计</h2>
<table><thead><tr><th onclick="sortTable(this)">模型</th><th onclick="sortTable(this)">调用次数</th><th onclick="sortTable(this)">成本(USD)</th><th onclick="sortTable(this)">输入tokens</th><th onclick="sortTable(this)">输出tokens</th><th onclick="sortTable(this)">推理tokens</th></tr></thead><tbody>
{{range .ModelRows}}<tr><td style="text-align:left"><code>{{.Model}}</code></td><td data-sort="{{.Count}}">{{thousands .Count}}</td><td data-sort="{{.CostUSD}}">{{thousandsF .CostUSD}}</td><td data-sort="{{.InputTokens}}">{{thousands .InputTokens}}</td><td data-sort="{{.OutputTokens}}">{{thousands .OutputTokens}}</td><td data-sort="{{.ReasoningTokens}}">{{thousands .ReasoningTokens}}</td></tr>{{end}}
</tbody></table></div></div>
<div id="tab-monthModel" class="tab"><div class="card"><h2>按月 × 模型 <span class="badge">每亿输入tok</span></h2>
<table><thead><tr><th onclick="sortTable(this)">月份</th><th onclick="sortTable(this)">模型</th><th onclick="sortTable(this)">调用次数</th><th onclick="sortTable(this)">成本</th><th onclick="sortTable(this)">每亿输入tok</th><th onclick="sortTable(this)">输入tokens</th><th onclick="sortTable(this)">缓存读取</th><th onclick="sortTable(this)">缓存写入</th><th onclick="sortTable(this)">输出tokens</th><th onclick="sortTable(this)">推理tokens</th></tr></thead><tbody>
{{range .MonthModelRows}}<tr><td>{{.Month}}</td><td><code>{{.Model}}</code></td><td data-sort="{{.Count}}">{{thousands .Count}}</td><td data-sort="{{.CostUSD}}">{{thousandsF .CostUSD}}</td><td>{{per100MIn .CostUSD .InputTokens}}</td><td data-sort="{{.InputTokens}}">{{thousands .InputTokens}}</td><td data-sort="{{.CacheRead}}">{{thousands .CacheRead}}</td><td data-sort="{{add .CacheWrite5m .CacheWrite1h}}">{{thousands (add .CacheWrite5m .CacheWrite1h)}}</td><td data-sort="{{.OutputTokens}}">{{thousands .OutputTokens}}</td><td data-sort="{{.ReasoningTokens}}">{{thousands .ReasoningTokens}}</td></tr>{{end}}
</tbody></table></div></div>
<div id="tab-cache" class="tab"><div class="card"><h2>缓存占比（按月）</h2><canvas id="chart-cache"></canvas></div>
<div class="card"><h2>缓存统计</h2>
<table><thead><tr><th onclick="sortTable(this)">月份</th><th onclick="sortTable(this)">调用次数</th><th onclick="sortTable(this)">输入(未命中)</th><th onclick="sortTable(this)">缓存读取</th><th onclick="sortTable(this)">缓存写入5m</th><th onclick="sortTable(this)">缓存写入1h</th><th onclick="sortTable(this)">缓存合计</th><th onclick="sortTable(this)">缓存占比</th></tr></thead><tbody>
{{range .CacheRows}}<tr><td>{{.Month}}</td><td data-sort="{{.Count}}">{{thousands .Count}}</td><td data-sort="{{.InputTokens}}">{{thousands .InputTokens}}</td><td data-sort="{{.CacheRead}}">{{thousands .CacheRead}}</td><td data-sort="{{.CacheWrite5m}}">{{thousands .CacheWrite5m}}</td><td data-sort="{{.CacheWrite1h}}">{{thousands .CacheWrite1h}}</td><td>{{thousands (add (add .CacheRead .CacheWrite5m) .CacheWrite1h)}}</td><td>{{printf "%.1f%%" (cachePct .InputTokens .CacheRead .CacheWrite5m .CacheWrite1h)}}</td></tr>{{end}}
</tbody></table></div></div>
<div id="tab-daily" class="tab"><div class="card"><h2>{{.DailyMonth}} 每日成本</h2><canvas id="chart-daily"></canvas></div>
<div class="card"><h2>{{.DailyMonth}} 每日 × 模型</h2>
<table><thead><tr><th onclick="sortTable(this)">日期</th><th onclick="sortTable(this)">模型</th><th onclick="sortTable(this)">调用次数</th><th onclick="sortTable(this)">成本</th><th onclick="sortTable(this)">每亿输入tok</th><th onclick="sortTable(this)">输入tokens</th><th onclick="sortTable(this)">缓存读取</th><th onclick="sortTable(this)">缓存写入</th><th onclick="sortTable(this)">输出tokens</th></tr></thead><tbody>
{{range .DailyRows}}<tr><td>{{.Day}}</td><td><code>{{.Model}}</code></td><td data-sort="{{.Count}}">{{thousands .Count}}</td><td data-sort="{{.CostUSD}}">{{thousandsF .CostUSD}}</td><td>{{per100MIn .CostUSD .InputTokens}}</td><td data-sort="{{.InputTokens}}">{{thousands .InputTokens}}</td><td data-sort="{{.CacheRead}}">{{thousands .CacheRead}}</td><td data-sort="{{add .CacheWrite5m .CacheWrite1h}}">{{thousands (add .CacheWrite5m .CacheWrite1h)}}</td><td data-sort="{{.OutputTokens}}">{{thousands .OutputTokens}}</td></tr>{{end}}
</tbody></table></div></div>
<div id="tab-peak" class="tab"><div class="card"><h2>DeepSeek 峰谷成本（峰 9-12,14-18 北京）</h2><canvas id="chart-peak"></canvas></div>
<div class="card"><h2>DeepSeek 峰谷明细</h2>
<table><thead><tr><th onclick="sortTable(this)">日期</th><th onclick="sortTable(this)">总调用</th><th onclick="sortTable(this)">峰调用</th><th onclick="sortTable(this)">峰占比</th><th onclick="sortTable(this)">峰成本</th><th onclick="sortTable(this)">谷成本</th><th onclick="sortTable(this)">峰每亿输入</th><th onclick="sortTable(this)">谷每亿输入</th></tr></thead><tbody>
{{range .PeakRows}}<tr><td>{{.Day}}</td><td data-sort="{{.Total}}">{{thousands .Total}}</td><td data-sort="{{.PeakCalls}}">{{thousands .PeakCalls}}</td><td>{{printf "%.1f%%" (peakPct .Total .PeakCalls)}}</td><td data-sort="{{.PeakCost}}">{{thousandsF .PeakCost}}</td><td data-sort="{{.OffCost}}">{{thousandsF .OffCost}}</td><td>{{per100MIn .PeakCost .PeakInput}}</td><td>{{per100MIn .OffCost .OffInput}}</td></tr>{{end}}
</tbody></table></div></div>
</main>
<script>
function openTab(e,n){document.querySelectorAll('.tab').forEach(x=>x.classList.remove('active'));document.getElementById('tab-'+n).classList.add('active');document.querySelectorAll('nav button').forEach(b=>b.classList.remove('active'));e.currentTarget.classList.add('active')}
function sortTable(th){
  const tbl=th.closest('table'), idx=[...th.parentNode.children].indexOf(th), tbody=tbl.tBodies[0];
  const asc=th.dataset.asc!=='1'; th.dataset.asc=asc?'1':'0';
  [...tbody.rows].sort((a,b)=>{
    const av=a.cells[idx].dataset.sort ?? a.cells[idx].innerText.replace(/[$,%]/g,'').replace(/,/g,'');
    const bv=b.cells[idx].dataset.sort ?? b.cells[idx].innerText.replace(/[$,%]/g,'').replace(/,/g,'');
    const an=parseFloat(av), bn=parseFloat(bv);
    if(!isNaN(an)&&!isNaN(bn)) return asc?an-bn:bn-an;
    return asc?av.localeCompare(bv):bv.localeCompare(av);
  }).forEach(r=>tbody.appendChild(r));
}
const mLabels={{.MonthLabelsJSON}}, mCosts={{.MonthCostsJSON}}, mCounts={{.MonthCountsJSON}};
const modLabels={{.ModelLabelsJSON}}, modCosts={{.ModelCostsJSON}};
const cLabels={{.CacheLabelsJSON}}, cIn={{.CacheInputJSON}}, cRd={{.CacheReadJSON}}, cWr={{.CacheWriteJSON}};
const dLabels={{.DailyLabelsJSON}}, dCosts={{.DailyCostsJSON}};
const pLabels={{.PeakLabelsJSON}}, pCosts={{.PeakCostsJSON}}, oCosts={{.OffCostsJSON}};
new Chart(document.getElementById('chart-month'),{type:'bar',data:{labels:mLabels,datasets:[{label:'成本 $',data:mCosts,yAxisID:'y',type:'line',borderColor:'#4f46e5',tension:.3},{label:'调用次数',data:mCounts,yAxisID:'y1',backgroundColor:'#e0e7ff'}]},options:{responsive:true,interaction:{mode:'index',intersect:false},scales:{y:{type:'linear',position:'left'},y1:{type:'linear',position:'right',grid:{drawOnChartArea:false}}}}});
new Chart(document.getElementById('chart-model'),{type:'bar',data:{labels:modLabels,datasets:[{label:'成本 $',data:modCosts,backgroundColor:'#6366f1'}]},options:{responsive:true,plugins:{legend:{display:false}}}});
new Chart(document.getElementById('chart-cache'),{type:'bar',data:{labels:cLabels,datasets:[{label:'输入(未命中)',data:cIn,backgroundColor:'#fca5a5'},{label:'缓存读取',data:cRd,backgroundColor:'#86efac'},{label:'缓存写入',data:cWr,backgroundColor:'#93c5fd'}]},options:{responsive:true,scales:{x:{stacked:true},y:{stacked:true}}}});
new Chart(document.getElementById('chart-daily'),{type:'line',data:{labels:dLabels,datasets:[{label:'每日成本 $',data:dCosts,borderColor:'#0ea5e9',backgroundColor:'rgba(14,165,233,.15)',fill:true,tension:.3}]},options:{responsive:true}});
new Chart(document.getElementById('chart-peak'),{type:'bar',data:{labels:pLabels,datasets:[{label:'峰成本',data:pCosts,backgroundColor:'#f59e0b'},{label:'谷成本',data:oCosts,backgroundColor:'#10b981'}]},options:{responsive:true,scales:{x:{stacked:true},y:{stacked:true}}}});
</script>
</body></html>
`
