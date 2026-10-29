package officership

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/officers"
)

// HQ is what the gates ask HQ, with the officer's own token.
type HQ interface {
	// Enabled is HQ's on/off switch for the officer (GET /officers/{id}).
	Enabled(ctx context.Context) (bool, error)
	// PostNotice posts the sensitive-path notice (POST to path under hq_url).
	PostNotice(ctx context.Context, path string, n officers.Notice) error
	// Preflight is HQ's database backup health (POST /officers/{id}/preflight).
	Preflight(ctx context.Context) (officers.Preflight, error)
}

// Gate names, as a refusal and the call log name them.
const (
	GateStage       = "stage"
	GateHQEnabled   = "hq_enabled"
	GatePR          = "pull_request"
	GateBase        = "base"
	GateHeadBranch  = "head_branch"
	GateAuthor      = "author"
	GateMergeable   = "mergeable"
	GateUpToDate    = "up_to_date"
	GateChecks      = "checks"
	GateReview      = "review"
	GateHold        = "hold"
	GateMigrations  = "migrations"
	GateNotice      = "notice"
	GateMerge       = "merge"
	GateTags        = "tags"
	GateAncestor    = "ancestor"
	GateInFlight    = "in_flight"
	GateRate        = "rate"
	GateQuietHours  = "quiet_hours"
	GateMainCI      = "main_ci"
	GateBackup      = "backup"
	GateTagCreate   = "tag_create"
	gateNothingToDo = "nothing_to_release"
)

// Refusal is a gate that did not pass. Nothing was written.
type Refusal struct {
	Gate   string
	Reason string
}

func (r *Refusal) Error() string { return "refused by gate " + r.Gate + ": " + r.Reason }

func refuse(gate, format string, args ...any) error {
	return &Refusal{Gate: gate, Reason: fmt.Sprintf(format, args...)}
}

// AsRefusal reports whether err is a gate refusal.
func AsRefusal(err error) (*Refusal, bool) {
	var r *Refusal
	ok := errors.As(err, &r)
	return r, ok
}

// Gates are officer_merge_pr's and officer_tag_release's checks for one
// officer. Every input is read from GitHub and HQ at call time; nothing is
// cached between calls.
type Gates struct {
	OfficerID string
	Repo      string
	Ship      *officers.Ship
	GH        GitHub
	HQ        HQ
	Now       func() time.Time
	// InFlight reports a release the rollout watch still owns (its tag).
	InFlight func() (string, bool)
	// LiveVersion reads the version the service reports now (the rollout's
	// health URL).
	LiveVersion func(ctx context.Context) (string, error)
}

func (g *Gates) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// MergeOutcome is what officer_merge_pr did.
type MergeOutcome struct {
	PR       int      `json:"pr"`
	HeadSHA  string   `json:"head_sha"`
	MergeSHA string   `json:"merge_sha"`
	Passed   []string `json:"gates_passed"`
	Noticed  []string `json:"sensitive_paths_noticed,omitempty"`
}

var holdRE = regexp.MustCompile(`(?i)(^|\s)/hold\b`)

