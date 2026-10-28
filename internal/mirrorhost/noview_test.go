package mirrorhost

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// environ writes pid's /proc/<pid>/environ in the fake tree: NUL-separated
// KEY=VALUE pairs, as the kernel exposes the environment a process was exec'd
// with.
func (f *fakeProc) environ(pid int, kv ...string) {
	f.t.Helper()
	var b []byte
	for _, s := range kv {
		b = append(b, s...)
		b = append(b, 0)
	}
	must(f.t, os.WriteFile(filepath.Join(f.root, strconv.Itoa(pid), "environ"), b, 0o644))
}

// A CLI started with its own CLAUDE_CONFIG_DIR — exported in the person's
// shell profile, which muster-mirror's service never reads — writes its
// sessions file and transcript THERE. The locator must read the process's own
// value, not muster-mirror's.
//
// The default home holds an OLD transcript for the same cwd (what is left in
// ~/.claude after a person moves their config): before the fix the locator
// found no sessions file in the default home, fell back to the newest
// transcript in the cwd there, and showed that OLD conversation under the
// live pane. Red at origin/main by assertion (it resolved the old one).
func TestLocateReadsTheCLIsOwnClaudeConfigDir(t *testing.T) {
	proc, home, custom := newFakeProc(t), newFakeHome(t), newFakeHome(t)
	const cwd = "/work/repo"
	proc.add(100, 1, "zsh", cwd, "-zsh")
	proc.add(400, 100, "claude", cwd, "claude")
	proc.environ(400, "HOME=/home/someone", "CLAUDE_CONFIG_DIR="+custom.root, "TERM=tmux-256color")

	old := home.transcript(cwd, "sess-old", time.Now())
	custom.session(400, "sess-live", cwd, "idle")
	live := custom.transcript(cwd, "sess-live", time.Now().Add(-time.Hour))

	got := ClaudeLocator{Proc: proc.root, Home: home.root}.LocatePanes([]pane{{Session: "work", ID: "%1", PID: 100}})
	c, ok := got["%1"]
	if !ok {
		t.Fatalf("pane %%1 not located (got %+v); want its CLI's own config dir's sess-live", got)
	}
	if c.Transcript != live || c.Gen != "sess-live" || c.Source != SourceSessions || c.Status != mirror.StatusIdle {
		t.Fatalf("pane %%1 = %+v, want %s (sess-live, from the CLI's own sessions file); the default home's %s is another conversation", c, live, old)
	}
	if c.ConfigDir != custom.root {
		t.Fatalf("ConfigDir = %q, want the CLI's own %q (what its shell snapshots are read under)", c.ConfigDir, custom.root)
	}
}

// Only an absolute CLAUDE_CONFIG_DIR is believed: a relative or "~" value is
// one the CLI resolved against ITS cwd or shell, which this side cannot
// reproduce, so the locator keeps its own default. An invariant guard, not a
// regression test: origin/main never read the process's value at all.
func TestLocateIgnoresARelativeClaudeConfigDir(t *testing.T) {
	for _, v := range []string{"relative/dir", "~/.claude-work", ""} {
		proc, home := newFakeProc(t), newFakeHome(t)
		const cwd = "/work/repo"
		proc.add(100, 1, "zsh", cwd, "-zsh")
		proc.add(400, 100, "claude", cwd, "claude")
		proc.environ(400, "CLAUDE_CONFIG_DIR="+v)
		home.session(400, "sess-a", cwd, "idle")
		want := home.transcript(cwd, "sess-a", time.Now())
		got := ClaudeLocator{Proc: proc.root, Home: home.root}.LocatePanes([]pane{{Session: "work", ID: "%1", PID: 100}})
		if c := got["%1"]; c.Transcript != want || c.ConfigDir != home.root {
			t.Errorf("CLAUDE_CONFIG_DIR=%q: pane = %+v, want the default home's %s", v, c, want)
		}
	}
}

