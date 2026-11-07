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

var (
	_ = json.Marshal
	_ = strconv.Itoa
	_ = redact.HasSecretValue
)

// GateItem is the shepherd's item gate: the instance holds the HQ item for
// the pull request.
const GateItem = "item"

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
	// STUB (red-first commit): the guard is not implemented yet.
	_, _, _ = ctx, gh, instance
	rule, _ := s.rule(sr.Repo, pr.Author)
	for _, g := range []string{GatePR, GateBase, GateHeadBranch, GateAuthor, GateItem} {
		pass(g)
	}
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
	// STUB (red-first commit): the guard is not implemented yet.
	_, _, _ = ctx, sr, cmds
	return nil, nil
}

// afterShepherdPush is the shepherd's push re-check, run by the connector
// proxy once GitHub answered the push. Per admitted ref GitHub applied (its
// report says "ok", or names nothing and the branch is now at the pushed
// commit): the push must be a fast-forward of the head checked before
// forwarding, and base...new must pass the pull request's wall. A failure,
// or a check that cannot be made, REVERTS the branch to that head (FAIL
// CLOSED) and tells HQ — paths, never content.
func (g *PushGate) afterShepherdPush(ctx context.Context, sr officers.ShipRepo, checks []shepherdPushCheck, res connectors.ForwardResult) {
	// STUB (red-first commit): the guard is not implemented yet.
	_, _, _, _ = ctx, sr, checks, res
}

// admitShepherdAPI is the REST half of the shepherd gate: a comment, or
// update-branch, on a pull request Admit admits. Nothing else.
func (g *PushGate) admitShepherdAPI(r *http.Request, sr officers.ShipRepo, gh GitHub, tail []string, path string) (*http.Request, error) {
	// STUB (red-first commit): the guard is not implemented yet.
	_, _, _, _ = sr, gh, tail, path
	return r, nil
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
	_ = instance
	if false {
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
