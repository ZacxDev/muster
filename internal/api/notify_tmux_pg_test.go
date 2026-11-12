package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/apitokens"
	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/notifypref"
	"github.com/ZacxDev/muster/internal/ui"
)

// "SHOW TMUX SESSION IN NOTIFICATIONS", end to end on a real Postgres: the
// stored preference (notify_preferences, row-level security) read on each
// real path that names a mirrored session in a notification — the episode
// push and its "Stalled / waiting" row, a held question's push (as an episode
// and as the queue's diff), a filed ask's push, the Asks list, the attention
// summary — and the Account page's switch.

// withTmuxPref wires the real notification-preferences store into f's server
// and, when on, turns the operator's switch on (removed after the test). Off
// leaves the store wired with NO row: the state of every existing user after
// migration 0094.
func withTmuxPref(t *testing.T, srv *Server, p *notifypref.PG, sys func(string, ...any), on bool) {
	t.Helper()
	srv.ext.NotifyPrefs = p
	sys(`DELETE FROM notify_preferences WHERE owner_id = $1`, authz.OperatorUserID)
	t.Cleanup(func() { sys(`DELETE FROM notify_preferences WHERE owner_id = $1`, authz.OperatorUserID) })
	if on {
		if err := p.SetTmuxAddress(asUser(authz.OperatorUserID), authz.OperatorUserID, true); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *stateFixture) tmuxPref(t *testing.T, on bool) {
	t.Helper()
	withTmuxPref(t, f.srv, notifypref.NewPG(f.pool), func(q string, a ...any) {
		_, _ = f.pool.Exec(authz.System(context.Background()), q, a...)
	}, on)
}

func (f *attnFixture) tmuxPref(t *testing.T, on bool) {
	t.Helper()
	withTmuxPref(t, f.srv, notifypref.NewPG(f.pool), func(q string, a ...any) {
		_, _ = f.pool.Exec(authz.System(context.Background()), q, a...)
	}, on)
}

// TestAWaitingEpisodesPushAndRowAddTheTmuxAddressWhenAsked: through the real
// episode push (the hub hook, session state on) and the Asks tab's "Stalled /
// waiting" row, a named window is "Release prep · main:1 @ ws" with the
// operator's switch on — and exactly "Release prep" with the store wired and
// no row (every existing user after 0094: off).
func TestAWaitingEpisodesPushAndRowAddTheTmuxAddressWhenAsked(t *testing.T) {
	for _, c := range []struct {
		on   bool
		want string
	}{{false, "Release prep"}, {true, "Release prep · Gold:1 @ ws"}} {
		t.Run(map[bool]string{true: "on", false: "off"}[c.on], func(t *testing.T) {
			f := newStateFixture(t, false)
			nameNudgeSession(t, f, "Release prep")
			f.tmuxPref(t, c.on)
			f.noGrace()
			f.waitWithSecrets()
			f.srv.AttentionMirrorChanged(authz.OperatorUserID)
			f.oneEpisode(t)
			ps := f.payloads()
			if len(ps) != 1 || ps[0].Title != c.want+" needs you" {
				t.Fatalf("pushes %+v, want one titled %q", ps, c.want+" needs you")
			}
			page := f.page(t, "/attention")
			if !strings.Contains(page, `data-attention-episode-where>`+c.want+`</span>`) {
				t.Fatalf("the Stalled / waiting row does not read %q:\n%s", c.want, episodeRows(page))
			}
		})
	}
}

// TestAHeldQuestionsEpisodePushAddsTheTmuxAddressWhenAsked: an AskUserQuestion
// pushed as the episode's ask (episodeAsk → attentionNotification, named in
// waitingSessionsIn) carries the address with the switch on, none with it off.
func TestAHeldQuestionsEpisodePushAddsTheTmuxAddressWhenAsked(t *testing.T) {
	for _, c := range []struct {
		on   bool
		want string
	}{{false, "Release prep is asking"}, {true, "Release prep · Gold:1 @ ws is asking"}} {
		t.Run(map[bool]string{true: "on", false: "off"}[c.on], func(t *testing.T) {
			f := newStateFixture(t, false)
			nameNudgeSession(t, f, "Release prep")
			f.tmuxPref(t, c.on)
			f.asks(t)
			f.advance(61 * time.Second)
			f.statePass(t)
			ps := f.payloads()
			if len(ps) != 1 || ps[0].Title != c.want {
				t.Fatalf("pushes %+v, want the ask titled %q", ps, c.want)
			}
		})
	}
}

// TestAHeldQuestionsDiffPushAddsTheTmuxAddressWhenAsked: with session state
// OFF, the queue's diff pushes a held question straight from
// waitingSessionsIn; with the switch on its title carries the address.
func TestAHeldQuestionsDiffPushAddsTheTmuxAddressWhenAsked(t *testing.T) {
	f := newAttnFixture(t)
	f.tmuxPref(t, true)
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
	nudge()
	qs := []askq.Question{{Question: "Which ledger?", Options: []askq.Option{{Label: "main"}, {Label: "archive"}}}}
	if _, err := f.srv.Questions().Open(askq.Key{Owner: 1, Source: askSourceMirror, Scope: "ws", Session: focusSID, ToolUseID: "toolu_tmux"},
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
	if len(titles) != 1 || titles[0] != "Ledger cleanup · main:1 @ ws is asking" {
		t.Fatalf("held question pushes %q, want one with the address", titles)
	}
}

// TestAWaitingSessionsListRowAndSummaryAddTheTmuxAddressWhenAsked: a named
// mirrored session waiting on its owner, on the Asks list (GET /attention) and
// the status bar's summary (GET /api/attention/summary, the bar's API token):
// with the switch off — the store wired, no row — each names it "Ledger
// cleanup" exactly as before; on, each reads "Ledger cleanup · main:1 @ ws".
func TestAWaitingSessionsListRowAndSummaryAddTheTmuxAddressWhenAsked(t *testing.T) {
	f := newSummaryFixture(t)
	tok, _, err := apitokens.NewPG(f.pool).Create(asUser(1), "bar-tmux", 0, apitokens.ScopeFull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM api_tokens WHERE user_id = 1 AND name = 'bar-tmux'`)
	})
	if _, err := focus.NewPG(f.pool).SetManualName(asUser(1), focus.MirrorSubject(focusSID), "Ledger cleanup"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM agent_focus WHERE subject_key = $1`, focusSID)
	})
	f.setWindows(mirror.StatusWaiting) // window @1 (focusSID) waits on its owner
	for _, c := range []struct {
		on   bool
		want string
	}{{false, "Ledger cleanup"}, {true, "Ledger cleanup · main:1 @ ws"}} {
		f.tmuxPref(t, c.on)
		page := f.page(t, "/attention")
		if !strings.Contains(page, `data-attention-title data-attention-name>`+c.want+`</span>`) || !strings.Contains(page, `data-attention-source>tmux main:1 @ ws</span>`) {
			i := strings.Index(page, "data-attention-source")
			t.Fatalf("on=%v: the Asks list does not name the session %q:\n%s", c.on, c.want, page[max(0, i-200):min(len(page), max(0, i)+400)])
		}
		code, sum, raw := summaryAs(t, f, tok)
		if code != http.StatusOK {
			t.Fatalf("summary: %d %s", code, raw)
		}
		var titles []string
		for _, it := range sum.Items {
			if it.Kind == attentionKindSession {
				titles = append(titles, it.Title)
			}
		}
		if len(titles) != 1 || titles[0] != c.want {
			t.Fatalf("on=%v: the summary's waiting sessions %q, want one titled %q", c.on, titles, c.want)
		}
	}
}

