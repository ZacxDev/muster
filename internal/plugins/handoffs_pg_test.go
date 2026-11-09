package plugins

import (
	"errors"
	"slices"
	"testing"
	"time"
)

const (
	hoOrigin = "https://*.viewer.example.net"
	hoEntry  = "https://w.viewer.example.net/enter"
)

func mustViewer(t *testing.T, s *PGStore, uid int64, name string, caps ...Capability) Plugin {
	t.Helper()
	_, p, err := s.CreatePlugin(session(uid), NewPlugin{Name: name, Label: "Example viewer", DataScope: "example",
		Capabilities: caps, Origin: hoOrigin, EntryURL: hoEntry})
	if err != nil {
		t.Fatalf("create viewer %s for %d: %v", name, uid, err)
	}
	return p
}

func hoIn() HandoffIn {
	return HandoffIn{Target: HandoffTarget{Mirror: &MirrorTarget{Host: "ws", Session: "main", Window: "@3"}},
		Purpose: PurposeView, SteppedUp: true}
}

// TestCreatePluginStoresTheViewerLinkAndNoneScope: a viewer-only token stores
// data scope `none` (whatever the form said), its origin and entry URL; a
// messages+viewer token keeps its scope; asking for an origin without viewer,
// viewer without one, an unknown capability or none at all is ErrInvalid.
func TestCreatePluginStoresTheViewerLinkAndNoneScope(t *testing.T) {
	s, _, _ := pgStore(t)
	v := mustViewer(t, s, 2, "viewer-only", CapViewer)
	if v.DataScope != ViewerOnlyDataScope || !slices.Equal(v.Capabilities, []Capability{CapViewer}) ||
		v.Origin == nil || *v.Origin != hoOrigin || v.EntryURL == nil || *v.EntryURL != hoEntry {
		t.Fatalf("viewer-only plugin = scope %q caps %v origin %v entry %v", v.DataScope, v.Capabilities, v.Origin, v.EntryURL)
	}
	both := mustViewer(t, s, 2, "viewer-both", CapViewer, CapMessages, CapViewer)
	if both.DataScope != "example" || !slices.Equal(both.Capabilities, []Capability{CapMessages, CapViewer}) {
		t.Fatalf("messages+viewer plugin = scope %q caps %v; want example, [messages viewer]", both.DataScope, both.Capabilities)
	}
	for name, in := range map[string]NewPlugin{
		"origin without viewer": {Name: "bad-a", Label: "x", DataScope: "x", Capabilities: []Capability{CapMessages}, Origin: hoOrigin, EntryURL: hoEntry},
		"viewer without origin": {Name: "bad-b", Label: "x", DataScope: "x", Capabilities: []Capability{CapViewer}},
		"viewer, no entry URL":  {Name: "bad-c", Label: "x", DataScope: "x", Capabilities: []Capability{CapViewer}, Origin: hoOrigin},
		"unknown capability":    {Name: "bad-d", Label: "x", DataScope: "x", Capabilities: []Capability{"admin"}},
		"no capability":         {Name: "bad-e", Label: "x", DataScope: "x"},
	} {
		if _, _, err := s.CreatePlugin(session(2), in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v; want ErrInvalid", name, err)
		}
	}
}

