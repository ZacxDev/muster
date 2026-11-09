package mirrorhost

import (
	"context"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/opencodehost"
)

// AN opencode DISPATCH'S FINAL REPLY (docs/design/chief-dispatch.md 3.7, "An
// opencode dispatch's outcome"; the wire is internal/mirror/outcomes.go). A
// host-spawned opencode agent has no muster tool, so muster cannot be TOLD it
// finished; this host reads it instead. For every window it lists that the
// chief started (its @muster-dispatch tag), running an opencode session the
// launcher bound (so the session is known, not guessed from a title), and
// IDLE, the session's newest messages are read and the final reply's last
// lines and its message id are sent (TypeOutcomes). The server decides done or
// waiting from them; nothing is decided here.
//
// One read per session per change: the read is remembered against the
// session's change count (OpenCodeSource.changed, which every event naming it
// moves), so an idle window costs nothing on later refreshes. A failed read
// sends nothing for that window and is retried after outcomeRetry.

// outcomeMessages is how many of a session's newest messages are read for
// its final reply.
const outcomeMessages = 30

// outcomeRetry bounds how often a failed read of one session is retried.
const outcomeRetry = 30 * time.Second

// outcomeMemo is one session's remembered read.
type outcomeMemo struct {
	changes uint64
	out     mirror.WindowOutcome
	ok      bool
	failed  time.Time
}

// finalReply is a session's final reply from msgs (oldest first): the id of
// its newest assistant message and the text of the newest assistant message
// that has any. ok is false when there is nothing to judge yet — no assistant
// message, one newer prompt of the person's not answered yet, or the newest
// assistant message still streaming.
func finalReply(msgs []opencodehost.Message) (mark, text string, ok bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		switch m.Type {
		case "user":
			if mark == "" {
				return "", "", false // a prompt not answered yet
			}
			return mark, text, true
		case "assistant":
			if mark == "" {
				if m.Time.Completed <= 0 && m.Finish == "" {
					return "", "", false // still streaming
				}
				mark = m.ID
			}
			if t := assistantText(m); t != "" {
				return mark, t, true
			}
		}
	}
	return mark, text, mark != ""
}

// assistantText is an assistant message's text items, joined by newlines.
func assistantText(m opencodehost.Message) string {
	var parts []string
	for _, c := range m.Content {
		if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// dispatchOutcomes is the final reply of each of windows that the chief
// started, runs a launcher-bound opencode session and is idle, by window id
// (what TypeOutcomes carries, before mirror.NormalizeOutcomes).
func (s *OpenCodeSource) dispatchOutcomes(windows []WindowInfo) map[string]mirror.WindowOutcome {
	var out map[string]mirror.WindowOutcome
	listed := map[string]bool{}
	defer func() {
		// A session no dispatch window shows any more is forgotten.
		s.mu.Lock()
		for id := range s.replies {
			if !listed[id] {
				delete(s.replies, id)
			}
		}
		s.mu.Unlock()
	}()
	for _, w := range windows {
		a := w.Agent
		if w.Dispatch <= 0 || a == nil || a.Runtime != mirror.RuntimeOpenCode || !a.Send || a.Gen == "" {
			continue
		}
		listed[a.Gen] = true
		if a.Status != mirror.StatusIdle {
			continue
		}
		o, ok := s.replyOf(a.Gen)
		if !ok {
			continue
		}
		if out == nil {
			out = map[string]mirror.WindowOutcome{}
		}
		out[w.ID] = o
	}
	return out
}

// replyOf is session id's final reply, read once per change of the session.
func (s *OpenCodeSource) replyOf(id string) (mirror.WindowOutcome, bool) {
	changes := s.changed(id)
	s.mu.Lock()
	m, seen := s.replies[id]
	s.mu.Unlock()
	switch {
	case seen && m.failed.IsZero() && m.changes == changes:
		return m.out, m.ok
	case seen && !m.failed.IsZero() && s.now().Sub(m.failed) < outcomeRetry:
		return mirror.WindowOutcome{}, false
	}
	cl, err := s.client()
	var msgs []opencodehost.Message
	if err == nil {
		ctx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
		msgs, err = cl.Messages(ctx, id, outcomeMessages)
		cancel()
	}
	m = outcomeMemo{changes: changes}
	if err != nil {
		m.failed = s.now()
	} else if mark, text, ok := finalReply(msgs); ok && mirror.ValidAgentSession(mark) {
		m.out, m.ok = mirror.WindowOutcome{Mark: mark, Lines: mirror.OutcomeLines(text)}, true
	}
	s.mu.Lock()
	if s.replies == nil {
		s.replies = map[string]outcomeMemo{}
	}
	s.replies[id] = m
	s.mu.Unlock()
	return m.out, m.ok
}
