package mirrorhost

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/askdrive"
	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/mirror"
)

// Every fixture here is synthetic: invented paths, pids and profile names.

// kernelChild lists child under parent's main thread in the fixture's
// /proc/<parent>/task/<parent>/children, as the kernel does.
func (f *fakeProc) kernelChild(parent, child int) {
	f.t.Helper()
	dir := filepath.Join(f.root, fmt.Sprint(parent), "task", fmt.Sprint(parent))
	must(f.t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "children")
	b, _ := os.ReadFile(path)
	must(f.t, os.WriteFile(path, append(b, []byte(fmt.Sprintf("%d ", child))...), 0o644))
}

// unreadable is a process whose /proc directory exists but whose stat cannot
// be read (here: is missing), listed as parent's child by the kernel only.
func (f *fakeProc) unreadable(pid, parent int) {
	f.t.Helper()
	must(f.t, os.MkdirAll(filepath.Join(f.root, fmt.Sprint(pid)), 0o755))
	f.kernelChild(parent, pid)
}

// stat rewrites pid's stat with another state ("Z" for a zombie).
func (f *fakeProc) state(pid, ppid int, comm, state string) {
	f.t.Helper()
	stat := fmt.Sprintf("%d (%s) %s %d 1 1 0 -1 4194560 0 0 0 0 0 0 0 0 20 0 1 0 100 0 0", pid, comm, state, ppid)
	must(f.t, os.WriteFile(filepath.Join(f.root, fmt.Sprint(pid), "stat"), []byte(stat), 0o644))
}

// guardCase is one pane: a root shell at root, and below it the processes
// `procs` adds. want is the pid the guard must name (0: none), wantRule its
// rule; wantErr: the guard cannot check, and so refuses.
type guardCase struct {
	name     string
	procs    func(f *fakeProc, root int)
	want     int
	wantRule string
	wantErr  string
}