// MergePR squash-merges the officer's own pull request n if, and only if,
// every merge gate passes now. A refusal is a *Refusal naming the gate; on a
// refusal nothing was written (a sensitive-path notice may have been posted
// only when every OTHER gate had passed).
//
// The gates, in order:
//
//  1. stage — the officer's stage allows merging;
//  2. hq_enabled — HQ answers and says the officer is enabled (fail closed);
//  3. base / head_branch / author — open, not draft, into the base branch,
//     from <prefix>… in the same repository, authored by the officer App's bot;
//  4. mergeable — GitHub says mergeable (unknown is "call again");
//  5. up_to_date — the head contains the base's head (behind_by 0);
//  6. checks — every required check is success on the head commit (a commit
//     status, or a completed check run);
//  7. review — the newest review check run on the head is completed+success
//     (neutral fails; from review_app when set);
//  8. hold — no human wrote /hold on the pull request;
//  9. migrations — no destructive DDL in an added or changed migration, and
//     no migration removed or renamed;
//  10. notice — if a sensitive path changed, HQ accepted the notice;
//  11. merge — GitHub squash-merges with the head pinned to the checked sha.
func (g *Gates) MergePR(ctx context.Context, n int) (MergeOutcome, error) {
	out := MergeOutcome{PR: n}
	pass := func(name string) { out.Passed = append(out.Passed, name) }
	if !g.Ship.MayMerge() {
		return out, refuse(GateStage, "this officer's stage is %q; merging needs %q or %q", g.Ship.Stage, officers.StageMerge, officers.StageRelease)
	}
	pass(GateStage)
	if err := g.hqEnabled(ctx); err != nil {
		return out, err
	}
	pass(GateHQEnabled)
	pr, err := g.GH.PR(ctx, n)
	if err != nil {
		return out, refuse(GatePR, "could not read pull request #%d: %v", n, err)
	}
	out.HeadSHA = pr.HeadSHA
	if pr.State != "open" || pr.Merged || pr.Draft {
		return out, refuse(GatePR, "pull request #%d is %s (merged %v, draft %v); only an open, ready pull request merges", n, pr.State, pr.Merged, pr.Draft)
	}
	if pr.BaseRef != g.Ship.Base || !connectors.SameRepo(pr.BaseRepo, g.Repo) {
		return out, refuse(GateBase, "pull request #%d targets %s:%s; the base must be %s:%s", n, pr.BaseRepo, pr.BaseRef, g.Repo, g.Ship.Base)
	}
	pass(GateBase)
	if !strings.HasPrefix(pr.HeadRef, g.Ship.BranchPrefix) || !connectors.SameRepo(pr.HeadRepo, g.Repo) {
		return out, refuse(GateHeadBranch, "pull request #%d comes from %s:%s; only a %s* branch of %s merges", n, pr.HeadRepo, pr.HeadRef, g.Ship.BranchPrefix, g.Repo)
	}
	pass(GateHeadBranch)
	slug, err := g.GH.AppSlug(ctx)
	if err != nil {
		return out, refuse(GateAuthor, "could not read the officer App's identity: %v", err)
	}
	if !strings.EqualFold(pr.Author, BotLogin(slug)) || pr.AuthorType != "Bot" {
		return out, refuse(GateAuthor, "pull request #%d is authored by %s (%s); the officer merges only its own (%s)", n, pr.Author, pr.AuthorType, BotLogin(slug))
	}
	pass(GateAuthor)
	if pr.Mergeable == nil {
		return out, refuse(GateMergeable, "GitHub has not finished computing whether #%d is mergeable; call again in a minute", n)
	}
	if !*pr.Mergeable {
		return out, refuse(GateMergeable, "GitHub says #%d is not mergeable (conflicts); rebase it on %s and push", n, g.Ship.Base)
	}
	pass(GateMergeable)
	cmp, err := g.GH.Compare(ctx, g.Ship.Base, pr.HeadSHA)
	if err != nil {
		return out, refuse(GateUpToDate, "could not compare %s with the head: %v", g.Ship.Base, err)
	}
	if cmp.BehindBy != 0 {
		return out, refuse(GateUpToDate, "the head is %d commit(s) behind %s; rebase on %s, push, and wait for CI and review on the new head", cmp.BehindBy, g.Ship.Base, g.Ship.Base)
	}
	pass(GateUpToDate)
	if err := g.checksGreen(ctx, pr.HeadSHA); err != nil {
		return out, err
	}
	pass(GateChecks)
	if err := g.reviewPassed(ctx, pr.HeadSHA); err != nil {
		return out, err
	}
	pass(GateReview)
	comments, err := g.GH.PRComments(ctx, n)
	if err != nil {
		return out, refuse(GateHold, "could not read the comments of #%d: %v", n, err)
	}
	for _, c := range comments {
		if c.AuthorType != "Bot" && holdRE.MatchString(c.Body) {
			return out, refuse(GateHold, "%s wrote /hold on #%d; a human must lift it (edit or delete that comment)", c.Author, n)
		}
	}
	pass(GateHold)
	files, err := g.GH.PRFiles(ctx, n)
	if err != nil {
		return out, refuse(GateMigrations, "could not list the files of #%d: %v", n, err)
	}
	if err := g.lintMigrations(ctx, files, pr.HeadSHA); err != nil {
		return out, err
	}
	pass(GateMigrations)
	out.Noticed = SensitiveFiles(g.Ship.SensitivePaths, files)
	if len(out.Noticed) > 0 {
		paths := out.Noticed
		summary := fmt.Sprintf("Officer %s is merging #%d %q, which changes %d sensitive path(s). Not blocked: this is a notice.",
			g.OfficerID, n, truncate(pr.Title, 120), len(paths))
		if len(paths) > officers.MaxNoticePaths {
			summary += fmt.Sprintf(" (%d more not listed)", len(paths)-officers.MaxNoticePaths)
			paths = paths[:officers.MaxNoticePaths]
		}
		if err := g.HQ.PostNotice(ctx, g.Ship.NoticeURLPath(g.OfficerID), officers.Notice{PR: n, HeadSHA: pr.HeadSHA, Paths: paths,
			Summary: summary, Channel: g.Ship.NoticeChannel}); err != nil {
			return out, refuse(GateNotice, "the sensitive-path notice could not be posted to HQ (%v); nothing was merged — call again once HQ answers", err)
		}
	}
	pass(GateNotice)
	sha, err := g.GH.Merge(ctx, n, pr.HeadSHA)
	if err != nil {
		return out, refuse(GateMerge, "GitHub refused the squash merge of #%d at %s (the head moved, or it stopped being mergeable): %v", n, pr.HeadSHA, err)
	}
	out.MergeSHA = sha
	pass(GateMerge)
	return out, nil
}

