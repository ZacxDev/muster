package officership

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/redact"
)

// ---------------------------------------------------------------------------
// THE SHEPHERD GATE: what a `shepherd` officer may do, on whose pull requests
// (officers.StageShepherd; docs/officers.md "The shepherd stage").
//
// 🔴 ONE RULE, Shepherd.Admit, decides whether a pull request is one the
// shepherd may act on NOW, for THIS instance: open; into the configured base
// of a ship repository; from a <branch_prefix>… branch of that repository;
// authored by a listed login that is not the shepherd's own App (and, for an
// author that names an officer, IS that officer's App's bot, read at call
// time); and the subject of an HQ item the instance holds under a live lease.
// Every write below asks it, and it reads everything fresh.
//
// The writes it admits (PushGate, shepherd branch):
//
//   - git push to that pull request's branch: ONE ref, an UPDATE (never a
//     create or a delete) from the pull request's CURRENT head. 🔴 A BAD PUSH
//     NEVER LANDS. The pack is forwarded unread (it may be thin), so muster
//     REWRITES the push to a fresh scratch ref outside refs/heads
//     (ShepherdScratchPrefix: no branch, no pull request, no `on: push`
//     branch filter matches it), and only once GitHub has it — before the
//     client is answered (connectors.WithResponseRewrite) — verifies it
//     (landShepherdPush): the pull request is re-admitted and still at old;
//     old...new is a fast-forward within the size caps; base...new passes the
//     AUTHORING officer's wall (officers.ShepherdRules) and the draft rules
//     (removed volume/database/canary manifests, secret shapes). Only then is
//     the pull request's branch moved, by a compare-and-swap that is also
//     fast-forward-only (GitHub.UpdateBranch: GraphQL updateRefs, beforeOid
//     old, force false). The client is told "ok" only when that landed, else
//     "ng" with the reason, and HQ hears of a refusal. muster makes NO forced
//     ref write anywhere. One push per (repository, branch) at a time
//     (lockBranch); the scratch ref is deleted once the request ends.
//   - POST issues/<n>/comments (no credential-shaped value, no line that
//     reads as a command: commandLine);
//   - PUT pulls/<n>/update-branch with expected_head_sha = the current head
//     (GitHub merges the base into the head; never a rebase).
//
// And two tools: ShepherdGates.MarkReady (officer_shepherd_mark_ready), only
// once the auditor's report on the held item says the ladder stopped with no
// 🔴 at the head (AuditVerdictClear), CI is green at the head and the head
// passes the wall; and ShepherdGates.MergeAsk (officer_shepherd_merge_ask),
// HQ's merge ask under the held item.
//
// 🔴 EVERY WRITE HONOURS SHADOW (shepherd_loop.go): when HQ runs the held
// item (or, the item saying nothing, the officer) in shadow, the push,
// comment, update-branch, mark-ready and merge ask are refused before
// anything reaches GitHub or HQ, and what would have been done is recorded
// for the item's completion.
//
// Everything else is refused: opening, editing (title, body, state — so
// closing and reopening), merging, the merge and refs APIs, GraphQL, every
// DELETE, a branch create or delete, a push to anything but a shepherded
// pull request's branch.
// ---------------------------------------------------------------------------

// GateItem is the shepherd's item gate: the instance holds the HQ item for
// the pull request. GateWall is mark-ready's forbidden-path re-check at the
// head; GateAudit its audit-ladder check.
const (
	GateItem  = "item"
	GateWall  = "wall"
	GateAudit = "audit"
)

// ShepherdScratchPrefix is where a shepherd's push lands first: a ref outside
// refs/heads (so no branch, no pull request and no branch-filtered workflow
// sees it), named per push, deleted when the request ends. GitHub accepts a
// push to such a ref (measured with a user token; see docs/officers.md).
const ShepherdScratchPrefix = "refs/muster/shepherd/"

// The size caps of one shepherd push (old...new): beyond them it is refused
// unverified-as-too-large, never landed.
const (
	maxShepherdPushCommits = 50
	maxShepherdPushFiles   = 100
)

// Shepherd is a shepherd-stage officer's rules, shared by its push gate and
// its gated tool.
type Shepherd struct {
	OfficerID string
	// Repo is the officer's `repo` (Ship.RepoList's primary).
	Repo string
	Ship *officers.Ship
	// Rules are officers.ShepherdRules of the officer: who, where, and the
	// wall a push must pass.
	Rules []officers.ShepherdRule
	// AuthorLogin is the bot login of authoring officer id's OWN App, read at
	// call time. nil, or an error: an author that names an officer is refused.
	AuthorLogin func(ctx context.Context, officerID string) (string, error)
	// ItemHeld returns the item HQ shows instance holding, under a live
	// lease, of this officer whose subject is officers.ShepherdSubject(repo,
	// n); an error when there is none. nil func: everything is refused (fail
	// closed).
	ItemHeld func(ctx context.Context, instance, repo string, n int) (officers.Item, error)
	// OfficerShadow reads HQ's officer row `shadow`, which decides when an
	// item carries no `shadow` of its own. nil, or an error: shadow (fail
	// closed).
	OfficerShadow func(ctx context.Context) (bool, error)
	// Shadowed records a write refused in shadow ("would have …") against
	// the item it was made under; nil records nothing.
	Shadowed func(item officers.Item, line string)
}

