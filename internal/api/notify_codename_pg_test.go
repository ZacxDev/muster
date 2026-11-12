package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/notifypref"
)

// "START NOTIFICATION TITLES WITH THE TMUX NAME", end to end on a real
// Postgres: the stored preference (notify_preferences.codename_first,
// migration 0117, row-level security) read by the real push title sink
// (goNotify, under the owner's attention principal) on each real path whose
// push names a mirrored session — the episode push, a held question's push
// (as an episode and as the queue's diff), the waiting window's own push, a
// filed ask's push — and the Account page's switch. The list decorations of
// "Show tmux sessions" are unchanged by it.

// withNotifyPrefs wires the real notification-preferences store into srv and
// sets the operator's two switches (the row is removed after the test). Both
// off leaves NO row: every existing user after migration 0117.
func withNotifyPrefs(t *testing.T, srv *Server, p *notifypref.PG, sys func(string, ...any), tmux, first bool) {
	t.Helper()
	srv.ext.NotifyPrefs = p
	sys(`DELETE FROM notify_preferences WHERE owner_id = $1`, authz.OperatorUserID)
	t.Cleanup(func() { sys(`DELETE FROM notify_preferences WHERE owner_id = $1`, authz.OperatorUserID) })
	op := asUser(authz.OperatorUserID)
	if tmux {
		if err := p.SetTmuxAddress(op, authz.OperatorUserID, true); err != nil {
			t.Fatal(err)
		}
	}
	if first {
		if err := p.SetCodenameFirst(op, authz.OperatorUserID, true); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *stateFixture) notifyPrefs(t *testing.T, tmux, first bool) *notifypref.PG {
	t.Helper()
	p := notifypref.NewPG(f.pool)
	withNotifyPrefs(t, f.srv, p, func(q string, a ...any) {
		_, _ = f.pool.Exec(authz.System(context.Background()), q, a...)
	}, tmux, first)
	return p
}

func (f *attnFixture) notifyPrefs(t *testing.T, tmux, first bool) *notifypref.PG {
	t.Helper()
	p := notifypref.NewPG(f.pool)
	withNotifyPrefs(t, f.srv, p, func(q string, a ...any) {
		_, _ = f.pool.Exec(authz.System(context.Background()), q, a...)
	}, tmux, first)
	return p
}

// switchCases is the two switches' four states, by name.
var switchCases = []struct {
	name        string
	tmux, first bool
}{
	{"both off", false, false},
	{"codename first only", false, true},
	{"show tmux sessions only", true, false},
	{"both on", true, true},
}

// TestAnEpisodesPushStartsWithTheCodenameWhenAsked: the real episode push of a
// named window ("… needs you") under each state of the two switches — the
// codename leads exactly when codename-first is on, ONCE with both on — and its
// "Stalled / waiting" row is "Show tmux sessions"' alone (not a push title).
func TestAnEpisodesPushStartsWithTheCodenameWhenAsked(t *testing.T) {
	want := map[string][2]string{ // push title, row
		"both off":                {"Release prep needs you", "Release prep"},
		"codename first only":     {"Gold:1 · Release prep needs you", "Release prep"},
		"show tmux sessions only": {"Release prep · Gold:1 @ ws needs you", "Release prep · Gold:1 @ ws"},
		"both on":                 {"Gold:1 · Release prep needs you", "Release prep · Gold:1 @ ws"},
	}
	for _, c := range switchCases {
		t.Run(c.name, func(t *testing.T) {
			f := newStateFixture(t, false)
			nameNudgeSession(t, f, "Release prep")
			f.notifyPrefs(t, c.tmux, c.first)
			f.noGrace()
			f.waitWithSecrets()
			f.srv.AttentionMirrorChanged(authz.OperatorUserID)
			f.oneEpisode(t)
			ps := f.payloads()
			if len(ps) != 1 || ps[0].Title != want[c.name][0] {
				t.Fatalf("pushes %+v, want one titled %q", ps, want[c.name][0])
			}
			page := f.page(t, "/attention")
			if !strings.Contains(page, `data-attention-episode-where>`+want[c.name][1]+`</code>`) {
				t.Fatalf("the Stalled / waiting row does not read %q:\n%s", want[c.name][1], episodeRows(page))
			}
		})
	}
}

// TestAMembersCodenameSwitchDoesNotReachTheOperatorsPush: row-level security
// and the sink's own principal: a member's switch on leaves the operator's
// push as it was (the control: the operator's own switch on changes it).
func TestAMembersCodenameSwitchDoesNotReachTheOperatorsPush(t *testing.T) {
	f := newStateFixture(t, false)
	nameNudgeSession(t, f, "Release prep")
	p := f.notifyPrefs(t, false, false)
	sys := authz.System(context.Background())
	var member int64
	if err := f.pool.QueryRow(sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`,
		"codename-"+strconv.FormatInt(time.Now().UnixNano(), 36)).Scan(&member); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(sys, `DELETE FROM notify_preferences WHERE owner_id = $1`, member)
		_, _ = f.pool.Exec(sys, `DELETE FROM users WHERE id = $1`, member)
	})
	if err := p.SetCodenameFirst(asUser(member), member, true); err != nil {
		t.Fatal(err)
	}
	if on, err := p.CodenameFirst(asUser(member), member); err != nil || !on {
		t.Fatalf("control: the member's switch reads on=%v err=%v", on, err)
	}
	f.noGrace()
	f.waitWithSecrets()
	f.srv.AttentionMirrorChanged(authz.OperatorUserID)
	f.oneEpisode(t)
	if ps := f.payloads(); len(ps) != 1 || ps[0].Title != "Release prep needs you" {
		t.Fatalf("pushes %+v, want the operator's unchanged title", ps)
	}
}

// TestAHeldQuestionsEpisodePushStartsWithTheCodenameWhenAsked: an
// AskUserQuestion pushed as the episode's ask (episodeAsk →
// attentionNotification) leads with the codename, once with both switches on.
func TestAHeldQuestionsEpisodePushStartsWithTheCodenameWhenAsked(t *testing.T) {
	want := map[string]string{
		"both off":                "Release prep is asking",
		"codename first only":     "Gold:1 · Release prep is asking",
		"show tmux sessions only": "Release prep · Gold:1 @ ws is asking",
		"both on":                 "Gold:1 · Release prep is asking",
	}
	for _, c := range switchCases {
		t.Run(c.name, func(t *testing.T) {
			f := newStateFixture(t, false)
			nameNudgeSession(t, f, "Release prep")
			f.notifyPrefs(t, c.tmux, c.first)
			f.asks(t)
			f.advance(61 * time.Second)
			f.statePass(t)
			if ps := f.payloads(); len(ps) != 1 || ps[0].Title != want[c.name] {
				t.Fatalf("pushes %+v, want the ask titled %q", ps, want[c.name])
			}
		})
	}
}

// diffPushTitles drives the queue's diff (session state OFF) through a
// window that starts waiting, past its grace, and returns the attention
// pushes' titles; question, when non-nil, is a held AskUserQuestion opened
// first.
func diffPushTitles(t *testing.T, f *attnFixture, question []askq.Question) []string {
	t.Helper()
	clock := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	f.srv.now = func() time.Time { return clock }
	nudge := func() {
		f.srv.BroadcastAgentsChangedFor(1)
		f.srv.WaitForPushes()
	}
	f.setWindows(mirror.StatusIdle)
	nudge()
	if question != nil {
		if _, err := f.srv.Questions().Open(askq.Key{Owner: 1, Source: askSourceMirror, Scope: "ws", Session: focusSID, ToolUseID: "toolu_codename"},
			question, askq.NewID()); err != nil {
			t.Fatal(err)
		}
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
	return titles
}

// nameFocusSID gives the attention fixture's window @1 conversation a manual
// name, removed after the test.
func nameFocusSID(t *testing.T, f *attnFixture, name string) {
	t.Helper()
	if _, err := focus.NewPG(f.pool).SetManualName(asUser(1), focus.MirrorSubject(focusSID), name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM agent_focus WHERE subject_key = $1`, focusSID)
	})
}

