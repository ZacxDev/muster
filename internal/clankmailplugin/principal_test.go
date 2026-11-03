package clankmailplugin

import (
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/ZacxDev/clankmail/go/clankmail"
)

// TOFU on the principal (SPEC §6.1). The peer's chief key leaves its owner and,
// after the hub's 24 h cooldown, a DIFFERENT principal claims the released
// name and enrolls the same key under it. Every object verifies and the pin
// still matches, so only the recorded principal tells the two apart: the peer
// is disabled until a human accepts the change.
func TestPrincipalChangeDisablesThePeerUntilAccepted(t *testing.T) {
	e := newEnv(t)
	p := e.ready("")
	h, agents, _, err := e.us.hub.Handle(bg, "alice")
	e.must(err)
	ho, err := clankmail.Verify(h)
	e.must(err)
	en, err := clankmail.Verify(agents[0].Enroll)
	e.must(err)
	revoke := func(k ed25519.PrivateKey, target string) {
		tok, _, err := SignObject(k, "revoke", e.clk.Now().UnixMilli(), map[string]any{"target": target})
		e.must(err)
		_, err = e.directHub(k).Submit(bg, tok)
		e.must(err)
	}
	revoke(e.peer.chief, en.ID)     // the chief withdraws its enroll
	revoke(e.peer.principal, ho.ID) // the owner releases @alice
	e.clk.Advance(24*time.Hour + time.Minute)
	impostor := newKey(t)
	ih := e.directHub(impostor)
	tok, _, err := SignObject(impostor, "handle", e.clk.Now().UnixMilli(), map[string]any{"name": "alice", "hub": testHub})
	e.must(err)
	_, err = ih.Submit(bg, tok)
	e.must(err)
	gtok, gid, err := SignObject(impostor, "grant", e.clk.Now().UnixMilli(),
		map[string]any{"agent": KeyIDOf(e.peer.chief), "name": "chief", "hub": testHub})
	e.must(err)
	_, err = ih.Submit(bg, gtok)
	e.must(err)
	etok, _, err := SignObject(e.peer.chief, "enroll", e.clk.Now().UnixMilli(), map[string]any{"grant": gid})
	e.must(err)
	_, err = e.peer.hub.Submit(bg, etok)
	e.must(err)
	// Control: without the recorded principal the moved chain verifies.
	h2, a2, f2, err := e.us.hub.Handle(bg, "alice")
	e.must(err)
	if _, err := VerifyChain(e.peerConf(e.peer), testHub, h2, a2, f2, "", e.clk.Now().UnixMilli()); err != nil {
		t.Fatalf("control: the moved chain should verify on its own: %v", err)
	}
	if err := p.SyncPeers(bg); err != nil {
		t.Fatal(err)
	}
	if _, status, _ := p.peerFor(KeyIDOf(e.peer.chief)); status != peerDisabled {
		t.Fatalf("after a principal change the peer is %q, want disabled", status)
	}
	if out, _ := p.VerifyPeer(bg, "alice/chief", false); out["status"] != peerDisabled {
		t.Fatalf("peer verify without --accept-principal re-enabled it: %v", out)
	}
	out, err := p.VerifyPeer(bg, "alice/chief", true)
	e.must(err)
	if out["status"] != peerOK || out["principal"] != KeyIDOf(impostor) {
		t.Fatalf("peer verify --accept-principal: %v", out)
	}
}
