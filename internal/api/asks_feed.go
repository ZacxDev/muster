package api

import (
	"context"
	"html"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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
	case attentionKindPlugin:
		return "pairing", "Pairing request"
	}
	return "other", "Item"
}

// asksCardContextInner is the words of a card's "why it's here" line
// (asksCardContextRow), "" for none.
//
// An agent's ask that carries a why (ask_media.go: its own `context`, or the
// fallback muster stored — the session's focus title and the agent's last
// message) and a held question (heldWhy: the same fallback, read at render
// time) draw THAT line (ui.AskContextInline). Any other item is drawn from
// what it already carries: the session's focus title (with its age), else the
// item's second line (a waiting session's last words), else — for a task to
// route — why it is in the queue.
func asksCardContextInner(it attentionItem, now time.Time) string {
	var inner string
	switch w := askCardWhy(it); {
	case w != (ui.AskWhy{}):
		inner = ui.AskContextInline(w)
	case textplain.Title(it.Focus) != "":
		inner = attentionFocusHTML(textplain.Title(it.Focus), ui.FocusAgeOf(it.focusAt, now))
	case it.Detail != "" && it.Kind != attentionKindAsk:
		inner = `<span data-attention-detail>` + itemDetailHTML(it) + `</span>`
	case it.Kind == attentionKindTask:
		inner = `<span>` + taskToRouteWhy + `</span>`
	}
	return inner
}

// asksCardContextRow is a card's "why it's here" line: its words (inner) and
// the card's ⓘ (info: the exact tmux address of the window it names,
// attentionPlaceSheetHTML) at the end of it — the head is the card's toggle,
// and a button cannot sit inside it. "" for neither.
//
// With no words the ⓘ is a row of its own: a 44px line holding one icon.
// The Asks feed's cards and the ask page draw that ⓘ in their footer instead,
// beside "View in session", when they have one (askFeedCardHTML,
// handleAskPage; the v0.250.0 phone check,
// F4: the lone ⓘ's empty line read as stray whitespace).
func asksCardContextRow(inner, info string) string {
	switch {
	case inner == "" && info == "":
		return ""
	case info == "":
		return `<div class="` + ui.AsksCardContextClass + `" data-asks-context>` + inner + `</div>
`
	}
	return `<div class="flex min-w-0 items-center gap-1" data-asks-place><div class="` + ui.AsksCardContextClass + ` min-w-0 flex-1" data-asks-context>` + inner + `</div>` + info + `</div>
`
}

// asksCardMediaSlot is a card's media strip (images inline, other files as
// chips), "" for none.
//
// An agent's ask's attachments (ask_media.go), through ui.AskMediaStrip:
// images as thumbnails that open full size, other files as chips. The strip
// carries data-asks-media, so the card's script never takes a tap or a swipe
// on it for the card's own (its thumbnails and chips are links besides).
//
// A share link's card (it.pageOpen) also draws its "Open the page" button
// here: the page it asks about is one tap away, never an address in its text.
func asksCardMediaSlot(it attentionItem) string {
	if it.Kind != attentionKindAsk {
		return ""
	}
	open := pageAskOpenHTML(it.pageOpen)
	if len(it.Attachments) == 0 {
		return open
	}
	return ui.AskMediaStrip(askMediaView(it.AskID, it.Attachments)) + "\n" + open
}

