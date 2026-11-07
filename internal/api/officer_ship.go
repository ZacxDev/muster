package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

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
//   - four MCP tools — officer_merge_pr, officer_tag_release,
//     officer_rollout_status, officer_mark_ready — whose gates run HERE, with
//     muster's own token;
//   - with a `ci_logs` block, a fifth, officer_ci_logs: read-only, the failed
//     CI tasks' excerpts of its OWN pull request, read here with a short-lived
//     token of a dedicated namespace-scoped ServiceAccount, minted per call
//     (officership.CILogsReader);
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
	// ci answers officer_ci_logs (nil without a ci_logs block); ciErr is why
	// it could not be built, said on every call rather than failing the boot.
	ci    *officership.CILogsReader
	ciErr error
	// shepherd answers officer_shepherd_mark_ready (a shepherd-stage officer
	// only; nil otherwise).
	shepherd *officership.ShepherdGates
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
		ghFor   = map[string]officership.GitHub{}
		cred    func(ctx context.Context, caller connectors.Caller, host, rest string) (string, bool, error)
		reject  func(connectors.Caller)
		gitHost = githubapp.DefaultGitHost
		apiHost = githubapp.DefaultAPIHost
	)
	if cfg.ShipGitHub != nil {
		gh = cfg.ShipGitHub(o)
		for _, r := range sp.RepoList(o.Repo) {
			ghFor[strings.ToLower(r.Repo)] = gh
			if cfg.ShipGitHubRepo != nil {
				ghFor[strings.ToLower(r.Repo)] = cfg.ShipGitHubRepo(o, r.Repo)
			}
		}
		cred = func(context.Context, connectors.Caller, string, string) (string, bool, error) {
			return "fixture-ship-credential", false, nil
		}
		if cfg.ShipHosts[0] != "" {
			gitHost, apiHost = cfg.ShipHosts[0], cfg.ShipHosts[1]
		}
	} else {
		// ONE App per officer, installed on every ship repository: its
		// accounts are the repositories' owners.
		var accounts []githubapp.Account
		seen := map[int64]bool{}
		for _, r := range sp.RepoList(o.Repo) {
			if !seen[r.AppAccount.ID] {
				seen[r.AppAccount.ID] = true
				accounts = append(accounts, r.AppAccount)
			}
		}
		app, err := githubapp.New(githubapp.Config{AppID: sp.AppID, PrivateKey: sp.AppKey(), Accounts: accounts,
			Access: githubapp.AccessWrite, Logger: s.logger})
		if err != nil {
			return nil, fmt.Errorf("officer %s: its GitHub App: %w", o.ID, err)
		}
		cred, reject = app.Credential, app.Rejected
		for _, r := range sp.RepoList(o.Repo) {
			repo := r.Repo
			ghFor[strings.ToLower(repo)] = &officership.REST{Base: strings.TrimRight(github.APIBaseURL(), "/"), Repo: repo,
				Token: func(ctx context.Context) (string, error) { return app.RepoToken(ctx, repo) },
				Slug:  app.Slug, HTTP: &http.Client{Timeout: officers.DefaultHQTimeout * 2}}
		}
		gh = ghFor[strings.ToLower(o.Repo)]
	}
	hq := officerHQ{c: s.officerRT.hq.Client(o)}
	rt := &shipRuntime{}
	rt.gate = &officership.PushGate{Repo: o.Repo, Ship: sp, OfficerID: o.ID, GitHost: gitHost, APIHost: apiHost, Cred: cred,
		ReadCred: s.deploymentReadCred(), OnRejected: reject, GH: gh, GHFor: ghFor, Now: cfg.Now, Logf: s.logger.Printf,
		Announce: func(ctx context.Context, n officers.Notice) error {
			return hq.PostNotice(ctx, sp.NoticeURLPath(o.ID), n)
		},
		ItemClaimed: func(ctx context.Context, instance string, item int) error {
			return itemClaimedBy(ctx, hq.c, o.ID, instance, item, cfg.Now)
		}}
	rt.gates = &officership.Gates{OfficerID: o.ID, Repo: o.Repo, Ship: sp, GH: gh, HQ: hq, Now: cfg.Now,
		InFlight: func() (string, bool) { return "", false }}
	if sp.IsShepherd() {
		sh := &officership.Shepherd{OfficerID: o.ID, Repo: o.Repo, Ship: sp, Rules: officers.ShepherdRules(o, s.officerRT.reg.All()),
			AuthorLogin: s.shepherdAuthorLogin,
			ItemHeld: func(ctx context.Context, instance, repo string, n int) error {
				return itemHeldFor(ctx, hq.c, o.ID, instance, repo, n, cfg.Now)
			}}
		rt.gate.Shepherd = sh
		rt.shepherd = &officership.ShepherdGates{Shepherd: sh, Now: cfg.Now,
			GHFor: func(repo string) officership.GitHub { return ghFor[strings.ToLower(repo)] }}
	}
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
	if o.CILogs != nil {
		rt.ci = &officership.CILogsReader{Repo: o.Repo, Ship: sp, Config: o.CILogs, GH: gh}
		if cfg.ShipCI != nil {
			r := cfg.ShipCI(o)
			rt.ci.Open = func(context.Context) (officership.CIReader, error) { return r, nil }
		} else {
			// 🔴 A CI-LOG SOURCE THAT CANNOT BE BUILT DOES NOT STOP THE SERVER:
			// it is a read-only aid, and a boot refusal here would stop every
			// officer's merges too. Logged now, and said on every call.
			rt.ci.Open, rt.ciErr = officership.NewKubeCIOpener(o.CILogs.Namespace, o.CILogs.ServiceAccount)
			if rt.ciErr != nil {
				s.logger.Printf("officers: %s: officer_ci_logs is unavailable: %v", o.ID, rt.ciErr)
			}
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
			w := rt.watch
			safeGo(s.logger, "officer rollout resume "+o.ID, func() { w.Resume(s.streams.context()) })
		}
		if o.Ship.IsDraft() && !cfg.NoShipResume {
			o, rt := o, rt
			safeGo(s.logger, "officer orphan report "+o.ID, func() { s.orphanReportLoop(s.streams.context(), o, rt) })
			safeGo(s.logger, "officer draft sweep "+o.ID, func() { s.draftSweepLoop(s.streams.context(), o, rt) })
		}
	}
	return nil
}

