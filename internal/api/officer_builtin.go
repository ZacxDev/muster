package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
)

// ---------------------------------------------------------------------------
// THE BUILT-IN OFFICER BACKEND'S ATTENTION SIDE (docs/officers-builtin.md)
//
//   - officerAsker raises a builtin officer's officer_ask_person as an
//     Attention ask to the officer's OWNER (muster's own: no agent waits on
//     its answer — the item does);
//   - RunOfficerAttentionSweep settles those asks back onto their items
//     (re-open on answer or withdrawal, escalate on expiry) and raises each
//     escalated item's notices as Attention notices, once per recipient.
//
// Both act through the store (internal/officers/builtin), whose rows are the
// truth, so they hold across a restart and across replicas.
// ---------------------------------------------------------------------------

// EnableOfficerStore turns on the built-in officer backend's store: the admin
// API (/api/officers/…, officer_admin.go) and the Attention sweep. Called by
// muster-server when MUSTER_OFFICERS is set — `[]` included, the empty
// roster — and there is a database; EnableOfficers takes the same store for
// the builtin officers it declares.
func (s *Server) EnableOfficerStore(store *builtin.Store) { s.officerStore = store }

// OfficerStoreOn reports whether EnableOfficerStore was called.
func (s *Server) OfficerStoreOn() bool { return s.officerStore != nil }

// officerAskLabel is the Attention label of an ask or notice a builtin
// officer raises.
func officerAskLabel(o string) string { return "officer " + o }

// officerAsker is builtin.Asker over the attention store.
type officerAsker struct{ s *Server }

var _ builtin.Asker = officerAsker{}

// Raise implements builtin.Asker: the ask, as the officer's OWNER, with
// muster as its asker (nothing waits on its answer in a session: the sweep
// carries it to the item), and the owner's devices told.
func (a officerAsker) Raise(ctx context.Context, o officers.Officer, question string, options []string, timeout time.Duration) (int64, error) {
	s := a.s
	if s.ext.Attention == nil {
		return 0, fmt.Errorf("%w: no attention store", officers.ErrHQUnavailable)
	}
	ask, err := s.ext.Attention.Create(s.attentionPrincipal(ctx, o.Owner), attention.New{
		Asker:    attention.Asker{System: true, Label: officerAskLabel(o.ID)},
		Question: question, Options: options, Timeout: timeout,
	})
	switch {
	case errors.Is(err, attention.ErrInvalid):
		return 0, officerDecision(http.StatusBadRequest, "invalid", err.Error())
	case errors.Is(err, attention.ErrTooMany):
		return 0, officerDecision(http.StatusConflict, "too_many_asks", err.Error())
	case err != nil:
		return 0, err
	}
	s.askFiled(ask)
	return ask.ID, nil
}

// Withdraw implements builtin.Asker.
func (a officerAsker) Withdraw(ctx context.Context, o officers.Officer, id int64) {
	if _, err := a.s.cancelAsk(a.s.attentionPrincipal(ctx, o.Owner), id); err != nil && !errors.Is(err, attention.ErrNotOpen) {
		a.s.logger.Printf("officers: %s: withdraw ask %d after its item could not be set waiting: %v", o.ID, id, err)
	}
}

// officerDecision is the contract's decision: an *officers.HQError with a
// {code, error} body.
func officerDecision(status int, code, msg string) error {
	raw, _ := json.Marshal(map[string]string{"code": code, "error": msg})
	return &officers.HQError{Status: status, Body: raw}
}

// officerAttentionBatch bounds one sweep's asks and notices.
const officerAttentionBatch = 100

// DefaultOfficerAttentionSweep is how often the sweep runs.
const DefaultOfficerAttentionSweep = 15 * time.Second

// RunOfficerAttentionSweep runs SweepOfficerAttention every interval until
// ctx ends; nothing without the store or the attention store.
func (s *Server) RunOfficerAttentionSweep(ctx context.Context, interval time.Duration) {
	if s.officerStore == nil || s.ext.Attention == nil {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.SweepOfficerAttention(ctx)
		}
	}
}