// TestAHeldQuestionsDiffPushStartsWithTheCodenameWhenAsked: with session state
// OFF the queue's diff pushes a held question straight from waitingSessionsIn;
// with both switches on its title leads with the window's codename (its tmux
// address: this host sends no label), said once.
func TestAHeldQuestionsDiffPushStartsWithTheCodenameWhenAsked(t *testing.T) {
	f := newAttnFixture(t)
	f.notifyPrefs(t, true, true)
	nameFocusSID(t, f, "Ledger cleanup")
	titles := diffPushTitles(t, f, []askq.Question{{Question: "Which ledger?", Options: []askq.Option{{Label: "main"}, {Label: "archive"}}}})
	if len(titles) != 1 || titles[0] != "main:1 · Ledger cleanup is asking" {
		t.Fatalf("held question pushes %q, want one led by its codename", titles)
	}
}

// TestAWaitingWindowsOwnPushStartsWithTheCodenameWhenAsked: the waiting
// window's own push keeps its fixed title, which the switch puts the codename
// in front of — named or not, since the fixed title names no session. Off,
// byte for byte today's fixed title.
func TestAWaitingWindowsOwnPushStartsWithTheCodenameWhenAsked(t *testing.T) {
	for _, c := range []struct {
		first bool
		want  string
	}{{false, sessionPushTitle}, {true, "main:1 · A session is waiting on you"}} {
		t.Run(map[bool]string{true: "on", false: "off"}[c.first], func(t *testing.T) {
			f := newAttnFixture(t)
			f.notifyPrefs(t, false, c.first)
			if titles := diffPushTitles(t, f, nil); len(titles) != 1 || titles[0] != c.want {
				t.Fatalf("waiting pushes %q, want one %q", titles, c.want)
			}
		})
	}
}

