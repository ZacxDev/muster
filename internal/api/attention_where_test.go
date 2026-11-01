package api

import (
	"context"
	"html"
	"io"
	"log"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/ui"
)

// WHERE AN ATTENTION TEXT SENDS THE OPERATOR: every surface that names a
// mirrored window names its exact tmux target, "<session>:<index>" on its
// host, led by the host's codename when it has one (mirror.Where). Before,
// they named the codename alone ("mango:1 @ ws", which is no tmux name) or the
// session alone with no window ("build @ ws").
//
// The fixture's fields are pairwise distinct so a target built from the wrong
// one cannot pass: the labelled window is tmux session "scratch3", window id
// "@37", index 5, codename "mango:1" (its codename's index is NOT its tmux
// index); the unlabelled one is session "build", id "@4", index 7.
const (
	whereLabelledSID   = "5a1b2c3d-1111-4111-8111-000000000037"
	whereUnlabelledSID = "5a1b2c3d-2222-4222-8222-000000000004"
	whereGoneSID       = "5a1b2c3d-3333-4333-8333-000000000099"
)

func whereServer(t *testing.T, asks ...attention.Ask) *Server {
	t.Helper()
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	fm := &fakeMirror{online: true, interactive: true, statusReport: true, v2: &fakeMirrorV2{}, windows: []mirror.Window{
		{Session: "scratch3", ID: "@37", Index: 5, Active: true, Label: "mango:1", Claude: true, ClaudeSession: whereLabelledSID, Status: mirror.StatusWaiting, StatusSince: attnSince},
		{Session: "build", ID: "@4", Index: 7, Active: true, Claude: true, ClaudeSession: whereUnlabelledSID, Status: mirror.StatusWaiting, StatusSince: attnSince},
	}}
	ext := Extensions{Users: stubUsers{}, Mirror: fm}
	if asks != nil {
		ext.Attention = openAsks{asks: asks}
	}
	s.UseExtensions(ext)
	return s
}

// whereSessionItem is the waiting-session item of window id.
func whereSessionItem(t *testing.T, s *Server, session, window string) attentionItem {
	t.Helper()
	for _, it := range s.waitingSessions(s.attentionPrincipal(context.Background(), 1)) {
		if it.Key == sessionKey("ws", session, window) {
			return it
		}
	}
	t.Fatalf("window %s:%s is not waiting", session, window)
	return attentionItem{}
}

// TestANudgeAskNamesItsWindowsExactTmuxTarget: a chief nudge's ask, built from
// the sweep's own candidate (stallInputs carries the window), opens with the
// tmux session and index — the codename first when the host gave one.
// nudgeAskPlace is the whole of what nudgeAskQuestion says of where the
// session is (after its focus title), pinned here whole; the live-ask PG test
// (TestChiefNudgeLiveAskIsAnsweredAndRelayedVerbatim) pins the whole question.
func TestANudgeAskNamesItsWindowsExactTmuxTarget(t *testing.T) {
	s := whereServer(t)
	byKey := map[string]chiefnudge.Session{}
	for _, c := range s.stallInputs(s.attentionPrincipal(context.Background(), 1), nil) {
		byKey[c.Session.Key] = c.Session
	}
	if len(byKey) != 2 {
		t.Fatalf("fixture: %d candidates, want 2", len(byKey))
	}
	for _, c := range []struct {
		name, sid, want string
	}{
		{"a window with a codename", whereLabelledSID, " (mango:1 · tmux scratch3:5 @ ws)"},
		{"a window without a codename", whereUnlabelledSID, " (tmux build:7 @ ws)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := nudgeAskPlace(byKey[c.sid]); got != c.want {
				t.Fatalf("ask place:\n%q\nwant:\n%q", got, c.want)
			}
		})
	}
}

// TestAnAgentNudgeAskKeepsItsAgentName: an agent has no tmux window; its ask
// still names it "(agent <name>)". An INVARIANT guard (it held before too).
func TestAnAgentNudgeAskKeepsItsAgentName(t *testing.T) {
	c := chiefnudge.Session{Kind: chiefnudge.KindAgent, Label: "Worker Seven", Title: "Fix CI"}
	if got, want := nudgeAskPlace(c), " (agent Worker Seven)"; got != want {
		t.Fatalf("ask place %q, want %q", got, want)
	}
}

// TestAWaitingSessionAndItsPushNameTheExactTmuxTarget: the queue's waiting
// session item and its notification body.
func TestAWaitingSessionAndItsPushNameTheExactTmuxTarget(t *testing.T) {
	s := whereServer(t)
	for _, c := range []struct {
		session, window, source, body string
	}{
		{"scratch3", "@37", "mango:1 · tmux scratch3:5 @ ws", "Waiting: mango:1 · tmux scratch3:5 @ ws"},
		{"build", "@4", "tmux build:7 @ ws", "Waiting: tmux build:7 @ ws"},
	} {
		it := whereSessionItem(t, s, c.session, c.window)
		if it.Source != c.source {
			t.Errorf("%s: source %q, want %q", c.window, it.Source, c.source)
		}
		if got := attentionPushBody(it); got != c.body {
			t.Errorf("%s: push body %q, want %q", c.window, got, c.body)
		}
	}
}

