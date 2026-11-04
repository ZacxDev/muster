package api

import (
	"go/ast"
	"go/token"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// THE REFERENCE-QUEUE LEDGER (task_ref_queue.go).
//
// 🔴 THE HAZARD. A status or comment written LOCALLY on a plugin task reference
// silently diverges from the upstream — the drift references exist to end. The
// rule that prevents it is one function, queueIfReference, and it only works if
// EVERY writer reaches it. The status half is pinned by the status-writer ledger
// (every status write goes through setNoteStatus, which asks the queue first —
// pinned below). This file pins the comment half and the wiring between them:
//
//   - the store's comment writers (AddComment, AddCommentOnce) are called from
//     the two funnels ONLY, and the other direct writers of a thread
//     (CreateOnceByTag's create-with-comment, FlagIdle) are a closed, named set;
//   - every caller of a funnel passes an origin CONSTANT, and the set of callers
//     with their origins is ledgered — growing OR shrinking fails;
//   - queueIfReference has exactly the callers that make the rule one rule, and
//     the store's EnqueueRefWrite is called from it alone;
//   - setNoteStatus asks queueStatusIfReference BEFORE it calls the store.
//
// ⚠ WHAT THIS DOES NOT CHECK, so nobody over-reads it: that each writer passes
// the RIGHT origin. `commentByMuster` on a person's comment would sail through
// and write that comment locally on a reference. The ledger is spelled with the
// origin beside the function so the review that catches that has somewhere to
// happen, and TestEveryWriterOnAReferenceIsQueuedNotStored drives the actor
// writers for real.
// ---------------------------------------------------------------------------

// expectedCommentWriterLedger is every function that writes a comment through
// a funnel (addTaskComment / addTaskCommentOnce), and the origin each passes.
var expectedCommentWriterLedger = map[string][]string{
	// A person or agent — queued on a reference:
	"handleAgentTaskComment": {"commentByActor"}, // POST /agent/task/comment
	"dispatchAgentTool":      {"commentByActor"}, // the native agent_comment_task tool
	"toolTaskComment":        {"commentByActor"}, // the all-agents task_comment tool (MCP)
	"handleAPITaskComment":   {"commentByActor"}, // POST /api/tasks/{id}/comments (CLI, producers)
	"handleNoteComment":      {"commentByActor"}, // POST /tasks/{id}/comments (the owner)
	"relayTaskRoute":         {"commentByActor"}, // a task route's answer
	// The plugin relaying its upstream — local:
	"handlePluginTask": {"commentByPlugin"},
	// Muster's own bookkeeping — local, rendered as muster-only history:
	"applyGateRelease": {"commentByMuster"},
	"handleTaskMerge":  {"commentByMuster", "commentByMuster"},
}

// expectedDirectThreadWriters is every function that calls a store method
// writing into a task's thread, and the method. The funnels are two of them;
// the rest write muster's or the plugin's own text and never a person's.
var expectedDirectThreadWriters = map[string][]string{
	"addTaskComment":     {"AddComment"},
	"addTaskCommentOnce": {"AddCommentOnce"},
	// The plugin's create-with-comment: the relay comment, in the create's
	// transaction (copy mode; a reference has no relay comment).
	"createTaskOnce": {"CreateOnceByTag"},
	// The idle reaper's `stale` note (muster's).
	"reapIdleTasks": {"FlagIdle"},
}

// threadWriteMethods are the notes.Store methods that write a comment.
var threadWriteMethods = map[string]bool{"AddComment": true, "AddCommentOnce": true, "CreateOnceByTag": true, "FlagIdle": true}

func TestEveryCommentWriterGoesThroughTheReferenceQueue(t *testing.T) {
	fset := token.NewFileSet()
	files := parsePackageSources(t, fset)
	if len(files) == 0 {
		t.Fatal("positive control FAILED: no non-test sources parsed")
	}

	// Pass 1: direct store thread writers. The receiver must be the server's
	// store; l.Store is liveNotes delegating to the store it wraps, reached
	// THROUGH one of these, so it is not a writer of its own.
	direct := map[string][]string{}
	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || !threadWriteMethods[sel.Sel.Name] {
					return true
				}
				recv := exprText(sel.X)
				if recv == "l.Store" {
					return true
				}
				if recv != "s.ext.Notes" {
					t.Errorf("%s calls %s.%s at %s: a comment written past the reference queue",
						fn.Name.Name, recv, sel.Sel.Name, fset.Position(call.Pos()))
					return true
				}
				direct[fn.Name.Name] = append(direct[fn.Name.Name], sel.Sel.Name)
				return true
			})
		}
	}
	if len(direct["addTaskComment"]) == 0 {
		t.Fatal("positive control FAILED: the scan did not find addTaskComment's AddComment; it is not measuring")
	}
	compareLedger(t, "direct thread writer", direct, expectedDirectThreadWriters)

	// Pass 2: the funnels' callers and the origin constant each passes.
	origins := map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			gen, ok := d.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, sp := range gen.Specs {
				vs, ok := sp.(*ast.ValueSpec)
				if ok && vs.Type != nil && exprText(vs.Type) == "commentOrigin" {
					for _, n := range vs.Names {
						origins[n.Name] = true
					}
				}
			}
		}
	}
	if len(origins) != 3 {
		t.Fatalf("positive control: want the 3 commentOrigin constants, found %v", origins)
	}
	got := map[string][]string{}
	for _, f := range files {
		for _, funnel := range []string{"addTaskComment", "addTaskCommentOnce"} {
			forEachCallTo(f, funnel, func(enclosing string, call *ast.CallExpr) {
				if len(call.Args) < 2 {
					t.Errorf("%s calls %s with %d argument(s)", enclosing, funnel, len(call.Args))
					return
				}
				id, ok := call.Args[1].(*ast.Ident)
				if !ok || !origins[id.Name] {
					t.Errorf("%s calls %s with origin %s, not a commentOrigin constant", enclosing, funnel, exprText(call.Args[1]))
					return
				}
				got[enclosing] = append(got[enclosing], id.Name)
			})
		}
	}
	compareLedger(t, "comment writer", got, expectedCommentWriterLedger)

	// Pass 3: the decision's own wiring.
	callers := func(name string) []string {
		var out []string
		for _, f := range files {
			forEachCallTo(f, name, func(enclosing string, _ *ast.CallExpr) { out = append(out, enclosing) })
		}
		sort.Strings(out)
		return out
	}
	if got, want := strings.Join(callers("queueIfReference"), ","),
		"addTaskComment,addTaskCommentOnce,queueStatusIfReference"; got != want {
		t.Errorf("queueIfReference is called from %s, want exactly %s", got, want)
	}
	if got, want := strings.Join(callers("EnqueueRefWrite"), ","), "queueIfReference"; got != want {
		t.Errorf("the store's EnqueueRefWrite is called from %s, want only %s (the one decision)", got, want)
	}
	if got, want := strings.Join(callers("queueStatusIfReference"), ","), "setNoteStatus"; got != want {
		t.Errorf("queueStatusIfReference is called from %s, want only %s", got, want)
	}

	// setNoteStatus must ask the queue BEFORE the store write.
	var queueAt, storeAt token.Pos
	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "setNoteStatus" {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					switch {
					case sel.Sel.Name == "queueStatusIfReference" && queueAt == 0:
						queueAt = call.Pos()
					case sel.Sel.Name == "SetStatus" && exprText(sel.X) == "s.ext.Notes" && storeAt == 0:
						storeAt = call.Pos()
					}
				}
				return true
			})
		}
	}
	if queueAt == 0 || storeAt == 0 || queueAt > storeAt {
		t.Errorf("setNoteStatus must call queueStatusIfReference before s.ext.Notes.SetStatus (queue at %v, store at %v)", queueAt, storeAt)
	}
	t.Logf("comment-writer ledger: %d funnel caller(s), %d direct thread writer(s)", len(got), len(direct))
}

// compareLedger fails when got and want differ in either direction.
func compareLedger(t *testing.T, what string, got, want map[string][]string) {
	t.Helper()
	for k := range got {
		sort.Strings(got[k])
	}
	for fn, w := range want {
		ww := append([]string(nil), w...)
		sort.Strings(ww)
		g, ok := got[fn]
		if !ok {
			t.Errorf("%s %s is on the ledger as %v but no longer writes", what, fn, w)
			continue
		}
		if strings.Join(g, ",") != strings.Join(ww, ",") {
			t.Errorf("%s %s writes as %v, ledgered as %v", what, fn, g, ww)
		}
	}
	for fn, g := range got {
		if _, ok := want[fn]; !ok {
			t.Errorf("%s %s writes as %v and is NOT on the ledger: route a person's or agent's comment "+
				"through addTaskComment(…, commentByActor, …) so a reference queues it, and ledger it here", what, fn, g)
		}
	}
}
