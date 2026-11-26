package api

import (
	"context"
	"html"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	focusstore "github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/ui"
)

// TestTheChiefPagesStripIsTheSwitcherOrder: the chief page's strip of chips
// (ui chiefSessionsTab) places the chats in agents.SwitcherOrder — the primary
// first, then the side chats by last activity, newest first, then the archived
// ones, newest first — as the server reads them (chiefChatViews), the seam
// between the store's order and the renderer.
//
// The fixture's activity times are pairwise distinct and NOT in id order (the
// oldest side chat is the newest row, an archived chat was active more recently
// than one side chat), so an order by id, by creation or by activity across the
// groups each draws a different strip. The chat the page is on (side chat B) is
// the one chip marked current, and the panel's body draws the same order.
func TestTheChiefPagesStripIsTheSwitcherOrder(t *testing.T) {
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
	a, b, c, oldArch, newArch := mk(), mk(), mk(), mk(), mk()
	cs, _ := f.srv.chiefChatStore()
	for _, s := range []agents.ChatSession{oldArch, newArch} {
		if _, err := cs.SetArchived(ctx, f.chief.ID, s.ID, true); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC)
	at := map[int64]time.Duration{
		primary.ID: 0, // the oldest of all: the primary is first whatever its age
		a.ID:       50 * time.Minute,
		b.ID:       10 * time.Minute,
		c.ID:       30 * time.Minute, // created last of the three, between a and b
		oldArch.ID: 5 * time.Minute,
		newArch.ID: 40 * time.Minute, // newer than side chat b
	}
	for id, d := range at {
		if _, err := f.pool.Exec(sys, `UPDATE chat_sessions SET updated_at = $2 WHERE id = $1`, id, base.Add(d)); err != nil {
			t.Fatal(err)
		}
	}
	want := []int64{primary.ID, a.ID, c.ID, b.ID, newArch.ID, oldArch.ID}

	chips := func(html string) []int64 {
		var out []int64
		for _, m := range regexp.MustCompile(`<li data-chief-chip="(\d+)"`).FindAllStringSubmatch(html, -1) {
			id, _ := strconv.ParseInt(m[1], 10, 64)
			out = append(out, id)
		}
		return out
	}
	same := func(got []int64) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	page := f.call(t, http.MethodGet, ui.AgentSessionHref(f.chief.Name, b.ID), nil, f.operator())
	if page.Code != http.StatusOK {
		t.Fatalf("GET the chief page: %d", page.Code)
	}
	html := page.Body.String()
	if got := chips(html); !same(got) {
		t.Errorf("the page's strip is %v, want %v (primary, side by activity, archived by activity)", got, want)
	}
	if n := strings.Count(html, `aria-current="true" class=`); n != 1 ||
		!regexp.MustCompile(`data-chief-chat-row="`+strconv.FormatInt(b.ID, 10)+`" title="[^"]*" aria-current="true"`).MatchString(html) {
		t.Errorf("the page's current chip is not chat %d alone (%d current)", b.ID, n)
	}
	if !strings.Contains(html, `>Archived (2)<`) {
		t.Error("the page's strip does not say Archived (2)")
	}

	panel := f.call(t, http.MethodGet, "/ui/chief/panel?session="+strconv.FormatInt(b.ID, 10), nil, f.operator())
	if panel.Code != http.StatusOK {
		t.Fatalf("GET the chief panel: %d", panel.Code)
	}
	if got := chips(panel.Body.String()); !same(got) {
		t.Errorf("the panel's strip is %v, want %v", got, want)
	}
}

