package mirrorhost

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/opencodehost"
	"github.com/ZacxDev/muster/internal/opencodehost/opencodetest"
)

// THE LAUNCHER BINDING AND THE GATED SEND, HOST HALF.

const ocFixtureSocket = "/srv/fixture/tmux-1000/default"

// launchRecord is a record binding pane to session for the TUI pid started at
// start (the fake proc's stat field 22 is "100"), on srv.
func launchRecord(srv *opencodetest.Server, pane string, pid int, start, session string) opencodehost.LaunchRecord {
	return opencodehost.LaunchRecord{V: opencodehost.LaunchVersion, Socket: ocFixtureSocket, Pane: pane, PID: pid, PIDStart: start,
		Session: session, Server: srv.URL, Directory: "/srv/fixture", Created: 946684800}
}

// boundSource is ocServer with launcher records under a temp dir, bound to
// this daemon's fixture socket.
func boundSource(t *testing.T, titles map[string]string) (*opencodetest.Server, *OpenCodeSource, string) {
	t.Helper()
	srv, src := ocServer(t, titles)
	dir := filepath.Join(t.TempDir(), "launch")
	src.SetLaunches(dir, ocFixtureSocket)
	return srv, src, dir
}

// TestALauncherRecordBindsItsPaneAndOnlyItsPane: a record naming the pane's
// TUI (pid AND start time), this host's server and a root session binds the
// pane to that session, sendable (Send), whatever its title says; a record for
// another process is stale — removed, and the pane title-matched, view-only;
// one for another server, a child session, or a daemon without records binds
// nothing.
func TestALauncherRecordBindsItsPaneAndOnlyItsPane(t *testing.T) {
	titles := map[string]string{"ses_fixtureA": "Fix the flaky fixture", "ses_fixtureB": "Write the docs", "ses_fixtureN": ""}
	procs, children, root := ocPaneTable(t, "opencode", "--session", "ses_fixtureA")
	locate := func(src *OpenCodeSource, title func(string) (string, bool)) AgentPane {
		t.Helper()
		return openCodeRuntime{proc: root, src: src, title: title}.LocatePanes(ocPane, procs, children)["%1"]
	}

	srv, src, dir := boundSource(t, titles)
	if err := opencodehost.WriteLaunch(dir, launchRecord(srv, "%1", 11, "100", "ses_fixtureA")); err != nil {
		t.Fatal(err)
	}
	ap := locate(src, titled("OC | Fix the flaky fixture"))
	if ap.Gen != "ses_fixtureA" || ap.Source != SourceLauncher || !ap.Send || ap.Note != "" {
		t.Fatalf("a bound pane: %+v (%+v)", ap, ap.OpenCode)
	}
	// 🔴 A title naming ANOTHER session never moves the binding: still the
	// recorded session, still sendable — and a warning on the window.
	ap = locate(src, titled("OC | Write the docs"))
	if ap.Gen != "ses_fixtureA" || !ap.Send || ap.Note != TitleMismatchWarning("Write the docs") {
		t.Fatalf("a bound pane titled another session: gen %q send %v note %q", ap.Gen, ap.Send, ap.Note)
	}
	// Titles off, or the title unreadable: bound, no warning.
	for name, title := range map[string]func(string) (string, bool){
		"empty":      titled(""),
		"unreadable": func(string) (string, bool) { return "", false },
	} {
		if ap := locate(src, title); ap.Gen != "ses_fixtureA" || !ap.Send || ap.Note != "" {
			t.Fatalf("%s title: %+v", name, ap)
		}
	}

	// A fresh session has no title: "OpenCode" is consistent with it.
	srv2, src2, dir2 := boundSource(t, titles)
	_ = opencodehost.WriteLaunch(dir2, launchRecord(srv2, "%1", 11, "100", "ses_fixtureN"))
	if ap := locate(src2, titled("OpenCode")); ap.Gen != "ses_fixtureN" || !ap.Send || ap.Note != "" {
		t.Fatalf("a fresh bound session on the home screen: %+v", ap)
	}

	notBound := func(name string, src *OpenCodeSource, rec *opencodehost.LaunchRecord, dir string) {
		t.Helper()
		if rec != nil {
			if err := opencodehost.WriteLaunch(dir, *rec); err != nil {
				t.Fatal(err)
			}
		}
		ap := locate(src, titled("OC | Write the docs"))
		if ap.Send || ap.Source == SourceLauncher || ap.Gen != "ses_fixtureB" || ap.Source != SourceTitle {
			t.Fatalf("%s: %+v; want the title match, unsendable", name, ap)
		}
	}
	srv3, src3, dir3 := boundSource(t, titles)
	r := launchRecord(srv3, "%1", 12, "100", "ses_fixtureA") // another pid
	notBound("another process", src3, &r, dir3)
	if _, err := opencodehost.ReadLaunch(dir3, ocFixtureSocket, "%1"); err != opencodehost.ErrNoLaunch {
		t.Fatalf("a stale record was not removed: %v", err)
	}
	srv4, src4, dir4 := boundSource(t, titles)
	r = launchRecord(srv4, "%1", 11, "999", "ses_fixtureA") // a reused pid
	notBound("a reused pid", src4, &r, dir4)
	srv5, src5, dir5 := boundSource(t, titles)
	r = launchRecord(srv5, "%1", 11, "100", "ses_fixtureA")
	r.Server = "http://127.0.0.1:1"
	notBound("another server", src5, &r, dir5)
	srv6, src6, dir6 := boundSource(t, titles)
	srv6.PutSession("ses_fixtureChild", "a subagent", "ses_fixtureA")
	r = launchRecord(srv6, "%1", 11, "100", "ses_fixtureChild")
	notBound("a child session", src6, &r, dir6)
	srv7, src7 := ocServer(t, titles) // no SetLaunches
	_ = srv7
	notBound("no records", src7, nil, "")

	// 🔴 ISOLATED from the sweep (LocatePanes removes a record whose process
	// is gone or another before it binds): the binding's OWN start-time check,
	// reached through the resolver directly, refuses a reused pid.
	srv8, src8, dir8 := boundSource(t, titles)
	_ = opencodehost.WriteLaunch(dir8, launchRecord(srv8, "%1", 11, "999", "ses_fixtureA"))
	if ap := ResolveOpenCodePaneStarted(src8, "%1", 11, "100", "OC | Write the docs"); ap.Send || ap.Gen != "ses_fixtureB" {
		t.Fatalf("a reused pid bound through the resolver: %+v", ap)
	}
	if ap := ResolveOpenCodePaneStarted(src8, "%1", 11, "999", "OC | Write the docs"); ap.Send {
		t.Fatalf("instrument check: the stale record was not removed by the first resolve: %+v", ap)
	}
}

