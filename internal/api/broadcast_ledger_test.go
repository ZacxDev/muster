package api

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestEveryBroadcastCallSiteIsClassified is the LEDGER behind broadcast's
// header (accounts PR 3b): every place in this package that emits an SSE event
// — and the OWNER it names — is listed here, so a new site must say whose event
// it is, and a site that changes its owner source shows up in review.
//
// Each entry is "<enclosing func> → <callee>(<owner argument as written>)".
// The three owner shapes the header allows:
//
//	broadcast(ctx | r.Context(), …)  a request: the principal's user owns it
//	broadcastTo(n.OwnerID, …)        a row read with its owner (the reaper)
//	…(s.agentOwner(…), …)            an agent-named nudge: owner read by name
//
// plus the plumbing between them (publish, agentChangedFor). The set is
// compared BOTH ways: an unlisted site fails, and so does a listed site that is
// gone. There is no shape for "everyone": a site that broadcast to all users
// would have to add one, here, in a reviewed diff.
//
// ⚠ IT IS STRUCTURAL: it pins WHICH expression each site passes as the
// owner, not that the expression is right. The behaviour is the isolation
// walk's SSE leg (isolation_test.go) and internal/sse's owner tests.
func TestEveryBroadcastCallSiteIsClassified(t *testing.T) {
	want := []string{
		// requests — the principal's user owns the event.
		"(*Server).advanceTaskToInProgress → broadcast(ctx)",
		// An agent's outcome changed (agent_outcome.go): its page and row, and the
		// chief panel of the chief told about it.
		"(*Server).outcomeChanged → agentChangedFor(a.OwnerID)",
		// THE outcome note's writer (writeChiefNote), for a pod's outcome and a
		// host dispatch's: the chief's owner, which both callers check is the
		// agent's (or the row's) owner.
		"(*Server).writeChiefNote → broadcastTo(chief.OwnerID)",
		"(*Server).applyTaskStatus → broadcast(ctx)",
		"(*Server).broadcastTaskLink → broadcast(ctx)",
		"(*Server).decideConnectorGrant → broadcast(ctx)",
		"(*Server).dismissTask → broadcast(ctx)",
		"(*Server).dispatchAgentTool → broadcast(ctx)",
		"(*Server).dispatchAgentTool → broadcast(ctx)",
		// The all-agents task tools (attention_tools.go): ctx is the calling
		// agent's (or mirrored session's) owner.
		"(*Server).toolTaskComment → broadcast(ctx)",
		"(*Server).toolTaskSetStatus → broadcast(ctx)",
		// agent_destroy's task reopen (agent_destroy_tool.go): ctx is the
		// teardown's WithoutCancel copy of the mirror request's, so its principal
		// is the mirror token's owner, who owns the task.
		"(*Server).reopenTaskAfterDestroy → broadcast(ctx)",
		// A task route's answer, added as a comment (session_routes.go): ctx is
		// the route's owner (route_reply's mirror principal, or the sweep's
		// routePrincipal), who owns the task.
		"(*Server).relayTaskRoute → broadcast(ctx)",
		// The gate released on the owner's typed word (chief_owner_release.go):
		// ctx is task_dispatch's, the chief's owner, who owns the task.
		"(*Server).applyGateRelease → broadcast(ctx)",
		// The gate released on the owner's Release tap (answer_release.go): ctx
		// is the answer's — the owner's browser session or paired phone — and
		// the ask's owner owns the task.
		"(*Server).applyAnswerRelease → broadcast(ctx)",
		// A host dispatch's late spawn reply (host_dispatch_reconcile.go): pctx
		// is hostDispatchPrincipal(ctx, owner), the row's owner.
		"(*Server).applySpawnReply → broadcast(pctx)",
		// A host dispatch ended (host_dispatch.go): its owner, named explicitly
		// because the reconciler's context carries no principal.
		"(*Server).endHostDispatch → broadcastTo(owner)",
		// A host dispatch's outcome changed (host_dispatch_done.go: done by its
		// own session's agent_report_done, or done/waiting from its host's
		// listing): h.Owner is the row's owner, read under the mirror token's or
		// the hook's owner principal (row-level security), so it is that owner.
		"(*Server).announceHostOutcome → broadcastTo(h.Owner)",
		// A later turn re-armed a host dispatch (host_dispatch_outcome.go): the
		// row's owner, read under hostDispatchPrincipal (row-level security).
		"(*Server).rearmHostDispatch → broadcastTo(r.Owner)",
		// A host's window listing moved a host dispatch (host_dispatch_reconcile.go):
		// pctx is hostDispatchPrincipal(ctx, owner), the host's owner.
		"(*Server).reconcileListing → broadcast(pctx)",
		// A chief proposal changed (chief_proposal.go, succession.go), always
		// lineage.changed, which carries no text: filed (agentOwnerID(chief) is the chief's owner, whose
		// principal the tool call runs as), declined on the owner's own door
		// (a.Owner is the card's owner, checked by attention.IsOwnerDoor), its
		// successor reserved on that tap (requestOwner(ctx), the tap's
		// principal), and its successor's dispatch moved (owner, the row's,
		// named explicitly as endHostDispatch's and the reconciler's are).
		"(*Server).fileProposal → broadcastTo(agentOwnerID(chief))",
		"(*Server).proposalForAnswer → broadcastTo(a.Owner)",
		"(*Server).reserveSuccession → broadcastTo(owner)",
		"(*Server).successionDispatchMoved → broadcastTo(owner)",
		// An end_session tapped (end_session.go acceptEndSession) and settled
		// (settleEnd): owner is the tap's principal (requestOwner), carried to
		// the settling goroutine.
		"(*Server).acceptEndSession → broadcastTo(owner)",
		"(*Server).settleEnd → broadcastTo(owner)",
		"(*Server).handleAPITaskComment → broadcast(r.Context())",
		// The machine-path task create (moved out of handleAPITaskCreate so a
		// second door cannot skip a side effect); ctx is the request's. Its
		// side effects are taskCreatedEffects, which createTaskOnce (createTask,
		// the plugin task route) and a plugin task reference filed new share.
		"(*Server).taskCreatedEffects → broadcast(ctx)",
		// Plugin task REFERENCES (plugin_task_refs.go, task_ref_queue.go): a
		// snapshot applied or a reference detached (the plugin request's
		// principal is the plugin token's owner, authz.ViaPlugin), and a write
		// queued on one (the writer's request — the task's owner).
		"(*Server).applyReference → broadcast(ctx)",
		"(*Server).applyReference → broadcast(ctx)",
		"(*Server).queueIfReference → broadcast(ctx)",
		// The owner retried or dismissed a failed write on a reference
		// (task_ref_write_actions.go): ctx is the owner's request, and the task
		// was read under it first.
		"(*Server).actOnRefWrite → broadcast(ctx)",
		// A plugin claimed or settled queued writes: owner is the authenticated
		// plugin row's OwnerID, and the claim and the result reach only that
		// plugin's writes, on that owner's references.
		"(*Server).handlePluginTaskWritesClaim → broadcastTo(p.OwnerID)",
		"(*Server).handlePluginTaskWriteResult → broadcastTo(p.OwnerID)",
		// The plugin task route's comment on an existing task (plugin_tasks.go),
		// once on the create path (the task existed) and once on the comment-only
		// path: the request's principal is the plugin token's owner (authz.ViaPlugin).
		"(*Server).handlePluginTask → broadcast(ctx)",
		"(*Server).handlePluginTask → broadcast(ctx)",
		"(*Server).handleAPITaskEdit → broadcast(r.Context())",
		"(*Server).handleAgentBlocked → broadcast(r.Context())",
		// The pod's live work counts (agent_work.go): the relay agent's request.
		"(*Server).handleAgentWork → broadcast(r.Context())",
		"(*Server).handleAgentTaskComment → broadcast(r.Context())",
		"(*Server).handleAgentTaskStatus → broadcast(r.Context())",
		"(*Server).persistTurn → broadcast(ctx)",
		// A chat-socket turn that FAILED stores nothing, so it asks pages to
		// refetch: one opened mid-turn draws "working…" from the session's count and
		// would otherwise keep it (session_turns.go). ctx is the socket's request.
		"(*Server).handleAgentWS → broadcast(ctx)",
		// An answer delivery's turn the runtime took NOTHING of stores nothing,
		// so it asks pages to refetch (session_turns.go's "working…"). a is the
		// asking agent, read under the ask's owner (attention_delivery.go).
		"(*Server).runDeliveredTurn → broadcastTo(a.OwnerID)",
		"(*Server).handleConnectorCreate → broadcast(r.Context())",
		"(*Server).handleConnectorCreate → broadcast(r.Context())",
		"(*Server).handleConnectorDelete → broadcast(r.Context())",
		"(*Server).handleConnectorRevoke → broadcast(r.Context())",
		"(*Server).handleConnectorRotate → broadcast(r.Context())",
		// Shared approvals (connector_standing.go, 0043): an automatic approval
		// runs under the requesting agent's owner (the agent routes, the proxy)
		// or the deciding owner (an approval's sweep) — the grant's owner either
		// way; the revoke is the tab's request.
		"(*Server).approveFromStanding → broadcast(ctx)",
		"(*Server).handleStandingGrantRevoke → broadcast(ctx)",
		"(*Server).handleNoteEdit → broadcast(r.Context())",
		"(*Server).handleNoteTagAdd → broadcast(r.Context())",
		"(*Server).handleNoteTagRemove → broadcast(r.Context())",
		"(*Server).handleTaskMerge → broadcast(r.Context())",
		"(*Server).handleTaskMerge → broadcast(r.Context())",
		"(*Server).onConnectorRequested → broadcast(ctx)",
		"(*Server).softDeleteTaskComment → broadcast(ctx)",
		"(*Server).createAndDispatchAgent → agentChangedFor(requestOwner(ctx))",
		// a row read with its owner.
		"(*Server).reapIdleTasks → broadcastTo(n.OwnerID)",
		// The shared destroy's agent.changed (both doors): a is the agent row
		// the chief check read under the caller's principal, so its owner is the
		// caller's own.
		"(*Server).destroyAgent → broadcastTo(a.OwnerID)",
		// A completed task's stop of the chief's agent (task_complete_stop.go):
		// a is the agent row read under the status writer's principal and checked
		// against the task's owner (a.OwnerID == n.OwnerID), so it is theirs.
		"(*Server).stopChiefAgentOnComplete → agentChangedFor(a.OwnerID)",
		// The Account page's chief switch (chief_toggle.go): owner is the
		// request's own principal, whose switch and chief it is.
		"(*Server).handleAccountChief → agentChangedFor(owner)",
		// …and once more on the Off whose Stop failed, before it says so.
		"(*Server).handleAccountChief → agentChangedFor(owner)",
		// agent-named nudges: the owner is read from the agent row by name.
		"(*Server).BroadcastAgentChanged → agentChangedFor(s.agentOwner(agentName))",
		// the mirror plugin's host nudge: the hub names the host's owner.
		"(*Server).BroadcastAgentsChangedFor → broadcastTo(owner)",
		// the attention queue (attention.go): every change names its owner — the
		// ask's row, the hub nudge's owner, or the request's principal.
		"(*Server).attentionChanged → broadcastTo(owner)",
		// A plugin's claim or result moved a reply draft's delivery status
		// (plugin_drafts.go): owner is the authenticated plugin row's OwnerID —
		// the plugin door confines both to that owner's drafts — so the Asks
		// page's Archived view re-reads it.
		"(*Server).draftStatusMoved → broadcastTo(owner)",
		// plumbing.
		"(*Server).agentChangedFor → broadcastTo(owner)",
		"(*Server).broadcast → broadcastTo(requestOwner(ctx))",
		"(*Server).broadcastTo → publish(sse.Event{Name: name, Data: string(data), OwnerID: owner})",
		"(*Server).publish → bus.Broadcast(ev)",
	}
	got := broadcastSites(t)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("SSE emit sites changed.\n got:\n  %s\nwant:\n  %s\n"+
			"Each site must name WHOSE event it is (see broadcast's header); add or correct the entry.",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	// Positive control on the scanner: it found the request sites at all.
	if len(got) < 30 {
		t.Fatalf("scanner found only %d sites; it is not reading the package", len(got))
	}
}

// broadcastSites scans this package's non-test source for every call to an
// emit function and renders it as a ledger entry.
func broadcastSites(t *testing.T) []string {
	t.Helper()
	emit := map[string]bool{"broadcast": true, "broadcastTo": true, "publish": true, "agentChangedFor": true}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var out []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			name := fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) == 1 {
				if star, ok := fn.Recv.List[0].Type.(*ast.StarExpr); ok {
					if id, ok := star.X.(*ast.Ident); ok {
						name = "(*" + id.Name + ")." + name
					}
				}
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				callee := ""
				switch x := sel.X.(type) {
				case *ast.Ident:
					if x.Name == "s" && emit[sel.Sel.Name] {
						callee = sel.Sel.Name
					}
				case *ast.SelectorExpr:
					if id, ok := x.X.(*ast.Ident); ok && id.Name == "s" && x.Sel.Name == "bus" && sel.Sel.Name == "Broadcast" {
						callee = "bus.Broadcast"
					}
				}
				if callee == "" || len(call.Args) == 0 {
					return true
				}
				var b bytes.Buffer
				_ = printer.Fprint(&b, fset, call.Args[0])
				out = append(out, name+" → "+callee+"("+b.String()+")")
				return true
			})
		}
	}
	sort.Strings(out)
	return out
}
