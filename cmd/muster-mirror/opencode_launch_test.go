package main

import (
	"bytes"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/opencodehost"
	"github.com/ZacxDev/muster/internal/opencodehost/opencodetest"
)

// THE LAUNCHER (`muster-mirror opencode`), against a fake opencode server.

type launchRig struct {
	srv     *opencodetest.Server
	homeDir string
	env     map[string]string
	execd   []string
	stderr  bytes.Buffer
	tuiVer  string
}

func newLaunchRig(t *testing.T) *launchRig {
	t.Helper()
	srv := opencodetest.New("fixture-launch-password")
	t.Cleanup(srv.Close)
	home := t.TempDir()
	if err := opencodehost.WriteTokenFile(filepath.Join(home, ".muster", "opencode-token"), opencodehost.TokenFile{URL: srv.URL, Token: srv.Issue()}); err != nil {
		t.Fatal(err)
	}
	return &launchRig{srv: srv, homeDir: home, tuiVer: "opencode v2.0.0-fixture",
		env: map[string]string{"HOME": home, "TMUX": "/srv/fixture/tmux-1000/default,4242,0", "TMUX_PANE": "%12"}}
}

func (r *launchRig) deps() launchDeps {
	return launchDeps{
		getenv:   func(k string) string { return r.env[k] },
		lookPath: func(string) (string, error) { return "/srv/fixture/bin/opencode", nil },
		version:  func(string) (string, error) { return r.tuiVer, nil },
		exec: func(bin string, argv, env []string) error {
			r.execd = append([]string{bin}, argv...)
			return nil
		},
		pid:   func() int { return 4242 },
		start: func(int) (string, bool) { return "777", true },
		now:   func() time.Time { return time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC) },
		cwd:   func() (string, error) { return "/srv/fixture/proj", nil },
	}
}

func (r *launchRig) run(args ...string) int { return runOpenCodeLaunch(args, r.deps(), &r.stderr) }

func (r *launchRig) record(t *testing.T) (opencodehost.LaunchRecord, error) {
	t.Helper()
	return opencodehost.ReadLaunch(opencodehost.LaunchDirDefault(r.homeDir), "/srv/fixture/tmux-1000/default", "%12")
}

// TestTheLauncherCreatesTheSessionRecordsItAndExecsTheTUIOnIt: with no -s it
// creates a session on the shared server under a fresh id, in the working
// directory, writes the record naming its own pid and start time, and execs
// `opencode --session <id>` with the rest of its flags.
func TestTheLauncherCreatesTheSessionRecordsItAndExecsTheTUIOnIt(t *testing.T) {
	r := newLaunchRig(t)
	if rc := r.run("--auto", "--prompt", "start here"); rc != 0 {
		t.Fatalf("rc %d: %s", rc, r.stderr.String())
	}
	rec, err := r.record(t)
	if err != nil {
		t.Fatal(err)
	}
	if !opencodehost.ValidSessionID(rec.Session) || rec.PID != 4242 || rec.PIDStart != "777" || rec.Server != r.srv.URL ||
		rec.Directory != "/srv/fixture/proj" || rec.Pane != "%12" {
		t.Fatalf("record %+v", rec)
	}
	created := false
	for _, q := range r.srv.Requests() {
		created = created || (q.Method == "POST" && q.Path == "/api/session")
	}
	if !created {
		t.Fatal("the session was not created first")
	}
	want := []string{"/srv/fixture/bin/opencode", "opencode", "--session", rec.Session, "--auto", "--prompt", "start here"}
	if !slices.Equal(r.execd, want) {
		t.Fatalf("exec %q, want %q", r.execd, want)
	}
}

// TestTheLauncherBindsAGivenSession: -s <id> of an existing root session is
// bound without creating one; -s of an unknown id creates it (as opencode's
// own -s does); a subagent's session runs unbound.
func TestTheLauncherBindsAGivenSession(t *testing.T) {
	r := newLaunchRig(t)
	r.srv.PutSession("ses_fixtureA", "a", "")
	if rc := r.run("-s", "ses_fixtureA", "/srv/fixture/other"); rc != 0 {
		t.Fatalf("rc %d: %s", rc, r.stderr.String())
	}
	if rec, err := r.record(t); err != nil || rec.Session != "ses_fixtureA" || rec.Directory != "/srv/fixture/other" {
		t.Fatalf("record %+v, %v", rec, err)
	}
	for _, q := range r.srv.Requests() {
		if q.Method == "POST" && q.Path == "/api/session" {
			t.Fatal("an existing session was created again")
		}
	}
	if want := []string{"/srv/fixture/bin/opencode", "opencode", "--session", "ses_fixtureA", "/srv/fixture/other"}; !slices.Equal(r.execd, want) {
		t.Fatalf("exec %q", r.execd)
	}

	r = newLaunchRig(t)
	if rc := r.run("--session=ses_fixtureNew"); rc != 0 {
		t.Fatalf("rc %d: %s", rc, r.stderr.String())
	}
	if rec, err := r.record(t); err != nil || rec.Session != "ses_fixtureNew" {
		t.Fatalf("an unknown -s: %+v, %v", rec, err)
	}

	r = newLaunchRig(t)
	r.srv.PutSession("ses_fixtureP", "parent", "")
	r.srv.PutSession("ses_fixtureC", "child", "ses_fixtureP")
	if rc := r.run("-s", "ses_fixtureC"); rc != 0 || !strings.Contains(r.stderr.String(), "UNBOUND") {
		t.Fatalf("a child session: rc %d %s", rc, r.stderr.String())
	}
	if _, err := r.record(t); !errors.Is(err, opencodehost.ErrNoLaunch) {
		t.Fatalf("a child session was recorded: %v", err)
	}
}

