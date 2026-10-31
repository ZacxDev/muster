package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// THROWAWAY CHECK LANES (X-Muster-CC-Ephemeral: 1, lanes.go): a chief's stall
// check runs in a lane whose CLI is stopped as soon as its turn is over, and
// which never takes an operator lane's slot.

// turnPane is a lane terminal whose CLI starts (and sends SessionStart) at once
// and answers a paste like the scripted CLI (fakecli_test.go): a whole turn,
// success or failure, through the real runTurn.
type turnPane struct {
	stillPane
	cli *scriptedCLI
}

func (p *turnPane) Start(context.Context, string, []string) (paneRef, error) {
	go p.cli.srv.onHook(hookEvent{Event: "SessionStart", SessionID: fixtureSessionID, Source: "startup"})
	return fakeRef, nil
}
func (p *turnPane) Paste(ctx context.Context, ref paneRef, text string) error {
	return p.cli.Paste(ctx, ref, text)
}
func (p *turnPane) Enter(ctx context.Context, ref paneRef) error { return p.cli.Enter(ctx, ref) }
func (p *turnPane) Alive(ctx context.Context, ref paneRef) error { return p.cli.Alive(ctx, ref) }

// laneRig is a lanes set whose every terminal is recorded by its tmux target,
// behind a ccd server, on a fake clock (lane recency is deterministic).
type laneRig struct {
	l   *lanes
	s   *server
	clk *fakeClock
	mu  sync.Mutex
	pan map[string]*stillPane // by tmux session name, latest
}

func newLaneRig(t *testing.T, max int) *laneRig {
	t.Helper()
	r := &laneRig{l: testLanes(t, max), pan: map[string]*stillPane{},
		clk: &fakeClock{t: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}}
	r.l.now = r.clk.now
	r.l.startTimeout = 300 * time.Millisecond
	mk := r.l.newServer
	r.l.newServer = func(term terminal) *server {
		s := mk(term)
		s.clock = r.clk.now
		return s
	}
	r.l.newTerm = func(target string) laneTerminal {
		p := &stillPane{}
		r.mu.Lock()
		r.pan[target] = p
		r.mu.Unlock()
		return p
	}
	r.s = newServer(serverConfig{Bearer: "b", ConfigDir: r.l.configDir}, nil, newAuthTracker())
	r.s.lanes = r.l // testLanes' cleanup stops every lane and waits for it
	return r
}

// scripted makes every lane started from now on a turnPane running script.
func (r *laneRig) scripted(t *testing.T, script cliScript) {
	cfg := r.l.configDir
	r.l.newTerm = func(target string) laneTerminal {
		p := &turnPane{cli: &scriptedCLI{t: t, dir: cfg, script: script}}
		r.mu.Lock()
		r.pan[target] = &p.stillPane
		r.mu.Unlock()
		return p
	}
	r.l.newServer = func(term terminal) *server {
		s := newServer(serverConfig{Bearer: "b", ConfigDir: cfg, SubmitTimeout: 2 * time.Second,
			TurnTimeout: 5 * time.Second, TranscriptGrace: 200 * time.Millisecond}, term, newAuthTracker())
		term.(*turnPane).cli.srv = s
		return s
	}
}

func (r *laneRig) pane(key string) *stillPane {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pan["cc-"+laneID(key)]
}

// operator starts key's operator lane and lets its turn go: an idle lane no
// turn holds. Each is a minute more recent than the last.
func (r *laneRig) operator(t *testing.T, key string) *lane {
	t.Helper()
	r.clk.add(time.Minute)
	ln, f := r.l.acquire(context.Background(), key, "")
	if f != nil {
		t.Fatalf("operator lane %s: %v", key, f)
	}
	r.l.release(ln)
	started(t, ln)
	return ln
}

