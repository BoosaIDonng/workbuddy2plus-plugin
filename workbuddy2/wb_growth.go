// wb_growth.go runs the daily activity-map task: report chat activity to light
// up the streak map, then claim the growth rewards that unlock (streak tiers,
// gift/compensation packs, makeup cards, lottery draws).
//
// Ported from workbuddy2api's scheduler with the same semantics but wired to
// this plugin's auth/pool: CN accounts only (Global returns 500 on the streak
// endpoint), per-account spacing to stay clear of upstream rate limits, and
// per-day idempotence so a restart or a second trigger cannot double-claim.
package main

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	wauth "github.com/sliverkiss/workbuddy2plus-plugin/internal/auth"
	wupstream "github.com/sliverkiss/workbuddy2plus-plugin/internal/upstream"
)

const (
	// activityReportCount mirrors the upstream task target: 5 reports in one
	// conversation light up the chat_5 prerequisite for buddy adoption.
	activityReportCount = 5
	// Spacing between accounts and between one account's reports. These are
	// real upstream writes; going faster trips rate limits.
	activityAccountDelay = 800 * time.Millisecond
	activityReportGap    = 1500 * time.Millisecond
)

// growthRewardClaimed tracks per-account, per-CST-day reward claims so a
// restart or a second trigger cannot replay the claim chain.
var (
	growthClaimedMu sync.Mutex
	growthClaimed   = map[string]string{} // uid -> CST date
)

func growthClaimedToday(uid string) bool {
	growthClaimedMu.Lock()
	defer growthClaimedMu.Unlock()
	return growthClaimed[uid] == cstDate(time.Now())
}

func markGrowthClaimed(uid string) {
	growthClaimedMu.Lock()
	defer growthClaimedMu.Unlock()
	growthClaimed[uid] = cstDate(time.Now())
}

// cstDate renders the upstream natural day (CST) — growth rewards reset on this
// boundary, not on the container's local midnight.
func cstDate(t time.Time) string {
	return t.In(wupstream.SoftRateResetLoc()).Format("2006-01-02")
}

// runActivityTask reports chat activity for every usable CN account, then runs
// the reward chain. Per-account failures never abort the sweep.
func runActivityTask() {
	accounts := cnAccounts()
	if len(accounts) == 0 {
		return
	}
	log.Printf("activity: start for %d account(s)", len(accounts))
	for i, acct := range accounts {
		if i > 0 {
			time.Sleep(activityAccountDelay)
		}
		reportActivityFor(acct)
	}
	log.Printf("activity: done")
}

// reportActivityFor sends the N reports for one account and, on full success,
// runs the streak self-check and the reward chain.
func reportActivityFor(acct *wauth.Auth) {
	label := accountLabel(acct)
	// All reports share one conversation id (one session, N turns) with a
	// distinct request id per turn — the shape the upstream expects.
	cid := fmt.Sprintf("wb2api-%d", time.Now().UnixMilli())
	sent := 0
	for i := 1; i <= activityReportCount; i++ {
		rid := fmt.Sprintf("%s-r%d", cid, i)
		if err := wbClient.ReportChatActivity(acct, cid, rid); err != nil {
			log.Printf("activity %s: report %d/%d: %v", label, i, activityReportCount, err)
			break
		}
		sent++
		if i < activityReportCount {
			time.Sleep(activityReportGap)
		}
	}
	if sent < activityReportCount {
		recordTask("activity", acct.Nickname, false, fmt.Sprintf("reported %d/%d", sent, activityReportCount))
		return
	}
	recordTask("activity", acct.Nickname, true, fmt.Sprintf("%d reports sent", sent))
	// Report 200 does not prove the streak counted (a missing user id makes the
	// upstream silently drop it), so read the streak back.
	checkActivityStreak(acct)
	claimGrowthRewards(acct)
}

// checkActivityStreak reads the streak back and warns when the report appears
// to have been dropped silently.
func checkActivityStreak(acct *wauth.Auth) {
	days, err := wbClient.GrowthStreak(acct)
	if err != nil {
		log.Printf("WARN: activity %s: streak check failed: %v", accountLabel(acct), err)
		return
	}
	if days == 0 {
		log.Printf("WARN: activity %s: report OK but streak.days=0 (silent drop?)", accountLabel(acct))
		return
	}
	log.Printf("activity %s: streak days=%d", accountLabel(acct), days)
}

// claimGrowthRewards runs the per-day claim chain: gift/compensation packs,
// makeup card for yesterday, the streak tier redeem, then the lottery draws.
// Idempotent per CST day; every step is best-effort and failures do not stop
// the rest of the chain.
func claimGrowthRewards(acct *wauth.Auth) {
	if acct == nil || acct.IsGlobal() {
		return // growth chain is CN-only (Global streak endpoint returns 500)
	}
	if growthClaimedToday(acct.UID) {
		return
	}
	// Gift / compensation packs first: their credits do not depend on the
	// streak state, and they are one-shot idempotent writes upstream.
	claimGiftPacks(acct)

	state, err := wbClient.GrowthRewardState(acct)
	if err != nil {
		log.Printf("WARN: activity %s: reward-state: %v", accountLabel(acct), err)
		return
	}
	// Makeup card: restore yesterday so today's redeem sees the recovered day
	// count (this is what protects the 7/14/28-day milestones).
	if makeupYesterday(acct) {
		if st2, err2 := wbClient.GrowthRewardState(acct); err2 == nil {
			state = st2
		}
	}
	days := state.Days()
	tier := growthEligibleTier(days, &state.Redemption)
	if tier == "" {
		// No newly reached tier — normal state for most days.
		claimGrowthLottery(acct)
		markGrowthClaimed(acct.UID)
		return
	}
	res, err := wbClient.GrowthRedeem(acct, tier, "")
	switch {
	case err == nil:
		log.Printf("activity %s: redeem tier=%s ok (+%d credit, +%d energy, +%d chances)",
			accountLabel(acct), tier, res.CreditGranted, res.EnergyGranted, res.ChancesGranted)
		recordTask("growth-redeem", acct.Nickname, true,
			fmt.Sprintf("tier=%s +%d credit", tier, res.CreditGranted))
	case wupstream.IsRedeemAlreadyClaimed(err) || wupstream.IsRedeemNotEnoughDays(err):
		log.Printf("activity %s: redeem tier=%s skip (already claimed or days not enough)", accountLabel(acct), tier)
	default:
		log.Printf("WARN: activity %s: redeem tier=%s: %v", accountLabel(acct), tier, err)
		recordTask("growth-redeem", acct.Nickname, false, err.Error())
	}
	claimGrowthLottery(acct)
	markGrowthClaimed(acct.UID)
}

