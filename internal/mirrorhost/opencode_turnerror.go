package mirrorhost

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/opencodehost"
)

// HOW AN IDLE OPENCODE WINDOW'S LATEST TURN ENDED (mirror.Window.TurnError,
// docs/design/opencode-first-class.md): the opencode twin of turnend.go, read
// from the session's newest messages instead of a transcript — so the send
// queue holds on a run of turns that each failed at once (its circuit
// breaker, internal/sendqueue's holdFor), exactly as it does for Claude Code,
// and the chief's nudge sweep sees the failure.
//
// A turn is a user message and everything after it. It ENDED IN AN ERROR when
// its last "idle" message says outcome "failed", or — with no idle message —
// its last assistant message finished in an error. An interrupted turn (the
// person stopped it) is no error. Kind is the error's own name in snake case
// ("APIError" → "api_error") when the wire carries it, else
// mirror.TurnErrorOther; At is when the failure was written (the idle
// message's time, else the assistant's completion), the server's clock in
// Unix ms. Quick counts the latest turns, this one first, that EACH failed
// within mirror.QuickTurnMS of their prompt with no real reply (no text, no
// tool call) — the breaker's count.
//
// ⚠ NOT DERIVED: TurnErrorLimit (a usage limit). Which opencode errors are a
// provider's usage limit, and the reset they name, were not measured, so an
// opencode failure is never read as one — it holds the queue only through
// the breaker. Nor Rejected/Prompt/PrevAt: the queue marks a refused message
// failed only for a window whose session muster tracks (AgentState.Session,
// Claude Code's), so they would be read by nothing.
//
// One read per session per change, as opencode_outcome.go: remembered against
// the session's change count, a failed read retried after outcomeRetry.

// turnErrorMessages is how many of a session's newest messages are read for
// its latest turns.
const turnErrorMessages = 60

// turnErrorMemo is one session's remembered read.
type turnErrorMemo struct {
	changes uint64
	e       *mirror.TurnError
	failed  time.Time
}

// openCodeTurnError is how the latest turn of msgs (a session's messages,
// oldest first) ended when it ended in an error, nil otherwise.
func openCodeTurnError(msgs []opencodehost.Message) *mirror.TurnError {
	var res *mirror.TurnError
	end := len(msgs)
	for end > 0 {
		// The latest turn still unread: from the last user message before end.
		start := -1
		for i := end - 1; i >= 0; i-- {
			if msgs[i].Type == "user" {
				start = i
				break
			}
		}
		if start < 0 {
			break
		}
		failed, kind, at, real := turnOutcome(msgs[start+1 : end])
		if !failed {
			break
		}
		quick := !real && at > 0 && at-int64(msgs[start].Time.Created) <= mirror.QuickTurnMS
		if res == nil {
			res = &mirror.TurnError{Kind: kind, At: at}
		}
		if !quick || res.Quick >= mirror.MaxTurnErrorQuick {
			break
		}
		res.Quick++
		end = start
	}
	return mirror.CleanTurnError(res)
}

// turnOutcome reads one turn's messages after its prompt: whether it failed,
// the error's kind and time, and whether any real reply (text, a tool call)
// came.
func turnOutcome(turn []opencodehost.Message) (failed bool, kind string, at int64, real bool) {
	var idle, last *opencodehost.Message
	for i := range turn {
		m := &turn[i]
		switch m.Type {
		case "idle":
			idle = m
		case "assistant":
			last = m
			for _, c := range m.Content {
				if (c.Type == "text" && strings.TrimSpace(c.Text) != "") || c.Type == "tool" {
					real = true
				}
			}
		}
	}
	switch {
	case idle != nil:
		if idle.Outcome != "failed" {
			return false, "", 0, real
		}
		raw := idle.Error
		if !hasErr(raw) && last != nil {
			raw = last.Error
		}
		return true, turnErrorKindOf(raw), int64(idle.Time.Created), real
	case last != nil && (last.Time.Completed > 0 || last.Finish != "") && (last.Finish == "error" || hasErr(last.Error)):
		at := int64(last.Time.Completed)
		if at <= 0 {
			at = int64(last.Time.Created)
		}
		return true, turnErrorKindOf(last.Error), at, real
	}
	return false, "", 0, real
}

