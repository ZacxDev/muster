package mirrorhost

import (
	"bytes"
	"io"
	"os"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/mirror"
)

// HOW AN IDLE CLAUDE WINDOW'S LATEST TURN ENDED (mirror.Window.TurnError),
// read here, on the host, from the session's own transcript: whether it ended
// in a record Claude Code wrote ITSELF to say it got no answer — a usage
// limit, "Not logged in", an API error (cctranscript.ErrorEndOf) — and, if so,
// whether the prompt it answered got no real reply at all (TurnError.Prompt)
// and how many such turns in a row each failed at once (TurnError.Quick).
//
// The turn is read BACKWARDS from the transcript's end, main conversation only
// (a subagent's records are not the session's turn):
//
//   - a synthetic record with no error flag ("No response requested.", the
//     reply to a resumed session's meta prompt), a meta or compaction record,
//     and every non-message record are passed over;
//   - the newest message decides: a real assistant record (any model's, a tool
//     call included) or a prompt means the latest turn did NOT end in an
//     error, and the window carries none;
//   - an error record starts the turn; walking back to its prompt, a real
//     assistant record or a tool result on the way means the turn did work
//     first (the limit hit mid-turn), so its prompt WAS processed and is not
//     named;
//   - a turn whose prompt got only the error, within mirror.QuickTurnMS,
//     counts toward Quick, and the walk goes on to the turn before it; the
//     first turn that is not one ends the count.
//
// 🔴 CHEAP, like lastmsg.go: only for an IDLE Claude window, at most
// turnEndMaxScan bytes from the end, and a transcript is re-read only when its
// size or modification time moved.

const turnEndMaxScan = 1 << 20

// TurnEndOf is how the latest turn in transcript r (of the given size) ended,
// as the file header says; nil when it did not end in an error, or r cannot
// be read.
func TurnEndOf(r io.ReaderAt, size int64) *mirror.TurnError {
	s := turnScan{}
	eachLineBackward(r, size, turnEndMaxScan, s.line)
	return s.result()
}

// turnScan is the backward walk's state.
type turnScan struct {
	res *mirror.TurnError
	// inTurn: an error record was read and its prompt not yet; worked: a real
	// reply or a tool result was read since.
	inTurn, worked bool
	// errAt is that error record's time (host clock, ms).
	errAt int64
	// counted: the newest error turn's prompt was read.
	counted bool
	// done: the walk has its answer.
	done bool
}

// line reads one record, newest first; false ends the walk.
func (s *turnScan) line(line []byte) bool {
	if s.done {
		return false
	}
	if !bytes.Contains(line, []byte(`"assistant"`)) && !bytes.Contains(line, []byte(`"user"`)) {
		return true // a cheap filter: no other record can count
	}
	rec, ok := cctranscript.DecodeLine(line)
	if !ok || rec.IsSidechain {
		return true
	}
	switch rec.Type {
	case "assistant":
		if e, ok := rec.ErrorEndOf(); ok {
			if s.inTurn {
				// A second error before the first one's prompt: the CLI
				// retried within the turn. It is still the same turn.
				return true
			}
			s.inTurn, s.worked, s.errAt = true, false, rec.Time()
			if s.res == nil {
				s.res = &mirror.TurnError{Kind: turnErrorKind(e), At: s.errAt}
				if e.Limit && !e.ResetsAt.IsZero() {
					s.res.ResetsAt = e.ResetsAt.UnixMilli()
				}
			}
			return true
		}
		if rec.Message.Model == cctranscript.SyntheticModel {
			return true
		}
		// A real reply.
		if s.inTurn {
			s.worked = true
			return true
		}
		s.done = true
		return false
	case "user":
		if rec.IsMeta || rec.IsCompactSummary {
			return true
		}
		text, isPrompt := rec.PromptText()
		if !isPrompt {
			if s.inTurn {
				s.worked = true // a tool ran
			}
			return true
		}
		if !s.inTurn {
			// The newest message is a prompt (no turn has ended since), or the
			// turn before the counted ones was not answered by an error.
			s.done = true
			return false
		}
		s.inTurn = false
		if !s.counted && !s.worked {
			s.res.Prompt = mirror.PromptDigest(text)
		}
		s.counted = true
		at := rec.Time()
		quick := !s.worked && at > 0 && s.errAt >= at && s.errAt-at <= mirror.QuickTurnMS
		if !quick || s.res.Quick >= mirror.MaxTurnErrorQuick {
			s.done = true
			return false
		}
		s.res.Quick++
		return true
	}
	return true
}

