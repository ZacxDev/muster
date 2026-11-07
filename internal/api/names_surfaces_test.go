package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/focus"
)

// namedFocusStore is a focus store where window @1's conversation is named
// "Billing migration" (auto) and focused on something else.
func namedFocusStore() *memFocus {
	return &memFocus{rows: map[focus.Subject]focus.Focus{
		focus.MirrorSubject(focusSID): {Subject: focus.MirrorSubject(focusSID), Title: "Fixing the Stripe webhook retry",
			UpdatedAt: time.Now(), Name: focus.Name{Auto: "Billing migration"}},
	}}
}

// TestTheChiefsToolsNameASessionByItsStableName: fleet_list names the window
// by its stable name, keeps its codename and place in `where`, and addresses
// it by `id`; focus_search's hit
// carries the same name; sessions_overview's row too.
func TestTheChiefsToolsNameASessionByItsStableName(t *testing.T) {
	fm := &fakeMirror{online: true, v2: &fakeMirrorV2{}, windows: namedWindows()}
	s, _ := focusMirrorServer(t, fm, namedFocusStore(), nil)
	ctx := operatorCtx()
	var fl struct {
		Sessions []fleetListRow `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(s.toolFleetList(ctx)), &fl); err != nil {
		t.Fatal(err)
	}
	var row *fleetListRow
	for i := range fl.Sessions {
		if fl.Sessions[i].ID == "mirror:"+focusSID {
			row = &fl.Sessions[i]
		}
	}
	if row == nil {
		t.Fatalf("fleet_list has no row for the window: %+v", fl.Sessions)
	}
	if row.Name != "Billing migration" || row.Code != "" || row.Where != "mango:1 · tmux main:1 @ ws" {
		t.Fatalf("fleet_list row = name %q code %q where %q; want the stable name, no code (where has it), the place", row.Name, row.Code, row.Where)
	}
	if got := s.subjectName(ctx, s.focusIndex(ctx), focus.MirrorSubject(focusSID)); got != "Billing migration" {
		t.Fatalf("a hit's name (focus_search) = %q", got)
	}
	// sessions_overview lists only RECENTLY active sessions: the window is
	// made active now, and the row must be found (or this proves nothing).
	fm.mu.Lock()
	for i := range fm.windows {
		fm.windows[i].Activity = time.Now().Unix()
	}
	fm.mu.Unlock()
	found := false
	for _, ls := range s.liveSessions(ctx, 0) {
		if ls.sub == focus.MirrorSubject(focusSID) {
			found = true
			if ls.name != "Billing migration" {
				t.Fatalf("sessions_overview names the window %q", ls.name)
			}
		}
	}
	if !found {
		t.Fatal("sessions_overview did not list the window at all")
	}
}

// TestAnAttentionItemAndItsPushCallTheSessionByItsName: an item about a
// named session carries the name (withFocus), its Asks row shows the name
// with its place as the tooltip, and its push title leads with the name
// (kept whole by the title rule); an item about an unnamed session keeps its
// place.
func TestAnAttentionItemAndItsPushCallTheSessionByItsName(t *testing.T) {
	fm := &fakeMirror{online: true, v2: &fakeMirrorV2{}, windows: namedWindows()}
	s, _ := focusMirrorServer(t, fm, namedFocusStore(), nil)
	items := []attentionItem{{Key: "ask-1", Kind: attentionKindAsk, Source: "mango:1 · tmux main:1 @ ws", target: "tmux main:1 @ ws",
		subject: focus.MirrorSubject(focusSID), claudeSession: focusSID}}
	s.withFocus(operatorCtx(), items)
	it := items[0]
	if it.Name != "Billing migration" {
		t.Fatalf("item name %q, want the stable name", it.Name)
	}
	n := attentionNotification(it, false)
	if n.Title != "Billing migration is asking" || n.TitleKeep != "Billing migration" {
		t.Fatalf("push title %q keep %q", n.Title, n.TitleKeep)
	}
	span := attentionSourceSpan(it)
	if !strings.Contains(span, `>Billing migration<`) || !strings.Contains(span, `title="mango:1 · tmux main:1 @ ws"`) {
		t.Fatalf("Asks row who = %s", span)
	}
	// The status bar's summary titles the item as its push does.
	if got := attentionSummaryItemOf(it, "", time.Now()).Title; got != "Billing migration" {
		t.Fatalf("summary title %q, want the push's who", got)
	}
	it.Name = ""
	if got := attentionNotification(it, false).Title; got != "mango:1 · tmux main:1 @ ws is asking" {
		t.Fatalf("unnamed push title %q, want its place", got)
	}
}

// TestTheStallPromptCarriesTheSessionsStableName: through the real sweep (pg
// fixture), the chief's stall check for a window whose conversation has a
// manual name names it by that name — the name rule's, not the codename.
func TestTheStallPromptCarriesTheSessionsStableName(t *testing.T) {
	f := newNudgeFixture(t, false)
	octx := asUser(authz.OperatorUserID)
	if _, err := focus.NewPG(f.pool).SetManualName(octx, focus.MirrorSubject(nudgeSID), "Release prep"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM agent_focus WHERE subject_key = $1`, nudgeSID)
	})
	if n := f.pass(t); n != 1 {
		t.Fatalf("decided %d, want the one stall", n)
	}
	f.gw.mu.Lock()
	prompts := append([]string(nil), f.gw.prompt...)
	f.gw.mu.Unlock()
	if len(prompts) != 1 || !strings.Contains(prompts[0], "session: Release prep") {
		t.Fatalf("the stall prompt does not name the session by its stable name: %q", prompts)
	}
}

// TestAnAgentsOutcomePushNamesItByItsStableName: an outcome's push and chief
// note label (agentLabel) is the agent's stable name — here its auto name over
// a slug-only display name — not its slug.
func TestAnAgentsOutcomePushNamesItByItsStableName(t *testing.T) {
	store := &memFocus{rows: map[focus.Subject]focus.Focus{
		focus.AgentSubject(7): {Subject: focus.AgentSubject(7), Title: "Fixing it", Name: focus.Name{Auto: "Billing migration"}},
	}}
	s, _ := focusMirrorServer(t, &fakeMirror{online: true}, store, nil)
	a := agents.Agent{ID: 7, Name: "brave-otter", DisplayName: "brave-otter"}
	if got := s.agentLabel(operatorCtx(), a); got != "Billing migration" {
		t.Fatalf("agentLabel = %q, want the stable name", got)
	}
}
