package officership

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
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
//   - (POST only) the branch's item is one HQ shows THIS instance holding
//     (ItemClaimed), and the rate limits hold: officers.MaxOpenPRsPerRepo open
//     in this repository, officers.MaxNewPRsPerDay opened this UTC day across
//     the officer's repositories, officers.MaxPRsPerItem for the item, in any
//     state — counted from EVERY pull request the officer's App opened
//     (GitHub.OfficerPRs, every page; too many pages is a refusal, never a
//     truncated count). One create of an officer runs at a time (createSem);
//   - GitHub's comparison base...head passes ForbiddenFiles (forbidden paths,
//     removed or renamed volume/database/canary manifests, secret shapes,
//     unverifiable files) and checkComparison's kustomization rule.
//     checkComparison is that check, the ONE every entry point runs.
//
// A content refusal (the comparison, or a secret shape) is answered 409 and
// posted to HQ as a notice. A comment (POST issues/<n>/comments) is checked
// for a secret shape too. A PATCH that only closes the pull request is not
// read further: closing never needs a wall.
//
// 🔴 A CHANGE THAT REACHES A PULL REQUEST WITHOUT A PATCH IS CHECKED AFTER
// GitHub HAS IT, THREE WAYS, and on a hit (or an unreadable comparison) the
// pull request is CLOSED, its branch deleted (a content hit only), and HQ told
// — paths, never content:
//
//   - afterPush: a push to a branch with an open pull request. The pack cannot
//     be diffed before it is forwarded (a THIN pack), so the push is admitted
//     and re-checked once the proxy has GitHub's answer — whatever the client
//     did (the proxy runs the hook deferred, on a detached context). GitHub's
//     report-status ("ok"/"ng" per ref) decides whether the push applied; an
//     answer that cannot be read is checked anyway (fail closed).
//   - afterCreate: a new pull request is checked again at the head GitHub
//     created it on, so a push landing between the create's check and
//     GitHub's create is caught.
//   - Sweep, every SweepInterval: every open pull request of the officer at
//     its current head — the backstop for anything the two hooks missed
//     (a muster restart mid-push included). A head already checked clean is
//     not read again.
//
// The officer cannot reopen a closed pull request (PATCH state "open" is
// refused) nor open another for the item (the per-item limit). Between a
// change landing and the close, the pull request shows it.
// ---------------------------------------------------------------------------

// DefaultCreateWait is how long a create waits for the officer's previous
// one to finish before it is refused.
const DefaultCreateWait = 2 * time.Minute

// draftCreate is the draft gate of POST pulls: head is the branch (no owner
// prefix), m the request body. 🔴 On nil it HOLDS the officer's create lock:
// the caller releases it (afterCreate, or releaseCreate on its own failure).
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
	if err := g.claimed(ctx, item); err != nil {
		return err
	}
	if err := g.acquireCreate(ctx); err != nil {
		return err
	}
	err := g.rateLimits(ctx, sr, item)
	if err == nil {
		err = g.refuseOnHit(ctx, sr, 0, head, g.checkComparison(ctx, sr, gh, sr.Base, head))
	}
	if err != nil {
		g.releaseCreate()
	}
	return err
}

// claimed refuses a new pull request for item unless HQ shows the requesting
// instance holding it (fail closed: no caller, no checker, or HQ's answer
// unreadable is a refusal).
func (g *PushGate) claimed(ctx context.Context, item int) error {
	c, ok := connectors.CallerOf(ctx)
	if !ok || c.AgentName == "" {
		return refused("a pull request for item %d from an unknown instance: its claim on the item cannot be checked", item)
	}
	if g.ItemClaimed == nil {
		return refused("a pull request for item %d: muster cannot check HQ's claim on it", item)
	}
	it, err := g.ItemClaimed(ctx, c.AgentName, item)
	if err != nil {
		return refused("a pull request for item %d, which HQ does not show %s holding (claim it with %s first): %v", item, c.AgentName,
			officers.ToolClaim, err)
	}
	// 🔴 A FIX ITEM OPENS NOTHING (fixpush.go): its fix goes to the pull
	// request it names.
	if IsPRFix(it.Kind) {
		return refused("a pull request for item %d, a fix item (%s): a fix item pushes to the pull request's existing branch (%s), it never opens a new one",
			item, it.Kind, it.SubjectKey)
	}
	return nil
}

// acquireCreate takes the officer's create lock, waiting at most CreateWait.
// ⚠ One muster process: replicas would each hold their own.
func (g *PushGate) acquireCreate(ctx context.Context) error {
	g.createOnce.Do(func() { g.createSem = make(chan struct{}, 1) })
	wait := g.CreateWait
	if wait <= 0 {
		wait = DefaultCreateWait
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case g.createSem <- struct{}{}:
		return nil
	case <-t.C:
		return refused("another pull request of this officer is being created; retry once it is done")
	case <-ctx.Done():
		return fmt.Errorf("waiting for this officer's previous create: %w", ctx.Err())
	}
}

