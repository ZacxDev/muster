package api

import (
	"context"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/sendqueue"
)

// A TIMED-OUT ROUTE SAYS WHETHER ITS MESSAGE WAS EVER DELIVERED. A route's
// paste waits in the send queue for the session to go idle; a session busy for
// the route's whole window never gets it. The time-out then cancels the still
// queued row, and the hand-back the chief reads — and the route's error — say
// it was never delivered, not that "no answer came". A row that was sent, or
// that a delivery claimed between the time-out and the cancel, keeps the
// "no answer came by" wording: the session may have it.
//
// RED AT 9bbccfb1 (the base): TestATimedOutRouteNeverPastedSaysItWasNeverDelivered
// — the route's error is "no answer before the deadline" and the hand-back
// "no answer came by …". The other two are INVARIANT GUARDS (green at the
// base too): they pin that the new wording is never claimed for a message the
// session may have.

// timeOutRouteFixture routes one message into the fixture's window with the
// chief's session set to hear the hand-back, and returns the route, its
// queued row and the chief's session.
func timeOutRouteFixture(t *testing.T, f *routeFixture, external string) (string, sendqueue.Item, agents.ChatSession) {
	t.Helper()
	m := f.deliveredMessage(t, f.owner, f.chief.ID, external)
	ps, _ := f.plugins.List(f.asOwner)
	sess, err := f.srv.ext.Agents.CreateSession(f.asOwner, f.chief.ID, f.chief.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.plugins.SetChiefSession(f.asOwner, ps[0].ID, f.chief.ID, sess.ID); err != nil {
		t.Fatal(err)
	}
	id := f.route(t, m)
	items := f.queued(t)
	if len(items) != 1 || items[0].RouteID != id || items[0].State != sendqueue.Queued {
		t.Fatalf("fixture: the injection: %+v", items)
	}
	return id, items[0], sess
}

// pastDeadline moves route id's deadline a minute into the past.
func pastDeadline(t *testing.T, id string) {
	t.Helper()
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE session_routes SET timeout_at = now() - interval '1 minute' WHERE route_id = '`+id+`'`)
}

// handBackNote is the one note the chief's session holds.
func (f *routeFixture) handBackNote(t *testing.T, sessID int64) string {
	t.Helper()
	msgs, err := f.srv.ext.Agents.ListChatMessages(f.asOwner, sessID)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("the chief's notes: %+v %v; want exactly the one hand-back", msgs, err)
	}
	return msgs[0].Content
}

// TestATimedOutRouteNeverPastedSaysItWasNeverDelivered: the row is still
// queued at the deadline, so the cancel wins: the route's error and the
// chief's hand-back say it was never delivered, naming the deadline.
func TestATimedOutRouteNeverPastedSaysItWasNeverDelivered(t *testing.T) {
	f := newRouteFixture(t)
	id, row, sess := timeOutRouteFixture(t, f, "route-never-delivered")
	pastDeadline(t, id)
	if n, err := f.srv.RouteTimeoutPass(context.Background()); err != nil || n < 1 {
		t.Fatalf("past the deadline: %d timed out (%v)", n, err)
	}
	r := f.routeState(t, id)
	if r.State != plugins.RouteTimedOut {
		t.Fatalf("state %s, want timed_out", r.State)
	}
	if it := f.queueRow(t, row.ID); it.State != sendqueue.Cancelled || !it.ClaimedAt.IsZero() {
		t.Fatalf("the row: %s, claimed %v; want cancelled, never claimed", it.State, it.ClaimedAt)
	}
	if want := "never delivered: the session stayed busy until the deadline, so its queued message was cancelled"; r.Error != want {
		t.Errorf("the route's error = %q, want %q", r.Error, want)
	}
	want := "[muster] Route " + id + " (plugin message_id " + itoa(r.MessageID) + " → mirror:" + routeSID + "): " +
		"it was never delivered: the session stayed busy until the deadline (" +
		r.TimeoutAt.UTC().Format(time.RFC3339) + "), and its queued message was cancelled unsent, so the message is back with you: " +
		routeHandBackChoices
	if got := f.handBackNote(t, sess.ID); got != want {
		t.Fatalf("the hand-back:\n got %q\nwant %q", got, want)
	}
	if hb := f.handBackOwed(t, id); hb != "" {
		t.Fatalf("a hand-back is still owed after delivery: %q", hb)
	}
}

// TestATimedOutRouteThatWasSentSaysNoAnswerCame: the row was pasted (sent)
// before the deadline, so the session had the message: the hand-back keeps
// "no answer came by", never "never delivered". An invariant guard.
func TestATimedOutRouteThatWasSentSaysNoAnswerCame(t *testing.T) {
	f := newRouteFixture(t)
	id, row, sess := timeOutRouteFixture(t, f, "route-sent-unanswered")
	if _, ok, err := f.queue.Claim(context.Background(), row.ID, time.Now(), sendqueue.Claim{By: "test", Lease: time.Minute}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := f.queue.Finish(context.Background(), row.ID, "test", sendqueue.Outcome{Sent: true, Detail: "Sent."}, time.Now()); err != nil {
		t.Fatal(err)
	}
	pastDeadline(t, id)
	if n, err := f.srv.RouteTimeoutPass(context.Background()); err != nil || n < 1 {
		t.Fatalf("past the deadline: %d timed out (%v)", n, err)
	}
	assertNoAnswerCame(t, f, id, sess.ID)
	if it := f.queueRow(t, row.ID); it.State != sendqueue.Sent {
		t.Fatalf("the row: %s, want sent", it.State)
	}
}

// claimAfterTimeOut is a route store that, the moment TimeOutRoute has timed
// a route out, claims its queued row as a delivery would: the race between
// the sweep's time-out and its cancel, which the claim wins.
type claimAfterTimeOut struct {
	plugins.RouteStore
	claim func()
}

func (c claimAfterTimeOut) TimeOutRoute(ctx context.Context, routeID string) (plugins.Route, bool, error) {
	r, timedOut, err := c.RouteStore.TimeOutRoute(ctx, routeID)
	if err == nil && timedOut {
		c.claim()
	}
	return r, timedOut, err
}

// TestARouteClaimedBetweenTheTimeOutAndTheCancelIsNotCalledNeverDelivered: a
// delivery claims the row after the route timed out and before the sweep's
// cancel, so the cancel fails (ErrNotCancellable): the row is being sent, as
// far as the sweep can know, so the hand-back keeps "no answer came by". An
// invariant guard.
func TestARouteClaimedBetweenTheTimeOutAndTheCancelIsNotCalledNeverDelivered(t *testing.T) {
	f := newRouteFixture(t)
	id, row, sess := timeOutRouteFixture(t, f, "route-claim-race")
	claimed := false
	f.srv.ext.Routes = claimAfterTimeOut{RouteStore: f.srv.ext.Routes, claim: func() {
		_, ok, err := f.queue.Claim(context.Background(), row.ID, time.Now(), sendqueue.Claim{By: "test", Lease: time.Minute})
		if err != nil {
			t.Errorf("the racing claim: %v", err)
		}
		claimed = ok
	}}
	pastDeadline(t, id)
	if n, err := f.srv.RouteTimeoutPass(context.Background()); err != nil || n < 1 {
		t.Fatalf("past the deadline: %d timed out (%v)", n, err)
	}
	if !claimed {
		t.Fatal("control: the racing claim did not take the row")
	}
	if it := f.queueRow(t, row.ID); it.State != sendqueue.Sending {
		t.Fatalf("the row: %s, want sending (the claim won; the cancel did not)", it.State)
	}
	assertNoAnswerCame(t, f, id, sess.ID)
}

// assertNoAnswerCame: route id timed out with TimeOutRoute's own words, and
// the chief's one note says no answer came by the deadline — never that the
// message was not delivered.
func assertNoAnswerCame(t *testing.T, f *routeFixture, id string, sessID int64) {
	t.Helper()
	r := f.routeState(t, id)
	if r.State != plugins.RouteTimedOut || r.Error != "no answer before the deadline" {
		t.Fatalf("the route: %s, error %q; want timed_out, %q", r.State, r.Error, "no answer before the deadline")
	}
	want := "[muster] Route " + id + " (plugin message_id " + itoa(r.MessageID) + " → mirror:" + routeSID + "): " +
		"no answer came by " + r.TimeoutAt.UTC().Format(time.RFC3339) + ", so the message is back with you: " + routeHandBackChoices
	if got := f.handBackNote(t, sessID); got != want {
		t.Fatalf("the hand-back:\n got %q\nwant %q", got, want)
	}
}
