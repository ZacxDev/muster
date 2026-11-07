package api

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
)

// 🔴 A LIVE SESSION WITH NO RECENT FOCUS IS FOUND BY ITS REPO — the hint and
// focus_search, on both transports, against a real Postgres; focus_search
// also by its window label, a host codename ("<codename>:<n>", as hosts send
// it). The shape of the miss: a window open on an app's repo, idle for ten
// days, with no focus row at all; a message about that app. Synthetic names
// only ("x-app-yt-thumbnail", codenames "Mars"/"Iris", host "box-1").

const (
	sidMetaIdle  = "0b2c3d4e-meta-idle"
	sidMetaOther = "0b2c3d4e-meta-other"
)

const metaMessage = "A new report in the owner inbox of an app you own.\n\napp: YT Thumbnail\nslug: yt-thumbnail\n\n" +
	"The desktop layout for the generator formats looks cramped."

// scopedMirror is overviewMirror listing its hosts to owner only, as
// mirror.Service does: another principal sees none.
type scopedMirror struct {
	overviewMirror
	owner int64
}

func (m scopedMirror) Hosts(ctx context.Context) []mirror.HostView {
	if p, ok := authz.From(ctx); !ok || p.UserID != m.owner {
		return nil
	}
	return m.hosts
}

// metaReplay is the miss: an idle window in repo "x-app-yt-thumbnail", label
// "Mars:3", ten days quiet and with no focus row; and a busy, unrelated window
// ("Iris:4") with a focus.
func metaReplay(t *testing.T) *gatedFixture {
	g := newGatedFixture(t)
	idle := time.Now().Add(-240 * time.Hour).Unix()
	h1 := overviewHost("box-1", true,
		mirror.Window{Session: "work", ID: "@7", Index: 3, Active: true, Claude: true, ClaudeSession: sidMetaIdle,
			Label: "Mars:3", Activity: idle},
		mirror.Window{Session: "work", ID: "@8", Index: 4, Claude: true, ClaudeSession: sidMetaOther, Label: "Iris:4",
			Activity: time.Now().Unix()})
	h1.Repos = map[string]string{"@7": "x-app-yt-thumbnail", "@8": "cache-quorum"}
	g.setFocus(t, focus.MirrorSubject(sidMetaOther), "Reading the cache dashboards", "")
	g.srv.ext.Mirror = scopedMirror{overviewMirror{hosts: []mirror.HostView{h1}}, g.owner}
	return g
}

func TestAnIdleSessionIsHintedByItsRepo(t *testing.T) {
	g := metaReplay(t)
	want := "mirror:" + sidMetaIdle
	h := g.srv.pluginSessionHints(g.asOwner, g.chief, plugins.Message{ID: 1, Subject: "[app feedback] YT Thumbnail #198", Body: metaMessage})
	var ids []string
	for _, s := range h.Sessions {
		ids = append(ids, s.ID)
	}
	if len(ids) != 1 || ids[0] != want {
		t.Fatalf("hint lists %v; want exactly %s", ids, want)
	}
	// Generic mail names nothing; nor does the app's prose without its name;
	// nor a codename spelled in prose — the label is not matched by the hint,
	// so "Mars" is no second term beside one repo part.
	for _, body := range []string{
		"Quick update on our meeting next week: please review the desktop layout and the generator formats.",
		"The desktop layout for the generator formats looks cramped.",
		"The thumbnail on the Mars page looks off.",
	} {
		if h := g.srv.pluginSessionHints(g.asOwner, g.chief, plugins.Message{ID: 2, Subject: "news", Body: body}); len(h.Sessions) != 0 {
			t.Errorf("%q named %+v", body, h.Sessions)
		}
	}
	// Another owner's principal sees no window, so no hint.
	other := g.newMember(t)
	if h := g.srv.pluginSessionHints(g.as(other), g.chief, plugins.Message{ID: 3, Subject: "YT Thumbnail", Body: metaMessage}); len(h.Sessions) != 0 {
		t.Errorf("another owner was hinted %+v", h.Sessions)
	}
}

func TestFocusSearchFindsAnIdleSessionByItsRepo(t *testing.T) {
	g := metaReplay(t)
	for _, args := range []string{`{"query":"thumbnail"}`, `{"query":"thumbnail","since_hours":24}`, `{"query":"yt-thumbnail"}`,
		`{"query":"Mars"}`} {
		for surface, text := range g.surfaces(t, g.chief, g.chiefTok, "focus_search", args) {
			if isToolErr(text) || !strings.Contains(text, `"id":"mirror:`+sidMetaIdle+`"`) {
				t.Errorf("%s %s: the idle session was not found: %s", surface, args, text)
			}
			if strings.Contains(text, sidMetaOther) {
				t.Errorf("%s %s: the unrelated session was found: %s", surface, args, text)
			}
		}
	}
	// Control: a word neither repo nor label holds finds nothing.
	for surface, text := range g.surfaces(t, g.chief, g.chiefTok, "focus_search", `{"query":"zeppelin"}`) {
		if strings.Contains(text, sidMetaIdle) {
			t.Errorf("%s: zeppelin found the idle session: %s", surface, text)
		}
	}
}

