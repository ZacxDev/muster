package api

import (
	"context"
	"html"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/textplain"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// THE ASK PAGE, /attention/asks/{id}: where an ask's notification lands
// (operator ask: "The place it lands on after I open it is not mobile
// optimized and doesn't feel native"; decision: a phone-width MOBILE WEB page,
// inside the Android app's WebView as in a browser).
//
// One question, as a phone app screen (ui.NavApp): a top bar with a back arrow
// to Asks and who is asking; the question, large; the asker's options as
// full-width buttons that answer at once; the suggested replies
// (attention_suggest.go) as chips that FILL the answer box — as the rendered
// view's chips do, nothing is sent until Answer; a "View in session" link to
// the asking session or agent (for a chief nudge's ask, the session the nudge
// is about); and the answer box pinned to the bottom, above the keyboard and
// inside the safe-area insets, with the microphone (dictate.go) beside Answer.
//
// 🔴 THE QUEUE'S OWN ROUTES. Its forms post to the queue's answer and dismiss
// routes (handleAttentionAnswerForm, handleAttentionCancelForm) with `return`
// naming this page (attentionReturnPage), so an answer given here is the same
// answer as anywhere else — one core, answerAskFrom.
//
// LIVE: the options, chips and state (#ask-live) re-read ?part=live on the
// queue's SSE event (ui.AttentionLiveScript), so suggestions that arrive after
// the page opened appear, and an ask answered elsewhere shows as answered (and
// its answer box goes).
// ---------------------------------------------------------------------------

// askPagePrefix is the ask page's path prefix.
const askPagePrefix = attentionPath + "/asks/"

// askPagePath is ask id's page.
func askPagePath(id int64) string { return askPagePrefix + strconv.FormatInt(id, 10) }

// handleAskPage is GET /attention/asks/{id} (?part=live: its live region only).
func (s *Server) handleAskPage(w http.ResponseWriter, r *http.Request) {
	if s.ext.Attention == nil {
		http.NotFound(w, r)
		return
	}
	id, ok := askIDParam(r)
	if !ok {
		s.renderAttentionPage(w, r, http.StatusNotFound, "No such question.")
		return
	}
	a, err := s.ext.Attention.Get(r.Context(), id)
	// Another owner's ask reads as a missing one (row-level security), and the
	// explicit owner check holds with it rolled back.
	if err != nil || !askHasPage(a, requestOwner(r.Context())) {
		if err != nil && attentionErrCode(err) == http.StatusInternalServerError {
			s.logger.Printf("attention: the ask page of %d: %v", id, err)
		}
		s.renderAttentionPage(w, r, http.StatusNotFound, "No such question.")
		return
	}
	s.renderAskPage(w, r, a)
}

// askHasPage is THE rule for whether ask a has a page viewer may open: their
// own ask, never one a session put to the chief. handleAskPage refuses every
// other with a 404, and every card that links to an ask's page (withAskPages,
// the Log's rows) links only where this holds — so no card links to a 404.
func askHasPage(a attention.Ask, viewer int64) bool {
	return a.ID > 0 && viewer > 0 && a.Owner == viewer && !a.ToChief
}

// withAskPages sets each ask's and notice's page (attentionItem.page) where
// its reader — ctx's principal — may open it (askHasPage).
func withAskPages(ctx context.Context, items []attentionItem) {
	viewer := requestOwner(ctx)
	for i := range items {
		it := &items[i]
		if it.Kind != attentionKindAsk && it.Kind != attentionKindNotice {
			continue
		}
		if askHasPage(attention.Ask{ID: it.AskID, Owner: it.owner, ToChief: it.toChief}, viewer) {
			it.page = askPagePath(it.AskID)
		}
	}
}

// askShowsSuggestions reports whether ask a's stored suggestions may be shown
// to its owner now: the same switches that decide whether they are made
// (MUSTER_SUGGEST=off, the owner's toggle, the auto-pause).
func (s *Server) askShowsSuggestions(ctx context.Context, a attention.Ask) bool {
	return len(a.Suggestions) > 0 && s.ext.Suggest != nil && s.ext.Suggest.Enabled(ctx, a.Owner) == ""
}

// renderAskPage writes ask a's page (or, for ?part=live, its live region).
func (s *Server) renderAskPage(w http.ResponseWriter, r *http.Request, a attention.Ask) {
	it := s.askItemNow(r.Context(), a)
	it.proposalCard = s.isProposalCard(r.Context(), a)
	// A chief proposal's card: its body (the kickoff) from the proposal row;
	// one that could not be read offers no accept (no start, no End it).
	it.proposal = s.proposalKickoffBody(r.Context(), a)
	if a.OwnerCard {
		it.pageOpen = s.pageAskOpen(r.Context(), a.ID)
	}
	if !s.askShowsSuggestions(r.Context(), a) {
		it.suggestions = nil
	}
	now := time.Now()
	live := askLiveHTML(a, it)
	if r.URL.Query().Get("part") == "live" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(live))
		return
	}
	esc := html.EscapeString
	// THE SAME CARD AS THE FEED'S, EXPANDED (asks_feed.go, ui/asks_card.go):
	// its badge and who, the question, the context and media slots, the
	// controls, and its footer — View in session, the task, Dismiss — at its
	// bottom. data-asks-fixed: it is always open, so a tap toggles nothing.
	slot := []attentionItem{it}
	s.withFocus(r.Context(), slot)
	kind, label := askFeedBadge(slot[0])
	// WHO ASKS is named as the feed's card names it (attentionTitle): the
	// session's name — never its tmux address, which is the card's ⓘ
	// (asksCardContextRow) — and on which computer.
	source, place, _ := attentionTitle(slot[0])
	// A notice (attention.KindNotice) is drawn as one: no options, no answer
	// box, only its session and Dismiss.
	notice := a.Kind == attention.KindNotice
	heading, dismiss := source+" is asking", "Dismiss without answering"
	switch {
	case a.Status != attention.Open:
		// A settled ask is not asking any more: its state is the heading.
		heading = askClosedState(a.Status)
	case a.StepUp != nil:
		// A step-up card is muster's own: its action is the heading.
		heading = textplain.Title(stepUpTitleText(a.StepUp))
	case notice:
		heading = source + noticePushTitleSuffix
	}
	if notice {
		dismiss = "Dismiss"
	}
	// The badge row: the badge, where (when it is not the heading's name) and
	// how long ago — and, on a settled page only, who asked: an open ask's
	// heading says who is asking, a settled one's says only its state.
	where := ""
	if place != "" && place != source {
		where = esc(place) + " · "
	}
	if a.Status != attention.Open {
		verb := "asked"
		if notice {
			verb = "sent a notice"
		}
		where = `<span class="font-semibold text-fg" data-ask-source>` + esc(source) + `</span> ` + verb + ` · ` + where
	}
	// With options to tap, they are the primary action: Answer is quiet and
	// Dismiss is a text link (a step-up card draws neither options nor box).
	optioned := a.StepUp == nil && !notice && len(withoutProposalStarts(it.proposal, a.Options)) > 0
	dismissClass := flowsQuietButton + ` w-full`
	if optioned {
		dismissClass = askDismissLinkClass
	}
	// The footer's links (askContextLinkList), known now: a lone ⓘ (no context
	// words beside it) goes there, after them, as on the feed's card
	// (asksCardContextRow), instead of on an empty row of its own.
	links := s.askContextLinkList(r.Context(), a, it)
	inner, info := asksCardContextInner(slot[0], now), attentionPlaceSheetHTML(slot[0], slot[0].Key)
	if inner == "" && info != "" && len(links) > 0 {
		links = append(links, info)
		info = ""
	}
	var b strings.Builder
	b.WriteString(`<section id="ask-page" class="flex min-w-0 flex-col gap-4" data-testid="ask-page" data-ask-id="` + strconv.FormatInt(a.ID, 10) + `">
<style>body:has(#ask-live [data-ask-closed]) :is(#app-compose, [data-ask-dismiss-form]) { display: none; }</style>
<article class="` + ui.AsksFeedCardClass + ` gap-4 p-4" data-asks-card data-asks-fixed data-asks-page-card data-expanded="true" data-kind="` + esc(it.Kind) + `">
` + ui.AsksHeadHTML(ui.AsksHead{Badge: ui.AsksKindBadge(kind, label), Who: askPageWhoHTML(where, `<time datetime="`+
		a.CreatedAt.UTC().Format(time.RFC3339)+`">`+esc(attentionAge(now.Sub(a.CreatedAt)))+`</time>`), Chip: ui.AskIDChipHTML(a.ID)}) + `
` + officerNoteLabelHTML(a.Context) + askQuestionHTML(a) + asksCardContextRow(inner, info) + askPageMediaSlot(slot[0]))
	// The proposal body, which the ask's question never holds.
	b.WriteString(proposalBodyHTML(it.proposal))
	// What a builtin officer's ask is about, apart from its note.
	b.WriteString(officerContextHTML(a.Context))
	// A closed ask — and a step-up card — has no answer box, where the notices
	// show: an answer or a dismissal refused because the ask closed meanwhile
	// (?notice=ask-closed), or a step-up card's answer refused
	// (?notice=ask-needs-phone), is said here instead.
	if note, _ := composerNotice(r); note != "" && (a.Status != attention.Open || a.StepUp != nil) {
		b.WriteString(`<p role="status" class="rounded-2xl bg-s1 px-4 py-3 text-sm text-fg ring-1 ring-line" data-ask-notice>` + esc(note) + `</p>
`)
	}
	b.WriteString(`<div id="ask-live" class="flex min-w-0 flex-col gap-4" aria-live="polite">` + live + `</div>
`)
	// The footer: the way to the session and the task, then Dismiss.
	if len(links) > 0 {
		b.WriteString(`<div class="` + ui.AsksCardFooterClass + ` gap-x-4 border-t border-line pt-2" data-asks-footer>` + strings.Join(links, "") + "</div>\n")
	}
	if a.Status == attention.Open {
		b.WriteString(`<form method="POST" action="` + attentionPath + `/asks/` + strconv.FormatInt(a.ID, 10) + `/cancel" hx-boost="false" data-ask-dismiss-form>
<input type="hidden" name="return" value="` + esc(askPagePath(a.ID)) + `">
<button type="submit" class="` + dismissClass + `" data-ask-dismiss>` + dismiss + `</button></form>
`)
	}
	b.WriteString(`</article>
</section>
`)
	b.WriteString(ui.AttentionLiveScript("ask-live", askPagePath(a.ID)+"?part=live", EventAttentionChanged))
	b.WriteString(askPageScript)
	// The header's id chip (ui/ask_id.go): plain text until this runs.
	b.WriteString(ui.IDCopyScriptHTML())

	app := &ui.NavApp{Back: attentionPath, BackLabel: "Back to Asks", Heading: heading}
	if a.Status == attention.Open && !notice && a.StepUp == nil {
		// 🔴 NO ANSWER BOX ON A STEP-UP CARD: only the paired phone answers it
		// (errHostDecisionNeedsPhone refuses every other door).
		// 🔴 A MULTI-SELECT ask is NEVER "quiet": its note box rides with the
		// free-text row, so the row stays open (quiet=false) and the boxes
		// beside it are the pick controls.
		app.Composer = askComposerHTML(r, a, s.dictatePath(), s.dictateOff(), optioned && !a.Multi)
	}
	s.writeNavDocument(w, r, http.StatusOK, "Question", ui.NavAttention, false, b.String(), app)
}

