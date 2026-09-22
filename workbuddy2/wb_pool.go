// wb_pool.go wires the vendored account pool (three-factor weighted pick,
// per-model 6004 cooldown, credit-expiry preference) into the plugin.
//
// Division of labour with the host: CPA's built-in scheduler still owns the
// final routing decision; this pool only *orders* the workbuddy candidates the
// host offers (scheduler_mode=credits) and records error outcomes so a
// model-limited account is skipped for that model only.
//
// Keys: pool indexes by auth UID, which is exactly what the host passes as
// SchedulerAuthCandidate.ID (toAuthDataOpts sets ID=uid) — so candidate IDs
// map 1:1 onto pool entries with no translation table.
package main

import (
	"path/filepath"
	"sync"
	"time"

	wauth "github.com/sliverkiss/workbuddy2plus-plugin/internal/auth"
	wpool "github.com/sliverkiss/workbuddy2plus-plugin/internal/pool"
	wupstream "github.com/sliverkiss/workbuddy2plus-plugin/internal/upstream"
)

var (
	wbPool     *wpool.Pool
	wbPoolOnce sync.Once
)

// poolInstance lazily builds the process-wide pool. State persists next to the
// panel data so cooldowns survive a plugin reload.
func poolInstance() *wpool.Pool {
	wbPoolOnce.Do(func() {
		stateFp := filepath.Join(panelDataDir(), "pool-state.json")
		p := wpool.New(stateFp)
		p.SetBreaker(3, 30*time.Minute, 6*time.Hour)
		p.SetSoftRateMax(2 * time.Hour)
		p.SetWeights(0.5, 5.0)
		p.SetMaxInFlight(3)
		wbPool = p
	})
	return wbPool
}

// syncPoolFromHost refreshes pool entries from the host auth list and feeds the
// credit snapshot used by the weighted pick. Called from the scheduler hook and
// from credit queries — both already run on panel traffic.
func syncPoolFromHost() {
	files, err := panelHostAuthList()
	if err != nil {
		return
	}
	p := poolInstance()
	auths := make([]*wauth.Auth, 0, len(files))
	for _, f := range files {
		if f.Disabled {
			continue
		}
		sa, _, err := hostAuthGetBundle(f.AuthIndex)
		if err != nil {
			continue
		}
		auths = append(auths, wbAuth(nil, sa))
	}
	p.SyncToDir(auths)
	// Credits: the cached snapshot is refreshed by the panel/credits path; here
	// we only push what is already known so the weighted pick sees fresh numbers
	// without adding upstream calls.
	for _, f := range files {
		if remain, exhausted := cachedCreditsScore(f.ID); remain >= 0 && !exhausted {
			p.SetCredits(f.ID, remain)
		}
	}
}

// pickFromPool chooses among host candidates by three-factor weight. Returns ""
// when no candidate is healthy in the pool, leaving the decision to the host.
// The host's candidate list is authoritative: Pick walks the whole pool in
// weight order, so we retry until it lands on an offered candidate.
func pickFromPool(candidates []string, model string) string {
	if len(candidates) == 0 {
		return ""
	}
	allowed := make(map[string]bool, len(candidates))
	for _, id := range candidates {
		allowed[id] = true
	}
	p := poolInstance()
	tried := map[string]bool{}
	for range candidates {
		acct := p.PickExcludingForRealm(tried, model, "")
		if acct == nil {
			return ""
		}
		tried[acct.UID] = true
		if allowed[acct.UID] {
			return acct.UID
		}
	}
	return ""
}

// applyUpstreamError records one classified upstream failure into the pool so
// the next pick avoids the affected account (or just that model for 6004).
// model is the upstream model id of the failed request — required for the
// model-scoped cases; pass "" when unknown.
func applyUpstreamError(authID, model string, status int, body string) {
	if authID == "" {
		return
	}
	kind := wupstream.Classify(status, body)
	p := poolInstance()
	switch kind {
	case wupstream.ErrModelBlocked:
		// 11102: this backend does not serve the model — avoid (account, model).
		if model != "" {
			p.BlockModelBackoff(authID, model, "11102 no such model")
		}
	case wupstream.ErrSoftRate:
		// 6004 (IsModelRateLimit) is the per-model limit: cool only that model,
		// aligned to the upstream reset wall-clock when the body carries one —
		// CooldownSoftForModel needs a non-zero resetAt to stay model-scoped
		// (zero falls back to an account-wide cooldown). Anything else is an
		// account-wide throttle.
		if model != "" && wupstream.IsModelRateLimit(body) {
			resetAt, _ := wupstream.ParseRateReset(body)
			p.CooldownSoftForModel(authID, 10*time.Minute, resetAt, model, "6004 model limit")
		} else {
			p.CooldownSoftRate(authID, 10*time.Minute, time.Time{}, "rate limited")
		}
	case wupstream.ErrHardCredit:
		p.Cooldown(authID, wpool.CoolHard, 0, "credits exhausted")
	case wupstream.ErrSessionDead:
		p.Disable(authID, "session dead")
	case wupstream.ErrServer:
		p.NoteError(authID)
	default:
		// Client-side errors are not the account's fault.
		return
	}
}

// notePoolSuccess clears failure counters after a clean request.
func notePoolSuccess(authID string) {
	if authID == "" {
		return
	}
	poolInstance().NoteSuccess(authID)
}

// expiringCredits sums the credit already available in packages whose billing
// cycle ends within `within`. Derived from the credits snapshot the panel
// already holds, so the pool's expiry preference costs no upstream call.
func expiringCredits(cr *creditsSummary, within time.Duration) int64 {
	if cr == nil {
		return 0
	}
	deadline := time.Now().Add(within)
	var sum int64
	for _, p := range cr.Packages {
		end, err := time.ParseInLocation("2006-01-02 15:04:05", p.CycleEnd, time.Local)
		if err != nil || end.After(deadline) {
			continue
		}
		sum += p.Remain
	}
	return sum
}
