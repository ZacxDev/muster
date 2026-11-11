package api

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/sendqueue"
)

// A PULL-REQUEST REVIEW ROUTE THAT FAILED ONLY BECAUSE A NEWER ROUND SUPERSEDED
// IT IS HANDLED (operator decision, 0.191; plugins' supersededByNewerRound):
// its message raises no "not acted on" ask, whether or not its paste was sent.
// The superseding round's message answers for the review: when ITS route fails
// for a real reason, the ask fires on it — the newest message — and only on
// it. A route that failed for a real reason still raises the ask.
//
// Through the real server, sweep and Postgres; every repository, sha and id is
// the pr fixture's invented one.

// supersededErr is the error of a route superseded by route newer's review of
// a NEWER head (plugins.SupersededByNewerHeadError), spelled from the base's
// own constant (so this file compiles on the pre-change tree and goes red there
// for a behavioural reason).
func supersededErr(newer string) string {
	return prRouteSuperseded + ", a newer head commit (route " + newer + ")"
}

// supersededByArrivalErr is the error of a route superseded on arrival alone
// (the heads' order unknown: plugins.SupersededRouteError).
func supersededByArrivalErr(newer string) string { return prRouteSuperseded + " (route " + newer + ")" }

// supersededPair routes an older round (its paste left queued, as the false
// alarms' were) and then a newer one to the same session: the older message
// and route, the newer message and route.
func (f *prFixture) supersededPair(t *testing.T, prefix string) (plugins.Message, plugins.Route, plugins.Message, plugins.Route) {
	t.Helper()
	f.withPRCommits(shaRoundA, shaRoundB) // the pull request's commits: A, then B
	f.authored(t, f.asOwner, routeSID, prCreated())
	oldMsg := f.review(t, prefix+"-old", prSender, true, part(1, 1, shaRoundA))
	old := f.oneRoute(t, oldMsg)
	if it := f.queueRow(t, *old.SendQueueID); it.State != sendqueue.Queued {
		t.Fatalf("fixture: the older round's paste is %s; want queued", it.State)
	}
	newMsg := f.review(t, prefix+"-new", prSender, true, part(1, 1, shaRoundB))
	cur := f.oneRoute(t, newMsg)
	if got := f.routeState(t, old.RouteID); got.State != plugins.RouteFailed || got.Error != supersededErr(cur.RouteID) {
		t.Fatalf("fixture: the older route is %s %q; want failed, %q", got.State, got.Error, supersededErr(cur.RouteID))
	}
	if r := settleReason(t, f.gatedFixture, oldMsg.ID); r != "" {
		t.Fatalf("fixture: the unsent superseded round was settled %q; the sweep must decide it", r)
	}
	return oldMsg, old, newMsg, cur
}

// TestALateReviewOfAnOlderHeadSupersedesNothing (REGRESSION, the #413
// invariant; red at #586's first head): the pull request's commits are A then
// B. B's review is routed (its paste still queued); a LATE review of A arrives.
// The newest head wins: B's route stays routed and its paste queued — the late
// older review hides nothing — and A's review is routed beside it.
func TestALateReviewOfAnOlderHeadSupersedesNothing(t *testing.T) {
	f := newPRFixture(t)
	f.withPRCommits(shaRoundA, shaRoundB)
	f.authored(t, f.asOwner, routeSID, prCreated())
	newerMsg := f.review(t, "late-old-head-b", prSender, true, part(1, 1, shaRoundB))
	newer := f.oneRoute(t, newerMsg)
	late := f.oneRoute(t, f.review(t, "late-old-head-a", prSender, true, part(1, 1, shaRoundA)))
	if got := f.routeState(t, newer.RouteID); got.State != plugins.RouteRouted {
		t.Fatalf("the newer head's route is %s %q after a late review of an older head; want routed", got.State, got.Error)
	}
	if it := f.queueRow(t, *newer.SendQueueID); it.State != sendqueue.Queued {
		t.Fatalf("the newer head's paste is %s; want still queued", it.State)
	}
	if got := f.routeState(t, late.RouteID); got.State != plugins.RouteRouted {
		t.Fatalf("the late older head's route is %s; want routed", got.State)
	}
	if f.askedAbout(t, newerMsg) {
		t.Fatal("the newer head's live route raised a \"not acted on\" ask")
	}
}

// TestAnUnknownHeadOrderStillRaisesTheAsk (red at #586's first head): with no
// way to read the pull request's commits, a later-arriving round supersedes by
// arrival (as before) and that is NOT handled — the superseded message still
// raises its ask, since muster cannot tell it was the older head.
func TestAnUnknownHeadOrderStillRaisesTheAsk(t *testing.T) {
	f := newPRFixture(t)
	f.authored(t, f.asOwner, routeSID, prCreated())
	oldMsg := f.review(t, "unknown-order-a", prSender, true, part(1, 1, shaRoundA))
	old := f.oneRoute(t, oldMsg)
	cur := f.oneRoute(t, f.review(t, "unknown-order-b", prSender, true, part(1, 1, shaRoundB)))
	if got := f.routeState(t, old.RouteID); got.State != plugins.RouteFailed || got.Error != supersededByArrivalErr(cur.RouteID) {
		t.Fatalf("the earlier round's route: %s %q; want failed, %q", got.State, got.Error, supersededByArrivalErr(cur.RouteID))
	}
	if !f.askedAbout(t, oldMsg) {
		t.Fatal("a round superseded by arrival alone (head order unknown) raised no ask")
	}
}

