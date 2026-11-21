package api

import (
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/ZacxDev/muster/internal/asklog"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/ui"
)

// EVERY TAB OF THE ASKS PAGE DRAWS THE OPEN TAB'S CARD (the operator: "the
// other tabs in the Asks page still don't have the updated UI"). Open got the
// redesign — the card head with its id chip, the step-up card's venue preview
// — while Answered drew a step-up card as one muted block of its question,
// Drafts drew a "POST" chip, a "New post on X" label and a plain box of text
// with every non-ASCII character spelled out (\u{2014}, \u{202e}), and the
// Log and Nudges rows and the ask page drew head rows of their own.
//
// These tests read the cards by STRUCTURE — the shared renderer's data
// attributes and classes — never by their words.

// asksNode is a parsed fragment's helpers.
type asksNode struct{ *html.Node }

func parseAsks(t *testing.T, frag string) asksNode {
	t.Helper()
	root, err := html.Parse(strings.NewReader("<ul>" + frag + "</ul>"))
	if err != nil {
		t.Fatal(err)
	}
	return asksNode{root}
}

func (n asksNode) attr(key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

func (n asksNode) has(key string) bool { _, ok := n.attr(key); return ok }

// all is every element under n (n excluded) carrying attribute key.
func (n asksNode) all(key string) []asksNode {
	var out []asksNode
	var walk func(*html.Node)
	walk = func(m *html.Node) {
		for c := m.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode && (asksNode{c}).has(key) {
				out = append(out, asksNode{c})
			}
			walk(c)
		}
	}
	walk(n.Node)
	return out
}

func (n asksNode) first(key string) (asksNode, bool) {
	if a := n.all(key); len(a) > 0 {
		return a[0], true
	}
	return asksNode{}, false
}

// children is n's element children.
func (n asksNode) children() []asksNode {
	var out []asksNode
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode {
			out = append(out, asksNode{c})
		}
	}
	return out
}

// text is every text node under n, joined.
func (n asksNode) text() string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(m *html.Node) {
		if m.Type == html.TextNode {
			b.WriteString(m.Data)
		}
		for c := m.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n.Node)
	return b.String()
}

// assertSharedHead checks card (an [data-asks-card]) opens with THE head
// (ui.AsksHeadHTML): its first element is the head — or the top row
// (data-asks-top) whose first element is the head and which holds the id
// chip — and the head is the toggle's or the fixed card's, its who line the
// shared one. chip: whether the card has an id chip.
func assertSharedHead(t *testing.T, view string, card asksNode, toggle, chip bool) {
	t.Helper()
	kids := card.children()
	if len(kids) == 0 {
		t.Fatalf("%s: an empty card", view)
	}
	head := kids[0]
	if head.has("data-asks-top") {
		if cls, _ := head.attr("class"); cls != ui.AsksCardTopClass {
			t.Errorf("%s: the top row's class %q", view, cls)
		}
		row := head.children()
		head = row[0]
		got := false
		for _, c := range row[1:] {
			got = got || c.has("data-id-copy")
		}
		if got != chip {
			t.Errorf("%s: id chip on the top row %v, want %v", view, got, chip)
		}
	} else if chip {
		t.Errorf("%s: the card has no top row for its id chip", view)
	}
	if !head.has("data-asks-head") {
		t.Fatalf("%s: the card does not open with the shared head: <%s %v>", view, head.Data, head.Attr)
	}
	want := ui.AsksFixedHeadClass
	if toggle {
		want = ui.AsksCardHeadClass
		if !head.has("data-asks-toggle") || head.Data != "button" {
			t.Errorf("%s: the head is not the card's toggle", view)
		}
	}
	if cls, _ := head.attr("class"); cls != want {
		t.Errorf("%s: the head's class %q, want %q", view, cls, want)
	}
	who := false
	for _, c := range head.children() {
		if cls, _ := c.attr("class"); cls == ui.AsksCardWhoClass {
			who = true
		}
	}
	if !who {
		t.Errorf("%s: the head has no shared who line", view)
	}
}

// cardsOf is every [data-asks-card] of frag.
func cardsOf(t *testing.T, frag string) []asksNode {
	t.Helper()
	return parseAsks(t, frag).all("data-asks-card")
}

