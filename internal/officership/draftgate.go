package officership

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

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
//   - its title and body carry no secret shape (redact.HasSecret);
//   - the rate limits (POST only): officers.MaxOpenPRsPerRepo open in this
//     repository, officers.MaxNewPRsPerDay opened this UTC day across the
//     officer's repositories, officers.MaxPRsPerItem for the item, in any
//     state;
//   - GitHub's comparison base...head: no changed, added, removed or renamed
//     path (old or new) matches the repository's forbidden_paths; no removed
//     line declares a PersistentVolumeClaim, a PersistentVolume, a CloudNative
//     PG resource or a Flagger Canary; no added line has a secret shape; and
//     no file's diff is withheld by GitHub (a binary or very large file).
//
// A content refusal (the comparison, or a secret shape) is answered 409 and
// posted to HQ as a notice. A comment (POST issues/<n>/comments) is checked
// for a secret shape too. A PATCH that only closes the pull request is not
// read further: closing never needs a wall.
//
// 🔴 WHAT THIS CANNOT SEE: a push to a branch that already has a pull request
// changes that pull request's diff without any PATCH. The next PATCH reads it
// again, and the target repository's own CI is the wall for the rest.
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
	var draft bool
	if raw, ok := m["draft"]; !ok || json.Unmarshal(raw, &draft) != nil || !draft {
		return refused(`a pull request that is not a draft: a draft officer opens every pull request with "draft": true`)
	}
	item, _ := officers.BranchItem(sr.BranchPrefix, g.OfficerID, head)
	title, body := jsonString(m["title"]), jsonString(m["body"])
	if err := g.bodyContract(body, item); err != nil {
		return err
	}
	if redact.HasSecret(title) || redact.HasSecret(body) {
		return g.contentRefusal(ctx, sr, 0, "", head, nil, "its title or body has a credential-shaped value")
	}
	if err := g.rateLimits(ctx, sr, item); err != nil {
		return err
	}
	return g.compareCheck(ctx, sr, gh, 0, head, sr.Base, head)
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
		if redact.HasSecret(body) {
			return g.contentRefusal(ctx, sr, pr.Number, pr.HeadSHA, pr.HeadRef, nil, "its new body has a credential-shaped value")
		}
	}
	if redact.HasSecret(jsonString(m["title"])) {
		return g.contentRefusal(ctx, sr, pr.Number, pr.HeadSHA, pr.HeadRef, nil, "its new title has a credential-shaped value")
	}
	return g.compareCheck(ctx, sr, gh, pr.Number, pr.HeadRef, pr.BaseRef, pr.HeadSHA)
}

// draftComment is the draft gate of POST issues/<n>/comments.
func (g *PushGate) draftComment(ctx context.Context, sr officers.ShipRepo, pr PR, body string) error {
	if redact.HasSecret(body) {
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

// compareCheck reads base...head in sr and refuses the pull request (n, 0 at
// create; branch names it) when the comparison holds anything the draft gate
// refuses. A read failure is a plain error: the policy could not decide.
func (g *PushGate) compareCheck(ctx context.Context, sr officers.ShipRepo, gh GitHub, n int, branch, base, head string) error {
	cmp, err := gh.Compare(ctx, base, head)
	if err != nil {
		return fmt.Errorf("could not compare %s...%s in %s: %v", base, head, sr.Repo, err)
	}
	headSHA := ""
	if len(cmp.Commits) > 0 {
		headSHA = cmp.Commits[len(cmp.Commits)-1]
	}
	if bad, why := ForbiddenFiles(sr.ForbiddenPaths, cmp.Files); len(bad) > 0 {
		return g.contentRefusal(ctx, sr, n, headSHA, branch, bad, why)
	}
	return nil
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
		if redact.HasSecret(strings.Join(added, "\n")) {
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
	if g.Notify != nil {
		ps := paths
		if len(ps) > officers.MaxNoticePaths {
			summary += fmt.Sprintf(" (+%d more paths)", len(ps)-officers.MaxNoticePaths)
			ps = ps[:officers.MaxNoticePaths]
		}
		if err := g.Notify(ctx, officers.Notice{PR: n, HeadSHA: headSHA, Paths: ps, Summary: summary, Channel: g.Ship.NoticeChannel}); err != nil && g.Logf != nil {
			g.Logf("officers: %s: the refusal notice did not reach HQ: %v", g.OfficerID, err)
		}
	}
	return conflict("%s in %s: %s", what, sr.Repo, why)
}
