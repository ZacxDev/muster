package previewgw

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestATeamOwnersCookieIsShort (team sharing 2b.3b; #696 round 1, 🟡3): the
// gateway knows nothing of a team's membership, so a member muster removed
// keeps a team preview until the owner cookie expires. When /self names the
// owner a TEAM, the cookie lasts TeamOwnerCookieTTL (15 min): its Max-Age
// says so, and its own claim is refused once that has passed. CONTROL: a
// person's cookie on a gateway built the same way keeps OwnerCookieTTL (8 h).
//
// RED at c47718e1: every owner's cookie lasted 8 h.
func TestATeamOwnersCookieIsShort(t *testing.T) {
	for _, c := range []struct {
		name, kind, maxAge  string
		admitsAt, refusesAt time.Duration
	}{
		{"team", "team", "Max-Age=900", 14 * time.Minute, 16 * time.Minute},
		{"CONTROL person", "", "Max-Age=28800", 7 * time.Hour, 9 * time.Hour},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newGW(t, gwOpts{})
			s := viewerSelf(1, "one@example.invalid")
			s.OwnerKind = c.kind
			e.fm.set("mpl_owner1", 200, s)
			e.g.CheckOwners(context.Background())
			_, _, id, code := e.openedPreview(t)
			a := e.useEntry(t, id, code, "document")
			if sc := strings.Join(a.header.Values("Set-Cookie"), "\n"); !strings.Contains(sc, c.maxAge) {
				t.Fatalf("owner cookie %q; want %s", sc, c.maxAge)
			}
			v, now := ownerCookieOf(t, a), e.clock.Now()
			if _, ok := e.g.sign.checkOwnerCookie(v, id, 1, now.Add(c.admitsAt)); !ok {
				t.Errorf("the cookie is refused at +%v", c.admitsAt)
			}
			if _, ok := e.g.sign.checkOwnerCookie(v, id, 1, now.Add(c.refusesAt)); ok {
				t.Errorf("the cookie still admits at +%v", c.refusesAt)
			}
		})
	}
}
