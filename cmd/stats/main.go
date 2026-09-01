// opencode-stats 从 SQLite 生成用量统计 Markdown（按月 / 按模型 / 按月x模型）
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"opencode-go-analysis/internal/store"
)

func main() {
	var (
		dbPath string
		outDir string
		ws     string
		view   string
		month  string
		start  string
	)
	flag.StringVar(&dbPath, "db", "usage.db", "SQLite 数据库路径")
	flag.StringVar(&outDir, "out", "export", "输出目录")
	flag.StringVar(&ws, "workspace", "wrk_01KQE6ZT476376EYCQDQ0AMC28", "workspace id")
	flag.StringVar(&view, "view", "month-model", "统计视图: month | model | month-model | cache | daily-model | deepseek-peak")
	flag.StringVar(&month, "month", "", "月份前缀(YYYY-MM)，与 daily-model 配合使用")
	flag.StringVar(&start, "start", "", "起始日期(YYYY-MM-DD, 北京时间)，与 deepseek-peak 配合使用")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "用法: %s [-db <sqlite路径>] [-out <dir>] [-workspace <id>] [-view ...] [-month YYYY-MM] [-start YYYY-MM-DD]\n\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	ctx := context.Background()
	st, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开数据库失败: %v\n", err)
		os.Exit(1)
	}
	defer st.Close()

	var total int64
	if err := st.CountAll(ctx, ws, &total); err != nil {
		fmt.Fprintf(os.Stderr, "统计失败: %v\n", err)
		os.Exit(1)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# opencode.ai 用量统计\n\n")
	fmt.Fprintf(&b, "生成时间: %s\n", time.Now().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "数据范围: %s ~ %s (共 %s 条记录)\n", time.UnixMilli(minTS(ctx, st, ws)).Format("2006-01-02"), time.UnixMilli(maxTS(ctx, st, ws)).Format("2006-01-02"), thousands(total))
	fmt.Fprintf(&b, "注: 输入tokens = input + cache_read + cache_write_5m + cache_write_1h (与页面一致)\n\n")

	switch view {
	case "month":
		rows, err := st.MonthStats(ctx, ws)
		if err != nil {
			fmt.Fprintf(os.Stderr, "查询失败: %v\n", err)
			os.Exit(1)
		}
		renderMonth(&b, rows)
	case "model":
		rows, err := st.ModelStats(ctx, ws)
		if err != nil {
			fmt.Fprintf(os.Stderr, "查询失败: %v\n", err)
			os.Exit(1)
		}
		renderModel(&b, rows)
	case "cache":
		rows, err := st.CacheStats(ctx, ws)
		if err != nil {
			fmt.Fprintf(os.Stderr, "查询失败: %v\n", err)
			os.Exit(1)
		}
		renderCache(&b, rows)
	case "daily-model":
		if month == "" {
			fmt.Fprintln(os.Stderr, "错误: daily-model 视图需要 -month YYYY-MM 参数")
			os.Exit(2)
		}
		rows, err := st.DailyModelStats(ctx, ws, month)
		if err != nil {
			fmt.Fprintf(os.Stderr, "查询失败: %v\n", err)
			os.Exit(1)
		}
		renderDailyModel(&b, rows)
	case "deepseek-peak":
		if start == "" {
			fmt.Fprintln(os.Stderr, "错误: deepseek-peak 视图需要 -start YYYY-MM-DD 参数")
			os.Exit(2)
		}
		rows, err := st.DeepseekPeak(ctx, ws, start)
		if err != nil {
			fmt.Fprintf(os.Stderr, "查询失败: %v\n", err)
			os.Exit(1)
		}
		renderDeepseekPeak(&b, rows)
	default:
		rows, err := st.Query(ctx, ws)
		if err != nil {
			fmt.Fprintf(os.Stderr, "查询失败: %v\n", err)
			os.Exit(1)
		}
		renderMonthModel(&b, rows)
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "创建输出目录失败: %v\n", err)
		os.Exit(1)
	}
	path := outDir + "/usage-by-" + view + ".md"
	if view == "daily-model" {
		path = fmt.Sprintf("%s/usage-%s-daily-model.md", outDir, month)
	}
	if view == "deepseek-peak" {
		path = fmt.Sprintf("%s/usage-deepseek-peak-%s.md", outDir, start)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "写入文件失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("已生成 %s\n", path)
}

func minTS(ctx context.Context, st *store.Store, ws string) int64 { return aggTS(ctx, st, ws, "MIN") }
func maxTS(ctx context.Context, st *store.Store, ws string) int64 { return aggTS(ctx, st, ws, "MAX") }

func aggTS(ctx context.Context, st *store.Store, ws, agg string) int64 {
	var ts int64
	st.QueryRow(ctx, ws, agg, &ts)
	return ts
}

