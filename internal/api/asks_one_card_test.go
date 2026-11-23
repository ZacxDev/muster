package api

import (
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/ZacxDev/muster/internal/asklog"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/sessionstate"
	"github.com/ZacxDev/muster/internal/ui"
)

// ONE CARD ON THE ASKS PAGE (the design review: the page drew three card
// designs — the feed card, the old card for drafts and the log, and the
// waiting-session row). The ledger: every <li> on the page that is not INSIDE
// a card is a card (data-asks-card), or one of the closed set of things that
// are not cards, each by its own attribute. A fourth card design — an <li>
// with neither — fails it, and so does a new non-card role nobody wrote down.
var asksNonCardItems = []string{
	"data-asks-empty",    // THE empty state (ui.AsksEmptyHTML): All clear, drafts waiting, a list's "nothing here"
	"data-asks-drafts",   // the Open feed's hidden drafts count (ui.AsksDraftsMarkerHTML)
	"data-asks-episodes", // the Open feed's wrapper of the stalled / waiting cards
}

// asksStrayItems is every <li> in doc that is not inside a card and is
// neither a card nor one of asksNonCardItems, as its opening tag; and how
// many cards it found.
func asksStrayItems(t *testing.T, doc string) (stray []string, cards int) {
	t.Helper()
	root, err := html.Parse(strings.NewReader("<ul>" + doc + "</ul>"))
	if err != nil {
		t.Fatal(err)
	}
	has := func(n *html.Node, key string) bool {
		for _, a := range n.Attr {
			if a.Key == key {
				return true
			}
		}
		return false
	}
	var walk func(n *html.Node, inCard bool)
	walk = func(n *html.Node, inCard bool) {
		if n.Type == html.ElementNode && n.Data == "li" && !inCard {
			card := has(n, "data-asks-card")
			ok := card
			for _, k := range asksNonCardItems {
				ok = ok || has(n, k)
			}
			if card {
				cards++
				inCard = true
			}
			if !ok {
				var b strings.Builder
				b.WriteString("<li")
				for _, a := range n.Attr {
					b.WriteString(" " + a.Key + `="` + a.Val + `"`)
				}
				stray = append(stray, b.String()+">")
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, inCard)
		}
	}
	walk(root, false)
	return stray, cards
}

