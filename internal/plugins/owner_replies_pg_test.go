package plugins

import (
	"testing"

	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestAnOwnerAnsweredMessageIsNotRetryable: a message whose route ended
// owner_answered (its reply went out directly, stamped) has had its answer, so the
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
	r := w.route(t, answered)
	timedOut(control)
	got, settled, err := w.s.SettleOwnerAnswered(plugin(2), w.p.ID, answered.ID, *answered.Thread)
	if err != nil || !settled || got.RouteID != r.RouteID || got.State != RouteOwnerAnswered || got.OwnerRepliedAt == nil {
		t.Fatalf("settle: %+v %v %v; want route %s owner_answered, with its time", got, settled, err, r.RouteID)
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
		t.Fatalf("an unconfirmed message answered directly (owner_answered) is retryable")
	}
	if got := auditActions(t, w, RouteObject(r.RouteID)); len(got) != 2 || got[1] != "session_route.owner_answered" {
		t.Fatalf("audit = %v; want routed, owner_answered", got)
	}
}
