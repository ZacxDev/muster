package officership

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
)

// rolloutRig: a fake service health endpoint, a fake escalation webhook, a
// fake clock that Sleep advances, and a watcher over the release fixture.
type rolloutRig struct {
	w      *Watcher
	gh     *fakeGH
	mu     sync.Mutex
	clock  time.Time
	health func(now time.Time) (int, map[string]any)
	hooks  []string
}

func newRolloutRig(t *testing.T, policy string) *rolloutRig {
	rr := &rolloutRig{clock: fxNow}
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code, body := rr.health(rr.now())
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(svc.Close)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]string
		_ = json.NewDecoder(r.Body).Decode(&m)
		rr.mu.Lock()
		rr.hooks = append(rr.hooks, m["content"])
		rr.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(hook.Close)
	g, gh, _, _ := greenReleaseFixture()
	sp := fxShip()
	sp.Rollout.HealthURL, sp.Rollout.Rollback, sp.Rollout.OKKey, sp.Rollout.OKValue = svc.URL, policy, "db", "ok"
	sp = sp.WithEscalateWebhook(hook.URL)
	g.Ship = sp
	rr.gh = gh
	rr.w = &Watcher{OfficerID: "engineering:example-web", Repo: fxRepo, Ship: sp, GH: gh, Gates: g, HTTP: svc.Client(), Now: rr.now,
		Sleep: func(ctx context.Context, d time.Duration) error {
			rr.mu.Lock()
			rr.clock = rr.clock.Add(d)
			rr.mu.Unlock()
			return ctx.Err()
		}}
	return rr
}

func (rr *rolloutRig) now() time.Time { rr.mu.Lock(); defer rr.mu.Unlock(); return rr.clock }

func (rr *rolloutRig) escalations() []string {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	return append([]string(nil), rr.hooks...)
}

func (rr *rolloutRig) runTo(t *testing.T, rel TagOutcome) WatchStatus {
	t.Helper()
	if err := rr.w.Start(context.Background(), rel, "item-7"); err != nil {
		t.Fatal(err)
	}
	rr.w.Wait()
	cur, _ := rr.w.Status()
	return *cur
}

var fxRelease = TagOutcome{Tag: "v1.5.0", SHA: fxMain, Previous: "v1.4.0", PreviousSHA: fxPrevTag, TaggedAt: fxNow}

func up(version string) func(time.Time) (int, map[string]any) {
	return func(time.Time) (int, map[string]any) { return 200, map[string]any{"version": version, "db": "ok"} }
}

// TestARolloutThatGoesLiveAndStaysHealthyIsReportedAndNotEscalated.
func TestARolloutThatGoesLiveAndStaysHealthyIsReportedAndNotEscalated(t *testing.T) {
	rr := newRolloutRig(t, officers.RollbackAuto)
	rr.health = up("v1.5.0")
	st := rr.runTo(t, fxRelease)
	if st.Verdict != VerdictHealthy || !st.Done || st.Escalated || st.ItemID != "item-7" {
		t.Fatalf("status %+v", st)
	}
	if n := len(rr.escalations()); n != 0 {
		t.Fatalf("a healthy release escalated %d time(s)", n)
	}
	if tag, busy := rr.w.InFlight(); busy {
		t.Fatalf("still in flight (%s) after the verdict", tag)
	}
}

// TestARolloutThatNeverGoesLiveEscalatesOutsideHQAndTouchesNothing: policy
// off — escalated through the webhook, no revert branch, no pull request.
func TestARolloutThatNeverGoesLiveEscalatesOutsideHQAndTouchesNothing(t *testing.T) {
	rr := newRolloutRig(t, officers.RollbackOff)
	rr.health = up("v1.4.0") // the old release keeps answering
	st := rr.runTo(t, fxRelease)
	if st.Verdict != VerdictNotLive || !st.Escalated {
		t.Fatalf("status %+v", st)
	}
	hooks := rr.escalations()
	if len(hooks) != 1 || !strings.Contains(hooks[0], "v1.5.0") || !strings.Contains(hooks[0], "not_live") {
		t.Fatalf("escalations %q", hooks)
	}
	if len(rr.gh.createdRefs) != 0 {
		t.Fatalf("policy off created %v", rr.gh.createdRefs)
	}
}

