package officership

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
)

// Round-1 audit regressions. Each test here was pushed BEFORE its fix and
// seen red in CI against the unfixed code (the PR records the run).

// --- F1: a rollback release is never itself rolled back ----------------------

// rollbackTagFixture: the newest tag v1.6.0 is the officer's ROLLBACK release
// (its message carries the marker), tagged just now, over v1.5.0. Its one
// merge is an ordinary officer merge, so nothing but the marker says
// "rollback".
func rollbackTagFixture(rr *rolloutRig, message string) {
	gh := rr.gh
	bot := BotLogin(gh.slug)
	gh.tags = []TagRef{
		{Name: "v1.6.0", ObjectSHA: "tagobj-v1.6.0", ObjectType: "tag"},
		{Name: "v1.5.0", ObjectSHA: fxPrevTag, ObjectType: "commit"},
	}
	gh.tagObjects["tagobj-v1.6.0"] = TagObject{SHA: "tagobj-v1.6.0", Tagger: bot, TaggedAt: fxNow, TargetSHA: fxMain,
		TargetType: "commit", Message: message}
	gh.prsFor[fxMain] = []PR{{Number: 77, Merged: true, MergeCommitSHA: fxMain, Author: bot, HeadRef: "officer/17-x", Title: "x"}}
	gh.commits[fxPrevTag] = commitInfo{tree: "tree-prev"}
}

func down(time.Time) (int, map[string]any) { return 503, map[string]any{"version": "v1.5.0"} }

// TestAResumedRollbackReleaseIsNeverRolledBack (F1): muster restarts while
// it watches a ROLLBACK release, and the service is still down. The resumed
// watch must escalate as a failed rollback and touch nothing — not revert
// the revert and re-tag the bad tree.
func TestAResumedRollbackReleaseIsNeverRolledBack(t *testing.T) {
	rr := newRolloutRig(t, officers.RollbackPR)
	rr.health = down
	rollbackTagFixture(rr, "v1.6.0\n\nRollback release by officer x (a revert of v1.5.0).\n\nMuster-Release: rollback\n")
	rr.w.Resume(context.Background())
	rr.w.Wait()
	cur, _ := rr.w.Status()
	if cur == nil || cur.Tag != "v1.6.0" {
		t.Fatalf("the watch was not resumed: %+v", cur)
	}
	if len(rr.gh.createdRefs) != 0 {
		t.Fatalf("a resumed rollback release was reverted: refs %v", rr.gh.createdRefs)
	}
	if h := rr.escalations(); len(h) != 1 || !strings.Contains(h[0], "ROLLBACK release v1.6.0") {
		t.Fatalf("escalations %q, want one naming the failed ROLLBACK release", h)
	}
}

// TestAResumedForwardReleaseStillRollsBack: the other half — an ordinary
// release resumed after a restart keeps its rollback (the revert branch is
// created).
func TestAResumedForwardReleaseStillRollsBack(t *testing.T) {
	rr := newRolloutRig(t, officers.RollbackPR)
	rr.health = down
	rollbackTagFixture(rr, "v1.6.0\n\nReleased by officer x.\n\nMuster-Release: release\n")
	rr.w.Resume(context.Background())
	rr.w.Wait()
	if len(rr.gh.createdRefs) != 1 || !strings.HasPrefix(rr.gh.createdRefs[0], "refs/heads/officer/revert-v1.6.0") {
		t.Fatalf("a resumed forward release was not rolled back: refs %v", rr.gh.createdRefs)
	}
}

// TestATagSaysWhetherItIsARollback (F1): the marker Resume reads is written
// by TagRelease, on both kinds of release.
func TestATagSaysWhetherItIsARollback(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		g, gh, _, _ := greenReleaseFixture()
		out, err := g.TagRelease(context.Background(), TagOptions{Rollback: rollback})
		if err != nil {
			t.Fatal(err)
		}
		msg := gh.tagObjects["tagobj-"+out.Tag].Message
		want := "Muster-Release: release"
		if rollback {
			want = "Muster-Release: rollback"
		}
		if !strings.Contains(msg, "\n"+want+"\n") {
			t.Fatalf("rollback=%v: tag message %q lacks the %q trailer", rollback, msg, want)
		}
	}
}

// TestAnAutomaticRevertIsNeverRevertedAutomatically (F1): a release whose
// officer merge is itself an automatic revert (its branch is
// <prefix>revert-…) is not reverted again.
func TestAnAutomaticRevertIsNeverRevertedAutomatically(t *testing.T) {
	rr := newRolloutRig(t, officers.RollbackPR)
	rollbackTagFixture(rr, "")
	rr.gh.prsFor[fxMain][0].HeadRef = "officer/revert-v1.5.0"
	if _, err := rr.w.openRevert(context.Background(), TagOutcome{Tag: "v1.6.0", SHA: fxMain, PreviousSHA: fxPrevTag}); err == nil {
		t.Fatal("an automatic revert was reverted")
	}
	if len(rr.gh.createdRefs) != 0 {
		t.Fatalf("created %v", rr.gh.createdRefs)
	}
}

// --- G2: an existing revert branch gets a fresh name -----------------------

func TestARevertBranchThatExistsGetsAFreshName(t *testing.T) {
	rr := newRolloutRig(t, officers.RollbackPR)
	rollbackTagFixture(rr, "")
	rr.gh.existingRefs = map[string]bool{"refs/heads/officer/revert-v1.6.0": true}
	pr, err := rr.w.openRevert(context.Background(), TagOutcome{Tag: "v1.6.0", SHA: fxMain, PreviousSHA: fxPrevTag})
	if err != nil || pr != 900 {
		t.Fatalf("openRevert: %d %v", pr, err)
	}
	if len(rr.gh.createdRefs) != 1 || !strings.HasPrefix(rr.gh.createdRefs[0], "refs/heads/officer/revert-v1.6.0-") {
		t.Fatalf("refs %v, want a fresh officer/revert-v1.6.0-<n>", rr.gh.createdRefs)
	}
}

