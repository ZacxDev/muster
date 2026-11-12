package api

import (
	"context"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/push"
)

// "Start notification titles with the tmux name" on the pushes about a tmux
// window that name it by no human name (#619 round 1: Y2, Y3, Y5, G1), end to
// end through goNotify to the device's payload.

// sinkServer is a server whose operator (user 1) has one browser device, a
// fake hub listing windows on host "ws", and a preference store with the
// operator's codename switch set to first; title is the one push the device
// got (it then forgets it).
func sinkServer(t *testing.T, windows []mirror.Window, first bool) (s *Server, title func() string) {
	t.Helper()
	store := &memPush{}
	web := ep("web-codename-sink")
	claim(t, store, 1, web)
	pusher := &rawPusher{}
	s = eventServer(store, pusher)
	s.ext.Mirror = &fakeMirror{online: true, v2: &fakeMirrorV2{}, windows: windows}
	prefs := &memNotifyPrefs{}
	_ = prefs.SetCodenameFirst(context.Background(), 1, first)
	s.ext.NotifyPrefs = prefs
	return s, func() string {
		t.Helper()
		s.WaitForPushes()
		var b push.Payload
		pusher.one(t, web, &b)
		pusher.mu.Lock()
		pusher.got = nil
		pusher.mu.Unlock()
		return b.Title
	}
}

// TestAChiefRoutedPushStartsWithItsTargetWindow: "Chief routed it to …" names
// the target window by its place; with the switch on it starts with that
// window's codename@host instead (its tmux session:window when unlabelled) —
// "Chief routed it here".
func TestAChiefRoutedPushStartsWithItsTargetWindow(t *testing.T) {
	labelled := mirror.SessionView{Host: "laptop", Session: "scratch13", Window: "@1", Index: 1, Label: "mango:1", Online: true}
	plain := mirror.SessionView{Host: "laptop", Session: "main", Window: "@3", Index: 3, Online: true}
	for _, c := range []struct {
		what   string
		first  bool
		target routeTarget
		want   string
	}{
		{"off, labelled", false, routeTarget{window: labelled}, "Chief routed it to mango:1 · tmux scratch13:1 @ laptop"},
		{"on, labelled", true, routeTarget{window: labelled}, "mango:1@laptop · Chief routed it here"},
		{"off, unlabelled", false, routeTarget{window: plain}, "Chief routed it to tmux main:3 @ laptop"},
		{"on, unlabelled", true, routeTarget{window: plain}, "main:3@laptop · Chief routed it here"},
	} {
		s, title := sinkServer(t, nil, c.first)
		s.notifyChiefRouted(operatorCtx(), 1, plugins.Route{RouteID: "r-" + c.what}, c.target, routedItem{msg: plugins.Message{Subject: "Invoice overdue"}})
		if got := title(); got != c.want {
			t.Errorf("%s: %q, want %q", c.what, got, c.want)
		}
	}
}

// TestAnApprovedSessionDispatchsPushStartsWithTheDispatchingWindow: "Agent
// dispatched from your session" starts, with the switch on, with the window
// running the dispatching session now; off, or a session no host lists, it is
// today's title.
func TestAnApprovedSessionDispatchsPushStartsWithTheDispatchingWindow(t *testing.T) {
	h := heldDispatch{Door: heldDoorMCP, Host: "ws", Session: focusSID, SessionLabel: "Billing migration", Target: "pod", Kind: "claude-code"}
	for _, c := range []struct {
		what  string
		first bool
		h     heldDispatch
		want  string
	}{
		{"off", false, h, "Agent dispatched from your session"},
		{"on", true, h, "mango:1@ws · Agent dispatched from your session"},
		{"on, a session no host lists", true, heldDispatch{Door: heldDoorMCP, Host: "ws", Session: "sid-nowhere", Target: "pod"}, "Agent dispatched from your session"},
	} {
		s, title := sinkServer(t, namedWindows(), c.first)
		s.pushSessionDispatch(operatorCtx(), 1, c.h, agents.Agent{Name: "fix-" + c.what}, "")
		if got := title(); got != c.want {
			t.Errorf("%s: %q, want %q", c.what, got, c.want)
		}
	}
}

// TestHostDispatchPlaceIsOnlyAListedWindowOfThatDispatch: the window tagged
// with the dispatch wins; else its stored tmux window id in the agents
// session — but not a window tagged with ANOTHER dispatch (window ids restart
// with the tmux server); a window no listing shows is no place at all (the
// stored value is a window id, not a typeable target).
func TestHostDispatchPlaceIsOnlyAListedWindowOfThatDispatch(t *testing.T) {
	h := plugins.HostDispatch{ID: 7, Host: "ws", Window: "@5"}
	for _, c := range []struct {
		what    string
		windows []mirror.Window
		want    string
	}{
		{"tagged with it", []mirror.Window{{Session: "agents", ID: "@9", Index: 4, Label: "plum:4", Dispatch: 7}}, "plum:4@ws"},
		{"its window id, untagged", []mirror.Window{{Session: "agents", ID: "@5", Index: 3}}, "agents:3@ws"},
		{"its window id, tagged with it", []mirror.Window{{Session: "agents", ID: "@5", Index: 3, Dispatch: 7}}, "agents:3@ws"},
		{"its window id, tagged with ANOTHER dispatch", []mirror.Window{{Session: "agents", ID: "@5", Index: 3, Dispatch: 9}}, ""},
		{"the tag beats an id match listed first", []mirror.Window{{Session: "agents", ID: "@5", Index: 3}, {Session: "agents", ID: "@8", Index: 6, Dispatch: 7}}, "agents:6@ws"},
		{"not listed", []mirror.Window{{Session: "agents", ID: "@6", Index: 2}}, ""},
		{"nothing listed", nil, ""},
	} {
		s, _ := sinkServer(t, c.windows, false)
		if got := codenameFront(s.hostDispatchPlace(operatorCtx(), h)); got != c.want {
			t.Errorf("%s: front %q, want %q", c.what, got, c.want)
		}
	}
}
