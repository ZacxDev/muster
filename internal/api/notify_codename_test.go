package api

import (
	"context"
	"errors"
	"testing"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/sessionstate"
)

// "START NOTIFICATION TITLES WITH THE TMUX NAME" (notify_prefs.go; names.go
// codenameTitle; server.go goNotify; docs/focus.md#tmux-name-first-in-push-titles).
// A phone cuts the END of a push title, so "Billing migration · mango:1 @ ws
// needs you" loses the codename the owner needs to jump to the window. With
// this switch on, every push title that names a mirrored session STARTS with
// it, with its host: "mango:1@ws · Billing migration needs you". Off, its
// title is unchanged.
//
// Every expected title below is a whole literal string.

// TestCodenameTitleLeadsWithTheCodenameAndHost pins the one rule with whole
// strings: the front is the codename and host, compact ("mango:1@atlas"), or
// the tmux address and host when the host sent no label; then " · ", the
// words and the rest. A human name that IS the codename becomes the front.
// Nothing for no window or no human name.
func TestCodenameTitleLeadsWithTheCodenameAndHost(t *testing.T) {
	mango := mirror.Place{Code: "mango:1", Host: "atlas"}
	for _, c := range []struct {
		words, rest string
		at          mirror.Place
		title, keep string
	}{
		{"Billing migration", " needs you", mango, "mango:1@atlas · Billing migration needs you", "mango:1@atlas · Billing migration"},
		{"Billing migration", " is stalled", mango, "mango:1@atlas · Billing migration is stalled", "mango:1@atlas · Billing migration"},
		// The SAME codename on another host: the front tells them apart.
		{"Billing migration", " needs you", mirror.Place{Code: "mango:1", Host: "laptop"}, "mango:1@laptop · Billing migration needs you", "mango:1@laptop · Billing migration"},
		// A host that sent no label: its tmux session:window and host.
		{"Billing migration", " is asking", mirror.Place{Code: "scratch3:5", Host: "atlas"}, "scratch3:5@atlas · Billing migration is asking", "scratch3:5@atlas · Billing migration"},
		// The human name IS the codename: the front replaces it, said once.
		{"mango:1", " is asking", mango, "mango:1@atlas is asking", "mango:1@atlas"},
		// No human name: the front, then the tmux address (a typeable target),
		// then its host offline; an unlabelled window is the front alone.
		{"", " is asking", mirror.Place{Code: "mango:1", Host: "atlas", Addr: "scratch13:1"}, "mango:1@atlas · tmux scratch13:1 is asking", "mango:1@atlas · tmux scratch13:1"},
		{"", " is asking", mirror.Place{Code: "main:1", Host: "atlas", Addr: "main:1"}, "main:1@atlas is asking", "main:1@atlas"},
		{"", " needs you", mirror.Place{Code: "mango:1", Host: "atlas", Addr: "scratch13:1", Offline: true}, "mango:1@atlas · tmux scratch13:1 (host offline) needs you", "mango:1@atlas · tmux scratch13:1 (host offline)"},
		// No window: an agent, a session no window shows now.
		{"Billing migration", " needs you", mirror.Place{}, "", ""},
	} {
		title, keep := codenameTitle(c.words, c.rest, c.at)
		if title != c.title || keep != c.keep {
			t.Errorf("codenameTitle(%q, %q, %#v) = %q keep %q, want %q keep %q", c.words, c.rest, c.at, title, keep, c.title, c.keep)
		}
	}
	for _, c := range []struct {
		title      string
		at         mirror.Place
		want, keep string
	}{
		{sessionPushTitle, mango, "mango:1@atlas · A session is waiting on you", "mango:1@atlas"},
		{"Claude Code is waiting for you: which branch?", mirror.Place{Code: "agents:3", Host: "atlas"}, "agents:3@atlas · Claude Code is waiting for you: which branch?", "agents:3@atlas"},
		// A place with no host (none known): the codename alone.
		{sessionPushTitle, mirror.Place{Code: "mango:1"}, "mango:1 · A session is waiting on you", "mango:1"},
		{sessionPushTitle, mirror.Place{}, "", ""},
	} {
		if got, keep := codenameLed(c.title, c.at); got != c.want || keep != c.keep {
			t.Errorf("codenameLed(%q, %#v) = %q keep %q, want %q keep %q", c.title, c.at, got, keep, c.want, c.keep)
		}
	}
}

