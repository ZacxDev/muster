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
// A push title is a RouterNotification literal with a Title. The switch reaches
// a title only if its surface DECLARES the codename-first title
// (CodenameTitle, made by codenameTitle); goNotify then sends it for an owner
// who turned it on. A new push surface that names a mirrored session and
// forgets to declare it would silently keep its codename at the end, cut off
// on a phone — so every push title is on this ledger, one way or the other.

// codenameFirstSurfaces are the functions whose push title names a mirrored
// session, and so declare its codename-first title.
var codenameFirstSurfaces = map[string]string{
	"attention.go attentionNotification":         "every attention item's push: a mirrored session's agent question, a held question, a chief notice, a waiting window's fixed title",
	"session_push.go Server.episodeNotification": "a stalled / waiting session's push: \"… needs you\", \"… is stalled\" (and its ask's, through attentionNotification)",
}

// noCodenameFirst is every OTHER function with a push title, with why the
// switch does not apply to it.
var noCodenameFirst = map[string]string{
	"agent_outcome.go Server.pushOutcome":                  "an agent's outcome: an agent has no tmux window",
	"chief_owner_release.go Server.applyGateRelease":       "a task's gate: names a task, not a session",
	"connectors.go Server.onConnectorRequested":            "a connector request: names a connector",
	"dispatch_hold_tools.go Server.pushSessionDispatch":    "its title names no session (\"Agent dispatched from your session\"); the body names it",
	"host_dispatch_done.go Server.announceHostOutcome":     "a host dispatch's agent window: names its runtime and host, not a named session",
	"officer_cap_hold.go Server.officerCapHoldNotice":      "an officer's pool: names an officer",
	"pages.go Server.pageResponded":                        "a shared page's response: names a page",
	"plugin_drafts.go Server.notifyDraftRefused":           "a plugin draft: fixed title, names no session",
	"push_task.go Server.flushCreatedTasks":                "a task: names a task",
	"push_task.go Server.notifyAgentRunning":               "a task's agent: an agent has no tmux window",
	"push_task.go Server.notifyTaskDone":                   "a task done / in review: names a task",
	"push_task.go Server.flushCommentedTasks":              "a task comment: names a task",
	"reply_draft_tool.go Server.notifyDraftAwaitingNoting": "a reply draft: fixed title, names no session",
	"session_route_notify.go Server.notifyChiefRouted":     "already names the window by its place, codename first (\"Chief routed it to mango:1 · tmux main:1 @ ws\"), never by a human name",
	"succession.go Server.armSuccessionPush":               "names the host only (\"Your new session started on atlas\"), never a session name",
}

// TestEveryPushTitleIsClassifiedForCodenameFirst pins the seam: (1) in this
// package, the functions that build a push title (a RouterNotification
// literal with a Title) are exactly codenameFirstSurfaces ∪ noCodenameFirst,
// each with a reason and none in both — it fails when the set GROWS or
// SHRINKS; (2) exactly the codenameFirstSurfaces set CodenameTitle, and only
// through the one rule (codenameTitle, whose callers are pinned too); (3)
// exactly goNotify reads the switch. Positive controls prove each walk sees
// what it counts.
func TestEveryPushTitleIsClassifiedForCodenameFirst(t *testing.T) {
	titled, writers := map[string]bool{}, map[string]bool{}
	rule, reads := map[string]bool{}, map[string]bool{}
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
			name := path + " " + nameLedgerFunc(fn)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CompositeLit:
					if id, ok := x.Type.(*ast.Ident); ok && id.Name == "RouterNotification" {
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
						if sel, ok := l.(*ast.SelectorExpr); ok && (sel.Sel.Name == "CodenameTitle" || sel.Sel.Name == "CodenameKeep") {
							writers[name] = true
						}
					}
				case *ast.CallExpr:
					switch c := x.Fun.(type) {
					case *ast.Ident:
						if c.Name == "codenameTitle" {
							rule[name] = true
						}
					case *ast.SelectorExpr:
						if c.Sel.Name == "notifyCodenameFirstOn" {
							reads[name] = true
						}
					}
				}
				return true
			})
		}
	}

	if !titled["attention.go attentionNotification"] {
		t.Fatalf("positive control FAILED: the walk did not see attentionNotification build a push title (found %v)", nameLedgerKeys(titled))
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
		t.Errorf("functions that build a push title:\n  got  %v\n  want %v\nA push title that names a mirrored session declares its codename-first title (codenameTitle) and goes in codenameFirstSurfaces; any other goes in noCodenameFirst with why.", got, want)
	}

	// The writers: the surfaces, and the sink's swap (goNotify assigns
	// n.Title FROM it, which is a read; it writes neither field).
	if got, want := nameLedgerKeys(writers), nameLedgerKeys(asSet(codenameFirstSurfaces)); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("functions that set CodenameTitle / CodenameKeep:\n  got  %v\n  want %v", got, want)
	}
	// The rule's callers: the attention items' kinds, and the episode's.
	if got, want := nameLedgerKeys(rule), []string{"attention.go attentionCodenameTitle", "session_push.go Server.episodeNotification"}; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("functions that call codenameTitle:\n  got  %v\n  want %v\nOnly the declared surfaces apply the rule.", got, want)
	}
	// One read of the switch, at the one sink.
	if got, want := nameLedgerKeys(reads), []string{"server.go Server.goNotify"}; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("functions that read the codename-first switch:\n  got  %v\n  want %v\nOnly goNotify decides which title is sent.", got, want)
	}
}

func asSet(m map[string]string) map[string]bool {
	out := map[string]bool{}
	for k := range m {
		out[k] = true
	}
	return out
}
