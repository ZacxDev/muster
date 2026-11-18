package officership

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// TestTheMergeGatesGreenPathMergesTheCheckedHead: with every gate green, the
// pull request is squash-merged with the head pinned to the sha the gates
// read, and every gate is reported passed, in order.
func TestTheMergeGatesGreenPathMergesTheCheckedHead(t *testing.T) {
	g, gh, hq := greenMergeFixture()
	out, err := g.MergePR(context.Background(), fxPR)
	if err != nil {
		t.Fatalf("green fixture refused: %v", err)
	}
	if len(gh.merged) != 1 || gh.merged[0] != fxPR || gh.mergedSHA[0] != fxHead {
		t.Fatalf("merged %v at %v, want #%d at %s (the match-head guard)", gh.merged, gh.mergedSHA, fxPR, fxHead)
	}
	want := "stage hq_enabled base head_branch author mergeable up_to_date checks review hold migrations notice merge"
	if got := strings.Join(out.Passed, " "); got != want {
		t.Fatalf("gates passed = %q, want %q", got, want)
	}
	if len(hq.notices) != 0 {
		t.Fatalf("a pull request touching no sensitive path posted %d notice(s)", len(hq.notices))
	}
}

// TestEveryMergeGateRefusesItsOwnFailure is the red half of every pair: each
// case breaks exactly ONE input of the green fixture, and the merge must be
// refused by THAT gate (not an earlier one), with nothing merged.
func TestEveryMergeGateRefusesItsOwnFailure(t *testing.T) {
	cases := []struct {
		name   string
		gate   string
		break_ func(g *Gates, gh *fakeGH, hq *fakeHQ)
	}{
		{"stage shadow", GateStage, func(g *Gates, _ *fakeGH, _ *fakeHQ) { g.Ship.Stage = officers.StageShadow }},
		{"HQ says disabled", GateHQEnabled, func(_ *Gates, _ *fakeGH, hq *fakeHQ) { hq.enabled = false }},
		{"HQ unreachable", GateHQEnabled, func(_ *Gates, _ *fakeGH, hq *fakeHQ) { hq.enabledErr = officers.ErrHQUnavailable }},
		{"closed", GatePR, func(_ *Gates, gh *fakeGH, _ *fakeHQ) { p := gh.prs[fxPR]; p.State = "closed"; gh.prs[fxPR] = p }},
		{"draft", GatePR, func(_ *Gates, gh *fakeGH, _ *fakeHQ) { p := gh.prs[fxPR]; p.Draft = true; gh.prs[fxPR] = p }},
		{"base is not main", GateBase, func(_ *Gates, gh *fakeGH, _ *fakeHQ) { p := gh.prs[fxPR]; p.BaseRef = "release"; gh.prs[fxPR] = p }},
		{"head is a human branch", GateHeadBranch, func(_ *Gates, gh *fakeGH, _ *fakeHQ) {
			p := gh.prs[fxPR]
			p.HeadRef = "feature/x"
			gh.prs[fxPR] = p
		}},
		{"head from a fork", GateHeadBranch, func(_ *Gates, gh *fakeGH, _ *fakeHQ) {
			p := gh.prs[fxPR]
			p.HeadRepo = "someone/example-web"
			gh.prs[fxPR] = p
		}},
		{"authored by a human", GateAuthor, func(_ *Gates, gh *fakeGH, _ *fakeHQ) {
			p := gh.prs[fxPR]
			p.Author, p.AuthorType = "a-human", "User"
			gh.prs[fxPR] = p
		}},
		{"authored by another bot", GateAuthor, func(_ *Gates, gh *fakeGH, _ *fakeHQ) {
			p := gh.prs[fxPR]
			p.Author = "other-app[bot]"
			gh.prs[fxPR] = p
		}},
		{"mergeable unknown", GateMergeable, func(_ *Gates, gh *fakeGH, _ *fakeHQ) { p := gh.prs[fxPR]; p.Mergeable = nil; gh.prs[fxPR] = p }},
		{"conflicts", GateMergeable, func(_ *Gates, gh *fakeGH, _ *fakeHQ) {
			p := gh.prs[fxPR]
			p.Mergeable = ptrBool(false)
			gh.prs[fxPR] = p
		}},
		{"behind main", GateUpToDate, func(_ *Gates, gh *fakeGH, _ *fakeHQ) {
			gh.compares["main..."+fxHead] = Compare{Status: "diverged", AheadBy: 2, BehindBy: 1}
		}},
		{"a check failed", GateChecks, func(_ *Gates, gh *fakeGH, _ *fakeHQ) { gh.statuses[fxHead]["ci / e2e"] = "failure" }},
		{"a check pending", GateChecks, func(_ *Gates, gh *fakeGH, _ *fakeHQ) { gh.statuses[fxHead]["ci / lint"] = "pending" }},
		{"a check missing", GateChecks, func(_ *Gates, gh *fakeGH, _ *fakeHQ) { delete(gh.statuses[fxHead], "ci / build") }},
		{"review neutral", GateReview, func(_ *Gates, gh *fakeGH, _ *fakeHQ) { gh.checkRuns[fxHead][0].Conclusion = "neutral" }},
		{"review in progress", GateReview, func(_ *Gates, gh *fakeGH, _ *fakeHQ) {
			gh.checkRuns[fxHead][0].Status, gh.checkRuns[fxHead][0].Conclusion = "in_progress", ""
		}},
		{"review missing", GateReview, func(_ *Gates, gh *fakeGH, _ *fakeHQ) { gh.checkRuns[fxHead] = nil }},
		{"newer review failed", GateReview, func(_ *Gates, gh *fakeGH, _ *fakeHQ) {
			gh.checkRuns[fxHead] = append(gh.checkRuns[fxHead], CheckRun{ID: 9, Name: "pr-review", Status: "completed", Conclusion: "failure"})
		}},
		{"review from the wrong app", GateReview, func(g *Gates, _ *fakeGH, _ *fakeHQ) { g.Ship.ReviewApp = "trusted-reviewer" }},
		{"a human /hold", GateHold, func(_ *Gates, gh *fakeGH, _ *fakeHQ) {
			gh.comments[fxPR] = []Comment{{Author: "a-human", AuthorType: "User", Body: "looks risky\n/hold until Monday"}}
		}},
		{"destructive DDL", GateMigrations, func(_ *Gates, gh *fakeGH, _ *fakeHQ) {
			gh.contents["db/migrations/0090_add.sql@"+fxHead] = "ALTER TABLE things DROP COLUMN note;"
		}},
		{"a migration removed", GateMigrations, func(_ *Gates, gh *fakeGH, _ *fakeHQ) {
			gh.files[fxPR] = append(gh.files[fxPR], File{Path: "db/migrations/0010_old.sql", Status: "removed"})
		}},
		{"a migration renamed", GateMigrations, func(_ *Gates, gh *fakeGH, _ *fakeHQ) {
			gh.files[fxPR] = append(gh.files[fxPR], File{Path: "db/migrations/0011_new.sql", Previous: "db/migrations/0010_old.sql", Status: "renamed"})
		}},
		{"notice refused by HQ", GateNotice, func(_ *Gates, gh *fakeGH, hq *fakeHQ) {
			gh.files[fxPR] = append(gh.files[fxPR], File{Path: "internal/auth/session.go", Status: "modified"})
			hq.noticeErr = errors.New("HQ answered 503")
		}},
		{"GitHub refuses the merge (head moved)", GateMerge, func(_ *Gates, gh *fakeGH, _ *fakeHQ) {
			gh.mergeErr = errors.New("409 Head branch was modified")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g, gh, hq := greenMergeFixture()
			c.break_(g, gh, hq)
			_, err := g.MergePR(context.Background(), fxPR)
			r, ok := AsRefusal(err)
			if !ok || r.Gate != c.gate {
				t.Fatalf("err = %v, want a refusal by gate %q", err, c.gate)
			}
			if len(gh.merged) != 0 {
				t.Fatalf("refused by %s, and still merged %v", r.Gate, gh.merged)
			}
		})
	}
}

