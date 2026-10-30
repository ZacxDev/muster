package api

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// listingRelay is the relay fixture's agent store, listing its two agents.
type listingRelay struct{ relayAgents }

func (l listingRelay) List(context.Context) ([]agents.Agent, error) {
	return []agents.Agent{relayAgentA, relayAgentB}, nil
}

func (l listingRelay) LastMessageByAgentIDs(context.Context, []int64) (map[int64]time.Time, error) {
	return nil, nil
}

// TestAnAgentAwaitingApprovalIsWaitingOnAnAskOnTheList: an agent blocked at a
// permission prompt (POST /agent/blocked — the "awaiting approval" badge) is
// drawn waiting on the viewer where agents are listed now — its sidebar row
// carries data-nav-wait (the retired list card's data-wait="you") and the
// session switcher's Waiting view holds it — and its page's status strip
// carries the badge; and the status poll, carrying what the page was drawn
// with, answers HX-Trigger agents:awaiting when that is no longer true — on
// the way in AND on the way out — and nothing when it still is.
//
// Red before: the badge was only the poll's own markup, so the card carried no
// data-wait and the chip hid a blocked agent.
func TestAnAgentAwaitingApprovalIsWaitingOnAnAskOnTheList(t *testing.T) {
	s := New(nil, AuthConfig{HookToken: relayHookToken, ServiceToken: relayServiceToken, UIPassword: relayPassword}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Agents: listingRelay{relayAgents{byToken: map[string]agents.Agent{relayTokenA: relayAgentA, relayTokenB: relayAgentB}}},
		SessionLiveness: stubLiveness{}})
	h := s.Handler()
	get := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Cookie", sessionCookieFor(s))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, w.Code, w.Body)
		}
		return w
	}
	block := func(on bool) {
		r := httptest.NewRequest(http.MethodPost, "/agent/blocked", strings.NewReader(`{"blocked":`+strconv.FormatBool(on)+`}`))
		r.Header.Set("X-Muster-Token", relayTokenA)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code >= 300 {
			t.Fatalf("POST /agent/blocked: %d %s", w.Code, w.Body)
		}
	}
	rowTag := func(id int64) string {
		row := navRow(t, get(ui.NavAgentsPath).Body.String(), agentNavID(id))
		return row[:strings.Index(row, ">")+1]
	}
	waiting := func() []string {
		var out []string
		for _, it := range s.switcher(operatorCtx(), ui.SwitcherCurrent{}).Waiting {
			out = append(out, it.Code)
		}
		return out
	}
	badged := func(id int64) bool {
		return strings.Contains(agentStrip(t, get, id), `data-badge="`+ui.AwaitingApprovalBadgeID+`"`)
	}
	if tag := rowTag(relayAgentA.ID); strings.Contains(tag, "data-nav-wait") || len(waiting()) != 0 || badged(relayAgentA.ID) {
		t.Fatalf("control: an agent not blocked is waiting on something: row %q, Waiting %v, badge %v", tag, waiting(), badged(relayAgentA.ID))
	}
	block(true)
	if tag := rowTag(relayAgentA.ID); !strings.Contains(tag, `data-nav-wait=""`) {
		t.Errorf("a blocked agent's sidebar row does not carry data-nav-wait: %s", tag)
	}
	if got := waiting(); !eqStrings(got, []string{relayAgentA.Name}) {
		t.Errorf("the switcher's Waiting view = %v, want only the blocked %s", got, relayAgentA.Name)
	}
	if !badged(relayAgentA.ID) {
		t.Errorf("a blocked agent's page strip does not carry the awaiting-approval badge")
	}
	if tag := rowTag(relayAgentB.ID); strings.Contains(tag, "data-nav-wait") || badged(relayAgentB.ID) {
		t.Errorf("the other agent is waiting too: %s", tag)
	}
	poll := func(drawn bool) string {
		return get(ui.StatusPollPath(relayAgentA.ID, drawn, mirror.WindowWork{})).Header().Get("HX-Trigger")
	}
	if got := poll(false); got != ui.AgentsAwaitingEvent {
		t.Errorf("a card drawn free, now blocked: HX-Trigger %q, want %q", got, ui.AgentsAwaitingEvent)
	}
	if got := poll(true); got != "" {
		t.Errorf("a card drawn blocked, still blocked: HX-Trigger %q, want none", got)
	}
	block(false)
	if got := poll(true); got != ui.AgentsAwaitingEvent {
		t.Errorf("a card drawn blocked, now free: HX-Trigger %q, want %q", got, ui.AgentsAwaitingEvent)
	}

	// The STATUS the strip was drawn with rides on the poll too (status=): the
	// poll swaps only its icon, so a status that moved asks for the strip,
	// which redraws data-agent-status — what the ⋯ menu's Start/Stop follows.
	m := regexp.MustCompile(`data-agent-status="([^"]+)"`).FindStringSubmatch(agentStrip(t, get, relayAgentA.ID))
	if m == nil {
		t.Fatal("instrument check FAILED: the strip carries no data-agent-status")
	}
	statusPoll := func(drawn string) string {
		return get(ui.StatusPollPath(relayAgentA.ID, false, mirror.WindowWork{}) + "&status=" + drawn).Header().Get("HX-Trigger")
	}
	if got := statusPoll(m[1]); got != "" {
		t.Errorf("drawn %s, still %s: HX-Trigger %q, want none", m[1], m[1], got)
	}
	other := "stopped"
	if m[1] == other {
		other = "running"
	}
	if got := statusPoll(other); got != ui.AgentsAwaitingEvent {
		t.Errorf("drawn %s, now %s: HX-Trigger %q, want %q", other, m[1], got, ui.AgentsAwaitingEvent)
	}
}
