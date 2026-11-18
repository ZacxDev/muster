package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE TOOL-LABEL SEAM LEDGER. A chat draws one of muster's own tool calls as a
// plain label ("Made a page", ui.MusterToolLabel) and keeps the raw name in the
// row's ⓘ. The labels live in internal/ui; the tools are declared here. Neither
// side can see the other, so this ledger pins the RELATIONSHIP: the set of
// names the registry and the mirror door can serve is exactly the set the label
// table names — a new tool without a label fails, and so does a label for a
// tool that no longer exists.

// agentToolDefFuncs are the definition functions agentTools calls, by name, as
// THIS test calls them (every boolean variant, so a name offered only on one
// side of a switch is still seen). TestEveryMusterToolHasAPlainLabel parses
// agentTools and fails when it calls a function this map does not name.
var agentToolDefFuncs = map[string]func() []agents.ToolDef{
	"agentReportDoneToolDef":       func() []agents.ToolDef { return []agents.ToolDef{agentReportDoneToolDef()} },
	"answerSessionQuestionToolDef": func() []agents.ToolDef { return []agents.ToolDef{answerSessionQuestionToolDef()} },
	"askOwnerToolDef":              func() []agents.ToolDef { return []agents.ToolDef{askOwnerToolDef()} },
	"attentionToolDefs": func() []agents.ToolDef {
		return append(attentionToolDefs(true), attentionToolDefs(false)...)
	},
	"chiefAttentionToolDefs":   chiefAttentionToolDefs,
	"connectorToolDefs":        connectorToolDefs,
	"endSessionToolDef":        func() []agents.ToolDef { return []agents.ToolDef{endSessionToolDef()} },
	"fleetReportToolDef":       func() []agents.ToolDef { return []agents.ToolDef{fleetReportToolDef()} },
	"flowToolDefs":             flowToolDefs,
	"focusToolDefs":            func() []agents.ToolDef { return append(focusToolDefs(true), focusToolDefs(false)...) },
	"gatedTaskToolDef":         func() []agents.ToolDef { return []agents.ToolDef{gatedTaskToolDef()} },
	"officerSpawnShareToolDef": func() []agents.ToolDef { return []agents.ToolDef{officerSpawnShareToolDef()} },
	"officerToolDefs":          officerToolDefs,
	"pageToolDefs":             pageToolDefs,
	"pluginMessageSettleToolDef": func() []agents.ToolDef {
		return []agents.ToolDef{pluginMessageSettleToolDef()}
	},
	"replyDraftToolDef":     func() []agents.ToolDef { return []agents.ToolDef{replyDraftToolDef()} },
	"reposListToolDef":      func() []agents.ToolDef { return []agents.ToolDef{reposListToolDef()} },
	"routeManyToolDef":      func() []agents.ToolDef { return []agents.ToolDef{routeManyToolDef()} },
	"routeToolDef":          func() []agents.ToolDef { return []agents.ToolDef{routeToolDef()} },
	"routeToolDefInProcess": func() []agents.ToolDef { return []agents.ToolDef{routeToolDefInProcess()} },
	"routingRuleProposeToolDef": func() []agents.ToolDef {
		return []agents.ToolDef{routingRuleProposeToolDef()}
	},
	"sessionContextToolDef":    func() []agents.ToolDef { return []agents.ToolDef{sessionContextToolDef()} },
	"sessionsOverviewToolDef":  func() []agents.ToolDef { return []agents.ToolDef{sessionsOverviewToolDef()} },
	"successionProposeToolDef": func() []agents.ToolDef { return []agents.ToolDef{successionProposeToolDef()} },
	"taskRoutingToolDefs": func() []agents.ToolDef {
		return append(taskRoutingToolDefs(true), taskRoutingToolDefs(false)...)
	},
	"taskToolDefs": taskToolDefs,
}

// toolDefCalls is every definition function fn (in file) calls — a call whose
// name ends in ToolDef or ToolDefs (or ToolDefInProcess), plain or as a method
// of the server — and every tool registered by literal name through a call to
// the local helper fn("name", …).
func toolDefCalls(t *testing.T, file, fn string) (calls, literal []string) {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != fn {
			continue
		}
		found = true
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var name string
			switch f := c.Fun.(type) {
			case *ast.Ident:
				name = f.Name
			case *ast.SelectorExpr:
				name = f.Sel.Name
			}
			switch {
			case name == "fn" && len(c.Args) > 0:
				if lit, ok := c.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					s, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatal(err)
					}
					literal = append(literal, s)
				}
			case strings.HasSuffix(name, "ToolDef") || strings.HasSuffix(name, "ToolDefs") || strings.HasSuffix(name, "ToolDefInProcess"):
				calls = append(calls, name)
			}
			return true
		})
	}
	if !found {
		t.Fatalf("instrument check FAILED: no func %s in %s", fn, file)
	}
	slices.Sort(calls)
	return slices.Compact(calls), literal
}

