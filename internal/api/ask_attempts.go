package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ZacxDev/muster/internal/askattempt"
	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/mirror"
)

// ---------------------------------------------------------------------------
// THE ANSWER-ATTEMPT LOG's server half (internal/askattempt, migration 0113;
// docs/mirroring.md#answer-attempts): one row per Submit or Dismiss of a
// question card, from every door — the card (mirror_ask.go: an answer, a
// dismissal, "Send as a message"; and an opencode card,
// mirror_opencode_ask.go), the Attention list's opencode answer
// (attention_opencode.go) and the card in the phone's shell (the card's own
// door field) — and the Answers page (answers_page.go).
//
// 🔴 EVERY EXIT OF A DOOR RECORDS EXACTLY ONE ROW. A door begins an attempt
// (beginAttempt) and defers its done; every return path names its reason
// (attempt.end) first, from askattempt.Reasons. A path that forgets still
// records — reason "error", with a log line saying so — so a forgotten path
// is a visible wrong row, never a missing one (the ledger test,
// TestEveryAnswerPathRecordsOneRow, pins which reason each path records).
//
// 🔴 CAPTURE NEVER FAILS OR DELAYS THE ANSWER. done (the door's defer) hands
// the row to a goroutine and returns, so the response does not wait for the
// INSERT. The write runs on a detached context bounded by
// askAttemptWriteTimeout, under the request's own principal (row-level
// security); at most askAttemptMaxWrites run at once, and past that a row is
// dropped with a log line — as is a failed write. Tests wait for the writes
// with flushAskAttempts.
// ---------------------------------------------------------------------------

// askAttemptWriteTimeout bounds one row's write.
const askAttemptWriteTimeout = 3 * time.Second

// askDoorField is the card form's field naming its door ("phone" in the
// Android shell; anything else is the card).
const askDoorField = "door"

// attempt is one Submit or Dismiss being recorded.
type attempt struct {
	s     *Server
	ctx   context.Context
	start time.Time
	a     askattempt.Attempt
	ended bool
}

// beginAttempt starts recording one attempt at door of r (the request's own
// principal records it), by action and method.
func (s *Server) beginAttempt(r *http.Request, door, action, method string) *attempt {
	return &attempt{s: s, ctx: r.Context(), start: time.Now(), a: askattempt.Attempt{
		Runtime: askattempt.RuntimeClaudeCode, Door: door, Action: action, Method: method}}
}

// cardDoor is the door a card's form names (askDoorField).
func cardDoor(v url.Values) string {
	if v.Get(askDoorField) == askattempt.DoorPhone {
		return askattempt.DoorPhone
	}
	return askattempt.DoorCard
}

// where records the window the attempt is on, and its card.
func (t *attempt) where(host, session, window, card string) {
	t.a.Host, t.a.Session, t.a.Window, t.a.CardID = host, session, window, card
}

// shape records the question set's shape (never its text); choices, when
// known, say whether an answer of one's own was given.
func (t *attempt) shape(qs []askq.Question, choices []askq.Choice) {
	t.a.Shape = askShape(qs, choices)
}

// askShape is qs's shape with choices' use of "Other".
func askShape(qs []askq.Question, choices []askq.Choice) askattempt.Shape {
	sh := askattempt.Shape{Options: make([]int, len(qs)), Multi: make([]bool, len(qs))}
	for i, q := range qs {
		sh.Options[i], sh.Multi[i] = len(q.Options), q.MultiSelect
	}
	for _, c := range choices {
		if strings.TrimSpace(c.Other) != "" {
			sh.Other = true
		}
	}
	return sh
}

// end names how the attempt ended (a key of askattempt.Reasons).
func (t *attempt) end(reason string) {
	if !t.ended {
		t.a.Reason, t.ended = reason, true
	}
}

// failed ends the attempt with an AnswerWindow's (or CancelAsk's) err: its
// reason, how many keys reached the picker, and the pane the host read.
func (t *attempt) failed(err error) {
	t.a.Keys = mirror.AskKeys(err)
	t.a.Snapshot = mirror.AskScreen(err)
	t.end(askTypingResult(err))
}