// SweepOfficerAttention is one pass: every open ask on a waiting builtin
// item whose Attention ask has settled is recorded on the item (answered,
// withdrawn or expired) — re-opening it once none is open, escalating it on
// expiry — and every escalated item's notice not yet raised is raised as an
// Attention notice to its recipient, recorded once (NoticeRaised: a replica
// that loses the race withdraws its duplicate).
func (s *Server) SweepOfficerAttention(ctx context.Context) {
	if s.officerStore == nil || s.ext.Attention == nil {
		return
	}
	sys := authz.System(ctx)
	open, err := s.officerStore.OpenAsks(sys, officerAttentionBatch)
	if err != nil {
		s.logger.Printf("officers: the built-in asks could not be read: %v", err)
	}
	for _, oa := range open {
		a, err := s.ext.Attention.Get(s.attentionPrincipal(ctx, oa.Owner), oa.AskID)
		status, answer := "", ""
		switch {
		case errors.Is(err, attention.ErrNotFound):
			status = builtin.AskWithdrawn // gone from the owner's asks: nothing will answer it
		case err != nil:
			s.logger.Printf("officers: %s: item %d: ask %d could not be read: %v", oa.Officer, oa.Item, oa.AskID, err)
			continue
		case a.Status == attention.Answered || a.Status == attention.AnsweredInSession:
			status, answer = builtin.AskAnswered, a.Answer
		case a.Status == attention.Cancelled:
			status = builtin.AskWithdrawn
		case a.Status == attention.Expired:
			status = builtin.AskExpired
		default:
			continue // still open
		}
		got, err := s.officerStore.ResolveAsk(s.officerPrincipal(ctx, oa.Owner), oa.Owner, oa.Item, oa.AskID, status, answer, s.officerNow())
		if err != nil {
			s.logger.Printf("officers: %s: item %d: record ask %d (%s): %v", oa.Officer, oa.Item, oa.AskID, status, err)
			continue
		}
		switch got {
		case builtin.Reopened:
			s.logger.Printf("officers: %s: item %d re-opened: its owner's ask %d was %s", oa.Officer, oa.Item, oa.AskID, status)
		case builtin.Escalated:
			s.logger.Printf("officers: %s: item %d escalated: its ask %d to the owner expired unanswered", oa.Officer, oa.Item, oa.AskID)
		}
	}
	due, err := s.officerStore.DueNotices(sys, officerAttentionBatch)
	if err != nil {
		s.logger.Printf("officers: the built-in escalation notices could not be read: %v", err)
	}
	for _, n := range due {
		s.raiseOfficerNotice(ctx, sys, n)
	}
}

// raiseOfficerNotice raises escalation notice n as an Attention notice to
// its recipient and records it; the loser of a race withdraws its copy.
func (s *Server) raiseOfficerNotice(ctx, sys context.Context, n builtin.DueNotice) {
	rctx := s.attentionPrincipal(ctx, n.Recipient)
	a, err := s.ext.Attention.Create(rctx, attention.New{
		Asker:    attention.Asker{System: true, Label: officerAskLabel(n.Officer)},
		Question: officerNoticeText(n), Notice: true,
	})
	if err != nil {
		s.logger.Printf("officers: %s: item %d: the escalation notice to user %d could not be raised: %v", n.Officer, n.Item, n.Recipient, err)
		return
	}
	won, err := s.officerStore.NoticeRaised(sys, n.Owner, n.Item, n.Recipient, a.ID)
	if err != nil || !won {
		if _, cerr := s.cancelAsk(rctx, a.ID); cerr != nil && !errors.Is(cerr, attention.ErrNotOpen) {
			s.logger.Printf("officers: %s: item %d: withdraw a duplicate escalation notice %d: %v", n.Officer, n.Item, a.ID, cerr)
		}
		if err != nil {
			s.logger.Printf("officers: %s: item %d: record the escalation notice to user %d: %v", n.Officer, n.Item, n.Recipient, err)
		}
		return
	}
	s.askFiled(a)
}

// officerNoticeText is an escalation notice: which officer escalated which
// item, and its summary (bounded to Attention's question limit).
func officerNoticeText(n builtin.DueNotice) string {
	head := fmt.Sprintf("Officer %s escalated item %d (%s %s) to you:\n\n", n.Officer, n.Item, n.Kind, n.SubjectKey)
	sum := strings.TrimSpace(n.Summary)
	if sum == "" {
		sum = "(no summary)"
	}
	if room := attention.MaxQuestionRunes - utf8.RuneCountInString(head); utf8.RuneCountInString(sum) > room {
		sum = string([]rune(sum)[:room-1]) + "…"
	}
	return head + sum
}

// officerNow is the officer runtime's clock, or time.Now without one.
func (s *Server) officerNow() time.Time {
	if s.officerRT != nil {
		return s.officerRT.clock()
	}
	return time.Now()
}
