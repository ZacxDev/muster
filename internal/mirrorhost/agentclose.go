package mirrorhost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// ENDING AN AGENT WINDOW ON MUSTER'S REQUEST (agent-close; the protocol is
// internal/mirror/agentclose.go): the host half.
//
// 🔴 NARROW BY CONSTRUCTION:
//   - only an -interactive v2 host whose backend lists windows, pastes into a
//     pane and closes a window advertises it (agentCloseOn);
//   - the request names ONE window, by its tmux session and window id, AND
//     the conversation muster expects it to run (AgentCloseReq.Key). The host
//     closes it only when it LISTS that window now, in that session, running
//     an agent (Claude Code, opencode) in exactly that conversation. Anything
//     else is refused (not_listed, no_agent, mismatch) and nothing is typed
//     or closed;
//   - the token the host connects with names its owner, so the request is
//     always that owner's.
//
// 🔴 /exit IS TYPED ONLY THROUGH THE CLAUDE PROMPT GUARD (claude_guard.go),
// never a raw paste: Enter on a permission prompt approves it, on an
// AskUserQuestion picker takes its first option, and after a half-typed
// draft submits the draft with "/exit" on it. So for a Claude Code window the
// host first runs the paste guard and claudeReady (its session status, its
// screen, an EMPTY input box), and types /exit only through the checked
// paste (ClaudeCheckedPaster), which checks again before the paste and before
// its Enter. Any refusal types nothing (or withholds the Enter) and the window
// is closed at once, reported not graceful with the reason.
//
// OPENCODE IS CLOSED AT ONCE: muster types into no opencode pane (the paste
// guard refuses it), and the opencode server API this host uses offers no
// call that exits its TUI. Its window is closed without waiting, reported not
// graceful.
//
// GRACEFUL, THEN CLOSED (Claude Code): after /exit the host re-lists every
// closePoll. The window gone, or the agent's OWN pane (the pane id read at
// the start, with its process) no longer running it, is a graceful end — the
// window is then closed if still there. Once the grace
// (mirror.AgentCloseGrace, Config.AgentCloseGrace in tests) passes without
// that, the window is listed once more and closed only if it still runs that
// conversation.

// WindowCloser is a Backend that can close one window by its id. Tmux is one.
type WindowCloser interface {
	KillWindow(ctx context.Context, window string) error
}

// KillWindow implements WindowCloser: tmux kill-window on the window id
// (unique on its server).
func (t Tmux) KillWindow(ctx context.Context, window string) error {
	if mirror.ValidWindowID(window) != nil {
		return fmt.Errorf("muster-mirror: %q is not a tmux window id", window)
	}
	_, err := t.run(ctx, "kill-window", "-t", window)
	return err
}

// closePoll is how often the host re-lists while it waits for the agent to exit.
const closePoll = 300 * time.Millisecond

// agentCloseOn is what this host advertises as Msg.AgentClose.
func (h *hostSide) agentCloseOn() bool {
	if h.version < 2 || !h.c.cfg.Interactive {
		return false
	}
	_, lists := capabilityOf[WindowLister](h.c.cfg.Backend)
	_, closes := capabilityOf[WindowCloser](h.c.cfg.Backend)
	_, pastes := PaneOf(h.c.cfg.Backend)
	return lists && closes && pastes
}

// agentClose serves an agent-close and answers agent-closed.
func (h *hostSide) agentClose(m mirror.Msg) {
	ans := mirror.Msg{Type: mirror.TypeAgentClosed, ID: m.ID}
	var req mirror.AgentCloseReq
	if m.CloseReq != nil {
		req = *m.CloseReq
	}
	r, err := h.closeAgent(h.ctx, h.agentCloseOn(), req)
	var e *mirror.CloseRefusedError
	switch {
	case err == nil:
		ans.CloseResult = &r
		h.c.log.Printf("muster-mirror: CLOSED %s:%s on muster's request (graceful: %t)", req.Session, req.Window, r.Graceful)
	case errors.As(err, &e):
		ans.Refusal, ans.Error = e.Code, e.Detail
		h.c.log.Printf("muster-mirror: close of %s:%s refused (%s): %s", req.Session, req.Window, e.Code, e.Detail)
	default:
		ans.Refusal, ans.Error = mirror.CloseFailed, err.Error()
		h.c.log.Printf("muster-mirror: close of %s:%s failed: %v", req.Session, req.Window, err)
	}
	_ = h.send(ans)
	h.nudge()
}

// closeGrace is the grace this client waits for the agent to exit.
func (c *Client) closeGrace() time.Duration {
	if c.cfg.AgentCloseGrace > 0 {
		return c.cfg.AgentCloseGrace
	}
	return mirror.AgentCloseGrace
}