// done writes the row: deferred by every door.
func (t *attempt) done() {
	if t == nil || t.s.ext.AskAttempts == nil {
		return
	}
	if !t.ended {
		t.s.logger.Printf("🔴 answer attempts: a %s at the %s door ended without a reason; recorded as \"error\"", t.a.Action, t.a.Door)
		t.a.Reason = "error"
	}
	t.a.Duration = time.Since(t.start)
	w := &t.s.askAttemptWrites
	if w.n.Add(1) > askAttemptMaxWrites {
		w.n.Add(-1)
		t.s.logger.Printf("answer attempts: %d writes already running; a %s (%s) was not recorded", askAttemptMaxWrites, t.a.Action, t.a.Reason)
		return
	}
	w.wg.Add(1)
	a, store, ctx := t.a, t.s.ext.AskAttempts, context.WithoutCancel(t.ctx)
	safeGo(t.s.logger, "answer attempts: record a row", func() {
		defer w.wg.Done()
		defer w.n.Add(-1)
		ctx, cancel := context.WithTimeout(ctx, askAttemptWriteTimeout)
		defer cancel()
		if _, err := store.Record(ctx, a); err != nil {
			t.s.logger.Printf("answer attempts: record a %s (%s): %v", a.Action, a.Reason, err)
		}
	})
}

// askAttemptMaxWrites bounds the row writes running at once.
const askAttemptMaxWrites = 64

// askAttemptWriters is the rows being written (done).
type askAttemptWriters struct {
	wg sync.WaitGroup
	n  atomic.Int32
}

// flushAskAttempts waits until every row handed to a write so far is written
// (or failed): the tests' hook.
func (s *Server) flushAskAttempts() { s.askAttemptWrites.wg.Wait() }

// askTypingResult is an AnswerWindow outcome's reason — the
// muster_mirror_ask_typing_total label, and the answer-attempt log's reason
// (askattempt.Reasons) — and a CancelAsk's.
func askTypingResult(err error) string {
	switch {
	case err == nil:
		return "typed"
	case errors.Is(err, mirror.ErrStepUpRequired):
		return "step-up"
	case errors.Is(err, mirror.ErrAskNotSent):
		return "not-sent"
	case errors.Is(err, mirror.ErrAskNoPicker):
		return "no-picker"
	case errors.Is(err, mirror.ErrAskUnreadable):
		return "unreadable"
	case errors.Is(err, mirror.ErrAskTouched):
		return "touched"
	case errors.Is(err, mirror.ErrAskLost):
		return "lost"
	case errors.Is(err, mirror.ErrAskUnrecognised):
		return "unrecognised"
	case errors.Is(err, mirror.ErrAskBusy):
		return "busy"
	case errors.Is(err, mirror.ErrAskUnsupported), errors.Is(err, mirror.ErrAskCancelUnsupported):
		return "unsupported"
	case errors.Is(err, mirror.ErrAskTimeout):
		return "timeout"
	}
	return "error"
}

// openCodeAnswerResult is an opencode relay's (AnswerOpenCodeAsk,
// AnswerOpenCodeForm) outcome's reason.
func openCodeAnswerResult(err error) string {
	switch {
	case err == nil:
		return "relayed"
	case errors.Is(err, mirror.ErrStepUpRequired):
		return "step-up"
	case errors.Is(err, mirror.ErrOpenCodeAskGone):
		return "gone"
	case errors.Is(err, mirror.ErrOpenCodeUnreachable):
		return "unreachable"
	case errors.Is(err, mirror.ErrOpenCodeAnswerUnknown):
		return "answer-unknown"
	case errors.Is(err, mirror.ErrOpenCodeAnswerRefused):
		return "answer-refused"
	case errors.Is(err, mirror.ErrOpenCodeAnswerUnsupported), errors.Is(err, mirror.ErrAskUnsupported), errors.Is(err, mirror.ErrOpenCodeFormsUnsupported):
		return "unsupported"
	case errors.Is(err, mirror.ErrHostOffline), errors.Is(err, mirror.ErrHostDisconnected):
		return "not-sent"
	}
	return "error"
}

// askAttemptPruneEvery is how often the attention sweep prunes the log.
const askAttemptPruneEvery = time.Hour

// pruneAskAttempts deletes the rows past askattempt.RowTTL (from the attention
// sweep, at most once per askAttemptPruneEvery).
func (s *Server) pruneAskAttempts(ctx context.Context) {
	if s.ext.AskAttempts == nil {
		return
	}
	now := s.now()
	s.askAttemptPrune.Lock()
	if !s.askAttemptPrune.last.IsZero() && now.Sub(s.askAttemptPrune.last) < askAttemptPruneEvery {
		s.askAttemptPrune.Unlock()
		return
	}
	s.askAttemptPrune.last = now
	s.askAttemptPrune.Unlock()
	n, err := s.ext.AskAttempts.Prune(ctx, now.Add(-askattempt.RowTTL))
	switch {
	case err != nil:
		s.logger.Printf("answer attempts: retention sweep: %v", err)
	case n > 0:
		s.logger.Printf("answer attempts: retention sweep deleted %d row(s) older than %s", n, askattempt.RowTTL)
	}
}
