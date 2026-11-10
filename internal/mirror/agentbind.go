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
	// AgentBindTitleCut: the pane's title is cut ("…"), so it names only the
	// start of a session title.
	AgentBindTitleCut = "title-cut"
	// AgentBindArgv: the TUI was started on one session (its -s/--session)
	// and its title names another.
	AgentBindArgv = "argv-session"
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
	case AgentBindTitleCut:
		return PasteRefusedOpenCodeTitleCutText
	case AgentBindArgv:
		return PasteRefusedOpenCodeArgvText
	case AgentBindStream:
		return PasteRefusedOpenCodeStreamText
	}
	return PasteRefusedOpenCodeUnboundText
}

// The refusals of a hand-opened opencode pane muster does not send to.
const (
	PasteRefusedOpenCodeTitleCutText = "Not sent: this opencode pane's title is cut short, so it names only the start of a " +
		"session's title, and muster can't be sure which session it shows. Type it in the terminal, or start it with " +
		"`muster-mirror opencode`."
	PasteRefusedOpenCodeArgvText = "Not sent: this opencode was started on one session (its -s) but its title names " +
		"another, so muster can't be sure which it shows. Type it in the terminal, or start it with `muster-mirror opencode`."
	PasteRefusedOpenCodeStreamText = "Not sent: muster can't confirm which session this opencode shows right now, " +
		"because the opencode server's event stream is not answering. Try again in a moment."
	// PasteRefusedOpenCodeMovedText: the pane resolves to another session
	// than the one muster named (PasteRefusalOpenCodeMoved).
	PasteRefusedOpenCodeMovedText = "Not sent: this opencode pane shows another session now than the one muster " +
		"showed you, so nothing was sent. Check the view, then send it again."
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
