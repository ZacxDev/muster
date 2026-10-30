package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
	"github.com/ZacxDev/muster/internal/officership"
	"github.com/ZacxDev/muster/internal/provision"
)

const (
	offShip    = "engineering:gamma-web"
	offShipTok = "fixture-officer-token-gamma-2b8e"
	shipRepo   = "example-org/gamma-web"
	shipHead   = "9999999999999999999999999999999999999999"
)

// apiFakeGH is the GitHub the merge path reads, in memory. Methods the merge
// path does not call are left to the nil embedded interface (a call panics:
// the test would then be exercising something it did not set up).
type apiFakeGH struct {
	officership.GitHub
	mu     sync.Mutex
	files  []officership.File
	merged []int
}

func (f *apiFakeGH) AppSlug(context.Context) (string, error) { return "gamma-officer", nil }
func (f *apiFakeGH) PR(_ context.Context, n int) (officership.PR, error) {
	ok := true
	return officership.PR{Number: n, State: "open", Mergeable: &ok, Author: "gamma-officer[bot]", AuthorType: "Bot",
		HeadRef: "officer/3-fix", HeadSHA: shipHead, HeadRepo: shipRepo, BaseRef: "main", BaseRepo: shipRepo, Title: "fix"}, nil
}
func (f *apiFakeGH) Compare(context.Context, string, string) (officership.Compare, error) {
	return officership.Compare{Status: "ahead", AheadBy: 1}, nil
}
func (f *apiFakeGH) Statuses(context.Context, string) (map[string]string, error) {
	return map[string]string{"ci / build": "success"}, nil
}
func (f *apiFakeGH) CheckRuns(context.Context, string, string) ([]officership.CheckRun, error) {
	return []officership.CheckRun{{ID: 1, Name: "pr-review", Status: "completed", Conclusion: "success"}}, nil
}
func (f *apiFakeGH) PRComments(context.Context, int) ([]officership.Comment, error) { return nil, nil }
func (f *apiFakeGH) PRFiles(context.Context, int) ([]officership.File, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.files, nil
}
func (f *apiFakeGH) Merge(_ context.Context, n int, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.merged = append(f.merged, n)
	return "squash-sha", nil
}
func (f *apiFakeGH) BranchSHA(context.Context, string) (string, error) { return "base-sha", nil }
func (f *apiFakeGH) Commit(context.Context, string) (string, []string, error) {
	return "tree", []string{"base-sha"}, nil
}
func (f *apiFakeGH) mergedPRs() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.merged...)
}

type shipRig struct {
	s       *Server
	hq      *hqtest.Server
	gh      *apiFakeGH
	ship    officers.Officer
	triage  officers.Officer
	inst    agents.Agent
	triInst agents.Agent
	plain   agents.Agent
}

// newShipRig: a muster with a SHIPPING officer (stage merge, a sensitive
// path, HQ's notice route) and a plain triage officer, each with one running
// instance, HQ faked in-process and GitHub faked in memory.
func newShipRig(t *testing.T) *shipRig {
	t.Helper()
	hq := hqtest.New(map[string]string{offShipTok: offShip, offBTok: offB})
	t.Cleanup(hq.Close)
	store := &officerAgents{rows: []agents.Agent{{ID: officerOrdinaryID, Name: "brave-otter", Status: agents.StatusRunning, OwnerID: 1}}}
	prov := &officerProv{store: store, phase: map[string]provision.Phase{}}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(&bytes.Buffer{}, "", 0))
	s.UseExtensions(Extensions{Agents: store, Provisioner: prov, Gateway: &repromptChat{}, SessionLiveness: stubLiveness{}, Kinds: stubKinds{pick: "acct-one"}})
	ship := officers.Officer{ID: offShip, HQURL: hq.BaseURL(), MaxInstances: 1, IdleAfter: 30 * time.Minute, PollInterval: time.Minute, Owner: 1,
		Repo: shipRepo, Ship: &officers.Ship{Stage: officers.StageMerge, Base: "main", BranchPrefix: "officer/", AppID: 9001,
			RequiredChecks: []string{"ci / build"}, ReviewCheck: "pr-review", SensitivePaths: []string{"internal/auth/"},
			NoticePath: officers.DefaultNoticePath, NoticeChannel: "ch/example-alerts", ReleasesPerDay: 6}}.WithToken(offShipTok)
	tri := officers.Officer{ID: offB, HQURL: hq.BaseURL(), MaxInstances: 1, IdleAfter: 30 * time.Minute, PollInterval: time.Minute, Owner: 1,
		Repo: shipRepo}.WithToken(offBTok)
	gh := &apiFakeGH{}
	if _, err := s.EnableOfficers(OfficersConfig{Officers: []officers.Officer{ship, tri}, InstallSession: discardSession, NoShipResume: true,
		ShipGitHub: func(officers.Officer) officership.GitHub { return gh }}); err != nil {
		t.Fatal(err)
	}
	r := &shipRig{s: s, hq: hq, gh: gh, ship: ship, triage: tri}
	add := func(o officers.Officer, id int64) agents.Agent {
		a := agents.Agent{ID: id, Name: officers.InstanceName(o.ID, 1), Kind: agents.KindClaudeCode, Status: agents.StatusRunning, OwnerID: 1, Namespace: "ns"}
		store.mu.Lock()
		store.rows = append(store.rows, a)
		store.mu.Unlock()
		return a
	}
	r.inst, r.triInst = add(ship, 951), add(tri, 952)
	r.plain, _ = store.Get(context.Background(), officerOrdinaryID)
	return r
}