// deploymentReadCred is the deployment App's credential for a shipping
// officer's read_repos (officership.PushGate.ReadCred): minted READ-ONLY for
// the one repository (the caller arrives ReadOnly with Repo set to it). nil
// when the deployment has no App, or it cannot mint per caller: those reads
// would then go anonymously, so a shipping officer with read_repos on a
// deployment with no App refuses to boot (muster-server's
// config.validateShipReadRepos).
func (s *Server) deploymentReadCred() func(ctx context.Context, caller connectors.Caller, host, rest string) (string, bool, error) {
	c, ok := s.ext.GitHubApp.(interface {
		Credential(ctx context.Context, caller connectors.Caller, host, rest string) (string, bool, error)
	})
	if !ok {
		return nil
	}
	return func(ctx context.Context, caller connectors.Caller, host, rest string) (string, bool, error) {
		caller.ReadOnly, caller.ReadRepos, caller.Gate = true, nil, nil
		return c.Credential(ctx, caller, host, rest)
	}
}

// itemClaimedBy is the draft gate's claim check (PushGate.ItemClaimed): nil
// when HQ (read with officer officerID's own token) shows instance holding
// item under a live lease at now. Anything else — another holder, an expired
// lease, no such claimed item, HQ unreadable — is an error, and the new pull
// request is refused.
func itemClaimedBy(ctx context.Context, c *officers.Client, officerID, instance string, item int, now func() time.Time) error {
	items, err := c.AllItems(ctx, officers.StatusClaimed)
	if err != nil {
		return fmt.Errorf("HQ's claimed items could not be read: %v", err)
	}
	at := time.Now()
	if now != nil {
		at = now()
	}
	want := strconv.Itoa(item)
	for _, it := range items {
		if string(it.ID) != want {
			continue
		}
		switch {
		case it.OfficerID != "" && it.OfficerID != officerID:
			return fmt.Errorf("item %d belongs to officer %s", item, it.OfficerID)
		case it.ClaimedBy != instance:
			return fmt.Errorf("item %d is claimed by %s", item, it.ClaimedBy)
		case !it.LeaseLive(at):
			return fmt.Errorf("the lease on item %d has expired", item)
		}
		return nil
	}
	return fmt.Errorf("item %d is not among officer %s's claimed items in HQ", item, officerID)
}

