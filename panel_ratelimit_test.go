// panel_ratelimit_test.go pins the panel's request-volume contract.
//
// The plugin used to run its own per-IP token bucket with a 5-request burst,
// which made a normal panel session hit 429 while walking the tabs. That limiter
// is gone: the host owns throttling (and bans an IP after repeated failures), so
// the plugin must answer every request a normal UI flow makes.
package main

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// panelTabRequests is the sequence a user generates: initial load, then one
// visit to each tab. Keep this in sync with panel.html's loadInitial/switchTab.
var panelTabRequests = []string{
	"/accounts",  // load(false) on startup
	"/egress-ip", // loadEgressIP()
	"/overview",  // loadOverview()
	"/usage",     // switchTab("usage")
	"/requests",  // switchTab("logs")
	"/tasks",     // switchTab("tasks")
	"/models",    // switchTab("models")
}

// TestPanelTabWalkIsNeverThrottled drives the real dispatch path and asserts a
// normal panel walk is always answered. Repeated passes must also stay clean:
// the plugin keeps no cross-request state a second pass could exhaust.
func TestPanelTabWalkIsNeverThrottled(t *testing.T) {
	base := loadedManagementBasePath() + "/plugins/" + providerName
	for pass := 0; pass < 3; pass++ {
		for _, path := range panelTabRequests {
			resp := managementResponseForTest(t, pluginapi.ManagementRequest{
				Method: http.MethodGet,
				Path:   base + path,
			})
			if resp.StatusCode == http.StatusTooManyRequests {
				t.Fatalf("pass %d: panel request %s was throttled (%d); the plugin must not rate limit",
					pass, path, resp.StatusCode)
			}
		}
	}
}

// TestManySequentialRequestsAreServed: a long session (60 requests) must not
// degrade — the regression guard for the removed burst-of-5 bucket.
func TestManySequentialRequestsAreServed(t *testing.T) {
	base := loadedManagementBasePath() + "/plugins/" + providerName
	for i := 0; i < 60; i++ {
		resp := managementResponseForTest(t, pluginapi.ManagementRequest{
			Method: http.MethodGet,
			Path:   base + "/keepalive/status",
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d got %d, want 200", i+1, resp.StatusCode)
		}
	}
}
