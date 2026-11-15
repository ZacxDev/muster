package mirror_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/mirror"
)

// mintTeam is memStore.mint for a TEAM host token (Token.Team): what
// PGStore.Authenticate reports for a token whose owner is a team.
func (m *memStore) mintTeam(t *testing.T, team int64, host string) string {
	t.Helper()
	tok, _ := m.mint(t, team, host)
	m.mu.Lock()
	defer m.mu.Unlock()
	rec := m.tokens[mirror.HashToken(tok)]
	rec.Team = true
	m.tokens[mirror.HashToken(tok)] = rec
	return tok
}

// peerHeader names, in these tests, the address a connection comes from: one
// process cannot dial from several addresses, so the rig reads it instead of
// the TCP peer (Service.SetPeerAddr). A request without it is liveAddr.
const (
	peerHeader = "X-Test-Peer"
	liveAddr   = "198.51.100.10" // the real team host
	movedAddr  = "203.0.113.7"   // the machine the host moved to
	keptAddr   = "192.0.2.99"    // an ex-operator retrying with a kept token
)

func newPeerRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	r.svc.SetPeerAddr(func(req *http.Request) string {
		if a := req.Header.Get(peerHeader); a != "" {
			return a
		}
		return liveAddr
	})
	return r
}

// connectFrom is a hand-driven host (rawConnect) connecting from addr.
func (r *rig) connectFrom(t *testing.T, token, addr string, hello mirror.Msg) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+token)
	hdr.Set(peerHeader, addr)
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(r.srv.URL, "http")+mirror.ConnectPath, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		return err
	}
	t.Cleanup(func() { ws.CloseNow() })
	b, _ := json.Marshal(hello)
	if err := ws.Write(ctx, websocket.MessageText, b); err != nil {
		return err
	}
	_, data, err := ws.Read(ctx)
	if err != nil {
		return err
	}
	var w mirror.Msg
	_ = json.Unmarshal(data, &w)
	if w.Error != "" {
		return errors.New(w.Error)
	}
	// Keep reading so the connection stays served (pings, requests).
	go func() {
		for {
			if _, _, err := ws.Read(context.Background()); err != nil {
				return
			}
		}
	}()
	return nil
}

func isHeld(err error) bool {
	return err != nil && strings.Contains(err.Error(), "pending a manager's confirmation")
}

// liveTeamHost connects the real team host (from liveAddr) for team.
func liveTeamHost(t *testing.T, r *rig, team int64, host string) (tok string, done <-chan error) {
	t.Helper()
	tok = r.store.mintTeam(t, team, host)
	b := &fakeBackend{}
	b.set("real-host")
	_, done = r.connect(t, host, tok, b, true)
	r.online(t, team, host, "real-host")
	return tok, done
}

