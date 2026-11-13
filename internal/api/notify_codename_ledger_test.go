package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// "START NOTIFICATION TITLES WITH THE TMUX NAME" — THE SEAM BETWEEN THE SWITCH
// AND EVERY PUSH TITLE (names.go codenameTitle, server.go goNotify).
//
// A push title is a RouterNotification literal with a Title, or an assignment
// to the Title of a RouterNotification-typed value. The switch reaches
// a title only if its surface DECLARES the codename-first title
// (CodenameTitle, made by codenameTitle); goNotify then sends it for an owner
// who turned it on. A new push surface that names a mirrored session and
// forgets to declare it would silently keep its codename at the end, cut off
// on a phone — so every push title is on this ledger, one way or the other.

// codenameFirstSurfaces are the functions whose push title names a mirrored
// session, and so declare its codename-first title.
var codenameFirstSurfaces = map[string]string{
	"attention.go attentionNotification":                "every attention item's push: a mirrored session's agent question, a held question, a chief notice, a waiting window's fixed title",
	"session_push.go Server.episodeNotification":        "a stalled / waiting session's push: \"… needs you\", \"… is stalled\" (and its ask's, through attentionNotification)",
	"host_dispatch_done.go Server.announceHostOutcome":  "a host dispatch's agent window, when a host lists it (no front for an unlisted one): \"agents:3@atlas · Claude Code is waiting for you: …\"",
	"succession.go Server.armSuccessionPush":            "the successor's window, when a host lists it: \"plum:4@atlas · Your new session started\"",
	"session_route_notify.go Server.notifyChiefRouted":  "the target window: \"mango:1@laptop · Chief routed it here\"",
	"dispatch_hold_tools.go Server.pushSessionDispatch": "the dispatching session's window: \"mango:1@laptop · Agent dispatched from your session\"",
}

// noCodenameFirst is every OTHER function with a push title, with why the
// switch does not apply to it.
var noCodenameFirst = map[string]string{
	"server.go Server.goNotify":                            "the sink: it sends a declared CodenameTitle in place of Title (and plains the title), never declares one",
	"agent_outcome.go Server.pushOutcome":                  "an agent's outcome: an agent has no tmux window",
	"chief_owner_release.go Server.applyGateRelease":       "a task's gate: names a task, not a session",
	"connectors.go Server.onConnectorRequested":            "a connector request: names a connector",
	"officer_cap_hold.go Server.officerCapHoldNotice":      "an officer's pool: names an officer",
	"pages.go Server.pageResponded":                        "a shared page's response: names a page",
	"plugin_drafts.go Server.notifyDraftRefused":           "a plugin draft: fixed title, names no session",
	"plugin_pairing.go Server.pairRequested":               "a plugin pairing request: fixed title, names no session",
	"push_task.go Server.flushCreatedTasks":                "a task: names a task",
	"push_task.go Server.notifyAgentRunning":               "a task's agent: an agent has no tmux window",
	"push_task.go Server.notifyTaskDone":                   "a task done / in review: names a task",
	"push_task.go Server.flushCommentedTasks":              "a task comment: names a task",
	"reply_draft_tool.go Server.notifyDraftAwaitingNoting": "a reply draft: fixed title, names no session",
}

