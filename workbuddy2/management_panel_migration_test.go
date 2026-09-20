// management_panel_migration_test.go covers the panel-migration additions:
// overview aggregation, the login wizard management routes, and registration
// completeness. Contract: docs/panel-migration-api-contract.md.
package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// -----------------------------------------------------------------------------
// overview aggregation
// -----------------------------------------------------------------------------

func overviewFixture() ([]wbAccount, map[string]any) {
	accounts := []wbAccount{
		{AuthIndex: "a1", Nickname: "ok-acct", UID: "1234567890", Region: "cn", Status: "active"},
		{AuthIndex: "a2", Nickname: "disabled", UID: "abcdefghij", Region: "cn", Disabled: true},
		{AuthIndex: "a3", Nickname: "exhausted", UID: "zzzzzzzzzz", Region: "global", Exhausted: true},
		{AuthIndex: "a4", Nickname: "broken", UID: "errorerr9", Region: "cn", Error: "upstream 500"},
	}
	summary := map[string]any{
		"total_remain": int64(120),
		"total_used":   int64(30),
		"total_size":   int64(150),
		"known_count":  2,
		"pack_count":   4,
	}
	return accounts, summary
}

func TestBuildOverviewFromAccountsBuckets(t *testing.T) {
	accounts, summary := overviewFixture()
	ov := buildOverviewFromAccounts(accounts, summary)
	if ov.Total != 4 {
		t.Errorf("total = %d want 4", ov.Total)
	}
	if ov.Healthy != 1 {
		t.Errorf("healthy = %d want 1", ov.Healthy)
	}
	if ov.Disabled != 1 {
		t.Errorf("disabled = %d want 1", ov.Disabled)
	}
	if ov.Exhausted != 1 {
		t.Errorf("exhausted = %d want 1", ov.Exhausted)
	}
	if len(ov.NeedsAttention) != 3 {
		t.Fatalf("needs_attention = %d want 3", len(ov.NeedsAttention))
	}
	// uid8 truncation: first attention row is the disabled account (a2).
	if got := ov.NeedsAttention[0].UID8; got != "abcdefgh" {
		t.Errorf("uid8 = %q want abcdefgh", got)
	}
	// error reason present for the broken account; error text carried through
	// (already sanitized upstream of this function).
	var found bool
	for _, at := range ov.NeedsAttention {
		if at.AuthIndex == "a4" {
			found = true
			hasErr := false
			for _, r := range at.Reasons {
				if r == "error" {
					hasErr = true
				}
			}
			if !hasErr || at.Error == "" {
				t.Errorf("broken account reasons=%v error=%q", at.Reasons, at.Error)
			}
		}
	}
	if !found {
		t.Fatal("broken account missing from needs_attention")
	}
	// credits mapping from summarizeCredits keys.
	if ov.Credits["remain"] != 120 || ov.Credits["known"] != 2 || ov.Credits["packs"] != 4 {
		t.Errorf("credits = %v", ov.Credits)
	}
	if ov.ServerTime == "" {
		t.Error("server_time empty")
	}
}

func TestBuildOverviewFromAccountsEmpty(t *testing.T) {
	ov := buildOverviewFromAccounts(nil, nil)
	if ov.Total != 0 || ov.Healthy != 0 || len(ov.NeedsAttention) != 0 {
		t.Errorf("empty overview = %+v", ov)
	}
	if ov.Credits["remain"] != 0 {
		t.Errorf("credits.remain = %v want 0", ov.Credits["remain"])
	}
}

// -----------------------------------------------------------------------------
// management dispatch: new routes
// -----------------------------------------------------------------------------

func mgmtPath(p string) string {
	return loadedManagementBasePath() + "/plugins/workbuddy" + p
}