// TestTheLauncherRefusesAVersionMismatchNamingBothAndTheFix: a TUI whose
// version differs from the running service is NOT started (operator
// decision): no exec, no session created, no record — and the message names
// both versions and `opencode service restart`. A matching version that
// spells the "opencode v" prefix is a match (both spellings measured).
func TestTheLauncherRefusesAVersionMismatchNamingBothAndTheFix(t *testing.T) {
	r := newLaunchRig(t)
	r.srv.SetVersion("2.0.33+abc1234")
	r.tuiVer = "opencode v2.0.32+6ec5682"
	if rc := r.run(); rc != 1 {
		t.Fatalf("rc %d, want 1: %s", rc, r.stderr.String())
	}
	msg := r.stderr.String()
	if !strings.Contains(msg, "2.0.32+6ec5682") || !strings.Contains(msg, "2.0.33+abc1234") || !strings.Contains(msg, "opencode service restart") {
		t.Fatalf("the refusal %q does not name both versions and the fix", msg)
	}
	if r.execd != nil {
		t.Fatalf("a mismatched TUI was started: %q", r.execd)
	}
	for _, q := range r.srv.Requests() {
		if q.Method == "POST" {
			t.Fatalf("a mismatched launch wrote to the server: %+v", q)
		}
	}
	if _, err := r.record(t); !errors.Is(err, opencodehost.ErrNoLaunch) {
		t.Fatalf("a mismatched launch was recorded: %v", err)
	}

	r = newLaunchRig(t)
	r.srv.SetVersion("2.0.32+6ec5682")
	r.tuiVer = "opencode v2.0.32+6ec5682"
	if rc := r.run(); rc != 0 || r.execd == nil {
		t.Fatalf("a matching version: rc %d %s", rc, r.stderr.String())
	}
}

// TestTheLauncherStartsUnboundWhatItCannotBind: outside tmux, with
// --standalone, --server or -c, with no token: the TUI starts as asked, no
// record, and stderr says why.
func TestTheLauncherStartsUnboundWhatItCannotBind(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		prep func(*launchRig)
		want []string
	}{
		{"outside tmux", nil, func(r *launchRig) { delete(r.env, "TMUX") }, []string{"opencode"}},
		{"standalone", []string{"--standalone"}, nil, []string{"opencode", "--standalone"}},
		{"another server", []string{"--server", "http://127.0.0.1:9"}, nil, []string{"opencode", "--server", "http://127.0.0.1:9"}},
		{"continue", []string{"-c"}, nil, []string{"opencode", "--continue"}},
		{"no token", nil, func(r *launchRig) { r.env["HOME"] = filepathJoinTemp(r) }, []string{"opencode"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newLaunchRig(t)
			if c.prep != nil {
				c.prep(r)
			}
			if rc := r.run(c.args...); rc != 0 || !strings.Contains(r.stderr.String(), "UNBOUND") {
				t.Fatalf("rc %d: %s", rc, r.stderr.String())
			}
			if want := append([]string{"/srv/fixture/bin/opencode"}, c.want...); !slices.Equal(r.execd, want) {
				t.Fatalf("exec %q, want %q", r.execd, want)
			}
			if _, err := opencodehost.ReadLaunch(opencodehost.LaunchDirDefault(r.homeDir), "/srv/fixture/tmux-1000/default", "%12"); !errors.Is(err, opencodehost.ErrNoLaunch) {
				t.Fatalf("recorded: %v", err)
			}
		})
	}
}

// filepathJoinTemp is a HOME with no token file.
func filepathJoinTemp(r *launchRig) string { return filepath.Join(r.homeDir, "elsewhere") }

// TestTheLauncherRefusesWhatItDoesNotKnow: an unknown flag, a subcommand and
// a second directory are refused (rc 2), never passed on blind.
func TestTheLauncherRefusesWhatItDoesNotKnow(t *testing.T) {
	for _, args := range [][]string{{"--yolo"}, {"run", "hello"}, {"/a", "/b"}, {"-s"}} {
		r := newLaunchRig(t)
		if rc := r.run(args...); rc != 2 || r.execd != nil {
			t.Fatalf("%q: rc %d exec %q", args, rc, r.execd)
		}
	}
}
