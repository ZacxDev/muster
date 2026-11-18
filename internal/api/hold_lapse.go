package api

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/plugins"
)

// ---------------------------------------------------------------------------
// A DISPATCH CARD EXPIRES WITH ITS HOLD.
//
// 🔴 MEASURED IN PRODUCTION: an approval card (an owner card raised by
// agent_dispatch) stayed open for hours while the held dispatch it approves
// had lapsed after 15 minutes. The owner tapped Approve; the tap was recorded
// as an answer; the chief's completing call was refused (the request_key had
// no waiting hold) and it had to file a new card. Operator decision: "Card
// expires with its hold".
//
//   - A card hold now lives plugins.CardHoldTTL (24 hours), and its card is
//     filed to wait a little longer (plugins.CardAskOutlives), so the hold
//     always lapses first.
//   - The attention sweep (withdrawLapsedHoldCards) finds every card hold that
//     lapsed unused with its card still open (plugins.LapsedCardHolds),
//     WITHDRAWS the card as its owner — the store's own Cancel, so it leaves
//     the Attention list and the owner's devices like any withdrawn ask
//     (askSettled) — and tells the chief that raised it, in muster's words.
//   - A tap that reaches a stale card before the sweep does
//     (lapsedCardRefusal, in the ONE answer path) records nothing: the owner
//     reads holdLapsedAnswer, and the card is withdrawn and the chief told
//     right then. Nothing starts: no answer exists for the completing call to
//     bind to, and the hold is not open.
// ---------------------------------------------------------------------------

// cardHoldAge is plugins.CardHoldTTL in words, for the chief's sentences
// (TestACardHoldLivesADayAndItsCardOutlivesIt pins the two together).
const cardHoldAge = "24 hours"

// holdLapsedAnswer is what the owner reads on a tap of a card whose hold lapsed.
const holdLapsedAnswer = "This request expired; ask your chief again."

// lapsedHoldsPerSweep bounds one sweep's withdrawals.
const lapsedHoldsPerSweep = 100

// lapsedHoldChiefNote is muster's note to the chief about its card whose hold
// lapsed: the card and the request_key, no card text.
func lapsedHoldChiefNote(askID int64, key string, at time.Time) string {
	what := "your dispatch card " + strconv.FormatInt(askID, 10)
	if key != "" {
		what += " (request_key " + strconv.Quote(key) + ")"
	}
	return "[muster] " + what + " expired unanswered at " + at.UTC().Format(time.RFC3339) + " (a dispatch card waits " + cardHoldAge +
		"), and muster withdrew it: nothing was started. If your owner still wants it, ask them again, with a new request_key."
}

// withdrawLapsedHoldCards is one sweep: every lapsed card hold's card,
// withdrawn as its owner, and its chief told.
func (s *Server) withdrawLapsedHoldCards(ctx context.Context) {
	st, ok := s.ext.ChiefDispatch.(interface {
		LapsedCardHolds(ctx context.Context, limit int) ([]plugins.LapsedCardHold, error)
	})
	if !ok || s.ext.Attention == nil {
		return
	}
	due, err := st.LapsedCardHolds(ctx, lapsedHoldsPerSweep)
	if err != nil {
		s.logger.Printf("agent_dispatch: lapsed card holds: %v", err)
		return
	}
	for _, l := range due {
		s.withdrawLapsedCard(s.attentionPrincipal(ctx, l.Owner), l.AskID, l.RequestKey, l.ExpiresAt)
	}
}

// withdrawLapsedCard withdraws card askID, whose hold (request_key key)
// lapsed at at, as its owner (octx), and tells the chief that raised it — only
// when THIS call withdrew it (a card already settled is left, and its chief
// was told by whoever settled it), so the chief is told once.
func (s *Server) withdrawLapsedCard(octx context.Context, askID int64, key string, at time.Time) {
	a, err := s.cancelAsk(octx, askID)
	if errors.Is(err, attention.ErrNotOpen) || errors.Is(err, attention.ErrNotFound) {
		return
	}
	if err != nil {
		s.logger.Printf("agent_dispatch: withdraw card %d, whose hold lapsed: %v", askID, err)
		return
	}
	s.logger.Printf("agent_dispatch: card %d withdrawn: its hold (request_key %q) lapsed at %s", askID, key, at.UTC().Format(time.RFC3339))
	if s.ext.Agents == nil || a.Asker.AgentID <= 0 {
		return
	}
	chief, err := s.ext.Agents.Get(octx, a.Asker.AgentID)
	if err != nil || chief.OwnerID != a.Owner {
		s.logger.Printf("agent_dispatch: the chief to tell of card %d: %v", askID, err)
		return
	}
	if err := s.writeChiefNote(context.WithoutCancel(octx), chief, lapsedHoldChiefNote(askID, key, at)); err != nil {
		s.logger.Printf("agent_dispatch: note card %d's lapse to the chief: %v", askID, err)
	}
}

// lapsedCardRefusal is the answer path's check of card id (answerAskFrom,
// before anything is recorded): nil unless id is an agent_dispatch card whose
// hold lapsed unused — then the card is withdrawn and its chief told, and the
// answer is refused with holdLapsedAnswer. A card hold that cannot be read
// lets the answer through (the completing call decides from the stored rows
// alone, and refuses a lapsed hold there too).
func (s *Server) lapsedCardRefusal(ctx context.Context, id int64) error {
	st := s.hostDispatches()
	if st == nil || s.ext.Attention == nil {
		return nil
	}
	a, err := s.ext.Attention.Get(ctx, id)
	if err != nil || !a.OwnerCard || a.Status != attention.Open {
		return nil
	}
	h, err := st.CardHoldByAsk(ctx, id)
	if err != nil || h.ConsumedBy != 0 || !h.Expired {
		return nil
	}
	var d heldDetail
	if json.Unmarshal(h.Detail, &d) != nil || d.AskID != id {
		return nil
	}
	s.withdrawLapsedCard(ctx, id, d.RequestKey, h.ExpiresAt)
	return errReleaseRefused(holdLapsedAnswer)
}