// TestAHandoffIsSingleUseExpiresAndIsItsPluginsAlone: a code redeems ONCE, with
// the target, purpose, stepped_up and mint time it was minted with; at exactly
// HandoffTTL after mint it is refused, a moment before it is not; another of
// the same owner's viewer plugins, and another owner's, get ErrNotFound for it;
// a messages-only or revoked plugin cannot mint; a malformed code is
// ErrNotFound without a query.
func TestAHandoffIsSingleUseExpiresAndIsItsPluginsAlone(t *testing.T) {
	s, _, _ := pgStore(t)
	p := mustViewer(t, s, 2, "viewer-a", CapViewer)
	other := mustViewer(t, s, 2, "viewer-b", CapViewer)
	stranger := mustViewer(t, s, 3, "viewer-c", CapViewer)
	t0 := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)

	code, got, err := s.MintHandoff(session(2), p.ID, hoIn(), t0)
	if err != nil || got.ID != p.ID || len(code) != 43 {
		t.Fatalf("MintHandoff = %q, %d, %v", code, got.ID, err)
	}
	for name, try := range map[string]func() error{
		"another of the owner's plugins":   func() error { _, err := s.RedeemHandoff(plugin(2), other.ID, code, t0); return err },
		"another owner's plugin":           func() error { _, err := s.RedeemHandoff(plugin(3), stranger.ID, code, t0); return err },
		"another owner, naming the plugin": func() error { _, err := s.RedeemHandoff(plugin(3), p.ID, code, t0); return err },
	} {
		if err := try(); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v; want ErrNotFound", name, err)
		}
	}
	h, err := s.RedeemHandoff(plugin(2), p.ID, code, t0.Add(HandoffTTL-time.Millisecond))
	if err != nil || h.OwnerID != 2 || h.Purpose != PurposeView || !h.SteppedUp || !h.MintedAt.Equal(t0) ||
		h.Target.Mirror == nil || *h.Target.Mirror != (MirrorTarget{Host: "ws", Session: "main", Window: "@3"}) {
		t.Fatalf("RedeemHandoff just before expiry = %+v, %v", h, err)
	}
	if _, err := s.RedeemHandoff(plugin(2), p.ID, code, t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a second redeem: %v; want ErrNotFound (single use)", err)
	}

	late, _, err := s.MintHandoff(session(2), p.ID, hoIn(), t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemHandoff(plugin(2), p.ID, late, t0.Add(HandoffTTL)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a redeem at exactly %v: %v; want ErrNotFound (expired)", HandoffTTL, err)
	}

	if _, err := s.RedeemHandoff(plugin(2), p.ID, "short", t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a malformed code: %v; want ErrNotFound", err)
	}
	msgs := mustCreateTok(t, s, 2, "messages-only")
	if _, _, err := s.MintHandoff(session(2), msgs.ID, hoIn(), t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("minting for a messages-only plugin: %v; want ErrNotFound", err)
	}
	if _, _, err := s.MintHandoff(session(3), p.ID, hoIn(), t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("minting for another owner's plugin: %v; want ErrNotFound", err)
	}
	if _, err := s.Revoke(session(2), other.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.MintHandoff(session(2), other.ID, hoIn(), t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("minting for a revoked plugin: %v; want ErrNotFound", err)
	}
}

// TestHandoffMintsAreCappedPerOwnerPerMinute: the owner's HandoffMintsPerMinute
// codes in one minute (across their plugins) are minted and the next is
// ErrHandoffRateLimited; a minute later it is minted again, and another
// owner's cap is their own.
func TestHandoffMintsAreCappedPerOwnerPerMinute(t *testing.T) {
	s, _, _ := pgStore(t)
	a := mustViewer(t, s, 2, "viewer-a", CapViewer)
	b := mustViewer(t, s, 2, "viewer-b", CapViewer)
	c := mustViewer(t, s, 3, "viewer-c", CapViewer)
	t0 := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	for i := 0; i < HandoffMintsPerMinute; i++ {
		id := a.ID
		if i%2 == 1 {
			id = b.ID
		}
		if _, _, err := s.MintHandoff(session(2), id, hoIn(), t0.Add(time.Duration(i)*time.Millisecond)); err != nil {
			t.Fatalf("mint %d: %v", i+1, err)
		}
	}
	if _, _, err := s.MintHandoff(session(2), a.ID, hoIn(), t0.Add(time.Second)); !errors.Is(err, ErrHandoffRateLimited) {
		t.Fatalf("mint %d in the minute: %v; want ErrHandoffRateLimited", HandoffMintsPerMinute+1, err)
	}
	if _, _, err := s.MintHandoff(session(3), c.ID, hoIn(), t0.Add(time.Second)); err != nil {
		t.Fatalf("another owner's first mint: %v; want minted", err)
	}
	if _, _, err := s.MintHandoff(session(2), a.ID, hoIn(), t0.Add(time.Minute+time.Second)); err != nil {
		t.Fatalf("a mint a minute later: %v; want minted", err)
	}
}

func mustCreateTok(t *testing.T, s *PGStore, uid int64, name string) Plugin {
	t.Helper()
	_, p := mustCreate(t, s, uid, name)
	return p
}