// TestTheSideChatRouteAsksForTheComposerAndNothingElseDoes: "+" lands with the
// cursor in the message box because its route's answer asks for it — the
// page's HX-Redirect carries focus=1 and the page drawn at that address
// autofocuses #chat-input; the panel's answer renders its body with the box
// autofocused — while a plain load of the same chat (?session=N, no flag), the
// panel opened on it, and the other chat routes' answers (archive) do not.
func TestTheSideChatRouteAsksForTheComposerAndNothingElseDoes(t *testing.T) {
	f := newChatsFixture(t, "")
	ctx := asUser(1)
	if _, err := f.srv.primarySession(ctx, f.chief); err != nil {
		t.Fatal(err)
	}
	const auto = `<textarea id="chat-input" autofocus`
	page := f.post(t, ui.ChiefSideChatPath(f.chief.ID))
	to := page.Header().Get("HX-Redirect")
	if page.Code != http.StatusOK || !strings.HasSuffix(to, "&focus=1") {
		t.Fatalf("the side chat's page answer: %d → %q, want an address ending &focus=1", page.Code, to)
	}
	sid := strings.TrimSuffix(strings.TrimPrefix(to, ui.AgentSessionHref(f.chief.Name, 0)+"?session="), "&focus=1")
	landed := f.call(t, http.MethodGet, to, nil, f.operator())
	if landed.Code != http.StatusOK || !strings.Contains(landed.Body.String(), auto) {
		t.Errorf("the page the side chat route lands on does not autofocus the message box (%d)", landed.Code)
	}
	plain := f.call(t, http.MethodGet, ui.AgentSessionHref(f.chief.Name, mustID(t, sid)), nil, f.operator())
	if plain.Code != http.StatusOK || strings.Contains(plain.Body.String(), auto) {
		t.Errorf("a plain load of ?session=%s takes the focus (%d)", sid, plain.Code)
	}
	panel := f.post(t, ui.ChiefSideChatPath(f.chief.ID)+"?surface=chief-panel")
	if panel.Code != http.StatusOK || panel.Header().Get("HX-Redirect") != "" || !strings.Contains(panel.Body.String(), auto) {
		t.Errorf("the side chat's panel answer: %d, redirect %q, autofocus %v", panel.Code, panel.Header().Get("HX-Redirect"), strings.Contains(panel.Body.String(), auto))
	}
	opened := f.call(t, http.MethodGet, "/ui/chief/panel?session="+sid, nil, f.operator())
	if opened.Code != http.StatusOK || strings.Contains(opened.Body.String(), auto) {
		t.Errorf("the panel opened on the chat takes the focus (%d)", opened.Code)
	}
	arch := f.post(t, ui.ChiefArchivePath(f.chief.ID, mustID(t, sid), true))
	if strings.Contains(arch.Header().Get("HX-Redirect"), "focus=") {
		t.Errorf("archive's answer asks for the focus: %q", arch.Header().Get("HX-Redirect"))
	}
}

func mustID(t *testing.T, s string) int64 {
	t.Helper()
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("not a session id: %q", s)
	}
	return id
}

// TestThePanelsSheetLoadsTheChiefsFocusHistory: the chief PANEL's body carries
// its own sheet (#chief-panel-index: Sessions, Your messages, What it worked on)
// and, with a focus store, its "What it worked on" tab loads the chief's own
// history (subject agent:<id>), as the chief page's does; with none, the tab is
// the empty state and loads nothing. RED at main: the panel had no sheet, and
// its view carried no focus history.
func TestThePanelsSheetLoadsTheChiefsFocusHistory(t *testing.T) {
	f := newChatsFixture(t, "")
	want := `data-href="` + html.EscapeString(ui.FocusHistoryPath(focusstore.AgentSubject(f.chief.ID).String(), "")) + `"`
	body := func() string {
		r := f.call(t, http.MethodGet, "/ui/chief/panel", nil, f.operator())
		if r.Code != http.StatusOK {
			t.Fatalf("GET the chief panel: %d", r.Code)
		}
		b := r.Body.String()
		i := strings.Index(b, `id="chief-panel-index-tabs-panel-focus"`)
		if i < 0 {
			t.Fatalf("the panel body has no sheet with a focus tab:\n%s", b)
		}
		// The tab's own list, not the scripts after it (which name its marks).
		b = b[i:]
		if j := strings.Index(b, "</ol>"); j >= 0 {
			b = b[:j]
		}
		return b
	}
	f.srv.ext.Focus = nil
	if tab := body(); strings.Contains(tab, "data-focus-history-more") || !strings.Contains(tab, "data-focus-history-empty") {
		t.Error("with no focus store the panel's focus tab should be the empty state, loading nothing")
	}
	f.srv.ext.Focus = focusstore.NewPG(f.pool)
	if tab := body(); !strings.Contains(tab, want) {
		t.Errorf("the panel's focus tab does not load the chief's history (%s)", want)
	}
}
