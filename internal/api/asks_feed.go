package api

import (
	"html"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/textplain"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE ASKS CARD FEED (operator decision "Vertical card feed"; the component's
// anatomy and motion are internal/ui/asks_card.go's, its documentation
// docs/attention.md "The card feed"). Every item the Asks page lists is one
// card in a vertically scrolling feed per tab:
//
//	Open      questions, then notices, then waiting sessions (and session
//	          state's stalled rows), then tasks to route — askGroups' order,
//	          each card carrying its old group's name as a kind badge;
//	Answered  the recent answers (answeredAsksHTML), moved off the Open tab;
//	Drafts    the reply drafts (plugins on), with its own Awaiting decision /
//	          Archived views;
//	Nudges    the chief's nudge decisions (the feature on);
//	Log       the ask log.
//
// 🔴 EVERY OLD ?tab= STILL OPENS WHERE IT DID (asksTab): "ask" — the Open
// tab's old name — and anything unknown open the Open tab; "drafts" the
// Drafts tab on Awaiting decision; "archived" the Drafts tab on its Archived
// view (the view a decided draft lands on: draftDecidedLink); "nudges" and
// "log" their tabs. A tab that is not drawn (Drafts with plugins off, Nudges
// with the feature off) falls back to Open. The fragment (#draft-<id>,
// #ask-<id>, #answered-<id>) is followed by the page's script to whichever
// panel holds it (asksPageScript reveal).

const (
	// asksTabOpen is the Open tab: what is waiting on the owner now.
	asksTabOpen = "open"
	// asksTabAnswered is the Answered tab: the recent answers.
	asksTabAnswered = "answered"
)

// asksCardID is the id of a card's more region.
func asksCardMoreID(key string) string { return key + "-more" }

// askFeedBadge is an item's kind badge: its key (data-asks-badge) and words.
// The words are the old group headers', singular.
func askFeedBadge(it attentionItem) (kind, label string) {
	switch it.Kind {
	case attentionKindAsk, attentionKindQuestion:
		return "question", "Question"
	case attentionKindNotice:
		return "notice", "Notice"
	case attentionKindSession:
		return "waiting", "Waiting"
	case attentionKindTask:
		return "task", "Task to route"
	}
	return "other", "Item"
}

// asksCardContextSlot is a card's "why it's here" line, "" for none.
//
// 🔴 THE SLOT THE ASK-CONTEXT WORK FILLS. Its PR adds an ask's optional
// `context` (and, when it is absent, a fallback from the session's focus title
// and the agent's last message before the ask) and exposes a render helper in
// internal/ui/ask_media.go; whichever of the two PRs merges second wires that
// helper in HERE. Until then the line is drawn from what the item already
// carries: the session's focus title (with its age), else the item's second
// line (a waiting session's last words), else — for a task to route — why it
// is in the queue.
func asksCardContextSlot(it attentionItem, now time.Time) string {
	var inner string
	switch {
	case textplain.Title(it.Focus) != "":
		inner = attentionFocusHTML(textplain.Title(it.Focus), ui.FocusAgeOf(it.focusAt, now))
	case it.Detail != "" && it.Kind != attentionKindAsk:
		inner = `<span data-attention-detail>` + itemDetailHTML(it) + `</span>`
	case it.Kind == attentionKindTask:
		inner = `<span>` + taskToRouteWhy + `</span>`
	}
	if inner == "" {
		return ""
	}
	return `<div class="` + ui.AsksCardContextClass + `" data-asks-context>` + inner + `</div>
`
}

// asksCardMediaSlot is a card's media strip (images inline, other files as
// chips), "" for none.
//
// 🔴 THE SLOT THE ASK-ATTACHMENTS WORK FILLS (internal/ui/ask_media.go's
// helper, wired here by whichever PR merges second). Nothing an item carries
// today is media, so it draws nothing. A strip that scrolls sideways must carry
// data-asks-media (the card's script then never takes a tap or a swipe on it
// for the card's own).
func asksCardMediaSlot(it attentionItem) string { return "" }

// taskToRouteWhy is a task to route's context line: why it is in the queue
// (unroutedTaskItems' Source, as a sentence).
const taskToRouteWhy = "Your chief has not routed it yet."

// itemDetailHTML is an item's Detail, escaped (an opencode ask's isolated).
func itemDetailHTML(it attentionItem) string {
	d := html.EscapeString(it.Detail)
	if it.openCode != nil {
		return `<bdi dir="ltr">` + d + `</bdi>`
	}
	return d
}

// taskCardParts splits a task item's text ("Task #7: its label") into the
// card's title ("Task #7") and its question line ("its label").
func taskCardParts(text string) (title, rest string) {
	if strings.HasPrefix(text, "Task #") {
		if i := strings.Index(text, ": "); i > 0 {
			return text[:i], text[i+2:]
		}
	}
	return "Task", text
}

// askFeedCardHTML is ONE item's card on the Asks page's Open feed (the
// component: ui/asks_card.go). The head is a toggle, never a link: the way to
// the session is the footer's "View in session", at the card's bottom whether
// it is expanded or not.
func askFeedCardHTML(it attentionItem, now time.Time) string {
	esc := html.EscapeString
	var b strings.Builder
	kind, label := askFeedBadge(it)
	more := asksCardMoreID(it.Key)
	b.WriteString(`<li id="` + esc(it.Key) + `" class="` + ui.AsksFeedCardClass + `" data-attention-item data-kind="` + esc(it.Kind) +
		`" data-asks-card data-expanded="false" tabindex="-1">
`)
	// THE HEAD: badge, who, how long — one toggle.
	who := attentionHeadLines(it, now)
	text := esc(it.Text)
	if it.Kind == attentionKindTask {
		title, rest := taskCardParts(it.Text)
		var sub string
		if !it.Since.IsZero() {
			sub = `<span class="` + ui.AsksSubClass + `"><time datetime="` + it.Since.UTC().Format(time.RFC3339) + `">` + esc(attentionAge(now.Sub(it.Since))) + `</time></span>`
		}
		who = `<span class="` + ui.AsksTitleClass + `" data-attention-title>` + esc(title) + `</span>` + sub
		text = esc(rest)
	}
	// 🔴 THE TOGGLE IS NAMED FOR WHAT IT DOES ("Details", ui.AsksCardToggleLabel),
	// its words (who, how long) its DESCRIPTION: named by its content it read
	// as a second control saying the card's words — "… just now" — beside the
	// card's own options (e2e waiting-composer: two buttons matched "no").
	b.WriteString(`<button type="button" class="` + ui.AsksCardHeadClass + `" aria-expanded="false" aria-controls="` + esc(more) + `" aria-label="` + ui.AsksCardToggleLabel +
		`" aria-describedby="` + esc(it.Key) + `-who" data-asks-toggle>` +
		ui.AsksKindBadge(kind, label) + `<span id="` + esc(it.Key) + `-who" class="` + ui.AsksCardWhoClass + `">` + who + `</span>` + ui.AsksCardChevron + `</button>
`)
	// THE QUESTION (what a waiting session waits on: its pending call).
	switch {
	case (it.Kind == attentionKindSession || it.Kind == attentionKindQuestion) && it.openCode == nil:
		line := strings.TrimSpace(it.Text)
		cls := ui.AsksCardQuestionClass + " font-mono text-sm"
		if it.Text == sessionWaitingText {
			cls = ui.AsksCardQuestionClass + " text-fg2"
		}
		b.WriteString(`<p class="` + cls + `" data-attention-text>` + esc(line) + `</p>
`)
	case it.openCode != nil:
		b.WriteString(`<p class="` + ui.AsksCardQuestionClass + `" data-attention-text><bdi dir="ltr">` + text + `</bdi></p>
`)
	default:
		b.WriteString(officerNoteLabelHTML(it.Context))
		b.WriteString(`<p class="` + ui.AsksCardQuestionClass + `" data-attention-text>` + text + `</p>
`)
		// A builtin officer's context (the item, the outside message it is
		// about) is the substance of its ask: drawn with the question, never
		// behind the expand.
		b.WriteString(officerContextHTML(it.Context))
	}
	b.WriteString(asksCardContextSlot(it, now))
	b.WriteString(asksCardMediaSlot(it))
	// THE CONTROLS.
	id := strconv.FormatInt(it.AskID, 10)
	dismissForm := "ask-" + id + "-dismiss"
	dismiss := ""
	switch {
	case it.Kind == attentionKindAsk && it.watchOnly != "":
		b.WriteString(`<p class="` + ui.AsksMutedTextClass + `" data-attention-watch-only>` + esc(it.watchOnly) + `</p>
`)
	case it.Kind == attentionKindAsk:
		b.WriteString(askFeedAnswerFormHTML(it, id))
		dismiss = `<button type="submit" form="` + dismissForm + `" aria-label="Dismiss without answering" title="Dismiss without answering" class="` + ui.AsksIconButtonClass + `" data-attention-dismiss>` + ui.AsksDismissIcon + `</button>`
	case it.Kind == attentionKindNotice:
		// 🔴 NO ANSWER BOX: a notice points at a session; it is looked at or
		// dismissed, never answered (attention.ErrNotice).
		dismiss = `<button type="submit" form="` + dismissForm + `" aria-label="Dismiss" title="Dismiss" class="` + ui.AsksIconButtonClass + `" data-attention-dismiss>` + ui.AsksDismissIcon + `</button>`
	case it.openCode != nil:
		b.WriteString(`<div data-asks-controls>` + openCodeAskHTML(it) + `</div>
`)
	case it.Kind == attentionKindQuestion:
		// A HELD QUESTION is answered in its session, never here.
		b.WriteString(attentionHeldCueHTML + "\n")
	}
	// THE MORE REGION: shown only expanded.
	var m strings.Builder
	if !it.Since.IsZero() {
		m.WriteString(`<p class="text-xs text-fg2" data-asks-since>Waiting since <time datetime="` + it.Since.UTC().Format(time.RFC3339) + `">` +
			esc(it.Since.UTC().Format("2006-01-02 15:04 UTC")) + `</time></p>`)
	}
	if it.Detail != "" && textplain.Title(it.Focus) != "" || it.Detail != "" && it.Kind == attentionKindAsk {
		// The second line the context slot did not draw.
		m.WriteString(`<p class="break-words text-sm text-fg2"><span data-attention-detail>` + itemDetailHTML(it) + `</span></p>`)
	}
	m.WriteString(proposalBodyHTML(it.proposal))
	b.WriteString(`<div id="` + esc(more) + `" class="` + ui.AsksCardMoreClass + `" data-asks-card-more><div class="` + ui.AsksCardMoreInnerClass + `">` + m.String() + `</div></div>
`)
	// The error slot, above the footer: a refused answer or dismissal's
	// fragment (ui.AsksCardErrorHTML), put here by the feed's script.
	b.WriteString(`<div data-asks-card-error></div>
`)
	// THE FOOTER: View in session, the task, Dismiss at its end.
	var links []string
	if href := askFeedSessionHref(it); href != "" {
		links = append(links, `<a href="`+esc(href)+`" class="`+ui.AsksCardLinkClass+`" data-asks-view-session>`+askViewInSession+`</a>`)
	}
	switch {
	case it.Kind == attentionKindTask && it.Link != "":
		links = append(links, `<a href="`+esc(it.Link)+`" class="`+ui.AsksCardLinkClass+`" data-asks-task>Open task</a>`)
	case it.taskID > 0:
		n := strconv.FormatInt(it.taskID, 10)
		links = append(links, `<a href="`+esc(canonicalTaskPath(it.taskID))+`" class="`+ui.AsksCardLinkClass+`" data-asks-task>Open task #`+n+`</a>`)
	}
	if len(links) > 0 || dismiss != "" {
		b.WriteString(`<div class="` + ui.AsksCardFooterClass + `" data-asks-footer>` + strings.Join(links, "") + `<span class="flex-1"></span>` + dismiss + `</div>
`)
	}
	if dismiss != "" {
		b.WriteString(`<form id="` + dismissForm + `" method="POST" action="` + attentionPath + `/asks/` + id + `/cancel" hidden` + asksCardFormAttrs(it.Key, attentionPath+`/asks/`+id+`/cancel`) + `></form>
`)
	}
	b.WriteString("</li>\n")
	return b.String()
}

// asksCardFormAttrs are a card form's htmx attributes: posted by htmx to its
// own route (action) with nothing swapped (the feed's script settles the card
// on the 204, or puts the refusal's fragment in its error slot: asksCardFailed),
// tagged with its card's key.
func asksCardFormAttrs(key, action string) string {
	return ` hx-post="` + html.EscapeString(action) + `" hx-swap="none" hx-headers='{"` + asksCardHeader + `": "1"}' data-asks-card-form="` + html.EscapeString(key) + `"`
}

// asksCardHeader marks a card form's htmx post (asksCardFormAttrs). HX-Request
// alone cannot: htmx sets it on every request it makes, a boosted plain form
// included, and every other caller of these routes wants its redirect.
const asksCardHeader = "X-Muster-Asks-Card"

// asksCardRequest reports whether r is a card form's htmx post from the Asks
// page (asksCardFormAttrs): HX-Request AND its asksCardHeader, not a boosted
// navigation, not a session page's form (its `return`). Only such a request is
// answered with the 204 / fragment; every other keeps its redirect and page.
func asksCardRequest(r *http.Request) bool {
	if r.Header.Get("HX-Request") != "true" || r.Header.Get(asksCardHeader) != "1" || r.Header.Get("HX-Boosted") == "true" {
		return false
	}
	_, page := attentionReturnPage(r)
	return !page
}

// asksCardFailed answers a card's refused post (asksCardRequest): the error's
// own status and the one sentence the queue page would say, as a fragment.
func asksCardFailed(w http.ResponseWriter, err error, cur attention.Ask) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(attentionErrCode(err))
	_, _ = w.Write([]byte(ui.AsksCardErrorHTML(attentionErrText(err, cur))))
}

