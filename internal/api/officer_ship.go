package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/github"
	"github.com/ZacxDev/muster/internal/githubapp"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officership"
)

// ---------------------------------------------------------------------------
// SHIPPING OFFICERS, BOUND TO muster (docs/officers.md "Shipping officers").
//
// An officer declared with a `ship` block gets, from this file:
//
//   - its OWN GitHub App (officers.Ship.AppID + its key), never the
//     deployment's — so suspending that App stops this officer and nothing else;
//   - a connectors.Gate (officership.PushGate) on every proxied GitHub request
//     its instances make (callerOf): the write filter, and the only source of
//     the credential those requests carry;
//   - three MCP tools — officer_merge_pr, officer_tag_release,
//     officer_rollout_status — whose gates run HERE, with muster's own token;
//   - the rollout watch, resumed at boot for a release still inside its window.
//
// 🔴 WHY THE INSTANCE CANNOT GO AROUND IT. The instance's pod holds no GitHub
// credential and its network policy reaches only muster's connect port. On
// that port the one route that attaches a GitHub credential is the connector
// proxy, which resolves the caller from the agent's own token and, for a
// shipping officer's instance, hands the request to the PushGate BEFORE any
// credential exists (connectors.Proxy.Serve). The gate, the gates, the
// policy and the App key all live in muster-server's code and environment,
// which no repository the officer's App is installed on can change.
// ---------------------------------------------------------------------------

// shipRuntime is one shipping officer's live state.
type shipRuntime struct {
	gate  *officership.PushGate
	gates *officership.Gates
	watch *officership.Watcher
}

// officerHQ adapts officers.Client to officership.HQ.
type officerHQ struct{ c *officers.Client }

func (h officerHQ) Enabled(ctx context.Context) (bool, error) { return h.c.Enabled(ctx) }
func (h officerHQ) PostNotice(ctx context.Context, path string, n officers.Notice) error {
	return h.c.PostNotice(ctx, path, n)
}
func (h officerHQ) Preflight(ctx context.Context) (officers.Preflight, error) {
	return h.c.Preflight(ctx)
}

// shipGitHubFor builds the GitHub side of officer o's ship runtime: the App,
// the push gate's credential, and the REST client the gates use. The test
// override (OfficersConfig.ShipGitHub) replaces the REST side and the
// credential.
func (s *Server) buildShip(cfg OfficersConfig, o officers.Officer) (*shipRuntime, error) {
	sp := o.Ship
	var (
		gh      officership.GitHub
		cred    func(ctx context.Context, caller connectors.Caller, host, rest string) (string, bool, error)
		reject  func(connectors.Caller)
		gitHost = githubapp.DefaultGitHost
		apiHost = githubapp.DefaultAPIHost
	)
	if cfg.ShipGitHub != nil {
		gh = cfg.ShipGitHub(o)
		cred = func(context.Context, connectors.Caller, string, string) (string, bool, error) {
			return "fixture-ship-credential", false, nil
		}
		if cfg.ShipHosts[0] != "" {
			gitHost, apiHost = cfg.ShipHosts[0], cfg.ShipHosts[1]
		}
	} else {
		app, err := githubapp.New(githubapp.Config{AppID: sp.AppID, PrivateKey: sp.AppKey(), Accounts: []githubapp.Account{sp.AppAccount},
			Access: githubapp.AccessWrite, Logger: s.logger})
		if err != nil {
			return nil, fmt.Errorf("officer %s: its GitHub App: %w", o.ID, err)
		}
		cred, reject = app.Credential, app.Rejected
		gh = &officership.REST{Base: strings.TrimRight(github.APIBaseURL(), "/"), Repo: o.Repo,
			Token: func(ctx context.Context) (string, error) { return app.RepoToken(ctx, o.Repo) },
			Slug:  app.Slug, HTTP: &http.Client{Timeout: officers.DefaultHQTimeout * 2}}
	}
	hq := officerHQ{c: s.officerRT.hq.Client(o)}
	rt := &shipRuntime{}
	rt.gate = &officership.PushGate{Repo: o.Repo, Ship: sp, GitHost: gitHost, APIHost: apiHost, Cred: cred, OnRejected: reject, GH: gh}
	rt.gates = &officership.Gates{OfficerID: o.ID, Repo: o.Repo, Ship: sp, GH: gh, HQ: hq, Now: cfg.Now,
		InFlight: func() (string, bool) { return "", false }}
	if sp.Rollout != nil {
		rt.watch = &officership.Watcher{OfficerID: o.ID, Repo: o.Repo, Ship: sp, GH: gh, Gates: rt.gates, Now: cfg.Now, HTTP: cfg.ShipHTTP,
			Logf: s.logger.Printf}
		rt.gates.InFlight = rt.watch.InFlight
		rt.gates.LiveVersion = rt.watch.LiveVersion
	} else {
		rt.gates.LiveVersion = func(context.Context) (string, error) {
			return "", fmt.Errorf("officer %s has no rollout block", o.ID)
		}
	}
	return rt, nil
}

