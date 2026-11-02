package ui

import (
	"regexp"
	"strings"
	"testing"
)

// THE RENDERED VIEW COMPOSER'S TWO CONTROLS (composer_menu.go), as markup: the
// split Send/Queue button and the `/` and `:` menu's place. What they DO in a
// browser is e2e/tests/composer-menu.spec.ts.

// elem is the opening tag of the element with id in page.
func elem(t *testing.T, page, id string) string {
	t.Helper()
	i := strings.Index(page, `id="`+id+`"`)
	if i < 0 {
		t.Fatalf("no element %q", id)
	}
	start := strings.LastIndex(page[:i], "<")
	return page[start : i+strings.Index(page[i:], ">")+1]
}

func renderedView(t *testing.T, queue *ComposerQueue, interactive bool) string {
	t.Helper()
	return renderString(t, MirrorViewPage(MirrorViewView{Host: "host-a", Session: "main", Window: "@3", Interactive: interactive,
		StepUp: stepUpFixture(), Queue: queue}))
}

// TestSendIsTheFaceAndQueueIsBehindTheCaret: the operator's "always Send" —
// with a queue the composer's first submit is Send, the caret beside it is a
// type=button (it cannot submit anything) that opens a popover menu, and Queue
// is the menu's one item, a submit to the queue route. Send comes BEFORE the
// queue in the form, so it is also the form's default button (an implicit
// submit is a Send, never a Queue).
func TestSendIsTheFaceAndQueueIsBehindTheCaret(t *testing.T) {
	page := renderedView(t, &ComposerQueue{Src: SendQueuePath + "?host=host-a"}, true)
	send, more, menu, queue := elem(t, page, "mv-send"), elem(t, page, "mv-send-more"), elem(t, page, "mv-send-menu"), elem(t, page, "mv-queue-send")
	if !strings.Contains(send, `type="submit"`) || strings.Contains(send, "formaction") || !strings.Contains(send, `aria-label="Send" title="Send" class="`+composerSendSplitClass+`">`) {
		t.Fatalf("Send: %s", send)
	}
	for _, a := range []string{`type="button"`, `popovertarget="mv-send-menu"`, `aria-haspopup="menu"`, `aria-expanded="false"`, `aria-controls="mv-send-menu"`, "min-h-[44px]", "min-w-[44px]"} {
		if !strings.Contains(more, a) {
			t.Errorf("the caret lacks %s: %s", a, more)
		}
	}
	if !strings.Contains(menu, `popover=""`) || !strings.Contains(menu, `role="menu"`) {
		t.Errorf("the menu: %s", menu)
	}
	if !strings.Contains(queue, `role="menuitem"`) || !strings.Contains(queue, `formaction="`+SendQueuePath+`"`) || !strings.Contains(queue, `type="submit"`) || !strings.Contains(queue, "min-h-[44px]") {
		t.Errorf("Queue: %s", queue)
	}
	if !strings.Contains(page, ">"+SendQueueLabel+"</button>") {
		t.Errorf("Queue does not read %q", SendQueueLabel)
	}
	// Queue is INSIDE the menu, and the menu after Send in the form.
	iSend, iMenu, iQueue := strings.Index(page, `id="mv-send"`), strings.Index(page, `id="mv-send-menu"`), strings.Index(page, `id="mv-queue-send"`)
	if !(iSend < iMenu && iMenu < iQueue) {
		t.Errorf("order Send %d, menu %d, Queue %d: want Send, then the menu holding Queue", iSend, iMenu, iQueue)
	}
	// Exactly one submit precedes Queue in the form: Send.
	form := page[strings.Index(page, `<form id="mv-form"`):strings.Index(page, `</form>`)]
	firstSubmit := regexp.MustCompile(`<button id="([^"]+)" type="submit"`).FindStringSubmatch(form)
	if firstSubmit == nil || firstSubmit[1] != "mv-send" {
		t.Errorf("the form's first submit is %v, want mv-send (the implicit-submit default)", firstSubmit)
	}
}