// askFeedSessionHref is where a card's "View in session" goes, "" for none:
// an ask's or a notice's session (withAskBoxes: attentionSessionPath, the ask
// page's own rule), a waiting window's or a held question's rendered view.
func askFeedSessionHref(it attentionItem) string {
	switch it.Kind {
	case attentionKindAsk, attentionKindNotice:
		return it.sessionLink
	case attentionKindSession, attentionKindQuestion:
		return it.Link
	}
	return ""
}

// askFeedAnswerFormHTML is an ask card's answer form: its options as
// full-width buttons (a tap answers), its suggested replies as chips (a tap
// FILLS the box), and the box — one line, growing — with its microphone and
// Answer. Every answerable card draws the box and the mic, so a and d (the
// feed's keys) always have something to reach.
func askFeedAnswerFormHTML(it attentionItem, id string) string {
	esc := html.EscapeString
	var b strings.Builder
	action := attentionPath + `/asks/` + id + `/answer`
	// A team card answered with a fresh sign-in is its confirm dialog's:
	// a plain (boosted) POST, no hx-post.
	gate := asksCardFormAttrs(it.Key, action)
	if it.stepUpWhy != "" {
		gate = ` data-step-up-form data-step-up-why="` + esc(it.stepUpWhy) + `"`
	}
	b.WriteString(`<form method="POST" action="` + action + `" class="flex min-w-0 flex-col gap-2" data-attention-answer data-asks-controls` + gate + `>
`)
	if len(it.Options) > 0 {
		b.WriteString(`<div class="` + ui.AsksOptionsClass + `" role="group" aria-label="The asker's options">`)
		for _, o := range it.Options {
			b.WriteString(`<button type="submit" name="option" value="` + esc(o) + `" class="` + ui.AsksOptionClass + `" data-attention-option>` + esc(o) + `</button>`)
		}
		b.WriteString("</div>\n")
	}
	if len(it.suggestions) > 0 {
		b.WriteString(`<div class="` + ui.AsksCardChipsClass + `" role="group" aria-label="` + suggestLabel + `" data-asks-suggestions>`)
		for _, t := range it.suggestions {
			b.WriteString(`<button type="button" class="` + ui.AsksCardChipClass + `" data-asks-suggestion data-text="` + esc(t) + `" aria-label="AI-suggested reply: ` + esc(t) + `">` + esc(t) + `</button>`)
		}
		b.WriteString("</div>\n")
	}
	input := "answer-" + id
	submit := input + "-submit"
	// With options to tap, they are the card's primary action and Answer is
	// quiet; with none, Answer is the one primary action.
	send := ui.AsksCardSendClass
	if len(it.Options) > 0 {
		send = ui.AsksCardSendQuietClass
	}
	b.WriteString(`<label for="` + input + `" class="sr-only">Your answer</label>
<div class="` + ui.AskFormBoxRowClass + `" data-asks-answer-row><textarea id="` + input + `" name="answer" rows="1" placeholder="Your answer…" maxlength="` + strconv.Itoa(attention.MaxAnswerRunes) + `" class="` + ui.AskFormInputClass + `"></textarea>` +
		ui.DictateWaveHTML(it.dictate, input+"-dictate") +
		answerMic(it.dictate, it.dictateOff, input, input+"-dictate-note", input+"-dictate") +
		ui.DictateSendHTML(it.dictate, input+"-dictate", submit, ui.DictateSendPill) +
		`<button type="submit" id="` + submit + `" class="` + send + `">Answer</button></div>
` + ui.DictateNoteHTML(it.dictate, input+"-dictate-note", ""))
	b.WriteString("</form>\n")
	return b.String()
}

