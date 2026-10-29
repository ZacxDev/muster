package plugins

import (
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestAnOwnerAnsweredMessageIsNotRetryable: a message whose route the owner
// answered upstream themselves (owner_answered) has had its answer, so the
// owner's Retry must not hand it to the chief again (liveRoute, retry.go).
// The control: the same shape with the route merely timed out IS retryable,
// so the list can name such a message at all.
func TestAnOwnerAnsweredMessageIsNotRetryable(t *testing.T) {
	w := newDraftWorld(t)
	unconfirmed := func(ext string) Message {
		t.Helper()
		in := msg(ext)
		in.Thread = "dm-" + ext
		m, _, err := w.s.Submit(plugin(2), w.p.ID, in)
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := w.s.BeginDelivery(plugin(2), m.ID, w.chief.ID); err != nil || !ok {
			t.Fatalf("begin delivery %s: %v %v", ext, ok, err)
		}
		if ok, err := w.s.RecordDelivery(plugin(2), m.ID, w.chief.ID, StatusUnconfirmed); err != nil || !ok {
			t.Fatalf("record %s unconfirmed: %v %v", ext, ok, err)
		}
		m, _ = w.s.Message(plugin(2), m.ID)
		return m
	}
	timedOut := func(m Message) Route {
		t.Helper()
		r := w.route(t, m)
		dbtest.OwnerExec(t, w.dsn, `UPDATE session_routes SET created_at = now() - interval '2 hours',
			timeout_at = now() - interval '1 minute' WHERE route_id = '`+r.RouteID+`'`)
		if _, ok, err := w.s.TimeOutRoute(session(2), r.RouteID); err != nil || !ok {
			t.Fatalf("time out: %v %v", ok, err)
		}
		return r
	}
	answered, control := unconfirmed("oa-retry-1"), unconfirmed("oa-retry-2")
	r := timedOut(answered)
	timedOut(control)
	got, err := w.s.SettleOwnerReplied(plugin(2), w.p.ID, *answered.Thread, time.Now())
	if err != nil || len(got) != 1 || got[0].Route.RouteID != r.RouteID || got[0].Route.State != RouteOwnerAnswered ||
		got[0].From != RouteTimedOut || !got[0].HandBackWithdrawn {
		t.Fatalf("settle: %+v %v; want route %s owner_answered from timed_out, its hand-back withdrawn", got, err, r.RouteID)
	}
	list, err := w.s.Retryable(session(2), 50)
	if err != nil {
		t.Fatal(err)
	}
	in := map[int64]bool{}
	for _, m := range list {
		in[m.ID] = true
	}
	if !in[control.ID] {
		t.Fatalf("CONTROL: an unconfirmed message whose route timed out is not retryable: %+v", list)
	}
	if in[answered.ID] {
		t.Fatalf("an unconfirmed message the owner answered upstream is retryable")
	}
	if got := auditActions(t, w, RouteObject(r.RouteID)); len(got) != 3 || got[2] != "session_route.owner_answered" {
		t.Fatalf("audit = %v; want routed, timed_out, owner_answered", got)
	}
}