// TestOnlyAShippingOfficerIsOfferedTheGatedTools: offered over MCP and
// pre-allowed for the shipping officer's instance; not offered to a plain
// officer or an ordinary agent, and refused if called anyway.
func TestOnlyAShippingOfficerIsOfferedTheGatedTools(t *testing.T) {
	r := newShipRig(t)
	ctx := context.Background()
	gated := []string{officers.ToolMergePR, officers.ToolTagRelease, officers.ToolRolloutStatus}
	got := officerToolNameSet(r.s.MCPToolDefsFor(ctx, r.inst))
	sess, _ := r.s.OfficerClaudeCodeSession(r.inst)
	for _, n := range gated {
		if !got[n] || !strings.Contains(","+strings.Join(sess.AllowMCPTools, ",")+",", ","+n+",") {
			t.Errorf("the shipping officer is not offered and pre-allowed %s", n)
		}
	}
	for _, a := range []agents.Agent{r.triInst, r.plain} {
		offered := officerToolNameSet(r.s.MCPToolDefsFor(ctx, a))
		for _, n := range gated {
			if offered[n] {
				t.Errorf("%s is offered %s", a.Name, n)
			}
			if out := r.s.dispatchAgentTool(ctx, a, n, `{"pr":1}`); !isToolErr(out) {
				t.Errorf("%s calling %s: %s, want a refusal", a.Name, n, out)
			}
		}
	}
	if n := len(r.gh.mergedPRs()); n != 0 {
		t.Fatalf("a refused caller merged %d pull request(s)", n)
	}
}

// TestOnlyAShippingOfficersProxyCallerIsGated: the shipping officer's
// instance reaches the proxy with its push gate; a plain officer and an
// ordinary agent with none (they keep the read-only App path).
func TestOnlyAShippingOfficersProxyCallerIsGated(t *testing.T) {
	r := newShipRig(t)
	c := r.s.callerOf(r.inst)
	pg, ok := c.Gate.(*officership.PushGate)
	if !ok || pg.Repo != shipRepo || pg.Ship.BranchPrefix != "officer/" {
		t.Fatalf("shipping officer's caller gate = %#v", c.Gate)
	}
	for _, a := range []agents.Agent{r.triInst, r.plain} {
		if g := r.s.callerOf(a).Gate; g != nil {
			t.Errorf("%s carries a gate %T", a.Name, g)
		}
	}
	if !r.s.callerOf(r.triInst).ReadOnly {
		t.Error("a plain officer is no longer read-only")
	}
}

func mergeResult(t *testing.T, out string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("tool result is not JSON: %s", out)
	}
	return m
}

