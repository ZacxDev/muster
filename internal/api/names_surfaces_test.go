package api

import (
	"context"
	"encoding/json"
	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/mirror"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/plugins"
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
// named session carries the name (withFocus), its Asks card shows the name
// with its place under it, and its push title leads with the name
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
	// The Asks card's head: the name as its title, the place (its tmux target,
	// the window's label cut off) small under it.
	head := attentionHeadLines(it, time.Now())
	if !strings.Contains(head, `data-attention-name>Billing migration</span>`) || !strings.Contains(head, `data-attention-source>tmux main:1 @ ws</span>`) {
		t.Fatalf("Asks row who = %s", head)
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

// TestAPRAuthorHintNamesTheSessionByItsStableName: the PR author hint a plugin
// message carries names its online session by its stable name once it has a
// human one; before, by its focus title or window title, as it always did.
func TestAPRAuthorHintNamesTheSessionByItsStableName(t *testing.T) {
	f := newPRFixture(t)
	fs := focus.NewPG(f.pool)
	f.srv.ext.Focus = fs
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM agent_focus WHERE subject_key = $1`, routeSID)
	})
	f.authored(t, f.asOwner, routeSID, prCreated())
	m, _, err := f.plugins.Submit(f.asOwner, f.agentMail.ID, plugins.MessageIn{ExternalID: "pr-name-hint-1", Sender: prSender,
		Subject: "review: Gizmo route", Body: reviewBody(fullFields()), SenderVerified: true})
	if err != nil {
		t.Fatal(err)
	}
	before := f.srv.prAuthorHint(f.asOwner, m)
	if before == nil || before.Name == "" || before.Name == "Gizmo release" {
		t.Fatalf("control: the hint before a name = %+v", before)
	}
	if _, err := fs.SetManualName(f.asOwner, focus.MirrorSubject(routeSID), "Gizmo release"); err != nil {
		t.Fatal(err)
	}
	if h := f.srv.prAuthorHint(f.asOwner, m); h == nil || h.Name != "Gizmo release" {
		t.Fatalf("the hint after the session was named = %+v, want its stable name", h)
	}
}

// nameNudgeSession gives the state/nudge fixture's window conversation
// (nudgeSID) a manual name, removed after the test.
func nameNudgeSession(t *testing.T, f *stateFixture, name string) {
	t.Helper()
	if _, err := focus.NewPG(f.pool).SetManualName(asUser(authz.OperatorUserID), focus.MirrorSubject(nudgeSID), name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM agent_focus WHERE subject_key = $1`, nudgeSID)
	})
}

// TestAWaitingEpisodesPushNamesTheSessionByItsName: through the real episode
// push (the hub hook, session state on), a named window's "needs you" push
// title is its stable name (episodeWho's human-name branch), not its place.
func TestAWaitingEpisodesPushNamesTheSessionByItsName(t *testing.T) {
	f := newStateFixture(t, false)
	nameNudgeSession(t, f, "Release prep")
	f.noGrace()
	f.waitWithSecrets()
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	f.oneEpisode(t)
	ps := f.payloads()
	if len(ps) != 1 || ps[0].Title != "Release prep needs you" {
		t.Fatalf("pushes %+v, want one titled by the session's name", ps)
	}
}

// TestAHeldQuestionsEpisodePushNamesTheSessionByItsName: an AskUserQuestion
// still open past its grace is pushed as the episode's ask (episodeAsk →
// attentionNotification, no withFocus in between); its title names the
// session by its stable name.
func TestAHeldQuestionsEpisodePushNamesTheSessionByItsName(t *testing.T) {
	f := newStateFixture(t, false)
	nameNudgeSession(t, f, "Release prep")
	f.asks(t)
	f.advance(61 * time.Second)
	f.statePass(t)
	ps := f.payloads()
	if len(ps) != 1 || ps[0].Title != "Release prep is asking" {
		t.Fatalf("pushes %+v, want the ask titled by the session's name", ps)
	}
}

