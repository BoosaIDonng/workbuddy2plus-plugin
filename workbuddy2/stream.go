// stream.go owns the upstream SSE data plane: emitting cleaned chunks back to
// the host stream (streamEmit/close), pumping the upstream SSE in a goroutine
// (pumpUpstreamStream), collecting it synchronously (collectUpstreamStream),
// and the SSE-frame helpers that re-frame, filter, and aggregate chunks.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	wauth "github.com/sliverkiss/workbuddy2plus-plugin/internal/auth"
	wupstream "github.com/sliverkiss/workbuddy2plus-plugin/internal/upstream"
)

var streamHostCall = hostCall

type upstreamStatusError struct {
	status  int
	message string
}

func (e *upstreamStatusError) Error() string { return e.message }

// streamEmit pushes one chunk payload to the host stream. Returns an error if
// the host rejected it (e.g. the client already disconnected and the stream
// was closed), which the pump uses to stop reading a dead upstream.
func streamEmit(streamID string, payload []byte) error {
	if streamID == "" {
		return fmt.Errorf("no stream id")
	}
	body, _ := json.Marshal(map[string]any{"stream_id": streamID, "payload": payload})
	_, err := streamHostCall(pluginabi.MethodHostStreamEmit, body)
	return err
}

func streamErrorWire(streamID, message string) []byte {
	raw, _ := json.Marshal(map[string]any{"stream_id": streamID, "error": redactSecrets(message)})
	return raw
}

func streamCloseWire(streamID string) []byte {
	raw, _ := json.Marshal(map[string]any{"stream_id": streamID})
	return raw
}

func streamEmitError(streamID, message string) {
	if streamID == "" {
		return
	}
	_, _ = streamHostCall(pluginabi.MethodHostStreamEmit, streamErrorWire(streamID, message))
}

func streamClose(streamID string) {
	if streamID == "" {
		return
	}
	_, _ = streamHostCall(pluginabi.MethodHostStreamClose, streamCloseWire(streamID))
}

func streamHeaders() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	return h
}

// pumpUpstreamStream reads the upstream SSE response in the background and
// emits each cleaned chunk to the host stream. It closes the stream when done.
// An emit failure (client disconnected → host closed the stream) aborts the
// pump so we stop reading a dead upstream. cancel is invoked on every exit so
// the underlying http request context is released promptly.
//
// v0.7.0: requests now route via host.http.do_stream so request-log captures
// the outbound call and host transport policy applies. The host bridge emits
// arbitrary 32KB chunks, so we adapt to io.Reader and keep the bufio.Scanner
// SSE line framing unchanged.
func pumpUpstreamStream(httpReq *http.Request, cancel context.CancelFunc, streamID string, sseFramed bool, requestedModel, upstreamModel, authUID string, started time.Time, authID string, callbackID string) {
	// Always close the host stream exactly once on every exit path.
	closed := false
	closeOnce := func() {
		if closed {
			return
		}
		closed = true
		streamClose(streamID)
	}
	defer closeOnce()
	if cancel != nil {
		defer cancel()
	}

	stream, statusCode, _, err := hostHTTPDoStreamWithCallback(httpReq, callbackID)
	if err != nil {
		publishUsage(requestedModel, upstreamModel, authUID, started, usage.Detail{}, true, 0, err.Error())
		streamEmitError(streamID, fmt.Sprintf("http_error: %v", err))
		return
	}
	defer stream.Close()
	if statusCode >= 400 {
		// Drain the error body via the same bridge so the message is complete.
		errPayload, _ := io.ReadAll(newHostStreamReader(stream))
		publishUsage(requestedModel, upstreamModel, authUID, started, usage.Detail{}, true, statusCode, string(errPayload))
		if authUID != "" {
			go reconcileByUID(authUID, statusCode, string(errPayload))
		}
		streamEmitError(streamID, fmt.Sprintf("upstream %d: %s", statusCode, truncateRedacted(string(errPayload), 200)))
		return
	}
	collector := &sseUsageCollector{}
	normalizer := wupstream.NewChunkNormalizer()
	scanner := bufio.NewScanner(newHostStreamReader(stream))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	validEvents := 0
	firstTokenMarked := false
	for scanner.Scan() {
		content := stripDataPrefix(scanner.Text())
		if content == "" {
			continue
		}
		cleaned, valid, done := normalizer.Normalize(content)
		if done {
			break
		}
		if cleaned == "" {
			continue
		}
		collector.feed(content)
		if valid {
			validEvents++
		}
		// TTFB: the first frame carrying assistant text (content or reasoning)
		// is the client-visible first token. Role-only and usage-only frames
		// are skipped so the measurement reflects upstream generation start.
		// Keyed on authUID — the same identifier publishUsage passes as authID.
		if !firstTokenMarked && hasAssistantText(content) {
			noteFirstToken(authUID, upstreamModel, started)
			firstTokenMarked = true
		}
		if sseFramed {
			cleaned = "data: " + cleaned
		}
		if err := streamEmit(streamID, []byte(cleaned)); err != nil {
			// Client disconnected / host closed stream — abort; do not report success.
			publishUsage(requestedModel, upstreamModel, authUID, started, collector.detail(), true, 0, "stream_emit: "+err.Error())
			return
		}
	}
	// A mid-stream read failure means the client received a truncated stream:
	// surface it as an error frame and record the attempt as failed.
	if err := scanner.Err(); err != nil {
		publishUsage(requestedModel, upstreamModel, authUID, started, collector.detail(), true, 0, err.Error())
		streamEmitError(streamID, fmt.Sprintf("upstream stream read error: %v", err))
		return
	}
	if validEvents == 0 {
		publishUsage(requestedModel, upstreamModel, authUID, started, collector.detail(), true, 0, "empty upstream stream")
		streamEmitError(streamID, "empty upstream stream")
		return
	}
	publishUsage(requestedModel, upstreamModel, authUID, started, collector.detail(), false, 0, "")
	invalidateAccountCredits(authID, authUID)
}

