package mirrorhost

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// ENDING AN AGENT WINDOW ON MUSTER'S REQUEST (agent-close; the protocol is
// internal/mirror/agentclose.go): the host half.
//
// 🔴 NARROW BY CONSTRUCTION:
//   - only an -interactive v2 host whose backend lists windows, pastes into a
//     pane and closes a window advertises it (agentCloseOn);
//   - the request names ONE window, by its tmux session and window id, and
//     the host closes it only when it LISTS that window now, in that session,
//     running an agent it knows how to ask to exit (Claude Code, opencode).
//     Anything else is refused (not_listed, no_agent) and nothing is typed or
//     closed;
//   - the token the host connects with names its owner, so the request is
//     always that owner's.
//
// GRACEFUL, THEN CLOSED: the host types /exit into the window's agent pane
// (both runtimes take it), then re-lists every closePoll: the window gone (the
// agent was its process) or its pane back at no agent is a graceful end — the
// window is then closed if still there. Once the grace (mirror.AgentCloseGrace,
// Config.AgentCloseGrace in tests) passes without that, the window is closed
// anyway.

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
	r, err := h.c.closeAgent(h.ctx, h.agentCloseOn(), req)
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

// closeAgent runs one agent-close (see the file's header).
func (c *Client) closeAgent(ctx context.Context, on bool, req mirror.AgentCloseReq) (mirror.AgentCloseResult, error) {
	refuse := func(code, f string, a ...any) (mirror.AgentCloseResult, error) {
		return mirror.AgentCloseResult{}, &mirror.CloseRefusedError{Code: code, Detail: fmt.Sprintf(f, a...)}
	}
	if !on {
		return refuse(mirror.CloseUnavailable, "this host does not close windows")
	}
	wl, _ := capabilityOf[WindowLister](c.cfg.Backend)
	wc, _ := capabilityOf[WindowCloser](c.cfg.Backend)
	pb, _ := PaneOf(c.cfg.Backend)
	if mirror.ValidSession(req.Session) != nil || mirror.ValidWindowID(req.Window) != nil {
		return refuse(mirror.CloseNotListed, "the request names no window")
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
		return refuse(mirror.CloseNoAgent, "window %s runs no agent this host can ask to exit", req.Window)
	}
	if err := pb.PastePane(ctx, pane, "/exit"); err != nil {
		c.log.Printf("muster-mirror: close %s:%s: /exit not typed (%v); closing after the grace", req.Session, req.Window, err)
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
		if agentPaneOf(w) == "" {
			// The agent exited; its pane is back at a shell: close the window.
			if err := wc.KillWindow(ctx, req.Window); err != nil {
				return mirror.AgentCloseResult{}, err
			}
			return mirror.AgentCloseResult{Graceful: true}, nil
		}
	}
	if err := wc.KillWindow(ctx, req.Window); err != nil {
		return mirror.AgentCloseResult{}, err
	}
	return mirror.AgentCloseResult{Graceful: false}, nil
}