// TestEveryAsksListItemIsACard: every producer of the Asks page's list items,
// with a fixture of each kind — the Open feed (asks, a held question, a
// waiting window, a task, a notice; the drafts' empty state and marker; All
// clear), a stalled / waiting card, the Answered feed and its empty line, a
// reply draft (pending, approved, outbound), an outreach batch, a nudge, a log
// row and a "most asked" row, and every list's empty line — draws only cards
// and the ledger's non-cards. With a positive control (a stray <li> IS found)
// and the card count asserted, so a checker that sees nothing cannot pass.
//
// RED at origin/main: drafts, outreach batches, nudges, log rows and the
// stalled / waiting rows carried no data-asks-card, and the empty lines were
// bare <li>s.
func TestEveryAsksListItemIsACard(t *testing.T) {
	now := designNow
	var b strings.Builder
	// A plugin pairing request is an Open card of its own (draftsTabItem).
	pair := attentionItem{Key: pairKey(9), Kind: attentionKindPlugin, Source: "plugin pairing", Text: "A plugin asks for a token", Link: pairLink(9), Since: now}
	open, _ := askFeedHTML(append(designItems(), pair), `<ul data-attention-episodes>`+
		episodeCardHTML("episode-1", sessionstate.StateNeedsYou, "Ledger cleanup", "", "", "", now.Add(-time.Hour), now)+
		episodeCardHTML("episode-2", sessionstate.StateStalled, "Billing", ui.AsksQuietCardClass, "", "", now.Add(-time.Hour), now)+`</ul>`, 2, now)
	b.WriteString(open)
	clear, _ := askFeedHTML(nil, "", 0, now)
	b.WriteString(clear)
	draftsOnly, _ := askFeedHTML(designItems()[5:6], "", 0, now)
	b.WriteString(draftsOnly)
	ans := attention.Ask{ID: 7, Owner: 1, Question: "Ship it?", Answer: "yes", Status: attention.Answered, CreatedAt: now.Add(-time.Hour), ResolvedAt: now.Add(-time.Minute),
		Asker: attention.Asker{Label: "deploy-bot", AgentID: 3}, Delivery: attention.DeliveryDelivered}
	b.WriteString(closedFeedHTML(closedCardsOf([]attention.Ask{ans}, []attentionItem{askItem(ans)}, nil), now))
	b.WriteString(closedFeedHTML(closedCardsOf(nil, nil, nil), now))
	agent := int64(4)
	pending := plugins.DraftCard{Draft: plugins.Draft{ID: 11, AgentID: &agent, AgentName: "marketing-acme-1", Body: "It is on by default.", Status: plugins.DraftPending,
		CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(24 * time.Hour), CheckState: plugins.CheckSendable},
		Message: plugins.Message{ID: 5, Sender: "dana@example.com", Body: "Is it on by default?"}, PluginLabel: "Example inbox", AgentDisplay: "Marketing"}
	approved := pending
	approved.Draft.ID, approved.Draft.Status = 12, plugins.DraftApproved
	b.WriteString(draftListHTML([]plugins.DraftCard{pending, approved}, draftsPendingEmpty))
	b.WriteString(draftListHTML(nil, draftsPendingEmpty))
	b.WriteString(outreachBatchCardHTML(batchGroup{id: "b1"}))
	b.WriteString(chiefNudgeCardHTML(chiefnudge.Decision{ID: 3, Decision: "nudge", Mode: chiefnudge.ModeShadow, DecidedAt: now.Add(-time.Minute), Title: "Billing"}, now))
	b.WriteString(askLogCardHTML(asklog.Ask{ID: 9, Kind: asklog.KindAttention, Question: "Ship it?", AskedAt: now.Add(-time.Hour)}, now, asklog.DefaultTextTTL, ""))
	b.WriteString(askLogSummaryHTML(asklog.Summary{Total: 2, Top: []asklog.TopQuestion{{Norm: "ship it", Count: 2}}}, asklog.DefaultTextTTL))
	b.WriteString(askLogSummaryHTML(asklog.Summary{Total: 1}, asklog.DefaultTextTTL))
	stray, cards := asksStrayItems(t, b.String())
	for _, s := range stray {
		t.Errorf("an Asks list item that is not a card: %s", s)
	}
	// 6 Open cards + a pairing request + 2 stalled + 1 answered + 2 drafts + 1 batch + 1 nudge + 1 log row + 1 most asked.
	if cards != 16 {
		t.Errorf("%d cards found, want 16 (the checker must see every producer's card)", cards)
	}
	// POSITIVE CONTROL: a fourth design IS caught.
	if s, _ := asksStrayItems(t, `<li class="rounded-xl bg-s1 p-3" data-asklog-row>x</li>`); len(s) != 1 {
		t.Fatalf("the checker missed a stray <li>: %v", s)
	}
}

// TestTheAsksPageIsCardsOnly is the ledger over the WHOLE rendered page
// (every tab's panel is in the document): an ask, the fixture's waiting
// window, a pending reply draft, and — on the Log tab — the ask log, so a new
// producer on the page that the unit fixture above does not know is caught.
func TestTheAsksPageIsCardsOnly(t *testing.T) {
	f := newAttnFixture(t)
	f.ask(t, f.opTok, "Ship it?")
	f.pendingDraft(t, "Thanks, it ships Friday.")
	for _, path := range []string{"/attention", "/attention?tab=drafts", "/attention?tab=answered"} {
		page := f.page(t, path)
		i := strings.Index(page, `data-testid="attention-page"`)
		if i < 0 {
			t.Fatalf("%s: no Asks page", path)
		}
		stray, cards := asksStrayItems(t, page[i:])
		for _, s := range stray {
			t.Errorf("%s: an Asks list item that is not a card: %s", path, s)
		}
		if cards < 3 {
			t.Errorf("%s: %d cards, want the ask, the waiting window and the draft at least", path, cards)
		}
	}
}

var _ = log.New(io.Discard, "", 0)