// TestTheSweepRemovesRecordsWhoseTUIIsGone: once a minute at most, LocatePanes
// removes each of this daemon's records whose process is gone or is another
// process (its start time differs) — a pane whose TUI exited holds a shell,
// which no runtime claims, so nothing else would — and keeps a live one.
func TestTheSweepRemovesRecordsWhoseTUIIsGone(t *testing.T) {
	srv, src, dir := boundSource(t, map[string]string{"ses_fixtureA": "a"})
	procs, children, root := ocPaneTable(t, "opencode")
	live := launchRecord(srv, "%1", 11, "100", "ses_fixtureA")
	gone := launchRecord(srv, "%2", 4040, "100", "ses_fixtureA") // no such process
	reused := launchRecord(srv, "%3", 10, "999", "ses_fixtureA") // pid 10 is the shell, started at 100
	for _, r := range []opencodehost.LaunchRecord{live, gone, reused} {
		if err := opencodehost.WriteLaunch(dir, r); err != nil {
			t.Fatal(err)
		}
	}
	_ = openCodeRuntime{proc: root, src: src, title: titled("OC | a")}.LocatePanes(ocPane, procs, children)
	for _, c := range []struct {
		r    opencodehost.LaunchRecord
		kept bool
	}{{live, true}, {gone, false}, {reused, false}} {
		_, err := opencodehost.ReadLaunch(dir, ocFixtureSocket, c.r.Pane)
		if (err == nil) != c.kept {
			t.Fatalf("pane %s: read %v, want kept %v", c.r.Pane, err, c.kept)
		}
	}
}