func (g *Gates) hqEnabled(ctx context.Context) error {
	on, err := g.HQ.Enabled(ctx)
	if err != nil {
		return refuse(GateHQEnabled, "HQ did not confirm the officer is enabled (%v); merges and releases fail closed while HQ cannot answer", err)
	}
	if !on {
		return refuse(GateHQEnabled, "HQ has this officer disabled")
	}
	return nil
}

// checksGreen: every required check is success on sha — a commit status
// whose latest state is success, or (when no status of that name exists) a
// completed, successful check run. Missing counts as not passed.
func (g *Gates) checksGreen(ctx context.Context, sha string) error {
	st, err := g.GH.Statuses(ctx, sha)
	if err != nil {
		return refuse(GateChecks, "could not read the commit statuses of %s: %v", short(sha), err)
	}
	var bad []string
	for _, name := range g.Ship.RequiredChecks {
		if state, ok := st[name]; ok {
			if state != "success" {
				bad = append(bad, name+"="+state)
			}
			continue
		}
		runs, err := g.GH.CheckRuns(ctx, sha, name)
		if err != nil {
			return refuse(GateChecks, "could not read check run %q on %s: %v", name, short(sha), err)
		}
		if r, ok := newest(runs, name); !ok {
			bad = append(bad, name+"=missing")
		} else if r.Status != "completed" || r.Conclusion != "success" {
			bad = append(bad, name+"="+r.Status+"/"+r.Conclusion)
		}
	}
	if len(bad) > 0 {
		return refuse(GateChecks, "on %s: %s", short(sha), strings.Join(bad, ", "))
	}
	return nil
}

func newest(runs []CheckRun, name string) (CheckRun, bool) {
	var best CheckRun
	found := false
	for _, r := range runs {
		if r.Name == name && (!found || r.ID > best.ID) {
			best, found = r, true
		}
	}
	return best, found
}

func (g *Gates) reviewPassed(ctx context.Context, sha string) error {
	runs, err := g.GH.CheckRuns(ctx, sha, g.Ship.ReviewCheck)
	if err != nil {
		return refuse(GateReview, "could not read check run %q on %s: %v", g.Ship.ReviewCheck, short(sha), err)
	}
	var mine []CheckRun
	for _, r := range runs {
		if g.Ship.ReviewApp == "" || r.AppSlug == g.Ship.ReviewApp {
			mine = append(mine, r)
		}
	}
	r, ok := newest(mine, g.Ship.ReviewCheck)
	if !ok {
		return refuse(GateReview, "no %q check run on the head %s yet; wait for the review", g.Ship.ReviewCheck, short(sha))
	}
	if r.Status != "completed" || r.Conclusion != "success" {
		return refuse(GateReview, "the newest %q check run on %s is %s/%s; only completed/success passes (neutral does not)", g.Ship.ReviewCheck, short(sha), r.Status, r.Conclusion)
	}
	return nil
}

