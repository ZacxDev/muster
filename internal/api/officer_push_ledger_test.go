package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// THE SEAM BETWEEN officerPushes AND EVERY PUSH (officer_push.go).
//
// officerPushes is only as good as the call sites that ask it. A new push
// path an officer can reach — an MCP tool every agent has, a plugin door, a
// sweep — would push for an officer without ever reading the table, and no
// test of the table would notice. So every call in this package of a push
// entry point (pushEntryPoints) is on this ledger, by its calling function,
// one of two ways:
//
//   - officerRouted: the call is on an officer path, classified in
//     officerPushes; the function must reach the table (call
//     officerPushSends or officerAskFiled, or be attentionNotification's
//     own gate) — checked structurally below;
//   - notOfficerRouted: why the table does not apply — no officer can reach
//     it, or it is a path every agent shares and the push is a decision as
//     it stands (officer_push.go's ⚠ list).
//
// The set is asserted both ways: a call site added anywhere in the package
// (say a notify_owner tool) fails until it is classified here, and one that
// is removed fails until its row goes.

// pushEntryPoints are the functions a push leaves through: goNotify (the one
// sink) and the package's wrappers that end in it.
var pushEntryPoints = map[string]bool{
	"goNotify": true, "pushTask": true, "attentionNotify": true, "askFiled": true,
	"proposalCardFiled": true, "repushAsk": true,
}

// officerRouted: call sites on an officer path, by the officerPush row(s)
// they are.
var officerRouted = map[string]string{
	"officer_push.go Server.officerAskFiled -> askFiled":                 "ask, escalation, unclaimed (its caller names the path)",
	"outreach_batch.go Server.notifyBatchAwaiting -> goNotify":           string(officerPushOutreachBatch),
	"reply_draft_tool.go Server.notifyDraftAwaitingNoting -> goNotify":   string(officerPushDraft),
	"plugin_drafts.go Server.notifyDraftRefused -> goNotify":             string(officerPushDraftRefused),
	"plugin_draft_retry.go Server.draftRetryScheduled -> goNotify":       string(officerPushDraftRetrying),
	"plugin_draft_retry.go Server.notifyRetryChainEnded -> goNotify":     string(officerPushDraftRetryEnded),
	"officer_cap_hold.go Server.officerBudgetNotice -> goNotify":         string(officerPushBudget),
	"officer_cap_hold.go Server.officerCapHoldNotice -> goNotify":        string(officerPushCapHold),
	"officer_autorestart.go Server.officerAutoRestartNotice -> goNotify": string(officerPushRestartHeld),
	"connectors.go Server.onConnectorRequested -> goNotify":              string(officerPushConnectorRequest),
	"page_tools.go Server.pagePublished -> pushTask":                     string(officerPushPagePublished),
	"page_tools.go Server.pageShareAuto -> pushTask":                     string(officerPushPageShared),
	"notify_owner.go Server.pageUpdated -> pushTask":                     string(officerPushPageUpdated),
	"notify_owner.go Server.toolNotifyOwner -> goNotify":                 string(officerPushNotifyOwner),
}