// started waits until ln's `tmux new-session` has returned (lane.startReturned)
// — the lane's CLI is up, as an idle lane's is.
//
// 🔴 WITHOUT IT A LANE IS EVICTED MID-START WHENEVER ITS run GOROUTINE IS SLOW
// TO BE SCHEDULED: acquire returns before that goroutine calls Start, so a lane
// stopped straight after is stopped while starting (lane.stoppedMidStart) and,
// by design, killed a SECOND time once Start returns (lane.killLateSession).
// Its pane then counts 2 kills, not 1, depending on goroutine scheduling — CI
// flaked exactly so. Eviction mid-start is a different case from evicting an
// idle lane; tests of the latter must wait for the start.
func started(t *testing.T, ln *lane) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		ln.startMu.Lock()
		done := ln.startReturned
		ln.startMu.Unlock()
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("lane %s (%s): its tmux new-session never returned", ln.id, ln.key)
		}
		time.Sleep(time.Millisecond)
	}
}

// checkTurn posts one turn for key marked as a throwaway check and returns the
// status and body.
func (r *laneRig) checkTurn(t *testing.T, key string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"input":"is this session stalled?","stream":false}`))
	req.Header.Set("Authorization", "Bearer b")
	req.Header.Set(headerCCSession, key)
	req.Header.Set(headerCCEphemeral, "1")
	w := httptest.NewRecorder()
	r.s.gatewayHandler().ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// 🔴 (1) A CHECK LANE'S CLI IS STOPPED, AND ITS SLOT FREED, WHEN ITS TURN ENDS —
// whatever the turn's outcome: a reply, a failed turn, and a lane whose CLI
// never reached its prompt (refused before any paste) alike.
func TestACheckLaneIsStoppedWhenItsTurnEnds(t *testing.T) {
	for _, c := range []struct {
		name     string
		script   *cliScript
		wantCode int
	}{
		{"success", &cliScript{transcript: fixture(t, "turn_success_tools_2.1.289.jsonl"), stopEvent: "Stop"}, http.StatusOK},
		{"failed turn", &cliScript{transcript: fixture(t, "turn_auth_failed_2.1.296.jsonl"), stopEvent: "StopFailure"}, http.StatusBadGateway},
		{"never reached its prompt", nil, http.StatusServiceUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newLaneRig(t, 3)
			if c.script != nil {
				r.scripted(t, *c.script)
			}
			key := "agent:chief:webchat:nudge:0123456789abcdef"
			code, body := r.checkTurn(t, key)
			if code != c.wantCode {
				t.Fatalf("precondition: the check's turn answered %d %s, want %d", code, body, c.wantCode)
			}
			if ln := r.l.get(laneID(key)); ln != nil {
				t.Errorf("CHECK LANE KEPT: after its turn (%d) the check's lane is still in the set (reserved=%d)", code, reservedOf(r.l, ln.id))
			}
			if p := r.pane(key); p == nil {
				t.Error("precondition: the check never started a CLI")
			} else if n := p.kills(); n != 1 {
				t.Errorf("CHECK CLI LEFT RUNNING: its tmux session was killed %d times after its turn, want 1", n)
			}
		})
	}
}

// (1, defended) Two turns holding one check lane — keys are unique per
// decision, so this should not happen — stop it only when the LAST releases
// it, and a turn for the key after that gets a fresh lane: never one that is
// half-killed.
func TestACheckLaneHeldTwiceIsStoppedByTheLastReleaseOnly(t *testing.T) {
	r := newLaneRig(t, 3)
	ctx := context.Background()
	key := "agent:chief:webchat:nudge:aaaaaaaaaaaaaaaa"
	a, f := r.l.acquireIn(ctx, key, "", "", laneMode{ephemeral: true})
	if f != nil {
		t.Fatal(f)
	}
	if b, f := r.l.acquireIn(ctx, key, "", "", laneMode{ephemeral: true}); f != nil || b != a {
		t.Fatalf("the same check key again: %v %v", b, f)
	}
	r.l.release(a)
	if r.l.get(a.id) != a || r.pane(key).kills() != 0 {
		t.Fatalf("one of two holders released: in set=%v kills=%d; want kept and running", r.l.get(a.id) == a, r.pane(key).kills())
	}
	r.l.release(a)
	if r.l.get(a.id) != nil || r.pane(key).kills() != 1 {
		t.Fatalf("both released: in set=%v kills=%d; want stopped", r.l.get(a.id) != nil, r.pane(key).kills())
	}
	again, f := r.l.acquireIn(ctx, key, "", "", laneMode{ephemeral: true})
	if f != nil {
		t.Fatal(f)
	}
	if again == a || r.l.get(a.id) != again || r.pane(key).kills() != 0 {
		t.Fatalf("a turn after the stop: fresh=%v in set=%v new pane kills=%d; want a fresh, running lane",
			again != a, r.l.get(a.id) == again, r.pane(key).kills())
	}
	r.l.release(again)
}

// 🔴 (2) A CHECK IS NEVER THE REASON AN OPERATOR LANE IS STOPPED. At the cap with
// only operator lanes — idle ones, and one stuck at a permission prompt past
// stuckAfter, both of which an operator session WOULD stop — a check is refused
// with the typed 503 capacity, and every operator lane keeps its CLI.
func TestACheckAtTheCapWithOnlyOperatorLanesIsRefusedAndStopsNothing(t *testing.T) {
	t.Run("idle operator lanes", func(t *testing.T) {
		r := newLaneRig(t, 2)
		a, b := r.operator(t, "sess-a"), r.operator(t, "sess-b")
		code, body := r.checkTurn(t, "agent:chief:webchat:nudge:1111111111111111")
		if code != http.StatusServiceUnavailable || !strings.Contains(body, `"type":"`+failCapacity+`"`) {
			t.Errorf("a check at the cap: %d %s; want 503 %s", code, body, failCapacity)
		}
		for _, ln := range []*lane{a, b} {
			if r.l.get(ln.id) != ln || r.pane(ln.key).kills() != 0 {
				t.Errorf("OPERATOR LANE STOPPED FOR A CHECK: %s in set=%v kills=%d", ln.key, r.l.get(ln.id) == ln, r.pane(ln.key).kills())
			}
		}
		if r.pane("agent:chief:webchat:nudge:1111111111111111") != nil {
			t.Error("the refused check started a CLI")
		}
	})
	t.Run("an operator lane stuck at a permission prompt", func(t *testing.T) {
		r := newLaneRig(t, 1)
		r.l.stuckAfter = 10 * time.Minute
		a := r.operator(t, "sess-a")
		atPrompt(a)
		r.clk.add(time.Hour)
		r.l.mu.Lock()
		ev, stuck := r.l.pickEvictLocked(false)
		r.l.mu.Unlock()
		if ev != a || !stuck {
			t.Fatalf("precondition: an operator session would stop the stuck lane (got %v, stuck=%v)", ev, stuck)
		}
		code, body := r.checkTurn(t, "agent:chief:webchat:nudge:2222222222222222")
		if code != http.StatusServiceUnavailable || !strings.Contains(body, `"type":"`+failCapacity+`"`) {
			t.Errorf("a check at the cap beside a stuck operator lane: %d %s; want 503 %s", code, body, failCapacity)
		}
		if r.l.get(a.id) != a || r.pane(a.key).kills() != 0 {
			t.Errorf("STUCK OPERATOR LANE STOPPED FOR A CHECK: in set=%v kills=%d", r.l.get(a.id) == a, r.pane(a.key).kills())
		}
	})
}

// (3) A check MAY stop another check lane that no turn holds. With checks
// stopped on release none is left idle, so the one here is planted (its
// reservation dropped by hand); the operator lane beside it, more recently
// used or not, is untouched. /healthz names the check lane as ephemeral.
func TestACheckMayStopAnotherCheckLane(t *testing.T) {
	r := newLaneRig(t, 2)
	ctx := context.Background()
	old, f := r.l.acquireIn(ctx, "agent:chief:webchat:nudge:3333333333333333", "", "", laneMode{ephemeral: true})
	if f != nil {
		t.Fatal(f)
	}
	started(t, old)
	r.l.mu.Lock()
	old.reserved = 0
	r.l.mu.Unlock()
	a := r.operator(t, "sess-a")
	r.l.mu.Lock()
	a.lastUsed = old.lastUsed.Add(-time.Hour) // the operator lane is the LRU one
	r.l.mu.Unlock()
	eph := map[string]bool{}
	for _, h := range r.l.health() {
		eph[h.ID] = h.Ephemeral
	}
	if !eph[old.id] || eph[a.id] {
		t.Errorf("/healthz ephemeral: %v; want the check lane %s only", eph, old.id)
	}

	next, f := r.l.acquireIn(ctx, "agent:chief:webchat:nudge:4444444444444444", "", "", laneMode{ephemeral: true})
	if f != nil {
		t.Fatalf("a check at the cap beside an idle check lane: %v; want that lane stopped", f)
	}
	r.l.release(next)
	if r.l.get(old.id) != nil || r.pane(old.key).kills() != 1 {
		t.Errorf("the idle check lane: in set=%v kills=%d; want stopped", r.l.get(old.id) != nil, r.pane(old.key).kills())
	}
	if r.l.get(a.id) != a || r.pane(a.key).kills() != 0 {
		t.Errorf("the operator lane: in set=%v kills=%d; want untouched", r.l.get(a.id) == a, r.pane(a.key).kills())
	}
}

// (4) Operator lanes at the cap behave as before: the least recently used idle
// lane is stopped — an idle check lane counts like any other — and operator
// lanes are kept after their turns. An INVARIANT guard (this PR must not
// change it).
func TestAnOperatorLaneAtTheCapStillStopsTheLeastRecentlyUsedIdleLane(t *testing.T) {
	r := newLaneRig(t, 2)
	ctx := context.Background()
	a, b := r.operator(t, "sess-a"), r.operator(t, "sess-b")
	r.operator(t, "sess-c")
	if r.l.get(a.id) != nil || r.pane(a.key).kills() != 1 || r.l.get(b.id) != b {
		t.Fatalf("an operator session at the cap: a in set=%v kills=%d, b in set=%v; want a (LRU) stopped, b kept",
			r.l.get(a.id) != nil, r.pane(a.key).kills(), r.l.get(b.id) == b)
	}
	if r.pane(b.key).kills() != 0 || r.pane("sess-c").kills() != 0 {
		t.Fatal("an operator lane's CLI was stopped after its own turn")
	}

	// An idle check lane (planted, see TestACheckMayStopAnotherCheckLane) that
	// is the least recently used is the one an operator session stops.
	r2 := newLaneRig(t, 2)
	chk, f := r2.l.acquireIn(ctx, "agent:chief:webchat:nudge:5555555555555555", "", "", laneMode{ephemeral: true})
	if f != nil {
		t.Fatal(f)
	}
	started(t, chk)
	r2.l.mu.Lock()
	chk.reserved = 0
	r2.l.mu.Unlock()
	e := r2.operator(t, "sess-e")
	r2.operator(t, "sess-f")
	if r2.l.get(chk.id) != nil || r2.pane(chk.key).kills() != 1 || r2.l.get(e.id) != e {
		t.Fatalf("an operator session beside an older idle check lane: check in set=%v kills=%d, e in set=%v; want the check stopped, e kept",
			r2.l.get(chk.id) != nil, r2.pane(chk.key).kills(), r2.l.get(e.id) == e)
	}
}

// A key's lane is a check lane or an operator lane for its life, like its
// model: the other kind of turn for a running lane is refused.
func TestWhetherALaneIsACheckIsFixed(t *testing.T) {
	r := newLaneRig(t, 3)
	r.operator(t, "sess-a")
	if _, f := r.l.acquireIn(context.Background(), "sess-a", "", "", laneMode{ephemeral: true}); f == nil || f.Type != failBadRequest {
		t.Fatalf("a check turn for a running operator lane: %v; want %s", f, failBadRequest)
	}
}
