package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// AN OPENCODE WINDOW'S ASK, IN ITS RENDERED VIEW (docs/design/opencode.md,
// "Asks"): the window's live question region (ui.MirrorAsksPath) draws what
// an opencode window waits on — read LIVE from its host's listing (its
// Pending), never stored — as the same card a Claude Code question gets:
//
//   - a QUESTION (the question tool's form): every question of the form, its
//     options and an answer of one's own, from the form the host sends for it
//     (mirror.Service.OpenCodeForm); a submit answers the whole form
//     (AnswerOpenCodeForm) — options by their index, never a label;
//   - a PERMISSION REQUEST: Allow once or Reject (never "always"), relayed as
//     the Attention item relays it (AnswerOpenCodeAsk).
//
// The card's `tool` is the ask's id. 🔴 A submit is refused unless the window
// still waits on EXACTLY that id now (and the hub and the host check again),
// so an answer can never reach another ask or another session than the one
// the card showed. The gates are a Claude answer's: the owner (mirrorEntry),
// same origin, -interactive, a fresh sign-in.

// openCodeFormer reads and answers a pending opencode question's whole form;
// *mirror.Service implements it.
type openCodeFormer interface {
	OpenCodeForm(ctx context.Context, host, session, window, promptID string) (mirror.OpenCodeForm, error)
	AnswerOpenCodeForm(ctx context.Context, host, session, window string, form mirror.OpenCodeForm, picks []mirror.OpenCodePick) error
}

// The opencode card's words.
const (
	openCodeAgentName = "opencode"
	// openCodePermissionHeader heads a permission request's card.
	openCodePermissionHeader = "Permission"
	// openCodeCardNoForms: the host cannot send the form (an older or
	// read-only muster-mirror).
	openCodeCardNoForms = "This host's muster-mirror can't show this opencode question here — update it, or answer in the terminal."
	// openCodeCardGone: the window no longer waits on that ask.
	openCodeCardGone = "opencode is no longer waiting on that (it was answered in the terminal, or the turn ended). Nothing was answered here."
)

// openCodePermissionOptions are a permission card's options, in order, and
// the answer each relays.
var openCodePermissionOptions = []struct{ label, choice string }{{"Allow once", "once"}, {"Reject", "reject"}}

// windowOfEntry is e's window in h's listing.
func windowOfEntry(h mirror.HostView, e mirror.SessionView) (mirror.Window, bool) {
	for _, w := range h.Windows {
		if w.Session == e.Session && w.ID == e.Window {
			return w, true
		}
	}
	return mirror.Window{}, false
}

// openCodePending is what e's opencode window waits on that muster can name:
// its Pending when it is waiting on a permission request or a question with an
// id; false otherwise.
func openCodePending(h mirror.HostView, e mirror.SessionView) (*mirror.Pending, bool) {
	if rt, ok := e.Spec(); !ok || !rt.PendingNamesAsk || e.Window == "" {
		return nil, false
	}
	w, ok := windowOfEntry(h, e)
	if !ok || w.Status != mirror.StatusWaiting || w.Pending == nil || w.Pending.PromptID == "" ||
		(w.Pending.Kind != mirror.PendingPermission && w.Pending.Kind != mirror.PendingQuestion) {
		return nil, false
	}
	return w.Pending, true
}

// permissionQuestion is a permission request's card question.
func permissionQuestion(p *mirror.Pending) askq.Question {
	q := askq.Question{Header: openCodePermissionHeader, Question: p.Summary}
	if p.Detail != "" {
		q.Question = p.Summary + " — " + p.Detail
	}
	for _, o := range openCodePermissionOptions {
		q.Options = append(q.Options, askq.Option{Label: o.label})
	}
	return q
}

// formQuestions is form's questions as the card draws them.
func formQuestions(f mirror.OpenCodeForm) []askq.Question {
	out := make([]askq.Question, 0, len(f.Questions))
	for _, fq := range f.Questions {
		q := askq.Question{Header: fq.Header, Question: fq.Question, MultiSelect: fq.Multiple}
		for _, o := range fq.Options {
			q.Options = append(q.Options, askq.Option{Label: o.Label, Description: o.Description})
		}
		out = append(out, q)
	}
	return out
}