// notOfficerRouted: every other call site, with why the table does not apply.
var notOfficerRouted = map[string]string{
	"plugin_server.go Server.uninstallServerApp -> goNotify":              "an admin's uninstall withdrawing each server-app knock still out (a resolved to the person it was for); no officer reaches an uninstall",
	"fleet_actions.go Server.fleetPushWaiting -> goNotify":                "the batched \"N proposals waiting\" push: fleet actions are filed only by the chief's chief-only fleet tools (agentTool chiefOnly); no officer can file one",
	"attention.go Server.attentionNotify -> goNotify":                     "a wrapper: its callers are on this ledger",
	"attention_suggest.go Server.askFiled -> attentionNotify":             "a wrapper: its callers are on this ledger (an officer's own asks come through officerAskFiled)",
	"attention_suggest.go Server.proposalCardFiled -> attentionNotify":    "a wrapper: its callers are on this ledger",
	"attention_suggest.go Server.repushAsk -> goNotify":                   "a re-push of a filed ask (its suggestions), under the tag askFiled's push already took; an officer's opens officerDecisionCard (attentionNotification)",
	"attention_suggest.go Server.suggestAsk -> repushAsk":                 "the suggestions' re-push of a filed ask (repushAsk)",
	"push_task.go Server.pushTask -> goNotify":                            "a wrapper: its callers are on this ledger",
	"attention.go Server.attentionClear -> goNotify":                      "a withdrawal (resolved), never a new notification",
	"attention_drafts.go Server.clearDraftNote -> goNotify":               "a withdrawal (resolved), never a new notification",
	"outreach_batch.go Server.clearBatchNote -> goNotify":                 "a withdrawal (resolved), never a new notification",
	"team_withdraw.go Server.withdrawTeamItemsNow -> goNotify":            "a withdrawal (resolved), never a new notification",
	"push_task.go Server.notifyTaskDone -> goNotify":                      "the withdrawal when a task leaves review (resolved)",
	"plugin_notify.go Server.handlePluginNotify -> goNotify":              "an app plugin's knock (POST /plugin/v1/notify): only a plugin's own token with `app` reaches it, never an agent or an officer instance, and its text is muster's",
	"plugin_notify.go Server.handlePluginNotifyWithdraw -> goNotify":      "a withdrawal (resolved), never a new notification",
	"attention_tools.go Server.toolAttentionAsk -> askFiled":              "shared: any agent's attention_ask, an officer instance's included — a question, a decision, opening the ask's card",
	"page_tools.go Server.toolPageShareRequest -> askFiled":               "shared: any agent's page_share_request — approve or deny the share, opening the ask's card",
	"host_stepup.go Server.hostStepUpGate -> askFiled":                    "shared: a host pane's step-up confirmation — confirm or refuse, opening its card",
	"attention.go Server.attentionMirrorChanged -> attentionNotify":       "shared: a mirrored window waiting at a prompt (a host officer's window too) — the prompt is a decision, opening the window",
	"session_push.go Server.pushEpisode -> goNotify":                      "shared: a mirrored window's waiting or stalled episode (a host officer's window too) — pushed as any session's; see officer_push.go's ⚠ list",
	"session_push.go Server.reshowEpisode -> goNotify":                    "shared: the re-show of an episode pushEpisode sent",
	"push_task.go Server.notifyTaskDone -> pushTask":                      "shared: any agent's task moved to ready for review — a review, opening the task",
	"agent_outcome.go Server.pushOutcome -> pushTask":                     "not officer-reachable: officer instances are not tracked for outcomes",
	"chief_owner_release.go Server.applyGateRelease -> pushTask":          "not officer-reachable: the chief's gated task",
	"chief_nudge.go Server.actAsk -> askFiled":                            "not officer-reachable: the chief's nudge",
	"chief_proposal.go Server.fileProposal -> proposalCardFiled":          "not officer-reachable: the chief's proposal",
	"dispatch_hold_tools.go Server.pushSessionDispatch -> pushTask":       "not officer-reachable: dispatch_request is refused to a host officer instance (hostInstanceTool)",
	"host_dispatch.go Server.fileHeldDispatch -> proposalCardFiled":       "not officer-reachable: the chief's held host dispatch",
	"host_dispatch_done.go Server.announceHostOutcome -> pushTask":        "not officer-reachable: host dispatches only, not officer spawns",
	"host_dispatch_reconcile.go Server.orphanWindow -> askFiled":          "not officer-reachable: a host dispatch's orphaned window",
	"hostjobs.go Server.hostJobReportFailed -> askFiled":                  "not officer-reachable: a host's jobs message muster could not record",
	"hostjobs.go Server.raiseHostJobNotice -> askFiled":                   "not officer-reachable: a scheduled host job's failed run (a job is a host's declared command, never an officer)",
	"owner_ask.go Server.raiseCard -> askFiled":                           "not officer-reachable: the chief's ask_owner",
	"routing_rules.go Server.toolRoutingRulePropose -> proposalCardFiled": "not officer-reachable: the chief's routing_rule_propose",
	"session_route_notify.go Server.notifyChiefRouted -> goNotify":        "not officer-reachable: the chief's route_to_session",
	"succession.go Server.armSuccessionPush -> pushTask":                  "not officer-reachable: the chief's succession",
	"task_routing.go Server.askOwnerAboutTask -> askFiled":                "not officer-reachable: the chief's task_route_attention",
	"team_dispatch.go Server.pushTeamDispatch -> pushTask":                "not officer-reachable: the chief's team dispatch",
	"unrouted.go Server.raiseUnroutedAsk -> askFiled":                     "not officer-reachable: muster's own ask about a plugin message nothing routed",
	"plugin_autostart.go Server.autostartStarted -> attentionNotify":      "not officer-reachable: muster's own stop ask for an agent it auto-started on a plugin's task (the owner's per-plugin opt-in); the plugin door files the task, never an officer",
	"plugin_pairing.go Server.pairRequested -> goNotify":                  "not officer-reachable: an unauthenticated plugin process asks to pair",
	"pages.go Server.pageResponded -> pushTask":                           "not an officer's push: an outside viewer responded through a page's link",
	"push_task.go Server.flushCreatedTasks -> pushTask":                   "not officer-reachable: the machine routes' task creation (the CLI, the hook token)",
	"push_task.go Server.flushCommentedTasks -> pushTask":                 "not officer-reachable: the machine route's task comment (an agent's comment pushes no one)",
	"push_task.go Server.teamTaskCommented -> pushTask":                   "not officer-reachable: a member's comment typed on a team task's page",
	"push_task.go Server.notifyAgentRunning -> pushTask":                  "not officer-reachable: an officer instance has no task",
	"morning_report.go Server.morningReportFor -> goNotify":               "not officer-reachable: muster's own daily job (MorningReportPass) pushes each owner their report; no tool or agent calls it",
}