func renderMonth(b *strings.Builder, rows []struct {
	Month           string
	Count           int64
	CostUSD         float64
	InputTokens     int64
	OutputTokens    int64
	ReasoningTokens int64
}) {
	b.WriteString("## 按月统计\n\n")
	b.WriteString("| 月份 | 调用次数 | 成本(USD) | 输入tokens | 输出tokens | 推理tokens |\n")
	b.WriteString("|---|---:|---:|---:|---:|---:|\n")
	for _, r := range rows {
		fmt.Fprintf(b, "| %s | %s | %s | %s | %s | %s |\n",
			r.Month, thousands(r.Count), thousandsF(r.CostUSD),
			thousands(r.InputTokens), thousands(r.OutputTokens), thousands(r.ReasoningTokens))
	}
}

func renderModel(b *strings.Builder, rows []struct {
	Model           string
	Count           int64
	CostUSD         float64
	InputTokens     int64
	OutputTokens    int64
	ReasoningTokens int64
}) {
	b.WriteString("## 按模型统计\n\n")
	b.WriteString("| 模型 | 调用次数 | 成本(USD) | 输入tokens | 输出tokens | 推理tokens |\n")
	b.WriteString("|---|---:|---:|---:|---:|---:|\n")
	for _, r := range rows {
		fmt.Fprintf(b, "| `%s` | %s | %s | %s | %s | %s |\n",
			r.Model, thousands(r.Count), thousandsF(r.CostUSD),
			thousands(r.InputTokens), thousands(r.OutputTokens), thousands(r.ReasoningTokens))
	}
}