// rule is the rule for login's pull requests in repo.
func (s *Shepherd) rule(repo, login string) (officers.ShepherdRule, bool) {
	for _, r := range s.Rules {
		if strings.EqualFold(r.Repo, repo) && strings.EqualFold(r.Login, login) {
			return r, true
		}
	}
	return officers.ShepherdRule{}, false
}

// Admit is THE shepherd rule (see the top of this file): it refuses (a
// *Refusal) unless pull request pr of ship repository sr is one instance may
// act on now, reporting each gate it passes, and returns the pull request's
// rule and the HQ item its item gate found (the one the instance holds for
// it: the audit verdict and shadow are read off it). gh is sr's GitHub, as
// the shepherd's App. Every read failure is a refusal: fail closed.
func (s *Shepherd) Admit(ctx context.Context, sr officers.ShipRepo, gh GitHub, pr PR, instance string, pass func(string)) (officers.ShepherdRule, officers.Item, error) {
	n := pr.Number
	var none officers.Item
	if s == nil || !s.Ship.IsShepherd() {
		return officers.ShepherdRule{}, none, refuse(GateStage, "this officer is not a shepherd")
	}
	if pr.State != "open" || pr.Merged {
		return officers.ShepherdRule{}, none, refuse(GatePR, "pull request #%d is %s (merged %v); a shepherd acts only on an open one", n, pr.State, pr.Merged)
	}
	pass(GatePR)
	if pr.BaseRef != sr.Base || !connectors.SameRepo(pr.BaseRepo, sr.Repo) {
		return officers.ShepherdRule{}, none, refuse(GateBase, "pull request #%d targets %s:%s; a shepherd acts only on pull requests into %s:%s", n, pr.BaseRepo, pr.BaseRef, sr.Repo, sr.Base)
	}
	pass(GateBase)
	if !strings.HasPrefix(pr.HeadRef, sr.BranchPrefix) || !officers.ValidRefSuffix(pr.HeadRef) || !connectors.SameRepo(pr.HeadRepo, sr.Repo) {
		return officers.ShepherdRule{}, none, refuse(GateHeadBranch, "pull request #%d comes from %s:%s; a shepherd acts only on a %s* branch of %s", n, pr.HeadRepo, pr.HeadRef, sr.BranchPrefix, sr.Repo)
	}
	pass(GateHeadBranch)
	rule, ok := s.rule(sr.Repo, pr.Author)
	if !ok {
		return rule, none, refuse(GateAuthor, "pull request #%d is authored by %s, who is not one of this shepherd's authors in %s", n, pr.Author, sr.Repo)
	}
	if strings.HasSuffix(strings.ToLower(rule.Login), "[bot]") && pr.AuthorType != "Bot" {
		return rule, none, refuse(GateAuthor, "pull request #%d is authored by %s typed %s; the listed login is an App's bot", n, pr.Author, pr.AuthorType)
	}
	slug, err := gh.AppSlug(ctx)
	if err != nil {
		return rule, none, refuse(GateAuthor, "could not read the shepherd App's identity: %v", err)
	}
	if strings.EqualFold(pr.Author, BotLogin(slug)) {
		return rule, none, refuse(GateAuthor, "pull request #%d is authored by the shepherd's own App (%s); a shepherd never acts on its own work", n, pr.Author)
	}
	if rule.AuthorOfficer == "" {
		return rule, none, refuse(GateAuthor, "the listed author %s names no officer: a shepherd acts only on an officer's pull requests", rule.Login)
	}
	{
		if s.AuthorLogin == nil {
			return rule, none, refuse(GateAuthor, "the App of officer %s (the listed author %s) cannot be read here", rule.AuthorOfficer, rule.Login)
		}
		login, err := s.AuthorLogin(ctx, rule.AuthorOfficer)
		if err != nil {
			return rule, none, refuse(GateAuthor, "could not read officer %s's App identity, so %s cannot be confirmed as its bot: %v", rule.AuthorOfficer, pr.Author, err)
		}
		if !strings.EqualFold(login, pr.Author) {
			return rule, none, refuse(GateAuthor, "pull request #%d is authored by %s, but officer %s's App is %s: the listed login is not that officer's", n, pr.Author, rule.AuthorOfficer, login)
		}
	}
	pass(GateAuthor)
	if instance == "" {
		return rule, none, refuse(GateItem, "a request from an unknown instance: its hold on the item for %s cannot be checked", officers.ShepherdSubject(sr.Repo, n))
	}
	if s.ItemHeld == nil {
		return rule, none, refuse(GateItem, "muster cannot check HQ's items for %s", officers.ShepherdSubject(sr.Repo, n))
	}
	item, err := s.ItemHeld(ctx, instance, sr.Repo, n)
	if err != nil {
		return rule, none, refuse(GateItem, "HQ does not show %s holding the item for %s (claim it with %s first): %v", instance, officers.ShepherdSubject(sr.Repo, n), officers.ToolClaim, err)
	}
	pass(GateItem)
	return rule, item, nil
}

