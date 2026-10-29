package api

import (
	"context"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/prroute"
	"github.com/ZacxDev/muster/internal/sendqueue"
)

// WHEN A ROUTED PASTE CAN NO LONGER REACH ITS SESSION, the route ends now
// rather than at its deadline (4h for a pull-request review):
//   - a newer review round of the same pull request for the same session
//     supersedes an older round still queued (supersedePRRoutes): no hand-back;
//   - a cancelled routed row — the owner's Cancel, or a disable / sign-out
//     everywhere (CancelOwner) — fails its route, owing the chief a hand-back;
//   - a target session no online window has shown for RouteWindowGrace fails
//     its still-queued route the same way (routedPastePass).

// handBackOwed reads route id's owed hand-back ("" for none).
func (f *routeFixture) handBackOwed(t *testing.T, id string) string {
	t.Helper()
	var hb *string
	if err := f.pool.QueryRow(authz.System(context.Background()), `SELECT handback FROM session_routes WHERE route_id = $1`, id).Scan(&hb); err != nil {
		t.Fatal(err)
	}
	if hb == nil {
		return ""
	}
	return *hb
}

// queueRow is send-queue row id as the owner reads it.
func (f *routeFixture) queueRow(t *testing.T, id int64) sendqueue.Item {
	t.Helper()
	it, err := f.queue.Get(f.asOwner, id)
	if err != nil {
		t.Fatal(err)
	}
	return it
}

// round is one review round's body: the machine block with its own head sha,
// so two rounds of one pull request differ as real ones do.
func round(pr int, sha string) string {
	return reviewBody(`"repo":"` + prRepo + `","pr":` + strconv.Itoa(pr) + `,"head_sha":"` + sha + `"`)
}

const (
	shaRound1 = "1111111111111111111111111111111111111111"
	shaRound2 = "2222222222222222222222222222222222222222"
)

// oneRoute is message m's only route.
func (f *prFixture) oneRoute(t *testing.T, m plugins.Message) plugins.Route {
	t.Helper()
	rs := f.routesOf(t, m)
	if len(rs) != 1 || rs[0].SendQueueID == nil {
		t.Fatalf("routes of message %d: %+v; want one, queued", m.ID, rs)
	}
	return rs[0]
}

// --- Y1: a newer review round supersedes an older one still queued ----------

func TestANewerReviewRoundSupersedesTheOlderStillQueued(t *testing.T) {
	f := newPRFixture(t)
	f.authored(t, f.asOwner, routeSID, prCreated())
	old := f.oneRoute(t, f.review(t, "sup-old-1", prSender, true, round(prNumber, shaRound1)))
	cur := f.oneRoute(t, f.review(t, "sup-new-1", prSender, true, round(prNumber, shaRound2)))

	o := f.routeState(t, old.RouteID)
	if o.State != plugins.RouteFailed || !strings.HasPrefix(o.Error, prRouteSuperseded) {
		t.Fatalf("the older round's route: %s %q; want failed, %q", o.State, o.Error, prRouteSuperseded)
	}
	if hb := f.handBackOwed(t, old.RouteID); hb != "" {
		t.Fatalf("the superseded round owes the chief a hand-back %q; want none", hb)
	}
	if it := f.queueRow(t, *old.SendQueueID); it.State != sendqueue.Cancelled {
		t.Fatalf("the older round's paste is %s; want cancelled", it.State)
	}
	if c := f.routeState(t, cur.RouteID); c.State != plugins.RouteRouted {
		t.Fatalf("the newer round's route is %s; want routed", c.State)
	}
	if it := f.queueRow(t, *cur.SendQueueID); it.State != sendqueue.Queued {
		t.Fatalf("the newer round's paste is %s; want queued", it.State)
	}
	// The chief is not told: no turn, no hand-back, even after the sweep.
	if _, err := f.srv.RouteTimeoutPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := f.turns.count(); n != 0 {
		t.Fatalf("the chief took %d turn(s); a superseded round takes none", n)
	}
	if note := f.chiefDefaultSessionText(t); strings.Contains(note, "back with you") || strings.Contains(note, prRouteSuperseded) {
		t.Fatalf("the chief was told of the superseded round: %q", note)
	}
	// Only the newer round is pasted: the older one's gate refuses it too.
	if out := f.deliverRow(t, *old.SendQueueID); out.Sent || f.mirror.pastes() != 0 {
		t.Fatalf("the superseded round: delivery %+v, %d paste(s); want refused", out, f.mirror.pastes())
	}
	if out := f.deliverRow(t, *cur.SendQueueID); !out.Sent || f.mirror.pastes() != 1 {
		t.Fatalf("delivery %+v, %d paste(s); want the newer round sent", out, f.mirror.pastes())
	}
}

// deliverRow hands send-queue row id to the send path, as the dispatcher would
// once it claimed the row.
func (f *routeFixture) deliverRow(t *testing.T, id int64) sendqueue.Outcome {
	t.Helper()
	return f.srv.sendQueueSend(f.srv.queuedPrincipal(context.Background(), f.owner), f.queueRow(t, id), sendqueue.AgentState{Idle: true})
}

