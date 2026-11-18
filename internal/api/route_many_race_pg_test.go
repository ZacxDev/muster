package api

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
)

// barrierAgents is the agent store with its chat reads held at a barrier: the
// first n ListChatMessages calls (the caller's "already on record?" check,
// once per racing delivery) each make their read and then wait until all n
// have read, or a second has passed. So every racer has checked the record
// BEFORE any of them writes — the race made certain, not left to the scheduler (without it the
// 8 racers ran one after another and a mutant removing the guard survived).
//
// queue, when set, also QUEUES the chief's session claim: a ClaimKickoff or
// ClaimKickoffForDelivery waits until the previous holder released it, so a
// racer that lost the race to the record still wins the claim later — the
// delivery that arrives just after another finished, which only a re-check
// under the claim stops (the note's in recordBatchDigest, the turn's in
// startRouteTurn). The turn race used to run unqueued, left to the
// scheduler: the code before that re-check existed failed it in only some
// runs (issue #732: 14 of 20; 99 of 200 measured on af368efa); queued, it
// fails every run.
type barrierAgents struct {
	agents.Store
	mu      sync.Mutex
	n, seen int
	arrived int // reads made by the n holders of a place (guarded by mu)
	open    chan struct{}
	queue   bool
	claim   sync.Mutex
	held    bool // the queued claim is held (guarded by claim itself)
	// blindAfterRelease, when set, makes every chat read FAIL once the first
	// queued claim has been released (released): the first racer's turn is
	// over, and every later racer's "on record?" read under the claim cannot
	// see it — a transient read error, which reads as "not yet" — so only the
	// "still owed?" read can stop a second delivery.
	blindAfterRelease bool
	released          bool // guarded by mu
}

func (b *barrierAgents) ClaimKickoff(ctx context.Context, id int64, owner string, ttl time.Duration) (bool, error) {
	return b.queued(func() (bool, error) { return b.Store.ClaimKickoff(ctx, id, owner, ttl) })
}

func (b *barrierAgents) ClaimKickoffForDelivery(ctx context.Context, id int64, owner string, ttl time.Duration) (bool, error) {
	return b.queued(func() (bool, error) { return b.Store.ClaimKickoffForDelivery(ctx, id, owner, ttl) })
}

// queued runs claim, in turn behind the claim's previous holder when queue is set.
func (b *barrierAgents) queued(claim func() (bool, error)) (bool, error) {
	if b.queue {
		b.claim.Lock()
	}
	won, err := claim()
	if b.queue {
		if err != nil || !won {
			b.claim.Unlock()
		} else {
			b.held = true
		}
	}
	return won, err
}

func (b *barrierAgents) ReleaseKickoffClaim(ctx context.Context, id int64, owner string) error {
	err := b.Store.ReleaseKickoffClaim(ctx, id, owner)
	if b.queue && b.held {
		b.held = false
		b.mu.Lock()
		b.released = true
		b.mu.Unlock()
		b.claim.Unlock()
	}
	return err
}

func newBarrierAgents(st agents.Store, n int) *barrierAgents {
	return &barrierAgents{Store: st, n: n, open: make(chan struct{})}
}

func (b *barrierAgents) ListChatMessages(ctx context.Context, sessionID int64) ([]agents.ChatMessage, error) {
	b.mu.Lock()
	if b.blindAfterRelease && b.released {
		b.mu.Unlock()
		return nil, errors.New("barrierAgents: injected chat read failure")
	}
	wait := b.seen < b.n
	if wait {
		b.seen++ // this call holds one of the n places
	}
	b.mu.Unlock()
	// The read is made FIRST and the racer then waits with what it read: a
	// read made after the barrier opened could run after the first racer
	// stored its row, and see it (2 of 30 runs of a mutant survived that way).
	msgs, err := b.Store.ListChatMessages(ctx, sessionID)
	if wait {
		b.mu.Lock()
		b.arrived++
		if b.arrived == b.n {
			close(b.open)
		}
		b.mu.Unlock()
		select {
		case <-b.open:
		case <-time.After(time.Second):
		}
	}
	return msgs, err
}

