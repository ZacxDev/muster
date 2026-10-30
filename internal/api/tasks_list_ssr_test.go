package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/sse"
)

// THE TASK BOARD IS IN THE `/` DOCUMENT (ui/tasks_list_ssr.go).

// boardTitles are the fixture's task titles: pairwise distinct, and none of
// them a word the shell spells anywhere else, so finding one in the document
// can only mean its card was drawn.
var boardTitles = []string{
	"Quillwort ledger reconciliation",
	"Bramblefinch cache warmup",
	"Saltmarsh webhook replay",
}

// tasksListOpen is #tasks-list's opening tag in a document.
var tasksListOpen = regexp.MustCompile(`<div id="tasks-list"[^>]*>`)

// boardFixture is a real Postgres with one fresh member owning the three
// boardTitles tasks (aged 90 days, so no relative time printed by two requests
// can tick between them), a server with a bus, and a session as that member.
type boardFixture struct {
	srv   *Server
	h     http.Handler
	bus   *sse.Broadcaster
	user  int64
	store notes.Store
	ctx   context.Context
}

const boardUserHeader = "X-Board-Test-User"

func newBoardFixture(t *testing.T) *boardFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dbtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	sys := authz.System(ctx)
	var uid int64
	if err := pool.QueryRow(sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`,
		"board-"+strconv.FormatInt(time.Now().UnixNano(), 36)).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM notes WHERE owner_id = $1`, `DELETE FROM audit_log WHERE user_id = $1`, `DELETE FROM users WHERE id = $1`} {
			_, _ = pool.Exec(authz.System(context.Background()), q, uid)
		}
	})
	ns := notes.NewPG(pool)
	for _, title := range boardTitles {
		if _, err := ns.Create(asUser(uid), notes.Note{Title: title, Status: notes.StatusOpen}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(sys, `UPDATE notes SET created_at = now() - interval '90 days', updated_at = now() - interval '90 days' WHERE owner_id = $1`, uid); err != nil {
		t.Fatal(err)
	}
	bus := sse.New(16)
	srv := New(bus, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(io.Discard, "", 0))
	srv.UseExtensions(Extensions{Notes: ns})
	srv.sessionUserOverride = func(r *http.Request) int64 {
		v, _ := strconv.ParseInt(r.Header.Get(boardUserHeader), 10, 64)
		return v
	}
	return &boardFixture{srv: srv, h: srv.Handler(), bus: bus, user: uid, store: ns, ctx: ctx}
}

// get is GET path as the fixture's member, through the production chain.
func (f *boardFixture) get(t *testing.T, path string) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil).WithContext(f.ctx)
	r.Header.Set("Cookie", sessionCookieFor(f.srv))
	r.Header.Set(boardUserHeader, strconv.FormatInt(f.user, 10))
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200: %s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// listOf is the document's #tasks-list: its opening tag and everything after.
func listOf(t *testing.T, doc string) (open, rest string) {
	t.Helper()
	loc := tasksListOpen.FindStringIndex(doc)
	if loc == nil {
		t.Fatalf("no #tasks-list in the document")
	}
	return doc[loc[0]:loc[1]], doc[loc[1]:]
}

