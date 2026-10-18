// desensitize_payload_test.go: the old prepareUpstreamBody-integration tests
// were removed with the vendored 2api pipeline takeover; the marker-semantics
// test (exact + case-sensitive matching, no marker forgery) survives because
// it exercises applyDesensitizeInPlace directly — the same entry point the
// new wb_upstream.go bridge calls on the final outbound body.
package main

import "testing"

func TestDesensitizeUserMarkersAreExactAndCaseSensitive(t *testing.T) {
	cfg, err := parseFeatureRuntime([]byte("desensitize: true\ndesensitize_terms: [attack]\n"))
	if err != nil {
		t.Fatal(err)
	}
	exact := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "<system-reminder> attack"}}}
	wrong := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "<System-Reminder> attack"}}}
	applyDesensitizeInPlace(exact, cfg)
	applyDesensitizeInPlace(wrong, cfg)
	if got := exact["messages"].([]any)[0].(map[string]any)["content"]; got != "<system-reminder> a"+zeroWidthSpace+"ttack" {
		t.Errorf("exact marker content = %q", got)
	}
	if got := wrong["messages"].([]any)[0].(map[string]any)["content"]; got != "<System-Reminder> attack" {
		t.Errorf("case-variant marker content = %q", got)
	}
}
