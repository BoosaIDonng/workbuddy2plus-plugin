// panel_observability_test.go locks the observability UI added in 2.4.x:
// the new tabs, the trend chart, the credit-expiry countdown and the account
// shelf/restore button. These are DOM/JS contracts — if a refactor removes a
// hook the panel silently loses a feature, so assert on the markup directly.
package main

import (
	"strings"
	"testing"
)

func TestPanelObservabilityContracts(t *testing.T) {
	html := strings.ReplaceAll(string(panelHTML), "\r\n", "\n")
	required := []struct {
		name string
		want string
	}{
		// Tab navigation for every new view.
		{name: "usage tab", want: `data-tab="usage"`},
		{name: "logs tab", want: `data-tab="logs"`},
		{name: "tasks tab", want: `data-tab="tasks"`},
		{name: "models tab", want: `data-tab="models"`},
		{name: "logs view", want: `id="tab-logs"`},
		{name: "tasks view", want: `id="tab-tasks"`},
		{name: "models view", want: `id="tab-models"`},

		// Loaders wired to the management endpoints.
		{name: "requests loader", want: `api("/requests?limit="`},
		{name: "ledger loader", want: `api("/ledger?limit=100")`},
		{name: "tasks loader", want: `api("/tasks?limit="`},
		{name: "models loader", want: `api("/models")`},
		{name: "account toggle call", want: `api("/account/toggle"`},

		// Trend chart (dependency-free SVG, mirroring the GUI's TrendChart).
		{name: "trend renderer", want: `function renderTrend()`},
		{name: "trend svg root", want: `class="trend-svg"`},
		{name: "trend metric select", want: `id="trendMetric"`},
		{name: "nice max axis", want: `function niceMax(v)`},

		// Custom usage range (B3).
		{name: "range mode select", want: `id="usageRangeMode"`},
		{name: "custom from date", want: `id="usageFrom"`},
		{name: "custom to date", want: `id="usageTo"`},

		// Credit expiry countdown (A1).
		{name: "expiry renderer", want: `function expiryHTML(cr)`},
		{name: "expiry uses cycle_end", want: `p.cycle_end`},
		{name: "expiry wired into card", want: `${expiryHTML(cr)}`},

		// Account shelf/restore (A6).
		{name: "toggle button", want: `data-action="toggle"`},
		{name: "toggle handler", want: `async function toggleAccount(authIndex, nickname, disabled, btn)`},

		// Tabs must actually load on switch.
		{name: "logs switch", want: `if(name==="logs") loadRequestLog();`},
		{name: "tasks switch", want: `if(name==="tasks") loadTaskLog();`},
		{name: "models switch", want: `if(name==="models") loadModelCenter();`},
	}
	for _, req := range required {
		if !strings.Contains(html, req.want) {
			t.Errorf("panel missing %s contract: %q", req.name, req.want)
		}
	}
}

// TestPanelObservabilityNoSecretLeak guards the new tables against echoing
// credential material. The import dialog legitimately documents the
// accessToken FIELD NAME in its help text, so only data-echoing patterns are
// banned here (a rendered value, not a schema hint).
func TestPanelObservabilityNoSecretLeak(t *testing.T) {
	html := strings.ReplaceAll(string(panelHTML), "\r\n", "\n")
	for _, banned := range []string{
		"r.accessToken", "r.refreshToken", "a.accessToken", "a.refreshToken",
		"storage_json", "StorageJSON", "cfg.api_key", "api_key:",
	} {
		if strings.Contains(html, banned) {
			t.Errorf("panel references credential value %q", banned)
		}
	}
}