// assertNoSpelledEscapes: no text of frag spells a character out, and no
// hidden character of hidden is written into it.
func assertNoSpelledEscapes(t *testing.T, view, frag string, hidden ...rune) {
	t.Helper()
	txt := parseAsks(t, frag).text()
	for _, bad := range []string{`\u{`, `\n`, `\t`} {
		if strings.Contains(txt, bad) {
			t.Errorf("%s: its text spells out %q:\n%s", view, bad, txt)
		}
	}
	for _, r := range hidden {
		if strings.ContainsRune(frag, r) {
			t.Errorf("%s: writes hidden character %U into the page", view, r)
		}
	}
}

// assertHiddenFlag: frag draws a flag for each of cps.
func assertHiddenFlag(t *testing.T, view, frag string, cps ...string) {
	t.Helper()
	flags := map[string]bool{}
	for _, m := range parseAsks(t, frag).all("data-stepup-hidden") {
		v, _ := m.attr("data-stepup-hidden")
		if cls, _ := m.attr("class"); cls == ui.StepUpHiddenClass && m.Data == "mark" && m.text() == v {
			flags[v] = true
		}
	}
	for _, cp := range cps {
		if !flags[cp] {
			t.Errorf("%s: no visible flag for %s", view, cp)
		}
	}
}

// draftFixtureBody is a reply with real line breaks, an em dash, an emoji, a
// right-to-left override and a pop.
const draftFixtureBody = "Thanks for the note — it ships Friday 🎉\n\nPay \u202egro.live\u202c later."

// TestTheDraftsTabDrawsEachDraftAsItsPost: a draft card on the Drafts tab is
// the Open card's head (ui.AsksHeadHTML) over the step-up card's venue
// preview (stepUpPostHTML: the venue's logo, the text as paragraphs, its
// media), its technical facts in the shared Details fold, and its own
// decision (Approve and send, Reject with a reason); a reply draft keeps the
// message it answers, quoted and marked untrusted. The text is never spelled
// out: an em dash and an emoji are themselves, a bidi control a visible flag.
//
// RED at the branch base: a "POST"/"REPLY DRAFT" badge head with no shared
// structure, a quote box saying "New post on X", the text in one box through
// flows.VisibleEscape (\u{2014}, \u{202e}), the media facts in a fold of
// their own.
func TestTheDraftsTabDrawsEachDraftAsItsPost(t *testing.T) {
	now := designNow
	plugin, venue, kind := int64(3), "x", plugins.TargetPost
	outbound := plugins.DraftCard{Draft: plugins.Draft{ID: 21, PluginID: &plugin, Venue: &venue, TargetKind: &kind, Body: draftFixtureBody,
		Status: plugins.DraftPending, CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), CheckState: plugins.CheckSendable,
		MediaRefs: []string{"launch/hero.png"}, Media: []plugins.DraftMedia{{Ref: "launch/hero.png", ContentType: "image/png", Size: 4096, SHA256: strings.Repeat("d", 64)}}},
		PluginLabel: "Acme social"}
	agent := int64(4)
	reply := plugins.DraftCard{Draft: plugins.Draft{ID: 22, AgentID: &agent, AgentName: "chief", Body: draftFixtureBody, Status: plugins.DraftPending,
		CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), CheckState: plugins.CheckSendable},
		Message: plugins.Message{ID: 5, Sender: "dana@example.com", Body: "Is it shipping?\nThanks"}, PluginLabel: "Example inbox"}

	for _, c := range []struct {
		name  string
		card  plugins.DraftCard
		logo  string
		media int
	}{{"outbound post", outbound, "x", 1}, {"reply draft", reply, ui.VenueLogoFallback, 0}} {
		frag := draftListHTML([]plugins.DraftCard{c.card}, draftsPendingEmpty)
		cards := cardsOf(t, frag)
		if len(cards) != 1 {
			t.Fatalf("%s: %d cards", c.name, len(cards))
		}
		card := cards[0]
		assertSharedHead(t, c.name, card, false, true)
		if card.all("data-asks-badge") != nil {
			t.Errorf("%s: the card still carries a kind badge", c.name)
		}
		post, ok := card.first("data-stepup-post")
		if !ok || post.Data != "article" {
			t.Fatalf("%s: no venue preview on the card:\n%s", c.name, frag)
		}
		if cls, _ := post.attr("class"); cls != ui.StepUpPostClass {
			t.Errorf("%s: the preview's class %q", c.name, cls)
		}
		if logo, _ := post.first("data-venue-logo"); logo.Node == nil {
			t.Errorf("%s: the preview has no logo", c.name)
		} else if v, _ := logo.attr("data-venue-logo"); v != c.logo {
			t.Errorf("%s: logo %q, want %q", c.name, v, c.logo)
		}
		text, _ := post.first("data-stepup-preview-text")
		if text.Node == nil || len(text.children()) != 2 {
			t.Errorf("%s: the text is not its two paragraphs:\n%s", c.name, frag)
		} else if got := text.children()[0].text(); got != "Thanks for the note — it ships Friday 🎉" {
			t.Errorf("%s: the first paragraph reads %q", c.name, got)
		}
		if n := len(post.all("data-stepup-medium")); n != c.media {
			t.Errorf("%s: %d media drawn, want %d", c.name, n, c.media)
		}
		assertHiddenFlag(t, c.name, frag, "U+202E", "U+202C")
		assertNoSpelledEscapes(t, c.name, frag, '\u202e', '\u202c')
		if _, ok := card.first("data-stepup-hidden-note"); !ok {
			t.Errorf("%s: the card does not say it holds hidden characters", c.name)
		}
		for _, k := range []string{"data-draft-approve", "data-draft-reject", "data-draft-reason-input"} {
			if _, ok := card.first(k); !ok {
				t.Errorf("%s: the card lost %s", c.name, k)
			}
		}
		if c.media > 0 {
			details, ok := card.first("data-stepup-details")
			if !ok || details.Data != "details" {
				t.Fatalf("%s: no Details fold", c.name)
			}
			if _, ok := details.first("data-draft-medium-facts"); !ok {
				t.Errorf("%s: the media facts are not in the Details fold", c.name)
			}
			if n := len(card.all("data-stepup-details")) + len(card.all("data-draft-medium-details")); n != 1 {
				t.Errorf("%s: %d Details folds, want the one", c.name, n)
			}
		}
	}
	// The reply draft keeps the message it answers, quoted and untrusted.
	card := cardsOf(t, draftListHTML([]plugins.DraftCard{reply}, draftsPendingEmpty))[0]
	quote, ok := card.first("data-draft-original")
	if !ok {
		t.Fatal("the reply draft lost the message it answers")
	}
	if _, ok := quote.first("data-draft-untrusted"); !ok {
		t.Error("the quoted message is not marked untrusted")
	}
	if body, _ := quote.first("data-draft-quote-body"); body.Node == nil || body.text() != "Is it shipping?\nThanks" {
		t.Errorf("the quoted message is not its text with its line break")
	}
}

