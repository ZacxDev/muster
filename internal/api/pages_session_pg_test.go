package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/pages"
)

// THE SESSION THAT MADE A PAGE (docs/pages.md "The owner's view"): a page a
// mirrored session makes records that session's id; its owner's view links
// it — a session button in the header, "Made by" in the version popover and
// on the page's screen — resolved to the session's CURRENT window at the
// moment the owner opens the page, over the owner's OWN hosts only. An
// agent's page links its agent. Unresolvable: no link. A link's viewer: none.
// Every name, id and host is invented.

// pageMirror is a mirror whose hosts are per OWNER: byOwner[uid][host] is the
// windows of that owner's host. Hosts is the one method the page's session
// link reads; the rest is fakeMirror's (nothing, for these owners).
type pageMirror struct {
	*fakeMirror
	mu      sync.Mutex
	byOwner map[int64]map[string][]mirror.Window
}

func (m *pageMirror) Hosts(ctx context.Context) []mirror.HostView {
	p, ok := authz.From(ctx)
	if !ok || p.All() {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []mirror.HostView
	for _, host := range []string{"laptop", "workstation"} {
		ws, ok := m.byOwner[p.UserID][host]
		if !ok {
			continue
		}
		hv := mirror.HostView{Host: host, Online: true}
		hv.Windows = mirror.NormalizeWindows(ws, 0)
		hv.Sessions, hv.Claude, hv.Labels, hv.Activity = mirror.SessionsOf(hv.Windows)
		out = append(out, hv)
	}
	return out
}

// set makes owner's host "workstation" exactly ws (no ws: the host is gone)
// and leaves their other hosts as they are.
func (m *pageMirror) set(owner int64, ws ...mirror.Window) { m.setHost(owner, "workstation", ws...) }

func (m *pageMirror) setHost(owner int64, host string, ws ...mirror.Window) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byOwner[owner] == nil {
		m.byOwner[owner] = map[string][]mirror.Window{}
	}
	if ws == nil {
		delete(m.byOwner[owner], host)
		return
	}
	m.byOwner[owner][host] = ws
}

const (
	makerSID = "0d1e2f3a-aaaa-4bbb-8ccc-0000000000aa"
	otherSID = "0d1e2f3a-aaaa-4bbb-8ccc-0000000000bb"
)

// sessionWindow is a Claude Code window of session sid, at tmux window id.
func sessionWindow(id string, index int, sid string) mirror.Window {
	return mirror.Window{Session: "main", ID: id, Index: index, Claude: true, ClaudeSession: sid, Status: mirror.StatusIdle}
}

// newSessionPageFixture is newPageFixture with a pageMirror; the owner's host
// runs makerSID at @3 and otherSID at @4.
func newSessionPageFixture(t *testing.T) (*pageFixture, *pageMirror) {
	t.Helper()
	f := newPageFixture(t)
	m := &pageMirror{fakeMirror: &fakeMirror{}, byOwner: map[int64]map[string][]mirror.Window{}}
	m.set(f.owner, sessionWindow("@3", 3, makerSID), sessionWindow("@4", 4, otherSID))
	f.srv.ext.Mirror = m
	return f, m
}

// mirrorPage has the session makerSID on host workstation make a page; its id.
func mirrorPage(t *testing.T, f *pageFixture, title string) int64 {
	t.Helper()
	return mirrorPageOn(t, f, "workstation", title)
}

// mirrorPageOn is mirrorPage from host.
func mirrorPageOn(t *testing.T, f *pageFixture, host, title string) int64 {
	t.Helper()
	out := f.toolAs(t, f.as(f.owner, authz.ViaMirrorHost), mirrorCaller(host, makerSID), pageCreateTool,
		map[string]any{"title": title, "blocks": json.RawMessage(`[{"type":"markdown","text":"hi"}]`)})
	id := pnum(out["page_id"])
	if id <= 0 {
		t.Fatalf("page_create: %v", out)
	}
	return id
}

