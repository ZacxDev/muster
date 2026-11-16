package api

import (
	"context"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/plugins"
)

// ---------------------------------------------------------------------------
// RACING DELIVERIES OF ONE THING OWED TO A CHIEF MAKE ONE TURN (issue #732),
// on #715's two other startRouteTurn paths: a request route's answer
// (deliverRouteAnswers) and an ended route's hand-back (deliverRouteHandBack →
// deliverToChief). The batch digest's race is route_many_race_pg_test.go.
//
// Each caller checks "still owed, not on record" BEFORE startRouteTurn wins the
// chief's delivery claim, so 8 deliveries are forced past that check together
// (barrierAgents holds the chat read until all 8 arrive) and then QUEUED on the
// claim: each wins it in turn, after the previous one's turn ended and took
// what it carried. Only the re-check startRouteTurn runs under the claim
// (owed) stops the second turn.
// ---------------------------------------------------------------------------

// raceRouteDeliveries runs deliver racers times at once, every one past the
// "on record?" chat read before any delivers and each winning the chief's
// claim in turn (barrierAgents, queued), then waits for every route turn to
// end and puts the plain agent store back.
func (f *routeFixture) raceRouteDeliveries(t *testing.T, racers int, deliver func(ctx context.Context)) {
	t.Helper()
	plain := f.srv.ext.Agents
	barrier := newBarrierAgents(plain, racers)
	barrier.queue = true
	f.srv.ext.Agents = barrier
	octx := f.srv.routePrincipal(context.Background(), f.owner)
	var wg sync.WaitGroup
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			deliver(octx)
		}()
	}
	wg.Wait()
	f.waitRouteTurns(t)
	barrier.mu.Lock()
	reached := barrier.seen
	barrier.mu.Unlock()
	f.srv.ext.Agents = plain
	if reached != racers {
		t.Fatalf("instrument: %d of %d racers reached the record check; the race was not run", reached, racers)
	}
}

// TestRacingDeliveriesOfAnAnswerMakeOneTurn: 8 deliveries of one request
// route's owed answer racing make exactly ONE turn, recorded once, and the
// answer is no longer owed.
func TestRacingDeliveriesOfAnAnswerMakeOneTurn(t *testing.T) {
	const racers = 8
	f, pod, tok := newRouteAnswerFixture(t)
	id, chat := f.owedAnswer(t, tok, answerBody(1))
	octx := f.srv.routePrincipal(context.Background(), f.owner)
	r, err := f.srv.ext.Routes.Route(octx, id)
	if err != nil {
		t.Fatal(err)
	}
	if f.handBackOwed(t, id) == "" {
		t.Fatal("control: the answer is not owed; the race would deliver nothing")
	}
	f.raceRouteDeliveries(t, racers, func(ctx context.Context) {
		f.srv.deliverRouteAnswers(ctx, chat, []plugins.Route{r})
	})
	if n := turnsCarrying(pod.taken(), answerBody(1)); n != 1 {
		t.Fatalf("%d answer turns from %d racing deliveries, want 1: %q", n, racers, pod.taken())
	}
	if n := countContaining(f.chatText(t, chat), answerBody(1)); n != 1 {
		t.Fatalf("the answer is in the chat %d time(s), want 1", n)
	}
	if hb := f.handBackOwed(t, id); hb != "" {
		t.Fatalf("the delivered answer is still owed: %q", hb)
	}
}

// TestRacingDeliveriesOfAHandBackMakeOneTurn: 8 deliveries of one timed-out
// route's hand-back racing make exactly ONE turn, recorded once, and the
// hand-back is taken.
func TestRacingDeliveriesOfAHandBackMakeOneTurn(t *testing.T) {
	const racers = 8
	f, pod, _ := newRouteAnswerFixture(t)
	said := f.typed(t, f.owner, "Ask the changelog session something.")
	id := f.relayAsOwner(t, 1, "Your owner asks: anything new?")
	if err := agents.NewPG(f.pool).ClearOwnerTurn(f.asOwner, f.chief.ID, said.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(authzSystem(), `UPDATE session_routes SET timeout_at = now() - interval '1 second' WHERE route_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	octx := f.srv.routePrincipal(context.Background(), f.owner)
	r, timedOut, err := f.srv.ext.Routes.TimeOutRoute(octx, id)
	if err != nil || !timedOut {
		t.Fatalf("time out the route: %v (timed out %v)", err, timedOut)
	}
	if f.handBackOwed(t, id) == "" {
		t.Fatal("control: the timed-out route owes no hand-back; the race would deliver nothing")
	}
	f.raceRouteDeliveries(t, racers, func(ctx context.Context) {
		f.srv.deliverRouteHandBack(ctx, r)
	})
	head := "[muster] Route " + id + " "
	if n := turnsCarrying(pod.taken(), head); n != 1 {
		t.Fatalf("%d hand-back turns from %d racing deliveries, want 1: %q", n, racers, pod.taken())
	}
	if n := countContaining(f.chatText(t, said.SessionID), head); n != 1 {
		t.Fatalf("the hand-back is in the chat %d time(s), want 1", n)
	}
	if hb := f.handBackOwed(t, id); hb != "" {
		t.Fatalf("the delivered hand-back is still owed: %q", hb)
	}
}