func (s *turnScan) result() *mirror.TurnError { return mirror.CleanTurnError(s.res) }

// turnErrorKind is an error record's kind on the wire.
func turnErrorKind(e cctranscript.ErrorEnd) string {
	switch {
	case e.Limit:
		return mirror.TurnErrorLimit
	case mirror.ValidTurnErrorKind(e.Code):
		return e.Code
	}
	return mirror.TurnErrorOther
}

// eachLineBackward calls fn with each whole line of r (of the given size),
// last first, reading at most max bytes from the end in lastMsgChunk pieces,
// until fn returns false. A read error ends it.
func eachLineBackward(r io.ReaderAt, size, max int64, fn func([]byte) bool) {
	var carry []byte
	end := size
	for end > 0 && size-end < max {
		start := end - lastMsgChunk
		if start < 0 {
			start = 0
		}
		buf := make([]byte, end-start, end-start+int64(len(carry)))
		if _, err := r.ReadAt(buf, start); err != nil && err != io.EOF {
			return
		}
		buf = append(buf, carry...)
		lines := bytes.Split(buf, []byte{'\n'})
		first := 0
		if start > 0 {
			carry = append([]byte(nil), lines[0]...)
			first = 1
		} else {
			carry = nil
		}
		for i := len(lines) - 1; i >= first; i-- {
			if l := bytes.TrimSpace(lines[i]); len(l) > 0 && !fn(l) {
				return
			}
		}
		end = start
	}
}

// turnEndReader is TurnEndOf for transcripts, re-reading one only when its
// size or modification time moved. Safe for concurrent use; the zero value is
// ready.
type turnEndReader struct {
	mu    sync.Mutex
	cache map[string]turnEndEntry
}

type turnEndEntry struct {
	size int64
	mod  time.Time
	e    *mirror.TurnError
}

func (r *turnEndReader) of(path string) *mirror.TurnError {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	r.mu.Lock()
	if e, ok := r.cache[path]; ok && e.size == st.Size() && e.mod.Equal(st.ModTime()) {
		r.mu.Unlock()
		return e.e
	}
	r.mu.Unlock()
	e := TurnEndOf(f, st.Size())
	r.mu.Lock()
	if r.cache == nil {
		r.cache = map[string]turnEndEntry{}
	}
	r.cache[path] = turnEndEntry{size: st.Size(), mod: st.ModTime(), e: e}
	r.mu.Unlock()
	return e
}

// attach sets each IDLE Claude window's TurnError from its transcript (a copy
// of its ClaudePane: the backend's value is not changed), and drops the cache
// of every transcript it did not read. A pane that already carries one keeps
// it.
func (r *turnEndReader) attach(windows []WindowInfo) {
	seen := map[string]bool{}
	for i := range windows {
		c := windows[i].Claude
		if c == nil || c.Status != mirror.StatusIdle || c.Transcript == "" || c.TurnError != nil {
			continue
		}
		seen[c.Transcript] = true
		cp := *c
		cp.TurnError = r.of(c.Transcript)
		windows[i].Claude = &cp
	}
	r.mu.Lock()
	for p := range r.cache {
		if !seen[p] {
			delete(r.cache, p)
		}
	}
	r.mu.Unlock()
}