// shepherdAuthorLogin is officership.Shepherd.AuthorLogin: the bot login of
// officer id's OWN App, read through that officer's ship runtime (its App's
// GET /app) at call time. An officer with no ship runtime is an error, so the
// shepherd refuses (fail closed).
func (s *Server) shepherdAuthorLogin(ctx context.Context, officerID string) (string, error) {
	rt, ok := s.officerRT.ships[officerID]
	if !ok || rt == nil || rt.gates == nil || rt.gates.GH == nil {
		return "", fmt.Errorf("officer %s has no ship runtime on this muster", officerID)
	}
	slug, err := rt.gates.GH.AppSlug(ctx)
	if err != nil {
		return "", err
	}
	return officership.BotLogin(slug), nil
}

// itemHeldFor is the shepherd's item gate (officership.Shepherd.ItemHeld):
// nil when HQ (read with officer officerID's own token) shows instance
// holding, under a live lease at now, an item of the officer whose subject_key
// names pull request n of repo (officers.SameShepherdSubject). Anything else —
// no such item, another holder, an expired lease, HQ unreadable — is an error.
func itemHeldFor(ctx context.Context, c *officers.Client, officerID, instance, repo string, n int, now func() time.Time) error {
	items, err := c.AllItems(ctx, officers.StatusClaimed)
	if err != nil {
		return fmt.Errorf("HQ's claimed items could not be read: %v", err)
	}
	at := time.Now()
	if now != nil {
		at = now()
	}
	var why []string
	for _, it := range items {
		if !officers.SameShepherdSubject(it.SubjectKey, repo, n) || (it.OfficerID != "" && it.OfficerID != officerID) {
			continue
		}
		switch {
		case it.ClaimedBy != instance:
			why = append(why, fmt.Sprintf("item %s is claimed by %s", it.ID, it.ClaimedBy))
		case !it.LeaseLive(at):
			why = append(why, fmt.Sprintf("the lease on item %s has expired", it.ID))
		default:
			return nil
		}
	}
	if len(why) > 0 {
		return fmt.Errorf("%s", strings.Join(why, "; "))
	}
	return fmt.Errorf("no claimed item of officer %s has subject %s", officerID, officers.ShepherdSubject(repo, n))
}

// draftSweepLoop runs draft officer o's sweep (officership.PushGate.Sweep)
// every officership.SweepInterval until ctx ends: the backstop that re-checks
// every open pull request at its current head.
func (s *Server) draftSweepLoop(ctx context.Context, o officers.Officer, rt *shipRuntime) {
	t := time.NewTicker(officership.SweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := rt.gate.Sweep(ctx); err != nil {
			s.logger.Printf("officers: %s: the draft sweep: %v", o.ID, err)
		}
	}
}

// orphanReportLoop runs officer o's orphan-branch report once a UTC day
// (first an hour after boot), until ctx ends.
func (s *Server) orphanReportLoop(ctx context.Context, o officers.Officer, rt *shipRuntime) {
	last := ""
	t := time.NewTimer(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if day := time.Now().UTC().Format(time.DateOnly); day != last {
			last = day
			if _, err := s.reportOrphans(ctx, o, rt, time.Now()); err != nil {
				s.logger.Printf("officers: %s: the orphan-branch report failed: %v", o.ID, err)
			}
		}
		t.Reset(time.Hour)
	}
}

