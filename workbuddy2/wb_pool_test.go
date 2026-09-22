// wb_pool_test.go covers the pool wiring: candidate filtering, the 6004
// model-scoped cooldown vs account-wide rate limit, and expiry bucketing.
package main

import (
	"sync"
	"testing"
	"time"

	wauth "github.com/sliverkiss/workbuddy2plus-plugin/internal/auth"
	wupstream "github.com/sliverkiss/workbuddy2plus-plugin/internal/upstream"
)

// testAuth builds a minimal cn-realm account for pool tests.
func testAuth(uid, realm string) *wauth.Auth {
	a := &wauth.Auth{UID: uid, AccessToken: "t", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	if realm == "global" {
		a.Domain = "www.workbuddy.ai"
	}
	return a
}

// TestApplyUpstreamErrorModelScoped6004: after a 6004 for one model, a healthy
// sibling account is preferred for that model — while the limited account stays
// usable for other models.
//
// The body carries the upstream reset wall-clock because CooldownSoftForModel
// stays model-scoped only when resetAt parses (a zero resetAt degrades to an
// account-wide cooldown by design).
func TestApplyUpstreamErrorModelScoped6004(t *testing.T) {
	resetPoolForTest(t)
	p := poolInstance()
	p.Add(testAuth("uid6004", "cn"))
	p.Add(testAuth("uid-other", "cn"))

	reset := time.Now().Add(3 * time.Hour).In(wupstream.SoftRateResetLoc()).Format("2006-01-02 15:04:05")
	body := `{"code":6004,"msg":"该模型使用量超限，将在 ` + reset + ` 重置"}`
	applyUpstreamError("uid6004", "glm-5.3-flash", 429, body)

	if got := p.Pick("glm-5.3-flash"); got == nil || got.UID != "uid-other" {
		t.Errorf("6004 should prefer the healthy sibling, got %v", got)
	}
	// Direct assertion: the limit is registered against that model only.
	st, ok := p.Status("uid6004")
	if !ok {
		t.Fatal("account missing from pool")
	}
	var limited bool
	for _, m := range st.RateLimitedModels {
		if m.Model == "glm-5.3-flash" {
			limited = true
		}
	}
	if !limited {
		t.Errorf("6004 should register a model-scoped limit, got %+v", st.RateLimitedModels)
	}
	// The account itself is not cooled: no account-level until.
	if !st.Until.IsZero() && time.Now().Before(st.Until) {
		t.Errorf("6004 must not cool the whole account: until=%v", st.Until)
	}
}

func TestApplyUpstreamErrorAccountWideRateLimit(t *testing.T) {
	resetPoolForTest(t)
	p := poolInstance()
	p.Add(testAuth("uid429", "cn"))
	p.Add(testAuth("uid-other", "cn"))

	applyUpstreamError("uid429", "glm-5.3-flash", 429, `{"msg":"too many requests"}`)
	// Account-wide cooling clears model exemptions, so the sibling wins for
	// every model.
	for _, model := range []string{"glm-5.3-flash", "deepseek-v4.1-flash"} {
		for i := 0; i < 10; i++ {
			if got := p.Pick(model); got != nil && got.UID == "uid429" {
				t.Errorf("account-wide 429 should cool the account for %s", model)
				break
			}
		}
	}
}

// resetPoolForTest rebuilds the process-wide pool singleton.
func resetPoolForTest(t *testing.T) {
	t.Helper()
	old := wbPool
	t.Cleanup(func() { wbPool = old })
	wbPool = nil
	wbPoolOnce = sync.Once{}
}

// TestExpiringCreditsBuckets: only packages ending inside the window count.
func TestExpiringCreditsBuckets(t *testing.T) {
	soon := time.Now().Add(2 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	far := time.Now().Add(30 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	cr := &creditsSummary{Packages: []packageSummary{
		{Name: "soon", Remain: 100, CycleEnd: soon},
		{Name: "far", Remain: 900, CycleEnd: far},
	}}
	if got := expiringCredits(cr, 7*24*time.Hour); got != 100 {
		t.Errorf("expiring = %d want 100", got)
	}
	if got := expiringCredits(nil, 7*24*time.Hour); got != 0 {
		t.Errorf("nil credits = %d want 0", got)
	}
}

// TestClassify6004IsSoftRate guards the assumption the wiring depends on.
func TestClassify6004IsSoftRate(t *testing.T) {
	if got := wupstream.Classify(429, `{"code":6004,"msg":"该模型使用量超限"}`); got != wupstream.ErrSoftRate {
		t.Errorf("6004 classified as %v, want ErrSoftRate", got)
	}
}