// codenameServer is a server whose window @1 (mango:1 @ ws) holds the
// conversation named "Billing migration", with a notification-preference
// store whose operator turned "Show tmux sessions" to tmux.
func codenameServer(t *testing.T, tmux bool) *Server {
	t.Helper()
	fm := &fakeMirror{online: true, v2: &fakeMirrorV2{}, statusReport: true, windows: namedWindows()}
	s, _ := focusMirrorServer(t, fm, namedFocusStore(), nil)
	prefs := &memNotifyPrefs{}
	_ = prefs.SetTmuxAddress(context.Background(), authz.OperatorUserID, tmux)
	s.ext.NotifyPrefs = prefs
	return s
}

// TestEveryAttentionPushDeclaresItsCodenameTitle: through withFocus (where
// every pushed item is named), each kind of attention push about the named
// window keeps today's Title and declares the codename-first one — with
// "Show tmux sessions" on too, the codename is said ONCE, at the front.
func TestEveryAttentionPushDeclaresItsCodenameTitle(t *testing.T) {
	kinds := []struct {
		kind, what string
		// title off / with "Show tmux sessions" on; the codename-first one.
		off, tmux, first, keep string
	}{
		{attentionKindAsk, "an agent question from a mirrored session",
			"Billing migration is asking", "Billing migration · mango:1 @ ws is asking", "mango:1@ws · Billing migration is asking", "mango:1@ws · Billing migration"},
		{attentionKindQuestion, "a held question",
			"Billing migration is asking", "Billing migration · mango:1 @ ws is asking", "mango:1@ws · Billing migration is asking", "mango:1@ws · Billing migration"},
		{attentionKindNotice, "a chief notice",
			"Billing migration may be waiting on you", "Billing migration · mango:1 @ ws may be waiting on you", "mango:1@ws · Billing migration may be waiting on you", "mango:1@ws · Billing migration"},
		{attentionKindSession, "a waiting window's own push",
			"A session is waiting on you", "A session is waiting on you", "mango:1@ws · A session is waiting on you", "mango:1@ws"},
	}
	for _, tmux := range []bool{false, true} {
		s := codenameServer(t, tmux)
		for _, k := range kinds {
			it := namedAskItem()
			it.Kind = k.kind
			items := []attentionItem{it}
			s.withFocus(operatorCtx(), items)
			n := attentionNotification(items[0], false)
			want := map[bool]string{false: k.off, true: k.tmux}[tmux]
			if n.Title != want {
				t.Errorf("tmux=%v %s: Title %q, want %q (today's)", tmux, k.what, n.Title, want)
			}
			if n.CodenameTitle != k.first || n.CodenameKeep != k.keep {
				t.Errorf("tmux=%v %s: CodenameTitle %q keep %q, want %q keep %q", tmux, k.what, n.CodenameTitle, n.CodenameKeep, k.first, k.keep)
			}
		}
	}
}

