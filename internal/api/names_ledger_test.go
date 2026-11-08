package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// sessionNameAdapters are names.go's doors to the one name rule
// (internal/sessionname): the only functions that may call sessionname.Agent
// or sessionname.Mirror.
var sessionNameAdapters = []string{
	"names.go focusIndex.agentName",
	"names.go focusIndex.mirrorName",
	"names.go focusIndex.storedMirrorName",
	"names.go Server.agentName",
	"names.go Server.mirrorName",
}

// sessionNameCallers is EVERY function in this package that names a session
// through an adapter — each a surface that shows or speaks a session's name.
//
// 🔴 A LEDGER, NOT A SPOT CHECK: it fails when the set GROWS (a new surface
// is a decision: add it here, with what it is) or SHRINKS (a surface that
// stopped asking the rule now names sessions some other way, or not at all).
var sessionNameCallers = []string{
	"agents.go Server.activeNotificationRows", // the notifications panel's active rows
	"agents.go Server.cardViewIndexed",        // the sidebar rows, switcher pills and palette (agents)
	"agent_outcome.go Server.agentLabel",      // an agent outcome's push title and its note to the chief
	"agents.go Server.handleAgentDetail",      // the agent page's header and <title>
	"agents.go Server.handleAgentName",        // a Save of the unchanged prefilled name is not a rename
	"agents.go Server.handleNotifications",    // the notifications panel's unread rows
	"attention.go Server.withNamesFrom",       // every attention item's name: the Asks list, pushes, attention_list, the summary
	"chat_log_live.go Server.dispatchCardFor",
	"chief_nudge.go Server.stallFacts",                           // the chief's stall prompt (and a notice's stored "agent <name>")                    // a dispatch card in the chief's chat
	"connector_decide_tools.go Server.toolConnectorRequestsList", // connector requests the chief decides
	"fleet_report.go Server.collectFleet",                        // the fleet report
	"focus_tools.go Server.toolFleetList",                        // the chief's fleet_list
	"focus_tools.go Server.toolFocusSearch",                      // the chief's focus_search hits
	"mirror.go Server.handleMirrorTerminalPage",                  // a mirrored terminal page's header and <title>
	"mirror.go Server.mirrorCards",                               // the sidebar rows, switcher pills and palette (mirrored)
	"mirror_mcp.go Server.handleMirrorMCP",                       // a mirrored session's ask label
	"mirror_view.go Server.handleMirrorViewPage",                 // the Rendered view's header and <title>
	"plugin_message_route_tools.go Server.routeSubjectFor",       // the route candidates the chief is offered
	"pr_routes.go Server.prAuthorHint",                           // a plugin message's PR author hint
	"session_names.go Server.mirrorNameUnchanged",                // a Save of the unchanged prefilled name is not a rename
	"session_push.go Server.episodeWho",                          // a stalled / waiting session's push title
	"session_routes.go Server.resolveRouteTarget",                // route_to_session's text
	"sessions_overview.go Server.repoHints",                      // a hint naming a session by its repo (#500)
	"sessions_overview.go Server.liveSessions",                   // the chief's sessions_overview and the session hints
	"task_actions.go Server.chiefTaskActions",                    // task_forward's label
	"task_routing.go Server.handTaskToChief",                     // a task event's agents (plugins)
	"terminal.go Server.handleAgentTerminalPage",                 // an agent's terminal page's header and <title>
}

// tmuxAddressSurfaces are the ledgered surfaces that name a mirrored session
// in a NOTIFICATION — a push, the Asks list, the attention summary — and so
// call notifyName, the one place "Show tmux session in notifications" adds the
// window's tmux address (notify_prefs.go).
var tmuxAddressSurfaces = []string{
	"attention.go Server.withNamesFrom", // every attention item's Name: its push title, the Asks list, the summary (and attention_list)
	"session_push.go Server.episodeWho", // a stalled / waiting session's push title and its "Stalled / waiting" row
}