// collectUpstreamStream is the synchronous fallback (no async stream id): drain
// the upstream, clean each chunk, return them as a slice. The collector, when
// non-nil, observes raw upstream chunks for usage extraction. statusCode is the
// upstream HTTP status (0 for transport-level failures).
func collectUpstreamStream(body []byte, sa *storedAuth, a *wauth.Auth, sseFramed bool, collector *sseUsageCollector, callbackID string) ([]pluginapi.ExecutorStreamChunk, int, error) {
	httpReq, err := http.NewRequest(http.MethodPost, wbClient.ChatEndpoint(a), bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	wbApplyChatHeaders(httpReq, a, body)
	// Compliance: route via host.http.do_stream so request-log captures the call.
	stream, statusCode, _, err := hostHTTPDoStreamWithCallback(httpReq, callbackID)
	if err != nil {
		return nil, 0, fmt.Errorf("http_error: %w", err)
	}
	defer stream.Close()
	reader := newHostStreamReader(stream)
	if statusCode >= 400 {
		errPayload, _ := io.ReadAll(reader)
		if sa != nil && sa.Account.UID != "" {
			go reconcileByUID(sa.Account.UID, statusCode, string(errPayload))
		}
		return nil, statusCode, &upstreamStatusError{
			status:  statusCode,
			message: fmt.Sprintf("upstream %d: %s", statusCode, truncateRedacted(string(errPayload), 200)),
		}
	}
	chunks, errAgg := aggregateSSEWithCollector(reader, sseFramed, collector)
	if errAgg != nil {
		return chunks, statusCode, errAgg
	}
	return chunks, statusCode, nil
}

// clientNeedsSSEFrame reports whether chunk payloads must carry their own
// "data: " SSE framing. CPA's chat-completions passthrough adds the prefix
// itself, but every cross-format response translator (claude/gemini/codex/...)
// only consumes payloads already framed as "data: " lines. The host hands the
// plugin the inbound request path in Metadata, so we frame chunks ourselves for
// any entry path other than the native OpenAI chat-completions one.
func clientNeedsSSEFrame(metadata map[string]any) bool {
	path, _ := metadata["request_path"].(string)
	switch strings.ToLower(strings.TrimSpace(path)) {
	case "/v1/chat/completions", "/v1/completions":
		return false
	default:
		return true
	}
}

// aggregateSSEWithCollector reads an upstream SSE stream and emits one chunk
// per data event, normalized through the vendored 2api per-frame pipeline
// (whitelist rebuild, error-frame passthrough, tool-call name convergence,
// id continuation). The trailing [DONE] is dropped (the host appends its own
// stream terminator). When sseFramed is true each payload is emitted as a
// "data: " line for cross-format translators; otherwise the payload is the
// raw JSON object and the host chat-completions writer adds the framing
// itself. A mid-stream read error aborts collection and is returned so the
// caller records the attempt as failed. The collector, when non-nil,
// observes raw upstream chunks for usage extraction.
func aggregateSSEWithCollector(r io.Reader, sseFramed bool, collector *sseUsageCollector) ([]pluginapi.ExecutorStreamChunk, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	normalizer := wupstream.NewChunkNormalizer()
	var chunks []pluginapi.ExecutorStreamChunk
	validEvents := 0
	for scanner.Scan() {
		content := stripDataPrefix(scanner.Text())
		if content == "" {
			continue
		}
		cleaned, valid, done := normalizer.Normalize(content)
		if done {
			break
		}
		if cleaned == "" {
			continue
		}
		if collector != nil {
			collector.feed(content)
		}
		if valid {
			validEvents++
		}
		if sseFramed {
			cleaned = "data: " + cleaned
		}
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: []byte(cleaned)})
	}
	if err := scanner.Err(); err != nil {
		return chunks, fmt.Errorf("upstream stream read error: %w", err)
	}
	if validEvents == 0 {
		return chunks, fmt.Errorf("upstream stream contained no valid data events")
	}
	return chunks, nil
}

func stripDataPrefix(s string) string {
	s = strings.TrimSpace(s)
	for strings.HasPrefix(s, "data:") {
		s = strings.TrimSpace(strings.TrimPrefix(s, "data:"))
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
