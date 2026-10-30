package officership

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
)

// fxNow is a Wednesday afternoon (UTC), outside any fixture's quiet hours.
var fxNow = time.Date(2000, 1, 5, 15, 0, 0, 0, time.UTC)

// greenReleaseFixture: main is one squash merge ahead of the newest tag
// v1.4.0, that merge's pull request head passed every check and has main's
// exact tree, v1.4.0 is live, and the officer's last release was days ago.
func greenReleaseFixture() (*Gates, *fakeGH, *fakeHQ, *string) {
	gh := newFakeGH()
	hq := &fakeHQ{enabled: true}
	bot := BotLogin(gh.slug)
	gh.tags = []TagRef{
		{Name: "v1.4.0", ObjectSHA: fxPrevTag, ObjectType: "commit"},
		{Name: "v1.3.0", ObjectSHA: "5555555555555555555555555555555555555555", ObjectType: "commit"},
		{Name: "v1.2.0", ObjectSHA: "tagobj-old", ObjectType: "tag"},
		{Name: "not-a-release", ObjectSHA: "6666666666666666666666666666666666666666", ObjectType: "commit"},
	}
	gh.tagObjects["tagobj-old"] = TagObject{SHA: "tagobj-old", Tagger: bot, TaggedAt: fxNow.Add(-72 * time.Hour),
		TargetSHA: "7777777777777777777777777777777777777777", TargetType: "commit"}
	gh.branches["main"] = fxMain
	gh.compares[fxPrevTag+"..."+fxMain] = Compare{Status: "ahead", AheadBy: 1, Files: []File{{Path: "internal/web/x.go", Status: "modified"}},
		Commits: []string{fxMain}}
	gh.prsFor[fxMain] = []PR{{Number: 77, Merged: true, MergeCommitSHA: fxMain, HeadSHA: fxPRHead, Author: bot}}
	gh.statuses[fxPRHead] = map[string]string{"ci / build": "success", "ci / e2e": "success", "ci / lint": "success"}
	gh.commits[fxMain] = commitInfo{tree: "tree-a", parents: []string{fxPrevTag}}
	gh.commits[fxPRHead] = commitInfo{tree: "tree-a", parents: []string{"8888888888888888888888888888888888888888"}}
	live := "v1.4.0"
	g := &Gates{OfficerID: "engineering:example-web", Repo: fxRepo, Ship: fxShip(), GH: gh, HQ: hq,
		Now:         func() time.Time { return fxNow },
		InFlight:    func() (string, bool) { return "", false },
		LiveVersion: func(context.Context) (string, error) { return live, nil }}
	return g, gh, hq, &live
}

// TestTheReleaseGatesGreenPathTagsTheNextMinorOnMainsHead: the next minor of
// the HIGHEST vX.Y.Z (not the newest-listed, not a non-release tag), as an
// annotated tag by the officer's bot, on main's head.
func TestTheReleaseGatesGreenPathTagsTheNextMinorOnMainsHead(t *testing.T) {
	g, gh, _, _ := greenReleaseFixture()
	out, err := g.TagRelease(context.Background(), TagOptions{})
	if err != nil {
		t.Fatalf("green fixture refused: %v", err)
	}
	if out.Tag != "v1.5.0" || out.SHA != fxMain || out.Previous != "v1.4.0" {
		t.Fatalf("tagged %s at %s after %s, want v1.5.0 at main's head after v1.4.0", out.Tag, out.SHA, out.Previous)
	}
	if got := strings.Join(gh.createdRefs, ","); got != "refs/tags/v1.5.0=tagobj-v1.5.0" {
		t.Fatalf("refs created: %q", got)
	}
	if obj := gh.tagObjects["tagobj-v1.5.0"]; obj.Tagger != BotLogin(gh.slug) || obj.TargetSHA != fxMain {
		t.Fatalf("tag object %+v: want tagger %s on %s", obj, BotLogin(gh.slug), fxMain)
	}
	want := "stage hq_enabled tags ancestor in_flight rate quiet_hours main_ci migrations tag_create"
	if got := strings.Join(out.Passed, " "); got != want {
		t.Fatalf("gates passed = %q, want %q", got, want)
	}
}