// askCardWhy is the why line an item's card draws in its context slot: an
// agent's ask's stored why, a held question's fallback (heldWhy); the zero
// value for any other item and for one with none.
func askCardWhy(it attentionItem) ui.AskWhy {
	switch it.Kind {
	case attentionKindAsk:
		return askWhyView(it.Why)
	case attentionKindQuestion:
		return heldWhy(it)
	}
	return ui.AskWhy{}
}

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
// component: ui/asks_card.go). An ask's or a notice's head is the link to its
// page (attentionItem.page, askHasPage), its chevron the expand; any other card's head is the
// expand. The way to the session is the footer's "View in session", at the
// card's bottom whether it is expanded or not.
func askFeedCardHTML(it attentionItem, now time.Time) string {
	esc := html.EscapeString
	var b strings.Builder
	kind, label := askFeedBadge(it)
	more := asksCardMoreID(it.Key)
	// data-asks-who: the session's name as the head titles it (attentionTitle,
	// the one naming rule) — what an accepted answer's "Sent to" toast says.
	// data-asks-uncounted: a card listed but not waiting on this reader (a
	// team's ask a viewer watches: waitingOnReader), which the open count
	// leaves out (asksPendingCount).
	whoName, _, _ := attentionTitle(it)
	uncounted := ""
	if it.Kind == attentionKindAsk && it.watchOnly != "" {
		uncounted = " data-asks-uncounted"
	}
	b.WriteString(`<li id="` + esc(it.Key) + `" class="` + ui.AsksFeedCardClass + `" data-attention-item data-kind="` + esc(it.Kind) +
		`" data-asks-card data-asks-who="` + esc(whoName) + `"` + uncounted + ` data-expanded="false" tabindex="-1">
`)
	// THE HEAD: badge, who, how long — one toggle.
	who := attentionHeadLines(it, now)
	text := askTextHTML(it.Text)
	if it.Kind == attentionKindTask {
		title, rest := taskCardParts(it.Text)
		var sub string
		if !it.Since.IsZero() {
			sub = `<span class="` + ui.AsksSubClass + `"><time datetime="` + it.Since.UTC().Format(time.RFC3339) + `">` + esc(attentionAge(now.Sub(it.Since))) + `</time></span>`
		}
		who = `<span class="` + ui.AsksTitleClass + `" data-attention-title>` + esc(title) + `</span>` + sub
		text = askTextHTML(rest)
	}
	// 🔴 THE TOGGLE IS NAMED FOR WHAT IT DOES ("Details", ui.AsksCardToggleLabel),
	// its words (who, how long) its DESCRIPTION: named by its content it read
	// as a second control saying the card's words — "… just now" — beside the
	// card's own options (e2e waiting-composer: two buttons matched "no").
	// THE MORE REGION's content, drawn below — known now, so the toggle names
	// it only when there is one (no empty region, no dangling aria-controls).
	var m strings.Builder
	if it.Detail != "" && textplain.Title(it.Focus) != "" || it.Detail != "" && it.Kind == attentionKindAsk {
		// The second line the context slot did not draw.
		m.WriteString(`<p class="break-words text-sm text-fg2"><span data-attention-detail>` + itemDetailHTML(it) + `</span></p>`)
	}
	m.WriteString(proposalBodyHTML(it.proposal))
	moreID := ""
	if m.Len() > 0 {
		moreID = more
	}
	// The kind badge only where it tells something: a question is the
	// default card, so it carries none (the design review: "Question" on most
	// cards was noise).
	badge := ""
	if kind != "question" {
		badge = ui.AsksKindBadge(kind, label)
	}
	// AN ASK'S ID ("Ask #859", ui.AskIDChipHTML) ends the top row, OUTSIDE the
	// toggle (a button cannot hold a button): a tap on it copies the id and
	// never expands the card.
	chip := ""
	if it.AskID > 0 && (it.Kind == attentionKindAsk || it.Kind == attentionKindNotice) {
		chip = ui.AskIDChipHTML(it.AskID)
	}
	b.WriteString(ui.AsksHeadHTML(ui.AsksHead{ID: it.Key, Badge: badge, Who: who, Toggle: true, More: moreID, Chip: chip, Href: it.page}) + "\n")
	// THE QUESTION (what a waiting session waits on: its pending call).
	b.WriteString(askCardQuestionHTML(it, text))
	// THE FOOTER's links: View in session, the task.
	var links []string
	if href := askFeedSessionHref(it); href != "" {
		links = append(links, `<a href="`+esc(href)+`" class="`+ui.AsksCardLinkClass+`" data-asks-view-session>`+askViewInSession+`</a>`)
	}
	switch {
	case it.Kind == attentionKindPlugin && it.Link != "":
		links = append(links, `<a href="`+esc(it.Link)+`" class="`+ui.AsksCardLinkClass+`" data-asks-pairing>Review on Plugins</a>`)
	case it.Kind == attentionKindTask && it.Link != "":
		links = append(links, `<a href="`+esc(it.Link)+`" class="`+ui.AsksCardLinkClass+`" data-asks-task>Open task</a>`)
	case it.taskID > 0:
		n := strconv.FormatInt(it.taskID, 10)
		links = append(links, `<a href="`+esc(canonicalTaskPath(it.taskID))+`" class="`+ui.AsksCardLinkClass+`" data-asks-task>Open task #`+n+`</a>`)
	}
	// THE CONTEXT LINE and the card's ⓘ (asksCardContextRow). An ⓘ with no
	// words beside it goes in the footer, after its links, when there is one:
	// on a line of its own it was one icon in an empty 44px row.
	inner, info := asksCardContextInner(it, now), attentionPlaceSheetHTML(it, it.Key)
	if inner == "" && info != "" && len(links) > 0 {
		links = append(links, info)
		info = ""
	}
	b.WriteString(asksCardContextRow(inner, info))
	b.WriteString(asksCardMediaSlot(it))
	// THE CONTROLS.
	id := strconv.FormatInt(it.AskID, 10)
	dismissForm := "ask-" + id + "-dismiss"
	dismiss := ""
	switch it.Kind {
	case attentionKindAsk:
		if it.watchOnly == "" {
			dismiss = askDismissButton(dismissForm, "Dismiss without answering")
		}
	case attentionKindNotice:
		dismiss = askDismissButton(dismissForm, "Dismiss")
	}
	// 🔴 DISMISS ENDS THE FOOTER WHEN THE FOOTER HAS LINKS, ELSE THE ANSWER
	// ROW — never the head (Round 0 of #620), and never a footer of a lone ✕
	// (the design review: a 44px band for one icon). A notice with nothing to
	// link has no answer row: its footer's Dismiss says its word.
	inRow := len(links) == 0 && it.Kind == attentionKindAsk
	if len(links) == 0 && it.Kind == attentionKindNotice {
		dismiss = `<button type="submit" form="` + dismissForm + `" class="` + ui.AsksSecondaryButtonClass + `" data-attention-dismiss>Dismiss</button>`
	}
	switch {
	case it.Kind == attentionKindAsk && it.watchOnly != "":
		b.WriteString(`<p class="` + ui.AsksMutedTextClass + `" data-attention-watch-only>` + esc(it.watchOnly) + `</p>
`)
	case it.Kind == attentionKindAsk && it.stepUp != nil:
		// 🔴 NO CONFIRM/REFUSE AND NO ANSWER BOX ON A STEP-UP CARD, as on its
		// page (handleAskPage): only the paired phone answers it
		// (errHostDecisionNeedsPhone refuses every other door). One status
		// line in their place, never drawn as a control; Dismiss ends it, as
		// it ends an answer row.
		rowDismiss := ""
		if inRow {
			rowDismiss = dismiss
		}
		// A card about a draft the reader may decide draws the DRAFT's own
		// decision controls (askStepUpDraftHTML, the Drafts tab's), returning
		// here; the card itself stays answerable only from the phone.
		line := askStepUpDraftHTML(it.stepUp, it.stepUpDraft, attentionPath, askPagePath(it.AskID), "text-sm", true, now)
		if line == "" {
			line = askStepUpPhoneHTML("text-sm")
		}
		b.WriteString(`<div class="flex min-w-0 items-center gap-2" data-asks-stepup-row><div class="min-w-0 flex-1">` + line + `</div>` + rowDismiss + "</div>\n")
	case it.Kind == attentionKindAsk:
		gate := asksCardFormAttrs(it.Key, attentionPath+`/asks/`+id+`/answer`)
		if it.stepUpWhy != "" {
			// A team card answered with a fresh sign-in is its confirm
			// dialog's: a plain (boosted) POST, no hx-post.
			gate = ` data-step-up-form data-step-up-why="` + esc(it.stepUpWhy) + `"`
		}
		rowDismiss := ""
		if inRow {
			rowDismiss = dismiss
		}
		b.WriteString(askAnswerFormHTML(it, id, askFormPlace{attrs: gate, dismiss: rowDismiss, chips: true, gated: true}))
	case it.openCode != nil:
		b.WriteString(`<div data-asks-controls>` + openCodeAskHTML(it) + `</div>
`)
	case it.Kind == attentionKindQuestion:
		// A HELD QUESTION is answered in its session, never here.
		b.WriteString(attentionHeldCueHTML + "\n")
	}
	// THE MORE REGION: shown only expanded, drawn only with something in it.
	// (Its old first line, "Waiting since <UTC>", said the head's age again.)
	// The card's error slot is not drawn here: the feed's script adds it when
	// a post is refused (ui asks_card.go errorSlot).
	if m.Len() > 0 {
		b.WriteString(`<div id="` + esc(more) + `" class="` + ui.AsksCardMoreClass + `" data-asks-card-more><div class="` + ui.AsksCardMoreInnerClass + `">` + m.String() + `</div></div>
`)
	}
	footDismiss := dismiss
	if inRow {
		footDismiss = ""
	}
	if len(links) > 0 || footDismiss != "" {
		b.WriteString(`<div class="` + ui.AsksCardFooterClass + `" data-asks-footer>` + strings.Join(links, "") + `<span class="flex-1"></span>` + footDismiss + `</div>
`)
	}
	if dismiss != "" {
		b.WriteString(`<form id="` + dismissForm + `" method="POST" action="` + attentionPath + `/asks/` + id + `/cancel" hidden` + asksCardFormAttrs(it.Key, attentionPath+`/asks/`+id+`/cancel`) + `></form>
`)
	}
	b.WriteString("</li>\n")
	return b.String()
}