// releaseCreate gives the create lock back (a no-op when it is free).
func (g *PushGate) releaseCreate() {
	g.createOnce.Do(func() { g.createSem = make(chan struct{}, 1) })
	select {
	case <-g.createSem:
	default:
	}
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

// rateLimits refuses a new pull request for item in sr over a limit. EVERY
// pull request the officer's App opened in each ship repository is read
// (OfficerPRs: GitHub's pulls list kept by author, every page). Only the
// officer's own count (its App's bot, typed Bot), each by its item: the body's
// first line when it names one for this officer (the gate enforces that line
// on every create and body edit), else the branch, read from the pull request
// itself (a human may have edited the body); a pull request whose item
// resolves neither way is not the officer's shape and is not counted.
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
		prs, err := g.officerPRs(ctx, r, gh, BotLogin(slug), "all")
		if err != nil {
			return fmt.Errorf("could not list %s's pull requests by this officer: %v", r.Repo, err)
		}
		for _, p := range prs {
			if !strings.EqualFold(p.Author, BotLogin(slug)) || p.AuthorType != "Bot" {
				continue
			}
			n, ok := g.itemOf(ctx, r, gh, p)
			if !ok {
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

// itemOf is the item of the officer's pull request p in r: from its body's
// first line, else its branch (read from GitHub; a failed read is not
// guessed: it counts nowhere, and is logged).
func (g *PushGate) itemOf(ctx context.Context, r officers.ShipRepo, gh GitHub, p PR) (int, bool) {
	first, _, _ := strings.Cut(strings.ReplaceAll(p.Body, "\r\n", "\n"), "\n")
	if n, ok := bodyItem(g.OfficerID, strings.TrimSpace(first)); ok {
		return n, true
	}
	head := p.HeadRef
	if head == "" {
		full, err := gh.PR(ctx, p.Number)
		if err != nil {
			g.logf("officers: %s: pull request #%d in %s could not be read for its item: %v", g.OfficerID, p.Number, r.Repo, err)
			return 0, false
		}
		head = full.HeadRef
	}
	return officers.BranchItem(r.BranchPrefix, g.OfficerID, head)
}

// bodyItem is the item a body's first line names, when it is exactly
// officers.BodyFirstLine(officer, item).
func bodyItem(officerID, first string) (int, bool) {
	prefix, suffix, ok := strings.Cut(officers.BodyFirstLine(officerID, 1), " 1 ")
	if !ok || !strings.HasPrefix(first, prefix+" ") || !strings.HasSuffix(first, " "+suffix) {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(first, prefix+" "), " "+suffix))
	if err != nil || n <= 0 || officers.BodyFirstLine(officerID, n) != first {
		return 0, false
	}
	return n, true
}

func (g *PushGate) logf(format string, args ...any) {
	if g.Logf != nil {
		g.Logf(format, args...)
	}
}

// comparisonVerdict is checkComparison's answer. Paths empty and Err nil:
// admitted. Err set: the comparison could not be read (the policy could not
// decide). Paths set: refused, for Why; Unverified marks a refusal that names
// no wrong content (the forbidden-path list could not be read or parsed), so
// a close on it deletes no branch and the sweep retries it.
type comparisonVerdict struct {
	HeadSHA    string
	Paths      []string
	Why        string
	Err        error
	Unverified bool
}

// checkComparison is THE content check of a draft officer's change, the ONE
// every entry point runs — POST pulls (draftCreate), PATCH pulls/<n>
// (draftEdit), a push into an open pull request's branch (afterPush), a new
// pull request (afterCreate) and the sweep (Sweep): GitHub's comparison
// base...head in sr through ForbiddenFiles, with sr's forbidden globs and,
// when set, its forbidden_paths_file read fresh from base, then the
// kustomization rule (removedKustomizeEntries). It decides; each caller
// answers the verdict its own way (a 409, or a close).
func (g *PushGate) checkComparison(ctx context.Context, sr officers.ShipRepo, gh GitHub, base, head string) comparisonVerdict {
	var files []string
	if sr.ForbiddenPathsFile != "" {
		files = []string{sr.ForbiddenPathsFile}
	}
	return g.checkWall(ctx, sr.Repo, gh, sr.ForbiddenPaths, files, base, head)
}

// checkWall is checkComparison with the wall spelled out: the globs, and the
// forbidden-path list files read fresh from base (a shepherd's wall has one
// per source: officers.ShepherdRules).
func (g *PushGate) checkWall(ctx context.Context, repo string, gh GitHub, globs, files []string, base, head string) comparisonVerdict {
	if gh == nil {
		return comparisonVerdict{Err: fmt.Errorf("no GitHub reader for %s", repo)}
	}
	cmp, err := gh.Compare(ctx, base, head)
	if err != nil {
		return comparisonVerdict{Err: fmt.Errorf("could not compare %s...%s in %s: %v", base, head, repo, err)}
	}
	v := comparisonVerdict{}
	if len(cmp.Commits) > 0 {
		v.HeadSHA = cmp.Commits[len(cmp.Commits)-1]
	}
	forbidden := append([]string(nil), globs...)
	for _, f := range files {
		// 🔴 ONE SOURCE, READ FRESH, FAIL CLOSED: the list is read from the
		// TARGET branch at every check (never the head, which the officer
		// writes), muster keeps no copy, and a list it cannot read or parse
		// refuses the pull request. The file itself is forbidden, so an
		// officer cannot widen its own wall.
		raw, err := gh.FileAt(ctx, f, base)
		if err != nil {
			v.Paths, v.Unverified = []string{f}, true
			v.Why = fmt.Sprintf("its forbidden-path list %s could not be read at %s, so nothing can be checked: %v", f, base, err)
			return v
		}
		list, err := officers.ParseForbiddenList(string(raw))
		if err != nil {
			v.Paths, v.Unverified = []string{f}, true
			v.Why = fmt.Sprintf("its forbidden-path list %s at %s does not parse (%v), so nothing can be checked", f, base, err)
			return v
		}
		forbidden = append(append(forbidden, list...), f)
	}
	var whys []string
	v.Paths, v.Why = ForbiddenFiles(forbidden, cmp.Files)
	if v.Why != "" {
		whys = append(whys, v.Why)
	}
	for _, f := range cmp.Files {
		if f.Status == "removed" && f.Patch == "" && f.Changes == 0 {
			if why := removedNoDiffWhy(ctx, gh, cmp, base, f.Path); why != "" {
				v.Paths = append(v.Paths, f.Path)
				whys = append(whys, f.Path+": "+why)
			}
		}
		for _, entry := range removedKustomizeEntries(f) {
			if why := g.removedEntryWhy(ctx, gh, base, entry); why != "" {
				v.Paths = append(v.Paths, f.Path)
				whys = append(whys, f.Path+": "+why)
			}
		}
	}
	v.Paths, v.Why = sortedUnique(v.Paths), strings.Join(whys, "; ")
	return v
}

// removedNoDiffWhy is why removing p, a file GitHub shows no diff for
// (removed, changes 0: a binary or empty file — or a manifest an attribute
// made look like one), is refused, "" when it is not: its content is read at
// the comparison's merge base (base when GitHub names none) and refused when
// it cannot be read, when a line declares a protected kind (protectedKind), or
// when it holds a credential (redact.HasSecretValueIn). FAIL CLOSED: what
// cannot be read is not admitted.
func removedNoDiffWhy(ctx context.Context, gh GitHub, cmp Compare, base, p string) string {
	at := cmp.MergeBase
	if at == "" {
		at = base
	}
	raw, err := gh.FileAt(ctx, p, at)
	if err != nil {
		return fmt.Sprintf("it removes a file GitHub shows no diff for, which could not be read at %s to check what it declares: %v", shortSHA(at), err)
	}
	for _, l := range strings.Split(string(raw), "\n") {
		if m := protectedKind(l); m != "" {
			return fmt.Sprintf("it removes a file GitHub shows no diff for, which declares %q (a volume, database or canary must never be deleted by an officer)", m)
		}
	}
	if redact.HasSecretValueIn(p, string(raw)) {
		return "it removes a file GitHub shows no diff for, which holds a credential-shaped value"
	}
	return ""
}

// removedEntryWhy is why dropping kustomization entry (a repository path) is
// refused, "" when it is not: an entry that is not a single YAML file (a
// directory, a remote base) cannot be read whole, and is refused; a file that
// cannot be read at base is refused; a file that declares a protected kind
// (protectedKindRE) is refused.
func (g *PushGate) removedEntryWhy(ctx context.Context, gh GitHub, base, entry string) string {
	if !strings.HasSuffix(entry, ".yaml") && !strings.HasSuffix(entry, ".yml") {
		return fmt.Sprintf("it drops the kustomization entry %s, which is not a single YAML file muster can read whole, "+
			"so what it stops applying cannot be checked", entry)
	}
	raw, err := gh.FileAt(ctx, entry, base)
	if err != nil {
		return fmt.Sprintf("it drops the kustomization entry %s, which could not be read at %s to check what it declares: %v", entry, base, err)
	}
	for _, l := range strings.Split(string(raw), "\n") {
		if m := protectedKind(l); m != "" {
			return fmt.Sprintf("it drops the kustomization entry %s, which declares %q (a volume, database or canary must never be deleted by an officer)", entry, m)
		}
	}
	return ""
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

// recheckTimeout bounds afterPush and afterCreate: they run in the handler
// once the upstream answered (on a context detached from the client's).
const recheckTimeout = time.Minute

// afterPush is a draft officer's push re-check (see the comment at the top),
// called by the connector proxy with what it saw of GitHub's answer to the
// push of cmds to sr — on every path out of the proxy, so it decides itself
// whether anything could have changed:
//
//   - not forwarded: GitHub never saw the push; nothing to read;
//   - an HTTP status other than 200: GitHub refused the request; nothing
//     changed;
//   - otherwise GitHub's report-status is the authority, per ref: "ng" was
//     not applied; "ok" was, and is checked at the pushed commit; a ref the
//     report does not name (no report: the client went away, or asked for
//     none) is checked anyway — at the pushed commit, and, when GitHub cannot
//     compare that commit (the push never landed), at the head the pull
//     request shows.
func (g *PushGate) afterPush(ctx context.Context, sr officers.ShipRepo, cmds []pushCommand, res connectors.ForwardResult) {
	if !res.Forwarded || len(cmds) == 0 || (res.Status != 0 && res.Status != http.StatusOK) {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recheckTimeout)
	defer cancel()
	gh := g.ghFor(sr.Repo)
	if gh == nil {
		g.logf("officers: %s: a push to %s cannot be re-checked: no GitHub reader", g.OfficerID, sr.Repo)
		return
	}
	report := parseReportStatus(res.Body)
	for _, c := range cmds {
		branch := strings.TrimPrefix(c.ref, "refs/heads/")
		applied, known := report[c.ref]
		if known && !applied {
			g.logf("officers: %s: GitHub did not apply the push of %s to %s in %s (its report says ng): not re-checked", g.OfficerID,
				shortSHA(c.newOID), branch, sr.Repo)
			continue
		}
		prs, err := g.openPRsFrom(ctx, sr, gh, branch)
		if err != nil {
			// 🔴 NOTHING TO CLOSE IS KNOWN, so this is told, not acted on: HQ
			// hears that a push went unchecked (the sweep reads it next).
			g.announce(ctx, officers.Notice{Channel: g.Ship.NoticeChannel, Summary: fmt.Sprintf(
				"officer %s: muster could not look up the pull requests from %s in %s after a push, so the push was NOT re-checked "+
					"(the next sweep reads it): %v", g.OfficerID, branch, sr.Repo, err)})
			continue
		}
		for _, pr := range prs {
			sha := c.newOID
			v := g.checkComparison(ctx, sr, gh, baseOf(sr, pr), sha)
			if v.Err != nil && !known {
				// The push may never have landed: what the pull request shows
				// is its head.
				if cur, err := gh.PR(ctx, pr.Number); err == nil && cur.HeadSHA != "" && cur.HeadSHA != c.newOID {
					sha, v = cur.HeadSHA, g.checkComparison(ctx, sr, gh, baseOf(sr, cur), cur.HeadSHA)
				}
			}
			g.answer(ctx, sr, gh, pr, sha, v, "a push of "+shortSHA(sha))
		}
	}
}

// afterCreate is a draft officer's create re-check, called by the connector
// proxy with what it saw of GitHub's answer to POST pulls from branch in sr.
// It releases the create lock draftCreate took. A 201 names the pull request
// and the head GitHub created it on, which is checked; any other answer
// GitHub could have created one under (no status, a 2xx it could not read) is
// looked up by the branch and each open one checked at its head. A refusal
// (another status) created nothing.
func (g *PushGate) afterCreate(ctx context.Context, sr officers.ShipRepo, branch string, res connectors.ForwardResult) {
	defer g.releaseCreate()
	if !res.Forwarded || (res.Status != 0 && res.Status/100 != 2) {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recheckTimeout)
	defer cancel()
	gh := g.ghFor(sr.Repo)
	if gh == nil {
		g.logf("officers: %s: a new pull request in %s cannot be re-checked: no GitHub reader", g.OfficerID, sr.Repo)
		return
	}
	var prs []PR
	var r restPR
	if res.Status == http.StatusCreated && res.Complete && json.Unmarshal(res.Body, &r) == nil && r.Number > 0 && r.Head.SHA != "" {
		prs = []PR{r.pr()}
	} else {
		var err error
		if prs, err = g.openPRsFrom(ctx, sr, gh, branch); err != nil {
			g.announce(ctx, officers.Notice{Channel: g.Ship.NoticeChannel, Summary: fmt.Sprintf(
				"officer %s: muster could not look up the pull requests from %s in %s after a create, so it was NOT re-checked "+
					"(the next sweep reads it): %v", g.OfficerID, branch, sr.Repo, err)})
			return
		}
	}
	for _, pr := range prs {
		if g.answer(ctx, sr, gh, pr, pr.HeadSHA, g.checkComparison(ctx, sr, gh, baseOf(sr, pr), pr.HeadSHA), "its creation at "+shortSHA(pr.HeadSHA)) {
			// 🔴 ONLY A CLEAN CREATE IS ANNOUNCED: one the re-check closed is
			// no pull request HQ should loop on.
			g.prOpened(ctx, sr, pr)
		}
	}
}

// DefaultPROpenedBackoff is the waits between pr-opened attempts after an
// outage (HQ unreachable, a 5xx): three attempts in all. A 4xx is HQ's
// decision and is not retried.
var DefaultPROpenedBackoff = []time.Duration{2 * time.Second, 10 * time.Second}

// prOpenedTimeout bounds one announcement, retries included.
const prOpenedTimeout = 2 * time.Minute

// prOpened tells HQ (PROpened) that pull request pr of sr was opened clean,
// on its OWN context — detached from the create request, so neither the
// client going away nor the create lock waits on it (run through Async). It
// never fails the create.
//
// The announcement is recorded first as 'sending' — this process's claim
// (Loops.Announced) — and settled from HQ's answer: a 200 naming a loop item
// in_loop (the author's writes then need a held fix item, fixpush.go), one
// naming none no_taker; 409 officer_shadow — expected for an officer in
// shadow, a note — closed; another permanent 4xx failed. A TRANSIENT failure
// (HQ unreachable, a 5xx, 429, 401) is retried with backoff
// (PROpenedBackoff), and if it lasts the record goes back to pending, which
// the sweep claims and re-sends (sweepAnnounce). A failure is posted to HQ as
// a notice.
func (g *PushGate) prOpened(ctx context.Context, sr officers.ShipRepo, pr PR) {
	if g.PROpened == nil {
		return
	}
	run := func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), prOpenedTimeout)
		defer cancel()
		if g.Loops != nil {
			if err := g.Loops.Announced(ctx, sr.Repo, pr.Number, pr.HeadSHA); err != nil {
				g.logf("officers: %s: the announcement of #%d in %s was not recorded: %v", g.OfficerID, pr.Number, sr.Repo, err)
			}
		}
		backoff := g.PROpenedBackoff
		if backoff == nil {
			backoff = DefaultPROpenedBackoff
		}
		if err := g.announceOpened(ctx, sr, pr, backoff); err != nil {
			g.announce(ctx, officers.Notice{PR: pr.Number, HeadSHA: pr.HeadSHA, Channel: g.Ship.NoticeChannel, Summary: fmt.Sprintf(
				"officer %s: muster could not tell HQ that #%d in %s was opened at %s (pr-opened: %v); the pull request stands",
				g.OfficerID, pr.Number, sr.Repo, shortSHA(pr.HeadSHA), err)})
		}
	}
	if g.Async != nil {
		g.Async(run)
		return
	}
	go run()
}

