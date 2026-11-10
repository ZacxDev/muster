package api

import (
	"context"
	"errors"
	"html"
	"net/http"
	"strings"

	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/stepup"
)

// A WAITING OPENCODE WINDOW IN THE ATTENTION QUEUE (docs/design/opencode.md,
// "Asks"). Its host reads what it waits on from the opencode server: a
// permission request, or a form opencode's question tool raised. It is shown
// — and NOTIFIED — exactly as the Claude Code window it stands beside:
//
//   - a QUESTION is a "question" item, as a Claude Code AskUserQuestion is:
//     "<window> is asking", its question and its options in the push;
//   - a PERMISSION REQUEST is a "session" item, as a Claude Code permission
//     prompt is: "A session is waiting on you", "Waiting: <tool> · <window>"
//     — the tool's NAME only on the lock screen, the command on the page.
//
// Both are read LIVE from the hub's window list, never stored: settled in the
// terminal, the window stops waiting, and the item and its notification are
// withdrawn by the same diff that withdraws a Claude window's
// (attentionMirrorChanged).
//
// An answer is RELAYED through the host to opencode's API — Allow once /
// Reject for a permission (never "always"), one of the question's own options
// — only when the host relays (-interactive, as for Claude Code) and the ask
// is one muster can answer (a single single-select question of at most
// mirror.MaxPendingOptions options). Otherwise the item still notifies, and
// says why it cannot be answered from here.
//
// 🔴 THE GATES: the session's owner (another owner's host is not found), same-origin
// (requireOperatorWrite), a fresh sign-in (internal/stepup) — and the hub and
// host check again that the window still waits on exactly this ask, offering
// this answer.

// attentionOpenCodePath answers an opencode ask (the no-script form).
const attentionOpenCodePath = attentionPath + "/opencode/answer"

// openCodeAsk is a waiting opencode window's ask as an attention item shows
// it.
type openCodeAsk struct {
	// kind is mirror.PendingPermission or mirror.PendingQuestion.
	kind string
	// promptID and choices: what muster can relay here, "" and nil when it
	// cannot; why then says why (the item says to answer in the terminal).
	promptID string
	choices  []string
	why      string
}

// The words an opencode ask shows when it cannot be answered here.
const (
	// openCodeNoRelayText: the host does not relay answers to opencode.
	openCodeNoRelayText = "This host's muster-mirror is read-only (not -interactive), so this can't be answered from here: answer it in the terminal."
	// openCodeTerminalText: the ask is not one muster answers (several
	// questions, a multi-select, a free-text form, too many options).
	openCodeTerminalText = "muster can't answer this one from here: answer it in the terminal."
	// openCodeViewText: a question muster answers in the session's view, not
	// with a button here.
	openCodeViewText = "Open the session to answer it: its view shows the whole question."
	// openCodeOfflineText: the host is offline.
	openCodeOfflineText = "The host is offline: answer it in the terminal."
)

// The words an answered or refused opencode ask shows.
const (
	openCodeAnsweredText = "Answered here: opencode took the answer."
	openCodeGoneText     = "opencode is no longer waiting on that (it was answered in the terminal, or the turn ended). Nothing was answered here."
	openCodeUnreachText  = "muster could not reach the opencode server on that host, so nothing was answered: answer it in the terminal."
	openCodeUnknownText  = "muster sent the answer but did not hear back: it may have been answered. Check the terminal before answering again."
	openCodeRefusedText  = "opencode refused that answer: the question can't take that value. Nothing was answered, and it is still waiting: answer it in the terminal."
)

// openCodeItem fills it — a waiting opencode window e's item — from what it
// waits on (p, nil for nothing named) and whether its host relays answers
// now (answers).
func (s *Server) openCodeItem(it *attentionItem, e mirror.SessionView, p *mirror.Pending, answers bool) {
	it.claudeSession = ""
	if key := e.SessionKey(); key != "" {
		// Its focus is keyed runtime-qualified ("opencode:<id>").
		it.claudeSession, it.subject = key, focus.MirrorSubject(key)
	}
	ask := &openCodeAsk{}
	it.openCode = ask
	if p == nil || p.Kind == "" {
		return // the generic "waiting" session item
	}
	ask.kind = p.Kind
	switch p.Kind {
	case mirror.PendingQuestion:
		// As a Claude Code AskUserQuestion: a question item, its choices in
		// its push.
		it.Kind = attentionKindQuestion
		if p.Summary != "" {
			it.Text = p.Summary
		}
		it.choices = p.OptionList()
	default:
		if p.Summary != "" {
			it.Text = p.Summary
		}
		it.Detail, it.tool = p.Detail, p.Tool
	}
	var offered []string
	if p.PromptID != "" {
		if p.Kind == mirror.PendingQuestion {
			offered = p.OptionList()
		} else {
			offered = strings.Fields(p.Choices)
		}
	}
	switch {
	case len(offered) == 0 && p.Kind == mirror.PendingQuestion && p.PromptID != "" && answers && !it.Offline:
		// A whole form (several questions, a multi-select, long options):
		// the session's view answers it (mirror_opencode_ask.go).
		ask.why = openCodeViewText
	case len(offered) == 0:
		ask.why = openCodeTerminalText
	case it.Offline:
		ask.why = openCodeOfflineText
	case !answers:
		ask.why = openCodeNoRelayText
	default:
		ask.promptID, ask.choices = p.PromptID, offered
	}
}

// openCodeChoiceLabel is a permission answer as its button says it.
var openCodeChoiceLabel = map[string]string{"once": "Allow once", "reject": "Reject"}

