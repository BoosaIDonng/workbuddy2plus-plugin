// management_auth_test.go covers the management dispatch contract.
//
// Authorization is owned by the CPA host, not the plugin: the host's
// remote-management middleware requires the management key on every
// /v0/management/* request and bans an IP after repeated failures. The plugin
// deliberately enforces nothing of its own, because a second key would reject
// the host's own key — the only credential the panel can obtain automatically.
// These tests pin that: the plugin must dispatch regardless of the
// Authorization header, and must never mint its own 401/403/429.
package main

import (
	"encoding/json"
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

// TestManagementDoesNotEnforcePluginLayerAuth: the plugin must serve a route
// whether or not an Authorization header is present. A plugin-minted 401 would
// break the panel, which only ever holds the host's key.
func TestManagementDoesNotEnforcePluginLayerAuth(t *testing.T) {
	base := loadedManagementBasePath() + "/plugins/" + providerName
	for _, tc := range []struct {
		name    string
		headers http.Header
	}{
		{name: "no header", headers: nil},
		{name: "empty bearer", headers: http.Header{"Authorization": []string{"Bearer "}}},
		{name: "unrelated key", headers: http.Header{"Authorization": []string{"Bearer not-the-host-key"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := managementResponseForTest(t, pluginapi.ManagementRequest{
				Method:  http.MethodGet,
				Path:    base + "/keepalive/status",
				Headers: tc.headers,
			})
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d want 200 (the plugin must not gate on the key)", resp.StatusCode)
			}
		})
	}
}

// TestManagementNeverMintsAuthStatuses: no route may answer 401/403/429 from
// plugin code — those statuses belong to the host's middleware, and the panel
// treats them as "clear the key and prompt", so a plugin-minted one would
// wrongly wipe a valid host key.
func TestManagementNeverMintsAuthStatuses(t *testing.T) {
	base := loadedManagementBasePath() + "/plugins/" + providerName
	paths := []string{
		"/keepalive/status", "/accounts", "/overview", "/requests",
		"/tasks", "/models", "/ledger", "/egress-ip",
	}
	for _, p := range paths {
		resp := managementResponseForTest(t, pluginapi.ManagementRequest{
			Method: http.MethodGet,
			Path:   base + p,
		})
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
			t.Errorf("GET %s returned %d; auth statuses must come from the host, not the plugin",
				p, resp.StatusCode)
		}
	}
}

// TestPanelRemainsPublic: the panel HTML must load without credentials so the
// host can embed it and the UI can prompt if it ever has no key.
func TestPanelRemainsPublic(t *testing.T) {
	resp := managementResponseForTest(t, pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   loadedResourceBasePath() + "/panel",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("panel status=%d body=%s", resp.StatusCode, resp.Body)
	}
}
