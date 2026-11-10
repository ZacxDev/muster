package mirrorhost

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/opencodehost"
	"github.com/ZacxDev/muster/internal/redact"
)

// AN OPENCODE QUESTION, WHOLE — HOST HALF (internal/mirror's opencode_form.go):
// the host sends the form of the question a window waits on
// (TypeOpenCodeForm → TypeOpenCodeFormed), and answers it with the picks
// muster sends (OpenCodeReply.Picks) through the opencode server's API.
//
// 🔴 THE SAME ASK, THE SAME SESSION: both are refused unless this host relays
// answers (openCodeAnswer), the window is listed, its pane — re-resolved NOW
// — runs opencode WAITING on exactly that ask, and the ask is one this host
// holds from the opencode server. The answer goes to the session the ASK
// names (the server's own record of it, never one muster sent), each pick is
// an index into that question's own options, mapped to the option's VALUE,
// and an answer of the person's own goes only to a question that takes one.

// formOf is ask a as the wire carries its form; false for an ask that is not
// a question-tool form, or one past the form's bounds.
func formOf(a openCodeAsk) (mirror.OpenCodeForm, bool) {
	if a.Kind != opencodehost.AskQuestion {
		return mirror.OpenCodeForm{}, false
	}
	f := mirror.OpenCodeForm{PromptID: a.ID, Title: redact.Secrets(a.Title)}
	for _, fl := range a.Fields {
		q := mirror.OpenCodeQuestion{Key: fl.Key, Header: redact.Secrets(fl.Title), Question: redact.Secrets(fl.Description),
			Multiple: fl.Type == "multiselect", Custom: fl.Custom}
		if fl.Type != "string" && fl.Type != "multiselect" {
			return mirror.OpenCodeForm{}, false // a field muster cannot draw
		}
		for _, o := range fl.Options {
			q.Options = append(q.Options, mirror.OpenCodeOption{Label: redact.Secrets(o.Label)})
		}
		f.Questions = append(f.Questions, q)
	}
	return mirror.NormalizeOpenCodeForm(f)
}

// waitingOn is the opencode pane of window (session, window) re-resolved NOW
// when it waits on exactly ask promptID, else why not.
func (h *hostSide) waitingOn(ctx context.Context, m mirror.Msg, promptID string) error {
	if err := h.allowed(m); err != nil {
		return err
	}
	wi, err := h.window(ctx, m.Session, m.Window)
	if err != nil {
		return fmt.Errorf("%w: %v", errOpenCodeGone, err)
	}
	ag := wi.Agent
	if ag == nil || ag.Runtime != mirror.RuntimeOpenCode || ag.Gen == "" || ag.Status != mirror.StatusWaiting ||
		ag.Pending == nil || ag.Pending.PromptID == "" || ag.Pending.PromptID != promptID {
		return fmt.Errorf("%w: the window's pane is not opencode waiting on that now", errOpenCodeGone)
	}
	return nil
}

// openCodeForm answers a form request (TypeOpenCodeForm) with the form of the
// ask it names.
func (h *hostSide) openCodeForm(m mirror.Msg) {
	reply := func(f *mirror.OpenCodeForm, err error) {
		out := mirror.Msg{Type: mirror.TypeOpenCodeFormed, ID: m.ID, Form: f}
		if err != nil {
			out.Error, out.Form = err.Error(), nil
		}
		_ = h.send(out)
	}
	if !h.openCodeAnswer() {
		reply(nil, fmt.Errorf("%w: this host does not relay answers to opencode", errCannotAnswer))
		return
	}
	r := m.OpenCodeReply
	if r == nil || !mirror.ValidOpenCodeAskID(r.PromptID) {
		reply(nil, fmt.Errorf("%w: not an ask muster names", errOpenCodeGone))
		return
	}
	ctx, cancel := context.WithTimeout(h.ctx, 4*time.Second)
	defer cancel()
	if err := h.waitingOn(ctx, m, r.PromptID); err != nil {
		reply(nil, err)
		return
	}
	src, _ := h.openCodeSource()
	src.mu.Lock()
	a, ok := src.asks[r.PromptID]
	src.mu.Unlock()
	if !ok {
		reply(nil, fmt.Errorf("%w: it is no longer pending on this host", errOpenCodeGone))
		return
	}
	f, ok := formOf(a)
	if !ok {
		reply(nil, fmt.Errorf("%w: this question is answered in the terminal", errCannotAnswer))
		return
	}
	reply(&f, nil)
}

// answerForm relays picks — the whole answer to question form promptID —
// through the opencode server: each field's key to its chosen options'
// VALUES (one for a single-select, a list for a multi-select) or the
// person's own text.
func (s *OpenCodeSource) answerForm(ctx context.Context, promptID string, picks []mirror.OpenCodePick) error {
	s.mu.Lock()
	a, ok := s.asks[promptID]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: it is no longer pending on this host", errOpenCodeGone)
	}
	f, ok := formOf(a)
	if !ok {
		return fmt.Errorf("%w: this question is answered in the terminal", errOpenCodeGone)
	}
	// 🔴 Checked against the form THIS host holds, whatever muster checked.
	if err := mirror.CheckOpenCodePicks(f, picks); err != nil {
		return fmt.Errorf("%w (%v)", errOpenCodeAnswerRefused, err)
	}
	answer := map[string]any{}
	for i, fl := range a.Fields {
		p := picks[i]
		var values []string
		for _, j := range p.Picks {
			values = append(values, fl.Options[j].Value)
		}
		if p.Other != "" {
			values = append(values, p.Other)
		}
		if fl.Type == "multiselect" {
			answer[fl.Key] = values
		} else {
			answer[fl.Key] = values[0]
		}
	}
	cl, err := s.client()
	if err != nil {
		return fmt.Errorf("%w: %v", errOpenCodeUnreachable, err)
	}
	return s.settleAnswer(promptID, cl.ReplyForm(ctx, a.Session, a.ID, answer))
}

// settleAnswer is the outcome of a reply to ask promptID that answered err:
// on success the ask leaves this host's set at once (the replied event
// follows); else err as a refusal muster can name.
func (s *OpenCodeSource) settleAnswer(promptID string, err error) error {
	switch {
	case err == nil:
		s.mu.Lock()
		delete(s.asks, promptID)
		s.settled[promptID] = true
		s.notifyLocked()
		s.mu.Unlock()
		return nil
	case errors.Is(err, opencodehost.ErrAskSettled):
		return fmt.Errorf("%w (%v)", errOpenCodeGone, err)
	case errors.Is(err, opencodehost.ErrAnswerRefused):
		return fmt.Errorf("%w (%v)", errOpenCodeAnswerRefused, err)
	case errors.Is(err, opencodehost.ErrUnauthorized):
		return fmt.Errorf("%w (%v)", errOpenCodeUnreachable, err)
	case isDialError(err):
		return fmt.Errorf("%w (%v)", errOpenCodeUnreachable, err)
	}
	return fmt.Errorf("%w (%v)", errOpenCodeUnknown, err)
}