// whyNoView names WHICH step left a Claude Code pane without a rendered view.
// Each case is built so that only its own branch can produce its line, and a
// control pane with no Claude Code must say nothing. "lost" and "shared" say
// the same line ON PURPOSE: an ambiguous guess is folded into "cannot match it
// to a transcript" (TestWhyNoViewNeverClaimsNeitherPaneNamesItsConversation). Red at origin/main: whyNoView did not exist, so a
// terminal-only Claude Code window said nothing at all.
func TestWhyNoViewSaysWhichStepFailed(t *testing.T) {
	proc, home := newFakeProc(t), newFakeHome(t)
	const fresh, lost, shared, shell, ok = "/work/fresh", "/work/lost", "/work/shared", "/work/shell", "/work/ok"
	// A CLI that named its session but has written no transcript yet.
	proc.add(100, 1, "zsh", fresh, "-zsh")
	proc.add(101, 100, "claude", fresh, "claude")
	home.session(101, "sess-fresh", fresh, "idle")
	// A CLI with no sessions file and nothing in its cwd's project dir.
	proc.add(200, 1, "zsh", lost, "-zsh")
	proc.add(201, 200, ".claude-wrapped", lost, "/nix/store/x-claude-code/bin/claude")
	// Two CLIs in one cwd, neither with a sessions file: one guess for both.
	proc.add(300, 1, "zsh", shared, "-zsh")
	proc.add(301, 300, "node", shared, "node", "/usr/lib/node_modules/@anthropic-ai/claude-code/cli.js")
	proc.add(310, 1, "zsh", shared, "-zsh")
	proc.add(311, 310, "claude", shared, "claude")
	home.transcript(shared, "sess-shared", time.Now())
	// A plain shell: nothing to explain.
	proc.add(400, 1, "zsh", shell, "-zsh")
	// A CLI that resolves: it has a view, so whyNoView must not invent a reason.
	proc.add(500, 1, "zsh", ok, "-zsh")
	proc.add(501, 500, "claude", ok, "claude")
	home.session(501, "sess-ok", ok, "idle")
	home.transcript(ok, "sess-ok", time.Now())

	l := ClaudeLocator{Proc: proc.root, Home: home.root}
	procs, children, err := readProcTable(proc.root)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		p    pane
		cmd  string
		want string
	}{
		{"fresh", pane{ID: "%1", PID: 100}, "claude", "Claude Code has not written this conversation yet. The Rendered view appears after its first message."},
		{"lost", pane{ID: "%2", PID: 200}, ".claude-wrapped", "Claude Code is running here, but muster-mirror cannot match it to a transcript in its Claude config directory."},
		{"shared", pane{ID: "%3", PID: 300}, "node", "Claude Code is running here, but muster-mirror cannot match it to a transcript in its Claude config directory."},
		{"shell", pane{ID: "%4", PID: 400}, "zsh", ""},
		{"resolves", pane{ID: "%5", PID: 500}, "claude", ""},
	} {
		if got := l.whyNoView(c.p, c.cmd, procs, children, nil); got != c.want {
			t.Errorf("%s: whyNoView = %q, want %q", c.name, got, c.want)
		}
	}

	// Positive control for the "shared" case: the locator itself really drops
	// both guesses (else that line would describe a pane that has a view).
	got := l.LocatePanes([]pane{{ID: "%3", PID: 300}, {ID: "%3b", PID: 310}})
	if len(got) != 0 {
		t.Fatalf("control: the two shared-cwd guesses were located %+v, want both dropped", got)
	}
	if one := l.LocatePanes([]pane{{ID: "%3", PID: 300}}); len(one) != 1 {
		t.Fatalf("control: one guess alone should resolve (got %+v)", one)
	}
}

// On a host whose process table cannot be read (an OS with neither /proc nor
// the macOS source, or a failed read), nothing can be found in any pane; a
// window whose command looks like Claude Code says so, and no other window
// does.
func TestWhyNoViewOnAHostWithNoProcessTable(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-proc-here")
	procs, children, err := readProcTable(missing)
	if err == nil {
		t.Fatal("instrument check: reading a missing proc root must fail")
	}
	l := ClaudeLocator{Proc: missing, Home: t.TempDir()}
	const want = "muster-mirror cannot read this host's process table, so it cannot find the Claude Code conversation in this window."
	for cmd, w := range map[string]string{
		"claude":          want,
		".claude-wrapped": want,
		"2.1.289":         "", // a version-named binary: unmeasured on macOS, so not claimed
		"zsh":             "",
		"vim":             "",
		"2.1":             "",
	} {
		if got := l.whyNoView(pane{ID: "%1", PID: 100}, cmd, procs, children, err); got != w {
			t.Errorf("command %q: whyNoView = %q, want %q", cmd, got, w)
		}
	}
}