// growthEligibleTier returns the highest tier the account has reached but not
// yet claimed, or "" when there is nothing to claim.
func growthEligibleTier(days int, rs *wupstream.GrowthRedemptionStatus) string {
	if rs == nil {
		return ""
	}
	// Prefer the upstream tier table when present (it carries the thresholds);
	// otherwise fall back to the fixed 7/14/28-day ladder.
	specs := rs.Tiers
	if len(specs) == 0 {
		specs = []wupstream.GrowthTierSpec{
			{Tier: "7d", Days: 7},
			{Tier: "14d", Days: 14},
			{Tier: "28d", Days: 28},
		}
	}
	best := ""
	bestDays := 0
	for _, spec := range specs {
		if spec.Days == 0 || days < spec.Days {
			continue
		}
		if rs.Claimed(spec.Tier) {
			continue
		}
		if spec.Days > bestDays {
			best, bestDays = spec.Tier, spec.Days
		}
	}
	return best
}

// claimGiftPacks claims the one-shot gift and compensation packs. Business
// errors are expected when there is nothing to claim, so they stay quiet.
func claimGiftPacks(acct *wauth.Auth) {
	if credits, err := wbClient.ClaimGift(acct); err == nil && credits > 0 {
		log.Printf("activity %s: gift +%d credit", accountLabel(acct), credits)
		recordTask("growth-gift", acct.Nickname, true, fmt.Sprintf("+%d credit", credits))
	}
	if credits, err := wbClient.ClaimCompensation(acct); err == nil && credits > 0 {
		log.Printf("activity %s: compensation +%d credit", accountLabel(acct), credits)
		recordTask("growth-compensation", acct.Nickname, true, fmt.Sprintf("+%d credit", credits))
	}
}

// makeupYesterday spends a makeup card on yesterday when it was missed.
// Returns true when a card was used (the caller then re-reads the state).
func makeupYesterday(acct *wauth.Auth) bool {
	cards, err := wbClient.GrowthStreakWithCards(acct)
	if err != nil || cards.MakeupCards.Balance <= 0 {
		return false
	}
	heatmap, err := wbClient.GrowthHeatmap(acct)
	if err != nil {
		return false
	}
	yesterday := time.Now().In(wupstream.SoftRateResetLoc()).AddDate(0, 0, -1).Format("2006-01-02")
	for _, cell := range heatmap {
		if !strings.HasPrefix(cell.Date, yesterday) {
			continue
		}
		if cell.Score > 0 {
			return false // yesterday already counted
		}
		if err := wbClient.UseMakeupCard(acct, yesterday); err != nil {
			log.Printf("WARN: activity %s: makeup card: %v", accountLabel(acct), err)
			return false
		}
		log.Printf("activity %s: makeup card used for %s", accountLabel(acct), yesterday)
		recordTask("growth-makeup", acct.Nickname, true, "yesterday restored")
		return true
	}
	return false
}

// claimGrowthLottery spends every available lottery chance (redeems and gift
// packs grant chances; unspent chances would otherwise expire).
func claimGrowthLottery(acct *wauth.Auth) {
	chances, err := wbClient.GrowthLotteryChances(acct)
	if err != nil || chances <= 0 {
		return
	}
	won := 0
	for i := 0; i < chances; i++ {
		res, err := wbClient.GrowthLotteryDraw(acct, "")
		if err != nil {
			log.Printf("WARN: activity %s: lottery draw %d/%d: %v", accountLabel(acct), i+1, chances, err)
			break
		}
		if res != nil && res.CreditAmount > 0 {
			won += res.CreditAmount
		}
	}
	if won > 0 {
		log.Printf("activity %s: lottery +%d credit", accountLabel(acct), won)
		recordTask("growth-lottery", acct.Nickname, true, fmt.Sprintf("+%d credit", won))
	}
}

// cnAccounts returns the CN-realm accounts that can run growth tasks: enabled
// and holding an access token. Global accounts are excluded (their streak
// endpoint returns 500).
func cnAccounts() []*wauth.Auth {
	files, err := panelHostAuthList()
	if err != nil {
		return nil
	}
	var out []*wauth.Auth
	for _, f := range files {
		if f.Disabled {
			continue
		}
		sa, _, err := hostAuthGetBundle(f.AuthIndex)
		if err != nil {
			continue
		}
		a := wbAuth(nil, sa)
		if a.UID == "" || a.AccessTokenValue() == "" || a.IsGlobal() {
			continue
		}
		a.Nickname = sa.Account.Nickname
		out = append(out, a)
	}
	return out
}

// accountLabel renders "nickname (uid8)" for logs without leaking credentials.
func accountLabel(a *wauth.Auth) string {
	nick := strings.TrimSpace(a.Nickname)
	if nick == "" {
		nick = uid8Of(a.UID)
	}
	return nick
}
