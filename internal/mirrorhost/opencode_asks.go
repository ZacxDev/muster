package mirrorhost

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/opencodehost"
	"github.com/ZacxDev/muster/internal/redact"
)

// WHAT AN OPENCODE WINDOW WAITS ON (docs/design/opencode.md, "Asks"): the
// permission requests and question-tool forms of the session its pane shows
// — or of a subagent's child session under it — read from the opencode
// server's event stream (permission.asked / form.created raise one;
// permission.replied, form.replied and form.cancelled settle it, wherever it
// was answered) and, for a session first matched after the stream opened, its
// pending lists (GET /api/session/{id}/permission and /form).
//
// A window whose session has one is WAITING (opencode's own status says
// "busy" while a tool waits on the person), its Pending naming the newest ask
// (mirror.PendingPermission, mirror.PendingQuestion) — which is what makes
// muster's attention queue show it and notify, exactly as for a Claude Code
// window. Settled in the terminal, it leaves the window, and the queue item
// and its notification go with it.
//
// ⚠ A CHILD SESSION IS ATTRIBUTED TO ITS ROOT ONLY WHEN THE STREAM SAW IT
// CREATED (session.created's parentID): a subagent started while the stream
// was down is not, and its ask is the terminal's.

// openCodeAskAncestry bounds how many parents an ask's session is walked up.
const openCodeAskAncestry = 8

// openCodeAsk is one pending ask, and when this host first saw it (Unix ms).
type openCodeAsk struct {
	opencodehost.Ask
	since int64
}

// askEvent records what e says about asks and sessions' parents; under s.mu.
// It reports the session an ask event names ("" for none): its change count
// moves (Event.SessionID cannot see a form's — it is nested).
func (s *OpenCodeSource) askEvent(e opencodehost.Event) string {
	if child, parent := e.ParentOf(); child != "" && parent != "" {
		s.parent[child] = parent
	}
	raised, settled, session, ok := e.AskEvent()
	if !ok {
		return ""
	}
	if raised != nil {
		if _, had := s.asks[raised.ID]; !had && !s.settled[raised.ID] {
			s.asks[raised.ID] = openCodeAsk{Ask: *raised, since: s.now().UnixMilli()}
		}
	} else {
		delete(s.asks, settled)
		s.settled[settled] = true
	}
	s.notifyLocked()
	return session
}

// resetAsks forgets every ask on a (re)connect: the stream had a gap, so a
// session is seeded again from its pending lists when next matched. Under
// s.mu.
func (s *OpenCodeSource) resetAsks() {
	s.asks, s.settled, s.seeded = map[string]openCodeAsk{}, map[string]bool{}, map[string]bool{}
	s.notifyLocked()
}

// rootOf reports whether session is root or descends from it, as far as the
// stream told (openCodeAskAncestry hops). Under s.mu.
func (s *OpenCodeSource) underLocked(session, root string) bool {
	for i := 0; i <= openCodeAskAncestry && session != ""; i++ {
		if session == root {
			return true
		}
		session = s.parent[session]
	}
	return false
}

// pendingAsk is the newest pending ask of root session root (or of a session
// under it), and whether there is one. While the stream is not open there is
// none: what the host cannot see now, it does not claim.
func (s *OpenCodeSource) pendingAsk(root string) (openCodeAsk, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best openCodeAsk
	found := false
	if !s.connected {
		return best, false
	}
	for _, a := range s.asks {
		if !s.underLocked(a.Session, root) {
			continue
		}
		if !found || a.since > best.since || (a.since == best.since && a.ID > best.ID) {
			best, found = a, true
		}
	}
	return best, found
}

// seed reads root session root's pending asks once per connect, in the
// background (a listing never waits on it), and nudges when it learned any.
func (s *OpenCodeSource) seed(root string) {
	s.mu.Lock()
	if !s.connected || s.seeded[root] {
		s.mu.Unlock()
		return
	}
	s.seeded[root] = true
	gen := s.gen
	s.mu.Unlock()
	go func() {
		cl, err := s.client()
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
		defer cancel()
		asks, err := cl.PendingAsks(ctx, root)
		if err != nil {
			s.mu.Lock()
			if s.gen == gen {
				delete(s.seeded, root) // try again on the next match
			}
			s.mu.Unlock()
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.gen != gen {
			return // the stream reconnected meanwhile; that connect seeds again
		}
		added := false
		for _, a := range asks {
			// One settled while the read was in flight stays settled.
			if _, had := s.asks[a.ID]; !had && !s.settled[a.ID] {
				s.asks[a.ID] = openCodeAsk{Ask: a, since: s.now().UnixMilli()}
				added = true
			}
		}
		if added {
			s.notifyLocked()
		}
	}()
}

// Changes is a channel that receives (coalesced) whenever an ask is raised
// or settled, and its cancel: what a connection listens on to send its
// listing at once rather than at the next refresh.
func (s *OpenCodeSource) Changes() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	if s.subs == nil {
		s.subs = map[chan struct{}]bool{}
	}
	s.subs[ch] = true
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}
}

