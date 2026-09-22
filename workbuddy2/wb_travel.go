// wb_travel.go runs the daily cat-travel task: adopt a buddy when there is
// none, otherwise advance its travel one step (depart when idle, claim when
// arrived). One action per account per run — the travel cycle is measured in
// hours, so extra polling only adds upstream requests.
//
// Ported from workbuddy2api's scheduler; CN-only (Global has no travel system),
// per-account spacing, and adoption debounced per CST day so a threshold miss
// is not retried in a loop.
package main

import (
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	wauth "github.com/sliverkiss/workbuddy2plus-plugin/internal/auth"
	wupstream "github.com/sliverkiss/workbuddy2plus-plugin/internal/upstream"
)

const (
	// travelLocationID is fixed at 4 (an old-town inn): all four destinations
	// share the same reward/duration range, so there is no optimal choice.
	travelLocationID = 4
	// travelAccountDelay spaces accounts to stay clear of upstream rate limits.
	travelAccountDelay = 800 * time.Millisecond
)

// adoptionAttempted records accounts whose adoption was rejected because the
// conversation threshold was not met (uid -> "<CST date>|<generation>").
// Retrying the same day would only hammer the upstream; the next day retries
// naturally.
//
// The generation half is what makes the activity task useful: a rejection at
// 09:00 is invalidated as soon as the 10:00 activity sweep tops the
// conversation count up, so the 21:00 travel run retries instead of waiting a
// full day for the +300 credit.
var (
	adoptMu        sync.Mutex
	adoptAttempted = map[string]string{}
)

// adoptGeneration advances whenever the activity task completes a sweep.
var adoptGeneration atomic.Int64

// adoptMark renders the current (CST day, generation) pair.
func adoptMark() string {
	return cstDate(time.Now()) + "|" + strconv.FormatInt(adoptGeneration.Load(), 10)
}

// reserveAdoptSlot atomically claims today's adoption attempt for uid at the
// current generation. It returns false when an attempt at this generation has
// already been made or is in flight — the guard must be taken *before* the
// upstream call, since BuddyFirst grants +300 credits and a check-then-act pair
// lets two concurrent sweeps both pass.
func reserveAdoptSlot(uid string) bool {
	mark := adoptMark()
	adoptMu.Lock()
	defer adoptMu.Unlock()
	if adoptAttempted[uid] == mark {
		return false
	}
	adoptAttempted[uid] = mark
	return true
}

// pruneAdoptAttempted drops marks from other CST days or older generations.
// Called from the dashboard prune path so the map does not keep one entry per
// uid ever seen.
func pruneAdoptAttempted() {
	today := cstDate(time.Now())
	adoptMu.Lock()
	defer adoptMu.Unlock()
	for uid, mark := range adoptAttempted {
		if !strings.HasPrefix(mark, today+"|") {
			delete(adoptAttempted, uid)
		}
	}
}

// runTravelTask advances every usable CN account's buddy by one step. Returns
// false when another sweep is already in flight.
func runTravelTask() bool {
	if !travelRunning.CompareAndSwap(false, true) {
		log.Printf("travel: skipped (a sweep is already running)")
		return false
	}
	defer travelRunning.Store(false)

	accounts := cnAccounts()
	if len(accounts) == 0 {
		return true
	}
	log.Printf("travel: start for %d account(s)", len(accounts))
	for i, acct := range accounts {
		if i > 0 {
			time.Sleep(travelAccountDelay)
		}
		travelOne(acct)
	}
	log.Printf("travel: done")
	return true
}

// travelOne is the per-account state machine: at most one action, no polling.
func travelOne(acct *wauth.Auth) {
	label := accountLabel(acct)
	buddy, err := wbClient.BuddyInfo(acct)
	if err != nil {
		log.Printf("travel %s: buddy-info: %v", label, err)
		return
	}
	if buddy == nil {
		adoptBuddy(acct)
		return
	}
	ts, err := wbClient.TravelStatus(acct)
	if err != nil {
		log.Printf("travel %s: status: %v", label, err)
		return
	}
	switch ts.State {
	case "arrived":
		travelClaim(acct, ts)
	case "idle":
		travelDepart(acct, ts)
	case "traveling":
		log.Printf("travel %s: skip (traveling record=%d)", label, ts.RecordID)
	default:
		log.Printf("travel %s: skip (unknown state %q)", label, ts.State)
	}
}

// travelDepart sends the buddy out when idle and under the daily limit
// (one departure per CST day).
func travelDepart(acct *wauth.Auth, ts *wupstream.TravelState) {
	label := accountLabel(acct)
	if ts.DailyLimitReached {
		log.Printf("travel %s: skip (daily limit reached)", label)
		return
	}
	if err := wbClient.TravelDepart(acct, travelLocationID); err != nil {
		log.Printf("travel %s: depart: %v", label, err)
		recordTask("travel", acct.Nickname, false, err.Error())
		return
	}
	log.Printf("travel %s: depart ok location=%d", label, travelLocationID)
	recordTask("travel", acct.Nickname, true, "departed")
}

// travelClaim collects the arrival reward (record_id is required).
func travelClaim(acct *wauth.Auth, ts *wupstream.TravelState) {
	label := accountLabel(acct)
	if ts.RecordID == 0 {
		log.Printf("travel %s: claim skipped (arrived but no record_id)", label)
		return
	}
	reward, err := wbClient.TravelClaim(acct, ts.RecordID)
	if err != nil {
		log.Printf("travel %s: claim record=%d: %v", label, ts.RecordID, err)
		recordTask("travel", acct.Nickname, false, err.Error())
		return
	}
	log.Printf("travel %s: claim ok record=%d reward=%d", label, ts.RecordID, reward)
	recordTask("travel", acct.Nickname, true, "claimed "+itoa(reward)+" credit")
}

// adoptBuddy adopts a buddy: agree to the terms (idempotent) then first-adopt.
// A conversation-threshold rejection is expected behaviour — it is recorded and
// skipped until the activity task tops the conversation count up (see
// adoptGeneration), or until the next CST day.
func adoptBuddy(acct *wauth.Auth) {
	label := accountLabel(acct)
	// Reserve before the upstream call: BuddyFirst grants +300 credits, so the
	// guard cannot be a check-then-act pair.
	if !reserveAdoptSlot(acct.UID) {
		return
	}
	if err := wbClient.BuddyAgreement(acct); err != nil {
		log.Printf("travel %s: agreement: %v", label, err)
		return
	}
	err := wbClient.BuddyFirst(acct)
	switch {
	case err == nil:
		log.Printf("travel %s: adopt ok (+300 credits)", label)
		recordTask("travel-adopt", acct.Nickname, true, "+300 credit")
	case wupstream.IsBuddyTaskIncomplete(err):
		log.Printf("travel %s: adopt skipped (conversation threshold not reached, retry after the activity task)", label)
	default:
		log.Printf("travel %s: adopt: %v", label, err)
	}
}

// itoa renders an int64 for log/task messages.
func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
