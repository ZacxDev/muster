package api

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/ui"
)

// TestTheAnswerFormIsCompactAndItsTwoPostsAreUnchanged: an ask's card — the
// Asks queue's and a session page's block, one form for both — draws the
// compact form (THE COMPACT ANSWER FORM, internal/ui/attention.go): options as
// wide as their words, a one-line box that grows (ui.AskFormFitClass), and
// Answer and Dismiss in ONE row. And the two posts are what they were: Answer
// submits the answer form to /answer; Dismiss is a button IN that row that
// belongs, by form=, to a SEPARATE form posting to /cancel (no formaction on
// the answer form, so a dismissal never carries the typed answer), with the
// same return field and the same boost attribute.
func TestTheAnswerFormIsCompactAndItsTwoPostsAreUnchanged(t *testing.T) {
	items := []attentionItem{{Key: "ask-9", Kind: attentionKindAsk, AskID: 9, Text: "ship?", Options: []string{"yes", "no"}}}
	for _, returnTo := range []string{"", "/agents/a"} {
		out := attentionItemsHTMLTo(items, time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC), returnTo)
		answer := regexp.MustCompile(`(?s)<form method="POST" action="/attention/asks/9/answer"[^>]*>.*?</form>`).FindString(out)
		if answer == "" {
			t.Fatalf("return %q: no answer form:\n%s", returnTo, out)
		}
		row := regexp.MustCompile(`<div class="` + regexp.QuoteMeta(ui.AskFormActionsClass) + `"><button type="submit" class="[^"]*">Answer</button><button type="submit" form="ask-9-dismiss" aria-label="Dismiss without answering"[^>]*>Dismiss</button></div>`)
		if !row.MatchString(answer) {
			t.Errorf("return %q: Answer and Dismiss are not one row inside the answer form:\n%s", returnTo, answer)
		}
		if strings.Contains(answer, "formaction") {
			t.Errorf("return %q: the answer form carries a formaction", returnTo)
		}
		dismiss := regexp.MustCompile(`<form id="ask-9-dismiss" method="POST" action="/attention/asks/9/cancel" hidden([^>]*)>([^<]*(?:<input[^>]*>)?)</form>`).FindStringSubmatch(out)
		if dismiss == nil {
			t.Fatalf("return %q: no dismiss form of that id posting to /cancel:\n%s", returnTo, out)
		}
		if returnTo != "" && (!strings.Contains(dismiss[1], `hx-boost="false"`) || !strings.Contains(dismiss[2], `name="return" value="/agents/a"`)) {
			t.Errorf("return %q: the dismiss form lost its boost or its return: %q", returnTo, dismiss[0])
		}
		for _, want := range []string{
			`rows="1"`, `class="` + ui.AskFormInputClass + `"`, `placeholder="Your answer…"`, `<label for="answer-9" class="sr-only">Your answer</label>`,
			`class="` + ui.AskFormOptionClass + `" data-attention-option>yes</button>`,
		} {
			if !strings.Contains(answer, want) {
				t.Errorf("return %q: the answer form lacks %q", returnTo, want)
			}
		}
		if !strings.HasPrefix(ui.AskFormInputClass, ui.AskFormFitClass+" ") || strings.Contains(ui.AskFormOptionClass, "w-full") {
			t.Errorf("the box does not grow, or an option is full width: %q / %q", ui.AskFormInputClass, ui.AskFormOptionClass)
		}
	}
}