// --- F2: the rate gate reads past many human tags --------------------------

// TestTheRateGateReadsPastManyHumanTags: a repository with more annotated
// HUMAN tags than the old read cap and no officer tag yet — the first officer
// release must not be refused. Both shapes: the human tags older than the
// window, and all of them inside it.
func TestTheRateGateReadsPastManyHumanTags(t *testing.T) {
	for _, age := range []time.Duration{48 * time.Hour, 2 * time.Hour} {
		g, gh, _, _ := greenReleaseFixture()
		gh.tags = []TagRef{{Name: "v1.4.0", ObjectSHA: fxPrevTag, ObjectType: "commit"}}
		for i := 1; i <= 70; i++ {
			obj := fmt.Sprintf("tagobj-human-%d", i)
			gh.tags = append(gh.tags, TagRef{Name: fmt.Sprintf("v1.3.%d", i), ObjectSHA: obj, ObjectType: "tag"})
			gh.tagObjects[obj] = TagObject{SHA: obj, Tagger: "a-human", TaggedAt: fxNow.Add(-age), TargetSHA: fxPrevTag, TargetType: "commit"}
		}
		if _, err := g.TagRelease(context.Background(), TagOptions{}); err != nil {
			t.Errorf("human tags %s old: %v", age, err)
		}
	}
}

// --- F3: /hold however it is written -----------------------------------------

func TestAHoldInMarkdownStillBlocks(t *testing.T) {
	for _, body := range []string{"`/hold`", "**/hold**", "(/hold)", "please /hold", "/HOLD", "lgtm\n> /hold"} {
		g, gh, _ := greenMergeFixture()
		gh.comments[fxPR] = []Comment{{Author: "a-human", AuthorType: "User", Body: body}}
		_, err := g.MergePR(context.Background(), fxPR)
		if r, ok := AsRefusal(err); !ok || r.Gate != GateHold {
			t.Errorf("%q from a human did not hold the merge: %v", body, err)
		}
	}
	g, gh, _ := greenMergeFixture()
	gh.comments[fxPR] = []Comment{{Author: BotLogin(gh.slug), AuthorType: "Bot", Body: "**/hold** noted"},
		{Author: "a-human", AuthorType: "User", Body: "see x/hold.go and /unhold"}}
	if _, err := g.MergePR(context.Background(), fxPR); err != nil {
		t.Fatalf("the officer's own comment or a path held the merge: %v", err)
	}
}

// --- F5: a merge that raced the base -----------------------------------------

// raceFixture: the release fixture plus a ready officer pull request on top
// of main's head.
func raceFixture() (*Gates, *fakeGH) {
	g, gh, _, _ := greenReleaseFixture()
	bot := BotLogin(gh.slug)
	gh.prs[fxPR] = PR{Number: fxPR, State: "open", Mergeable: ptrBool(true), Author: bot, AuthorType: "Bot",
		HeadRef: "officer/17-fix", HeadSHA: fxHead, HeadRepo: fxRepo, BaseRef: "main", BaseRepo: fxRepo, Title: "fix"}
	gh.compares["main..."+fxHead] = Compare{Status: "ahead", AheadBy: 1}
	gh.statuses[fxHead] = map[string]string{"ci / build": "success", "ci / e2e": "success", "ci / lint": "success"}
	gh.checkRuns[fxHead] = []CheckRun{{ID: 5, Name: "pr-review", Status: "completed", Conclusion: "success"}}
	merge := "squash" + fxHead
	gh.compares[fxPrevTag+"..."+merge] = Compare{Status: "ahead", AheadBy: 2}
	gh.statuses[merge] = map[string]string{"ci / build": "success", "ci / e2e": "success", "ci / lint": "success"}
	return g, gh
}

// TestAMergeThatRacedTheBaseBlocksTheNextRelease: main moved between the
// gates' read and GitHub's merge, so the merge's parent is not the base the
// gates checked. The merge is reported as an untested tree, and the next
// release is refused while main's head is that merge.
func TestAMergeThatRacedTheBaseBlocksTheNextRelease(t *testing.T) {
	g, gh := raceFixture()
	gh.commits["squash"+fxHead] = commitInfo{tree: "tree-raced", parents: []string{"5ce05ce05ce05ce05ce05ce05ce05ce05ce05ce0"}}
	out, err := g.MergePR(context.Background(), fxPR)
	if err != nil {
		t.Fatal(err)
	}
	if !out.UntestedTree || !strings.Contains(out.Warning, "untested") {
		t.Fatalf("a raced merge was not reported: %+v", out)
	}
	_, err = g.TagRelease(context.Background(), TagOptions{})
	if r, ok := AsRefusal(err); !ok || r.Gate != "untested_merge" {
		t.Fatalf("release after a raced merge: %v, want a refusal by gate untested_merge", err)
	}
}

// TestAMergeOnTheCheckedBaseIsTested: the green half.
func TestAMergeOnTheCheckedBaseIsTested(t *testing.T) {
	g, gh := raceFixture()
	gh.commits["squash"+fxHead] = commitInfo{tree: "tree-ok", parents: []string{fxMain}}
	out, err := g.MergePR(context.Background(), fxPR)
	if err != nil || out.UntestedTree {
		t.Fatalf("%+v %v", out, err)
	}
	if _, err := g.TagRelease(context.Background(), TagOptions{}); err != nil {
		t.Fatalf("release after a clean merge: %v", err)
	}
}