// TestTheSensitivePathNoticeGoesThroughHQsRouteAndItsFailureRefusesTheMerge:
// the operator's contract. A pull request touching a sensitive path posts
// {pr, head_sha, paths, summary, channel} to HQ's notices route with the
// OFFICER's token, then merges. When HQ answers 5xx, the merge is refused by
// the notice gate and nothing is merged.
func TestTheSensitivePathNoticeGoesThroughHQsRouteAndItsFailureRefusesTheMerge(t *testing.T) {
	r := newShipRig(t)
	ctx := context.Background()
	r.gh.files = []officership.File{{Path: "internal/auth/login.go", Status: "modified"}, {Path: "web/x.html", Status: "modified"}}

	out := r.s.dispatchAgentTool(ctx, r.inst, officers.ToolMergePR, `{"pr":12}`)
	if m := mergeResult(t, out); m["merged"] != true {
		t.Fatalf("merge with HQ accepting the notice: %s", out)
	}
	notices := r.hq.NoticesPosted()
	if len(notices) != 1 || notices[0].Officer != offShip {
		t.Fatalf("notices %+v, want one for %s", notices, offShip)
	}
	b := notices[0].Body
	if b["pr"] != float64(12) || b["head_sha"] != shipHead || b["channel"] != "ch/example-alerts" || b["summary"] == "" {
		t.Fatalf("notice body %v", b)
	}
	if paths, _ := b["paths"].([]any); len(paths) != 1 || paths[0] != "internal/auth/login.go" {
		t.Fatalf("notice paths %v, want exactly the sensitive one", b["paths"])
	}
	var authed bool
	for _, q := range r.hq.Requests() {
		if strings.HasSuffix(q.Path, "/notices") {
			authed = q.Auth == "Bearer "+offShipTok && q.Path == "/api/v1/officers/"+offShip+"/notices"
		}
	}
	if !authed {
		t.Fatal("the notice did not go to /officers/{id}/notices with the officer's own token")
	}

	r.hq.SetNoticeStatus(http.StatusServiceUnavailable)
	out = r.s.dispatchAgentTool(ctx, r.inst, officers.ToolMergePR, `{"pr":13}`)
	if m := mergeResult(t, out); m["gate"] != officership.GateNotice {
		t.Fatalf("merge with HQ's notice route down: %s, want a refusal by the notice gate", out)
	}
	if got := r.gh.mergedPRs(); len(got) != 1 || got[0] != 12 {
		t.Fatalf("merged %v; #13 must not merge when its notice failed", got)
	}
}

// TestAMergeIsRefusedWhenHQDisablesTheOfficer: HQ's switch, read at call
// time, is a kill switch for merges.
func TestAMergeIsRefusedWhenHQDisablesTheOfficer(t *testing.T) {
	r := newShipRig(t)
	r.hq.SetDisabled(offShip, true)
	out := r.s.dispatchAgentTool(context.Background(), r.inst, officers.ToolMergePR, `{"pr":12}`)
	if m := mergeResult(t, out); m["gate"] != officership.GateHQEnabled {
		t.Fatalf("disabled officer: %s", out)
	}
	r.hq.SetDisabled(offShip, false)
	out = r.s.dispatchAgentTool(context.Background(), r.inst, officers.ToolMergePR, `{"pr":12}`)
	if m := mergeResult(t, out); m["merged"] != true {
		t.Fatalf("re-enabled officer: %s", out)
	}
}

// TestAShippingOfficersKickoffTeachesTheGatesNotTheReadOnlyRecipe.
func TestAShippingOfficersKickoffTeachesTheGatesNotTheReadOnlyRecipe(t *testing.T) {
	r := newShipRig(t)
	text := officers.KickoffText(r.ship, 1)
	for _, want := range []string{officers.ToolMergePR, officers.ToolTagRelease, "refs/heads/officer/*", "git push origin officer/"} {
		if !strings.Contains(text, want) {
			t.Errorf("the kickoff does not say %q", want)
		}
	}
	for _, not := range []string{"You never merge", "READ-ONLY token", offShipTok} {
		if strings.Contains(text, not) {
			t.Errorf("the shipping officer's kickoff says %q", not)
		}
	}
	if plain := officers.KickoffText(r.triage, 1); !strings.Contains(plain, "You never merge") || strings.Contains(plain, officers.ToolMergePR) {
		t.Error("the plain officer's kickoff changed")
	}
}
