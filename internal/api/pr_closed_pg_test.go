package api

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/githubapp"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/sendqueue"
)

// A REVIEW OF A PULL REQUEST ALREADY MERGED OR CLOSED IS SETTLED BY MUSTER.
// Through the real server against a real Postgres, with GitHub's answer faked:
//   - a review arriving for a merged pull request is settled on arrival: no
//     route, no paste, no chief turn, no ask;
//   - a review whose route ends undelivered (timed out) after its pull request
//     closed is settled instead of handed back;
//   - a review the chief holds (no author recorded), and a review round a late
//     older round superseded before its paste, raise no "not acted on" ask once
//     their pull request is closed;
//   - the invariants: a review of an OPEN pull request still routes and still
//     goes back to the chief undelivered, and a pull request GitHub cannot
//     answer for is handled as open.
//
// Fixture values are invented; the closed-PR settle reason names the pull
// request, so a hardcoded reason cannot pass.

// fakePRStates is GitHub's answer per "<repo>#<n>", and how often it was asked.
type fakePRStates struct {
	mu    sync.Mutex
	state map[string]githubapp.PRState
	err   error
	calls int
}

func (f *fakePRStates) PullRequestState(_ context.Context, repo string, n int64) (githubapp.PRState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return githubapp.PRState{}, f.err
	}
	st, ok := f.state[repo+"#"+strconv.FormatInt(n, 10)]
	if !ok {
		return githubapp.PRState{}, errors.New("not found")
	}
	return st, nil
}

func (f *fakePRStates) set(st githubapp.PRState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state[prRepo+"#"+strconv.Itoa(prNumber)] = st
}

func (f *fakePRStates) asked() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

var (
	prOpen   = githubapp.PRState{State: "open"}
	prMerged = githubapp.PRState{State: "closed", Merged: true}
	prClosed = githubapp.PRState{State: "closed"}
)

// withPRStates wires the closed-PR settle with GitHub answering st.
func (f *prFixture) withPRStates(st githubapp.PRState) *fakePRStates {
	gh := &fakePRStates{state: map[string]githubapp.PRState{}}
	gh.set(st)
	f.srv.ext.ClosedPRs = &ClosedPRs{States: gh, Store: f.plugins}
	return gh
}

const closedPRName = prRepo + "#4817"