// TestTheBoardIsInTheFirstPaint: GET /tasks carries the board — every fixture
// task's title inside #tasks-list, no skeleton in its place — and the list
// issues no `load` request of its own while keeping every live trigger. (`/`
// served the board too until home became the chief's chat: nav_agents.go.)
//
// Red on the base tree: the document drew the skeleton and the titles arrived
// only with the list's own GET /ui/tasks.
func TestTheBoardIsInTheFirstPaint(t *testing.T) {
	f := newBoardFixture(t)
	// The Connectors tab on, so its panel follows the board's and bounds it.
	f.srv.ext.Connectors = shellOnlyConnectors{}
	for _, path := range []string{"/tasks"} {
		open, rest := listOf(t, f.get(t, path))
		// The list ends where the panel's next sibling begins; the titles must be
		// INSIDE it, not anywhere in the document.
		end := strings.Index(rest, `<div id="panel-connectors"`)
		if end < 0 {
			t.Fatalf("%s: no #panel-connectors after #tasks-list to bound it", path)
		}
		list := rest[:end]
		for _, title := range boardTitles {
			if !strings.Contains(list, title) {
				t.Errorf("%s: #tasks-list does not carry the task %q", path, title)
			}
		}
		if n := strings.Count(list, `<article `); n != len(boardTitles) {
			t.Errorf("%s: #tasks-list drew %d cards, want the fixture's %d", path, n, len(boardTitles))
		}
		if strings.Contains(list, `data-tasks-skeleton`) {
			t.Errorf("%s: the drawn board still carries the loading skeleton", path)
		}
		trig := regexp.MustCompile(`hx-trigger="([^"]*)"`).FindStringSubmatch(open)
		if trig == nil {
			t.Fatalf("%s: #tasks-list has no hx-trigger: %s", path, open)
		}
		if strings.HasPrefix(trig[1], "load") || strings.Contains(trig[1], " load,") {
			t.Errorf("%s: the drawn board still loads itself: hx-trigger=%q", path, trig[1])
		}
		for _, live := range []string{"tasks:changed from:body", "sse:task.changed from:body", "sse:agent.changed from:body", "muster:resync from:body"} {
			if !strings.Contains(trig[1], live) {
				t.Errorf("%s: #tasks-list lost its live trigger %q: %q", path, live, trig[1])
			}
		}
		if !strings.Contains(open, `hx-get="/ui/tasks"`) {
			t.Errorf("%s: #tasks-list no longer refreshes from /ui/tasks: %s", path, open)
		}
	}
}

// TestTheBoardDocumentDrawsThePartial: ONE RENDERER — the `/tasks` document's
// #tasks-list holds exactly what GET /ui/tasks answers, byte for byte, for one
// state. Red on the base tree (the document held the skeleton).
func TestTheBoardDocumentDrawsThePartial(t *testing.T) {
	f := newBoardFixture(t)
	part := f.get(t, "/ui/tasks")
	// Control: the partial is the board the fixture made, so a prefix match
	// against it is a match against three real cards.
	if n := strings.Count(part, `<article `); n != len(boardTitles) {
		t.Fatalf("control: /ui/tasks drew %d cards, want %d:\n%s", n, len(boardTitles), part)
	}
	_, rest := listOf(t, f.get(t, "/tasks"))
	if !strings.HasPrefix(rest, part+"</div>") {
		t.Errorf("the /tasks document's #tasks-list is not the /ui/tasks partial.\n--- document list (first 600 bytes):\n%.600s\n--- partial (first 600 bytes):\n%.600s", rest, part)
	}
}

// TestOtherTabsStillLoadTheBoardThemselves: a tab that is not Tasks draws the
// hidden board panel as before — the skeleton and a `load` trigger — and
// carries no mark. INVARIANT GUARD (it held before the change): the change must
// not make every page of the app pay for the board.
func TestOtherTabsStillLoadTheBoardThemselves(t *testing.T) {
	f := newBoardFixture(t)
	// The Connectors tab is drawn only with a connector store; the document
	// itself reads nothing from it (its panel loads /ui/connectors).
	f.srv.ext.Connectors = shellOnlyConnectors{}
	// /connectors: the shell's one other tab (the Agents tab is retired; GET
	// /agents is home's redirect).
	open, rest := listOf(t, f.get(t, "/connectors"))
	if !strings.Contains(open, `hx-trigger="load, `) {
		t.Errorf("the /connectors document's hidden board does not load itself: %s", open)
	}
	if strings.Contains(open, "data-sse-mark") || strings.Contains(open, "data-tasks-drawn") {
		t.Errorf("an undrawn board carries a drawn board's marks: %s", open)
	}
	if !strings.HasPrefix(rest, `<div id="tasks-list-skeleton"`) {
		t.Errorf("the /connectors document drew the board instead of the skeleton: %.200s", rest)
	}
}

// shellOnlyConnectors is a connector store that is only ever present: it
// turns the shell's Connectors tab on (Features.Connectors) and panics if any
// document read it.
type shellOnlyConnectors struct{ connectors.Store }

// markOf is the data-sse-mark #tasks-list carries.
func markOf(t *testing.T, doc string) string {
	t.Helper()
	open, _ := listOf(t, doc)
	m := regexp.MustCompile(`data-sse-mark="([^"]+)"`).FindStringSubmatch(open)
	if m == nil {
		t.Fatalf("#tasks-list carries no data-sse-mark: %s", open)
	}
	return m[1]
}