// viewSessionHref is the session button's href in the owner's view of page
// id ("" for none), and the whole document.
func (f *pageFixture) viewSessionHref(t *testing.T, id int64) (string, string) {
	t.Helper()
	w := f.get(t, "/pages/"+strconv.FormatInt(id, 10)+"/view", f.ownerHdr())
	if w.Code != http.StatusOK {
		t.Fatalf("the owner's view: %d %s", w.Code, w.Body.String())
	}
	doc := w.Body.String()
	markup := doc[:strings.Index(doc, "<script nonce=")]
	i := strings.Index(markup, `<a class="sess" href="`)
	if i < 0 {
		return "", doc
	}
	rest := markup[i+len(`<a class="sess" href="`):]
	return rest[:strings.Index(rest, `"`)], doc
}

// TestAPageFromAMirroredSessionRecordsTheSession: page_create from a mirrored
// session records the session id it came from (with its host); an agent's
// page records none (its agent is its maker).
func TestAPageFromAMirroredSessionRecordsTheSession(t *testing.T) {
	f, _ := newSessionPageFixture(t)
	id := mirrorPage(t, f, "Recorded")
	p, err := f.pages.Page(f.as(f.owner, authz.ViaSession), id)
	if err != nil || p.Creator.MirrorHost != "workstation" || p.Creator.MirrorSession != makerSID || p.Creator.AgentID != 0 {
		t.Fatalf("the page's creator: %+v %v", p.Creator, err)
	}
	aid := f.create(t, f.agent, "An agent's", `[{"type":"markdown","text":"x"}]`)
	if a, _ := f.pages.Page(f.as(f.owner, authz.ViaSession), aid); a.Creator.MirrorSession != "" || a.Creator.AgentID != f.agent.ID {
		t.Fatalf("an agent's page's creator: %+v", a.Creator)
	}
}

// TestTheSessionButtonOpensTheSessionsCurrentWindow: the owner's view links
// the session that made the page — in the header, the popover and on the
// page's screen — at its window NOW: after the session moved from @3 to @7,
// the same page links @7. The control: the other session on the host is not
// the one linked.
func TestTheSessionButtonOpensTheSessionsCurrentWindow(t *testing.T) {
	f, m := newSessionPageFixture(t)
	id := mirrorPage(t, f, "Made here")
	sid := strconv.FormatInt(id, 10)
	href, doc := f.viewSessionHref(t, id)
	if want := "/mirror/view?host=workstation&amp;session=main&amp;window=%403"; href != want {
		t.Fatalf("the session button: %q, want %q", href, want)
	}
	// Labelled by where it runs NOW (its window's current name), not the
	// label stored when the page was made.
	for _, want := range []string{`aria-label="Open the session that made this page: main on workstation"`, `<span class="sl">main on workstation</span>`,
		`<a href="/mirror/view?host=workstation&amp;session=main&amp;window=%403" data-page-made-by>Made by main on workstation</a>`} {
		if !strings.Contains(doc, want) {
			t.Errorf("the owner's view lacks %q", want)
		}
	}
	detail := f.get(t, "/pages/"+sid, f.ownerHdr()).Body.String()
	if !strings.Contains(detail, `Made by <a href="/mirror/view?host=workstation&amp;session=main&amp;window=%403" class="text-accent underline" data-page-session>main on workstation</a>.`) {
		t.Errorf("the page's screen lacks the Made by link: %s", pageDetailSection(t, detail))
	}
	// The window moved (renumbered, a new tmux id), and another session now
	// has the first window: the link follows THIS session to @7.
	m.set(f.owner, sessionWindow("@2", 1, otherSID), sessionWindow("@7", 5, makerSID))
	href, doc = f.viewSessionHref(t, id)
	if href != "/mirror/view?host=workstation&amp;session=main&amp;window=%407" || !strings.Contains(doc, `<span class="sl">main on workstation</span>`) {
		// (The label names the session in words, without tmux's window index,
		// so it reads the same in either window; the href is what moved.)
		t.Fatalf("after the window moved: %q (the label: %v)", href, strings.Contains(doc, "main on workstation"))
	}
}