// TestAFiledAsksPushStartsWithTheCodenameWhileTheAsksRowKeepsItsOwn: an agent
// question from a named, labelled mirrored session, filed through the real
// path (askFiled): with both switches on its push leads with the codename,
// once — while the Asks list row is "Show tmux sessions"' text, unchanged by
// the codename switch (a list, not a push title).
func TestAFiledAsksPushStartsWithTheCodenameWhileTheAsksRowKeepsItsOwn(t *testing.T) {
	f := newAttnFixture(t)
	nameFocusSID(t, f, "Ledger cleanup")
	ws := attnWindows(mirror.StatusIdle)
	ws[0].Label = "Gold:1"
	f.mirror.mu.Lock()
	f.mirror.windows = ws
	f.mirror.mu.Unlock()
	for _, c := range switchCases {
		t.Run(c.name, func(t *testing.T) {
			f.notifyPrefs(t, c.tmux, c.first)
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
			name := map[bool]string{false: "Ledger cleanup", true: "Ledger cleanup · Gold:1 @ ws"}[c.tmux]
			want := name + " is asking"
			if c.first {
				want = "Gold:1 · Ledger cleanup is asking"
			}
			if len(titles) != 1 || titles[0] != want {
				t.Fatalf("the ask's push titles %q, want one %q", titles, want)
			}
			row := `<span data-attention-source data-attention-name title="Gold:1 · tmux main:1 @ ws">` + name + `</span>`
			if page := f.page(t, "/attention"); !strings.Contains(page, row) {
				i := strings.Index(page, "data-attention-source")
				t.Fatalf("the Asks row is not %q:\n%s", name, page[max(0, i-100):min(len(page), max(0, i)+300)])
			}
		})
	}
}