// TestAnUnnamedSessionsPushGetsTheFrontAndKeepsItsTmuxTarget: an unnamed
// window's ask (today called by its place) now starts with the compact front
// and keeps its tmux address after it — a typeable target, the label never a
// window's only name — or is the front alone when the window has no label (the
// front IS its address); its host offline is still said. A window whose human
// name IS its codename gets the front IN PLACE of the name. An agent's ask (no
// tmux window) and a waiting item no host lists declare nothing.
func TestAnUnnamedSessionsPushGetsTheFrontAndKeepsItsTmuxTarget(t *testing.T) {
	s := codenameServer(t, true)
	store := s.ext.Focus.(*memFocus)
	store.rows[focus.MirrorSubject("sid-same")] = focus.Focus{Subject: focus.MirrorSubject("sid-same"), Name: focus.Name{Manual: "kiwi:2"}}
	store.rows[focus.AgentSubject(8)] = focus.Focus{Subject: focus.AgentSubject(8), Name: focus.Name{Auto: "Deploy importer"}}
	unnamed := attentionItem{Key: "ask-2", Kind: attentionKindAsk, Source: "kiwi:2 · tmux side:2 @ ws", target: "tmux side:2 @ ws",
		at: mirror.Place{Code: "kiwi:2", Host: "ws", Addr: "side:2"}, subject: focus.MirrorSubject("sid-unnamed"), claudeSession: "sid-unnamed"}
	unlabelled := attentionItem{Key: "ask-4", Kind: attentionKindAsk, Source: "tmux main:2 @ ws", target: "tmux main:2 @ ws",
		at: mirror.Place{Code: "main:2", Host: "ws", Addr: "main:2"}, subject: focus.MirrorSubject("sid-plain"), claudeSession: "sid-plain"}
	offline := unnamed
	offline.Key, offline.Source, offline.at.Offline = "ask-5", "kiwi:2 · tmux side:2 @ ws (host offline)", true
	same := unnamed
	same.Key, same.subject, same.claudeSession = "ask-3", focus.MirrorSubject("sid-same"), "sid-same"
	agent := askItem(attention.Ask{ID: 61, Owner: 1, Asker: attention.Asker{AgentID: 8, Label: plainName}, Question: "Deploy now?"})
	waitingAgentless := attentionItem{Key: "session-x", Kind: attentionKindSession, Source: "A session"}
	items := []attentionItem{unnamed, unlabelled, offline, same, agent, waitingAgentless, namedAskItem()}
	s.withFocus(operatorCtx(), items)
	for i, c := range []struct{ title, first, keep string }{
		{"kiwi:2 · tmux side:2 @ ws is asking", "kiwi:2@ws · tmux side:2 is asking", "kiwi:2@ws · tmux side:2"},
		{"tmux main:2 @ ws is asking", "main:2@ws is asking", "main:2@ws"},
		{"kiwi:2 · tmux side:2 @ ws (host offline) is asking", "kiwi:2@ws · tmux side:2 (host offline) is asking", "kiwi:2@ws · tmux side:2 (host offline)"},
		{"kiwi:2 is asking", "kiwi:2@ws is asking", "kiwi:2@ws"},
		{"Deploy importer is asking", "", ""},
		{"A session is waiting on you", "", ""},
		{"Billing migration · mango:1 @ ws is asking", "mango:1@ws · Billing migration is asking", "mango:1@ws · Billing migration"},
	} {
		n := attentionNotification(items[i], false)
		if n.Title != c.title || n.CodenameTitle != c.first || n.CodenameKeep != c.keep {
			t.Errorf("item %d: Title %q CodenameTitle %q keep %q, want %q, %q keep %q", i, n.Title, n.CodenameTitle, n.CodenameKeep, c.title, c.first, c.keep)
		}
	}
}