// enableShips builds every shipping officer's runtime and resumes their
// rollout watches. Called by EnableOfficers once s.officerRT exists.
func (s *Server) enableShips(cfg OfficersConfig) error {
	for _, o := range s.officerRT.reg.All() {
		if o.Ship == nil {
			continue
		}
		rt, err := s.buildShip(cfg, o)
		if err != nil {
			return err
		}
		if s.officerRT.ships == nil {
			s.officerRT.ships = map[string]*shipRuntime{}
		}
		s.officerRT.ships[o.ID] = rt
		s.logger.Printf("officers: %s SHIPS to %s: %s; pushes only to refs/heads/%s*, merges and tags only through %s/%s",
			o.ID, o.Repo, o.Ship, o.Ship.BranchPrefix, officers.ToolMergePR, officers.ToolTagRelease)
		if rt.watch != nil && !cfg.NoShipResume {
			go rt.watch.Resume(s.streams.context())
		}
	}
	return nil
}

// shipOf is officer instance a's ship runtime, or nil.
func (s *Server) shipOf(a agents.Agent) *shipRuntime {
	o, _, ok := s.officerOf(a)
	if !ok || o.Ship == nil || s.officerRT.ships == nil {
		return nil
	}
	return s.officerRT.ships[o.ID]
}

// shipGate is the connectors.Gate for agent a's proxied requests, or nil.
//
// 🔴 A SHIPPING OFFICER WHOSE RUNTIME IS MISSING GETS A GATE THAT REFUSES
// EVERYTHING, never nil: nil would mean "no gate", and its requests would fall
// through to the deployment's read-only App path — safe today, but the
// ungated path must not be what a misconfiguration silently selects.
func (s *Server) shipGate(a agents.Agent) connectors.Gate {
	o, _, ok := s.officerOf(a)
	if !ok || o.Ship == nil {
		return nil
	}
	if rt := s.shipOf(a); rt != nil {
		return rt.gate
	}
	return refuseAllGate{}
}

// refuseAllGate refuses every request (a shipping officer with no runtime).
type refuseAllGate struct{}

func (refuseAllGate) Connector() string { return officers.GitHubConnector }
func (refuseAllGate) Describe() connectors.Connector {
	return connectors.Connector{Name: officers.GitHubConnector, Hosts: []string{githubapp.DefaultAPIHost, githubapp.DefaultGitHost}}
}
func (refuseAllGate) Admit(*http.Request, string, string) (*http.Request, error) {
	return nil, fmt.Errorf("%w: this officer's ship runtime is not running", connectors.ErrGateRefused)
}
func (refuseAllGate) Credential(context.Context, connectors.Caller, string, string) (string, bool, error) {
	return "", false, fmt.Errorf("%w: no ship runtime", connectors.ErrTargetRefused)
}
func (refuseAllGate) Rejected(connectors.Caller) {}

// shipToolDefs are the shipping officer's tools (offered by officers.ToolOffered
// only to an officer with a ship block).
func shipToolDefs() []agents.ToolDef {
	return []agents.ToolDef{
		{Type: "function", Name: officers.ToolMergePR,
			Description: "Squash-merge YOUR OWN pull request (head branch under your branch prefix, authored by your App) into the base branch. " +
				"muster checks every gate itself at call time — stage, HQ enabled, base/head/author, mergeable, up to date with the base, every required CI check success on the head, " +
				"the review check run completed+success (neutral does not pass), no /hold from a human, no destructive DDL in migrations — and posts HQ a notice first when a sensitive path changed. " +
				"A refusal names the gate that failed; fix that and call again. Nothing is merged on a refusal.",
			Parameters: objSchema(map[string]any{"pr": intProp("The pull request number.", 1, 999999999)}, "pr")},
		{Type: "function", Name: officers.ToolTagRelease,
			Description: "Tag the next release (the highest vX.Y.Z with its minor bumped) on the base branch's head, as an annotated tag. " +
				"muster checks every release gate at call time — stage, HQ enabled, the highest tag is an ancestor of the head, no release in flight and the previous one live, " +
				"the daily rate and minimum gap, quiet hours, CI green for the head's exact tree, migrations (and HQ's backup preflight when one is added) — then creates the tag and watches the rollout. " +
				"A tag someone else pushes at the same moment makes this fail and nothing is created. Give item_id to have the watch name your item.",
			Parameters: objSchema(map[string]any{"item_id": strProp("Optional: the HQ item this release is for.")})},
		{Type: "function", Name: officers.ToolRolloutStatus,
			Description: "Read muster's rollout watch of your latest release: phase, verdict (watching, healthy, not_live, unhealthy, superseded), the revert pull request and rollback tag if one was made, and its events. Put the verdict in your item's officer_complete summary.",
			Parameters:  objSchema(map[string]any{})},
	}
}

