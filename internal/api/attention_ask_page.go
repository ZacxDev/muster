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
// view's chips do, nothing is sent until Answer; a link to the session or
// agent for context; and the answer box pinned to the bottom, above the
// keyboard and inside the safe-area insets.
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
	if err != nil || a.Owner != requestOwner(r.Context()) || a.ToChief {
		if err != nil && attentionErrCode(err) == http.StatusInternalServerError {
			s.logger.Printf("attention: the ask page of %d: %v", id, err)
		}
		s.renderAttentionPage(w, r, http.StatusNotFound, "No such question.")
		return
	}
	s.renderAskPage(w, r, a)
}

// askShowsSuggestions reports whether ask a's stored suggestions may be shown
// to its owner now: the same switches that decide whether they are made
// (MUSTER_SUGGEST=off, the owner's toggle, the auto-pause).
func (s *Server) askShowsSuggestions(ctx context.Context, a attention.Ask) bool {
	return len(a.Suggestions) > 0 && s.ext.Suggest != nil && s.ext.Suggest.Enabled(ctx, a.Owner) == ""
}

// renderAskPage writes ask a's page (or, for ?part=live, its live region).
func (s *Server) renderAskPage(w http.ResponseWriter, r *http.Request, a attention.Ask) {
	it := askItem(a)
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
	source := textplain.Title(it.Source)
	var b strings.Builder
	b.WriteString(`<section id="ask-page" class="flex min-w-0 flex-col gap-4" data-testid="ask-page" data-ask-id="` + strconv.FormatInt(a.ID, 10) + `">
<style>body:has(#ask-live [data-ask-closed]) :is(#app-compose, [data-ask-dismiss-form]) { display: none; }</style>
<p class="text-sm text-fg2"><span class="font-semibold text-fg" data-ask-source>` + esc(source) + `</span> asks · <time datetime="` +
		a.CreatedAt.UTC().Format(time.RFC3339) + `">` + esc(attentionAge(now.Sub(a.CreatedAt))) + `</time></p>
<p class="whitespace-pre-wrap break-words text-lg leading-relaxed text-fg" data-ask-question>` + esc(a.Question) + `</p>
`)
	// A closed ask has no answer box, where the notices show: an answer or a
	// dismissal refused because the ask closed meanwhile (?notice=ask-closed)
	// is said here instead.
	if note, _ := composerNotice(r); note != "" && a.Status != attention.Open {
		b.WriteString(`<p role="status" class="rounded-2xl bg-s1 px-4 py-3 text-sm text-fg ring-1 ring-line" data-ask-notice>` + esc(note) + `</p>
`)
	}
	if links := s.askContextLinks(r.Context(), a, it); links != "" {
		b.WriteString(links)
	}
	b.WriteString(`<div id="ask-live" class="flex min-w-0 flex-col gap-4" aria-live="polite">` + live + `</div>
`)
	if a.Status == attention.Open {
		b.WriteString(`<form method="POST" action="` + attentionPath + `/asks/` + strconv.FormatInt(a.ID, 10) + `/cancel" hx-boost="false" class="pt-2" data-ask-dismiss-form>
<input type="hidden" name="return" value="` + esc(askPagePath(a.ID)) + `">
<button type="submit" class="` + flowsQuietButton + ` w-full" data-ask-dismiss>Dismiss without answering</button></form>
`)
	}
	b.WriteString(`</section>
`)
	b.WriteString(ui.AttentionLiveScript("ask-live", askPagePath(a.ID)+"?part=live", EventAttentionChanged))
	b.WriteString(askPageScript)

	app := &ui.NavApp{Back: attentionPath, BackLabel: "Back to Asks", Heading: source + " is asking"}
	if a.Status == attention.Open {
		app.Composer = askComposerHTML(r, a)
	}
	s.writeNavDocument(w, r, http.StatusOK, "Question", ui.NavAttention, false, b.String(), app)
}

// askContextLinks is the ask's way to its context: the asking session's or
// agent's page (attentionSessionPath, the rule the tap used to follow), and the
// task it is about; "" for none.
func (s *Server) askContextLinks(ctx context.Context, a attention.Ask, it attentionItem) string {
	esc := html.EscapeString
	const cls = `inline-flex min-h-[44px] items-center gap-1 text-sm font-semibold text-accent underline`
	var links []string
	if p := s.attentionSessionPath(ctx, a.Owner, it); p != "" {
		label := "Open the session for context"
		if it.card.agentID > 0 {
			label = "Open the agent for context"
		}
		links = append(links, `<a href="`+esc(p)+`" class="`+cls+`" data-ask-context>`+label+`</a>`)
	}
	if a.TaskID > 0 {
		links = append(links, `<a href="/tasks/`+strconv.FormatInt(a.TaskID, 10)+`" class="`+cls+`" data-ask-task>Open task #`+strconv.FormatInt(a.TaskID, 10)+`</a>`)
	}
	if len(links) == 0 {
		return ""
	}
	return `<div class="flex flex-wrap gap-x-4">` + strings.Join(links, "") + "</div>\n"
}