// announceOpened is ONE announcement of pr, whose record this process holds
// claimed ('sending'): its attempts (one, then one per wait of backoff while
// the failure is transient), then the record settled from HQ's answer (see
// prOpened). It returns the failure when HQ did not answer 200 or
// officer_shadow.
func (g *PushGate) announceOpened(ctx context.Context, sr officers.ShipRepo, pr PR, backoff []time.Duration) error {
	p := officers.PROpened{Repo: sr.Repo, PR: pr.Number, HeadSHA: pr.HeadSHA, HeadRef: pr.HeadRef, Base: baseOf(sr, pr), Author: pr.Author}
	var (
		ans officers.PROpenedAnswer
		err error
	)
	for i := 0; ; i++ {
		ans, err = g.PROpened(ctx, p)
		if err == nil || !transientHQ(err) || i >= len(backoff) {
			break
		}
		select {
		case <-ctx.Done():
			err = fmt.Errorf("%w (then: %v)", err, ctx.Err())
		case <-time.After(backoff[i]):
			continue
		}
		break
	}
	settle := func(state string) { g.settleLoop(ctx, sr.Repo, pr.Number, state) }
	var hqErr *officers.HQError
	switch {
	case err == nil:
		state := officers.PRLoopNoTaker
		if len(ans.Items) > 0 {
			state = officers.PRLoopInLoop
		}
		settle(state)
		g.logf("officers: %s: told HQ #%d in %s was opened at %s: %d loop item(s), %d officer(s) skipped",
			g.OfficerID, pr.Number, sr.Repo, shortSHA(pr.HeadSHA), len(ans.Items), len(ans.Skipped))
		return nil
	case errors.As(err, &hqErr) && hqErr.Status == http.StatusConflict && hqErr.Code() == "officer_shadow":
		settle(officers.PRLoopClosed)
		g.logf("officers: %s: note: HQ runs this officer in shadow, so #%d in %s starts no review loop (pr-opened answered 409 officer_shadow)",
			g.OfficerID, pr.Number, sr.Repo)
		return nil
	case transientHQ(err):
		// Back to pending: the sweep claims and re-sends it, within its bound.
		settle(officers.PRLoopPending)
		g.logf("officers: %s: HQ was not told #%d in %s was opened (pr-opened, transient): %v; the sweep announces it again",
			g.OfficerID, pr.Number, sr.Repo, err)
		return err
	default:
		// 🔴 A PERMANENT 4xx IS HQ'S DECISION: failed (logged once, here),
		// never re-sent.
		settle(officers.PRLoopFailed)
		g.logf("officers: %s: HQ refused pr-opened for #%d in %s (%v): not announced again", g.OfficerID, pr.Number, sr.Repo, err)
		return err
	}
}