func isShipTool(name string) bool {
	return name == officers.ToolMergePR || name == officers.ToolTagRelease || name == officers.ToolRolloutStatus
}

// dispatchShipTool runs a shipping officer's tool for instance a of o.
func (s *Server) dispatchShipTool(ctx context.Context, a agents.Agent, o officers.Officer, name, arguments string) string {
	rt := s.shipOf(a)
	if rt == nil {
		return toolErr("officer %s's ship runtime is not running; nothing was done", o.ID)
	}
	var in struct {
		PR     int    `json:"pr"`
		ItemID string `json:"item_id"`
	}
	if err := json.Unmarshal([]byte(orEmptyJSON(arguments)), &in); err != nil {
		return toolErr("invalid arguments for %s: %v", name, err)
	}
	switch name {
	case officers.ToolMergePR:
		if in.PR <= 0 {
			return toolErr("pr is required (the pull request number)")
		}
		res, err := rt.gates.MergePR(ctx, in.PR)
		s.logShip(o, a, name, fmt.Sprintf("pr #%d head %s", in.PR, res.HeadSHA), res.Passed, err)
		if err != nil {
			return shipRefusal(name, err, res.Passed)
		}
		return toolJSON(map[string]any{"merged": true, "result": res})
	case officers.ToolTagRelease:
		if strings.ContainsAny(in.ItemID, "/?#") || len(in.ItemID) > 64 {
			return toolErr("item_id is not an item id")
		}
		res, err := rt.gates.TagRelease(ctx, officership.TagOptions{})
		s.logShip(o, a, name, fmt.Sprintf("previous %s at %s, head %s", res.Previous, res.PreviousSHA, res.SHA), res.Passed, err)
		if err != nil {
			return shipRefusal(name, err, res.Passed)
		}
		watching := false
		if rt.watch != nil {
			if werr := rt.watch.Start(s.streams.context(), res, in.ItemID); werr != nil {
				s.logger.Printf("officers: %s: the watch of %s did not start: %v", o.ID, res.Tag, werr)
			} else {
				watching = true
			}
		}
		return toolJSON(map[string]any{"tagged": true, "result": res, "watching": watching,
			"next": "call " + officers.ToolRolloutStatus + " for the rollout verdict before completing your item"})
	case officers.ToolRolloutStatus:
		if rt.watch == nil {
			return toolErr("officer %s has no rollout watch configured", o.ID)
		}
		cur, hist := rt.watch.Status()
		return toolJSON(map[string]any{"current": cur, "earlier": hist})
	}
	return toolErr("unknown tool %s", name)
}

// logShip is the audit line of every gated call: who, what, the gate inputs
// and the verdict.
func (s *Server) logShip(o officers.Officer, a agents.Agent, tool, inputs string, passed []string, err error) {
	verdict := "DONE"
	if err != nil {
		verdict = "REFUSED: " + err.Error()
	}
	s.logger.Printf("officers: %s %s by %s (%s); gates passed [%s]; %s", o.ID, tool, a.Name, inputs, strings.Join(passed, " "), verdict)
}

func shipRefusal(tool string, err error, passed []string) string {
	if r, ok := officership.AsRefusal(err); ok {
		return toolJSON(map[string]any{"error": tool + " refused: " + r.Reason, "gate": r.Gate, "gates_passed": passed})
	}
	return toolErr("%s failed: %v", tool, err)
}

// shipGrantsDescription is what approving a shipping officer instance's
// `github` request grants: its OWN App's token, behind the push gate.
func shipGrantsDescription(o officers.Officer) string {
	sp := o.Ship
	return fmt.Sprintf("officer %s's OWN GitHub App (ID %d), for %s only, behind muster's push gate: reads; pushes ONLY to refs/heads/%s* "+
		"(never %s, never a tag); opening and editing its own pull requests and commenting. Merging and tagging happen only in muster, "+
		"through the gated %s and %s tools (stage %s)", o.ID, sp.AppID, o.Repo, sp.BranchPrefix, sp.Base, officers.ToolMergePR, officers.ToolTagRelease, sp.Stage)
}