// askPageWhoHTML is the ask page card's head lines, as the Open card's are
// (ui.AsksWhoHTML): who asked and where (where, already HTML, its trailing
// " · " dropped) on the title line, and how long ago (age) under it — so the
// age is never the part a phone's width cuts off. With no where, the age
// alone.
func askPageWhoHTML(where, age string) string {
	where = strings.TrimSuffix(where, " · ")
	if where == "" {
		return `<span class="` + ui.AsksSubClass + `">` + age + `</span>`
	}
	return ui.AsksWhoHTML(`<span class="`+ui.AsksTitleClass+` font-normal text-fg2" data-ask-place>`+where+`</span>`, age)
}

// askQuestionHTML is the ask page's question: a step-up card's parts
// (stepUpCardHTML) when it has them, else its question as filed — which is how
// a card filed before 0171 still renders.
func askQuestionHTML(a attention.Ask) string {
	if a.StepUp != nil {
		return stepUpCardHTML(a.StepUp)
	}
	return `<div class="whitespace-pre-wrap break-words text-lg leading-relaxed text-fg" data-ask-question>` + askTextHTML(a.Question) + "</div>\n"
}

// The ask page's own words and classes.
const (
	// askStepUpPhoneLine replaces a step-up card's options and answer box:
	// the browser can never answer one (errHostDecisionNeedsPhone).
	askStepUpPhoneLine = "Confirm this on your phone."
	// askOtherAnswerLabel opens the free-text answer row an ask with options
	// draws collapsed (askComposerHTML).
	askOtherAnswerLabel = "Other answer…"
	// askOtherAnswerClass is that control: quiet, a text link's weight —
	// the options above it are the page's primary action.
	askOtherAnswerClass = "inline-flex min-h-[44px] items-center self-start rounded-lg px-1 text-sm font-semibold text-fg2 underline transition hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus"
	// askErrorClass is the inline line under the answer form when an answer
	// could not be posted (askPageScript).
	askErrorClass = "px-1 pt-2 text-sm text-st-error-fg"
	// askSendClass is Answer when it is the page's primary action (no options).
	askSendClass = "press min-h-[44px] shrink-0 rounded-full bg-accent px-5 text-base font-semibold text-on-accent transition hover:bg-accent/90 " + ui.AsksSendDisabledClass
	// askDismissLinkClass is Dismiss drawn as a text link, beside options
	// that are the primary action.
	askDismissLinkClass = "inline-flex min-h-[44px] items-center self-start rounded-lg px-1 text-sm font-semibold text-fg2 underline transition hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus"
)