// TestEveryReleaseGateRefusesItsOwnFailure: each case breaks ONE input, the
// tag is refused by THAT gate, and no ref is created.
func TestEveryReleaseGateRefusesItsOwnFailure(t *testing.T) {
	bot := BotLogin(newFakeGH().slug)
	officerTag := func(gh *fakeGH, name string, ago time.Duration) {
		obj := "tagobj-" + name
		gh.tags = append([]TagRef{{Name: name, ObjectSHA: obj, ObjectType: "tag"}}, gh.tags...)
		gh.tagObjects[obj] = TagObject{SHA: obj, Tagger: bot, TaggedAt: fxNow.Add(-ago), TargetSHA: fxPrevTag, TargetType: "commit"}
	}
	goodPreflight := func() officers.Preflight {
		var p officers.Preflight
		a, b := fxNow.Add(-time.Minute), fxNow.Add(-2*time.Hour)
		p.WAL.LastArchivedTime, p.LastBackupTime = &a, &b
		return p
	}
	addMigration := func(gh *fakeGH, body string) {
		c := gh.compares[fxPrevTag+"..."+fxMain]
		c.Files = append(c.Files, File{Path: "db/migrations/0091_x.sql", Status: "added"})
		gh.compares[fxPrevTag+"..."+fxMain] = c
		gh.contents["db/migrations/0091_x.sql@"+fxMain] = body
	}
	cases := []struct {
		name   string
		gate   string
		break_ func(g *Gates, gh *fakeGH, hq *fakeHQ, live *string)
	}{
		{"stage merge", GateStage, func(g *Gates, _ *fakeGH, _ *fakeHQ, _ *string) { g.Ship.Stage = officers.StageMerge }},
		{"HQ says disabled", GateHQEnabled, func(_ *Gates, _ *fakeGH, hq *fakeHQ, _ *string) { hq.enabled = false }},
		{"no release tag", GateTags, func(_ *Gates, gh *fakeGH, _ *fakeHQ, _ *string) { gh.tags = gh.tags[3:] }},
		{"nothing new on main", gateNothingToDo, func(_ *Gates, gh *fakeGH, _ *fakeHQ, _ *string) {
			gh.compares[fxPrevTag+"..."+fxMain] = Compare{Status: "identical"}
		}},
		{"the tag is off main", GateAncestor, func(_ *Gates, gh *fakeGH, _ *fakeHQ, _ *string) {
			gh.compares[fxPrevTag+"..."+fxMain] = Compare{Status: "diverged", AheadBy: 1, BehindBy: 2}
		}},
		{"a release is being watched", GateInFlight, func(g *Gates, _ *fakeGH, _ *fakeHQ, _ *string) {
			g.InFlight = func() (string, bool) { return "v1.4.0", true }
		}},
		{"the newest tag is not live", GateInFlight, func(_ *Gates, _ *fakeGH, _ *fakeHQ, live *string) { *live = "v1.3.0" }},
		{"the live version cannot be read", GateInFlight, func(g *Gates, _ *fakeGH, _ *fakeHQ, _ *string) {
			g.LiveVersion = func(context.Context) (string, error) { return "", errors.New("unreachable") }
		}},
		{"daily limit reached", GateRate, func(_ *Gates, gh *fakeGH, _ *fakeHQ, live *string) {
			officerTag(gh, "v1.4.1", 6*time.Hour)
			officerTag(gh, "v1.4.2", 4*time.Hour)
			officerTag(gh, "v1.4.3", 2*time.Hour)
			*live = "v1.4.3"
			gh.compares[fxPrevTag+"..."+fxMain] = Compare{Status: "ahead", AheadBy: 1}
		}},
		{"inside the minimum gap", GateRate, func(_ *Gates, gh *fakeGH, _ *fakeHQ, live *string) {
			officerTag(gh, "v1.4.1", 10*time.Minute)
			*live = "v1.4.1"
		}},
		{"quiet hours", GateQuietHours, func(g *Gates, _ *fakeGH, _ *fakeHQ, _ *string) {
			g.Ship.Quiet = &officers.QuietHours{Loc: time.UTC, Start: 14 * 60, End: 16 * 60}
		}},
		{"quiet weekend", GateQuietHours, func(g *Gates, _ *fakeGH, _ *fakeHQ, _ *string) {
			g.Ship.Quiet = &officers.QuietHours{Loc: time.UTC, Start: 0, End: 0, Weekends: true}
			g.Now = func() time.Time { return fxNow.AddDate(0, 0, 3) } // Saturday
		}},
		{"main's PR head is red", GateMainCI, func(_ *Gates, gh *fakeGH, _ *fakeHQ, _ *string) {
			gh.statuses[fxPRHead]["ci / e2e"] = "failure"
		}},
		{"main's tree was never tested", GateMainCI, func(_ *Gates, gh *fakeGH, _ *fakeHQ, _ *string) {
			gh.commits[fxPRHead] = commitInfo{tree: "tree-b"}
		}},
		{"main's head is no PR's merge", GateMainCI, func(_ *Gates, gh *fakeGH, _ *fakeHQ, _ *string) { gh.prsFor[fxMain] = nil }},
		{"a destructive migration in the release", GateMigrations, func(_ *Gates, gh *fakeGH, _ *fakeHQ, _ *string) {
			addMigration(gh, "DROP TABLE widgets;")
		}},
		{"a migration and no preflight", GateBackup, func(_ *Gates, gh *fakeGH, hq *fakeHQ, _ *string) {
			addMigration(gh, "CREATE TABLE widgets (id bigint);")
			hq.preErr = &officers.HQError{Status: 404}
		}},
		{"a migration and a stale WAL archive", GateBackup, func(_ *Gates, gh *fakeGH, hq *fakeHQ, _ *string) {
			addMigration(gh, "CREATE TABLE widgets (id bigint);")
			hq.preflight = goodPreflight()
			old := fxNow.Add(-20 * time.Minute)
			hq.preflight.WAL.LastArchivedTime = &old
		}},
		{"a migration and a recent archive failure", GateBackup, func(_ *Gates, gh *fakeGH, hq *fakeHQ, _ *string) {
			addMigration(gh, "CREATE TABLE widgets (id bigint);")
			hq.preflight = goodPreflight()
			f := fxNow.Add(-30 * time.Minute)
			hq.preflight.WAL.LastFailedTime = &f
		}},
		{"a migration and an old base backup", GateBackup, func(_ *Gates, gh *fakeGH, hq *fakeHQ, _ *string) {
			addMigration(gh, "CREATE TABLE widgets (id bigint);")
			hq.preflight = goodPreflight()
			b := fxNow.Add(-30 * time.Hour)
			hq.preflight.LastBackupTime = &b
		}},
		{"someone tagged first", GateTagCreate, func(_ *Gates, gh *fakeGH, _ *fakeHQ, _ *string) {
			gh.refErr = fmt.Errorf("%w: refs/tags/v1.5.0", ErrRefExists)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g, gh, hq, live := greenReleaseFixture()
			c.break_(g, gh, hq, live)
			_, err := g.TagRelease(context.Background(), TagOptions{})
			r, ok := AsRefusal(err)
			if !ok || r.Gate != c.gate {
				t.Fatalf("err = %v, want a refusal by gate %q", err, c.gate)
			}
			if c.gate != GateTagCreate && len(gh.createdRefs) != 0 {
				t.Fatalf("refused by %s and still created %v", r.Gate, gh.createdRefs)
			}
			if c.gate == GateTagCreate && !strings.Contains(r.Reason, "already exists") {
				t.Fatalf("a concurrent tag is reported as %q", r.Reason)
			}
		})
	}
}