// shadowRefused is a shepherd write refused (403) because it runs in shadow.
func shadowRefused(why string) error { return fmt.Errorf("%w: %s", connectors.ErrGateRefused, why) }

// shepherdRefused is a shepherd write refused (403), saying what a shepherd
// never does.
func shepherdRefused(format string, args ...any) error {
	return fmt.Errorf("%w: %s (a shepherd only pushes verified fast-forward commits to, comments on, updates the branch of and marks ready a pull request it holds the item for; "+
		"it never opens, edits, closes or merges one, never creates or deletes a branch and never force-pushes)", connectors.ErrGateRefused, fmt.Sprintf(format, args...))
}

// asShepherdRefusal turns Admit's answer into the push gate's: a refusal is
// a 403 naming the gate.
func asShepherdRefusal(n int, err error) error {
	if r, ok := AsRefusal(err); ok {
		return shepherdRefused("pull request #%d: refused by gate %s: %s", n, r.Gate, r.Reason)
	}
	return err
}

func callerInstance(ctx context.Context) string {
	c, ok := connectors.CallerOf(ctx)
	if !ok {
		return ""
	}
	return c.AgentName
}

func zeroOID(s string) bool { return strings.Trim(s, "0") == "" }

// shepherdPush is an admitted shepherd push: the one ref update the agent
// asked for, the pull request its branch is the head of and that pull
// request's rule, the scratch ref the push was rewritten to, and what the
// client asked to be told (its report-status and side-band capabilities).
type shepherdPush struct {
	// instance is the agent that pushed (the caller Admit read): the one
	// whose item hold is checked again before the push lands.
	instance string
	cmd      pushCommand
	branch   string
	pr       PR
	rule     officers.ShepherdRule
	scratch  string
	report   bool
	sideband bool
}

// branchKey is lockBranch's key.
func branchKey(repo, branch string) string { return strings.ToLower(repo) + " " + branch }

// lockBranch takes the one-push-at-a-time lock of (repo, branch), false when a
// push to it is being verified now. unlockBranch releases it.
func (g *PushGate) lockBranch(repo, branch string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.shepherdBusy == nil {
		g.shepherdBusy = map[string]bool{}
	}
	k := branchKey(repo, branch)
	if g.shepherdBusy[k] {
		return false
	}
	g.shepherdBusy[k] = true
	return true
}

func (g *PushGate) unlockBranch(repo, branch string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.shepherdBusy, branchKey(repo, branch))
}

// admitShepherdPush is the push half of the shepherd gate: exactly one
// command, an update (never a create or a delete) of the branch of exactly one
// open pull request Admit admits, from that pull request's current head, while
// no other push to that branch is being verified. It returns the push and the
// command section to forward instead of pre: the same command aimed at a new
// scratch ref (ShepherdScratchPrefix). The branch lock it takes is released by
// the caller's after-forward hook (or here, on a refusal).
func (g *PushGate) admitShepherdPush(ctx context.Context, sr officers.ShipRepo, cmds []pushCommand, pre []byte) (*shepherdPush, []byte, error) {
	gh := g.ghFor(sr.Repo)
	if gh == nil {
		return nil, nil, fmt.Errorf("no GitHub reader for %s", sr.Repo)
	}
	if len(cmds) != 1 {
		return nil, nil, shepherdRefused("a push of %d refs: a shepherd pushes one pull request's branch at a time", len(cmds))
	}
	c := cmds[0]
	branch := strings.TrimPrefix(c.ref, "refs/heads/")
	switch {
	case zeroOID(c.oldOID):
		return nil, nil, shepherdRefused("creating %s: a shepherd pushes only to an existing pull request's branch", c.ref)
	case zeroOID(c.newOID):
		return nil, nil, shepherdRefused("deleting %s: a shepherd never deletes a branch", c.ref)
	}
	// 🔴 ONE PUSH PER BRANCH AT A TIME (R1, R6): from the head read below to
	// the branch moving, nothing else muster forwards writes this branch.
	if !g.lockBranch(sr.Repo, branch) {
		return nil, nil, conflict("a push to %s in %s is being verified now: wait for it to finish, fetch, and push again", branch, sr.Repo)
	}
	sp, out, err := g.admitShepherdPushLocked(ctx, sr, gh, c, branch, pre)
	if err != nil {
		g.unlockBranch(sr.Repo, branch)
		return nil, nil, err
	}
	return sp, out, nil
}

