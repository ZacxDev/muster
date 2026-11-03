package mirror

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ZacxDev/muster/internal/stepup"
)

// ANSWERING AN OPENCODE ASK FROM MUSTER (docs/design/opencode.md, "Asks"):
// the server half. A permission request or a question-tool form an opencode
// window waits on (its Pending: PendingPermission, PendingQuestion) is
// answered by the HOST through the opencode server's API (internal/
// opencodehost's ReplyPermission and ReplyForm) — never by keys into the pane,
// which the paste guard refuses. This side carries the request and the
// outcome.
//
// 🔴 ONLY THE ASK MUSTER SHOWED, WITH AN ANSWER IT OFFERED. The hub re-reads
// the window's listing at the moment of sending: it must still be an opencode
// window waiting on an ask with exactly this PromptID, offering this answer
// (Pending.Offers) — "once" or "reject" for a permission, never "always"; one
// of the question's own option labels. The host checks again against what the
// opencode server holds NOW.
//
// 🔴 THE GATES COPY THE HERMES APPROVAL PATH (hermes_answer.go), which copies
// the Claude ask-answer path: an -interactive v2 host that advertised the
// capability (Msg.OpenCodeAnswer: -interactive, as for Claude Code), and a fresh sign-in
// (internal/stepup); the owner and same-origin gates are the caller's
// (internal/api's handleAttentionOpenCodeAnswer).

// OpenCodeReply is an opencode ask's answer on the wire (Msg.OpenCodeReply).
type OpenCodeReply struct {
	// PromptID is the request's or form's id muster showed (Pending.PromptID).
	PromptID string `json:"promptId"`
	// Choice is the answer: one of OpenCodePermissionChoices for a permission
	// request, one of the question's option labels for a form.
	Choice string `json:"choice"`
}

// The refusals a host names on answered for an opencode ask (Msg.Refusal).
const (
	// AskRefusalOpenCodeGone: the ask is no longer pending (answered in the
	// terminal, the turn ended). Nothing was answered.
	AskRefusalOpenCodeGone = "opencode-gone"
	// AskRefusalOpenCodeUnreachable: the host could not reach the opencode
	// server, or it refused the host's token. Nothing was answered.
	AskRefusalOpenCodeUnreachable = "opencode-unreachable"
	// AskRefusalOpenCodeUnknown: the answer was sent and no outcome came back.
	// It may have been answered.
	AskRefusalOpenCodeUnknown = "opencode-unknown"
	// AskRefusalOpenCodeRefused: opencode refused the answer itself (HTTP 400:
	// the field cannot take that value). Nothing was answered; it is still
	// pending.
	AskRefusalOpenCodeRefused = "opencode-refused"
)

var (
	// ErrOpenCodeAnswerUnsupported: the host does not relay answers to opencode
	// (an older or read-only muster-mirror, or one with no opencode token).
	ErrOpenCodeAnswerUnsupported = errors.New("mirror: this host's muster-mirror does not relay answers to opencode (it needs -interactive); answer it in the terminal")
	// ErrOpenCodeAskGone: the window is no longer waiting on that ask, or not
	// offering that answer. Nothing was answered.
	ErrOpenCodeAskGone = errors.New("mirror: opencode is no longer waiting on that")
	// ErrOpenCodeUnreachable: the host could not reach the opencode server.
	// Nothing was answered.
	ErrOpenCodeUnreachable = errors.New("mirror: the host could not reach its opencode server; answer it in the terminal")
	// ErrOpenCodeAnswerUnknown: the answer was sent and no outcome came back in
	// time. It may have been answered: check the terminal.
	ErrOpenCodeAnswerUnknown = errors.New("mirror: muster sent the answer but did not hear back; it may have been answered — check the terminal")
	// ErrOpenCodeAnswerRefused: opencode refused the answer (the field cannot
	// take that value). Nothing was answered; the ask is still pending.
	ErrOpenCodeAnswerRefused = errors.New("mirror: opencode refused that answer; nothing was answered")
)

// openCodeAnswerTimeout bounds one answer end to end: the host makes one POST
// to a loopback server.
const openCodeAnswerTimeout = 20 * time.Second

// AnswerOpenCodeAsk answers the opencode ask promptID in one WINDOW of owner's
// host's session with choice, through the host, and waits for the host's
// outcome. nil means opencode took the answer.
func (h *Hub) AnswerOpenCodeAsk(ctx context.Context, owner int64, host, session, window, promptID, choice string) (err error) {
	ctx, done := hubOp(ctx, "opencode_answer")
	defer func() { done(err) }()
	h.mu.Lock()
	hc := h.hosts[hostKey{owner, host}]
	h.mu.Unlock()
	if hc == nil {
		return fmt.Errorf("%w: %w (host %q)", ErrAskNotSent, ErrHostOffline, host)
	}
	windowID, runtime, err := hc.targetRuntime(session, window)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrAskNotSent, err)
	}
	if runtime != RuntimeOpenCode {
		return fmt.Errorf("%w: %w (the window does not run opencode)", ErrAskNotSent, ErrOpenCodeAskGone)
	}
	if !hc.gatedAnswers[RuntimeOpenCode] || !hc.interactive || hc.version < 2 {
		return fmt.Errorf("%w: %w (host %q)", ErrAskNotSent, ErrOpenCodeAnswerUnsupported, host)
	}
	if !stepup.Fresh(ctx, h.now()) {
		return fmt.Errorf("%w: %w", ErrAskNotSent, ErrStepUpRequired)
	}
	if p := hc.pendingOf(session, windowID); p == nil || (p.Kind != PendingPermission && p.Kind != PendingQuestion) || p.PromptID != promptID || !p.Offers(choice) {
		return fmt.Errorf("%w: %w", ErrAskNotSent, ErrOpenCodeAskGone)
	}
	hc.mu.Lock()
	hc.next++
	id := hc.next
	ch := make(chan error, 1)
	hc.answers[id] = ch
	hc.mu.Unlock()
	defer func() {
		hc.mu.Lock()
		delete(hc.answers, id)
		hc.mu.Unlock()
	}()
	m := Msg{Type: TypeAnswer, ID: id, Session: session, Window: windowID, Runtime: RuntimeOpenCode,
		OpenCodeReply: &OpenCodeReply{PromptID: promptID, Choice: choice}}
	if err := writeJSON(ctx, hc.ws, m); err != nil {
		return fmt.Errorf("%w: %w", ErrOpenCodeAnswerUnknown, err)
	}
	t := time.NewTimer(openCodeAnswerTimeout)
	defer t.Stop()
	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		return fmt.Errorf("%w: %w", ErrOpenCodeAnswerUnknown, ctx.Err())
	case <-t.C:
		return ErrOpenCodeAnswerUnknown
	}
}

// AnswerOpenCodeAsk is Hub.AnswerOpenCodeAsk for the viewing principal's host.
func (s *Service) AnswerOpenCodeAsk(ctx context.Context, host, session, window, promptID, choice string) error {
	o, err := owner(ctx)
	if err != nil {
		return err
	}
	return s.hub.AnswerOpenCodeAsk(ctx, o, host, session, window, promptID, choice)
}
