package agents_test

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// ---------------------------------------------------------------------------
// THE AGENT-ROW CONSUMERS LEDGER (docs/design/officers-on-hosts.md, "The
// instance's agent row").
//
// A host officer instance has an agent row that nothing provisions
// (Agent.RunsOnHost). Every piece of code that ITERATES agent rows — calls
// agents.Store.List or AgentsByNoteIDs, through the interface or the
// Postgres store — must either skip such a row or handle it correctly, and
// each one's disposition is written below. The set is found by TYPE, not by
// spelling: the whole module is type-checked and every call whose callee is
// one of those methods of package agents is collected. The test fails when the
// set GROWS (a new consumer nobody classified) or SHRINKS (a stale entry).
//
// The dispositions name the guard. "skips" means the code branches on
// agents.Agent.Provisioned (or calls a helper that does); "reads" means it
// only reads the row in a way a provisioner-less row cannot mislead. The
// provisioner itself (agentprovision.Adapter) refuses every lifecycle call on
// such a row whatever its caller (TestAHostRowIsNeverProvisioned), which is
// why a consumer that only REACHES the provisioner through it is "reads".
// ---------------------------------------------------------------------------

var hostRowConsumers = map[string]string{
	// SKIPS: loops that act on rows, and lists that show rows to a person.
	"internal/agentkickoff/agentkickoff.go:Tick":            "skips: delivers first turns through the gateway and errors stuck rows",
	"internal/agentstatus/agentstatus.go:Tick":              "skips: polls the provisioner for provisioning rows",
	"internal/memorelay/relay.go:Tick":                      "skips: delivers memos to agents through the gateway",
	"internal/api/agent_watchdog.go:AgentWatchdogPass":      "skips: marks stuck agents and pushes about them",
	"internal/api/consumer_usage.go:consumerUsageInstances": "skips: reads Claude usage from pods",
	"internal/api/chief_nudge.go:stallFacts":                "skips: nudges stalled agents",
	"internal/api/focus.go:Candidates":                      "skips: spends a model describing each agent",
	"internal/api/agents.go:agentsListEntries":              "skips: the agent list and sidebar (the instance is its mirrored window)",
	"internal/api/agents_list_tool.go:toolAgentsList":       "skips: the owner's agent list over MCP",
	"internal/api/palette.go:paletteTargets":                "skips: the command palette's agents",
	"internal/api/focus_tools.go:toolFleetList":             "skips: the chief's fleet list",
	"internal/api/focus_tools.go:focusSearchHistory":        "skips: the live agents a focus search ranks",
	"internal/api/sessions_overview.go:liveSessions":        "skips: the chief's live sessions overview",
	"internal/api/fleet_report.go:collectFleet":             "skips: the fleet report's agents",
	// READS: lookups that cannot select a host row, or read it harmlessly.
	"internal/api/agent_outcome.go:outcomeIsChief":        "reads: the chief by name; a host row is named officer-…",
	"internal/api/agent_outcome.go:outcomeTaskDone":       "reads: agents on a task; a host row has no note",
	"internal/api/agents.go:agentPageCard":                "reads: the one agent a page shows, by id",
	"internal/api/chief_agent.go:agentIsChief":            "reads: the chief by name",
	"internal/api/chief_agent.go:chiefAgent":              "reads: the chief by name",
	"internal/api/chief_replace.go:replaceChief":          "reads: the chief by name",
	"internal/api/chief_restart_tool.go:toolChiefRestart": "reads: the chief by name",
	"internal/api/chief_toggle.go:accountChiefSection":    "reads: the chief by name",
	"internal/api/chief_toggle.go:chiefOnFor":             "reads: the chief by name",
	"internal/api/chief_toggle.go:handleAccountChief":     "reads: the chief by name",
	"internal/api/compose_chief.go:reapplyComposeChief":   "reads: the Compose chief only",
	"internal/api/jobs.go:createPoolJobOnce":              "reads: worker-pool members; a host row has no pool (agents_runs_on_host_check)",
	"internal/api/machine_agents.go:handleAPIAgentList":   "reads: GET /api/agents shows every row, runsOnHost included, for `muster agent ls`",
	"internal/api/nav_agents.go:homeTarget":               "reads: the chief by name, then agentsListEntries (which skips)",
	"internal/api/notes.go:actorIsAgent":                  "reads: an agent by name as a task comment's author",
	"internal/api/notes.go:apiAgentsByNote":               "reads: agents on a task; a host row has no note",
	"internal/api/notes.go:dismissTask":                   "reads: agents on a task; a host row has no note",
	"internal/api/notes.go:taskCardView":                  "reads: agents on a task; a host row has no note",
	"internal/api/notes.go:taskCardViews":                 "reads: agents on tasks; a host row has no note",
	"internal/api/officers.go:agentIsDeleteProtected":     "reads: a declared officer's instance rows are protected, a host one's included",
	"internal/api/officers.go:byName":                     "reads: the POD fleet's instances; officerFleet routes a host officer to the host fleet first",
	"internal/api/task_routing.go:liveOnTask":             "reads: agents on a task; a host row has no note",
	"internal/api/task_routing.go:mentionAsker":           "reads: an agent by name",
}

// iterators are the agents methods that return many rows.
var iterators = []string{"List", "AgentsByNoteIDs"}