// TestAnEpisodesPushDeclaresItsCodenameTitle: "… needs you" and "… is
// stalled" of the named window keep today's title and declare the
// codename-first one, the codename once with "Show tmux sessions" on; an
// unnamed one whose window no host lists now (its session's codename fallback,
// a stored index) and an agent's declare none.
func TestAnEpisodesPushDeclaresItsCodenameTitle(t *testing.T) {
	ep := func(state string) sessionstate.Episode {
		return sessionstate.Episode{ID: 51, Owner: 1, Kind: sessionstate.KindMirror, Host: "ws", Key: focusSID,
			TmuxSession: "main", Window: "@1", Index: 1, State: state}
	}
	for _, c := range []struct {
		tmux                   bool
		state                  string
		title, first, firstKey string
	}{
		{false, sessionstate.StateNeedsYou, "Billing migration needs you", "mango:1@ws · Billing migration needs you", "mango:1@ws · Billing migration"},
		{false, sessionstate.StateStalled, "Billing migration is stalled", "mango:1@ws · Billing migration is stalled", "mango:1@ws · Billing migration"},
		{true, sessionstate.StateNeedsYou, "Billing migration · mango:1 @ ws needs you", "mango:1@ws · Billing migration needs you", "mango:1@ws · Billing migration"},
		{true, sessionstate.StateStalled, "Billing migration · mango:1 @ ws is stalled", "mango:1@ws · Billing migration is stalled", "mango:1@ws · Billing migration"},
	} {
		s := codenameServer(t, c.tmux)
		n := s.episodeNotification(operatorCtx(), ep(c.state), 0, nil)
		if n.Title != c.title || n.CodenameTitle != c.first || n.CodenameKeep != c.firstKey {
			t.Errorf("tmux=%v %s: Title %q CodenameTitle %q keep %q, want %q, %q keep %q", c.tmux, c.state, n.Title, n.CodenameTitle, n.CodenameKeep, c.title, c.first, c.firstKey)
		}
	}
	s := codenameServer(t, true)
	unnamed := sessionstate.Episode{ID: 52, Owner: 1, Kind: sessionstate.KindMirror, Host: "ws", Key: "sid-unnamed",
		TmuxSession: "main", Window: "@2", Index: 2, State: sessionstate.StateNeedsYou}
	if n := s.episodeNotification(operatorCtx(), unnamed, 0, nil); n.Title != "mango · tmux main:2 @ ws needs you" || n.CodenameTitle != "" {
		t.Errorf("an unnamed window: Title %q CodenameTitle %q, want its place and none", n.Title, n.CodenameTitle)
	}
	agent := sessionstate.Episode{ID: 53, Owner: 1, Kind: sessionstate.KindAgent, Key: focusSID, AgentID: 999, State: sessionstate.StateStalled}
	if n := s.episodeNotification(operatorCtx(), agent, 0, nil); n.Title != "An agent is stalled" || n.CodenameTitle != "" {
		t.Errorf("an agent: Title %q CodenameTitle %q, want no codename", n.Title, n.CodenameTitle)
	}
}

// TestGoNotifySendsTheCodenameTitleOnlyToAnOwnerWhoTurnedItOn: the one sink,
// end to end on both kinds of device (the browser's payload and the paired
// phone's payload v2). The codename-first title is sent exactly when the
// OWNER turned the switch on; with no store, no choice, another user's switch
// on, a failed read, or switched off again, the title is byte for byte the
// one the push was built with. A push that declared no codename title is
// never changed, switch or not.
func TestGoNotifySendsTheCodenameTitleOnlyToAnOwnerWhoTurnedItOn(t *testing.T) {
	const today, first = "Billing migration · mango:1 @ ws needs you", "mango:1@ws · Billing migration needs you"
	store := &memPush{}
	web, native := ep("web-codename"), ep("native-codename")
	claim(t, store, 7, web)
	store.mu.Lock()
	store.nextID++
	var sub push.Subscription
	sub.Endpoint, sub.Keys.P256dh, sub.Keys.Auth = native, "k", "a"
	store.devs = append(store.devs, push.Device{ID: store.nextID, OwnerID: 7, Sub: sub, Kind: push.KindAndroid})
	store.mu.Unlock()
	pusher := &rawPusher{}
	s := eventServer(store, pusher)
	n := 0 // a tag per send; pushAllowed's burst (30) is far above this test's count
	send := func(rn RouterNotification) (string, string) {
		t.Helper()
		pusher.mu.Lock()
		pusher.got = nil
		pusher.mu.Unlock()
		n++
		rn.Tag = "codename-" + string(rune('a'+n))
		s.goNotify(7, push.EventAttention, rn, nil)
		s.WaitForPushes()
		var b push.Payload
		pusher.one(t, web, &b)
		var v2 push.PayloadV2
		pusher.one(t, native, &v2)
		return b.Title, v2.Title
	}
	episode := RouterNotification{Type: attentionNotificationType, ID: "e1", Title: today, TitleKeep: "Billing migration · mango:1 @ ws",
		CodenameTitle: first, CodenameKeep: "mango:1@ws · Billing migration"}
	expect := func(what, want string) {
		t.Helper()
		if b, v2 := send(episode); b != want || v2 != want {
			t.Fatalf("%s: browser %q, native %q, want both %q", what, b, v2, want)
		}
	}
	expect("no store (no database)", today)
	prefs := &memNotifyPrefs{}
	s.ext.NotifyPrefs = prefs
	expect("a store with no choice", today)
	_ = prefs.SetCodenameFirst(context.Background(), 8, true)
	expect("another user's switch on", today)
	_ = prefs.SetTmuxAddress(context.Background(), 7, true)
	expect("only \"Show tmux sessions\" on", today)
	_ = prefs.SetCodenameFirst(context.Background(), 7, true)
	expect("on", first)
	prefs.err = errors.New("read failed")
	expect("a failed read", today)
	prefs.err = nil
	plain := RouterNotification{Type: taskNotificationType, ID: "31", Title: "Task #31: **Ship** it"}
	if b, v2 := send(plain); b != "Task #31: Ship it" || v2 != "Task #31: Ship it" {
		t.Fatalf("on, a push with no codename title: browser %q native %q, want its own (plained) title", b, v2)
	}
	_ = prefs.SetCodenameFirst(context.Background(), 7, false)
	expect("switched off again", today)
}