// WireWindows sends a terminal-only Claude Code window's reason as its
// AgentNote, and NormalizeWindows keeps it (it used to drop every note on a
// window of no wire runtime). A Claude window never carries one.
func TestWireWindowsCarriesWhyAClaudePaneHasNoView(t *testing.T) {
	const why = "Claude Code has not written this conversation yet. The Rendered view appears after its first message."
	now := time.Unix(1_800_000_000, 0)
	got := WireWindows([]WindowInfo{
		{Session: "work", ID: "@1", Index: 1, Pane: "%1", Command: "claude", NoView: why},
		{Session: "work", ID: "@2", Index: 2, Pane: "%2", Command: "claude",
			Claude: &ClaudePane{Pane: "%2", Transcript: "/x/sess-b.jsonl", Gen: "sess-b"}},
		{Session: "work", ID: "@3", Index: 3, Pane: "%3", Command: "zsh"},
	}, nil, true, now)
	if len(got) != 3 {
		t.Fatalf("windows = %+v", got)
	}
	if got[0].Claude || got[0].AgentNote != why {
		t.Errorf("terminal-only Claude window = %+v, want no Claude flag and note %q", got[0], why)
	}
	if !got[1].Claude || got[1].AgentNote != "" {
		t.Errorf("Claude window = %+v, want Claude and no note", got[1])
	}
	if got[2].AgentNote != "" {
		t.Errorf("shell window = %+v, want no note", got[2])
	}
	// A tunnel that carries no rendered view carries no claim about one.
	if v1 := WireWindows([]WindowInfo{{Session: "work", ID: "@1", Index: 1, Pane: "%1", NoView: why}}, nil, false, now); v1[0].AgentNote != "" {
		t.Errorf("v1 tunnel window = %+v, want no note", v1[0])
	}
}

// THE SEAM, through the real backend's listing (a fake tmux script and a fake
// /proc): Tmux.Windows asks whyNoView for each window no runtime claimed, and
// WireWindows puts the answer on the wire. Pinned as what muster RECEIVES
// (mirror.Window), so a listing that stops asking, or a wire that drops the
// note, fails here — the unit tests above cannot see either.
//
// Window @1: Claude Code just started (sessions file, no transcript yet).
// Window @2: Claude Code whose CLAUDE_CONFIG_DIR is its own, not muster-mirror's.
// Window @3: a shell. Red at origin/main by assertion: @1 carried no note, and
// @2 had no rendered view (its sessions file was looked for in the wrong dir).
func TestTmuxListingSaysWhyAClaudeWindowHasNoView(t *testing.T) {
	proc, home, custom := newFakeProc(t), newFakeHome(t), newFakeHome(t)
	proc.add(100, 1, "zsh", "/work/fresh", "-zsh")
	proc.add(101, 100, "claude", "/work/fresh", "claude")
	home.session(101, "sess-fresh", "/work/fresh", "idle")
	proc.add(200, 1, "zsh", "/work/custom", "-zsh")
	proc.add(201, 200, "claude", "/work/custom", "claude")
	proc.environ(201, "CLAUDE_CONFIG_DIR="+custom.root)
	custom.session(201, "sess-custom", "/work/custom", "idle")
	custom.transcript("/work/custom", "sess-custom", time.Now())
	proc.add(300, 1, "zsh", "/work/shell", "-zsh")

	bin := filepath.Join(t.TempDir(), "tmux")
	out := "1\t@1\t1\t1\t%1\t100\t1791903000\t1791903000\tclaude\t/dev/pts/1\t\t\twork\n" +
		"1\t@2\t2\t0\t%2\t200\t1791903000\t1791903000\tclaude\t/dev/pts/2\t\t\twork\n" +
		"1\t@3\t3\t0\t%3\t300\t1791903000\t1791903000\tzsh\t/dev/pts/3\t\t\twork\n"
	listing := filepath.Join(t.TempDir(), "listing")
	must(t, os.WriteFile(listing, []byte(out), 0o644))
	must(t, os.WriteFile(bin, []byte("#!/bin/sh\ncat '"+listing+"'\n"), 0o755))
	ws, err := Tmux{Bin: bin, Claude: ClaudeLocator{Proc: proc.root, Home: home.root}}.Windows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 3 {
		t.Fatalf("instrument check: want three windows, got %+v", ws)
	}
	wire := WireWindows(ws, nil, true, time.Unix(1_800_000_000, 0))
	by := map[string]mirror.Window{}
	for _, w := range wire {
		by[w.ID] = w
	}
	if w := by["@1"]; w.Claude || w.AgentNote != "Claude Code has not written this conversation yet. The Rendered view appears after its first message." {
		t.Errorf("window @1 (fresh Claude Code) = %+v, want no view and the not-written-yet note", w)
	}
	if w := by["@2"]; !w.Claude || w.ClaudeSession != "sess-custom" || w.AgentNote != "" {
		t.Errorf("window @2 (own CLAUDE_CONFIG_DIR) = %+v, want its rendered view of sess-custom", w)
	}
	if w := by["@3"]; w.Claude || w.AgentNote != "" {
		t.Errorf("window @3 (shell) = %+v, want no view and no note", w)
	}
}

