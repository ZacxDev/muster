package api

import (
	"context"
	"io"
	"log"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE ASKS PAGE'S DESIGN PASS (operator decision 2026-11-12): a flat list in
// labelled groups with counts, a card whose head (or, with nothing to answer,
// the whole card) is the link to the session, a waiting row that says what it
// waits on, the answer box behind the options, and Dismiss a small icon button
// with an accessible name. Every test here is written against functions that
// exist at origin/main too (attentionItemsHTML, askListHTML), so each is RED
// there by its assertion, not by a compile error.

var designNow = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

// designItems is one of every kind the Ask tab draws, plus a reply draft (the
// Reply drafts tab's, never the Ask tab's).
func designItems() []attentionItem {
	since := designNow.Add(-9 * time.Minute)
	win := mirror.Place{Code: "fern:1", Host: "atlas"}
	return []attentionItem{
		{Key: "ask-1", Kind: attentionKindAsk, AskID: 1, Source: "deploy-bot", Text: "Deploy now?", Options: []string{"Deploy now", "Wait"}, Since: since,
			sessionLink: "/agents/deploy-bot"},
		{Key: "ask-2", Kind: attentionKindAsk, AskID: 2, Source: "deploy-bot", Text: "Rotate the key?", Since: since},
		{Key: "session-aaaaaaaaaaaaaaaa", Kind: attentionKindQuestion, Source: "tmux fern:2 @ atlas", target: "tmux fern:2 @ atlas",
			at: mirror.Place{Code: "fern:2", Host: "atlas"}, Text: "Which database?\nPick one.", Link: "/mirror/view?host=atlas&session=fern&window=%402", Since: since},
		{Key: "session-bbbbbbbbbbbbbbbb", Kind: attentionKindSession, Source: "tmux fern:1 @ atlas", target: "tmux fern:1 @ atlas", at: win,
			Text: "Bash: npm test", Detail: "Running the tests.", Link: "/mirror/view?host=atlas&session=fern&window=%401", Since: since},
		{Key: "task-3", Kind: attentionKindTask, Source: "muster", Text: "Task 3: route it", Link: "/tasks/3", Since: since},
		{Key: "draft-4", Kind: attentionKindPlugin, Source: "Example inbox", Text: "someone: hi", Link: draftLink(4), Since: since},
	}
}

var groupLabel = regexp.MustCompile(`data-asks-group="([a-z]+)"><h2 [^>]*data-asks-group-label>([^<]+)<span class="sr-only">, </span><span[^>]*data-asks-group-count>(\d+)</span></h2>`)

// TestTheAskTabIsGroupedUnderLabelsWithCounts: one label per group that holds
// something, with its count, in the page's order (Questions, Notices,
// Waiting, Tasks to route); an empty group draws no label; the reply draft is
// not on the Ask tab. RED at origin/main: no groups, no labels.
func TestTheAskTabIsGroupedUnderLabelsWithCounts(t *testing.T) {
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	out := s.askListHTML(context.Background(), designItems(), designNow)
	var got []string
	for _, m := range groupLabel.FindAllStringSubmatch(out, -1) {
		got = append(got, m[1]+"="+m[2]+" "+m[3])
	}
	if want := "questions=Questions 3|waiting=Waiting 1|tasks=Tasks to route 1"; strings.Join(got, "|") != want {
		t.Fatalf("groups %q, want %q:\n%s", strings.Join(got, "|"), want, out)
	}
	if strings.Contains(out, `id="draft-4"`) || strings.Contains(out, `data-asks-group="notices"`) {
		t.Fatalf("the Ask tab draws a draft or an empty group:\n%s", out)
	}
	// Each item sits inside its own group.
	for key, group := range map[string]string{"ask-1": "questions", "session-aaaaaaaaaaaaaaaa": "questions", "session-bbbbbbbbbbbbbbbb": "waiting", "task-3": "tasks"} {
		at := strings.Index(out, `id="`+key+`"`)
		g := strings.LastIndex(out[:max(at, 0)], `data-asks-group="`)
		if at < 0 || g < 0 || !strings.HasPrefix(out[g:], `data-asks-group="`+group+`"`) {
			t.Errorf("%s is not in the %s group", key, group)
		}
	}
	// Nothing waiting: the empty line, no label.
	if out := s.askListHTML(context.Background(), nil, designNow); out != attentionEmptyHTML {
		t.Fatalf("an empty list draws %q, want the empty line", out)
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

// TestAWaitingRowIsOneLinkSayingWhatItWaitsOn: a waiting window's card is ONE
// link to its rendered view, titled by the window's name with its age and
// tmux target under it, and its one line is the pending call — the server's
// own text, escaped. With nothing named it says the fallback; a held question
// shows its first line. RED at origin/main: an "Open the session" text link.
func TestAWaitingRowIsOneLinkSayingWhatItWaitsOn(t *testing.T) {
	items := designItems()
	out := attentionItemsHTML(items, designNow)
	li := itemLi(t, out, "session-bbbbbbbbbbbbbbbb")
	for _, want := range []string{
		`<a href="/mirror/view?host=atlas&amp;session=fern&amp;window=%401" class="` + ui.AsksRowClass + `" data-attention-open>`,
		`data-attention-title>fern:1</span>`, `>9m ago</time>`, `data-attention-source>tmux fern:1 @ atlas</span>`,
		`class="` + ui.AsksWaitLineClass + `" data-attention-text>Bash: npm test</span>`, `data-attention-detail>Running the tests.</span>`,
		ui.AsksChevron + `</a>`,
	} {
		if !strings.Contains(li, want) {
			t.Errorf("the waiting row lacks %q:\n%s", want, li)
		}
	}
	if strings.Contains(li, "Open the session") || strings.Contains(li, "underline") || strings.Count(li, "<a ") != 1 {
		t.Errorf("the waiting row is not one link with no text link in it:\n%s", li)
	}
	// The held question: its first line only, one link to its question card.
	q := itemLi(t, out, "session-aaaaaaaaaaaaaaaa")
	if !strings.Contains(q, `data-attention-text>Which database?</span>`) || strings.Contains(q, "Pick one.") || strings.Contains(q, "Answer in the session") {
		t.Errorf("the held question's row:\n%s", q)
	}
	// Nothing named: the fallback, muted. Markup in the host's words: escaped.
	items[3].Text = sessionWaitingText
	if li := itemLi(t, attentionItemsHTML(items, designNow), "session-bbbbbbbbbbbbbbbb"); !strings.Contains(li, `class="`+ui.AsksMutedTextClass+`" data-attention-text>`+sessionWaitingText+`</span>`) {
		t.Errorf("the fallback row:\n%s", li)
	}
	items[3].Text = `Bash: echo <b>hi</b>`
	if li := itemLi(t, attentionItemsHTML(items, designNow), "session-bbbbbbbbbbbbbbbb"); !strings.Contains(li, `data-attention-text>Bash: echo &lt;b&gt;hi&lt;/b&gt;</span>`) || strings.Contains(li, "<b>") {
		t.Errorf("the pending call is not escaped:\n%s", li)
	}
}

// TestAnAskCardsHeadIsTheLinkToItsSession: the card's head — the name, the
// age — is a real <a> to the session (attentionSessionPath's link) ending in
// a chevron, with no "View in session" words; a card with no session to open
// has no link at all. RED at origin/main: a separate underlined text link.
func TestAnAskCardsHeadIsTheLinkToItsSession(t *testing.T) {
	out := attentionItemsHTML(designItems(), designNow)
	li := itemLi(t, out, "ask-1")
	head := regexp.MustCompile(`<a href="/agents/deploy-bot" class="` + regexp.QuoteMeta(ui.AsksHeadLinkClass) + ` flex-1" data-attention-session>(.*?)</a>`).FindStringSubmatch(li)
	if head == nil || !strings.Contains(head[1], `data-attention-title data-attention-source>deploy-bot</span>`) || !strings.HasSuffix(head[1], ui.AsksChevron) {
		t.Fatalf("the ask's head is not its session link:\n%s", li)
	}
	if strings.Contains(out, askViewInSession) {
		t.Errorf("the list still says %q:\n%s", askViewInSession, out)
	}
	if li := itemLi(t, out, "ask-2"); strings.Contains(li, "<a ") {
		t.Errorf("an ask with no session draws a link:\n%s", li)
	}
}

// TestDismissIsASmallIconButtonWithAnAccessibleName: an ask's Dismiss and a
// notice's are icon buttons (the close glyph, no words) named by aria-label
// AND title, each joined by form= to its own /cancel form. RED at origin/main:
// a text "Dismiss" button.
func TestDismissIsASmallIconButtonWithAnAccessibleName(t *testing.T) {
	items := append(designItems(), attentionItem{Key: "ask-5", Kind: attentionKindNotice, AskID: 5, Source: "tmux fern:3 @ atlas", Text: "Waiting on you?", Since: designNow})
	out := attentionItemsHTML(items, designNow)
	for key, name := range map[string]string{"ask-1": "Dismiss without answering", "ask-5": "Dismiss"} {
		li := itemLi(t, out, key)
		id := strings.TrimPrefix(key, "ask-")
		want := `<button type="submit" form="ask-` + id + `-dismiss" aria-label="` + name + `" title="` + name + `" class="` + ui.AsksIconButtonClass + `" data-attention-dismiss>` + ui.AsksDismissIcon + `</button>`
		if !strings.Contains(li, want) {
			t.Errorf("%s: no icon Dismiss named %q:\n%s", key, name, li)
		}
		if !strings.Contains(li, `<form id="ask-`+id+`-dismiss" method="POST" action="/attention/asks/`+id+`/cancel" hidden>`) {
			t.Errorf("%s: its Dismiss has no /cancel form:\n%s", key, li)
		}
	}
}

// TestTheAnswerBoxIsBehindTheOptionsOnTheAsksPage: on the Asks page an ask
// with options draws them as full-width chips FIRST, and its box, mic and
// Answer inside a SHUT "Type an answer" <details>; an ask with no options
// draws its box open; a session page's block keeps its box open too. RED at
// origin/main: the box was always open beside the options.
func TestTheAnswerBoxIsBehindTheOptionsOnTheAsksPage(t *testing.T) {
	items := designItems()
	items[0].dictate = ui.TranscribePath
	li := itemLi(t, attentionItemsHTML(items, designNow), "ask-1")
	det := regexp.MustCompile(`(?s)<details class="` + regexp.QuoteMeta(ui.AsksTypeClass) + ` flex-1" data-ask-type>(.*?)</details>`).FindStringSubmatch(li)
	if det == nil {
		t.Fatalf("no shut Type an answer on an ask with options:\n%s", li)
	}
	if !strings.Contains(det[1], `data-ask-type-toggle>`+ui.AsksTypeIcon+`Type an answer</summary>`) || !strings.Contains(det[1], `<textarea id="answer-1"`) ||
		!strings.Contains(det[1], `data-dictate-input="answer-1"`) || !strings.Contains(det[1], `>Answer</button>`) {
		t.Errorf("the box, its mic or Answer is not behind Type an answer:\n%s", det[1])
	}
	if strings.Contains(li, "<details open") || strings.Index(li, `data-attention-option>Deploy now<`) > strings.Index(li, "<details") ||
		!strings.Contains(li, `class="`+ui.AsksOptionClass+`" data-attention-option>Deploy now</button>`) {
		t.Errorf("the options are not full-width chips before a shut box:\n%s", li)
	}
	if strings.Count(li, "<textarea") != 1 {
		t.Errorf("the card draws %d boxes", strings.Count(li, "<textarea"))
	}
	// No options: nothing to hide the box behind.
	if li := itemLi(t, attentionItemsHTML(items, designNow), "ask-2"); strings.Contains(li, "<details") || !strings.Contains(li, `<textarea id="answer-2"`) {
		t.Errorf("an ask with no options hides its box:\n%s", li)
	}
	// A session page's block: the box open, beside its options.
	if li := itemLi(t, attentionItemsHTMLTo(items, designNow, "/agents/deploy-bot"), "ask-1"); strings.Contains(li, "<details") || !strings.Contains(li, `<textarea id="answer-1"`) {
		t.Errorf("a session page's block hides its box:\n%s", li)
	}
}

// TestADraftCardKeepsItsSafetyWithOnePrimaryAction: the redesigned draft card
// still draws exactly what is approved (flows.VisibleEscape, its length), the
// message it answers marked untrusted and escaped, ONE primary action (Approve
// and send, a step-up form) with Reject small beside it; an approval not yet
// collected keeps Withdraw; a draft no longer pending offers no Approve. A
// guard on the redesign (the safety itself is the store's, asks_drafts_pg).
func TestADraftCardKeepsItsSafetyWithOnePrimaryAction(t *testing.T) {
	at := designNow
	c := plugins.DraftCard{
		Draft:       plugins.Draft{ID: 7, AgentName: "chief", Body: "ok\u200b", Status: plugins.DraftPending, CreatedAt: at, ExpiresAt: at.Add(plugins.DraftTTL)},
		Message:     plugins.Message{ID: 3, Sender: "someone", Subject: "", Body: "<i>hi</i>"},
		PluginLabel: "Example inbox",
	}
	got := draftCardHTML(c)
	for _, want := range []string{`class="` + ui.AsksQuoteClass + `" data-draft-original>`, "· untrusted · message 3", "&lt;i&gt;hi&lt;/i&gt;",
		`data-runes="3"`, `ok\u{200b}`, `/approve" data-step-up-form class="min-w-0 flex-1"><button type="submit" class="` + ui.AsksPrimaryButtonClass + `" data-draft-approve>`,
		`<button type="submit" class="` + ui.AsksSecondaryButtonClass + `" data-draft-reject>Reject</button>`} {
		if !strings.Contains(got, want) {
			t.Errorf("the pending card lacks %q:\n%s", want, got)
		}
	}
	if strings.Count(got, ui.AsksPrimaryButtonClass) != 1 || strings.Contains(got, "<i>") {
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

// headOf is the head of card li: from its start to the end of its head link
// (or, with no link, its head block) — attentionHeadHTML's element.
func headOf(t *testing.T, li string) string {
	t.Helper()
	for _, end := range []string{"</a>", "</div>"} {
		if strings.Contains(li, `data-attention-session>`) && end == "</div>" {
			continue
		}
		if i := strings.Index(li, end); i > 0 {
			return li[:i+len(end)]
		}
	}
	t.Fatalf("no head in:\n%s", li)
	return ""
}

// TestDismissIsInTheCardsFooterNeverItsHead (Round 0 of #620): the ✕ never
// sits in the head row beside the head link's chevron — a thumb aimed at
// "open the session" must not cancel the ask. It ends the card instead, after
// the question: the answer form's last row (beside "Type an answer", or beside
// Answer), a notice's own footer row; on the Asks page and a session page's
// block alike. RED at 674b99c9: the ✕ was the head row's last element.
func TestDismissIsInTheCardsFooterNeverItsHead(t *testing.T) {
	items := append(designItems(), attentionItem{Key: "ask-5", Kind: attentionKindNotice, AskID: 5, Source: "tmux fern:3 @ atlas", Text: "Waiting on you?",
		Since: designNow, sessionLink: "/mirror/view?host=atlas&session=fern&window=%403"})
	for _, returnTo := range []string{"", "/agents/deploy-bot"} {
		out := attentionItemsHTMLTo(items, designNow, returnTo)
		for _, key := range []string{"ask-1", "ask-2", "ask-5"} {
			li := itemLi(t, out, key)
			head := headOf(t, li)
			if strings.Contains(head, "data-attention-dismiss") {
				t.Errorf("return %q, %s: Dismiss is in the head:\n%s", returnTo, key, head)
			}
			// The head is the card's first child, nothing beside it in a row.
			if !strings.HasPrefix(li[strings.Index(li, ">\n")+2:], `<a href=`) && !strings.HasPrefix(li[strings.Index(li, ">\n")+2:], `<div class="`+ui.AsksHeadClass) {
				t.Errorf("return %q, %s: the head is wrapped in a row with something else:\n%s", returnTo, key, li)
			}
			d, txt := strings.Index(li, "data-attention-dismiss"), strings.Index(li, "data-attention-text")
			if d < 0 || txt < 0 || d < txt {
				t.Errorf("return %q, %s: Dismiss does not come after the question:\n%s", returnTo, key, li)
			}
		}
		// With options on the Asks page, the ✕ ends the "Type an answer" row.
		if returnTo == "" && !regexp.MustCompile(`(?s)</details><button type="submit" form="ask-1-dismiss"`).MatchString(itemLi(t, out, "ask-1")) {
			t.Errorf("the ✕ does not end the Type an answer row:\n%s", itemLi(t, out, "ask-1"))
		}
	}
}

// TestAGroupLabelReadsItsCountAndNamesItsList: a screen reader reads a group's
// label as "Questions, 2" (a visually-hidden ", " between the words and the
// count), and the group's list is labelled by that heading (aria-labelledby
// naming the h2's id) — on the Ask tab's groups and the stalled rows' list.
func TestAGroupLabelReadsItsCountAndNamesItsList(t *testing.T) {
	if got := ui.AsksGroupLabel("asks-group-questions", "Questions", 2); !strings.Contains(got, `id="asks-group-questions"`) ||
		!strings.Contains(got, `>Questions<span class="sr-only">, </span><span`) || !strings.Contains(got, `data-asks-group-count>2</span>`) {
		t.Fatalf("the label does not read \"Questions, 2\": %s", got)
	}
	out := askGroupsHTML(askTabItems(designItems()), `<ul aria-labelledby="asks-group-waiting" data-testid="attention-episodes"></ul>`, 0, "", designNow)
	for _, key := range []string{"questions", "waiting", "tasks"} {
		if !strings.Contains(out, `<h2 id="asks-group-`+key+`"`) || !strings.Contains(out, `aria-labelledby="asks-group-`+key+`"`) {
			t.Errorf("the %s group's list is not labelled by its heading:\n%s", key, out)
		}
	}
}

// TestCardTextIsEscapedEverywhere: markup in the head's title (a session's
// Name), its place (Source/target; its tag-shaped markup is dropped by the
// title rule first, so it carries bare < & " >), an option chip's label and a waiting
// row's detail line is drawn as text: escaped, the raw tag never in the page.
func TestCardTextIsEscapedEverywhere(t *testing.T) {
	const evil = `<img src=x onerror=alert(1)>"&`
	const want = `&lt;img src=x onerror=alert(1)&gt;&#34;&amp;`
	// A place is a title: the title rule (textplain) drops a whole HTML tag
	// before escaping, so its markup is the kind the rule keeps.
	const placeEvil, placeWant = `1 < 2 & "x" >`, `1 &lt; 2 &amp; &#34;x&#34; &gt;`
	items := []attentionItem{
		{Key: "ask-1", Kind: attentionKindAsk, AskID: 1, Name: "Name " + evil, Source: "tmux fern:1 @ atlas " + placeEvil, target: "tmux fern:1 @ atlas",
			at: mirror.Place{Code: "fern:1", Host: "atlas"}, Text: "q?", Options: []string{"opt " + evil}, Since: designNow},
		{Key: "session-cccccccccccccccc", Kind: attentionKindSession, Source: "tmux fern:2 @ atlas", target: "tmux fern:2 @ atlas",
			at: mirror.Place{Code: "fern:2", Host: "atlas"}, Text: "Bash: ls", Detail: "detail " + evil, Link: "/mirror/view", Since: designNow},
	}
	out := attentionItemsHTML(items, designNow)
	if strings.Contains(out, "<img") || strings.Contains(out, `onerror=alert(1)>`) {
		t.Fatalf("a raw tag reached the page:\n%s", out)
	}
	for name, frag := range map[string]string{
		"title":  `data-attention-name>Name ` + want + `</span>`,
		"place":  `data-attention-source>tmux fern:1 @ atlas ` + placeWant + `</span>`,
		"option": `data-attention-option>opt ` + want + `</button>`,
		"detail": `data-attention-detail>detail ` + want + `</span>`,
	} {
		if !strings.Contains(out, frag) {
			t.Errorf("the %s is not drawn escaped (want %q):\n%s", name, frag, out)
		}
	}
}
