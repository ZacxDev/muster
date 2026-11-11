package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

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

// permissionForm is a permission request's card as a form: one question,
// Allow once or Reject, no answer of one's own.
func permissionForm(p *mirror.Pending) mirror.OpenCodeForm {
	q := mirror.OpenCodeQuestion{Key: "decision", Header: openCodePermissionHeader, Question: p.Summary}
	for _, o := range openCodePermissionOptions {
		q.Options = append(q.Options, mirror.OpenCodeOption{Label: o.label})
	}
	return mirror.OpenCodeForm{PromptID: p.PromptID, Questions: []mirror.OpenCodeQuestion{q}}
}

// formPicks is a card's submit as the picks that answer form — for question
// i, the values a<i> are the chosen options' INDEXES (the card's ByIndex), or
// askq.OtherValue whose text is o<i> — checked against the form
// (mirror.CheckOpenCodePicks: one answer each, in range, one's own only where
// the question takes one), and the answers as the card shows them, keyed by
// question text. An API answer goes to opencode as given: newlines included.
func formPicks(f mirror.OpenCodeForm, v url.Values) ([]mirror.OpenCodePick, map[string]string, error) {
	picks := make([]mirror.OpenCodePick, 0, len(f.Questions))
	answers := make(map[string]string, len(f.Questions))
	for i, q := range f.Questions {
		idx := strconv.Itoa(i)
		p := mirror.OpenCodePick{Key: q.Key}
		var shown []string
		other := false
		for _, val := range v["a"+idx] {
			if val == askq.OtherValue {
				other = true
				continue
			}
			j, err := strconv.Atoi(val)
			if err != nil || strconv.Itoa(j) != val || j < 0 || j >= len(q.Options) {
				return nil, nil, fmt.Errorf("question %d: %q is not one of its options", i+1, val)
			}
			p.Picks = append(p.Picks, j)
			shown = append(shown, q.Options[j].Label)
		}
		if other {
			p.Other = strings.TrimSpace(v.Get("o" + idx))
			if p.Other == "" {
				return nil, nil, fmt.Errorf("question %d: type your own answer, or choose an option", i+1)
			}
			shown = append(shown, p.Other)
		}
		picks = append(picks, p)
		answers[q.Question] = strings.Join(shown, askq.MultiJoin)
	}
	if err := mirror.CheckOpenCodePicks(f, picks); err != nil {
		return nil, nil, errors.New(strings.TrimPrefix(err.Error(), mirror.ErrOpenCodeFormInvalid.Error()+": "))
	}
	return picks, answers, nil
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
	// ByIndex: every option is answered by its place in the form, never by a
	// label (two options can read alike; the host maps an index to a value).
	v := ui.MirrorAskLiveView{Ref: mirrorRef(e), ToolID: p.PromptID, State: ui.AskStatePending, WindowName: e.DisplayTitle(), Agent: openCodeAgentName,
		ByIndex: true}
	var form mirror.OpenCodeForm
	if p.Kind == mirror.PendingPermission {
		v.Questions, v.NoOther = []askq.Question{permissionQuestion(p)}, true
		form = permissionForm(p)
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
			// A question that takes no answer of one's own has no "Other" —
			// that question only: the others keep theirs.
			for _, q := range f.Questions {
				v.NoOtherFor = append(v.NoOtherFor, !q.Custom)
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
// sign-in): the window's ask, answered through its host. att is the caller's
// attempt (ask_attempts.go): every return here names its reason.
func (s *Server) handleOpenCodeAskAnswer(w http.ResponseWriter, r *http.Request, h mirror.HostView, e mirror.SessionView, att *attempt) {
	tool := r.PostFormValue("tool")
	p, waiting := openCodePending(h, e)
	if !waiting || tool == "" || p.PromptID != tool {
		// 🔴 Not the ask the window waits on now: nothing is sent.
		att.end("not-waiting")
		v := ui.MirrorAskLiveView{Ref: mirrorRef(e), ToolID: tool, State: ui.AskStateExpired, Note: openCodeCardGone, Agent: openCodeAgentName}
		s.writeAskLive(w, http.StatusConflict, v)
		return
	}
	v, form := s.openCodeCard(r.Context(), h, e, p)
	att.shape(v.Questions, nil)
	if v.Unanswerable != "" {
		// The form read is refused as gone once opencode has the answer: a
		// second submit of an ask answered here gets the answered card.
		if a, ok := s.openCodeAnsweredCard(r, h, e, tool); ok {
			att.end("duplicate")
			s.writeAskLive(w, http.StatusOK, a)
			return
		}
		att.end("unanswerable")
		s.writeAskLive(w, http.StatusConflict, v)
		return
	}
	picks, answers, err := formPicks(form, r.PostForm)
	if err != nil {
		att.end("invalid")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if a, ok := s.openCodeAnsweredCard(r, h, e, tool); ok {
		// Answered here already (the listing still names it): the answered
		// card, never relayed twice.
		att.end("duplicate")
		s.writeAskLive(w, http.StatusOK, a)
		return
	}
	for _, pk := range picks {
		if pk.Other != "" {
			att.a.Shape.Other = true
		}
	}
	// The page is not the wait's owner: the hub bounds it.
	ctx := context.WithoutCancel(r.Context())
	if p.Kind == mirror.PendingPermission {
		a, can := s.ext.Mirror.(openCodeAnswerer)
		if !can {
			att.end("unsupported")
			http.Error(w, mirror.ErrOpenCodeAnswerUnsupported.Error(), http.StatusConflict)
			return
		}
		err = a.AnswerOpenCodeAsk(ctx, h.Host, e.Session, e.Window, p.PromptID, openCodePermissionOptions[picks[0].Picks[0]].choice)
	} else {
		f, can := s.ext.Mirror.(openCodeFormer)
		if !can {
			att.end("unsupported")
			http.Error(w, mirror.ErrOpenCodeFormsUnsupported.Error(), http.StatusConflict)
			return
		}
		err = f.AnswerOpenCodeForm(ctx, h.Host, e.Session, e.Window, form, picks)
	}
	att.end(openCodeAnswerResult(err))
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
		v.Record = answersCardPath(tool)
		s.writeAskLive(w, http.StatusConflict, v)
		return
	}
	s.logger.Printf("mirror: answered the opencode ask in %s/%s%s", h.Host, e.Session, e.Window)
	s.ocAnswered.record(openCodeAnswerKey(r, h, e, tool), v.Questions, answers, time.Now())
	v.State, v.Answers = ui.AskStateAnswered, answers
	s.writeAskLive(w, http.StatusOK, v)
}

// 🔴 AN ASK ANSWERED HERE STAYS ANSWERED WHILE THE LISTING CATCHES UP. The
// window's listing — and so openCodePending — is the hub's copy of the host's
// LAST listing, which names the ask until the host's next one. A poll of the
// live region in that gap read the form again; opencode no longer holds it
// (it was answered), so the host refused the read as gone and the card was
// redrawn saying "opencode is no longer waiting on that … Nothing was answered
// here" — a successful answer shown as a failure (the operator's report). So
// a successful answer is recorded here (owner, host, session, window, ask id),
// and while the listing still names that ask, the region draws it answered
// with its answers — no form read — and a second submit of it relays nothing.
// An ask id is opencode's own and never reused, so the record cannot stand
// for another ask; it is kept mirrorAskShownFor, like a Claude card's end.
type openCodeAnswerKeyT struct {
	owner                           int64
	host, session, window, promptID string
}

type openCodeAnsweredAsk struct {
	questions []askq.Question
	answers   map[string]string
	at        time.Time
}

// openCodeAnswers is the asks lately answered from a card.
type openCodeAnswers struct {
	mu sync.Mutex
	m  map[openCodeAnswerKeyT]openCodeAnsweredAsk
}

// openCodeAnswerKey is ask promptID of e's window (h its host), for the
// request's owner.
func openCodeAnswerKey(r *http.Request, h mirror.HostView, e mirror.SessionView, promptID string) openCodeAnswerKeyT {
	return openCodeAnswerKeyT{owner: requestOwner(r.Context()), host: h.Host, session: e.Session, window: e.Window, promptID: promptID}
}

func (a *openCodeAnswers) record(k openCodeAnswerKeyT, qs []askq.Question, answers map[string]string, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.m == nil {
		a.m = map[openCodeAnswerKeyT]openCodeAnsweredAsk{}
	}
	for old, x := range a.m {
		if now.Sub(x.at) > mirrorAskShownFor {
			delete(a.m, old)
		}
	}
	a.m[k] = openCodeAnsweredAsk{questions: qs, answers: answers, at: now}
}

func (a *openCodeAnswers) find(k openCodeAnswerKeyT, now time.Time) (openCodeAnsweredAsk, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	x, ok := a.m[k]
	if !ok || now.Sub(x.at) > mirrorAskShownFor {
		return openCodeAnsweredAsk{}, false
	}
	return x, true
}

// openCodeAnsweredCard is ask promptID of e's window drawn answered here, when
// it was answered from a card lately (openCodeAnswers).
func (s *Server) openCodeAnsweredCard(r *http.Request, h mirror.HostView, e mirror.SessionView, promptID string) (ui.MirrorAskLiveView, bool) {
	x, ok := s.ocAnswered.find(openCodeAnswerKey(r, h, e, promptID), time.Now())
	if !ok {
		return ui.MirrorAskLiveView{}, false
	}
	return ui.MirrorAskLiveView{Ref: mirrorRef(e), ToolID: promptID, State: ui.AskStateAnswered, Questions: x.questions, Answers: x.answers,
		WindowName: e.DisplayTitle(), Agent: openCodeAgentName, ByIndex: true}, true
}

// openCodeLiveCard is the live region's card for the ask p of e's window: the
// answered card when it was answered here (checked again after the form read,
// which an answer landing meanwhile turns into "gone"), else openCodeCard's.
func (s *Server) openCodeLiveCard(r *http.Request, h mirror.HostView, e mirror.SessionView, p *mirror.Pending) ui.MirrorAskLiveView {
	if v, ok := s.openCodeAnsweredCard(r, h, e, p.PromptID); ok {
		return v
	}
	v, _ := s.openCodeCard(r.Context(), h, e, p)
	if a, ok := s.openCodeAnsweredCard(r, h, e, p.PromptID); ok {
		return a
	}
	return v
}