// racedDigest answers both members of a two-session batch and then has 8
// deliverRouteBatch calls race for its digest — what several sweeps (or
// replicas) offering the same owed batch at once do — every one past the
// "on record?" check before any delivers (barrierAgents). Then it sweeps until
// the digest is on record and its members owe nothing, and a few more times.
// It returns the pod's turns, the owner's chat rows and the batch.
func racedDigest(t *testing.T, noTurn bool) ([]string, []string, routeManyRes) {
	t.Helper()
	const racers = 8
	f, pod, tok, res, chat := twoSessionBatch(t)
	for i, sid := range []string{routeSID, otherRouteSID} {
		if out, isErr := f.routeReplyCall(t, tok, sid, map[string]any{"route_id": res.routeOf(t, "mirror:"+sid), "body": answerBody(i + 1)}); isErr {
			t.Fatalf("route_reply %d: %s", i+1, out)
		}
	}
	if noTurn {
		// A chief that can take no turn (no gateway): the digest is a note.
		f.srv.ext.Gateway = nil
	}
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
			f.srv.deliverRouteBatch(octx, res.BatchID)
		}()
	}
	wg.Wait()
	barrier.mu.Lock()
	reached := barrier.arrived
	barrier.mu.Unlock()
	if reached != racers {
		t.Fatalf("instrument: %d of %d racers reached the record check; the race was not run", reached, racers)
	}
	if !f.sweepUntil(t, func() bool {
		return countContaining(f.chatText(t, chat), routeBatchHead(res.BatchID)+" ") > 0 &&
			f.handBackOwed(t, res.Routed[0].RouteID) == "" && f.handBackOwed(t, res.Routed[1].RouteID) == ""
	}) {
		t.Fatal("the digest was never delivered and its members' hand-backs cleared")
	}
	f.sweeps(t, 4)
	f.srv.ext.Agents = plain
	return pod.taken(), f.chatText(t, chat), res
}

// TestABatchIsLeftAloneWhileItsDigestTurnIsInFlight: once the digest's turn
// has started (its row stored before the turn, as #715 stores every route
// turn's), a sweep running meanwhile neither clears the members' hand-backs —
// the turn may still be refused and its row removed, and the digest would be
// lost — nor writes a second copy; once the turn ends, the hand-backs are
// cleared and there is one digest.
func TestABatchIsLeftAloneWhileItsDigestTurnIsInFlight(t *testing.T) {
	f, pod, tok, res, chat := twoSessionBatch(t)
	hold := make(chan struct{})
	release := releaseOnce(t, hold)
	inTurn := make(chan string, 1)
	f.srv.ext.Gateway = &blockingPod{answerPod: pod, hold: hold, inTurn: inTurn}
	for i, sid := range []string{routeSID, otherRouteSID} {
		if out, isErr := f.routeReplyCall(t, tok, sid, map[string]any{"route_id": res.routeOf(t, "mirror:"+sid), "body": answerBody(i + 1)}); isErr {
			t.Fatalf("route_reply %d: %s", i+1, out)
		}
	}
	f.sweeps(t, 1)
	select {
	case <-inTurn:
	case <-time.After(10 * time.Second):
		t.Fatal("the digest's turn never reached the runtime")
	}
	f.sweeps(t, 3) // sweeps while the digest's turn is in flight
	for _, r := range res.Routed {
		if f.handBackOwed(t, r.RouteID) == "" {
			t.Fatalf("member %s's hand-back was cleared while the digest's turn was still in flight", r.RouteID)
		}
	}
	release()
	if !f.sweepUntil(t, func() bool {
		return f.handBackOwed(t, res.Routed[0].RouteID) == "" && f.handBackOwed(t, res.Routed[1].RouteID) == ""
	}) {
		t.Fatal("the hand-backs were never cleared after the digest's turn")
	}
	if n := countContaining(f.chatText(t, chat), routeBatchHead(res.BatchID)+" "); n != 1 {
		t.Fatalf("the digest is in the chat %d time(s), want 1", n)
	}
}

// TestRacingDeliveriesOfABatchMakeOneDigestTurn: 8 deliveries of one owed
// batch racing make exactly ONE digest turn, recorded once — with the chief's
// delivery claim queued (barrierAgents.queue), so every racer, already past
// the record check, wins the claim in turn AFTER the first one's turn ended:
// only startRouteTurn's re-check under the claim stops the second turn.
func TestRacingDeliveriesOfABatchMakeOneDigestTurn(t *testing.T) {
	turns, chat, res := racedDigest(t, false)
	if n := countContaining(turns, routeBatchHead(res.BatchID)+" "); n != 1 {
		t.Fatalf("%d digest turns from 8 racing deliveries, want 1: %q", n, turns)
	}
	if n := countContaining(chat, routeBatchHead(res.BatchID)+" "); n != 1 {
		t.Fatalf("the digest is in the chat %d time(s), want 1", n)
	}
}

// TestRacingDeliveriesOfABatchToAChiefWithNoTurnWriteOneNote: the same race
// on the note fallback (a chief that can take no turn) writes ONE note — with
// the chief's session claim queued (barrierAgents.queue), so every racer
// takes it in turn and must find the note already on record.
func TestRacingDeliveriesOfABatchToAChiefWithNoTurnWriteOneNote(t *testing.T) {
	turns, chat, res := racedDigest(t, true)
	if len(turns) != 0 {
		t.Fatalf("a chief with no gateway took %d turn(s)", len(turns))
	}
	if n := countContaining(chat, routeBatchHead(res.BatchID)+" "); n != 1 {
		t.Fatalf("the digest note is in the chat %d time(s) after 8 racing deliveries, want 1", n)
	}
}