// TestAHeldQuestionsDiffPushNamesTheSessionByItsName: with session state OFF,
// a held question is pushed by the queue's diff (attentionMirrorChanged →
// attentionNotify straight from waitingSessionsIn); its title names the
// session by its stable name.
func TestAHeldQuestionsDiffPushNamesTheSessionByItsName(t *testing.T) {
	f := newAttnFixture(t)
	if _, err := focus.NewPG(f.pool).SetManualName(asUser(1), focus.MirrorSubject(focusSID), "Ledger cleanup"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM agent_focus WHERE subject_key = $1`, focusSID)
	})
	clock := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	f.srv.now = func() time.Time { return clock }
	nudge := func() {
		f.srv.BroadcastAgentsChangedFor(1)
		f.srv.WaitForPushes()
	}
	f.setWindows(mirror.StatusIdle)
	nudge() // the host's first observation: nothing waits
	qs := []askq.Question{{Question: "Which ledger?", Options: []askq.Option{{Label: "main"}, {Label: "archive"}}}}
	if _, err := f.srv.Questions().Open(askq.Key{Owner: 1, Source: askSourceMirror, Scope: "ws", Session: focusSID, ToolUseID: "toolu_named"},
		qs, askq.NewID()); err != nil {
		t.Fatal(err)
	}
	f.setWindows(mirror.StatusWaiting)
	nudge()
	clock = clock.Add(EpisodeWaitingGrace + time.Second)
	nudge()
	var titles []string
	for _, p := range f.pusher.to(ep("attn-operator")) {
		if p.Type == "attention" {
			titles = append(titles, p.Title)
		}
	}
	if len(titles) != 1 || titles[0] != "Ledger cleanup is asking" {
		t.Fatalf("held question pushes %q, want one titled by the session's name", titles)
	}
}

// TestTheChiefsToolsNameAnAgentByItsStableName: through the real tools on a
// real store, an agent whose display name is only its slug and whose auto
// name froze is named by the auto name on its fleet_list row (with its slug
// as `code`) and on its focus_search hit.
func TestTheChiefsToolsNameAnAgentByItsStableName(t *testing.T) {
	f := newAttnFixture(t)
	slug := "fleet-name-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	a := f.agentFor(t, 1, slug, slug, "fleet-name-tok-"+slug)
	if _, err := focus.NewPG(f.pool).Upsert(asUser(1), focus.AgentSubject(a.ID), focus.Write{Title: "Reconciling the zebrafish ledger",
		Source: focus.SourceSummarizer}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE agent_focus SET name_auto = 'Zebrafish accounts', name_auto_at = now()
		WHERE subject_kind = 'agent' AND subject_key = $1`, strconv.FormatInt(a.ID, 10)); err != nil {
		t.Fatal(err)
	}
	ctx := asUser(1)
	var fl struct {
		Sessions []fleetListRow `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(f.srv.toolFleetList(ctx)), &fl); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range fl.Sessions {
		if r.ID == focus.AgentSubject(a.ID).String() {
			found = true
			if r.Name != "Zebrafish accounts" || r.Code != slug {
				t.Fatalf("fleet_list agent row: name %q code %q, want the auto name and the slug", r.Name, r.Code)
			}
		}
	}
	if !found {
		t.Fatalf("fleet_list has no row for agent %d", a.ID)
	}
	var fs struct {
		Results []struct {
			ID, Name string
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(f.srv.toolFocusSearch(ctx, `{"query":"zebrafish ledger"}`)), &fs); err != nil {
		t.Fatal(err)
	}
	if len(fs.Results) != 1 || fs.Results[0].ID != focus.AgentSubject(a.ID).String() || fs.Results[0].Name != "Zebrafish accounts" {
		t.Fatalf("focus_search: %+v, want the agent named by its auto name", fs.Results)
	}
}
