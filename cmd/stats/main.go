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
	)
	flag.StringVar(&dbPath, "db", "usage.db", "SQLite 数据库路径")
	flag.StringVar(&outDir, "out", "export", "输出目录")
	flag.StringVar(&ws, "workspace", "wrk_01KQE6ZT476376EYCQDQ0AMC28", "workspace id")
	flag.StringVar(&view, "view", "month-model", "统计视图: month | model | month-model | cache")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "用法: %s [-db <sqlite路径>] [-out <dir>] [-workspace <id>] [-view month|model|month-model|cache]\n\n", os.Args[0])
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
	b.WriteString("| 月份 | 模型 | 调用次数 | 成本(USD) | 输入tokens | 缓存读取 | 缓存写入 | 输出tokens | 推理tokens |\n")
	b.WriteString("|---|---|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, r := range rows {
		fmt.Fprintf(b, "| %s | `%s` | %s | %s | %s | %s | %s | %s | %s |\n",
			r.Month, r.Model, thousands(r.Count), thousandsF(r.CostUSD),
			thousands(r.InputTokens), thousands(r.CacheRead), thousands(r.CacheWrite5m+r.CacheWrite1h),
			thousands(r.OutputTokens), thousands(r.ReasoningTokens))
	}
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

// thousands 将非负整数格式化为千分位字符串
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