func (g *PushGate) admitShepherdPushLocked(ctx context.Context, sr officers.ShipRepo, gh GitHub, c pushCommand, branch string, pre []byte) (*shepherdPush, []byte, error) {
	prs, err := g.openPRsFrom(ctx, sr, gh, branch)
	if err != nil {
		return nil, nil, fmt.Errorf("could not look up the pull requests from %s in %s: %v", branch, sr.Repo, err)
	}
	if len(prs) != 1 {
		return nil, nil, shepherdRefused("a push to %s, which is the head of %d open pull requests in %s; a shepherd pushes only to the branch of exactly one", branch, len(prs), sr.Repo)
	}
	pr, err := gh.PR(ctx, prs[0].Number)
	if err != nil {
		return nil, nil, fmt.Errorf("could not read pull request #%d: %v", prs[0].Number, err)
	}
	instance := callerInstance(ctx)
	rule, item, err := g.Shepherd.Admit(ctx, sr, gh, pr, instance, func(string) {})
	if err != nil {
		return nil, nil, asShepherdRefusal(pr.Number, err)
	}
	if pr.HeadSHA != c.oldOID {
		return nil, nil, shepherdRefused("a push to %s from %s, which is not pull request #%d's head %s: only a fast-forward of the current head is pushed — fetch, and build on it",
			branch, shortSHA(c.oldOID), pr.Number, shortSHA(pr.HeadSHA))
	}
	// 🔴 SHADOW BEFORE THE SCRATCH REF: nothing of a shadowed push reaches
	// GitHub (the rewrite below is what would send it).
	if why, on := g.Shepherd.shadowRefusal(ctx, item, fmt.Sprintf("pushed %s..%s to %s", head12(c.oldOID), head12(c.newOID), branch)); on {
		return nil, nil, shadowRefused(why)
	}
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, nil, fmt.Errorf("could not name a scratch ref: %v", err)
	}
	sp := &shepherdPush{instance: instance, cmd: c, branch: branch, pr: pr, rule: rule, scratch: ShepherdScratchPrefix + hex.EncodeToString(nonce[:])}
	out, err := rewriteShepherdPreamble(pre, sp)
	if err != nil {
		return nil, nil, shepherdRefused("%v", err)
	}
	return sp, out, nil
}

// upstreamDropCaps are the client's capabilities muster does not pass on for
// a shepherd push: it reads GitHub's report-status itself, plain (no side
// band), and answers the client in the form the client asked for.
var upstreamDropCaps = map[string]bool{"side-band": true, "side-band-64k": true, "report-status-v2": true}

// rewriteShepherdPreamble is pre (a receive-pack command section, up to and
// including its flush-pkt) with its one command `old new refs/heads/<b>`
// replaced by the create `0… new <scratch>`, the capabilities rewritten for
// the upstream (upstreamDropCaps dropped, report-status added), and
// shallow lines kept. It records on sp what the client asked to be told.
func rewriteShepherdPreamble(pre []byte, sp *shepherdPush) ([]byte, error) {
	var out bytes.Buffer
	b := pre
	seen := false
	for {
		if len(b) < 4 {
			return nil, errors.New("a push command section that ends early")
		}
		n, err := strconv.ParseUint(string(b[:4]), 16, 16)
		if err != nil || (n != 0 && (n < 4 || int(n) > len(b))) {
			return nil, errors.New("a push with a malformed pkt-line")
		}
		if n == 0 {
			out.WriteString("0000")
			break
		}
		line := string(b[4:n])
		b = b[n:]
		if strings.HasPrefix(line, "shallow ") {
			out.WriteString(pktLine(line))
			continue
		}
		if seen {
			return nil, errors.New("a shepherd push carries one command")
		}
		seen = true
		cmd, caps, _ := strings.Cut(strings.TrimSuffix(line, "\n"), "\x00")
		if f := strings.Split(cmd, " "); len(f) != 3 || f[0] != sp.cmd.oldOID || f[1] != sp.cmd.newOID || f[2] != sp.cmd.ref {
			return nil, errors.New("the push command changed while it was read")
		}
		var up []string
		hasReport := false
		for _, cp := range strings.Fields(caps) {
			switch cp {
			case "report-status", "report-status-v2":
				sp.report = true
			case "side-band", "side-band-64k":
				sp.sideband = true
			}
			if cp == "report-status" {
				hasReport = true
			}
			if !upstreamDropCaps[cp] {
				up = append(up, cp)
			}
		}
		if !hasReport {
			up = append(up, "report-status")
		}
		out.WriteString(pktLine(strings.Repeat("0", len(sp.cmd.newOID)) + " " + sp.cmd.newOID + " " + sp.scratch + "\x00" + strings.Join(up, " ") + "\n"))
	}
	if !seen {
		return nil, errors.New("a shepherd push with no command")
	}
	return out.Bytes(), nil
}

