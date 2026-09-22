// wb_growth_test.go covers the growth/travel task logic that can be tested
// without upstream calls: tier selection, per-day idempotence, and the CST
// day boundary the claim chain keys on.
package main

import (
	"sync"
	"sync/atomic"
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

	if growthClaimedTodayForTest("uid1") {
		t.Fatal("fresh account should not be marked claimed")
	}
	if _, ok := claimGrowthSlot("uid1"); !ok {
		t.Fatal("reservation must succeed on a fresh account")
	}
	if !growthClaimedTodayForTest("uid1") {
		t.Fatal("account should be marked claimed today")
	}
	if growthClaimedTodayForTest("uid2") {
		t.Fatal("other accounts must be unaffected")
	}
	// Yesterday's mark does not block today.
	growthClaimedMu.Lock()
	growthClaimed["uid3"] = "2000-01-01"
	growthClaimedMu.Unlock()
	if growthClaimedTodayForTest("uid3") {
		t.Fatal("stale mark should not block today")
	}
}

// TestClaimGrowthSlotIsExclusive: the slot is reserved before the claim chain
// runs, so concurrent triggers cannot both execute the real credit grants. This
// is the regression test for the check-then-act window that let the 10:00
// scheduler tick and a panel POST redeem the same tier twice.
func TestClaimGrowthSlotIsExclusive(t *testing.T) {
	growthClaimedMu.Lock()
	growthClaimed = map[string]string{}
	growthClaimedMu.Unlock()

	if _, ok := claimGrowthSlot("uid1"); !ok {
		t.Fatal("first reservation must succeed")
	}
	if _, ok := claimGrowthSlot("uid1"); ok {
		t.Fatal("second reservation must be refused while the first is held")
	}
	if _, ok := claimGrowthSlot("uid2"); !ok {
		t.Fatal("a different account must not be blocked")
	}
}

// TestClaimGrowthSlotConcurrent runs the reservation under real contention: the
// race detector plus the exactly-one-winner assertion cover both the data race
// and the double-grant window.
func TestClaimGrowthSlotConcurrent(t *testing.T) {
	growthClaimedMu.Lock()
	growthClaimed = map[string]string{}
	growthClaimedMu.Unlock()

	const workers = 16
	var granted atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, ok := claimGrowthSlot("uid-shared"); ok {
				granted.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := granted.Load(); got != 1 {
		t.Fatalf("exactly one caller may hold the slot, got %d", got)
	}
}

// TestReleaseGrowthSlotAllowsRetry: an aborted chain (reward-state read failed
// before any grant) gives the day back so a later run can retry.
func TestReleaseGrowthSlotAllowsRetry(t *testing.T) {
	growthClaimedMu.Lock()
	growthClaimed = map[string]string{}
	growthClaimedMu.Unlock()

	if _, ok := claimGrowthSlot("uid1"); !ok {
		t.Fatal("first reservation must succeed")
	}
	releaseGrowthSlot("uid1")
	if _, ok := claimGrowthSlot("uid1"); !ok {
		t.Fatal("a released slot must be re-acquirable")
	}
}

// TestPruneGrowthClaimedDropsOtherDays: stale per-day marks are removed so a
// re-imported account with a reused uid is not silently skipped.
func TestPruneGrowthClaimedDropsOtherDays(t *testing.T) {
	growthClaimedMu.Lock()
	growthClaimed = map[string]string{"old": "2000-01-01"}
	growthClaimedMu.Unlock()

	markGrowthClaimedForTest("today")
	pruneGrowthClaimed()

	growthClaimedMu.Lock()
	defer growthClaimedMu.Unlock()
	if _, ok := growthClaimed["old"]; ok {
		t.Error("stale entry should have been pruned")
	}
	if growthClaimed["today"] != cstDate(time.Now()) {
		t.Error("today's entry must survive the prune")
	}
}

// TestAdoptSlotIsExclusive: buddy adoption grants +300 credits, so its guard is
// reserved before the upstream call for the same reason as the claim slot.
func TestAdoptSlotIsExclusive(t *testing.T) {
	adoptMu.Lock()
	adoptAttempted = map[string]string{}
	adoptMu.Unlock()

	if !reserveAdoptSlot("uid1") {
		t.Fatal("first adoption reservation must succeed")
	}
	if reserveAdoptSlot("uid1") {
		t.Fatal("second adoption reservation must be refused")
	}
}

// TestAdoptGenerationInvalidatesSameDayMark: the activity task advances the
// generation, so a threshold miss at 09:00 does not block the 21:00 retry once
// the conversation count has been topped up.
func TestAdoptGenerationInvalidatesSameDayMark(t *testing.T) {
	adoptMu.Lock()
	adoptAttempted = map[string]string{}
	adoptMu.Unlock()

	if !reserveAdoptSlot("uid1") {
		t.Fatal("first reservation must succeed")
	}
	if reserveAdoptSlot("uid1") {
		t.Fatal("same generation must stay blocked")
	}
	adoptGeneration.Add(1) // the activity sweep completed
	if !reserveAdoptSlot("uid1") {
		t.Fatal("a new generation must allow a retry on the same CST day")
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

	if !reserveAdoptSlot("uid1") {
		t.Fatal("fresh account should be reservable")
	}
	if reserveAdoptSlot("uid1") {
		t.Fatal("account should be blocked after a reservation")
	}
}

// markGrowthClaimedForTest records a claim mark without going through the
// reservation API (test-only helper for the prune case).
func markGrowthClaimedForTest(uid string) {
	growthClaimedMu.Lock()
	defer growthClaimedMu.Unlock()
	growthClaimed[uid] = cstDate(time.Now())
}

// growthClaimedTodayForTest reports whether today's slot is held for uid.
func growthClaimedTodayForTest(uid string) bool {
	growthClaimedMu.Lock()
	defer growthClaimedMu.Unlock()
	return growthClaimed[uid] == cstDate(time.Now())
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
