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
		// One card shows: the cap rises so a real card fits whole (AskerAsksBlock)
		// — except on an agent's page (askerAsksSection's data-asks-page), where
		// the block stays inside its 40dvh cap and shrinks.
		`#` + AskerAsksID + `:not([` + askerAsksPageAttr + `]):has(> [data-asks-collapsed]),`,
		`#` + AskerAsksID + `:not([` + askerAsksPageAttr + `]):has(> ul > [data-attention-item]:only-child) {`, `max-height: 65dvh;`,
		// The heading's fold: folded, the block is its heading row alone.
		`#` + AskerAsksID + `[data-asks-folded] > :not(:first-child) {`,
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
		// The fold: a tap on the heading's button flips it, and fit() — on every
		// refresh — re-applies it to the block and the button's aria-expanded.
		`closest('[data-asks-fold]')) { folded = !folded; fit(); return; }`,
		`block.setAttribute('data-asks-folded', '')`, `f.setAttribute('aria-expanded', on ? 'false' : 'true')`,
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
	head := AskerAsksHeading("2 questions", 2)
	btn := regexp.MustCompile(`<button type="button" data-asker-asks-hide class="[^"]*">` + askerAsksHideLabel + `</button>`).FindString(head)
	if btn == "" {
		t.Fatalf("no hide button in the heading:\n%s", head)
	}
	for _, banned := range []string{"<form", "hx-", "formaction", `type="submit"`} {
		if strings.Contains(head, banned) {
			t.Errorf("the heading carries %q", banned)
		}
	}
	if !strings.Contains(head, `data-asker-asks-heading>2 questions waiting</span>`) {
		t.Errorf("the count is not in the heading:\n%s", head)
	}
	if !strings.Contains(AskerAsksHeading("<b>", 1), "&lt;b&gt;") {
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

// TestTheAsksHeadingFoldsOnlyWithMoreThanOneQuestion: with two or more
// questions the heading's text is a fold button (aria-expanded, controlling
// the block) that keeps the "N questions waiting" text; with one it is the
// plain heading, nothing to fold.
func TestTheAsksHeadingFoldsOnlyWithMoreThanOneQuestion(t *testing.T) {
	fold := regexp.MustCompile(`<button type="button" data-asks-fold aria-expanded="true" aria-controls="` + AskerAsksID + `" class="[^"]*min-h-\[44px\][^"]*">.*<span[^>]*data-asker-asks-heading>3 questions waiting</span></button>`)
	if three := AskerAsksHeading("3 questions", 3); !fold.MatchString(three) {
		t.Errorf("three questions: no fold button carrying the count:\n%s", three)
	}
	one := AskerAsksHeading("1 question", 1)
	if strings.Contains(one, "data-asks-fold") || !strings.Contains(one, `data-asker-asks-heading>1 question waiting</h2>`) {
		t.Errorf("one question: the heading folds, or lost its count:\n%s", one)
	}
}

// TestTheAgentPageAsksBlockShrinksInsideItsCap: on an agent's page (viaHTMX)
// the asks block is capped at 40dvh and SHRINKS (flex-shrink, min-h-0,
// scrolling inside), marked data-asks-page so the one-card raise skips it; the
// mirrored view's block (viaHTMX false) keeps its flex-none 45vh. And the
// transcript beside it keeps a floor (#chat-log's min-h), so what the asks
// block can take is what is left after the composer and that floor.
func TestTheAgentPageAsksBlockShrinksInsideItsCap(t *testing.T) {
	section := func(viaHTMX bool) string {
		html := renderString(t, askerAsksSection("x", "/attention/asker?agent=a", viaHTMX))
		return html[:strings.Index(html, ">")]
	}
	classes := func(tag string) map[string]bool {
		m := map[string]bool{}
		mm := regexp.MustCompile(`class="([^"]*)"`).FindStringSubmatch(tag)
		if mm == nil {
			t.Fatalf("no class on %s", tag)
		}
		for _, c := range strings.Fields(mm[1]) {
			m[c] = true
		}
		return m
	}
	page := section(true)
	pc := classes(page)
	for _, want := range []string{"max-h-[40dvh]", "min-h-0", "shrink", "overflow-y-auto"} {
		if !pc[want] {
			t.Errorf("the page's block lacks %s: %s", want, page)
		}
	}
	if pc["flex-none"] || pc["max-h-[45vh]"] || !strings.Contains(page, askerAsksPageAttr+`=""`) {
		t.Errorf("the page's block is still flex-none / 45vh, or unmarked: %s", page)
	}
	mirror := section(false)
	mc := classes(mirror)
	if !mc["flex-none"] || !mc["max-h-[45vh]"] || strings.Contains(mirror, askerAsksPageAttr) {
		t.Errorf("the mirrored view's block changed: %s", mirror)
	}

	pane := renderString(t, agentChatPane(AgentDetailView{ID: 1, Name: "w", Kind: "claude-code", AsksSrc: "/attention/asker?agent=w", Asks: "x"}))
	log := regexp.MustCompile(`<div id="chat-log"[^>]*>`).FindString(pane)
	if lc := classes(log); !lc["min-h-[7.5rem]"] || lc["min-h-0"] || !lc["flex-1"] {
		t.Errorf("#chat-log has no floor: %s", log)
	}
	// The order in the column: transcript, asks, composer.
	l, a, f := strings.Index(pane, `id="chat-log"`), strings.Index(pane, `id="`+AskerAsksID+`"`), strings.Index(pane, `id="chat-form"`)
	if !(l >= 0 && l < a && a < f) {
		t.Errorf("the pane's order: log %d, asks %d, form %d", l, a, f)
	}
}