// TestEveryPushTitleIsClassifiedForCodenameFirst pins the seam: (1) in this
// package, the functions that build a push title — a RouterNotification
// literal with a Title, or a Title assigned on a RouterNotification-typed
// value (a literal, a parameter, a call to a function returning one) — are
// exactly codenameFirstSurfaces ∪ noCodenameFirst, each with a reason and none
// in both; it fails when the set GROWS or SHRINKS; (2) exactly the
// codenameFirstSurfaces set CodenameTitle; (3) a Title assigned on such a
// value is followed, in the same function, by an assignment of its
// CodenameTitle (set or cleared) — else a caller overriding a declared title
// (n := attentionNotification(it); n.Title = …) would have the sink send the
// stale codename-first one. The one exception is the sink itself (goNotify),
// which assigns Title FROM CodenameTitle. A positive control proves the walk
// sees what it counts.
func TestEveryPushTitleIsClassifiedForCodenameFirst(t *testing.T) {
	titled, writers := map[string]bool{}, map[string]bool{}
	type fileFn struct {
		path string
		fn   *ast.FuncDecl
	}
	var fns []fileFn
	// returnsRN: the package's functions whose results include a
	// RouterNotification (a call to one yields a value that may carry a
	// declared CodenameTitle).
	returnsRN := map[string]bool{}
	isRN := func(e ast.Expr) bool {
		if st, ok := e.(*ast.StarExpr); ok {
			e = st.X
		}
		id, ok := e.(*ast.Ident)
		return ok && id.Name == "RouterNotification"
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			fns = append(fns, fileFn{path, fn})
			if fn.Type.Results != nil {
				for _, r := range fn.Type.Results.List {
					if isRN(r.Type) {
						returnsRN[fn.Name.Name] = true
					}
				}
			}
		}
	}
	var unswapped []string
	for _, ff := range fns {
		fn := ff.fn
		name := ff.path + " " + nameLedgerFunc(fn)
		// The function's RouterNotification-typed variables, by their declaring
		// object (the parser's resolution), so two variables of one name in
		// different scopes are two variables.
		vars := map[*ast.Object]bool{}
		for _, list := range []*ast.FieldList{fn.Recv, fn.Type.Params} {
			if list == nil {
				continue
			}
			for _, p := range list.List {
				if isRN(p.Type) {
					for _, n := range p.Names {
						vars[n.Obj] = true
					}
				}
			}
		}
		rnValue := func(e ast.Expr) bool {
			switch x := e.(type) {
			case *ast.CompositeLit:
				return isRN(x.Type)
			case *ast.UnaryExpr:
				if c, ok := x.X.(*ast.CompositeLit); ok {
					return isRN(c.Type)
				}
			case *ast.CallExpr:
				switch f := x.Fun.(type) {
				case *ast.Ident:
					return returnsRN[f.Name]
				case *ast.SelectorExpr:
					return returnsRN[f.Sel.Name]
				}
			}
			return false
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.AssignStmt:
				if len(x.Lhs) == len(x.Rhs) {
					for i, l := range x.Lhs {
						if id, ok := l.(*ast.Ident); ok && id.Obj != nil && rnValue(x.Rhs[i]) {
							vars[id.Obj] = true
						}
					}
				}
			case *ast.ValueSpec:
				if x.Type != nil && isRN(x.Type) {
					for _, n := range x.Names {
						vars[n.Obj] = true
					}
				}
			}
			return true
		})
		// lastTitle / lastCodename: the position of each variable's last
		// Title and CodenameTitle assignment.
		lastTitle, lastCodename := map[*ast.Object]token.Pos{}, map[*ast.Object]token.Pos{}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CompositeLit:
				if isRN(x.Type) {
					for _, e := range x.Elts {
						if kv, ok := e.(*ast.KeyValueExpr); ok {
							if k, ok := kv.Key.(*ast.Ident); ok {
								switch k.Name {
								case "Title":
									titled[name] = true
								case "CodenameTitle", "CodenameKeep":
									writers[name] = true
								}
							}
						}
					}
				}
			case *ast.AssignStmt:
				for _, l := range x.Lhs {
					sel, ok := l.(*ast.SelectorExpr)
					if !ok {
						continue
					}
					id, isVar := sel.X.(*ast.Ident)
					switch sel.Sel.Name {
					case "CodenameTitle", "CodenameKeep":
						writers[name] = true
						if isVar && id.Obj != nil && sel.Sel.Name == "CodenameTitle" {
							lastCodename[id.Obj] = x.Pos()
						}
					case "Title":
						if isVar && id.Obj != nil && vars[id.Obj] {
							titled[name] = true
							lastTitle[id.Obj] = x.Pos()
						}
					}
				}
			}
			return true
		})
		if name == "server.go Server.goNotify" {
			continue // the sink: it assigns Title FROM CodenameTitle
		}
		for v, p := range lastTitle {
			if lastCodename[v] < p {
				unswapped = append(unswapped, name+" ("+v.Name+".Title)")
			}
		}
	}

	if !titled["attention.go attentionNotification"] || !titled["server.go Server.goNotify"] {
		t.Fatalf("positive control FAILED: the walk did not see attentionNotification's literal and goNotify's assignment build a push title (found %v)", nameLedgerKeys(titled))
	}
	classified := map[string]bool{}
	for f := range codenameFirstSurfaces {
		if _, both := noCodenameFirst[f]; both {
			t.Errorf("%s is classified both ways", f)
		}
		classified[f] = true
	}
	for f, why := range noCodenameFirst {
		if why == "" {
			t.Errorf("%s: no reason given", f)
		}
		classified[f] = true
	}
	if got, want := nameLedgerKeys(titled), nameLedgerKeys(classified); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("functions that build a push title:\n  got  %v\n  want %v\nA push title about a tmux window declares its codename-first title (codenameTitle / codenameLed) and goes in codenameFirstSurfaces; any other goes in noCodenameFirst with why.", got, want)
	}
	// The writers: the surfaces (goNotify assigns n.Title FROM it, which is a
	// read; it writes neither field).
	if got, want := nameLedgerKeys(writers), nameLedgerKeys(asSet(codenameFirstSurfaces)); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("functions that set CodenameTitle / CodenameKeep:\n  got  %v\n  want %v", got, want)
	}
	if len(unswapped) > 0 {
		t.Errorf("a push Title overridden with no CodenameTitle set (or cleared) after it — with the switch on the sink would send the stale codename-first title: %v", unswapped)
	}
}

func asSet(m map[string]string) map[string]bool {
	out := map[string]bool{}
	for k := range m {
		out[k] = true
	}
	return out
}
