package api

import (
	"html"
	"strings"
	"testing"
)

// TestAnEndCardWhoseProposalCannotBeReadOffersNoEnd: an end-session card
// whose proposal row cannot be read as it is drawn offers NO End it on the
// Attention view and the ask page (Not now stays), and says so — as an
// unreadable succession card offers no start. The control: the same card,
// read, draws End it.
func TestAnEndCardWhoseProposalCannotBeReadOffersNoEnd(t *testing.T) {
	f := newSuccFixture(t)
	f.viewLine(t, sxOpenCodeKey, "close this agent session")
	_, id := f.proposeEnd(t, sxOpenCodeKey)
	x, err := f.plugins.ProposalByID(f.asOwner, id)
	if err != nil {
		t.Fatal(err)
	}
	endIt := `value="End it"`
	if page := f.ownerGet(t, f.srv.handleAttentionItems, "/attention/items", 0); !strings.Contains(page, endIt) {
		t.Fatalf("control: the readable card draws no End it:\n%s", page)
	}
	if page := f.ownerGet(t, f.srv.handleAskPage, askPagePath(x.AskID)+"?part=live", x.AskID); !strings.Contains(page, endIt) {
		t.Fatalf("control: the readable ask page draws no End it:\n%s", page)
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
		if strings.Contains(pg.body, endIt) || !strings.Contains(pg.body, `value="Not now"`) {
			t.Errorf("%s of an unreadable end card: End it drawn, or Not now missing:\n%s", pg.name, pg.body)
		}
		if pg.note && !strings.Contains(pg.body, html.EscapeString(proposalUnreadNote)) {
			t.Errorf("%s of an unreadable end card does not say so:\n%s", pg.name, pg.body)
		}
	}
}