// noTmuxAddress is every OTHER ledgered surface, with why the preference does
// not apply to it.
//
// 🔴 TOGETHER WITH tmuxAddressSurfaces IT IS THE WHOLE LEDGER: a surface added
// to sessionNameCallers must be put in one or the other, so a new notification
// surface is a decision, never a quiet bypass of the preference.
var noTmuxAddress = map[string]string{
	"agents.go Server.activeNotificationRows":                    "notifications panel: agents only (no tmux window)",
	"agents.go Server.handleNotifications":                       "notifications panel: agents only (no tmux window)",
	"agent_outcome.go Server.agentLabel":                         "an agent outcome's push: an agent (no tmux window)",
	"agents.go Server.cardViewIndexed":                           "navigation, not a notification",
	"agents.go Server.handleAgentDetail":                         "a page header, not a notification",
	"agents.go Server.handleAgentName":                           "the rename form",
	"chat_log_live.go Server.dispatchCardFor":                    "a chat card, not a notification",
	"chief_nudge.go Server.stallFacts":                           "the chief's prompt (it is given the place itself)",
	"connector_decide_tools.go Server.toolConnectorRequestsList": "a chief tool",
	"fleet_report.go Server.collectFleet":                        "a report, not a notification",
	"focus_tools.go Server.toolFleetList":                        "a chief tool",
	"focus_tools.go Server.toolFocusSearch":                      "a chief tool",
	"mirror.go Server.handleMirrorTerminalPage":                  "a page header, not a notification",
	"mirror.go Server.mirrorCards":                               "navigation, not a notification",
	"mirror_mcp.go Server.handleMirrorMCP":                       "an ask's STORED label: shown only when no window holds the session, so there is no address to vouch for",
	"mirror_view.go Server.handleMirrorViewPage":                 "a page header, not a notification",
	"plugin_message_route_tools.go Server.routeSubjectFor":       "a chief tool",
	"pr_routes.go Server.prAuthorHint":                           "a plugin message hint",
	"session_names.go Server.mirrorNameUnchanged":                "the rename form",
	"session_routes.go Server.resolveRouteTarget":                "a chief tool",
	"sessions_overview.go Server.repoHints":                      "a chief hint",
	"sessions_overview.go Server.liveSessions":                   "a chief tool",
	"task_actions.go Server.chiefTaskActions":                    "a chief tool",
	"task_routing.go Server.handTaskToChief":                     "a task event for plugins",
	"terminal.go Server.handleAgentTerminalPage":                 "a page header, not a notification",
}

// nameAdapterSelectors are names.go's functions a surface calls to name a
// session: the adapters and the helpers built on them.
var nameAdapterSelectors = map[string]bool{"agentName": true, "mirrorName": true, "storedMirrorName": true,
	"subjectName": true, "subjectNameOf": true, "humanName": true, "mirrorLabel": true,
	"humanMirrorName": true}

