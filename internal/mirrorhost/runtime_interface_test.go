package mirrorhost

import (
	"slices"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestTheHostsRuntimesAreTheServersRuntimes: the host's runtimes and the
// server's runtime table (mirror.Runtimes) are the SAME set, and each reports
// the same capabilities on both sides — a seam guard over the two halves of
// the runtime interface, failing when either grows, shrinks or disagrees.
func TestTheHostsRuntimesAreTheServersRuntimes(t *testing.T) {
	var host, server []string
	for _, r := range knownRuntimes {
		host = append(host, r.Name())
		spec, ok := mirror.LookupRuntime(r.Name())
		if !ok {
			t.Errorf("the host's runtime %q is not in the server's table", r.Name())
			continue
		}
		if spec.Caps != r.Capabilities() {
			t.Errorf("%s: the host reports %+v, the server %+v", r.Name(), r.Capabilities(), spec.Caps)
		}
		// A capability the host claims has its implementation here.
		if _, ok := r.(viewSource); r.Capabilities().View != ok {
			t.Errorf("%s: View %v, but a view source %v", r.Name(), r.Capabilities().View, ok)
		}
		_, gated := r.(gatedSender)
		if gated != spec.GatedSend {
			t.Errorf("%s: a gated sender %v, the server's GatedSend %v", r.Name(), gated, spec.GatedSend)
		}
	}
	for _, r := range mirror.Runtimes() {
		server = append(server, r.Name)
	}
	slices.Sort(host)
	slices.Sort(server)
	if !slices.Equal(host, server) {
		t.Fatalf("host runtimes %q, server runtimes %q", host, server)
	}
	// The listing's runtimes are the ones that locate panes: Claude Code
	// first, then Hermes; the opencode stub locates nothing.
	var loc []string
	for _, r := range (Tmux{}).runtimes() {
		loc = append(loc, r.Name())
	}
	if want := []string{mirror.RuntimeClaude, mirror.RuntimeHermes}; !slices.Equal(loc, want) {
		t.Fatalf("locating runtimes %q, want %q", loc, want)
	}
}

// TestTheOpenCodeStubReportsNoViewAndNoSend: opencode is a runtime of the
// interface that can do nothing yet — "view: no, send: no", no prompts —
// claims no pane even when one runs opencode, has no view source, no gated
// send and no answerer, and its one behaviour is the paste guard's refusal.
func TestTheOpenCodeStubReportsNoViewAndNoSend(t *testing.T) {
	r, ok := runtimeNamed(mirror.RuntimeOpenCode)
	if !ok {
		t.Fatal("no opencode runtime")
	}
	if caps := r.Capabilities(); caps.View || caps.Send || caps.Prompts {
		t.Fatalf("the stub reports %+v, want view: no, send: no", caps)
	}
	if _, ok := r.(viewSource); ok {
		t.Error("the stub has a view source")
	}
	if _, ok := r.(gatedSender); ok {
		t.Error("the stub has a send")
	}
	if _, ok := r.(answerer); ok {
		t.Error("the stub answers")
	}
	proc := newFakeProc(t)
	proc.add(10, 1, "zsh", "/w", "-zsh")
	proc.stdin(10, "/dev/pts/1")
	proc.add(11, 10, ".opencode-wrapp", "/w", "opencode")
	procs, children := (ClaudeLocator{Proc: proc.root}).table()
	if got := r.LocatePanes([]pane{{Session: "s", ID: "%1", PID: 10, TTY: "/dev/pts/1"}}, procs, children); len(got) != 0 {
		t.Fatalf("the stub claimed %+v", got)
	}
	g, ok := r.(pasteGuard)
	if !ok {
		t.Fatal("the stub is not a paste guard")
	}
	if rule, ok := g.shape(procInfo{comm: ".opencode-wrapp", argv: []string{"opencode"}}, func() string { return "" }); !ok || !g.owns(rule) ||
		g.refusal() != ErrOpenCodePane || g.refusalKind() != mirror.PasteRefusalOpenCode {
		t.Fatalf("the stub's guard: rule %q ok %v", rule, ok)
	}
}
