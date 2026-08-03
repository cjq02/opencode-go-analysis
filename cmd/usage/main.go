// opencode-usage 抓取 opencode.ai workspace 用量历史（日期/模型/输入/输出/成本）
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"opencode-go-analysis/internal/api"
	"opencode-go-analysis/internal/envfile"
	"opencode-go-analysis/internal/export"
	"opencode-go-analysis/internal/model"
	"opencode-go-analysis/internal/store"
)

func main() {
	var (
		cookieArg   string
		workspaceID string
		csvOut      string
		dbPath      string
	)
	flag.StringVar(&cookieArg, "cookie", "", "opencode.ai 会话 cookie（也可用环境变量 OPENCODE_COOKIE）")
	flag.StringVar(&workspaceID, "workspace", "wrk_01KQE6ZT476376EYCQDQ0AMC28", "workspace id")
	flag.StringVar(&dbPath, "db", "usage.db", "SQLite 数据库路径（数据保存在这里）")
	flag.StringVar(&csvOut, "csv", "", "抓取完成后额外导出 CSV 文件路径（可选）")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "用法: %s -cookie <会话cookie> [-workspace <id>] [-db <sqlite路径>] [-csv <path>]\n\n", os.Args[0])
		fmt.Fprintln(os.Stderr, "cookie 获取: 浏览器 DevTools -> Application -> Cookies -> https://opencode.ai\n"+
			"            （复制 Cookie 头的完整值，多个 cookie 用 ; 分隔）")
		flag.PrintDefaults()
	}
	flag.Parse()

	if err := envfile.Load(".env"); err != nil {
		fmt.Fprintf(os.Stderr, "读取 .env 失败: %v\n", err)
		os.Exit(2)
	}

	cookie := cookieArg
	if cookie == "" {
		cookie = os.Getenv("OPENCODE_COOKIE")
	}
	if cookie == "" && flag.NArg() > 0 {
		cookie = flag.Arg(0)
	}
	if cookie == "" {
		fmt.Fprintln(os.Stderr, "错误: 未提供 cookie（-cookie 或 OPENCODE_COOKIE）")
		os.Exit(2)
	}

	ctx := context.Background()
	st, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开数据库失败: %v\n", err)
		os.Exit(1)
	}
	defer st.Close()

	known, err := st.AllIDs(ctx, workspaceID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取已入库记录失败: %v\n", err)
		os.Exit(1)
	}
	client := api.New(cookie)
	start := time.Now()

	fmt.Fprintf(os.Stderr, "抓取 workspace %s (已有 %d 条, 增量模式)...\n", workspaceID, len(known))
	const staleStopLimit = 3 // 连续多少页全部是已入库记录则停止
	stalePages := 0
	err = client.FetchUsagePages(ctx, workspaceID, func(page int, recs []model.UsageRecord) (stop bool) {
		var fresh []model.UsageRecord
		for _, r := range recs {
			if _, dup := known[r.ID]; dup {
				continue
			}
			known[r.ID] = struct{}{}
			fresh = append(fresh, r)
		}
		if len(fresh) == 0 {
			stalePages++
			fmt.Fprintf(os.Stderr, "\rpage %d: 无新数据 (%d/%d, 再连续 %d 页停止)", page, stalePages, staleStopLimit, staleStopLimit-stalePages)
			return stalePages >= staleStopLimit
		}
		stalePages = 0
		if err := st.BulkUpsert(ctx, fresh); err != nil {
			fmt.Fprintf(os.Stderr, "\n写入数据库失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "\r进度: 第 %d 页, 新增 %d 条 (共入库 %d)", page, len(fresh), len(known))
		return false
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "\n抓取中断: %v\n", err)
		fmt.Fprintf(os.Stderr, "已保存 %d 条, 重跑本命令即可断点续传\n", countRecords(ctx, st, workspaceID))
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr)

	n := countRecords(ctx, st, workspaceID)
	fmt.Fprintf(os.Stderr, "抓取完成: 共入库 %d 条, 耗时 %s\n", n, time.Since(start).Round(time.Second))

	if csvOut != "" {
		if err := exportDBToCSV(ctx, st, workspaceID, csvOut); err != nil {
			fmt.Fprintf(os.Stderr, "导出 CSV 失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "已导出 CSV %s (%d 条)\n", csvOut, n)
	}
}

func countRecords(ctx context.Context, st *store.Store, workspaceID string) int64 {
	n, err := st.Count(ctx, workspaceID)
	if err != nil {
		return 0
	}
	return n
}

func exportDBToCSV(ctx context.Context, st *store.Store, workspaceID, path string) error {
	records, err := st.All(ctx, workspaceID)
	if err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return export.WriteUsageCSV(f, records)
}
