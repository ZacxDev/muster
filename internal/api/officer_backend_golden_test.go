package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
	"github.com/ZacxDev/muster/internal/officership"
	"github.com/ZacxDev/muster/internal/provision"
)

// officerBackendGolden is what every HQ officer shape is told and offered:
// for each pool of each officer, the kickoff its first instance is given, the
// MCP tools its session pre-allows, and the officer tools it is offered over
// MCP, definitions included. It was RECORDED BEFORE the officer-backend
// refactor (muster#503 slices 1-2: the Backend interface and capabilities),
// so a diff here is an HQ officer told or offered something different.
//
//	MUSTER_UPDATE_GOLDEN=1 go test ./internal/api -run TestHQOfficerKickoffAndToolsAreGolden
//
// 🔴 Rewrite it only for a change that MEANS to alter what an HQ officer sees,
// and say so in the pull request. A refactor that moves this file is wrong.
const officerBackendGolden = "testdata/officer_backend.golden"

// goldenOfficers are the officer shapes the golden covers: every officer-type
// tool pack (release, task-reconciler, datastore, web-performance), both
// pools of a tiered officer (fresh-session routine, capped judgement), a
// read-only officer with read_repos and connectors, and a merge-stage
// shipping officer.
func goldenOfficers(t *testing.T, hqURL string) ([]officers.Officer, map[string]string) {
	t.Helper()
	ids := []string{"triage:example-web", "release:example-web", "gpu-health:gamma", "web-performance:example",
		"task-reconciler:example", "datastore:example"}
	tokens := map[string]string{}
	env := map[string]string{}
	for _, id := range ids {
		tok := "fixture-golden-token-" + officers.Slug(id)
		tokens[tok] = id
		env[officers.TokenEnvPrefix+officers.TokenEnvSuffix(id)] = tok
	}
	tiers := `"tiers":{"routine":{"model":"haiku","max_instances":2,"fresh_session_per_item":true},"judgement":{"model":"opus","max_instances":1,"max_item_minutes":30}}`
	raw := `[` +
		`{"id":"triage:example-web","hq_url":"` + hqURL + `","max_instances":2,"repo":"example-org/web","read_repos":["example-org/infra"],"connectors":["warehouse"]},` +
		`{"id":"release:example-web","hq_url":"` + hqURL + `","repo":"example-org/web"},` +
		`{"id":"gpu-health:gamma","hq_url":"` + hqURL + `",` + tiers + `},` +
		`{"id":"web-performance:example","hq_url":"` + hqURL + `",` + tiers + `},` +
		`{"id":"task-reconciler:example","hq_url":"` + hqURL + `"},` +
		`{"id":"datastore:example","hq_url":"` + hqURL + `",` + tiers + `}]`
	list, err := officers.Parse(raw, func(k string) string { return env[k] }, "muster-agent-")
	if err != nil {
		t.Fatalf("parse the golden officers: %v", err)
	}
	ship := officers.Officer{ID: "engineering:gamma-web", HQURL: hqURL, MaxInstances: 1, IdleAfter: 30 * time.Minute, PollInterval: time.Minute, Owner: 1,
		Repo: "example-org/gamma-web", Ship: &officers.Ship{Stage: officers.StageMerge, Base: "main", BranchPrefix: "officer/", AppID: 9001,
			RequiredChecks: []string{"ci / build"}, ReviewCheck: "pr-review", SensitivePaths: []string{"internal/auth/"},
			NoticePath: officers.DefaultNoticePath, NoticeChannel: "ch/example-alerts", ReleasesPerDay: 6}}.WithToken("fixture-golden-token-ship")
	tokens["fixture-golden-token-ship"] = ship.ID
	return append(list, ship), tokens
}

// TestHQOfficerKickoffAndToolsAreGolden: see officerBackendGolden.
func TestHQOfficerKickoffAndToolsAreGolden(t *testing.T) {
	got := renderOfficerGolden(t)
	if os.Getenv("MUSTER_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(officerBackendGolden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(officerBackendGolden)
	if err != nil {
		t.Fatalf("golden %s: %v (MUSTER_UPDATE_GOLDEN=1 writes it)", officerBackendGolden, err)
	}
	if got == string(want) {
		return
	}
	gl, wl := strings.Split(got, "\n"), strings.Split(string(want), "\n")
	for i := 0; i < len(gl) || i < len(wl); i++ {
		var g, w string
		if i < len(gl) {
			g = gl[i]
		}
		if i < len(wl) {
			w = wl[i]
		}
		if g != w {
			t.Fatalf("what an HQ officer is told or offered changed (first difference at line %d of %s):\n got: %q\nwant: %q",
				i+1, officerBackendGolden, g, w)
		}
	}
	t.Fatalf("what an HQ officer is told or offered changed (%s)", officerBackendGolden)
}

func renderOfficerGolden(t *testing.T) string {
	t.Helper()
	hq := hqtest.New(nil)
	t.Cleanup(hq.Close)
	list, _ := goldenOfficers(t, hq.BaseURL())
	store := &officerAgents{rows: []agents.Agent{{ID: officerOrdinaryID, Name: "brave-otter", Status: agents.StatusRunning, OwnerID: 1}}}
	prov := &officerProv{store: store, phase: map[string]provision.Phase{}}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(&bytes.Buffer{}, "", 0))
	s.UseExtensions(Extensions{Agents: store, Provisioner: prov, Gateway: &repromptChat{}, SessionLiveness: stubLiveness{}, Kinds: stubKinds{pick: "acct-one"}})
	gh := &apiFakeGH{}
	if _, err := s.EnableOfficers(OfficersConfig{Officers: list, InstallSession: discardSession, NoShipResume: true,
		ShipGitHub: func(officers.Officer) officership.GitHub { return gh }}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var b strings.Builder
	id := int64(9100)
	for _, o := range list {
		for _, p := range o.Pools() {
			id++
			a := agents.Agent{ID: id, Name: p.InstanceName(1), Kind: agents.KindClaudeCode, Status: agents.StatusRunning, OwnerID: 1, Namespace: "ns"}
			store.mu.Lock()
			store.rows = append(store.rows, a)
			store.mu.Unlock()
			fmt.Fprintf(&b, "=== officer %s pool %q instance %s\n--- kickoff\n%s\n", o.ID, p.Tier, a.Name, officers.KickoffText(p, 1))
			sess, ok := s.OfficerClaudeCodeSession(a)
			if !ok {
				t.Fatalf("%s is not an officer instance", a.Name)
			}
			fmt.Fprintf(&b, "--- allow\n%s\n--- tools\n", strings.Join(sess.AllowMCPTools, "\n"))
			var defs []agents.ToolDef
			for _, d := range s.MCPToolDefsFor(ctx, a) {
				if isOfficerTool(d.Name) {
					defs = append(defs, d)
				}
			}
			sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
			for _, d := range defs {
				j, err := json.MarshalIndent(d, "", "  ")
				if err != nil {
					t.Fatal(err)
				}
				fmt.Fprintf(&b, "%s\n", j)
			}
		}
	}
	return b.String()
}
