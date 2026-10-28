package api

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/suggest"
)

// ---------------------------------------------------------------------------
// SUGGESTED REPLIES TO AN ASK (operator ask: "Include suggested responses the
// same way we have AI-generated next messages in the rendered agent chat
// view"; docs/attention.md#suggested-replies-to-an-ask).
//
// 🔴 FILED, THEN SUGGESTED, NEVER THE OTHER WAY ROUND. askFiled is every path
// that files an ask for its owner (attention_ask, a task's ask, muster's own
// unrouted-message ask): it pushes the notification AT ONCE, with no
// suggestion, and only then starts the generation in the background — a model
// call never delays the first push.
//
// 🔴 THE SAME ENGINE AND THE SAME SWITCHES as the rendered view's chips
// (internal/suggest): MUSTER_SUGGEST=off, the owner's Account toggle and the
// auto-pause (Engine.Enabled), and no OpenRouter key for the owner, each mean
// no call and no chips. The engine's rate caps, concurrency bound and
// one-call-per-turn rule apply (the ask's id is the turn), and its filter keeps
// only grounded, one-line suggestions.
//
// 🔴 A SETTLED ASK IS NEVER RE-PUSHED. The set is stored by one conditional
// UPDATE on the ask still being open (attention.PGStore.SetSuggestions), and
// the re-push happens only when that moved the row. A settle that lands
// between the store and the push's delivery is caught after it: the delivered
// re-push re-reads the ask and withdraws itself (attentionClear) if it is no
// longer open.
// ---------------------------------------------------------------------------

// askSuggestTimeout bounds one ask's generation, end to end.
const askSuggestTimeout = 45 * time.Second

// askSuggestSubject is an ask's subject in the suggestion engine's cache.
func askSuggestSubject(id int64) string { return "ask:" + strconv.FormatInt(id, 10) }

// askFiled is every path that filed ask a for its owner: its notification (at
// once, without suggestions), the views, and — in the background — its
// suggested replies.
func (s *Server) askFiled(a attention.Ask) {
	s.attentionNotify(a.Owner, askItem(a))
	s.attentionChanged(a.Owner, attentionAskKey(a.ID))
	s.startAskSuggestions(a)
}

// startAskSuggestions generates ask a's suggested replies in the background
// (suggestAsk). Nothing for a question to the chief (not the owner's) or
// without an engine.
func (s *Server) startAskSuggestions(a attention.Ask) {
	if s.ext.Suggest == nil || s.ext.Attention == nil || a.ToChief || a.Owner <= 0 {
		return
	}
	s.attention.suggesting.Add(1)
	safeGo(s.logger, "ask suggestions "+strconv.FormatInt(a.ID, 10), func() {
		defer s.attention.suggesting.Done()
		ctx, cancel := context.WithTimeout(s.attentionPrincipal(context.Background(), a.Owner), askSuggestTimeout)
		defer cancel()
		s.suggestAsk(ctx, a)
	})
}

// WaitForAskSuggestions waits for every ask-suggestion generation this server
// started: the tests, and cmd/muster-server's shutdown, which bounds it
// (askSuggestDrainTimeout) before the push drain.
func (s *Server) WaitForAskSuggestions() { s.attention.suggesting.Wait() }

// askSuggestInput is ask a as the suggestion engine's input: the question, and
// the options it offered, as the final reply R the suggestions must be
// grounded in (suggest.Build).
func askSuggestInput(a attention.Ask) (suggest.Built, bool) {
	text := strings.TrimSpace(a.Question)
	if len(a.Options) > 0 {
		text += "\n\nOptions:\n- " + strings.Join(a.Options, "\n- ")
	}
	return suggest.Build(suggest.Input{Tail: []suggest.Entry{{Role: suggest.RoleAssistant, Text: text}}})
}

// suggestAsk is one ask's generation, under ctx (its owner's principal): the
// switches, the call, the conditional store, and the re-push.
func (s *Server) suggestAsk(ctx context.Context, a attention.Ask) {
	b, ok := askSuggestInput(a)
	if !ok {
		return
	}
	// The switches (MUSTER_SUGGEST=off, the owner's toggle and auto-pause, no
	// OpenRouter key) are the engine's own first checks, before any call.
	set, out, err := s.ext.Suggest.Suggest(ctx, a.Owner, askSuggestSubject(a.ID), b, a.ID)
	if err != nil && !errors.Is(err, suggest.ErrCapped) && !errors.Is(err, suggest.ErrNoKey) {
		s.logger.Printf("attention: suggestions for ask %d: %s", a.ID, suggest.Reason(err))
	}
	if (out != suggest.OutcomeGenerated && out != suggest.OutcomeCached) || len(set.Suggestions) == 0 {
		return
	}
	texts := make([]string, 0, len(set.Suggestions))
	for _, x := range set.Suggestions {
		texts = append(texts, x.Text)
	}
	stored, moved, err := s.ext.Attention.SetSuggestions(ctx, a.ID, texts)
	if err != nil {
		s.logger.Printf("attention: store the suggestions for ask %d: %v", a.ID, err)
		return
	}
	if !moved {
		return // settled meanwhile (or nothing survived the cleaning): no re-push
	}
	s.attentionChanged(a.Owner, attentionAskKey(a.ID))
	s.repushAsk(stored)
}

// repushAsk sends open ask a's notification again under its own tag, now with
// its suggested replies, as an update (the phone replaces it without alerting
// again). Once delivered, the ask is read again: one settled meanwhile — its
// clear may have overtaken this push — is withdrawn again.
func (s *Server) repushAsk(a attention.Ask) {
	s.goNotify(a.Owner, push.EventAttention, attentionNotification(askItem(a), true), func(int) {
		ctx, cancel := context.WithTimeout(s.attentionPrincipal(context.Background(), a.Owner), pushTargetLookupTimeout)
		defer cancel()
		cur, err := s.ext.Attention.Get(ctx, a.ID)
		if err != nil && !errors.Is(err, attention.ErrNotFound) {
			s.logger.Printf("attention: re-read ask %d after its re-push: %v", a.ID, err)
			return
		}
		if err != nil || cur.Status != attention.Open {
			s.attentionClear(a.Owner, attentionAskKey(a.ID))
		}
	})
}
