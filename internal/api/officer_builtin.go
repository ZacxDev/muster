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

	"github.com/jackc/pgx/v5/pgconn"

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
//     (re-open on answer or withdrawal, escalate on expiry), raises each
//     escalated item's recipients' Attention asks once per recipient, and
//     carries an answer to one back to the officer (the item re-opens).
//
// Both act through the store (internal/officers/builtin), whose rows are the
// truth, so they hold across a restart and across replicas.
// ---------------------------------------------------------------------------

// EnableOfficerStore turns on the built-in officer backend's store: the admin
// API (/api/officers/…, officer_admin.go) and the Attention sweep. Called by
// muster-server when MUSTER_OFFICERS is set — `[]` included, the empty
// roster — and there is a database; EnableOfficers takes the same store for
// the builtin officers it declares.
func (s *Server) EnableOfficerStore(store *builtin.Store) {
	s.officerStore = store
	// A plugin can now be bound to one of the store's officers
	// (plugin_outbound.go).
	s.linkPluginsToOfficers(store)
}

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
// expiry — every escalated item's recipient not yet asked is raised an
// Attention ask, recorded once (NoticeRaised: a replica that loses the race
// withdraws its duplicate), and every settled escalation ask is carried back
// (settleOfficerEscalation).
func (s *Server) SweepOfficerAttention(ctx context.Context) {
	if s.officerStore == nil || s.ext.Attention == nil {
		return
	}
	sys := authz.System(ctx)
	pageThrough(func(after int64) ([]builtin.OpenAsk, error) {
		return s.officerStore.OpenAsks(sys, after, officerAttentionBatch)
	},
		func(oa builtin.OpenAsk) int64 { return oa.Item }, s.settleOfficerAsk, "asks", s.logger.Printf)
	pageThrough(func(after int64) ([]builtin.DueNotice, error) {
		return s.officerStore.DueNotices(sys, after, officerAttentionBatch)
	},
		func(n builtin.DueNotice) int64 { return n.Item }, func(n builtin.DueNotice) { s.raiseOfficerNotice(ctx, sys, n) }, "escalations", s.logger.Printf)
	pageThrough(func(after int64) ([]builtin.OpenEscalation, error) {
		return s.officerStore.OpenEscalations(sys, after, officerAttentionBatch)
	}, func(e builtin.OpenEscalation) int64 { return e.Item }, func(e builtin.OpenEscalation) { s.settleOfficerEscalation(ctx, sys, e) },
		"escalation asks", s.logger.Printf)
}

// officerAttentionPages bounds the pages one sweep reads of each kind
// (officerAttentionBatch items a page).
const officerAttentionPages = 50

// pageThrough reads page after page (each a page of items above the last
// one's id) and handles every row, until a page holds fewer than a batch of
// items or officerAttentionPages pages are read. Paging is what keeps rows
// that are still waiting (an ask nobody has answered yet) from crowding
// settled ones out of every sweep.
func pageThrough[T any](read func(after int64) ([]T, error), item func(T) int64, handle func(T), what string, logf func(string, ...any)) {
	var after int64
	for page := 0; page < officerAttentionPages; page++ {
		rows, err := read(after)
		if err != nil {
			logf("officers: the built-in %s could not be read: %v", what, err)
			return
		}
		items := map[int64]bool{}
		for _, r := range rows {
			handle(r)
			items[item(r)] = true
			if id := item(r); id > after {
				after = id
			}
		}
		if len(items) < officerAttentionBatch {
			return
		}
	}
}

// settleOfficerAsk carries open ask oa's settled Attention ask back onto its
// item (ResolveAsk); still open: nothing.
func (s *Server) settleOfficerAsk(oa builtin.OpenAsk) {
	ctx := context.Background()
	a, err := s.ext.Attention.Get(s.attentionPrincipal(ctx, oa.Owner), oa.AskID)
	status, answer := "", ""
	switch {
	case errors.Is(err, attention.ErrNotFound):
		status = builtin.AskWithdrawn // gone from the owner's asks: nothing will answer it
	case err != nil:
		s.logger.Printf("officers: %s: item %d: ask %d could not be read: %v", oa.Officer, oa.Item, oa.AskID, err)
		return
	case a.Status == attention.Answered || a.Status == attention.AnsweredInSession:
		status, answer = builtin.AskAnswered, a.Answer
	case a.Status == attention.Cancelled:
		status = builtin.AskWithdrawn
	case a.Status == attention.Expired:
		status = builtin.AskExpired
	default:
		return // still open
	}
	got, err := s.officerStore.ResolveAsk(s.officerPrincipal(ctx, oa.Owner), oa.Owner, oa.Item, oa.AskID, status, answer, s.officerNow())
	if err != nil {
		s.logger.Printf("officers: %s: item %d: record ask %d (%s): %v", oa.Officer, oa.Item, oa.AskID, status, err)
		return
	}
	switch got {
	case builtin.Reopened:
		s.logger.Printf("officers: %s: item %d re-opened: its owner's ask %d was %s", oa.Officer, oa.Item, oa.AskID, status)
	case builtin.Escalated:
		s.logger.Printf("officers: %s: item %d escalated: its ask %d to the owner expired unanswered", oa.Officer, oa.Item, oa.AskID)
	}
}