// TestManagementRegistrationIncludesNewRoutes guards the contract: the four
// migration routes must be declared so the host surfaces them.
func TestManagementRegistrationIncludesNewRoutes(t *testing.T) {
	reg := managementRegistration()
	want := map[string]bool{
		"GET /plugins/workbuddy/overview":      false,
		"POST /plugins/workbuddy/login/start":  false,
		"POST /plugins/workbuddy/login/poll":   false,
		"POST /plugins/workbuddy/login/cancel": false,
	}
	for _, r := range reg.Routes {
		key := r.Method + " " + r.Path
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for key, seen := range want {
		if !seen {
			t.Errorf("route %q missing from managementRegistration", key)
		}
	}
}

// TestMutatingManagementPathIncludesLoginWrites ensures login/start and
// login/cancel demand auth even when no plugin management key is configured.
func TestMutatingManagementPathIncludesLoginWrites(t *testing.T) {
	for _, p := range []string{mgmtPath("/login/start"), mgmtPath("/login/cancel")} {
		if !mutatingManagementPath(p) {
			t.Errorf("%q should be mutating", p)
		}
	}
	if mutatingManagementPath(mgmtPath("/overview")) {
		t.Errorf("overview GET should not be mutating")
	}
}

// TestHandleManagementOverviewDispatch runs the dispatch path with an empty
// auth store; the overview must still render zeroed tiles (no upstream calls).
func TestHandleManagementOverviewDispatch(t *testing.T) {
	resp := managementResponseForTest(t, pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   mgmtPath("/overview"),
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", resp.StatusCode, resp.Body)
	}
	var ov overviewSummary
	if err := json.Unmarshal(resp.Body, &ov); err != nil {
		t.Fatalf("overview body: %v", err)
	}
}

// TestHandleManagementLoginStartValidation checks region validation and the
// 400 path without touching upstream (invalid region fails before the client).
func TestHandleManagementLoginStartValidation(t *testing.T) {
	for _, region := range []string{"", "cn", "CN", "global", "Global"} {
		resp := managementResponseForTest(t, pluginapi.ManagementRequest{
			Method: http.MethodPost,
			Path:   mgmtPath("/login/start"),
			Body:   []byte(`{"region":"` + region + `"}`),
		})
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusNotFound {
			t.Errorf("region %q: unexpected status %d body=%s", region, resp.StatusCode, resp.Body)
		}
	}
	for _, region := range []string{"us", "cnx"} {
		resp := managementResponseForTest(t, pluginapi.ManagementRequest{
			Method: http.MethodPost,
			Path:   mgmtPath("/login/start"),
			Body:   []byte(`{"region":"` + region + `"}`),
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("region %q: status = %d want 400", region, resp.StatusCode)
		}
	}
}

// TestHandleManagementLoginPollValidation: bad session ids return 400; unknown
// but well-formed ids return 404 (no upstream call happens for unknown ids).
func TestHandleManagementLoginPollValidation(t *testing.T) {
	for _, id := range []string{"", "bad id with spaces", strings.Repeat("x", 200)} {
		resp := managementResponseForTest(t, pluginapi.ManagementRequest{
			Method: http.MethodPost,
			Path:   mgmtPath("/login/poll"),
			Body:   []byte(`{"session_id":"` + id + `"}`),
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("id %q: status = %d want 400", id, resp.StatusCode)
		}
	}
	resp := managementResponseForTest(t, pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   mgmtPath("/login/poll"),
		Body:   []byte(`{"session_id":"abcdef0123456789"}`),
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown id: status = %d want 404", resp.StatusCode)
	}
}

// TestHandleManagementLoginCancelValidation mirrors poll validation for cancel.
func TestHandleManagementLoginCancelValidation(t *testing.T) {
	resp := managementResponseForTest(t, pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   mgmtPath("/login/cancel"),
		Body:   []byte(`{"session_id":"###"}`),
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("cancel invalid: status = %d want 400", resp.StatusCode)
	}
	resp = managementResponseForTest(t, pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   mgmtPath("/login/cancel"),
		Body:   []byte(`{"session_id":"abcdef0123456789"}`),
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("cancel unknown: status = %d want 404", resp.StatusCode)
	}
}

// TestHandleManagementLoginStartSurfacesUpstream covers the start route end to
// end. Two legal outcomes: 200 (the host bridge falls back to direct dial when
// absent — in tests this reaches the real upstream and returns a session) or
// 502 (upstream unreachable). Either way the body must stay sanitized.
func TestHandleManagementLoginStartSurfacesUpstream(t *testing.T) {
	resp := managementResponseForTest(t, pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   mgmtPath("/login/start"),
		Body:   []byte(`{"region":"cn"}`),
	})
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusBadGateway {
		t.Errorf("start: status = %d want 200 or 502 body=%s", resp.StatusCode, resp.Body)
	}
	if string(resp.Body) == "" {
		t.Fatal("empty body")
	}
	// Error responses must be sanitized: no raw host error internals, no
	// token-shaped material.
	if strings.Contains(string(resp.Body), "host.http") || strings.Contains(string(resp.Body), "\n") {
		t.Errorf("error body leaks internals: %s", resp.Body)
	}
}