// pktLine is s as one pkt-line.
func pktLine(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

// landShepherdPush is the shepherd push's verification and landing, run by
// the connector proxy on GitHub's answer BEFORE the client sees any of it
// (connectors.WithResponseRewrite). GitHub has stored the push at sp.scratch
// only; nothing of the pull request moved. The push lands — the pull
// request's branch is moved from old to new by a compare-and-swap,
// fast-forward-only update — only when verifyShepherdPush finds nothing.
// Either way the client is answered for ITS ref: "ok" when the branch moved,
// "ng <why>" when it did not, and HQ is told of a refusal.
func (g *PushGate) landShepherdPush(ctx context.Context, sr officers.ShipRepo, sp *shepherdPush, resp *http.Response) error {
	if resp.StatusCode != http.StatusOK {
		return nil // GitHub's own error, relayed as it is: nothing landed
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, connectors.MaxAfterForwardBody+1))
	resp.Body.Close()
	unpack, why := "unpack ok", ""
	switch {
	case err != nil:
		why = fmt.Sprintf("GitHub's answer could not be read (%v); nothing landed", err)
	case len(raw) > connectors.MaxAfterForwardBody:
		why = "GitHub's answer was larger than muster reads; nothing landed"
	default:
		unpack, why = g.verifyAndLand(ctx, sr, sp, raw)
	}
	body := shepherdAnswer(sp, unpack, why)
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Del("Content-Length")
	return nil
}

// verifyAndLand reads GitHub's report of the scratch push and, when it was
// stored, verifies and lands it. It returns the unpack line to tell the
// client and why the push did not land ("" when it did).
func (g *PushGate) verifyAndLand(ctx context.Context, sr officers.ShipRepo, sp *shepherdPush, raw []byte) (string, string) {
	lines := reportStatusLines(raw)
	unpack := ""
	stored, storedKnown := false, false
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "unpack "):
			unpack = l
		case l == "ok "+sp.scratch:
			stored, storedKnown = true, true
		case strings.HasPrefix(l, "ng "+sp.scratch+" ") || l == "ng "+sp.scratch:
			storedKnown = true
		}
	}
	if unpack == "" {
		return "unpack error: no report from GitHub", "GitHub sent no report of the push; nothing landed"
	}
	if unpack != "unpack ok" || !storedKnown || !stored {
		return unpack, "GitHub did not store the push; nothing landed"
	}
	ctx, cancel := context.WithTimeout(ctx, recheckTimeout)
	defer cancel()
	why, paths, tell := g.verifyShepherdPush(ctx, sr, sp)
	if why == "" {
		gh := g.ghFor(sr.Repo)
		if err := gh.UpdateBranch(ctx, sp.branch, sp.cmd.oldOID, sp.cmd.newOID); err != nil {
			why, tell = fmt.Sprintf("GitHub did not move the branch from %s to %s (%v)", shortSHA(sp.cmd.oldOID), shortSHA(sp.cmd.newOID), err), true
		} else {
			return unpack, ""
		}
	}
	if tell {
		summary := fmt.Sprintf("officer %s: muster refused a push to %s (#%d) in %s, %s -> %s, before it landed: %s; the branch was not moved",
			g.OfficerID, sp.branch, sp.pr.Number, sr.Repo, shortSHA(sp.cmd.oldOID), shortSHA(sp.cmd.newOID), why)
		if len(paths) > officers.MaxNoticePaths {
			summary += fmt.Sprintf(" (+%d more paths)", len(paths)-officers.MaxNoticePaths)
			paths = paths[:officers.MaxNoticePaths]
		}
		g.announce(ctx, officers.Notice{PR: sp.pr.Number, HeadSHA: sp.cmd.newOID, Paths: paths, Summary: summary, Channel: g.Ship.NoticeChannel})
	}
	return unpack, why + "; nothing landed"
}