// dispatchRow is the dispatcher's whole turn for row id: claimed, delivered,
// and finished with the outcome — so the row ends sent, as in production.
func (f *routeFixture) dispatchRow(t *testing.T, id int64) sendqueue.Outcome {
	t.Helper()
	if _, ok, err := f.queue.Claim(context.Background(), id, time.Now(), sendqueue.Claim{By: "test", Lease: time.Minute}); err != nil || !ok {
		t.Fatalf("claim %d: %v %v", id, ok, err)
	}
	out := f.deliverRow(t, id)
	if err := f.queue.Finish(context.Background(), id, "test", out, time.Now()); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAnOlderReviewRoundAlreadyPastedIsLeftAlone(t *testing.T) {
	f := newPRFixture(t)
	f.authored(t, f.asOwner, routeSID, prCreated())
	old := f.oneRoute(t, f.review(t, "sup-old-2", prSender, true, round(prNumber, shaRound1)))
	if out := f.dispatchRow(t, *old.SendQueueID); !out.Sent || f.queueRow(t, *old.SendQueueID).State != sendqueue.Sent {
		t.Fatalf("fixture: the older round's paste was not delivered: %+v", out)
	}
	cur := f.oneRoute(t, f.review(t, "sup-new-2", prSender, true, round(prNumber, shaRound2)))
	if o := f.routeState(t, old.RouteID); o.State != plugins.RouteRouted {
		t.Fatalf("the pasted older round's route is %s %q; want routed (the session may be answering it)", o.State, o.Error)
	}
	if c := f.routeState(t, cur.RouteID); c.State != plugins.RouteRouted {
		t.Fatalf("the newer round's route is %s; want routed", c.State)
	}
}

func TestAReviewOfAnotherPRDoesNotSupersede(t *testing.T) {
	f := newPRFixture(t)
	f.authored(t, f.asOwner, routeSID, prCreated())
	f.authored(t, f.asOwner, routeSID, prroute4818())
	a := f.oneRoute(t, f.review(t, "sup-pr-a", prSender, true, round(prNumber, shaRound1)))
	b := f.oneRoute(t, f.review(t, "sup-pr-b", prSender, true, round(prNumber+1, shaRound2)))
	for _, r := range []plugins.Route{a, b} {
		if got := f.routeState(t, r.RouteID); got.State != plugins.RouteRouted {
			t.Fatalf("route %s is %s %q; want routed (different pull requests)", r.RouteID, got.State, got.Error)
		}
		if it := f.queueRow(t, *r.SendQueueID); it.State != sendqueue.Queued {
			t.Fatalf("route %s's paste is %s; want queued", r.RouteID, it.State)
		}
	}
}

func TestARoundForAnotherSessionDoesNotSupersede(t *testing.T) {
	f := newPRFixture(t)
	f.mirror.set(func(m *routeMirror) { m.otherSession = prOtherID })
	f.authored(t, f.asOwner, routeSID, prCreated())
	old := f.oneRoute(t, f.review(t, "sup-sess-a", prSender, true, round(prNumber, shaRound1)))
	// Another session is now the pull request's most recent author.
	f.authored(t, f.asOwner, prOtherID, prCreated())
	cur := f.oneRoute(t, f.review(t, "sup-sess-b", prSender, true, round(prNumber, shaRound2)))
	if cur.TargetKey != prOtherID {
		t.Fatalf("fixture: the newer round went to %s; want %s", cur.TargetKey, prOtherID)
	}
	if o := f.routeState(t, old.RouteID); o.State != plugins.RouteRouted {
		t.Fatalf("the round for the other session is %s %q; want routed", o.State, o.Error)
	}
	if it := f.queueRow(t, *old.SendQueueID); it.State != sendqueue.Queued {
		t.Fatalf("its paste is %s; want queued", it.State)
	}
}

// --- Y2: a cancelled routed row ends its route, back to the chief now -------

func TestTheOwnersCancelOfARoutedMessageHandsItBackNow(t *testing.T) {
	f := newRouteFixture(t)
	p, m := f.pluginMessage(t, "example-inbox", "y2-cancel-1", "Ada <ada@example.test>", true)
	sess := f.chiefSession(t, p)
	id := f.route(t, m)
	r := f.routeState(t, id)
	req := httptest.NewRequest("POST", "/send-queue/"+strconv.FormatInt(*r.SendQueueID, 10)+"/cancel", nil).WithContext(f.asOwner)
	req.SetPathValue("id", strconv.FormatInt(*r.SendQueueID, 10))
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	w := httptest.NewRecorder()
	f.srv.handleSendQueueCancel(w, req)
	if w.Code != 204 {
		t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
	}
	got := f.routeState(t, id)
	if got.State != plugins.RouteFailed || got.Error != routePasteCancelled || f.handBackOwed(t, id) != routePasteCancelled {
		t.Fatalf("after the cancel the route is %s %q owing %q; want failed, owing the chief %q",
			got.State, got.Error, f.handBackOwed(t, id), routePasteCancelled)
	}
	if _, err := f.srv.RouteTimeoutPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.handedBack(t, sess.ID, id)
}

func TestASignOutEverywhereHandsARoutedMessageBack(t *testing.T) {
	f := newRouteFixture(t)
	p, m := f.pluginMessage(t, "example-inbox", "y2-owner-1", "Ada <ada@example.test>", true)
	sess := f.chiefSession(t, p)
	id := f.route(t, m)
	f.srv.cancelQueuedFor(context.Background(), f.owner, SendQueueCancelSignOut)
	if it := f.queueRow(t, *f.routeState(t, id).SendQueueID); it.State != sendqueue.Cancelled {
		t.Fatalf("fixture: the routed row is %s; want cancelled", it.State)
	}
	if _, err := f.srv.RouteTimeoutPass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.routeState(t, id); got.State != plugins.RouteFailed || got.Error != routePasteCancelled {
		t.Fatalf("after CancelOwner and a sweep the route is %s %q; want failed, %q", got.State, got.Error, routePasteCancelled)
	}
	f.handedBack(t, sess.ID, id)
}

// handedBack fails the test unless the chief's session sessID holds route id's
// hand-back, delivered (none still owed).
func (f *routeFixture) handedBack(t *testing.T, sessID int64, id string) {
	t.Helper()
	if hb := f.handBackOwed(t, id); hb != "" {
		t.Fatalf("the hand-back is still owed: %q", hb)
	}
	msgs, err := f.srv.ext.Agents.ListChatMessages(f.asOwner, sessID)
	if err != nil || len(msgs) == 0 || !strings.Contains(msgs[len(msgs)-1].Content, "Route "+id) ||
		!strings.Contains(msgs[len(msgs)-1].Content, "back with you") {
		t.Fatalf("the chief was not handed the message back: %+v %v", msgs, err)
	}
}

// --- Y3: a target window gone past the grace ends the route ------------------

func TestARoutedMessageWhoseWindowIsGoneIsHandedBackAfterTheGrace(t *testing.T) {
	f := newRouteFixture(t)
	p, m := f.pluginMessage(t, "example-inbox", "y3-gone-1", "Ada <ada@example.test>", true)
	sess := f.chiefSession(t, p)
	id := f.route(t, m)
	t0 := time.Now()
	now := t0
	f.srv.sendQueueClock = func() time.Time { return now }
	pass := func(at time.Duration) plugins.Route {
		t.Helper()
		now = t0.Add(at)
		if _, err := f.srv.RouteTimeoutPass(context.Background()); err != nil {
			t.Fatal(err)
		}
		return f.routeState(t, id)
	}
	// Positive control: the window is there, so nothing ends, however long.
	if r := pass(10 * time.Minute); r.State != plugins.RouteRouted {
		t.Fatalf("with its window present the route is %s %q; want routed", r.State, r.Error)
	}
	// The window goes: inside the grace nothing ends, and a reconnect resets it.
	f.mirror.set(func(m *routeMirror) { m.claudeSession = "5e55a1d0-0000-4000-8000-0000000c0ffe" })
	if r := pass(11 * time.Minute); r.State != plugins.RouteRouted {
		t.Fatalf("the window just gone: %s; want routed", r.State)
	}
	f.mirror.set(func(m *routeMirror) { m.claudeSession = "" })
	if r := pass(12*time.Minute + 30*time.Second); r.State != plugins.RouteRouted {
		t.Fatalf("the window back: %s; want routed", r.State)
	}
	f.mirror.set(func(m *routeMirror) { m.claudeSession = "5e55a1d0-0000-4000-8000-0000000c0ffe" })
	if r := pass(13 * time.Minute); r.State != plugins.RouteRouted {
		t.Fatalf("gone again, 30s: %s; want routed (the grace restarted)", r.State)
	}
	if r := pass(14*time.Minute + 30*time.Second); r.State != plugins.RouteRouted {
		t.Fatalf("gone again, 90s: %s; want routed", r.State)
	}
	r := pass(15*time.Minute + 10*time.Second)
	if r.State != plugins.RouteFailed || r.Error != routeWindowGone {
		t.Fatalf("gone for 2m10s: %s %q; want failed, %q", r.State, r.Error, routeWindowGone)
	}
	if it := f.queueRow(t, *r.SendQueueID); it.State != sendqueue.Cancelled {
		t.Fatalf("its paste is %s; want cancelled", it.State)
	}
	f.handedBack(t, sess.ID, id)
}

// prroute4818 is another pull request of the same repo, authored by the same
// session.
func prroute4818() prroute.Signal {
	return prroute.Signal{Kind: prroute.KindPRCreate, Repo: prRepo, PR: prNumber + 1}
}
