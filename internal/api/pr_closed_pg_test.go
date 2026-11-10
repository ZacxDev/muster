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

// fakePRStates is GitHub's answer per "<repo>#<n>", how often it was asked,
// and the closed-PR cache's clock. block makes every read wait for its
// context's deadline (a GitHub that never answers).
type fakePRStates struct {
	mu    sync.Mutex
	state map[string]githubapp.PRState
	err   error
	block bool
	calls int
	now   time.Time
}

func (f *fakePRStates) PullRequestState(ctx context.Context, repo string, n int64) (githubapp.PRState, error) {
	f.mu.Lock()
	f.calls++
	block, err := f.block, f.err
	st, ok := f.state[repo+"#"+strconv.FormatInt(n, 10)]
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return githubapp.PRState{}, ctx.Err()
	}
	if err != nil {
		return githubapp.PRState{}, err
	}
	if !ok {
		return githubapp.PRState{}, errors.New("not found")
	}
	return st, nil
}

// answer sets GitHub's answer for pull request n of prRepo.
func (f *fakePRStates) answer(n int, st githubapp.PRState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state[prRepo+"#"+strconv.Itoa(n)] = st
}

// set changes GitHub's answer for the fixture's pull request AND moves the
// cache's clock past openPRTTL: the pull request changed later, once muster's
// earlier open answer had expired.
func (f *fakePRStates) set(st githubapp.PRState) {
	f.answer(prNumber, st)
	f.later(openPRTTL + time.Minute)
}

// later moves the cache's clock on by d.
func (f *fakePRStates) later(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

func (f *fakePRStates) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakePRStates) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
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

// withPRStates wires the closed-PR settle with GitHub answering st for the
// fixture's pull request, on the fake's clock.
func (f *prFixture) withPRStates(st githubapp.PRState) *fakePRStates {
	gh := &fakePRStates{state: map[string]githubapp.PRState{}, now: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)}
	gh.answer(prNumber, st)
	f.srv.ext.ClosedPRs = &ClosedPRs{States: gh, Store: f.plugins, Now: gh.clock}
	return gh
}

// reviewOf is a one-part review of pull request n of prRepo at head sha.
func reviewOf(n int, sha string) string {
	return "Review of pull request " + strconv.Itoa(n) + ": one finding.\n\n```json\n{\"repo\":\"" + prRepo + "\",\"pr\":" +
		strconv.Itoa(n) + ",\"head_sha\":\"" + sha + "\"}\n```\n"
}

const closedPRName = prRepo + "#4817"