// askCardQuestionHTML is THE body of an ask's card — what it asks, or what a
// waiting session waits on — on the Open feed and the Answered tab alike (the
// one body: an answered card drew its question as one muted block of text, a
// step-up card's included, while the Open card drew the post's preview).
// text is the question's own HTML (askTextHTML of it, or of a task's label).
func askCardQuestionHTML(it attentionItem, text string) string {
	switch {
	case (it.Kind == attentionKindSession || it.Kind == attentionKindQuestion) && it.openCode == nil:
		line := strings.TrimSpace(it.Text)
		// 🔴 A QUESTION IS A SENTENCE, NOT CODE: it reads in the card's own
		// face (the jargon audit, U-03 — in monospace it read as terminal
		// output). Only a session waiting on a TOOL CALL ("Bash: npm
		// publish") keeps the code face, since that line is a command.
		cls := ui.AsksCardQuestionClass
		if it.Kind == attentionKindSession && it.tool != "" && it.tool != claudeCodeQuestionTool {
			cls += " font-mono text-sm"
		}
		if it.Text == sessionWaitingText {
			cls = ui.AsksCardQuestionClass + " text-fg2"
		}
		// A tool call's line is a command, drawn as written (plainTextHTML);
		// a question is ask text.
		body := askTextHTML(line)
		if strings.Contains(cls, "font-mono") {
			body = plainTextHTML(line)
		}
		return `<div class="` + cls + `" data-attention-text>` + body + `</div>
`
	case it.openCode != nil:
		return `<p class="` + ui.AsksCardQuestionClass + `" data-attention-text><bdi dir="ltr">` + plainTextHTML(it.Text) + `</bdi></p>
`
	case it.Kind == attentionKindAsk && it.stepUp != nil:
		// A step-up card: its title, why and a compact mock of the post
		// (stepUpFeedHTML), never its question as one block of text.
		return stepUpFeedHTML(it.stepUp)
	}
	// A builtin officer's context (the item, the outside message it is
	// about) is the substance of its ask: drawn with the question, never
	// behind the expand.
	return officerNoteLabelHTML(it.Context) + `<div class="` + ui.AsksCardQuestionClass + `" data-attention-text>` + text + `</div>
` + officerContextHTML(it.Context)
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

// askOptionsShortRunes is the longest option label laid out two to a row:
// at a phone's width half a card holds about this many characters on one
// line.
const askOptionsShortRunes = 18

// askOptionsShort reports whether options are laid out two to a row
// (ui.AsksOptionsPairClass): two to four of them, each at most
// askOptionsShortRunes long.
func askOptionsShort(options []string) bool {
	if len(options) < 2 || len(options) > 4 {
		return false
	}
	for _, o := range options {
		if utf8.RuneCountInString(o) > askOptionsShortRunes {
			return false
		}
	}
	return true
}

// askDismissButton is an ask card's Dismiss: a small icon button joined by
// form= to the card's separate /cancel form (so a dismissal never carries the
// typed answer), named for a screen reader.
func askDismissButton(form, name string) string {
	return `<button type="submit" form="` + form + `" aria-label="` + name + `" title="` + name + `" class="` + ui.AsksIconButtonClass + `" data-attention-dismiss>` + ui.AsksDismissIcon + `</button>`
}

// askFormPlace is where an ask's answer form is drawn, and what that place
// adds to it: back (a session page's return field), attrs (the form's own
// attributes: the Asks card's htmx post or step-up, a session page's
// hx-boost="false" and step-up), dismiss (Dismiss at the end of the answer
// row, "" when the card's footer holds it) and chips (the suggested replies:
// only where the Asks feed's script fills the box from them) and gated
// (Answer drawn aria-disabled while its box is blank: only where the Asks feed's
// script enables it as the box fills).
//
// The ask PAGE (attention_ask_page.go, askPageAnswerHTML) draws this same form
// and adds: ids (its controls' fixed ids, which its own script and the mic
// address) and micOff (the muted mic, linking to how to turn dictation on,
// when the server has none). Its chips are its live region's, outside the
// form (chips: false).
type askFormPlace struct {
	back, attrs, dismiss string
	chips, gated         bool
	ids                  askFormIDs
	micOff               bool
}

// askFormIDs are an answer form's ids: the form's own ("" for none), the
// answer box's, Answer's, the mic's (its note is the mic's id + "-note") and
// the optional note box's. The zero value is a card's: every id the ask's own
// (askCardFormIDs), since a page of cards draws one form per ask.
type askFormIDs struct {
	form, input, send, mic, note string
}

// askCardFormIDs is ask id's form's ids on a card: answer-<id>,
// answer-<id>-submit, answer-<id>-dictate and note-<id>.
func askCardFormIDs(id string) askFormIDs {
	input := "answer-" + id
	return askFormIDs{input: input, send: input + "-submit", mic: input + "-dictate", note: "note-" + id}
}

// askSuggestionChipsHTML is an ask's suggested replies as chips (a tap FILLS
// the answer box, never sends), "" for none: the card's and the ask page's.
func askSuggestionChipsHTML(suggestions []string) string {
	if len(suggestions) == 0 {
		return ""
	}
	esc := html.EscapeString
	var b strings.Builder
	b.WriteString(`<div class="` + ui.AsksCardChipsClass + `" role="group" aria-label="` + suggestLabel + `" data-asks-suggestions>` + ui.AsksAIPillHTML)
	for _, t := range suggestions {
		b.WriteString(`<button type="button" class="` + ui.AsksCardChipClass + `" data-asks-suggestion data-text="` + esc(t) + `" aria-label="AI-suggested reply: ` + esc(t) + `">` + esc(t) + `</button>`)
	}
	b.WriteString("</div>\n")
	return b.String()
}

// askAnswerFormHTML is THE answer form, on the Asks page's card and on a
// session page's asks block alike (the design review: the two surfaces drew
// two forms): the asker's options as full-width buttons (a tap answers), the
// suggested replies as chips (a tap FILLS the box), and the box — one line,
// growing — with its microphone (only while dictation is on: a disabled mic
// on every card was noise) and Answer. With options to tap, they are the
// primary action and Answer is quiet.
//
// 🔴 EVERY ID IS THE ASK'S OWN: a page draws one box per ask, and the mic
// finds its box and note by id (ui.DictateControlHTML). The mic's ✓ FILLS the
// box at the caret; its ↑ fills it and clicks THIS Answer (its id), so Answer
// is still the only send (ui/dictate.go).
func askAnswerFormHTML(it attentionItem, id string, p askFormPlace) string {
	esc := html.EscapeString
	var b strings.Builder
	action := attentionPath + `/asks/` + id + `/answer`
	ids := p.ids
	if ids == (askFormIDs{}) {
		ids = askCardFormIDs(id)
	}
	formID := ""
	if ids.form != "" {
		formID = ` id="` + ids.form + `"`
	}
	b.WriteString(`<form` + formID + ` method="POST" action="` + action + `" class="flex min-w-0 flex-col gap-2" data-attention-answer data-asks-controls` + p.attrs + `>
` + p.back)
	if len(it.Options) > 0 && it.multi {
		// A MULTI-SELECT ask (migration 0175): each option a CHECKBOX row, and
		// ONE Send — never one submit per option. The free-text answer row and
		// the note below carry the same Send, so a tap of any box sends every
		// box ticked (joined by attention.OptionSep) and, with a note, that
		// too. The send is not GATED on the box: a pick with no text is a
		// complete answer.
		b.WriteString(`<div class="` + ui.AsksOptionsCheckClass + `" role="group" aria-label="The asker's options">`)
		for _, o := range it.Options {
			b.WriteString(`<label class="` + ui.AsksOptionCheckClass + `"><input type="checkbox" name="option" value="` + esc(o) + `" class="` + ui.AsksOptionCheckBoxClass + `" data-attention-option>` + esc(o) + `</label>`)
		}
		b.WriteString("</div>\n")
	} else if len(it.Options) > 0 {
		cls := ui.AsksOptionsClass
		if askOptionsShort(it.Options) {
			cls = ui.AsksOptionsPairClass
		}
		b.WriteString(`<div class="` + cls + `" role="group" aria-label="The asker's options">`)
		for _, o := range it.Options {
			b.WriteString(`<button type="submit" name="option" value="` + esc(o) + `" class="` + ui.AsksOptionClass + `" data-attention-option>` + esc(o) + `</button>`)
		}
		b.WriteString("</div>\n")
	}
	if p.chips {
		b.WriteString(askSuggestionChipsHTML(it.suggestions))
	}
	input, submit := ids.input, ids.send
	send := ui.AsksCardSendClass
	sendLabel := "Answer"
	if len(it.Options) > 0 && !it.multi {
		send = ui.AsksCardSendQuietClass
	}
	if it.multi {
		// One Send for the whole multi-select form: the boxes, the free text
		// and the note go together, so it is the primary action and it is
		// never gated on the box (a pick with no text is complete). The feed's
		// script still refuses a post with neither a pick nor text
		// (ui/asks_card.go, htmx:beforeRequest).
		sendLabel = "Send answer"
	}
	// GATED (the Asks feed, whose script runs here): Answer starts OFF
	// (aria-disabled: ui.AsksSendDisabledClass says why not disabled) — the box
	// is empty — and the feed's script turns it on while the box holds text
	// (ui/asks_card.go). Elsewhere (a morning report, an officer's page:
	// no feed script) it is drawn enabled, as before.
	sendAttrs := ""
	if p.gated && !it.multi {
		send += " " + ui.AsksSendDisabledClass
		sendAttrs = ` aria-disabled="true" data-asks-send`
	}
	mic, note := "", ""
	if it.dictate != "" || p.micOff && it.dictateOff {
		// answerMic: the real mic, or — the ask page only (micOff), with no
		// dictation on the server — the muted one linking to how to turn it on.
		mic = ui.DictateWaveHTML(it.dictate, ids.mic) +
			answerMic(it.dictate, p.micOff && it.dictateOff, input, ids.mic+"-note", ids.mic) +
			ui.DictateSendHTML(it.dictate, ids.mic, submit, ui.DictateSendPill)
		note = ui.DictateNoteHTML(it.dictate, ids.mic+"-note", "")
	}
	b.WriteString(`<label for="` + input + `" class="sr-only">Your answer</label>
<div class="` + ui.AskFormBoxRowClass + `" data-asks-answer-row><textarea id="` + input + `" name="answer" rows="1" placeholder="Your answer…" maxlength="` + strconv.Itoa(attention.MaxAnswerRunes) + `" class="` + ui.AskFormInputClass + `"></textarea>` +
		mic + `<button type="submit" id="` + submit + `" class="` + send + `"` + sendAttrs + `>` + sendLabel + `</button>` + p.dismiss + `</div>
` + note)
	if it.multi {
		// THE OPTIONAL NOTE (migration 0175): one line under the answer, its
		// own name="note", appended to the recorded answer as " — note: …".
		noteID := ids.note
		b.WriteString(`<div class="` + ui.AsksNoteRowClass + `" data-asks-note-row>
<label for="` + noteID + `" class="` + ui.AsksNoteLabelClass + `">Note (optional)</label>
<textarea id="` + noteID + `" name="note" rows="1" placeholder="Add a note…" maxlength="` + strconv.Itoa(attention.MaxNoteRunes) + `" class="` + ui.AsksNoteClass + `" data-asks-note></textarea>
</div>
`)
	}
	b.WriteString("</form>\n")
	return b.String()
}

// askFeedHTML is the Open feed's <li>s and how many cards it holds: the items
// (the queue without its reply drafts, askTabItems) in askGroups' order —
// newest first within a kind — with session state's stalled rows (episodes,
// n of them) after the waiting windows.
//
// 🔴 "ALL CLEAR" ONLY WHEN NOTHING IS PENDING, DRAFTS INCLUDED. The reply
// drafts in items are the Drafts tab's, never a card here — but they wait on
// the owner too, so an Open feed with no card and a draft pending draws the
// drafts' empty state (ui.AsksDraftsWaitHTML, pointing at the tab), never All
// clear. The drafts' count rides along on a hidden marker
// (ui.AsksDraftsMarkerHTML), so the feed's script counts the same total as
// the server (asksPendingOf) after every live refresh.
func askFeedHTML(items []attentionItem, episodes string, n int, now time.Time) (string, int) {
	// The drafts the Drafts tab LISTS (draftsTabItem), never every plugin item.
	drafts := len(items) - len(askTabItems(items))
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
	marker := ui.AsksDraftsMarkerHTML(drafts)
	switch {
	case count == 0 && drafts == 0:
		return ui.AsksAllClearHTML + marker, 0
	case count == 0:
		return ui.AsksDraftsWaitHTML(drafts) + marker, 0
	}
	return b.String() + marker, count
}

// answeredCardHTML is one recent answer's card on the Answered tab: who asked
// and when, the question, the answer, where it went — and Retry on a failed
// delivery. Its more region holds where the answer came from when it was not
// this view.
//
// 🔴 IT IS TITLED BY THE ONE NAMING RULE (attentionTitle over it, the ask as an
// attention item with its stored name: answeredItems) — the session's human
// name, its tmux target small under it — exactly as its card was titled on
// the Open tab. It used to be titled by its window's place ("tmux fern:1 @
// atlas"), so a session was "Rate-limit the public API" until answered and a
// tmux address after.
func answeredCardHTML(a attention.Ask, it attentionItem, now time.Time, proposal bool) string {
	esc := html.EscapeString
	id := strconv.FormatInt(a.ID, 10)
	who, src, srcIsTitle := attentionTitle(it)
	// The delivery line names the session as the card's title does (who: in
	// words, never a tmux target); the exact target — where a queued or
	// waiting delivery sends the owner — is the card's ⓘ, beside the line.
	text, retry := answerDeliveryText(a, who, proposal)
	pill := answerDeliveryPill(a, proposal)
	delivery := firstNonBlank(string(a.Delivery), "none")
	wakeForm := ""
	if it.wake != nil {
		// A mirrored session's answer: where its session wake went
		// (session_wake.go), with the wake's own action in place of Retry.
		var act wakeAction
		text, act = wakeDeliveryText(*it.wake, who)
		retry = false
		pill, delivery = wakeDeliveryPill(*it.wake), "wake-"+string(it.wake.State)
		wakeForm = wakeActionFormHTML(it.wake.ID, act, ui.AsksSecondaryButtonClass, ` data-attention-redeliver`)
	}
	var sub []string
	if !a.ResolvedAt.IsZero() {
		sub = append(sub, `<time datetime="`+a.ResolvedAt.UTC().Format(time.RFC3339)+`">`+esc(attentionAge(now.Sub(a.ResolvedAt)))+`</time>`)
	}
	if !srcIsTitle && src != "" && src != who {
		sub = append(sub, `<span class="`+ui.AsksTargetClass+`" data-attention-source>`+esc(src)+`</span>`)
	}
	age := ""
	if len(sub) > 0 {
		age = `<span class="` + ui.AsksSubClass + `">` + strings.Join(sub, ` · `) + `</span>`
	}
	titleAttrs := ` data-attention-title`
	if srcIsTitle || src == "" || src == who {
		titleAttrs += ` data-attention-source`
	}
	key := "answered-" + id
	var b strings.Builder
	// THE QUESTION is drawn ONCE, whole, clamped while collapsed (it was a
	// one-line cut here and the full text again in the more region). WHERE
	// THE ANSWER WENT is a pill (answerDeliveryPill); the sentence that says
	// it in full is the expanded card's.
	// THE OPEN CARD'S HEAD AND BODY (ui.AsksHeadHTML, askCardQuestionHTML):
	// a step-up card answered is still drawn as its post's preview.
	b.WriteString(`<li id="` + key + `" class="` + ui.AsksFeedCardClass + `" data-attention-answered data-asks-card data-expanded="false" tabindex="-1" data-ask-id="` + id + `" data-delivery="` + esc(delivery) + `">
` + ui.AsksHeadHTML(ui.AsksHead{ID: key, Who: `<span class="` + ui.AsksTitleClass + `"` + titleAttrs + `>` + esc(who) + `</span>` + age, Toggle: true, More: asksCardMoreID(key), Chip: ui.AskIDChipHTML(a.ID), Href: it.page}) + `
` + askCardQuestionHTML(it, askTextHTML(a.Question)) + `<div class="` + ui.AsksAnswerTextClass + `" data-attention-answer-text>` + answerLabel(a) + askTextHTML(a.Answer) + `</div>
` + answeredDeliveryRowHTML(it, key, `<div class="`+ui.AsksPillsClass+`"><span class="`+ui.AsksPillClass+`" data-attention-delivery-pill>`+esc(pill)+`</span></div>`) + `
`)
	b.WriteString(wakeForm)
	if retry {
		b.WriteString(`<form method="POST" action="` + attentionPath + `/asks/` + id + `/redeliver" data-asks-controls><button type="submit" class="` + ui.AsksSecondaryButtonClass + `" data-attention-redeliver>Retry delivery</button></form>
`)
	}
	b.WriteString(`<div id="` + asksCardMoreID(key) + `" class="` + ui.AsksCardMoreClass + `" data-asks-card-more><div class="` + ui.AsksCardMoreInnerClass + `">` +
		`<p class="break-words text-xs text-fg2" data-attention-delivery>` + esc(text) + `</p>` + answeredViaHTML(a) + `</div></div>
</li>
`)
	return b.String()
}

// answerDeliveryPill is where an answer went, in a pill's few words
// (answerDeliveryText says it in full, in the expanded card).
func answerDeliveryPill(a attention.Ask, proposal bool) string {
	if proposal && a.Delivery == attention.DeliveryWaited {
		return "Recorded"
	}
	switch a.Delivery {
	case attention.DeliveryQueued:
		return "Queued"
	case attention.DeliveryDelivering:
		return "Delivering"
	case attention.DeliveryDelivered:
		return "Delivered"
	case attention.DeliveryWaited:
		return "Read by the session"
	case attention.DeliveryUnconfirmed:
		return "Delivery failed"
	case attention.DeliveryPull:
		return "Waiting for the session to read it"
	}
	if a.Asker.System {
		return "Recorded"
	}
	return "Not delivered"
}

// answeredFeedHTML is the Answered tab's feed: one card per recent answer, or
// its empty line.
// items[i] is asks[i] as an attention item, named (answeredItems).
func answeredFeedHTML(asks []attention.Ask, items []attentionItem, now time.Time, proposals map[int64]bool) string {
	if len(asks) == 0 {
		return ui.AsksEmptyHTML(` data-asks-empty="answered" data-asks-answered-empty`, "", "", "No answers in the last day.", "")
	}
	var b strings.Builder
	for i, a := range asks {
		b.WriteString(answeredCardHTML(a, items[i], now, proposals[a.ID]))
	}
	return b.String()
}

// answeredItems is asks as attention items (askItem over hosts: the source,
// target and place an Open card has) — named, as an Open card is, by
// withNames — for the Answered cards' titles.
func (s *Server) answeredItems(ctx context.Context, asks []attention.Ask, hosts []mirror.HostView) []attentionItem {
	items := make([]attentionItem, len(asks))
	for i, a := range asks {
		items[i] = askItem(a, hosts...)
	}
	s.withNames(ctx, items)
	withAskPages(ctx, items)
	if ws := s.wakesForAsks(ctx, asks); len(ws) > 0 {
		for i, a := range asks {
			if w, ok := ws[a.ID]; ok {
				items[i].wake = &w
			}
		}
	}
	return items
}

// answeredDeliveryRowHTML is an Answered card's delivery line and, when the
// ask names a window, its ⓘ (attentionPlaceSheetHTML) beside it — outside the
// card's toggle, which a button cannot sit inside.
func answeredDeliveryRowHTML(it attentionItem, key, line string) string {
	info := attentionPlaceSheetHTML(it, key)
	if info == "" {
		return line
	}
	return `<div class="flex min-w-0 items-center gap-1" data-asks-place><div class="min-w-0 flex-1">` + line + `</div>` + info + `</div>`
}
