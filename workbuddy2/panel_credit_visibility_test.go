// panel_credit_visibility_test.go locks the credit-visibility surface added in
// 2.8.0: what each task actually earned, and today's check-in state.
//
// Two defects motivated these tests, both "data existed but was never wired":
//  1. taskRow had no credit field, so the amount was only ever free text in
//     Message — and check-in, the task users care most about, recorded none at
//     all because it copied the upstream `message` (usually empty) instead of
//     the `credit`/`daily_credit` numbers sitting in the same response.
//  2. /accounts is a light load that never populates check-in, and the panel's
//     lazy top-up only merged credits — so a card always rendered "未签到"
//     even on a day the account had signed in.
package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// readSourceFile reads a Go source file for call-site assertions. Asserting on
// the call site is the only way to pin wiring that needs a live upstream
// account to exercise end to end.
func readSourceFile(t *testing.T, file string) string {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestTaskRowCarriesStructuredCredit: the amount must be a field, not text.
func TestTaskRowCarriesStructuredCredit(t *testing.T) {
	raw, err := json.Marshal(taskRow{Task: "checkin", OK: true, Credit: 150, Message: "+150 credit"})
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if got, ok := back["credit"]; !ok || got.(float64) != 150 {
		t.Fatalf("credit field = %v (present=%v), want 150", got, ok)
	}
	// Zero credits are omitted so tasks that grant none (keepalive) stay clean.
	rawZero, _ := json.Marshal(taskRow{Task: "keepalive", OK: true})
	if strings.Contains(string(rawZero), "credit") {
		t.Fatalf("zero credit should be omitted, got %s", rawZero)
	}
}

// TestCheckinCreditUsesStructuredFields: the check-in row must read the same
// fields the panel's success toast reads, so the number is never lost.
func TestCheckinCreditUsesStructuredFields(t *testing.T) {
	// Mirrors the upstream response shape flattened into the check-in result.
	out := map[string]any{"credit": float64(150), "streak_days": float64(3)}
	if got := jsonI64(out, "credit", "daily_credit"); got != 150 {
		t.Fatalf("credit = %d want 150", got)
	}
	// daily_credit is the fallback when the grant is not echoed as `credit`.
	out2 := map[string]any{"daily_credit": "120"}
	if got := jsonI64(out2, "credit", "daily_credit"); got != 120 {
		t.Fatalf("daily_credit fallback = %d want 120", got)
	}
	// Neither present (an "already signed in" response) yields 0, not a guess.
	if got := jsonI64(map[string]any{}, "credit", "daily_credit"); got != 0 {
		t.Fatalf("absent credit = %d want 0", got)
	}
}

// TestCheckinRecordsCreditInTaskLog pins the wiring: the check-in loop must call
// the credit-recording variant. Asserting on the call site is the only way to
// catch a regression here without an upstream account in the test.
func TestCheckinRecordsCreditInTaskLog(t *testing.T) {
	src := readSourceFile(t, "checkin.go")
	if !strings.Contains(src, `recordTaskCredit("checkin"`) {
		t.Fatal("check-in must record its credit via recordTaskCredit")
	}
	if strings.Contains(src, `recordTask("checkin"`) {
		t.Fatal("check-in must not use the credit-less recordTask")
	}
	if !strings.Contains(src, `jsonI64(out, "credit", "daily_credit")`) {
		t.Fatal("check-in credit must come from the structured response fields")
	}
	start := strings.Index(src, "func processAutoCheckinAccount")
	end := strings.Index(src, "\n// handleManualCheckin serves POST /checkin.")
	if start < 0 || end <= start || !strings.Contains(src[start:end], "recordCheckinTaskResult") {
		t.Fatal("scheduled check-in must record its task result")
	}
}

// TestCreditGrantingTasksRecordAmounts: every task that grants credits must
// report the amount structurally, not only in its message.
func TestCreditGrantingTasksRecordAmounts(t *testing.T) {
	for _, tc := range []struct {
		file string
		want string
	}{
		{"wb_growth.go", `recordTaskCredit("growth-redeem"`},
		{"wb_growth.go", `recordTaskCredit("growth-gift"`},
		{"wb_growth.go", `recordTaskCredit("growth-compensation"`},
		{"wb_growth.go", `recordTaskCredit("growth-lottery"`},
		{"wb_travel.go", `recordTaskCredit("travel"`},
		{"wb_travel.go", `recordTaskCredit("travel-adopt"`},
	} {
		if src := readSourceFile(t, tc.file); !strings.Contains(src, tc.want) {
			t.Errorf("%s: missing %s", tc.file, tc.want)
		}
	}
}

// TestCreditsEndpointReturnsCheckin: the panel's lazy top-up reads check-in from
// the credits response, so the field must be there.
func TestCreditsEndpointReturnsCheckin(t *testing.T) {
	src := readSourceFile(t, "credits_handler.go")
	if !strings.Contains(src, `acct["checkin"] = ci`) {
		t.Fatal("single-account credits response must include the check-in snapshot")
	}
	// Credits and check-in are fetched together; serially this branch pays two
	// round-trips for one card.
	if !strings.Contains(src, "fetchCheckinStatusWithCallback(sa, callbackID)") {
		t.Fatal("credits branch must fetch check-in")
	}
}

// TestPanelRendersCheckinAndTaskCredit: the frontend must consume both fields.
func TestPanelRendersCheckinAndTaskCredit(t *testing.T) {
	html := strings.ReplaceAll(string(panelHTML), "\r\n", "\n")
	for _, tc := range []struct {
		name string
		want string
	}{
		{"checkin merged on lazy load", `if(acct&&acct.checkin) a.checkin=acct.checkin;`},
		{"checkin merged on manual refresh", `if(acct.checkin) a.checkin=acct.checkin;`},
		{"checkin card row", `function checkinHTML(a)`},
		{"checkin row rendered", `${checkinHTML(a)}`},
		{"streak shown", `连续 `},
		{"task credit column", `<th class="num">积分</th>`},
		{"task credit total tile", `累计积分`},
		{"task label mapping", `function taskLabel(t)`},
		{"task label used", `esc(taskLabel(t.task))`},
		{"overview checkin tile", `k:"今日签到"`},
	} {
		if !strings.Contains(html, tc.want) {
			t.Errorf("panel.html: missing %s (%q)", tc.name, tc.want)
		}
	}
}

// TestOverviewReportsCheckinCounts: the overview tile needs server-side counts.
func TestOverviewReportsCheckinCounts(t *testing.T) {
	src := readSourceFile(t, "overview.go")
	for _, want := range []string{"CheckedIn", "CheckinKnown", "checked_in", "checkin_known"} {
		if !strings.Contains(src, want) {
			t.Errorf("overview.go: missing %s", want)
		}
	}
	// An unfetched account has no snapshot and must not count as "not signed in".
	if !strings.Contains(src, "if a.Checkin != nil {") {
		t.Error("check-in counts must skip accounts with no snapshot")
	}
}