// reportOrphans lists officer o's orphan branches at now, logs them, and
// posts them to HQ as a notice when there is any. It never deletes one.
func (s *Server) reportOrphans(ctx context.Context, o officers.Officer, rt *shipRuntime, now time.Time) ([]officership.Orphan, error) {
	orphans, err := officership.OrphanBranches(ctx, o.ID, o.Repo, o.Ship, func(repo string) officership.GitHub {
		return rt.gate.GHFor[strings.ToLower(repo)]
	}, now)
	if err != nil || len(orphans) == 0 {
		return orphans, err
	}
	n := officership.OrphanNotice(o.ID, o.Ship.NoticeChannel, orphans)
	s.logger.Printf("officers: %s: %s: %s", o.ID, n.Summary, strings.Join(n.Paths, " "))
	if err := rt.gate.Announce(ctx, n); err != nil {
		return orphans, fmt.Errorf("posting the report to HQ: %w", err)
	}
	return orphans, nil
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
		{Type: "function", Name: officers.ToolMarkReady,
			Description: "Mark YOUR OWN draft pull request ready for review (head branch under your branch prefix, authored by your App), so the review check runs — " +
				"a draft is not reviewed, and " + officers.ToolMergePR + " needs that review. Call it once CI is green on the draft's head. " +
				"muster checks at call time that the pull request is open, still a draft, into the base branch, from your prefix in your repository and authored by your App, " +
				"then converts it with GitHub's GraphQL mutation (REST PATCH cannot, and the push gate refuses a `draft` field). There is no way back to draft. " +
				"A refusal names the gate that failed. Before the ready gate nothing is changed; a ready-gate refusal means GitHub did not CONFIRM the conversion, " +
				"which may still have happened — re-read the pull request (its draft field) before calling again.",
			Parameters: objSchema(map[string]any{"pr": intProp("The pull request number.", 1, 999999999)}, "pr")},
		{Type: "function", Name: officers.ToolShepherdReady,
			Description: "Mark a pull request YOU SHEPHERD ready for review: another officer's draft, in one of your repositories, whose HQ item you hold. " +
				"muster checks at call time that it is an open draft into the repository's base, from its branch prefix, authored by one of your listed authors (never your own App), " +
				"that you hold the item whose subject is <owner>/<name>#<pr>, and that every required check is green on the head; then converts it with GitHub's GraphQL mutation. " +
				"There is no way back to draft, and you never merge: a human does. A refusal names the gate that failed.",
			Parameters: objSchema(map[string]any{"repo": strProp("The repository, owner/name."), "pr": intProp("The pull request number.", 1, 999999999)}, "repo", "pr")},
		{Type: "function", Name: officers.ToolRolloutStatus,
			Description: "Read muster's rollout watch of your latest release: phase, verdict (watching, healthy, not_live, unhealthy, superseded), the revert pull request and rollback tag if one was made, and its events. Put the verdict in your item's officer_complete summary.",
			Parameters:  objSchema(map[string]any{})},
		{Type: "function", Name: officers.ToolCILogs,
			Description: "Read WHY CI failed on YOUR OWN pull request (head branch under your branch prefix, authored by your App): muster finds the CI run for the pull request's CURRENT head " +
				"and returns, for each failed task, its failed steps and an excerpt of their logs — failing test names (--- FAIL / FAIL / ✘ lines), error lines (ERROR, SQLSTATE, panic, file:line messages) " +
				"and the last lines — capped in size, with lines that look like credentials withheld. Read-only. Call it whenever a required check is red, BEFORE escalating; " +
				"its note says when there is no run yet, the run is still going, or nothing failed.",
			Parameters: objSchema(map[string]any{"pr": intProp("The pull request number.", 1, 999999999)}, "pr")},
	}
}

