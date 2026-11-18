package api

import (
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/pages"
	"github.com/ZacxDev/muster/internal/ui"
)

// A SESSION'S PAGES BESIDE ITS PREVIEWS (plugin_chip.go): the session view's
// Open button counts the pages its session CREATED — with no viewer plugin at
// all — and its panel's Pages tab lists exactly them, newest first. Through
// the real page_create tool (a mirrored session's, an agent's), the real
// pages store, the real routes. Every name, id and host is invented.

// sessionPageFrom has session sid on the owner's host workstation make a page.
func sessionPageFrom(t *testing.T, f *pageFixture, owner int64, sid, title string) int64 {
	t.Helper()
	out := f.toolAs(t, f.as(owner, authz.ViaMirrorHost), mirrorCaller("workstation", sid), pageCreateTool,
		map[string]any{"title": title, "blocks": json.RawMessage(`[{"type":"markdown","text":"hi"}]`)})
	id := pnum(out["page_id"])
	if id <= 0 {
		t.Fatalf("page_create: %v", out)
	}
	return id
}

// windowQuery is the owner's window @id on workstation, as the slot names it.
func windowQuery(id string) string {
	return "host=workstation&session=main&window=" + strings.ReplaceAll(id, "@", "%40")
}

var listedPage = regexp.MustCompile(`data-session-page="(\d+)"`)

// listedPages is the page ids a Pages tab answer lists, in order.
func listedPages(body string) []int64 {
	var out []int64
	for _, m := range listedPage.FindAllStringSubmatch(body, -1) {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		out = append(out, n)
	}
	return out
}