// askStepUpPhoneHTML is a step-up card's one line in place of its options and
// answer box, on the ask page and the feed's card alike: plain text in the
// card's secondary colour — no border, no fill, no button weight — since
// nothing here can be tapped (only the paired phone answers the card).
// size is its text size class.
func askStepUpPhoneHTML(size string) string {
	return `<p class="break-words ` + size + ` text-fg2" data-ask-stepup-phone>` + askStepUpPhoneLine + `</p>`
}

// askClosedState is a settled ask's state in words: the ask page's heading and
// its live region's outcome line.
func askClosedState(st attention.Status) string {
	switch st {
	case attention.Answered:
		return "Answered"
	case attention.AnsweredInSession:
		return "Answered in the session"
	case attention.Cancelled:
		return "Dismissed"
	case attention.Expired:
		return "Expired"
	}
	return "No longer open"
}

// askPageMediaSlot is the ask page's media slot: the feed card's
// (asksCardMediaSlot), its images drawn large (ui.AskMediaStripLarge) — the
// page is where an attachment is looked at.
func askPageMediaSlot(it attentionItem) string {
	if it.Kind != attentionKindAsk || len(it.Attachments) == 0 {
		return asksCardMediaSlot(it)
	}
	return ui.AskMediaStripLarge(askMediaView(it.AskID, it.Attachments)) + "\n" + pageAskOpenHTML(it.pageOpen)
}