// DefaultSettleBackoff is the waits between writes of one announcement's
// outcome (settleLoop): four attempts in all.
var DefaultSettleBackoff = []time.Duration{200 * time.Millisecond, time.Second, 5 * time.Second}

// settleLoop records repo#n's outcome in Loops, retried with backoff
// (SettleBackoff). One that still cannot be written leaves the record as it
// was — 'sending', claimed by this process — so it is NEVER re-sent, by this
// process, after a restart or by another replica: an outcome HQ gave is never
// followed by a second announcement (which, after a 200, would restart a
// finished loop). The cost, logged: a transient failure whose 'pending' could
// not be written is not announced again either.
func (g *PushGate) settleLoop(ctx context.Context, repo string, n int, state string) {
	if g.Loops == nil {
		return
	}
	backoff := g.SettleBackoff
	if backoff == nil {
		backoff = DefaultSettleBackoff
	}
	var err error
	for i := 0; ; i++ {
		if err = g.Loops.Settle(ctx, repo, n, state); err == nil {
			return
		}
		if i >= len(backoff) {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(backoff[i]):
			continue
		}
		break
	}
	g.logf("officers: %s: the announcement of #%d in %s could not be recorded %s (%v); it stays claimed and is never announced again",
		g.OfficerID, n, repo, state, err)
}