// verifyShepherdPush is THE pre-landing check of a stored shepherd push, read
// fresh: the pull request is admitted again (its item still held, its author
// still the officer's) and still at old; old...new is a fast-forward of at
// most maxShepherdPushCommits commits and maxShepherdPushFiles files; and
// base...new passes the rule's wall and the draft rules. FAIL CLOSED: what
// cannot be read is a refusal. It returns why it refused ("" when it did
// not), the paths at fault, and whether HQ should hear of it.
func (g *PushGate) verifyShepherdPush(ctx context.Context, sr officers.ShipRepo, sp *shepherdPush) (string, []string, bool) {
	gh := g.ghFor(sr.Repo)
	if gh == nil {
		return "no GitHub reader for " + sr.Repo, nil, true
	}
	pr, err := gh.PR(ctx, sp.pr.Number)
	if err != nil {
		return fmt.Sprintf("pull request #%d could not be read again (%v)", sp.pr.Number, err), nil, true
	}
	if _, _, err := g.Shepherd.Admit(ctx, sr, gh, pr, sp.instance, func(string) {}); err != nil {
		return asShepherdRefusal(pr.Number, err).Error(), nil, true
	}
	if pr.HeadSHA != sp.cmd.oldOID || pr.HeadRef != sp.branch {
		return fmt.Sprintf("pull request #%d moved to %s:%s while the push was checked", pr.Number, pr.HeadRef, shortSHA(pr.HeadSHA)), nil, true
	}
	cmp, err := gh.Compare(ctx, sp.cmd.oldOID, sp.cmd.newOID)
	switch {
	case err != nil:
		return fmt.Sprintf("its fast-forward could not be checked (%v)", err), nil, true
	case cmp.Status != "ahead" || cmp.BehindBy != 0:
		return fmt.Sprintf("it is not a fast-forward of %s (GitHub compares the push %q, %d behind): a shepherd never rewrites a branch", shortSHA(sp.cmd.oldOID), cmp.Status, cmp.BehindBy), nil, true
	case cmp.AheadBy > maxShepherdPushCommits || len(cmp.Commits) > maxShepherdPushCommits:
		return fmt.Sprintf("it adds %d commits, over the cap of %d for one shepherd push", max(cmp.AheadBy, len(cmp.Commits)), maxShepherdPushCommits), nil, true
	case len(cmp.Files) > maxShepherdPushFiles:
		return fmt.Sprintf("it changes %d files, over the cap of %d for one shepherd push", len(cmp.Files), maxShepherdPushFiles), nil, true
	}
	v := g.checkWall(ctx, sr.Repo, gh, sp.rule.ForbiddenPaths, sp.rule.ForbiddenPathsFiles, baseOf(sr, pr), sp.cmd.newOID)
	switch {
	case v.Err != nil:
		return v.Err.Error() + ", so the change could not be checked", nil, true
	case len(v.Paths) > 0:
		return v.Why, v.Paths, true
	}
	return "", nil, false
}

// shepherdAnswer is the receive-pack answer the client gets for its ref: the
// report-status it asked for (when it asked: "ok", or "ng" with why), on side
// band 1 when it asked for a side band — with why on band 2, its progress
// channel, so `git push` prints it.
func shepherdAnswer(sp *shepherdPush, unpack, why string) []byte {
	var rep bytes.Buffer
	if sp.report {
		rep.WriteString(pktLine(unpack + "\n"))
		if why == "" {
			rep.WriteString(pktLine("ok " + sp.cmd.ref + "\n"))
		} else {
			rep.WriteString(pktLine("ng " + sp.cmd.ref + " " + oneLine(why, 900) + "\n"))
		}
		rep.WriteString("0000")
	}
	if !sp.sideband {
		return rep.Bytes()
	}
	var out bytes.Buffer
	if why != "" {
		out.WriteString(pktLine("\x02muster: " + oneLine(why, 900) + "\n"))
	}
	for b := rep.Bytes(); len(b) > 0; {
		n := min(len(b), 65515)
		out.WriteString(pktLine("\x01" + string(b[:n])))
		b = b[n:]
	}
	out.WriteString("0000")
	return out.Bytes()
}

// oneLine is s on one line, at most n bytes.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}

// endShepherdPush is a shepherd push's after-forward hook, run on every path
// out of the proxy once the push was admitted: the scratch ref is deleted
// (whether or not GitHub stored it — a delete of a missing ref is harmless)
// and the branch lock released.
func (g *PushGate) endShepherdPush(ctx context.Context, sr officers.ShipRepo, sp *shepherdPush) {
	defer g.unlockBranch(sr.Repo, sp.branch)
	ctx, cancel := context.WithTimeout(ctx, recheckTimeout)
	defer cancel()
	if gh := g.ghFor(sr.Repo); gh != nil {
		if err := gh.DeleteRef(ctx, sp.scratch); err != nil && !errors.Is(err, ErrRefMissing) {
			g.logf("officers: %s: the scratch ref %s in %s was not deleted: %v", g.OfficerID, sp.scratch, sr.Repo, err)
		}
	}
}