// TestAReleaseWithACleanMigrationPassesThePreflight: the green half of the
// backup gate.
func TestAReleaseWithACleanMigrationPassesThePreflight(t *testing.T) {
	g, gh, hq, _ := greenReleaseFixture()
	c := gh.compares[fxPrevTag+"..."+fxMain]
	c.Files = append(c.Files, File{Path: "db/migrations/0091_x.sql", Status: "added"})
	gh.compares[fxPrevTag+"..."+fxMain] = c
	gh.contents["db/migrations/0091_x.sql@"+fxMain] = "CREATE TABLE widgets (id bigint);"
	a, b := fxNow.Add(-time.Minute), fxNow.Add(-2*time.Hour)
	hq.preflight.WAL.LastArchivedTime, hq.preflight.LastBackupTime = &a, &b
	out, err := g.TagRelease(context.Background(), TagOptions{})
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	if !strings.Contains(strings.Join(out.Passed, " "), "backup") || strings.Join(out.Migrations, ",") != "db/migrations/0091_x.sql" {
		t.Fatalf("passed %v, migrations %v", out.Passed, out.Migrations)
	}
}

// TestMainCIPassesOnStatusesOnTheHeadItself: the other green route of
// main_ci — the checks posted on main's head directly, no pull request needed.
func TestMainCIPassesOnStatusesOnTheHeadItself(t *testing.T) {
	g, gh, _, _ := greenReleaseFixture()
	gh.prsFor[fxMain] = nil
	gh.statuses[fxMain] = map[string]string{"ci / build": "success", "ci / e2e": "success", "ci / lint": "success"}
	if _, err := g.TagRelease(context.Background(), TagOptions{}); err != nil {
		t.Fatalf("refused: %v", err)
	}
}

