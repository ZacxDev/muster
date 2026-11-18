package ui

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// THE PLAIN LABEL OF EACH OF MUSTER'S OWN TOOLS (the operator rule: muster's
// chrome has no raw tool name, id or routing text in its top layer). A chat
// draws one of muster's tool calls as "Made a page", never as
// mcp__muster__page_create; the raw name is in the row's ⓘ (toolNameSheet).
//
// 🔴 ONE TABLE, EVERY SURFACE. The persisted transcript and the mirrored view
// (toolChip) and the chat's live stream (agentChatScript, which is handed this
// table as JSON by toolLabelsJSON) all read it. A second copy is how one
// surface goes on showing raw names after the other is fixed.
//
// 🔴 THE SET IS LEDGERED AGAINST THE TOOLS MUSTER SERVES, not against this
// file: internal/api's TestEveryMusterToolHasAPlainLabel walks agentTools and
// mirrorTools and fails when a served tool has no label here, or when this
// table names a tool nothing serves.
//
// Another server's tool (Bash, Read, mcp__hq__task_get) is not muster's and is
// not looked up here: it keeps its own name.
//
// Labels are past tense ("what it did") and name no tool, id or address.
var musterToolLabels = map[string]string{
	// Your tasks.
	"agent_get_task":           "Read its task",
	"agent_comment_task":       "Commented on its task",
	"agent_set_task_status":    "Updated its task",
	"agent_list_pull_requests": "Looked at its code changes",
	"agent_report_done":        "Said it's finished",
	"tasks_list":               "Looked at your tasks",
	"task_get":                 "Read a task",
	"task_comment":             "Commented on a task",
	"task_set_status":          "Updated a task",
	"task_create_gated":        "Filed a task for you to approve",
	"task_forward":             "Passed a task update on",
	"task_route_attention":     "Put a task in front of you",
	"track_task":               "Noted which task it's on",

	// Questions for you.
	"ask_owner":               "Asked you a question",
	"attention_ask":           "Asked you a question",
	"attention_wait":          "Waited for your answer",
	"attention_cancel":        "Withdrew a question",
	"attention_list":          "Checked what needs you",
	"attention_answer":        "Passed on your answer",
	"chief_ask":               "Asked your chief",
	"answer_session_question": "Answered a session's question",

	// Pages.
	"page_create":        "Made a page",
	"page_update":        "Updated a page",
	"page_get":           "Read a page",
	"page_list":          "Looked at its pages",
	"page_responses":     "Read a page's responses",
	"page_share_request": "Shared a page",
	"page_link_revoke":   "Turned off a page link",

	// Sessions and agents.
	"fleet_list":          "Looked over your sessions",
	"fleet_report":        "Reported on your sessions",
	"focus_search":        "Looked up what sessions are working on",
	"sessions_overview":   "Checked what your sessions are doing",
	"session_context":     "Looked at a session",
	"agents_list":         "Looked at your agents",
	"agent_dispatch":      "Started an agent",
	"agent_destroy":       "Stopped an agent",
	"dispatch_request":    "Asked to start an agent",
	"dispatch_approve":    "Started an agent you approved",
	"chief_restart":       "Restarted your chief",
	"repos_list":          "Looked at your code projects",
	"succession_propose":  "Suggested a new session to carry on",
	"end_session_propose": "Suggested ending a session",

	// Messages and routing.
	"route_to_session":             "Sent something to a session",
	"route_many":                   "Sent your request to several sessions",
	"route_reply":                  "Answered a message sent to it",
	"reply_draft":                  "Drafted a reply",
	"routing_rule_propose":         "Suggested a rule for messages like this",
	"plugin_message_settle":        "Marked a message as handled",
	"plugin_messages_list":         "Looked at your messages",
	"plugin_message_route":         "Sent a message to a session",
	"plugin_drafts_list":           "Looked at reply drafts",
	"plugin_draft_decide":          "Passed on your decision on a reply",
	"plugin_requests_list":         "Looked at connection requests",
	"plugin_request_decide":        "Passed on your decision on a connection",
	"plugin_outbound_batch_get":    "Looked at planned follows and likes",
	"plugin_outbound_batch_decide": "Passed on your decision on follows and likes",

	// Access to services.
	"connectors_list":          "Checked what it can use",
	"connector_request":        "Asked for access to a service",
	"connector_status":         "Checked its access to a service",
	"connector_requests_list":  "Looked at access requests",
	"connector_request_decide": "Passed on your decision on access",
	"postgres_query":           "Looked something up in a database",
	"internal_http_request":    "Used a connected service",
	"browser_flow_request":     "Asked to use a website for you",
	"browser_flow_status":      "Checked on a website request",

	// Officers.
	"officer_brief":                   "Read its instructions",
	"officer_items":                   "Looked at its work list",
	"officer_claim":                   "Picked up a piece of work",
	"officer_heartbeat":               "Kept hold of its work",
	"officer_release":                 "Gave a piece of work back",
	"officer_complete":                "Finished a piece of work",
	"officer_ask_person":              "Asked someone a question",
	"officer_task_ask":                "Asked a task's owner to approve something",
	"officer_task_get":                "Read a task",
	"officer_task_list":               "Looked at tasks",
	"officer_gap_task":                "Filed a task about a gap",
	"officer_contacts_list":           "Looked at people who engaged",
	"officer_contact_get":             "Looked up a person",
	"officer_contact_note":            "Made a note about a person",
	"officer_post_draft":              "Asked to post something",
	"officer_flood_block_propose":     "Suggested blocking a traffic flood",
	"officer_kube_read":               "Checked a server",
	"officer_obs_queries":             "Looked at monitoring checks",
	"officer_obs_query":               "Checked monitoring",
	"officer_obs_raw":                 "Ran its own monitoring check",
	"officer_ci_logs":                 "Read why checks failed",
	"officer_pr_context":              "Read about a code change",
	"officer_mark_ready":              "Marked its code change ready",
	"officer_merge_pr":                "Merged its code change",
	"officer_shepherd_mark_ready":     "Marked a code change ready",
	"officer_shepherd_merge_ask":      "Asked for a code change to be merged",
	"officer_public_branch":           "Named a branch for its work",
	"officer_tag_release":             "Tagged a release",
	"officer_release_evaluation":      "Reported on a release",
	"officer_rollout_status":          "Checked a release's rollout",
	"officer_rollout_promote_propose": "Suggested releasing to everyone",
	"officer_spawn_share":             "Managed shared helper sessions",

	// The MCP transport's own tool.
	"muster_help": "Looked up how a tool works",
}

