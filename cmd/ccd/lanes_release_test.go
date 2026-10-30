package main

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"
)

// 🔴 A CHECK LANE'S RELEASE STOPS ONLY THE LANE IT HOLDS (lanes.release,
// `l.set[ln.id] == ln`). A check lane's CLI that exits on its own is dropped and
// killed by lane.run while its turn still holds it; a retry for the same key
// then starts a NEW lane under the same id and tmux name. When the first turn's
// holder finally releases, the set's lane for that id is the replacement — and
// the replacement must stay in the set with its session alive. The session is
// killed BY NAME, so a kill through the old lane's terminal is a kill of the
// replacement's CLI: the kill count is summed over both terminals.
func TestACheckLaneReleaseNeverStopsItsReplacement(t *testing.T) {
	l := testLanes(t, 2)
	var mu sync.Mutex
	var panes []*losablePane
	l.newTerm = func(string) laneTerminal {
		p := &losablePane{lost: make(chan struct{})}
		mu.Lock()
		panes = append(panes, p)
		mu.Unlock()
		return p
	}
	ctx := context.Background()
	key := "agent:chief:webchat:nudge:feedfacefeedface"

	a, f := l.acquireIn(ctx, key, "", "", true)
	if f != nil {
		t.Fatal(f)
	}
	// A's CLI exits while A's turn still holds it: lane.run drops A and kills
	// cc-<id> (the positive control for the kill counting below).
	close(panes[0].lost)
	waitDone(t, a)
	if l.get(a.id) != nil || panes[0].kills() != 1 {
		t.Fatalf("precondition: A's CLI ended: in set=%v kills=%d, want dropped and killed once", l.get(a.id) != nil, panes[0].kills())
	}
	// The retry for the same key starts lane B: same id, same tmux name.
	b, f := l.acquireIn(ctx, key, "", "", true)
	if f != nil {
		t.Fatal(f)
	}
	if b == a || b.id != a.id {
		t.Fatalf("precondition: the retry got %p (id %s), want a new lane under id %s", b, b.id, a.id)
	}
	t.Cleanup(func() { l.release(b) })
	killsByName := func() int { return panes[0].kills() + panes[1].kills() }
	before := killsByName()

	// A's holder releases now.
	l.release(a)
	if got := l.get(a.id); got != b {
		t.Fatalf("A's release took the REPLACEMENT out of the set (set holds %p, want B %p)", got, b)
	}
	if n := killsByName() - before; n != 0 {
		t.Fatalf("A's release killed session cc-%s %d time(s) — by name, that is B's CLI", a.id, n)
	}
}

// lateStartPane is a lane terminal whose `tmux new-session` returns only once
// gate is closed and, like tmux's server, finishes creating the session
// whatever became of the caller's context. alive is whether that session
// exists now.
type lateStartPane struct {
	stillPane
	gate    chan struct{}
	smu     sync.Mutex
	created bool
	alive   bool
}

func (p *lateStartPane) Start(context.Context, string, []string) (paneRef, error) {
	<-p.gate
	p.smu.Lock()
	p.created, p.alive = true, true
	p.smu.Unlock()
	return fakeRef, nil
}

func (p *lateStartPane) KillSession(ctx context.Context) error {
	p.smu.Lock()
	p.alive = false
	p.smu.Unlock()
	return p.stillPane.KillSession(ctx)
}

func (p *lateStartPane) state() (created, alive bool) {
	p.smu.Lock()
	defer p.smu.Unlock()
	return p.created, p.alive
}

// (killLateSession, defended) The late kill is BY NAME, so it never runs while
// a replacement lane holds that name: a retry for the key that started lane B
// before A's slow new-session returned owns cc-<id>, and A's late kill would be
// a kill of B's CLI.
func TestALateSessionKillSparesAReplacement(t *testing.T) {
	l := testLanes(t, 2)
	late := &lateStartPane{gate: make(chan struct{})}
	var terms []laneTerminal
	l.newTerm = func(string) laneTerminal {
		if len(terms) == 0 {
			terms = append(terms, late)
			return late
		}
		p := &stillPane{}
		terms = append(terms, p)
		return p
	}
	ctx := context.Background()
	key := "agent:chief:webchat:nudge:5ca1ab1e5ca1ab1e"
	a, f := l.acquireIn(ctx, key, "", "", true)
	if f != nil {
		t.Fatal(f)
	}
	l.release(a) // stopped while its new-session is still running
	b, f := l.acquireIn(ctx, key, "", "", true)
	if f != nil {
		t.Fatal(f)
	}
	t.Cleanup(func() { l.release(b) })
	if late.kills() != 1 {
		t.Fatalf("precondition: A's stop killed %d time(s), want 1", late.kills())
	}
	close(late.gate)
	waitDone(t, a)
	if n := late.kills(); n != 1 {
		t.Fatalf("A's late kill ran with replacement B holding cc-%s (A's kills %d, want 1): by name, that kills B's CLI", a.id, n)
	}
	if l.get(a.id) != b {
		t.Fatal("B is no longer the set's lane")
	}
}

// 🔴 A CHECK STOPPED WHILE ITS CLI'S new-session IS STILL RUNNING LEAVES NO
// SESSION BEHIND (lane.killLateSession). The check's turn gives up waiting
// (waitStarted, here 300ms; 60s live) and its release stops the lane: stop's
// kill-session finds nothing yet, then new-session returns having created
// cc-<id> — which, a check's key never being used again, nothing would kill.
func TestACheckStoppedMidStartLeavesNoSession(t *testing.T) {
	r := newLaneRig(t, 3)
	p := &lateStartPane{gate: make(chan struct{})}
	r.l.newTerm = func(string) laneTerminal { return p }
	key := "agent:chief:webchat:nudge:0badc0de0badc0de"

	code, body := r.checkTurn(t, key)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("precondition: the check whose CLI never started answered %d %s, want 503 not_ready", code, body)
	}
	if r.l.get(laneID(key)) != nil {
		t.Fatal("precondition: the refused check's lane is still in the set")
	}
	close(p.gate) // new-session returns now, having created the session
	deadline := time.Now().Add(3 * time.Second)
	for {
		created, alive := p.state()
		if created && !alive {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("ORPHAN CLI: after the stopped check's new-session returned, its session created=%v alive=%v; want created and then killed", created, alive)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
