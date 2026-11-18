package api

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/ui"
)

// answerFormOf is the answer form of ask 9 in out.
func answerFormOf(t *testing.T, out string) string {
	t.Helper()
	f := regexp.MustCompile(`(?s)<form method="POST" action="/attention/asks/9/answer"[^>]*>.*?</form>`).FindString(out)
	if f == "" {
		t.Fatalf("no answer form:\n%s", out)
	}
	return f
}

// placeFree is an answer form without what its PLACE adds (askFormPlace):
// the form tag's own attributes (the Asks card's htmx post, a session page's
// hx-boost), a session page's return field, and a Dismiss in the answer row.
func placeFree(form string) string {
	form = regexp.MustCompile(`^<form [^>]*>`).ReplaceAllString(form, "<form>")
	form = regexp.MustCompile(`<input type="hidden" name="return" value="[^"]*">`).ReplaceAllString(form, "")
	return regexp.MustCompile(`<button type="submit" form="ask-9-dismiss"[^>]*>.*?</button>`).ReplaceAllString(form, "")
}

// TestTheAsksCardAndASessionBlockDrawOneAnswerForm (the design review, 7):
// an ask's answer form is THE SAME on the Asks page's card and on a session
// page's asks block — options full width first, the one-line growing box with
// Answer ending its row (quiet beside options) — differing only in what each
// place adds (askFormPlace). RED at origin/main: the block drew its own form
// (options as wide as their words, Answer in a row of its own, a wide ↑).
func TestTheAsksCardAndASessionBlockDrawOneAnswerForm(t *testing.T) {
	items := []attentionItem{{Key: "ask-9", Kind: attentionKindAsk, AskID: 9, Text: "ship?", Options: []string{"yes", "no"}}}
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	card := answerFormOf(t, attentionItemsHTMLTo(items, now, ""))
	block := answerFormOf(t, attentionItemsHTMLTo(items, now, "/agents/a"))
	if placeFree(card) != placeFree(block) {
		t.Fatalf("the card's and the block's answer forms differ:\ncard:  %s\nblock: %s", placeFree(card), placeFree(block))
	}
	for _, want := range []string{
		`class="` + ui.AsksOptionClass + `" data-attention-option>yes</button>`,
		`<div class="` + ui.AskFormBoxRowClass + `" data-asks-answer-row><textarea id="answer-9"`,
		`<button type="submit" id="answer-9-submit" class="` + ui.AsksCardSendQuietClass + `">Answer</button>`,
		`rows="1"`, `class="` + ui.AskFormInputClass + `"`, `<label for="answer-9" class="sr-only">Your answer</label>`,
	} {
		if !strings.Contains(card, want) {
			t.Errorf("the answer form lacks %q:\n%s", want, card)
		}
	}
	if !strings.HasPrefix(ui.AskFormInputClass, ui.AskFormFitClass+" ") || !strings.Contains(ui.AsksOptionClass, "w-full") {
		t.Errorf("the box does not grow, or an option is not full width: %q / %q", ui.AskFormInputClass, ui.AsksOptionClass)
	}
}

// TestTheAnswerFormsTwoPostsAreUnchanged: Answer submits the answer form to
// /answer; Dismiss is a button that belongs, by form=, to a SEPARATE form
// posting to /cancel (no formaction, so a dismissal never carries the typed
// answer), with the same return field and boost attribute on a session page.
// WHERE Dismiss sits: never the head; the card's footer when the footer has
// links, else the end of the answer row (no footer of a lone ✕: the design
// review); a session page's block, the answer row.
func TestTheAnswerFormsTwoPostsAreUnchanged(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	linked := attentionItem{Key: "ask-9", Kind: attentionKindAsk, AskID: 9, Text: "ship?", Options: []string{"yes", "no"}, sessionLink: "/agents/a"}
	bare := linked
	bare.sessionLink = ""
	for _, c := range []struct {
		name     string
		it       attentionItem
		returnTo string
		inRow    bool
	}{
		{"a card with View in session", linked, "", false},
		{"a card with nothing to link", bare, "", true},
		{"a session page's block", linked, "/agents/a", true},
	} {
		out := attentionItemsHTMLTo([]attentionItem{c.it}, now, c.returnTo)
		answer := answerFormOf(t, out)
		row := regexp.MustCompile(`(?s)<div class="` + regexp.QuoteMeta(ui.AskFormBoxRowClass) + `" data-asks-answer-row>.*?</div>`).FindString(answer)
		if got := strings.Contains(row, `form="ask-9-dismiss"`); got != c.inRow {
			t.Errorf("%s: Dismiss in the answer row = %v, want %v:\n%s", c.name, got, c.inRow, out)
		}
		footer := regexp.MustCompile(`(?s)<div class="[^"]*" data-asks-footer>.*?</div>`).FindString(out)
		if inFooter := strings.Contains(footer, `form="ask-9-dismiss"`); c.returnTo == "" && inFooter == c.inRow {
			t.Errorf("%s: Dismiss in the footer = %v, want %v:\n%s", c.name, inFooter, !c.inRow, out)
		}
		if c.returnTo == "" && !c.inRow && footer == "" {
			t.Errorf("%s: no footer", c.name)
		}
		if c.returnTo == "" && c.inRow && footer != "" {
			t.Errorf("%s: a footer of a lone ✕:\n%s", c.name, footer)
		}
		if !regexp.MustCompile(`<button type="submit" form="ask-9-dismiss" aria-label="Dismiss without answering" title="Dismiss without answering"[^>]*>` +
			regexp.QuoteMeta(ui.AsksDismissIcon) + `</button>`).MatchString(out) {
			t.Errorf("%s: no icon Dismiss named \"Dismiss without answering\":\n%s", c.name, out)
		}
		if strings.Contains(answer, "formaction") {
			t.Errorf("%s: the answer form carries a formaction", c.name)
		}
		head := regexp.MustCompile(`(?s)<button type="button"[^>]*data-asks-toggle>.*?</button>`).FindString(out)
		if strings.Contains(head, "ask-9-dismiss") {
			t.Errorf("%s: Dismiss in the head", c.name)
		}
		dismiss := regexp.MustCompile(`<form id="ask-9-dismiss" method="POST" action="/attention/asks/9/cancel" hidden([^>]*)>([^<]*(?:<input[^>]*>)?)</form>`).FindStringSubmatch(out)
		if dismiss == nil {
			t.Fatalf("%s: no dismiss form of that id posting to /cancel:\n%s", c.name, out)
		}
		if c.returnTo != "" && (!strings.Contains(dismiss[1], `hx-boost="false"`) || !strings.Contains(dismiss[2], `name="return" value="/agents/a"`)) {
			t.Errorf("%s: the dismiss form lost its boost or its return: %q", c.name, dismiss[0])
		}
		if c.returnTo != "" && (!strings.Contains(answer, `hx-boost="false"`) || !strings.Contains(answer, `name="return" value="/agents/a"`)) {
			t.Errorf("%s: the answer form lost its boost or its return:\n%s", c.name, answer)
		}
	}
}
