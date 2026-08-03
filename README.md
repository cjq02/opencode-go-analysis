# opencode-go-analysis

抓取 [opencode.ai](https://opencode.ai) workspace 用量历史（日期 / 模型 / 输入 / 输出 / 成本），
保存到 SQLite 并可选导出 CSV。

## 项目结构

```
.
├── cmd/
│   └── usage/                # CLI 入口
├── internal/
│   ├── api/                  # opencode.ai server function API client
│   │   ├── client.go         # seroval 请求编码、HTTP 调用、全局限速
│   │   └── usage.go          # usage.list 分页抓取、seroval 流响应解析
│   ├── export/               # CSV 导出
│   ├── model/                # 数据模型
│   ├── envfile/              # 极简 .env 解析（零第三方依赖）
│   └── store/                # SQLite 持久化（modernc.org/sqlite，纯 Go 零 CGO）
├── .env.example              # 环境变量示例（复制为 .env 使用）
├── go.mod / go.sum
└── README.md
```

## 原理

`https://opencode.ai/workspace/<id>/usage` 是 SolidStart 应用，数据通过
server function `usage.list`（POST `https://opencode.ai/_server`）获取：

- 鉴权：浏览器登录后的会话 cookie（`auth=Fe26.2**...`），从 DevTools 复制
- 请求体：seroval JSON 格式（非普通 JSON），每页 50 条
- 响应：seroval JS 流（`;0x<hex长度>;<代码>`），正则解析各字段

### 已知坑

1. **限流**：服务端对请求频率敏感（约 60 秒窗口 30 次请求），过密会返回空数组
   （与"数据末尾"难以区分）。已内置：
   - 全局限速 2s/请求（约 0.5 req/s）
   - 空页视为疑似限流，等待 60s 重试，连续 3 次仍空才判定为数据末尾
2. **成本单位**：`cost_raw` 为整数，`cost_raw / 1e8` 才是美元（与页面换算一致）

## 快速开始

```bash
# 1. 配置 cookie：复制 .env.example 为 .env 并填入 OPENCODE_COOKIE
cp .env.example .env
#    cookie 获取: 浏览器 DevTools -> Application -> Cookies -> https://opencode.ai
#    复制 Cookie 头的完整值（多个 cookie 用 ; 分隔）

# 2. 运行（数据写入 usage.db；首次全量约 30 分钟，之后为增量）
go run ./cmd/usage

# 常用参数
go run ./cmd/usage -workspace wrk_xxx -db data.db -csv usage.csv

# 构建
go build -o bin/usage ./cmd/usage
./bin/usage
```

## 增量更新（8/15 再跑会怎样）

分页是**最新在前**（page 0 = 最新记录），因此不能按"已抓页数"断点续传
（新数据插入后页号会整体偏移，从旧页号继续会漏掉新数据）。

改为按记录 id 增量：
- 每次从 page 0 开始抓，逐条对比已入库的 `usage_records.id`
- 只有"该页全部是已入库记录"时继续，**连续 3 页无新数据即停止**
- 新记录按 id 幂等 upsert，重复抓取不产生脏数据
- 每页立即入库，中断不丢已抓数据

实测：46,025 条存量时再跑一次，8 秒完成，只抓当日新增的 18 条。

## 数据

### 本次抓取结果（2026-08-01）

| 指标 | 值 |
|---|---|
| 总记录数 | 46,025（每次 LLM 调用一条） |
| 时间范围 | 2026-04-30 ~ 2026-08-01 |
| 模型数 | 22 种（deepseek-v4-flash、glm-5.2、qwen3.7-plus、minimax-m3 等） |
| 总成本 | $171.12 |

### SQLite 表 `usage_records`

| 列 | 说明 |
|---|---|
| id | 用量记录 id (`usg_...`) |
| workspace_id | workspace id |
| time_created | 时间戳（毫秒） |
| model / provider | 模型名 / 提供商 |
| session_id | 会话 id |
| input_tokens / output_tokens | 输入 / 输出 token |
| reasoning_tokens | 推理 token |
| cache_read_tokens / cache_write_5m / cache_write_1h | 缓存读取 / 5 分钟写入 / 1 小时写入 |
| cost_raw | 原始成本（1e-8 美元） |

输入总量 = `input_tokens + cache_read_tokens + cache_write_5m + cache_write_1h`（与页面一致）

### 查询示例

```sql
-- 按天汇总成本与用量
SELECT date(time_created/1000, 'unixepoch', 'localtime') AS day,
       SUM(cost_raw)/1e8 AS cost_usd,
       SUM(input_tokens) AS input,
       SUM(output_tokens) AS output
FROM usage_records
GROUP BY day ORDER BY day;

-- 按模型统计
SELECT model, COUNT(*) AS calls, SUM(cost_raw)/1e8 AS cost_usd,
       SUM(input_tokens) AS input, SUM(output_tokens) AS output
FROM usage_records
GROUP BY model ORDER BY cost_usd DESC;

-- 最贵的单次调用
SELECT * FROM usage_records ORDER BY cost_raw DESC LIMIT 10;

-- 按会话汇总
SELECT session_id, COUNT(*), SUM(cost_raw)/1e8 AS cost_usd
FROM usage_records WHERE session_id != ''
GROUP BY session_id ORDER BY cost_usd DESC LIMIT 10;
```

## CSV 导出

`-csv usage.csv` 在抓取完成后从数据库导出：

```csv
date,model,input_tokens,output_tokens,cost_usd,session_id
2026-04-30 21:23:08,kimi-k2.5,13325,182,0.0074,
2026-04-30 21:31:02,minimax-m2.5,15873,111,0.0049,
```

## 许可

仅供个人研究使用。使用前请确保你有权访问对应 workspace 的数据。