// askContextLinkList is the ask's way to its context, the ask page's footer
// links: the asking session's or agent's page (attentionSessionPath, the rule
// the tap used to follow), and the task it is about; none for neither.
func (s *Server) askContextLinkList(ctx context.Context, a attention.Ask, it attentionItem) []string {
	esc := html.EscapeString
	cls := ui.AsksCardLinkClass
	var links []string
	if p := s.attentionSessionPath(ctx, a.Owner, it); p != "" {
		// "View in session" for a mirrored session and an agent alike (the
		// operator's words); the Asks list's cards say the same (withAskBoxes).
		links = append(links, `<a href="`+esc(p)+`" class="`+cls+`" data-ask-context>`+askViewInSession+`</a>`)
	}
	if a.TaskID > 0 {
		links = append(links, `<a href="/tasks/`+strconv.FormatInt(a.TaskID, 10)+`" class="`+cls+`" data-ask-task>Open task #`+strconv.FormatInt(a.TaskID, 10)+`</a>`)
	}
	return links
}

// askLiveHTML is the ask page's live region: an open ask's options and
// suggested replies, or a settled ask's outcome.
func askLiveHTML(a attention.Ask, it attentionItem) string {
	esc := html.EscapeString
	var b strings.Builder
	if a.Status != attention.Open {
		state := askClosedState(a.Status)
		b.WriteString(`<div class="flex flex-col gap-2 rounded-2xl bg-s1 px-4 py-3 ring-1 ring-line" data-ask-closed data-status="` + esc(string(a.Status)) + `">
<p class="text-base font-semibold text-fg">` + esc(state) + `</p>
`)
		if a.Answer != "" {
			b.WriteString(`<div class="whitespace-pre-wrap break-words text-base text-fg" data-ask-answer>` + answerLabel(a) + askTextHTML(a.Answer) + `</div>
`)
		}
		if a.Status == attention.Answered {
			// The session in words (attentionTitle), as the card's head names
			// it; its exact target is the card's ⓘ.
			who, _, _ := attentionTitle(it)
			text, _ := answerDeliveryText(a, who, it.proposalCard)
			b.WriteString(`<p class="break-words text-sm text-fg2" data-ask-delivery>` + esc(text) + `</p>
`)
		}
		b.WriteString(`<a href="` + attentionPath + `" class="inline-flex min-h-[44px] items-center text-sm font-semibold text-accent underline">Back to Asks</a>
</div>`)
		return b.String()
	}
	if a.StepUp != nil {
		// A step-up card is answered only from the paired phone
		// (errHostDecisionNeedsPhone): no options, no suggestions, one line —
		// a status line, never drawn as a control (askStepUpPhoneHTML).
		return askStepUpPhoneHTML("text-base")
	}
	if opts := withoutProposalStarts(it.proposal, a.Options); len(opts) > 0 {
		if a.Multi {
			// A MULTI-SELECT ask (migration 0175): a CHECKBOX per option,
			// form-associated with the answer form (form="ask-answer") so a
			// tap of the one Send below posts every box ticked — joined by
			// attention.OptionSep — with the free text and note if any. The
			// free-text row is NOT collapsed for a multi ask (askComposerHTML
			// gets quiet=false): the note belongs to the same answer.
			b.WriteString(`<div role="group" aria-label="The asker's options" class="flex flex-col gap-2" data-ask-options>
`)
			for _, o := range opts {
				b.WriteString(`<label class="` + ui.AsksOptionCheckClass + `"><input type="checkbox" form="ask-answer" name="option" value="` + esc(o) + `" class="` + ui.AsksOptionCheckBoxClass + `" data-ask-option>` + esc(o) + `</label>
`)
			}
			b.WriteString(`<button type="submit" form="ask-answer" class="` + ui.AsksMultiSendClass + `" data-ask-multi-send>Send answer</button>
</div>
`)
		} else {
			b.WriteString(`<div role="group" aria-label="The asker's options" class="flex flex-col gap-2" data-ask-options>
`)
			for _, o := range opts {
				b.WriteString(`<button type="submit" form="ask-answer" name="option" value="` + esc(o) + `" class="` + ui.AsksOptionClass + ` break-words text-base" data-ask-option>` + esc(o) + `</button>
`)
			}
			b.WriteString("</div>\n")
		}
	}
	if len(it.suggestions) > 0 {
		b.WriteString(`<div role="group" aria-label="` + suggestLabel + `" class="flex flex-col gap-2" data-ask-suggestions>
<p class="text-xs text-fg2">` + ui.AsksAIPillHTML + ` suggestions — tap to edit</p>
<div class="flex flex-wrap gap-2">
`)
		for _, t := range it.suggestions {
			b.WriteString(`<button type="button" data-ask-suggestion data-text="` + esc(t) + `" aria-label="AI-suggested reply: ` + esc(t) +
				`" class="` + ui.AsksCardChipClass + `">` + esc(t) + `</button>
`)
		}
		b.WriteString("</div></div>\n")
	}
	return b.String()
}

