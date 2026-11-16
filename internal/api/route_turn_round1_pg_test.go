package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/plugins"
)

// ---------------------------------------------------------------------------
// PR #715 round 1: a route turn the runtime refuses, a delivery turn in
// flight, a process that stops mid-turn, and a hand-back already taken
// (session_routes.go startRouteTurn, deliverRouteAnswers, deliverToChief,
// RunRouteTimeouts).
// ---------------------------------------------------------------------------

// owedAnswer makes one request route on an owner turn that then ends, and
// answers it from the session: its answer is owed to the chief, and no sweep
// has run. It returns the route id and the owner's chat session id.
func (f *routeFixture) owedAnswer(t *testing.T, tok, body string) (string, int64) {
	t.Helper()
	said := f.typed(t, f.owner, "Ask the changelog session one thing.")
	id := f.relayAsOwner(t, 1, "Your owner asks: one thing?")
	if err := agents.NewPG(f.pool).ClearOwnerTurn(f.asOwner, f.chief.ID, said.ID); err != nil {
		t.Fatal(err)
	}
	if out, isErr := f.routeReplyCall(t, tok, routeSID, map[string]any{"route_id": id, "body": body}); isErr {
		t.Fatalf("route_reply: %s", out)
	}
	return id, said.SessionID
}

// endedLongAgo moves route id's end past routeChiefWaitMax.
func (f *routeFixture) endedLongAgo(t *testing.T, id string) {
	t.Helper()
	if _, err := f.pool.Exec(authzSystem(), `UPDATE session_routes SET finished_at = now() - interval '3 hours' WHERE route_id = $1`, id); err != nil {
		t.Fatal(err)
	}
}

// waitIdle waits until no route turn holds the chief's claim.
func (f *routeFixture) waitIdle(t *testing.T) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		if !f.srv.routeTurnInFlight(f.asOwner, f.chief.ID) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("a route turn still holds the chief's claim")
}

// TestARuntimeThatAlwaysRefusesIsAskedOncePerInterval: an owed answer whose
// chief's runtime refuses every turn is offered at most once per sweep
// interval — a refused turn does not wake the sweep, and kicks never run
// passes back to back.
//
// 🔴 REGRESSION: at 69a799a5 a refused turn kicks the sweep, which offers it
// again at once: a loop of attempts (hundreds a second).
func TestARuntimeThatAlwaysRefusesIsAskedOncePerInterval(t *testing.T) {
	f, pod, tok := newRouteAnswerFixture(t)
	f.owedAnswer(t, tok, answerBody(1))
	pod.setBusy(true) // and never freed
	const interval = 300 * time.Millisecond
	const window = 1500 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); f.srv.RunRouteTimeouts(ctx, interval) }()
	f.srv.kickRouteSweep() // a kick, as an answer's arrival makes one
	time.Sleep(window)
	cancel()
	<-done
	f.waitIdle(t)
	// One per tick, plus the kick's, plus one for a tick at the edge.
	most := int(window/interval) + 2
	if n := pod.refusals(); n == 0 || n > most {
		t.Fatalf("the refusing runtime was asked %d time(s) in %s at a %s interval; want 1..%d", n, window, interval, most)
	}
}

// TestARefusedRouteTurnDoesNotWakeTheSweep: the first of the two guards
// against the loop, alone — a turn the runtime refused leaves no kick behind
// (the sweep's own interval offers it again).
//
// 🔴 REGRESSION: at 69a799a5 every refused turn kicks the sweep.
func TestARefusedRouteTurnDoesNotWakeTheSweep(t *testing.T) {
	f, pod, tok := newRouteAnswerFixture(t)
	f.owedAnswer(t, tok, answerBody(1))
	pod.setBusy(true)
	kick := f.srv.routeKickCh()
	for len(kick) > 0 {
		<-kick
	}
	if _, err := f.srv.RouteTimeoutPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	f.waitIdle(t)
	if pod.refusals() != 1 {
		t.Fatalf("control: the runtime was asked %d time(s), want 1", pod.refusals())
	}
	if len(kick) != 0 {
		t.Fatal("a refused route turn woke the sweep")
	}
}