// TestTheAnsweredTabDrawsTheOpenCard: an answered card is the Open card's
// head (the toggle, the id chip on its top row) over the Open card's body
// (askCardQuestionHTML) — a step-up card's the venue preview, as on Open —
// then the answer, its line breaks kept and nothing spelled out.
//
// RED at the branch base: the answered card drew every question, a step-up
// card's included, as one muted block of text, and its head was its own.
func TestTheAnsweredTabDrawsTheOpenCard(t *testing.T) {
	now := designNow
	step := stepUpFixtureAsk(t, "Big news: acme is now acme-two.\nRead more on the blog.")
	step.Status, step.Answer, step.ResolvedAt, step.CreatedAt = attention.Answered, hostDecisionConfirm, now.Add(-time.Minute), now.Add(-time.Hour)
	plain := attention.Ask{ID: 8, Owner: 1, Question: "Ship it?\nTonight or Monday", Answer: "Monday —\nafter the freeze", Status: attention.Answered,
		CreatedAt: now.Add(-time.Hour), ResolvedAt: now.Add(-time.Minute), Asker: attention.Asker{Label: "deploy-bot", AgentID: 3}, Delivery: attention.DeliveryDelivered}
	asks := []attention.Ask{step, plain}
	frag := answeredFeedHTML(asks, []attentionItem{askItem(step), askItem(plain)}, now, nil)
	cards := cardsOf(t, frag)
	if len(cards) != 2 {
		t.Fatalf("%d answered cards", len(cards))
	}
	for i, card := range cards {
		assertSharedHead(t, "answered "+asks[i].Question[:4], card, true, true)
	}
	// The step-up card: the Open card's body for it — its feed preview.
	open := askFeedCardHTML(askItem(func() attention.Ask { a := step; a.Status = attention.Open; return a }()), now)
	if _, ok := parseAsks(t, open).first("data-stepup-feed"); !ok {
		t.Fatalf("the Open control has no step-up feed body:\n%s", open)
	}
	if feed, ok := cards[0].first("data-stepup-feed"); !ok {
		t.Errorf("the answered step-up card is not drawn as its post:\n%s", frag)
	} else if _, ok := feed.first("data-stepup-post"); !ok {
		t.Errorf("the answered step-up card has no venue preview:\n%s", frag)
	}
	ans, ok := cards[1].first("data-attention-answer-text")
	if !ok || !strings.Contains(ans.text(), "Monday —\nafter the freeze") {
		t.Errorf("the answer is not its text with its line break: %q", ans.text())
	}
	assertNoSpelledEscapes(t, "answered", frag)
}

