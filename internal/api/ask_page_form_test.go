package api

import (
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE ASK PAGE DRAWS THE FEED CARD'S ANSWER FORM (askAnswerFormHTML): the
// operator found controls on an ask's card (the note box) that its page did
// not offer where the card does; the page drew a composer of its own. These
// pin the controls each surface's ONE answer form offers, as literal lists.

// askFormControls is the answer controls of the one answer form in markup
// (data-attention-answer), in document order: "tick:<value>" a multi-select
// box, "option:<value>" an option button, "box:<name>" a textarea, and
// "send:<words>" a submit with no name. Every control must sit inside the form
// and be shown: a control under a hidden ancestor fails the test, as does a
// control associated by form= from outside it.
func askFormControls(t *testing.T, markup string) []string {
	t.Helper()
	doc := parseHTML(t, markup)
	forms := nodesWith(doc, "data-attention-answer", "")
	if len(forms) != 1 {
		t.Fatalf("%d answer forms, want 1:\n%s", len(forms), markup)
	}
	form := forms[0]
	if id, _ := nodeAttr(form, "id"); id != "" {
		for _, n := range nodesWith(doc, "form", id) {
			t.Errorf("a control is associated with the form from outside it: <%s %v>", n.Data, n.Attr)
		}
	}
	var out []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			typ, _ := nodeAttr(n, "type")
			name, _ := nodeAttr(n, "name")
			val, _ := nodeAttr(n, "value")
			c := ""
			_, other := nodeAttr(n, "form")
			switch {
			case other:
				// Another form's control drawn in the row (the card's Dismiss,
				// joined by form= to its own /cancel form): not an answer.
			case n.Data == "input" && typ == "checkbox":
				c = "tick:" + val
			case n.Data == "textarea":
				c = "box:" + name
			case n.Data == "button" && typ == "submit" && name == "option":
				c = "option:" + val
			case n.Data == "button" && typ == "submit":
				c = "send:" + nodeText(n)
			}
			if c != "" {
				for p := n; p != nil; p = p.Parent {
					if _, hidden := nodeAttr(p, "hidden"); hidden {
						t.Errorf("the control %s is hidden (under <%s %v>)", c, p.Data, p.Attr)
					}
				}
				out = append(out, c)
			}
		}
		for k := n.FirstChild; k != nil; k = k.NextSibling {
			walk(k)
		}
	}
	walk(form)
	return out
}

// askFormSendClass is the class of the form's submit with no name (Answer).
func askFormSendClass(t *testing.T, markup string) string {
	t.Helper()
	form := nodesWith(parseHTML(t, markup), "data-attention-answer", "")[0]
	for _, b := range nodesWith(form, "type", "submit") {
		_, named := nodeAttr(b, "name")
		_, other := nodeAttr(b, "form")
		if !named && !other {
			c, _ := nodeAttr(b, "class")
			return c
		}
	}
	return ""
}

