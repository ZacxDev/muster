package api

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/ui"
)

// TestTheChiefOpensOnTheChatViewedLast: every way into the chief that names no
// chat — the bare page, its sidebar row (a LIST link, &from=list, naming the
// chat that was latest when it was drawn), home and the panel's body — opens
// on the chat the owner viewed last, on either surface (chiefOpenSession,
// chat_sessions.viewed_at); a ?session= naming a chat still wins and becomes
// the one viewed last; a prefetch is not a view; and an archived chat viewed
// last, like a chief nobody has opened, falls back to the primary.
//
// 🔴 THE FIXTURE KEEPS THE THREE RULES APART: the primary, the chat viewed
// (side A) and the chat touched last (side B, the newest updated_at — where a
// delivery to it would have put it) are three different chats, so an answer by
// the primary, by updated_at or by viewed_at each names a different one. On
// main the bare page and the panel answered the primary and the sidebar row
// and home the latest: every "want A" below was red there.
func TestTheChiefOpensOnTheChatViewedLast(t *testing.T) {
	f := newChatsFixture(t, "")
	ctx := asUser(1)
	sys := authz.System(context.Background())
	primary, err := f.srv.primarySession(ctx, f.chief)
	if err != nil {
		t.Fatal(err)
	}
	mk := func() agents.ChatSession {
		s, err := f.srv.ext.Agents.CreateSession(ctx, f.chief.ID, f.chief.Name)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	a, b := mk(), mk()
	base := time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC)
	for id, d := range map[int64]time.Duration{primary.ID: 0, a.ID: time.Hour, b.ID: 2 * time.Hour} {
		if _, err := f.pool.Exec(sys, `UPDATE chat_sessions SET updated_at = $2 WHERE id = $1`, id, base.Add(d)); err != nil {
			t.Fatal(err)
		}
	}
	if latest := f.srv.latestSessionIDs(ctx, []int64{f.chief.ID})[f.chief.ID]; latest != b.ID {
		t.Fatalf("fixture: the latest chat is %d, want side B %d", latest, b.ID)
	}
	name := f.chief.Name
	get := func(path string, extra map[string]string) *http.Response {
		t.Helper()
		hdr := map[string]string{"Cookie": sessionCookieFor(f.srv)}
		for k, v := range extra {
			hdr[k] = v
		}
		return f.call(t, http.MethodGet, path, nil, hdr).Result()
	}
	// opens is the chat a request that names none lands on: the bare page's
	// redirect.
	opens := func(path string) int64 {
		t.Helper()
		w := get(path, nil)
		if w.StatusCode != http.StatusSeeOther {
			t.Fatalf("GET %s: %d, want a 303 to a chat", path, w.StatusCode)
		}
		m := regexp.MustCompile(`[?&]session=(\d+)`).FindStringSubmatch(w.Header.Get("Location"))
		if m == nil {
			t.Fatalf("GET %s: redirect %q names no chat", path, w.Header.Get("Location"))
		}
		id, _ := strconv.ParseInt(m[1], 10, 64)
		return id
	}
	view := func(sid int64, extra map[string]string) {
		t.Helper()
		if w := get(ui.AgentSessionHref(name, sid), extra); w.StatusCode != http.StatusOK {
			t.Fatalf("GET chat %d: %d", sid, w.StatusCode)
		}
	}
	panelOn := func(q string) int64 {
		t.Helper()
		w := f.call(t, http.MethodGet, "/ui/chief/panel"+q, nil, f.operator())
		m := regexp.MustCompile(`data-chief-active-session="(\d+)"`).FindStringSubmatch(w.Body.String())
		if w.Code != http.StatusOK || m == nil {
			t.Fatalf("GET the panel%s: %d, no active session", q, w.Code)
		}
		id, _ := strconv.ParseInt(m[1], 10, 64)
		return id
	}
	home := func() string {
		t.Helper()
		w := f.call(t, http.MethodGet, "/", nil, f.operator())
		return w.Header().Get("HX-Redirect")
	}
	listLink := ui.AgentListHref(name, b.ID) // the sidebar row, drawn while B was latest

	// Nobody has opened it: the primary.
	if got := opens(ui.AgentPath(name)); got != primary.ID {
		t.Errorf("never opened: the bare page opens %d, want the primary %d", got, primary.ID)
	}

	// Side A, viewed on the page.
	view(a.ID, nil)
	if got := opens(ui.AgentPath(name)); got != a.ID {
		t.Errorf("the bare page opens %d, want side A %d (viewed last)", got, a.ID)
	}
	if got := opens(listLink); got != a.ID {
		t.Errorf("the sidebar row opens %d, want side A %d (viewed last), not the latest %d", got, a.ID, b.ID)
	}
	if got, want := home(), ui.AgentSessionHref(name, a.ID); got != want {
		t.Errorf("home goes to %q, want %q", got, want)
	}
	if got := panelOn(""); got != a.ID {
		t.Errorf("the panel opens on %d, want side A %d (viewed on the page)", got, a.ID)
	}

	// Side B, picked in the panel: the page follows.
	if got := panelOn("?session=" + strconv.FormatInt(b.ID, 10)); got != b.ID {
		t.Fatalf("the panel on ?session=%d answered %d", b.ID, got)
	}
	if got := opens(ui.AgentPath(name)); got != b.ID {
		t.Errorf("after the panel showed side B, the page opens %d, want %d", got, b.ID)
	}
	// A list link naming the chat viewed last is not moved.
	if w := get(ui.AgentListHref(name, b.ID), nil); w.StatusCode != http.StatusOK {
		t.Errorf("a list link to the chat viewed last: %d, want 200 (no hop)", w.StatusCode)
	}

	// A prefetch of the primary is not a view.
	view(primary.ID, map[string]string{"Sec-Purpose": "prefetch"})
	if got := opens(ui.AgentPath(name)); got != b.ID {
		t.Errorf("after a prefetch of the primary the page opens %d, want side B %d", got, b.ID)
	}
	// An explicit ?session= wins, and is then the chat viewed last.
	view(primary.ID, nil)
	if got := opens(ui.AgentPath(name)); got != primary.ID {
		t.Errorf("after viewing the primary the page opens %d, want %d", got, primary.ID)
	}
	view(a.ID, nil)
	// A ?session= naming no chat of the chief's is its primary, not the memory.
	if w := get(ui.AgentSessionHref(name, 1<<40), nil); w.StatusCode != http.StatusOK {
		t.Fatalf("GET a stale ?session=: %d", w.StatusCode)
	}
	if got := panelOn("?session=" + strconv.FormatInt(1<<40, 10)); got != primary.ID {
		t.Errorf("a stale ?session= opens %d, want the primary %d", got, primary.ID)
	}

	// Archived while it is the one viewed last: the primary, on every way in.
	view(a.ID, nil)
	cs, _ := f.srv.chiefChatStore()
	if _, err := cs.SetArchived(ctx, f.chief.ID, a.ID, true); err != nil {
		t.Fatal(err)
	}
	if got := opens(ui.AgentPath(name)); got != primary.ID {
		t.Errorf("side A archived: the page opens %d, want the primary %d", got, primary.ID)
	}
	if got := panelOn(""); got != primary.ID {
		t.Errorf("side A archived: the panel opens on %d, want the primary %d", got, primary.ID)
	}
}
