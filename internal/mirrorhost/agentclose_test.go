package mirrorhost

import (
	"context"
	"errors"
	"io"
	"log"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// THE HOST HALF OF agent-close (agentclose.go), over a fake backend that plays
// tmux: its windows, the pane a paste reached, and the windows it closed. The
// fixture's windows are pairwise distinct: @1 Claude Code, @2 opencode, @3 a
// plain shell, all in session "work", and @4 Claude Code in session "other".

type closeBackend struct {
	mu      sync.Mutex
	windows []WindowInfo
	pastes  []string // "<pane> <text>"
	killed  []string
	// onExit is what typing /exit into a pane does: "gone" (the agent was the
	// window's process), "shell" (the pane is back at a shell) or "" (nothing:
	// the agent ignores it).
	onExit string
}

func (b *closeBackend) List(context.Context) ([]string, error) { return []string{"work", "other"}, nil }
func (b *closeBackend) Attach(context.Context, string, bool, uint16, uint16) (Terminal, error) {
	return nil, errors.New("no attach in this fixture")
}
func (b *closeBackend) Windows(context.Context) ([]WindowInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.windows), nil
}
func (b *closeBackend) ActivePane(context.Context, string) (string, bool) { return "", false }
func (b *closeBackend) PastePane(_ context.Context, pane, text string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pastes = append(b.pastes, pane+" "+text)
	for i, w := range b.windows {
		if agentPaneOf(w) != pane || text != "/exit" {
			continue
		}
		switch b.onExit {
		case "gone":
			b.windows = slices.Delete(b.windows, i, i+1)
		case "shell":
			b.windows[i].Claude, b.windows[i].Agent, b.windows[i].Command = nil, nil, "zsh"
		}
		break
	}
	return nil
}
func (b *closeBackend) KillWindow(_ context.Context, window string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.killed = append(b.killed, window)
	b.windows = slices.DeleteFunc(b.windows, func(w WindowInfo) bool { return w.ID == window })
	return nil
}

func closeFixture(onExit string) (*Client, *closeBackend) {
	b := &closeBackend{onExit: onExit, windows: []WindowInfo{
		{Session: "work", ID: "@1", Pane: "%11", Command: "claude", Claude: &ClaudePane{Pane: "%11"}},
		{Session: "work", ID: "@2", Pane: "%12", Command: "opencode", Agent: &AgentPane{}},
		{Session: "work", ID: "@3", Pane: "%13", Command: "zsh"},
		{Session: "other", ID: "@4", Pane: "%14", Command: "claude", Claude: &ClaudePane{Pane: "%14"}},
	}}
	return &Client{log: log.New(io.Discard, "", 0), cfg: Config{Interactive: true, Backend: b, AgentCloseGrace: 600 * time.Millisecond}}, b
}

// TestAnAgentCloseExitsGracefullyThenCloses: /exit is typed into the agent's
// pane; an agent that exits with its window is a graceful end with no
// kill-window; one whose pane returns to a shell is graceful and its window
// is closed; one that ignores /exit is closed after the grace, not graceful.
// Both runtimes.
func TestAnAgentCloseExitsGracefullyThenCloses(t *testing.T) {
	for _, c := range []struct {
		onExit, window, pane string
		graceful             bool
		killed               []string
	}{
		{"gone", "@1", "%11", true, nil},
		{"shell", "@2", "%12", true, []string{"@2"}},
		{"", "@1", "%11", false, []string{"@1"}},
		{"", "@2", "%12", false, []string{"@2"}},
	} {
		cl, b := closeFixture(c.onExit)
		start := time.Now()
		r, err := cl.closeAgent(context.Background(), true, mirror.AgentCloseReq{Session: "work", Window: c.window})
		took := time.Since(start)
		if err != nil || r.Graceful != c.graceful {
			t.Fatalf("%q on %s: %+v %v; want graceful %t", c.onExit, c.window, r, err, c.graceful)
		}
		if !slices.Equal(b.pastes, []string{c.pane + " /exit"}) || !slices.Equal(b.killed, c.killed) {
			t.Errorf("%q on %s: typed %v, closed %v; want %q then closed %v", c.onExit, c.window, b.pastes, b.killed, c.pane+" /exit", c.killed)
		}
		// The timeout fallback waits the grace (600 ms here), the graceful
		// ends do not.
		if !c.graceful && took < 600*time.Millisecond {
			t.Errorf("%s was closed after %s, before the grace", c.window, took)
		}
		if c.graceful && took > 550*time.Millisecond {
			t.Errorf("a graceful end of %s took %s", c.window, took)
		}
	}
}

// TestAnAgentCloseRefusesAnythingElse: a window not listed, a window of
// another session (the id is listed, the session is not its), a window with
// no agent, a host that does not close windows: each is refused with its code,
// and NOTHING is typed or closed.
func TestAnAgentCloseRefusesAnythingElse(t *testing.T) {
	for _, c := range []struct {
		name string
		on   bool
		req  mirror.AgentCloseReq
		code string
	}{
		{"no such window", true, mirror.AgentCloseReq{Session: "work", Window: "@9"}, mirror.CloseNotListed},
		{"another session's window", true, mirror.AgentCloseReq{Session: "work", Window: "@4"}, mirror.CloseNotListed},
		{"not a window id", true, mirror.AgentCloseReq{Session: "work", Window: "work:1"}, mirror.CloseNotListed},
		{"a plain shell", true, mirror.AgentCloseReq{Session: "work", Window: "@3"}, mirror.CloseNoAgent},
		{"a host that does not close", false, mirror.AgentCloseReq{Session: "work", Window: "@1"}, mirror.CloseUnavailable},
	} {
		cl, b := closeFixture("gone")
		_, err := cl.closeAgent(context.Background(), c.on, c.req)
		var e *mirror.CloseRefusedError
		if !errors.As(err, &e) || e.Code != c.code {
			t.Errorf("%s: %v; want refused %s", c.name, err, c.code)
		}
		if len(b.pastes) != 0 || len(b.killed) != 0 {
			t.Errorf("%s: typed %v and closed %v; want nothing", c.name, b.pastes, b.killed)
		}
	}
	// Positive control: the same fixture closes a listed agent window.
	cl, b := closeFixture("gone")
	if _, err := cl.closeAgent(context.Background(), true, mirror.AgentCloseReq{Session: "other", Window: "@4"}); err != nil || len(b.pastes) != 1 {
		t.Fatalf("control: %v, typed %v", err, b.pastes)
	}
}

// TestOnlyAnInteractiveHostThatCanAdvertisesAgentClose: the hello's
// AgentClose is an -interactive v2 host's whose backend lists, pastes and
// closes.
func TestOnlyAnInteractiveHostThatCanAdvertisesAgentClose(t *testing.T) {
	cl, _ := closeFixture("gone")
	for _, c := range []struct {
		interactive bool
		version     int
		want        bool
	}{{true, 2, true}, {false, 2, false}, {true, 1, false}} {
		cl.cfg.Interactive = c.interactive
		h := &hostSide{c: cl, version: c.version}
		if got := h.agentCloseOn(); got != c.want {
			t.Errorf("interactive %t v%d: %t; want %t", c.interactive, c.version, got, c.want)
		}
	}
	h := &hostSide{c: &Client{cfg: Config{Interactive: true, Backend: &windowsBackend{}}}, version: 2}
	if h.agentCloseOn() {
		t.Error("a backend that cannot close a window advertised agent-close")
	}
}