// sweptWithNoAsk runs the unrouted-message sweep as if m had waited an hour and
// reports whether it raised NO ask for m — asserting first that m WAS due one
// (the positive control: the sweep reached it, and something other than "not
// due" kept the ask away). askedAbout cannot serve here: it requires the sweep
// and UnroutedDue to agree, which a closed pull request's settle is the one
// deliberate exception to.
func (f *prFixture) sweptWithNoAsk(t *testing.T, m plugins.Message) bool {
	t.Helper()
	if f.srv.ext.Attention == nil {
		f.srv.ext.Attention = attention.NewPG(f.pool)
	}
	dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE plugin_messages SET delivered_at = now() - interval '1 hour' WHERE id = `+strconv.FormatInt(m.ID, 10))
	due, err := f.plugins.UnroutedDue(context.Background(), time.Now(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	isDue := false
	for _, d := range due {
		isDue = isDue || d.ID == m.ID
	}
	if !isDue {
		t.Fatalf("fixture: message %d was not due an ask, so the sweep never judged it", m.ID)
	}
	unroutedSweep(t, f.gatedFixture, 0)
	return len(unroutedAsks(t, f.gatedFixture, m.ID)) == 0
}

func TestAReviewOfAMergedPullRequestIsSettledOnArrival(t *testing.T) {
	f := newPRFixture(t)
	gh := f.withPRStates(prMerged)
	f.authored(t, f.asOwner, routeSID, prCreated())
	m := f.review(t, "closed-arrival-1", prSender, true, part(1, 1, shaRoundA))

	if rs := f.routesOf(t, m); len(rs) != 0 {
		t.Fatalf("a review of a merged pull request was routed: %+v", rs)
	}
	if n := f.mirror.pastes(); n != 0 {
		t.Fatalf("%d paste(s) reached the window; want none", n)
	}
	if n := f.turns.count(); n != 0 {
		t.Fatalf("the chief took %d turn(s) for it; want none", n)
	}
	want := "muster: a review of pull request " + closedPRName + ", which GitHub reports already merged: nothing left to act on"
	if r := settleReason(t, f.gatedFixture, m.ID); r != want {
		t.Fatalf("settle reason %q; want %q", r, want)
	}
	if f.askedAbout(t, m) {
		t.Fatal("a review of a merged pull request raised a \"not acted on\" ask")
	}
	if gh.asked() == 0 {
		t.Fatal("GitHub was never asked")
	}
}

func TestAnUndeliveredReviewWhosePullRequestClosedIsNotHandedBack(t *testing.T) {
	f := newPRFixture(t)
	gh := f.withPRStates(prOpen)
	f.authored(t, f.asOwner, routeSID, prCreated())
	m := f.review(t, "closed-handback-1", prSender, true, part(1, 1, shaRoundA))
	r := f.oneRoute(t, m)
	gh.set(prClosed)
	expire(t, r.RouteID)
	if n, err := f.srv.RouteTimeoutPass(context.Background()); err != nil || n != 1 {
		t.Fatalf("past the deadline: %d timed out (%v), want 1", n, err)
	}
	if got := f.routeState(t, r.RouteID); got.State != plugins.RouteTimedOut || got.Error != plugins.RouteNeverDeliveredError {
		t.Fatalf("the route: %s %q; want timed_out, never delivered", got.State, got.Error)
	}
	if f.handedBack(t) || f.turns.count() != 0 {
		t.Fatalf("the chief was handed a review of a closed pull request (%d turns): %q", f.turns.count(), f.chiefDefaultSessionText(t))
	}
	want := "muster: a review of pull request " + closedPRName + ", which GitHub reports already closed without being merged: nothing left to act on"
	if got := settleReason(t, f.gatedFixture, m.ID); got != want {
		t.Fatalf("settle reason %q; want %q", got, want)
	}
	if f.askedAbout(t, m) {
		t.Fatal("a review of a closed pull request raised a \"not acted on\" ask")
	}
}

func TestAReviewTheChiefHoldsRaisesNoAskOnceItsPullRequestIsMerged(t *testing.T) {
	f := newPRFixture(t)
	gh := f.withPRStates(prOpen)
	// No author recorded: the review goes to the chief.
	m := f.review(t, "closed-chief-1", prSender, true, part(1, 1, shaRoundA))
	if rs := f.routesOf(t, m); len(rs) != 0 {
		t.Fatalf("fixture: routed with no author: %+v", rs)
	}
	gh.set(prMerged)
	if !f.sweptWithNoAsk(t, m) {
		t.Fatal("a review of a merged pull request the chief held raised a \"not acted on\" ask")
	}
	if r := settleReason(t, f.gatedFixture, m.ID); !strings.HasPrefix(r, "muster: a review of pull request "+closedPRName) {
		t.Fatalf("settle reason %q; want muster's, naming the pull request", r)
	}
}

func TestASupersededUnsentRoundRaisesNoAskOnceItsPullRequestIsClosed(t *testing.T) {
	f := newPRFixture(t)
	gh := f.withPRStates(prOpen)
	f.authored(t, f.asOwner, routeSID, prCreated())
	newerMsg := f.review(t, "closed-super-newer-1", prSender, true, part(1, 1, shaRoundB))
	newer := f.oneRoute(t, newerMsg)
	f.oneRoute(t, f.review(t, "closed-super-older-1", prSender, true, part(1, 1, shaRoundA)))
	if got := f.routeState(t, newer.RouteID); got.State != plugins.RouteFailed || !strings.HasPrefix(got.Error, prRouteSuperseded) {
		t.Fatalf("fixture: the queued round is %s %q; want superseded", got.State, got.Error)
	}
	gh.set(prClosed)
	if !f.sweptWithNoAsk(t, newerMsg) {
		t.Fatal("a superseded round of a closed pull request raised a \"not acted on\" ask")
	}
	if r := settleReason(t, f.gatedFixture, newerMsg.ID); !strings.HasPrefix(r, "muster: a review of pull request "+closedPRName) {
		t.Fatalf("settle reason %q; want muster's, naming the pull request", r)
	}
}

// 🔴 INVARIANT GUARD. A review of an OPEN pull request is routed and pasted as
// before — and GitHub was asked, so this is the closed check answering "open",
// not a check that never ran.
func TestAReviewOfAnOpenPullRequestStillRoutes(t *testing.T) {
	f := newPRFixture(t)
	gh := f.withPRStates(prOpen)
	f.authored(t, f.asOwner, routeSID, prCreated())
	m := f.review(t, "open-routes-1", prSender, true, part(1, 1, shaRoundA))
	r := f.oneRoute(t, m)
	if got := f.routeState(t, r.RouteID); got.State != plugins.RouteRouted {
		t.Fatalf("the route is %s; want routed", got.State)
	}
	if it := f.queueRow(t, *r.SendQueueID); it.State != sendqueue.Queued {
		t.Fatalf("the paste is %s; want queued", it.State)
	}
	if r := settleReason(t, f.gatedFixture, m.ID); r != "" {
		t.Fatalf("a review of an open pull request was settled: %q", r)
	}
	if gh.asked() == 0 {
		t.Fatal("GitHub was never asked: the check did not run")
	}
}

// 🔴 INVARIANT GUARD. An undelivered review of an OPEN pull request — and one
// GitHub cannot answer for — still goes back to the chief, unsettled.
func TestAnUndeliveredReviewOfAnOpenOrUnknownPullRequestStillGoesBack(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{{"open", nil}, {"unknown", errors.New("no installation for that owner")}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPRFixture(t)
			gh := f.withPRStates(prOpen)
			f.authored(t, f.asOwner, routeSID, prCreated())
			m := f.review(t, "open-handback-"+tc.name, prSender, true, part(1, 1, shaRoundA))
			r := f.oneRoute(t, m)
			gh.mu.Lock()
			gh.err = tc.err
			gh.mu.Unlock()
			before := gh.asked()
			expire(t, r.RouteID)
			if n, err := f.srv.RouteTimeoutPass(context.Background()); err != nil || n != 1 {
				t.Fatalf("past the deadline: %d timed out (%v), want 1", n, err)
			}
			if !f.handedBack(t) {
				t.Fatalf("an undelivered review of an %s pull request was not handed back: %q", tc.name, f.chiefDefaultSessionText(t))
			}
			if got := settleReason(t, f.gatedFixture, m.ID); got != "" {
				t.Fatalf("an undelivered review of an %s pull request was settled %q", tc.name, got)
			}
			if gh.asked() == before {
				t.Fatal("GitHub was not asked at the hand-back: the check did not run")
			}
		})
	}
}
