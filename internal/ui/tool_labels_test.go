package ui

import (
	"strings"
	"testing"
)

// TestAMusterToolIsLabelledUnderEachOfItsNames: bare (a gateway chief),
// mcp__muster__ (Claude Code) and muster_ (opencode) read the same label;
// another server's tool and a runtime's built-in have none.
func TestAMusterToolIsLabelledUnderEachOfItsNames(t *testing.T) {
	for _, c := range []struct {
		name string
		bare bool
	}{{"page_create", true}, {"mcp__muster__page_create", false}, {"muster_page_create", false}, {"mcp__muster__page_create", true}} {
		if l, ok := MusterToolLabel(c.name, "", c.bare); !ok || l != "Made a page" {
			t.Errorf("MusterToolLabel(%q, bare %v) = %q, %v; want Made a page", c.name, c.bare, l, ok)
		}
	}
	if l, ok := MusterToolLabel("muster_muster_help", "", false); !ok || l != "Looked up how a tool works" {
		t.Errorf("opencode's muster_help = %q, %v", l, ok)
	}
	for _, n := range []string{"mcp__hq__task_get", "Bash", "bash", "mcp__muster__", "muster_", "mcp__other__page_create", "hq_task_get"} {
		if l, ok := MusterToolLabel(n, "", true); ok {
			t.Errorf("MusterToolLabel(%q) = %q: not muster's tool", n, l)
		}
	}
	// 🔴 A BARE NAME IS MUSTER'S ONLY IN THE CHIEF'S CHAT: in a mirrored
	// session a bare page_create or task_get is a built-in or another
	// server's tool, and keeps its own name.
	for _, n := range []string{"page_create", "task_get", "muster_help"} {
		if l, ok := MusterToolLabel(n, "", false); ok {
			t.Errorf("MusterToolLabel(%q, bare false) = %q: a bare name in a mirrored session is not muster's", n, l)
		}
	}
}

// TestADecidingToolIsLabelledByWhatItDid: a call that decides something is
// labelled by the decision its arguments carry — never as "your decision" —
// and one whose arguments say nothing keeps the neutral label.
func TestADecidingToolIsLabelledByWhatItDid(t *testing.T) {
	for _, c := range []struct{ name, args, want string }{
		{"mcp__muster__connector_request_decide", `{"id":3,"decision":"approve"}`, "Approved access to a service"},
		{"mcp__muster__connector_request_decide", `{"id":3,"decision":"deny"}`, "Denied access to a service"},
		{"mcp__muster__connector_request_decide", `{"id":3}`, "Decided on an access request"},
		{"mcp__muster__plugin_draft_decide", `{"id":3,"decision":"reject"}`, "Rejected a reply"},
		{"mcp__muster__plugin_draft_decide", `{"id":3,"decision":"approve"}`, "Approved a reply"},
		{"mcp__muster__plugin_request_decide", `{"id":3,"decision":"deny"}`, "Denied a connection"},
		{"mcp__muster__plugin_outbound_batch_decide", `{"batch_id":"b","approve_ids":[]}`, "Turned down planned follows and likes"},
		{"mcp__muster__plugin_outbound_batch_decide", `{"batch_id":"b","approve_ids":[4,5]}`, "Approved planned follows and likes"},
		{"mcp__muster__plugin_outbound_batch_decide", `not json`, "Decided on planned follows and likes"},
		{"mcp__muster__dispatch_approve", `{"hold_id":"h"}`, "Started an agent that was waiting"},
		{"mcp__muster__attention_answer", `{"id":1,"answer":"yes"}`, "Answered an open question"},
		{"mcp__muster__page_share_request", `{"page_id":1}`, "Asked to share a page"},
		{"mcp__muster__agent_destroy", `{"name":"x"}`, "Removed an agent"},
	} {
		if got, _ := MusterToolLabel(c.name, c.args, false); got != c.want {
			t.Errorf("MusterToolLabel(%s, %s) = %q, want %q", c.name, c.args, got, c.want)
		}
	}
	for tool, by := range musterToolDecisions {
		for _, l := range append([]string{musterToolLabels[tool]}, mapValues(by)...) {
			if strings.Contains(strings.ToLower(l), "your decision") || strings.Contains(strings.ToLower(l), "passed on") {
				t.Errorf("%s's label %q claims the owner decided", tool, l)
			}
		}
	}
}

func mapValues(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// TestAMusterToolRowsTargetIsWordsNeverAnID: a muster tool's target comes
// only from a field holding words — never a subject, a route or a task id,
// and never the host's generic reading; any other tool keeps its own rule.
func TestAMusterToolRowsTargetIsWordsNeverAnID(t *testing.T) {
	for _, c := range []struct{ name, args, host, want string }{
		{"mcp__muster__page_create", `{"title":"Retry report","blocks":[]}`, "", "Retry report"},
		{"route_to_session", `{"subject":"mirror:cs-heron-1","text":"Please log each retry"}`, "", "Please log each retry"},
		{"mcp__muster__route_to_session", `{"subject":"mirror:cs-heron-1","task_id":7}`, "mirror:cs-heron-1", ""},
		{"muster_task_get", `{"id":"7"}`, "7", ""},
		{"mcp__muster__attention_ask", `{"question":"Ship it?\nor wait"}`, "", "Ship it?"},
		{"Bash", `{"command":"make docs"}`, "", "make docs"},
		{"Bash", `{"command":"make docs"}`, "make docs (host)", "make docs (host)"},
		{"mcp__hq__task_get", `{"task_id":"868abc"}`, "", "868abc"},
	} {
		if got := ToolRowTarget(c.name, c.args, c.host, true); got != c.want {
			t.Errorf("ToolRowTarget(%s, %s, %q) = %q, want %q", c.name, c.args, c.host, got, c.want)
		}
	}
}

// TestAMusterToolRowDrawsItsLabelAndItsNameInTheInfoSheet: the persisted row
// of a muster call reads "Made a page" in its summary, and its body's ⓘ holds
// mcp__muster__page_create; a Bash row is drawn as before, with no ⓘ.
func TestAMusterToolRowDrawsItsLabelAndItsNameInTheInfoSheet(t *testing.T) {
	ok := &ChatLine{Kind: "tool_result", ToolID: "toolu_x", ToolOK: true, Content: `{"page_id":3}`}
	row := render(chatToolChip(ChatLine{Kind: "tool_call", ToolID: "toolu_x", ToolName: "mcp__muster__page_create", Content: `{"title":"Retry report"}`}, ok))
	summary := row[:strings.Index(row, "</summary>")]
	if !strings.Contains(summary, `data-tool-label=""`) || !strings.Contains(summary, `>Made a page</span>`) || strings.Contains(summary, "mcp__") {
		t.Errorf("the summary is not the plain label:\n%s", summary)
	}
	if !strings.Contains(row, `data-info-code="">mcp__muster__page_create</code>`) {
		t.Errorf("no ⓘ holds the raw name:\n%s", row)
	}
	bash := render(chatToolChip(ChatLine{Kind: "tool_call", ToolID: "toolu_y", ToolName: "Bash", Content: `{"command":"make docs"}`}, ok))
	if !strings.Contains(bash, `>Bash</span>`) || strings.Contains(bash, "data-info-sheet") || strings.Contains(bash, "data-tool-label") {
		t.Errorf("a Bash row changed:\n%s", bash)
	}
}