// MaxReannounceAge bounds the sweep's re-sends: a pending announcement first
// made longer ago than this is failed, not sent again.
const MaxReannounceAge = 24 * time.Hour

// transientHQ reports whether err is a failure worth another attempt later:
// HQ unreachable or a 5xx (ErrHQUnavailable, RetryableError, an HQError of
// 5xx), 429 (rate limited) or 401 (a credential HQ may accept again). Every
// other 4xx is a decision.
func transientHQ(err error) bool {
	var re *officers.RetryableError
	var hqErr *officers.HQError
	if errors.Is(err, officers.ErrHQUnavailable) || errors.As(err, &re) {
		return true
	}
	return errors.As(err, &hqErr) && (hqErr.Status >= 500 || hqErr.Status == http.StatusTooManyRequests || hqErr.Status == http.StatusUnauthorized)
}

// sweepAnnounce re-announces, once each (no retries, no notice: the next
// sweep tries again), every announcement of this officer that is pending —
// an outage recorded — and ONLY those: HQ's pr-opened restarts a loop whose
// item has finished, so one HQ answered is never sent again. The sweep CLAIMS
// them (Loops.Claim: pending -> sending in one conditional write), so two
// replicas sweeping at once never both send one. One first announced longer
// ago than MaxReannounceAge is failed by the claim (logged once); one whose
// pull request closed is closed; one whose pull request cannot be read goes
// back to pending.
//
// It also moves every in_loop record whose pull request is closed or merged
// to closed, so the officer's in-loop set (InLoopPRs) does not keep a loop
// that ended.
func (g *PushGate) sweepAnnounce(ctx context.Context) error {
	if g.Loops == nil {
		return nil
	}
	var errs []error
	now := time.Now()
	if g.Now != nil {
		now = g.Now()
	}
	if inLoop, err := g.Loops.InLoopPRs(ctx); err != nil {
		errs = append(errs, fmt.Errorf("the in-loop pull requests could not be read: %v", err))
	} else {
		for _, p := range inLoop {
			sr, ok := g.Ship.RepoFor(g.Repo, p.Repo)
			gh := g.ghFor(sr.Repo)
			if !ok || gh == nil {
				continue
			}
			if pr, err := gh.PR(ctx, p.PR); err == nil && (pr.State != "open" || pr.Merged) {
				g.settleLoop(ctx, sr.Repo, p.PR, officers.PRLoopClosed)
			}
		}
	}
	if g.PROpened == nil {
		return errors.Join(errs...)
	}
	claimed, expired, err := g.Loops.Claim(ctx, now.Add(-MaxReannounceAge))
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("the pending pr-opened announcements could not be claimed: %v", err))...)
	}
	for _, p := range expired {
		g.logf("officers: %s: #%d in %s was announced %s ago and HQ never answered; not announced again", g.OfficerID, p.PR, p.Repo,
			now.Sub(p.AnnouncedAt).Round(time.Minute))
	}
	for _, p := range claimed {
		sr, ok := g.Ship.RepoFor(g.Repo, p.Repo)
		gh := g.ghFor(sr.Repo)
		if !ok || gh == nil {
			g.settleLoop(ctx, p.Repo, p.PR, officers.PRLoopPending)
			continue
		}
		pr, err := gh.PR(ctx, p.PR)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: reading pull request #%d to announce it again: %v", sr.Repo, p.PR, err))
			g.settleLoop(ctx, sr.Repo, p.PR, officers.PRLoopPending)
			continue
		}
		if pr.State != "open" || pr.Merged {
			g.settleLoop(ctx, sr.Repo, p.PR, officers.PRLoopClosed)
			continue
		}
		_ = g.announceOpened(ctx, sr, pr, nil)
	}
	return errors.Join(errs...)
}