// askLiveHTML is the ask page's live region: an open ask's options and
// suggested replies, or a settled ask's outcome.
func askLiveHTML(a attention.Ask, it attentionItem) string {
	esc := html.EscapeString
	var b strings.Builder
	if a.Status != attention.Open {
		state := map[attention.Status]string{attention.Answered: "Answered", attention.AnsweredInSession: "Answered in the session",
			attention.Cancelled: "Dismissed", attention.Expired: "Expired"}[a.Status]
		if state == "" {
			state = "No longer open"
		}
		b.WriteString(`<div class="flex flex-col gap-2 rounded-2xl bg-s1 px-4 py-3 ring-1 ring-line" data-ask-closed data-status="` + esc(string(a.Status)) + `">
<p class="text-base font-semibold text-fg">` + esc(state) + `</p>
`)
		if a.Answer != "" {
			b.WriteString(`<p class="whitespace-pre-wrap break-words text-base text-fg" data-ask-answer>` + answerLabel(a) + esc(a.Answer) + `</p>
`)
		}
		if a.Status == attention.Answered {
			text, _ := answerDeliveryText(a)
			b.WriteString(`<p class="break-words text-sm text-fg2" data-ask-delivery>` + esc(text) + `</p>
`)
		}
		b.WriteString(`<a href="` + attentionPath + `" class="inline-flex min-h-[44px] items-center text-sm font-semibold text-accent underline">Back to Asks</a>
</div>`)
		return b.String()
	}
	if len(a.Options) > 0 {
		b.WriteString(`<div role="group" aria-label="The asker's options" class="flex flex-col gap-2" data-ask-options>
`)
		for _, o := range a.Options {
			b.WriteString(`<button type="submit" form="ask-answer" name="option" value="` + esc(o) + `" class="press min-h-[48px] w-full break-words rounded-2xl bg-s2 px-4 py-3 text-left text-base font-semibold text-fg ring-1 ring-inset ring-edge transition hover:bg-s2/80" data-ask-option>` + esc(o) + `</button>
`)
		}
		b.WriteString("</div>\n")
	}
	if len(it.suggestions) > 0 {
		b.WriteString(`<div role="group" aria-label="` + suggestLabel + `" class="flex flex-col gap-2" data-ask-suggestions>
<p class="text-xs text-fg2"><span class="rounded bg-accent/15 px-1.5 py-0.5 font-semibold text-fg">AI</span> Suggested replies — a tap puts one in your answer, to edit or send</p>
<div class="flex flex-wrap gap-2">
`)
		for _, t := range it.suggestions {
			b.WriteString(`<button type="button" data-ask-suggestion data-text="` + esc(t) + `" aria-label="AI-suggested reply: ` + esc(t) +
				`" class="press min-h-[44px] max-w-full break-words rounded-2xl bg-bg px-4 py-2 text-left text-sm text-fg ring-1 ring-inset ring-accent/40 transition hover:bg-s2">` + esc(t) + `</button>
`)
		}
		b.WriteString("</div></div>\n")
	}
	return b.String()
}

// askComposerHTML is an open ask's answer box, pinned to the bottom of its
// page: free text, posted to the queue's answer route, back to this page.
func askComposerHTML(r *http.Request, a attention.Ask) string {
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
	return `<p id="ask-note" role="status" aria-live="polite" class="px-1 pb-2 text-sm text-fg"` + noteAttrs + `>` + esc(note) + `</p>
<form id="ask-answer" method="POST" action="` + attentionPath + `/asks/` + id + `/answer" hx-boost="false" class="flex items-end gap-2" data-attention-answer>
<input type="hidden" name="return" value="` + esc(askPagePath(a.ID)) + `">
<label for="ask-input" class="sr-only">Your answer</label>
<textarea id="ask-input" name="answer" rows="1" maxlength="` + strconv.Itoa(attention.MaxAnswerRunes) + `" enterkeyhint="send" placeholder="` +
		"Your answer…" + `" class="max-h-40 min-h-[44px] min-w-0 flex-1 resize-none rounded-2xl bg-s1 px-4 py-2.5 text-base text-fg ring-1 ring-inset ring-edge placeholder:text-fg2 focus:outline-none focus:ring-2 focus:ring-focus"></textarea>
<button type="submit" class="press min-h-[44px] shrink-0 rounded-full bg-accent px-5 text-base font-semibold text-on-accent transition hover:bg-accent/90" data-ask-send>Answer</button>
</form>`
}

// askPageScript: a suggestion chip fills the answer box (and grows it to its
// text); the box grows as it is typed in. Nothing here sends.
const askPageScript = `<script>
(function () {
  // The answer box is drawn AFTER this script (it is pinned below the
  // content), so it is looked up when used, never at load.
  function grow(b) { b.style.height = 'auto'; b.style.height = Math.min(b.scrollHeight, 160) + 'px'; }
  document.addEventListener('input', function (e) { if (e.target && e.target.id === 'ask-input') grow(e.target); });
  document.addEventListener('click', function (e) {
    var chip = e.target.closest && e.target.closest('[data-ask-suggestion]');
    var b = document.getElementById('ask-input');
    if (!chip || !b) return;
    e.preventDefault();
    var t = chip.getAttribute('data-text') || '';
    b.value = b.value.trim() ? b.value.replace(/\s+$/, '') + '\n' + t : t;
    grow(b);
    b.focus();
    try { b.setSelectionRange(b.value.length, b.value.length); } catch (err) {}
  });
})();
</script>`
