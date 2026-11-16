package api

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/plugins"
)

// ---------------------------------------------------------------------------
// RACING DELIVERIES OF ONE THING OWED TO A CHIEF DELIVER IT ONCE (issue #732),
// on all three startRouteTurn paths: a request route's answer
// (deliverRouteAnswers), an ended route's hand-back (deliverRouteHandBack →
// deliverToChief) and a route_many batch's digest (deliverRouteBatch; its
// first racer tests are route_many_race_pg_test.go).
//
// Each caller checks "still owed, not on record" BEFORE startRouteTurn wins the
// chief's delivery claim, so 8 deliveries are forced past that check together
// (barrierAgents holds the chat read until all 8 arrive) and then QUEUED on the
// claim: each wins it in turn, after the previous one's turn ended and took
// what it carried. Only the re-check startRouteTurn runs under the claim
// (owed) stops a second delivery. The deployment runs no concurrent
// deliveries today (one sweep goroutine, one replica): this is the defence for
// when it does.
//
// Each path runs in four shapes, so each part of its re-check is the ONLY
// thing standing in one of them:
//
//   - plain: the first racer delivers and takes; every later one must not.
//   - waited: the item ended more than routeChiefWaitMax ago, so a caller that
//     believed "cannot take it now" would write its fallback NOTE — what
//     startRouteTurn reporting TRUE for "nothing left to deliver" prevents.
//   - take fails: the first racer's take (TakeHandBack / FinishBatchDigest)
//     fails once, so the item is STILL OWED but ON RECORD: only the re-check's
//     record read stops a second turn, and it must take the item itself.
//   - blind: once the first racer's turn is over every chat read fails (reads
//     as "not on record"): only the re-check's "still owed" read stops it.
// ---------------------------------------------------------------------------

// routeRaceShape is how raceRouteDeliveries bends the stores for one shape.
type routeRaceShape struct {
	name      string
	waited    bool // the item ended longer than routeChiefWaitMax ago
	takeFails bool // the first take of the item fails
	blind     bool // chat reads fail once the first racer's turn is over
}

var routeRaceShapes = []routeRaceShape{
	{name: "plain"},
	{name: "waited", waited: true},
	{name: "take fails", takeFails: true},
	{name: "blind", blind: true},
}

// failFirstTake is a route store whose first TakeHandBack or
// FinishBatchDigest fails without doing anything.
type failFirstTake struct {
	plugins.RouteStore
	mu     sync.Mutex
	failed bool
}

var errInjectedTake = errors.New("failFirstTake: injected take failure")

func (s *failFirstTake) first() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed {
		return false
	}
	s.failed = true
	return true
}

func (s *failFirstTake) TakeHandBack(ctx context.Context, routeID string) (plugins.Route, string, bool, error) {
	if s.first() {
		return plugins.Route{}, "", false, errInjectedTake
	}
	return s.RouteStore.TakeHandBack(ctx, routeID)
}

func (s *failFirstTake) FinishBatchDigest(ctx context.Context, batchID string) error {
	if s.first() {
		return errInjectedTake
	}
	return s.RouteStore.FinishBatchDigest(ctx, batchID)
}

// raceRouteDeliveries runs deliver racers times at once, every one past the
// "on record?" chat read before any delivers and each winning the chief's
// claim in turn (barrierAgents, queued), with the stores bent for shape; then
// waits for every route turn to end and puts the plain stores back.
func (f *routeFixture) raceRouteDeliveries(t *testing.T, racers int, shape routeRaceShape, deliver func(ctx context.Context)) {
	t.Helper()
	plain, plainRoutes := f.srv.ext.Agents, f.srv.ext.Routes
	barrier := newBarrierAgents(plain, racers)
	barrier.queue = true
	barrier.blindAfterRelease = shape.blind
	f.srv.ext.Agents = barrier
	var takes *failFirstTake
	if shape.takeFails {
		takes = &failFirstTake{RouteStore: plainRoutes}
		f.srv.ext.Routes = takes
	}
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
	reached, released := barrier.arrived, barrier.released
	barrier.mu.Unlock()
	f.srv.ext.Agents, f.srv.ext.Routes = plain, plainRoutes
	if reached != racers {
		t.Fatalf("instrument: %d of %d racers reached the record check; the race was not run", reached, racers)
	}
	if shape.blind && !released {
		t.Fatal("instrument: no claim was released, so the chat reads were never blinded")
	}
	if takes != nil {
		takes.mu.Lock()
		failed := takes.failed
		takes.mu.Unlock()
		if !failed {
			t.Fatal("instrument: nothing tried to take the item, so no take failed")
		}
	}
}

