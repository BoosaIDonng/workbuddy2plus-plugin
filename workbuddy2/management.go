// management.go implements the WorkBuddy management API and web panel:
// account dashboard (nickname, credits, plan, check-in streak), manual/auto
// check-in (daily at 09:00 and 21:00 local time), and quota refresh.
package main

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type managementRequestWire struct {
	pluginapi.ManagementRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// billingBase hosts the Buddy-gas-station check-in and resource-package APIs.
// It is a var (not const) so tests can override it with an httptest server.
var billingBase = "https://www.codebuddy.cn"

// billingBaseGlobal is the international (www.workbuddy.ai) billing base.
var billingBaseGlobal = "https://www.workbuddy.ai"

// If the panel later wants to surface "usage export ready", re-add it and wire
// it into buildDashboardEx's response.

// -----------------------------------------------------------------------------
// Account listing via host auth callbacks
// -----------------------------------------------------------------------------

type creditsSummary struct {
	// TotalRemain is currently usable credits across all active packages.
	TotalRemain int64 `json:"total_remain"`
	// TotalUsed is consumed credits in the current cycle (sum of packages).
	TotalUsed int64 `json:"total_used"`
	// TotalSize is the credit capacity/pool (sum of package sizes). remain+used ≈ size.
	TotalSize int64 `json:"total_size"`
	// PackCount is number of resource packages included in the aggregate.
	PackCount int `json:"pack_count"`
	// FetchedAt is when this snapshot was taken (RFC3339). Upstream billing lag
	// can make remain/used look "stuck" for minutes after chat; compare this
	// timestamp — not only the numbers — when diagnosing frozen credits.
	FetchedAt string           `json:"fetched_at,omitempty"`
	Packages  []packageSummary `json:"packages"`
}

type packageSummary struct {
	Name       string `json:"name"`
	Remain     int64  `json:"remain"`
	Used       int64  `json:"used"`
	Size       int64  `json:"size"`
	CycleStart string `json:"cycle_start"`
	CycleEnd   string `json:"cycle_end"`
}

type checkinSummary struct {
	Active          bool     `json:"active"`
	TodayCheckedIn  bool     `json:"today_checked_in"`
	StreakDays      int64    `json:"streak_days"`
	DailyCredit     int64    `json:"daily_credit"`
	TodayCredit     int64    `json:"today_credit"`
	TotalCredits    int64    `json:"total_credits"`
	WeekCheckinDays int64    `json:"week_checkin_days"`
	ActivityName    string   `json:"activity_name"`
	Season          int64    `json:"season"`
	CheckinDates    []string `json:"checkin_dates,omitempty"`
}

// with a transient error (HTTP 5xx or transport error). codebuddy.cn
// intermittently returns 500s; without a retry a single hiccup surfaces as a
// panel error even though the very next request would succeed.
var billingRetryDelays = []time.Duration{300 * time.Millisecond, 900 * time.Millisecond}

// CapacityRemain/Used/Size         — lifetime package totals (Used often ≈0
//
//	for monthly-refresh free packs)
//
// CycleCapacityRemain/Used/Size    — the active billing cycle; Used is
//
//	sometimes omitted entirely
type resourcePackage struct {
	PackageName         string `json:"PackageName"`
	CapacityRemain      int64  `json:"CapacityRemain"`
	CapacityUsed        int64  `json:"CapacityUsed"`
	CapacitySize        int64  `json:"CapacitySize"`
	CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
	CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
	CycleCapacitySize   int64  `json:"CycleCapacitySize"`
	CycleStartTime      string `json:"CycleStartTime"`
	CycleEndTime        string `json:"CycleEndTime"`
}

// -----------------------------------------------------------------------------
// Auto check-in scheduler (09:00 / 21:00 local)
// -----------------------------------------------------------------------------

// Management API routes + handler
// -----------------------------------------------------------------------------

type managementRoute struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
}