// commandLine is the first line of a comment body that reads as a command —
// its first non-space character is '/' (a slash command) or '@' (a mention
// that bot comment commands answer: "@<bot> merge") — ok false when none
// does. A shepherd's comment never carries one: comment automation in the
// target repository could act on it.
func commandLine(body string) (string, bool) {
	for _, l := range strings.Split(body, "\n") {
		t := strings.TrimLeft(l, " \t\r >")
		if strings.HasPrefix(t, "/") || strings.HasPrefix(t, "@") {
			return strings.TrimSpace(l), true
		}
	}
	return "", false
}

// admitShepherdAPI is the REST half of the shepherd gate: a comment, or
// update-branch, on a pull request Admit admits. Nothing else.
func (g *PushGate) admitShepherdAPI(r *http.Request, sr officers.ShipRepo, gh GitHub, tail []string, path string) (*http.Request, error) {
	read := func(n int) (PR, error) {
		if gh == nil {
			return PR{}, fmt.Errorf("no GitHub reader for %s", sr.Repo)
		}
		pr, err := gh.PR(r.Context(), n)
		if err != nil {
			return PR{}, fmt.Errorf("could not read pull request #%d: %v", n, err)
		}
		return pr, nil
	}
	switch {
	case r.Method == http.MethodPost && len(tail) == 3 && tail[0] == "issues" && isNumber(tail[1]) && tail[2] == "comments":
		n, _ := strconv.Atoi(tail[1])
		return g.admitJSON(r, map[string]bool{"body": true}, func(m map[string]json.RawMessage) error {
			pr, err := read(n)
			if err != nil {
				return err
			}
			_, item, err := g.Shepherd.Admit(r.Context(), sr, gh, pr, callerInstance(r.Context()), func(string) {})
			if err != nil {
				return asShepherdRefusal(n, err)
			}
			body := jsonString(m["body"])
			if redact.HasSecretValue(body) {
				return g.contentRefusal(r.Context(), sr, n, pr.HeadSHA, pr.HeadRef, nil, "the comment has a credential-shaped value")
			}
			if line, ok := commandLine(body); ok {
				return shepherdRefused("a comment on #%d with the line %q, which reads as a command to a bot or a slash command (a line starting with / or @): "+
					"comment automation in the repository could act on it; reword it so no line starts with / or @", n, truncate(line, 80))
			}
			if why, on := g.Shepherd.shadowRefusal(r.Context(), item, fmt.Sprintf("commented on %s#%d (%d chars)", sr.Repo, n, utf8.RuneCountInString(body))); on {
				return shadowRefused(why)
			}
			return nil
		})
	case r.Method == http.MethodPut && len(tail) == 3 && tail[0] == "pulls" && isNumber(tail[1]) && tail[2] == "update-branch":
		n, _ := strconv.Atoi(tail[1])
		return g.admitJSON(r, map[string]bool{"expected_head_sha": true}, func(m map[string]json.RawMessage) error {
			want := jsonString(m["expected_head_sha"])
			if !isOID(want) {
				return shepherdRefused("update-branch of #%d without expected_head_sha (the head you read): the update is pinned to the head the gates checked", n)
			}
			pr, err := read(n)
			if err != nil {
				return err
			}
			_, item, err := g.Shepherd.Admit(r.Context(), sr, gh, pr, callerInstance(r.Context()), func(string) {})
			if err != nil {
				return asShepherdRefusal(n, err)
			}
			if pr.HeadSHA != want {
				return shepherdRefused("update-branch of #%d at %s, which is not its head %s: re-read the pull request", n, shortSHA(want), shortSHA(pr.HeadSHA))
			}
			if why, on := g.Shepherd.shadowRefusal(r.Context(), item, fmt.Sprintf("updated %s from its base", pr.HeadRef)); on {
				return shadowRefused(why)
			}
			return nil
		})
	}
	return nil, shepherdRefused("%s api.github.com/%s", r.Method, path)
}

// ShepherdGates are a shepherd's gated tools.
type ShepherdGates struct {
	Shepherd *Shepherd
	// GHFor is the GitHub of ship repository repo (as the shepherd's App), nil
	// for any other.
	GHFor func(repo string) GitHub
	Now   func() time.Time
	// ItemByID is the merge ask's item gate: item id as HQ shows it, an error
	// unless instance holds it under a live lease and its subject is
	// officers.ShepherdSubject(repo, n). PostMergeAsk POSTs the merge ask
	// with the shepherd officer's own token (officers.Client.MergeAsk). nil:
	// the merge ask is refused.
	ItemByID     func(ctx context.Context, instance string, id officers.ItemID, repo string, n int) (officers.Item, error)
	PostMergeAsk func(ctx context.Context, item officers.ItemID, body []byte) ([]byte, error)
}