// askFeedHTML is the Open feed's <li>s and how many cards it holds: the items
// (the queue without its reply drafts, askTabItems) in askGroups' order —
// newest first within a kind — with session state's stalled rows (episodes,
// n of them) after the waiting windows; the All clear state when there is
// nothing at all.
func askFeedHTML(items []attentionItem, episodes string, n int, now time.Time) (string, int) {
	items = askTabItems(items)
	var b strings.Builder
	count := n
	for _, g := range askGroups {
		for _, it := range items {
			if slices.Contains(g.kinds, it.Kind) {
				b.WriteString(askFeedCardHTML(it, now))
				count++
			}
		}
		if g.key == "waiting" && episodes != "" {
			// Session state's rows (episodeRowsHTML: a labelled <ul>), in the
			// Waiting place, each one row linking to its session.
			b.WriteString(`<li class="min-w-0" data-asks-episodes>` + episodes + "</li>\n")
		}
	}
	if count == 0 {
		return ui.AsksAllClearHTML, 0
	}
	return b.String(), count
}

// answeredCardHTML is one recent answer's card on the Answered tab: who asked
// and when, the question, the answer, where it went — and Retry on a failed
// delivery. Its more region holds where the answer came from when it was not
// this view.
func answeredCardHTML(a attention.Ask, hosts []mirror.HostView, now time.Time, proposal bool) string {
	esc := html.EscapeString
	id := strconv.FormatInt(a.ID, 10)
	who := textplain.TitleKeeping(askSourceParts(a, hosts))
	text, retry := answerDeliveryText(a, who, proposal)
	age := ""
	if !a.ResolvedAt.IsZero() {
		age = `<span class="` + ui.AsksSubClass + `"><time datetime="` + a.ResolvedAt.UTC().Format(time.RFC3339) + `">` + esc(attentionAge(now.Sub(a.ResolvedAt))) + `</time></span>`
	}
	key := "answered-" + id
	var b strings.Builder
	b.WriteString(`<li id="` + key + `" class="` + ui.AsksFeedCardClass + `" data-attention-answered data-asks-card data-expanded="false" tabindex="-1" data-ask-id="` + id + `" data-delivery="` + esc(firstNonBlank(string(a.Delivery), "none")) + `">
<button type="button" class="` + ui.AsksCardHeadClass + `" aria-expanded="false" aria-controls="` + asksCardMoreID(key) + `" aria-label="` + ui.AsksCardToggleLabel +
		`" aria-describedby="` + key + `-who" data-asks-toggle>` + ui.AsksKindBadge("answered", "Answered") +
		`<span id="` + key + `-who" class="` + ui.AsksCardWhoClass + `"><span class="` + ui.AsksTitleClass + `" data-attention-source>` + esc(who) + `</span>` + age + `</span>` + ui.AsksCardChevron + `</button>
<p class="` + ui.AsksCardQuestionClass + ` text-fg2" data-attention-text>` + esc(mirror.OneLine(a.Question, 200)) + `</p>
<p class="whitespace-pre-wrap break-words text-sm text-fg" data-attention-answer-text>` + answerLabel(a) + esc(a.Answer) + `</p>
<p class="break-words text-xs text-fg2" data-attention-delivery>` + esc(text) + `</p>
`)
	if retry {
		b.WriteString(`<form method="POST" action="` + attentionPath + `/asks/` + id + `/redeliver" data-asks-controls><button type="submit" class="` + ui.AsksSecondaryButtonClass + `" data-attention-redeliver>Retry delivery</button></form>
`)
	}
	b.WriteString(`<div id="` + asksCardMoreID(key) + `" class="` + ui.AsksCardMoreClass + `" data-asks-card-more><div class="` + ui.AsksCardMoreInnerClass + `">` + answeredViaHTML(a) +
		`<p class="whitespace-pre-wrap break-words text-sm text-fg2" data-asks-question-full>` + esc(a.Question) + `</p></div></div>
</li>
`)
	return b.String()
}

// answeredFeedHTML is the Answered tab's feed: one card per recent answer, or
// its empty line.
func answeredFeedHTML(asks []attention.Ask, hosts []mirror.HostView, now time.Time, proposals map[int64]bool) string {
	if len(asks) == 0 {
		return `<li class="px-1 py-6 text-center text-sm text-fg2" data-asks-answered-empty>No answers in the last day.</li>`
	}
	var b strings.Builder
	for _, a := range asks {
		b.WriteString(answeredCardHTML(a, hosts, now, proposals[a.ID]))
	}
	return b.String()
}