// helloOf opens GET /events as the fixture's member and returns the mark of
// the stream's muster.hello, which must be its first event.
func (f *boardFixture) helloOf(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/events", nil)
	req.Header.Set("Cookie", sessionCookieFor(f.srv))
	req.Header.Set(boardUserHeader, strconv.FormatInt(f.user, 10))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /events = %d", resp.StatusCode)
	}
	br := bufio.NewReader(resp.Body)
	var lines []string
	for len(lines) < 5 {
		l, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading the stream's opening frames: %v (so far %q)", err, lines)
		}
		lines = append(lines, strings.TrimRight(l, "\n"))
	}
	want := []string{": connected", "", "event: muster.hello"}
	for i, w := range want {
		if lines[i] != w {
			t.Fatalf("stream line %d = %q, want %q (opening: %q)", i, lines[i], w, lines)
		}
	}
	data, ok := strings.CutPrefix(lines[3], "data: ")
	if !ok {
		t.Fatalf("the hello has no data line: %q", lines)
	}
	var hello struct{ Mark string }
	if err := json.Unmarshal([]byte(data), &hello); err != nil || hello.Mark == "" {
		t.Fatalf("the hello's data %q carries no mark (%v)", data, err)
	}
	return hello.Mark
}

// TestTheStreamOpensAtTheDocumentsMarkUntilAnEventLandsBetween pins the
// RELATIONSHIP the first-open skip rests on: a stream opened after the
// document, with no event in between, says hello at the document's mark; an
// event for this user between the two makes them differ; another user's event
// does not. Red on the base tree: the document carried no mark and the stream
// sent no hello.
func TestTheStreamOpensAtTheDocumentsMarkUntilAnEventLandsBetween(t *testing.T) {
	f := newBoardFixture(t)
	ts := httptest.NewServer(f.h)
	t.Cleanup(ts.Close)

	doc := markOf(t, f.get(t, "/tasks"))
	if got := f.helloOf(t, ts); got != doc {
		t.Errorf("no event between the document and the stream, but the hello's mark %q != the document's %q", got, doc)
	}
	// Another user's event is not this reader's: the marks still agree.
	f.srv.broadcastTo(f.user+1_000_000, "task.changed", "1")
	if got := f.helloOf(t, ts); got != doc {
		t.Errorf("another user's event moved this reader's mark: %q != %q", got, doc)
	}
	// This user's event between the document and the stream: they differ.
	f.srv.broadcastTo(f.user, "task.changed", "1")
	if got := f.helloOf(t, ts); got == doc {
		t.Errorf("an event for this user landed after the document was built, and the stream still said hello at the document's mark %q — the first open would skip the refetch and miss it", got)
	}
}

// eventDuringList is a notes store whose ListPage publishes an event for the
// reader WHILE the list is read — a write landing between whatever the
// document reads before the list and whatever it reads after.
type eventDuringList struct {
	notes.Store
	during func()
}

func (e eventDuringList) ListPage(ctx context.Context, f notes.ListFilter) (notes.Page, error) {
	e.during()
	return e.Store.ListPage(ctx, f)
}

// TestTheOfflineShellURLIsBoardless is the SERVER half of the offline shell:
// "/?shell=1" is answered with NO board — no fixture title, the skeleton and
// its own load, no mark. The WORKER half (that sw.js fetches this URL, and
// refuses any "/" carrying data-tasks-drawn at install and carry-forward) is
// pinned by behaviour, not by its source text: e2e/tests/board-first-paint.spec.ts
// (the offline shell holds no task) and e2e/tests/update.spec.ts (a board copy
// is neither installed nor carried forward).
func TestTheOfflineShellURLIsBoardless(t *testing.T) {
	f := newBoardFixture(t)
	shellURL := "/?" + offlineShellParam + "=1"
	const drawn = "data-tasks-drawn"
	// Control: the board's own document (/tasks — `/` itself now opens the
	// chief's chat, and only its ?shell=1 copy is a shell) draws the board and
	// carries the marker the worker looks for, so its absence below is a
	// property of the URL.
	full := f.get(t, "/tasks")
	if !strings.Contains(full, boardTitles[0]) || !strings.Contains(full, drawn) {
		t.Fatalf("control: GET /tasks did not draw the board with %s", drawn)
	}
	shell := f.get(t, shellURL)
	for _, title := range boardTitles {
		if strings.Contains(shell, title) {
			t.Errorf("GET %s carries the task %q: the offline shell would keep this user's board", shellURL, title)
		}
	}
	open, rest := listOf(t, shell)
	if strings.Contains(shell, drawn) || strings.Contains(open, "data-sse-mark") {
		t.Errorf("GET %s draws a board's marks: %s", shellURL, open)
	}
	if !strings.Contains(open, `hx-trigger="load, `) || !strings.HasPrefix(rest, `<div id="tasks-list-skeleton"`) {
		t.Errorf("GET %s is not the skeleton that loads itself: %s %.120s", shellURL, open, rest)
	}
}