// TestAMirroredAsksPushAndRowAreTitledByTheSessionsName (regression): an
// agent question from a NAMED mirrored session, filed through the real path
// (askFiled: its push) and drawn on the Asks list (GET /attention), is titled
// by the session's stable name like every other notification — "Ledger
// cleanup is asking", its place in the row's tooltip — and with the switch on
// it is followed by the window's codename and host, "Ledger cleanup · Gold:1
// @ ws". Before, both named the window's place whatever its name.
func TestAMirroredAsksPushAndRowAreTitledByTheSessionsName(t *testing.T) {
	f := newAttnFixture(t)
	if _, err := focus.NewPG(f.pool).SetManualName(asUser(1), focus.MirrorSubject(focusSID), "Ledger cleanup"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM agent_focus WHERE subject_key = $1`, focusSID)
	})
	ws := attnWindows(mirror.StatusIdle)
	ws[0].Label = "Gold:1"
	f.mirror.mu.Lock()
	f.mirror.windows = ws
	f.mirror.mu.Unlock()
	for _, c := range []struct {
		on   bool
		want string
	}{{false, "Ledger cleanup"}, {true, "Ledger cleanup · Gold:1 @ ws"}} {
		t.Run(map[bool]string{true: "on", false: "off"}[c.on], func(t *testing.T) {
			f.tmuxPref(t, c.on)
			f.pusher.reset()
			a := f.mirrorAsk(t, "Ship the ledger fix?")
			f.srv.askFiled(a)
			f.srv.WaitForPushes()
			var titles []string
			for _, p := range f.pusher.to(ep("attn-operator")) {
				if p.Type == "attention" && p.Tag == attentionTag(attentionAskKey(a.ID)) {
					titles = append(titles, p.Title)
				}
			}
			if len(titles) != 1 || titles[0] != c.want+" is asking" {
				t.Fatalf("the ask's push titles %q, want one %q", titles, c.want+" is asking")
			}
			page := f.page(t, "/attention")
			row := `data-attention-title data-attention-name>` + c.want + `</span>`
			if !strings.Contains(page, row) || !strings.Contains(page, `data-attention-source>tmux main:1 @ ws</span>`) {
				i := strings.Index(page, "data-attention-source")
				t.Fatalf("the Asks list does not title the ask %q:\n%s", c.want, page[max(0, i-100):min(len(page), max(0, i)+300)])
			}
		})
	}
}

// TestTheBarsPillsCarryTheCodenameByTheViewersOwnSwitch: on real Postgres
// (row-level security on the switch), an agent page's switcher bar — the pills
// the dock copies — carries a named window's codename (data-switcher-tmux,
// and the pill's codename line) exactly when the VIEWER turned the switch on:
// default off; a member's switch on does not reach the operator; the
// operator's on does. A window with no human name never carries it.
func TestTheBarsPillsCarryTheCodenameByTheViewersOwnSwitch(t *testing.T) {
	f := newAttnFixture(t)
	if _, err := focus.NewPG(f.pool).SetManualName(asUser(1), focus.MirrorSubject(focusSID), "Ledger cleanup"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM agent_focus WHERE subject_key = $1`, focusSID)
	})
	ws := attnWindows(mirror.StatusIdle)
	ws[0].Label = "Gold:1"
	f.mirror.mu.Lock()
	f.mirror.windows = ws
	f.mirror.mu.Unlock()
	member := f.newMember(t)
	p := notifypref.NewPG(f.pool)
	f.srv.ext.NotifyPrefs = p
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM notify_preferences WHERE owner_id = ANY($1)`, []int64{1, member})
	})
	bar := func() string {
		t.Helper()
		page := f.pageFollow(t, "/agents/"+f.op.Name)
		i := strings.Index(page, `id="switcher-list-all"`)
		if i < 0 {
			t.Fatal("the agent page draws no switcher bar")
		}
		return page[i:min(len(page), i+20000)]
	}
	const on = `data-switcher-tmux="Gold:1 @ ws"`
	if b := bar(); strings.Contains(b, "data-switcher-tmux") || !strings.Contains(b, `data-switcher-name="Ledger cleanup"`) {
		t.Fatalf("default: the bar carries a codename (or lost the named pill):\n%s", truncate(b, 1500))
	}
	if err := p.SetTmuxAddress(asUser(member), member, true); err != nil {
		t.Fatal(err)
	}
	if b := bar(); strings.Contains(b, "data-switcher-tmux") {
		t.Fatal("a member's switch reached the operator's bar")
	}
	if err := p.SetTmuxAddress(asUser(1), 1, true); err != nil {
		t.Fatal(err)
	}
	b := bar()
	if !strings.Contains(b, on) || !strings.Contains(b, `data-switcher-tmux-line="" class="`+ui.SwitcherTmuxLineClass+`">Gold:1</span>`) {
		t.Fatalf("on: the named pill has no codename:\n%s", truncate(b, 2000))
	}
	// Window @2 has no human name: called by its place, so never twice.
	if n := strings.Count(b, "data-switcher-tmux="); n != 1 {
		t.Fatalf("on: %d pills carry a codename, want only the named one", n)
	}
}

// TestTheSidebarRowsCarryTheCodenameByTheViewersOwnSwitch: on real Postgres
// (row-level security on the switch), the sidebar's rows — GET
// ui.NavAgentsPath, the partial every SSE/htmx refresh of the sidebar
// re-fetches, and what the command palette reads on a page with no bar —
// draw a named window's place (its data-nav-tmux line, and data-switcher-tmux
// for the palette's row) exactly when the VIEWER turned the switch on: default
// off; a member's switch on does not reach the operator's rows; the operator's
// on does, and is read again on the next fetch (the refresh). A window with no
// human name, and the agent's row, never carry it.
func TestTheSidebarRowsCarryTheCodenameByTheViewersOwnSwitch(t *testing.T) {
	f := newAttnFixture(t)
	if _, err := focus.NewPG(f.pool).SetManualName(asUser(1), focus.MirrorSubject(focusSID), "Ledger cleanup"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM agent_focus WHERE subject_key = $1`, focusSID)
	})
	ws := attnWindows(mirror.StatusIdle)
	ws[0].Label = "Gold:1"
	f.mirror.mu.Lock()
	f.mirror.windows = ws
	f.mirror.mu.Unlock()
	member := f.newMember(t)
	p := notifypref.NewPG(f.pool)
	f.srv.ext.NotifyPrefs = p
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM notify_preferences WHERE owner_id = ANY($1)`, []int64{1, member})
	})
	rows := func() string { t.Helper(); return f.page(t, ui.NavAgentsPath) }
	const attr, line = `data-switcher-tmux="Gold:1 @ ws"`, `<span data-nav-tmux="" class="min-w-0 truncate text-xs font-normal text-muted">Gold:1 @ ws</span>`

	off := rows()
	if strings.Contains(off, "data-switcher-tmux") || strings.Contains(off, "data-nav-tmux") || !strings.Contains(off, `data-switcher-name="Ledger cleanup"`) {
		t.Fatalf("default: the rows carry a codename (or lost the named row):\n%s", truncate(off, 2000))
	}
	if err := p.SetTmuxAddress(asUser(member), member, true); err != nil {
		t.Fatal(err)
	}
	if r := rows(); r != off {
		t.Fatalf("a member's switch changed the operator's rows:\n got  %s\n want %s", truncate(r, 2000), truncate(off, 2000))
	}
	if err := p.SetTmuxAddress(asUser(1), 1, true); err != nil {
		t.Fatal(err)
	}
	on := rows()
	if !strings.Contains(on, attr) || !strings.Contains(on, line) {
		t.Fatalf("on: the named row has no place:\n%s", truncate(on, 3000))
	}
	// Exactly one row: the named window. Window @2 is called by its place,
	// and the operator's agent has no tmux window.
	if a, l := strings.Count(on, "data-switcher-tmux="), strings.Count(on, "data-nav-tmux="); a != 1 || l != 1 {
		t.Fatalf("on: %d rows carry the attribute and %d the line, want only the named one", a, l)
	}
	if !strings.Contains(on, `id="`+agentNavID(f.op.ID)+`"`) {
		t.Fatal("control: the agent's row is not in the list")
	}
	// The next fetch — an SSE nudge or the minute's tick re-fetching the
	// partial — reads the switch again: off again, gone.
	if err := p.SetTmuxAddress(asUser(1), 1, false); err != nil {
		t.Fatal(err)
	}
	if r := rows(); r != off {
		t.Fatalf("off again: the rows are not the default's:\n got  %s\n want %s", truncate(r, 2000), truncate(off, 2000))
	}
}

// TestTheAccountSwitchIsOffForEveryoneAndPersistsPerUser: on real Postgres
// (row-level security, the session tier), a member and the operator who never
// chose both read OFF on the Account page; the member turns it on — stored as
// theirs, read back on a fresh load, the operator still off — and off again.
func TestTheAccountSwitchIsOffForEveryoneAndPersistsPerUser(t *testing.T) {
	w := newIsoWorld(t)
	b := cred{sessionUser: w.b.uid}
	a := cred{sessionUser: w.a.uid}
	t.Cleanup(func() {
		_, _ = w.pool.Exec(w.sys, `DELETE FROM notify_preferences WHERE owner_id = ANY($1)`, []int64{w.a.uid, w.b.uid})
	})
	state := func(c cred) string {
		body := w.do(w.h, "GET", "/account", "", c).Body.String()
		const attr = `data-testid="notify-tmux" data-state="`
		i := strings.Index(body, attr)
		if i < 0 {
			t.Fatalf("the Account page has no tmux-session switch:\n%s", truncate(body, 600))
		}
		rest := body[i+len(attr):]
		return rest[:strings.Index(rest, `"`)]
	}
	if got := state(a); got != "off" {
		t.Fatalf("A by default: %q, want off", got)
	}
	if got := state(b); got != "off" {
		t.Fatalf("B by default: %q, want off", got)
	}
	if !strings.Contains(w.do(w.h, "GET", "/account", "", b).Body.String(), notifyTmuxLabel) {
		t.Fatal("the switch is not labelled in plain words")
	}
	rec := w.do(w.h, "POST", accountNotifyTmuxPath, "on=1", b)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/account?notify=saved#notify-tmux" {
		t.Fatalf("on: %d → %q", rec.Code, rec.Header().Get("Location"))
	}
	if got := state(b); got != "on" {
		t.Fatalf("B after on: %q", got)
	}
	var on bool
	if err := w.pool.QueryRow(w.sys, `SELECT tmux_address FROM notify_preferences WHERE owner_id = $1`, w.b.uid).Scan(&on); err != nil || !on {
		t.Fatalf("B's stored row: on=%v err=%v", on, err)
	}
	if got := state(a); got != "off" {
		t.Fatalf("B's switch reached A: %q", got)
	}
	if rec := w.do(w.h, "POST", accountNotifyTmuxPath, "on=maybe", b); rec.Code != http.StatusBadRequest {
		t.Fatalf("a bad value: %d", rec.Code)
	}
	if rec := w.do(w.h, "POST", accountNotifyTmuxPath, "on=0", b); rec.Code != http.StatusSeeOther {
		t.Fatalf("off: %d", rec.Code)
	}
	if got := state(b); got != "off" {
		t.Fatalf("B after off: %q", got)
	}
}