// settleOfficerEscalation carries recipient e's settled escalation ask back
// to its item: an ANSWER re-opens the item for the officer with the answer on
// it, and the other recipients' asks are withdrawn; a dismissal or expiry
// only records it settled (the item stays escalated). Still open: nothing.
func (s *Server) settleOfficerEscalation(ctx, sys context.Context, e builtin.OpenEscalation) {
	rctx := s.attentionPrincipal(ctx, e.Recipient)
	a, err := s.ext.Attention.Get(rctx, e.AttentionID)
	answered, answer := false, ""
	switch {
	case errors.Is(err, attention.ErrNotFound):
	case err != nil:
		s.logger.Printf("officers: %s: item %d: escalation ask %d could not be read: %v", e.Officer, e.Item, e.AttentionID, err)
		return
	case a.Status == attention.Answered || a.Status == attention.AnsweredInSession:
		answered, answer = true, a.Answer
	case a.Status == attention.Open:
		return
	}
	reopened, withdraw, err := s.officerStore.SettleEscalation(sys, e.Owner, e.Item, e.Recipient, e.AttentionID, answered, answer, s.officerNow())
	if err != nil {
		s.logger.Printf("officers: %s: item %d: record escalation ask %d: %v", e.Officer, e.Item, e.AttentionID, err)
		return
	}
	if reopened {
		s.logger.Printf("officers: %s: item %d sent back to the officer: user %d answered its escalation", e.Officer, e.Item, e.Recipient)
	} else if answered {
		s.logger.Printf("officers: %s: item %d: user %d answered its escalation, but the item could not be re-opened (no longer escalated, or another live item holds its subject); the answer is recorded on the escalation only", e.Officer, e.Item, e.Recipient)
	}
	for _, w := range withdraw {
		if _, err := s.cancelAsk(s.attentionPrincipal(ctx, w.Recipient), w.AttentionID); err != nil && !errors.Is(err, attention.ErrNotOpen) {
			s.logger.Printf("officers: %s: item %d: withdraw user %d's escalation ask %d: %v", w.Officer, w.Item, w.Recipient, w.AttentionID, err)
		}
	}
}

// raiseOfficerNotice raises escalation n as an Attention ask to its
// recipient and records it; the loser of a race withdraws its copy.
func (s *Server) raiseOfficerNotice(ctx, sys context.Context, n builtin.DueNotice) {
	rctx := s.attentionPrincipal(ctx, n.Recipient)
	a, err := s.ext.Attention.Create(rctx, attention.New{
		Asker:    attention.Asker{System: true, Label: officerAskLabel(n.Officer)},
		Question: officerNoticeText(n),
	})
	var pg *pgconn.PgError
	switch {
	case errors.Is(err, attention.ErrInvalid) || errors.As(err, &pg) && pg.Code == "23503":
		// Never raisable (no such user to ask, or a text Attention refuses):
		// given up for good, so it cannot hold the queue or retry for ever.
		s.logger.Printf("officers: %s: item %d: the escalation to user %d cannot be raised and is given up: %v", n.Officer, n.Item, n.Recipient, err)
		if aerr := s.officerStore.NoticeAbandoned(sys, n.Owner, n.Item, n.Recipient); aerr != nil {
			s.logger.Printf("officers: %s: item %d: give up the escalation to user %d: %v", n.Officer, n.Item, n.Recipient, aerr)
		}
		return
	case err != nil:
		s.logger.Printf("officers: %s: item %d: the escalation to user %d could not be raised: %v", n.Officer, n.Item, n.Recipient, err)
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

// officerEscalationTail ends every escalation ask: what answering does.
const officerEscalationTail = "\n\nAnswer to send the item back to the officer with your answer; dismiss it to leave the item escalated."

// officerNoticeText is an escalation's Attention ask: which officer escalated
// which item, its summary (bounded to Attention's question limit), and what
// an answer does.
func officerNoticeText(n builtin.DueNotice) string {
	head := fmt.Sprintf("Officer %s escalated item %d (%s %s) to you:\n\n", n.Officer, n.Item, n.Kind, n.SubjectKey)
	sum := strings.TrimSpace(n.Summary)
	if sum == "" {
		sum = "(no summary)"
	}
	if room := attention.MaxQuestionRunes - utf8.RuneCountInString(head) - utf8.RuneCountInString(officerEscalationTail); utf8.RuneCountInString(sum) > room {
		sum = string([]rune(sum)[:room-1]) + "…"
	}
	return head + sum + officerEscalationTail
}

// officerNow is the officer runtime's clock, or time.Now without one.
func (s *Server) officerNow() time.Time {
	if s.officerRT != nil {
		return s.officerRT.clock()
	}
	return time.Now()
}