// At the hint's seam: a session whose focus matches is listed before one whose
// repo does.
func TestAFocusHitIsHintedBeforeARepoHit(t *testing.T) {
	g := metaReplay(t)
	g.setFocus(t, focus.MirrorSubject(sidMetaOther), "Porting the yt-thumbnail generator", "")
	h := g.srv.pluginSessionHints(g.asOwner, g.chief, plugins.Message{ID: 1, Subject: "[app feedback] YT Thumbnail #198", Body: metaMessage})
	var ids []string
	for _, s := range h.Sessions {
		ids = append(ids, s.ID)
	}
	if want := []string{"mirror:" + sidMetaOther, "mirror:" + sidMetaIdle}; fmt.Sprint(ids) != fmt.Sprint(want) {
		t.Fatalf("hint lists %v; want %v (the focus hit first)", ids, want)
	}
}

// mergeHints: focus hits first, then repo hits not among them, up to the
// limit; a session in both keeps its focus place and gains its repo line.
func TestTheHintListsFocusHitsBeforeRepoHits(t *testing.T) {
	a, b, c, d := plugins.SessionHint{ID: "a"}, plugins.SessionHint{ID: "b"}, plugins.SessionHint{ID: "c"}, plugins.SessionHint{ID: "d"}
	ra := plugins.SessionHint{ID: "a", Repo: "r", ByRepo: true}
	ids := func(hs []plugins.SessionHint) string {
		var out []string
		for _, h := range hs {
			out = append(out, h.ID+map[bool]string{true: "+repo"}[h.ByRepo])
		}
		return strings.Join(out, ",")
	}
	for _, tc := range []struct {
		focus, repo []plugins.SessionHint
		want        string
	}{
		{[]plugins.SessionHint{b}, []plugins.SessionHint{d, ra}, "b,d,a+repo"},
		{[]plugins.SessionHint{a}, []plugins.SessionHint{d, ra}, "a+repo,d"},
		{[]plugins.SessionHint{a, b, c}, []plugins.SessionHint{d}, "a,b,c"},
		{nil, []plugins.SessionHint{d, ra, b, c}, "d,a+repo,b"},
	} {
		if got := ids(mergeHints(tc.focus, tc.repo, 3)); got != tc.want {
			t.Errorf("mergeHints(%s | %s) = %s; want %s", ids(tc.focus), ids(tc.repo), got, tc.want)
		}
	}
}

// The hint's bounds hold for repo matches: many windows in the app's repo
// are listed MaxHintSessions at most.
func TestARepoHintIsBounded(t *testing.T) {
	g := newGatedFixture(t)
	var ws []mirror.Window
	repos := map[string]string{}
	for i := 0; i < plugins.MaxHintSessions+3; i++ {
		id := fmt.Sprintf("@%d", 20+i)
		ws = append(ws, mirror.Window{Session: "work", ID: id, Index: i, Claude: true,
			ClaudeSession: fmt.Sprintf("0b2c3d4e-meta-b%02d", i), Activity: time.Now().Unix()})
		repos[id] = "x-app-yt-thumbnail"
	}
	h := overviewHost("box-1", true, ws...)
	h.Repos = repos
	g.srv.ext.Mirror = overviewMirror{hosts: []mirror.HostView{h}}
	got := g.srv.pluginSessionHints(g.asOwner, g.chief, plugins.Message{ID: 1, Subject: "YT Thumbnail", Body: metaMessage})
	if len(got.Sessions) != plugins.MaxHintSessions {
		t.Fatalf("%d sessions hinted; want %d", len(got.Sessions), plugins.MaxHintSessions)
	}
}

// 🔴 At the hint's seam: four windows in one repo, one active now and three
// idle for 7–10 days (whose session ids sort before the active one's) — the
// active one is hinted, first.
func TestTheActiveWindowInARepoIsHintedOverIdleOnes(t *testing.T) {
	g := newGatedFixture(t)
	now := time.Now()
	var ws []mirror.Window
	repos := map[string]string{}
	for i, idle := range []time.Duration{7 * 24 * time.Hour, 8 * 24 * time.Hour, 10 * 24 * time.Hour, 0} {
		id := fmt.Sprintf("@%d", 40+i)
		ws = append(ws, mirror.Window{Session: "work", ID: id, Index: i, Claude: true,
			ClaudeSession: fmt.Sprintf("0b2c3d4e-tie-%c", 'a'+i), Activity: now.Add(-idle).Unix()})
		repos[id] = "x-app-yt-thumbnail"
	}
	h := overviewHost("box-1", true, ws...)
	h.Repos = repos
	g.srv.ext.Mirror = overviewMirror{hosts: []mirror.HostView{h}}
	got := g.srv.pluginSessionHints(g.asOwner, g.chief, plugins.Message{ID: 1, Subject: "YT Thumbnail", Body: metaMessage})
	var ids []string
	for _, s := range got.Sessions {
		ids = append(ids, s.ID)
	}
	if want := "[mirror:0b2c3d4e-tie-d mirror:0b2c3d4e-tie-a mirror:0b2c3d4e-tie-b]"; fmt.Sprint(ids) != want {
		t.Fatalf("hint lists %v; want %s", ids, want)
	}
}
