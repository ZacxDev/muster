package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/asklog"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/ui"
)

// askLinkFixture is a question carrying a YouTube timestamp link, as a watch
// officer files one (its machine tag first).
const askLinkFixture = "[watch:dQw4w9WgXcQ#3] Does this change your view? https://youtu.be/dQw4w9WgXcQ?t=120"

// askLinkAnchor is the anchor that link must be drawn as: the one every
// rendered link is (ui's mdAnchor attributes), its clip range after it.
const askLinkAnchor = `<a href="https://youtu.be/dQw4w9WgXcQ?t=120" target="_blank" rel="noopener" class="text-accent underline decoration-accent/40 underline-offset-2 hover:text-fg">https://youtu.be/dQw4w9WgXcQ?t=120</a><span class="` + ui.AskClipClass + `" data-ask-clip>2:00</span>`

// textOf is the element carrying attr in frag, failing when there is none.
func textOf(t *testing.T, view, frag, attr string) asksNode {
	t.Helper()
	n, ok := parseAsks(t, frag).first(attr)
	if !ok {
		t.Fatalf("%s: no [%s] in:\n%s", view, attr, frag)
	}
	return n
}

// TestAskTextIsLinkedOnEveryAsksSurface: an ask's question — and an answer —
// carrying a link draws it as an anchor (with its clip range) on the Open
// card, the Answered card, the ask page (open and answered), a log row and a
// search result; the watch tag is kept verbatim; the text's box is a <div>
// (a list in it is a block, which a <p> cannot hold).
//
// RED at origin/main: every one of them drew the URL as escaped text.
func TestAskTextIsLinkedOnEveryAsksSurface(t *testing.T) {
	now := designNow
	open := attention.Ask{ID: 41, Owner: 1, Question: askLinkFixture, Status: attention.Open, CreatedAt: now.Add(-time.Hour),
		Asker: attention.Asker{Label: "watch officer", AgentID: 3}}
	answered := open
	answered.Status, answered.Answer, answered.ResolvedAt = attention.Answered, "Yes, see https://example.com/notes", now.Add(-time.Minute)
	answered.Delivery = attention.DeliveryDelivered

	surfaces := []struct {
		view, frag, attr string
		answer           string
	}{
		{"open card", askFeedCardHTML(askItem(open), now), "data-attention-text", ""},
		{"answered card", answeredFeedHTML([]attention.Ask{answered}, []attentionItem{askItem(answered)}, now, nil), "data-attention-text", "data-attention-answer-text"},
		{"ask page", askQuestionHTML(open), "data-ask-question", ""},
		{"ask page answered", askLiveHTML(answered, askItem(answered)), "", "data-ask-answer"},
		{"log row", askLogCardHTML(asklog.Ask{ID: 9, Kind: asklog.KindAttention, SourceRef: "41", Question: askLinkFixture, AskedAt: now.Add(-time.Hour)}, now, asklog.DefaultTextTTL, ""), "data-asklog-question", ""},
		{"search result", askSearchCardHTML(answered, now), "data-asks-search-text", "data-asks-search-answer"},
	}
	for _, s := range surfaces {
		if s.attr != "" {
			q := textOf(t, s.view, s.frag, s.attr)
			if q.Data != "div" {
				t.Errorf("%s: the question's box is a <%s>, not a <div>", s.view, q.Data)
			}
			if !strings.Contains(s.frag, askLinkAnchor) {
				t.Errorf("%s: the question's link is not its anchor:\n%s", s.view, s.frag)
			}
			if !strings.HasPrefix(q.text(), "[watch:dQw4w9WgXcQ#3] Does this") {
				t.Errorf("%s: the watch tag is not drawn as written: %q", s.view, q.text())
			}
		}
		if s.answer != "" {
			a := textOf(t, s.view, s.frag, s.answer)
			if len(a.all("href")) != 1 {
				t.Errorf("%s: the answer's link is not an anchor:\n%s", s.view, s.frag)
			}
		}
	}
}

// TestAStepUpPreviewIsNotMarkdown: a step-up card's post preview is drawn as
// the venue will post it — `**` and a URL are its text, never <strong> or an
// anchor — on the Open card, the ask page and a search result alike. An
// invariant guard: the preview never went through ask text, and this pins
// that it still does not.
func TestAStepUpPreviewIsNotMarkdown(t *testing.T) {
	a := stepUpFixtureAsk(t, "**Big** news: read https://example.com/post now")
	for view, frag := range map[string]string{
		"open card": askFeedCardHTML(askItem(a), designNow),
		"ask page":  askQuestionHTML(a),
		"search":    askSearchCardHTML(a, designNow),
	} {
		if strings.Contains(frag, "<strong>") || strings.Contains(frag, `href="https://example.com/post"`) {
			t.Errorf("%s: the post preview was drawn as markdown:\n%s", view, frag)
		}
		if !strings.Contains(frag, "**Big**") {
			t.Errorf("%s: the post's own ** is not its text:\n%s", view, frag)
		}
	}
}

