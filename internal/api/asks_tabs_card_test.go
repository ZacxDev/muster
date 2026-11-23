package api

import (
	"context"
	"io"
	"log"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/ZacxDev/muster/internal/asklog"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/officers/builtin"
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
	var row []asksNode
	if head.has("data-asks-top") {
		if cls, _ := head.attr("class"); cls != ui.AsksCardTopClass {
			t.Errorf("%s: the top row's class %q", view, cls)
		}
		row = head.children()
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
	if head.has("data-asks-card-link") {
		// A card with a page: its head is the link to it, and a toggle's
		// expand is the chevron, a button of its own on the top row.
		want = ui.AsksCardLinkHeadClass
		if head.Data != "a" {
			t.Errorf("%s: the card's link head is a <%s>", view, head.Data)
		}
		chevron := false
		for _, c := range row {
			chevron = chevron || (c.Data == "button" && c.has("data-asks-toggle"))
		}
		if chevron != toggle {
			t.Errorf("%s: a chevron toggle on the top row %v, want %v", view, chevron, toggle)
		}
	} else if toggle {
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
	frag := closedFeedHTML(closedCardsOf(asks, []attentionItem{askItem(step), askItem(plain)}, nil), now)
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
	log := askLogCardHTML(asklog.Ask{ID: 9, Kind: asklog.KindAttention, SourceRef: "41", Question: "Ship it\u200b?\nTonight", AskedAt: now.Add(-time.Hour)}, now, asklog.DefaultTextTTL, "")
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

// assertNodeFlags: node (an element a test found by its data attribute)
// draws a visible flag for each of hidden, and never writes the hidden
// character itself. what names the node in the failure.
func assertNodeFlags(t *testing.T, what string, node asksNode, hidden ...rune) {
	t.Helper()
	if node.Node == nil {
		t.Errorf("%s: not drawn", what)
		return
	}
	flags := map[string]bool{}
	for _, m := range node.all("data-stepup-hidden") {
		v, _ := m.attr("data-stepup-hidden")
		if cls, _ := m.attr("class"); cls == ui.StepUpHiddenClass && m.Data == "mark" && m.text() == v {
			flags[v] = true
		}
	}
	for _, r := range hidden {
		cp := fmtCodePoint(r)
		if !flags[cp] {
			t.Errorf("%s: no visible flag for %s", what, cp)
		}
		if strings.ContainsRune(node.text(), r) {
			t.Errorf("%s: writes hidden character %U into the page", what, r)
		}
	}
}

// fmtCodePoint is r as the flag names it: "U+202E".
func fmtCodePoint(r rune) string {
	s := strings.ToUpper(strconv.FormatInt(int64(r), 16))
	for len(s) < 4 {
		s = "0" + s
	}
	return "U+" + s
}

// TestTheAnsweredCardFlagsHiddenCharsInItsQuestionAndAnswer: an answered
// card's question and its answer each draw a hidden character as a visible
// flag (askTextHTML), never as itself.
//
// Mutation-checked: askTextHTML reverted to html.EscapeString fails this
// test's question and answer assertions.
func TestTheAnsweredCardFlagsHiddenCharsInItsQuestionAndAnswer(t *testing.T) {
	now := designNow
	a := attention.Ask{ID: 8, Owner: 1, Question: "Ship it‮ tonight?", Answer: "Yes​, ship it", Status: attention.Answered,
		CreatedAt: now.Add(-time.Hour), ResolvedAt: now.Add(-time.Minute), Asker: attention.Asker{Label: "deploy-bot", AgentID: 3}, Delivery: attention.DeliveryDelivered}
	card := cardsOf(t, closedFeedHTML(closedCardsOf([]attention.Ask{a}, []attentionItem{askItem(a)}, nil), now))
	if len(card) != 1 {
		t.Fatalf("%d answered cards", len(card))
	}
	q, _ := card[0].first("data-attention-text")
	assertNodeFlags(t, "the answered card's question", q, '‮')
	ans, _ := card[0].first("data-attention-answer-text")
	assertNodeFlags(t, "the answered card's answer", ans, '​')
}

// TestTheAskPageFlagsHiddenCharsInItsQuestionAndAnswer: an answered ask's
// page draws its question and its answer with each hidden character a
// visible flag.
//
// Mutation-checked: askTextHTML reverted to html.EscapeString fails this
// test's question and answer assertions.
func TestTheAskPageFlagsHiddenCharsInItsQuestionAndAnswer(t *testing.T) {
	a := attention.Ask{ID: 36, Owner: 1, Asker: attention.Asker{AgentID: 7, Label: "Worker Seven"}, Question: "Rotate‮ the key?", Answer: "Rotate​ it now",
		Status: attention.Answered, CreatedAt: time.Now().Add(-time.Hour), ResolvedAt: time.Now()}
	page := parseAsks(t, askPageBody(t, whereServer(t, a), a, askPagePath(a.ID)))
	q, _ := page.first("data-ask-question")
	assertNodeFlags(t, "the ask page's question", q, '‮')
	ans, _ := page.first("data-ask-answer")
	assertNodeFlags(t, "the ask page's answer", ans, '​')
}

// TestTheOpenCardFlagsHiddenCharsInItsQuestion: an open ask's card on the
// Open feed draws its question with each hidden character a visible flag.
//
// Mutation-checked: askTextHTML reverted to html.EscapeString fails it.
func TestTheOpenCardFlagsHiddenCharsInItsQuestion(t *testing.T) {
	a := attention.Ask{ID: 37, Owner: 1, Asker: attention.Asker{AgentID: 7, Label: "Worker Seven"}, Question: "Merge‮ it?", Options: []string{"yes", "no"},
		Status: attention.Open, CreatedAt: designNow.Add(-time.Minute)}
	card := cardsOf(t, askFeedCardHTML(askItem(a), designNow))
	if len(card) != 1 {
		t.Fatalf("%d open cards", len(card))
	}
	q, _ := card[0].first("data-attention-text")
	assertNodeFlags(t, "the open card's question", q, '‮')
}

// TestADraftsQuotedMessageFlagsHiddenChars: the message a reply draft
// answers — the outside party's text — is drawn in its quote box with each
// hidden character a visible flag.
//
// Mutation-checked: askTextHTML reverted to html.EscapeString fails it.
func TestADraftsQuotedMessageFlagsHiddenChars(t *testing.T) {
	agent := int64(4)
	reply := plugins.DraftCard{Draft: plugins.Draft{ID: 23, AgentID: &agent, AgentName: "chief", Body: "Soon.", Status: plugins.DraftPending,
		CreatedAt: designNow.Add(-time.Minute), ExpiresAt: designNow.Add(time.Hour), CheckState: plugins.CheckSendable},
		Message: plugins.Message{ID: 6, Sender: "dana@example.com", Body: "Pay ‮gro.live‬ now"}, PluginLabel: "Example inbox"}
	card := cardsOf(t, draftListHTML([]plugins.DraftCard{reply}, draftsPendingEmpty))
	if len(card) != 1 {
		t.Fatalf("%d draft cards", len(card))
	}
	body, _ := card[0].first("data-draft-quote-body")
	assertNodeFlags(t, "the draft's quoted message", body, '‮', '‬')
}

// chatDraftStore is the plugin store, played: DraftCard answers card.
type chatDraftStore struct {
	plugins.Store
	card plugins.DraftCard
}

func (c chatDraftStore) DraftCard(context.Context, int64) (plugins.DraftCard, error) {
	return c.card, nil
}

// TestAChatConfirmCardQuotesWhatAReplyAnswers: an officer chat's confirm card
// for an outbound REPLY draft quotes the message it answers — the outside
// party's, in its own box, marked untrusted — and, when muster holds no
// message with that reference and its officer gave no context, says the post
// it answers was not provided and to check the target before approving:
// the Drafts tab's card's own part (draftAnswersHTML), not a copy.
//
// RED at 23adc35: the chat card drew the preview and Details only.
func TestAChatConfirmCardQuotesWhatAReplyAnswers(t *testing.T) {
	plugin, venue, kind, ref := int64(3), "x", plugins.TargetReply, "1890"
	d := plugins.Draft{ID: 24, PluginID: &plugin, Venue: &venue, TargetKind: &kind, TargetRef: &ref, Body: "Thanks — glad it helped.",
		Status: plugins.DraftPending, CreatedAt: designNow, ExpiresAt: designNow.Add(time.Hour), CheckState: plugins.CheckSendable}
	known := plugins.DraftCard{Draft: d, PluginLabel: "Acme social", ReplyTo: &plugins.Message{ID: 9, Sender: "@dana", Body: "Did‮ it ship?"}}
	unknown := plugins.DraftCard{Draft: d, PluginLabel: "Acme social"}
	cc := builtin.ChatCard{ID: 1, Kind: builtin.CardDraft, Officer: "marketing", DraftID: d.ID, BodySHA256: plugins.DraftBodySHA256(d.Body), MediaSHA256: []string{}}

	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	s.ext.Plugins = chatDraftStore{card: known}
	raw := s.chatCardHTML(context.Background(), officerScope{}, cc)
	card := parseAsks(t, raw)
	quote, ok := card.first("data-draft-reply-to")
	if !ok {
		t.Fatalf("the chat card does not quote the message the reply answers:\n%s", raw)
	}
	if _, ok := quote.first("data-draft-untrusted"); !ok {
		t.Error("the quoted message is not marked untrusted")
	}
	if from, _ := quote.first("data-draft-quote-from"); from.Node == nil || !strings.Contains(from.text(), "@dana") {
		t.Error("the quote does not say who it is from")
	}
	body, _ := quote.first("data-draft-quote-body")
	assertNodeFlags(t, "the chat card's quoted message", body, '‮')
	if _, ok := card.first("data-draft-reply-to-missing"); ok {
		t.Error("a known message also drew the no-such-reply warning")
	}
	if _, ok := card.first("data-chat-card-body"); !ok {
		t.Error("the chat card lost its preview body")
	}

	s.ext.Plugins = chatDraftStore{card: unknown}
	card = parseAsks(t, s.chatCardHTML(context.Background(), officerScope{}, cc))
	// A reply with no context and no message on its thread: the card says
	// what it answers was not provided (draft_reply_context.go), never fetched.
	if w, ok := card.first("data-draft-context-missing"); !ok || w.text() != "The post this replies to was not provided, so it is not shown: check the target before you approve." {
		t.Errorf("an unknown target does not draw the warning")
	}
	if _, ok := card.first("data-draft-reply-to"); ok {
		t.Error("an unknown target drew a quote")
	}
}

// lookalikeHandle is a handle with a Cyrillic 'а' (U+0430) among Latin letters.
const lookalikeHandle = "@pаypal"

// assertMarked: node holds a look-alike mark whose text is want.
func assertMarked(t *testing.T, what string, node asksNode, want string) {
	t.Helper()
	if node.Node == nil {
		t.Errorf("%s: not drawn", what)
		return
	}
	for _, m := range node.all("data-stepup-lookalike") {
		if cls, _ := m.attr("class"); cls == ui.StepUpLookalikeClass && m.Data == "mark" && m.text() == want {
			return
		}
	}
	t.Errorf("%s: %q is not marked as look-alike", what, want)
}

// TestAPostPreviewMarksItsTargetHandle: the preview head's target handle
// ("Direct message to @…", "Follow @…") is the outside party's text: a
// look-alike handle is marked and a hidden character in it is a visible
// flag — on the page's preview and the feed's — even reaching the renderer
// directly (a card not made by NormalizeHandle); the note under the preview
// says it has look-alike letters.
//
// RED at 23adc35: the handle was only HTML-escaped.
func TestAPostPreviewMarksItsTargetHandle(t *testing.T) {
	for _, target := range []string{plugins.TargetDM, plugins.TargetFollow} {
		for _, feed := range []bool{false, true} {
			view := target + map[bool]string{false: " page", true: " feed"}[feed]
			c := &attention.StepUpCard{DraftID: 9, Venue: "X", Platform: "x", Target: target, Handle: lookalikeHandle, Preview: "Hi."}
			frag := parseAsks(t, stepUpPostHTML(c, feed))
			kind, _ := frag.first("data-stepup-post-kind")
			assertMarked(t, view+": the head's handle", kind, lookalikeHandle)
			if _, ok := frag.first("data-stepup-lookalike-note"); !ok {
				t.Errorf("%s: no look-alike note for the handle", view)
			}
			c.Handle = "@some​one"
			frag = parseAsks(t, stepUpPostHTML(c, feed))
			kind, _ = frag.first("data-stepup-post-kind")
			assertNodeFlags(t, view+": the head's handle", kind, '​')
			// CONTROL: a plain handle is drawn as itself, unmarked.
			c.Handle = "@someone"
			frag = parseAsks(t, stepUpPostHTML(c, feed))
			if kind, _ = frag.first("data-stepup-post-kind"); kind.all("data-stepup-lookalike") != nil || !strings.HasSuffix(kind.text(), " @someone") {
				t.Errorf("%s: a plain handle reads %q", view, kind.text())
			}
		}
	}
}

// TestADraftsDetailsMarkLookalikeTargets: the Details fold's To / Target,
// Because-of and Reference lines mark a look-alike word and flag a hidden
// character.
//
// RED at 23adc35: they flagged hidden characters only.
func TestADraftsDetailsMarkLookalikeTargets(t *testing.T) {
	plugin, venue, dm, reply := int64(3), "x", plugins.TargetDM, plugins.TargetReply
	handle, because, ref := lookalikeHandle, "x.com/pаypal/status/1", "pаypal.com/status/2"
	dmCard := plugins.DraftCard{Draft: plugins.Draft{ID: 25, PluginID: &plugin, Venue: &venue, TargetKind: &dm, TargetRef: &handle, BecauseRef: &because,
		ExternalRef: "mаil​-ref", Body: "Hi."}}
	replyCard := plugins.DraftCard{Draft: plugins.Draft{ID: 26, PluginID: &plugin, Venue: &venue, TargetKind: &reply, TargetRef: &ref, Body: "Hi."}}
	details := parseAsks(t, draftDetailsHTML(dmCard))
	to, _ := details.first("data-draft-target")
	assertMarked(t, "the To line", to, lookalikeHandle)
	bc, _ := details.first("data-draft-because")
	assertMarked(t, "the Because-of line", bc, because)
	xr, _ := details.first("data-draft-ref")
	assertNodeFlags(t, "the Reference line", xr, '​')
	if xr.Node != nil && len(xr.all("data-stepup-lookalike")) != 1 {
		t.Errorf("the Reference line: its look-alike word is not marked")
	}
	target, _ := parseAsks(t, draftDetailsHTML(replyCard)).first("data-draft-target")
	assertMarked(t, "the Target line", target, ref)
}

// TestADraftSearchResultShowsItsVenuePreview: a draft found by the search
// box is drawn as the Drafts tab draws it — the post it becomes on its venue
// (the feed's compact preview: logo, what it does, the text, a hidden
// character a flag) — never its text spelled out.
//
// RED at 23adc35: the text was one box through flows.VisibleEscape
// (visibleLines: \u{202e}).
func TestADraftSearchResultShowsItsVenuePreview(t *testing.T) {
	plugin, venue, kind := int64(3), "x", plugins.TargetPost
	c := plugins.DraftCard{Draft: plugins.Draft{ID: 27, PluginID: &plugin, Venue: &venue, TargetKind: &kind, Body: draftFixtureBody,
		Status: plugins.DraftPending, CreatedAt: designNow.Add(-time.Minute), ExpiresAt: designNow.Add(time.Hour), AgentName: "w​riter"}, PluginLabel: "Acme social"}
	frag := draftSearchCardHTML(c, designNow)
	li := parseAsks(t, frag)
	text, ok := li.first("data-asks-search-text")
	if !ok {
		t.Fatalf("no text on the result:\n%s", frag)
	}
	post, ok := text.first("data-stepup-post")
	if !ok {
		t.Fatalf("the result has no venue preview:\n%s", frag)
	}
	if logo, _ := post.first("data-venue-logo"); logo.Node == nil {
		t.Error("the preview has no logo")
	} else if v, _ := logo.attr("data-venue-logo"); v != "x" {
		t.Errorf("the preview's logo %q", v)
	}
	if k, _ := post.first("data-stepup-post-kind"); k.Node == nil || k.text() != "New post" {
		t.Error("the preview does not say what it does there")
	}
	pt, _ := post.first("data-stepup-preview-text")
	assertNodeFlags(t, "the result's text", pt, '‮', '‬')
	title, _ := li.first("data-asks-search-title")
	assertNodeFlags(t, "the result's title", title, '​')
	assertNoSpelledEscapes(t, "search result", frag, '‮', '‬', '​')
}
