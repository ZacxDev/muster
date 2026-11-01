package api

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
	"github.com/ZacxDev/muster/internal/officership"
	"github.com/ZacxDev/muster/internal/provision"
)

// apiFakeCI is one failed PipelineRun for shipHead (PR 12), in memory.
type apiFakeCI struct{ selectors []string }

func (f *apiFakeCI) PipelineRuns(_ context.Context, sel string) ([]officership.PipelineRun, error) {
	f.selectors = append(f.selectors, sel)
	return []officership.PipelineRun{{Name: "gamma-pr-check-x1", Created: time.Unix(10, 0), Succeeded: "False",
		Labels: map[string]string{"tekton.dev/pipeline": "gamma-pr-check", officers.DefaultCICommitLabel: shipHead},
		Params: map[string]string{officers.DefaultCIPRParam: "12"}}}, nil
}
func (f *apiFakeCI) TaskRuns(context.Context, string) ([]officership.TaskRun, error) {
	return []officership.TaskRun{{Name: "gamma-pr-check-x1-go", PipelineTask: "go", PodName: "gamma-pr-check-x1-go-pod", Succeeded: "False",
		Steps: []officership.Step{{Name: "test", Container: "step-test", Terminated: true, ExitCode: 1}}}}, nil
}
func (f *apiFakeCI) PodLog(context.Context, string, string, int64) (string, error) {
	return "--- FAIL: TestGammaFeed (0.2s)\n    feed_test.go:9: ERROR: column \"x\" does not exist (SQLSTATE 42703)\nFAIL\n", nil
}

// TestOfficerCILogsIsOfferedWithACILogsBlockAndAnswersOverDispatch: the
// shipping officer WITH a ci_logs block is offered and pre-allowed
// officer_ci_logs and gets the failed task's excerpt through dispatch; the
// shipping officer WITHOUT one, a plain officer and an ordinary agent are not
// offered it and are refused if they call it.
func TestOfficerCILogsIsOfferedWithACILogsBlockAndAnswersOverDispatch(t *testing.T) {
	const offCI, offCITok = "engineering:delta-web", "fixture-officer-token-delta-7c1d"
	hq := hqtest.New(map[string]string{offShipTok: offShip, offCITok: offCI, offBTok: offB})
	t.Cleanup(hq.Close)
	store := &officerAgents{rows: []agents.Agent{{ID: officerOrdinaryID, Name: "brave-otter", Status: agents.StatusRunning, OwnerID: 1}}}
	prov := &officerProv{store: store, phase: map[string]provision.Phase{}}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(&bytes.Buffer{}, "", 0))
	s.UseExtensions(Extensions{Agents: store, Provisioner: prov, Gateway: &repromptChat{}, SessionLiveness: stubLiveness{}, Kinds: stubKinds{pick: "acct-one"}})
	shipOf := func() *officers.Ship {
		return &officers.Ship{Stage: officers.StageShadow, Base: "main", BranchPrefix: "officer/", AppID: 9001,
			RequiredChecks: []string{"ci / build"}, ReviewCheck: "pr-review", NoticePath: officers.DefaultNoticePath, ReleasesPerDay: 6}
	}
	base := func(id, tok string) officers.Officer {
		return officers.Officer{ID: id, HQURL: hq.BaseURL(), MaxInstances: 1, IdleAfter: 30 * time.Minute, PollInterval: time.Minute, Owner: 1,
			Repo: shipRepo}.WithToken(tok)
	}
	withCI := base(offCI, offCITok)
	withCI.Ship = shipOf()
	withCI.CILogs = &officers.CILogs{Namespace: "gamma-ci", Pipeline: "gamma-pr-check", CommitLabel: officers.DefaultCICommitLabel,
		PRParam: officers.DefaultCIPRParam, TokenFile: "/var/run/secrets/gamma-ci/token", MaxBytes: 4096}
	noCI := base(offShip, offShipTok)
	noCI.Ship = shipOf()
	tri := base(offB, offBTok)
	ci := &apiFakeCI{}
	if _, err := s.EnableOfficers(OfficersConfig{Officers: []officers.Officer{withCI, noCI, tri}, InstallSession: discardSession, NoShipResume: true,
		ShipGitHub: func(officers.Officer) officership.GitHub { return &apiFakeGH{} },
		ShipCI:     func(officers.Officer) officership.CIReader { return ci }}); err != nil {
		t.Fatal(err)
	}
	add := func(o officers.Officer, id int64) agents.Agent {
		a := agents.Agent{ID: id, Name: officers.InstanceName(o.ID, 1), Kind: agents.KindClaudeCode, Status: agents.StatusRunning, OwnerID: 1, Namespace: "ns"}
		store.mu.Lock()
		store.rows = append(store.rows, a)
		store.mu.Unlock()
		return a
	}
	inst, noCIInst, triInst := add(withCI, 961), add(noCI, 962), add(tri, 963)
	plain, _ := store.Get(context.Background(), officerOrdinaryID)
	ctx := context.Background()

	got := officerToolNameSet(s.MCPToolDefsFor(ctx, inst))
	sess, _ := s.OfficerClaudeCodeSession(inst)
	if !got[officers.ToolCILogs] || !strings.Contains(","+strings.Join(sess.AllowMCPTools, ",")+",", ","+officers.ToolCILogs+",") {
		t.Fatalf("the ci_logs officer is not offered and pre-allowed %s", officers.ToolCILogs)
	}
	out := s.dispatchAgentTool(ctx, inst, officers.ToolCILogs, `{"pr":12}`)
	m := mergeResult(t, out)
	if m["state"] != officership.CIStateFailed || m["pipeline_run"] != "gamma-pr-check-x1" ||
		!strings.Contains(out, "--- FAIL: TestGammaFeed") || !strings.Contains(out, "SQLSTATE 42703") {
		t.Fatalf("officer_ci_logs answered %s", out)
	}
	if len(ci.selectors) != 1 || ci.selectors[0] != "tekton.dev/pipeline=gamma-pr-check,pipeline.jquad.rocks/pr.branch.commit="+shipHead {
		t.Fatalf("selectors %q", ci.selectors)
	}
	for _, a := range []agents.Agent{noCIInst, triInst, plain} {
		if officerToolNameSet(s.MCPToolDefsFor(ctx, a))[officers.ToolCILogs] {
			t.Errorf("%s is offered %s", a.Name, officers.ToolCILogs)
		}
		if out := s.dispatchAgentTool(ctx, a, officers.ToolCILogs, `{"pr":12}`); !isToolErr(out) {
			t.Errorf("%s calling %s: %s, want a refusal", a.Name, officers.ToolCILogs, out)
		}
	}
	if len(ci.selectors) != 1 {
		t.Fatalf("a refused caller reached the cluster (%d lists)", len(ci.selectors))
	}
}