// openCodeCard is the pending card of the ask p of e's window, h being its
// host, and the form it answers (zero for a permission request, or when the
// host could not send one: the card then says why it cannot be answered).
func (s *Server) openCodeCard(ctx context.Context, h mirror.HostView, e mirror.SessionView, p *mirror.Pending) (ui.MirrorAskLiveView, mirror.OpenCodeForm) {
	v := ui.MirrorAskLiveView{Ref: mirrorRef(e), ToolID: p.PromptID, State: ui.AskStatePending, WindowName: e.DisplayTitle(), Agent: openCodeAgentName}
	var form mirror.OpenCodeForm
	if p.Kind == mirror.PendingPermission {
		v.Questions, v.NoOther = []askq.Question{permissionQuestion(p)}, true
	} else {
		f, err := s.openCodeFormOf(ctx, h, e, p.PromptID)
		if err != nil {
			v.Questions = []askq.Question{{Header: p.Tool, Question: p.Summary}}
			v.Unanswerable = openCodeCardNoForms
			if errors.Is(err, mirror.ErrOpenCodeAskGone) {
				v.Unanswerable = openCodeCardGone
			}
		} else {
			form, v.Questions = f, formQuestions(f)
			// A question that takes no answer of one's own has no "Other".
			for _, q := range f.Questions {
				if !q.Custom {
					v.NoOther = true
				}
			}
		}
	}
	switch {
	case !h.Interactive:
		v.Unanswerable = ui.MirrorReadOnlyNote
	case !h.Online:
		v.Unanswerable = "This host is offline — answer it in the terminal."
	case !h.OpenCodeAnswer:
		v.Unanswerable = unprefixed(mirror.ErrOpenCodeAnswerUnsupported)
	}
	return v, form
}

// openCodeFormOf is the form of question promptID of e's window, read from its
// host.
func (s *Server) openCodeFormOf(ctx context.Context, h mirror.HostView, e mirror.SessionView, promptID string) (mirror.OpenCodeForm, error) {
	f, can := s.ext.Mirror.(openCodeFormer)
	if !can || !h.OpenCodeForms {
		return mirror.OpenCodeForm{}, mirror.ErrOpenCodeFormsUnsupported
	}
	return f.OpenCodeForm(ctx, h.Host, e.Session, e.Window, promptID)
}

// handleOpenCodeAskAnswer is a card's submit for e's opencode window (the
// caller has checked the owner, same origin, -interactive and a fresh
// sign-in): the window's ask, answered through its host.
func (s *Server) handleOpenCodeAskAnswer(w http.ResponseWriter, r *http.Request, h mirror.HostView, e mirror.SessionView) {
	tool := r.PostFormValue("tool")
	p, waiting := openCodePending(h, e)
	if !waiting || tool == "" || p.PromptID != tool {
		// 🔴 Not the ask the window waits on now: nothing is sent.
		v := ui.MirrorAskLiveView{Ref: mirrorRef(e), ToolID: tool, State: ui.AskStateExpired, Note: openCodeCardGone, Agent: openCodeAgentName}
		s.writeAskLive(w, http.StatusConflict, v)
		return
	}
	v, form := s.openCodeCard(r.Context(), h, e, p)
	if v.Unanswerable != "" {
		s.writeAskLive(w, http.StatusConflict, v)
		return
	}
	choices, answers, err := askq.FromForm(v.Questions, r.PostForm)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// The page is not the wait's owner: the hub bounds it.
	ctx := context.WithoutCancel(r.Context())
	if p.Kind == mirror.PendingPermission {
		c := choices[0]
		if c.Other != "" || len(c.Picks) != 1 {
			http.Error(w, "choose Allow once or Reject", http.StatusBadRequest)
			return
		}
		a, can := s.ext.Mirror.(openCodeAnswerer)
		if !can {
			http.Error(w, mirror.ErrOpenCodeAnswerUnsupported.Error(), http.StatusConflict)
			return
		}
		err = a.AnswerOpenCodeAsk(ctx, h.Host, e.Session, e.Window, p.PromptID, openCodePermissionOptions[c.Picks[0]].choice)
	} else {
		picks := make([]mirror.OpenCodePick, 0, len(choices))
		for i, c := range choices {
			picks = append(picks, mirror.OpenCodePick{Key: form.Questions[i].Key, Picks: c.Picks, Other: c.Other})
		}
		f, can := s.ext.Mirror.(openCodeFormer)
		if !can {
			http.Error(w, mirror.ErrOpenCodeFormsUnsupported.Error(), http.StatusConflict)
			return
		}
		err = f.AnswerOpenCodeForm(ctx, h.Host, e.Session, e.Window, form, picks)
	}
	if err != nil {
		s.logger.Printf("mirror: answering the opencode ask in %s/%s%s: %v", h.Host, e.Session, e.Window, err)
		why := err.Error()
		switch {
		case errors.Is(err, mirror.ErrStepUpRequired):
			refuseStale(w)
			return
		case errors.Is(err, mirror.ErrOpenCodeAskGone):
			why = openCodeGoneText
		case errors.Is(err, mirror.ErrOpenCodeUnreachable):
			why = openCodeUnreachText
		case errors.Is(err, mirror.ErrOpenCodeAnswerUnknown):
			why = openCodeUnknownText
		case errors.Is(err, mirror.ErrOpenCodeAnswerRefused):
			why = openCodeRefusedText
		}
		v.Error = why
		s.writeAskLive(w, http.StatusConflict, v)
		return
	}
	s.logger.Printf("mirror: answered the opencode ask in %s/%s%s", h.Host, e.Session, e.Window)
	v.State, v.Answers = ui.AskStateAnswered, answers
	s.writeAskLive(w, http.StatusOK, v)
}