// servedToolNames is every tool name muster's own doors can serve: the agent
// registry (agentTools), the mirror door (mirrorTools) and the MCP transport's
// own muster_help. ledgerErrs are the definition functions a door calls that
// no ledger here knows.
func servedToolNames(t *testing.T) (names []string, ledgerErrs []string) {
	t.Helper()
	seen := map[string]bool{}
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	for _, door := range []struct {
		file, fn string
		ledger   map[string]func() []agents.ToolDef
	}{
		{"agent.go", "agentTools", agentToolDefFuncs},
		{"mirror_mcp.go", "mirrorTools", mirrorToolDefFuncs},
	} {
		calls, literal := toolDefCalls(t, door.file, door.fn)
		if len(calls) < 10 {
			t.Fatalf("instrument check FAILED: %s calls %d definition functions as parsed", door.fn, len(calls))
		}
		for _, n := range literal {
			add(n)
		}
		for _, c := range calls {
			defs, ok := door.ledger[c]
			if !ok {
				ledgerErrs = append(ledgerErrs, door.fn+" calls "+c)
				continue
			}
			for _, d := range defs() {
				add(d.Name)
			}
		}
		for c := range door.ledger {
			if !slices.Contains(calls, c) {
				ledgerErrs = append(ledgerErrs, "the ledger names "+c+", which "+door.fn+" no longer calls")
			}
		}
	}
	add(mcpHelpTool)
	sort.Strings(names)
	return names, ledgerErrs
}

// TestEveryMusterToolHasAPlainLabel: every tool muster's doors can serve has a
// plain label (ui.MusterToolLabel) under each name a chat can carry it by —
// bare (a gateway chief), mcp__muster__… (Claude Code) and muster_… (opencode)
// — and the label table names no tool the doors cannot serve. Seam ledger:
// fails when the served set GROWS or SHRINKS without the table.
func TestEveryMusterToolHasAPlainLabel(t *testing.T) {
	names, ledgerErrs := servedToolNames(t)
	for _, e := range ledgerErrs {
		t.Errorf("tool-definition ledger: %s — update agentToolDefFuncs/mirrorToolDefFuncs", e)
	}
	// Positive control: the scan must see tools from every family it walks.
	for _, must := range []string{"page_create", "page_share_request", "attention_ask", "focus_search", "route_reply",
		"ask_owner", "agent_get_task", "route_to_session", "muster_help", "connectors_list", "agent_destroy"} {
		if !slices.Contains(names, must) {
			t.Errorf("instrument check FAILED: the served set lacks %s (%d names: %v)", must, len(names), names)
		}
	}
	if len(names) < 60 {
		t.Fatalf("instrument check FAILED: only %d served names: %v", len(names), names)
	}
	for _, n := range names {
		for _, as := range []string{n, "mcp__muster__" + n, "muster_" + n} {
			if l, ok := ui.MusterToolLabel(as, "", as == n); !ok || strings.TrimSpace(l) == "" {
				t.Errorf("muster tool %s has no plain label (as %q): add it to ui's musterToolLabels", n, as)
			}
		}
	}
	for _, n := range ui.MusterToolNames() {
		if !slices.Contains(names, n) {
			t.Errorf("ui's musterToolLabels names %s, which no door serves: remove it", n)
		}
	}
}

// TestEveryDecisionAToolCanTakeHasItsOwnLabel: a served tool whose arguments
// carry a decision enum ("approve", "deny", "reject") is labelled by what each
// decision did (ui's musterToolDecisions) — never by one neutral label for
// all, and never as the owner's decision. Seam ledger over the definitions.
func TestEveryDecisionAToolCanTakeHasItsOwnLabel(t *testing.T) {
	seen := 0
	for _, ledger := range []map[string]func() []agents.ToolDef{agentToolDefFuncs, mirrorToolDefFuncs} {
		for _, defs := range ledger {
			for _, d := range defs() {
				props, _ := d.Parameters["properties"].(map[string]any)
				dec, _ := props["decision"].(map[string]any)
				enum, _ := dec["enum"].([]string)
				if len(enum) == 0 {
					continue
				}
				seen++
				neutral, _ := ui.MusterToolLabel(d.Name, "", true)
				labels := map[string]string{}
				for _, v := range enum {
					l, _ := ui.MusterToolLabel(d.Name, `{"decision":"`+v+`"}`, true)
					if l == neutral || labels[l] != "" {
						t.Errorf("%s: decision %q is labelled %q, not by what it did", d.Name, v, l)
					}
					labels[l] = v
				}
			}
		}
	}
	if seen < 3 {
		t.Fatalf("instrument check FAILED: %d tools with a decision enum", seen)
	}
}
