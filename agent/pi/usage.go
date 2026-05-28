package pi

import (
	"strings"

	"github.com/tasercake/pi-connect/core"
)

type piUsage struct {
	Input       int
	Output      int
	CacheRead   int
	CacheWrite  int
	TotalTokens int
}

func parsePiUsage(value any) (piUsage, bool) {
	usage, _ := value.(map[string]any)
	if nested, ok := usage["usage"].(map[string]any); ok {
		usage = nested
	}
	if usage == nil {
		return piUsage{}, false
	}
	u := piUsage{
		Input:       intFromAny(usage["input"]),
		Output:      intFromAny(usage["output"]),
		CacheRead:   intFromAny(usage["cacheRead"]),
		CacheWrite:  intFromAny(usage["cacheWrite"]),
		TotalTokens: intFromAny(usage["totalTokens"]),
	}
	return u, u.Input > 0 || u.Output > 0 || u.CacheRead > 0 || u.CacheWrite > 0 || u.TotalTokens > 0
}

func intFromAny(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case float32:
		return int(n)
	case jsonNumber:
		i, _ := n.Int64()
		return int(i)
	default:
		return 0
	}
}

type jsonNumber interface{ Int64() (int64, error) }

func (u piUsage) usedTokens() int {
	if u.TotalTokens > 0 {
		return u.TotalTokens
	}
	return u.Input + u.CacheRead + u.CacheWrite + u.Output
}

func (u piUsage) contextUsage() *core.ContextUsage {
	used := u.usedTokens()
	if used <= 0 {
		return nil
	}
	return &core.ContextUsage{
		UsedTokens:        used,
		TotalTokens:       u.TotalTokens,
		InputTokens:       u.Input,
		CachedInputTokens: u.CacheRead + u.CacheWrite,
		OutputTokens:      u.Output,
	}
}

func cloneContextUsage(u *core.ContextUsage) *core.ContextUsage {
	if u == nil {
		return nil
	}
	copy := *u
	return &copy
}

func isPiContextOverflowError(errMsg string) bool {
	msg := strings.ToLower(errMsg)
	return strings.Contains(msg, "context_length_exceeded") ||
		strings.Contains(msg, "exceeds the context window") ||
		strings.Contains(msg, "context length exceeded")
}

func isRecoverablePiOverflow(errMsg string) bool {
	return isPiContextOverflowError(errMsg)
}

func assistantMessageSucceeded(msg map[string]any) bool {
	if msg == nil {
		return false
	}
	if errMsg, _ := msg["errorMessage"].(string); errMsg != "" {
		return false
	}
	if stopReason, _ := msg["stopReason"].(string); stopReason != "" {
		return true
	}
	content, _ := msg["content"].([]any)
	return len(content) > 0
}
