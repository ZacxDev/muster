package mirror_test

import (
	"strings"
	"testing"
	"time"

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

// TestASecondTeamHostConnectionWaitsForAManager (team sharing 2b.2): a team
// host's connection is PINNED. With (team, build-01) live, a second connection
// presenting the team's token is HELD: refused as pending a manager's
// confirmation, registered nowhere (the live host keeps serving, and nothing
// the second one lists shows), and recorded for the manager's card. A
// manager's confirm arms the replacement: the held host's next attempt is
// taken and the old connection is cancelled, as a replacement is today. Once
// the host is OFFLINE, a connection is taken at once with no confirm.
//
// CONTROL: a PERSONAL host's reconnect still replaces the live one at once.
//
// The team id is a fixture value no constant in the code equals.
func TestASecondTeamHostConnectionWaitsForAManager(t *testing.T) {
	const team = 900233
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

	tok := r.store.mintTeam(t, team, "build-01")
	b1 := &fakeBackend{}
	b1.set("real-host")
	stop1, done1 := r.connect(t, "build-01", tok, b1, true)
	r.online(t, team, "build-01", "real-host")

	// A second connection with the same team token while the host is live.
	_, err := r.rawConnect(t, tok, hello("build-01", true, "retained-copy"))
	if err == nil || !strings.Contains(err.Error(), "pending a manager's confirmation") {
		t.Fatalf("a second connection for a live team host = %v; want it held, pending a manager's confirmation", err)
	}
	if r.svc.Hub().Online(team, "build-01", "retained-copy") || !r.svc.Hub().Online(team, "build-01", "real-host") {
		t.Fatal("the held connection was swapped in: the retained copy is listed, or the real host stopped serving")
	}
	select {
	case err := <-done1:
		t.Fatalf("the live team host's connection ended while the second one was held: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	held := r.svc.Hub().HeldReplacements(team)
	if len(held) != 1 || held[0].Host != "build-01" || held[0].Attempts != 1 || held[0].Confirmed {
		t.Fatalf("the manager's card = %+v; want one unconfirmed hold of build-01 after one attempt", held)
	}
	if got := r.svc.Hub().HeldReplacements(1); len(got) != 0 {
		t.Fatalf("the personal owner sees holds %+v; want none (a hold is its team's)", got)
	}
	// Still held on a second attempt, until a manager confirms.
	if _, err := r.rawConnect(t, tok, hello("build-01", true, "retained-copy")); err == nil {
		t.Fatal("a second attempt was taken with no manager's confirm")
	}

	// A manager confirms: the next attempt is taken, the old one cancelled.
	if !r.svc.Hub().ConfirmReplacement(team, "build-01") {
		t.Fatal("the confirm found nothing held")
	}
	b2 := &fakeBackend{}
	b2.set("moved-host")
	r.connect(t, "build-01", tok, b2, true)
	eventually(t, "the confirmed connection replaces the live team host", func() bool {
		return r.svc.Hub().Online(team, "build-01", "moved-host") && !r.svc.Hub().Online(team, "build-01", "real-host")
	})
	select {
	case <-done1:
	case <-time.After(5 * time.Second):
		t.Fatal("the replaced team host's connection did not end")
	}
	_ = stop1
	// The confirm was one-shot: a further connection is held again.
	if _, err := r.rawConnect(t, tok, hello("build-01", true, "third")); err == nil || !strings.Contains(err.Error(), "pending a manager's confirmation") {
		t.Fatalf("a third connection after the confirm was spent = %v; want it held", err)
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
