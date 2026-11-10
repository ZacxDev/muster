package mirrorhost

import (
	"context"
	"errors"
	"strings"

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
//   - exactly ONE session the server knows — roots AND children (a
//     subagent's, which the title match's root list leaves out) — carries the
//     title: the whole title, or for a title cut with "…" every title that
//     starts with what precedes it. Two root sessions sharing it are no match
//     at all (MatchTitle: no view); a CHILD session sharing it — exactly, or
//     as the start of a cut title — is AgentBindTitleShared: a TUI can show
//     a subagent's session, whose title the pane then carries;
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
// verifyTitleBind is how a pane whose title matched session m.Session is
// bound (mirror.Window.AgentBind): mirror.AgentBindVerified, or why it is not
// sent to. sessionArg is the session the TUI's command line names ("" none).
func (s *OpenCodeSource) verifyTitleBind(m opencodehost.Match, sessionArg string, sessions []opencodehost.Session) string {
	switch {
	case m.Session == "":
		return ""
	case sessionArg != "" && sessionArg != m.Session:
		return mirror.AgentBindArgv
	}
	for _, o := range sessions {
		if o.ID != m.Session && titleCarries(m.Title, o.Title) {
			return mirror.AgentBindTitleShared
		}
	}
	s.mu.Lock()
	connected := s.connected
	s.mu.Unlock()
	if !connected {
		return mirror.AgentBindStream
	}
	return mirror.AgentBindVerified
}

// titleCarries reports whether a pane whose title is body (its "OC | "
// stripped) can be showing a session titled title: the same title, or — for
// a body cut with "…" — a title starting with what precedes the "…"
// (opencodehost.MatchTitle's rule).
func titleCarries(body, title string) bool {
	if title == "" {
		return false
	}
	if want, cut := strings.CutSuffix(body, "…"); cut {
		return strings.TrimSpace(want) != "" && strings.HasPrefix(title, want)
	}
	return title == body
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
	case sess.ID != id || sess.ParentID != "" || !titleCarries(title, sess.Title):
		return mirror.PasteRefusedOpenCodeMovedText, mirror.PasteRefusalOpenCodeMoved
	}
	return "", ""
}
