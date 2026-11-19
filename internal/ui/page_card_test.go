package ui

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/cctranscript"
)

// pageCreateResult is page_create's result as the tool writes it.
const pageCreateResult = `{"page_id":42,"version":1,"title":"Launch plan","preview_url":"https://muster.example.test/pages/42/preview","next":"…"}`

// TestThePageCardIsDrawnInTheAgentChat: a stored page_create call and its
// result draw the tool row AND the page card after it — the card the SERVER
// read (PageCards: here a title unlike the result's, so the card's is
// visibly the page's): the title, Open (the preview), Share (a POST to the
// page's share route, with its label) and ⋯. A refused call ({"error"}), and a
// call with no server-read card, draw no card.
func TestThePageCardIsDrawnInTheAgentChat(t *testing.T) {
	html := render(chatLogInner(AgentDetailView{Name: "a", Status: "running", PageCards: map[string]PageCardView{
		"t1": {ID: 42, Title: "Launch plan", Version: 1, Kind: PageCardPublished},
	}, Messages: []ChatLine{
		{Role: "assistant", Kind: "tool_call", ToolID: "t1", ToolName: "page_create", Content: `{"title":"Launch plan"}`},
		{Role: "user", Kind: "tool_result", ToolID: "t1", ToolOK: true, Content: strings.Replace(pageCreateResult, "Launch plan", "What the agent claims", 1)},
		{Role: "assistant", Kind: "tool_call", ToolID: "t2", ToolName: "page_update", Content: `{}`},
		{Role: "user", Kind: "tool_result", ToolID: "t2", ToolOK: true, Content: `{"error":"no such page: 9"}`},
	}}))
	for _, want := range []string{
		`id="` + PageCardID("t1") + `"`, `data-page-card=""`, `data-page-card-kind="published"`, `>Launch plan<`,
		`href="/pages/42/view"`, `aria-label="Open “Launch plan”"`,
		`action="/pages/42/share"`, `data-page-share=""`, `aria-label="Share “Launch plan”"`,
		`aria-label="More for “Launch plan”"`, `popovertarget="` + PageCardID("t1") + `-menu"`,
		`id="` + PageCardID("t1") + `-menu" popover="auto" role="menu"`, `data-floating-menu=""`, `>Page settings</a>`,
		`data-tool-row`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the chat lacks %s", want)
		}
	}
	if n := strings.Count(html, `data-page-card=""`); n != 1 {
		t.Errorf("%d page cards, want 1 (a refused call draws none)", n)
	}
	// A page with no live link has nothing to revoke or replace.
	for _, banned := range []string{`action="/pages/42/share/revoke"`, `action="/pages/42/share/rotate"`} {
		if strings.Contains(html, banned) {
			t.Errorf("a card for a page with no live link draws %s", banned)
		}
	}
	// 🔴 The card never carries an address.
	if strings.Contains(html, "data-share-url") {
		t.Errorf("a card carries a share address")
	}
}

// TestThePageCardIsDrawnInTheMirroredView: a mirrored session's muster
// page_create, then its result, draw the row and then the card's PLACEHOLDER,
// by the card's own id (a replace, so a resent result redraws one), which the
// page fills from the server's card route for that page; a failed result
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
	if card == nil || card.Op != "replace" || !strings.Contains(card.HTML, `data-page-card-src="/pages/42/card?tool=page_create&amp;version=1"`) ||
		!strings.Contains(card.HTML, `id="`+PageCardID("toolu_P")+`"`) {
		t.Fatalf("the mirrored view's ops: %+v", ops)
	}
	tr.Ops([]cctranscript.Event{{Kind: cctranscript.KindToolCall, ToolID: "toolu_F", Tool: "mcp__muster__page_create", Input: `{}`}})
	for _, op := range tr.Ops([]cctranscript.Event{{Kind: cctranscript.KindToolResult, ToolID: "toolu_F", OK: false, Output: `{"error":"x"}`}}) {
		if strings.Contains(op.HTML, "data-page-card") {
			t.Fatalf("a failed call drew a card or a placeholder: %+v", op)
		}
	}
}