// TestThePaneGuardRefusesMultiplexersAndTheUnreadableAndNothingElse is the
// guard's walk over a fixture /proc (its opencode half is
// TestAnOpenCodeOnlyRefusesAPaneWhoseTerminalItIsOn). Each pane is a shell
// with the process under test as its CHILD (or deeper), so the walk below
// #{pane_pid} is exercised, not just the root. The pid each case must name is
// distinct from the root's and from every other case's, so a walk that
// answered "the root" or "some process" cannot pass.
func TestThePaneGuardRefusesMultiplexersAndTheUnreadableAndNothingElse(t *testing.T) {
	cases := []guardCase{
		// 🔴 A TERMINAL MULTIPLEXER CLIENT: it can show an agent this walk
		// cannot see (`TMUX= tmux -L other attach`). Refused, accepted as safe.
		{name: "a nested tmux client (comm as tmux sets it)", want: 1171, wantRule: ruleMultiplexer, procs: func(f *fakeProc, root int) {
			f.add(1171, root, "tmux: client", "/work/m", "tmux", "-L", "other", "attach")
		}},
		{name: "GNU screen", want: 1181, wantRule: ruleMultiplexer, procs: func(f *fakeProc, root int) {
			f.add(1181, root, "screen", "/work/n", "screen", "-r")
		}},
		{name: "dtach", want: 1191, wantRule: ruleMultiplexer, procs: func(f *fakeProc, root int) {
			f.add(1191, root, "dtach", "/work/o", "/usr/bin/dtach", "-a", "/tmp/s")
		}},
		{name: "abduco", want: 1201, wantRule: ruleMultiplexer, procs: func(f *fakeProc, root int) {
			f.add(1201, root, "abduco", "/work/p", "abduco", "-a", "s")
		}},
		{name: "zellij, below a shell", want: 1212, wantRule: ruleMultiplexer, procs: func(f *fakeProc, root int) {
			f.add(1211, root, "bash", "/work/q", "bash")
			f.add(1212, 1211, "zellij", "/work/q", "/opt/zellij/bin/zellij", "attach")
		}},

		// 🔴 COULD NOT CHECK: refused, never presumed safe.
		{name: "a child whose stat cannot be read", wantErr: "could not be read", procs: func(f *fakeProc, root int) {
			f.unreadable(1221, root)
		}},
		{name: "a child whose cmdline cannot be read", wantErr: "command line", procs: func(f *fakeProc, root int) {
			f.add(1231, root, "sleep", "/work/r", "sleep", "60")
			must(t, os.Remove(filepath.Join(f.root, "1231", "cmdline")))
		}},

		// 🔴 MUST NOT BE REFUSED.
		{name: "a plain shell", want: 0, procs: func(f *fakeProc, root int) {}},
		{name: "a shell running vim", want: 0, procs: func(f *fakeProc, root int) {
			f.add(1301, root, "vim", "/work/s", "vim", "notes.md")
		}},
		{name: "a Claude Code pane", want: 0, procs: func(f *fakeProc, root int) {
			f.add(1311, root, "claude", "/work/t", "claude", "--continue")
			f.add(1312, 1311, "node", "/work/t", "node", "/usr/lib/node_modules/@anthropic-ai/claude-code/cli.js")
		}},
		{name: "a NixOS-wrapped Claude Code", want: 0, procs: func(f *fakeProc, root int) {
			f.add(1321, root, ".claude-wrapped", "/work/u", "/nix/store/0000000000000000000000000000000c-claude-code/bin/.claude-wrapped")
		}},
		{name: "Python programs", want: 0, procs: func(f *fakeProc, root int) {
			f.add(1331, root, "python3", "/work/v", "python3", "-m", "http.server", "8000")
			f.add(1332, root, "python3", "/work/v", "python3", "manage.py", "migrate")
			f.add(1333, root, "python3", "/work/v", "python3", "cli.py")
		}},
		{name: "a zombie child (no command line, reads no input)", want: 0, procs: func(f *fakeProc, root int) {
			f.add(1351, root, "sleep", "/work/x")
			f.state(1351, root, "sleep", "Z")
		}},
		{name: "a child the kernel listed that has exited since", want: 0, procs: func(f *fakeProc, root int) {
			f.kernelChild(root, 1361)
		}},
	}

	proc := newFakeProc(t)
	for i, c := range cases {
		root := 100 + i // each pane's shell
		proc.add(root, 1, "zsh", "/work", "-zsh")
		c.procs(proc, root)
	}
	id := PaneProcs{Proc: proc.root}
	for i, c := range cases {
		root := 100 + i
		got, found, err := id.InTree(root)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%s: InTree = %+v, found %v, err %v; want an error naming %q (fail closed)", c.name, got, found, err, c.wantErr)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: InTree: %v", c.name, err)
		}
		if c.want == 0 {
			if found {
				t.Errorf("%s: REFUSED (%+v); it is neither opencode nor a multiplexer", c.name, got)
			}
			continue
		}
		if !found || got.PID != c.want || got.Rule != c.wantRule {
			t.Errorf("%s: InTree = %+v, found %v; want pid %d by %q", c.name, got, found, c.want, c.wantRule)
		}
	}
}

