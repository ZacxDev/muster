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

// TestTheSwitchersRowsResolveTheVisitsAgainstTheList: the session switcher's
// rows (they replaced the retired Agents list's "Recently viewed" strip) are
// resolved against the entries the list is built from (agentsListCardsLive):
// Recent is the visits newest first, pinned ones INCLUDED (Pins is a view of
// its own), gone ones skipped; Pins the pinned entries. Fixture: agents 5, 6, 7
// and windows main@1, chief@2 (pinWindows); agent 6 and chief@2 are pinned;
// agent 99 and window main@9 do not exist. (It read GET /ui/agents' out-of-band
// copy of the rows; that partial is retired, and the rows are the switcher's
// own build — s.switcher, what every session page draws.)
func TestTheSwitchersRowsResolveTheVisitsAgainstTheList(t *testing.T) {
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
	s, _ := pinServer(t, &fakeMirror{online: true, windows: pinWindows()}, mp)
	s.ext.Recents = mem
	mp.pinFor(1).Agents[6] = true
	mp.pinFor(1).Mirrors[pins.MirrorKey{Host: "ws", Session: "chief", Window: "@2"}] = true

	sw := s.switcher(operatorCtx(), ui.SwitcherCurrent{})
	if sw == nil {
		t.Fatal("instrument check FAILED: no switcher was built")
	}
	hrefs := func(items []ui.SwitcherItem) []string {
		var out []string
		for _, it := range items {
			out = append(out, it.Href)
		}
		return out
	}
	want := []string{"/agents/idle-newest", "/agents/status-moved", ref("main", "@1").TerminalPath(),
		ref("chief", "@2").TerminalPath(), "/agents/chatting-oldest"}
	if got := hrefs(sw.Recent); !eqStrings(got, want) {
		t.Errorf("Recent = %v, want %v (newest first, pinned included, the gone 99 / main@9 left out)", got, want)
	}
	if got, want := hrefs(sw.Pins), []string{"/agents/status-moved", ref("chief", "@2").TerminalPath()}; !eqStrings(got, want) {
		t.Errorf("Pins = %v, want %v", got, want)
	}
}

// TestASessionPageDrawsTheSwitcherOnFirstPaint: an agent's page draws the
// session switcher's rows WITH their entries in the document itself, so the
// bar paints whole with the page — Recent resolved as above, the page's own
// session left out of it. (It was the retired Agents shell's first paint;
// GET /agents is home's redirect now, and the shell's tabs draw no switcher —
// the control below.)
func TestASessionPageDrawsTheSwitcherOnFirstPaint(t *testing.T) {
	mem := &memRecents{}
	mem.put(recents.Entry{AgentID: 7}) // the page's own agent: never in its Recent
	mem.put(recents.Entry{Mirror: pins.MirrorKey{Host: "ws", Session: "scratch2", Window: "@4"}})
	mem.put(recents.Entry{AgentID: 8})
	s, ts := sendServer(t, newSendAgents(), &turnGateway{})
	s.ext.Mirror = goldHost()
	s.ext.Recents = mem

	_, body := mirrorGet(t, s, ts, "/agents/keen-wren?session=41")
	want := []string{"/agents/plain-otter", amp(ref("scratch2", "@4").TerminalPath())} // 8, then the window
	if got := rowHrefs(switcherRow(t, body, "switcher-list-recent")); !eqStrings(got, want) {
		t.Errorf("first-paint Recent = %v, want %v (newest first, the page's own keen-wren left out)", got, want)
	}

	// Control: the shell (/tasks) draws no switcher at all, so the rows above
	// are the session page's own.
	if _, tasks := mirrorGet(t, s, ts, "/tasks"); strings.Contains(tasks, `id="switcher-list-recent"`) {
		t.Error("the /tasks shell draws a session switcher")
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
