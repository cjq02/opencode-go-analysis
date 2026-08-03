// Package api 封装 opencode.ai 的 server function 调用
package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

const (
	DefaultBaseURL = "https://opencode.ai/_server"
	usageFnID      = "bfd684bfc2e4eed05cd0b518f5e4eafd3f3376e3938abb9e536e7c03df831e5c"
	PageSize       = 50
)

// Client 调用 opencode.ai server function API
type Client struct {
	BaseURL string
	Cookie  string // 浏览器会话 cookie（Cookie 头原文）
	HTTP    *http.Client

	mu       sync.Mutex // 限速锁
	minGap   time.Duration
	lastCall time.Time
}

// New 创建 API client
func New(cookie string) *Client {
	return &Client{
		BaseURL: DefaultBaseURL,
		Cookie:  cookie,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
		minGap: 2 * time.Second, // 全局限速 ~0.5 req/s，服务端限流敏感（60s 窗口约 30 请求）
	}
}

// pace 全局限速
func (c *Client) pace() {
	if c.minGap <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		now := time.Now()
		if now.Sub(c.lastCall) >= c.minGap {
			c.lastCall = now
			return
		}
		time.Sleep(c.lastCall.Add(c.minGap).Sub(now))
	}
}

// serovalNode 序列化单个参数为 seroval JSON 节点
// 参考客户端格式: 数字 {t:0,s:v} 字符串 {t:1,s:v} null {t:4} 数组 {t:9,i:0,l:n,a:[...],o:0}
func serovalNode(v any) map[string]any {
	switch x := v.(type) {
	case string:
		return map[string]any{"t": 1, "s": x}
	case bool:
		return map[string]any{"t": 3, "s": x}
	case nil:
		return map[string]any{"t": 4}
	case float64:
		return map[string]any{"t": 0, "s": x}
	case int:
		return map[string]any{"t": 0, "s": x}
	case []any:
		items := make([]map[string]any, len(x))
		for i, el := range x {
			items[i] = serovalNode(el)
		}
		return map[string]any{"t": 9, "i": 0, "l": len(x), "a": items, "o": 0}
	default:
		panic(fmt.Sprintf("unsupported arg type %T", v))
	}
}

// serovalBody 生成与浏览器一致的 seroval JSON 请求体
func serovalBody(args ...any) ([]byte, error) {
	argNodes := make([]map[string]any, len(args))
	for i, a := range args {
		argNodes[i] = serovalNode(a)
	}
	root := map[string]any{
		"t": map[string]any{
			"t": 9, "i": 0, "l": len(args), "a": argNodes, "o": 0,
		},
		"f": 31, // 序列化 flags（与浏览器请求一致）
		"m": []any{},
	}
	return json.Marshal(root)
}

// call 调用指定 server function
func (c *Client) call(fnID string, args ...any) ([]byte, error) {
	body, err := serovalBody(args...)
	if err != nil {
		return nil, fmt.Errorf("marshal args: %w", err)
	}
	c.pace()
	req, err := http.NewRequest(http.MethodPost, c.BaseURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Server-Id", fnID)
	req.Header.Set("X-Server-Instance", "server-fn:1")
	req.Header.Set("Cookie", c.Cookie)
	req.Header.Set("Origin", "https://opencode.ai")
	req.Header.Set("Referer", "https://opencode.ai/workspace/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36")
	if os.Getenv("USAGE_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[debug] POST %s body=%s\n", c.BaseURL, body)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", fnID, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server function %s: HTTP %d: %s", fnID, resp.StatusCode, truncate(data, 200))
	}
	return data, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