// lintMigrations reads every added or changed migration at ref and lints it;
// a removed or renamed migration is refused outright (an applied migration's
// name is its identity).
func (g *Gates) lintMigrations(ctx context.Context, files []File, ref string) error {
	if g.Ship.Migrations == "" {
		return nil
	}
	var findings []string
	for _, f := range files {
		in := strings.HasPrefix(f.Path, g.Ship.Migrations)
		was := f.Previous != "" && strings.HasPrefix(f.Previous, g.Ship.Migrations)
		if !in && !was {
			continue
		}
		switch f.Status {
		case "removed":
			findings = append(findings, f.Path+": a migration file is removed")
			continue
		case "renamed":
			if was {
				findings = append(findings, f.Previous+" → "+f.Path+": a migration file is renamed")
				continue
			}
		}
		if !in {
			continue
		}
		body, err := g.GH.FileAt(ctx, f.Path, ref)
		if err != nil {
			return refuse(GateMigrations, "could not read %s at %s: %v", f.Path, short(ref), err)
		}
		for _, fd := range LintMigration(f.Path, string(body)) {
			findings = append(findings, fmt.Sprintf("%s: %s (%s)", fd.File, fd.Rule, fd.Text))
		}
	}
	if len(findings) > 0 {
		return refuse(GateMigrations, "destructive DDL is refused; a human must write and ship it: %s", strings.Join(findings, "; "))
	}
	return nil
}