// TestAnUnhealthyReleaseOpensTheRevertPullRequest: live, then three bad
// answers in a row → unhealthy; policy pr opens the revert of the officer's
// own merge on a <prefix>revert-<tag> branch from main's head, escalates
// with its number, and merges nothing.
func TestAnUnhealthyReleaseOpensTheRevertPullRequest(t *testing.T) {
	rr := newRolloutRig(t, officers.RollbackPR)
	rr.health = func(now time.Time) (int, map[string]any) {
		if now.Sub(fxNow) > time.Minute { // live at +30s, then failing from +90s
			return 503, map[string]any{"version": "v1.5.0", "db": "down"}
		}
		return 200, map[string]any{"version": "v1.5.0", "db": "ok"}
	}
	rr.gh.compares[fxPrevTag+"..."+fxMain] = Compare{Status: "ahead", AheadBy: 1, Commits: []string{fxMain}}
	rr.gh.prsFor[fxMain] = []PR{{Number: 77, Merged: true, MergeCommitSHA: fxMain, Author: BotLogin(rr.gh.slug), Title: "fix"}}
	rr.gh.commits[fxMain] = commitInfo{tree: "tree-a", parents: []string{fxPrevTag}}
	rr.gh.commits[fxPrevTag] = commitInfo{tree: "tree-prev"}
	st := rr.runTo(t, fxRelease)
	if st.Verdict != VerdictUnhealthy || st.RevertPR != 900 || !st.Escalated {
		t.Fatalf("status %+v", st)
	}
	if got := strings.Join(rr.gh.createdRefs, ","); got != "refs/heads/officer/revert-v1.5.0="+fxMain {
		t.Fatalf("refs created %q", got)
	}
	if len(rr.gh.merged) != 0 {
		t.Fatalf("policy pr merged %v", rr.gh.merged)
	}
	if h := rr.escalations(); len(h) != 1 || !strings.Contains(h[0], "#900") {
		t.Fatalf("escalations %q", h)
	}
}

// TestARevertIsRefusedWhenTheReleaseHoldsNoOfficerMerge: a release made of a
// human's merge only is not the officer's to revert: escalate, create nothing.
func TestARevertIsRefusedWhenTheReleaseHoldsNoOfficerMerge(t *testing.T) {
	rr := newRolloutRig(t, officers.RollbackPR)
	rr.gh.prsFor[fxMain] = []PR{{Number: 78, Merged: true, MergeCommitSHA: fxMain, Author: "a-human"}}
	if _, err := rr.w.openRevert(context.Background(), fxRelease); err == nil {
		t.Fatal("a human-only release was reverted")
	}
	if len(rr.gh.createdRefs) != 0 {
		t.Fatalf("created %v", rr.gh.createdRefs)
	}
}

// TestASupersededReleaseEndsTheWatchWithoutRollback: a HIGHER version
// answering while healthy means someone released over it.
func TestASupersededReleaseEndsTheWatchWithoutRollback(t *testing.T) {
	rr := newRolloutRig(t, officers.RollbackAuto)
	rr.health = func(now time.Time) (int, map[string]any) {
		if now.Sub(fxNow) > time.Minute {
			return 200, map[string]any{"version": "v1.6.0", "db": "ok"}
		}
		return 200, map[string]any{"version": "v1.5.0", "db": "ok"}
	}
	st := rr.runTo(t, fxRelease)
	if st.Verdict != VerdictSuperseded || st.Escalated || len(rr.gh.createdRefs) != 0 {
		t.Fatalf("status %+v, refs %v", st, rr.gh.createdRefs)
	}
}
