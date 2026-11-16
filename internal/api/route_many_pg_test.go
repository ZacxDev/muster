package api

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/plugins"
)

// route_many (route_many.go) through the real server against a real Postgres:
// one owner request to several sessions; each answer is OWED, never a turn of
// its own; ONE digest turn reaches the chief when all have answered or at the
// deadline; a session that cannot take it is refused in the call's result.

// routeManyRes is route_many's answer as these tests read it.
type routeManyRes struct {
	BatchID string `json:"batch_id"`
	Routed  []struct {
		Subject string `json:"subject"`
		RouteID string `json:"route_id"`
	} `json:"routed"`
	Refused []routeManyRefused `json:"refused"`
	Skipped []routeManyRefused `json:"skipped"`
	Error   string             `json:"error"`
	Code    string             `json:"refused_code"`
}

// routeMany calls route_many as the chief over /mcp.
func (f *routeFixture) routeMany(t *testing.T, subjects []string, text string) (routeManyRes, string) {
	t.Helper()
	out, _ := f.chiefMCP(t, routeManyToolName, map[string]any{"subjects": subjects, "text": text, "owner_asked": true})
	var res routeManyRes
	_ = json.Unmarshal([]byte(out), &res)
	return res, out
}

// routeOf is the route id route_many made for subject sub.
func (r routeManyRes) routeOf(t *testing.T, sub string) string {
	t.Helper()
	for _, x := range r.Routed {
		if x.Subject == sub {
			return x.RouteID
		}
	}
	t.Fatalf("no route for %s in %+v", sub, r)
	return ""
}

// twoSessionBatch routes one request to routeSID and otherRouteSID in an
// owner turn that then ends: the fixture, its pod, the reply token, the
// batch and the owner's chat session.
func twoSessionBatch(t *testing.T) (*routeFixture, *answerPod, string, routeManyRes, int64) {
	t.Helper()
	f, pod, tok := newRouteAnswerFixture(t)
	said := f.typed(t, f.owner, "Ask both sessions which task they work under.")
	res, out := f.routeMany(t, []string{"mirror:" + routeSID, "mirror:" + otherRouteSID}, "Which task do you work under?")
	if len(res.Routed) != 2 || len(res.Refused) != 0 || !plugins.ValidBatchID(res.BatchID) {
		t.Fatalf("route_many: %s", out)
	}
	if err := agents.NewPG(f.pool).ClearOwnerTurn(f.asOwner, f.chief.ID, said.ID); err != nil {
		t.Fatal(err)
	}
	return f, pod, tok, res, said.SessionID
}

