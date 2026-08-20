package engine

import "testing"

// A tag must always resolve through the registry, because a tag can move. A
// digest must not: it is immutable, so a second pull cannot return different
// bytes — it only spends registry quota and makes a re-enable fail on a host
// that is offline or rate-limited while already holding exactly what it needs.
func TestOnlyDigestPinnedReferencesSkipTheRegistry(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reference string
		skippable bool
	}{
		{"tag", "postgres:18", false},
		{"tag that looks pinned", "postgres:sha256-abc", false},
		{"digest", "postgres@sha256:" + "a" + "b" + "c", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The guard is the reference shape; the registry call is what it
			// gates. Asserting the shape keeps the rule readable without a
			// docker daemon.
			got := containsDigest(tc.reference)
			if got != tc.skippable {
				t.Fatalf("containsDigest(%q) = %v, want %v", tc.reference, got, tc.skippable)
			}
		})
	}
}
