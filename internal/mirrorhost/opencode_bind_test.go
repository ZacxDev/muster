package mirrorhost

import (
	"context"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/opencodehost"
	"github.com/ZacxDev/muster/internal/opencodehost/opencodetest"
)

// HAND-OPENED OPENCODE PANES (opencode_bind.go): a pane muster's launcher
// did not start is SENT to only when its title binds it unambiguously and the
// binding is verified through opencode's own API; every ambiguous case stays
// view-only and says why. And a send never reaches a session other than the
// one the pane — and so muster's view — shows.

// streamUp opens src's event stream and waits for it.
func streamUp(t *testing.T, srv *opencodetest.Server, src *OpenCodeSource) {
	t.Helper()
	_ = src.state()
	waitUntil(t, "the event stream", func() bool { s, _ := src.status("x"); return s != "" && srv.Subscribers() > 0 })
}

// TestAWholeUniqueTitleOnAnOpenStreamBindsVerified: the control — a TUI on
// the shared server whose whole title names exactly one root session, its
// command line naming none or that very one, the stream open: verified, the
// window listed AgentBind "verified" (and NOT AgentSend: that is the
// launcher's). A CUT title whose prefix starts exactly one session — of every
// session, roots and children — binds too. And each ambiguous case is not
// verified, with its own code: a child (subagent's) session sharing the whole
// title, or the start of a cut one.
func TestAWholeUniqueTitleOnAnOpenStreamBindsVerified(t *testing.T) {
	titles := map[string]string{"ses_fixtureA": "Fix the flaky fixture", "ses_fixtureB": "Write the release notes for the fixture project now",
		"ses_fixtureC": "Shared with a subagent", "ses_fixtureD": "Investigate the failing fixture tests in the backend"}
	srv, src := ocServer(t, titles)
	// Children (subagents' sessions): one titled exactly like C, one whose
	// title starts like D's cut title, and one unrelated under A.
	srv.PutSession("ses_fixtureKidC", "Shared with a subagent", "ses_fixtureC")
	srv.PutSession("ses_fixtureKidD", "Investigate the failing fixture tests (@explore subagent)", "ses_fixtureD")
	srv.PutSession("ses_fixtureKidA", "Unrelated subagent work", "ses_fixtureA")
	streamUp(t, srv, src)
	locate := func(title string, argv ...string) AgentPane {
		t.Helper()
		procs, children, root := ocPaneTable(t, append([]string{"opencode"}, argv...)...)
		return openCodeRuntime{proc: root, src: src, title: titled(title)}.LocatePanes(ocPane, procs, children)["%1"]
	}
	for _, c := range []struct {
		name, title string
		argv        []string
		gen, bind   string
	}{
		{"a whole unique title", "OC | Fix the flaky fixture", nil, "ses_fixtureA", mirror.AgentBindVerified},
		{"argv naming that session", "OC | Fix the flaky fixture", []string{"-s", "ses_fixtureA"}, "ses_fixtureA", mirror.AgentBindVerified},
		{"argv naming it with =", "OC | Fix the flaky fixture", []string{"--session=ses_fixtureA"}, "ses_fixtureA", mirror.AgentBindVerified},
		{"argv naming ANOTHER session", "OC | Fix the flaky fixture", []string{"--session", "ses_fixtureB"}, "ses_fixtureA", mirror.AgentBindArgv},
		// opencode cuts a title at 37 characters and adds "…".
		{"a cut title, the start of one session", "OC | Write the release notes for the fix…", nil, "ses_fixtureB", mirror.AgentBindVerified},
		{"a whole title a child session shares", "OC | Shared with a subagent", nil, "ses_fixtureC", mirror.AgentBindTitleShared},
		{"a cut title a child session starts with", "OC | Investigate the failing fixture te…", nil, "ses_fixtureD", mirror.AgentBindTitleShared},
	} {
		ap := locate(c.title, c.argv...)
		if ap.Gen != c.gen || ap.Bind != c.bind || ap.Send || ap.Source != SourceTitle {
			t.Errorf("%s: gen %q bind %q send %v source %q; want gen %q bind %q, not Send", c.name, ap.Gen, ap.Bind, ap.Send, ap.Source, c.gen, c.bind)
		}
	}
}