// hasErr reports whether raw is a non-null error.
func hasErr(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null"
}

// turnErrorKindOf is an opencode error's kind: its name (name, _tag or type)
// in snake case when the wire carries that (mirror.ValidTurnErrorKind), else
// mirror.TurnErrorOther.
func turnErrorKindOf(raw json.RawMessage) string {
	var e struct {
		Name string `json:"name"`
		Tag  string `json:"_tag"`
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &e) == nil {
		for _, n := range []string{e.Name, e.Tag, e.Type} {
			if k := snakeCase(n); mirror.ValidTurnErrorKind(k) && k != mirror.TurnErrorLimit {
				return k
			}
		}
	}
	return mirror.TurnErrorOther
}

// snakeCase is an identifier in snake case: "APIError" → "api_error",
// "ProviderAuthError" → "provider_auth_error"; any character outside
// [A-Za-z0-9] is a separator.
func snakeCase(s string) string {
	rs := []rune(s)
	var b strings.Builder
	sep := func() {
		if b.Len() > 0 && !strings.HasSuffix(b.String(), "_") {
			b.WriteByte('_')
		}
	}
	for i, r := range rs {
		switch {
		case r < 0x80 && unicode.IsUpper(r):
			lowerBefore := i > 0 && (unicode.IsLower(rs[i-1]) || unicode.IsDigit(rs[i-1]))
			acronymEnd := i > 0 && unicode.IsUpper(rs[i-1]) && i+1 < len(rs) && unicode.IsLower(rs[i+1])
			if lowerBefore || acronymEnd {
				sep()
			}
			b.WriteRune(unicode.ToLower(r))
		case r < 0x80 && (unicode.IsLower(r) || unicode.IsDigit(r)):
			b.WriteRune(r)
		default:
			sep()
		}
	}
	return strings.Trim(b.String(), "_")
}

// attachTurnErrors sets each IDLE opencode window's TurnError from its
// session's newest messages (a copy of its AgentPane: the backend's value is
// not changed), read once per change of the session; it forgets every
// session no idle window shows.
func (s *OpenCodeSource) attachTurnErrors(windows []WindowInfo) {
	listed := map[string]bool{}
	for i := range windows {
		a := windows[i].Agent
		if a == nil || a.Runtime != mirror.RuntimeOpenCode || a.Gen == "" || a.Status != mirror.StatusIdle || a.TurnError != nil {
			continue
		}
		listed[a.Gen] = true
		if e := s.turnErrorOf(a.Gen); e != nil {
			cp := *a
			cp.TurnError = e
			windows[i].Agent = &cp
		}
	}
	s.mu.Lock()
	for id := range s.turnErrs {
		if !listed[id] {
			delete(s.turnErrs, id)
		}
	}
	s.mu.Unlock()
}

// turnErrorOf is session id's latest turn error, read once per change.
func (s *OpenCodeSource) turnErrorOf(id string) *mirror.TurnError {
	changes := s.changed(id)
	s.mu.Lock()
	m, seen := s.turnErrs[id]
	s.mu.Unlock()
	switch {
	case seen && m.failed.IsZero() && m.changes == changes:
		return m.e
	case seen && !m.failed.IsZero() && s.now().Sub(m.failed) < outcomeRetry:
		return nil
	}
	cl, err := s.client()
	var msgs []opencodehost.Message
	if err == nil {
		ctx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
		msgs, err = cl.Messages(ctx, id, turnErrorMessages)
		cancel()
	}
	m = turnErrorMemo{changes: changes}
	if err != nil {
		m.failed = s.now()
	} else {
		m.e = openCodeTurnError(msgs)
	}
	s.mu.Lock()
	if s.turnErrs == nil {
		s.turnErrs = map[string]turnErrorMemo{}
	}
	s.turnErrs[id] = m
	s.mu.Unlock()
	return m.e
}