// officerPRs is OfficerPRs(login, state) of sr, CROSS-CHECKED by the
// officer's branches. 🔴 OfficerPRs keeps the pulls list's entries whose
// author is login, and an author filter that stopped matching the officer's
// App (a login spelled otherwise than GitHub reports it) answers an EMPTY
// list, not an error, which would read as "no pull requests" and void the
// rate limits and the sweep. Every branch under the officer's
// prefix in sr is looked up by its branch (PRsForBranch, in state); a pull
// request of login from this repository that the filter did not list is
// added to the answer AND makes it an error (fail closed: rateLimits refuses;
// Sweep reports it and checks what was found). A branch already deleted
// (a merged or closed pull request's) cannot be cross-checked, so this is a
// positive control while the officer has a branch, not a proof.
func (g *PushGate) officerPRs(ctx context.Context, sr officers.ShipRepo, gh GitHub, login, state string) ([]PR, error) {
	prs, err := gh.OfficerPRs(ctx, login, state)
	if err != nil {
		return nil, err
	}
	listed := map[int]bool{}
	for _, p := range prs {
		listed[p.Number] = true
	}
	prefix := sr.BranchPrefix + officers.Slug(g.OfficerID) + "/"
	branches, err := gh.Branches(ctx, prefix)
	if err != nil {
		return prs, fmt.Errorf("could not list the officer's branches to cross-check the author filter: %v", err)
	}
	var missed []string
	for _, b := range branches {
		if !strings.HasPrefix(b.Name, prefix) {
			continue
		}
		found, err := gh.PRsForBranch(ctx, b.Name, state)
		if err != nil {
			return prs, fmt.Errorf("could not look up the pull requests from %s to cross-check the author filter: %v", b.Name, err)
		}
		for _, p := range found {
			if listed[p.Number] || !strings.EqualFold(p.Author, login) || (p.HeadRepo != "" && !strings.EqualFold(p.HeadRepo, sr.Repo)) {
				continue
			}
			listed[p.Number] = true
			prs = append(prs, p)
			missed = append(missed, "#"+strconv.Itoa(p.Number))
		}
	}
	if len(missed) > 0 {
		return prs, fmt.Errorf("the author filter (author %s) did not list the officer's pull request(s) %s, found by their branches, "+
			"so it cannot be trusted to list them all", login, strings.Join(missed, ", "))
	}
	return prs, nil
}