// TestTheLogAndNudgesRowsDrawTheOpenHead: a Log row and a Nudges decision
// open with the Open card's head (fixed: they do not expand), their state
// pill and — a log row of an agent's question — its id chip on the top row;
// a hidden character in their text is a visible flag.
//
// RED at the branch base: both drew a head row of their own (no
// data-asks-head, no top row).
func TestTheLogAndNudgesRowsDrawTheOpenHead(t *testing.T) {
	now := designNow
	log := askLogCardHTML(asklog.Ask{ID: 9, Kind: asklog.KindAttention, SourceRef: "41", Question: "Ship it\u200b?\nTonight", AskedAt: now.Add(-time.Hour)}, now, asklog.DefaultTextTTL)
	cards := cardsOf(t, log)
	if len(cards) != 1 {
		t.Fatalf("%d log cards", len(cards))
	}
	assertSharedHead(t, "log", cards[0], false, true)
	if top := cards[0].children()[0]; len(top.all("data-asklog-outcome")) != 1 {
		t.Errorf("the log row's outcome pill is not on its top row")
	}
	assertHiddenFlag(t, "log", log, "U+200B")
	assertNoSpelledEscapes(t, "log", log, '\u200b')

	nudge := chiefNudgeCardHTML(chiefnudge.Decision{ID: 3, Decision: "nudge", Mode: chiefnudge.ModeShadow, DecidedAt: now.Add(-time.Minute), Title: "Billing",
		Message: "Keep going\u202e until merged", Reason: "It asked twice."}, now)
	cards = cardsOf(t, nudge)
	if len(cards) != 1 {
		t.Fatalf("%d nudge cards", len(cards))
	}
	assertSharedHead(t, "nudge", cards[0], false, false)
	if top := cards[0].children()[0]; len(top.all("data-nudge-mode")) != 1 {
		t.Errorf("the nudge's mode pill is not on its top row")
	}
	if d := cards[0].all("data-stepup-details"); len(d) != 1 || d[0].Data != "details" {
		t.Errorf("the nudge's facts are not in the shared Details fold")
	}
	assertHiddenFlag(t, "nudge", nudge, "U+202E")
	assertNoSpelledEscapes(t, "nudge", nudge, '\u202e')
}

// TestAClosedAskPageDrawsTheOpenHead: the page of an answered, dismissed or
// expired ask — a step-up card's included — opens with the Open card's head
// (badge, who and when, the id chip on its top row), its answer's line breaks
// kept and nothing spelled out.
//
// RED at the branch base: the page drew its badge row as a head of its own.
func TestAClosedAskPageDrawsTheOpenHead(t *testing.T) {
	now := time.Now()
	plain := attention.Ask{ID: 34, Owner: 1, Asker: attention.Asker{AgentID: 7, Label: "Worker Seven"}, Question: "Ship it?", Answer: "yes —\ntonight", CreatedAt: now.Add(-time.Hour)}
	step := stepUpFixtureAsk(t, "Big news.\n\nMore soon.")
	step.ID, step.Owner, step.CreatedAt = 35, 1, now.Add(-time.Hour)
	for _, st := range []attention.Status{attention.Answered, attention.Cancelled, attention.Expired} {
		for _, a := range []attention.Ask{plain, step} {
			a.Status = st
			s := whereServer(t, a)
			body := askPageBody(t, s, a, askPagePath(a.ID))
			card, ok := parseAsks(t, body).first("data-asks-page-card")
			if !ok {
				t.Fatalf("%s ask %d: no page card", st, a.ID)
			}
			assertSharedHead(t, string(st), card, false, true)
			if txt := card.text(); strings.Contains(txt, `\u{`) || strings.Contains(txt, `\n`) {
				t.Errorf("%s ask %d: the card spells a character out:\n%s", st, a.ID, txt)
			}
			if a.Answer != "" && !strings.Contains(card.text(), "yes —\ntonight") && a.StepUp == nil {
				t.Errorf("%s ask %d: the answer is not its text with its line break", st, a.ID)
			}
		}
	}
}
