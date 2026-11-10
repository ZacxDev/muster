package mirror

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/stepup"
)

// AN OPENCODE QUESTION, WHOLE (docs/design/opencode.md, "Asks"; the view's
// card): what opencode's question tool asks is a FORM of one or more
// questions, each a single- or a multi-select, each offering options and an
// answer of the person's own (custom). The window list carries only its
// first question (Pending, bounded for the frame), so a card that answers
// the whole form ASKS the host for it (TypeOpenCodeForm → TypeOpenCodeFormed:
// OpenCodeForm), and answers it with one pick list per question
// (OpenCodeReply.Picks) — options by their INDEX in the form the host sent,
// never by a label muster may have cleaned, and an answer of the person's own
// only where the question takes one.
//
// 🔴 BACKWARD-COMPATIBLE BOTH WAYS: a host advertises it (Msg.OpenCodeForms);
// muster asks only a host that did, and an older muster never asks — its
// Attention item still says to answer such a question in the terminal.
//
// 🔴 ONLY THE ASK MUSTER SHOWED, IN THE WINDOW MUSTER SHOWED: the hub checks
// the window's listing still waits on exactly this PromptID before it asks
// for the form or sends an answer, and the host checks again against what the
// opencode server holds NOW — the ask must be pending in a session the
// window's pane shows (its own, or a subagent's under it), and every pick
// must index one of that very question's options.

const (
	// TypeOpenCodeForm (muster → host): send ask OpenCodeReply.PromptID's
	// form, of the opencode window (Session, Window).
	TypeOpenCodeForm = "opencode-form"
	// TypeOpenCodeFormed (host → muster): the answer to form request ID —
	// Form, or Error.
	TypeOpenCodeFormed = "opencode-formed"
)

// OpenCodeForm is an opencode ask's question form, as the card draws it.
type OpenCodeForm struct {
	PromptID  string             `json:"promptId"`
	Title     string             `json:"title,omitempty"`
	Questions []OpenCodeQuestion `json:"questions"`
}

// OpenCodeQuestion is one question of a form: Key is the field's key
// ("q0"), Header its short title, Question its text; Multiple a
// multi-select; Custom that it takes an answer of the person's own.
type OpenCodeQuestion struct {
	Key      string           `json:"key"`
	Header   string           `json:"header,omitempty"`
	Question string           `json:"question"`
	Multiple bool             `json:"multiple,omitempty"`
	Custom   bool             `json:"custom,omitempty"`
	Options  []OpenCodeOption `json:"options"`
}

// OpenCodeOption is one option of a question.
type OpenCodeOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// OpenCodePick is the answer to one question of a form (OpenCodeReply.Picks):
// the indexes of the chosen options, and the person's own answer ("" none).
type OpenCodePick struct {
	Key   string `json:"key"`
	Picks []int  `json:"picks,omitempty"`
	Other string `json:"other,omitempty"`
}

// The OpenCodeForm bounds: a form past them is not answered from muster
// (NormalizeOpenCodeForm refuses it whole, so an index the card shows is
// always the host's).
const (
	MaxFormQuestions   = 8
	MaxFormOptions     = 10
	MaxFormKey         = 32
	MaxFormHeader      = 80
	MaxFormQuestion    = 600
	MaxFormLabel       = 200
	MaxFormDescription = 400
	// MaxFormOther bounds an answer of the person's own, in CHARACTERS
	// (runes): the card's box holds askq.MaxFreeText of them (its maxlength,
	// which counts UTF-16 units — never fewer than runes), so whatever the box
	// lets a person type, in any script, is taken here too.
	MaxFormOther = askq.MaxFreeText
)

// ErrOpenCodeFormInvalid wraps every refusal of a form or of its picks.
var ErrOpenCodeFormInvalid = errors.New("mirror: not an answer this question takes")

// NormalizeOpenCodeForm is f as the wire carries it — its text cleaned to one
// line each, cut to its bound — and false when it is not one muster answers:
// no prompt id, no question, more questions or options than the bounds, a
// question with no options it does not let the person answer themselves, a
// key that is empty, too long or repeated. Applied by BOTH halves.
func NormalizeOpenCodeForm(f OpenCodeForm) (OpenCodeForm, bool) {
	if !ValidOpenCodeAskID(f.PromptID) || len(f.Questions) == 0 || len(f.Questions) > MaxFormQuestions {
		return OpenCodeForm{}, false
	}
	out := OpenCodeForm{PromptID: f.PromptID, Title: cleanLine(f.Title, MaxFormHeader)}
	keys := map[string]bool{}
	for _, q := range f.Questions {
		if q.Key == "" || len(q.Key) > MaxFormKey || keys[q.Key] || cleanLine(q.Key, MaxFormKey) != q.Key ||
			len(q.Options) > MaxFormOptions || (len(q.Options) == 0 && !q.Custom) {
			return OpenCodeForm{}, false
		}
		keys[q.Key] = true
		labels := map[string]bool{}
		nq := OpenCodeQuestion{Key: q.Key, Header: cleanLine(q.Header, MaxFormHeader), Question: cleanLine(q.Question, MaxFormQuestion),
			Multiple: q.Multiple, Custom: q.Custom}
		if nq.Question == "" {
			nq.Question = nq.Header
		}
		for _, o := range q.Options {
			l := cleanLine(o.Label, MaxFormLabel)
			if l == "" || labels[l] {
				// Two options a person cannot tell apart are not offered.
				return OpenCodeForm{}, false
			}
			labels[l] = true
			nq.Options = append(nq.Options, OpenCodeOption{Label: l, Description: cleanLine(o.Description, MaxFormDescription)})
		}
		out.Questions = append(out.Questions, nq)
	}
	return out, true
}