func renderMonthModel(b *strings.Builder, rows []struct {
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
}) {
	b.WriteString("## 按月 x 模型统计\n\n")
	b.WriteString("| 月份 | 模型 | 调用次数 | 成本(USD) | 每亿输入tok($) | 输入tokens | 缓存读取 | 缓存写入 | 输出tokens | 推理tokens |\n")
	b.WriteString("|---|---|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	var curMonth string
	var sCnt int64
	var sCost float64
	var sIn, sCacheR, sCacheW, sOut, sReason int64
	flush := func() {
		if curMonth == "" {
			return
		}
		fmt.Fprintf(b, "| **%s 小计** | — | %s | %s | %s | %s | %s | %s | %s | %s |\n",
			curMonth, thousands(sCnt), thousandsF(sCost), per100MIn(sCost, sIn),
			thousands(sIn), thousands(sCacheR), thousands(sCacheW),
			thousands(sOut), thousands(sReason))
	}
	for _, r := range rows {
		if r.Month != curMonth {
			flush()
			curMonth = r.Month
			sCnt, sCost, sIn, sCacheR, sCacheW, sOut, sReason = 0, 0, 0, 0, 0, 0, 0
		}
		sCnt += r.Count
		sCost += r.CostUSD
		sIn += r.InputTokens
		sCacheR += r.CacheRead
		sCacheW += r.CacheWrite5m + r.CacheWrite1h
		sOut += r.OutputTokens
		sReason += r.ReasoningTokens
		fmt.Fprintf(b, "| %s | `%s` | %s | %s | %s | %s | %s | %s | %s | %s |\n",
			r.Month, r.Model, thousands(r.Count), thousandsF(r.CostUSD), per100MIn(r.CostUSD, r.InputTokens),
			thousands(r.InputTokens), thousands(r.CacheRead), thousands(r.CacheWrite5m+r.CacheWrite1h),
			thousands(r.OutputTokens), thousands(r.ReasoningTokens))
	}
	flush()
}

// renderCache 渲染按月的缓存读写统计
func renderCache(b *strings.Builder, rows []struct {
	Month        string
	Count        int64
	InputTokens  int64
	CacheRead    int64
	CacheWrite5m int64
	CacheWrite1h int64
}) {
	b.WriteString("## 缓存统计（按月）\n\n")
	b.WriteString("| 月份 | 调用次数 | 输入tokens(未命中) | 缓存读取 | 缓存写入(5m) | 缓存写入(1h) | 缓存合计 | 缓存占比 |\n")
	b.WriteString("|---|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, r := range rows {
		cache := r.CacheRead + r.CacheWrite5m + r.CacheWrite1h
		pct := 0.0
		if sum := r.InputTokens + cache; sum > 0 {
			pct = float64(cache) / float64(sum) * 100
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s | %s | %s | %s | %.1f%% |\n",
			r.Month, thousands(r.Count), thousands(r.InputTokens),
			thousands(r.CacheRead), thousands(r.CacheWrite5m), thousands(r.CacheWrite1h),
			thousands(cache), pct)
	}
}

func renderDailyModel(b *strings.Builder, rows []struct {
	Day, Model       string
	Count            int64
	CostUSD          float64
	InputTokens      int64
	OutputTokens     int64
	ReasoningTokens  int64
	CacheRead        int64
	CacheWrite5m     int64
	CacheWrite1h     int64
}) {
	b.WriteString(fmt.Sprintf("## %s 每日 x 模型统计\n\n", rows[0].Day[:7]))
	b.WriteString("| 日期 | 模型 | 调用次数 | 成本(USD) | 每亿输入tok($) | 输入tokens | 缓存读取 | 缓存写入 | 输出tokens | 推理tokens |\n")
	b.WriteString("|---|---|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	var curDay string
	var sCnt int64
	var sCost float64
	var sIn, sCacheR, sCacheW, sOut, sReason int64
	flush := func() {
		if curDay == "" {
			return
		}
		fmt.Fprintf(b, "| **%s 小计** | — | %s | %s | %s | %s | %s | %s | %s | %s |\n",
			curDay, thousands(sCnt), thousandsF(sCost), per100MIn(sCost, sIn),
			thousands(sIn), thousands(sCacheR), thousands(sCacheW),
			thousands(sOut), thousands(sReason))
	}
	for _, r := range rows {
		if r.Day != curDay {
			flush()
			curDay = r.Day
			sCnt, sCost, sIn, sCacheR, sCacheW, sOut, sReason = 0, 0, 0, 0, 0, 0, 0
		}
		sCnt += r.Count
		sCost += r.CostUSD
		sIn += r.InputTokens
		sCacheR += r.CacheRead
		sCacheW += r.CacheWrite5m + r.CacheWrite1h
		sOut += r.OutputTokens
		sReason += r.ReasoningTokens
		fmt.Fprintf(b, "| %s | `%s` | %s | %s | %s | %s | %s | %s | %s | %s |\n",
			r.Day, r.Model, thousands(r.Count), thousandsF(r.CostUSD), per100MIn(r.CostUSD, r.InputTokens),
			thousands(r.InputTokens), thousands(r.CacheRead),
			thousands(r.CacheWrite5m+r.CacheWrite1h),
			thousands(r.OutputTokens), thousands(r.ReasoningTokens))
	}
	flush()
}

// per100MIn 返回每亿输入token(含缓存)成本，token 为 0 时返回 —
func per100MIn(cost float64, tokens int64) string {
	if tokens <= 0 {
		return "—"
	}
	return fmt.Sprintf("$%.2f", cost/float64(tokens)*1e8)
}

func renderDeepseekPeak(b *strings.Builder, rows []struct {
	Day                                       string
	Total                                     int64
	PeakCalls                                 int64
	PeakCost                                  float64
	PeakInput, PeakOutput                     int64
	OffCalls                                  int64
	OffCost                                   float64
	OffInput, OffOutput                       int64
}) {
	// 区间汇总，推算每亿 token 成本
	var sPeakCost, sPeakIn, sPeakOut, sOffCost, sOffIn, sOffOut float64
	for _, r := range rows {
		sPeakCost += r.PeakCost
		sPeakIn += float64(r.PeakInput)
		sPeakOut += float64(r.PeakOutput)
		sOffCost += r.OffCost
		sOffIn += float64(r.OffInput)
		sOffOut += float64(r.OffOutput)
	}
	pIn := func(c, t float64) string {
		if t <= 0 {
			return "—"
		}
		return fmt.Sprintf("$%.2f", c/t*1e8)
	}
	b.WriteString("## deepseek 峰谷时段统计（峰: 北京 9-12,14-18）\n\n")
	b.WriteString("### 每亿 token 成本推算（区间汇总）\n\n")
	b.WriteString("| 时段 | 总成本 | 总输入tokens | 总输出tokens | 每亿输入token | 每亿输出token |\n")
	b.WriteString("|---|---:|---:|---:|---:|---:|\n")
	fmt.Fprintf(b, "| 峰时段 | %s | %s | %s | %s | %s |\n",
		thousandsF(sPeakCost), thousands(int64(sPeakIn)), thousands(int64(sPeakOut)),
		pIn(sPeakCost, sPeakIn), pIn(sPeakCost, sPeakOut))
	fmt.Fprintf(b, "| 谷时段 | %s | %s | %s | %s | %s |\n",
		thousandsF(sOffCost), thousands(int64(sOffIn)), thousands(int64(sOffOut)),
		pIn(sOffCost, sOffIn), pIn(sOffCost, sOffOut))
	b.WriteString("\n")

	b.WriteString("### 每日明细\n\n")
	b.WriteString("| 日期 | 总调用 | 峰调用 | 峰占比 | 峰成本 | 峰每亿输入 | 峰每亿输出 | 谷调用 | 谷成本 | 谷每亿输入 | 谷每亿输出 |\n")
	b.WriteString("|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, r := range rows {
		pct := 0.0
		if r.Total > 0 {
			pct = float64(r.PeakCalls) / float64(r.Total) * 100
		}
		fmt.Fprintf(b, "| %s | %s | %s | %.1f%% | %s | %s | %s | %s | %s | %s | %s |\n",
			r.Day, thousands(r.Total), thousands(r.PeakCalls), pct, thousandsF(r.PeakCost),
			pIn(r.PeakCost, float64(r.PeakInput)), pIn(r.PeakCost, float64(r.PeakOutput)),
			thousands(r.OffCalls), thousandsF(r.OffCost),
			pIn(r.OffCost, float64(r.OffInput)), pIn(r.OffCost, float64(r.OffOutput)))
	}
}
func thousands(n int64) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// thousandsF 将金额格式化为千分位字符串（如 $1,234.56）
func thousandsF(f float64) string {
	s := fmt.Sprintf("%.2f", f)
	ip := strings.IndexByte(s, '.')
	for i := ip - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return "$" + s
}