// TestNoQueueNoCaret: without a queue (no database) and on a read-only host,
// Send is the whole control, as before: no caret, no menu, no Queue.
func TestNoQueueNoCaret(t *testing.T) {
	for name, page := range map[string]string{
		"no queue":  renderedView(t, nil, true),
		"read-only": renderedView(t, &ComposerQueue{Src: SendQueuePath}, false),
	} {
		for _, id := range []string{"mv-send-more", "mv-send-menu", "mv-queue-send"} {
			if strings.Contains(page, `id="`+id+`"`) {
				t.Errorf("%s: carries %s", name, id)
			}
		}
		if !strings.Contains(elem(t, page, "mv-send"), `class="`+composerSendIconClass+`"`) {
			t.Errorf("%s: Send is not the plain one: %s", name, elem(t, page, "mv-send"))
		}
	}
}

// TestTheMenuIsTheRenderedViewsAlone: the `/` and `:` menu is drawn on an
// enabled Rendered view composer, pointed at ITS window's catalog; not on a
// read-only host, and not on the Terminal view's composer (which pastes into
// whatever runs in the pane, not into Claude Code).
func TestTheMenuIsTheRenderedViewsAlone(t *testing.T) {
	page := renderedView(t, nil, true)
	box := elem(t, page, "mv-complete-box")
	if !strings.Contains(box, `data-src="`+MirrorCompletePath+`?host=host-a&amp;session=main&amp;window=%403"`) || !strings.Contains(box, `hidden`) || !strings.Contains(box, "bottom-full") {
		t.Fatalf("the menu box: %s", box)
	}
	if lb := elem(t, page, "mv-complete"); !strings.Contains(lb, `role="listbox"`) {
		t.Errorf("the list: %s", lb)
	}
	if !strings.Contains(page, "input.setAttribute('aria-autocomplete', 'list')") {
		t.Error("the menu script is not on the page")
	}
	// The composer is its positioning box, so the menu sits above IT.
	if c := elem(t, page, "mv-compose"); !strings.Contains(c, "fixed") {
		t.Errorf("the composer is not a containing block: %s", c)
	}
	for name, p := range map[string]string{
		"read-only": renderedView(t, nil, false),
		"terminal": renderString(t, AgentTerminalPage(AgentTerminalView{Name: "main", StepUp: stepUpFixture(),
			Composer: &MirrorComposerView{SendPath: MirrorTerminalSendPath, Host: "host-a", Session: "main", Aria: "x"}})),
	} {
		// The menu script's own aria-autocomplete, not the attribute: the
		// sidebar's command palette (palette.go) carries one on every page.
		if strings.Contains(p, "mv-complete") || strings.Contains(p, "input.setAttribute('aria-autocomplete', 'list')") {
			t.Errorf("%s: carries the menu", name)
		}
	}
}

// buttonByID is the whole <button>…</button> with id in page.
func buttonByID(t *testing.T, page, id string) string {
	t.Helper()
	open := elem(t, page, id)
	i := strings.Index(page, open)
	end := strings.Index(page[i:], "</button>")
	if !strings.HasPrefix(open, "<button") || end < 0 {
		t.Fatalf("%q is not a button: %s", id, open)
	}
	return page[i : i+end+len("</button>")]
}