// TestASecondTeamHostConnectionWaitsForAManager (team sharing 2b.2): a team
// host's connection is PINNED. With (team, build-01) live, a second
// connection presenting the team's token is HELD — refused as pending a
// manager's confirmation, registered nowhere (the live host keeps serving,
// and nothing it lists shows) — and recorded by its ADDRESS for the
// manager's card. A manager confirms THAT attempt: its machine's next retry
// takes the host, and the old connection is cancelled, as a replacement is
// today. The confirm is one-shot.
//
// CONTROL: a PERSONAL host's reconnect still replaces the live one at once.
// The team id is a fixture value no constant in the code equals.
func TestASecondTeamHostConnectionWaitsForAManager(t *testing.T) {
	const team = 900233
	r := newPeerRig(t)

	// CONTROL: a personal host's second connection replaces the first.
	ptok, _ := r.store.mint(t, 1, "laptop")
	p1, p2 := &fakeBackend{}, &fakeBackend{}
	p1.set("p-one")
	p2.set("p-two")
	_, pdone := r.connect(t, "laptop", ptok, p1, false)
	r.online(t, 1, "laptop", "p-one")
	r.connect(t, "laptop", ptok, p2, false)
	eventually(t, "the personal reconnect replaces the live host", func() bool {
		return r.svc.Hub().Online(1, "laptop", "p-two") && !r.svc.Hub().Online(1, "laptop", "p-one")
	})
	<-pdone

	tok, done1 := liveTeamHost(t, r, team, "build-01")
	if err := r.connectFrom(t, tok, movedAddr, hello("build-01", true, "moved-host")); !isHeld(err) {
		t.Fatalf("a second connection for a live team host = %v; want it held, pending a manager's confirmation", err)
	}
	if r.svc.Hub().Online(team, "build-01", "moved-host") || !r.svc.Hub().Online(team, "build-01", "real-host") {
		t.Fatal("the held connection was swapped in: its listing shows, or the real host stopped serving")
	}
	select {
	case err := <-done1:
		t.Fatalf("the live team host's connection ended while the second one was held: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	held := r.svc.Hub().HeldReplacements(team)
	if len(held) != 1 || held[0].Host != "build-01" || held[0].Addr != movedAddr || held[0].Attempts != 1 || held[0].Confirmed {
		t.Fatalf("the manager's card = %+v; want one unconfirmed hold of build-01 from %s after one attempt", held, movedAddr)
	}
	if got := r.svc.Hub().HeldReplacements(1); len(got) != 0 {
		t.Fatalf("the personal owner sees holds %+v; want none (a hold is its team's)", got)
	}
	if err := r.connectFrom(t, tok, movedAddr, hello("build-01", true, "moved-host")); !isHeld(err) {
		t.Fatalf("a second attempt with no manager's confirm = %v; want it held", err)
	}
	if got := r.svc.Hub().HeldReplacements(team); len(got) != 1 || got[0].Attempts != 2 || got[0].First.After(got[0].Last) {
		t.Fatalf("after a second attempt from the same address the card = %+v; want one attempt row counting 2", got)
	}

	// The manager chooses that attempt: the chosen machine's next retry takes the host.
	if !r.svc.Hub().ConfirmReplacement(team, "build-01", movedAddr) {
		t.Fatal("the confirm of the held attempt found nothing")
	}
	if err := r.connectFrom(t, tok, movedAddr, hello("build-01", true, "moved-host")); err != nil {
		t.Fatalf("the chosen machine's retry after the confirm = %v; want it taken", err)
	}
	eventually(t, "the chosen connection replaces the live team host", func() bool {
		return r.svc.Hub().Online(team, "build-01", "moved-host") && !r.svc.Hub().Online(team, "build-01", "real-host")
	})
	select {
	case <-done1:
	case <-time.After(5 * time.Second):
		t.Fatal("the replaced team host's connection did not end")
	}
	// One-shot: the same address again is held.
	if err := r.connectFrom(t, tok, movedAddr, hello("build-01", true, "third")); !isHeld(err) {
		t.Fatalf("a connection after the confirm was spent = %v; want it held", err)
	}
}

// TestAConfirmForOneAddressDoesNotLetAnotherIn (#685 round 0): the confirm is
// bound to the CHOSEN attempt. With the moved machine and a kept-token copy
// both held, a manager confirms the moved machine's address; the kept copy,
// retrying first inside the window, is still held and the real host keeps
// serving; only the chosen address then takes the host.
func TestAConfirmForOneAddressDoesNotLetAnotherIn(t *testing.T) {
	const team = 900239
	r := newPeerRig(t)
	tok, _ := liveTeamHost(t, r, team, "build-03")
	for _, a := range []string{movedAddr, keptAddr} {
		if err := r.connectFrom(t, tok, a, hello("build-03", true, "from-"+a)); !isHeld(err) {
			t.Fatalf("an attempt from %s = %v; want it held", a, err)
		}
	}
	if !r.svc.Hub().ConfirmReplacement(team, "build-03", movedAddr) {
		t.Fatal("the confirm of the moved machine's attempt found nothing")
	}
	if err := r.connectFrom(t, tok, keptAddr, hello("build-03", true, "from-"+keptAddr)); !isHeld(err) {
		t.Fatalf("the kept copy's retry after a confirm for ANOTHER address = %v; want it still held", err)
	}
	if !r.svc.Hub().Online(team, "build-03", "real-host") || r.svc.Hub().Online(team, "build-03", "from-"+keptAddr) {
		t.Fatal("the kept copy displaced the real host on a confirm meant for another address")
	}
	if err := r.connectFrom(t, tok, movedAddr, hello("build-03", true, "from-"+movedAddr)); err != nil {
		t.Fatalf("the chosen address's retry = %v; want it taken", err)
	}
	eventually(t, "the chosen machine takes the host", func() bool {
		return r.svc.Hub().Online(team, "build-03", "from-"+movedAddr) && !r.svc.Hub().Online(team, "build-03", "real-host")
	})
}

// TestTwoHeldAttemptsArmNothingUntilOneIsChosen: two machines are held for
// one live team host; the card lists BOTH, by address, and nothing is armed:
// both stay held on retry. A confirm that names no attempt, or an address no
// attempt came from, arms nothing either. Only a chosen one is let in.
func TestTwoHeldAttemptsArmNothingUntilOneIsChosen(t *testing.T) {
	const team = 900241
	r := newPeerRig(t)
	tok, _ := liveTeamHost(t, r, team, "build-04")
	for _, a := range []string{movedAddr, keptAddr} {
		if err := r.connectFrom(t, tok, a, hello("build-04", true, "s")); !isHeld(err) {
			t.Fatalf("an attempt from %s = %v; want it held", a, err)
		}
	}
	held := r.svc.Hub().HeldReplacements(team)
	if len(held) != 2 || held[0].Addr != keptAddr || held[1].Addr != movedAddr || held[0].Confirmed || held[1].Confirmed {
		t.Fatalf("the card = %+v; want both attempts, by address, neither confirmed", held)
	}
	for _, a := range []string{"", "198.18.0.1"} {
		if r.svc.Hub().ConfirmReplacement(team, "build-04", a) {
			t.Fatalf("a confirm naming %q (no held attempt) armed a replacement", a)
		}
	}
	for _, a := range []string{movedAddr, keptAddr} {
		if err := r.connectFrom(t, tok, a, hello("build-04", true, "s")); !isHeld(err) {
			t.Fatalf("with no attempt chosen, %s's retry = %v; want it held", a, err)
		}
	}
	if !r.svc.Hub().Online(team, "build-04", "real-host") {
		t.Fatal("the real host stopped serving with nothing chosen")
	}
}

// TestATeamHostReconnectingWhileOfflineIsTakenAtOnce: the hold is only for a
// LIVE team host. Once its connection has ended (a restart of the real host),
// the next connection is taken with no manager's confirm.
func TestATeamHostReconnectingWhileOfflineIsTakenAtOnce(t *testing.T) {
	const team = 900237
	r := newRig(t)
	tok := r.store.mintTeam(t, team, "build-02")
	b1 := &fakeBackend{}
	b1.set("before-restart")
	stop1, done1 := r.connect(t, "build-02", tok, b1, false)
	r.online(t, team, "build-02", "before-restart")
	stop1()
	<-done1
	eventually(t, "the team host goes offline", func() bool { return !r.svc.Hub().Online(team, "build-02", "before-restart") })
	b2 := &fakeBackend{}
	b2.set("after-restart")
	r.connect(t, "build-02", tok, b2, false)
	r.online(t, team, "build-02", "after-restart")
	if held := r.svc.Hub().HeldReplacements(team); len(held) != 0 {
		t.Fatalf("a reconnect while offline left a hold: %+v", held)
	}
}

// TestAHeldMachineIsToldItIsHeld: Service.HeldHere — what `muster doctor`'s
// this-host check asks — is true for the machine whose attempt is held (it
// shares the live host's token, so the token alone says "connected"), and
// false for the live machine itself.
func TestAHeldMachineIsToldItIsHeld(t *testing.T) {
	const team = 900247
	r := newPeerRig(t)
	tok, _ := liveTeamHost(t, r, team, "build-05")
	if err := r.connectFrom(t, tok, keptAddr, hello("build-05", true, "s")); !isHeld(err) {
		t.Fatalf("the second machine = %v; want it held", err)
	}
	ask := func(addr string) bool {
		req := httptest.NewRequest(http.MethodGet, "/mirror/host", nil)
		req.Header.Set(peerHeader, addr)
		return r.svc.HeldHere(asUser(team), "build-05", req)
	}
	if !ask(keptAddr) {
		t.Fatal("the held machine is not told it is held")
	}
	if ask(liveAddr) {
		t.Fatal("the LIVE machine is told it is held")
	}
}