type resourceRoute struct {
	Path        string `json:"path"`
	Menu        string `json:"menu,omitempty"`
	Description string `json:"description,omitempty"`
}

type managementRegistrationResponse struct {
	Routes    []managementRoute `json:"routes,omitempty"`
	Resources []resourceRoute   `json:"resources,omitempty"`
}

// managementBasePathCache holds the host-injected BasePath so handleManagement
// doesn't hardcode /v0/management. Falls back to the historical default if the
// host doesn't provide one (older CPA builds).
var (
	managementBasePathCache   = "/v0/management"
	managementBasePathCacheMu sync.RWMutex
	resourceBasePathCache     = "/v0/resource/plugins/" + providerName
	resourceBasePathCacheMu   sync.RWMutex
)

func loadedManagementBasePath() string {
	managementBasePathCacheMu.RLock()
	defer managementBasePathCacheMu.RUnlock()
	return managementBasePathCache
}

func setManagementBasePath(p string) {
	p = strings.TrimRight(strings.TrimSpace(p), "/")
	if p == "" {
		return
	}
	managementBasePathCacheMu.Lock()
	managementBasePathCache = p
	managementBasePathCacheMu.Unlock()
}

func loadedResourceBasePath() string {
	resourceBasePathCacheMu.RLock()
	defer resourceBasePathCacheMu.RUnlock()
	return resourceBasePathCache
}

func setResourceBasePath(p string) {
	p = strings.TrimRight(strings.TrimSpace(p), "/")
	if p == "" {
		return
	}
	resourceBasePathCacheMu.Lock()
	resourceBasePathCache = p
	resourceBasePathCacheMu.Unlock()
}

func managementRegistration() managementRegistrationResponse {
	base := "/plugins/" + providerName
	return managementRegistrationResponse{
		Routes: []managementRoute{
			{Method: http.MethodGet, Path: base + "/desensitize", Description: "Get effective WorkBuddy desensitize runtime settings."},
			{Method: http.MethodGet, Path: base + "/accounts", Description: "List WorkBuddy accounts with credits, plan and check-in status."},
			{Method: http.MethodGet, Path: base + "/egress-ip", Description: "Get the current egress IP through the active WorkBuddy HTTP route."},
			{Method: http.MethodPost, Path: base + "/refresh", Description: "Force refresh quota/cache for all accounts."},
			{Method: http.MethodPost, Path: base + "/checkin", Description: "Manually check in one account (auth_index) or all."},
			{Method: http.MethodPost, Path: base + "/checkin/config", Description: "Toggle auto check-in (enabled: true/false)."},
			{Method: http.MethodGet, Path: base + "/credits", Description: "Get real-time credits for one (auth_index query) or all accounts."},
			{Method: http.MethodPost, Path: base + "/import", Description: "Import WorkBuddy credential JSON (nested or flat) into host auth store."},
			{Method: http.MethodPost, Path: base + "/trial", Description: "Claim expert trial pack for one Global account (auth_index). One-time 250 credits / 14 days."},
			{Method: http.MethodPost, Path: base + "/select", Description: "Select the active account card used for chat routing (body: {auth_index})."},
			{Method: http.MethodPost, Path: base + "/keepalive", Description: "Manually refresh access tokens for all accounts (or one with auth_index)."},
			{Method: http.MethodGet, Path: base + "/keepalive/status", Description: "Last keepalive run summary + config."},
			{Method: http.MethodGet, Path: base + "/overview", Description: "Aggregated panel overview: account health tiles, credits totals, accounts needing attention."},
			{Method: http.MethodGet, Path: base + "/usage", Description: "Credit spend by model/day/client from official billing rows (query: days=1..31, default 7)."},
			{Method: http.MethodGet, Path: base + "/requests", Description: "Request log from panel-recorded executors: model, status, TTFB, latency, tokens (query: limit=1..500)."},
			{Method: http.MethodGet, Path: base + "/ledger", Description: "Credit acquisition ledger: balance increases from check-in, activity and travel."},
			{Method: http.MethodGet, Path: base + "/tasks", Description: "Automated task history: check-in, keepalive and manual account state changes."},
			{Method: http.MethodGet, Path: base + "/models", Description: "Full model catalog per account: display name, context window, reasoning tiers, credit multiplier, tags."},
			{Method: http.MethodPost, Path: base + "/account/toggle", Description: "Temporarily disable or re-enable one account without deleting credentials (body: {auth_index, disabled})."},
			{Method: http.MethodPost, Path: base + "/activity", Description: "Run the activity-map task now: report chat activity and claim growth rewards."},
			{Method: http.MethodPost, Path: base + "/travel", Description: "Run the cat-travel task now: adopt, depart or claim for every CN account."},
		},
		Resources: []resourceRoute{
			{Path: "/panel", Menu: "WorkBuddy", Description: "WorkBuddy dashboard: credits, check-in, plan, import."},
		},
	}
}

