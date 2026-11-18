package api

import (
	"html"
	"net/http"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestAWindowWhoseConversationRunsElsewhereSaysSoEverywhere: a Claude window
// the host reports a live copy for (mirror.Window.LiveCopy) carries the
// warning at the top of its Rendered view and of its Terminal view — and a
// Claude window with none carries it nowhere. The two handlers each pass
// mirror.SessionView.LiveCopyWarning to their page; this pins that seam (a
// handler that drops the assignment fails here only). (The retired Agents
// list's card carried it too; the sidebar row that replaced the card has no
// room for a sentence, so the pages are where it is said.)
func TestAWindowWhoseConversationRunsElsewhereSaysSoEverywhere(t *testing.T) {
	const own, other = "11111111-2222-4333-8444-555555555555", "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	fm := goldHost()
	for i := range fm.windows {
		if fm.windows[i].ID == "@7" {
			fm.windows[i].ClaudeSession, fm.windows[i].LiveCopy = own, other
		}
		if fm.windows[i].ID == "@9" {
			fm.windows[i].ClaudeSession = "99999999-2222-4333-8444-555555555555"
		}
	}
	s, ts := viewServer(t, fm)
	want := html.EscapeString(mirror.SessionView{Claude: true, LiveCopy: other}.LiveCopyWarning())
	if want == "" || !strings.Contains(want, other[:mirror.LiveCopyShortID]) {
		t.Fatalf("instrument check: the warning %q does not name the other session", want)
	}

	for _, page := range []string{ref("scratch2", "@7").ViewPath(), ref("scratch2", "@7").TerminalPath()} {
		resp, body := mirrorGet(t, s, ts, page)
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, `<span data-live-copy-text="">`+want+`</span>`) {
			t.Errorf("%s: %d, want the live-copy banner", page, resp.StatusCode)
		}
	}
	for _, page := range []string{ref("scratch2", "@9").ViewPath(), ref("scratch2", "@9").TerminalPath()} {
		resp, body := mirrorGet(t, s, ts, page)
		if resp.StatusCode != http.StatusOK || strings.Contains(body, "data-live-copy") {
			t.Errorf("%s: %d, a window with no live copy has the banner", page, resp.StatusCode)
		}
	}
}
