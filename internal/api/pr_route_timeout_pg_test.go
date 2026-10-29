package api

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/sendqueue"
)

// A ROUTE'S DEADLINE BY WHO MADE IT (routeTimeout): a route muster makes for a
// pull-request review (plugins.RouteViaPR) waits PRRouteTimeout (4h); the
// chief's route and a route by thread keep RouteTimeout (30m). The session
// that authored a pull request is usually still working, its paste waits for
// the window to go idle, and a 30-minute deadline handed the review back to
// the chief before it was ever pasted.

// deadlineIs fails the test unless route r's deadline is want after it was
// made (the row's own created_at and timeout_at, both stamped by the store).
func deadlineIs(t *testing.T, what string, r plugins.Route, want time.Duration) {
	t.Helper()
	if got := r.TimeoutAt.Sub(r.CreatedAt); got < want-5*time.Second || got > want+5*time.Second {
		t.Errorf("%s: the route's deadline is %v after it was made, want %v", what, got, want)
	}
}

func TestAPullRequestReviewRouteHasTheFourHourDeadline(t *testing.T) {
	f := newPRFixture(t)
	f.authored(t, f.asOwner, routeSID, prCreated())
	m := f.review(t, "pr-deadline-1", prSender, true, reviewBody(fullFields()))
	rs := f.routesOf(t, m)
	if len(rs) != 1 || rs[0].State != plugins.RouteRouted {
		t.Fatalf("routes: %+v", rs)
	}
	deadlineIs(t, "a route by pull request", rs[0], 4*time.Hour)
	if d := time.Until(rs[0].TimeoutAt); d < 4*time.Hour-time.Minute || d > 4*time.Hour+time.Minute {
		t.Errorf("timeout_at is %v from now, want about 4h", d)
	}
}

func TestAChiefRouteKeepsTheThirtyMinuteDeadline(t *testing.T) {
	f := newRouteFixture(t)
	id := f.route(t, f.deliveredMessage(t, f.owner, f.chief.ID, "chief-deadline"))
	deadlineIs(t, "the chief's route", f.routeState(t, id), 30*time.Minute)
}

func TestAThreadRouteKeepsTheThirtyMinuteDeadline(t *testing.T) {
	f := newThreadFixture(t)
	if code, _ := f.bind(t, "", "5151", routeSID); code != 201 {
		t.Fatal("bind")
	}
	_, m := f.post(t, f.mailTok, "thr-deadline-1", "5151")
	rs := f.routesOf(t, m)
	if len(rs) != 1 || rs[0].State != plugins.RouteRouted {
		t.Fatalf("routes: %+v", rs)
	}
	deadlineIs(t, "a route by thread", rs[0], 30*time.Minute)
}

// ageRoute moves route id, and its queued paste, back by d: as if they were
// made d ago. The store stamps both from the database clock, so this is how a
// test lets time pass.
func ageRoute(t *testing.T, id string, d time.Duration) {
	t.Helper()
	iv := "interval '" + strconv.Itoa(int(d/time.Minute)) + " minutes'"
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE session_routes SET created_at = created_at - `+iv+
		`, timeout_at = timeout_at - `+iv+` WHERE route_id = '`+id+`'`)
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE send_queue SET created_at = created_at - `+iv+
		` WHERE route_id = '`+id+`'`)
}

// 🔴 The paste outlives 30 minutes with its PR route: 31 minutes on, the route
// sweep times nothing out, the queue's own expiry leaves the row queued, and
// the delivery pastes it. The control — a chief route aged the same — is
// timed out and its paste cancelled, so the aging does reach the sweep.
func TestAPullRequestRoutesQueuedPasteSurvivesPastThirtyMinutes(t *testing.T) {
	f := newPRFixture(t)
	f.authored(t, f.asOwner, routeSID, prCreated())
	m := f.review(t, "pr-survive-1", prSender, true, reviewBody(fullFields()))
	rs := f.routesOf(t, m)
	if len(rs) != 1 || rs[0].SendQueueID == nil {
		t.Fatalf("routes: %+v", rs)
	}
	ageRoute(t, rs[0].RouteID, 31*time.Minute)
	if n, err := f.srv.RouteTimeoutPass(context.Background()); err != nil || n != 0 {
		t.Fatalf("31m on: the sweep timed out %d (%v), want 0", n, err)
	}
	if r := f.routeState(t, rs[0].RouteID); r.State != plugins.RouteRouted {
		t.Fatalf("31m on: the route is %s, want routed", r.State)
	}
	if _, err := f.queue.Expire(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	items := f.queued(t)
	if len(items) != 1 || items[0].ID != *rs[0].SendQueueID || items[0].State != sendqueue.Queued {
		t.Fatalf("31m on: the paste is %+v, want still queued", items)
	}
	if out := f.deliver(t); !out.Sent || f.mirror.pastes() != 1 {
		t.Fatalf("31m on: delivery %+v with %d paste(s); want sent, 1", out, f.mirror.pastes())
	}
}

func TestAChiefRoutesQueuedPasteIsCancelledPastThirtyMinutes(t *testing.T) {
	f := newRouteFixture(t)
	id := f.route(t, f.deliveredMessage(t, f.owner, f.chief.ID, "chief-aged"))
	ageRoute(t, id, 31*time.Minute)
	if n, err := f.srv.RouteTimeoutPass(context.Background()); err != nil || n < 1 {
		t.Fatalf("31m on: the sweep timed out %d (%v), want the chief's route", n, err)
	}
	if r := f.routeState(t, id); r.State != plugins.RouteTimedOut {
		t.Fatalf("31m on: the route is %s, want timed_out", r.State)
	}
	if items := f.queued(t); len(items) != 1 || items[0].State != sendqueue.Cancelled {
		t.Fatalf("31m on: the paste is %+v, want cancelled", items)
	}
}