// openCodeAskHTML is a waiting opencode item's answer area: its answers as
// buttons when muster can relay one, else why it can't. "" for an item that
// is not an opencode window's.
func openCodeAskHTML(it attentionItem) string {
	ask := it.openCode
	if ask == nil {
		return ""
	}
	esc := html.EscapeString
	note := func(s string) string {
		return `<p class="text-sm text-fg2" data-opencode-note>` + esc(s) + `</p>
`
	}
	if ask.kind == "" {
		return ""
	}
	if ask.promptID == "" || len(ask.choices) == 0 {
		return note(ask.why)
	}
	ref := it.card.ref
	var b strings.Builder
	b.WriteString(`<form method="POST" action="` + attentionOpenCodePath + `" class="flex flex-col gap-2" data-opencode-answer>
<input type="hidden" name="host" value="` + esc(ref.Host) + `">
<input type="hidden" name="session" value="` + esc(ref.Session) + `">
<input type="hidden" name="window" value="` + esc(ref.Window) + `">
<input type="hidden" name="prompt" value="` + esc(ask.promptID) + `">
<div class="flex flex-wrap gap-2">`)
	for _, c := range ask.choices {
		label, class := c, flowsQuietButton
		if ask.kind == mirror.PendingPermission {
			l, ok := openCodeChoiceLabel[c]
			if !ok {
				continue
			}
			label = l
			if c == "reject" {
				class = flowsButtonClass
			}
		}
		b.WriteString(`<button type="submit" name="choice" value="` + esc(c) + `" class="` + class + `" data-opencode-choice="` + esc(c) + `">` + esc(label) + `</button>`)
	}
	b.WriteString(`</div>
</form>
`)
	return b.String()
}

// openCodeAnswerer relays an answer to an opencode ask; *mirror.Service
// implements it.
type openCodeAnswerer interface {
	AnswerOpenCodeAsk(ctx context.Context, host, session, window, promptID, choice string) error
}

// handleAttentionOpenCodeAnswer is POST /attention/opencode/answer (host,
// session, window, prompt, choice): relay that answer to the window's opencode
// ask through its host, then back to the queue.
func (s *Server) handleAttentionOpenCodeAnswer(w http.ResponseWriter, r *http.Request) {
	if s.ext.Mirror == nil {
		http.NotFound(w, r)
		return
	}
	a, can := s.ext.Mirror.(openCodeAnswerer)
	if !can {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		s.renderAttentionPage(w, r, http.StatusBadRequest, "That answer could not be read.")
		return
	}
	h, e, listed, _, found := s.mirrorEntry(r, r.PostFormValue("host"), r.PostFormValue("session"), r.PostFormValue("window"))
	if !found || !listed {
		s.renderAttentionPage(w, r, http.StatusNotFound, "That window is gone: "+openCodeGoneText)
		return
	}
	if !stepup.Fresh(r.Context(), s.stepUpNow()) {
		w.Header().Set("X-Muster-Step-Up", "required")
		s.renderAttentionPage(w, r, http.StatusForbidden, stepup.ErrStale.Error())
		return
	}
	// 🔴 NOT THE REQUEST'S CONTEXT: a page that goes away must not cut the
	// wait while the host may be answering. The hub bounds it.
	err := a.AnswerOpenCodeAsk(context.WithoutCancel(r.Context()), h.Host, e.Session, e.Window, r.PostFormValue("prompt"), r.PostFormValue("choice"))
	if err != nil {
		code, text := http.StatusConflict, openCodeGoneText
		switch {
		case errors.Is(err, mirror.ErrStepUpRequired):
			code, text = http.StatusForbidden, stepup.ErrStale.Error()
		case errors.Is(err, mirror.ErrOpenCodeAnswerUnsupported), errors.Is(err, mirror.ErrAskUnsupported):
			text = openCodeNoRelayText
		case errors.Is(err, mirror.ErrOpenCodeUnreachable):
			code, text = http.StatusBadGateway, openCodeUnreachText
		case errors.Is(err, mirror.ErrOpenCodeAnswerUnknown):
			code, text = http.StatusGatewayTimeout, openCodeUnknownText
		case errors.Is(err, mirror.ErrOpenCodeAnswerRefused):
			code, text = http.StatusUnprocessableEntity, openCodeRefusedText
		case errors.Is(err, mirror.ErrHostOffline):
			code, text = http.StatusBadGateway, "That host is offline: nothing was answered. Answer it in the terminal."
		case errors.Is(err, mirror.ErrOpenCodeAskGone):
		default:
			s.logger.Printf("attention: answer an opencode ask on %s: %v", h.Host, err)
			code, text = http.StatusBadGateway, "It was not answered: "+mirror.OneLine(err.Error(), 200)
		}
		s.renderAttentionPage(w, r, code, text)
		return
	}
	s.logger.Printf("attention: answered an opencode ask on %s", h.Host)
	// The ask log (ask_log.go): the answer muster relayed.
	s.askLogOpenCodeAnswered(requestOwner(r.Context()), r.PostFormValue("prompt"), r.PostFormValue("choice"))
	http.Redirect(w, r, attentionPath+"?answered=opencode", http.StatusSeeOther)
}

// attentionNotice is the page's notice for a query that names one (an
// answer that went through), "" for none.
func attentionNotice(r *http.Request) string {
	if r.Method != http.MethodGet || r.URL.Query().Get("answered") != "opencode" {
		return ""
	}
	return `<p role="status" class="rounded-lg bg-s2 px-3 py-2 text-sm text-fg ring-1 ring-inset ring-line" data-testid="attention-notice">` +
		html.EscapeString(openCodeAnsweredText) + `</p>`
}
