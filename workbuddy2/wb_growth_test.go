// wb_growth_test.go covers the growth/travel task logic that can be tested
// without upstream calls: tier selection, per-day idempotence, and the CST
// day boundary the claim chain keys on.
package main

import (
	"testing"
	"time"

	wupstream "github.com/sliverkiss/workbuddy2plus-plugin/internal/upstream"
)

// TestGrowthEligibleTierPicksHighestReached: the highest reached-but-unclaimed
// tier wins; claimed tiers are skipped; unreached tiers are ignored.
func TestGrowthEligibleTierPicksHighestReached(t *testing.T) {
	rs := &wupstream.GrowthRedemptionStatus{}
	cases := []struct {
		days int
		want string
	}{
		{0, ""}, {6, ""}, {7, "7d"}, {13, "7d"}, {14, "14d"}, {27, "14d"}, {28, "28d"}, {99, "28d"},
	}
	for _, c := range cases {
		if got := growthEligibleTier(c.days, rs); got != c.want {
			t.Errorf("days=%d tier=%q want %q", c.days, got, c.want)
		}
	}
}

// TestGrowthEligibleTierSkipsClaimed: an already-claimed tier is not re-claimed,
// so the next lower unclaimed tier (or nothing) is returned.
func TestGrowthEligibleTierSkipsClaimed(t *testing.T) {
	rs := &wupstream.GrowthRedemptionStatus{Tier7dStatus: "claimed"}
	if got := growthEligibleTier(7, rs); got != "" {
		t.Errorf("claimed 7d should yield no tier, got %q", got)
	}
	if got := growthEligibleTier(14, rs); got != "14d" {
		t.Errorf("days=14 with 7d claimed should yield 14d, got %q", got)
	}
}

// TestGrowthEligibleTierNilStatus: a missing redemption status must not panic.
func TestGrowthEligibleTierNilStatus(t *testing.T) {
	if got := growthEligibleTier(30, nil); got != "" {
		t.Errorf("nil status should yield no tier, got %q", got)
	}
}

// TestGrowthClaimedPerDay: the claim chain is idempotent per CST day.
func TestGrowthClaimedPerDay(t *testing.T) {
	growthClaimedMu.Lock()
	growthClaimed = map[string]string{}
	growthClaimedMu.Unlock()

	if growthClaimedToday("uid1") {
		t.Fatal("fresh account should not be marked claimed")
	}
	markGrowthClaimed("uid1")
	if !growthClaimedToday("uid1") {
		t.Fatal("account should be marked claimed today")
	}
	if growthClaimedToday("uid2") {
		t.Fatal("other accounts must be unaffected")
	}
	// Yesterday's mark does not block today.
	growthClaimedMu.Lock()
	growthClaimed["uid3"] = "2000-01-01"
	growthClaimedMu.Unlock()
	if growthClaimedToday("uid3") {
		t.Fatal("stale mark should not block today")
	}
}

// TestCstDateUsesUpstreamBoundary: growth rewards reset on the CST day, not the
// container's local midnight.
func TestCstDateUsesUpstreamBoundary(t *testing.T) {
	// 2026-09-21 23:30 UTC = 2026-09-22 07:30 CST → next CST day.
	utc := time.Date(2026, 9, 21, 23, 30, 0, 0, time.UTC)
	if got := cstDate(utc); got != "2026-09-22" {
		t.Errorf("cstDate(%v) = %q want 2026-09-22", utc, got)
	}
}

// TestAdoptTriedPerDay mirrors the growth per-day check for buddy adoption.
func TestAdoptTriedPerDay(t *testing.T) {
	adoptMu.Lock()
	adoptAttempted = map[string]string{}
	adoptMu.Unlock()

	if adoptTriedToday("uid1") {
		t.Fatal("fresh account should not be marked")
	}
	markAdoptTried("uid1")
	if !adoptTriedToday("uid1") {
		t.Fatal("account should be marked after a threshold miss")
	}
}

// TestActivityAndTravelEnabledDefaults: both growth tasks default to on, and
// the config toggles flip them.
func TestActivityAndTravelEnabledDefaults(t *testing.T) {
	activityMu.Lock()
	oldA := activityAuto
	activityMu.Unlock()
	travelMu.Lock()
	oldT := travelAuto
	travelMu.Unlock()
	t.Cleanup(func() {
		activityMu.Lock()
		activityAuto = oldA
		activityMu.Unlock()
		travelMu.Lock()
		travelAuto = oldT
		travelMu.Unlock()
	})

	if !activityEnabled() {
		t.Error("activity task should default to enabled")
	}
	if !travelEnabled() {
		t.Error("travel task should default to enabled")
	}
	activityMu.Lock()
	activityAuto = false
	activityMu.Unlock()
	if activityEnabled() {
		t.Error("activity toggle should disable the task")
	}
}
