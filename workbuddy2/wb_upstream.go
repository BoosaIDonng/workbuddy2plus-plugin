// wb_upstream.go bridges the plugin executor to the vendored workbuddy2api
// upstream pipeline: outbound body assembly, chat headers with the official
// session-header family, SSE chunk normalization, and folded completions.
//
// Division of labor: HTTP transport stays on the host bridge
// (host.http.do_stream) so outbound calls keep appearing in CPA's request log
// and honor the plugin's explicit proxy config; the vendored client only
// assembles bodies and headers and never performs the chat call itself.
package main

import (
	"encoding/json"
	"io"
	"net/http"

	wauth "github.com/sliverkiss/workbuddy2plus-plugin/internal/auth"
	wsession "github.com/sliverkiss/workbuddy2plus-plugin/internal/session"
	wupstream "github.com/sliverkiss/workbuddy2plus-plugin/internal/upstream"
)

// wbClient is the process-wide vendored upstream client used for body/header
// assembly. GlobalEnabled mirrors the gateway config default (global.enabled
// true): global-realm accounts route to www.workbuddy.ai. Fingerprint
// sanitize stays on (the old plugin's template sanitization was unconditional
// too; the vendored pipeline is its superset).
var wbClient = newWBClient()

func newWBClient() *wupstream.Client {
	c := wupstream.New()
	c.GlobalEnabled = true
	c.SanitizeFingerprints = true
	return c
}

// wbAuth converts the executor's storage JSON into the vendored Auth type by
// re-parsing the nested form (single source of truth for field mapping; realm
// backfill from the domain suffix stays inside the auth package). Falls back
// to re-marshalling the already-parsed storedAuth when the raw JSON is absent.
func wbAuth(storageJSON []byte, sa *storedAuth) *wauth.Auth {
	if len(storageJSON) > 0 {
		if a, err := wauth.Parse(storageJSON); err == nil {
			return a
		}
	}
	if sa != nil {
		if raw, err := json.Marshal(sa); err == nil {
			if a, err := wauth.Parse(raw); err == nil {
				return a
			}
		}
	}
	return &wauth.Auth{}
}

// wbPrepareBody assembles the outbound chat body. Order matters:
//  1. client-alias → upstream model rewrite first (effort downgrade and
//     thinking injection key off the model field);
//  2. the full vendored pipeline (stream forcing, max_completion_tokens
//     translation, stream_options include_usage, tool_choice/roles
//     normalization, tool pairing, thinking injection, effort downgrade,
//     reasoning backfill, fingerprint sanitize, prompt_cache_key, global
//     console system);
//  3. the plugin's opt-in U+200B desensitizer last so its rewrites are
//     guaranteed present in the outbound body.
func wbPrepareBody(payload, original []byte, sa *storedAuth, a *wauth.Auth, upstreamModel string) []byte {
	src := payload
	if len(src) == 0 {
		src = original
	}
	if len(src) == 0 || a == nil {
		return src
	}
	src = rewriteModelInBody(src, upstreamModel)
	convID := wsession.ResolveConversationID(src)
	src = wbClient.PrepareChatBodyForRealm(src, a, convID)
	return applyDesensitizeBody(src)
}

// applyDesensitizeBody applies the plugin's opt-in U+200B desensitizer to the
// final outbound body. No-op when the feature is disabled (default).
func applyDesensitizeBody(src []byte) []byte {
	var obj map[string]any
	if json.Unmarshal(src, &obj) != nil {
		return src
	}
	if !applyDesensitizeInPlace(obj, currentFeatureRuntime()) {
		return src
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// wbChatMeta derives the official session-header metadata from the outbound
// body, mirroring the gateway handler's turn-level aggregation: all upstream
// calls within one user turn (tool calls, retries) share one
// X-Conversation-Request-ID, so the backend no longer fragments a single
// conversation into dozens of request records. Without a turn key the ID is
// request-scoped (fresh per call), matching the gateway's fallback.
func wbChatMeta(body []byte) wupstream.ChatMeta {
	meta := wupstream.ChatMeta{ConversationID: wsession.ResolveConversationID(body)}
	meta.ConversationRequestID = wsession.TurnRequestID(wsession.TurnKey(body))
	return meta
}

// wbApplyChatHeaders applies the vendored chat headers (auth account headers,
// official session family, client identity, realm-aware UA/Origin) to a
// request that the host bridge will transport. clientIP is empty: the plugin
// executor does not see the downstream client IP, and the vendored client
// skips IP headers for empty values.
func wbApplyChatHeaders(req *http.Request, a *wauth.Auth, body []byte) {
	wbClient.ChatHeaders(req, a, "", wbChatMeta(body))
}

// wbAggregate folds an SSE stream into one chat.completion object via the
// vendored aggregator (truncated tool_call dropping on length/EOF, usage
// total synthesis, missing-index tool call merge), with the plugin's
// model/id fallbacks for streams that omit them.
func wbAggregate(r io.Reader, model string) ([]byte, error) {
	result, err := wupstream.Aggregate(r)
	if err != nil {
		return nil, err
	}
	if m, _ := result["model"].(string); m == "" {
		result["model"] = model
	}
	if id, _ := result["id"].(string); id == "" {
		result["id"] = "chatcmpl-workbuddy"
	}
	return json.Marshal(result)
}
