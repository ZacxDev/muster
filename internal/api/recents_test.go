package api

import (
	"context"
	"io"
	"log"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/pins"
	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/recents"
	"github.com/ZacxDev/muster/internal/ui"
)

// memRecents is recents.Store in memory, for ONE viewer: a list of visits,
// newest first, deduplicated as the real upsert is. The Postgres behaviour
// (RLS, the bound, the cascade) is internal/recents' own tests'.
type memRecents struct {
	mu sync.Mutex
	es []recents.Entry
}

func (m *memRecents) put(e recents.Entry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []recents.Entry{e}
	for _, x := range m.es {
		if x.AgentID != e.AgentID || x.Mirror != e.Mirror {
			out = append(out, x)
		}
	}
	m.es = out
}

func (m *memRecents) List(context.Context) ([]recents.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]recents.Entry(nil), m.es...), nil
}

func (m *memRecents) VisitAgent(_ context.Context, id int64) error {
	m.put(recents.Entry{AgentID: id})
	return nil
}

func (m *memRecents) VisitMirror(_ context.Context, k pins.MirrorKey) error {
	m.put(recents.Entry{Mirror: k})
	return nil
}

// labels is the store's content as "a<id>" / "<session><window>", newest first.
func (m *memRecents) labels() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, e := range m.es {
		if e.IsMirror() {
			out = append(out, e.Mirror.Session+e.Mirror.Window)
		} else {
			out = append(out, "a"+itoa(e.AgentID))
		}
	}
	return out
}

// TestOpeningAnEntryRecordsAVisit: the agent detail page, a window's rendered
// view and its terminal page each record the entry they RENDERED — by the
// address the card links by — and a redirect records nothing (the page it lands
// on does). Red before: no page recorded anything.
func TestOpeningAnEntryRecordsAVisit(t *testing.T) {
	mem := &memRecents{}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Notes: stubNotesStore{}, Agents: newSendAgents(), SessionLiveness: stubLiveness{},
		Provisioner: newFakeTerminal(), Mirror: goldHost(), Recents: mem})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	steps := []struct {
		path string
		want []string
	}{
		// A bare open redirects to ?session=; only the rendered page records.
		{"/agents/keen-wren", nil},
		{"/agents/keen-wren?session=41", []string{"a7"}},
		// The view of a Claude window.
		{ref("scratch2", "@9").ViewPath(), []string{"scratch2@9", "a7"}},
		// The view of a NON-Claude window redirects to its terminal: nothing yet.
		{ref("scratch2", "@4").ViewPath(), []string{"scratch2@9", "a7"}},
		{ref("scratch2", "@4").TerminalPath(), []string{"scratch2@4", "scratch2@9", "a7"}},
		// Only the session: redirected to its active window, which records.
		{ui.MirrorTerminalPath("ws", "scratch2"), []string{"scratch2@4", "scratch2@9", "a7"}},
		{ref("scratch2", "@7").TerminalPath(), []string{"scratch2@7", "scratch2@4", "scratch2@9", "a7"}},
		// Revisiting moves the entry to the front; it is not a second entry.
		{"/agents/keen-wren?session=41", []string{"a7", "scratch2@7", "scratch2@4", "scratch2@9"}},
		// An entry the viewer cannot open (404) records nothing.
		{"/agents/no-such-agent?session=1", []string{"a7", "scratch2@7", "scratch2@4", "scratch2@9"}},
		{ui.MirrorRef{Host: "not-mine", Session: "x"}.TerminalPath(), []string{"a7", "scratch2@7", "scratch2@4", "scratch2@9"}},
	}
	for _, st := range steps {
		mirrorGet(t, s, ts, st.path)
		if got := mem.labels(); !eqStrings(got, st.want) {
			t.Fatalf("after GET %s: recents = %v, want %v", st.path, got, st.want)
		}
	}
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var switcherEntryHref = regexp.MustCompile(`<a href="([^"]+)" hx-boost="false" (?:data-snap-warm="" )?data-switcher-entry="(agent|mirror)"`)

// switcherRow is the markup of the session switcher's row id in body (its
// opening tag to the row's close).
func switcherRow(t *testing.T, body, id string) string {
	t.Helper()
	at := strings.Index(body, `id="`+id+`"`)
	if at < 0 {
		t.Fatalf("no #%s in:\n%s", id, body)
	}
	r := body[at:]
	return r[:strings.Index(r, "</div>")]
}

func rowHrefs(row string) []string {
	var got []string
	for _, m := range switcherEntryHref.FindAllStringSubmatch(row, -1) {
		got = append(got, m[1])
	}
	return got
}

