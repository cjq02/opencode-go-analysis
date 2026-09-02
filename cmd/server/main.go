// opencode-server 入口：仅解析参数并启动 internal/server
package main

import (
	"flag"
	"log"

	"opencode-go-analysis/internal/server"
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
	flag.StringVar(&addr, "addr", ":18080", "监听地址")
	flag.StringVar(&ws, "workspace", "wrk_01KQE6ZT476376EYCQDQ0AMC28", "workspace id")
	flag.StringVar(&peakStart, "peak-start", "2026-08-17", "deepseek 峰谷起始日")
	flag.StringVar(&dailyMon, "daily-month", "", "每日表月份(默认当月 YYYY-MM)")
	flag.Parse()

	srv, err := server.New(dbPath, ws, dailyMon, peakStart)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer srv.Close()
	log.Fatal(srv.Run(addr))
}