// TestAnEpisodesFrontIsOnlyAWindowAHostListsNow: an episode's front names a
// window the owner can jump to NOW — the window holding its conversation, or
// the stored window itself, still listed — never a place rebuilt from what the
// episode stored (the stored index may be stale after a tmux renumber). An
// unnamed one keeps its tmux address after the front.
func TestAnEpisodesFrontIsOnlyAWindowAHostListsNow(t *testing.T) {
	fm := &fakeMirror{online: true, v2: &fakeMirrorV2{}, statusReport: true, windows: []mirror.Window{
		{Session: "scratch13", ID: "@1", Index: 1, Label: "mango:1", Claude: true, ClaudeSession: "sid-a"},
		{Session: "scratch13", ID: "@2", Index: 2, Label: "mango:2", Claude: true, ClaudeSession: "sid-other"},
	}}
	store := &memFocus{rows: map[focus.Subject]focus.Focus{
		focus.MirrorSubject("sid-named-gone"): {Subject: focus.MirrorSubject("sid-named-gone"), Name: focus.Name{Manual: "Gone session"}},
	}}
	s, _ := focusMirrorServer(t, fm, store, nil)
	ep := func(key, host, window string, index int) sessionstate.Episode {
		return sessionstate.Episode{ID: 61, Owner: 1, Kind: sessionstate.KindMirror, Host: host, Key: key,
			TmuxSession: "scratch13", Window: window, Index: index, State: sessionstate.StateNeedsYou}
	}
	for _, c := range []struct {
		what        string
		ep          sessionstate.Episode
		title, want string
	}{
		{"the window holding the conversation now", ep("sid-a", "ws", "@1", 1),
			"mango:1 · tmux scratch13:1 @ ws needs you", "mango:1@ws · tmux scratch13:1 needs you"},
		{"the stored window itself, still listed", ep("sid-gone", "ws", "@2", 2),
			"mango:2 · tmux scratch13:2 @ ws needs you", "mango:2@ws · tmux scratch13:2 needs you"},
		{"the stored window gone, its session's codename and a stored index", ep("sid-gone", "ws", "@7", 7),
			"mango · tmux scratch13:7 @ ws needs you", ""},
		{"a host that lists nothing of it", ep("sid-gone", "elsewhere", "@1", 1),
			"tmux scratch13:1 @ elsewhere needs you", ""},
		{"a NAMED session whose window is gone", ep("sid-named-gone", "ws", "@7", 7),
			"Gone session needs you", ""},
	} {
		n := s.episodeNotification(operatorCtx(), c.ep, 0, nil)
		if n.Title != c.title || n.CodenameTitle != c.want {
			t.Errorf("%s: Title %q CodenameTitle %q, want %q, %q", c.what, n.Title, n.CodenameTitle, c.title, c.want)
		}
	}
}