// sweeps runs the route sweep n times, letting any turn it started finish.
func (f *routeFixture) sweeps(t *testing.T, n int) {
	t.Helper()
	for range n {
		if _, err := f.srv.RouteTimeoutPass(context.Background()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(40 * time.Millisecond)
	}
}

// TestRouteManyWhenEveryoneAnswersTheChiefGetsOneDigest: the first answer
// reaches NO turn and no chat row while the other session has not answered
// (no per-answer wake); once both have, exactly ONE turn carries both answers,
// fenced, and further sweeps add none. Both members' hand-backs are cleared.
func TestRouteManyWhenEveryoneAnswersTheChiefGetsOneDigest(t *testing.T) {
	f, pod, tok, res, chat := twoSessionBatch(t)
	first, second := res.routeOf(t, "mirror:"+routeSID), res.routeOf(t, "mirror:"+otherRouteSID)
	if out, isErr := f.routeReplyCall(t, tok, routeSID, map[string]any{"route_id": first, "body": answerBody(1)}); isErr {
		t.Fatalf("route_reply 1: %s", out)
	}
	f.sweeps(t, 4)
	if got := pod.taken(); len(got) != 0 {
		t.Fatalf("a batch member's answer woke the chief by itself (%d turn(s)): %q", len(got), got)
	}
	if n := countContaining(f.chatText(t, chat), answerBody(1)); n != 0 {
		t.Fatalf("a batch member's answer is in the chief's chat on its own (%d row(s))", n)
	}
	if hb := f.handBackOwed(t, first); hb == "" {
		t.Fatal("control: the answered member owes nothing; the digest would have nothing to wait on")
	}

	if out, isErr := f.routeReplyCall(t, tok, otherRouteSID, map[string]any{"route_id": second, "body": answerBody(2)}); isErr {
		t.Fatalf("route_reply 2: %s", out)
	}
	if !f.sweepUntil(t, func() bool { return len(pod.taken()) > 0 }) {
		t.Fatal("the digest never reached the chief's runtime")
	}
	f.sweeps(t, 4)
	turns := pod.taken()
	if len(turns) != 1 {
		t.Fatalf("%d chief turns, want exactly the one digest: %q", len(turns), turns)
	}
	d := turns[0]
	for _, want := range []string{routeBatchHead(res.BatchID) + " ", "2 answered, 0 no reply", "[mirror:" + routeSID + "] answered: " + answerBody(1),
		"[mirror:" + otherRouteSID + "] answered: " + answerBody(2), "<<<" + routeBatchDigestMarker + " untrusted nonce="} {
		if !strings.Contains(d, want) {
			t.Fatalf("the digest lacks %q:\n%s", want, d)
		}
	}
	if n := countContaining(f.chatText(t, chat), routeBatchHead(res.BatchID)+" "); n != 1 {
		t.Fatalf("the digest is in the owner's chief chat %d time(s), want 1", n)
	}
	for _, id := range []string{first, second} {
		if hb := f.handBackOwed(t, id); hb != "" {
			t.Fatalf("member %s still owes %q after the digest", id, hb)
		}
	}
}

// TestRouteManyAtTheDeadlineTheDigestMarksTheRestNoReply: one member answers,
// the other is still routed when the deadline passes: ONE digest carries the
// answer and marks the other no reply.
func TestRouteManyAtTheDeadlineTheDigestMarksTheRestNoReply(t *testing.T) {
	f, pod, tok, res, _ := twoSessionBatch(t)
	first := res.routeOf(t, "mirror:"+routeSID)
	if out, isErr := f.routeReplyCall(t, tok, routeSID, map[string]any{"route_id": first, "body": answerBody(1)}); isErr {
		t.Fatalf("route_reply: %s", out)
	}
	f.sweeps(t, 2)
	if got := pod.taken(); len(got) != 0 {
		t.Fatalf("a turn before the deadline: %q", got)
	}
	// The deadline passes for the member still waiting.
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE session_routes SET timeout_at = now() - interval '1 second'
		WHERE batch_id = $1 AND state = 'routed'`, res.BatchID); err != nil {
		t.Fatal(err)
	}
	if !f.sweepUntil(t, func() bool { return len(pod.taken()) > 0 }) {
		t.Fatal("the digest never reached the chief at the deadline")
	}
	f.sweeps(t, 4)
	turns := pod.taken()
	if len(turns) != 1 {
		t.Fatalf("%d chief turns at the deadline, want one digest: %q", len(turns), turns)
	}
	for _, want := range []string{"1 answered, 1 no reply", "[mirror:" + routeSID + "] answered: " + answerBody(1),
		"[mirror:" + otherRouteSID + "] no reply ("} {
		if !strings.Contains(turns[0], want) {
			t.Fatalf("the digest lacks %q:\n%s", want, turns[0])
		}
	}
}

// TestRouteManyRefusesEachSessionThatCannotTakeItInItsResult: in ONE call, a
// window the chief started (dispatched_window), a session no window shows
// (window_gone), a malformed subject and a repeat are each refused by
// subject, synchronously; a dispatched agent and an opencode session — never
// a route target — are SKIPPED, not routed and refused; and the one session
// that can take it is routed.
func TestRouteManyRefusesEachSessionThatCannotTakeItInItsResult(t *testing.T) {
	f, _, _ := newRouteAnswerFixture(t)
	tagRouteWindow(f, 77) // @4 (routeSID) runs an agent the chief started
	f.typed(t, f.owner, "Ask them all.")
	gone := "mirror:6a6a6a6a-0000-4000-8000-00000000dead"
	routes, _ := f.ownersRoutesAndRows(t)
	res, out := f.routeMany(t, []string{"mirror:" + routeSID, "mirror:" + otherRouteSID, gone, "agent:7", "mirror:opencode:ses_launch",
		"bogus", "mirror:" + otherRouteSID}, "Which task do you work under?")
	if len(res.Routed) != 1 || res.Routed[0].Subject != "mirror:"+otherRouteSID || !plugins.ValidBatchID(res.BatchID) {
		t.Fatalf("routed: %s", out)
	}
	var skipped []string
	for _, r := range res.Skipped {
		skipped = append(skipped, r.Subject)
	}
	if !slices.Equal(skipped, []string{"agent:7", "mirror:opencode:ses_launch"}) {
		t.Fatalf("skipped %v, want the agent and the opencode session: %s", skipped, out)
	}
	got := map[string]string{}
	for _, r := range res.Refused {
		if r.Error == "" {
			t.Errorf("refusal of %s gives no reason", r.Subject)
		}
		got[r.Subject+" "+r.Refused] = r.Error
	}
	for _, want := range []string{"mirror:" + routeSID + " " + RefusedDispatchedWindow, gone + " " + RefusedWindowGone,
		"bogus " + RefusedBadArguments, "mirror:" + otherRouteSID + " " + RefusedBadArguments} {
		if _, ok := got[want]; !ok {
			t.Errorf("no refusal %q in %v", want, got)
		}
	}
	if len(res.Refused) != 4 {
		t.Fatalf("%d refusals, want 4: %s", len(res.Refused), out)
	}
	if r2, _ := f.ownersRoutesAndRows(t); r2 != routes+1 {
		t.Fatalf("%d routes made, want 1 (the skipped subjects make none)", r2-routes)
	}
}

// TestRouteManyIsRefusedOutsideTheOwnersTurn: route_to_session's owner_asked
// rule, unchanged — with no owner-typed turn in flight the whole call is
// refused not_owners_turn with route_to_session's sentence, and nothing is
// routed or queued; owner_asked false is bad arguments. Control: the same
// call in the owner's turn routes.
func TestRouteManyIsRefusedOutsideTheOwnersTurn(t *testing.T) {
	f, _, _ := newRouteAnswerFixture(t)
	routes, rows := f.ownersRoutesAndRows(t)
	subjects := []string{"mirror:" + routeSID, "mirror:" + otherRouteSID}
	_, out := f.routeMany(t, subjects, "Which task do you work under?")
	if refusedAs(out) != RefusedNotOwnersTurn || !strings.Contains(out, routeOwnerAskedSibling[chiefReleaseNoOwnerTurn]) {
		t.Fatalf("route_many outside the owner's turn: %s", out)
	}
	if r2, q2 := f.ownersRoutesAndRows(t); r2 != routes || q2 != rows {
		t.Fatalf("a refused route_many left %d routes and %d queued rows (was %d, %d)", r2, q2, routes, rows)
	}
	if text, _ := f.chiefMCP(t, routeManyToolName, map[string]any{"subjects": subjects, "text": "x", "owner_asked": false}); refusedAs(text) != RefusedBadArguments {
		t.Fatalf("owner_asked false: %s", text)
	}
	f.typed(t, f.owner, "Ask both.")
	if res, out := f.routeMany(t, subjects, "Which task do you work under?"); len(res.Routed) != 2 {
		t.Fatalf("control: route_many in the owner's turn: %s", out)
	}
}
