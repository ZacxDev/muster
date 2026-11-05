package officership

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/redact"
)

// ---------------------------------------------------------------------------
// THE DRAFT GATE: what a `draft` officer's pull requests must also pass
// (officers.StageDraft; docs/officers.md "The draft stage").
//
// At POST pulls and at every PATCH pulls/<n>, after the allowlist and the
// own-pull-request rule:
//
//   - the pull request is a draft (POST: "draft": true; PATCH: never `draft`,
//     never state "open" — refused in Admit before any read);
//   - its body keeps the contract: the first line
//     officers.BodyFirstLine(officer, item) — the item is the branch's — and
//     every officers.BodySections heading on a line of its own;
//   - its title and body carry no secret-shaped value (redact.HasSecretValue);
//   - the rate limits (POST only): officers.MaxOpenPRsPerRepo open in this
//     repository, officers.MaxNewPRsPerDay opened this UTC day across the
//     officer's repositories, officers.MaxPRsPerItem for the item, in any
//     state;
//   - GitHub's comparison base...head: no changed, added, removed or renamed
//     path (old or new) matches the repository's forbidden_paths; no removed
//     line declares a PersistentVolumeClaim, a PersistentVolume, a CloudNative
//     PG resource or a Flagger Canary; no added line has a secret-shaped
//     value; and no file's diff is withheld by GitHub (a binary or very large
//     file). checkComparison is that check, the ONE every entry point runs.
//
// A content refusal (the comparison, or a secret shape) is answered 409 and
// posted to HQ as a notice. A comment (POST issues/<n>/comments) is checked
// for a secret shape too. A PATCH that only closes the pull request is not
// read further: closing never needs a wall.
//
// 🔴 A PUSH IS CHECKED TOO, AFTER GitHub HAS IT. A push to a branch that
// already has an open pull request changes that pull request's diff without
// any PATCH. The pack cannot be diffed before it is forwarded (it is a THIN
// pack: its objects are deltas against objects only GitHub holds), so the
// push is admitted, and once GitHub answers it 200 (afterPush) the comparison
// base...<the pushed commit> of every open pull request from that branch is
// run through checkComparison. On a hit — or when the comparison cannot be
// read — the pull request is CLOSED and HQ told (paths only). The officer
// cannot reopen it: a draft officer's PATCH state "open" is refused before
// any read (pushgate.go), and a new pull request for the item is over the
// per-item limit. Between the push and the close (one comparison read) the
// pull request shows the pushed diff.
// ---------------------------------------------------------------------------

// removedKindRE are the manifest lines whose REMOVAL a draft officer may never
// propose: deleting one deletes data or a running service's rollout state.
var removedKindRE = []*regexp.Regexp{
	regexp.MustCompile(`["']?\bkind["']?\s*:\s*["']?(PersistentVolumeClaim|PersistentVolume|Canary)\b`),
	regexp.MustCompile(`["']?\bapiVersion["']?\s*:\s*["']?postgresql\.cnpg\.io/`),
}

// draftCreate is the draft gate of POST pulls: head is the branch (no owner
// prefix), m the request body.
func (g *PushGate) draftCreate(ctx context.Context, sr officers.ShipRepo, gh GitHub, head string, m map[string]json.RawMessage) error {
	if gh == nil {
		return fmt.Errorf("no GitHub reader for %s", sr.Repo)
	}
	var draft bool
	if raw, ok := m["draft"]; !ok || json.Unmarshal(raw, &draft) != nil || !draft {
		return refused(`a pull request that is not a draft: a draft officer opens every pull request with "draft": true`)
	}
	item, _ := officers.BranchItem(sr.BranchPrefix, g.OfficerID, head)
	title, body := jsonString(m["title"]), jsonString(m["body"])
	if err := g.bodyContract(body, item); err != nil {
		return err
	}
	if redact.HasSecretValue(title) || redact.HasSecretValue(body) {
		return g.contentRefusal(ctx, sr, 0, "", head, nil, "its title or body has a credential-shaped value")
	}
	if err := g.rateLimits(ctx, sr, item); err != nil {
		return err
	}
	return g.refuseOnHit(ctx, sr, 0, head, g.checkComparison(ctx, sr, gh, sr.Base, head))
}

