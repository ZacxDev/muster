package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/ui"
)

// THE MESSAGE SEARCH IS THE VIEWER'S OWN CHIEF'S. The Sessions tab's message
// search asks GET /ui/chief/threads/search, which searches the chief chiefAgent
// resolves for the VIEWER — so it is wired only where the chief on screen is
// that one (ui.ChiefChatBar.MessageSearch, chiefChatBarFor). RED before the
// round-1 fix: every chief's tab was wired.

const messageSearchWiring = `hx-get="/ui/chief/threads/search?`

// TestTheOwnersChiefWiresTheMessageSearch: the viewer's own chief — its page
// and its panel — has the message search.
func TestTheOwnersChiefWiresTheMessageSearch(t *testing.T) {
	f := newChatsFixture(t, "")
	r := f.call(t, http.MethodGet, ui.AgentSessionHref(f.chief.Name, 0), nil, f.operator())
	if r.Code == http.StatusSeeOther || r.Code == http.StatusFound {
		r = f.call(t, http.MethodGet, r.Header().Get("Location"), nil, f.operator())
	}
	own := r.Body.String()
	if r.Code != http.StatusOK || !strings.Contains(own, "data-chief-sessions-search") {
		t.Fatalf("control: the chief's page (%d) has no Sessions tab", r.Code)
	}
	if !strings.Contains(own, messageSearchWiring) || !strings.Contains(own, `data-chief-message-matches=""`) {
		t.Error("the viewer's own chief's page has no message search")
	}
	if panel := f.call(t, http.MethodGet, "/ui/chief/panel", nil, f.operator()).Body.String(); !strings.Contains(panel, messageSearchWiring) {
		t.Error("the viewer's own chief's panel has no message search")
	}
}

// TestATeamChiefsSharedRoomHasNoMessageSearch: a member viewing a TEAM's chief
// (its one shared room) gets the Sessions tab and its title filter, but no
// message search: the route would have searched the member's own chief — or,
// with none, answered a failure — never the team chief's chats.
func TestATeamChiefsSharedRoomHasNoMessageSearch(t *testing.T) {
	tw := newTeamAgentWalk(t)
	rec := tw.w.do(tw.w.h, "GET", "/agents/"+tw.chief.Name, "", cred{sessionUser: tw.v.uid})
	if rec.Code == http.StatusSeeOther || rec.Code == http.StatusFound {
		rec = tw.w.do(tw.w.h, "GET", rec.Header().Get("Location"), "", cred{sessionUser: tw.v.uid})
	}
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "data-chief-sessions-search") {
		t.Fatalf("control: the team chief's page (%d) has no Sessions tab for the member", rec.Code)
	}
	if strings.Contains(body, messageSearchWiring) || strings.Contains(body, `data-chief-message-matches=""`) {
		t.Error("the team chief's shared room wires the message search, which searches the viewer's own chief")
	}
}