// TestABotHoldOrAWordContainingHoldDoesNotBlock: /hold blocks only from a
// human, and only as the command itself. The control (a human /hold) is in
// the table above.
func TestABotHoldOrAWordContainingHoldDoesNotBlock(t *testing.T) {
	for _, c := range []Comment{
		{Author: "some-bot[bot]", AuthorType: "Bot", Body: "/hold"},
		{Author: "a-human", AuthorType: "User", Body: "see /holdings and /unhold"},
	} {
		g, gh, _ := greenMergeFixture()
		gh.comments[fxPR] = []Comment{c}
		if _, err := g.MergePR(context.Background(), fxPR); err != nil {
			t.Errorf("%+v blocked the merge: %v", c, err)
		}
	}
}

// TestARequiredCheckMayBeACheckRun: a required check with no commit status of
// its name passes on a completed+success check run, and is refused on a
// skipped one.
func TestARequiredCheckMayBeACheckRun(t *testing.T) {
	g, gh, _ := greenMergeFixture()
	delete(gh.statuses[fxHead], "ci / lint")
	gh.checkRuns[fxHead] = append(gh.checkRuns[fxHead], CheckRun{ID: 7, Name: "ci / lint", Status: "completed", Conclusion: "success"})
	if _, err := g.MergePR(context.Background(), fxPR); err != nil {
		t.Fatalf("a successful check run did not count: %v", err)
	}
	g, gh, _ = greenMergeFixture()
	delete(gh.statuses[fxHead], "ci / lint")
	gh.checkRuns[fxHead] = append(gh.checkRuns[fxHead], CheckRun{ID: 7, Name: "ci / lint", Status: "completed", Conclusion: "skipped"})
	if r, ok := AsRefusal(func() error { _, err := g.MergePR(context.Background(), fxPR); return err }()); !ok || r.Gate != GateChecks {
		t.Fatalf("a skipped check run passed: %v", r)
	}
}