// THE AUDITOR'S PROBE (round 1, F3): two Claude Code panes in one directory,
// %1 WITH a sessions file naming its conversation, %2 with none. %2's guess is
// %1's transcript, so the locator drops it — and the line used to say
// "neither names its conversation", which is false: %1 does. The line must be
// true here. Red before the fold (it said the ambiguous line).
func TestWhyNoViewNeverClaimsNeitherPaneNamesItsConversation(t *testing.T) {
	proc, home := newFakeProc(t), newFakeHome(t)
	const cwd = "/work/same"
	proc.add(100, 1, "zsh", cwd, "-zsh")
	proc.add(101, 100, "claude", cwd, "claude")
	home.session(101, "sess-named", cwd, "idle")
	home.transcript(cwd, "sess-named", time.Now())
	proc.add(200, 1, "zsh", cwd, "-zsh")
	proc.add(201, 200, "claude", cwd, "claude") // no sessions file
	l := ClaudeLocator{Proc: proc.root, Home: home.root}
	got := l.LocatePanes([]pane{{ID: "%1", PID: 100}, {ID: "%2", PID: 200}})
	if _, ok := got["%1"]; !ok || len(got) != 1 {
		t.Fatalf("control: want %%1 resolved by its sessions file and %%2's shared guess dropped, got %+v", got)
	}
	procs, children, err := readProcTable(proc.root)
	if err != nil {
		t.Fatal(err)
	}
	const want = "Claude Code is running here, but muster-mirror cannot match it to a transcript in its Claude config directory."
	if why := l.whyNoView(pane{ID: "%2", PID: 200}, "claude", procs, children, nil); why != want {
		t.Fatalf("whyNoView(%%2) = %q, want %q", why, want)
	}
}

// F7: a CLI whose environment sets NO CLAUDE_CONFIG_DIR uses its own home's
// .claude — whatever muster-mirror's service environment says. Before, the
// locator fell back to the SERVICE's $CLAUDE_CONFIG_DIR, and here found an
// OLD transcript there for the same cwd. Red before the fix (resolved
// sess-service-old from the service's dir).
func TestAProcessWithNoClaudeConfigDirUsesItsOwnHome(t *testing.T) {
	proc, service, home := newFakeProc(t), newFakeHome(t), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", service.root)
	const cwd = "/work/repo"
	service.transcript(cwd, "sess-service-old", time.Now())
	own := &fakeHome{t: t, root: filepath.Join(home, ".claude")}
	own.session(400, "sess-own", cwd, "idle")
	want := own.transcript(cwd, "sess-own", time.Now().Add(-time.Hour))
	proc.add(100, 1, "zsh", cwd, "-zsh")
	proc.add(400, 100, "claude", cwd, "claude")
	proc.environ(400, "HOME="+home, "TERM=tmux-256color")
	got := ClaudeLocator{Proc: proc.root}.LocatePanes([]pane{{Session: "work", ID: "%1", PID: 100}})
	if c := got["%1"]; c.Transcript != want || c.Gen != "sess-own" {
		t.Fatalf("pane %%1 = %+v, want its own home's %s", c, want)
	}
}

// F4: resume's liveness check (check 4) sees a session that runs under the
// CLI's OWN CLAUDE_CONFIG_DIR, outside any tmux pane — the case only the
// process-table half can see. Before, it read sessions files from
// muster-mirror's dir only, so this session read as ENDED and a resume would
// start a second CLI on one conversation. Red before the fix.
func TestResumeSeesALiveSessionUnderTheCLIsOwnConfigDir(t *testing.T) {
	proc, home, custom := newFakeProc(t), newFakeHome(t), newFakeHome(t)
	const sid = "0b5e55ed-0000-4000-8000-00000000c0de"
	proc.add(400, 1, "claude", "/work/repo", "claude")
	proc.environ(400, "CLAUDE_CONFIG_DIR="+custom.root)
	custom.session(400, sid, "/work/repo", "idle")
	loc := ClaudeLocator{Proc: proc.root, Home: home.root}
	live, err := resumeLive(context.Background(), loc, noWindowLister{}, sid)
	if err != nil || !live {
		t.Fatalf("resumeLive = %v, %v; want live (its CLI's own sessions file names it)", live, err)
	}
	// Control: another id is not live.
	if live, err := resumeLive(context.Background(), loc, noWindowLister{}, "0b5e55ed-0000-4000-8000-00000000beef"); live || err != nil {
		t.Fatalf("control: resumeLive(other id) = %v, %v; want false, nil", live, err)
	}
}