// ShepherdReadyOutcome is what officer_shepherd_mark_ready did.
type ShepherdReadyOutcome struct {
	Repo   string   `json:"repo"`
	PR     int      `json:"pr"`
	Ready  bool     `json:"ready"`
	Passed []string `json:"gates_passed"`
}

// MarkReady converts draft pull request n of repo, which instance shepherds,
// to ready for review. The gates, in order, read fresh:
//
//  1. stage — the officer is a shepherd;
//  2. base — repo is one of its ship repositories;
//  3. pull_request / base / head_branch / author / item — Shepherd.Admit, and
//     the pull request is still a draft with a node id;
//  4. checks — ship.required_checks is not empty and every entry is success on
//     the head (none listed: refused — CI green cannot be confirmed);
//  5. wall — base...head passes the authoring officer's wall and the draft
//     rules (the same check a push passes before it lands);
//  6. audit — AuditVerdictClear over the item the item gate found: the
//     auditor's report HQ recorded on it says no-red, at this round and head,
//     with no 🔴 line;
//  7. shadow — the item (or the officer) is not in shadow;
//  8. ready — GitHub's GraphQL markPullRequestReadyForReview.
func (g *ShepherdGates) MarkReady(ctx context.Context, instance, repo string, n int) (ShepherdReadyOutcome, error) {
	out := ShepherdReadyOutcome{Repo: repo, PR: n}
	pass := func(name string) { out.Passed = append(out.Passed, name) }
	if g == nil || g.Shepherd == nil || !g.Shepherd.Ship.IsShepherd() {
		return out, refuse(GateStage, "this officer is not a shepherd")
	}
	pass(GateStage)
	sp := g.Shepherd.Ship
	sr, ok := sp.RepoFor(g.Shepherd.Repo, repo)
	if !ok {
		return out, refuse(GateBase, "%q is not one of this shepherd's repositories", repo)
	}
	var gh GitHub
	if g.GHFor != nil {
		gh = g.GHFor(sr.Repo)
	}
	if gh == nil {
		return out, refuse(GatePR, "no GitHub reader for %s", sr.Repo)
	}
	pr, err := gh.PR(ctx, n)
	if err != nil {
		return out, refuse(GatePR, "could not read pull request #%d: %v", n, err)
	}
	if !pr.Draft || pr.NodeID == "" {
		return out, refuse(GatePR, "pull request #%d is not a draft (or GitHub gave no node id); only a draft is marked ready", n)
	}
	rule, item, err := g.Shepherd.Admit(ctx, sr, gh, pr, instance, pass)
	if err != nil {
		return out, err
	}
	if len(sp.RequiredChecks) == 0 {
		return out, refuse(GateChecks, "this shepherd lists no required_checks, so CI green on #%d's head cannot be confirmed: a pull request is marked ready only with CI green", n)
	}
	if err := (&Gates{Ship: sp, GH: gh}).checksGreen(ctx, pr.HeadSHA); err != nil {
		return out, err
	}
	pass(GateChecks)
	// checkWall reads only gh (no PushGate state): the same wall a push passes
	// before it lands, now at the head being marked ready.
	v := (&PushGate{Repo: sr.Repo}).checkWall(ctx, sr.Repo, gh, rule.ForbiddenPaths, rule.ForbiddenPathsFiles, baseOf(sr, pr), pr.HeadSHA)
	switch {
	case v.Err != nil:
		return out, refuse(GateWall, "%v, so #%d's head %s cannot be checked against the wall", v.Err, n, short(pr.HeadSHA))
	case len(v.Paths) > 0:
		return out, refuse(GateWall, "#%d's head %s touches the wall: %s (%s)", n, short(pr.HeadSHA), strings.Join(v.Paths, ", "), v.Why)
	}
	pass(GateWall)
	if err := AuditVerdictClear(item, pr); err != nil {
		return out, err
	}
	pass(GateAudit)
	if why, on := g.Shepherd.shadowRefusal(ctx, item, fmt.Sprintf("marked %s#%d ready", sr.Repo, n)); on {
		return out, refuse(GateShadow, "%s", why)
	}
	if err := gh.MarkReady(ctx, n, pr.NodeID); err != nil {
		return out, refuse(GateReady, "GitHub did not confirm #%d ready for review (%v); it may still have been converted — re-read the pull request before calling again", n, err)
	}
	out.Ready = true
	pass(GateReady)
	return out, nil
}
