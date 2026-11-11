package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/ZacxDev/muster/internal/askattempt"
	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sendqueue"
)

// ---------------------------------------------------------------------------
// "SEND AS A MESSAGE": a failed question card's way out when typing again
// cannot work — the picker is gone, or muster cannot read it
// (askattempt.RecoverMessage: no-picker, unreadable, unrecognised). The answer
// muster was typing is sent to the session as an ORDINARY MESSAGE, through
// the send queue, after the picker is dismissed if it is still on screen.
//
//   1. the gates a queue post passes (sendQueueGate): the owner's host,
//      -interactive, a FRESH sign-in (403 + X-Muster-Step-Up), a listed
//      window the queue can wait on;
//   2. the card must be failed with a message recovery and an answer muster
//      was typing (askq.Entry.Typed) — the page never names the text;
//   3. the dismissal: the existing Escape (CancelAsk), unless the failure
//      said the picker is not on screen. A host that answers "no picker" is a
//      success here too; any other refusal sends nothing ("dismiss-failed"),
//      because a message typed into an open picker would be keys into it;
//   4. the message, queued (enqueueSend): "Answer to “<header or first
//      words>”: <the answer>", one line per question.
//
// It is recorded as its own attempt (ask_attempts.go): door card (or phone),
// method message.
// ---------------------------------------------------------------------------

// askMessageWords is how many of a question's first words name it in the
// message when it has no header.
const askMessageWords = 8

// askMessageText is the message that answers qs with answers (keyed by
// question text, as askq.Entry.Typed): one line per answered question.
func askMessageText(qs []askq.Question, answers map[string]string) string {
	var lines []string
	for _, q := range qs {
		a, ok := answers[q.Question]
		if !ok || strings.TrimSpace(a) == "" {
			continue
		}
		name := strings.TrimSpace(q.Header)
		if name == "" {
			words := strings.Fields(q.Question)
			if len(words) > askMessageWords {
				words = append(words[:askMessageWords], "…")
			}
			name = strings.Join(words, " ")
		}
		lines = append(lines, "Answer to “"+name+"”: "+a)
	}
	return strings.Join(lines, "\n")
}