// TestAChildForkedAfterTheTableWasReadIsWalkedNotRefused is the regression
// behind the guard's spurious "could not check" refusals:
// a busy Claude Code pane forks a tool call's process between the guard's
// read of the process table and its walk, the kernel lists it as a child,
// and the walk — finding it absent from the table — refused the paste as
// unreadable. Now it reads that child on its own: a readable one is walked
// like any other (and an opencode or a multiplexer there is still refused),
// one gone since is skipped, and only one that STILL cannot be read refuses.
//
// The fork is made deterministic by wrapping the table read (paneProcTable):
// the child's /proc entry is written AFTER the table was read, exactly the
// window the race opens.
func TestAChildForkedAfterTheTableWasReadIsWalkedNotRefused(t *testing.T) {
	for _, c := range []struct {
		name     string
		fork     func(f *fakeProc)
		want     int
		wantRule string
		wantErr  string
	}{
		{name: "a tool call's process (the operator's case)", want: 0, fork: func(f *fakeProc) {
			f.add(21, 11, "bash", "/work", "/bin/bash", "-c", "go test ./...")
		}},
		{name: "a multiplexer forked late is still refused", want: 21, wantRule: ruleMultiplexer, fork: func(f *fakeProc) {
			f.add(21, 11, "tmux: client", "/work", "tmux", "-L", "other", "attach")
		}},
		{name: "a child forked late that still cannot be read refuses", wantErr: "could not be read", fork: func(f *fakeProc) {
			must(f.t, os.MkdirAll(filepath.Join(f.root, "21"), 0o755))
		}},
		{name: "a child forked late and gone again is skipped", want: 0, fork: func(f *fakeProc) {}},
	} {
		t.Run(c.name, func(t *testing.T) {
			proc := newFakeProc(t)
			proc.add(10, 1, "zsh", "/work", "-zsh")
			proc.add(11, 10, "claude", "/work", "claude")
			proc.kernelChild(11, 21)
			defer func(f func(string) (map[int]procInfo, map[int][]int, error)) { paneProcTable = f }(paneProcTable)
			read := 0
			paneProcTable = func(root string) (map[int]procInfo, map[int][]int, error) {
				procs, children, err := readProcTable(root)
				read++
				c.fork(proc)
				return procs, children, err
			}
			got, found, err := PaneProcs{Proc: proc.root}.InTree(10)
			if read != 1 {
				t.Fatalf("instrument check: the table was read %d times, want 1", read)
			}
			switch {
			case c.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("InTree = %+v, found %v, err %v; want an error naming %q", got, found, err, c.wantErr)
				}
			case err != nil:
				t.Fatalf("InTree: %v — a child forked after the table was read was refused as unreadable", err)
			case c.want == 0 && found:
				t.Fatalf("REFUSED %+v; want admitted", got)
			case c.want != 0 && (!found || got.PID != c.want || got.Rule != c.wantRule):
				t.Fatalf("InTree = %+v, found %v; want pid %d by %q", got, found, c.want, c.wantRule)
			}
		})
	}
}

// TestAnEmptyCommandLineIsReadOnceMore: a process in the pane whose command
// line reads empty — mid-exec, or exiting — is read once more before the
// guard decides. Gone by then, its pid reused by another process, or a
// zombie, it is skipped and the paste goes; with a command line by then it is
// walked like any other (a multiplexer there is still refused); still alive
// with none, or unreadable while its /proc entry is still there, the guard
// refuses. The
// change between the two reads is made by wrapping the second one
// (paneProcEntry), so the walk sees exactly that window.
func TestAnEmptyCommandLineIsReadOnceMore(t *testing.T) {
	cmdline := func(f *fakeProc, pid int, argv ...string) {
		must(f.t, os.WriteFile(filepath.Join(f.root, fmt.Sprint(pid), "cmdline"), []byte(strings.Join(argv, "\x00")), 0o644))
	}
	for _, c := range []struct {
		name     string
		then     func(f *fakeProc)
		want     int
		wantRule string
		wantErr  string
	}{
		{name: "gone by the second read", want: 0, then: func(f *fakeProc) {
			must(f.t, os.RemoveAll(filepath.Join(f.root, "21")))
		}},
		{name: "a zombie by the second read", want: 0, then: func(f *fakeProc) { f.state(21, 11, "bash", "Z") }},
		{name: "exec'd by the second read: walked like any other", want: 0, then: func(f *fakeProc) {
			cmdline(f, 21, "/bin/bash", "-c", "go test ./...")
		}},
		{name: "exec'd into a multiplexer by the second read: still refused", want: 21, wantRule: ruleMultiplexer, then: func(f *fakeProc) {
			f.state(21, 11, "tmux: client", "S")
			cmdline(f, 21, "tmux", "-L", "other", "attach")
		}},
		{name: "still alive with an empty command line: refused", wantErr: "command line", then: func(*fakeProc) {}},
		{name: "unreadable on the second read while still there: refused", wantErr: "could not be read again", then: func(f *fakeProc) {
			must(f.t, os.WriteFile(filepath.Join(f.root, "21", "stat"), []byte("not a stat line"), 0o644))
		}},
		// The pid reused between the reads: a new process (another start
		// time) — here a multiplexer client, which the walk would refuse were
		// it taken for the process first read. That one is gone: skipped.
		{name: "its pid reused by another process by the second read: skipped", want: 0, then: func(f *fakeProc) {
			must(f.t, os.RemoveAll(filepath.Join(f.root, "21")))
			f.addStarted(21, 11, "tmux: client", "/work", "999", "tmux", "-L", "other", "attach")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			proc := newFakeProc(t)
			proc.add(10, 1, "zsh", "/work", "-zsh")
			proc.add(11, 10, "claude", "/work", "claude")
			proc.add(21, 11, "bash", "/work") // no argv: an empty command line
			defer func(f func(string, int) (procInfo, error)) { paneProcEntry = f }(paneProcEntry)
			reread := 0
			paneProcEntry = func(root string, pid int) (procInfo, error) {
				reread++
				c.then(proc)
				return readProcEntry(root, pid)
			}
			got, found, err := PaneProcs{Proc: proc.root}.InTree(10)
			if reread != 1 {
				t.Fatalf("instrument check: the empty command line was read again %d times, want once", reread)
			}
			switch {
			case c.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("InTree = %+v, found %v, err %v; want an error naming %q", got, found, err, c.wantErr)
				}
			case err != nil:
				t.Fatalf("InTree: %v — refused a process whose command line was read again", err)
			case c.want == 0 && found:
				t.Fatalf("REFUSED %+v; want admitted", got)
			case c.want != 0 && (!found || got.PID != c.want || got.Rule != c.wantRule):
				t.Fatalf("InTree = %+v, found %v; want pid %d by %q", got, found, c.want, c.wantRule)
			}
		})
	}
}

