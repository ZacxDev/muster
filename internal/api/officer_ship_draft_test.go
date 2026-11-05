package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
	"github.com/ZacxDev/muster/internal/officership"
	"github.com/ZacxDev/muster/internal/provision"
)

// ---------------------------------------------------------------------------
// SHIP v2 BOUND TO muster: a TIERED draft officer shipping to two
// repositories. Only its judgement instance is gated and served the officer
// App; it is not offered the merge, ready, tag or rollout tools; its content
// refusals and its orphan report reach HQ's notices route with its own token.
// ---------------------------------------------------------------------------

const (
	offDraft    = "cluster-watch:example"
	offDraftTok = "fixture-officer-token-draft-7c41"
	draftWeb    = "example-org/delta-web"
	draftInfra  = "example-org/delta-infra"
	draftBranch = "officer/cluster-watch-example/12-fix-limit"
)

// draftFakeGH is one repository's GitHub for the draft rig. Methods the rig
// does not call are the nil embedded interface (a call panics).
type draftFakeGH struct {
	officership.GitHub
	mu       sync.Mutex
	compare  officership.Compare
	branches []officership.Branch
	old      time.Time
}

func (f *draftFakeGH) AppSlug(context.Context) (string, error) { return "delta-officer", nil }
func (f *draftFakeGH) Compare(context.Context, string, string) (officership.Compare, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.compare, nil
}
func (f *draftFakeGH) ListPRs(context.Context) ([]officership.PR, error) { return nil, nil }
func (f *draftFakeGH) PRsForBranch(context.Context, string, string) ([]officership.PR, error) {
	return nil, nil
}
func (f *draftFakeGH) OfficerPRs(context.Context, string, string) ([]officership.PR, error) {
	return nil, nil
}
func (f *draftFakeGH) Branches(context.Context, string) ([]officership.Branch, error) {
	return f.branches, nil
}
func (f *draftFakeGH) CommitTime(context.Context, string) (time.Time, error) { return f.old, nil }

type draftRigAPI struct {
	s           *Server
	hq          *hqtest.Server
	web, infra  *draftFakeGH
	judge, rout agents.Agent
	officer     officers.Officer
}

func newDraftAPIRig(t *testing.T) *draftRigAPI {
	t.Helper()
	hq := hqtest.New(map[string]string{offDraftTok: offDraft})
	t.Cleanup(hq.Close)
	store := &officerAgents{}
	prov := &officerProv{store: store, phase: map[string]provision.Phase{}}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(&bytes.Buffer{}, "", 0))
	s.UseExtensions(Extensions{Agents: store, Provisioner: prov, Gateway: &repromptChat{}, SessionLiveness: stubLiveness{}, Kinds: stubKinds{pick: "acct-one"}})
	ship := &officers.Ship{Stage: officers.StageDraft, Base: "main", BranchPrefix: "officer/", AppID: 9100,
		NoticePath: officers.DefaultNoticePath, NoticeChannel: "ch/example-alerts",
		Repos: []officers.ShipRepo{{Repo: draftWeb, Base: "main", BranchPrefix: "officer/", AppID: 9100},
			{Repo: draftInfra, Base: "trunk", BranchPrefix: "officer/", AppID: 9100, ForbiddenPaths: []string{"nodes/**"}}}}
	o := officers.Officer{ID: offDraft, HQURL: hq.BaseURL(), MaxInstances: 1, Model: "opus", IdleAfter: 30 * time.Minute, PollInterval: time.Minute,
		Owner: 1, Repo: draftWeb, ReadRepos: []string{"example-org/delta-docs"}, Ship: ship, Tier: officers.TierJudgement,
		Tiers: &officers.Tiers{Routine: officers.TierPool{Model: "haiku", MaxInstances: 1}, Judgement: officers.TierPool{Model: "opus", MaxInstances: 1}}}.WithToken(offDraftTok)
	web, infra := &draftFakeGH{}, &draftFakeGH{}
	if _, err := s.EnableOfficers(OfficersConfig{Officers: []officers.Officer{o}, InstallSession: discardSession, NoShipResume: true,
		ShipGitHub: func(officers.Officer) officership.GitHub { return web },
		ShipGitHubRepo: func(_ officers.Officer, repo string) officership.GitHub {
			if repo == draftInfra {
				return infra
			}
			return web
		}}); err != nil {
		t.Fatal(err)
	}
	r := &draftRigAPI{s: s, hq: hq, web: web, infra: infra, officer: o}
	add := func(name string, id int64) agents.Agent {
		a := agents.Agent{ID: id, Name: name, Kind: agents.KindClaudeCode, Status: agents.StatusRunning, OwnerID: 1, Namespace: "ns"}
		store.mu.Lock()
		store.rows = append(store.rows, a)
		store.mu.Unlock()
		return a
	}
	r.judge = add(o.Pool(officers.TierJudgement).InstanceName(1), 961)
	r.rout = add(o.Pool(officers.TierRoutine).InstanceName(1), 962)
	return r
}

