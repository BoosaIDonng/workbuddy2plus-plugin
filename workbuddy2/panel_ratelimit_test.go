// panel_ratelimit_test.go is the regression check for the management rate
// limiter: a panel session must be able to issue the requests a normal UI flow
// makes (initial load + walking the tabs) without hitting 429.
//
// The bug it locks down: with the host not forwarding a client IP every
// request shares one bucket, so the old 5-burst limit was exhausted by simply
// opening the panel and clicking through the tabs.
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

// TestPanelTabWalkDoesNotRateLimit drives the real dispatch path with the
// management key configured and asserts every request in a normal panel walk
// is answered (no 429).
func TestPanelTabWalkDoesNotRateLimit(t *testing.T) {
	managementAPIKeyMu.Lock()
	oldKey := managementAPIKey
	managementAPIKey = "test-key"
	managementAPIKeyMu.Unlock()
	t.Cleanup(func() {
		managementAPIKeyMu.Lock()
		managementAPIKey = oldKey
		managementAPIKeyMu.Unlock()
		resetMgmtRateLimitForTest()
	})
	resetMgmtRateLimitForTest()

	for _, path := range panelTabRequests {
		req := pluginapi.ManagementRequest{
			Method:  http.MethodGet,
			Path:    loadedManagementBasePath() + "/plugins/" + providerName + path,
			Headers: http.Header{"Authorization": []string{"Bearer test-key"}},
		}
		resp := managementResponseForTest(t, req)
		if resp.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("panel request %s hit the rate limit (%d); a normal tab walk must not 429",
				path, resp.StatusCode)
		}
	}
}

// resetMgmtRateLimitForTest clears the per-IP token buckets so a test starts
// from a full burst. The bucket map is process-global (a real request arrives
// with no client IP and shares one bucket), so tests must reset it explicitly.
func resetMgmtRateLimitForTest() {
	mgmtRateLimitMu.Lock()
	defer mgmtRateLimitMu.Unlock()
	mgmtRateLimit = map[string]*mgmtRateEntry{}
}

// TestAuthFailureRateLimitIsScopedToFailures documents the intended semantics:
// successful requests do not consume tokens, so a valid key never rate-limits
// itself — only failed authentications do (brute-force protection).
func TestAuthFailureRateLimitIsScopedToFailures(t *testing.T) {
	managementAPIKeyMu.Lock()
	oldKey := managementAPIKey
	managementAPIKey = "good-key"
	managementAPIKeyMu.Unlock()
	t.Cleanup(func() {
		managementAPIKeyMu.Lock()
		managementAPIKey = oldKey
		managementAPIKeyMu.Unlock()
		resetMgmtRateLimitForTest()
	})
	resetMgmtRateLimitForTest()

	base := loadedManagementBasePath() + "/plugins/" + providerName
	// Many successful calls in a row must all pass.
	for i := 0; i < 20; i++ {
		resp := managementResponseForTest(t, pluginapi.ManagementRequest{
			Method:  http.MethodGet,
			Path:    base + "/keepalive/status",
			Headers: http.Header{"Authorization": []string{"Bearer good-key"}},
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("call %d with a valid key got %d, want 200", i, resp.StatusCode)
		}
	}
}
