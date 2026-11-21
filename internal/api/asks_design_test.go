package api

import (
	"context"
	"io"
	"log"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE ASKS CARD FEED (operator decision "Vertical card feed"; asks_feed.go,
// ui/asks_card.go): every item is a card in one feed per tab — Open (questions,
// notices, waiting sessions, tasks to route, each with its kind badge),
// Answered, Drafts, Nudges, Log. A card's head is a toggle that expands it in
// place (never a link); its footer holds View in session, the task link and
// Dismiss. Every expected value here is a literal, not one read back from the
// code under test.

var designNow = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

// designItems is one of every kind the Open feed draws, plus a reply draft (the
// Drafts tab's, never the Open feed's).
func designItems() []attentionItem {
	since := designNow.Add(-9 * time.Minute)
	win := mirror.Place{Code: "fern:1", Host: "atlas"}
	return []attentionItem{
		{Key: "ask-1", Kind: attentionKindAsk, AskID: 1, Source: "deploy-bot", Text: "Deploy now?", Options: []string{"Deploy now", "Wait"}, Since: since,
			sessionLink: "/agents/deploy-bot", taskID: 42, suggestions: []string{"Deploy, then post the link"}},
		{Key: "ask-2", Kind: attentionKindAsk, AskID: 2, Source: "deploy-bot", Text: "Rotate the key?", Since: since},
		{Key: "session-aaaaaaaaaaaaaaaa", Kind: attentionKindQuestion, Source: "tmux fern:2 @ atlas", target: "tmux fern:2 @ atlas",
			at: mirror.Place{Code: "fern:2", Host: "atlas"}, Text: "Which database?\nPick one.", Link: "/mirror/view?host=atlas&session=fern&window=%402", Since: since},
		{Key: "session-bbbbbbbbbbbbbbbb", Kind: attentionKindSession, Source: "tmux fern:1 @ atlas", target: "tmux fern:1 @ atlas", at: win,
			Text: "Bash: npm test", Detail: "Running the tests.", Link: "/mirror/view?host=atlas&session=fern&window=%401", Since: since},
		{Key: "task-3", Kind: attentionKindTask, Source: "your chief has not routed it", Text: "Task #3: route it", Link: "/tasks/3", Since: since},
		{Key: "draft-4", Kind: attentionKindPlugin, Source: "Example inbox", Text: "someone: hi", Link: draftLink(4), Since: since},
		{Key: "ask-5", Kind: attentionKindNotice, AskID: 5, Source: "tmux fern:3 @ atlas", Text: "Waiting on you?", Since: since,
			sessionLink: "/mirror/view?host=atlas&session=fern&window=%403"},
	}
}

// itemLi is the <li> of key in out.
func itemLi(t *testing.T, out, key string) string {
	t.Helper()
	i := strings.Index(out, `<li id="`+key+`"`)
	if i < 0 {
		t.Fatalf("no item %s:\n%s", key, out)
	}
	li := out[i:]
	return li[:strings.Index(li, "</li>")+len("</li>")]
}

// cardBadgeLink is the same for a card with a page (an ask's, a notice's):
// its head is the link to the page, holding the badge and the who line, and
// the toggle is the chevron, a button of its own after it.
var cardBadgeLink = regexp.MustCompile(`<li id="([^"]+)"[^>]*data-asks-card[^>]*>\s*<div class="[^"]*" data-asks-top><a href="/attention/asks/[0-9]+" class="[^"]*" data-asks-head data-asks-card-link>(?:<span class="[^"]*" data-asks-badge="([a-z]+)">([^<]+)</span>)?<span id="([^"]+)".*?</a><button type="button" class="[^"]*" aria-expanded="false"(?: aria-controls="([^"]+)")? aria-label="Expand" aria-describedby="([^"]+)" data-asks-toggle>`)

var cardBadge = regexp.MustCompile(`<li id="([^"]+)"[^>]*data-asks-card[^>]*>\s*(?:<div class="[^"]*" data-asks-top>)?<button type="button" class="[^"]*" aria-expanded="false"(?: aria-controls="([^"]+)")? aria-label="Details" aria-describedby="([^"]+)" data-asks-head data-asks-toggle>(?:<span class="[^"]*" data-asks-badge="([a-z]+)">([^<]+)</span>)?<span id="([^"]+)"`)

// TestTheOpenFeedOrdersCardsByKindWithABadgeEach: one flat feed, in the
// order questions, notices, waiting sessions, tasks to route; each card opens
// with its head — a toggle naming its more region — whose first child is its
// kind badge (the old group header's words); no group headers; the reply
// draft is not on it. RED at origin/main: grouped lists under labels, no
// badge, no toggle.
func TestTheOpenFeedOrdersCardsByKindWithABadgeEach(t *testing.T) {
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	out := s.askListHTML(context.Background(), designItems(), asksRead{}, designNow)
	// Both shapes, in the feed's order: m is id, controls, describedby, badge
	// kind, badge words, who id.
	type hit struct {
		at int
		m  []string
	}
	var hits []hit
	for _, ix := range cardBadge.FindAllStringSubmatchIndex(out, -1) {
		m := cardBadge.FindStringSubmatch(out[ix[0]:])
		hits = append(hits, hit{ix[0], m})
	}
	for _, ix := range cardBadgeLink.FindAllStringSubmatchIndex(out, -1) {
		l := cardBadgeLink.FindStringSubmatch(out[ix[0]:])
		hits = append(hits, hit{ix[0], []string{l[0], l[1], l[5], l[6], l[2], l[3], l[4]}})
	}
	slices.SortFunc(hits, func(a, b hit) int { return a.at - b.at })
	var got []string
	for _, h := range hits {
		m := h.m
		// A toggle names its more region only when the card has one.
		if m[2] != "" && m[2] != m[1]+"-more" || m[2] != "" && !strings.Contains(out, `id="`+m[2]+`"`) {
			t.Errorf("%s's toggle controls %q", m[1], m[2])
		}
		// Named "Details", described by its who-line (its own id).
		if m[3] != m[1]+"-who" || m[6] != m[1]+"-who" {
			t.Errorf("%s's toggle is described by %q, its who-line is %q", m[1], m[3], m[6])
		}
		got = append(got, m[1]+"="+m[4]+":"+m[5])
	}
	// 🔴 A QUESTION CARRIES NO BADGE (the design review: "Question" on most
	// cards was noise): only the other kinds say what they are. RED at
	// origin/main, where every card had one.
	want := "ask-1=:|ask-2=:|session-aaaaaaaaaaaaaaaa=:|ask-5=notice:Notice|" +
		"session-bbbbbbbbbbbbbbbb=waiting:Waiting|task-3=task:Task to route"
	if strings.Join(got, "|") != want {
		t.Fatalf("cards %q,\nwant %q:\n%s", strings.Join(got, "|"), want, out)
	}
	for _, bad := range []string{`id="draft-4"`, "data-asks-group", "<h2"} {
		if strings.Contains(out, bad) {
			t.Errorf("the Open feed draws %q:\n%s", bad, out)
		}
	}
}

// TestTheOpenFeedIsAllClearWhenNothingWaits: nothing waiting and no stalled
// rows is the calm empty state — "All clear", the old sentence under it, its
// testid — and counts 0; a stalled row alone is not empty. RED at origin/main:
// a bare "Nothing is waiting on you." line.
func TestTheOpenFeedIsAllClearWhenNothingWaits(t *testing.T) {
	out, n := askFeedHTML(nil, "", 0, designNow)
	if n != 0 || !strings.Contains(out, `data-testid="attention-empty"`) || !strings.Contains(out, `>All clear</p>`) ||
		!strings.Contains(out, `>Nothing is waiting on you.</p>`) {
		t.Fatalf("the empty feed (%d): %s", n, out)
	}
	// Only a reply draft: still nothing on the Open feed.
	if _, n := askFeedHTML(designItems()[5:6], "", 0, designNow); n != 0 {
		t.Errorf("a draft alone counts %d on the Open feed", n)
	}
	out, n = askFeedHTML(nil, `<ul data-testid="attention-episodes"><li data-attention-episode></li></ul>`, 1, designNow)
	if n != 1 || strings.Contains(out, "All clear") || !strings.Contains(out, `<li class="min-w-0" data-asks-episodes><ul data-testid="attention-episodes">`) {
		t.Errorf("a stalled row alone (%d): %s", n, out)
	}
	if _, n := askFeedHTML(designItems(), "", 0, designNow); n != 6 {
		t.Errorf("the design items count %d, want 6", n)
	}
	if got := ui.AsksOpenCountHTML(6); !strings.Contains(got, `data-asks-open-count`) || !strings.Contains(got, `>6 open</span>`) {
		t.Errorf("the open count: %s", got)
	}
}

// TestEachCardDrawsTheActionsItsKindHas: per kind, what a card offers — an
// ask: its options, its suggested-reply chips, the answer box with the mic
// and Answer, View in session, its task, Dismiss; a free-text ask: the box
// (no options); a notice: View in session and Dismiss, no box; a held
// question: no box, "Answer in session", View in session; a waiting window:
// View in session, nothing to answer or dismiss; a task to route: "Open task",
// no box, no Dismiss. RED at origin/main: no chips, no task link, no View in
// session on the list, no footer.
func TestEachCardDrawsTheActionsItsKindHas(t *testing.T) {
	items := designItems()
	for i := range items {
		if items[i].Kind == attentionKindAsk {
			items[i].dictate = ui.TranscribePath
		}
	}
	out := attentionItemsHTML(items, designNow)
	has := map[string]string{
		"options": `data-attention-option>`, "chips": `data-asks-suggestion data-text=`, "box": `<textarea id="answer-`, "mic": `data-dictate-input="answer-`,
		"answer": `>Answer</button>`, "view": `data-asks-view-session>View in session</a>`, "task": `data-asks-task>`, "dismiss": `data-attention-dismiss>`,
		"held": `data-attention-held-cue>Answer in session</span>`,
	}
	order := []string{"options", "chips", "box", "mic", "answer", "view", "task", "dismiss", "held"}
	for key, want := range map[string]string{
		"ask-1":                    "options chips box mic answer view task dismiss",
		"ask-2":                    "box mic answer dismiss",
		"ask-5":                    "view dismiss",
		"session-aaaaaaaaaaaaaaaa": "view held",
		"session-bbbbbbbbbbbbbbbb": "view",
		"task-3":                   "task",
	} {
		li := itemLi(t, out, key)
		var got []string
		for _, k := range order {
			if strings.Contains(li, has[k]) {
				got = append(got, k)
			}
		}
		if strings.Join(got, " ") != want {
			t.Errorf("%s offers %q, want %q:\n%s", key, strings.Join(got, " "), want, li)
		}
	}
	// The links go where they say.
	for key, want := range map[string]string{
		"ask-1":                    `<a href="/agents/deploy-bot" class="` + ui.AsksCardLinkClass + `" data-asks-view-session>`,
		"ask-5":                    `<a href="/mirror/view?host=atlas&amp;session=fern&amp;window=%403" class="` + ui.AsksCardLinkClass + `" data-asks-view-session>`,
		"session-bbbbbbbbbbbbbbbb": `<a href="/mirror/view?host=atlas&amp;session=fern&amp;window=%401" class="` + ui.AsksCardLinkClass + `" data-asks-view-session>`,
		"task-3":                   `<a href="/tasks/3" class="` + ui.AsksCardLinkClass + `" data-asks-task>Open task</a>`,
	} {
		if li := itemLi(t, out, key); !strings.Contains(li, want) {
			t.Errorf("%s lacks %q:\n%s", key, want, li)
		}
	}
	if li := itemLi(t, out, "ask-1"); !strings.Contains(li, `<a href="/tasks/42" class="`+ui.AsksCardLinkClass+`" data-asks-task>Open task #42</a>`) {
		t.Errorf("the ask's task link:\n%s", li)
	}
	// A task card names the task, says why it is here, and reads the rest.
	task := itemLi(t, out, "task-3")
	for _, want := range []string{`data-attention-title>Task #3</span>`, `data-attention-text>route it</div>`, `data-asks-context><span>Your chief has not routed it yet.</span>`} {
		if !strings.Contains(task, want) {
			t.Errorf("the task card lacks %q:\n%s", want, task)
		}
	}
	// The waiting window: its pending call, and its last words as the context.
	wait := itemLi(t, out, "session-bbbbbbbbbbbbbbbb")
	if !strings.Contains(wait, `data-attention-text>Bash: npm test</div>`) || !strings.Contains(wait, `data-asks-context><span data-attention-detail>Running the tests.</span>`) {
		t.Errorf("the waiting card:\n%s", wait)
	}
	// A card with no page of its own (a waiting window, a task) has a head
	// that is its toggle, never a link: it is reached by its footer.
	for _, key := range []string{"session-bbbbbbbbbbbbbbbb", "task-3"} {
		li := itemLi(t, out, key)
		head := li[:strings.Index(li, "</button>")]
		if strings.Contains(head, "<a ") || !strings.Contains(head, "data-asks-toggle") {
			t.Errorf("%s's head is a link, or no toggle:\n%s", key, head)
		}
	}
	// An ask's and a notice's head is the link to its page, its chevron the
	// toggle beside it.
	for key, href := range map[string]string{"ask-1": "/attention/asks/1", "ask-5": "/attention/asks/5"} {
		li := itemLi(t, out, key)
		head := li[:strings.Index(li, "</button>")]
		if !strings.Contains(head, `<a href="`+href+`"`) || !strings.Contains(head, "</a><button") || !strings.Contains(head, "data-asks-toggle") {
			t.Errorf("%s's head is not its page's link beside its toggle:\n%s", key, head)
		}
	}
}

// TestTheAnswerFormIsQuietBesideOptions: with options to tap, Answer is the
// outlined button and the options are the primary action; with none, Answer
// is the filled primary. RED before this change (the filled Answer on every
// card was the loudest thing on the page).
func TestTheAnswerFormIsQuietBesideOptions(t *testing.T) {
	out := attentionItemsHTML(designItems(), designNow)
	if li := itemLi(t, out, "ask-1"); !strings.Contains(li, `id="answer-1-submit" class="`+ui.AsksCardSendQuietClass+` `+ui.AsksSendDisabledClass+`" aria-disabled="true" data-asks-send>Answer</button>`) {
		t.Errorf("Answer beside options is not quiet:\n%s", li)
	}
	if li := itemLi(t, out, "ask-2"); !strings.Contains(li, `id="answer-2-submit" class="`+ui.AsksCardSendClass+` `+ui.AsksSendDisabledClass+`" aria-disabled="true" data-asks-send>Answer</button>`) {
		t.Errorf("Answer with no options is not the primary:\n%s", li)
	}
}

// TestDismissIsASmallIconButtonWithAnAccessibleName: an ask's Dismiss and a
// notice's are icon buttons (the close glyph, no words) named by aria-label
// AND title, each joined by form= to its own /cancel form.
func TestDismissIsASmallIconButtonWithAnAccessibleName(t *testing.T) {
	out := attentionItemsHTML(designItems(), designNow)
	for key, name := range map[string]string{"ask-1": "Dismiss without answering", "ask-5": "Dismiss"} {
		li := itemLi(t, out, key)
		id := strings.TrimPrefix(key, "ask-")
		want := `<button type="submit" form="ask-` + id + `-dismiss" aria-label="` + name + `" title="` + name + `" class="` + ui.AsksIconButtonClass + `" data-attention-dismiss>` + ui.AsksDismissIcon + `</button>`
		if !strings.Contains(li, want) {
			t.Errorf("%s: no icon Dismiss named %q:\n%s", key, name, li)
		}
		if !strings.Contains(li, `<form id="ask-`+id+`-dismiss" method="POST" action="/attention/asks/`+id+`/cancel" hidden hx-post="/attention/asks/`+id+`/cancel"`) {
			t.Errorf("%s: its Dismiss has no /cancel form:\n%s", key, li)
		}
	}
}

// TestDismissIsInTheCardsFooterNeverItsHead (Round 0 of #620): the ✕ never
// sits in the head — a thumb aimed at the head (now: expand) must not cancel
// the ask. It is after the question: at the footer's end when the footer has
// links (ask-1: View in session), else at the answer row's end (ask-2: no
// footer of a lone ✕, the design review). The list drawn with a return (an
// officer's page) keeps the
// same rule.
func TestDismissIsInTheCardsFooterNeverItsHead(t *testing.T) {
	out := attentionItemsHTML(designItems(), designNow)
	for _, key := range []string{"ask-1", "ask-2", "ask-5"} {
		li := itemLi(t, out, key)
		head := li[:strings.Index(li, "</button>")]
		if strings.Contains(head, "data-attention-dismiss") {
			t.Errorf("%s: Dismiss is in the head:\n%s", key, head)
		}
		footer := regexp.MustCompile(`(?s)<div class="` + regexp.QuoteMeta(ui.AsksCardFooterClass) + `" data-asks-footer>(.*?)</div>`).FindStringSubmatch(li)
		row := regexp.MustCompile(`(?s)data-asks-answer-row>(.*?)</div>`).FindStringSubmatch(li)
		switch {
		case footer != nil && strings.Contains(footer[1], "<a "):
			if !strings.HasSuffix(footer[1], `</svg></button>`) || !strings.Contains(footer[1], "data-attention-dismiss") {
				t.Errorf("%s: Dismiss does not end the footer:\n%s", key, li)
			}
		case footer != nil:
			t.Errorf("%s: a footer with no links (a lone ✕):\n%s", key, li)
		case row == nil || !strings.HasSuffix(row[1], `</svg></button>`) || !strings.Contains(row[1], "data-attention-dismiss"):
			t.Errorf("%s: no footer, and Dismiss does not end the answer row:\n%s", key, li)
		}
		if d, txt := strings.Index(li, "data-attention-dismiss"), strings.Index(li, "data-attention-text"); d < txt {
			t.Errorf("%s: Dismiss comes before the question", key)
		}
	}
	block := attentionItemsHTMLTo(designItems(), designNow, designReturn)
	for _, key := range []string{"ask-1", "ask-2"} {
		li := itemLi(t, block, key)
		if d, txt := strings.Index(li, "data-attention-dismiss"), strings.Index(li, "data-attention-text"); d < 0 || d < txt {
			t.Errorf("a list drawn with a return, %s: Dismiss is not after the question:\n%s", key, li)
		}
	}
}

// designReturn is a page outside the Asks page that draws the answer form
// with a return: an officer's page (officer_needs_you.go).
const designReturn = officersPagePath + "/builtin:triage"

// TestAListWithAReturnKeepsItsCompactCard: the answer form drawn outside the
// Asks page (returnTo set: an officer's page) still draws the compact card —
// its box open beside its options, its forms posting back to that page with
// boost off — not the feed's card: the feed's toggle, badge and footer are the
// Asks page's alone.
func TestAListWithAReturnKeepsItsCompactCard(t *testing.T) {
	li := itemLi(t, attentionItemsHTMLTo(designItems(), designNow, designReturn), "ask-1")
	// The form is THE answer form (askAnswerFormHTML, the design review): its
	// options are the Asks card's.
	for _, want := range []string{`<textarea id="answer-1"`, `<input type="hidden" name="return" value="` + designReturn + `">`, ` hx-boost="false"`, `class="` + ui.AsksOptionClass + `" data-attention-option>Deploy now</button>`} {
		if !strings.Contains(li, want) {
			t.Errorf("the card lacks %q:\n%s", want, li)
		}
	}
	for _, bad := range []string{"data-asks-toggle", "data-asks-badge", "data-asks-footer", "<details"} {
		if strings.Contains(li, bad) {
			t.Errorf("the card draws %q:\n%s", bad, li)
		}
	}
}

// TestCardTextIsEscapedEverywhere: markup in the head's title (a session's
// Name), its place (Source/target; its tag-shaped markup is dropped by the
// title rule first, so it carries bare < & " >), an option's label, a
// suggestion chip and a waiting card's context line is drawn as text.
func TestCardTextIsEscapedEverywhere(t *testing.T) {
	const evil = `<img src=x onerror=alert(1)>"&`
	const want = `&lt;img src=x onerror=alert(1)&gt;&#34;&amp;`
	const placeEvil, placeWant = `1 < 2 & "x" >`, `1 &lt; 2 &amp; &#34;x&#34; &gt;`
	items := []attentionItem{
		{Key: "ask-1", Kind: attentionKindAsk, AskID: 1, Name: "Name " + evil, Source: "tmux fern:1 @ atlas " + placeEvil, target: "tmux fern:1 @ atlas " + placeEvil,
			at: mirror.Place{Code: "fern:1", Host: "atlas " + placeEvil}, Text: "q? " + evil, Options: []string{"opt " + evil}, suggestions: []string{"chip " + evil}, Since: designNow},
		{Key: "session-cccccccccccccccc", Kind: attentionKindSession, Source: "tmux fern:2 @ atlas", target: "tmux fern:2 @ atlas",
			at: mirror.Place{Code: "fern:2", Host: "atlas"}, Text: "Bash: ls", Detail: "detail " + evil, Link: "/mirror/view", Since: designNow},
	}
	out := attentionItemsHTML(items, designNow)
	if strings.Contains(out, "<img") || strings.Contains(out, `onerror=alert(1)>`) {
		t.Fatalf("a raw tag reached the page:\n%s", out)
	}
	for name, frag := range map[string]string{
		"title": `data-attention-name>Name ` + want + `</span>`,
		// The place is words ("on <host>"); the exact target is the ⓘ's.
		"place":   `data-attention-source>on atlas ` + placeWant + `</span>`,
		"address": `data-info-code="">tmux fern:1 @ atlas ` + placeWant + `</code>`,
		"copy":    `data-info-copy=""`,
		// The question is ask text (markdown's escape: " is &quot;).
		"question": `data-attention-text>q? &lt;img src=x onerror=alert(1)&gt;&quot;&amp;</div>`,
		"option":   `data-attention-option>opt ` + want + `</button>`,
		"chip":     `>chip ` + want + `</button>`,
		"detail":   `data-attention-detail>detail ` + want + `</span>`,
	} {
		if !strings.Contains(out, frag) {
			t.Errorf("the %s is not drawn escaped (want %q):\n%s", name, frag, out)
		}
	}
}

// TestEveryTabValueOpensWhereItDid: the tab a request names, through
// asksTab and asksTabsHTML, as the selected top tab (and, for the Drafts
// tab, its selected view) — every new name, every old one, junk, and a tab
// that is not drawn. Pinned literally, request by request. RED at origin/main:
// no Open or Answered tab ("ask" was the first), "answered" unknown.
func TestEveryTabValueOpensWhereItDid(t *testing.T) {
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	sel := regexp.MustCompile(`id="asks-tabs-tab-([a-z]+)" [^>]*aria-selected="true"|aria-selected="true"[^>]* id="asks-tabs-tab-([a-z]+)"`)
	arch := regexp.MustCompile(`<section id="asks-drafts-archived-section"[^>]*>`)
	draw := func(query string, drafts bool, nudges string) (top, view string) {
		r := httptest.NewRequest("GET", "/attention"+query, nil)
		out := s.asksTabsHTML("<ul></ul>", "<ul></ul>", drafts, nil, nil, nudges, "log", asksTab(r))
		for _, b := range regexp.MustCompile(`<button[^>]*role="tab"[^>]*>`).FindAllString(out, -1) {
			if !strings.Contains(b, `aria-selected="true"`) {
				continue
			}
			if m := sel.FindStringSubmatch(b); m != nil {
				top = m[1] + m[2]
			}
		}
		// The Drafts tab's view: its archived list shown ("archived") or not
		// ("drafts"); "" when the tab is not drawn. No second tab strip.
		if sec := arch.FindString(out); sec != "" {
			view = "drafts"
			if !strings.Contains(sec, " hidden") {
				view = "archived"
			}
		}
		if strings.Contains(out, `id="asks-drafts-tabs"`) {
			t.Errorf("%q: the Drafts tab draws a second tab strip", query)
		}
		return top, view
	}
	for _, c := range []struct {
		query, top, view string
		drafts           bool
		nudges           string
	}{
		{"", "open", "drafts", true, "n"},
		{"?tab=open", "open", "drafts", true, "n"},
		{"?tab=ask", "open", "drafts", true, "n"},
		{"?tab=answered", "answered", "drafts", true, "n"},
		{"?tab=drafts", "drafts", "drafts", true, "n"},
		{"?tab=archived", "drafts", "archived", true, "n"},
		{"?tab=drafts&draft=9", "drafts", "drafts", true, "n"},
		{"?tab=nudges", "nudges", "drafts", true, "n"},
		{"?tab=log", "log", "drafts", true, "n"},
		{"?tab=junk", "open", "drafts", true, "n"},
		// Not drawn: Drafts with plugins off, Nudges with the feature off.
		{"?tab=drafts", "open", "", false, ""},
		{"?tab=archived", "open", "", false, ""},
		{"?tab=nudges", "open", "", false, ""},
	} {
		// view is the Drafts tab's own selected view, "" when it is not drawn.
		if top, view := draw(c.query, c.drafts, c.nudges); top != c.top || view != c.view {
			t.Errorf("%q (drafts %v, nudges %q): top %q view %q, want %q %q", c.query, c.drafts, c.nudges, top, view, c.top, c.view)
		}
	}
	// The ask log off (no Log panel): no Log tab, and ?tab=log opens Open.
	off := s.asksTabsHTML("<ul></ul>", "<ul></ul>", true, nil, nil, "n", "", asksTab(httptest.NewRequest("GET", "/attention?tab=log", nil)))
	if strings.Contains(off, `id="asks-tabs-tab-log"`) || !regexp.MustCompile(`id="asks-tabs-tab-open" [^>]*aria-selected="true"|aria-selected="true"[^>]* id="asks-tabs-tab-open"`).MatchString(off) {
		t.Errorf("with the ask log off ?tab=log draws a Log tab or does not open Open:\n%s", off)
	}
	// The labels and their order: Open · Answered · Drafts · Nudges · Log.
	out := s.asksTabsHTML("", "", true, []plugins.DraftCard{{}, {}}, nil, "n", "log", asksTabOpen)
	var labels []string
	for _, m := range regexp.MustCompile(`role="tab"[^>]*>([^<]+)</button>`).FindAllStringSubmatch(out, -1) {
		labels = append(labels, m[1])
	}
	if got := strings.Join(labels, " · "); !strings.HasPrefix(got, "Open · Answered · Drafts (2) · Nudges · Log") {
		t.Errorf("the tabs read %q", got)
	}
	// The draft deep link (a push's tap target) names the Drafts tab and the card.
	if got := draftLink(9); got != "/attention?tab=drafts#draft-9" {
		t.Errorf("draftLink(9) = %q", got)
	}
	if got := draftDecidedLink(9); got != "/attention?tab=archived&draft=9#draft-9" {
		t.Errorf("draftDecidedLink(9) = %q", got)
	}
	// The panels are the swipe area.
	if !strings.Contains(out, `<div class="`+ui.AsksSwipeAreaClass+`" data-asks-swipe data-tab-swipe="asks-tabs"><div id="asks-tabs-panel-open"`) {
		t.Errorf("the panels are not in the swipe area:\n%s", out)
	}
}

// TestADraftCardKeepsItsSafetyWithOnePrimaryAction: the draft card still
// draws exactly what is approved (the post's preview, every hidden character
// a visible flag; its length), the
// message it answers marked untrusted and escaped, ONE primary action
// (Approve and send, a step-up form, on its own row: ui.AsksDecisionClass)
// with Reject under it; an approval not yet collected keeps Withdraw; a draft
// no longer pending offers no Approve. A guard on the redesign (the safety
// itself is the store's, asks_drafts_pg; the layout is
// e2e/tests/draft-card.spec.ts's).
func TestADraftCardKeepsItsSafetyWithOnePrimaryAction(t *testing.T) {
	at := designNow
	c := plugins.DraftCard{
		Draft:       plugins.Draft{ID: 7, AgentName: "chief", Body: "ok\u200b", Status: plugins.DraftPending, CreatedAt: at, ExpiresAt: at.Add(plugins.DraftTTL)},
		Message:     plugins.Message{ID: 3, Sender: "someone", Subject: "", Body: "<i>hi</i>"},
		PluginLabel: "Example inbox",
	}
	got := draftCardHTML(c)
	for _, want := range []string{`class="` + ui.AsksQuoteClass + `" data-draft-original>`, `data-draft-untrusted>untrusted</span>`, "&lt;i&gt;hi&lt;/i&gt;",
		`data-runes="3"`, `ok<mark class="` + ui.StepUpHiddenClass + `" data-stepup-hidden="U+200B" title="Hidden character U+200B">U+200B</mark>`, `<div class="` + ui.AsksDecisionClass + `"><form method="POST" action="` + pluginsPath + `/drafts/7/approve" data-step-up-form class="min-w-0"><button type="submit" class="` + ui.AsksPrimaryButtonClass + `" data-draft-approve>`,
		`<button type="submit" class="` + ui.AsksSecondaryButtonClass + `" data-draft-reject>Reject</button>`} {
		if !strings.Contains(got, want) {
			t.Errorf("the pending card lacks %q:\n%s", want, got)
		}
	}
	// The zero-width space is flagged where it sits, never written into the
	// page and never spelled out.
	if strings.Contains(got, "​") || strings.Contains(got, `\u{`) {
		t.Errorf("the pending card writes the hidden character, or spells it out:\n%s", got)
	}
	if strings.Count(got, ui.AsksPrimaryButtonClass) != 1 || strings.Contains(got, "<i>") || strings.Contains(got, "message 3") {
		t.Errorf("the pending card has %d primary actions, or unescaped markup:\n%s", strings.Count(got, ui.AsksPrimaryButtonClass), got)
	}
	approved := c
	approved.Draft.Status, approved.Draft.ApprovedAt = plugins.DraftApproved, &at
	if got := draftCardHTML(approved); !strings.Contains(got, `data-draft-withdraw>Withdraw the approval</button>`) || strings.Contains(got, "data-draft-approve") {
		t.Errorf("an uncollected approval lost Withdraw, or offers Approve:\n%s", got)
	}
	approved.Draft.ClaimedAt = &at
	if got := draftCardHTML(approved); strings.Contains(got, "data-draft-withdraw") {
		t.Errorf("a collected approval offers Withdraw:\n%s", got)
	}
	gone := c
	gone.Draft.Status = plugins.DraftSuperseded
	if got := draftCardHTML(gone); strings.Contains(got, "data-draft-approve") || strings.Contains(got, "data-draft-reject") {
		t.Errorf("a replaced draft offers a decision:\n%s", got)
	}
}

// TestADraftCardIsTheFeedCardWithItsFactsAsPills (the design review, 6, 9,
// 10): a draft is THE feed card (data-asks-card, fixed), its head the
// drafting agent's DISPLAY name (AgentDisplay, never the stored slug when
// there is one) with its age and no kind badge (the preview's head says the
// venue: the plugin's label); its facts are pills — the state, the expiry
// RELATIVE ("expires in 6d", never a UTC stamp); the reply is the post's
// preview, in the page's normal text, wrapping at words (it was monospace
// broken anywhere); and the duplicate lines are gone ("message N",
// "approving sends exactly this, once", the "Reply · N characters" label).
// RED at origin/main: the old card, a raw UTC expiry, mono break-all.
func TestADraftCardIsTheFeedCardWithItsFactsAsPills(t *testing.T) {
	at := designNow
	agent := int64(4)
	c := plugins.DraftCard{
		Draft: plugins.Draft{ID: 7, AgentID: &agent, AgentName: "marketing-acme-1", Body: "It is on by default at 600 requests a minute per key.", Status: plugins.DraftPending,
			CreatedAt: at.Add(-2 * time.Minute), ExpiresAt: at.Add(6*24*time.Hour + time.Hour), CheckState: plugins.CheckSendable},
		Message:      plugins.Message{ID: 3, Sender: "dana@example.com", Body: "Is it on by default?"},
		PluginLabel:  "Example inbox",
		AgentDisplay: "Marketing for Acme",
	}
	got := draftCardHTMLAt(c, at)
	for _, want := range []string{
		`class="` + ui.AsksCardClass + `"` + ui.AsksFixedCardAttrs,
		`data-draft-agent>Marketing for Acme</span>`,
		`>2m ago</time>`,
		`data-stepup-venue>Example inbox</span>`,
		`data-draft-state>Awaiting your decision</span>`,
		`>expires in 6d</time></span>`,
		`data-stepup-preview-text><p class="whitespace-pre-wrap break-words">It is on by default at 600 requests a minute per key.</p></div>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the draft card lacks %q:\n%s", want, got)
		}
	}
	// No length pill (the asks audit: "53 characters" was noise beside the
	// text itself; the length is the box's data-runes).
	for _, bad := range []string{"marketing-acme-1", "UTC", "font-mono", "break-all font-mono", "data-asks-badge", "approving sends exactly this", "message 3", "Reply · ", "characters</span>", "data-draft-runes"} {
		if strings.Contains(got, bad) {
			t.Errorf("the draft card still draws %q:\n%s", bad, got)
		}
	}
	// No display name resolved: the stored name, as before.
	c.AgentDisplay = ""
	if got := draftCardHTMLAt(c, at); !strings.Contains(got, `data-draft-agent>marketing-acme-1</span>`) {
		t.Errorf("without a display name the card does not fall back to the stored one:\n%s", got)
	}
}