// TestAQuotedMessageAndACommandAreNotMarkdown: an outside party's message a
// draft answers, and a command a session waits on, are drawn as written: a
// link there is text (the quote is untrusted), a `*` a `*`.
func TestAQuotedMessageAndACommandAreNotMarkdown(t *testing.T) {
	q := draftQuoteHTML("someone", "", "Click [here](https://example.com/x) **now**", "")
	if strings.Contains(q, "<a ") || strings.Contains(q, "<strong>") {
		t.Errorf("the quoted message was drawn as markdown:\n%s", q)
	}
	cmd := askFeedCardHTML(attentionItem{Kind: attentionKindSession, Key: "s-1", Text: "Bash: rm -rf *.tmp _cache_ https://example.com", tool: "Bash", Since: designNow}, designNow)
	if n := textOf(t, "command", cmd, "data-attention-text"); n.text() != "Bash: rm -rf *.tmp _cache_ https://example.com" || len(n.all("href")) != 0 {
		t.Errorf("a command was drawn as markdown:\n%s", cmd)
	}
}

// paged is items as the reader user 1 reads them: each ask's page set where
// they may open it (withAskPages, askHasPage).
func paged(items ...attentionItem) []attentionItem {
	withAskPages(asUser(1), items)
	return items
}

// TestOnlyAnAskTheReaderMayOpenHasAPage: an ask's or a notice's card links
// to its page only where handleAskPage would draw it for the reader
// (askHasPage, the one rule): their own ask — never one a session put to the
// chief, never another owner's (a team's), never with no reader; and no other
// kind of item has one.
func TestOnlyAnAskTheReaderMayOpenHasAPage(t *testing.T) {
	own := attention.Ask{ID: 7, Owner: 1, Question: "q"}
	chief, team := own, own
	chief.ID, chief.ToChief = 8, true
	team.ID, team.Owner = 9, 2
	notice := askItem(attention.Ask{ID: 10, Owner: 1})
	notice.Kind = attentionKindNotice
	items := []attentionItem{askItem(own), askItem(chief), askItem(team), notice, {Kind: attentionKindSession, AskID: 11, owner: 1}}
	withAskPages(asUser(1), items)
	got := []string{}
	for _, it := range items {
		got = append(got, it.page)
	}
	if want := []string{"/attention/asks/7", "", "", "/attention/asks/10", ""}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("pages %q, want %q", got, want)
	}
	// No reader (a system context): no page at all.
	none := []attentionItem{askItem(own)}
	withAskPages(context.Background(), none)
	if none[0].page != "" {
		t.Errorf("a page with no reader: %q", none[0].page)
	}
	// The ask page's own gate is the same rule.
	for _, c := range []struct {
		a    attention.Ask
		want bool
	}{{own, true}, {chief, false}, {team, false}} {
		if askHasPage(c.a, 1) != c.want {
			t.Errorf("askHasPage(%d) = %v", c.a.ID, !c.want)
		}
	}
}

// TestADraftSearchResultOpensItsCard: a draft's search result is its card's
// link too — to the draft's card on the Drafts tab (draftFocusLink), there
// being no draft page — one link, holding no control.
func TestADraftSearchResultOpensItsCard(t *testing.T) {
	c := plugins.DraftCard{Draft: plugins.Draft{ID: 484, AgentName: "e2e", Body: "Yes, noon works.", Status: plugins.DraftPending, CreatedAt: designNow}}
	cards := cardsOf(t, draftSearchCardHTML(c, designNow))
	if len(cards) != 1 {
		t.Fatalf("%d cards", len(cards))
	}
	links := cardLinks(cards[0])
	if len(links) != 1 {
		t.Fatalf("%d card links", len(links))
	}
	if href, _ := links[0].attr("href"); href != draftFocusLink(484) || links[0].Data != "a" {
		t.Errorf("the result links to %q, want %q", href, draftFocusLink(484))
	}
	if s := interactiveIn(links[0]); s != "" {
		t.Errorf("the link holds a %s", s)
	}
}

// cardLinks is every element of card carrying data-asks-card-link.
func cardLinks(card asksNode) []asksNode { return card.all("data-asks-card-link") }

// interactiveIn reports the first interactive element inside n, "" for none.
func interactiveIn(n asksNode) string {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		kid := asksNode{c}
		switch c.Data {
		case "a", "button", "input", "textarea", "select", "label", "summary", "details", "form":
			return c.Data
		}
		if kid.has("data-id-copy") || kid.has("tabindex") {
			return "[data-id-copy|tabindex]"
		}
		if s := interactiveIn(kid); s != "" {
			return s
		}
	}
	return ""
}

