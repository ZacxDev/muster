package api

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefnudge"
)

// #415 round 1: a failed read of the notices' sessions, a rolling deploy's
// older replica, and the asks 0071 withdrew behind everyone's back.

// failingByAsks is the nudge store whose batch read of the notices' sessions
// fails (ByAsk alone keeps working).
type failingByAsks struct{ chiefnudge.Store }

func (failingByAsks) ByAsks(context.Context, []int64) (map[int64]chiefnudge.Decision, error) {
	return nil, errors.New("failingByAsks: the database went away")
}

// TestAFailedNoticeReadMakesTheQueuePartial: when the read of the notices'
// sessions fails, the notice is still listed (by its label, with no tmux
// target) and the queue says it is PARTIAL — so the summary is partial and
// `muster nudge next` exits 1, never 11 ("none waiting"). RED before the fix:
// the error was only logged and the summary read as complete.
func TestAFailedNoticeReadMakesTheQueuePartial(t *testing.T) {
	f := newNudgeFixture(t, true)
	f.gw.set(nudgeAskReply)
	f.pass(t)
	d := f.one(t)
	f.srv.ext.ChiefNudges = failingByAsks{Store: f.nudges}
	ctx := f.srv.attentionPrincipal(context.Background(), authz.OperatorUserID)
	hosts := f.srv.ext.Mirror.Hosts(ctx)
	items, err := f.srv.attentionItemsIn(ctx, hosts)
	if err == nil {
		t.Fatal("a failed read of the notices' sessions did not make the queue partial")
	}
	found := false
	for _, it := range items {
		if it.AskID == d.AskID {
			found = it.Kind == attentionKindNotice && it.Target == ""
		}
	}
	if !found {
		t.Fatalf("the notice is not listed (untargeted) on a partial read: %+v", items)
	}
	if sum := f.srv.attentionSummaryOf(authz.OperatorUserID, items, err != nil, hosts, time.Now()); !sum.Partial {
		t.Fatal("the summary is not partial")
	}
}

// TestARelayAnOlderReplicaClaimedIsFailedNotLeftInFlight: during a rolling
// deploy the older replica still answers, and can claim an answer to a nudge
// ask for a relay (relay_queued) after 0071 ran. The new sweep fails it, with
// its audit row, instead of leaving it in flight for ever. RED before the fix:
// Pending never selected relay_queued, so the row stayed.
func TestARelayAnOlderReplicaClaimedIsFailedNotLeftInFlight(t *testing.T) {
	f := newNudgeFixture(t, true)
	f.gw.set(nudgeAskReply)
	f.pass(t)
	d := f.legacyNudgeAsk(t, f.one(t))
	// What the older replica's relayNudgeAnswer wrote.
	if moved, err := f.nudges.Move(asUser(authz.OperatorUserID), d.ID, []string{chiefnudge.OutcomeAsked},
		chiefnudge.Move{Outcome: chiefnudge.OutcomeRelayQueued}, ""); err != nil || !moved {
		t.Fatalf("seeding the older replica's claim: %v %v", moved, err)
	}
	f.pass(t)
	if got := f.one(t); got.Outcome != chiefnudge.OutcomeFailed || got.Detail != NudgeRelayRetired {
		t.Fatalf("the older replica's relay after a pass: %q (%q), want failed with %q", got.Outcome, got.Detail, NudgeRelayRetired)
	}
	if got := f.audited(t, d.ID); !slices.Contains(got, "chief_nudge.failed") {
		t.Fatalf("no audit row for the failed relay: %v", got)
	}
	if q, p := f.queuedRows(t), f.pastes(); q != 0 || len(p) != 0 {
		t.Fatalf("the claimed relay was sent: %d queued, pastes %+v", q, p)
	}
}

// TestANudgeAskWithdrawnByTheMigrationIsWithdrawnFromPhones: 0071 cancels an
// open nudge ask with a bare UPDATE, which sends nothing. The sweep's next pass
// closes its decision AND withdraws its notification (a `resolved` push under
// the ask's tag), once. RED before the fix: the decision was closed, but the
// phone kept the stale notification.
func TestANudgeAskWithdrawnByTheMigrationIsWithdrawnFromPhones(t *testing.T) {
	f := newNudgeFixture(t, true)
	f.gw.set(nudgeAskReply)
	f.pass(t)
	d := f.legacyNudgeAsk(t, f.one(t))
	tag := "resolved " + attentionTag(attentionAskKey(d.AskID))
	count := func() int {
		n := 0
		for _, p := range f.pushes() {
			if p == tag {
				n++
			}
		}
		return n
	}
	before := count()
	// What 0071's data step does.
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE attention_asks SET status = 'cancelled', resolved_at = now()
		WHERE status = 'open' AND id IN (SELECT ask_id FROM chief_nudges WHERE ask_id IS NOT NULL AND outcome = 'asked')`); err != nil {
		t.Fatal(err)
	}
	f.pass(t)
	if got := f.one(t); got.Outcome != chiefnudge.OutcomeAskClosed {
		t.Fatalf("the decision after the pass: %+v", got)
	}
	if got := count() - before; got != 1 {
		t.Fatalf("%d withdrawal push(es) for ask %s after the pass, want 1", got, strconv.FormatInt(d.AskID, 10))
	}
	f.pass(t)
	if got := count() - before; got != 1 {
		t.Fatalf("a later pass withdrew it again: %d", got)
	}
}