// TestTheAskPageAndItsCardOfferTheSameAnswer: for a multi-select ask, a
// single-select one and a free-text one, the ask page's answer form and the
// Asks feed card's offer the SAME controls — the literal lists below — with
// the same Answer, and the note of a multi-select ask on both, shown, never
// collapsed behind a toggle. RED at the base: the page's form held only the
// box and Answer (its ticks, Send and note were outside it, and a
// single-select ask's box was hidden behind "Other answer…").
func TestTheAskPageAndItsCardOfferTheSameAnswer(t *testing.T) {
	now := time.Now()
	base := attention.Ask{Owner: 1, Asker: attention.Asker{AgentID: 7, Label: "Worker Seven"}, Status: attention.Open, CreatedAt: now}
	multi, single, free := base, base, base
	multi.ID, multi.Question, multi.Options, multi.Multi = 1001, "Which checks?", []string{"lint", "typecheck", "e2e"}, true
	single.ID, single.Question, single.Options = 1002, "Squash or merge?", []string{"Squash", "Merge"}
	free.ID, free.Question = 1003, "Which branch?"
	s := whereServer(t, multi, single, free)
	for _, c := range []struct {
		name string
		a    attention.Ask
		want []string
		send string
	}{
		{"multi-select", multi, []string{"tick:lint", "tick:typecheck", "tick:e2e", "box:answer", "send:Send answer", "box:note"}, ui.AsksCardSendClass},
		{"single-select", single, []string{"option:Squash", "option:Merge", "box:answer", "send:Answer"}, ui.AsksCardSendQuietClass + " " + ui.AsksSendDisabledClass},
		{"free text", free, []string{"box:answer", "send:Answer"}, ui.AsksCardSendClass + " " + ui.AsksSendDisabledClass},
	} {
		surfaces := map[string]string{
			"the card": askFeedCardHTML(askItem(c.a), now),
			"the page": askPageBody(t, s, c.a, askPagePath(c.a.ID)),
		}
		for where, markup := range surfaces {
			if got := askFormControls(t, markup); strings.Join(got, " | ") != strings.Join(c.want, " | ") {
				t.Errorf("%s, %s: the answer controls\n got %q\nwant %q", c.name, where, got, c.want)
			}
			if got := askFormSendClass(t, markup); got != c.send {
				t.Errorf("%s, %s: Answer's class %q, want %q", c.name, where, got, c.send)
			}
		}
	}
	// The note's label and box, literally, on the page (ids the page's own).
	page := askPageBody(t, s, multi, askPagePath(multi.ID))
	for _, want := range []string{
		`<label for="ask-input-note" class="` + ui.AsksNoteLabelClass + `">Note (optional)</label>`,
		`<textarea id="ask-input-note" name="note" rows="1" placeholder="Add a note…" maxlength="1000" class="` + ui.AsksNoteClass + `" data-asks-note></textarea>`,
		`<form id="ask-answer" method="POST" action="/attention/asks/1001/answer" class="flex min-w-0 flex-col gap-2" data-attention-answer data-asks-controls hx-boost="false">
<input type="hidden" name="return" value="/attention/asks/1001">`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the multi-select page lacks %s:\n%s", want, page)
		}
	}
	if strings.Contains(page, "Other answer…") || strings.Contains(page, "data-ask-other") {
		t.Errorf("the page still draws an Other answer… toggle:\n%s", page)
	}
}

// TestTheAskPageHasBreadcrumbsBackToAsks: the ask page's breadcrumbs are
// "Asks › Ask #N", Asks linking to the asks list and the ask the current page.
// RED at the base: the page had no breadcrumbs.
func TestTheAskPageHasBreadcrumbsBackToAsks(t *testing.T) {
	a := attention.Ask{ID: 1001, Owner: 1, Asker: attention.Asker{AgentID: 7, Label: "Worker Seven"}, Question: "Which checks?",
		Status: attention.Open, CreatedAt: time.Now()}
	page := askPageBody(t, whereServer(t, a), a, askPagePath(a.ID))
	crumbs := nodesWith(parseHTML(t, page), "aria-label", "Breadcrumb")
	if len(crumbs) != 1 || crumbs[0].Data != "nav" {
		t.Fatalf("%d breadcrumb trails, want one <nav>", len(crumbs))
	}
	if got := nodeText(crumbs[0]); got != "Asks›Ask #1001" {
		t.Errorf("the breadcrumbs read %q, want %q", got, "Asks›Ask #1001")
	}
	links := nodesWith(crumbs[0], "href", "")
	if len(links) != 1 || nodeText(links[0]) != "Asks" {
		t.Fatalf("the breadcrumbs' links: %d, want the one to Asks", len(links))
	}
	if href, _ := nodeAttr(links[0], "href"); href != "/attention" {
		t.Errorf("Asks links %q, want /attention", href)
	}
	cur := nodesWith(crumbs[0], "aria-current", "page")
	if len(cur) != 1 || nodeText(cur[0]) != "Ask #1001" {
		t.Errorf("the current page is not Ask #1001")
	}
}
