package plugins

import (
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/dbtest"
)

// TimedOutUndelivered's own guards (the sweep that calls it is internal/api's
// route_never_delivered_pg_test.go): it rewrites only a TIMED-OUT route, and
// re-words a hand-back only while one is still owed — never owing a second.

// TestTimedOutUndeliveredTouchesOnlyATimedOutRoute: a route still routed is
// left as it is (noted false, no error, nothing owed).
func TestTimedOutUndeliveredTouchesOnlyATimedOutRoute(t *testing.T) {
	w := newDraftWorld(t)
	r := w.route(t, w.m)
	got, noted, err := w.s.TimedOutUndelivered(session(2), r.RouteID)
	if err != nil || noted || got.State != RouteRouted || got.Error != "" {
		t.Fatalf("a routed route: %+v noted=%v %v; want unchanged", got, noted, err)
	}
	if _, reason, ok, err := w.s.TakeHandBack(session(2), r.RouteID); err != nil || ok {
		t.Fatalf("a routed route owes %q (%v %v); want nothing", reason, ok, err)
	}
}

// TestTimedOutUndeliveredRewordsTheOwedHandBack: owed, the hand-back says it
// was never delivered, naming the route's deadline; the error is
// RouteNeverDeliveredError; and the hand-back is still given out exactly once.
func TestTimedOutUndeliveredRewordsTheOwedHandBack(t *testing.T) {
	w := newDraftWorld(t)
	r := w.route(t, w.m)
	dbtest.OwnerExec(t, w.dsn, `UPDATE session_routes SET timeout_at = now() - interval '1 minute' WHERE route_id = '`+r.RouteID+`'`)
	to, timedOut, err := w.s.TimeOutRoute(session(2), r.RouteID)
	if err != nil || !timedOut {
		t.Fatal(timedOut, err)
	}
	got, noted, err := w.s.TimedOutUndelivered(session(2), r.RouteID)
	if err != nil || !noted || got.State != RouteTimedOut ||
		got.Error != "never delivered: the session stayed busy until the deadline, so its queued message was cancelled" {
		t.Fatalf("noted=%v %v, route %+v", noted, err, got)
	}
	want := "it was never delivered: the session stayed busy until the deadline (" +
		to.TimeoutAt.UTC().Format(time.RFC3339) + "), and its queued message was cancelled unsent"
	if _, reason, ok, err := w.s.TakeHandBack(session(2), r.RouteID); err != nil || !ok || reason != want {
		t.Fatalf("the hand-back: %q (%v %v), want %q", reason, ok, err, want)
	}
	if _, _, ok, _ := w.s.TakeHandBack(session(2), r.RouteID); ok {
		t.Fatal("the hand-back was given out twice")
	}
	if got := auditActions(t, w, RouteObject(r.RouteID)); len(got) != 3 || got[2] != "session_route.undelivered" {
		t.Fatalf("audit = %v", got)
	}
}

// TestTimedOutUndeliveredOwesNothingOnceTheHandBackWasTaken: a hand-back
// already delivered (in TimeOutRoute's words) is not owed again — the error
// is corrected, nothing more.
func TestTimedOutUndeliveredOwesNothingOnceTheHandBackWasTaken(t *testing.T) {
	w := newDraftWorld(t)
	r := w.route(t, w.m)
	dbtest.OwnerExec(t, w.dsn, `UPDATE session_routes SET timeout_at = now() - interval '1 minute' WHERE route_id = '`+r.RouteID+`'`)
	if _, timedOut, err := w.s.TimeOutRoute(session(2), r.RouteID); err != nil || !timedOut {
		t.Fatal(timedOut, err)
	}
	if _, _, ok, err := w.s.TakeHandBack(session(2), r.RouteID); err != nil || !ok {
		t.Fatal("control: the timeout owes a hand-back", ok, err)
	}
	got, noted, err := w.s.TimedOutUndelivered(session(2), r.RouteID)
	if err != nil || !noted || got.Error != RouteNeverDeliveredError {
		t.Fatalf("noted=%v %v, route %+v", noted, err, got)
	}
	if _, reason, ok, err := w.s.TakeHandBack(session(2), r.RouteID); err != nil || ok {
		t.Fatalf("a taken hand-back is owed again: %q (%v %v)", reason, ok, err)
	}
}