// TestTheCodenameSwitchIsOffForEveryoneAndPersistsPerUser: on real Postgres
// (row-level security, the session tier), the Account page draws the second
// switch OFF for a member and the operator who never chose; the member turns
// it on — stored as theirs, read back on a fresh load, the operator still off,
// and "Show tmux sessions" untouched — and off again.
func TestTheCodenameSwitchIsOffForEveryoneAndPersistsPerUser(t *testing.T) {
	w := newIsoWorld(t)
	b := cred{sessionUser: w.b.uid}
	a := cred{sessionUser: w.a.uid}
	t.Cleanup(func() {
		_, _ = w.pool.Exec(w.sys, `DELETE FROM notify_preferences WHERE owner_id = ANY($1)`, []int64{w.a.uid, w.b.uid})
	})
	state := func(c cred, id string) string {
		body := w.do(w.h, "GET", "/account", "", c).Body.String()
		attr := `data-testid="` + id + `" data-state="`
		i := strings.Index(body, attr)
		if i < 0 {
			t.Fatalf("the Account page has no %s switch:\n%s", id, truncate(body, 600))
		}
		rest := body[i+len(attr):]
		return rest[:strings.Index(rest, `"`)]
	}
	for _, c := range []cred{a, b} {
		if got := state(c, "notify-codename"); got != "off" {
			t.Fatalf("by default: %q, want off", got)
		}
	}
	page := w.do(w.h, "GET", "/account", "", b).Body.String()
	if !strings.Contains(page, notifyCodenameLabel) || !strings.Contains(page, notifyCodenameHelp) {
		t.Fatal("the switch is not labelled in plain words")
	}
	if strings.Index(page, `id="notify-tmux"`) > strings.Index(page, `id="notify-codename"`) {
		t.Fatal("the codename switch is drawn before \"Show tmux sessions\"")
	}
	rec := w.do(w.h, "POST", accountNotifyCodenamePath, "on=1", b)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/account?notify=codename-saved#notify-codename" {
		t.Fatalf("on: %d → %q", rec.Code, rec.Header().Get("Location"))
	}
	if got := state(b, "notify-codename"); got != "on" {
		t.Fatalf("B after on: %q", got)
	}
	if got := state(b, "notify-tmux"); got != "off" {
		t.Fatalf("turning the codename switch on turned \"Show tmux sessions\" %q", got)
	}
	saved := w.do(w.h, "GET", "/account?notify=codename-saved", "", b).Body.String()
	if i, j := strings.Index(saved, "Saved."), strings.Index(saved, `id="notify-codename"`); i < 0 || i < j || strings.Count(saved, "Saved.") != 1 {
		t.Fatal("the save's \"Saved.\" is not drawn once, under the codename switch")
	}
	var first, tmux bool
	if err := w.pool.QueryRow(w.sys, `SELECT codename_first, tmux_address FROM notify_preferences WHERE owner_id = $1`, w.b.uid).Scan(&first, &tmux); err != nil || !first || tmux {
		t.Fatalf("B's stored row: codename_first=%v tmux_address=%v err=%v", first, tmux, err)
	}
	if got := state(a, "notify-codename"); got != "off" {
		t.Fatalf("B's switch reached A: %q", got)
	}
	// "Show tmux sessions" turned on afterwards leaves the codename switch on.
	if rec := w.do(w.h, "POST", accountNotifyTmuxPath, "on=1", b); rec.Code != http.StatusSeeOther {
		t.Fatalf("tmux on: %d", rec.Code)
	}
	if got := state(b, "notify-codename"); got != "on" {
		t.Fatalf("turning \"Show tmux sessions\" on turned the codename switch %q", got)
	}
	if rec := w.do(w.h, "POST", accountNotifyCodenamePath, "on=maybe", b); rec.Code != http.StatusBadRequest {
		t.Fatalf("a bad value: %d", rec.Code)
	}
	if rec := w.do(w.h, "POST", accountNotifyCodenamePath, "on=0", b); rec.Code != http.StatusSeeOther {
		t.Fatalf("off: %d", rec.Code)
	}
	if got, tm := state(b, "notify-codename"), state(b, "notify-tmux"); got != "off" || tm != "on" {
		t.Fatalf("B after off: codename %q tmux %q, want off and on", got, tm)
	}
}