// TestTheOpenButtonCountsASessionsPagesWithNoViewerPlugin: a window whose
// session made pages draws the Open button, counting them, though the owner
// has NO viewer plugin (red before: the button was the viewer plugin's and
// the window's previews' only — no plugin, no slot, no button, no route
// answer). Another window's session's pages, an agent's and another owner's
// from the same host and session are not counted.
func TestTheOpenButtonCountsASessionsPagesWithNoViewerPlugin(t *testing.T) {
	f, m := newSessionPageFixture(t)
	for i := range 3 {
		sessionPageFrom(t, f, f.owner, makerSID, "maker "+strconv.Itoa(i))
	}
	sessionPageFrom(t, f, f.owner, otherSID, "other window's")
	f.create(t, f.agent, "An agent's", `[{"type":"markdown","text":"x"}]`)
	// Another owner, a host of the same name, the same session id: theirs.
	m.set(f.other, sessionWindow("@3", 3, makerSID))
	sessionPageFrom(t, f, f.other, makerSID, "another owner's")
	sessionPageFrom(t, f, f.other, makerSID, "another owner's too")

	button := func(win string) string {
		t.Helper()
		w := f.get(t, ui.PreviewButtonPath+"?"+windowQuery(win), f.ownerHdr())
		if w.Code != http.StatusOK {
			t.Fatalf("the button route for %s: %d %s", win, w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	b := button("@3")
	for _, want := range []string{`id="preview-toggle"`, `data-preview-count="3"`, `data-open-pages="3"`, `data-open-previews="0"`, `aria-label="Open: 3 pages"`} {
		if !strings.Contains(b, want) {
			t.Errorf("@3's button lacks %s:\n%s", want, b)
		}
	}
	if b := button("@4"); !strings.Contains(b, `data-preview-count="1"`) || !strings.Contains(b, `aria-label="Open: 1 page"`) {
		t.Errorf("@4's button: %s", b)
	}
	// A window whose session made none: no button.
	m.set(f.owner, sessionWindow("@3", 3, makerSID), sessionWindow("@4", 4, otherSID), sessionWindow("@5", 5, "0d1e2f3a-aaaa-4bbb-8ccc-0000000000cc"))
	if b := button("@5"); b != "" {
		t.Errorf("@5 made no page, its button route answered %q", b)
	}
	// The page itself (the window's Terminal view: this fake host draws no
	// Rendered one) draws the slot (polling) and the button, and the panel
	// with its Pages tab, with no viewer plugin.
	w := f.get(t, "/mirror/terminal?"+windowQuery("@3"), f.ownerHdr())
	if w.Code != http.StatusOK {
		t.Fatalf("the Terminal view: %d", w.Code)
	}
	doc := w.Body.String()
	for _, want := range []string{`id="preview-slot"`, `id="preview-toggle"`, `data-preview-count="3"`, `id="preview-panel"`,
		`data-pages-src="` + ui.SessionPagesPath + `?`, `data-open-empty="previews"`} {
		if !strings.Contains(doc, want) {
			t.Errorf("the Terminal view lacks %s", want)
		}
	}
	if strings.Contains(doc, `data-mint=`) {
		t.Error("the panel carries the mint's data with no viewer plugin to mint for")
	}
}

// TestThePagesTabListsExactlyTheSessionsPagesNewestFirst: the Pages tab
// lists the window's session's pages, newest made first, each with its
// current version and whether it is shared — and none of another window's,
// an agent's, or another owner's; another owner's request for the same
// window lists theirs alone.
func TestThePagesTabListsExactlyTheSessionsPagesNewestFirst(t *testing.T) {
	f, m := newSessionPageFixture(t)
	first := sessionPageFrom(t, f, f.owner, makerSID, "First made")
	second := sessionPageFrom(t, f, f.owner, makerSID, "Second made")
	third := sessionPageFrom(t, f, f.owner, makerSID, "Third made")
	sessionPageFrom(t, f, f.owner, otherSID, "Other window's")
	f.create(t, f.agent, "An agent's", `[{"type":"markdown","text":"x"}]`)
	m.set(f.other, sessionWindow("@3", 3, makerSID))
	theirs := sessionPageFrom(t, f, f.other, makerSID, "Another owner's")
	// The first made gets a new version (now the most recently ACTIVE) and a
	// live link: it stays last, at v2, shared.
	out := f.toolAs(t, f.as(f.owner, authz.ViaMirrorHost), mirrorCaller("workstation", makerSID), pageUpdateTool,
		map[string]any{"page_id": first, "title": "First made", "blocks": json.RawMessage(`[{"type":"markdown","text":"v2"}]`)})
	if pnum(out["version"]) != 2 {
		t.Fatalf("page_update: %v", out)
	}
	f.liveLink(t, f.owner, first)

	list := func(hdr map[string]string) string {
		t.Helper()
		w := f.get(t, ui.SessionPagesPath+"?"+windowQuery("@3"), hdr)
		if w.Code != http.StatusOK {
			t.Fatalf("the Pages tab: %d %s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	body := list(f.ownerHdr())
	if got, want := listedPages(body), []int64{third, second, first}; !slices.Equal(got, want) {
		t.Fatalf("the Pages tab lists %v, want %v (newest made first, this session's only)", got, want)
	}
	for _, want := range []string{
		`data-session-page="` + strconv.FormatInt(first, 10) + `" data-page-embed="/pages/` + strconv.FormatInt(first, 10) + `/view?embed=1" data-page-view="/pages/` + strconv.FormatInt(first, 10) + `/view" data-page-title="First made" data-page-version="v2"`,
		`data-page-version="v1"`, `data-page-share="true"`, `data-page-share="false"`, `Shared · live link`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the Pages tab lacks %s:\n%s", want, body)
		}
	}
	if n := strings.Count(body, `data-page-share="true"`); n != 1 {
		t.Errorf("%d pages shown shared, want 1", n)
	}
	for _, not := range []string{"Other window", "An agent", "Another owner"} {
		if strings.Contains(body, not) {
			t.Errorf("the Pages tab lists %q", not)
		}
	}
	// The other owner, at THEIR window of the same name: theirs alone.
	f.session = f.other
	if got, want := listedPages(list(f.ownerHdr())), []int64{theirs}; !slices.Equal(got, want) {
		t.Errorf("the other owner's Pages tab lists %v, want %v", got, want)
	}
	f.session = f.owner
	// A window the viewer's hosts do not list: the empty list.
	if b := f.get(t, ui.SessionPagesPath+"?host=workstation&session=main&window=%4099", f.ownerHdr()).Body.String(); !strings.Contains(b, ui.SessionPagesNone) || len(listedPages(b)) != 0 {
		t.Errorf("an unlisted window's Pages tab = %q", b)
	}
}

// TestAnAgentSessionListsItsAgentsPages: an agent's page draws the Open
// button with its agent's pages (and its own routes answer them, newest
// first); another agent's are not its.
func TestAnAgentSessionListsItsAgentsPages(t *testing.T) {
	f, _ := newSessionPageFixture(t)
	a1 := f.create(t, f.agent, "Agent first", `[{"type":"markdown","text":"x"}]`)
	a2 := f.create(t, f.agent, "Agent second", `[{"type":"markdown","text":"x"}]`)
	f.create(t, f.agent2, "The other agent's", `[{"type":"markdown","text":"x"}]`)
	sessionPageFrom(t, f, f.owner, makerSID, "A window's")
	w := f.get(t, ui.AgentOpenButtonPath(f.agent.Name), f.ownerHdr())
	if b := w.Body.String(); w.Code != http.StatusOK || !strings.Contains(b, `data-preview-count="2"`) || !strings.Contains(b, `aria-label="Open: 2 pages"`) {
		t.Fatalf("the agent's button: %d %s", w.Code, b)
	}
	w = f.get(t, ui.AgentSessionPagesPath(f.agent.Name), f.ownerHdr())
	if got, want := listedPages(w.Body.String()), []int64{a2, a1}; w.Code != http.StatusOK || !slices.Equal(got, want) {
		t.Fatalf("the agent's Pages tab: %d %v, want %v", w.Code, got, want)
	}
	// Another owner's agent reads as a missing one (row-level security).
	f.session = f.other
	for _, p := range []string{ui.AgentOpenButtonPath(f.agent.Name), ui.AgentSessionPagesPath(f.agent.Name)} {
		if w := f.get(t, p, f.ownerHdr()); w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "data-session-page") {
			t.Errorf("another owner's GET %s = %d %q, want 404", p, w.Code, w.Body.String())
		}
	}
	f.session = f.owner
}

// TestATeamWindowListsNoPages: a team host's window (e.Owner != 0) names no
// session to list (windowPagesRef): a team's pages are its own rows, which a
// member never reads, and the viewer's own pages of a host of the same NAME
// are not this window's. Its own window of the same name and session does.
func TestATeamWindowListsNoPages(t *testing.T) {
	h := mirror.HostView{Host: "workstation", Online: true}
	h.Windows = mirror.NormalizeWindows([]mirror.Window{sessionWindow("@3", 3, makerSID)}, 0)
	own := mirror.SessionView{Host: "workstation", Session: "main", Window: "@3"}
	if got := windowPagesRef(h, own); !got.Valid() || got.MirrorHost != "workstation" || !slices.Equal(got.MirrorSessions, []string{makerSID}) {
		t.Fatalf("CONTROL: the viewer's own window names %+v", got)
	}
	team := own
	team.Owner = 41
	if got := windowPagesRef(h, team); got.Valid() {
		t.Errorf("a team window names %+v, want nothing", got)
	}
	gone := own
	gone.Window = "@9"
	if got := windowPagesRef(h, gone); got.Valid() {
		t.Errorf("a window the host does not report names %+v", got)
	}
}

// TestTheOwnersViewIsFramedByMusterOnlyEmbedded: /pages/{id}/view?embed=1 —
// the panel's frame — may be framed by muster's own origin (frame-ancestors
// 'self', X-Frame-Options SAMEORIGIN), the rest of its policy unchanged;
// without embed, and a share link's shell with it, by no page.
func TestTheOwnersViewIsFramedByMusterOnlyEmbedded(t *testing.T) {
	f, _ := newSessionPageFixture(t)
	id := sessionPageFrom(t, f, f.owner, makerSID, "Framed")
	view := "/pages/" + strconv.FormatInt(id, 10) + "/view"
	w := f.get(t, view+"?embed=1", f.ownerHdr())
	if w.Code != http.StatusOK {
		t.Fatalf("embed: %d", w.Code)
	}
	csp := w.Header().Get("Content-Security-Policy")
	nonce := regexp.MustCompile(`'nonce-([^']+)'`).FindStringSubmatch(csp)
	if len(nonce) != 2 || csp != pages.EmbedShellCSP(nonce[1]) || w.Header().Get("X-Frame-Options") != "SAMEORIGIN" {
		t.Errorf("embed: CSP %q XFO %q", csp, w.Header().Get("X-Frame-Options"))
	}
	if !strings.Contains(csp, "frame-ancestors 'self';") {
		t.Errorf("embed CSP %q", csp)
	}
	// Its version links stay embedded; the session button is not drawn (it
	// is in that session).
	if b := w.Body.String(); !strings.Contains(b, `?v=1&amp;embed=1"`) || strings.Contains(b, `data-page-session`) || !strings.Contains(b, `target="_top" data-page-details`) {
		t.Errorf("the embedded view's links:\n%s", b)
	}
	w = f.get(t, view, f.ownerHdr())
	csp = w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors 'none';") || w.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("the full view: CSP %q XFO %q", csp, w.Header().Get("X-Frame-Options"))
	}
	l := f.liveLink(t, f.owner, id)
	w = f.get(t, pages.LinkPath(l.Token)+"?embed=1", nil)
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none';") || w.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("a share link's shell with embed=1: CSP %q XFO %q", w.Header().Get("Content-Security-Policy"), w.Header().Get("X-Frame-Options"))
	}
}
