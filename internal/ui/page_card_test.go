package ui

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/cctranscript"
)

// pageCreateResult is page_create's result as the tool writes it.
const pageCreateResult = `{"page_id":42,"version":1,"title":"Launch plan","preview_url":"https://muster.example.test/pages/42/preview","next":"…"}`

// TestThePageCardIsDrawnInTheAgentChat: a stored page_create call and its
// result draw the tool row AND the page card after it: the title, Open (the
// preview), Share (a POST to the page's share route, with its label) and ⋯.
// A refused call ({"error"}) draws no card.
func TestThePageCardIsDrawnInTheAgentChat(t *testing.T) {
	html := render(chatLogInner(AgentDetailView{Name: "a", Status: "running", Messages: []ChatLine{
		{Role: "assistant", Kind: "tool_call", ToolID: "t1", ToolName: "page_create", Content: `{"title":"Launch plan"}`},
		{Role: "user", Kind: "tool_result", ToolID: "t1", ToolOK: true, Content: pageCreateResult},
		{Role: "assistant", Kind: "tool_call", ToolID: "t2", ToolName: "page_update", Content: `{}`},
		{Role: "user", Kind: "tool_result", ToolID: "t2", ToolOK: true, Content: `{"error":"no such page: 9"}`},
	}}))
	for _, want := range []string{
		`id="` + PageCardID("t1") + `"`, `data-page-card=""`, `data-page-card-kind="published"`, `>Launch plan<`,
		`href="/pages/42/preview"`, `aria-label="Open “Launch plan”"`,
		`action="/pages/42/share"`, `data-page-share=""`, `aria-label="Share “Launch plan”"`,
		`action="/pages/42/share/revoke"`, `action="/pages/42/share/rotate"`, `aria-label="More for “Launch plan”"`,
		`data-tool-row`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the chat lacks %s", want)
		}
	}
	if n := strings.Count(html, `data-page-card=""`); n != 1 {
		t.Errorf("%d page cards, want 1 (a refused call draws none)", n)
	}
	// 🔴 The card never carries an address.
	if strings.Contains(html, "data-share-url") {
		t.Errorf("a card carries a share address")
	}
}

// TestThePageCardIsDrawnInTheMirroredView: a mirrored session's MCP-named
// page_create, then its result, draw the row and then the card, by the card's
// own id (a replace, so a resent result redraws one card); a failed result
// draws none.
func TestThePageCardIsDrawnInTheMirroredView(t *testing.T) {
	var tr MirrorTranscript
	tr.Ops([]cctranscript.Event{{Kind: cctranscript.KindToolCall, ToolID: "toolu_P", Tool: "mcp__muster__page_create", Input: `{"title":"Launch plan"}`}})
	ops := tr.Ops([]cctranscript.Event{{Kind: cctranscript.KindToolResult, ToolID: "toolu_P", OK: true, Output: pageCreateResult}})
	var card *MirrorOp
	for i := range ops {
		if ops[i].ID == PageCardID("toolu_P") {
			card = &ops[i]
		}
	}
	if card == nil || card.Op != "replace" || !strings.Contains(card.HTML, `data-page-card-kind="published"`) ||
		!strings.Contains(card.HTML, `action="/pages/42/share"`) || !strings.Contains(card.HTML, `id="`+PageCardID("toolu_P")+`"`) {
		t.Fatalf("the mirrored view's ops: %+v", ops)
	}
	tr.Ops([]cctranscript.Event{{Kind: cctranscript.KindToolCall, ToolID: "toolu_F", Tool: "mcp__muster__page_create", Input: `{}`}})
	for _, op := range tr.Ops([]cctranscript.Event{{Kind: cctranscript.KindToolResult, ToolID: "toolu_F", OK: false, Output: `{"error":"x"}`}}) {
		if strings.Contains(op.HTML, "data-page-card") {
			t.Fatalf("a failed call drew a card: %+v", op)
		}
	}
}

// TestReadPageToolResult reads each page tool's result, any MCP server's
// prefix, and refuses every other shape.
func TestReadPageToolResult(t *testing.T) {
	for _, c := range []struct {
		tool, out, kind string
		ok              bool
	}{
		{"page_create", pageCreateResult, PageCardPublished, true},
		{"mcp__muster__page_create", pageCreateResult, PageCardPublished, true},
		{"mcp__other_srv__page_update", `{"page_id":4,"version":3,"title":"T"}`, PageCardUpdated, true},
		{"page_share_request", `{"page_id":4,"status":"live","title":"T","shared_by":"worker"}`, PageCardShared, true},
		{"page_share_request", `{"page_id":4,"status":"pending","title":"T"}`, PageCardAsked, true},
		{"page_get", `{"page_id":4}`, "", false},
		{"xpage_create", pageCreateResult, "", false},
		{"page_create", `{"error":"x"}`, "", false},
		{"page_create", `not json`, "", false},
		{"page_create", `{"title":"no id"}`, "", false},
	} {
		v, ok := ReadPageToolResult(c.tool, c.out)
		if ok != c.ok || (ok && v.Kind != c.kind) {
			t.Errorf("ReadPageToolResult(%q, %q) = %+v, %v; want kind %q, %v", c.tool, c.out, v, ok, c.kind, c.ok)
		}
	}
	if v, _ := ReadPageToolResult("page_share_request", `{"page_id":4,"status":"live","title":"T","shared_by":"worker"}`); !strings.Contains(render(PageCard(v, "")), "Shared by worker") {
		t.Errorf("a shared card does not say who shared it")
	}
}

// TestTheSidebarHasPagesOnlyWhenServed: the Pages entry is drawn with the
// feature on, and not without it.
func TestTheSidebarHasPagesOnlyWhenServed(t *testing.T) {
	on := render(sidebar("", NavPages, false, Features{Pages: true}))
	if !strings.Contains(on, `id="sidebar-pages" href="/pages"`) || !strings.Contains(on, `aria-current="page"`) {
		t.Fatalf("no Pages entry with pages on")
	}
	if off := render(sidebar("", "", false, Features{})); strings.Contains(off, `sidebar-pages`) {
		t.Fatalf("a Pages entry with pages off")
	}
}

// TestThePageShareScriptIsInTheAppScript: the Share button's behaviour ships
// on every document with the nav — the native sheet, the AbortError silence,
// the copy fallback — once per window.
func TestThePageShareScriptIsInTheAppScript(t *testing.T) {
	js := renderString(t, appScript(Features{}))
	for _, want := range []string{"window.__musterPageShare", "navigator.share(data)", "e.name === 'AbortError'", "window.cgCopyText(url)", "'Link copied'", "addEventListener('pointerdown', early, true)", "new ClipboardItem("} {
		if !strings.Contains(js, want) {
			t.Errorf("appScript lacks %s", want)
		}
	}
}