// TestTheShellDocumentIsPrivateAndRevalidated: a shell document carries this
// user's board, so no shared cache may keep it and every ordinary navigation
// asks again — but it is not no-store, which would take it out of the
// back/forward cache (restore.go). Red before: "/" sent no Cache-Control.
func TestTheShellDocumentIsPrivateAndRevalidated(t *testing.T) {
	f := newBoardFixture(t)
	// "/?shell=1", not "/": `/` itself is a redirect now (redirectHome).
	// (/agents is not a shell document any more: home's no-store redirect.)
	f.srv.ext.Connectors = shellOnlyConnectors{}
	for _, path := range []string{"/?" + offlineShellParam + "=1", "/tasks", "/connectors"} {
		r := httptest.NewRequest(http.MethodGet, path, nil).WithContext(f.ctx)
		r.Header.Set("Cookie", sessionCookieFor(f.srv))
		r.Header.Set(boardUserHeader, strconv.FormatInt(f.user, 10))
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, r)
		if got := rec.Header().Get("Cache-Control"); got != "private, no-cache" {
			t.Errorf("GET %s: Cache-Control = %q, want \"private, no-cache\"", path, got)
		}
	}
}

// stalledList is a notes store whose board read never answers — not even to
// its context — until release is closed (a stalled database).
type stalledList struct {
	notes.Store
	release chan struct{}
}

func (s stalledList) ListPage(context.Context, notes.ListFilter) (notes.Page, error) {
	<-s.release
	return notes.Page{}, nil
}

// TestAStalledBoardDoesNotHoldTheDocument: past the board-build bound the
// document is served anyway, drawn the old way — the skeleton and its own
// `load` — so a stalled database costs the board a moment, not the page.
// Red without the bound: GET / waits on the read for as long as it stalls.
func TestAStalledBoardDoesNotHoldTheDocument(t *testing.T) {
	f := newBoardFixture(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	f.srv.ext.Notes = stalledList{Store: f.store, release: release}
	f.srv.boardWithinOverride = 50 * time.Millisecond
	got := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequest(http.MethodGet, "/tasks", nil).WithContext(f.ctx)
		r.Header.Set("Cookie", sessionCookieFor(f.srv))
		r.Header.Set(boardUserHeader, strconv.FormatInt(f.user, 10))
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, r)
		got <- rec
	}()
	var doc string
	select {
	case rec := <-got:
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /tasks = %d: %s", rec.Code, rec.Body.String())
		}
		doc = rec.Body.String()
	case <-time.After(5 * time.Second):
		t.Fatal("GET /tasks was still waiting on a stalled board read after 5s (the bound is 50ms)")
	}
	open, rest := listOf(t, doc)
	if !strings.Contains(open, `hx-trigger="load, `) || !strings.HasPrefix(rest, `<div id="tasks-list-skeleton"`) {
		t.Errorf("a stalled board did not fall back to the skeleton that loads itself: %s %.120s", open, rest)
	}
	if strings.Contains(open, "data-tasks-drawn") || strings.Contains(open, "data-sse-mark") {
		t.Errorf("the fallback carries a drawn board's marks: %s", open)
	}
}

// TestTheDocumentReadsItsMarkBeforeItsBoard: an event published while the
// board is being read must leave the document's mark BEHIND the stream's, so
// the first open re-fetches. A mark read after the board would already
// include the event and match the stream's hello — a board that may predate
// the event, with nothing left to correct it.
func TestTheDocumentReadsItsMarkBeforeItsBoard(t *testing.T) {
	f := newBoardFixture(t)
	f.srv.ext.Notes = eventDuringList{Store: f.store, during: func() { f.srv.broadcastTo(f.user, "task.changed", "1") }}
	doc := markOf(t, f.get(t, "/tasks"))
	if after := f.bus.Mark(f.user); after == doc {
		t.Errorf("the document's mark %q includes the event published during its board read: it was read AFTER the board", doc)
	}
}