// askComposerHTML is an open ask's answer box, pinned to the bottom of its
// page: free text, posted to the queue's answer route, back to this page —
// with the microphone (ui.DictateControlHTML) beside Answer when dictate is
// not "". The mic's ✓ FILLS the box at the caret; its ↑ fills it and clicks Answer
// (ui.DictateSendHTML), so only Answer sends. Its script is
// the composer's LAST child: the composer is drawn after the page's content,
// and the script's first run reveals only the mics already parsed.
//
// quiet: the ask has options to tap, which are its primary action, so Answer
// is drawn quiet (ui.AsksCardSendQuietClass). Answer starts OFF (aria-disabled:
// ui.AsksSendDisabledClass says why not disabled) — the box is empty — and
// askPageScript turns it on while the box holds text.
func askComposerHTML(r *http.Request, a attention.Ask, dictate string, dictateOff, quiet bool) string {
	esc := html.EscapeString
	id := strconv.FormatInt(a.ID, 10)
	note, ok := composerNotice(r)
	noteAttrs := ` hidden`
	if note != "" {
		noteAttrs = ""
		if ok {
			noteAttrs = ` data-ok`
		}
	}
	send := askSendClass
	// QUIET: the options are the answer, so the free-text row is drawn
	// collapsed behind "Other answer…" (askPageScript opens it and moves focus
	// into the box). The form stays in the document, hidden: the option
	// buttons submit it (form="ask-answer").
	other, formHidden := "", ""
	if quiet {
		send = ui.AsksCardSendQuietClass + " " + ui.AsksSendDisabledClass
		other = `<button type="button" class="` + askOtherAnswerClass + `" aria-controls="ask-answer" aria-expanded="false" data-ask-other>` + askOtherAnswerLabel + `</button>
`
		formHidden = ` hidden`
	}
	// A MULTI-SELECT ask (migration 0175) adds the optional NOTE: its own
	// name="note", form-associated with the answer form so the one Send posts
	// it with the boxes ticked. It sits above the form, beside the free-text
	// row that stays open for a multi ask (quiet is false).
	noteBox := ""
	if a.Multi {
		noteBox = `<div class="` + ui.AsksNoteRowClass + `" data-ask-note-row>
<label for="ask-input-note" class="` + ui.AsksNoteLabelClass + `">Note (optional)</label>
<textarea id="ask-input-note" name="note" rows="1" form="ask-answer" placeholder="Add a note…" maxlength="` + strconv.Itoa(attention.MaxNoteRunes) + `" class="` + ui.AsksNoteClass + `" data-ask-note></textarea>
</div>
`
	}
	return `<p id="ask-note" role="status" aria-live="polite" class="px-1 pb-2 text-sm text-fg"` + noteAttrs + `>` + esc(note) + `</p>
` + ui.DictateNoteHTML(dictate, "ask-dictate-note", "px-1 pb-2") + other + noteBox + `<form id="ask-answer" method="POST" action="` + attentionPath + `/asks/` + id + `/answer" hx-boost="false" class="flex items-end gap-2" data-attention-answer` + formHidden + `>
<input type="hidden" name="return" value="` + esc(askPagePath(a.ID)) + `">
<label for="ask-input" class="sr-only">Your answer</label>
<textarea id="ask-input" name="answer" rows="1" maxlength="` + strconv.Itoa(attention.MaxAnswerRunes) + `" enterkeyhint="send" placeholder="` +
		"Your answer…" + `" class="max-h-40 min-h-[44px] min-w-0 flex-1 resize-none rounded-2xl bg-s1 px-4 py-2.5 text-base text-fg ring-1 ring-inset ring-edge placeholder:text-fg2 focus:outline-none focus:ring-2 focus:ring-focus"></textarea>
` + ui.DictateWaveHTML(dictate, "ask-dictate") + answerMic(dictate, dictateOff, "ask-input", "ask-dictate-note", "ask-dictate") +
		ui.DictateSendHTML(dictate, "ask-dictate", "ask-send", ui.DictateSendPill) +
		`<button type="submit" id="ask-send" class="` + send + `" aria-disabled="true" data-ask-send>Answer</button>
</form>
<p id="ask-error" role="alert" class="` + askErrorClass + `" hidden data-ask-error></p>` + ui.DictateScriptHTML(dictate)
}

