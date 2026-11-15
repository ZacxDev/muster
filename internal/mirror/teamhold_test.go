package mirror_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/attemptcode"
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

// connectRaw is a hand-driven host (rawConnect) presenting attempt code
// ("" = none), from the loopback peer every test connection shares.
func (r *rig) connectRaw(t *testing.T, token, code string, hello mirror.Msg) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+token)
	if code != "" {
		hdr.Set(attemptcode.Header, code)
	}
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

// isHeld: err is a held team host connection's refusal, with a code
// (ErrHeldForManager) or without one (ErrHeldNoCode).
func isHeld(err error) bool {
	return err != nil && (strings.Contains(err.Error(), mirror.ErrHeldForManager.Error()) || strings.Contains(err.Error(), mirror.ErrHeldNoCode.Error()))
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
// and nothing it lists shows) — and recorded by its attempt code for the
// manager's card. A manager confirms THAT code: its machine's next retry
// takes the host, and the old connection is cancelled, as a replacement is
// today. The confirm is one-shot.
//
// CONTROL: a PERSONAL host's reconnect still replaces the live one at once.
// The team id is a fixture value no constant in the code equals.
func TestASecondTeamHostConnectionWaitsForAManager(t *testing.T) {
	const team = 900233
	const moved = "M0VD-0000-0001"
	r := newRig(t)

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
	if err := r.connectRaw(t, tok, moved, hello("build-01", true, "moved-host")); !isHeld(err) {
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
	if len(held) != 1 || held[0].Host != "build-01" || !held[0].Coded || held[0].Attempts != 1 || held[0].Confirmed {
		t.Fatalf("the manager's card = %+v; want one unconfirmed coded hold of build-01 after one attempt", held)
	}
	if got := r.svc.Hub().HeldReplacements(1); len(got) != 0 {
		t.Fatalf("the personal owner sees holds %+v; want none (a hold is its team's)", got)
	}
	if err := r.connectRaw(t, tok, moved, hello("build-01", true, "moved-host")); !isHeld(err) {
		t.Fatalf("a second attempt with no manager's confirm = %v; want it held", err)
	}
	if got := r.svc.Hub().HeldReplacements(team); len(got) != 1 || got[0].Attempts != 2 || got[0].First.After(got[0].Last) {
		t.Fatalf("after a second attempt with the same code the card = %+v; want one attempt row counting 2", got)
	}

	// The manager types that code: the chosen machine's next retry takes the host.
	if _, _, err := r.svc.Hub().ConfirmCode(team, moved); err != nil {
		t.Fatalf("the confirm of the held attempt's code = %v", err)
	}
	if err := r.connectRaw(t, tok, moved, hello("build-01", true, "moved-host")); err != nil {
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
	// One-shot: the same code again is held.
	if err := r.connectRaw(t, tok, moved, hello("build-01", true, "third")); !isHeld(err) {
		t.Fatalf("a connection after the confirm was spent = %v; want it held", err)
	}
}

// TestTwoHeldAttemptsArmNothingUntilOneIsChosen: two machines are held for
// one live team host; the card lists BOTH, and nothing is armed: both stay
// held on retry. A confirm with no code, or a code no attempt presented,
// arms nothing either.
func TestTwoHeldAttemptsArmNothingUntilOneIsChosen(t *testing.T) {
	const team = 900241
	const one, two = "0NE0-1111-1111", "TW00-2222-2222"
	r := newRig(t)
	tok, _ := liveTeamHost(t, r, team, "build-04")
	for _, c := range []string{one, two} {
		if err := r.connectRaw(t, tok, c, hello("build-04", true, "s")); !isHeld(err) {
			t.Fatalf("an attempt with %s = %v; want it held", c, err)
		}
	}
	held := r.svc.Hub().HeldReplacements(team)
	if len(held) != 2 || held[0].Confirmed || held[1].Confirmed {
		t.Fatalf("the card = %+v; want both attempts, neither confirmed", held)
	}
	for _, c := range []string{"", "9999-9999-9999"} {
		if _, _, err := r.svc.Hub().ConfirmCode(team, c); !errors.Is(err, mirror.ErrNoSuchCode) {
			t.Fatalf("a confirm naming %q (no held attempt) = %v; want ErrNoSuchCode", c, err)
		}
	}
	for _, c := range []string{one, two} {
		if err := r.connectRaw(t, tok, c, hello("build-04", true, "s")); !isHeld(err) {
			t.Fatalf("with no attempt chosen, %s's retry = %v; want it held", c, err)
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