// draftEdit is the draft gate of PATCH pulls/<n> of the officer's own pr.
func (g *PushGate) draftEdit(ctx context.Context, sr officers.ShipRepo, gh GitHub, pr PR, m map[string]json.RawMessage) error {
	if len(m) == 1 && strings.EqualFold(jsonString(m["state"]), "closed") {
		return nil
	}
	if raw, ok := m["body"]; ok {
		var body string
		if json.Unmarshal(raw, &body) != nil {
			return refused("a body that is not a string")
		}
		item, ok := officers.BranchItem(sr.BranchPrefix, g.OfficerID, pr.HeadRef)
		if !ok {
			return refused("pull request #%d's branch %s is not %s%s/<item>-<slug>, so its item is unknown", pr.Number, pr.HeadRef,
				sr.BranchPrefix, officers.Slug(g.OfficerID))
		}
		if err := g.bodyContract(body, item); err != nil {
			return err
		}
		if redact.HasSecretValue(body) {
			return g.contentRefusal(ctx, sr, pr.Number, pr.HeadSHA, pr.HeadRef, nil, "its new body has a credential-shaped value")
		}
	}
	if redact.HasSecretValue(jsonString(m["title"])) {
		return g.contentRefusal(ctx, sr, pr.Number, pr.HeadSHA, pr.HeadRef, nil, "its new title has a credential-shaped value")
	}
	return g.refuseOnHit(ctx, sr, pr.Number, pr.HeadRef, g.checkComparison(ctx, sr, gh, pr.BaseRef, pr.HeadSHA))
}

// draftComment is the draft gate of POST issues/<n>/comments.
func (g *PushGate) draftComment(ctx context.Context, sr officers.ShipRepo, pr PR, body string) error {
	if redact.HasSecretValue(body) {
		return g.contentRefusal(ctx, sr, pr.Number, pr.HeadSHA, pr.HeadRef, nil, "the comment has a credential-shaped value")
	}
	return nil
}

// bodyContract refuses a body without the first line and the sections.
func (g *PushGate) bodyContract(body string, item int) error {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	want := officers.BodyFirstLine(g.OfficerID, item)
	if strings.TrimSpace(lines[0]) != want {
		return refused("the body's first line must be exactly %q (the item is the branch's)", want)
	}
	have := map[string]bool{}
	for _, l := range lines[1:] {
		have[strings.TrimSpace(l)] = true
	}
	var missing []string
	for _, h := range officers.BodySections {
		if !have[h] {
			missing = append(missing, h)
		}
	}
	if len(missing) > 0 {
		return refused("the body is missing the section heading(s) %s, each on a line of its own", strings.Join(missing, ", "))
	}
	return nil
}