// SensitiveFiles are the changed paths (a rename's old path included) that
// match any pattern, sorted and unique.
func SensitiveFiles(patterns []string, files []File) []string {
	set := map[string]bool{}
	for _, f := range files {
		for _, p := range []string{f.Path, f.Previous} {
			if p == "" {
				continue
			}
			for _, pat := range patterns {
				if officers.SensitiveMatch(pat, p) {
					set[p] = true
				}
			}
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// --- releases ----------------------------------------------------------------

// Version is a vMAJOR.MINOR.PATCH tag.
type Version struct{ Major, Minor, Patch int }

var versionRE = regexp.MustCompile(`^v(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})$`)

// ParseVersion reads a release tag; ok is false for anything else.
func ParseVersion(tag string) (Version, bool) {
	m := versionRE.FindStringSubmatch(tag)
	if m == nil {
		return Version{}, false
	}
	a, _ := strconv.Atoi(m[1])
	b, _ := strconv.Atoi(m[2])
	c, _ := strconv.Atoi(m[3])
	return Version{a, b, c}, true
}

func (v Version) String() string { return fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch) }

// Less orders versions.
func (v Version) Less(w Version) bool {
	if v.Major != w.Major {
		return v.Major < w.Major
	}
	if v.Minor != w.Minor {
		return v.Minor < w.Minor
	}
	return v.Patch < w.Patch
}

// NextMinor is the release after v: minor bumped, patch 0.
func (v Version) NextMinor() Version { return Version{v.Major, v.Minor + 1, 0} }

// release is one release tag, peeled to its commit.
type release struct {
	Version Version
	Ref     TagRef
	Commit  string
}

// sortedReleases are every vX.Y.Z tag, highest first.
func sortedReleases(tags []TagRef) []release {
	var out []release
	for _, t := range tags {
		if v, ok := ParseVersion(t.Name); ok {
			out = append(out, release{Version: v, Ref: t})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[j].Version.Less(out[i].Version) })
	return out
}

func (g *Gates) peel(ctx context.Context, r release) (string, error) {
	switch r.Ref.ObjectType {
	case "commit":
		return r.Ref.ObjectSHA, nil
	case "tag":
		t, err := g.GH.TagObject(ctx, r.Ref.ObjectSHA)
		if err != nil {
			return "", err
		}
		if t.TargetType != "commit" {
			return "", fmt.Errorf("tag %s points at a %s, not a commit", r.Ref.Name, t.TargetType)
		}
		return t.TargetSHA, nil
	}
	return "", fmt.Errorf("tag %s points at a %s", r.Ref.Name, r.Ref.ObjectType)
}

// TagOptions are officer_tag_release's inputs.
type TagOptions struct {
	// Rollback is the rollout watch's own release of a revert: it skips the
	// in-flight, rate, quiet-hours and HQ-enabled gates (the release being
	// rolled back is the one in flight, and HQ may be what is down) and keeps
	// every other.
	Rollback bool
}

// TagOutcome is what officer_tag_release did.
type TagOutcome struct {
	Tag         string    `json:"tag"`
	SHA         string    `json:"sha"`
	Previous    string    `json:"previous"`
	PreviousSHA string    `json:"previous_sha"`
	TaggedAt    time.Time `json:"tagged_at"`
	Passed      []string  `json:"gates_passed"`
	Migrations  []string  `json:"migrations,omitempty"`
}

// Rate-limit bookkeeping bounds.
const (
	rateWindow       = 24 * time.Hour
	maxTagObjectRead = 60
	// Backup preflight thresholds.
	walMaxAge       = 5 * time.Minute
	walQuietFailure = time.Hour
	backupMaxAge    = 26 * time.Hour
)

// TagRelease creates the next minor release tag (annotated) on the base
// branch's head if, and only if, every release gate passes now:
//
//  1. stage — the officer's stage allows releasing;
//  2. hq_enabled — HQ says the officer is enabled (fail closed);
//  3. tags / ancestor — the highest vX.Y.Z tag T is an ancestor of the base's
//     head H, and H is ahead of it (else nothing to release);
//  4. in_flight — no release the rollout watch still owns, and the service
//     reports T's version (the previous release is live);
//  5. rate — fewer than releases_per_day officer tags in the last 24 h, and
//     none in the last min_release_gap;
//  6. quiet_hours — not inside them;
//  7. main_ci — the required checks are success for H's exact tree;
//  8. migrations / backup — no destructive DDL in a migration added in T..H,
//     and, when one was added, HQ's backup preflight passes;
//  9. tag_create — GitHub creates refs/tags/<next>; a tag pushed by someone
//     else first makes this fail, and the tool reports it.
func (g *Gates) TagRelease(ctx context.Context, opt TagOptions) (TagOutcome, error) {
	var out TagOutcome
	pass := func(name string) { out.Passed = append(out.Passed, name) }
	if !g.Ship.MayRelease() {
		return out, refuse(GateStage, "this officer's stage is %q; releasing needs %q", g.Ship.Stage, officers.StageRelease)
	}
	pass(GateStage)
	if !opt.Rollback {
		if err := g.hqEnabled(ctx); err != nil {
			return out, err
		}
		pass(GateHQEnabled)
	}
	tags, err := g.GH.Tags(ctx)
	if err != nil {
		return out, refuse(GateTags, "could not list the release tags: %v", err)
	}
	rels := sortedReleases(tags)
	if len(rels) == 0 {
		return out, refuse(GateTags, "no vX.Y.Z tag exists; the first release is a human's")
	}
	top := rels[0]
	if top.Commit, err = g.peel(ctx, top); err != nil {
		return out, refuse(GateTags, "could not resolve %s: %v", top.Ref.Name, err)
	}
	out.Previous, out.PreviousSHA = top.Version.String(), top.Commit
	pass(GateTags)
	head, err := g.GH.BranchSHA(ctx, g.Ship.Base)
	if err != nil {
		return out, refuse(GateAncestor, "could not read %s: %v", g.Ship.Base, err)
	}
	cmp, err := g.GH.Compare(ctx, top.Commit, head)
	if err != nil {
		return out, refuse(GateAncestor, "could not compare %s with %s: %v", top.Ref.Name, g.Ship.Base, err)
	}
	if cmp.Status == "identical" || cmp.AheadBy == 0 {
		return out, refuse(gateNothingToDo, "%s is at %s already; there is nothing to release", g.Ship.Base, top.Ref.Name)
	}
	if cmp.BehindBy != 0 {
		return out, refuse(GateAncestor, "%s (%s) is not an ancestor of %s's head %s (%s): someone tagged a commit off %s; a human must decide",
			top.Ref.Name, short(top.Commit), g.Ship.Base, short(head), cmp.Status, g.Ship.Base)
	}
	pass(GateAncestor)
	slug, err := g.GH.AppSlug(ctx)
	if err != nil {
		return out, refuse(GateRate, "could not read the officer App's identity: %v", err)
	}
	if !opt.Rollback {
		if tag, busy := g.InFlight(); busy {
			return out, refuse(GateInFlight, "release %s is still being watched; wait for its verdict (%s)", tag, officers.ToolRolloutStatus)
		}
		live, err := g.LiveVersion(ctx)
		if err != nil {
			return out, refuse(GateInFlight, "could not read the live version (%v); a release is never cut blind", err)
		}
		if live != top.Ref.Name {
			return out, refuse(GateInFlight, "the service reports %q, not the newest tag %s: that release is not live yet (or was not rolled out)", live, top.Ref.Name)
		}
		pass(GateInFlight)
		if err := g.rateAllows(ctx, rels, slug); err != nil {
			return out, err
		}
		pass(GateRate)
		if g.Ship.Quiet.Quiet(g.now()) {
			return out, refuse(GateQuietHours, "it is inside the quiet hours (%s); no officer release now", g.now().In(g.Ship.Quiet.Loc).Format("Mon 15:04 MST"))
		}
		pass(GateQuietHours)
	}
	if err := g.mainCIGreen(ctx, head); err != nil {
		return out, err
	}
	pass(GateMainCI)
	if g.Ship.Migrations != "" {
		var added []File
		for _, f := range cmp.Files {
			if strings.HasPrefix(f.Path, g.Ship.Migrations) && f.Status == "added" {
				added = append(added, f)
				out.Migrations = append(out.Migrations, f.Path)
			}
		}
		if err := g.lintMigrations(ctx, cmp.Files, head); err != nil {
			return out, err
		}
		pass(GateMigrations)
		if len(added) > 0 {
			if err := g.backupReady(ctx); err != nil {
				return out, err
			}
			pass(GateBackup)
		}
	}
	next := top.Version.NextMinor().String()
	now := g.now().UTC()
	bot := BotLogin(slug)
	msg := fmt.Sprintf("%s\n\nReleased by officer %s through muster's gates.\n%s %s, previous %s (%s).\n", next, g.OfficerID, g.Ship.Base,
		head, top.Ref.Name, top.Commit)
	if opt.Rollback {
		msg = fmt.Sprintf("%s\n\nRollback release by officer %s (a revert of %s).\n%s %s.\n", next, g.OfficerID, top.Ref.Name, g.Ship.Base, head)
	}
	obj, err := g.GH.CreateTag(ctx, next, msg, head, Tagger{Name: bot, Email: bot + "@users.noreply.github.com", When: now})
	if err != nil {
		return out, refuse(GateTagCreate, "GitHub refused the tag object %s: %v", next, err)
	}
	if err := g.GH.CreateRef(ctx, "refs/tags/"+next, obj); err != nil {
		if errors.Is(err, ErrRefExists) {
			return out, refuse(GateTagCreate, "%s already exists: someone tagged at the same moment, and nothing was created; call again to re-check every gate", next)
		}
		return out, refuse(GateTagCreate, "GitHub refused refs/tags/%s: %v", next, err)
	}
	pass(GateTagCreate)
	out.Tag, out.SHA, out.TaggedAt = next, head, now
	return out, nil
}

// rateAllows counts the officer's own (annotated, tagged by its bot) release
// tags in the last 24 h, newest first, stopping at the first older one.
func (g *Gates) rateAllows(ctx context.Context, rels []release, slug string) error {
	bot := BotLogin(slug)
	now := g.now()
	count, reads := 0, 0
	var newestOwn time.Time
	for _, r := range rels {
		if r.Ref.ObjectType != "tag" {
			continue
		}
		if reads++; reads > maxTagObjectRead {
			return refuse(GateRate, "could not establish the release rate within %d tag reads", maxTagObjectRead)
		}
		t, err := g.GH.TagObject(ctx, r.Ref.ObjectSHA)
		if err != nil {
			return refuse(GateRate, "could not read tag %s: %v", r.Ref.Name, err)
		}
		if t.Tagger != bot {
			continue
		}
		if newestOwn.IsZero() || t.TaggedAt.After(newestOwn) {
			newestOwn = t.TaggedAt
		}
		if now.Sub(t.TaggedAt) > rateWindow {
			break
		}
		count++
	}
	if count >= g.Ship.ReleasesPerDay {
		return refuse(GateRate, "%d officer releases in the last 24 h (limit %d)", count, g.Ship.ReleasesPerDay)
	}
	if !newestOwn.IsZero() && now.Sub(newestOwn) < g.Ship.MinReleaseGap {
		return refuse(GateRate, "the last officer release was %s ago; at least %s must pass", now.Sub(newestOwn).Round(time.Second), g.Ship.MinReleaseGap)
	}
	return nil
}

// mainCIGreen: the required checks passed for the EXACT tree at head. Either
// they are success on head itself, or head is the squash merge of a pull
// request whose head commit has them all success and the SAME tree — which a
// squash merge of an up-to-date head always has. CI that tests pull requests
// only (and posts nothing on the base branch) is covered by the second; a
// merge of a stale head, whose tree no CI run saw, is refused.
func (g *Gates) mainCIGreen(ctx context.Context, head string) error {
	direct := g.checksGreen(ctx, head)
	if direct == nil {
		return nil
	}
	prs, err := g.GH.PRsForCommit(ctx, head)
	if err != nil {
		return refuse(GateMainCI, "%v; and the pull request of %s could not be read: %v", direct, short(head), err)
	}
	for _, pr := range prs {
		if !pr.Merged || pr.MergeCommitSHA != head {
			continue
		}
		if err := g.checksGreen(ctx, pr.HeadSHA); err != nil {
			return refuse(GateMainCI, "%s is #%d's merge, and its head's checks are not green: %v", short(head), pr.Number, err)
		}
		ht, _, err := g.GH.Commit(ctx, head)
		if err != nil {
			return refuse(GateMainCI, "could not read %s: %v", short(head), err)
		}
		pt, _, err := g.GH.Commit(ctx, pr.HeadSHA)
		if err != nil {
			return refuse(GateMainCI, "could not read %s: %v", short(pr.HeadSHA), err)
		}
		if ht != pt {
			return refuse(GateMainCI, "%s (#%d's merge) has a tree its checks never saw: #%d was merged behind %s. Its tree is untested; release after the next up-to-date merge",
				short(head), pr.Number, pr.Number, g.Ship.Base)
		}
		return nil
	}
	return refuse(GateMainCI, "the checks are not green on %s's head %s, and it is no pull request's merge: %v", g.Ship.Base, short(head), direct)
}

// backupReady is HQ's preflight: WAL archived in the last 5 minutes, no
// archive failure in the last hour, a base backup in the last 26 hours.
func (g *Gates) backupReady(ctx context.Context) error {
	p, err := g.HQ.Preflight(ctx)
	if err != nil {
		return refuse(GateBackup, "this release adds a migration, and HQ's backup preflight did not answer (%v)", err)
	}
	now := g.now()
	switch {
	case p.WAL.LastArchivedTime == nil || now.Sub(*p.WAL.LastArchivedTime) > walMaxAge:
		return refuse(GateBackup, "the last WAL archive is missing or older than %s", walMaxAge)
	case p.WAL.LastFailedTime != nil && now.Sub(*p.WAL.LastFailedTime) < walQuietFailure:
		return refuse(GateBackup, "WAL archiving failed within the last %s", walQuietFailure)
	case p.LastBackupTime == nil || now.Sub(*p.LastBackupTime) > backupMaxAge:
		return refuse(GateBackup, "the last base backup is missing or older than %s", backupMaxAge)
	}
	return nil
}