// notifyLocked signals every Changes listener. Under s.mu.
func (s *OpenCodeSource) notifyLocked() {
	for ch := range s.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// openCodeQuestionTool is the Tool an opencode question's Pending names:
// opencode's question tool (packages/core/src/tool/plugin/question.ts).
const openCodeQuestionTool = "question"

// pendingOf is ask a as the window's Pending (mirror.CleanPending applies the
// wire's rules): a permission request's action and what it acts on, with the
// answers muster may send; a question's text, with its options when muster
// can offer them all as they are (one single-select question of
// 1..MaxPendingOptions options).
func (a openCodeAsk) pendingOf() *mirror.Pending {
	switch a.Kind {
	case opencodehost.AskPermission:
		target := redact.Secrets(strings.Join(a.Resources, " "))
		p := &mirror.Pending{Kind: mirror.PendingPermission, Tool: a.Action, Summary: a.Action, PromptID: a.ID,
			Choices: strings.Join(mirror.OpenCodePermissionChoices, " ")}
		if target != "" {
			p.Summary = a.Action + ": " + target
		}
		return mirror.CleanPending(p)
	default:
		p := &mirror.Pending{Kind: mirror.PendingQuestion, Tool: openCodeQuestionTool, Summary: redact.Secrets(a.Title)}
		if len(a.Fields) > 0 {
			f := a.Fields[0]
			if q := strings.TrimSpace(f.Description); q != "" {
				p.Summary = redact.Secrets(q)
			} else if f.Title != "" {
				p.Summary = redact.Secrets(f.Title)
			}
			if n := len(a.Fields) - 1; n > 0 {
				more := " (and 1 more question)"
				if n > 1 {
					more = fmt.Sprintf(" (and %d more questions)", n)
				}
				p.Summary = mirror.OneLine(p.Summary, mirror.MaxPendingSummary-len([]rune(more))) + more
			}
		}
		if labels, ok := a.offerable(); ok {
			p.PromptID, p.Options = a.ID, strings.Join(labels, "\n")
		}
		return mirror.CleanPending(p)
	}
}

// offerable is a question form's option labels when muster can offer them as
// the answer: the question tool's form, ONE single-select field, 1..
// MaxPendingOptions options whose labels are unique and unchanged by the
// wire's cleaning (mirror.CleanOption), so the label muster shows is the
// label it sends back.
func (a openCodeAsk) offerable() ([]string, bool) {
	if a.Kind != opencodehost.AskQuestion || len(a.Fields) != 1 {
		return nil, false
	}
	f := a.Fields[0]
	if f.Type != "string" || len(f.Options) == 0 || len(f.Options) > mirror.MaxPendingOptions {
		return nil, false
	}
	seen := map[string]bool{}
	var out []string
	for _, o := range f.Options {
		if !mirror.CleanOption(o.Label) || seen[o.Label] || redact.Secrets(o.Label) != o.Label {
			return nil, false
		}
		seen[o.Label] = true
		out = append(out, o.Label)
	}
	return out, true
}

// The causes of an opencode answer's refusal (mirror.AskRefusalOpenCode…).
var (
	errOpenCodeGone        = errors.New("opencode is no longer waiting on that")
	errOpenCodeUnreachable = errors.New("the opencode server could not be reached")
	errOpenCodeUnknown     = errors.New("the answer was sent to opencode, which did not say how it went")
)

// answerAsk relays choice to pending ask promptID through the opencode
// server: a permission request's decision, or a question's option — sent as
// the option's VALUE, found by its label among the form's own options. On
// success the ask leaves this host's set at once (the replied event follows).
func (s *OpenCodeSource) answerAsk(ctx context.Context, promptID, choice string) error {
	s.mu.Lock()
	a, ok := s.asks[promptID]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: it is no longer pending on this host", errOpenCodeGone)
	}
	cl, err := s.client()
	if err != nil {
		return fmt.Errorf("%w: %v", errOpenCodeUnreachable, err)
	}
	switch a.Kind {
	case opencodehost.AskPermission:
		if choice != string(opencodehost.PermissionOnce) && choice != string(opencodehost.PermissionReject) {
			return fmt.Errorf("%w: not an answer muster sends a permission request", errOpenCodeGone)
		}
		err = cl.ReplyPermission(ctx, a.Session, a.ID, opencodehost.PermissionReply(choice))
	case opencodehost.AskQuestion:
		labels, ok := a.offerable()
		if !ok {
			return fmt.Errorf("%w: this question is answered in the terminal", errOpenCodeGone)
		}
		value := ""
		for i, l := range labels {
			if l == choice {
				value = a.Fields[0].Options[i].Value
			}
		}
		if value == "" {
			return fmt.Errorf("%w: not one of the question's options", errOpenCodeGone)
		}
		err = cl.ReplyForm(ctx, a.Session, a.ID, map[string]any{a.Fields[0].Key: value})
	default:
		return fmt.Errorf("%w: this form is answered in the terminal", errOpenCodeGone)
	}
	switch {
	case err == nil:
		s.mu.Lock()
		delete(s.asks, promptID)
		s.settled[promptID] = true
		s.notifyLocked()
		s.mu.Unlock()
		return nil
	case errors.Is(err, opencodehost.ErrAskSettled), errors.Is(err, opencodehost.ErrAnswerRefused):
		return fmt.Errorf("%w (%v)", errOpenCodeGone, err)
	case errors.Is(err, opencodehost.ErrUnauthorized):
		return fmt.Errorf("%w (%v)", errOpenCodeUnreachable, err)
	case isDialError(err):
		return fmt.Errorf("%w (%v)", errOpenCodeUnreachable, err)
	}
	return fmt.Errorf("%w (%v)", errOpenCodeUnknown, err)
}

