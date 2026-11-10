package mirrorhost

import (
	"context"
	"errors"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/opencodehost"
)

// HAND-OPENED OPENCODE PANES (docs/design/opencode-first-class.md, "Panes the
// launcher did not start"): a pane whose TUI `muster-mirror opencode` did not
// start has no launcher record, and opencode reports no endpoint or event
// naming the session a TUI shows. Its session is the one its pane title
// names (opencodehost.MatchTitle). That match is SENT to only when every one
// of these holds (verifyTitleBind) — otherwise it stays view-only and says
// why (mirror.Window.AgentBind):
//
//   - the TUI is a client of THIS host's opencode server: not --standalone,
//     not --server (resolve gives such a pane no view at all), and the token
//     reads that server;
//   - the server answered the session list, and its event stream is OPEN, so
//     the list is current — every session event marks it stale and it is
//     read again (AgentBindStream otherwise);
//   - the title is WHOLE ("OC | <title>", not cut with "…"), and exactly one
//     ROOT session carries it: two sessions sharing a title, or a cut title
//     that is the start of two, are no match at all (MatchTitle), and a cut
//     title that is the start of one is refused too (AgentBindTitleCut) —
//     a cut title names only a prefix, which a subagent's child session
//     (absent from the root list) or a session created since can share;
//   - the TUI's own command line names no session, or names that very one
//     (its -s/--session): a TUI started on one session whose title names
//     another is ambiguous (AgentBindArgv).
//
// 🔴 A SEND CAN NEVER REACH ANOTHER SESSION THAN THE PANE SHOWS. The binding
// is re-made at the moment of sending, from the pane's title read NOW
// (sendOpenCode re-resolves the window), and the send is refused unless:
//
//   - the paste names the session muster's window list names for the window
//     (Msg.AgentSession: the one the Rendered view shows) and the pane still
//     resolves to exactly it (PasteRefusalOpenCodeMoved otherwise; a paste
//     naming none — an older muster — is refused for such a pane);
//   - a fresh read of that session through opencode's API (GET
//     /api/session/{id}) finds it a root session whose title is still the
//     pane's (confirmTitleBind).
//
// ⚠ RESIDUAL, NAMED: a child session whose title EQUALS a root session's
// whole title (a subagent's title is "<description> (@<agent> subagent)")
// cannot be told apart by any of the above, since the root list has no
// children. muster binds the root one.

// verifyTitleBind is how a pane whose title matched session m.Session is
// bound (mirror.Window.AgentBind): mirror.AgentBindVerified, or why it is not
// sent to. sessionArg is the session the TUI's command line names ("" none).
func (s *OpenCodeSource) verifyTitleBind(m opencodehost.Match, sessionArg string) string {
	switch {
	case m.Session == "":
		return ""
	case m.Truncated:
		return mirror.AgentBindTitleCut
	case sessionArg != "" && sessionArg != m.Session:
		return mirror.AgentBindArgv
	}
	s.mu.Lock()
	connected := s.connected
	s.mu.Unlock()
	if !connected {
		return mirror.AgentBindStream
	}
	return mirror.AgentBindVerified
}

// confirmTitleBind reads session id through the API NOW and reports why a
// send to a pane titled title (prefix stripped) must not go — it is gone,
// not a root session, or titled otherwise — "" when it may.
func confirmTitleBind(ctx context.Context, cl *opencodehost.Client, id, title string) (why, refusal string) {
	sess, err := cl.GetSession(ctx, id)
	switch {
	case errors.Is(err, opencodehost.ErrSessionNotFound):
		return mirror.PasteRefusedOpenCodeGoneText, mirror.PasteRefusalOpenCodeGone
	case err != nil:
		return "Not sent: muster could not confirm this pane's opencode session with opencode: " + err.Error(), mirror.PasteRefusalOpenCodeUnbound
	case sess.ID != id || sess.ParentID != "" || sess.Title != title:
		return mirror.PasteRefusedOpenCodeMovedText, mirror.PasteRefusalOpenCodeMoved
	}
	return "", ""
}