// openPRsFrom is every open pull request from branch in sr (not a fork's).
func (g *PushGate) openPRsFrom(ctx context.Context, sr officers.ShipRepo, gh GitHub, branch string) ([]PR, error) {
	prs, err := gh.PRsForBranch(ctx, branch, "open")
	if err != nil {
		return nil, err
	}
	var out []PR
	for _, pr := range prs {
		if pr.State == "open" && pr.HeadRef == branch && (pr.HeadRepo == "" || strings.EqualFold(pr.HeadRepo, sr.Repo)) {
			out = append(out, pr)
		}
	}
	return out, nil
}

func baseOf(sr officers.ShipRepo, pr PR) string {
	if pr.BaseRef != "" {
		return pr.BaseRef
	}
	return sr.Base
}

// answer is a re-check's answer to verdict v of open pull request pr at sha:
// a clean one is recorded (the sweep does not read sha again); a hit, or an
// unreadable comparison, closes the pull request (FAIL CLOSED).
//
// It reports whether the verdict was clean (the pull request stays open).
func (g *PushGate) answer(ctx context.Context, sr officers.ShipRepo, gh GitHub, pr PR, sha string, v comparisonVerdict, after string) bool {
	key := sr.Repo + "#" + strconv.Itoa(pr.Number)
	switch {
	case v.Err != nil:
		g.closeOnHit(ctx, sr, gh, pr, sha, nil, v.Err.Error()+", so the change could not be checked", false, after)
	case len(v.Paths) > 0:
		g.closeOnHit(ctx, sr, gh, pr, sha, v.Paths, v.Why, !v.Unverified, after)
	default:
		g.mu.Lock()
		if g.checked == nil {
			g.checked = map[string]string{}
		}
		g.checked[key] = sha
		delete(g.misses, key)
		g.mu.Unlock()
		return true
	}
	g.mu.Lock()
	delete(g.checked, key)
	delete(g.misses, key)
	g.mu.Unlock()
	return false
}