// TestAmbiguousTitlesAreNeverBound: two sessions sharing the title (a title
// collision), and a cut title that is the start of two sessions, are no
// match at all — no session, no binding, and the reason on the card.
func TestAmbiguousTitlesAreNeverBound(t *testing.T) {
	srv, src := ocServer(t, map[string]string{"ses_fixtureA": "Shared title", "ses_fixtureB": "Shared title",
		"ses_fixtureC": "Investigate the failing fixture tests in the backend", "ses_fixtureD": "Investigate the failing fixture tests in CI"})
	streamUp(t, srv, src)
	for name, title := range map[string]string{
		"a title collision":             "OC | Shared title",
		"a cut title, the start of two": "OC | Investigate the failing fixture te…",
	} {
		procs, children, root := ocPaneTable(t, "opencode")
		ap := openCodeRuntime{proc: root, src: src, title: titled(title)}.LocatePanes(ocPane, procs, children)["%1"]
		if ap.Gen != "" || ap.Bind != "" || ap.Send || !strings.Contains(ap.Note, "2 opencode sessions match the title") {
			t.Errorf("%s: gen %q bind %q send %v note %q", name, ap.Gen, ap.Bind, ap.Send, ap.Note)
		}
	}
}

// TestAServerThatDoesNotAnswerIsNeverBound: a server whose session list
// cannot be read gives no view at all; one whose list was read but whose
// event stream is down (the list may be stale) keeps the view, unbound,
// AgentBindStream.
func TestAServerThatDoesNotAnswerIsNeverBound(t *testing.T) {
	srv, src := ocServer(t, map[string]string{"ses_fixtureA": "Fix the flaky fixture"})
	srv.FailEvents(true)
	procs, children, root := ocPaneTable(t, "opencode")
	ap := openCodeRuntime{proc: root, src: src, title: titled("OC | Fix the flaky fixture")}.LocatePanes(ocPane, procs, children)["%1"]
	if ap.Gen != "ses_fixtureA" || ap.Bind != mirror.AgentBindStream || ap.Send {
		t.Fatalf("a stream that is down: gen %q bind %q send %v", ap.Gen, ap.Bind, ap.Send)
	}
	// No server at all (nothing listening): no session list, no view.
	creds, err := opencodehost.EnvCredentials("http://127.0.0.1:1", srv.Issue())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	gone := NewOpenCodeSource(ctx, creds, "", log.New(io.Discard, "", 0))
	ap = openCodeRuntime{proc: root, src: gone, title: titled("OC | Fix the flaky fixture")}.LocatePanes(ocPane, procs, children)["%1"]
	if ap.Gen != "" || ap.Bind != "" || ap.Send || !strings.Contains(ap.Note, "cannot be read") {
		t.Fatalf("an unreachable server: gen %q bind %q send %v note %q", ap.Gen, ap.Bind, ap.Send, ap.Note)
	}
}

// TestTheSessionArgIsReadFromTheTUIsCommandLine: -s/--session's value, as
// a separate word or after "=", and never another flag's value.
func TestTheSessionArgIsReadFromTheTUIsCommandLine(t *testing.T) {
	for want, argv := range map[string][]string{
		"ses_one":   {"opencode", "-s", "ses_one"},
		"ses_two":   {"opencode", "--session=ses_two", "/srv/fixture"},
		"ses_three": {"opencode", "--prompt", "-s", "--session", "ses_three"},
		"":          {"opencode", "--prompt", "-s"},
	} {
		if got := openCodeSessionArg(argv); got != want {
			t.Errorf("openCodeSessionArg(%q) = %q, want %q", argv, got, want)
		}
	}
}

// titledOCBackend lists one opencode window @7 (pane %7, TUI pid 70, no
// launcher record) titled whatever title holds NOW.
type titledOCBackend struct {
	*windowsBackend
	src    *OpenCodeSource
	mu     sync.Mutex
	title  string
	client *Client
}

func (b *titledOCBackend) OpenCodeSource() *OpenCodeSource { return b.src }
func (b *titledOCBackend) set(title string) {
	b.mu.Lock()
	b.title = title
	b.mu.Unlock()
}
func (b *titledOCBackend) Windows(context.Context) ([]WindowInfo, error) {
	b.mu.Lock()
	title := b.title
	b.mu.Unlock()
	ap := ResolveOpenCodePane(b.src, "%7", 70, title)
	return []WindowInfo{{Session: "o", ID: "@7", Index: 0, Active: true, Pane: "%7", PanePID: 69, Command: "opencode", Agent: &ap}}, nil
}

