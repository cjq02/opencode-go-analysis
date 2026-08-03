package model

// UsageRecord 对应 usage.list server function 返回的单个记录
type UsageRecord struct {
	ID              string `json:"id"`
	WorkspaceID     string `json:"workspaceID"`
	TimeCreated     int64  `json:"timeCreated"`
	Model           string `json:"model"`
	Provider        string `json:"provider"`
	SessionID       string `json:"sessionID"`
	InputTokens     int64  `json:"inputTokens"`
	OutputTokens    int64  `json:"outputTokens"`
	ReasoningTokens int64  `json:"reasoningTokens,omitempty"`
	CacheReadTokens int64  `json:"cacheReadTokens,omitempty"`
	CacheWrite5m    int64  `json:"cacheWrite5mTokens,omitempty"`
	CacheWrite1h    int64  `json:"cacheWrite1hTokens,omitempty"`
	// Cost 原始单位（1e-8 美元），用 CostUSD() 获取美元
	Cost int64 `json:"cost"`
}

// CostUSD 返回美元金额
func (u *UsageRecord) CostUSD() float64 {
	return float64(u.Cost) / 1e8
}

// TotalInputTokens 计算页面展示的输入总量（含缓存读取/写入）
func (u *UsageRecord) TotalInputTokens() int64 {
	return u.InputTokens + u.CacheReadTokens + u.CacheWrite5m + u.CacheWrite1h
}
