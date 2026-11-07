package agentprovision

import (
	"bytes"
	"context"
	"log"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/agentspec"
	"github.com/ZacxDev/muster/internal/podagent"
	"github.com/ZacxDev/muster/internal/provision"
)

// THE CLAUDE-CODE CHIEF, AT THE SEAM: which row gets which prose, where it is
// placed, and what is left out — read from the BUILT spec, which is what the
// instance receives.

// ccSpecConfig is a deployment with the claude-code kind on and the connector
// surface (so the MCP server) offered. The values are pairwise distinct and
// match no constant either package names.
func ccSpecConfig() agentspec.Config {
	cfg := fixtureSpecConfig()
	cfg.ClaudeCode = &agentspec.ClaudeCodeConfig{Image: "ghcr.io/example-org/claude-code-agent:7c1", Accounts: map[string]string{"acct-x": "sk-ant-oat01-FAKE-chiefcc-3e9a"}}
	cfg.ConnectURL, cfg.ConnectPort = "http://muster.example.test:18091", 18091
	return cfg
}

func ccRow(name string) agents.Agent {
	ag := fixtureAgent()
	ag.Name = name
	ag.Kind, ag.CCAccount, ag.HooksToken = agents.KindClaudeCode, "acct-x", "fixture-hooks-chiefcc"
	ag.GatewaySecret = "fixture-turn-credential-chiefcc"
	ag.Model, ag.Repo, ag.RepoBranch = "", "", ""
	return ag
}

// TestAClaudeCodeChiefGetsTheChiefProseAsItsManagedClaudeMD is the matrix over the
// three independent axes — chief or worker, store configured or not, plugins on
// or off — asserting the WHOLE file set of each built spec: the chief carries
// exactly one file, the managed CLAUDE.md, holding exactly the claude-code chief
// prose for that plugins state; a worker carries none; neither ever carries the
// store's client or credential.
func TestAClaudeCodeChiefGetsTheChiefProseAsItsManagedClaudeMD(t *testing.T) {
	for _, chief := range []bool{true, false} {
		for _, store := range []bool{true, false} {
			for _, combo := range []struct{ plugins, routing bool }{{true, true}, {true, false}, {false, true}, {false, false}} {
				plugins, routing := combo.plugins, combo.routing
				cfg := ccSpecConfig()
				if store {
					cfg.CairnURL, cfg.CairnToken = "https://store.example.test:19001", "seam-fixture-store-credential-51ac9d"
				}
				cfg.Plugins, cfg.SessionRouting = plugins, routing
				row := ccRow("plain-worker")
				if chief {
					row = ccRow(agents.ChiefName)
				}
				a := adapterWith(t, cfg, row)
				spec, err := a.buildSpec(context.Background(), row)
				if err != nil {
					t.Fatalf("chief=%v store=%v plugins=%v: %v", chief, store, plugins, err)
				}
				if len(spec.Init) != 0 {
					t.Errorf("chief=%v store=%v: a claude-code spec carries Init %v", chief, store, spec.Init)
				}
				if !chief {
					if len(spec.Files) != 0 {
						t.Errorf("store=%v plugins=%v: a claude-code WORKER carries files %v; its prose (the in-pod CLI, "+
							"the autosave log) would describe tools its image lacks", store, plugins, filePaths(spec))
					}
					continue
				}
				if len(spec.Files) != 1 || spec.Files[0].Path != "/etc/claude-code/CLAUDE.md" {
					t.Fatalf("store=%v plugins=%v: the chief's files = %v, want exactly [/etc/claude-code/CLAUDE.md]",
						store, plugins, filePaths(spec))
				}
				got := string(spec.Files[0].Content)
				if got != agentspec.ChiefInstructionsClaudeCode(plugins, routing) {
					t.Errorf("store=%v plugins=%v routing=%v: the CLAUDE.md is not the claude-code chief prose for this state", store, plugins, routing)
				}
				// Independent of the constant: the request-routing section rides on
				// cfg.SessionRouting alone, with or without plugins.
				if strings.Contains(got, "`owner_asked: true`. muster checks that the turn you are in is their own message, pastes") != routing {
					t.Errorf("plugins=%v routing=%v: the request-routing section is in the prose = %v", plugins, routing, !routing)
				}
				// Independent of the constant: the claims that decide whether the
				// prose is TRUE in this image.
				if strings.Contains(got, "READ AND WRITE across ALL scopes") || strings.Contains(got, "`cairn`") {
					t.Errorf("store=%v: the claude-code chief's prose claims the store it is never given", store)
				}
				if strings.Contains(got, "CLI is on your PATH") || !strings.Contains(got, "mcp__muster__") {
					t.Errorf("the claude-code chief's prose sends it to a muster CLI its image lacks, or never names its MCP tools")
				}
				if strings.Contains(got, "mcp__muster__task_create_gated") != plugins || strings.Contains(got, "mcp__muster__reply_draft") != plugins {
					t.Errorf("plugins=%v: the plugin tools are named in the prose = %v", plugins, !plugins)
				}
			}
		}
	}
}