func isShipTool(name string) bool {
	return name == officers.ToolMergePR || name == officers.ToolTagRelease || name == officers.ToolRolloutStatus || name == officers.ToolMarkReady || name == officers.ToolCILogs ||
		name == officers.ToolShepherdReady
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
		Repo   string `json:"repo"`
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
		out := map[string]any{"merged": true, "result": res}
		if res.UntestedTree {
			// Said on the result and in the log: put it in your item too.
			out["warning"] = res.Warning + " — record this on your item"
			s.logger.Printf("officers: %s: %s", o.ID, res.Warning)
		}
		return toolJSON(out)
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
	case officers.ToolMarkReady:
		if in.PR <= 0 {
			return toolErr("pr is required (the pull request number)")
		}
		res, err := rt.gates.MarkReady(ctx, in.PR)
		s.logShip(o, a, name, fmt.Sprintf("pr #%d", in.PR), res.Passed, err)
		if err != nil {
			return shipRefusal(name, err, res.Passed)
		}
		return toolJSON(map[string]any{"ready": true, "result": res,
			"next": "wait for the " + o.Ship.ReviewCheck + " check on the head, then call " + officers.ToolMergePR})
	case officers.ToolShepherdReady:
		if rt.shepherd == nil {
			return toolErr("officer %s is not a shepherd", o.ID)
		}
		if in.PR <= 0 || strings.TrimSpace(in.Repo) == "" {
			return toolErr("repo (owner/name) and pr (the pull request number) are required")
		}
		res, err := rt.shepherd.MarkReady(ctx, a.Name, strings.TrimSpace(in.Repo), in.PR)
		s.logShip(o, a, name, fmt.Sprintf("%s#%d", in.Repo, in.PR), res.Passed, err)
		if err != nil {
			return shipRefusal(name, err, res.Passed)
		}
		return toolJSON(map[string]any{"ready": true, "result": res, "next": "a human reviews and merges it; complete your item with what you did"})
	case officers.ToolRolloutStatus:
		if rt.watch == nil {
			return toolErr("officer %s has no rollout watch configured", o.ID)
		}
		cur, hist := rt.watch.Status()
		return toolJSON(map[string]any{"current": cur, "earlier": hist})
	case officers.ToolCILogs:
		if rt.ci == nil {
			return toolErr("officer %s has no ci_logs source configured", o.ID)
		}
		if rt.ciErr != nil {
			return toolErr("officer_ci_logs is unavailable on this muster: %v — escalate with the red check's name; a human reads the logs", rt.ciErr)
		}
		if in.PR <= 0 {
			return toolErr("pr is required (the pull request number)")
		}
		res, err := rt.ci.Read(ctx, in.PR)
		s.logShip(o, a, name, fmt.Sprintf("pr #%d head %s run %s state %s failed %d", in.PR, res.HeadSHA, res.PipelineRun, res.State, len(res.Failed)), nil, err)
		if err != nil {
			return shipRefusal(name, err, nil)
		}
		return toolJSON(res)
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
// `github` request grants: its OWN App's token, behind the push gate, and —
// at EVERY stage, read_repos being accepted beside any — a read of each of its
// read_repos with the deployment App's READ-ONLY token. The read_repos clause
// is appended here and only here, after whichever stage text applies.
func shipGrantsDescription(o officers.Officer) string {
	read := ""
	if len(o.ReadRepos) > 0 {
		read = "; reads of " + strings.Join(o.ReadRepos, ", ") + " with the deployment App's READ-ONLY token"
	}
	return shipStageGrants(o) + read
}

// shipStageGrants is shipGrantsDescription's stage-specific part: what the
// officer's own App may do in its ship repositories.
func shipStageGrants(o officers.Officer) string {
	sp := o.Ship
	if sp.IsShepherd() {
		var repos, authors []string
		for _, r := range sp.RepoList(o.Repo) {
			repos = append(repos, fmt.Sprintf("%s (pull requests into %s from %s* branches)", r.Repo, r.Base, r.BranchPrefix))
		}
		for _, a := range sp.Shepherd.Authors {
			authors = append(authors, a.Login)
		}
		return fmt.Sprintf("officer %s's OWN GitHub App (ID %d), behind muster's push gate, stage %s: reads; on OTHER officers' pull requests only (authored by %s) in %s, "+
			"and only one whose HQ item the instance holds: fast-forward pushes checked against the authoring officer's forbidden paths (a push that fails is reverted), "+
			"comments, update-branch, and marking ready (%s). It never opens, edits, closes or merges a pull request, never creates or deletes a branch and never force-pushes",
			o.ID, sp.AppID, sp.Stage, strings.Join(authors, ", "), strings.Join(repos, "; "), officers.ToolShepherdReady)
	}
	if sp.IsDraft() {
		var repos []string
		for _, r := range sp.RepoList(o.Repo) {
			repos = append(repos, fmt.Sprintf("%s (pushes ONLY to refs/heads/%s%s/<item>-<slug>, never %s)", r.Repo, r.BranchPrefix, officers.Slug(o.ID), r.Base))
		}
		return fmt.Sprintf("officer %s's OWN GitHub App (ID %d), behind muster's push gate, stage %s: reads; DRAFT pull requests only, in %s; "+
			"editing and commenting on its own; every pull request checked for forbidden paths, removed volume/database/canary manifests and "+
			"secret shapes. It never marks ready, merges, tags or deletes a branch", o.ID, sp.AppID, sp.Stage, strings.Join(repos, "; "))
	}
	return fmt.Sprintf("officer %s's OWN GitHub App (ID %d), for %s only, behind muster's push gate: reads; pushes ONLY to refs/heads/%s* "+
		"(never %s, never a tag); opening and editing its own pull requests and commenting; marking its own draft ready (%s). Merging and tagging happen only in muster, "+
		"through the gated %s and %s tools (stage %s)", o.ID, sp.AppID, o.Repo, sp.BranchPrefix, sp.Base, officers.ToolMarkReady, officers.ToolMergePR, officers.ToolTagRelease, sp.Stage)
}
