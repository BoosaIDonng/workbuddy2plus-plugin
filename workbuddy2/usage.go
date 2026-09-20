// usage.go keeps only the local usage-detail extraction used by the executor
// paths (sseUsageCollector, usageDetailFromMap/Completion). The CPAMP usage
// forwarder was removed: publishUsage is now a no-op kept for call-site
// stability, and the host UsagePlugin callback is acknowledged without
// forwarding (usage stays inside CPA's own DefaultManager).
package main

import (
	"encoding/json"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// handleUsage is the UsagePlugin entry point. The forwarder is removed, so we
// simply acknowledge: the host still records usage into its own DefaultManager.
func handleUsage(raw []byte) ([]byte, error) {
	return okEnvelope(map[string]any{"forwarded": false})
}

// publishUsage records one executor attempt into the panel request log
// (model, status, TTFB, latency, tokens, account). It is called at every
// executor outcome, so the chat path needs no extra branching. The previous
// CPAMP forwarding was removed; this local record is the only consumer.
func publishUsage(requestedModel, upstreamModel, authID string, started time.Time, detail usage.Detail, failed bool, statusCode int, errBody string) {
	recordRequest(authID, requestedModel, upstreamModel, started, detail, failed, statusCode, errBody)
}

// hasAssistantText reports whether an SSE frame carries assistant-visible
// text (content or reasoning_content). Role-only and usage-only frames return
// false so TTFB reflects the first real token.
func hasAssistantText(rawJSON string) bool {
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content   string `json:"content"`
				Reasoning string `json:"reasoning_content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal([]byte(rawJSON), &chunk) != nil {
		return false
	}
	for _, c := range chunk.Choices {
		if c.Delta.Content != "" || c.Delta.Reasoning != "" {
			return true
		}
	}
	return false
}

// intFromAny extracts a token count from the loosely-typed usage JSON.
func intFromAny(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case json.Number:
		n, _ := x.Int64()
		return int(n)
	}
	return 0
}

// promptDetailMap safely reads prompt_tokens_details.{cached_tokens,...}.
func promptDetailMap(m map[string]any, key string) int {
	details, _ := m["prompt_tokens_details"].(map[string]any)
	if details == nil {
		return 0
	}
	return intFromAny(details[key])
}

// usageDetailFromMap converts an OpenAI-style "usage" JSON object into a
// usage.Detail for the executor's local accounting.
func usageDetailFromMap(m map[string]any) usage.Detail {
	if m == nil {
		return usage.Detail{}
	}
	return usage.Detail{
		InputTokens:     int64(intFromAny(m["prompt_tokens"])),
		OutputTokens:    int64(intFromAny(m["completion_tokens"])),
		CachedTokens:    int64(promptDetailMap(m, "cached_tokens")),
		CacheReadTokens: int64(promptDetailMap(m, "cached_tokens")),
		TotalTokens:     int64(intFromAny(m["total_tokens"])),
	}
}

// usageDetailFromCompletion extracts the usage block from an aggregated
// completion payload (sync chat path).
func usageDetailFromCompletion(payload []byte) usage.Detail {
	var m struct {
		Usage map[string]any `json:"usage"`
	}
	if json.Unmarshal(payload, &m) != nil {
		return usage.Detail{}
	}
	return usageDetailFromMap(m.Usage)
}

// sseUsageCollector observes raw upstream SSE chunks and keeps the last usage
// object seen (upstream emits usage on the terminal chunk).
type sseUsageCollector struct {
	last map[string]any
}

func (c *sseUsageCollector) feed(rawJSON string) {
	var chunk map[string]any
	if json.Unmarshal([]byte(rawJSON), &chunk) != nil {
		return
	}
	if u, ok := chunk["usage"].(map[string]any); ok && len(u) > 0 {
		c.last = u
	}
}

func (c *sseUsageCollector) detail() usage.Detail {
	return usageDetailFromMap(c.last)
}