// What the ask page says under its answer form when an answer could not be
// posted (askPageScript): the server failed, or it could not be reached.
const (
	askPostFailedText  = "Your answer wasn't sent: muster couldn't record it. Try again."
	askPostOfflineText = "Your answer wasn't sent: muster couldn't be reached. Check your connection and try again."
)

// askPageScript: a suggestion chip fills the answer box (and grows it to its
// text); the box grows as it is typed in; Answer is enabled only while the box
// holds text (it is drawn aria-disabled: the box starts empty), and a blank
// answer is not posted. A chip never sends; the answer form's own submit (an
// option, Answer, the mic's ↑ clicking Answer) is posted in place, so a post
// that fails is said inline (#ask-error) with the typed text kept. "Other
// answer…" opens the collapsed free-text row of an ask with options.
const askPageScript = `<script>
(function () {
  // The answer box is drawn AFTER this script (it is pinned below the
  // content), so it is looked up when used, never at load.
  function grow(b) { b.style.height = 'auto'; b.style.height = Math.min(b.scrollHeight, 160) + 'px'; }
  // gatePick: the multi-select ask's first TICKED box of form f (migration
  // 0175), null for none and for a single-select ask (no such box).
  //
  // 🔴 f.elements, NOT f.querySelector: a multi ask's checkboxes are
  // FORM-ASSOCIATED with f (form="ask-answer") but are drawn in the live region
  // ABOVE it, so they are never DOM DESCENDANTS of the form — a querySelector
  // subtree walk cannot see them and returned null however many were ticked,
  // leaving the composer's Answer off and its tap preventDefault()ed.
  // f.elements is every control the form owns, wherever it sits in the
  // document.
  function gatePick(f) {
    if (!f || !f.elements) return null;
    var els = f.elements;
    for (var i = 0; i < els.length; i++) {
      var el = els[i];
      if (el.name === 'option' && el.type === 'checkbox' && el.checked) return el;
    }
    return null;
  }
  function gate(b) { var s = document.getElementById('ask-send'); if (!s) return; if (b.value.trim() || gatePick(b.form)) s.removeAttribute('aria-disabled'); else s.setAttribute('aria-disabled', 'true'); }
  // OTHER ANSWER…: an ask with options draws its free-text row collapsed
  // (askComposerHTML); opening it shows the row and puts the caret in the box.
  function openOther(focus) {
    var f = document.getElementById('ask-answer'), o = document.querySelector('[data-ask-other]');
    if (!f || !f.hidden) return;
    f.hidden = false;
    if (o) { o.setAttribute('aria-expanded', 'true'); o.hidden = true; }
    var b = document.getElementById('ask-input');
    if (b && focus) b.focus();
  }
  function errorLine(text) {
    var el = document.getElementById('ask-error');
    if (!el) return;
    el.textContent = text || '';
    el.hidden = !text;
  }
  // THE ANSWER IS POSTED IN PLACE: on success the page goes where the
  // server's redirect says (the same page, settled, or with its notice), as
  // the plain form post did — a refused answer (closed, invalid) comes back
  // as that redirect with its notice; a post that FAILS (a server or proxy
  // error, no network) says so in one line under the form and keeps the page
  // — the typed text included — so the same tap retries it; a 401 or 403
  // (signed out, another site's post) loads the page again. Without fetch the
  // form posts as before.
  var posting = false;
  document.addEventListener('submit', function (e) {
    var f = e.target, b = document.getElementById('ask-input');
    if (!b || !f || f.id !== 'ask-answer') return;
    // A blank answer is not posted (Answer is aria-disabled, not disabled:
    // ui.AsksSendDisabledClass says why): the box is focused instead. A
    // MULTI-SELECT ask's composer Answer counts a ticked box as an answer
    // too (migration 0175), so only "no text AND no pick" is blank.
    if (e.submitter && e.submitter.id === 'ask-send' && !b.value.trim() && !gatePick(f)) { e.preventDefault(); b.focus(); return; }
    if (!window.fetch || !window.URLSearchParams || !window.FormData) return;
    e.preventDefault();
    if (posting) return;
    // The tapped button's name=value (an option) rides along: FormData's
    // submitter argument where the browser takes it, appended by hand where
    // it is ignored (an older browser drops it without throwing).
    var sub = e.submitter, data;
    try { data = new FormData(f, sub || undefined); } catch (err) { data = new FormData(f); }
    if (sub && sub.name && !data.has(sub.name)) data.append(sub.name, sub.value);
    posting = true;
    errorLine('');
    f.setAttribute('aria-busy', 'true');
    function failed(text) {
      posting = false;
      f.removeAttribute('aria-busy');
      errorLine(text);
    }
    fetch(f.action, { method: 'POST', body: new URLSearchParams(data), credentials: 'same-origin', headers: { 'Accept': 'text/html' } }).then(function (r) {
      if (r.ok) { window.location.assign(r.url || window.location.href); return; }
      // Signed out, or refused as another site's post: no retry here can
      // succeed, so the page is loaded again and its sign-in gate takes over.
      if (r.status === 401 || r.status === 403) { posting = false; window.location.assign(window.location.href); return; }
      failed(` + "`" + askPostFailedText + "`" + `);
    }, function () {
      failed(` + "`" + askPostOfflineText + "`" + `);
    });
  });
  document.addEventListener('input', function (e) { if (e.target && e.target.id === 'ask-input') { grow(e.target); gate(e.target); } });
  // A multi-select box ticked or unticked re-decides whether the composer's
  // Answer is on (a pick alone is a complete answer, migration 0175).
  document.addEventListener('change', function (e) {
    var t = e.target;
    if (!t || t.type !== 'checkbox' || t.name !== 'option') return;
    var b = document.getElementById('ask-input');
    if (b) gate(b);
  });
  // A box the browser restored (back, a reload) holds text: Answer follows
  // it, and a collapsed row holding text is shown. A page restored from the
  // back-forward cache mid-post (it navigated away on success) is no longer
  // posting: its form is usable again.
  window.addEventListener('pageshow', function () {
    posting = false;
    var f = document.getElementById('ask-answer');
    if (f) f.removeAttribute('aria-busy');
    var b = document.getElementById('ask-input');
    if (!b) return;
    if (b.value.trim()) openOther(false);
    gate(b);
  });
  document.addEventListener('click', function (e) {
    if (e.target.closest && e.target.closest('[data-ask-other]')) { e.preventDefault(); openOther(true); return; }
    var chip = e.target.closest && e.target.closest('[data-ask-suggestion]');
    var b = document.getElementById('ask-input');
    if (!chip || !b) return;
    e.preventDefault();
    openOther(false);
    var t = chip.getAttribute('data-text') || '';
    b.value = b.value.trim() ? b.value.replace(/\s+$/, '') + '\n' + t : t;
    grow(b);
    gate(b);
    b.focus();
    try { b.setSelectionRange(b.value.length, b.value.length); } catch (err) {}
  });
})();
</script>`

// answerMic is an answer box's microphone: the real one (button, for the box
// input, reporting into note), or — when the server has no dictation — the
// muted one linking to its Features ⓘ sheet, id button+"-off". The ask page and
// every Asks answer box draw it, so the two cannot differ.
func answerMic(dictate string, off bool, input, note, button string) string {
	if dictate == "" && off {
		return ui.DictateOffControlHTML(button + "-off")
	}
	return ui.DictateControlHTML(dictate, input, note, button)
}