// TestARouteSupersededByANewerRoundRaisesNoAsk (REGRESSION, red at the base):
// the older round's paste was never sent and its route failed only as
// superseded; the newer round's route is live. Neither message raises an ask.
func TestARouteSupersededByANewerRoundRaisesNoAsk(t *testing.T) {
	f := newPRFixture(t)
	oldMsg, _, newMsg, cur := f.supersededPair(t, "sup-only")
	if got := f.routeState(t, cur.RouteID); got.State != plugins.RouteRouted {
		t.Fatalf("the newer round's route is %s; want routed", got.State)
	}
	if f.askedAbout(t, oldMsg) {
		t.Fatal("a review round superseded by a newer one raised a \"not acted on\" ask")
	}
	if f.askedAbout(t, newMsg) {
		t.Fatal("the newer round, routed and live, raised a \"not acted on\" ask")
	}
}

// TestASupersedingRouteThatFailsRaisesTheAskOnTheNewestMessage: the newer
// round's own route fails for a real reason. The ask fires on the NEWER
// message, and still not on the superseded one (one ask, on the newest).
func TestASupersedingRouteThatFailsRaisesTheAskOnTheNewestMessage(t *testing.T) {
	f := newPRFixture(t)
	oldMsg, _, newMsg, cur := f.supersededPair(t, "sup-fail")
	if _, err := f.plugins.FailRoute(f.asOwner, cur.RouteID, "the window moved on", ""); err != nil {
		t.Fatal(err)
	}
	if !f.askedAbout(t, newMsg) {
		t.Fatal("the superseding round's route failed for a real reason and its message raised no ask")
	}
	if f.askedAbout(t, oldMsg) {
		t.Fatal("the superseded round raised an ask too; the newest message carries it")
	}
}

// TestAnAskAlreadyRaisedForASupersededRoundIsClosed: the false alarms raised
// before this rule (the older round's ask is open) are closed by the sweep
// once it runs with the rule. The ask is raised here the only way left — the
// route failing for a real reason — and its error is then rewritten to the
// supersede the older code recorded.
func TestAnAskAlreadyRaisedForASupersededRoundIsClosed(t *testing.T) {
	f := newPRFixture(t)
	oldMsg, old, _, cur := f.supersededPair(t, "sup-close")
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE session_routes SET error = 'the window moved on' WHERE route_id = '`+old.RouteID+`'`)
	if !f.askedAbout(t, oldMsg) {
		t.Fatal("fixture: no ask was raised for the failed route")
	}
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE session_routes SET error = '`+supersededErr(cur.RouteID)+`' WHERE route_id = '`+old.RouteID+`'`)
	unroutedSweep(t, f.gatedFixture, 0)
	asks := unroutedAsks(t, f.gatedFixture, oldMsg.ID)
	if len(asks) != 1 || asks[0].Status == "open" {
		t.Fatalf("the superseded round's ask: %+v; want the one ask, no longer open", asks)
	}
}

// TestARealRouteFailureStillRaisesTheAsk (INVARIANT GUARD: green at the base
// too): a review route that failed for a real reason — no newer round — is
// unhandled, and so is one whose error names a superseding route that does not
// exist (the predicate needs the newer route itself, not the words).
func TestARealRouteFailureStillRaisesTheAsk(t *testing.T) {
	t.Run("a real failure", func(t *testing.T) {
		f := newPRFixture(t)
		f.authored(t, f.asOwner, routeSID, prCreated())
		m := f.review(t, "real-fail-1", prSender, true, part(1, 1, shaRoundA))
		r := f.oneRoute(t, m)
		if _, err := f.plugins.FailRoute(f.asOwner, r.RouteID, "the window moved on", ""); err != nil {
			t.Fatal(err)
		}
		if !f.askedAbout(t, m) {
			t.Fatal("a review route that failed for a real reason raised no ask")
		}
	})
	t.Run("superseded by a route that is not there", func(t *testing.T) {
		f := newPRFixture(t)
		f.authored(t, f.asOwner, routeSID, prCreated())
		// Another message's route exists (an earlier round, which m's
		// supersedes), so "some other route" cannot pass for "the named one".
		f.oneRoute(t, f.review(t, "ghost-sup-0", prSender, true, part(1, 1, shaRoundB)))
		m := f.review(t, "ghost-sup-1", prSender, true, part(1, 1, shaRoundA))
		r := f.oneRoute(t, m)
		ghost := supersededErr("rt_" + strings.Repeat("0f", 12))
		if _, err := f.plugins.FailRoute(f.asOwner, r.RouteID, ghost, ""); err != nil {
			t.Fatal(err)
		}
		if got := f.routeState(t, r.RouteID); got.Error != ghost {
			t.Fatalf("fixture: the route's error is %q; want %q", got.Error, ghost)
		}
		if !f.askedAbout(t, m) {
			t.Fatal("a route 'superseded' by a route that does not exist raised no ask")
		}
	})
}

// TestASupersededRoundOfTheSameMessageIsNoExcuse: a route whose error names
// a route of the SAME message (not a newer round's) is not handled by it.
func TestASupersededRoundOfTheSameMessageIsNoExcuse(t *testing.T) {
	f := newPRFixture(t)
	f.authored(t, f.asOwner, routeSID, prCreated())
	m := f.review(t, "self-sup-1", prSender, true, part(1, 1, shaRoundA))
	r := f.oneRoute(t, m)
	if _, err := f.plugins.FailRoute(f.asOwner, r.RouteID, supersededErr(r.RouteID), ""); err != nil {
		t.Fatal(err)
	}
	if !f.askedAbout(t, m) {
		t.Fatal("a route naming its own message's route as the newer round raised no ask")
	}
}
