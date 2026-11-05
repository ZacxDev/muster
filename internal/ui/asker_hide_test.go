package ui

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestTheAsksShowOneCardAtATimeEachClampedInPlace pins ONE AT A TIME, CLAMPED
// as a RELATIONSHIP across its three places: the list's markup (AskerAsksList),
// the stylesheet's rules (web/css/input.css) and the script that flips them
// (askerAsksHideScript). Each names the same attributes; a rename in one that
// the others miss would leave every card shown, or every text clamped with no
// way to open it, and no single file's test would see it. e2e
// chief-mobile.spec.ts drives the behaviour at 412px.
func TestTheAsksShowOneCardAtATimeEachClampedInPlace(t *testing.T) {
	one := AskerAsksList(`<li id="ask-1" data-attention-item></li>`, 1)
	if strings.Contains(one, "data-asks-collapsed") || strings.Contains(one, "data-asks-more") {
		t.Errorf("one ask is collapsed behind a \"more\": %s", one)
	}
	three := AskerAsksList(`<li id="ask-3" data-attention-item></li><li id="ask-2" data-attention-item></li><li id="ask-1" data-attention-item></li>`, 3)
	if !strings.Contains(three, `data-testid="asker-asks" data-asks-collapsed>`) {
		t.Errorf("three asks are not collapsed: %s", three)
	}
	if !regexp.MustCompile(`<button type="button" data-asks-more class="[^"]*min-h-\[44px\][^"]*">Show 2 more</button>$`).MatchString(three) {
		t.Errorf("no \"Show 2 more\" after the list: %s", three)
	}

	css, err := os.ReadFile("../../web/css/input.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`#` + AskerAsksID + ` [data-asks-collapsed] > [data-attention-item]:not(:first-child) {`,
		`#` + AskerAsksID + ` [data-attention-item] [data-attention-text]:not([data-ask-open]) {`,
		`-webkit-line-clamp: 4;`,
		// One card shows: the cap rises so a real card fits whole (AskerAsksBlock).
		`#` + AskerAsksID + `:has(> [data-asks-collapsed]),`, `#` + AskerAsksID + `:has(> ul > [data-attention-item]:only-child) {`, `max-height: 65dvh;`,
	} {
		if !strings.Contains(string(css), want) {
			t.Errorf("web/css/input.css lacks %q", want)
		}
	}

	js := renderString(t, askerAsksHideScript())
	for _, want := range []string{
		`'[data-asks-collapsed]'`, `removeAttribute('data-asks-collapsed')`, `closest('[data-asks-more]')`,
		`'[data-attention-item] [data-attention-text]'`, `setAttribute('data-ask-open', '')`, `removeAttribute('data-ask-open')`,
		// "Show more" goes right after the text, so inside its card.
		`t.after(b)`, `'Show less' : 'Show more'`,
		// apply() fits the block whenever it shows it: first parse, every refresh.
		"else fit();",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("the script lacks %q", want)
		}
	}
}

// TestTheAsksHeadingCarriesAHideThatSendsNothing: the heading row's "Hide until
// next ask" is a plain type="button" — not a submit, not inside any form, no
// htmx verb — so pressing it cannot reach the queue's answer or dismiss route.
// The chip that reopens the block follows the block, drawn hidden, and so does
// the script; the script makes no request of any kind.
func TestTheAsksHeadingCarriesAHideThatSendsNothing(t *testing.T) {
	head := AskerAsksHeading("2 questions")
	btn := regexp.MustCompile(`<button type="button" data-asker-asks-hide class="[^"]*">` + askerAsksHideLabel + `</button>`).FindString(head)
	if btn == "" {
		t.Fatalf("no hide button in the heading:\n%s", head)
	}
	for _, banned := range []string{"<form", "hx-", "formaction", `type="submit"`} {
		if strings.Contains(head, banned) {
			t.Errorf("the heading carries %q", banned)
		}
	}
	if !strings.Contains(head, `data-asker-asks-heading>2 questions waiting</h2>`) {
		t.Errorf("the count is not in the heading:\n%s", head)
	}
	if !strings.Contains(AskerAsksHeading("<b>"), "&lt;b&gt;") {
		t.Error("the count is not escaped")
	}

	for _, viaHTMX := range []bool{true, false} {
		page := renderString(t, AskerAsksBlock(head+`<ul><li id="ask-5" data-attention-item>q</li></ul>`, "/attention/asker?agent=a", viaHTMX))
		block, chip, script := strings.Index(page, `id="`+AskerAsksID+`"`), strings.Index(page, `id="`+AskerAsksID+`-chip"`), strings.Index(page, "<script>")
		if block < 0 || chip < block || script < chip {
			t.Fatalf("viaHTMX=%v: block %d, chip %d, script %d — want block, then chip, then its script", viaHTMX, block, chip, script)
		}
		chipTag := page[chip:]
		chipTag = chipTag[:strings.Index(chipTag, ">")]
		if !strings.Contains(chipTag, `type="button"`) || !strings.Contains(chipTag, `hidden=""`) {
			t.Errorf("viaHTMX=%v: the chip is not a hidden plain button: %s", viaHTMX, chipTag)
		}
		js := page[script:]
		// A guard an htmx history snapshot carries (an attribute) leaves a
		// restored block unbound after Back.
		if strings.Contains(js, "-bound") {
			t.Errorf("viaHTMX=%v: the hide script guards its binding with an attribute", viaHTMX)
		}
		for _, banned := range []string{"fetch(", "XMLHttpRequest", "htmx.ajax", "sendBeacon", ".submit(", "requestSubmit"} {
			if strings.Contains(js, banned) {
				t.Errorf("viaHTMX=%v: the hide script makes a request (%s)", viaHTMX, banned)
			}
		}
		// Per ask id: the hidden set is the items' ids, and the block is hidden
		// only while every one of them is in it.
		for _, want := range []string{`'` + askerAsksHiddenKey + `'`, `querySelectorAll('[data-attention-item][id]')`, `if (d.indexOf(k[i]) < 0) hide = false;`,
			`new MutationObserver(apply).observe(block, { childList: true })`, `' waiting'`, `block.__musterHideBound`,
			`window.addEventListener('storage'`} {
			if !strings.Contains(js, want) {
				t.Errorf("viaHTMX=%v: the script lacks %q", viaHTMX, want)
			}
		}
	}
}
