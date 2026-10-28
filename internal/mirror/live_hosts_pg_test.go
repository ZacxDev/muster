package mirror

import (
	"io"
	"log"
	"slices"
	"testing"
)

// TestLiveTokenHostsAreTheCallersOnly is what GET /api/doctor's host list
// rests on: each user sees the hosts of THEIR OWN live tokens, over a real
// Postgres with row-level security, never another user's. A revoked token's
// host drops out for its owner.
func TestLiveTokenHostsAreTheCallersOnly(t *testing.T) {
	s, _, _ := pgStore(t)
	svc := NewService(s, log.New(io.Discard, "", 0))

	for _, c := range []struct {
		owner int64
		host  string
	}{{1, "alpha-one"}, {1, "beta-one"}, {2, "gamma-two"}} {
		if _, _, err := s.CreateToken(user(c.owner), c.host); err != nil {
			t.Fatalf("mint %s for user %d: %v", c.host, c.owner, err)
		}
	}
	_, revoked, err := s.CreateToken(user(1), "gone-one")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.RevokeToken(user(1), revoked.ID); err != nil || !ok {
		t.Fatalf("revoke: %v %v", ok, err)
	}

	one, err := svc.LiveTokenHosts(user(1))
	if err != nil {
		t.Fatal(err)
	}
	two, err := svc.LiveTokenHosts(user(2))
	if err != nil {
		t.Fatal(err)
	}
	// Other tests in this package mint tokens too, so pin membership, not the
	// whole slice: each user's own hosts present, the other's absent.
	for _, h := range []string{"alpha-one", "beta-one"} {
		if !slices.Contains(one, h) || slices.Contains(two, h) {
			t.Errorf("%s: user 1 sees it=%v (want true), user 2 sees it=%v (want false)", h, slices.Contains(one, h), slices.Contains(two, h))
		}
	}
	if !slices.Contains(two, "gamma-two") || slices.Contains(one, "gamma-two") {
		t.Errorf("gamma-two: user 2 sees it=%v (want true), user 1 sees it=%v (want false)", slices.Contains(two, "gamma-two"), slices.Contains(one, "gamma-two"))
	}
	if slices.Contains(one, "gone-one") {
		t.Error("a host whose only token is revoked is still listed")
	}
}