// TestKicksNeverRunRoutePassesBackToBack: the second guard, alone — however
// often something kicks, the sweep runs at most one pass per routeKickMinGap
// (here the interval is long, so only kicks can start a pass).
//
// 🔴 REGRESSION: at 69a799a5 every kick runs a pass at once.
func TestKicksNeverRunRoutePassesBackToBack(t *testing.T) {
	f, pod, tok := newRouteAnswerFixture(t)
	f.owedAnswer(t, tok, answerBody(1))
	pod.setBusy(true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); f.srv.RunRouteTimeouts(ctx, time.Hour) }()
	for end := time.Now().Add(time.Second); time.Now().Before(end); {
		f.srv.kickRouteSweep()
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	f.waitIdle(t)
	// The first kick's pass, and at most one more after routeKickMinGap (2s >
	// the 1s of kicks): one attempt each.
	if n := pod.refusals(); n == 0 || n > 1 {
		t.Fatalf("a second of kicks ran %d attempt(s), want 1", n)
	}
}

// TestRefusalsCountTowardTheWaitForATurn: an answer whose chief's runtime
// keeps refusing it is recorded as a note once it has waited routeChiefWaitMax
// — the refusals count toward the wait — and is owed no more.
//
// 🔴 REGRESSION: at 69a799a5 the wait is read only when no turn can start, so
// a runtime that refuses for ever leaves the answer owed for ever.
func TestRefusalsCountTowardTheWaitForATurn(t *testing.T) {
	f, pod, tok := newRouteAnswerFixture(t)
	id, sessID := f.owedAnswer(t, tok, answerBody(1))
	f.endedLongAgo(t, id)
	pod.setBusy(true)
	// Each pass once the last refused turn is over: no pass overlaps an
	// attempt (one that did would find the claim held — not the refusal).
	for i := 0; i < 3 && f.handBackOwed(t, id) != ""; i++ {
		if _, err := f.srv.RouteTimeoutPass(context.Background()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
		f.waitIdle(t)
	}
	if f.handBackOwed(t, id) != "" {
		t.Fatalf("an answer the runtime refused for %s is still owed (%d refusals)", routeChiefWaitMax, pod.refusals())
	}
	if n := countContaining(f.chatText(t, sessID), answerBody(1)); n != 1 {
		t.Fatalf("the answer is in the chat %d time(s), want 1 (the note)", n)
	}
	if len(pod.taken()) != 0 {
		t.Fatalf("a refusing runtime took %d turn(s)", len(pod.taken()))
	}
}

// TestNoFallbackNoteWhileTheDeliveryTurnIsInFlight: an answer whose turn is
// running is not ALSO written as a fallback note when it passes
// routeChiefWaitMax mid-turn: it ends up in the chat once.
//
// 🔴 REGRESSION: at 69a799a5 a sweep during that turn finds the claim held,
// reads the wait as over and writes a note — then the turn stores its own row.
func TestNoFallbackNoteWhileTheDeliveryTurnIsInFlight(t *testing.T) {
	f, pod, tok := newRouteAnswerFixture(t)
	id, sessID := f.owedAnswer(t, tok, answerBody(1))
	hold := make(chan struct{})
	release := releaseOnce(t, hold)
	inTurn := make(chan string, 1)
	f.srv.ext.Gateway = &blockingPod{answerPod: pod, hold: hold, inTurn: inTurn}
	if _, err := f.srv.RouteTimeoutPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-inTurn:
	case <-time.After(10 * time.Second):
		t.Fatal("the answer's turn never reached the runtime")
	}
	f.endedLongAgo(t, id)
	for i := 0; i < 2; i++ {
		if _, err := f.srv.RouteTimeoutPass(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	release()
	f.waitIdle(t)
	if _, err := f.srv.RouteTimeoutPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := countContaining(f.chatText(t, sessID), answerBody(1)); n != 1 {
		t.Fatalf("the answer is in the chat %d time(s), want 1", n)
	}
	if f.handBackOwed(t, id) != "" {
		t.Fatal("the delivered answer is still owed")
	}
}

// TestAProcessThatStopsMidTurnDoesNotPasteTheAnswerAgain: the answer's row is
// stored BEFORE its turn, so a process that stops mid-turn (a rollout, a
// crash) leaves it on record; once that process's claim is gone, the next
// process's sweep takes the answer instead of pasting it a second time.
//
// The restart is played in one process: the claim released (its TTL, a new
// pod) and the in-process turn gate emptied (a new process has none), while
// the first turn is still blocked in the runtime.
//
// 🔴 REGRESSION: at 69a799a5 the row is stored only after the turn, so the
// next process finds nothing on record and pastes the answer again.
func TestAProcessThatStopsMidTurnDoesNotPasteTheAnswerAgain(t *testing.T) {
	f, pod, tok := newRouteAnswerFixture(t)
	id, _ := f.owedAnswer(t, tok, answerBody(1))
	hold := make(chan struct{})
	release := releaseOnce(t, hold)
	inTurn := make(chan string, 1)
	f.srv.ext.Gateway = &blockingPod{answerPod: pod, hold: hold, inTurn: inTurn}
	if _, err := f.srv.RouteTimeoutPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-inTurn:
	case <-time.After(10 * time.Second):
		t.Fatal("the answer's turn never reached the runtime")
	}
	// The first process is gone: its claim lapsed, its turn gate with it.
	holder, err := f.srv.ext.Agents.KickoffClaimHolder(f.asOwner, f.chief.ID)
	if err != nil || holder == "" {
		t.Fatalf("control: the turn holds no claim (%q, %v)", holder, err)
	}
	if err := f.srv.ext.Agents.ReleaseKickoffClaim(f.asOwner, f.chief.ID, holder); err != nil {
		t.Fatal(err)
	}
	f.srv.turns.leave(f.chief.ID)
	if _, err := f.srv.RouteTimeoutPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if n := turnsCarrying(pod.taken(), answerBody(1)); n != 1 {
		t.Fatalf("the answer was pasted into the chief %d time(s) across the restart, want 1", n)
	}
	if f.handBackOwed(t, id) != "" {
		t.Fatal("the answer on record is still owed after the restart")
	}
	release()
}

// TestATakenHandBackTheRuntimeRefusesIsNeverLost: a hand-back already taken
// (a refused reply whose redraft failed falls to the chief taken) whose turn
// the runtime refuses is recorded as a note — never gone.
//
// 🔴 REGRESSION: at 69a799a5 the refused turn leaves it neither delivered nor
// owed nor noted.
func TestATakenHandBackTheRuntimeRefusesIsNeverLost(t *testing.T) {
	f, pod, tok := newRouteAnswerFixture(t)
	id, sessID := f.owedAnswer(t, tok, answerBody(1))
	r := f.routeState(t, id)
	if _, _, ok, err := f.srv.ext.Routes.TakeHandBack(f.asOwner, id); err != nil || !ok {
		t.Fatalf("control: take the hand-back: %v %v", ok, err)
	}
	pod.setBusy(true)
	const text = "[muster] Route test: its reply was refused and could not be redrafted, so the message is back with you."
	f.srv.deliverToChief(f.srv.routePrincipal(context.Background(), f.owner), r, text, true)
	f.waitIdle(t)
	time.Sleep(100 * time.Millisecond)
	if n := countContaining(f.chatText(t, sessID), text); n != 1 {
		t.Fatalf("a taken hand-back the runtime refused is in the chat %d time(s), want 1 (a note)", n)
	}
	if r.State != plugins.RouteRelayed {
		t.Fatalf("control: the route is %s", r.State)
	}
	if strings.Contains(strings.Join(pod.taken(), "\n"), text) {
		t.Fatal("control: the refusing runtime took the hand-back")
	}
}