// CheckOpenCodePicks is why picks cannot answer form f, nil when they can:
// exactly one pick list per question, in the form's order, by its key; each
// index one of that question's options, none twice; an answer of one's own
// only on a question that takes one, within MaxFormOther characters; a
// single-select answered exactly once (an option or one's own), a
// multi-select at least once. Applied by BOTH halves.
func CheckOpenCodePicks(f OpenCodeForm, picks []OpenCodePick) error {
	if len(picks) != len(f.Questions) {
		return fmt.Errorf("%w: %d answers for %d questions", ErrOpenCodeFormInvalid, len(picks), len(f.Questions))
	}
	for i, q := range f.Questions {
		p := picks[i]
		if p.Key != q.Key {
			return fmt.Errorf("%w: answer %d is for %q, not question %q", ErrOpenCodeFormInvalid, i+1, p.Key, q.Key)
		}
		seen := map[int]bool{}
		for _, j := range p.Picks {
			if j < 0 || j >= len(q.Options) || seen[j] {
				return fmt.Errorf("%w: question %d: option %d is not one it offers, or is chosen twice", ErrOpenCodeFormInvalid, i+1, j+1)
			}
			seen[j] = true
		}
		n := len(p.Picks)
		if p.Other != "" {
			switch {
			case !q.Custom:
				return fmt.Errorf("%w: question %d takes only its own options", ErrOpenCodeFormInvalid, i+1)
			case !utf8.ValidString(p.Other) || utf8.RuneCountInString(p.Other) > MaxFormOther || strings.TrimSpace(p.Other) == "" || strings.ContainsFunc(p.Other, isControl):
				return fmt.Errorf("%w: question %d: an answer of your own is at most %d characters of text", ErrOpenCodeFormInvalid, i+1, MaxFormOther)
			}
			n++
		}
		switch {
		case n == 0:
			return fmt.Errorf("%w: question %d is not answered", ErrOpenCodeFormInvalid, i+1)
		case !q.Multiple && n > 1:
			return fmt.Errorf("%w: question %d takes one answer", ErrOpenCodeFormInvalid, i+1)
		}
	}
	return nil
}

// isControl reports whether r is a control character other than a newline
// or a tab.
func isControl(r rune) bool {
	return (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}

// openCodeFormTimeout bounds a form request: the host answers from what it
// holds.
const openCodeFormTimeout = 5 * time.Second

// formAnswer is a host's opencode-formed message, as the waiting request gets
// it.
type formAnswer struct {
	f   OpenCodeForm
	err error
}

var (
	// ErrOpenCodeFormsUnsupported: the host does not send opencode forms (an
	// older or read-only muster-mirror, or one with no opencode token).
	ErrOpenCodeFormsUnsupported = errors.New("mirror: this host's muster-mirror cannot show this opencode question here; answer it in the terminal")
	// ErrOpenCodeFormTimeout: the host did not send the form in time.
	ErrOpenCodeFormTimeout = errors.New("mirror: the host did not send the opencode question in time")
)

// openCodeAskWindow is owner's host and the opencode window (session,
// window) on it — window "" the session's active one — when the host speaks
// opencode forms and lists that window waiting on a QUESTION whose id is
// promptID; else why not (ErrAskNotSent wrapping the reason).
func (h *Hub) openCodeAskWindow(owner int64, host, session, window, promptID string) (*hostConn, string, error) {
	h.mu.Lock()
	hc := h.hosts[hostKey{owner, host}]
	h.mu.Unlock()
	if hc == nil {
		return nil, "", fmt.Errorf("%w: %w (host %q)", ErrAskNotSent, ErrHostOffline, host)
	}
	windowID, runtime, err := hc.targetRuntime(session, window)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %w", ErrAskNotSent, err)
	}
	if runtime != RuntimeOpenCode {
		return nil, "", fmt.Errorf("%w: %w (the window does not run opencode)", ErrAskNotSent, ErrOpenCodeAskGone)
	}
	if !hc.openCodeForms || hc.version < 2 {
		return nil, "", fmt.Errorf("%w: %w (host %q)", ErrAskNotSent, ErrOpenCodeFormsUnsupported, host)
	}
	if p := hc.pendingOf(session, windowID); p == nil || p.Kind != PendingQuestion || p.PromptID == "" || p.PromptID != promptID {
		return nil, "", fmt.Errorf("%w: %w", ErrAskNotSent, ErrOpenCodeAskGone)
	}
	return hc, windowID, nil
}

