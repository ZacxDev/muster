package api

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/ui"
)

// TestACardPostIsAnsweredWithTheSettledCardOrItsRefusalFragment (round 0 of
// #768, D4): the Asks page's card forms post by htmx to the queue's OWN routes
// with asksCardHeader. Recorded: 200 and the card SETTLED — its id the open
// card's, so the feed's script puts it in the card's place (asks_done.go; it
// was a 204 and the card vanished). Refused: the error's status
// and one fragment (ui.AsksCardErrorHTML) with the queue's own sentence —
// never the whole page. Every other caller of the same routes is unchanged:
// without the header (a boosted form; a session page's form) the 303 back.
func TestACardPostIsAnsweredWithTheSettledCardOrItsRefusalFragment(t *testing.T) {
	f := newAttnFixture(t)
	card := map[string]string{"HX-Request": "true", asksCardHeader: "1"}
	post := func(id int64, verb string, v url.Values, hdr map[string]string) (int, string, string) {
		t.Helper()
		rd, ct := form(v)
		w := f.call(t, http.MethodPost, "/attention/asks/"+strconv.FormatInt(id, 10)+"/"+verb, rd, merge(merge(f.operator(), ct), hdr))
		return w.Code, w.Body.String(), w.Header().Get("Location")
	}

	// settledCard asserts body is ask id's card settled: its own id, closed
	// as status, its outcome.
	settledCard := func(what string, id int64, body, status, state string) {
		t.Helper()
		open := `<li id="ask-` + strconv.FormatInt(id, 10) + `" `
		if !strings.HasPrefix(body, open) || strings.Count(body, "<li ") != 1 || !strings.Contains(body, " data-asks-settled") ||
			!strings.Contains(body, `data-status="`+status+`"`) || !strings.Contains(body, `data-asks-outcome-state>`+state+`</p>`) {
			t.Fatalf("%s: not the settled card (%s, %q): %s", what, status, state, body)
		}
		// Nothing to answer on it any more.
		if strings.Contains(body, "<form") && strings.Contains(body, "/answer") {
			t.Fatalf("%s: the settled card still draws an answer form: %s", what, body)
		}
	}
	// Answered from a card: 200, the settled card, and it IS recorded.
	a := f.ask(t, f.opTok, "Ship it?", "ship", "wait")
	code0, body0, _ := post(a, "answer", url.Values{"option": {"ship"}}, card)
	if code0 != http.StatusOK {
		t.Fatalf("a card's answer: %d %q, want 200 and the settled card", code0, body0)
	}
	settledCard("a card's answer", a, body0, "answered", "Answered")
	if !strings.Contains(body0, `data-attention-answer-text>Your answer: ship</div>`) || !strings.Contains(body0, `data-asks-ask-link`) {
		t.Fatalf("the settled card lacks its answer or its page's link: %s", body0)
	}
	if got := f.askStatus(t, a); got.Status != attention.Answered || got.Answer != "ship" {
		t.Fatalf("the card's answer was not recorded: %+v", got)
	}
	// Again: refused, 409, and exactly the fragment saying why.
	code, body, _ := post(a, "answer", url.Values{"option": {"wait"}}, card)
	if want := ui.AsksCardErrorHTML("That question is no longer open (it is answered)."); code != http.StatusConflict || body != want {
		t.Fatalf("a refused card answer: %d %q, want 409 %q", code, body, want)
	}
	// An unknown ask: 404 and its fragment.
	if code, body, _ := post(a+100000, "cancel", url.Values{"x": {""}}, card); code != http.StatusNotFound || body != ui.AsksCardErrorHTML("No such question.") {
		t.Fatalf("a card dismissal of no ask: %d %q", code, body)
	}

	// Dismissed from a card: 200, the settled card, and cancelled.
	d := f.ask(t, f.opTok, "Rotate it?")
	code1, body1, _ := post(d, "cancel", url.Values{"x": {""}}, card)
	if code1 != http.StatusOK {
		t.Fatalf("a card's dismissal: %d %q", code1, body1)
	}
	settledCard("a card's dismissal", d, body1, "cancelled", "Dismissed")
	if got := f.askStatus(t, d); got.Status != attention.Cancelled {
		t.Fatalf("the card's dismissal was not recorded: %+v", got)
	}

	// Every other caller keeps its redirect: htmx without the card header,
	// a boosted form with it, a session page's form with it — on the Asks
	// page, to the ask drawn settled in its old spot (settledLink).
	for name, hdr := range map[string]map[string]string{
		"no card header": {"HX-Request": "true"},
		"boosted":        {"HX-Request": "true", "HX-Boosted": "true", asksCardHeader: "1"},
	} {
		id := f.ask(t, f.opTok, "Plain? "+name)
		want := "/attention?settled=" + strconv.FormatInt(id, 10) + "#ask-" + strconv.FormatInt(id, 10)
		if code, _, loc := post(id, "answer", url.Values{"answer": {"yes"}}, hdr); code != http.StatusSeeOther || loc != want {
			t.Errorf("%s: %d -> %q, want 303 to %s", name, code, loc, want)
		}
	}
	id := f.ask(t, f.opTok, "From a session page?")
	if code, _, loc := post(id, "answer", url.Values{"answer": {"yes"}, "return": {askPagePath(id)}}, card); code != http.StatusSeeOther || loc != askPagePath(id) {
		t.Errorf("a session page's form: %d -> %q, want 303 to its page", code, loc)
	}
	// A refusal without the header is still the queue page with its alert.
	if code, body, _ := post(a, "answer", url.Values{"option": {"wait"}}, nil); code != http.StatusConflict || !strings.Contains(body, `data-testid="attention-page"`) {
		t.Errorf("a plain refused answer: %d, want 409 and the page", code)
	}
}