// TestTheRateCountsOnlyTheOfficersOwnRecentTags: two officer tags today and
// one human annotated tag stay under a limit of 3; the control (three of the
// officer's) is in the table above.
func TestTheRateCountsOnlyTheOfficersOwnRecentTags(t *testing.T) {
	g, gh, _, live := greenReleaseFixture()
	bot := BotLogin(gh.slug)
	for i, who := range []string{bot, "a-human", bot} {
		name := fmt.Sprintf("v1.4.%d", i+1)
		obj := "tagobj-" + name
		gh.tags = append([]TagRef{{Name: name, ObjectSHA: obj, ObjectType: "tag"}}, gh.tags...)
		gh.tagObjects[obj] = TagObject{SHA: obj, Tagger: who, TaggedAt: fxNow.Add(-time.Duration(5-i) * time.Hour), TargetSHA: fxPrevTag, TargetType: "commit"}
	}
	*live = "v1.4.3"
	if _, err := g.TagRelease(context.Background(), TagOptions{}); err != nil {
		t.Fatalf("refused under the limit: %v", err)
	}
}

// TestARollbackReleaseSkipsOnlyTheGatesItMust: the rollout watch's own
// release passes with a release in flight, HQ unreachable, the limit reached
// and inside quiet hours — and is still refused on red CI.
func TestARollbackReleaseSkipsOnlyTheGatesItMust(t *testing.T) {
	g, gh, hq, live := greenReleaseFixture()
	g.InFlight = func() (string, bool) { return "v1.4.0", true }
	hq.enabledErr = officers.ErrHQUnavailable
	*live = ""
	g.Ship.Quiet = &officers.QuietHours{Loc: time.UTC, Start: 14 * 60, End: 16 * 60}
	out, err := g.TagRelease(context.Background(), TagOptions{Rollback: true})
	if err != nil || out.Tag != "v1.5.0" {
		t.Fatalf("rollback release: %v %v", out.Tag, err)
	}
	g, gh, _, _ = greenReleaseFixture()
	gh.statuses[fxPRHead]["ci / build"] = "failure"
	if r, ok := AsRefusal(func() error { _, err := g.TagRelease(context.Background(), TagOptions{Rollback: true}); return err }()); !ok || r.Gate != GateMainCI {
		t.Fatalf("a rollback on red CI: %v", r)
	}
}

func TestVersionParsingAndNextMinor(t *testing.T) {
	for in, want := range map[string]string{"v0.133.0": "v0.134.0", "v1.2.3": "v1.3.0", "v10.0.9": "v10.1.0"} {
		v, ok := ParseVersion(in)
		if !ok || v.NextMinor().String() != want {
			t.Errorf("%s -> %v %v, want %s", in, v.NextMinor(), ok, want)
		}
	}
	for _, bad := range []string{"1.2.3", "v1.2", "v01.2.3", "v1.2.3-rc1", "v1.2.3 "} {
		if _, ok := ParseVersion(bad); ok {
			t.Errorf("%q parsed as a release", bad)
		}
	}
	rels := sortedReleases([]TagRef{{Name: "v0.9.0"}, {Name: "v0.10.0"}, {Name: "v0.133.0"}, {Name: "v0.99.0"}})
	if rels[0].Version.String() != "v0.133.0" || rels[3].Version.String() != "v0.9.0" {
		t.Fatalf("numeric order: %v", rels)
	}
}
