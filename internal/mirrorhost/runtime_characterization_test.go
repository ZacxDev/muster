package mirrorhost

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/mirror"
)

// CHARACTERIZATION GOLDENS for the runtime-interface refactor, host half:
// recorded BEFORE it against the per-runtime branches it replaces, and read
// the same after it (see internal/mirror's runtime_characterization_test.go).
// A golden that changes is a behaviour change. Every fixture is synthetic.

var updateCharacterization = flag.Bool("update-characterization", false, "rewrite testdata/characterization/*.golden")

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "characterization", name+".golden")
	if *updateCharacterization {
		must(t, os.MkdirAll(filepath.Dir(path), 0o755))
		must(t, os.WriteFile(path, []byte(got), 0o644))
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the golden (record it with -update-characterization BEFORE a refactor, never after): %v", err)
	}
	if string(want) != got {
		t.Fatalf("%s differs from its golden %s.\n--- got\n%s\n--- want\n%s", name, path, got, want)
	}
}

func charJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "!" + err.Error()
	}
	return string(b)
}

// charProcs is one pane of every shape the guard and the locators decide on,
// each a root shell (pid) with its process tree.
func charProcs(f *fakeProc) map[string]int {
	py, nixHermes := "/usr/bin/python3.14", "/nix/store/0000000000000000000000000000000b-hermes-agent/bin/hermes"
	add := func(pid int, tty string) {
		f.add(pid, 1, "zsh", "/w", "-zsh")
		f.stdin(pid, tty)
	}
	add(100, "/dev/pts/1") // shell
	add(110, "/dev/pts/2") // Claude Code
	f.add(111, 110, "claude", "/w", "claude")
	add(120, "/dev/pts/3") // hermes chat
	f.add(121, 120, "hermes", "/w", py, nixHermes, "chat")
	f.stdin(121, "/dev/pts/3")
	add(130, "/dev/pts/4") // hermes gateway (broad, not strict)
	f.add(131, 130, "hermes", "/w", py, nixHermes, "gateway", "run")
	f.stdin(131, "/dev/pts/4")
	add(140, "/dev/pts/5") // a profile CLI (broad, not strict)
	f.add(141, 140, "hermes", "/w", py, nixHermes, "-p", "coder", "chat")
	f.stdin(141, "/dev/pts/5")
	add(150, "/dev/pts/6") // python -m hermes_cli
	f.add(151, 150, "python3", "/w", "python3", "-m", "hermes_cli.main")
	add(160, "/dev/pts/7") // nested tmux
	f.add(161, 160, "tmux: client", "/w", "tmux", "-L", "x", "attach")
	add(170, "/dev/pts/8") // opencode TUI
	f.add(171, 170, ".opencode-wrapp", "/w", "opencode")
	add(180, "/dev/pts/9") // Claude, then hermes in the foreground
	f.add(181, 180, "claude", "/w", "claude")
	f.add(182, 180, "hermes", "/w", py, nixHermes, "chat")
	f.stdin(182, "/dev/pts/9")
	add(190, "/dev/pts/10") // hermes chat on ANOTHER terminal (not this pane's)
	f.add(191, 190, "hermes", "/w", py, nixHermes, "chat")
	f.stdin(191, "/dev/pts/99")
	add(200, "/dev/pts/11") // opencode under node
	f.add(201, 200, "node", "/w", "node", "/x/bin/opencode")
	add(210, "/dev/pts/12") // vim on a hermes file: nothing
	f.add(211, 210, "nvim", "/w", "nvim", "hermes.md")
	return map[string]int{"%shell": 100, "%claude": 110, "%hermes": 120, "%gateway": 130, "%profile": 140, "%module": 150,
		"%tmux": 160, "%opencode": 170, "%claudehermes": 180, "%othertty": 190, "%ocnode": 200, "%nvim": 210, "%absent": 999}
}

// TestCharacterizeTheHostsRuntimeDetectionAndGuard: per pane, the paste
// guard's verdict (InTree: pid and rule; the refusal kind it becomes), the
// strict Hermes rule's claim, and which runtime the listing attaches — and
// the window list those panes put on the wire.
func TestCharacterizeTheHostsRuntimeDetectionAndGuard(t *testing.T) {
	proc := newFakeProc(t)
	roots := charProcs(proc)
	panes := []string{"%shell", "%claude", "%hermes", "%gateway", "%profile", "%module", "%tmux", "%opencode", "%claudehermes", "%othertty", "%ocnode", "%nvim", "%absent"}
	var b strings.Builder
	id := HermesIdentity{Proc: proc.root}
	for _, p := range panes {
		hp, found, err := id.InTree(roots[p])
		e := "nil"
		if err != nil {
			e = "error"
		}
		mp, mfound, _ := id.MultiplexerInTree(roots[p])
		fmt.Fprintf(&b, "guard %s found=%v pid=%d rule=%q err=%s multiplexer=%v/%d\n", p, found, hp.PID, hp.Rule, e, mfound, mp.PID)
	}
	t3 := Tmux{Claude: ClaudeLocator{Proc: proc.root, Home: t.TempDir()}}
	names := []string{}
	for _, rt := range t3.runtimes() {
		names = append(names, rt.Name())
	}
	fmt.Fprintf(&b, "runtimes %q\n", names)
	var windows []WindowInfo
	for i, p := range panes {
		tty := fmt.Sprintf("/dev/pts/%d", i+1)
		w := WindowInfo{Session: "work", ID: fmt.Sprintf("@%d", i+1), Index: i, Active: i == 0, Pane: p, PanePID: roots[p], TTY: tty, Command: "zsh"}
		if p == "%claude" || p == "%claudehermes" {
			w.Claude = &ClaudePane{Pane: p, Gen: "c-" + p[1:], Status: mirror.StatusIdle, StatusSince: 1}
		}
		windows = append(windows, w)
	}
	procs, children := t3.Claude.table()
	attachAgents(windows, procs, children, t3.runtimes()[1:])
	for _, w := range windows {
		fmt.Fprintf(&b, "attached %s claude=%v agent=%s\n", w.Pane, w.Claude != nil, charJSON(w.Agent))
	}
	for _, w := range WireWindows(windows, nil, true, time.Unix(1792000002, 0)) {
		fmt.Fprintf(&b, "wire %s\n", charJSON(w))
	}
	golden(t, "detect", b.String())
}