// TestOnlyTheJudgementInstanceOfATieredDraftOfficerShips: the judgement
// instance's caller carries the push gate (both repositories, the officer id);
// the routine instance's carries none and stays read-only on the deployment
// App, and is offered no ship tool.
func TestOnlyTheJudgementInstanceOfATieredDraftOfficerShips(t *testing.T) {
	r := newDraftAPIRig(t)
	pg, ok := r.s.callerOf(r.judge).Gate.(*officership.PushGate)
	if !ok || pg.OfficerID != offDraft || pg.GHFor[draftInfra] != officership.GitHub(r.infra) || pg.GHFor[draftWeb] != officership.GitHub(r.web) ||
		pg.Announce == nil {
		t.Fatalf("judgement caller gate = %#v", r.s.callerOf(r.judge).Gate)
	}
	rc := r.s.callerOf(r.rout)
	if rc.Gate != nil || !rc.ReadOnly {
		t.Fatalf("routine caller gate %T, read-only %v; want no gate, read-only", rc.Gate, rc.ReadOnly)
	}
	ctx := context.Background()
	for _, a := range []agents.Agent{r.judge, r.rout} {
		offered := officerToolNameSet(r.s.MCPToolDefsFor(ctx, a))
		for _, n := range []string{officers.ToolMergePR, officers.ToolTagRelease, officers.ToolRolloutStatus, officers.ToolMarkReady} {
			if offered[n] {
				t.Errorf("%s is offered %s", a.Name, n)
			}
			if out := r.s.dispatchAgentTool(ctx, a, n, `{"pr":1}`); !isToolErr(out) {
				t.Errorf("%s calling %s: %s, want a refusal", a.Name, n, out)
			}
		}
	}
}

// claimItem12 adds HQ items up to id 12 (draftBranch's item) and claims 12
// for holder.
func (r *draftRigAPI) claimItem12(holder string) {
	for id := ""; id != "12"; {
		id = r.hq.AddItem(offDraft, "fix", "subject-"+id)
	}
	if holder != "" {
		r.hq.SetClaim("12", holder, time.Now().Add(time.Hour))
	}
}

// draftCreate is the judgement instance's POST pulls from draftBranch,
// admitted through its gate with the caller in the request's context.
func (r *draftRigAPI) draftCreate() error {
	pg := r.s.callerOf(r.judge).Gate.(*officership.PushGate)
	body := officers.BodyFirstLine(offDraft, 12) + "\n## Root cause\n## Evidence\n## Test\n## Rollback\n"
	raw := `{"title":"t","head":"` + draftBranch + `","base":"trunk","draft":true,"body":` + string(mustJSON(body)) + `}`
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(raw))
	req = req.WithContext(connectors.WithCaller(req.Context(), r.s.callerOf(r.judge)))
	out, err := pg.Admit(req, "api.github.com", "repos/"+draftInfra+"/pulls")
	if err == nil {
		if after := connectors.AfterForwardOf(out); after != nil {
			after(context.Background(), connectors.ForwardResult{})
		}
	}
	return err
}

