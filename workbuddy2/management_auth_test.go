package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func managementResponseForTest(t *testing.T, req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	t.Helper()
	raw, err := handleManagement(mustJSON(req))
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestManagementKeyProtectsReadOnlyStatusRoutes(t *testing.T) {
	managementAPIKeyMu.Lock()
	oldKey := managementAPIKey
	managementAPIKey = "secret"
	managementAPIKeyMu.Unlock()
	// This test sends unauthenticated requests, which consume rate-limit tokens
	// from the process-global bucket. Reset it so a repeated run (-count=N) does
	// not inherit the previous run's exhausted bucket and see 429 instead of 401.
	resetMgmtRateLimitForTest()
	t.Cleanup(func() {
		managementAPIKeyMu.Lock()
		managementAPIKey = oldKey
		managementAPIKeyMu.Unlock()
		resetMgmtRateLimitForTest()
	})

	base := loadedManagementBasePath() + "/plugins/" + providerName
	for _, path := range []string{base + "/accounts", base + "/credits", base + "/keepalive/status"} {
		resp := managementResponseForTest(t, pluginapi.ManagementRequest{Method: http.MethodGet, Path: path})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s status=%d want %d", path, resp.StatusCode, http.StatusUnauthorized)
		}
	}

	headers := http.Header{"Authorization": []string{"Bearer secret"}}
	resp := managementResponseForTest(t, pluginapi.ManagementRequest{
		Method:  http.MethodGet,
		Path:    base + "/keepalive/status",
		Headers: headers,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authorized status=%d body=%s", resp.StatusCode, resp.Body)
	}
}

func TestManagementPanelRemainsPublicWithKeyConfigured(t *testing.T) {
	managementAPIKeyMu.Lock()
	oldKey := managementAPIKey
	managementAPIKey = "secret"
	managementAPIKeyMu.Unlock()
	t.Cleanup(func() {
		managementAPIKeyMu.Lock()
		managementAPIKey = oldKey
		managementAPIKeyMu.Unlock()
	})

	resp := managementResponseForTest(t, pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   loadedResourceBasePath() + "/panel",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("panel status=%d body=%s", resp.StatusCode, resp.Body)
	}
}

func TestManagementRateLimitDoesNotChargeValidKey(t *testing.T) {
	managementAPIKeyMu.Lock()
	oldKey := managementAPIKey
	managementAPIKey = "secret"
	managementAPIKeyMu.Unlock()
	mgmtRateLimitMu.Lock()
	oldBuckets := mgmtRateLimit
	mgmtRateLimit = map[string]*mgmtRateEntry{}
	mgmtRateLimitMu.Unlock()
	t.Cleanup(func() {
		managementAPIKeyMu.Lock()
		managementAPIKey = oldKey
		managementAPIKeyMu.Unlock()
		mgmtRateLimitMu.Lock()
		mgmtRateLimit = oldBuckets
		mgmtRateLimitMu.Unlock()
	})
	base := loadedManagementBasePath() + "/plugins/" + providerName + "/not-found"
	for i := 0; i < 6; i++ {
		resp := managementResponseForTest(t, pluginapi.ManagementRequest{
			Method: http.MethodPost,
			Path:   base,
			Headers: http.Header{
				"Authorization": []string{"Bearer secret"},
				"X-Real-Ip":     []string{"192.0.2.10"},
			},
		})
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("request %d status=%d, want 404", i+1, resp.StatusCode)
		}
	}
}

func TestManagementRateLimitChargesFailedKey(t *testing.T) {
	managementAPIKeyMu.Lock()
	oldKey := managementAPIKey
	managementAPIKey = "secret"
	managementAPIKeyMu.Unlock()
	mgmtRateLimitMu.Lock()
	oldBuckets := mgmtRateLimit
	mgmtRateLimit = map[string]*mgmtRateEntry{}
	mgmtRateLimitMu.Unlock()
	t.Cleanup(func() {
		managementAPIKeyMu.Lock()
		managementAPIKey = oldKey
		managementAPIKeyMu.Unlock()
		mgmtRateLimitMu.Lock()
		mgmtRateLimit = oldBuckets
		mgmtRateLimitMu.Unlock()
	})
	base := loadedManagementBasePath() + "/plugins/" + providerName + "/not-found"
	for i := 0; i < 6; i++ {
		resp := managementResponseForTest(t, pluginapi.ManagementRequest{
			Method: http.MethodPost,
			Path:   base,
			Headers: http.Header{
				"Authorization": []string{"Bearer wrong"},
				"X-Real-Ip":     []string{"192.0.2.10"},
			},
		})
		want := http.StatusForbidden
		if i == 5 {
			want = http.StatusTooManyRequests
		}
		if resp.StatusCode != want {
			t.Fatalf("request %d status=%d, want %d", i+1, resp.StatusCode, want)
		}
	}
}

// TestRateLimitCannotBeBypassedByRotatingForwardedFor locks down the bypass the
// global bucket exists to close: the per-IP key comes from a client-supplied
// header, so an attacker rotating X-Forwarded-For must still be throttled.
func TestRateLimitCannotBeBypassedByRotatingForwardedFor(t *testing.T) {
	managementAPIKeyMu.Lock()
	oldKey := managementAPIKey
	managementAPIKey = "secret"
	managementAPIKeyMu.Unlock()
	resetMgmtRateLimitForTest()
	t.Cleanup(func() {
		managementAPIKeyMu.Lock()
		managementAPIKey = oldKey
		managementAPIKeyMu.Unlock()
		resetMgmtRateLimitForTest()
	})

	base := loadedManagementBasePath() + "/plugins/" + providerName + "/not-found"
	throttled := false
	for i := 0; i < mgmtRateLimitCapacity*3; i++ {
		resp := managementResponseForTest(t, pluginapi.ManagementRequest{
			Method: http.MethodPost,
			Path:   base,
			Headers: http.Header{
				"Authorization": []string{"Bearer wrong"},
				// A distinct spoofed client IP on every attempt.
				"X-Forwarded-For": []string{fmt.Sprintf("203.0.113.%d", i)},
			},
		})
		if resp.StatusCode == http.StatusTooManyRequests {
			throttled = true
			break
		}
	}
	if !throttled {
		t.Fatalf("rotating X-Forwarded-For bypassed the rate limit entirely; %d attempts all got a fresh burst",
			mgmtRateLimitCapacity*3)
	}
}