// TestASensitivePathPostsTheNoticeThenMerges: the notice goes to HQ's
// configured route with the configured channel and the matched paths only —
// BEFORE the merge, which then proceeds (notify, never block).
func TestASensitivePathPostsTheNoticeThenMerges(t *testing.T) {
	g, gh, hq := greenMergeFixture()
	gh.files[fxPR] = append(gh.files[fxPR],
		File{Path: "internal/auth/session.go", Status: "modified"},
		File{Path: "internal/api/api_tokens.go", Status: "added"})
	out, err := g.MergePR(context.Background(), fxPR)
	if err != nil {
		t.Fatalf("a sensitive path blocked the merge: %v", err)
	}
	if len(hq.notices) != 1 || len(gh.merged) != 1 {
		t.Fatalf("notices %d, merges %d; want 1 and 1", len(hq.notices), len(gh.merged))
	}
	n := hq.notices[0]
	if hq.noticePath != "/officers/engineering:example-web/notices" || n.Channel != "ch/example-alerts" || n.PR != fxPR || n.HeadSHA != fxHead {
		t.Fatalf("notice %+v at %q", n, hq.noticePath)
	}
	if got := strings.Join(n.Paths, ","); got != "internal/api/api_tokens.go,internal/auth/session.go" {
		t.Fatalf("notice paths = %q", got)
	}
	if strings.Join(out.Noticed, ",") != strings.Join(n.Paths, ",") {
		t.Fatalf("outcome noticed %v, notice paths %v", out.Noticed, n.Paths)
	}
}