// rateLimits refuses a new pull request for item in sr over a limit. Every
// ship repository's pull requests are read; only the officer's own count (its
// App's bot, typed Bot, from its branch shape).
func (g *PushGate) rateLimits(ctx context.Context, sr officers.ShipRepo, item int) error {
	now := time.Now
	if g.Now != nil {
		now = g.Now
	}
	t := now().UTC()
	midnight := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	open, today, forItem := 0, 0, 0
	for _, r := range g.Ship.RepoList(g.Repo) {
		gh := g.ghFor(r.Repo)
		if gh == nil {
			return fmt.Errorf("no GitHub reader for %s", r.Repo)
		}
		slug, err := gh.AppSlug(ctx)
		if err != nil {
			return fmt.Errorf("could not read the officer App's identity: %v", err)
		}
		prs, err := gh.ListPRs(ctx)
		if err != nil {
			return fmt.Errorf("could not list %s's pull requests: %v", r.Repo, err)
		}
		for _, p := range prs {
			n, ok := officers.BranchItem(r.BranchPrefix, g.OfficerID, p.HeadRef)
			if !ok || !strings.EqualFold(p.Author, BotLogin(slug)) || p.AuthorType != "Bot" {
				continue
			}
			if strings.EqualFold(r.Repo, sr.Repo) && p.State == "open" {
				open++
			}
			if !p.CreatedAt.Before(midnight) {
				today++
			}
			if n == item {
				forItem++
			}
		}
	}
	switch {
	case forItem >= officers.MaxPRsPerItem:
		return refused("item %d already has %d pull request(s) (any state); the limit is %d per item", item, forItem, officers.MaxPRsPerItem)
	case open >= officers.MaxOpenPRsPerRepo:
		return refused("%d of this officer's pull requests are open in %s; the limit is %d", open, sr.Repo, officers.MaxOpenPRsPerRepo)
	case today >= officers.MaxNewPRsPerDay:
		return refused("this officer opened %d pull request(s) today (UTC); the limit is %d a day", today, officers.MaxNewPRsPerDay)
	}
	return nil
}

// comparisonVerdict is checkComparison's answer. Paths empty and Err nil:
// admitted. Err set: the comparison could not be read (the policy could not
// decide). Paths set: refused, for Why.
type comparisonVerdict struct {
	HeadSHA string
	Paths   []string
	Why     string
	Err     error
}

// checkComparison is THE content check of a draft officer's change, the ONE
// all three entry points run — POST pulls (draftCreate), PATCH pulls/<n>
// (draftEdit) and a push into an open pull request's branch (afterPush):
// GitHub's comparison base...head in sr through ForbiddenFiles, with sr's
// forbidden globs and, when set, its forbidden_paths_file read fresh from
// base. It decides; each caller answers the verdict its own way (a 409, or a
// close).
func (g *PushGate) checkComparison(ctx context.Context, sr officers.ShipRepo, gh GitHub, base, head string) comparisonVerdict {
	if gh == nil {
		return comparisonVerdict{Err: fmt.Errorf("no GitHub reader for %s", sr.Repo)}
	}
	cmp, err := gh.Compare(ctx, base, head)
	if err != nil {
		return comparisonVerdict{Err: fmt.Errorf("could not compare %s...%s in %s: %v", base, head, sr.Repo, err)}
	}
	v := comparisonVerdict{}
	if len(cmp.Commits) > 0 {
		v.HeadSHA = cmp.Commits[len(cmp.Commits)-1]
	}
	forbidden := sr.ForbiddenPaths
	if f := sr.ForbiddenPathsFile; f != "" {
		// 🔴 ONE SOURCE, READ FRESH, FAIL CLOSED: the list is read from the
		// TARGET branch at every check (never the head, which the officer
		// writes), muster keeps no copy, and a list it cannot read or parse
		// refuses the pull request. The file itself is forbidden, so an
		// officer cannot widen its own wall.
		raw, err := gh.FileAt(ctx, f, base)
		if err != nil {
			v.Paths = []string{f}
			v.Why = fmt.Sprintf("its forbidden-path list %s could not be read at %s, so nothing can be checked: %v", f, base, err)
			return v
		}
		list, err := officers.ParseForbiddenList(string(raw))
		if err != nil {
			v.Paths = []string{f}
			v.Why = fmt.Sprintf("its forbidden-path list %s at %s does not parse (%v), so nothing can be checked", f, base, err)
			return v
		}
		forbidden = append(append(append([]string(nil), forbidden...), list...), f)
	}
	v.Paths, v.Why = ForbiddenFiles(forbidden, cmp.Files)
	return v
}

// refuseOnHit is a create's or a PATCH's answer to a verdict: an unreadable
// comparison is a plain error (502: the policy could not decide), a hit a 409
// and an HQ notice.
func (g *PushGate) refuseOnHit(ctx context.Context, sr officers.ShipRepo, n int, branch string, v comparisonVerdict) error {
	if v.Err != nil {
		return v.Err
	}
	if len(v.Paths) > 0 {
		return g.contentRefusal(ctx, sr, n, v.HeadSHA, branch, v.Paths, v.Why)
	}
	return nil
}