// TestAnAgentsPageLinksItsAgent: an agent's page's session button opens the
// agent's own page.
func TestAnAgentsPageLinksItsAgent(t *testing.T) {
	f, _ := newSessionPageFixture(t)
	id := f.create(t, f.agent, "By an agent", `[{"type":"markdown","text":"x"}]`)
	href, doc := f.viewSessionHref(t, id)
	if want := "/agents/" + f.agent.Name; href != want {
		t.Fatalf("an agent's page's session button: %q, want %q", href, want)
	}
	if !strings.Contains(doc, `<span class="sl">`+f.agent.Name+`</span>`) {
		t.Error("the button does not name the agent")
	}
}

// TestAnUnresolvableSessionDrawsNoButton: the session no longer runs on any
// window; the host is gone; a mirrored window's page made before the session
// was recorded — no session button, no Made by link, the label alone.
func TestAnUnresolvableSessionDrawsNoButton(t *testing.T) {
	f, m := newSessionPageFixture(t)
	id := mirrorPage(t, f, "Gone")
	if href, _ := f.viewSessionHref(t, id); href == "" {
		t.Fatal("control: the running session has no button")
	}
	m.set(f.owner, sessionWindow("@4", 4, otherSID))
	if href, doc := f.viewSessionHref(t, id); href != "" || strings.Contains(doc, "data-page-made-by") {
		t.Errorf("a session that runs nowhere is linked: %q", href)
	}
	if detail := f.get(t, "/pages/"+strconv.FormatInt(id, 10), f.ownerHdr()).Body.String(); !strings.Contains(detail, "Made by workstation · build.") ||
		strings.Contains(pageDetailSection(t, detail), "data-page-session") {
		t.Errorf("the page's screen without the session: %s", pageDetailSection(t, detail))
	}
	m.set(f.owner)
	if href, _ := f.viewSessionHref(t, id); href != "" {
		t.Errorf("a host that is gone is linked: %q", href)
	}
	// A page made before the session was recorded (mirror_session NULL), its
	// host's window running: still no guess.
	m.set(f.owner, sessionWindow("@3", 3, makerSID))
	old := mirrorPage(t, f, "Older")
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE pages SET mirror_session = NULL WHERE id = $1`, old); err != nil {
		t.Fatal(err)
	}
	if href, _ := f.viewSessionHref(t, old); href != "" {
		t.Errorf("a page with no recorded session is linked to %q", href)
	}
}

// TestALinkViewerNeverSeesTheSession: the share link's shell draws no session
// button or Made by, its session running.
func TestALinkViewerNeverSeesTheSession(t *testing.T) {
	f, _ := newSessionPageFixture(t)
	id := mirrorPage(t, f, "Shared")
	if href, _ := f.viewSessionHref(t, id); href == "" {
		t.Fatal("control: the owner's view has the button")
	}
	l := f.liveLink(t, f.owner, id)
	shell := f.get(t, pages.LinkPath(l.Token), nil).Body.String()
	for _, banned := range []string{"data-page-session", "data-page-made-by", "/mirror/view", makerSID, "workstation"} {
		if strings.Contains(shell, banned) {
			t.Errorf("a link's viewer sees %q", banned)
		}
	}
}

// TestAnotherOwnersSessionIsNeverResolved: the session id is looked up only
// on the VIEWER's own hosts. The page owner's session running nowhere of
// theirs, while another member runs a window with the very same session id
// and host name: no button. And the other member cannot open the page.
func TestAnotherOwnersSessionIsNeverResolved(t *testing.T) {
	f, m := newSessionPageFixture(t)
	id := mirrorPage(t, f, "Mine")
	m.set(f.owner, sessionWindow("@4", 4, otherSID))
	m.set(f.other, sessionWindow("@9", 9, makerSID))
	if href, _ := f.viewSessionHref(t, id); href != "" {
		t.Errorf("another owner's window was resolved: %q", href)
	}
	f.session = f.other
	if w := f.get(t, "/pages/"+strconv.FormatInt(id, 10)+"/view", f.ownerHdr()); w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "/mirror/view") {
		t.Errorf("another member's view: %d", w.Code)
	}
}

// TestTheSessionIsLookedUpOnItsOwnHost (two hosts): a page made on "laptop"
// links laptop's window, workstation running another session; and a page
// made on "workstation" is not linked to a window of ANOTHER host that
// happens to run the same session id.
func TestTheSessionIsLookedUpOnItsOwnHost(t *testing.T) {
	f, m := newSessionPageFixture(t)
	m.setHost(f.owner, "laptop", sessionWindow("@2", 2, makerSID))
	m.set(f.owner, sessionWindow("@4", 4, otherSID))
	id := mirrorPageOn(t, f, "laptop", "On the laptop")
	if href, _ := f.viewSessionHref(t, id); href != "/mirror/view?host=laptop&amp;session=main&amp;window=%402" {
		t.Fatalf("a laptop page's session button: %q", href)
	}
	ws := mirrorPage(t, f, "On the workstation")
	if href, _ := f.viewSessionHref(t, ws); href != "" {
		t.Errorf("a workstation page linked another host's window: %q", href)
	}
}

// TestTheSessionIsNamedAsMusterNamesIt: the label is the session's CURRENT
// human name (mirrorLabel: its stored manual name here), plained — in the
// header button, the popover's "Made by" and on the page's screen; with no
// name, its window and host.
func TestTheSessionIsNamedAsMusterNamesIt(t *testing.T) {
	f, _ := newSessionPageFixture(t)
	id := mirrorPage(t, f, "Named")
	if _, doc := f.viewSessionHref(t, id); !strings.Contains(doc, `<span class="sl">main on workstation</span>`) {
		t.Fatal("control: an unnamed session is not its window and host")
	}
	f.srv.ext.Focus = &memFocus{rows: map[focus.Subject]focus.Focus{
		focus.MirrorSubject(makerSID): {Subject: focus.MirrorSubject(makerSID), Name: focus.Name{Manual: "**Checkout** rewrite"}},
	}}
	_, doc := f.viewSessionHref(t, id)
	for _, want := range []string{`<span class="sl">Checkout rewrite</span>`, `data-page-made-by>Made by Checkout rewrite</a>`,
		`aria-label="Open the session that made this page: Checkout rewrite"`} {
		if !strings.Contains(doc, want) {
			t.Errorf("the owner's view lacks %q", want)
		}
	}
	if detail := pageDetailSection(t, f.get(t, "/pages/"+strconv.FormatInt(id, 10), f.ownerHdr()).Body.String()); !strings.Contains(detail, `data-page-session>Checkout rewrite</a>.`) {
		t.Errorf("the page's screen: %s", detail)
	}
}

// TestAForeignAgentIsNeverLinked — an INVARIANT GUARD: a page whose agent_id
// names another owner's agent (it cannot be made so through the tools; set
// by hand here) links nothing. Row-level security already hides that agent
// from the owner's Agents.Get, so pageSessionLink's own OwnerID check is not
// what holds it here (it stays for a team agent a member can read, 0131).
func TestAForeignAgentIsNeverLinked(t *testing.T) {
	f, _ := newSessionPageFixture(t)
	id := f.create(t, f.agent, "Agent page", `[{"type":"markdown","text":"x"}]`)
	if href, _ := f.viewSessionHref(t, id); href == "" {
		t.Fatal("control: the page's own agent is not linked")
	}
	if _, err := f.pool.Exec(authz.System(context.Background()), `UPDATE pages SET agent_id = $2 WHERE id = $1`, id, f.otherAgent.ID); err != nil {
		t.Fatal(err)
	}
	if href, doc := f.viewSessionHref(t, id); href != "" || strings.Contains(doc, f.otherAgent.Name) {
		t.Errorf("another owner's agent is linked: %q", href)
	}
}
