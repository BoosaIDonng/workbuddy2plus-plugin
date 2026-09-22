// panel_metrics_test.go covers the TTFB key contract: the mark written by the
// streaming pump and the lookup done by recordRequest must derive the model the
// same way, or the mark is never consumed.
package main

import (
	"fmt"
	"testing"
	"time"
)

// TestTTFBMarkIsConsumedWhenUpstreamModelEmpty is the regression test for the
// leak: the pump marks with the raw upstream model (empty when the alias
// resolves to nothing) while recordRequest looks up the alias fallback, so the
// entry was orphaned — one per streaming request, until the size guard wiped the
// whole map including in-flight marks.
func TestTTFBMarkIsConsumedWhenUpstreamModelEmpty(t *testing.T) {
	ttfbTracker.Lock()
	ttfbTracker.marks = map[string]time.Time{}
	ttfbTracker.Unlock()

	started := time.Now().Add(-250 * time.Millisecond)
	// The pump passes an empty upstream model; recordRequest would fall back to
	// the requested alias. Both must land on the same key.
	noteFirstToken("uid-1", "", "gpt-5-alias", started)
	if got := takeTTFB("uid-1", ttfbModelKey("", "gpt-5-alias"), started); got <= 0 {
		t.Fatalf("mark with an empty upstream model was not consumed (ttfb=%d)", got)
	}

	ttfbTracker.Lock()
	leaked := len(ttfbTracker.marks)
	ttfbTracker.Unlock()
	if leaked != 0 {
		t.Fatalf("%d TTFB marks leaked", leaked)
	}
}

// TestTTFBMarkConsumedForNormalModel covers the ordinary path where the upstream
// model is present.
func TestTTFBMarkConsumedForNormalModel(t *testing.T) {
	ttfbTracker.Lock()
	ttfbTracker.marks = map[string]time.Time{}
	ttfbTracker.Unlock()

	started := time.Now().Add(-120 * time.Millisecond)
	noteFirstToken("uid-2", "upstream-model", "alias-model", started)
	if got := takeTTFB("uid-2", ttfbModelKey("upstream-model", "alias-model"), started); got <= 0 {
		t.Fatalf("mark was not consumed (ttfb=%d)", got)
	}
}

// TestTTFBModelKeyPrefersUpstream: the upstream model wins when both are set, so
// the key identifies what was actually called.
func TestTTFBModelKeyPrefersUpstream(t *testing.T) {
	if got := ttfbModelKey("upstream", "alias"); got != "upstream" {
		t.Errorf("got %q want upstream", got)
	}
	if got := ttfbModelKey("", "alias"); got != "alias" {
		t.Errorf("got %q want alias", got)
	}
	if got := ttfbModelKey("  ", "  "); got != "" {
		t.Errorf("got %q want empty", got)
	}
}

// TestTTFBEvictionKeepsInFlightMarks: the size guard must drop only stale marks,
// not wipe the map (a full wipe erased marks for streams still running, so their
// TTFB reported 0).
func TestTTFBEvictionKeepsInFlightMarks(t *testing.T) {
	ttfbTracker.Lock()
	ttfbTracker.marks = map[string]time.Time{}
	// Fill past the eviction threshold, then add one fresh mark (a stream
	// currently in flight) and one stale mark.
	for i := 0; i < 4200; i++ {
		ttfbTracker.marks[fmt.Sprintf("filler-%d", i)] = time.Now().Add(-30 * time.Minute)
	}
	ttfbTracker.marks["inflight"] = time.Now()
	ttfbTracker.marks["stale"] = time.Now().Add(-30 * time.Minute)
	ttfbTracker.Unlock()

	noteFirstToken("uid-3", "m", "m", time.Now())

	ttfbTracker.Lock()
	defer ttfbTracker.Unlock()
	if _, ok := ttfbTracker.marks["inflight"]; !ok {
		t.Error("in-flight mark must survive eviction")
	}
	if _, ok := ttfbTracker.marks["stale"]; ok {
		t.Error("stale mark should have been evicted")
	}
}