type goListPkg struct {
	ImportPath string
	Dir        string
	GoFiles    []string
	Export     string
	Standard   bool
	Module     *struct{ Path string }
}

// agentRowIterations is every "<import path relative to the module>/<file>:<func>"
// whose body calls an agents row iterator, found by type.
func agentRowIterations(t *testing.T) []string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "list", "-e", "-export", "-deps", "-json", "./...")
	cmd.Dir = root
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go list: %v\n%s", err, stderr.String())
	}
	exports := map[string]string{}
	var mine []goListPkg
	dec := json.NewDecoder(&out)
	for {
		var p goListPkg
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		exports[p.ImportPath] = p.Export
		if p.Module != nil && p.Module.Path == "github.com/ZacxDev/muster" {
			mine = append(mine, p)
		}
	}
	const agentsPkg = "github.com/ZacxDev/muster/internal/agents"
	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		f := exports[path]
		if f == "" {
			return nil, os.ErrNotExist
		}
		return os.Open(f)
	})
	var found []string
	for _, p := range mine {
		var files []*ast.File
		for _, name := range p.GoFiles {
			f, err := parser.ParseFile(fset, filepath.Join(p.Dir, name), nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, f)
		}
		info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
		conf := types.Config{Importer: imp, Error: func(error) {}}
		if _, err := conf.Check(p.ImportPath, fset, files, info); err != nil && len(files) > 0 {
			t.Fatalf("type-check %s: %v", p.ImportPath, err)
		}
		rel := strings.TrimPrefix(p.ImportPath, "github.com/ZacxDev/muster/")
		for i, f := range files {
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					sel, ok := n.(*ast.SelectorExpr)
					if !ok || !slices.Contains(iterators, sel.Sel.Name) {
						return true
					}
					obj := info.Uses[sel.Sel]
					fn, ok := obj.(*types.Func)
					if !ok || fn.Pkg() == nil || fn.Pkg().Path() != agentsPkg {
						return true
					}
					recv := fn.Type().(*types.Signature).Recv()
					if recv == nil {
						return true
					}
					rt := recv.Type()
					if ptr, ok := rt.(*types.Pointer); ok {
						rt = ptr.Elem()
					}
					named, ok := rt.(*types.Named)
					if !ok || (named.Obj().Name() != "Store" && named.Obj().Name() != "PGStore") {
						return true
					}
					found = append(found, rel+"/"+p.GoFiles[i]+":"+fd.Name.Name)
					return true
				})
			}
		}
	}
	sort.Strings(found)
	return slices.Compact(found)
}

func TestEveryAgentRowConsumerHandlesAHostRow(t *testing.T) {
	got := agentRowIterations(t)
	if len(got) < 20 {
		t.Fatalf("instrument check FAILED: only %d consumers found by type; the type-check is not seeing the module", len(got))
	}
	for _, g := range got {
		if _, ok := hostRowConsumers[g]; !ok {
			t.Errorf("%s iterates agent rows and is not in hostRowConsumers: decide whether it skips a host officer's row "+
				"(agents.Agent.Provisioned) or handles it, and write that down", g)
		}
	}
	for k, v := range hostRowConsumers {
		if !slices.Contains(got, k) {
			t.Errorf("hostRowConsumers lists %s, which no longer iterates agent rows: remove it", k)
		}
		if !strings.HasPrefix(v, "skips: ") && !strings.HasPrefix(v, "reads: ") {
			t.Errorf("%s: disposition %q is neither skips: nor reads:", k, v)
		}
	}
	// 🔴 A "skips" ENTRY IS CHECKED, NOT TRUSTED: its function body must call
	// agents.ProvisionedOnly or Agent.Provisioned.
	root, _ := filepath.Abs(filepath.Join("..", ".."))
	fset := token.NewFileSet()
	skipping := 0
	for k, v := range hostRowConsumers {
		if !strings.HasPrefix(v, "skips: ") {
			continue
		}
		file, fn, _ := strings.Cut(k, ":")
		f, err := parser.ParseFile(fset, filepath.Join(root, file), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		calls := false
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Name.Name != fn || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && (sel.Sel.Name == "ProvisionedOnly" || sel.Sel.Name == "Provisioned") {
					calls = true
				}
				return true
			})
		}
		if !calls {
			t.Errorf("%s says it skips a host officer's row, and its body calls neither agents.ProvisionedOnly nor Agent.Provisioned", k)
		}
		skipping++
	}
	if skipping < 10 {
		t.Fatalf("instrument check FAILED: %d skipping consumers checked", skipping)
	}
}

// TestProvisionedOnlyDropsExactlyTheHostRows: the one skip.
func TestProvisionedOnlyDropsExactlyTheHostRows(t *testing.T) {
	list := []agents.Agent{{ID: 3, Name: "brave-otter"}, {ID: 5, Name: "officer-marketing-example-1", RunsOnHost: "host-alpha"}, {ID: 7, Name: "officer-x-y-1"}}
	got := agents.ProvisionedOnly(list)
	if len(got) != 2 || got[0].ID != 3 || got[1].ID != 7 {
		t.Fatalf("ProvisionedOnly = %+v, want rows 3 and 7", got)
	}
	if list[1].Provisioned() || !list[0].Provisioned() {
		t.Fatal("Provisioned disagrees with RunsOnHost")
	}
}