// pushTimeout bounds afterPush: it runs after the upstream answered, in the
// handler, so the agent's push returns once it is done.
const pushTimeout = time.Minute

// afterPush is a draft officer's push re-check (see the comment at the top):
// called by the connector proxy once GitHub answered the push of cmds to sr.
// A push GitHub refused (any status but 200) changed nothing and is not read.
func (g *PushGate) afterPush(ctx context.Context, sr officers.ShipRepo, cmds []pushCommand, res connectors.ForwardResult) {
	if status := res.Status; status != http.StatusOK || len(cmds) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pushTimeout)
	defer cancel()
	logf := func(format string, args ...any) {
		if g.Logf != nil {
			g.Logf(format, args...)
		}
	}
	gh := g.ghFor(sr.Repo)
	if gh == nil {
		logf("officers: %s: a push to %s cannot be re-checked: no GitHub reader", g.OfficerID, sr.Repo)
		return
	}
	prs, err := gh.ListPRs(ctx)
	if err != nil {
		// 🔴 NOTHING TO CLOSE IS KNOWN, so this is told, not acted on: HQ
		// hears that a push went unchecked.
		g.announce(ctx, officers.Notice{Channel: g.Ship.NoticeChannel, Summary: fmt.Sprintf(
			"officer %s: muster could not list %s's pull requests after a push, so the push was NOT re-checked: %v", g.OfficerID, sr.Repo, err)})
		return
	}
	for _, c := range cmds {
		branch := strings.TrimPrefix(c.ref, "refs/heads/")
		// GitHub answers 200 to a push whose ref update it REFUSED (a
		// non-fast-forward is an "ng" line in the body). A branch that is
		// readable and not at the pushed commit was not changed by it.
		if at, err := gh.BranchSHA(ctx, branch); err == nil && at != c.newOID {
			logf("officers: %s: %s in %s is at %s, not the pushed %s: the push did not update it", g.OfficerID, branch, sr.Repo,
				shortSHA(at), shortSHA(c.newOID))
			continue
		}
		for _, pr := range prs {
			if pr.State != "open" || pr.HeadRef != branch || (pr.HeadRepo != "" && !strings.EqualFold(pr.HeadRepo, sr.Repo)) {
				continue
			}
			v := g.checkComparison(ctx, sr, gh, pr.BaseRef, c.newOID)
			paths, why := v.Paths, v.Why
			if v.Err != nil {
				// FAIL CLOSED: an unreadable comparison is an unchecked one.
				why = v.Err.Error() + ", so the pushed change could not be checked"
			} else if len(paths) == 0 {
				continue
			}
			g.closeOnHit(ctx, sr, gh, pr, c.newOID, paths, why)
		}
	}
}

// closeOnHit closes pull request pr after a push put a refused change (or an
// uncheckable one) into it, and tells HQ: paths, never content.
func (g *PushGate) closeOnHit(ctx context.Context, sr officers.ShipRepo, gh GitHub, pr PR, sha string, paths []string, why string) {
	what := fmt.Sprintf("closed pull request #%d (%s) in %s after a push of %s", pr.Number, pr.HeadRef, sr.Repo, shortSHA(sha))
	if err := gh.ClosePR(ctx, pr.Number); err != nil {
		what = fmt.Sprintf("could NOT close pull request #%d (%s) in %s after a push of %s (%v); a human must", pr.Number, pr.HeadRef, sr.Repo,
			shortSHA(sha), err)
	}
	summary := fmt.Sprintf("officer %s: muster %s: %s", g.OfficerID, what, why)
	ps := paths
	if len(ps) > officers.MaxNoticePaths {
		summary += fmt.Sprintf(" (+%d more paths)", len(ps)-officers.MaxNoticePaths)
		ps = ps[:officers.MaxNoticePaths]
	}
	g.announce(ctx, officers.Notice{PR: pr.Number, HeadSHA: sha, Paths: ps, Summary: summary, Channel: g.Ship.NoticeChannel})
}