// listedWindow is the window session:window as this host lists it now.
func listedWindow(ctx context.Context, wl WindowLister, session, window string) (WindowInfo, bool, error) {
	ws, err := wl.Windows(ctx)
	if err != nil {
		return WindowInfo{}, false, err
	}
	for _, w := range ws {
		if w.Session == session && w.ID == window {
			return w, true, nil
		}
	}
	return WindowInfo{}, false, nil
}

// agentPaneOf is the pane of w's agent (Claude Code or another runtime's),
// "" for none.
func agentPaneOf(w WindowInfo) string {
	switch {
	case w.Claude != nil:
		return w.Claude.Pane
	case w.Agent != nil:
		return w.Pane
	}
	return ""
}

// windowSessionKey is the session key of the conversation w runs, as the
// host SENDS it (WireWindows, mirror.Window.SessionKey): "" for none.
func windowSessionKey(w WindowInfo) string {
	ws := WireWindows([]WindowInfo{w}, nil, true, time.Now())
	if len(ws) != 1 {
		return ""
	}
	return ws[0].SessionKey()
}

// windowRuns reports whether window w runs the conversation keyed key: in
// its active pane (windowSessionKey), or — a Claude Code session whose pane
// is no longer the active one — in one of its other panes (w.Others).
func windowRuns(w WindowInfo, key string) bool {
	if windowSessionKey(w) == key {
		return true
	}
	for _, o := range w.Others {
		if o.Gen == key {
			return true
		}
	}
	return false
}

// agentProcAlive reports whether process pid still runs. A var only so a
// test can play a process that outlives its pane's focus.
var agentProcAlive = func(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

// The reasons a close was not graceful (AgentCloseResult.Reason), as the
// owner reads them.
const (
	closeReasonOpenCode = "opencode is not asked to exit: muster types nothing into an opencode pane, and opencode offers no exit call, so its window was closed at once"
	closeReasonGrace    = "Claude Code did not exit within the grace after /exit, so its window was closed"
	closeReasonNotTyped = "nothing was typed into Claude Code, because %s; its window was closed at once"
	// closeReasonWithheld: the check before the Enter refused, after /exit
	// was pasted (ClaudePastedError).
	closeReasonWithheld = "/exit was typed into Claude Code but Enter was withheld, because %s; its window was closed at once"
	// closeReasonOther is a graceful end's note when the window was left open.
	closeReasonOther = "Claude Code exited; its window runs another agent now, so it was left open"
)

// closeAgent runs one agent-close (see the file's header).
func (h *hostSide) closeAgent(ctx context.Context, on bool, req mirror.AgentCloseReq) (mirror.AgentCloseResult, error) {
	c := h.c
	refuse := func(code, f string, a ...any) (mirror.AgentCloseResult, error) {
		return mirror.AgentCloseResult{}, &mirror.CloseRefusedError{Code: code, Detail: fmt.Sprintf(f, a...)}
	}
	if !on {
		return refuse(mirror.CloseUnavailable, "this host does not close windows")
	}
	wl, _ := capabilityOf[WindowLister](c.cfg.Backend)
	wc, _ := capabilityOf[WindowCloser](c.cfg.Backend)
	if mirror.ValidSession(req.Session) != nil || mirror.ValidWindowID(req.Window) != nil {
		return refuse(mirror.CloseNotListed, "the request names no window")
	}
	if !mirror.ValidSessionKey(req.Key) {
		return refuse(mirror.CloseMismatch, "the request names no conversation")
	}
	w, ok, err := listedWindow(ctx, wl, req.Session, req.Window)
	if err != nil {
		return mirror.AgentCloseResult{}, err
	}
	if !ok {
		return refuse(mirror.CloseNotListed, "no window %s in session %s", req.Window, req.Session)
	}
	pane := agentPaneOf(w)
	if pane == "" {
		return refuse(mirror.CloseNoAgent, "window %s runs no agent this host can end", req.Window)
	}
	if k := windowSessionKey(w); k != req.Key {
		return refuse(mirror.CloseMismatch, "window %s runs a different conversation now", req.Window)
	}
	// kill closes the window, re-listing it first: only a window still
	// running the request's conversation is closed (one gone meanwhile is
	// gone; one running another is refused).
	kill := func(reason string) (mirror.AgentCloseResult, error) {
		w, ok, err := listedWindow(ctx, wl, req.Session, req.Window)
		if err != nil {
			return mirror.AgentCloseResult{}, err
		}
		if !ok {
			return mirror.AgentCloseResult{Graceful: reason == "", Reason: reason}, nil
		}
		if !windowRuns(w, req.Key) {
			return refuse(mirror.CloseMismatch, "window %s runs a different conversation now; it was not closed", req.Window)
		}
		if err := wc.KillWindow(ctx, req.Window); err != nil {
			return mirror.AgentCloseResult{}, err
		}
		return mirror.AgentCloseResult{Graceful: reason == "", Reason: reason}, nil
	}
	if w.Claude == nil {
		return kill(closeReasonOpenCode)
	}
	agent := *w.Claude
	if reason := h.typeExit(ctx, req, agent); reason != "" {
		c.log.Printf("muster-mirror: close %s:%s: %s", req.Session, req.Window, reason)
		return kill(reason)
	}
	deadline := time.Now().Add(c.closeGrace())
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return mirror.AgentCloseResult{}, ctx.Err()
		case <-time.After(closePoll):
		}
		w, ok, err := listedWindow(ctx, wl, req.Session, req.Window)
		if err != nil {
			continue
		}
		if !ok {
			return mirror.AgentCloseResult{Graceful: true}, nil
		}
		if agentExited(w, agent) {
			if agentPaneOf(w) != "" {
				// It exited, and the window runs another agent now: that one
				// is not the request's, so the window is left open.
				return mirror.AgentCloseResult{Graceful: true, Reason: closeReasonOther}, nil
			}
			// The agent exited; its pane is back at a shell: close the window.
			if err := wc.KillWindow(ctx, req.Window); err != nil {
				return mirror.AgentCloseResult{}, err
			}
			return mirror.AgentCloseResult{Graceful: true}, nil
		}
	}
	return kill(closeReasonGrace)
}

