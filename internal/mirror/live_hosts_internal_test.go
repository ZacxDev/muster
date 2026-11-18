package mirror

import (
	"slices"
	"testing"
	"time"
)

// TestLiveHostsDropsOnlyFullyRevokedHosts: a host is expected while ANY of its
// tokens is live. A rotation (one revoked, one live) keeps it; a host whose
// every token is revoked drops out; duplicates collapse; the order is sorted
// whatever order the rows came in.
func TestLiveHostsDropsOnlyFullyRevokedHosts(t *testing.T) {
	now := time.Now()
	toks := []Token{
		{ID: 1, Host: "tower"},
		{ID: 2, Host: "laptop", RevokedAt: &now},
		{ID: 3, Host: "laptop"},
		{ID: 4, Host: "old-box", RevokedAt: &now},
		{ID: 5, Host: "alpha"},
		{ID: 6, Host: "tower"},
	}
	got := liveHosts(toks)
	want := []string{"alpha", "laptop", "tower"}
	if !slices.Equal(got, want) {
		t.Fatalf("liveHosts = %v, want %v", got, want)
	}
	if got := liveHosts(nil); got == nil || len(got) != 0 {
		t.Fatalf("liveHosts(nil) = %#v, want an empty non-nil slice (it is JSON-encoded as [])", got)
	}
}
