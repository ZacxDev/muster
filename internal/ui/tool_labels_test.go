package ui

import (
	"strings"
	"testing"
)

// TestAMusterToolIsLabelledUnderEachOfItsNames: bare (a gateway chief),
// mcp__muster__ (Claude Code) and muster_ (opencode) read the same label;
// another server's tool and a runtime's built-in have none.
func TestAMusterToolIsLabelledUnderEachOfItsNames(t *testing.T) {
	for _, n := range []string{"page_create", "mcp__muster__page_create", "muster_page_create"} {
		if l, ok := MusterToolLabel(n); !ok || l != "Made a page" {
			t.Errorf("MusterToolLabel(%q) = %q, %v; want Made a page", n, l, ok)
		}
	}
	if l, ok := MusterToolLabel("muster_muster_help"); !ok || l != "Looked up how a tool works" {
		t.Errorf("opencode's muster_help = %q, %v", l, ok)
	}
	for _, n := range []string{"mcp__hq__task_get", "Bash", "bash", "mcp__muster__", "muster_", "mcp__other__page_create"} {
		if l, ok := MusterToolLabel(n); ok {
			t.Errorf("MusterToolLabel(%q) = %q: not muster's tool", n, l)
		}
	}
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
		if got := ToolRowTarget(c.name, c.args, c.host); got != c.want {
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