// TestCharacterizeTheHostsRuntimeRouting: through a REAL host client, what
// the host answers each message a runtime decides — a paste (each Runtime
// value, each target), an answer (each Runtime value), a watch — into each
// kind of window, with -hermes-send off and on, and what it typed.
func TestCharacterizeTheHostsRuntimeRouting(t *testing.T) {
	var b strings.Builder
	for _, hermesSend := range []bool{false, true} {
		proc := newFakeProc(t)
		roots := charProcs(proc)
		windows := []WindowInfo{
			{Session: "work", ID: "@1", Index: 0, Active: true, Pane: "%shell", PanePID: 100, Command: "zsh"},
			{Session: "work", ID: "@2", Index: 1, Pane: "%claude", PanePID: 110, Command: "claude", Claude: &ClaudePane{Pane: "%claude", Gen: "c-1"}},
			{Session: "work", ID: "@3", Index: 2, Pane: "%hermes", PanePID: 120, Command: "python3.14",
				Agent: &AgentPane{Runtime: mirror.RuntimeHermes, Pane: "%hermes", PID: 121, Gen: "local-x", Status: mirror.StatusIdle, StatusSince: 5, Source: SourceBreadcrumb}},
			{Session: "work", ID: "@4", Index: 3, Pane: "%opencode", PanePID: 170, Command: "opencode"},
			{Session: "work", ID: "@5", Index: 4, Pane: "%tmux", PanePID: 160, Command: "tmux"},
		}
		bk := &windowsBackend{windows: windows}
		var typed []string
		g := guardedBackend{windowsBackend: bk, proc: proc.root, mu: &sync.Mutex{}, typed: &typed, roots: roots}
		d := newDrivenMuster(t)
		d.runHost(t, wrapped{g}, true, hermesSend)
		hello := d.of(mirror.TypeHello)[0]
		fmt.Fprintf(&b, "== hermesSend=%v\nhello interactive=%v panePaste=%v hermesSend=%v hermesAnswer=%v statusReport=%v version=%d\n",
			hermesSend, hello.Interactive, hello.PanePaste, hello.HermesSend, hello.HermesAnswer, hello.StatusReport, hello.Version)
		for _, w := range hello.Windows {
			fmt.Fprintf(&b, "hello window %s\n", charJSON(w))
		}
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
		next := uint32(100)
		for _, win := range []string{"@1", "@2", "@3", "@4", "@5"} {
			for _, rt := range []string{"", mirror.RuntimeClaude, mirror.RuntimeHermes, "opencode"} {
				for _, anyPane := range []bool{false, true} {
					next++
					d.send(t, mirror.Msg{Type: mirror.TypePaste, ID: next, Session: "work", Window: win, Text: "t", AnyPane: anyPane, Runtime: rt, Since: 5})
					a := reply(mirror.TypePasted, next)
					fmt.Fprintf(&b, "paste %s runtime=%q anyPane=%v -> error=%q refusal=%q\n", win, rt, anyPane, a.Error, a.Refusal)
				}
			}
			for _, rt := range []string{"", mirror.RuntimeHermes} {
				next++
				m := mirror.Msg{Type: mirror.TypeAnswer, ID: next, Session: "work", Window: win, Runtime: rt}
				if rt == mirror.RuntimeHermes {
					m.HermesApproval = &mirror.HermesApprovalAnswer{PromptID: "p-1", Choice: "once"}
				} else {
					m.Ask = ask
				}
				d.send(t, m)
				a := reply(mirror.TypeAnswered, next)
				fmt.Fprintf(&b, "answer %s runtime=%q -> error=%q refusal=%q\n", win, rt, a.Error, a.Refusal)
			}
			next++
			d.send(t, mirror.Msg{Type: mirror.TypeWatch, ID: next, Session: "work", Window: win})
			a := reply(mirror.TypeClosed, next)
			fmt.Fprintf(&b, "watch %s -> closed error=%q\n", win, a.Error)
		}
		pasted := func() []string { _, _, _, p, _ := bk.snapshot(); return p }
		g.mu.Lock()
		fmt.Fprintf(&b, "typed pastes %q\ntyped answers %q\n", pasted(), typed)
		g.mu.Unlock()
	}
	golden(t, "routing", b.String())
}