// handHost runs an -interactive -opencode-send host whose window @7 is a
// hand-opened pane on two idle sessions, Alpha and Beta, titled Alpha.
func handHost(t *testing.T) (*drivenMuster, *opencodetest.Server, *titledOCBackend) {
	t.Helper()
	srv, src := ocServer(t, map[string]string{"ses_fixtureAlpha": "Alpha fixture", "ses_fixtureBeta": "Beta fixture"})
	streamUp(t, srv, src)
	srv.SetStatus("ses_fixtureAlpha", "idle")
	srv.SetStatus("ses_fixtureBeta", "idle")
	waitUntil(t, "idle", func() bool {
		a, at := src.status("ses_fixtureAlpha")
		b, bt := src.status("ses_fixtureBeta")
		return a == mirror.StatusIdle && b == mirror.StatusIdle && at > 0 && bt > 0
	})
	d := newDrivenMuster(t)
	b := &titledOCBackend{windowsBackend: &windowsBackend{}, src: src, title: "OC | Alpha fixture"}
	c, err := New(Config{URL: d.srv.URL, Token: mirror.TokenPrefix + "t", Host: "laptop", Backend: b, Interactive: true,
		OpenCodeSend: true, AllowHTTP: true, Refresh: time.Hour, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	b.client = c
	go func() { _ = c.RunOnce(ctx) }()
	waitUntil(t, "the hello", func() bool { return len(d.of(mirror.TypeHello)) == 1 })
	return d, srv, b
}

// namedPaste is ocPaste naming session as the one muster's list shows.
func namedPaste(id uint32, text string, since int64, msgID, session string) mirror.Msg {
	m := ocPaste(id, text, since, msgID)
	m.AgentSession = session
	return m
}

// TestAVerifiedHandOpenedPaneIsListedSendableAndTakesASend: the window is
// listed AgentBind "verified" (not AgentSend), and a paste naming its
// session is POSTED to exactly that session — once, delivery queue.
func TestAVerifiedHandOpenedPaneIsListedSendableAndTakesASend(t *testing.T) {
	d, srv, _ := handHost(t)
	w := windowOf(t, d)
	if w.AgentSend || w.AgentBind != mirror.AgentBindVerified || w.AgentSession != "ses_fixtureAlpha" {
		t.Fatalf("the hand-opened window: %+v", w)
	}
	d.send(t, namedPaste(1, "hello alpha", w.StatusSince, "msg_fixtureone", "ses_fixtureAlpha"))
	if m := pastedOf(t, d, 1); m.Error != "" {
		t.Fatalf("refused: %+v", m)
	}
	if p := srv.Prompts(); len(p) != 1 || p[0].Session != "ses_fixtureAlpha" || p[0].Text != "hello alpha" || p[0].Delivery != "queue" {
		t.Fatalf("admitted %+v; want one, to Alpha", p)
	}
}

// TestASendNeverReachesAnotherSessionThanThePaneShows: every combination of
// the session muster's paste names and the session the pane shows NOW (its
// title moved between muster's listing and the send). A post happens ONLY
// when the two are the same session, and then to exactly it; otherwise the
// refusal is PasteRefusalOpenCodeMoved and nothing is posted anywhere. A
// paste naming no session (an older muster's) is refused for such a pane.
func TestASendNeverReachesAnotherSessionThanThePaneShows(t *testing.T) {
	d, srv, b := handHost(t)
	sids := map[string]string{"OC | Alpha fixture": "ses_fixtureAlpha", "OC | Beta fixture": "ses_fixtureBeta"}
	id := uint32(0)
	posted := 0
	for showing, shown := range sids {
		for _, named := range []string{"ses_fixtureAlpha", "ses_fixtureBeta", ""} {
			id++
			b.set(showing)
			// The pane's status time is the SHOWN session's (the idle gate's).
			_, since := b.src.status(shown)
			d.send(t, namedPaste(id, "to "+named, since, "msg_fixture"+strings.Repeat("x", int(id)), named))
			m := pastedOf(t, d, id)
			if named == shown {
				if m.Error != "" {
					t.Fatalf("pane shows %s, paste names it: refused %+v", shown, m)
				}
				posted++
			} else if m.Refusal != mirror.PasteRefusalOpenCodeMoved || m.Error != mirror.PasteRefusedOpenCodeMovedText {
				t.Fatalf("pane shows %s, paste names %q: answered %+v; want the moved refusal", shown, named, m)
			}
			// 🔴 Every admitted prompt went to the session the pane showed
			// AND the paste named.
			p := srv.Prompts()
			if len(p) != posted {
				t.Fatalf("pane shows %s, paste names %q: %d prompts admitted, want %d", shown, named, len(p), posted)
			}
			if posted > 0 && named == shown && (p[len(p)-1].Session != shown || p[len(p)-1].Text != "to "+named) {
				t.Fatalf("the post went to %+v, want %s", p[len(p)-1], shown)
			}
		}
	}
	if posted != 2 {
		t.Fatalf("instrument check: %d sends landed, want exactly the 2 matching ones", posted)
	}
}

// TestAVerifiedSendReadsTheSessionAgainFirst: the session the pane resolves
// to is read again through the API at the moment of sending
// (confirmTitleBind): renamed since, a child session, or gone — refused, and
// nothing posted.
func TestAVerifiedSendReadsTheSessionAgainFirst(t *testing.T) {
	srv, src := ocServer(t, map[string]string{"ses_fixtureAlpha": "Alpha fixture"})
	srv.PutSession("ses_fixtureKid", "Alpha fixture kid", "ses_fixtureAlpha")
	cl, err := opencodehost.NewClient(srv.URL, func() string { return srv.Issue() })
	if err != nil {
		t.Fatal(err)
	}
	_ = src
	ctx := context.Background()
	for _, c := range []struct {
		name, id, title, refusal string
	}{
		{"the same session, the same title", "ses_fixtureAlpha", "Alpha fixture", ""},
		{"the same session, a cut title it starts with", "ses_fixtureAlpha", "Alpha fix…", ""},
		{"a cut title it does not start with", "ses_fixtureAlpha", "Beta fix…", mirror.PasteRefusalOpenCodeMoved},
		{"renamed since", "ses_fixtureAlpha", "Alpha fixture (old)", mirror.PasteRefusalOpenCodeMoved},
		{"a child session", "ses_fixtureKid", "Alpha fixture kid", mirror.PasteRefusalOpenCodeMoved},
		{"gone", "ses_fixtureNone", "Alpha fixture", mirror.PasteRefusalOpenCodeGone},
	} {
		why, refusal := confirmTitleBind(ctx, cl, c.id, c.title)
		if refusal != c.refusal || (c.refusal == "") != (why == "") {
			t.Errorf("%s: refusal %q (%q), want %q", c.name, refusal, why, c.refusal)
		}
	}
}

// TestAnUnverifiedHandOpenedPaneIsRefusedInItsOwnWords: a cut title a
// subagent's session starts with too keeps the view but is refused at the
// host with its code's words, nothing posted.
func TestAnUnverifiedHandOpenedPaneIsRefusedInItsOwnWords(t *testing.T) {
	d, srv, b := handHost(t)
	srv.PutSession("ses_fixtureLong", "A long fixture title that opencode cuts in the pane", "")
	srv.PutSession("ses_fixtureLongKid", "A long fixture title that opencode cuts (@general subagent)", "ses_fixtureLong")
	b.src.sessionsChanged()
	b.set("OC | A long fixture title that opencode cut…")
	// The host re-resolves the pane at the moment of sending: no listing needed.
	_, since := b.src.status("ses_fixtureLong")
	d.send(t, namedPaste(1, "hi", since, "msg_fixtureone", "ses_fixtureLong"))
	if m := pastedOf(t, d, 1); m.Refusal != mirror.PasteRefusalOpenCodeUnbound || m.Error != mirror.PasteRefusedOpenCodeTitleSharedText {
		t.Fatalf("answered %+v; want the shared title's refusal", m)
	}
	if p := srv.Prompts(); len(p) != 0 {
		t.Fatalf("admitted %+v", p)
	}
}

// TestAVerifiedPanesViewSaysItIsSentTo: on a host that sends to opencode, the
// view's first row says the title match was verified and is sent to.
func TestAVerifiedPanesViewSaysItIsSentTo(t *testing.T) {
	d, _, _ := handHost(t)
	d.send(t, mirror.Msg{Type: mirror.TypeWatch, ID: 4, Session: "o", Window: "@7"})
	waitUntil(t, "the backfill", func() bool { evs, _, _ := watchEvents(d, 4); return len(evs) >= 1 })
	evs, _, _ := watchEvents(d, 4)
	if !strings.HasPrefix(evs[0].Text, "matched by title, verified: Alpha fixture") || !strings.Contains(evs[0].Output, "only while the pane still shows this session") {
		t.Fatalf("the first row: %+v", evs[0])
	}
}

// TestACutTitleBindsVerifiedAndTakesASend: a hand-opened pane whose title
// opencode CUT, starting exactly one session, is sent to — that session.
func TestACutTitleBindsVerifiedAndTakesASend(t *testing.T) {
	d, srv, b := handHost(t)
	srv.PutSession("ses_fixtureLong", "A long fixture title that opencode cuts in the pane", "")
	b.src.sessionsChanged()
	b.set("OC | A long fixture title that opencode cut…")
	waitUntil(t, "the new session timed idle", func() bool { st, at := b.src.status("ses_fixtureLong"); return st == mirror.StatusIdle && at > 0 })
	_, since := b.src.status("ses_fixtureLong")
	d.send(t, namedPaste(1, "to the long one", since, "msg_fixtureone", "ses_fixtureLong"))
	if m := pastedOf(t, d, 1); m.Error != "" {
		t.Fatalf("refused: %+v", m)
	}
	if p := srv.Prompts(); len(p) != 1 || p[0].Session != "ses_fixtureLong" {
		t.Fatalf("admitted %+v; want one, to the long session", p)
	}
}

// TestAListCutShortIsNeverVerified: when the last read of the sessions hit
// the client's page bound with sessions still to come (sessionsCut), a title
// unique in what was read is NOT verified — list-cut, the composer's reason.
// The control is the same pane on a whole list.
//
// 🔴 REGRESSION TEST. At 2afa5ab8 the cut list was taken for every session.
func TestAListCutShortIsNeverVerified(t *testing.T) {
	srv, src := ocServer(t, map[string]string{"ses_fixtureA": "Fix the flaky fixture"})
	streamUp(t, srv, src)
	procs, children, root := ocPaneTable(t, "opencode")
	locate := func() AgentPane {
		return openCodeRuntime{proc: root, src: src, title: titled("OC | Fix the flaky fixture")}.LocatePanes(ocPane, procs, children)["%1"]
	}
	if ap := locate(); ap.Bind != mirror.AgentBindVerified {
		t.Fatalf("the control, a whole list: bind %q", ap.Bind)
	}
	src.mu.Lock()
	src.sessionsCut = true
	src.mu.Unlock()
	if ap := locate(); ap.Gen != "ses_fixtureA" || ap.Bind != mirror.AgentBindListCut {
		t.Fatalf("a cut list: gen %q bind %q; want list-cut", ap.Gen, ap.Bind)
	}
	if w := mirror.AgentBindWords(mirror.AgentBindListCut); !strings.Contains(w, "holds more sessions than muster reads") {
		t.Fatalf("the words: %q", w)
	}
}

// TestAnEventDuringASessionsReadKeepsTheListStale: a session event that
// lands while a read of the sessions is in flight leaves the list stale, so
// the next use reads again — the read that began before it never clears it.
//
// 🔴 REGRESSION TEST. At 2afa5ab8 the read set stale=false over it.
func TestAnEventDuringASessionsReadKeepsTheListStale(t *testing.T) {
	srv, src := ocServer(t, map[string]string{"ses_fixtureA": "Fix the flaky fixture"})
	streamUp(t, srv, src)
	src.sessionsChanged()
	src.afterRead = func() { src.sessionsChanged() } // the event, mid-read
	if _, err := src.knownSessions(); err != nil {
		t.Fatal(err)
	}
	src.afterRead = nil
	src.mu.Lock()
	stale := src.stale
	src.mu.Unlock()
	if !stale {
		t.Fatal("an event during the read was lost: the list reads as current")
	}
	// The control: a read with no event during it leaves the list current.
	if _, err := src.knownSessions(); err != nil {
		t.Fatal(err)
	}
	src.mu.Lock()
	stale = src.stale
	src.mu.Unlock()
	if stale {
		t.Fatal("control: a quiet read left the list stale")
	}
}