// handleMirrorAskMessage is POST ui.MirrorAskMessagePath (host, session,
// window, tool): send a failed card's answer as a message. 200 with the card
// saying it was queued; 403 + X-Muster-Step-Up for a stale sign-in; 409 with
// the card when the card offers no such thing, or with what happened when
// the picker could not be dismissed or the queue refused it; 404 for a card
// that is not the request's owner's.
func (s *Server) handleMirrorAskMessage(w http.ResponseWriter, r *http.Request) {
	att := s.beginAttempt(r, askattempt.DoorCard, askattempt.ActionSubmit, askattempt.MethodMessage)
	defer att.done()
	r.Body = http.MaxBytesReader(w, r.Body, mirrorAskFormMax)
	if err := r.ParseForm(); err != nil {
		att.end("too-large")
		http.Error(w, "the request is too large", http.StatusRequestEntityTooLarge)
		return
	}
	att.a.Door = cardDoor(r.PostForm)
	tool := r.PostFormValue("tool")
	att.where(r.PostFormValue("host"), r.PostFormValue("session"), r.PostFormValue("window"), tool)
	h, e, ok := s.askWindow(w, r, r.PostFormValue("host"), r.PostFormValue("session"), r.PostFormValue("window"))
	if !ok {
		att.end("not-found")
		return
	}
	if !toolUseIDPattern.MatchString(tool) {
		att.end("invalid")
		http.Error(w, "a message names the call it answers (tool)", http.StatusBadRequest)
		return
	}
	if e.ClaudeSession == "" {
		att.end("not-found")
		http.NotFound(w, r)
		return
	}
	key := askKey(r, h, e, tool)
	entry, found := s.asks.Find(key)
	if !found {
		att.end("not-found")
		http.NotFound(w, r)
		return
	}
	att.shape(entry.Questions, nil)
	if s.ext.SendQueue == nil {
		att.end("unsupported")
		http.Error(w, "this server has no send queue", http.StatusConflict)
		return
	}
	// The queue's gates, a Send's: -interactive, a fresh sign-in, a listed
	// window the queue can wait on.
	q := sendQueueTarget{t: sendqueue.Target{Host: h.Host, Session: e.Session, Window: e.Window}, back: mirrorRef(e).ViewPath(), h: h, e: e, listed: true}
	if g := s.sendQueueGate(r.Context(), q); g.status != 0 {
		switch {
		case g.stale:
			att.end("step-up")
			refuseStale(w)
		case !h.Interactive:
			att.end("read-only")
			http.Error(w, g.msg, g.status)
		default:
			att.end("queue-refused")
			http.Error(w, g.msg, g.status)
		}
		return
	}
	// 🔴 THE CARD IS CLAIMED BEFORE ANYTHING IS DONE (askq's BeginMessage,
	// under the store's lock): a second request while this one dismisses or
	// queues is refused with the card, so one answer is never sent twice.
	// The claim checks the card offers it — failed with a message recovery,
	// and an answer muster was typing — and is released on every exit.
	offered := func(x askq.Entry) bool {
		return askattempt.Recovery(x.Reason) == askattempt.RecoverMessage && askMessageText(x.Questions, x.Typed) != ""
	}
	entry, err := s.asks.BeginMessage(key, offered)
	switch {
	case errors.Is(err, askq.ErrNotFound):
		att.end("not-found")
		http.NotFound(w, r)
		return
	case errors.Is(err, askq.ErrBusy):
		att.end("in-progress")
		s.writeAskLive(w, http.StatusConflict, questionCard(h, e, entry))
		return
	case err != nil:
		att.end("not-offered")
		s.writeAskLive(w, http.StatusConflict, questionCard(h, e, entry))
		return
	}
	// end is the claim's release: note "" leaves the card offering what it
	// did; otherwise it says note and offers nothing more.
	ended := false
	end := func(note string) askq.Entry {
		ended = true
		return s.asks.EndMessage(key, note)
	}
	defer func() {
		if !ended {
			s.asks.EndMessage(key, "")
		}
	}()
	text := askMessageText(entry.Questions, entry.Typed)
	// The answer as it was typed: whether it used "Other" is in its shape.
	att.a.Shape.Other = askTypedOther(entry.Questions, entry.Typed)
	// The picker first: dismissed, unless the failure said it is not on the
	// screen. A message must never be typed into an open picker. A host that
	// finds NO picker to dismiss (the screen is not a picker at all) is the
	// same as dismissed: nothing there can take the message's keys. A host
	// that sees something like a picker it cannot read refuses, and then the
	// card stops offering it and says why: muster cannot send it safely.
	if entry.Reason != "no-picker" {
		c, can := s.ext.Mirror.(mirrorAskCanceller)
		if !can || !h.AskCancel {
			att.end("dismiss-failed")
			s.writeAskLive(w, http.StatusConflict, questionCard(h, e, end(askMessageNoDismissNote)))
			return
		}
		if _, err := c.CancelAsk(context.WithoutCancel(r.Context()), h.Host, e.Session, e.Window); err != nil && !errors.Is(err, mirror.ErrAskNoPicker) {
			att.end("dismiss-failed")
			s.logger.Printf("mirror: dismissing a question in %s/%s%s before sending its answer as a message: %v", h.Host, e.Session, e.Window, err)
			s.writeAskLive(w, http.StatusConflict, questionCard(h, e, end(askMessageDismissFailedNote(err))))
			return
		}
	}
	if _, err := s.enqueueSend(r.Context(), q, text); err != nil {
		att.end("queue-refused")
		msg := "could not queue the message"
		if errors.Is(err, sendqueue.ErrFull) {
			msg = strings.TrimPrefix(err.Error(), "sendqueue: ")
		}
		http.Error(w, "The picker was dismissed, but "+msg+" — send your answer from the composer.", http.StatusConflict)
		return
	}
	att.end("queued")
	s.logger.Printf("mirror: a question's answer in %s/%s%s was queued as a message (%s)", h.Host, e.Session, e.Window, entry.ID)
	// The card offers no recovery any more — its answer went — and says so on
	// every poll until the CLI's record of the call resolves it.
	s.writeAskLive(w, http.StatusOK, questionCard(h, e, end(askSentAsMessageNote)))
}

// askMessageNoDismissNote and askMessageDismissFailedNote are what a card
// says once its picker could not be dismissed for a message: nothing was
// sent, and why.
const askMessageNoDismissNote = "Couldn't dismiss the terminal's picker (this host's muster-mirror cannot), so your answer was not sent as a message. " +
	"Press Esc in the terminal, then send your answer from the composer."

func askMessageDismissFailedNote(err error) string {
	return "Couldn't dismiss the terminal's picker, so your answer was not sent as a message: " + askCancelFailure(err) +
		" Press Esc in the terminal, then send your answer from the composer."
}

// askSentAsMessageNote is what a card whose answer went as a message says.
const askSentAsMessageNote = "Sent as a message: Claude gets your answer once it is idle (the send queue)."

// askTypedOther reports whether answers gives any question of qs an answer
// that is not one of its options (an answer of one's own).
func askTypedOther(qs []askq.Question, answers map[string]string) bool {
	for _, q := range qs {
		a, ok := answers[q.Question]
		if !ok {
			continue
		}
		for _, part := range strings.Split(a, askq.MultiJoin) {
			known := false
			for _, o := range q.Options {
				if o.Label == part {
					known = true
				}
			}
			if !known && part != "" {
				return true
			}
		}
	}
	return false
}
