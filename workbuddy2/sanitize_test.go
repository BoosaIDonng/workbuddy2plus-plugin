// sanitize_test.go: the old prepareUpstreamBody/rewriteSystem pipeline and its
// tests were removed with the vendored workbuddy2api pipeline takeover (the
// fingerprint/template sanitization now lives in internal/upstream/sanitize.go
// with its own test suite). Only the truncate test for redact.go remains here.
package main

import "testing"

func TestTruncate(t *testing.T) {
	if truncate("hello", 10) != "hello" {
		t.Fatal("short string should be unchanged")
	}
	if truncate("hello world", 5) != "hello" {
		t.Fatal("should truncate to 5 chars")
	}
	if truncate("", 5) != "" {
		t.Fatal("empty string")
	}
}