// TestAClaudeCodeChiefSaysItSkippedAConfiguredStore: the store is a capability
// the gateway chief would have had, so dropping it for this kind is logged —
// and only when there was a store to drop.
func TestAClaudeCodeChiefSaysItSkippedAConfiguredStore(t *testing.T) {
	for _, store := range []bool{true, false} {
		cfg := ccSpecConfig()
		if store {
			cfg.CairnURL, cfg.CairnToken = "https://store.example.test:19001", "seam-fixture-store-credential-51ac9d"
		}
		var buf bytes.Buffer
		row := ccRow(agents.ChiefName)
		rec := &recordingStore{agent: row}
		a, err := New(Config{Driver: &flakyDriver{Noop: provision.MustNewNoop(), rec: rec}, Store: rec, Spec: cfg, Logger: log.New(&buf, "", 0)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.buildSpec(context.Background(), row); err != nil {
			t.Fatal(err)
		}
		said := strings.Contains(buf.String(), "the subsystem store (cairn) is configured but is SKIPPED")
		if said != store {
			t.Errorf("store configured=%v, but the skip was logged=%v (log %q)", store, said, buf.String())
		}
	}
}

// TestAClaudeCodeChiefWithNoMCPServerIsRefused: every tool its prose names is an
// MCP tool, and with no connector URL ccd registers no MCP server — so the build
// refuses, naming the setting, instead of booting a chief with no tools.
func TestAClaudeCodeChiefWithNoMCPServerIsRefused(t *testing.T) {
	cfg := ccSpecConfig()
	cfg.ConnectURL, cfg.ConnectPort = "", 0
	row := ccRow(agents.ChiefName)
	_, err := adapterWith(t, cfg, row).buildSpec(context.Background(), row)
	if err == nil || !strings.Contains(err.Error(), "MUSTER_AGENT_CONNECT_URL") {
		t.Fatalf("buildSpec = %v, want a refusal naming MUSTER_AGENT_CONNECT_URL", err)
	}
	// Control: a WORKER on the same config builds (it names no MCP tool).
	if _, err := adapterWith(t, cfg, ccRow("plain-worker")).buildSpec(context.Background(), ccRow("plain-worker")); err != nil {
		t.Fatalf("control: a claude-code worker without the MCP server: %v", err)
	}
}

// TestOnlyTheClaudeCodeChiefIsOfferedTheHQServer: the built spec of the chief,
// and of no worker, carries podagent.EnvHQMCP=1 — the pod's `ccd seed` then
// registers the HQ MCP server and allows EVERY one of its tools
// (internal/podagent/hqmcp.go). A worker must never carry it: a standing grant
// would hand it the same unprompted reach into HQ.
func TestOnlyTheClaudeCodeChiefIsOfferedTheHQServer(t *testing.T) {
	for _, name := range []string{agents.ChiefName, "plain-worker"} {
		row := ccRow(name)
		spec, err := adapterWith(t, ccSpecConfig(), row).buildSpec(context.Background(), row)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var got []string
		for _, e := range spec.Env {
			if e.Name == "CCD_HQ_MCP" {
				got = append(got, e.Value)
			}
		}
		want := []string(nil)
		if name == agents.ChiefName {
			want = []string{"1"}
		}
		if strings.Join(got, ",") != strings.Join(want, ",") || len(got) != len(want) {
			t.Errorf("%s: CCD_HQ_MCP values %q, want %q", name, got, want)
		}
		for _, s := range spec.Secrets {
			if s.Name == "CCD_HQ_MCP" {
				t.Errorf("%s: CCD_HQ_MCP is a secret; it is a flag", name)
			}
		}
	}
}

// TestTheComposeChiefsProseNamesOnlyTheToolsItsSpecAllows is the seam between
// the two halves of the Compose chief's built spec: every muster tool its
// CLAUDE.md names is in the spec's own pre-allow list (the env the supervisor
// turns into permissions.allow), in every plugins/routing state — every other
// prompt is denied in that container. Control: the Kubernetes chief's spec
// carries no allow list and keeps the claude-code chief prose.
func TestTheComposeChiefsProseNamesOnlyTheToolsItsSpecAllows(t *testing.T) {
	re := regexp.MustCompile(`mcp__muster__([a-z_]+)`)
	for _, compose := range []bool{true, false} {
		for _, combo := range []struct{ plugins, routing bool }{{true, true}, {true, false}, {false, true}, {false, false}} {
			cfg := ccSpecConfig()
			cfg.ComposeContainer = compose
			cfg.Plugins, cfg.SessionRouting = combo.plugins, combo.routing
			row := ccRow(agents.ChiefName)
			spec, err := adapterWith(t, cfg, row).buildSpec(context.Background(), row)
			if err != nil {
				t.Fatalf("compose=%v %+v: %v", compose, combo, err)
			}
			prose := string(spec.Files[0].Content)
			var allow string
			for _, e := range spec.Env {
				if e.Name == podagent.EnvAllowMCPTools {
					allow = e.Value
				}
			}
			if !compose {
				if allow != "" || prose != agentspec.ChiefInstructionsClaudeCode(combo.plugins, combo.routing) {
					t.Errorf("control %+v: the Kubernetes chief's allow list = %q, or its prose changed", combo, allow)
				}
				continue
			}
			if prose != agentspec.ChiefInstructionsCompose(combo.plugins, combo.routing) {
				t.Errorf("%+v: the Compose chief's CLAUDE.md is not ChiefInstructionsCompose", combo)
			}
			allowed := strings.Split(allow, ",")
			for _, m := range re.FindAllStringSubmatch(prose, -1) {
				if !slices.Contains(allowed, m[1]) {
					t.Errorf("%+v: the Compose chief's prose names %s, which its spec does not pre-allow (%s)", combo, m[1], allow)
				}
			}
		}
	}
}