// boundOCBackend lists one opencode window @7 (pane %7, TUI pid 70 started at
// "100") resolved NOW from src, as the tmux backend does on every listing.
type boundOCBackend struct {
	*windowsBackend
	src   *OpenCodeSource
	title string
}

func (b boundOCBackend) OpenCodeSource() *OpenCodeSource { return b.src }
func (b boundOCBackend) Windows(context.Context) ([]WindowInfo, error) {
	ap := ResolveOpenCodePaneStarted(b.src, "%7", 70, "100", b.title)
	return []WindowInfo{{Session: "o", ID: "@7", Index: 0, Active: true, Pane: "%7", PanePID: 69, Command: "opencode", Agent: &ap}}, nil
}

// runSendHost runs a host listing the window of boundOCBackend.
func runSendHost(t *testing.T, src *OpenCodeSource, interactive, send bool) (*drivenMuster, *windowsBackend) {
	t.Helper()
	d := newDrivenMuster(t)
	wb := &windowsBackend{}
	b := boundOCBackend{windowsBackend: wb, src: src, title: "OC | Bound session"}
	c, err := New(Config{URL: d.srv.URL, Token: mirror.TokenPrefix + "t", Host: "laptop", Backend: b, Interactive: interactive,
		OpenCodeSend: send, AllowHTTP: true, Refresh: time.Hour, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = c.RunOnce(ctx) }()
	waitUntil(t, "the hello", func() bool { return len(d.of(mirror.TypeHello)) == 1 })
	return d, wb
}

// sendFixture is a server with the bound session ses_fixtureS ("Bound
// session", idle), its record for pane %7, and the window's status time.
func sendFixture(t *testing.T) (*opencodetest.Server, *OpenCodeSource) {
	t.Helper()
	srv, src, dir := boundSource(t, map[string]string{"ses_fixtureS": "Bound session"})
	if err := opencodehost.WriteLaunch(dir, launchRecord(srv, "%7", 70, "100", "ses_fixtureS")); err != nil {
		t.Fatal(err)
	}
	_ = src.state()
	waitUntil(t, "the event stream", func() bool { s, _ := src.status("x"); return s != "" })
	srv.SetStatus("ses_fixtureS", "idle")
	waitUntil(t, "idle", func() bool { s, at := src.status("ses_fixtureS"); return s == mirror.StatusIdle && at > 0 })
	return srv, src
}

// windowOf is the window @7 the host last listed.
func windowOf(t *testing.T, d *drivenMuster) mirror.Window {
	t.Helper()
	ms := append(d.of(mirror.TypeHello), d.of(mirror.TypeSessions)...)
	for i := len(ms) - 1; i >= 0; i-- {
		for _, w := range ms[i].Windows {
			if w.ID == "@7" {
				return w
			}
		}
	}
	t.Fatal("the host listed no @7")
	return mirror.Window{}
}

func ocPaste(id uint32, text string, since int64, msgID string) mirror.Msg {
	return mirror.Msg{Type: mirror.TypePaste, ID: id, Session: "o", Window: "@7", Text: text, Runtime: mirror.RuntimeOpenCode,
		Since: since, MessageID: msgID}
}

func pastedOf(t *testing.T, d *drivenMuster, id uint32) mirror.Msg {
	t.Helper()
	var out mirror.Msg
	waitUntil(t, "the pasted answer", func() bool {
		for _, m := range d.of(mirror.TypePasted) {
			if m.ID == id {
				out = m
				return true
			}
		}
		return false
	})
	return out
}

// TestTheHostAdvertisesTheSendOnlyWhenEveryGateIsOpen: -interactive AND
// -opencode-send AND a token; the bound window is listed AgentSend.
func TestTheHostAdvertisesTheSendOnlyWhenEveryGateIsOpen(t *testing.T) {
	_, src := sendFixture(t)
	for _, c := range []struct {
		interactive, send, want bool
	}{{true, true, true}, {true, false, false}, {false, true, false}} {
		d, _ := runSendHost(t, src, c.interactive, c.send)
		if got := d.of(mirror.TypeHello)[0].OpenCodeSend; got != c.want {
			t.Fatalf("interactive %v send %v: advertised %v", c.interactive, c.send, got)
		}
		if w := windowOf(t, d); !w.AgentSend || w.AgentSession != "ses_fixtureS" {
			t.Fatalf("the bound window: %+v", w)
		}
	}
	none := NewOpenCodeSource(context.Background(), nil, "no token", log.New(io.Discard, "", 0))
	if d, _ := runSendHost(t, none, true, true); d.of(mirror.TypeHello)[0].OpenCodeSend {
		t.Fatal("a host with no opencode token advertised the send")
	}
}

// TestTheHostPostsAGatedSendOnceAndTypesNothing: a paste naming the runtime,
// the status time the window holds and an id is POSTED to the bound session
// (delivery queue) and answered OK — no key typed; the same id again is
// answered OK and admits nothing more.
func TestTheHostPostsAGatedSendOnceAndTypesNothing(t *testing.T) {
	srv, src := sendFixture(t)
	d, wb := runSendHost(t, src, true, true)
	since := windowOf(t, d).StatusSince
	d.send(t, ocPaste(1, "hello there", since, "msg_fixtureone"))
	if m := pastedOf(t, d, 1); m.Error != "" {
		t.Fatalf("refused: %+v", m)
	}
	d.send(t, ocPaste(2, "hello there", since, "msg_fixtureone"))
	if m := pastedOf(t, d, 2); m.Error != "" {
		t.Fatalf("the repeat refused: %+v", m)
	}
	p := srv.Prompts()
	if len(p) != 1 || p[0].Session != "ses_fixtureS" || p[0].ID != "msg_fixtureone" || p[0].Text != "hello there" || p[0].Delivery != "queue" {
		t.Fatalf("admitted %+v; want one, to the bound session, delivery queue", p)
	}
	wb.mu.Lock()
	typed := len(wb.pasted)
	wb.mu.Unlock()
	if typed != 0 {
		t.Fatalf("keys were typed into a pane: %d", typed)
	}
}

// TestTheHostRefusesAGatedSendItCannotMake: each refusal names its kind and
// posts nothing.
func TestTheHostRefusesAGatedSendItCannotMake(t *testing.T) {
	cases := []struct {
		name, text, refusal, words string
		send                       bool
		since                      func(int64) int64
		prep                       func(*opencodetest.Server, *OpenCodeSource)
	}{
		{"the flag off", "hi", mirror.PasteRefusalOpenCodeOff, mirror.PasteRefusedOpenCodeOffText, false, nil, nil},
		{"a stale status time", "hi", mirror.PasteRefusalOpenCodeNotIdle, mirror.PasteRefusedOpenCodeUnknownText, true, func(s int64) int64 { return s - 1 }, nil},
		{"busy", "hi", mirror.PasteRefusalOpenCodeNotIdle, mirror.PasteRefusedOpenCodeBusyText, true, nil, func(srv *opencodetest.Server, src *OpenCodeSource) {
			srv.SetStatus("ses_fixtureS", "busy")
			waitUntil(t, "busy", func() bool { s, _ := src.status("ses_fixtureS"); return s == mirror.StatusBusy })
		}},
		{"a slash command", "/review the branch", mirror.PasteRefusalOpenCodeSlash, mirror.PasteRefusedOpenCodeSlashText, true, nil, nil},
		// Any leading whitespace, and any whitespace after the command.
		{"a slash command after a space", " /review", mirror.PasteRefusalOpenCodeSlash, mirror.PasteRefusedOpenCodeSlashText, true, nil, nil},
		{"a slash command after a newline and a tab", "\n\t/review now", mirror.PasteRefusalOpenCodeSlash, mirror.PasteRefusedOpenCodeSlashText, true, nil, nil},
		{"a slash command after a no-break space", "\u00a0/review now", mirror.PasteRefusalOpenCodeSlash, mirror.PasteRefusedOpenCodeSlashText, true, nil, nil},
		{"a slash command followed by a tab", "/review\tx", mirror.PasteRefusalOpenCodeSlash, mirror.PasteRefusedOpenCodeSlashText, true, nil, nil},
		{"a slash command while the commands cannot be read", "/review", mirror.PasteRefusalOpenCodeSlash, "", true, nil, func(srv *opencodetest.Server, _ *OpenCodeSource) {
			srv.FailCommands(true)
		}},
		// No record, and the title names TWO sessions: no binding at all.
		{"an unbound pane", "hi", mirror.PasteRefusalOpenCodeUnbound, mirror.PasteRefusedOpenCodeUnboundText, true, nil, func(srv *opencodetest.Server, src *OpenCodeSource) {
			src.SetLaunches("", "")
			srv.PutSession("ses_fixtureTwin", "Bound session", "")
			src.sessionsChanged()
		}},
		// No record, a verified title match — and a paste naming NO session
		// (an older muster's): refused, never posted to the title's session.
		{"a verified pane, a paste naming no session", "hi", mirror.PasteRefusalOpenCodeMoved, mirror.PasteRefusedOpenCodeMovedText, true, nil, func(_ *opencodetest.Server, src *OpenCodeSource) {
			src.SetLaunches("", "")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, src := sendFixture(t)
			d, _ := runSendHost(t, src, true, c.send)
			since := windowOf(t, d).StatusSince
			if c.prep != nil {
				c.prep(srv, src)
			}
			if c.since != nil {
				since = c.since(since)
			}
			d.send(t, ocPaste(1, c.text, since, "msg_fixtureone"))
			if m := pastedOf(t, d, 1); m.Refusal != c.refusal || m.Error == "" || (c.words != "" && m.Error != c.words) {
				t.Fatalf("answered %+v; want refusal %q saying %q", m, c.refusal, c.words)
			}
			if p := srv.Prompts(); len(p) != 0 {
				t.Fatalf("admitted %+v", p)
			}
		})
	}
}

// TestAPathIsNotASlashCommand: text that starts with "/" but names no
// command of the session's (a path) is sent.
func TestAPathIsNotASlashCommand(t *testing.T) {
	srv, src := sendFixture(t)
	d, _ := runSendHost(t, src, true, true)
	d.send(t, ocPaste(1, "/etc/hosts is wrong", windowOf(t, d).StatusSince, "msg_fixtureone"))
	if m := pastedOf(t, d, 1); m.Error != "" {
		t.Fatalf("refused: %+v", m)
	}
	d.send(t, ocPaste(2, "/nosuchcommand please", windowOf(t, d).StatusSince, "msg_fixturetwo"))
	if m := pastedOf(t, d, 2); m.Error != "" {
		t.Fatalf("an unknown slash word was refused: %+v", m)
	}
	if n := len(srv.Prompts()); n != 2 {
		t.Fatalf("admitted %d", n)
	}
	// 🔴 A path is not even LOOKED UP: with the commands unreadable it is
	// still sent (a "/word" then is refused: TestTheHostRefusesAGatedSend…).
	srv.FailCommands(true)
	d.send(t, ocPaste(3, "/srv/fixture/notes.txt says hi", windowOf(t, d).StatusSince, "msg_fixturethree"))
	if m := pastedOf(t, d, 3); m.Error != "" {
		t.Fatalf("a path was refused while the commands cannot be read: %+v", m)
	}
}

// TestAPostWithNoAnswerIsUnconfirmedAndItsRetryLandsOnce: opencode admits the
// message but answers too late: the host says UNCONFIRMED (it may have
// landed), never "not sent" — and the retry under the SAME id is answered OK
// with nothing admitted twice. That is what makes "Send again" safe.
func TestAPostWithNoAnswerIsUnconfirmedAndItsRetryLandsOnce(t *testing.T) {
	was := openCodeSendTimeout
	openCodeSendTimeout = 300 * time.Millisecond
	t.Cleanup(func() { openCodeSendTimeout = was })
	srv, src := sendFixture(t)
	d, _ := runSendHost(t, src, true, true)
	since := windowOf(t, d).StatusSince
	srv.DelayPromptAnswers(3 * time.Second)
	d.send(t, ocPaste(1, "hello", since, "msg_fixtureone"))
	if m := pastedOf(t, d, 1); m.Refusal != mirror.PasteRefusalOpenCodeUnconfirmed || !strings.Contains(m.Error, "did not confirm") {
		t.Fatalf("answered %+v; want unconfirmed", m)
	}
	if p := srv.Prompts(); len(p) != 1 {
		t.Fatalf("instrument check: the delayed post was not admitted (%d)", len(p))
	}
	srv.DelayPromptAnswers(0)
	d.send(t, ocPaste(2, "hello", since, "msg_fixtureone"))
	if m := pastedOf(t, d, 2); m.Error != "" {
		t.Fatalf("the retry: %+v", m)
	}
	if p := srv.Prompts(); len(p) != 1 || srv.PromptPosts() != 2 {
		t.Fatalf("admitted %d of %d posts; want one of two", len(p), srv.PromptPosts())
	}
}

// TestASessionCreatedAfterTheStreamOpenedTakesASendAtOnce: the launcher
// creates its session BEFORE the TUI starts, while this host's stream is
// already open; with no turn yet, the session must still be idle WITH a status
// time — the idle gate refuses a status of unknown age — so a send from the
// phone works before anyone types in the terminal (operator decision).
func TestASessionCreatedAfterTheStreamOpenedTakesASendAtOnce(t *testing.T) {
	srv, src, dir := boundSource(t, map[string]string{"ses_fixtureOld": "an older session"})
	_ = src.state()
	waitUntil(t, "the event stream", func() bool { s, _ := src.status("x"); return s != "" })
	cl, err := opencodehost.NewClient(srv.URL, func() string { return srv.Issue() })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cl.CreateSession(context.Background(), "ses_fixtureS", "/srv/fixture", nil); err != nil {
		t.Fatal(err)
	}
	if err := opencodehost.WriteLaunch(dir, launchRecord(srv, "%7", 70, "100", "ses_fixtureS")); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the new session timed idle", func() bool { st, at := src.status("ses_fixtureS"); return st == mirror.StatusIdle && at > 0 })
	d, _ := runSendHost(t, src, true, true)
	w := windowOf(t, d)
	if !w.AgentSend || w.Status != mirror.StatusIdle || w.StatusSince <= 0 {
		t.Fatalf("the fresh bound session: %+v", w)
	}
	d.send(t, ocPaste(1, "first words, from the phone", w.StatusSince, "msg_fixtureone"))
	if m := pastedOf(t, d, 1); m.Error != "" {
		t.Fatalf("refused: %+v", m)
	}
	if p := srv.Prompts(); len(p) != 1 || p[0].Session != "ses_fixtureS" {
		t.Fatalf("admitted %+v", p)
	}
}

// TestAnIDAnotherSessionHoldsIsAConflictNotAFailure: a send whose message id
// opencode already holds in ANOTHER session (a Send again after the pane was
// started on another session) answers 409: the host names that refusal, so
// muster releases the hold and the next try is a new message.
func TestAnIDAnotherSessionHoldsIsAConflictNotAFailure(t *testing.T) {
	srv, src := sendFixture(t)
	srv.PutSession("ses_fixtureOther", "the earlier session", "")
	cl, err := opencodehost.NewClient(srv.URL, func() string { return srv.Issue() })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Prompt(context.Background(), "ses_fixtureOther", "msg_fixtureone", "hello"); err != nil {
		t.Fatal(err)
	}
	d, _ := runSendHost(t, src, true, true)
	d.send(t, ocPaste(1, "hello", windowOf(t, d).StatusSince, "msg_fixtureone"))
	if m := pastedOf(t, d, 1); m.Refusal != mirror.PasteRefusalOpenCodeConflict || m.Error != mirror.PasteRefusedOpenCodeConflictText {
		t.Fatalf("answered %+v; want the conflict refusal", m)
	}
	if n := len(srv.Prompts()); n != 1 {
		t.Fatalf("admitted %d; want only the other session's", n)
	}
}