// TestACardsFormsCarryTheirHtmxPost: an ask card's answer form and its
// dismiss form (the Asks page) post by htmx to their own routes with nothing
// swapped, tagged with the card's key; a step-up gated form does not (its
// confirm dialog submits it); the list drawn with a return (an officer's page)
// keeps plain forms.
func TestACardsFormsCarryTheirHtmxPost(t *testing.T) {
	items := designItems()
	out := attentionItemsHTML(items, designNow)
	li := itemLi(t, out, "ask-1")
	for _, want := range []string{
		`<form method="POST" action="/attention/asks/1/answer" class="flex min-w-0 flex-col gap-2" data-attention-answer data-asks-controls hx-post="/attention/asks/1/answer" hx-swap="none" hx-headers='{"X-Muster-Asks-Card": "1"}' data-asks-card-form="ask-1">`,
		`<form id="ask-1-dismiss" method="POST" action="/attention/asks/1/cancel" hidden hx-post="/attention/asks/1/cancel" hx-swap="none" hx-headers='{"X-Muster-Asks-Card": "1"}' data-asks-card-form="ask-1"></form>`,
	} {
		if !strings.Contains(li, want) {
			t.Errorf("the card lacks %q:\n%s", want, li)
		}
	}
	// No empty error slot drawn (the design review: it was a gap on every
	// card); the feed's script makes it when a post is refused.
	if strings.Contains(li, `data-asks-card-error`) {
		t.Errorf("the card draws an empty error slot:\n%s", li)
	}
	items[0].stepUpWhy = "Sign in again"
	if li := itemLi(t, attentionItemsHTML(items, designNow), "ask-1"); strings.Contains(li, `data-attention-answer data-asks-controls hx-post`) || !strings.Contains(li, `data-step-up-form`) {
		t.Errorf("a step-up form posts by htmx:\n%s", li)
	}
	if li := itemLi(t, attentionItemsHTMLTo(designItems(), designNow, designReturn), "ask-1"); strings.Contains(li, "hx-post") {
		t.Errorf("a list drawn with a return posts by htmx:\n%s", li)
	}
}
