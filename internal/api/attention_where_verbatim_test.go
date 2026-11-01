package api

import (
	"context"
	"html"
	"io"
	"log"
	"regexp"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/push"
)

// THE TARGET IS SHOWN VERBATIM. tmux session names are free text: "_scratch_",
// "*dev*" and "a`b`c" are real names an operator types. The title rule
// (textplain) plains markdown out of what a page or a notification shows; it
// must plain the codename around a window's target and never the target
// itself (textplain.TitleKeeping with mirror.WhereTarget).
const (
	verbatimSIDScratch = "6b1c2d3e-1111-4111-8111-00000000a001"
	verbatimSIDDev     = "6b1c2d3e-2222-4222-8222-00000000a002"
	verbatimSIDTicks   = "6b1c2d3e-3333-4333-8333-00000000a003"
)

func verbatimServer(t *testing.T, store push.Store, pusher push.Pusher) *Server {
	t.Helper()
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	fm := &fakeMirror{online: true, interactive: true, statusReport: true, v2: &fakeMirrorV2{}, windows: []mirror.Window{
		// A markdown-shaped codename (plained) on a markdown-shaped session (kept).
		{Session: "_scratch_", ID: "@11", Index: 2, Active: true, Label: "*mango*:1", Claude: true, ClaudeSession: verbatimSIDScratch, Status: mirror.StatusWaiting, StatusSince: attnSince},
		{Session: "*dev*", ID: "@12", Index: 3, Active: true, Claude: true, ClaudeSession: verbatimSIDDev, Status: mirror.StatusWaiting, StatusSince: attnSince},
		{Session: "a`b`c", ID: "@13", Index: 4, Active: true, Claude: true, ClaudeSession: verbatimSIDTicks, Status: mirror.StatusWaiting, StatusSince: attnSince},
	}}
	ext := Extensions{Users: stubUsers{}, Mirror: fm}
	if store != nil {
		ext.Push = push.NewService(store, pusher, push.VAPIDConfig{PublicKey: "k"}, log.New(io.Discard, "", 0))
	}
	s.UseExtensions(ext)
	return s
}

// verbatimWhere is each window's where as every surface must show it.
var verbatimWhere = map[string]string{
	sessionKey("ws", "_scratch_", "@11"): "mango:1 · tmux _scratch_:2 @ ws",
	sessionKey("ws", "*dev*", "@12"):     "tmux *dev*:3 @ ws",
	sessionKey("ws", "a`b`c", "@13"):     "tmux a`b`c:4 @ ws",
}

var verbatimCardSource = regexp.MustCompile(`<li id="(session-[0-9a-f]+)"(?s:.*?)data-attention-source>([^<]*)</span>`)

// TestAWindowsTargetIsShownVerbatimOnThePushTheSummaryAndTheCard: the waiting
// session's notification body, its summary title and why, and its card on the
// queue, whole.
func TestAWindowsTargetIsShownVerbatimOnThePushTheSummaryAndTheCard(t *testing.T) {
	s := verbatimServer(t, nil, nil)
	items := s.waitingSessions(s.attentionPrincipal(context.Background(), 1))
	if len(items) != len(verbatimWhere) {
		t.Fatalf("%d waiting items, want %d", len(items), len(verbatimWhere))
	}
	for _, it := range items {
		want, ok := verbatimWhere[it.Key]
		if !ok {
			t.Fatalf("unexpected item %s", it.Key)
		}
		if got := attentionPushBody(it); got != "Waiting: "+want {
			t.Errorf("%s: push body %q, want %q", it.Key, got, "Waiting: "+want)
		}
		sum := attentionSummaryItemOf(it, "", time.Now())
		if sum.Title != want || sum.Why != "Waiting: "+want {
			t.Errorf("%s: summary title %q why %q, want %q / %q", it.Key, sum.Title, sum.Why, want, "Waiting: "+want)
		}
	}
	cards := verbatimCardSource.FindAllStringSubmatch(attentionItemsHTML(items, time.Now()), -1)
	if len(cards) != len(verbatimWhere) {
		t.Fatalf("%d cards, want %d", len(cards), len(verbatimWhere))
	}
	for _, m := range cards {
		if got, want := html.UnescapeString(m[2]), verbatimWhere[m[1]]; got != want {
			t.Errorf("%s: card source %q, want %q", m[1], got, want)
		}
	}
}

// TestAnAsksPushTitleKeepsItsTargetVerbatim: the push sink (goNotify) plains
// a notification's title — but not the asking window's target in it.
func TestAnAsksPushTitleKeepsItsTargetVerbatim(t *testing.T) {
	store, pusher := &memPush{}, &rawPusher{}
	claim(t, store, 1, ep("verbatim-ask"))
	s := verbatimServer(t, store, pusher)
	s.askFiled(attention.Ask{ID: 96, Owner: 1, Asker: attention.Asker{MirrorHost: "ws", MirrorSession: verbatimSIDDev, Label: "dev:3 @ ws"}, Question: "Which branch?"})
	s.WaitForPushes()
	var b push.Payload
	pusher.one(t, ep("verbatim-ask"), &b)
	if want := "tmux *dev*:3 @ ws is asking"; b.Title != want {
		t.Fatalf("title %q, want %q", b.Title, want)
	}
}

// TestAWhereOnAnOfflineHostSaysSo: the hub keeps a dropped host's windows
// listed for a grace period; an ask's where from one is its last-known window
// and says so.
func TestAWhereOnAnOfflineHostSaysSo(t *testing.T) {
	hosts := []mirror.HostView{{Host: "ws", Online: false, Windows: []mirror.Window{
		{Session: "build", ID: "@4", Index: 7, Claude: true, ClaudeSession: verbatimSIDDev}}}}
	a := attention.Ask{Asker: attention.Asker{MirrorHost: "ws", MirrorSession: verbatimSIDDev, Label: "build:7 @ ws"}}
	if got, want := askSource(a, hosts), "tmux build:7 @ ws (host offline)"; got != want {
		t.Fatalf("where %q, want %q", got, want)
	}
	hosts[0].Online = true
	if got, want := askSource(a, hosts), "tmux build:7 @ ws"; got != want {
		t.Fatalf("online: where %q, want %q", got, want)
	}
}
