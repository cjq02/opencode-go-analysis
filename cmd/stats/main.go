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
		dbPath   string
		outDir   string
		ws       string
		fileName string
	)
	flag.StringVar(&dbPath, "db", "usage.db", "SQLite 数据库路径")
	flag.StringVar(&outDir, "out", "export", "输出目录")
	flag.StringVar(&ws, "workspace", "wrk_01KQE6ZT476376EYCQDQ0AMC28", "workspace id")
	flag.StringVar(&fileName, "file", "usage-by-month-model.md", "输出文件名")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "用法: %s [-db <sqlite路径>] [-out <dir>] [-workspace <id>] [-file <name>]\n\n", os.Args[0])
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

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "创建输出目录失败: %v\n", err)
		os.Exit(1)
	}
	path := outDir + "/" + fileName

	var total int64
	rows, err := st.Query(ctx, ws)
	if err != nil {
		fmt.Fprintf(os.Stderr, "查询失败: %v\n", err)
		os.Exit(1)
	}
	if err := st.CountAll(ctx, ws, &total); err != nil {
		fmt.Fprintf(os.Stderr, "统计失败: %v\n", err)
		os.Exit(1)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# opencode.ai 用量统计（按月 x 模型）\n\n")
	fmt.Fprintf(&b, "生成时间: %s\n", time.Now().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "数据范围: %s ~ %s (共 %s 条记录)\n", rows[0].Month, rows[len(rows)-1].Month, thousands(total))
	fmt.Fprintf(&b, "注: 输入tokens = input + cache_read + cache_write_5m + cache_write_1h (与页面一致)\n\n")
	fmt.Fprintf(&b, "| 月份 | 模型 | 调用次数 | 成本(USD) | 输入tokens | 输出tokens | 推理tokens |\n")
	fmt.Fprintf(&b, "|---|---|---:|---:|---:|---:|---:|\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "| %s | `%s` | %s | %s | %s | %s | %s |\n",
			r.Month, r.Model, thousands(r.Count), thousandsF(r.CostUSD),
			thousands(r.InputTokens), thousands(r.OutputTokens), thousands(r.ReasoningTokens))
	}

	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "写入文件失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("已生成 %s (共 %d 行)\n", path, len(rows))
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

type statRow struct {
	Month, Model     string
	Count            int64
	CostUSD          float64
	InputTokens      int64
	OutputTokens     int64
	ReasoningTokens  int64
}
