package api

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/plugins"
)

// ---------------------------------------------------------------------------
// PR #715 round 2: a refused route turn releases its claim once, leaves its
// session untouched, and writes no second copy when its row cannot be
// removed; and the second in-flight checks (deliverRouteAnswers,
// deliverToChief) are reached.
// ---------------------------------------------------------------------------

// waitRouteTurns waits for every route turn this server started to end.
func (f *routeFixture) waitRouteTurns(t *testing.T) {
	t.Helper()
	ended := make(chan struct{})
	go func() { f.srv.attention.deliveries.Wait(); close(ended) }()
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("a route turn never ended")
	}
}

// TestARefusedRouteTurnReleasesItsClaimOnce: the claim's owner string is the
// same in every process, so another replica can hold the same route's claim
// the moment this turn's fallback runs. The turn's end must not clear it.
//
// 🔴 REGRESSION: at 60cdee0e the refusal releases the claim before the
// fallback and the deferred release runs again after it — clearing the claim
// another holder took in between.
func TestARefusedRouteTurnReleasesItsClaimOnce(t *testing.T) {
	f, pod, _ := newRouteAnswerFixture(t)
	pod.setBusy(true)
	const routeID = "rt_0000000000000000000000aa"
	owner := routeTurnClaimPrefix + routeID
	var otherWon atomic.Bool
	refused := func(ctx context.Context) {
		// Another replica, offering the same route, takes its claim now.
		won, err := f.srv.ext.Agents.ClaimKickoff(f.asOwner, f.chief.ID, owner, time.Minute)
		if err != nil {
			t.Errorf("the other holder's claim: %v", err)
		}
		otherWon.Store(won)
	}
	if !f.srv.startRouteTurn(f.asOwner, f.chief, f.chiefChat(t), routeID, "[muster] a route turn the runtime refuses", nil, refused) {
		t.Fatal("control: the route turn did not start")
	}
	f.waitRouteTurns(t)
	if !otherWon.Load() {
		// The claim was still this turn's when the fallback ran (released once,
		// at the end): the other holder takes it now, and nothing clears it.
		won, err := f.srv.ext.Agents.ClaimKickoff(f.asOwner, f.chief.ID, owner, time.Minute)
		if err != nil || !won {
			holder, _ := f.srv.ext.Agents.KickoffClaimHolder(f.asOwner, f.chief.ID)
			t.Fatalf("the turn's claim was not released at its end: %v %v (holder %q)", won, err, holder)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if holder, err := f.srv.ext.Agents.KickoffClaimHolder(f.asOwner, f.chief.ID); err != nil || holder != owner {
		t.Fatalf("another holder's claim was cleared by the refused turn's release: holder %q (%v)", holder, err)
	}
	_ = f.srv.ext.Agents.ReleaseKickoffClaim(f.asOwner, f.chief.ID, owner)
}

// TestARefusedRouteTurnLeavesItsSessionAsItWas: a refused attempt's row —
// stored before the turn, removed on the refusal — neither moves the chat up
// the list (updated_at) nor names an untitled one.
//
// 🔴 REGRESSION: at 60cdee0e the row's write bumps updated_at and titles the
// session, and its removal undoes neither.
func TestARefusedRouteTurnLeavesItsSessionAsItWas(t *testing.T) {
	f, pod, _ := newRouteAnswerFixture(t)
	pod.setBusy(true)
	st := agents.NewPG(f.pool)
	sess, err := st.CreateSession(f.asOwner, f.chief.ID, f.chief.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(authzSystem(), `UPDATE chat_sessions SET updated_at = now() - interval '1 day', title = '' WHERE id = $1`, sess.ID); err != nil {
		t.Fatal(err)
	}
	read := func() (time.Time, string) {
		var at time.Time
		var title string
		if err := f.pool.QueryRow(authzSystem(), `SELECT updated_at, title FROM chat_sessions WHERE id = $1`, sess.ID).Scan(&at, &title); err != nil {
			t.Fatal(err)
		}
		return at, title
	}
	beforeAt, beforeTitle := read()
	if !f.srv.startRouteTurn(f.asOwner, f.chief, sess, "rt_0000000000000000000000bb", "[muster] Route answers a refused turn carried", nil, nil) {
		t.Fatal("control: the route turn did not start")
	}
	f.waitRouteTurns(t)
	if n := len(f.chatText(t, sess.ID)); n != 0 {
		t.Fatalf("control: the refused turn left %d row(s)", n)
	}
	if at, title := read(); !at.Equal(beforeAt) || title != beforeTitle {
		t.Fatalf("a refused route turn changed its session: updated_at %s → %s, title %q → %q", beforeAt, at, beforeTitle, title)
	}
}

// failingDeleteAgents is the agents store whose DeleteChatMessage always fails.
type failingDeleteAgents struct{ agents.Store }

func (failingDeleteAgents) DeleteChatMessage(context.Context, int64, int64) error {
	return errors.New("delete refused (test)")
}

// TestARefusedTurnWhoseRowCannotBeRemovedWritesNoSecondCopy: a taken
// hand-back's turn is refused, and its row cannot be removed: that row stands
// as the record — the fallback writes no second copy.
//
// 🔴 REGRESSION: at 60cdee0e the fallback writes the note beside the row that
// survived (two copies).
func TestARefusedTurnWhoseRowCannotBeRemovedWritesNoSecondCopy(t *testing.T) {
	f, pod, tok := newRouteAnswerFixture(t)
	id, sessID := f.owedAnswer(t, tok, answerBody(1))
	r := f.routeState(t, id)
	if _, _, ok, err := f.srv.ext.Routes.TakeHandBack(f.asOwner, id); err != nil || !ok {
		t.Fatalf("control: take the hand-back: %v %v", ok, err)
	}
	pod.setBusy(true)
	f.srv.ext.Agents = failingDeleteAgents{f.srv.ext.Agents}
	const text = "[muster] Route test: a taken hand-back whose refused row cannot be removed."
	f.srv.deliverToChief(f.srv.routePrincipal(context.Background(), f.owner), r, text, true)
	f.waitRouteTurns(t)
	if n := countContaining(f.chatText(t, sessID), text); n != 1 {
		t.Fatalf("the hand-back is in the chat %d time(s), want 1 (the row that could not be removed)", n)
	}
}

// claimRaceAgents reads the chief's claim as FREE the first `free` times — the
// moment just before another route turn took it — and as it is after.
type claimRaceAgents struct {
	agents.Store
	free atomic.Int32
}

func (c *claimRaceAgents) KickoffClaimHolder(ctx context.Context, id int64) (string, error) {
	if c.free.Add(-1) >= 0 {
		return "", nil
	}
	return c.Store.KickoffClaimHolder(ctx, id)
}

// TestTheSecondInFlightCheckWritesNoFallbackNote reaches the race the second
// routeTurnInFlight check exists for: another route turn takes the chief's
// claim between a sweep's first check and its own claim attempt. Its answer —
// past routeChiefWaitMax — and its hand-back are not written as notes then
// (that turn may be carrying them), and stay owed.
//
// COVERAGE of guards that exist at 60cdee0e (green there); mutation-checked:
// removing either second check turns its sub-test red.
func TestTheSecondInFlightCheckWritesNoFallbackNote(t *testing.T) {
	for _, path := range []string{"answers", "hand-back"} {
		t.Run(path, func(t *testing.T) {
			f, _, tok := newRouteAnswerFixture(t)
			id, sessID := f.owedAnswer(t, tok, answerBody(1))
			f.endedLongAgo(t, id)
			r := f.routeState(t, id)
			const other = routeTurnClaimPrefix + "rt_0000000000000000000000cc"
			if won, err := f.srv.ext.Agents.ClaimKickoff(f.asOwner, f.chief.ID, other, time.Minute); err != nil || !won {
				t.Fatalf("control: the other turn's claim: %v %v", won, err)
			}
			defer func() { _ = agents.NewPG(f.pool).ReleaseKickoffClaim(f.asOwner, f.chief.ID, other) }()
			race := &claimRaceAgents{Store: f.srv.ext.Agents}
			race.free.Store(1)
			f.srv.ext.Agents = race
			ctx := f.srv.routePrincipal(context.Background(), f.owner)
			text := answerBody(1)
			if path == "answers" {
				f.srv.deliverRouteAnswers(ctx, sessID, []plugins.Route{r})
			} else {
				text = "[muster] Route " + id + ": a hand-back that waited past the limit."
				f.srv.deliverToChief(ctx, r, text, false)
			}
			if race.free.Load() == 1 {
				t.Fatal("control: the first in-flight check was never read")
			}
			if n := countContaining(f.chatText(t, sessID), text); n != 0 {
				t.Fatalf("a fallback note was written while another route turn held the claim (%d row(s))", n)
			}
			if f.handBackOwed(t, id) == "" {
				t.Fatal("the answer was taken while another route turn held the claim")
			}
		})
	}
}