// TestACardWithAPageIsOneLinkToIt: every Asks card for an ask has exactly ONE
// link to that ask's page (data-asks-card-link, a real <a href>), holding no
// interactive element; on an expanding card the chevron is a separate button
// (data-asks-toggle) beside it — never inside it — and the card is otherwise
// unchanged. A card with no page of its own (a waiting session, a team ask
// the reader only watches) keeps its head as the expand toggle and has no
// card link.
//
// RED at origin/main: no card had a link to its page; the head was one
// <button> holding badge, who and chevron.
func TestACardWithAPageIsOneLinkToIt(t *testing.T) {
	now := designNow
	open := attention.Ask{ID: 41, Owner: 1, Question: "Ship it? https://example.com/pr", Status: attention.Open, CreatedAt: now.Add(-time.Hour),
		Options: []string{"Yes", "No"}, Asker: attention.Asker{Label: "deploy-bot", AgentID: 3}}
	answered := open
	answered.ID, answered.Status, answered.Answer, answered.ResolvedAt = 42, attention.Answered, "Yes", now.Add(-time.Minute)
	toChief := answered
	toChief.ID, toChief.ToChief = 43, true

	withPage := []struct {
		view, frag, href string
		toggle           bool
	}{
		{"open", askFeedCardHTML(paged(askItem(open))[0], now), "/attention/asks/41", true},
		{"answered", answeredFeedHTML([]attention.Ask{answered}, paged(askItem(answered)), now, nil), "/attention/asks/42", true},
		{"log", askLogCardHTML(asklog.Ask{ID: 9, Kind: asklog.KindAttention, SourceRef: "41", Question: "Ship it?", AskedAt: now}, now, asklog.DefaultTextTTL, askPagePath(41)), "/attention/asks/41", false},
		{"search", askSearchCardHTML(answered, now), "/attention/asks/42", false},
	}
	for _, c := range withPage {
		cards := cardsOf(t, c.frag)
		if len(cards) != 1 {
			t.Fatalf("%s: %d cards", c.view, len(cards))
		}
		links := cardLinks(cards[0])
		if len(links) != 1 {
			t.Fatalf("%s: %d card links, want 1:\n%s", c.view, len(links), c.frag)
		}
		l := links[0]
		if href, _ := l.attr("href"); l.Data != "a" || href != c.href {
			t.Errorf("%s: the card link is <%s href=%q>, want <a href=%q>", c.view, l.Data, href, c.href)
		}
		if s := interactiveIn(l); s != "" {
			t.Errorf("%s: the card link holds a %s:\n%s", c.view, s, c.frag)
		}
		if strings.TrimSpace(l.text()) == "" {
			t.Errorf("%s: the card link has no accessible name", c.view)
		}
		toggles := cards[0].all("data-asks-toggle")
		if c.toggle {
			if len(toggles) != 1 || toggles[0].Data != "button" {
				t.Fatalf("%s: %d toggles (want one <button>):\n%s", c.view, len(toggles), c.frag)
			}
			tg := toggles[0]
			if tg.has("data-asks-card-link") || tg.has("href") {
				t.Errorf("%s: the chevron is a link", c.view)
			}
			if name, _ := tg.attr("aria-label"); name != ui.AsksCardExpandLabel {
				t.Errorf("%s: the chevron's name %q", c.view, name)
			}
			if _, ok := tg.attr("aria-expanded"); !ok {
				t.Errorf("%s: the chevron has no aria-expanded", c.view)
			}
			for p := tg.Parent; p != nil; p = p.Parent {
				if p.Data == "a" {
					t.Errorf("%s: the chevron sits inside a link", c.view)
				}
			}
			// The chevron's description is the card's who line, inside the link.
			who, _ := tg.attr("aria-describedby")
			found := false
			for _, n := range l.all("id") {
				id, _ := n.attr("id")
				found = found || id == who
			}
			if !found {
				t.Errorf("%s: the chevron's description %q is not the link's who line", c.view, who)
			}
		} else if len(toggles) != 0 {
			t.Errorf("%s: a fixed card drew a toggle", c.view)
		}
	}

	// No page of its own: no card link, the head is the toggle as before.
	// A team's ask the reader only watches: its owner is not the reader.
	team := open
	team.Owner = 2
	watched := paged(askItem(team))[0]
	watched.watchOnly = TeamAskViewerNote
	without := map[string]string{
		"to the chief": answeredFeedHTML([]attention.Ask{toChief}, paged(askItem(toChief)), now, nil),
		"watched":      askFeedCardHTML(watched, now),
		"session":      askFeedCardHTML(attentionItem{Kind: attentionKindSession, Key: "s-1", Text: "Continue?", Since: now}, now),
		"log in chat":  askLogCardHTML(asklog.Ask{ID: 10, Kind: asklog.KindPlainText, Question: "Go?", AskedAt: now}, now, asklog.DefaultTextTTL, ""),
	}
	for view, frag := range without {
		cards := cardsOf(t, frag)
		if len(cards) != 1 {
			t.Fatalf("%s: %d cards", view, len(cards))
		}
		if n := len(cardLinks(cards[0])); n != 0 {
			t.Errorf("%s: a card with no page drew %d card links", view, n)
		}
	}
	for _, view := range []string{"to the chief", "watched", "session"} {
		assertSharedHead(t, view, cardsOf(t, without[view])[0], true, view != "session")
	}
}
