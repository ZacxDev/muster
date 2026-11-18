package mirror

import (
	"slices"
	"strings"
	"sync/atomic"
	"unicode/utf8"
)

// HOW A HOST-SPAWNED opencode AGENT'S TURN ENDED (docs/design/chief-dispatch.md
// 3.7, "An opencode dispatch's outcome"): the host reads, for each window it
// lists that the chief started (Window.Dispatch) running opencode and IDLE, the
// final reply of that window's opencode session, and sends its last lines. The
// server decides from them whether the agent said it finished (a last line in
// the done grammar, internal/agentoutcome.ParseDone) or is waiting on its
// owner. opencode is given NO muster tool (it runs without asking), so this
// observation is its only way to report an outcome.
//
// 🔴 ITS OWN MESSAGE (TypeOutcomes), NOT A Window FIELD, as TypeWork is: the
// window list's worst case already fills 98% of MaxFrame. The WHOLE map of
// window id → outcome, sent after the window list whenever it changed; an
// empty map clears it. Both halves restrict it to what the listing shows
// (NormalizeOutcomes): an outcome rides only an idle opencode window that the
// chief started.
//
// 🔴 NO VERSION BUMP, BACKWARD-COMPATIBLE BOTH WAYS: an older muster has no
// case for the type and drops it; an older host never sends one, and its
// opencode dispatches stay "Working" until their window closes, as before.

// TypeOutcomes is the host → muster message carrying Msg.Outcomes.
const TypeOutcomes = "outcomes"

// The WindowOutcome bounds: how many lines of a reply are sent, each line's
// bytes, and how many windows of one map carry one
// (TestTheLargestOutcomesMessageFitsOneFrame measures the worst case).
const (
	MaxOutcomeLines   = 3
	MaxOutcomeLine    = 200
	MaxOutcomeWindows = 32
)

// WindowOutcome is the final reply of an idle opencode window the chief
// started: Mark names it (the opencode message id of the session's newest
// assistant message, so a new turn is a new mark), Lines are its last
// non-empty lines, oldest first, at most MaxOutcomeLines — none when the turn
// ended with no reply text.
type WindowOutcome struct {
	Mark  string   `json:"mark"`
	Lines []string `json:"lines,omitempty"`
}

// OutcomeLines is text's last MaxOutcomeLines non-empty lines, oldest first,
// each with control characters made spaces, trimmed and cut to MaxOutcomeLine
// bytes: what a WindowOutcome carries. The host builds Lines with it and both
// halves apply it again (NormalizeOutcomes).
func OutcomeLines(text string) []string {
	var out []string
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0 && len(out) < MaxOutcomeLines; i-- {
		if t := cleanBounded(lines[i], MaxOutcomeLine); t != "" {
			out = append([]string{t}, out...)
		}
	}
	return out
}

// NormalizeOutcomes is in restricted to windows (a host cannot report an
// outcome for a window it does not list): only a window the chief started
// (Dispatch > 0) that runs opencode, names its session and is IDLE keeps one,
// with a mark ValidAgentSession accepts and its lines through OutcomeLines;
// at most MaxOutcomeWindows, in listing order; nil when nothing is left. Both
// halves apply it. windows must be NormalizeWindows' output.
func NormalizeOutcomes(in map[string]WindowOutcome, windows []Window) map[string]WindowOutcome {
	if len(in) == 0 {
		return nil
	}
	var out map[string]WindowOutcome
	for _, w := range windows {
		if len(out) >= MaxOutcomeWindows {
			break
		}
		v, ok := in[w.ID]
		if !ok || w.Dispatch <= 0 || w.AgentRuntime() != RuntimeOpenCode || w.AgentSession == "" || w.Status != StatusIdle ||
			!ValidAgentSession(v.Mark) {
			continue
		}
		v.Lines = OutcomeLines(strings.Join(v.Lines, "\n"))
		if out == nil {
			out = map[string]WindowOutcome{}
		}
		out[w.ID] = v
	}
	return out
}

// OutcomesEqual reports whether two outcome maps say the same.
func OutcomesEqual(a, b map[string]WindowOutcome) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		o, ok := b[k]
		if !ok || o.Mark != v.Mark || !slices.Equal(o.Lines, v.Lines) {
			return false
		}
	}
	return true
}

// cleanBounded is s with control characters made spaces, trimmed, and cut to
// max bytes on a rune boundary (cleanNote's rule at another bound).
func cleanBounded(s string, max int) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s))
	for len(s) > max {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

// outcomeSeq numbers every outcomes snapshot the hub hands its hook, across
// every connection: the hook runs on its own goroutine, so a snapshot can be
// applied after a newer one, and the number is how the receiver drops it.
var outcomeSeq atomic.Uint64

// SetOutcomesNotify installs the hook called (on its own goroutine) with a
// host's owner and name, a snapshot number that grows with every call, and its
// current windows and outcomes, after every listing and every outcomes message
// the hub applies: the host-dispatch outcome reconciler's input. A receiver
// drops a snapshot older than one it has applied.
func (h *Hub) SetOutcomesNotify(fn func(owner int64, host string, seq uint64, windows []Window, outcomes map[string]WindowOutcome)) {
	h.mu.Lock()
	h.outcomesNotify = fn
	h.mu.Unlock()
}

// outcomesApplied calls the outcomes hook with hc's current windows and its
// outcomes re-restricted to them (a window that went busy carries none).
func (h *Hub) outcomesApplied(hc *hostConn) {
	h.mu.Lock()
	fn := h.outcomesNotify
	h.mu.Unlock()
	if fn == nil {
		return
	}
	hc.mu.Lock()
	hc.outcomes = NormalizeOutcomes(hc.outcomes, hc.windows)
	ws := append([]Window(nil), hc.windows...)
	oc := make(map[string]WindowOutcome, len(hc.outcomes))
	for k, v := range hc.outcomes {
		v.Lines = append([]string(nil), v.Lines...)
		oc[k] = v
	}
	seq := outcomeSeq.Add(1)
	hc.mu.Unlock()
	go fn(hc.key.owner, hc.key.host, seq, ws, oc)
}