// TestThePaneGuardFailsClosed: a pane whose processes cannot be read is not
// presumed safe. The root missing from /proc, an unreadable proc root,
// and a tree larger than the walk's bound are errors, which the guard turns
// into a refusal.
func TestThePaneGuardFailsClosed(t *testing.T) {
	proc := newFakeProc(t)
	proc.add(10, 1, "zsh", "/work", "-zsh")
	for pid := 11; pid <= 15; pid++ {
		proc.add(pid, 10, "sleep", "/work", "sleep", "60")
	}
	id := PaneProcs{Proc: proc.root}
	if _, _, err := id.InTree(10); err != nil {
		t.Fatalf("instrument check: a six-process tree under the default bound answered %v", err)
	}
	if _, found, err := id.InTree(99); err == nil || found {
		t.Fatalf("a root not in /proc: found %v, err %v; want an error", found, err)
	}
	if _, _, err := (PaneProcs{Proc: filepath.Join(proc.root, "absent")}).InTree(10); err == nil {
		t.Fatal("an unreadable proc root answered no error")
	}
	// The bound: 6 processes, a bound of 4 (deliberately not a divisor of
	// anything here). Every one of them is harmless, so only the bound can
	// make it refuse.
	defer func(b int) { procWalkBound = b }(procWalkBound)
	procWalkBound = 4
	if _, found, err := id.InTree(10); err == nil || found || !strings.Contains(err.Error(), "larger than 4") {
		t.Fatalf("a tree past the walk's bound: found %v, err %v; want the bound's error", found, err)
	}
	procWalkBound = 6
	if _, _, err := id.InTree(10); err != nil {
		t.Fatalf("control: the same tree at a bound it fits answered %v", err)
	}
}

// guardedBackend is windowsBackend whose panes have processes: each pane id is
// a root pid in a fixture /proc, and PaneGuard is the real walk over it.
// It also types answers into pickers, recording them.
type guardedBackend struct {
	*windowsBackend
	proc  string
	roots map[string]int
	mu    *sync.Mutex
	typed *[]string
}

func (g guardedBackend) PaneGuard(_ context.Context, pane string) (GuardProc, bool, error) {
	return PaneProcs{Proc: g.proc}.InTree(g.roots[pane])
}

func (g guardedBackend) AnswerAsk(_ context.Context, pane string, _ []askq.Question, _ []askdrive.Choice, _ *askq.Progress) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	*g.typed = append(*g.typed, pane)
	return 0, nil
}