// TestTheSessionNameCallerLedger pins the SEAM between the name rule and the
// surfaces: (1) in the whole module, only names.go's adapters call
// sessionname.Agent / sessionname.Mirror; (2) in this package, exactly the
// ledgered functions call an adapter. A positive control proves the walk can
// see a call: names.go's own adapters ARE found calling the rule.
func TestTheSessionNameCallerLedger(t *testing.T) {
	ruleCallers := map[string]bool{}
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", ".git", "e2e", "web":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		for _, fn := range nameLedgerCallers(t, path, func(c *ast.CallExpr) bool {
			sel, ok := c.Fun.(*ast.SelectorExpr)
			if !ok {
				return false
			}
			id, ok := sel.X.(*ast.Ident)
			return ok && id.Name == "sessionname" && (sel.Sel.Name == "Agent" || sel.Sel.Name == "Mirror")
		}) {
			rel, _ := filepath.Rel(root, path)
			if filepath.Dir(rel) == filepath.Join("internal", "api") {
				fn = filepath.Base(rel) + " " + fn
			} else {
				fn = rel + " " + fn
			}
			ruleCallers[fn] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ruleCallers["names.go Server.agentName"] {
		t.Fatalf("positive control FAILED: the walk did not find names.go's adapter calling the rule (found %v)", nameLedgerKeys(ruleCallers))
	}
	if got, want := nameLedgerKeys(ruleCallers), nameLedgerSorted(sessionNameAdapters); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("callers of sessionname.Agent / sessionname.Mirror:\n  got  %v\n  want %v\nOnly names.go's adapters may apply the rule; a surface asks an adapter.", got, want)
	}

	adapterCallers := map[string]bool{}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") || path == "names.go" {
			continue
		}
		for _, fn := range nameLedgerCallers(t, path, func(c *ast.CallExpr) bool {
			sel, ok := c.Fun.(*ast.SelectorExpr)
			return ok && nameAdapterSelectors[sel.Sel.Name]
		}) {
			adapterCallers[path+" "+fn] = true
		}
	}
	if got, want := nameLedgerKeys(adapterCallers), nameLedgerSorted(sessionNameCallers); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("functions that name a session through an adapter:\n  got  %v\n  want %v\nA new surface goes on the ledger with what it is; a dropped one is removed with why.", got, want)
	}
}

// TestEveryNotificationSurfaceAsksForTheTmuxAddress pins the seam between
// "Show tmux session in notifications" and the surfaces: (1) every function on
// the name ledger is classified — a tmuxAddressSurfaces entry or a
// noTmuxAddress one with its reason, never both; (2) in this package, exactly
// the tmuxAddressSurfaces call notifyName. A positive control proves the walk
// can see a call: withNamesFrom IS found calling it.
func TestEveryNotificationSurfaceAsksForTheTmuxAddress(t *testing.T) {
	classified := map[string]bool{}
	for _, f := range tmuxAddressSurfaces {
		if _, both := noTmuxAddress[f]; both {
			t.Errorf("%s is both a tmux-address surface and not one", f)
		}
		classified[f] = true
	}
	for f, why := range noTmuxAddress {
		if why == "" {
			t.Errorf("%s: no reason given for leaving the address out", f)
		}
		classified[f] = true
	}
	if got, want := nameLedgerKeys(classified), nameLedgerSorted(sessionNameCallers); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("classified surfaces:\n  got  %v\n  want %v\nEvery surface on the name ledger goes in tmuxAddressSurfaces (it names a session in a notification: call notifyName) or noTmuxAddress (with why not).", got, want)
	}

	callers := map[string]bool{}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	// names.go is walked too: a name helper there that called notifyName
	// would hand the address to every surface that calls the helper, unledgered.
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		for _, fn := range nameLedgerCallers(t, path, func(c *ast.CallExpr) bool {
			id, ok := c.Fun.(*ast.Ident)
			return ok && id.Name == "notifyName"
		}) {
			callers[path+" "+fn] = true
		}
	}
	if !callers["attention.go Server.withNamesFrom"] {
		t.Fatalf("positive control FAILED: the walk did not find withNamesFrom calling notifyName (found %v)", nameLedgerKeys(callers))
	}
	if got, want := nameLedgerKeys(callers), nameLedgerSorted(tmuxAddressSurfaces); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("functions that call notifyName:\n  got  %v\n  want %v\nA notification surface names a mirrored session through notifyName, and is a tmuxAddressSurfaces entry.", got, want)
	}
}

// callersOf is the functions of the file at path ("Recv.Name" or "Name")
// containing a call match accepts.
func nameLedgerCallers(t *testing.T, path string, match func(*ast.CallExpr) bool) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		found := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok && match(c) {
				found = true
			}
			return !found
		})
		if found {
			out = append(out, nameLedgerFunc(fn))
		}
	}
	return out
}

func nameLedgerFunc(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	t := fn.Recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}

func nameLedgerKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func nameLedgerSorted(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}
