// cleanchunk_test.go: cleanChunkJSON/isEmptyValue/normalizeToolsForUpstream and
// their tests were removed with the vendored 2api ChunkNormalizer takeover
// (internal/upstream/sse.go). Only the stripDataPrefix test remains here.
package main

import "testing"

func TestStripDataPrefix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"data: hello", "hello"},
		{"data:hello", "hello"},
		{"data:  {\"a\":1}", "{\"a\":1}"},
		{"hello", "hello"},
		{"", ""},
		{"DATA: x", "DATA: x"}, // case-sensitive: only lowercase data:
	}
	for _, tc := range cases {
		if got := stripDataPrefix(tc.in); got != tc.want {
			t.Fatalf("stripDataPrefix(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}