// SweepInterval is how often the draft sweep (Sweep) re-checks every open
// pull request of a draft officer.
const SweepInterval = 5 * time.Minute

// maxSweepMisses is how many sweeps in a row may fail to read a pull
// request's comparison before the sweep closes it (fail closed).
const maxSweepMisses = 3

// Sweep is the draft sweep: every open pull request of this officer, in each
// ship repository, re-checked at its current head.
func (g *PushGate) Sweep(ctx context.Context) error { return nil }

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// announce posts n to HQ; a failure is logged, never fatal.
func (g *PushGate) announce(ctx context.Context, n officers.Notice) {
	if g.Announce == nil {
		return
	}
	if err := g.Announce(ctx, n); err != nil && g.Logf != nil {
		g.Logf("officers: %s: the notice did not reach HQ: %v", g.OfficerID, err)
	}
}

// ForbiddenFiles is THE content rule of a draft officer's comparison: the
// paths that make it refused and why (none: admitted).
func ForbiddenFiles(forbidden []string, files []File) ([]string, string) {
	var hit []string
	var why []string
	add := func(p, reason string) {
		hit = append(hit, p)
		why = append(why, p+": "+reason)
	}
	for _, f := range files {
		for _, p := range []string{f.Path, f.Previous} {
			if p == "" {
				continue
			}
			for _, pat := range forbidden {
				if officers.MatchForbidden(pat, p) {
					add(p, fmt.Sprintf("a forbidden path (%s)", pat))
					break
				}
			}
		}
		if f.Patch == "" && f.Changes > 0 {
			add(f.Path, "GitHub withholds its diff (binary or too large), so it cannot be checked")
			continue
		}
		var added []string
		removedKind := ""
		for _, l := range strings.Split(f.Patch, "\n") {
			switch {
			case strings.HasPrefix(l, "+"):
				added = append(added, l[1:])
			case strings.HasPrefix(l, "-") && removedKind == "":
				for _, re := range removedKindRE {
					if m := re.FindString(l[1:]); m != "" {
						removedKind = strings.TrimSpace(m)
					}
				}
			}
		}
		if removedKind != "" {
			add(f.Path, fmt.Sprintf("it removes a %q line (a volume, database or canary must never be deleted by an officer)", removedKind))
		}
		if redact.HasSecretValueIn(f.Path, strings.Join(added, "\n")) {
			add(f.Path, "an added line has a credential-shaped value")
		}
	}
	if len(hit) == 0 {
		return nil, ""
	}
	sort.Strings(hit)
	return dedupe(hit), strings.Join(why, "; ")
}

func dedupe(in []string) []string {
	out := in[:0]
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// contentRefusal is a 409 refusal posted to HQ as a notice (paths may be
// none: a title or body). The notice and the refusal never quote content.
func (g *PushGate) contentRefusal(ctx context.Context, sr officers.ShipRepo, n int, headSHA, branch string, paths []string, why string) error {
	what := "a new pull request from " + branch
	if n > 0 {
		what = fmt.Sprintf("an edit of pull request #%d (%s)", n, branch)
	}
	summary := fmt.Sprintf("officer %s: muster refused %s in %s: %s", g.OfficerID, what, sr.Repo, why)
	ps := paths
	if len(ps) > officers.MaxNoticePaths {
		summary += fmt.Sprintf(" (+%d more paths)", len(ps)-officers.MaxNoticePaths)
		ps = ps[:officers.MaxNoticePaths]
	}
	g.announce(ctx, officers.Notice{PR: n, HeadSHA: headSHA, Paths: ps, Summary: summary, Channel: g.Ship.NoticeChannel})
	return conflict("%s in %s: %s", what, sr.Repo, why)
}