// OpenCodeForm asks owner's host for the whole form of question promptID that
// the opencode window (session, window) waits on, and waits for it.
func (h *Hub) OpenCodeForm(ctx context.Context, owner int64, host, session, window, promptID string) (_ OpenCodeForm, err error) {
	ctx, done := hubOp(ctx, "opencode_form")
	defer func() { done(err) }()
	hc, windowID, err := h.openCodeAskWindow(owner, host, session, window, promptID)
	if err != nil {
		return OpenCodeForm{}, err
	}
	hc.mu.Lock()
	hc.next++
	id := hc.next
	ch := make(chan formAnswer, 1)
	hc.forms[id] = ch
	hc.mu.Unlock()
	defer func() {
		hc.mu.Lock()
		delete(hc.forms, id)
		hc.mu.Unlock()
	}()
	m := Msg{Type: TypeOpenCodeForm, ID: id, Session: session, Window: windowID, Runtime: RuntimeOpenCode,
		OpenCodeReply: &OpenCodeReply{PromptID: promptID}}
	if err := writeJSON(ctx, hc.ws, m); err != nil {
		return OpenCodeForm{}, fmt.Errorf("mirror: ask the host for the opencode question: %w", err)
	}
	t := time.NewTimer(openCodeFormTimeout)
	defer t.Stop()
	select {
	case a := <-ch:
		if a.err == nil && a.f.PromptID != promptID {
			// 🔴 Another ask's form is never drawn as this one.
			return OpenCodeForm{}, fmt.Errorf("%w: %w", ErrAskNotSent, ErrOpenCodeAskGone)
		}
		return a.f, a.err
	case <-ctx.Done():
		return OpenCodeForm{}, ctx.Err()
	case <-t.C:
		return OpenCodeForm{}, ErrOpenCodeFormTimeout
	}
}

// openCodeFormed hands the host's answer to the request waiting on m.ID — the
// form normalised again here, whatever the host did.
func (hc *hostConn) openCodeFormed(m Msg) {
	hc.mu.Lock()
	ch := hc.forms[m.ID]
	delete(hc.forms, m.ID)
	hc.mu.Unlock()
	if ch == nil {
		return
	}
	if m.Error != "" || m.Form == nil {
		why := CleanText(m.Error, 200)
		if why == "" {
			why = "no form"
		}
		ch <- formAnswer{err: fmt.Errorf("%w: %w (%s)", ErrAskNotSent, ErrOpenCodeAskGone, why)}
		return
	}
	f, ok := NormalizeOpenCodeForm(*m.Form)
	if !ok {
		ch <- formAnswer{err: fmt.Errorf("%w: %w", ErrAskNotSent, ErrOpenCodeFormsUnsupported)}
		return
	}
	ch <- formAnswer{f: f}
}

// AnswerOpenCodeForm answers question promptID that the opencode window
// (session, window) waits on with picks (one per question of form, the form
// the host sent for it), through the host, and waits for its outcome. nil
// means opencode took the answer.
func (h *Hub) AnswerOpenCodeForm(ctx context.Context, owner int64, host, session, window string, form OpenCodeForm, picks []OpenCodePick) (err error) {
	ctx, done := hubOp(ctx, "opencode_answer")
	defer func() { done(err) }()
	if err := CheckOpenCodePicks(form, picks); err != nil {
		return fmt.Errorf("%w: %w", ErrAskNotSent, err)
	}
	hc, windowID, err := h.openCodeAskWindow(owner, host, session, window, form.PromptID)
	if err != nil {
		return err
	}
	if !hc.runtimeAnswers[RuntimeOpenCode] || !hc.interactive {
		return fmt.Errorf("%w: %w (host %q)", ErrAskNotSent, ErrOpenCodeAnswerUnsupported, host)
	}
	if !stepup.Fresh(ctx, h.now()) {
		return fmt.Errorf("%w: %w", ErrAskNotSent, ErrStepUpRequired)
	}
	return h.relayOpenCodeAnswer(ctx, hc, session, windowID, OpenCodeReply{PromptID: form.PromptID, Picks: picks})
}

// OpenCodeForm is Hub.OpenCodeForm for the viewing principal's host. No
// step-up: it reads, and answers nothing.
func (s *Service) OpenCodeForm(ctx context.Context, host, session, window, promptID string) (OpenCodeForm, error) {
	o, err := owner(ctx)
	if err != nil {
		return OpenCodeForm{}, err
	}
	return s.hub.OpenCodeForm(ctx, o, host, session, window, promptID)
}

// AnswerOpenCodeForm is Hub.AnswerOpenCodeForm for the viewing principal's
// host.
func (s *Service) AnswerOpenCodeForm(ctx context.Context, host, session, window string, form OpenCodeForm, picks []OpenCodePick) error {
	o, err := owner(ctx)
	if err != nil {
		return err
	}
	return s.hub.AnswerOpenCodeForm(ctx, o, host, session, window, form, picks)
}
