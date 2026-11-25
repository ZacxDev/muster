package api

import (
	"testing"

	"github.com/ZacxDev/muster/internal/pages"
)

// TestPageFeedbackWakeIsTheOwnersOrALinksOnly: the wake a feedback message
// owes is the owner's own (sent from their view, recorded as theirs) or a
// share link's — never a team member's message from the owner's view, which
// is neither the owner's words nor a link's; never a response that is not
// feedback; never a page no mirrored Claude Code session made.
func TestPageFeedbackWakeIsTheOwnersOrALinksOnly(t *testing.T) {
	pg := pages.Page{ID: 71, OwnerID: 73, Creator: pages.Creator{MirrorHost: "tower", MirrorSession: routeSID}}
	fb := func(byOwner bool, link, user int64) pages.Response {
		return pages.Response{ID: 79, PageID: 71, ByOwner: byOwner, LinkID: link, ResponderUser: user, Version: 2,
			Component: pages.FeedbackComponent, Kind: pages.KindFeedback, Data: pages.FeedbackData("fb-words-83")}
	}
	if n, ok := pageFeedbackWake(pg, fb(true, 0, 73), 2, "Plan"); !ok || n.LinkID != 0 || n.OperatorText != "fb-words-83" || n.Count != 2 {
		t.Fatalf("the owner's own: %+v %v", n, ok)
	}
	if n, ok := pageFeedbackWake(pg, fb(false, 89, 0), 2, "Plan"); !ok || n.LinkID != 89 {
		t.Fatalf("a link's: %+v %v", n, ok)
	}
	for name, c := range map[string]struct {
		pg pages.Page
		r  pages.Response
	}{
		"a team member's, from the owner's view":  {pg, fb(true, 0, 97)},
		"an owner-view response recording no one": {pg, fb(true, 0, 0)},
		"a submit, not feedback":                  {pg, func() pages.Response { r := fb(true, 0, 73); r.Kind = pages.KindSubmit; return r }()},
		"an agent's page":                         {pages.Page{ID: 71, OwnerID: 73, Creator: pages.Creator{AgentID: 5}}, fb(true, 0, 73)},
		"another page's response":                 {pages.Page{ID: 72, OwnerID: 73, Creator: pg.Creator}, fb(true, 0, 73)},
	} {
		if n, ok := pageFeedbackWake(c.pg, c.r, 2, "Plan"); ok {
			t.Errorf("%s files a wake: %+v", name, n)
		}
	}
}