// closedUnmergedNote is the chief's note for message id, settled because its
// pull request (the fixture's) was closed WITHOUT being merged.
func closedUnmergedNote(id int64) string {
	return "[muster] plugin message_id " + strconv.FormatInt(id, 10) + " is a review of pull request " + closedPRName +
		", which GitHub reports closed WITHOUT being merged, so muster settled it. If that work was re-filed as another " +
		"pull request, its findings may still apply there."
}

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
	// The usage page's record: muster's own path, by name (chief_usage.go).
	if h, _, _ := handledBy(t, f.routeFixture, m.ID); h != "closed_pr" {
		t.Fatalf("handled by %q, want closed_pr", h)
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
	want := "muster: a review of pull request " + closedPRName + ", which GitHub reports already closed WITHOUT being merged: nothing left to act on there"
	if got := settleReason(t, f.gatedFixture, m.ID); got != want {
		t.Fatalf("settle reason %q; want %q", got, want)
	}
	if !strings.Contains(f.chiefDefaultSessionText(t), closedUnmergedNote(m.ID)) {
		t.Fatalf("a closed, unmerged pull request's settle left the chief no note: %q", f.chiefDefaultSessionText(t))
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
	if !strings.Contains(f.chiefDefaultSessionText(t), closedUnmergedNote(newerMsg.ID)) {
		t.Fatalf("a closed, unmerged pull request's settle on the ask path left the chief no note: %q", f.chiefDefaultSessionText(t))
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
			gh.setErr(tc.err)
			gh.later(openPRTTL + time.Minute)
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

// --- audit of #417, round 1 --------------------------------------------------

// 🔴 REGRESSION (🟡-3): a review of a pull request closed WITHOUT being merged
// is settled with a reason saying so, and the chief is left a one-line note;
// a merged one's settle leaves the chief nothing.
func TestAClosedUnmergedReviewOnArrivalLeavesTheChiefANote(t *testing.T) {
	f := newPRFixture(t)
	f.withPRStates(prClosed)
	f.authored(t, f.asOwner, routeSID, prCreated())
	m := f.review(t, "unmerged-arrival-1", prSender, true, part(1, 1, shaRoundA))
	want := "muster: a review of pull request " + closedPRName + ", which GitHub reports already closed WITHOUT being merged: nothing left to act on there"
	if r := settleReason(t, f.gatedFixture, m.ID); r != want {
		t.Fatalf("settle reason %q; want %q", r, want)
	}
	if text := f.chiefDefaultSessionText(t); !strings.Contains(text, closedUnmergedNote(m.ID)) {
		t.Fatalf("no note for the chief: %q", text)
	}
	if n := f.turns.count(); n != 0 {
		t.Fatalf("the note cost the chief %d turn(s); want none", n)
	}
	// Control: a merged pull request's review leaves no note.
	g := newPRFixture(t)
	g.withPRStates(prMerged)
	g.authored(t, g.asOwner, routeSID, prCreated())
	gm := g.review(t, "merged-arrival-nonote-1", prSender, true, part(1, 1, shaRoundA))
	if r := settleReason(t, g.gatedFixture, gm.ID); !strings.HasSuffix(r, "already merged: nothing left to act on") {
		t.Fatalf("control: merged settle reason %q", r)
	}
	if text := g.chiefDefaultSessionText(t); strings.Contains(text, "WITHOUT being merged") {
		t.Fatalf("a merged pull request's settle left the chief a note: %q", text)
	}
}

// 🟢-1: an unknown answer on ARRIVAL — a GitHub error, and a GitHub that never
// answers (cut at the read's timeout) — routes the review as before.
func TestAnUnknownStateOnArrivalStillRoutes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		block bool
	}{{"error", errors.New("GitHub answered 502"), false}, {"timeout", nil, true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPRFixture(t)
			gh := f.withPRStates(prMerged) // what a later answer would say; never reached
			gh.setErr(tc.err)
			gh.mu.Lock()
			gh.block = tc.block
			gh.mu.Unlock()
			f.srv.ext.ClosedPRs.Timeout = 100 * time.Millisecond
			f.authored(t, f.asOwner, routeSID, prCreated())
			start := time.Now()
			m := f.review(t, "unknown-arrival-"+tc.name, prSender, true, part(1, 1, shaRoundA))
			if el := time.Since(start); el > 3*time.Second {
				t.Fatalf("arrival took %s; want it cut at the 100ms read timeout", el)
			}
			r := f.oneRoute(t, m)
			if got := f.routeState(t, r.RouteID); got.State != plugins.RouteRouted {
				t.Fatalf("the route is %s; want routed", got.State)
			}
			if got := settleReason(t, f.gatedFixture, m.ID); got != "" {
				t.Fatalf("a review whose pull request's state is unknown was settled: %q", got)
			}
			if gh.asked() == 0 {
				t.Fatal("GitHub was never asked")
			}
		})
	}
}

// 🟢-1: an unknown answer on the ASK path raises the ask as before.
func TestAnUnknownStateOnTheAskPathStillAsks(t *testing.T) {
	f := newPRFixture(t)
	gh := f.withPRStates(prOpen)
	m := f.review(t, "unknown-ask-1", prSender, true, part(1, 1, shaRoundA)) // no author: the chief holds it
	gh.answer(prNumber, prMerged)
	gh.setErr(errors.New("GitHub answered 502"))
	gh.later(openPRTTL + time.Minute)
	before := gh.asked()
	if f.sweptWithNoAsk(t, m) {
		t.Fatal("an unknown state on the ask path raised no ask")
	}
	if gh.asked() == before {
		t.Fatal("GitHub was not asked on the ask path")
	}
	if r := settleReason(t, f.gatedFixture, m.ID); r != "" {
		t.Fatalf("settled on an unknown state: %q", r)
	}
}