// TestReadPageToolResult reads each of MUSTER's page tools' results (bare, or
// mcp__muster__…) as a reference — the page, the tool, the share status — and
// refuses every other shape, another MCP server's same-named tool included.
func TestReadPageToolResult(t *testing.T) {
	for _, c := range []struct {
		tool, out, kind string
		ok              bool
	}{
		{"page_create", pageCreateResult, PageCardPublished, true},
		{"mcp__muster__page_create", pageCreateResult, PageCardPublished, true},
		{"mcp__muster__page_update", `{"page_id":4,"version":3,"title":"T"}`, PageCardUpdated, true},
		{"mcp__other_srv__page_update", `{"page_id":4,"version":3,"title":"T"}`, "", false},
		{"page_share_request", `{"page_id":4,"status":"live","title":"T","shared_by":"worker"}`, PageCardShared, true},
		{"page_share_request", `{"page_id":4,"status":"pending","title":"T"}`, PageCardAsked, true},
		{"page_get", `{"page_id":4}`, "", false},
		{"xpage_create", pageCreateResult, "", false},
		{"page_create", `{"error":"x"}`, "", false},
		{"page_create", `not json`, "", false},
		{"page_create", `{"title":"no id"}`, "", false},
	} {
		r, ok := ReadPageToolResult(c.tool, c.out)
		if ok != c.ok || (ok && PageCardKind(r.Tool, r.Status) != c.kind) {
			t.Errorf("ReadPageToolResult(%q, %q) = %+v, %v; want kind %q, %v", c.tool, c.out, r, ok, c.kind, c.ok)
		}
	}
	if !strings.Contains(render(PageCard(PageCardView{ID: 4, Title: "T", Kind: PageCardShared, SharedBy: "worker"}, "")), "Shared by worker") {
		t.Errorf("a shared card does not say who shared it")
	}
}

// TestTheCardsShareButtonPrefetchesOnlyAReusableLink: the Share button carries
// data-share-reusable — the mark pageShareJS's pointerdown/focus prefetch needs
// — only when the server keeps one reusable link per page; and a page that has
// a live link never reads as private on its card.
func TestTheCardsShareButtonPrefetchesOnlyAReusableLink(t *testing.T) {
	keyed := render(PageCard(PageCardView{ID: 4, Title: "T", Kind: PageCardPublished, Reusable: true}, ""))
	plain := render(PageCard(PageCardView{ID: 4, Title: "T", Kind: PageCardPublished}, ""))
	if !strings.Contains(keyed, "data-share-reusable") || strings.Contains(plain, "data-share-reusable") {
		t.Fatalf("data-share-reusable: keyed %v, no key %v; want only keyed", strings.Contains(keyed, "data-share-reusable"), strings.Contains(plain, "data-share-reusable"))
	}
	js := renderString(t, appScript(Features{}))
	if !strings.Contains(js, "btn.hasAttribute('data-share-reusable') && !btn.getAttribute('data-share-url')) ensure(btn)") {
		t.Fatal("the prefetch is not gated on data-share-reusable")
	}
	live := render(PageCard(PageCardView{ID: 4, Title: "T", Kind: PageCardPublished, Live: true}, ""))
	if strings.Contains(live, "only you can see it") || !strings.Contains(live, "it has a live link") {
		t.Fatalf("a page with a live link reads as private: %s", live)
	}
	if !strings.Contains(plain, `data-note-live="New page — it has a live link: anyone with it can open it."`) {
		t.Fatal("a private page's card has no note for when Share makes its link")
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

// TestAPageCardsMenuOffersRevokeOnlyWithALiveLink: the chat card's ⋯ is a
// popover menu (MoreMenu: Escape and a tap outside close it), never a
// <details>; it holds Revoke link and New link only while the page has a live
// link, and the page's own screen either way; two cards drawn without an id
// (the card route's) never share a menu id.
//
// REGRESSION TEST: red at 35b55f6f3, whose card drew a <details> ⋯ with Revoke
// link and New link on every card, a page with no link included.
func TestAPageCardsMenuOffersRevokeOnlyWithALiveLink(t *testing.T) {
	for _, live := range []bool{false, true} {
		html := render(PageCard(PageCardView{ID: 42, Title: "Launch plan", Kind: PageCardShared, Live: live}, "card-a"))
		for _, want := range []string{`popovertarget="card-a-menu"`, `id="card-a-menu" popover="auto"`, `href="/pages/42"`} {
			if !strings.Contains(html, want) {
				t.Errorf("live=%v: the card lacks %s", live, want)
			}
		}
		if strings.Contains(html, "<details") {
			t.Errorf("live=%v: the card's menu is a <details>", live)
		}
		for _, link := range []string{`action="/pages/42/share/revoke"`, `action="/pages/42/share/rotate"`} {
			if strings.Contains(html, link) != live {
				t.Errorf("live=%v: %s drawn = %v", live, link, !live)
			}
		}
	}
	a, b := render(PageCard(PageCardView{ID: 42, Title: "T"}, "")), render(PageCard(PageCardView{ID: 42, Title: "T"}, ""))
	idOf := func(h string) string {
		i := strings.Index(h, `popovertarget="`)
		if i < 0 {
			return ""
		}
		h = h[i+len(`popovertarget="`):]
		return h[:strings.Index(h, `"`)]
	}
	if idOf(a) == "" || idOf(a) == idOf(b) {
		t.Errorf("two id-less cards share the menu id %q", idOf(a))
	}
}
