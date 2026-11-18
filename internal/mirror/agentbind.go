package mirror

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// A HAND-OPENED OPENCODE PANE (docs/design/opencode-first-class.md, "Panes
// the launcher did not start"): a pane whose TUI was not started by
// `muster-mirror opencode` has no launcher record, so its session is found
// from its title. Its host verifies that binding through opencode's own API
// (mirrorhost's verifyTitleBind) and says how it came out in
// Window.AgentBind, a closed code:
//
//   - AgentBindVerified: the binding is unambiguous and verified — muster
//     sends to it, and the paste NAMES the session (Msg.AgentSession), which
//     the host must find the pane still showing at the moment it posts;
//   - any other code: why muster does not send to it (AgentBindWords).
//
// A launcher-bound pane is AgentSend and carries no code.

// The AgentBind codes.
const (
	// AgentBindVerified: the host sends to the window's session, to a paste
	// naming it.
	AgentBindVerified = "verified"
	// AgentBindTitleShared: another session — a subagent's child session —
	// carries the pane's title too (exactly, or as the start of a cut title),
	// and a TUI can show a child session.
	AgentBindTitleShared = "title-shared"
	// AgentBindArgv: the TUI was started on one session (its -s/--session)
	// and its title names another.
	AgentBindArgv = "argv-session"
	// AgentBindListCut: the server holds more sessions than muster reads, so
	// no title can be shown unique against them.
	AgentBindListCut = "list-cut"
	// AgentBindStream: the opencode server's event stream is not open, so the
	// session list the title was matched against may be stale.
	AgentBindStream = "stream-down"
)

// agentBindPattern is an AgentBind code as the wire carries one: a code this
// muster does not know (a newer host's) is kept, and spoken of generically.
var agentBindPattern = regexp.MustCompile(`^[a-z][a-z-]{0,23}$`)

// cleanAgentBind is w's AgentBind as the wire carries it: "" unless w is a
// window of a gated-send runtime (gated) that names its session and is not
// launcher-bound (AgentSend), and the code is well-formed.
func cleanAgentBind(w Window, gated bool) string {
	if !gated || w.AgentSend || w.AgentSession == "" || !agentBindPattern.MatchString(w.AgentBind) {
		return ""
	}
	return w.AgentBind
}

// agentSendsTo is THE rule for whether a host sends to a window of a
// gated-send runtime: launcher-bound (AgentSend), or bound by verification
// (AgentBindVerified). The hub (gatedSendCheck) and the page (SendBlocked)
// both ask it.
func agentSendsTo(agentSend bool, bind string) bool {
	return agentSend || bind == AgentBindVerified
}

// AgentBindWords is the refusal of a send to a window of a gated-send
// runtime its host does not send to, in the person's words: the code's own
// reason, else (no code: an older host; a code this muster does not know) the
// launcher's.
func AgentBindWords(code string) string {
	switch code {
	case AgentBindTitleShared:
		return PasteRefusedOpenCodeTitleSharedText
	case AgentBindArgv:
		return PasteRefusedOpenCodeArgvText
	case AgentBindStream:
		return PasteRefusedOpenCodeStreamText
	case AgentBindListCut:
		return PasteRefusedOpenCodeListCutText
	}
	return PasteRefusedOpenCodeUnboundText
}

// The refusals of a hand-opened opencode pane muster does not send to.
const (
	PasteRefusedOpenCodeTitleSharedText = "Not sent: a subagent's opencode session has the same title as this pane " +
		"(or one that starts the same way), so muster can't be sure which session it shows. Type it in the terminal, " +
		"or start it with `muster-mirror opencode`."
	PasteRefusedOpenCodeArgvText = "Not sent: this opencode was started on one session (its -s) but its title names " +
		"another, so muster can't be sure which it shows. Type it in the terminal, or start it with `muster-mirror opencode`."
	PasteRefusedOpenCodeListCutText = "Not sent: the opencode server holds more sessions than muster reads, so it " +
		"can't be sure no other session shares this pane's title. Type it in the terminal, or start it with " +
		"`muster-mirror opencode`."
	PasteRefusedOpenCodeStreamText = "Not sent: muster can't confirm which session this opencode shows right now, " +
		"because the opencode server's event stream is not answering. Try again in a moment."
	// PasteRefusedOpenCodeMovedText: the window shows another session now
	// than the one the message was written in view of — the page's, or the
	// one a queued message was stored with (PasteRefusalOpenCodeMoved). The
	// hub refuses it against its list, and the host against the pane.
	PasteRefusedOpenCodeMovedText = "Not sent: this opencode pane shows another session now than the one this " +
		"message was written for, so nothing was sent. To send it there, write it again while viewing the session shown now."
	// PasteRefusedOpenCodeUnnamedText: a message for a hand-opened pane that
	// names no session (a page or a queued message from before muster
	// recorded one), which muster cannot tell is meant for the session shown.
	PasteRefusedOpenCodeUnnamedText = "Not sent: muster can't tell which opencode session this message was written " +
		"for (it was queued or typed on an older page), so nothing was sent. Reload the page, then write it again."
)

// AgentContext is an agent's context fill (Msg.Context): Tokens in context
// now, of a window of Limit tokens (0: unknown), on model Model ("" unknown).
// opencode's is opencodehost.ContextOf: the newest step's input + cache read
// + cache write — the quantity a Claude Code view's readout counts.
type AgentContext struct {
	Tokens int64  `json:"tokens"`
	Limit  int64  `json:"limit,omitempty"`
	Model  string `json:"model,omitempty"`
}

// MaxContextModel bounds AgentContext.Model, in bytes.
const MaxContextModel = 96

// CleanAgentContext is c as muster keeps it: nil for none or no tokens, no
// negative limit, the model stripped of control characters and cut to
// MaxContextModel bytes on a rune boundary.
func CleanAgentContext(c *AgentContext) *AgentContext {
	if c == nil || c.Tokens <= 0 {
		return nil
	}
	out := *c
	out.Limit = max(out.Limit, 0)
	out.Model = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, out.Model))
	for len(out.Model) > MaxContextModel {
		_, size := utf8.DecodeLastRuneInString(out.Model)
		out.Model = out.Model[:len(out.Model)-size]
	}
	return &out
}