// TestSendIsAPaperPlane: every composer's Send — the Rendered view's plain
// one, its split one beside the Queue caret, and the agent chat's (page and
// chief panel) — is an icon button: an inline aria-hidden svg drawn in
// currentColor, no visible text, named "Send" by aria-label and title, a
// 44×44 target. The ask card's Submit, which wears composerSendClass, keeps
// its words; and the script's "Send again" takes that worded look rather than
// copying the icon button's padless square.
func TestSendIsAPaperPlane(t *testing.T) {
	agent := AgentDetailView{Name: "a", Status: "running", ActiveSessionID: 1}
	panel := agent
	panel.Surface = ChatSurfaceChiefPanel
	pages := map[string][2]string{
		"plain (no queue)":  {renderedView(t, nil, true), "mv-send"},
		"split (queue)":     {renderedView(t, &ComposerQueue{Src: SendQueuePath + "?host=host-a"}, true), "mv-send"},
		"read-only":         {renderedView(t, &ComposerQueue{Src: SendQueuePath}, false), "mv-send"},
		"agent chat page":   {renderString(t, agentChatPane(agent)), "chat-send"},
		"agent chief panel": {renderString(t, agentChatPane(panel)), "chat-send"},
	}
	for name, p := range pages {
		b := buttonByID(t, p[0], p[1])
		if got := strings.TrimSpace(visibleText(t, b)); got != "" {
			t.Errorf("%s: Send shows text %q, want an icon only: %s", name, got, b)
		}
		for _, want := range []string{`type="submit"`, `aria-label="Send"`, `title="Send"`, "min-h-[44px]", "min-w-[44px]", "<svg ", `aria-hidden="true"`, "currentColor"} {
			if !strings.Contains(b, want) {
				t.Errorf("%s: Send lacks %s: %s", name, want, b)
			}
		}
		if strings.Contains(b, "<img") || strings.Contains(b, " src=") || strings.Contains(b, "<use") {
			t.Errorf("%s: the icon is not inline: %s", name, b)
		}
	}
	// The split half is rounded on the left only (the caret takes the right):
	// one control.
	if split := buttonByID(t, pages["split (queue)"][0], "mv-send"); !strings.Contains(split, "rounded-l-xl") || !strings.Contains(split, "rounded-r-none") {
		t.Errorf("the split Send is not the left half: %s", split)
	}
	// CONTROL: the ask card's Submit keeps its words and its look.
	ask := renderString(t, MirrorAskLive(MirrorAskLiveView{Ref: MirrorRef{Host: "lap", Session: "work", Window: "@3"}, ToolID: "toolu_Q", State: AskStatePending, Questions: askQs}))
	sub := regexp.MustCompile(`<button type="submit" data-ask-submit=""[^>]*>([^<]*)</button>`).FindStringSubmatch(ask)
	if sub == nil || sub[1] != "Submit answers" || !strings.Contains(sub[0], `class="`+composerSendClass+`"`) {
		t.Errorf("the ask card's Submit lost its words or its look: %v", sub)
	}
	// "Send again" is built by the script: the worded look, not whatever Send wears.
	if !strings.Contains(pages["plain (no queue)"][0], `againBtn.className = '`+composerSendClass+`';`) {
		t.Error("the script's Send again does not take composerSendClass")
	}
}

// TestBothDocumentsRankWithOneFunction: 🔴 a SEAM guard. The Rendered view is
// its own document, and its `/` menu embeds the ranker ITSELF — it must not
// depend on another script (appScript, which the view also carries for its
// side nav) having run first. So the view holds two copies, and the guard is
// that there is no second VERSION: every definition of window.musterRank in
// the document is a byte-identical rankJS — not a copy that drifts from
// internal/searchrank's table (which e2e holds each document to).
func TestBothDocumentsRankWithOneFunction(t *testing.T) {
	view := renderedView(t, nil, true)
	app := renderString(t, appScript(Features{}))
	menu := renderString(t, composerMenuScript("/x"))
	for name, page := range map[string]string{"rendered view": view, "app shell": app, "the menu's own script": menu} {
		copies, defs := strings.Count(page, rankJS), strings.Count(page, "window.musterRank = function")
		if copies == 0 {
			t.Errorf("%s carries no rankJS — its ranking would match nothing", name)
		}
		if defs != copies {
			t.Errorf("%s defines window.musterRank %d times but carries rankJS %d times — a definition that is not "+
				"rankJS is a second version", name, defs, copies)
		}
	}
}
