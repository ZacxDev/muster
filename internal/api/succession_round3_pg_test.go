package api

import (
	"context"
	"errors"
	"html"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/plugins"
)

// #546 ROUND 3: a card whose proposal cannot be read as it is drawn offers
// no start.

// unreadProposals is the plugin store with its proposal-by-card read
// failing (a database blip as a card is drawn).
type unreadProposals struct{ *plugins.PGStore }

func (unreadProposals) ProposalByAsk(context.Context, int64) (plugins.Proposal, error) {
	return plugins.Proposal{}, errors.New("fixture: the read failed")
}

// TestAProposalAnswerSaysWhereItWent: a proposal card's answer reads as
// "recorded, not sent to your chief" only once its delivery was closed
// (waited); one whose close failed says where it went, as any other.
func TestAProposalAnswerSaysWhereItWent(t *testing.T) {
	waited := attention.Ask{Delivery: attention.DeliveryWaited}
	if got, _ := answerDeliveryText(waited, "chief", true); got != proposalAnsweredText {
		t.Errorf("a closed delivery: %q", got)
	}
	queued := attention.Ask{Delivery: attention.DeliveryQueued}
	if got, _ := answerDeliveryText(queued, "chief", true); got == proposalAnsweredText || !strings.HasPrefix(got, "Queued for chief") {
		t.Errorf("a delivery that was not closed: %q; want where it went", got)
	}
}

// TestACardWhoseProposalCannotBeReadOffersNoStart: when the proposal row
// cannot be read as the card is drawn, the Attention view and the ask page
// say so and draw NO start button (Not now stays). The control: the same
// card, read, draws Start it.
func TestACardWhoseProposalCannotBeReadOffersNoStart(t *testing.T) {
	f := newSuccFixture(t)
	f.viewLine(t, sxOpenCodeKey, "continue it")
	_, id := f.propose(t, sxCall(sxOpenCodeKey, "spinoff", sxRefFenc, intp(0)))
	x, _ := f.plugins.ProposalByID(f.asOwner, id)
	start := `value="Start it"`
	if page := f.ownerGet(t, f.srv.handleAttentionItems, "/attention/items", 0); !strings.Contains(page, start) {
		t.Fatalf("control: the readable card draws no Start it:\n%s", page)
	}
	if page := f.ownerGet(t, f.srv.handleAskPage, askPagePath(x.AskID)+"?part=live", x.AskID); !strings.Contains(page, start) {
		t.Fatalf("control: the readable ask page draws no Start it:\n%s", page)
	}
	saved := f.srv.ext.ChiefDispatch
	f.srv.ext.ChiefDispatch = unreadProposals{f.plugins}
	defer func() { f.srv.ext.ChiefDispatch = saved }()
	for _, pg := range []struct {
		name, body string
		note       bool
	}{
		{"the Attention view", f.ownerGet(t, f.srv.handleAttentionItems, "/attention/items", 0), true},
		{"the ask page", f.ownerGet(t, f.srv.handleAskPage, askPagePath(x.AskID), x.AskID), true},
		{"the ask page's live part", f.ownerGet(t, f.srv.handleAskPage, askPagePath(x.AskID)+"?part=live", x.AskID), false},
	} {
		if strings.Contains(pg.body, start) || !strings.Contains(pg.body, `value="Not now"`) {
			t.Errorf("%s of an unreadable card: Start it drawn, or Not now missing:\n%s", pg.name, pg.body)
		}
		if pg.note && !strings.Contains(pg.body, html.EscapeString(proposalUnreadNote)) {
			t.Errorf("%s of an unreadable card does not say so:\n%s", pg.name, pg.body)
		}
	}
}