// TestTheListRefreshesTheSwitchersRows: GET /ui/agents sends the session
// switcher's rows OUT OF BAND (they replaced the "Recently viewed" strip),
// resolved against the cards it renders: Recent is the visits newest first,
// pinned ones INCLUDED (Pins is a view of its own there), gone ones skipped;
// Pins the pinned entries. Fixture: agents 5, 6, 7 and windows main@1, chief@2
// (pinWindows); agent 6 and chief@2 are pinned; agent 99 and window main@9 do
// not exist.
func TestTheListRefreshesTheSwitchersRows(t *testing.T) {
	mem := &memRecents{}
	for _, v := range []recents.Entry{
		{AgentID: 5},
		{Mirror: pins.MirrorKey{Host: "ws", Session: "chief", Window: "@2"}},
		{AgentID: 99},
		{Mirror: pins.MirrorKey{Host: "ws", Session: "main", Window: "@1"}},
		{AgentID: 6},
		{Mirror: pins.MirrorKey{Host: "ws", Session: "main", Window: "@9"}},
		{AgentID: 7},
	} {
		mem.put(v) // put prepends: 7 ends up newest
	}
	mp := &memPins{}
	s, h := pinServer(t, &fakeMirror{online: true, windows: pinWindows()}, mp)
	s.ext.Recents = mem
	mp.pinFor(1).Agents[6] = true
	mp.pinFor(1).Mirrors[pins.MirrorKey{Host: "ws", Session: "chief", Window: "@2"}] = true

	body := getOwed(t, s, h, "/ui/agents").Body.String()
	if strings.Contains(body, "agents-recent") {
		t.Error("the list still sends the Recently viewed strip")
	}
	if !strings.Contains(body, `<div id="switcher-list-recent" hx-swap-oob="innerHTML">`) {
		t.Fatalf("no out-of-band Recent row in the list:\n%s", body)
	}
	got := rowHrefs(switcherRow(t, body, "switcher-list-recent"))
	want := []string{"/agents/idle-newest", "/agents/status-moved", amp(ref("main", "@1").TerminalPath()),
		amp(ref("chief", "@2").TerminalPath()), "/agents/chatting-oldest"}
	if !eqStrings(got, want) {
		t.Errorf("Recent = %v, want %v (newest first, pinned included, the gone 99 / main@9 left out)", got, want)
	}
	if got, want := rowHrefs(switcherRow(t, body, "switcher-list-pins")), []string{"/agents/status-moved", amp(ref("chief", "@2").TerminalPath())}; !eqStrings(got, want) {
		t.Errorf("Pins = %v, want %v", got, want)
	}
}

// TestTheAgentsShellDrawsTheSwitcherOnFirstPaint: GET /agents draws the
// switcher's rows WITH their entries in the document itself (ui.ShellData), so
// the bar paints whole with the page; the list's first load refreshes them in
// place. Same resolution as the list's out-of-band copy. Any other tab draws
// the bar pending — no entries and no "nothing here" line — its Agents panel
// hidden until a switch to it, whose list load fills the rows.
func TestTheAgentsShellDrawsTheSwitcherOnFirstPaint(t *testing.T) {
	mem := &memRecents{}
	mem.put(recents.Entry{AgentID: 5})
	mem.put(recents.Entry{AgentID: 6}) // pinned: still a Recent entry
	mem.put(recents.Entry{AgentID: 7})
	mp := &memPins{}
	s, h := pinServer(t, &fakeMirror{online: true, windows: pinWindows()}, mp)
	s.ext.Recents = mem
	mp.pinFor(1).Agents[6] = true

	body := getOwed(t, s, h, "/agents").Body.String()
	if want := []string{"/agents/idle-newest", "/agents/status-moved", "/agents/chatting-oldest"}; !eqStrings(rowHrefs(switcherRow(t, body, "switcher-list-recent")), want) { // 7, 6, 5
		t.Errorf("first-paint Recent = %v, want %v", rowHrefs(switcherRow(t, body, "switcher-list-recent")), want)
	}
	if strings.Contains(body, "data-switcher-current") {
		t.Error("an entry is drawn current on the Agents list")
	}

	// Control: the board's Agents panel draws the bar pending.
	tasks := getOwed(t, s, h, "/tasks").Body.String()
	row := switcherRow(t, tasks, "switcher-list-recent")
	if switcherEntryHref.MatchString(row) || strings.Contains(row, "No other recent sessions") {
		t.Errorf("the /tasks shell's Recent row should be pending (empty), got: %s", row)
	}
}

// countingInstances is a Provisioner that counts its Instances reads (the
// Kubernetes call the Agents list's live status needs).
type countingInstances struct {
	Provisioner
	n int
}

func (c *countingInstances) Instances(ctx context.Context) ([]provision.Instance, error) {
	c.n++
	return c.Provisioner.Instances(ctx)
}