func handleManagement(raw []byte) ([]byte, error) {
	var req managementRequestWire
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	path := strings.TrimRight(req.Path, "/")

	// Browser UI resource routes (unauthenticated).
	resPrefix := loadedResourceBasePath()
	if req.Method == http.MethodGet && strings.HasPrefix(path, resPrefix) {
		sub := strings.TrimPrefix(path, resPrefix)
		return okEnvelope(mgmtHTMLResponse(servePanel(sub)))
	}

	// Authorization is owned by the CPA host: its remote-management middleware
	// requires the management key on every /v0/management/* request and bans an
	// IP after repeated failures. The plugin deliberately enforces nothing of its
	// own — a second key here would reject the host's own key, which is the only
	// credential the panel can obtain automatically.
	// The panel still forwards the host key as a Bearer token; see panel.html.

	base := loadedManagementBasePath() + "/plugins/" + providerName
	switch {
	case req.Method == http.MethodGet && path == base+"/desensitize":
		cfg := currentFeatureRuntime()
		return okEnvelope(mgmtJSONResponse(http.StatusOK, map[string]any{
			"enabled": cfg.desensitizeEnabled,
			"terms":   append([]string(nil), cfg.desensitizeTerms...),
			"source":  cfg.desensitizeSource,
		}))
	case req.Method == http.MethodGet && path == base+"/accounts":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, buildDashboardExWithCallback(false, false, req.HostCallbackID)))
	case req.Method == http.MethodGet && path == base+"/egress-ip":
		ip, err := fetchEgressIPWithCallback(req.HostCallbackID)
		if err != nil {
			return okEnvelope(mgmtJSONResponse(http.StatusBadGateway, map[string]any{
				"error": "egress IP unavailable",
			}))
		}
		return okEnvelope(mgmtJSONResponse(http.StatusOK, map[string]any{"ip": ip}))
	case req.Method == http.MethodPost && path == base+"/refresh":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, buildDashboardExWithCallback(true, true, req.HostCallbackID)))
	case req.Method == http.MethodPost && path == base+"/checkin":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleManualCheckinWithCallback(req.ManagementRequest, req.HostCallbackID)))
	case req.Method == http.MethodPost && path == base+"/checkin/config":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleCheckinConfig(req.ManagementRequest)))
	case req.Method == http.MethodGet && path == base+"/credits":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleCreditsQueryWithCallback(req.ManagementRequest, req.HostCallbackID)))
	case req.Method == http.MethodPost && path == base+"/import":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleImportAuth(req.ManagementRequest)))
	case req.Method == http.MethodPost && path == base+"/trial":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleClaimTrialWithCallback(req.ManagementRequest, req.HostCallbackID)))
	case req.Method == http.MethodPost && path == base+"/select":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleSelectAuth(req.ManagementRequest)))
	case req.Method == http.MethodPost && path == base+"/keepalive":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleKeepaliveNowWithCallback(req.ManagementRequest, req.HostCallbackID)))
	case req.Method == http.MethodGet && path == base+"/keepalive/status":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleKeepaliveStatus()))
	case req.Method == http.MethodGet && path == base+"/overview":
		dash := buildDashboardExWithCallback(false, false, req.HostCallbackID)
		accounts, _ := dash["accounts"].([]wbAccount)
		summary, _ := dash["summary"].(map[string]any)
		return okEnvelope(mgmtJSONResponse(http.StatusOK, buildOverviewFromAccounts(accounts, summary)))
	case req.Method == http.MethodGet && path == base+"/usage":
		status, payload := handleUsageQuery(req.ManagementRequest)
		return okEnvelope(mgmtJSONResponse(status, payload))
	case req.Method == http.MethodGet && path == base+"/requests":
		status, payload := handleRequestsQuery(req.ManagementRequest)
		return okEnvelope(mgmtJSONResponse(status, payload))
	case req.Method == http.MethodGet && path == base+"/ledger":
		status, payload := handleLedgerQuery(req.ManagementRequest)
		return okEnvelope(mgmtJSONResponse(status, payload))
	case req.Method == http.MethodGet && path == base+"/tasks":
		status, payload := handleTasksQuery(req.ManagementRequest)
		return okEnvelope(mgmtJSONResponse(status, payload))
	case req.Method == http.MethodGet && path == base+"/models":
		status, payload := handleModelCenter(req.ManagementRequest)
		return okEnvelope(mgmtJSONResponse(status, payload))
	case req.Method == http.MethodPost && path == base+"/account/toggle":
		status, payload := handleAccountToggle(req.ManagementRequest)
		return okEnvelope(mgmtJSONResponse(status, payload))
	case req.Method == http.MethodPost && path == base+"/activity":
		// Minutes-long sweep; the panel polls /tasks. The in-flight guard makes a
		// double-click a no-op instead of a second report storm.
		if !activityEnabled() {
			return okEnvelope(mgmtJSONResponse(http.StatusConflict, map[string]any{"error": "activity task is disabled"}))
		}
		if activityRunning.Load() {
			return okEnvelope(mgmtJSONResponse(http.StatusConflict, map[string]any{"error": "activity task is already running"}))
		}
		go runActivityTask()
		return okEnvelope(mgmtJSONResponse(http.StatusAccepted, map[string]any{"started": true}))
	case req.Method == http.MethodPost && path == base+"/travel":
		if !travelEnabled() {
			return okEnvelope(mgmtJSONResponse(http.StatusConflict, map[string]any{"error": "travel task is disabled"}))
		}
		if travelRunning.Load() {
			return okEnvelope(mgmtJSONResponse(http.StatusConflict, map[string]any{"error": "travel task is already running"}))
		}
		go runTravelTask()
		return okEnvelope(mgmtJSONResponse(http.StatusAccepted, map[string]any{"started": true}))
	}
	return okEnvelope(mgmtJSONResponse(http.StatusNotFound, map[string]any{"error": "not found: " + path}))
}

func mgmtJSONResponse(status int, v any) pluginapi.ManagementResponse {
	body, _ := json.Marshal(v)
	h := http.Header{}
	h.Set("Content-Type", "application/json; charset=utf-8")
	return pluginapi.ManagementResponse{StatusCode: status, Headers: h, Body: body}
}

func mgmtHTMLResponse(body []byte) pluginapi.ManagementResponse {
	h := http.Header{}
	h.Set("Content-Type", "text/html; charset=utf-8")
	return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: h, Body: body}
}

// checkinLocks serializes per-account manual check-in (B4).
// Entries are pruned during dashboard prune to avoid unbounded growth
// when auth accounts are deleted/rotated.
var (
	checkinLocks sync.Map // auth_index -> *sync.Mutex
)
