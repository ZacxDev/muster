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

// openCodeAsk is one pending ask, when this host first saw it (Unix ms), and
// the stream generation (OpenCodeSource.gen) that last confirmed it: raised on
// it, or listed by a re-read since it opened.
type openCodeAsk struct {
	opencodehost.Ask
	since int64
	gen   uint64
}

// turnEndEvents end a session's turn (upstream 6ec5682,
// packages/core/src/session/execution.ts: settled publishes exactly one of
// these per busy period).
var turnEndEvents = map[string]bool{"session.execution.succeeded": true, "session.execution.failed": true, "session.execution.interrupted": true}

// askEvent records what e says about asks and sessions' parents; under s.mu.
// It reports the session an ask event names ("" for none): its change count
// moves (Event.SessionID cannot see a form's — it is nested).
//
// 🔴 A TURN'S END TAKES ITS PERMISSION REQUESTS WITH IT. Upstream, a pending
// permission request whose turn is interrupted is dropped from the server's
// pending map WITHOUT a permission.replied (packages/core/src/permission.ts:
// 251-262, an Effect.ensuring that only deletes it) — measured against a real
// `opencode serve` (testdata/abort-v2.0.32.sse): permission.asked, then
// session.execution.interrupted, and nothing else names the request. A form
// is cancelled on interrupt (form.ts:157-158 → form.cancelled), so only
// permission requests are cleared here.
func (s *OpenCodeSource) askEvent(e opencodehost.Event) string {
	if child, parent := e.ParentOf(); child != "" && parent != "" {
		s.parent[child] = parent
	}
	if turnEndEvents[e.Type] {
		id, cleared := e.SessionID(), false
		for aid, a := range s.asks {
			if a.Session == id && a.Kind == opencodehost.AskPermission {
				delete(s.asks, aid)
				s.settled[aid], cleared = true, true
			}
		}
		if cleared {
			s.notifyLocked()
		}
		return ""
	}
	raised, settled, session, ok := e.AskEvent()
	if !ok {
		return ""
	}
	if raised != nil {
		if a, had := s.asks[raised.ID]; had {
			a.gen = s.gen
			s.asks[raised.ID] = a
		} else if !s.settled[raised.ID] {
			s.asks[raised.ID] = openCodeAsk{Ask: *raised, since: s.now().UnixMilli(), gen: s.gen}
		}
	} else {
		delete(s.asks, settled)
		s.settled[settled] = true
	}
	s.notifyLocked()
	return session
}

// holdAsks starts a new stream generation on a (re)connect or a skipped
// event: every ask known so far is HELD — still reported, stale but believed
// — and each matched session's pending lists are read again (seed), which is
// the one place an ask settled in the gap leaves. Clearing them at the gap
// instead would withdraw the owner's notification, and the same ask coming
// back within the attention queue's re-push window would never be pushed
// again. Under s.mu.
func (s *OpenCodeSource) holdAsks() {
	s.gen++
	s.settled, s.seeded = map[string]bool{}, map[string]bool{}
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
// under it), and whether there is one. While the stream is down the asks last
// known are HELD and reported as they are (holdAsks): a disconnect is not an
// answer.
func (s *OpenCodeSource) pendingAsk(root string) (openCodeAsk, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best openCodeAsk
	found := false
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

// openCodeSeedSessions bounds how many sessions (a root and the subagents
// under it) one seed reads.
const openCodeSeedSessions = 16

// seed reads the pending lists of root session root, and of each session the
// stream told is under it, once per stream generation, in the background (a
// listing never waits on it), and RECONCILES: an ask those sessions list is
// added (or confirmed); a held ask of theirs from an earlier generation that
// none lists was settled in the gap, and leaves. A failed read changes
// nothing (the held asks stay) and is retried on the next match.
func (s *OpenCodeSource) seed(root string) {
	s.mu.Lock()
	if !s.connected || s.seeded[root] {
		s.mu.Unlock()
		return
	}
	s.seeded[root] = true
	gen := s.gen
	sessions := []string{root}
	for child := range s.parent {
		if len(sessions) < openCodeSeedSessions && child != root && s.underLocked(child, root) {
			sessions = append(sessions, child)
		}
	}
	s.mu.Unlock()
	go func() {
		retry := func() {
			s.mu.Lock()
			if s.gen == gen {
				delete(s.seeded, root) // try again on the next match
			}
			s.mu.Unlock()
		}
		cl, err := s.client()
		if err != nil {
			retry()
			return
		}
		ctx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
		defer cancel()
		var listed []opencodehost.Ask
		read := map[string]bool{}
		for _, id := range sessions {
			asks, err := cl.PendingAsks(ctx, id)
			if err != nil {
				retry()
				return
			}
			listed, read[id] = append(listed, asks...), true
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.gen != gen {
			return // a newer generation began meanwhile; it seeds again
		}
		changed, on := false, map[string]bool{}
		for _, a := range listed {
			on[a.ID] = true
			if cur, had := s.asks[a.ID]; had {
				cur.gen = gen
				s.asks[a.ID] = cur
			} else if !s.settled[a.ID] { // one settled while the read was in flight stays settled
				s.asks[a.ID] = openCodeAsk{Ask: a, since: s.now().UnixMilli(), gen: gen}
				changed = true
			}
		}
		for id, a := range s.asks {
			if read[a.Session] && a.gen < gen && !on[id] {
				delete(s.asks, id) // settled while the stream could not say so
				changed = true
			}
		}
		if changed {
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
	errOpenCodeGone          = errors.New("opencode is no longer waiting on that")
	errOpenCodeUnreachable   = errors.New("the opencode server could not be reached")
	errOpenCodeUnknown       = errors.New("the answer was sent to opencode, which did not say how it went")
	errOpenCodeAnswerRefused = errors.New("opencode refused the answer: the question cannot take that value")
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
	case errors.Is(err, opencodehost.ErrAskSettled):
		return fmt.Errorf("%w (%v)", errOpenCodeGone, err)
	case errors.Is(err, opencodehost.ErrAnswerRefused):
		// The server's 400: the ask is still pending; its field refused the value.
		return fmt.Errorf("%w (%v)", errOpenCodeAnswerRefused, err)
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
// relays answers to opencode — an -interactive v2 host (the same gate as an
// answer typed into Claude Code's picker) whose backend reads an opencode
// server with a token. A read-only host still lists every ask; muster then
// says to answer it in the terminal.
func (h *hostSide) openCodeAnswer() bool {
	if h.version < 2 || !h.c.cfg.Interactive {
		return false
	}
	src, ok := h.openCodeSource()
	return ok && src.paired()
}

// answerOpenCode answers an opencode ask (m.Runtime "opencode",
// m.OpenCodeReply) through the opencode server's API, and answers answered.
//
// 🔴 REFUSED UNLESS THIS HOST IS -interactive (and holds an opencode token),
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
			case errors.Is(err, errOpenCodeAnswerRefused):
				ans.Refusal = mirror.AskRefusalOpenCodeRefused
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
		reply(fmt.Errorf("%w: this host does not relay answers to opencode (muster-mirror needs -interactive, and an opencode token)", errCannotAnswer))
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
