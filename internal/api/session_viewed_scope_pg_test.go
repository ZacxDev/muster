package api

import (
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/sessionstate"
)

// TestTheViewRuleEndsOnlyTheRequestersOwnEpisode — the view rule's owner
// scoping, below the page (episodeViewed): called as another user (a member)
// for the operator's session it ends nothing; called as the owner it ends
// their episode (the control). The page's
// own refusal is TestAnotherUserOpeningTheSessionDoesNotEndIt.
func TestTheViewRuleEndsOnlyTheRequestersOwnEpisode(t *testing.T) {
	f := newStateFixture(t, false)
	id, _ := f.pushedStall(t)
	f.srv.episodeViewed(asUser(f.member), sessionstate.KindMirror, nudgeSID)
	if e := f.episodeByID(t, id); !e.Open() {
		t.Fatalf("a member's view ended the operator's stop: %+v", e)
	}
	f.srv.episodeViewed(asUser(authz.OperatorUserID), sessionstate.KindMirror, nudgeSID)
	if e := f.episodeByID(t, id); e.EndReason != sessionstate.EndViewed {
		t.Fatalf("the control — the owner's view: reason %q", e.EndReason)
	}
}
