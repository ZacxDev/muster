package mirror

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ENDING A SESSION (docs/design/chief-in-agent-view.md, end-session): muster
// asks a host to end ONE of its owner's agent windows, on the owner's tap of
// the chief's proposal card — never on its own.
//
//   - agent-close (muster → host) names exactly one window: its tmux session
//     and window id, and the conversation muster expects there (CloseReq.Key,
//     a session key). The host checks the window is one it LISTS now, of that
//     session, running an agent (Claude Code or opencode) in THAT
//     conversation, and refuses anything else.
//   - A Claude Code agent is asked to exit cleanly: the host types /exit only
//     through the Claude prompt guard's checked paste, and only when the pane
//     is at an idle prompt with an empty input box. Then it waits for the
//     agent's process to end — the window closes with it, or its pane is back
//     at a shell, which it then closes — and closes the window itself once
//     the grace (AgentCloseGrace) has passed without that. A pane waiting on
//     the person (a permission prompt, a picker, a dialog) or holding a draft
//     is typed nothing: its window is closed at once.
//   - An opencode window is closed at once: muster types into no opencode
//     pane, and opencode's server offers no call that exits its TUI.
//   - agent-closed (host → muster) says how it ended (CloseResult.Graceful,
//     and Reason when it was not), or the refusal.
//
// 🔴 OPT-IN BY CAPABILITY, BACKWARD-COMPATIBLE BOTH WAYS: a host advertises
// Msg.AgentClose (an -interactive v2 host whose backend can close a window);
// muster asks no other host. An older host never advertises it.
const (
	// TypeAgentClose (muster → host): end the agent in the window CloseReq
	// names.
	TypeAgentClose = "agent-close"
	// TypeAgentClosed (host → muster): its outcome (CloseResult, or Refusal
	// and Error), answering the request's ID.
	TypeAgentClosed = "agent-closed"
)

// The host's agent-close refusals (Msg.Refusal).
const (
	// CloseNotListed: no window of that session with that id is listed.
	CloseNotListed = "not_listed"
	// CloseNoAgent: the window runs no agent (Claude Code or opencode).
	CloseNoAgent = "no_agent"
	// CloseUnavailable: this host does not close windows (not -interactive,
	// or a backend that cannot).
	CloseUnavailable = "unavailable"
	// CloseFailed: the close itself failed.
	CloseFailed = "failed"
	// CloseMismatch: the window runs a different conversation than the one
	// the request names (CloseReq.Key), or none at all now.
	CloseMismatch = "mismatch"
)

// AgentCloseGrace is how long a host waits for the agent to exit on its own
// before it closes the window anyway (operator decision: ~30 s).
const AgentCloseGrace = 30 * time.Second

// AgentCloseReq is an agent-close's request (Msg.CloseReq): the window, and
// Key, the session key of the conversation muster expects it to run
// (Window.SessionKey's rule: a Claude session id, or "opencode:<id>"). The
// host refuses (CloseMismatch) a window running any other.
type AgentCloseReq struct {
	Session string `json:"session"`
	Window  string `json:"window"`
	Key     string `json:"key"`
}

// AgentCloseResult is an agent-closed's outcome (Msg.CloseResult): Graceful
// when the agent exited on its own after /exit, false when the host closed
// the window without that, with Reason saying why (it was waiting on the
// person, an opencode window, the grace passed).
type AgentCloseResult struct {
	Graceful bool   `json:"graceful"`
	Reason   string `json:"reason,omitempty"`
}

// CloseRefusedError is a host's refusal of an agent-close.
type CloseRefusedError struct{ Code, Detail string }

func (e *CloseRefusedError) Error() string {
	return "mirror: the host refused to close (" + e.Code + "): " + e.Detail
}

var (
	// ErrCloseUnsupported: the host did not advertise agent-close.
	ErrCloseUnsupported = errors.New("mirror: this host's muster-mirror does not close windows (it needs -interactive and a current muster-mirror)")
	// ErrCloseUnconfirmed: the host did not answer in time; the window may be closing.
	ErrCloseUnconfirmed = errors.New("mirror: the host did not answer the close in time; the window may still be closing")
)

// closeWait is the server's wait for agent-closed: the grace plus a margin
// for the host's own checks and the frame to cross.
const closeWait = AgentCloseGrace + 15*time.Second

// closeWaitOverride, when set (tests), replaces closeWait.
var closeWaitOverride time.Duration

type closeAnswer struct {
	r   AgentCloseResult
	err error
}

// CloseAgent asks owner's host to end the agent in window req and waits for
// its answer. 🔴 ITS ONE CALLER is internal/api's end-session accept, on the
// owner's tap (a seam ledger pins that).
func (h *Hub) CloseAgent(ctx context.Context, owner int64, host string, req AgentCloseReq) (r AgentCloseResult, err error) {
	ctx, done := hubOp(ctx, "agent-close")
	defer func() { done(err) }()
	if ValidSession(req.Session) != nil || ValidWindowID(req.Window) != nil || !ValidSessionKey(req.Key) {
		return AgentCloseResult{}, errors.New("mirror: a close names one window (its session and its window id) and the conversation it runs")
	}
	h.mu.Lock()
	hc := h.hosts[hostKey{owner, host}]
	h.mu.Unlock()
	if hc == nil {
		return AgentCloseResult{}, fmt.Errorf("%w (host %q)", ErrHostOffline, host)
	}
	if !hc.interactive || !hc.agentClose {
		return AgentCloseResult{}, fmt.Errorf("%w (host %q)", ErrCloseUnsupported, host)
	}
	hc.mu.Lock()
	hc.next++
	id := hc.next
	ch := make(chan closeAnswer, 1)
	hc.closes[id] = ch
	hc.mu.Unlock()
	defer func() {
		hc.mu.Lock()
		delete(hc.closes, id)
		hc.mu.Unlock()
	}()
	q := req
	if err := writeJSON(ctx, hc.ws, Msg{Type: TypeAgentClose, ID: id, CloseReq: &q}); err != nil {
		return AgentCloseResult{}, fmt.Errorf("mirror: send the close to the host: %w", err)
	}
	wait := closeWait
	if closeWaitOverride > 0 {
		wait = closeWaitOverride
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case a := <-ch:
		return a.r, a.err
	case <-ctx.Done():
		return AgentCloseResult{}, ErrCloseUnconfirmed
	case <-t.C:
		return AgentCloseResult{}, ErrCloseUnconfirmed
	}
}

// CloseReplyResult reads an agent-closed frame: the outcome, or the refusal.
func CloseReplyResult(m Msg) (AgentCloseResult, error) {
	if m.Refusal != "" || m.Error != "" {
		code := m.Refusal
		if code == "" {
			code = CloseFailed
		}
		return AgentCloseResult{}, &CloseRefusedError{Code: code, Detail: m.Error}
	}
	if m.CloseResult == nil {
		return AgentCloseResult{}, &CloseRefusedError{Code: CloseFailed, Detail: "the host named no outcome"}
	}
	return *m.CloseResult, nil
}

// agentClosed hands the host's answer to the close waiting on m.ID, if any.
func (h *Hub) agentClosed(hc *hostConn, m Msg) {
	r, err := CloseReplyResult(m)
	hc.mu.Lock()
	ch := hc.closes[m.ID]
	delete(hc.closes, m.ID)
	hc.mu.Unlock()
	if ch != nil {
		ch <- closeAnswer{r: r, err: err}
	}
}