// agentExited reports whether the Claude Code agent was (its pane and
// process, read at the start) no longer runs in window w: read from ITS OWN
// pane — while that pane is still the window's active one, by what the
// listing says runs there; once another pane is active, by its process.
func agentExited(w WindowInfo, agent ClaudePane) bool {
	if w.Pane == agent.Pane {
		return w.Claude == nil || w.Claude.PID != agent.PID
	}
	return !agentProcAlive(agent.PID)
}

// typeExit types /exit into Claude Code pane agent of req's window, through
// the paste guard and the Claude prompt guard's checked paste, only when the
// pane is at an idle prompt with an empty input box: "" when it was typed and
// submitted, else the close's reason (closeWhy): why nothing was typed, or why
// the Enter was withheld.
func (h *hostSide) typeExit(ctx context.Context, req mirror.AgentCloseReq, agent ClaudePane) string {
	cb, ok := h.claudeBackend()
	cp, okc := capabilityOf[ClaudeCheckedPaster](h.c.cfg.Backend)
	if !ok || !okc {
		return "this host cannot check Claude Code's screen before typing"
	}
	m := mirror.Msg{Session: req.Session, Window: req.Window}
	before := func(ctx context.Context) error { return h.claudeReady(ctx, cb, m, agent.Pane, true) }
	beforeEnter := func(ctx context.Context) error { return h.claudeReady(ctx, cb, m, agent.Pane, false) }
	if err := h.guardPane(ctx, agent.Pane); err != nil {
		return closeWhy(err)
	}
	if err := before(ctx); err != nil {
		return closeWhy(err)
	}
	if err := h.typeInto(ctx, agent.Pane, func() error { return cp.PasteChecked(ctx, agent, "/exit", before, beforeEnter) }); err != nil {
		return closeWhy(err)
	}
	return ""
}

// closeWhy is a guard's refusal as the close's reason says it. 🔴 THE
// WITHHELD ENTER FIRST: a ClaudePastedError unwraps to the refusal that
// withheld it, so the plain cases would otherwise claim nothing was typed.
func closeWhy(err error) string {
	var pe *ClaudePastedError
	if errors.As(err, &pe) {
		return fmt.Sprintf(closeReasonWithheld, guardWhy(pe.Why))
	}
	return fmt.Sprintf(closeReasonNotTyped, guardWhy(err))
}

// guardWhy is a guard's refusal as a clause.
func guardWhy(err error) string {
	switch {
	case errors.Is(err, ErrClaudeWaiting):
		return "it was waiting on you (a permission prompt or a question)"
	case errors.Is(err, ErrClaudeDialog):
		return "its screen showed a dialog (a permission prompt, a picker or a menu)"
	case errors.Is(err, ErrClaudeBox):
		return "its input box held a draft"
	}
	return strings.TrimSuffix(strings.TrimPrefix(err.Error(), "Not sent: "), ".")
}