// whereAsks are a mirrored session's asks: one whose Claude session runs in
// the labelled window, one in the unlabelled window, one no window runs, and
// an agent's.
func whereAsks() []attention.Ask {
	now := time.Now()
	return []attention.Ask{
		{ID: 91, Owner: 1, Asker: attention.Asker{MirrorHost: "ws", MirrorSession: whereLabelledSID, Label: "mango:1 @ ws"}, Question: "Which branch?", CreatedAt: now},
		{ID: 92, Owner: 1, Asker: attention.Asker{MirrorHost: "ws", MirrorSession: whereUnlabelledSID, Label: "build:7 @ ws"}, Question: "Which branch?", CreatedAt: now},
		{ID: 93, Owner: 1, Asker: attention.Asker{MirrorHost: "ws", MirrorSession: whereGoneSID, Label: "pear:2 @ ws"}, Question: "Which branch?", CreatedAt: now},
		{ID: 94, Owner: 1, Asker: attention.Asker{AgentID: 7, Label: "Worker Seven"}, Question: "Which branch?", CreatedAt: now},
	}
}

// whereAskSources are whereAsks' sources, by id. 93's window is gone: its
// stored name, and that no window shows it — never a fabricated target.
var whereAskSources = map[int64]string{
	91: "mango:1 · tmux scratch3:5 @ ws",
	92: "tmux build:7 @ ws",
	93: "pear:2 @ ws (not shown in any mirrored window now)",
	94: "Worker Seven",
}

// TestAMirroredSessionsAskNamesTheWindowRunningItNow: on the queue (and so its
// push title), an ask by a mirrored session names the window running its
// Claude session now; one no window runs says so.
func TestAMirroredSessionsAskNamesTheWindowRunningItNow(t *testing.T) {
	s := whereServer(t, whereAsks()...)
	items, err := s.attentionItems(s.attentionPrincipal(context.Background(), 1))
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, it := range items {
		if it.Kind != attentionKindAsk {
			continue
		}
		seen++
		if want := whereAskSources[it.AskID]; it.Source != want {
			t.Errorf("ask %d: source %q, want %q", it.AskID, it.Source, want)
		}
		if want := whereAskSources[it.AskID] + " is asking"; attentionPushTitle(it) != want {
			t.Errorf("ask %d: push title %q, want %q", it.AskID, attentionPushTitle(it), want)
		}
	}
	if seen != len(whereAskSources) {
		t.Fatalf("%d asks on the queue, want %d", seen, len(whereAskSources))
	}
}

var askPageSource = regexp.MustCompile(`data-ask-source>([^<]*)</span> asks`)

// TestTheAskPageHeaderNamesTheExactTmuxTarget: "<source> asks" on an ask's
// own page.
func TestTheAskPageHeaderNamesTheExactTmuxTarget(t *testing.T) {
	asks := whereAsks()
	s := whereServer(t, asks...)
	for _, a := range asks {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("GET", askPagePath(a.ID), nil).WithContext(s.attentionPrincipal(context.Background(), 1))
		s.renderAskPage(rec, r, a)
		m := askPageSource.FindStringSubmatch(rec.Body.String())
		if m == nil {
			t.Fatalf("ask %d: no header on the page:\n%s", a.ID, rec.Body.String())
		}
		if got, want := html.UnescapeString(m[1]), whereAskSources[a.ID]; got != want {
			t.Errorf("ask %d: header %q asks, want %q asks", a.ID, got, want)
		}
	}
}

// TestASessionPagesAsksBlockNamesTheExactTmuxTarget: the asks block on the
// labelled window's own page.
func TestASessionPagesAsksBlockNamesTheExactTmuxTarget(t *testing.T) {
	s := whereServer(t, whereAsks()...)
	items := s.askerAsks(s.attentionPrincipal(context.Background(), 1), askerRef{mirror: ui.MirrorRef{Host: "ws", Session: "scratch3", Window: "@37"}})
	if len(items) != 1 || items[0].AskID != 91 {
		t.Fatalf("block items %+v, want ask 91 alone", items)
	}
	if got, want := items[0].Source, whereAskSources[91]; got != want {
		t.Fatalf("source %q, want %q", got, want)
	}
}

// TestAFiledAsksNotificationNamesTheExactTmuxTarget: the notification sent when
// a mirrored session's ask is filed (askFiled) — the path that builds its item
// with no request of its own — names the window running it now. Its stored
// name is from before the window was renumbered (main:9): the push says main:2.
func TestAFiledAsksNotificationNamesTheExactTmuxTarget(t *testing.T) {
	store, pusher := &memPush{}, &rawPusher{}
	claim(t, store, 1, ep("where-filed"))
	s := pushTargetServer(t, store, pusher)
	s.askFiled(attention.Ask{ID: 95, Owner: 1, Asker: attention.Asker{MirrorHost: "ws", MirrorSession: ptOtherSID, Label: "main:9 @ ws"}, Question: "Which branch?"})
	s.WaitForPushes()
	var b push.Payload
	pusher.one(t, ep("where-filed"), &b)
	if want := "tmux main:2 @ ws is asking"; b.Title != want {
		t.Fatalf("title %q, want %q", b.Title, want)
	}
}