// 🔴 REGRESSION (🟡-2): GitHub's answer is cached per pull request — closed for
// closedPRTTL, open for openPRTTL — so reviews of one pull request do not each
// cost a read.
func TestTheClosedPRStateIsCachedPerPullRequest(t *testing.T) {
	f := newPRFixture(t)
	gh := f.withPRStates(prMerged)
	f.authored(t, f.asOwner, routeSID, prCreated())
	f.review(t, "cache-merged-1", prSender, true, part(1, 1, shaRoundA))
	f.review(t, "cache-merged-2", prSender, true, part(1, 1, shaRoundB))
	if n := gh.asked(); n != 1 {
		t.Fatalf("%d reads for two reviews of one merged pull request; want 1 (cached)", n)
	}
	gh.later(closedPRTTL - time.Minute)
	f.review(t, "cache-merged-3", prSender, true, part(1, 1, shaRoundA))
	if n := gh.asked(); n != 1 {
		t.Fatalf("%d reads within the closed TTL; want 1", n)
	}

	// An open answer is reused within openPRTTL and read again after it.
	const other = 5150
	gh.answer(other, prOpen)
	f.review(t, "cache-open-1", prSender, true, reviewOf(other, shaRoundA))
	f.review(t, "cache-open-2", prSender, true, reviewOf(other, shaRoundB))
	if n := gh.asked(); n != 2 {
		t.Fatalf("%d reads after two reviews of an open pull request; want 2 (its answer cached)", n)
	}
	gh.later(openPRTTL + time.Minute)
	gh.answer(other, prMerged)
	m := f.review(t, "cache-open-3", prSender, true, reviewOf(other, shaRoundA))
	if n := gh.asked(); n != 3 {
		t.Fatalf("%d reads once the open answer expired; want 3", n)
	}
	if r := settleReason(t, f.gatedFixture, m.ID); !strings.HasSuffix(r, "already merged: nothing left to act on") {
		t.Fatalf("the re-read merged answer did not settle the review: %q", r)
	}
}

// 🔴 REGRESSION (🟡-2): one sweep pass makes at most prReadsPerPass GitHub
// reads; past that the state is unknown and the ask is raised as before.
func TestASweepPassCapsItsGitHubReads(t *testing.T) {
	f := newPRFixture(t)
	gh := f.withPRStates(prOpen)
	if f.srv.ext.Attention == nil {
		f.srv.ext.Attention = attention.NewPG(f.pool)
	}
	const n = prReadsPerPass + 2
	var ms []plugins.Message
	for i := 0; i < n; i++ {
		pr := 6000 + i
		gh.answer(pr, prOpen)
		ms = append(ms, f.review(t, "cap-"+strconv.Itoa(i), prSender, true, reviewOf(pr, shaRoundA))) // no author: the chief holds it
	}
	for i := 0; i < n; i++ {
		gh.answer(6000+i, prMerged)
	}
	gh.later(openPRTTL + time.Minute)
	for _, m := range ms {
		dbtest.OwnerExec(t, dbtest.DSN(t), `UPDATE plugin_messages SET delivered_at = now() - interval '1 hour' WHERE id = `+strconv.FormatInt(m.ID, 10))
	}
	before := gh.asked()
	unroutedSweep(t, f.gatedFixture, 0)
	if reads := gh.asked() - before; reads != prReadsPerPass {
		t.Fatalf("one sweep pass made %d GitHub reads; want the cap, %d", reads, prReadsPerPass)
	}
	settled, asked := 0, 0
	for _, m := range ms {
		if strings.HasPrefix(settleReason(t, f.gatedFixture, m.ID), "muster: a review of pull request") {
			settled++
		}
		if len(unroutedAsks(t, f.gatedFixture, m.ID)) > 0 {
			asked++
		}
	}
	if settled != prReadsPerPass || asked != n-prReadsPerPass {
		t.Fatalf("settled %d, asked %d; want %d settled (within the cap) and %d asked (over it: unknown)", settled, asked, prReadsPerPass, n-prReadsPerPass)
	}
}
