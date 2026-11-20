package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// A MIRRORED WINDOW'S PAGES ACROSS ITS CLAUDE SESSIONS (operator decision on
// #826 round 0): the Pages tab lists every page made IN the window — same
// host, same tmux window — so a /clear, a restart or a resume (a new Claude
// session id in the window) keeps the earlier ones. Every name, id and host
// is invented.

const clearedSID = "0d1e2f3a-aaaa-4bbb-8ccc-0000000000dd"

// windowPageFrom has session sid in tmux window (tmuxSess, win) on host make
// a page, as the mirror door binds such a caller (mirror_mcp.go).
func windowPageFrom(t *testing.T, f *pageFixture, owner int64, host, tmuxSess, win, sid, title string) int64 {
	t.Helper()
	c := mirrorCaller(host, sid)
	c.mirrorWindow, c.mirrorTmuxSession = win, tmuxSess
	out := f.toolAs(t, f.as(owner, authz.ViaMirrorHost), c, pageCreateTool,
		map[string]any{"title": title, "blocks": json.RawMessage(`[{"type":"markdown","text":"hi"}]`)})
	id := pnum(out["page_id"])
	if id <= 0 {
		t.Fatalf("page_create: %v", out)
	}
	return id
}

// TestAWindowKeepsItsPagesAcrossAClear: a page made in @3 under makerSID is
// still on @3's Pages tab, and counted on its Open button, once @3 runs a new
// session (a /clear). A page made in @4 of the same host is not @3's, nor is
// one made in a window of the same name and session on another host.
func TestAWindowKeepsItsPagesAcrossAClear(t *testing.T) {
	f, m := newSessionPageFixture(t)
	mine := windowPageFrom(t, f, f.owner, "workstation", "main", "@3", makerSID, "Made before the clear")
	windowPageFrom(t, f, f.owner, "workstation", "main", "@4", otherSID, "Another window's")
	windowPageFrom(t, f, f.owner, "laptop", "main", "@3", clearedSID, "Another host's @3")
	// The /clear: @3 now runs a NEW session; makerSID runs nowhere.
	m.set(f.owner, sessionWindow("@3", 3, clearedSID), sessionWindow("@4", 4, otherSID))
	m.setHost(f.owner, "laptop", sessionWindow("@3", 3, clearedSID))

	list := func(host, win string) []int64 {
		t.Helper()
		w := f.get(t, ui.SessionPagesPath+"?host="+host+"&session=main&window="+win, f.ownerHdr())
		if w.Code != http.StatusOK {
			t.Fatalf("the Pages tab of %s %s: %d", host, win, w.Code)
		}
		return listedPages(w.Body.String())
	}
	if got, want := list("workstation", "%403"), []int64{mine}; !slices.Equal(got, want) {
		t.Fatalf("@3 after the clear lists %v, want %v (the page made in it under the session before)", got, want)
	}
	if b := f.get(t, ui.PreviewButtonPath+"?"+windowQuery("@3"), f.ownerHdr()).Body.String(); !containsAll(b, `data-preview-count="1"`, `data-open-pages="1"`) {
		t.Errorf("@3's Open button after the clear: %s", b)
	}
	for _, id := range list("workstation", "%404") {
		if id == mine {
			t.Error("@4 of the same host lists @3's page")
		}
	}
	for _, id := range list("laptop", "%403") {
		if id == mine {
			t.Error("@3 of ANOTHER host lists workstation's @3 page")
		}
	}
	// And the team window of the same name lists none (windowPagesRef).
	team := mirror.SessionView{Owner: 41, Host: "workstation", Session: "main", Window: "@3"}
	if ref := windowPagesRef(mirror.HostView{Host: "workstation", Windows: mirror.NormalizeWindows([]mirror.Window{sessionWindow("@3", 3, clearedSID)}, 0)}, team); ref.Valid() {
		t.Errorf("a team window names %+v", ref)
	}
}

// TestAPageFromTheMirrorDoorRecordsItsWindow: page_create through the REAL
// mirror door (POST /mirror/mcp, bound to the host's listing) records the
// tmux session and window id the calling Claude session runs in — focusSID in
// main:@1, splitSID in a split pane of side:@3 — beside its session.
func TestAPageFromTheMirrorDoorRecordsItsWindow(t *testing.T) {
	f := newSizeFixture(t)
	for _, tc := range []struct{ sid, tmuxSess, win string }{{focusSID, "main", "@1"}, {splitSID, "side", "@3"}} {
		body := mcpToolCall(pageCreateTool, map[string]any{"title": "Recorded window", "blocks": json.RawMessage(`[{"type":"markdown","text":"hi"}]`)})
		code, mr, raw := f.mirrorRPC(t, mirrorAs(f.ws, tc.sid), body)
		if code != http.StatusOK || mr == nil || mr.Error != nil {
			t.Fatalf("%s: %d %.300s", tc.sid, code, raw)
		}
		text, isErr := toolTextRaw(t, mr)
		if isErr {
			t.Fatalf("%s: refused: %.300s", tc.sid, text)
		}
		id, _ := f.createdPage(t, text)
		var sess, tmuxSess, win string
		if err := f.pool.QueryRow(authz.System(context.Background()),
			`SELECT coalesce(mirror_session, ''), coalesce(mirror_tmux_session, ''), coalesce(mirror_window, '') FROM pages WHERE id = $1`, id).
			Scan(&sess, &tmuxSess, &win); err != nil {
			t.Fatal(err)
		}
		if sess != tc.sid || tmuxSess != tc.tmuxSess || win != tc.win {
			t.Errorf("%s's page recorded session %q, window %q:%q; want %s:%s", tc.sid, sess, tmuxSess, win, tc.tmuxSess, tc.win)
		}
	}
}

func containsAll(s string, subs ...string) bool {
	for _, x := range subs {
		if !strings.Contains(s, x) {
			return false
		}
	}
	return true
}