// TestADraftOfficersNewPullRequestNeedsItsHQClaim (Y2, wired): the gate asks
// HQ, with the officer's own token, whether THIS instance holds the branch's
// item; unclaimed, or claimed by another instance, is a 403 before any
// comparison; claimed by it, the request reaches the content check.
func TestADraftOfficersNewPullRequestNeedsItsHQClaim(t *testing.T) {
	r := newDraftAPIRig(t)
	r.claimItem12("")
	r.infra.compare = officership.Compare{Commits: []string{"abc"}, Files: []officership.File{{Path: "nodes/cp-1/x.yaml", Status: "added", Changes: 1, Patch: "@@ -0,0 +1 @@\n+a: 1"}}}
	if err := r.draftCreate(); !errors.Is(err, connectors.ErrGateRefused) || errors.Is(err, connectors.ErrGateConflict) || !strings.Contains(err.Error(), "item 12") {
		t.Fatalf("an unclaimed item: err = %v, want a 403 naming item 12", err)
	}
	r.hq.SetClaim("12", r.rout.Name, time.Now().Add(time.Hour))
	if err := r.draftCreate(); !errors.Is(err, connectors.ErrGateRefused) || errors.Is(err, connectors.ErrGateConflict) {
		t.Fatalf("an item another instance holds: err = %v, want a 403", err)
	}
	r.hq.SetClaim("12", r.judge.Name, time.Now().Add(time.Hour))
	if err := r.draftCreate(); !errors.Is(err, connectors.ErrGateConflict) {
		t.Fatalf("its own claim: err = %v, want the content check's 409 (it got past the claim)", err)
	}
}

// TestADraftOfficersContentRefusalReachesHQ: a pull request whose comparison
// changes a forbidden path is answered 409 by the gate, and HQ's notices route
// receives the notice with the officer's own token.
func TestADraftOfficersContentRefusalReachesHQ(t *testing.T) {
	r := newDraftAPIRig(t)
	r.claimItem12(r.judge.Name)
	r.infra.compare = officership.Compare{Commits: []string{"abc"}, Files: []officership.File{{Path: "nodes/cp-1/x.yaml", Status: "added", Changes: 1, Patch: "@@ -0,0 +1 @@\n+a: 1"}}}
	err := r.draftCreate()
	if !errors.Is(err, connectors.ErrGateConflict) {
		t.Fatalf("a forbidden path: err = %v, want the 409 conflict", err)
	}
	notices := r.hq.NoticesPosted()
	if len(notices) != 1 || notices[0].Officer != offDraft {
		t.Fatalf("notices %+v, want one for %s", notices, offDraft)
	}
	if paths, _ := notices[0].Body["paths"].([]any); len(paths) != 1 || paths[0] != "nodes/cp-1/x.yaml" {
		t.Fatalf("notice paths %v", notices[0].Body["paths"])
	}
}

// TestTheOrphanReportPostsAListToHQ: the daily report's one run lists the old
// branch with no pull request and posts it; it deletes nothing (the fake has
// no delete, and a call to one would panic).
func TestTheOrphanReportPostsAListToHQ(t *testing.T) {
	r := newDraftAPIRig(t)
	r.infra.branches = []officership.Branch{{Name: draftBranch, SHA: "s1"}}
	r.infra.old = time.Now().Add(-10 * 24 * time.Hour)
	r.web.old = time.Now()
	got, err := r.s.reportOrphans(context.Background(), r.officer, r.s.officerRT.ships[offDraft], time.Now())
	if err != nil || len(got) != 1 || got[0].Branch != draftBranch || got[0].Repo != draftInfra {
		t.Fatalf("orphans %+v, err %v", got, err)
	}
	notices := r.hq.NoticesPosted()
	if len(notices) != 1 {
		t.Fatalf("notices %+v, want the one report", notices)
	}
	if paths, _ := notices[0].Body["paths"].([]any); len(paths) != 1 || paths[0] != draftInfra+":"+draftBranch {
		t.Fatalf("report paths %v", notices[0].Body["paths"])
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
