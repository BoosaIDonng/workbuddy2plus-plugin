// panel_metrics.go records per-request observability rows (request log with
// TTFB, credit ledger, task records) into panel_store, and serves the panel
// query endpoints. publishUsage — already called at every executor outcome —
// is the single hook, so the chat path gains no new branching.
package main

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// requestLogRow is one entry of the request log stream.
type requestLogRow struct {
	Time         string `json:"time"`  // RFC3339 (local)
	Model        string `json:"model"` // upstream model id
	Alias        string `json:"alias,omitempty"`
	AuthUID8     string `json:"uid8,omitempty"`
	StatusCode   int    `json:"status"`
	Failed       bool   `json:"failed"`
	Streaming    bool   `json:"streaming"`
	TTFBMs       int64  `json:"ttfb_ms"`
	LatencyMs    int64  `json:"latency_ms"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	TotalTokens  int64  `json:"total_tokens"`
	Error        string `json:"error,omitempty"`
}

// ledgerRow is one credit-balance increase (checkin / activity / travel).
type ledgerRow struct {
	Time   string `json:"time"`
	UID8   string `json:"uid8,omitempty"`
	Nick   string `json:"nickname,omitempty"`
	Delta  int64  `json:"delta"`
	Before int64  `json:"before"`
	After  int64  `json:"after"`
	Source string `json:"source"`
}

// taskRow is one automated task execution outcome.
type taskRow struct {
	Time    string `json:"time"`
	Task    string `json:"task"` // checkin / keepalive / lifecycle
	Account string `json:"account,omitempty"`
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

// ttfbTracker measures time-to-first-token per streaming request. The executor
// hot path records the first non-empty content delta; publishUsage reads it.
//
// The mark and its consumer must derive the model identically. noteFirstToken
// receives the raw upstreamModel (which can be empty) while recordRequest falls
// back to the requested alias, so both sides normalise through ttfbModelKey —
// otherwise a mark written under an empty model is looked up under the alias and
// never consumed, leaking one entry per streaming request until the size guard
// wipes the whole map (taking in-flight marks and their TTFB with it).
var ttfbTracker = struct {
	sync.Mutex
	marks map[string]time.Time // key: authID+model+startedUnixNano
}{marks: map[string]time.Time{}}

// ttfbModelKey normalises the model component of a TTFB key, mirroring the
// alias fallback in recordRequest.
func ttfbModelKey(upstreamModel, requestedModel string) string {
	m := strings.TrimSpace(upstreamModel)
	if m == "" {
		m = strings.TrimSpace(requestedModel)
	}
	return m
}

func ttfbKey(authID, model string, started time.Time) string {
	return authID + "|" + model + "|" + fmt.Sprint(started.UnixNano())
}

// noteFirstToken marks the first token of a streaming request. authID is the
// account identifier the executor passes to publishUsage (uid when known), so
// the mark and its consumption always use the same key.
func noteFirstToken(authID, upstreamModel, requestedModel string, started time.Time) {
	ttfbTracker.Lock()
	defer ttfbTracker.Unlock()
	key := ttfbKey(authID, ttfbModelKey(upstreamModel, requestedModel), started)
	if len(ttfbTracker.marks) > 4096 { // defensive: never grow unbounded
		// Drop only entries old enough that their request must have finished —
		// a full wipe would erase marks for streams still in flight.
		cutoff := time.Now().Add(-5 * time.Minute)
		for k, at := range ttfbTracker.marks {
			if at.Before(cutoff) {
				delete(ttfbTracker.marks, k)
			}
		}
	}
	ttfbTracker.marks[key] = time.Now()
}

// takeTTFB consumes the first-token mark for a request (0 when absent).
func takeTTFB(authID, model string, started time.Time) int64 {
	ttfbTracker.Lock()
	defer ttfbTracker.Unlock()
	key := ttfbKey(authID, model, started)
	mark, ok := ttfbTracker.marks[key]
	if !ok {
		return 0
	}
	delete(ttfbTracker.marks, key)
	return mark.Sub(started).Milliseconds()
}

// recordRequest appends one request-log row. Called from publishUsage, which
// already runs at every executor outcome.
func recordRequest(authID, requestedModel, upstreamModel string, started time.Time, detail usage.Detail, failed bool, statusCode int, errBody string) {
	model := ttfbModelKey(upstreamModel, requestedModel)
	alias := strings.TrimSpace(requestedModel)
	if alias == "" {
		alias = model
	}
	latency := int64(0)
	if !started.IsZero() {
		latency = time.Since(started).Milliseconds()
		if latency < 0 {
			latency = 0
		}
	}
	// TTFB is consumed on every outcome so a failed-then-retried request does
	// not leak a stale mark into the next attempt.
	ttfb := takeTTFB(authID, model, started)
	row := requestLogRow{
		Time:         time.Now().Format(time.RFC3339),
		Model:        model,
		Alias:        alias,
		AuthUID8:     uid8Of(authID),
		StatusCode:   statusCode,
		Failed:       failed,
		Streaming:    ttfb > 0,
		TTFBMs:       ttfb,
		LatencyMs:    latency,
		InputTokens:  detail.InputTokens,
		OutputTokens: detail.OutputTokens,
		TotalTokens:  detail.TotalTokens,
	}
	if failed {
		row.Error = truncateRedacted(strings.ReplaceAll(errBody, "\n", " "), 160)
	} else if row.StatusCode == 0 {
		// The executor reports 0 for a clean run (no upstream status to surface);
		// the panel shows 200 so the column reads as a real HTTP outcome.
		row.StatusCode = 200
	}
	panelData.append(streamRequests, row)
}

// recordLedger appends a credit-increase row (only increases are recorded:
// decreases are consumption, not acquisition).
func recordLedger(uid, nickname string, before, after int64, source string) {
	delta := after - before
	if delta <= 0 || delta > 1_000_000 {
		return
	}
	panelData.append(streamLedger, ledgerRow{
		Time:   time.Now().Format(time.RFC3339),
		UID8:   uid8Of(uid),
		Nick:   nickname,
		Delta:  delta,
		Before: before,
		After:  after,
		Source: source,
	})
}

// recordTask appends one task execution outcome.
func recordTask(task, account string, ok bool, message string) {
	panelData.append(streamTasks, taskRow{
		Time:    time.Now().Format(time.RFC3339),
		Task:    task,
		Account: account,
		OK:      ok,
		Message: truncateRedacted(strings.ReplaceAll(message, "\n", " "), 160),
	})
}

// -----------------------------------------------------------------------------
// Panel query endpoints
// -----------------------------------------------------------------------------

// handleRequestsQuery serves GET /requests?limit=N — the request log.
func handleRequestsQuery(req pluginapi.ManagementRequest) (int, any) {
	limit := queryInt(req.Query, "limit", 100, 1, 500)
	return http.StatusOK, map[string]any{
		"requests": panelData.list(streamRequests, limit),
		"stats":    panelData.stats(),
	}
}

// handleLedgerQuery serves GET /ledger?limit=N — credit acquisition history.
func handleLedgerQuery(req pluginapi.ManagementRequest) (int, any) {
	limit := queryInt(req.Query, "limit", 100, 1, 500)
	return http.StatusOK, map[string]any{"ledger": panelData.list(streamLedger, limit)}
}

// handleTasksQuery serves GET /tasks?limit=N — automated task history.
func handleTasksQuery(req pluginapi.ManagementRequest) (int, any) {
	limit := queryInt(req.Query, "limit", 100, 1, 500)
	return http.StatusOK, map[string]any{"tasks": panelData.list(streamTasks, limit)}
}

// queryInt reads a bounded integer query parameter.
func queryInt(q urlValues, key string, def, min, max int) int {
	v := strings.TrimSpace(q.Get(key))
	if v == "" {
		return def
	}
	n := 0
	for _, c := range v {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
		if n > max {
			return max
		}
	}
	if n < min {
		return min
	}
	return n
}
