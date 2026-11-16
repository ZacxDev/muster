package pages

import (
	"bytes"
	"errors"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
)

// testLinkKey is an invented master key (never a deployment's).
var testLinkKey = bytes.Repeat([]byte{7}, LinkKeySize)

func keyedStore(t *testing.T) *PGStore {
	t.Helper()
	s, _ := pgStore(t)
	if _, err := s.WithLinkKey(testLinkKey); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestASecondShareReturnsTheSameLink: the page's first ShareLink makes its
// reusable link (created), every later one answers the SAME token, and that
// token serves (LinkByToken). The link is the owner's, marked as who made it.
func TestASecondShareReturnsTheSameLink(t *testing.T) {
	s := keyedStore(t)
	two := asUser(2)
	b, n := blocksOf(t, qBlocks)
	p, _, err := s.Create(two, agentA, "Reusable", b, n)
	if err != nil {
		t.Fatal(err)
	}
	first, created, err := s.ShareLink(two, p.ID, "worker-a", true)
	if err != nil || !created || !first.Reusable || !first.ByAgent || first.State != LinkLive || !TokenShapeOK(first.Token) {
		t.Fatalf("the first share: %+v created=%v %v", first, created, err)
	}
	again, created, err := s.ShareLink(two, p.ID, "you", false)
	if err != nil || created || again.Token != first.Token || again.ID != first.ID {
		t.Fatalf("the second share: %+v created=%v %v; want link %d's token again", again, created, err, first.ID)
	}
	if l, err := s.LinkByToken(two, first.Token); err != nil || l.ID != first.ID {
		t.Fatalf("the reusable link does not serve: %+v %v", l, err)
	}
	all, err := s.ShareLinks(two)
	if err != nil || len(all) != 1 || all[p.ID].Token != first.Token {
		t.Fatalf("ShareLinks: %+v %v", all, err)
	}
	// Another owner sees no page and no link.
	if _, _, err := s.ShareLink(asUser(3), p.ID, "you", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another owner's ShareLink: %v, want ErrNotFound", err)
	}
	if other, _ := s.ShareLinks(asUser(3)); len(other) != 0 {
		t.Fatalf("another owner's ShareLinks: %+v", other)
	}
}

// TestTheTokenIsNotStoredInPlaintext: the row's token_sealed bytes are not the
// token, do not contain it, and are not its hash; token_sha256 is still the
// hash (the lookup).
func TestTheTokenIsNotStoredInPlaintext(t *testing.T) {
	s, pool := pgStore(t)
	if _, err := s.WithLinkKey(testLinkKey); err != nil {
		t.Fatal(err)
	}
	two := asUser(2)
	b, n := blocksOf(t, qBlocks)
	p, _, _ := s.Create(two, agentA, "Sealed", b, n)
	l, _, err := s.ShareLink(two, p.ID, "you", false)
	if err != nil {
		t.Fatal(err)
	}
	var sealed []byte
	var hash string
	if err := pool.QueryRow(authz.System(two), `SELECT token_sealed, token_sha256 FROM page_links WHERE id = $1`, l.ID).Scan(&sealed, &hash); err != nil {
		t.Fatal(err)
	}
	// The positive control: the column holds something (a NULL would pass
	// every "is not the token" check below).
	if len(sealed) < 28 {
		t.Fatalf("token_sealed is %d bytes: nothing was sealed", len(sealed))
	}
	if bytes.Equal(sealed, []byte(l.Token)) || bytes.Contains(sealed, []byte(l.Token)) || bytes.Contains(sealed, []byte(hash)) {
		t.Fatalf("token_sealed holds the token (or its hash) in the clear: %x", sealed)
	}
	if hash != TokenHash(l.Token) {
		t.Fatalf("token_sha256 = %s, want the token's hash", hash)
	}
}

// TestNewLinkRotatesAndRevokeTakesItAway: RotateShareLink revokes the old link
// (it 404s: LinkByToken refuses it, its sealed copy is wiped) and makes a new
// one that serves and is what Share answers next; RevokeShareLink takes that
// away, and the next Share makes yet another.
func TestNewLinkRotatesAndRevokeTakesItAway(t *testing.T) {
	s, pool := pgStore(t)
	if _, err := s.WithLinkKey(testLinkKey); err != nil {
		t.Fatal(err)
	}
	two := asUser(2)
	b, n := blocksOf(t, qBlocks)
	p, _, _ := s.Create(two, agentA, "Rotate", b, n)
	old, _, err := s.ShareLink(two, p.ID, "you", false)
	if err != nil {
		t.Fatal(err)
	}
	fresh, revoked, err := s.RotateShareLink(two, p.ID, "you")
	if err != nil || len(revoked) != 1 || revoked[0].ID != old.ID || fresh.Token == old.Token || !fresh.Reusable {
		t.Fatalf("rotate: %+v revoked %+v %v", fresh, revoked, err)
	}
	if _, err := s.LinkByToken(two, old.Token); !errors.Is(err, ErrLinkNotFound) {
		t.Fatalf("the rotated-away link still serves: %v", err)
	}
	var wiped bool
	if err := pool.QueryRow(authz.System(two), `SELECT token_sealed IS NULL FROM page_links WHERE id = $1`, old.ID).Scan(&wiped); err != nil || !wiped {
		t.Fatalf("the revoked link's sealed token was kept: %v %v", wiped, err)
	}
	if l, _, err := s.ShareLink(two, p.ID, "you", false); err != nil || l.Token != fresh.Token {
		t.Fatalf("Share after New link: %+v %v, want the new token", l, err)
	}
	if _, err := s.LinkByToken(two, fresh.Token); err != nil {
		t.Fatalf("the new link does not serve: %v", err)
	}
	gone, err := s.RevokeShareLink(two, p.ID)
	if err != nil || gone.ID != fresh.ID || gone.State != LinkRevoked {
		t.Fatalf("revoke: %+v %v", gone, err)
	}
	if _, err := s.LinkByToken(two, fresh.Token); !errors.Is(err, ErrLinkNotFound) {
		t.Fatalf("a revoked link still serves: %v", err)
	}
	if _, err := s.RevokeShareLink(two, p.ID); !errors.Is(err, ErrLinkNotFound) {
		t.Fatalf("revoking with none live: %v, want ErrLinkNotFound", err)
	}
	if l, created, err := s.ShareLink(two, p.ID, "you", false); err != nil || !created || l.Token == fresh.Token {
		t.Fatalf("Share after a revoke: %+v created=%v %v, want a new link", l, created, err)
	}
}

// TestALegacyHashOnlyLinkStillServes: a link made the old way (CreateLink:
// hash only, no sealed copy) serves, is not the page's reusable link, and a
// Share beside it makes the reusable one rather than reusing it.
func TestALegacyHashOnlyLinkStillServes(t *testing.T) {
	s := keyedStore(t)
	two := asUser(2)
	b, n := blocksOf(t, qBlocks)
	p, _, _ := s.Create(two, agentA, "Legacy", b, n)
	legacy, err := s.CreateLink(two, p.ID, LinkLive, "you", false)
	if err != nil {
		t.Fatal(err)
	}
	if l, err := s.LinkByToken(two, legacy.Token); err != nil || l.Reusable {
		t.Fatalf("the legacy link: %+v %v", l, err)
	}
	if all, _ := s.ShareLinks(two); len(all) != 0 {
		t.Fatalf("a hash-only link reads as reusable: %+v", all)
	}
	l, created, err := s.ShareLink(two, p.ID, "you", false)
	if err != nil || !created || l.ID == legacy.ID {
		t.Fatalf("Share beside a legacy link: %+v created=%v %v", l, created, err)
	}
	if _, err := s.LinkByToken(two, legacy.Token); err != nil {
		t.Fatalf("the legacy link stopped serving: %v", err)
	}
}

// TestNoKeyNoReusableLink: a store without a key makes no reusable link.
func TestNoKeyNoReusableLink(t *testing.T) {
	s, _ := pgStore(t)
	two := asUser(2)
	b, n := blocksOf(t, qBlocks)
	p, _, _ := s.Create(two, agentA, "No key", b, n)
	if s.LinksReusable() {
		t.Fatal("a store with no key says it keeps reusable links")
	}
	if _, _, err := s.ShareLink(two, p.ID, "you", false); !errors.Is(err, ErrNoLinkKey) {
		t.Fatalf("ShareLink without a key: %v", err)
	}
	if _, _, err := s.RotateShareLink(two, p.ID, "you"); !errors.Is(err, ErrNoLinkKey) {
		t.Fatalf("RotateShareLink without a key: %v", err)
	}
}

// TestASealedTokenMovedToAnotherRowDoesNotOpen: a ciphertext copied onto
// another page's reusable row (a bad UPDATE) does not open there — Share
// answers ErrLinkUnreadable and ShareLinks leaves that page out — so one
// page's address is never shown as another's. Its own row still opens (the
// control).
func TestASealedTokenMovedToAnotherRowDoesNotOpen(t *testing.T) {
	s, pool := pgStore(t)
	if _, err := s.WithLinkKey(testLinkKey); err != nil {
		t.Fatal(err)
	}
	two := asUser(2)
	b, n := blocksOf(t, qBlocks)
	pa, _, _ := s.Create(two, agentA, "A", b, n)
	pb, _, _ := s.Create(two, agentA, "B", b, n)
	la, _, err := s.ShareLink(two, pa.ID, "you", false)
	if err != nil {
		t.Fatal(err)
	}
	lb, _, err := s.ShareLink(two, pb.ID, "you", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(authz.System(two), `UPDATE page_links SET token_sealed = (SELECT token_sealed FROM page_links WHERE id = $1) WHERE id = $2`,
		la.ID, lb.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ShareLink(two, pb.ID, "you", false); !errors.Is(err, ErrLinkUnreadable) {
		t.Fatalf("a moved ciphertext opened: %v, want ErrLinkUnreadable", err)
	}
	all, err := s.ShareLinks(two)
	if err != nil || all[pa.ID].Token != la.Token {
		t.Fatalf("the control: page A's own link no longer opens: %+v %v", all, err)
	}
	if _, ok := all[pb.ID]; ok {
		t.Fatalf("ShareLinks showed page B with page A's token")
	}
	// The other key does not open it either.
	other, _ := NewPG(pool).WithLinkKey(bytes.Repeat([]byte{8}, LinkKeySize))
	if _, _, err := other.ShareLink(two, pa.ID, "you", false); !errors.Is(err, ErrLinkUnreadable) {
		t.Fatalf("another key opened the token: %v", err)
	}
}
