// management_login.go wires the existing OAuth device-flow handlers
// (handleStartLogin/handlePollLogin) into the plugin management API for the
// panel login wizard. Response bodies are reduced to non-secret fields
// (uid/nickname/file name); tokens and raw credential JSON never leave the
// plugin. See docs/panel-migration-api-contract.md §2.2–2.4.
package main

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// loginSessionIDPattern guards poll/cancel session ids (host OAuth state or
// desktop loginSessionID hex; both are hex/alnum with hyphens at most).
var loginSessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,128}$`)

// validRegion reports whether the login wizard region value is known.
func validRegion(r string) bool {
	switch strings.ToLower(strings.TrimSpace(r)) {
	case "cn", "global":
		return true
	}
	return false
}

type mgmtLoginStartRequest struct {
	Region string `json:"region"`
}

type mgmtLoginStartResponse struct {
	SessionID string `json:"session_id"`
	AuthURL   string `json:"auth_url"`
	Region    string `json:"region"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

type mgmtLoginSessionRequest struct {
	SessionID string `json:"session_id"`
}

type mgmtLoginPollResponse struct {
	Status   string `json:"status"`
	Message  string `json:"message,omitempty"`
	UID      string `json:"uid,omitempty"`
	Nickname string `json:"nickname,omitempty"`
	File     string `json:"file,omitempty"`
}

func decodeBody(body []byte, v any) bool {
	if len(body) == 0 {
		return true // empty body = zero-value request (region default etc.)
	}
	return json.Unmarshal(body, v) == nil
}

// loginRegionForRequest resolves the effective region: explicit cn/global
// passes through; empty defaults to cn (panel wizard only offers the two).
func loginRegionForRequest(region string) (string, bool) {
	r := strings.ToLower(strings.TrimSpace(region))
	if r == "" {
		return "cn", true
	}
	if !validRegion(r) {
		return "", false
	}
	return r, true
}

// startLoginEnvelope is the subset of the SDK start response the plugin needs.
// NOTE: SDK types have no json tags — encoding/json matches the exported Go
// field names case-insensitively, but the wire keys are the Go names
// (Provider/URL/State/ExpiresAt), verified by test.
type startLoginEnvelope struct {
	Result pluginapi.AuthLoginStartResponse `json:"result"`
}

// pollLoginEnvelope is the subset of the SDK poll response used here.
type pollLoginEnvelope struct {
	Result pluginapi.AuthLoginPollResponse `json:"result"`
}

// handleLoginStartManagement wraps handleStartLogin for the management route.
// The upstream device-auth flow is region-agnostic at the endpoint level, so
// region is validated and echoed for the wizard UI while the existing handler
// keeps full control of the flow.
func handleLoginStartManagement(req pluginapi.ManagementRequest) (int, any) {
	var body mgmtLoginStartRequest
	if !decodeBody(req.Body, &body) {
		return http.StatusBadRequest, map[string]any{"error": "invalid request body"}
	}
	region, ok := loginRegionForRequest(body.Region)
	if !ok {
		return http.StatusBadRequest, map[string]any{"error": "invalid region"}
	}
	raw, err := handleStartLogin(nil)
	if err != nil {
		return http.StatusBadGateway, map[string]any{"error": sanitizeUpstreamError(err)}
	}
	var env startLoginEnvelope
	if err := json.Unmarshal(raw, &env); err != nil || env.Result.State == "" {
		return http.StatusBadGateway, map[string]any{"error": "login start: unexpected response"}
	}
	return http.StatusOK, mgmtLoginStartResponse{
		SessionID: env.Result.State,
		AuthURL:   env.Result.URL,
		Region:    region,
		ExpiresAt: env.Result.ExpiresAt.UTC().Format(time.RFC3339),
	}
}

// handleLoginPollManagement wraps handlePollLogin, reducing the SDK response
// to status + safe account identity fields (uid/label/file name only).
func handleLoginPollManagement(req pluginapi.ManagementRequest) (int, any) {
	var body mgmtLoginSessionRequest
	if !decodeBody(req.Body, &body) {
		return http.StatusBadRequest, map[string]any{"error": "invalid request body"}
	}
	id := strings.TrimSpace(body.SessionID)
	if !loginSessionIDPattern.MatchString(id) {
		return http.StatusBadRequest, map[string]any{"error": "invalid session id"}
	}
	raw, err := handlePollLogin([]byte(`{"state":"` + id + `"}`))
	if err != nil {
		// Unknown/expired state → session gone; 404 with a sanitized message
		// instead of leaking internal error strings.
		return http.StatusNotFound, map[string]any{"error": "unknown or expired session"}
	}
	var env pollLoginEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return http.StatusBadGateway, map[string]any{"error": "login poll: unexpected response"}
	}
	out := mgmtLoginPollResponse{Status: string(env.Result.Status), Message: env.Result.Message}
	if env.Result.Status == pluginapi.AuthLoginStatusSuccess {
		ad := env.Result.Auth
		// AuthData.ID for a fresh login is the UID (toAuthDataOpts naming);
		// Label carries the user-facing nickname; FileName the persisted file.
		out.UID = ad.ID
		out.Nickname = ad.Label
		out.File = ad.FileName
	}
	return http.StatusOK, out
}

// handleLoginCancelManagement drops the pending session directly (the host SDK
// has no cancel RPC; loginStates is plugin-owned). The next poll returns
// unknown → the wizard shows the session as gone.
func handleLoginCancelManagement(req pluginapi.ManagementRequest) (int, any) {
	var body mgmtLoginSessionRequest
	if !decodeBody(req.Body, &body) {
		return http.StatusBadRequest, map[string]any{"error": "invalid request body"}
	}
	id := strings.TrimSpace(body.SessionID)
	if !loginSessionIDPattern.MatchString(id) {
		return http.StatusBadRequest, map[string]any{"error": "invalid session id"}
	}
	if _, ok := loginStates.Load(id); !ok {
		return http.StatusNotFound, map[string]any{"error": "unknown or expired session"}
	}
	loginStates.Delete(id)
	return http.StatusOK, mgmtLoginPollResponse{Status: "cancelled"}
}

// sanitizeUpstreamError reduces an internal error to a bounded message for
// management responses (handlers already redact; this is defense in depth).
func sanitizeUpstreamError(err error) string {
	return truncateRedacted(strings.ReplaceAll(err.Error(), "\n", " "), 200)
}