// TestEveryHostPasteTheGuardRefusesTypesNothing is the host paste guard, end
// to end through the host's three entry points that type into a pane — the
// Terminal view's paste (AnyPane), the rendered view's paste into Claude Code,
// and an answer typed into a picker — each refused with the guard's message
// and kind, and NOTHING typed: into an opencode pane by window and by session
// (the active-pane path), into a nested multiplexer, into a pane that cannot
// be read, and into a Claude Code pane with opencode in the foreground
// (`claude &`, then `opencode`). The same host types into a shell window and a
// Claude Code window as before by every path (the controls, which also prove
// the instrument sees what is typed).
func TestEveryHostPasteTheGuardRefusesTypesNothing(t *testing.T) {
	proc := newFakeProc(t)
	proc.add(1400, 1, "zsh", "/work", "-zsh") // %14: a plain shell
	proc.add(2100, 1, "zsh", "/work", "-zsh") // %21: Claude Code
	proc.add(2110, 2100, "claude", "/work", "claude")
	proc.term(2200, 1, "zsh", 7, ptsOf(7), "-zsh") // %22: Claude Code backgrounded, opencode in the foreground
	proc.add(2210, 2200, "claude", "/work", "claude")
	proc.term(2220, 2200, ".opencode-wrapp", 7, ptsOf(7), "opencode")
	proc.term(3000, 1, "zsh", 8, ptsOf(8), "-zsh") // %30: opencode, by hand in a shell
	proc.term(3010, 3000, ".opencode-wrapp", 8, ptsOf(8), "opencode")
	proc.add(3100, 1, "zsh", "/work", "-zsh") // %31: a client of ANOTHER tmux server
	proc.add(3110, 3100, "tmux: client", "/work", "tmux", "-L", "other", "attach")
	proc.term(5000, 1, "zsh", 9, ptsOf(9), "-zsh") // %legacy: the session's active pane, opencode
	proc.term(5010, 5000, "opencode", 9, ptsOf(9), "/opt/oc/bin/opencode")
	windows := []WindowInfo{
		{Session: "scratch2", ID: "@4", Index: 1, Pane: "%14", PanePID: 1400, Command: "zsh"},
		{Session: "scratch2", ID: "@7", Index: 8, Active: true, Pane: "%21", PanePID: 2100, Command: "claude", Claude: &ClaudePane{Pane: "%21", Gen: "g7"}},
		{Session: "scratch2", ID: "@8", Index: 9, Pane: "%22", PanePID: 2200, Command: "opencode", Claude: &ClaudePane{Pane: "%22", Gen: "g8"}},
		{Session: "solo", ID: "@9", Index: 0, Active: true, Pane: "%30", PanePID: 3000, Command: "opencode"},
		{Session: "solo", ID: "@10", Index: 1, Pane: "%31", PanePID: 3100, Command: "tmux"},
		{Session: "gone", ID: "@11", Index: 0, Active: true, Pane: "%40", PanePID: 4000, Command: "zsh"},
	}
	b := &windowsBackend{windows: windows}
	var typed []string
	g := guardedBackend{windowsBackend: b, proc: proc.root, mu: &sync.Mutex{}, typed: &typed,
		roots: map[string]int{"%14": 1400, "%21": 2100, "%22": 2200, "%30": 3000, "%31": 3100, "%legacy": 5000, "%40": 4000 /* not in /proc */}}
	d := newDrivenMuster(t)
	d.run(t, wrapped{g}, nil, true) // through a wrapper, as NoShrink wraps the real backend
	pasted := func() []string { _, _, _, p, _ := b.snapshot(); return p }
	answers := func() []string { g.mu.Lock(); defer g.mu.Unlock(); return slices.Clone(typed) }
	reply := func(typ string, id uint32) mirror.Msg {
		var out mirror.Msg
		waitUntil(t, "the host's answer", func() bool {
			for _, m := range d.of(typ) {
				if m.ID == id {
					out = m
					return true
				}
			}
			return false
		})
		return out
	}
	ask := &mirror.AskTyping{Questions: hostAskQs, Choices: []askq.Choice{{Picks: []int{1}}}}

	// Controls first: a shell, and a Claude Code window by all three paths.
	for i, m := range []mirror.Msg{
		{Type: mirror.TypePaste, Session: "scratch2", Window: "@4", Text: "ls -la", AnyPane: true},
		{Type: mirror.TypePaste, Session: "scratch2", Window: "@7", Text: "to claude, terminal", AnyPane: true},
		{Type: mirror.TypePaste, Session: "scratch2", Window: "@7", Text: "to claude, rendered"},
		{Type: mirror.TypeAnswer, Session: "scratch2", Window: "@7", Ask: ask},
	} {
		m.ID = uint32(1 + i)
		d.send(t, m)
		typ := mirror.TypePasted
		if m.Type == mirror.TypeAnswer {
			typ = mirror.TypeAnswered
		}
		if a := reply(typ, m.ID); a.Error != "" || a.Refusal != "" {
			t.Fatalf("control %d (%s into %s): refused %q (%q)", m.ID, m.Type, m.Window, a.Error, a.Refusal)
		}
	}
	wantPasted := []string{"%14 ls -la", "%21 to claude, terminal", "%21 to claude, rendered"}
	if got := pasted(); !slices.Equal(got, wantPasted) {
		t.Fatalf("instrument check: pasted %q, want %q", got, wantPasted)
	}
	if got := answers(); !slices.Equal(got, []string{"%21"}) {
		t.Fatalf("instrument check: answers typed into %q, want [%%21]", got)
	}

	for i, c := range []struct {
		m       mirror.Msg
		why     error
		refusal string
	}{
		{mirror.Msg{Type: mirror.TypePaste, Session: "solo", Window: "@9", Text: "4", AnyPane: true}, ErrOpenCodePane, mirror.PasteRefusalOpenCode},
		{mirror.Msg{Type: mirror.TypePaste, Session: "solo", Text: "approve nothing", AnyPane: true}, ErrOpenCodePane, mirror.PasteRefusalOpenCode},
		{mirror.Msg{Type: mirror.TypePaste, Session: "solo", Window: "@10", Text: "nested", AnyPane: true}, ErrMultiplexerPane, mirror.PasteRefusalMultiplexer},
		{mirror.Msg{Type: mirror.TypePaste, Session: "gone", Window: "@11", Text: "unchecked", AnyPane: true}, ErrPaneUnchecked, mirror.PasteRefusalUnchecked},
		// 🔴 Claude Code's own paths, into its pane with opencode in the foreground.
		{mirror.Msg{Type: mirror.TypePaste, Session: "scratch2", Window: "@8", Text: "for claude, rendered"}, ErrOpenCodePane, mirror.PasteRefusalOpenCode},
		{mirror.Msg{Type: mirror.TypePaste, Session: "scratch2", Window: "@8", Text: "for claude, terminal", AnyPane: true}, ErrOpenCodePane, mirror.PasteRefusalOpenCode},
		{mirror.Msg{Type: mirror.TypeAnswer, Session: "scratch2", Window: "@8", Ask: ask}, ErrOpenCodePane, ""},
	} {
		c.m.ID = uint32(10 + i)
		d.send(t, c.m)
		typ := mirror.TypePasted
		if c.m.Type == mirror.TypeAnswer {
			typ = mirror.TypeAnswered
		}
		a := reply(typ, c.m.ID)
		// The refusal is the guard's own words EXACTLY: nothing internal (a
		// pid, /proc, tmux's stderr) appended.
		if a.Error != c.why.Error() || (typ == mirror.TypePasted && a.Refusal != c.refusal) {
			t.Fatalf("%s into %s/%s answered %q (kind %q); want %q (kind %q)", c.m.Type, c.m.Session, c.m.Window, a.Error, a.Refusal, c.why.Error(), c.refusal)
		}
	}
	if got := pasted(); !slices.Equal(got, wantPasted) {
		t.Fatalf("a refused paste was typed: pasted %q, want only %q", got, wantPasted)
	}
	if got := answers(); !slices.Equal(got, []string{"%21"}) {
		t.Fatalf("a refused answer was typed: into %q, want only [%%21]", got)
	}
}
