package mirror

import "errors"

// THE SESSION A MESSAGE WAS WRITTEN FOR — ONE RULE FOR EVERY RUNTIME.
//
// A message muster delivers later than it was written (a queued row) names the
// agent session it was written for (ShownSession; send_queue.agent_session),
// and is refused — never superseded, so it fails visibly instead of waiting —
// when the window no longer holds that session. The runtime decides only
// which of the window's ids is "its session" and the words of the refusal:
//
//   - a gated-send runtime (opencode): Window.AgentSession, and a hand-opened
//     pane's message naming none is refused too (ShownSessionCheck);
//   - Claude Code: Window.ClaudeSession (the host's ClaudePane.Gen), and a
//     message naming none goes as before (claudeShownSessionCheck) — a row
//     queued before muster recorded the session, a person's own Send (which
//     never names one), a chief's route or forward (gated on their own pins).
//
// Hub.PasteWindow applies it to the window its host lists NOW; internal/api's
// fake mirror plays the same function, so the two cannot drift. For Claude
// Code the paste then carries the session (Msg.AgentSession) and a current
// muster-mirror checks it again against the pane's Gen at the moment it types
// (mirrorhost's claudeReady), which closes the window between the host's last
// report and the paste. An older muster-mirror ignores the field: this
// server-side check is then the only one.

// sessionMoved reports whether a message naming shown ("" none) was written
// for another session than listed, the one the window holds now. A message
// naming none has not moved: each runtime decides what none means.
func sessionMoved(shown, listed string) bool { return shown != "" && shown != listed }

// ErrClaudeMoved: the Claude Code window holds another session now (Claude
// Code was restarted in the pane, or /clear started a new conversation) than
// the one the message was queued for. Nothing was sent.
var ErrClaudeMoved = errors.New("mirror: muster did not send it: the window no longer holds the Claude session this was queued for")

// claudeShownSessionCheck is the Claude Code half of ShownSessionRefusal (its
// RuntimeSpec.ShownRefusal): the refusal of a message naming shown to a window
// holding Claude session listed.
func claudeShownSessionCheck(shown, listed string) error {
	if sessionMoved(shown, listed) {
		return &PasteRefusedError{Why: ClaudeMovedText, Refusal: PasteRefusalClaudeMoved, Cause: ErrClaudeMoved}
	}
	return nil
}

// ShownSessionRefusal is THE rule above for window w: the refusal of a
// rendered-view message naming shown (the session it was written for, ""
// none), nil when it may go. A window of no agent runtime has no session to
// hold: nil (the paste's other checks refuse it).
func (w Window) ShownSessionRefusal(shown string) error {
	if r, ok := w.Spec(); ok && r.ShownRefusal != nil {
		return r.ShownRefusal(w, shown)
	}
	return nil
}

// shownSessionRefusal is ShownSessionRefusal for the window hc lists as
// (session, window id) now; nil when it lists none (a per-session host, which
// names no session: the host's own check is all there is).
func (hc *hostConn) shownSessionRefusal(session, id, shown string) error {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	for _, w := range hc.windows {
		if w.Session == session && w.ID == id {
			return w.ShownSessionRefusal(shown)
		}
	}
	return nil
}

// pastePinned reports whether hc lists (session, window id) now as a window of
// a runtime whose messages are PASTED (no gated send) and pinned to a session
// (RuntimeSpec.ShownRefusal) — Claude Code: its paste then carries the
// session for the host's own check (Msg.AgentSession).
func (hc *hostConn) pastePinned(session, id string) bool {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	for _, w := range hc.windows {
		if w.Session == session && w.ID == id {
			r, ok := w.Spec()
			return ok && !r.GatedSend && r.ShownRefusal != nil
		}
	}
	return false
}