// isDialError reports whether err is a failure to connect at all (nothing was
// sent), as opposed to one after the request went out.
func isDialError(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

// openCodeAnswer is what this host advertises as Msg.OpenCodeAnswer: it
// relays answers to opencode — an -interactive v2 host started with
// -opencode-answer, whose backend reads an opencode server with a token.
func (h *hostSide) openCodeAnswer() bool {
	if h.version < 2 || !h.c.cfg.Interactive || !h.c.cfg.OpenCodeAnswer {
		return false
	}
	src, ok := h.openCodeSource()
	return ok && src.paired()
}

// answerOpenCode answers an opencode ask (m.Runtime "opencode",
// m.OpenCodeReply) through the opencode server's API, and answers answered.
//
// 🔴 REFUSED UNLESS THIS HOST OPTED IN (-interactive and -opencode-answer),
// whatever muster asks — muster refuses it too — and only for a listed
// window whose pane runs opencode NOW, waiting on exactly this ask and
// offering this answer: the window is re-resolved here. Never a key into the
// pane.
func (h *hostSide) answerOpenCode(m mirror.Msg) {
	reply := func(err error) {
		ans := mirror.Msg{Type: mirror.TypeAnswered, ID: m.ID}
		if err != nil {
			ans.Error = err.Error()
			switch {
			case errors.Is(err, errOpenCodeGone):
				ans.Refusal = mirror.AskRefusalOpenCodeGone
			case errors.Is(err, errOpenCodeUnreachable):
				ans.Refusal = mirror.AskRefusalOpenCodeUnreachable
			case errors.Is(err, errOpenCodeUnknown):
				ans.Refusal = mirror.AskRefusalOpenCodeUnknown
			case errors.Is(err, errCannotAnswer):
				ans.Refusal = mirror.AskRefusalUnsupported
			}
			h.c.log.Printf("muster-mirror: opencode ask in %q%s not answered: %v", m.Session, windowSuffix(m.Window), err)
		} else {
			h.c.log.Printf("muster-mirror: answered an opencode ask in %q%s through its server", m.Session, windowSuffix(m.Window))
		}
		_ = h.send(ans)
	}
	if !h.openCodeAnswer() {
		reply(fmt.Errorf("%w: this host does not relay answers to opencode (muster-mirror needs -interactive and -opencode-answer, and an opencode token)", errCannotAnswer))
		return
	}
	if err := h.allowed(m); err != nil {
		reply(err)
		return
	}
	r := m.OpenCodeReply
	if r == nil || !mirror.ValidOpenCodeAskID(r.PromptID) || r.Choice == "" {
		reply(fmt.Errorf("%w: not an answer muster sends", errOpenCodeGone))
		return
	}
	src, _ := h.openCodeSource()
	ctx, cancel := context.WithTimeout(h.ctx, 15*time.Second)
	defer cancel()
	wi, err := h.window(ctx, m.Session, m.Window)
	if err != nil {
		reply(fmt.Errorf("%w: %v", errOpenCodeGone, err))
		return
	}
	ag := wi.Agent
	if ag == nil || ag.Runtime != mirror.RuntimeOpenCode || ag.Gen == "" || ag.Status != mirror.StatusWaiting ||
		ag.Pending == nil || ag.Pending.PromptID != r.PromptID || !ag.Pending.Offers(r.Choice) {
		reply(fmt.Errorf("%w: the window's pane is not opencode waiting on that now", errOpenCodeGone))
		return
	}
	reply(src.answerAsk(ctx, r.PromptID, r.Choice))
}