// assertDeliveredOnce: marker is in exactly one turn and one chat row of
// session chat, and none of ids still owes a hand-back.
func (f *routeFixture) assertDeliveredOnce(t *testing.T, pod *answerPod, chat int64, marker, what string, ids ...string) {
	t.Helper()
	if n := turnsCarrying(pod.taken(), marker); n != 1 {
		t.Fatalf("%d %s turns from 8 racing deliveries, want 1: %q", n, what, pod.taken())
	}
	if n := countContaining(f.chatText(t, chat), marker); n != 1 {
		t.Fatalf("the %s is in the chat %d time(s), want 1", what, n)
	}
	for _, id := range ids {
		if hb := f.handBackOwed(t, id); hb != "" {
			t.Fatalf("the delivered %s still owes route %s's hand-back: %q", what, id, hb)
		}
	}
}

// TestRacingDeliveriesOfAnAnswerMakeOneTurn: 8 deliveries of one request
// route's owed answer racing make exactly ONE turn and ONE chat row, and the
// answer is no longer owed — in every shape (routeRaceShapes).
func TestRacingDeliveriesOfAnAnswerMakeOneTurn(t *testing.T) {
	for _, shape := range routeRaceShapes {
		t.Run(shape.name, func(t *testing.T) {
			f, pod, tok := newRouteAnswerFixture(t)
			id, chat := f.owedAnswer(t, tok, answerBody(1))
			if shape.waited {
				f.endedLongAgo(t, id)
			}
			octx := f.srv.routePrincipal(context.Background(), f.owner)
			r, err := f.srv.ext.Routes.Route(octx, id)
			if err != nil {
				t.Fatal(err)
			}
			if f.handBackOwed(t, id) == "" {
				t.Fatal("control: the answer is not owed; the race would deliver nothing")
			}
			if shape.waited != routeChiefWaitOver(r, time.Now()) {
				t.Fatalf("control: the answer has waited past routeChiefWaitMax = %v, want %v", !shape.waited, shape.waited)
			}
			f.raceRouteDeliveries(t, 8, shape, func(ctx context.Context) {
				f.srv.deliverRouteAnswers(ctx, chat, []plugins.Route{r})
			})
			f.assertDeliveredOnce(t, pod, chat, answerBody(1), "answer", id)
		})
	}
}

// TestRacingDeliveriesOfAHandBackMakeOneTurn: 8 deliveries of one timed-out
// route's hand-back racing make exactly ONE turn and ONE chat row, and the
// hand-back is taken — in every shape (routeRaceShapes).
func TestRacingDeliveriesOfAHandBackMakeOneTurn(t *testing.T) {
	for _, shape := range routeRaceShapes {
		t.Run(shape.name, func(t *testing.T) {
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
			if _, timedOut, err := f.srv.ext.Routes.TimeOutRoute(octx, id); err != nil || !timedOut {
				t.Fatalf("time out the route: %v (timed out %v)", err, timedOut)
			}
			if shape.waited {
				f.endedLongAgo(t, id)
			}
			r, err := f.srv.ext.Routes.Route(octx, id)
			if err != nil {
				t.Fatal(err)
			}
			if f.handBackOwed(t, id) == "" {
				t.Fatal("control: the timed-out route owes no hand-back; the race would deliver nothing")
			}
			if shape.waited != routeChiefWaitOver(r, time.Now()) {
				t.Fatalf("control: the hand-back has waited past routeChiefWaitMax = %v, want %v", !shape.waited, shape.waited)
			}
			f.raceRouteDeliveries(t, 8, shape, func(ctx context.Context) {
				f.srv.deliverRouteHandBack(ctx, r)
			})
			f.assertDeliveredOnce(t, pod, said.SessionID, "[muster] Route "+id+" ", "hand-back", id)
		})
	}
}

// TestRacingDeliveriesOfABatchInEveryShapeMakeOneDigest: 8 deliveries of one
// owed route_many batch make ONE digest turn and ONE chat row, and its members
// owe nothing, when the first digest's finish fails (take fails) and when the
// record cannot be read after it (blind). The plain shape is
// TestRacingDeliveriesOfABatchMakeOneDigestTurn; a batch has no "waited"
// shape here (its note re-checks the record under ClaimKickoff:
// TestRacingDeliveriesOfABatchToAChiefWithNoTurnWriteOneNote).
func TestRacingDeliveriesOfABatchInEveryShapeMakeOneDigest(t *testing.T) {
	for _, shape := range routeRaceShapes {
		if !shape.takeFails && !shape.blind {
			continue
		}
		t.Run(shape.name, func(t *testing.T) {
			f, pod, tok, res, chat := twoSessionBatch(t)
			for i, sid := range []string{routeSID, otherRouteSID} {
				if out, isErr := f.routeReplyCall(t, tok, sid, map[string]any{"route_id": res.routeOf(t, "mirror:"+sid), "body": answerBody(i + 1)}); isErr {
					t.Fatalf("route_reply %d: %s", i+1, out)
				}
			}
			f.raceRouteDeliveries(t, 8, shape, func(ctx context.Context) {
				f.srv.deliverRouteBatch(ctx, res.BatchID)
			})
			f.assertDeliveredOnce(t, pod, chat, routeBatchHead(res.BatchID)+" ", "digest", res.Routed[0].RouteID, res.Routed[1].RouteID)
		})
	}
}