// The prefixes a runtime puts on one of muster's MCP tools: Claude Code's
// mcp__<server>__ and opencode's <server>_, for the server named "muster".
const (
	claudeCodeMusterPrefix = "mcp__muster__"
	openCodeMusterPrefix   = "muster_"
)

// MusterToolLabel is the plain label of muster's tool name — bare (a gateway
// chief's call), mcp__muster__… (Claude Code) or muster_… (opencode) — and ok
// false for any other tool (a runtime's built-in, another server's).
func MusterToolLabel(name string) (string, bool) {
	if l, ok := musterToolLabels[name]; ok {
		return l, true
	}
	for _, p := range []string{claudeCodeMusterPrefix, openCodeMusterPrefix} {
		if base, ok := strings.CutPrefix(name, p); ok {
			if l, ok := musterToolLabels[base]; ok {
				return l, true
			}
		}
	}
	return "", false
}

// toolSheetPlaceholder stands for a row's id in toolLabelsJS's template.
const toolSheetPlaceholder = "__tool_sheet__"

// toolLabelsJS is the live stream's copy of the rule (agentChatScript): the
// label table as JSON and two functions —
//
//   - musterToolLabel(nm): the plain label of muster's tool nm, as
//     MusterToolLabel reads it (bare, mcp__muster__…, muster_…), or ”.
//   - musterToolSheet(rowKey, nm): the row's ⓘ line (toolNameSheet) as a
//     node, drawn from the server's own markup (InfoSheetHTML) with the raw
//     name set as TEXT, never as HTML.
func toolLabelsJS() string {
	labels, _ := json.Marshal(musterToolLabels)
	line, _ := toolNameSheet("", toolSheetPlaceholder, toolSheetPlaceholder)
	sheet, _ := json.Marshal(render(line))
	return `
  var MUSTER_TOOL_LABELS = ` + string(labels) + `;
  var MUSTER_TOOL_PREFIXES = ` + mustJSON([]string{claudeCodeMusterPrefix, openCodeMusterPrefix}) + `;
  function musterToolLabel(nm) {
    nm = String(nm || '');
    if (Object.prototype.hasOwnProperty.call(MUSTER_TOOL_LABELS, nm)) return MUSTER_TOOL_LABELS[nm];
    for (var i = 0; i < MUSTER_TOOL_PREFIXES.length; i++) {
      var p = MUSTER_TOOL_PREFIXES[i];
      if (nm.indexOf(p) === 0) {
        var base = nm.slice(p.length);
        if (Object.prototype.hasOwnProperty.call(MUSTER_TOOL_LABELS, base)) return MUSTER_TOOL_LABELS[base];
      }
    }
    return '';
  }
  var MUSTER_TOOL_SHEET = ` + string(sheet) + `;
  function musterToolSheet(rowKey, nm) {
    var key = 'ls-' + String(rowKey || '').replace(/[^A-Za-z0-9_-]/g, '_');
    var t = document.createElement('template');
    t.innerHTML = MUSTER_TOOL_SHEET.split(` + strconv.Quote(toolSheetPlaceholder+"-tn") + `).join(key + '-tn');
    var line = t.content.firstElementChild;
    if (!line) return null;
    var code = line.querySelector('[data-info-code]');
    if (code) code.textContent = String(nm || '');
    var copy = line.querySelector('[data-info-copy]');
    if (copy) copy.setAttribute('data-copy-text', String(nm || ''));
    return line;
  }
`
}

// mustJSON is v as JSON (v is a literal of this package: it cannot fail).
func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// MusterToolNames is every tool the table labels, sorted: the label side of
// internal/api's seam ledger.
func MusterToolNames() []string {
	out := make([]string, 0, len(musterToolLabels))
	for n := range musterToolLabels {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
