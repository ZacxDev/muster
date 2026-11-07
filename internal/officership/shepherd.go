package officership

import (
	"context"
	"encoding/json"
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
//   - git push to that pull request's branch, every ref an UPDATE (never a
//     create or a delete) from the pull request's CURRENT head. The pack is
//     forwarded unread (it may be thin), so once GitHub has it the push is
//     checked again (afterShepherdPush): GitHub's comparison old...new must be
//     a fast-forward, and base...new must pass the AUTHORING officer's wall
//     (officers.ShepherdRules) and the draft rules (removed volume/database/
//     canary manifests, secret shapes). A push that fails either — or that
//     cannot be checked — is REVERTED to the head checked before forwarding
//     (RestoreBranch) and HQ is told. That revert is the one ref rewrite
//     muster makes, and only ever back to that head.
//   - POST issues/<n>/comments (no credential-shaped value);
//   - PUT pulls/<n>/update-branch with expected_head_sha = the current head
//     (GitHub merges the base into the head; never a rebase).
//
// And one tool, ShepherdGates.MarkReady (officer_shepherd_mark_ready).
//
// Everything else is refused: opening, editing (title, body, state — so
// closing and reopening), merging, the merge and refs APIs, GraphQL, every
// DELETE, a branch create or delete, a push to anything but a shepherded
// pull request's branch.
// ---------------------------------------------------------------------------

// GateItem is the shepherd's item gate: the instance holds the HQ item for
// the pull request.
const GateItem = "item"

// RED-FIRST STUBS: names the tests use, with the pre-fix behaviour.
const (
	GateWall              = "wall"
	GateAudit             = "audit"
	ShepherdScratchPrefix = "refs/muster/shepherd/"
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
	// ItemHeld is nil when HQ shows instance holding, under a live lease, an
	// item of this officer whose subject is officers.ShepherdSubject(repo, n).
	// nil func: everything is refused (fail closed).
	ItemHeld func(ctx context.Context, instance, repo string, n int) error
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
// rule. gh is sr's GitHub, as the shepherd's App. Every read failure is a
// refusal: fail closed.
func (s *Shepherd) Admit(ctx context.Context, sr officers.ShipRepo, gh GitHub, pr PR, instance string, pass func(string)) (officers.ShepherdRule, error) {
	n := pr.Number
	if s == nil || !s.Ship.IsShepherd() {
		return officers.ShepherdRule{}, refuse(GateStage, "this officer is not a shepherd")
	}
	if pr.State != "open" || pr.Merged {
		return officers.ShepherdRule{}, refuse(GatePR, "pull request #%d is %s (merged %v); a shepherd acts only on an open one", n, pr.State, pr.Merged)
	}
	pass(GatePR)
	if pr.BaseRef != sr.Base || !connectors.SameRepo(pr.BaseRepo, sr.Repo) {
		return officers.ShepherdRule{}, refuse(GateBase, "pull request #%d targets %s:%s; a shepherd acts only on pull requests into %s:%s", n, pr.BaseRepo, pr.BaseRef, sr.Repo, sr.Base)
	}
	pass(GateBase)
	if !strings.HasPrefix(pr.HeadRef, sr.BranchPrefix) || !officers.ValidRefSuffix(pr.HeadRef) || !connectors.SameRepo(pr.HeadRepo, sr.Repo) {
		return officers.ShepherdRule{}, refuse(GateHeadBranch, "pull request #%d comes from %s:%s; a shepherd acts only on a %s* branch of %s", n, pr.HeadRepo, pr.HeadRef, sr.BranchPrefix, sr.Repo)
	}
	pass(GateHeadBranch)
	rule, ok := s.rule(sr.Repo, pr.Author)
	if !ok {
		return rule, refuse(GateAuthor, "pull request #%d is authored by %s, who is not one of this shepherd's authors in %s", n, pr.Author, sr.Repo)
	}
	if strings.HasSuffix(strings.ToLower(rule.Login), "[bot]") && pr.AuthorType != "Bot" {
		return rule, refuse(GateAuthor, "pull request #%d is authored by %s typed %s; the listed login is an App's bot", n, pr.Author, pr.AuthorType)
	}
	slug, err := gh.AppSlug(ctx)
	if err != nil {
		return rule, refuse(GateAuthor, "could not read the shepherd App's identity: %v", err)
	}
	if strings.EqualFold(pr.Author, BotLogin(slug)) {
		return rule, refuse(GateAuthor, "pull request #%d is authored by the shepherd's own App (%s); a shepherd never acts on its own work", n, pr.Author)
	}
	if rule.AuthorOfficer != "" {
		if s.AuthorLogin == nil {
			return rule, refuse(GateAuthor, "the App of officer %s (the listed author %s) cannot be read here", rule.AuthorOfficer, rule.Login)
		}
		login, err := s.AuthorLogin(ctx, rule.AuthorOfficer)
		if err != nil {
			return rule, refuse(GateAuthor, "could not read officer %s's App identity, so %s cannot be confirmed as its bot: %v", rule.AuthorOfficer, pr.Author, err)
		}
		if !strings.EqualFold(login, pr.Author) {
			return rule, refuse(GateAuthor, "pull request #%d is authored by %s, but officer %s's App is %s: the listed login is not that officer's", n, pr.Author, rule.AuthorOfficer, login)
		}
	}
	pass(GateAuthor)
	if instance == "" {
		return rule, refuse(GateItem, "a request from an unknown instance: its hold on the item for %s cannot be checked", officers.ShepherdSubject(sr.Repo, n))
	}
	if s.ItemHeld == nil {
		return rule, refuse(GateItem, "muster cannot check HQ's items for %s", officers.ShepherdSubject(sr.Repo, n))
	}
	if err := s.ItemHeld(ctx, instance, sr.Repo, n); err != nil {
		return rule, refuse(GateItem, "HQ does not show %s holding the item for %s (claim it with %s first): %v", instance, officers.ShepherdSubject(sr.Repo, n), officers.ToolClaim, err)
	}
	pass(GateItem)
	return rule, nil
}

// shepherdRefused is a shepherd write refused (403), saying what a shepherd
// never does.
func shepherdRefused(format string, args ...any) error {
	return fmt.Errorf("%w: %s (a shepherd only pushes fast-forward commits to, comments on, updates the branch of and marks ready a pull request it holds the item for; "+
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

// shepherdPushCheck is one admitted ref update of a shepherd push: the pull
// request its branch is the head of, and that pull request's rule.
type shepherdPushCheck struct {
	cmd  pushCommand
	pr   PR
	rule officers.ShepherdRule
}

// admitShepherdPush is the push half of the shepherd gate: every command is an
// update (never a create or a delete) of the branch of exactly one open pull
// request Admit admits, from that pull request's current head.
func (g *PushGate) admitShepherdPush(ctx context.Context, sr officers.ShipRepo, cmds []pushCommand) ([]shepherdPushCheck, error) {
	gh := g.ghFor(sr.Repo)
	if gh == nil {
		return nil, fmt.Errorf("no GitHub reader for %s", sr.Repo)
	}
	instance := callerInstance(ctx)
	var out []shepherdPushCheck
	for _, c := range cmds {
		branch := strings.TrimPrefix(c.ref, "refs/heads/")
		switch {
		case zeroOID(c.oldOID):
			return nil, shepherdRefused("creating %s: a shepherd pushes only to an existing pull request's branch", c.ref)
		case zeroOID(c.newOID):
			return nil, shepherdRefused("deleting %s: a shepherd never deletes a branch", c.ref)
		}
		prs, err := g.openPRsFrom(ctx, sr, gh, branch)
		if err != nil {
			return nil, fmt.Errorf("could not look up the pull requests from %s in %s: %v", branch, sr.Repo, err)
		}
		if len(prs) != 1 {
			return nil, shepherdRefused("a push to %s, which is the head of %d open pull requests in %s; a shepherd pushes only to the branch of exactly one", branch, len(prs), sr.Repo)
		}
		pr, err := gh.PR(ctx, prs[0].Number)
		if err != nil {
			return nil, fmt.Errorf("could not read pull request #%d: %v", prs[0].Number, err)
		}
		rule, err := g.Shepherd.Admit(ctx, sr, gh, pr, instance, func(string) {})
		if err != nil {
			return nil, asShepherdRefusal(pr.Number, err)
		}
		if pr.HeadSHA != c.oldOID {
			return nil, shepherdRefused("a push to %s from %s, which is not pull request #%d's head %s: only a fast-forward of the current head is pushed — fetch, and build on it",
				branch, shortSHA(c.oldOID), pr.Number, shortSHA(pr.HeadSHA))
		}
		out = append(out, shepherdPushCheck{cmd: c, pr: pr, rule: rule})
	}
	return out, nil
}

// afterShepherdPush is the shepherd's push re-check, run by the connector
// proxy once GitHub answered the push. Per admitted ref GitHub applied (its
// report says "ok", or names nothing and the branch is now at the pushed
// commit): the push must be a fast-forward of the head checked before
// forwarding, and base...new must pass the pull request's wall. A failure,
// or a check that cannot be made, REVERTS the branch to that head (FAIL
// CLOSED) and tells HQ — paths, never content.
func (g *PushGate) afterShepherdPush(ctx context.Context, sr officers.ShipRepo, checks []shepherdPushCheck, res connectors.ForwardResult) {
	if !res.Forwarded || len(checks) == 0 || (res.Status != 0 && res.Status != http.StatusOK) {
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
	for _, c := range checks {
		branch := strings.TrimPrefix(c.cmd.ref, "refs/heads/")
		applied, known := report[c.cmd.ref]
		if known && !applied {
			continue
		}
		if !known {
			if cur, err := gh.BranchSHA(ctx, branch); err == nil && cur != c.cmd.newOID {
				g.logf("officers: %s: the push of %s to %s in %s did not land (the branch is at %s): not re-checked", g.OfficerID,
					shortSHA(c.cmd.newOID), branch, sr.Repo, shortSHA(cur))
				continue
			}
		}
		var why string
		var paths []string
		cmp, err := gh.Compare(ctx, c.cmd.oldOID, c.cmd.newOID)
		switch {
		case err != nil:
			why = fmt.Sprintf("its fast-forward could not be checked (%v)", err)
		case cmp.Status != "ahead" || cmp.BehindBy != 0:
			why = fmt.Sprintf("it is not a fast-forward of %s (GitHub compares the push %q, %d behind): a shepherd never rewrites a branch", shortSHA(c.cmd.oldOID), cmp.Status, cmp.BehindBy)
		default:
			v := g.checkWall(ctx, sr.Repo, gh, c.rule.ForbiddenPaths, c.rule.ForbiddenPathsFiles, baseOf(sr, c.pr), c.cmd.newOID)
			switch {
			case v.Err != nil:
				why = v.Err.Error() + ", so the change could not be checked"
			case len(v.Paths) > 0:
				why, paths = v.Why, v.Paths
			}
		}
		if why == "" {
			continue
		}
		what := fmt.Sprintf("reverted a push to %s (#%d) in %s from %s back to %s", branch, c.pr.Number, sr.Repo, shortSHA(c.cmd.newOID), shortSHA(c.cmd.oldOID))
		if err := gh.RestoreBranch(ctx, branch, c.cmd.newOID, c.cmd.oldOID); err != nil {
			what = fmt.Sprintf("could NOT revert a push to %s (#%d) in %s from %s back to %s (%v); a human must", branch, c.pr.Number, sr.Repo,
				shortSHA(c.cmd.newOID), shortSHA(c.cmd.oldOID), err)
		}
		summary := fmt.Sprintf("officer %s: muster %s: %s", g.OfficerID, what, why)
		if len(paths) > officers.MaxNoticePaths {
			summary += fmt.Sprintf(" (+%d more paths)", len(paths)-officers.MaxNoticePaths)
			paths = paths[:officers.MaxNoticePaths]
		}
		g.announce(ctx, officers.Notice{PR: c.pr.Number, HeadSHA: c.cmd.newOID, Paths: paths, Summary: summary, Channel: g.Ship.NoticeChannel})
	}
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
			if _, err := g.Shepherd.Admit(r.Context(), sr, gh, pr, callerInstance(r.Context()), func(string) {}); err != nil {
				return asShepherdRefusal(n, err)
			}
			if redact.HasSecretValue(jsonString(m["body"])) {
				return g.contentRefusal(r.Context(), sr, n, pr.HeadSHA, pr.HeadRef, nil, "the comment has a credential-shaped value")
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
			if _, err := g.Shepherd.Admit(r.Context(), sr, gh, pr, callerInstance(r.Context()), func(string) {}); err != nil {
				return asShepherdRefusal(n, err)
			}
			if pr.HeadSHA != want {
				return shepherdRefused("update-branch of #%d at %s, which is not its head %s: re-read the pull request", n, shortSHA(want), shortSHA(pr.HeadSHA))
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
//  4. checks — every ship.required_checks entry is success on the head (none
//     listed: nothing to wait for);
//  5. ready — GitHub's GraphQL markPullRequestReadyForReview.
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
	if _, err := g.Shepherd.Admit(ctx, sr, gh, pr, instance, pass); err != nil {
		return out, err
	}
	if len(sp.RequiredChecks) > 0 {
		if err := (&Gates{Ship: sp, GH: gh}).checksGreen(ctx, pr.HeadSHA); err != nil {
			return out, err
		}
		pass(GateChecks)
	}
	if err := gh.MarkReady(ctx, n, pr.NodeID); err != nil {
		return out, refuse(GateReady, "GitHub did not confirm #%d ready for review (%v); it may still have been converted — re-read the pull request before calling again", n, err)
	}
	out.Ready = true
	pass(GateReady)
	return out, nil
}