// officerGateCalls are the calls that read officerPushes.
var officerGateCalls = map[string]bool{"officerPushSends": true}

// pushCallSite is one call of a push entry point: "<file> <func> -> <callee>".
type pushCallSite struct {
	key   string
	gated bool // the calling function also calls an officerGateCalls function
}

// pushCallSites walks this package's non-test files for every call of a
// pushEntryPoints function, by calling function.
func pushCallSites(t *testing.T) []pushCallSite {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]*pushCallSite{}
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
			var callees []string
			gated := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				c, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var name string
				switch f := c.Fun.(type) {
				case *ast.SelectorExpr:
					name = f.Sel.Name
				case *ast.Ident:
					name = f.Name
				}
				if pushEntryPoints[name] {
					callees = append(callees, name)
				}
				if officerGateCalls[name] {
					gated = true
				}
				return true
			})
			for _, c := range callees {
				k := path + " " + nameLedgerFunc(fn) + " -> " + c
				if seen[k] == nil {
					seen[k] = &pushCallSite{key: k}
				}
				seen[k].gated = seen[k].gated || gated
			}
		}
	}
	out := make([]pushCallSite, 0, len(seen))
	for _, s := range seen {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out
}

// TestEveryPushCallSiteIsClassifiedForOfficers pins the seam (see above): the
// call sites are exactly officerRouted ∪ notOfficerRouted, none in both, each
// notOfficerRouted with a reason; every officerRouted function reads the
// table; and the walk finds the sink's own known callers (positive control:
// a zero here would be a walk wired to nothing).
func TestEveryPushCallSiteIsClassifiedForOfficers(t *testing.T) {
	sites := pushCallSites(t)
	found := map[string]bool{}
	for _, s := range sites {
		found[s.key] = true
		_, routed := officerRouted[s.key]
		why, other := notOfficerRouted[s.key]
		switch {
		case routed && other:
			t.Errorf("push call site %q is on both ledgers", s.key)
		case !routed && !other:
			t.Errorf("NEW push call site %q: an officer may reach it without officerPushes — add it to officerRouted (and route it through officerPushSends) or to notOfficerRouted with the reason no officer reaches it", s.key)
		case other && strings.TrimSpace(why) == "":
			t.Errorf("push call site %q is not officer-routed but gives no reason", s.key)
		case routed && !s.gated:
			t.Errorf("push call site %q is officer-routed but its function never reads officerPushes (officerPushSends)", s.key)
		}
	}
	for _, ledger := range []map[string]bool{pushLedgerKeys(officerRouted), pushLedgerKeys(notOfficerRouted)} {
		for k := range ledger {
			if !found[k] {
				t.Errorf("ledger row %q names no push call site any more: remove it", k)
			}
		}
	}
	// Positive control: known call sites of the sink are seen.
	for _, k := range []string{"outreach_batch.go Server.notifyBatchAwaiting -> goNotify", "push_task.go Server.pushTask -> goNotify"} {
		if !found[k] {
			t.Fatalf("positive control: the walk did not find %q among %d call sites", k, len(sites))
		}
	}
	var ks []string
	for _, s := range sites {
		ks = append(ks, s.key)
	}
	if !slices.IsSorted(ks) {
		t.Fatal("call sites are not sorted")
	}
}

func pushLedgerKeys[V any](m map[string]V) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}