// Sweep is the draft sweep: every open pull request the officer's App opened
// in each ship repository (OfficerPRs, every page), re-read for its current
// head and, unless that head was already checked clean, checked through
// checkComparison and answered like a push. 🔴 An unreadable comparison (or
// forbidden-path list) is retried on the next sweep, and closes the pull
// request once it has failed maxSweepMisses sweeps in a row: a GitHub blip
// closes nothing, a comparison that stays unreadable is not left open.
func (g *PushGate) Sweep(ctx context.Context) error {
	var errs []error
	for _, sr := range g.Ship.RepoList(g.Repo) {
		gh := g.ghFor(sr.Repo)
		if gh == nil {
			errs = append(errs, fmt.Errorf("no GitHub reader for %s", sr.Repo))
			continue
		}
		slug, err := gh.AppSlug(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: the officer App's identity: %v", sr.Repo, err))
			continue
		}
		prs, err := g.officerPRs(ctx, sr, gh, BotLogin(slug), "open")
		if err != nil {
			// The pull requests the branches found are still checked.
			errs = append(errs, fmt.Errorf("%s: listing this officer's open pull requests: %v", sr.Repo, err))
			if len(prs) == 0 {
				continue
			}
		}
		seen := map[string]bool{}
		for _, p := range prs {
			key := sr.Repo + "#" + strconv.Itoa(p.Number)
			seen[key] = true
			pr, err := gh.PR(ctx, p.Number)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: reading pull request #%d: %v", sr.Repo, p.Number, err))
				continue
			}
			if pr.State != "open" || pr.HeadSHA == "" || (pr.HeadRepo != "" && !strings.EqualFold(pr.HeadRepo, sr.Repo)) {
				continue
			}
			g.mu.Lock()
			done := g.checked[key] == pr.HeadSHA
			g.mu.Unlock()
			if done {
				continue
			}
			v := g.checkComparison(ctx, sr, gh, baseOf(sr, pr), pr.HeadSHA)
			if v.Err != nil || v.Unverified {
				g.mu.Lock()
				if g.misses == nil {
					g.misses = map[string]int{}
				}
				g.misses[key]++
				n := g.misses[key]
				g.mu.Unlock()
				if n < maxSweepMisses {
					g.logf("officers: %s: the sweep could not check pull request #%d in %s (%d of %d): %v %s", g.OfficerID, pr.Number, sr.Repo,
						n, maxSweepMisses, v.Err, v.Why)
					continue
				}
			}
			g.answer(ctx, sr, gh, pr, pr.HeadSHA, v, "the sweep's read of "+shortSHA(pr.HeadSHA))
		}
		g.mu.Lock()
		for key := range g.checked {
			if strings.HasPrefix(key, sr.Repo+"#") && !seen[key] {
				delete(g.checked, key)
			}
		}
		for key := range g.misses {
			if strings.HasPrefix(key, sr.Repo+"#") && !seen[key] {
				delete(g.misses, key)
			}
		}
		g.mu.Unlock()
	}
	if err := g.sweepAnnounce(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// SweepInterval is how often the draft sweep (Sweep) re-checks every open
// pull request of a draft officer.
const SweepInterval = 5 * time.Minute

// maxSweepMisses is how many sweeps in a row may fail to read a pull
// request's comparison before the sweep closes it (fail closed).
const maxSweepMisses = 3

// closeOnHit closes pull request pr after a change put a refused (or an
// uncheckable) diff into it, deletes its branch when deleteBranch (a content
// hit: the branch holds the refused change) and the branch is the officer's
// own shape in sr, and tells HQ: paths, never content.
//
// 🔴 muster deletes the branch with the officer App's token, server-side; the
// OFFICER still cannot delete one (the push gate refuses its deletions).
func (g *PushGate) closeOnHit(ctx context.Context, sr officers.ShipRepo, gh GitHub, pr PR, sha string, paths []string, why string,
	deleteBranch bool, after string) {
	what := fmt.Sprintf("closed pull request #%d (%s) in %s after %s", pr.Number, pr.HeadRef, sr.Repo, after)
	closed := true
	if err := gh.ClosePR(ctx, pr.Number); err != nil {
		closed = false
		what = fmt.Sprintf("could NOT close pull request #%d (%s) in %s after %s (%v); a human must", pr.Number, pr.HeadRef, sr.Repo, after, err)
	}
	if _, own := officers.BranchItem(sr.BranchPrefix, g.OfficerID, pr.HeadRef); closed && deleteBranch && own &&
		(pr.HeadRepo == "" || strings.EqualFold(pr.HeadRepo, sr.Repo)) {
		if err := gh.DeleteBranch(ctx, pr.HeadRef); err != nil {
			what += fmt.Sprintf(", and could NOT delete branch %s, which holds the refused change (%v); a human must", pr.HeadRef, err)
		} else {
			what += fmt.Sprintf(", and deleted branch %s, which held the refused change", pr.HeadRef)
		}
	}
	summary := fmt.Sprintf("officer %s: muster %s: %s", g.OfficerID, what, why)
	ps := paths
	if len(ps) > officers.MaxNoticePaths {
		summary += fmt.Sprintf(" (+%d more paths)", len(ps)-officers.MaxNoticePaths)
		ps = ps[:officers.MaxNoticePaths]
	}
	g.announce(ctx, officers.Notice{PR: pr.Number, HeadSHA: sha, Paths: ps, Summary: summary, Channel: g.Ship.NoticeChannel})
}

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
	if err := g.Announce(ctx, n); err != nil {
		g.logf("officers: %s: the notice did not reach HQ: %v", g.OfficerID, err)
	}
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
