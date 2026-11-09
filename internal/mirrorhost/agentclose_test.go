package mirrorhost

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// THE HOST HALF OF agent-close (agentclose.go), over a fake backend that plays
// tmux AS PRODUCTION GUARDS IT: its windows, Claude Code's screen (real
// captures of the CLI), the checked paste (CheckedPaste, the Claude prompt
// guard's), the paste guard (an opencode pane is refused, as the real
// process walk refuses it), the pane a paste reached and the windows it
// closed. The fixture's windows are pairwise distinct: @1 Claude Code, @2
// opencode, @3 a plain shell, all in session "work", and @4 Claude Code in
// session "other".

const (
	ccKey1 = "11111111-aaaa-4bbb-8ccc-000000000001"
	ccKey4 = "44444444-aaaa-4bbb-8ccc-000000000004"
	ocSID2 = "ses_0closefixture2"
)

// capture is a real capture of the Claude Code CLI (or its picker).
func capture(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type closeBackend struct {
	mu      sync.Mutex
	windows []WindowInfo
	screens map[string]string // pane → its screen
	pastes  []string          // "<pane> <text>": what a paste typed
	enters  []string          // panes an Enter was pressed in
	killed  []string
	// onExit is what an Enter on /exit does: "gone" (the agent was the
	// window's process), "shell" (the pane is back at a shell) or "" (nothing:
	// the agent ignores it).
	onExit string
	// afterExit, when set, runs after an Enter on /exit (with the lock held).
	afterExit func(b *closeBackend)
	// afterPaste, when set, runs between the paste and the check before its
	// Enter (with the lock held): a dialog opening as /exit lands.
	afterPaste func(b *closeBackend)
}

func (b *closeBackend) List(context.Context) ([]string, error) { return []string{"work", "other"}, nil }
func (b *closeBackend) Attach(context.Context, string, bool, uint16, uint16) (Terminal, error) {
	return nil, errors.New("no attach in this fixture")
}
func (b *closeBackend) Windows(context.Context) ([]WindowInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := slices.Clone(b.windows)
	for i := range out {
		if out[i].Claude != nil {
			c := *out[i].Claude
			out[i].Claude = &c
		}
	}
	return out, nil
}
func (b *closeBackend) ActivePane(context.Context, string) (string, bool) { return "", false }

// PastePane is the raw paste (paste-buffer and Enter, unchecked): recorded,
// and it does NOTHING to the agent. agent-close must never use it.
func (b *closeBackend) PastePane(_ context.Context, pane, text string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pastes = append(b.pastes, pane+" "+text)
	b.enters = append(b.enters, pane)
	return nil
}

// ClaudePanes and Paste: a per-window host's Claude backend.
func (b *closeBackend) ClaudePanes(context.Context, []string) map[string]ClaudePane { return nil }
func (b *closeBackend) Paste(ctx context.Context, p ClaudePane, text string) error {
	return b.PastePane(ctx, p.Pane, text)
}

// ClaudeScreen is the pane's screen: the CLI at its idle prompt unless the
// case set another.
func (b *closeBackend) ClaudeScreen(_ context.Context, pane string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.screens[pane]; ok {
		return s, nil
	}
	return "", errors.New("capture-pane: no such pane")
}

// PasteChecked is the Claude prompt guard's checked paste, through the real
// CheckedPaste: before, the paste, beforeEnter, then Enter.
func (b *closeBackend) PasteChecked(ctx context.Context, p ClaudePane, text string, before, beforeEnter func(context.Context) error) error {
	return CheckedPaste(ctx,
		func(context.Context) error {
			b.mu.Lock()
			defer b.mu.Unlock()
			b.pastes = append(b.pastes, p.Pane+" "+text)
			if b.afterPaste != nil {
				b.afterPaste(b)
			}
			return nil
		},
		func(context.Context) error {
			b.mu.Lock()
			defer b.mu.Unlock()
			b.enters = append(b.enters, p.Pane)
			if text != "/exit" {
				return nil
			}
			for i, w := range b.windows {
				if w.Claude == nil || w.Claude.Pane != p.Pane {
					continue
				}
				switch b.onExit {
				case "gone":
					b.windows = slices.Delete(b.windows, i, i+1)
				case "shell":
					b.windows[i].Claude, b.windows[i].Command = nil, "zsh"
				}
				break
			}
			if b.afterExit != nil {
				b.afterExit(b)
			}
			return nil
		},
		before, beforeEnter)
}

// PaneGuard plays the paste guard's process walk: an opencode pane holds
// opencode (refused, as production refuses it); any other holds nothing it
// refuses.
func (b *closeBackend) PaneGuard(_ context.Context, pane string) (GuardProc, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, w := range b.windows {
		if w.Pane == pane && w.Agent != nil {
			return GuardProc{PID: 2, Rule: ruleOpenCode}, true, nil
		}
	}
	return GuardProc{}, false, nil
}

func (b *closeBackend) KillWindow(_ context.Context, window string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.killed = append(b.killed, window)
	b.windows = slices.DeleteFunc(b.windows, func(w WindowInfo) bool { return w.ID == window })
	return nil
}

func closeFixture(t *testing.T, onExit string) (*Client, *closeBackend) {
	idle := capture(t, "../../cmd/ccd/testdata/pane_idle_2.1.296.txt")
	b := &closeBackend{onExit: onExit, screens: map[string]string{"%11": idle, "%14": idle}, windows: []WindowInfo{
		{Session: "work", ID: "@1", Pane: "%11", Command: "claude", Claude: &ClaudePane{Pane: "%11", PID: 101, Gen: ccKey1, Status: mirror.StatusIdle}},
		{Session: "work", ID: "@2", Pane: "%12", Command: "opencode", Agent: &AgentPane{Runtime: mirror.RuntimeOpenCode, Pane: "%12", PID: 102, Gen: ocSID2}},
		{Session: "work", ID: "@3", Pane: "%13", Command: "zsh"},
		{Session: "other", ID: "@4", Pane: "%14", Command: "claude", Claude: &ClaudePane{Pane: "%14", PID: 104, Gen: ccKey4, Status: mirror.StatusIdle}},
	}}
	return &Client{log: log.New(io.Discard, "", 0), cfg: Config{Interactive: true, Backend: b, AgentCloseGrace: 600 * time.Millisecond}}, b
}

func runClose(cl *Client, req mirror.AgentCloseReq) (mirror.AgentCloseResult, error, time.Duration) {
	start := time.Now()
	r, err := (&hostSide{c: cl, ctx: context.Background(), version: 2}).closeAgent(context.Background(), true, req)
	return r, err, time.Since(start)
}

var req1 = mirror.AgentCloseReq{Session: "work", Window: "@1", Key: ccKey1}

// TestAnIdleClaudeIsAskedToExitThenClosed: at an idle prompt with an empty
// box, /exit is typed through the CHECKED paste and Enter pressed; an agent
// that exits with its window is a graceful end with no kill-window; one
// whose pane returns to a shell is graceful and its window is closed; one
// that ignores /exit is closed after the grace, not graceful, with its reason.
func TestAnIdleClaudeIsAskedToExitThenClosed(t *testing.T) {
	for _, c := range []struct {
		onExit   string
		graceful bool
		killed   []string
		reason   string
	}{
		{"gone", true, nil, ""},
		{"shell", true, []string{"@1"}, ""},
		{"", false, []string{"@1"}, closeReasonGrace},
	} {
		cl, b := closeFixture(t, c.onExit)
		r, err, took := runClose(cl, req1)
		if err != nil || r.Graceful != c.graceful || r.Reason != c.reason {
			t.Fatalf("%q: %+v %v; want graceful %t, reason %q", c.onExit, r, err, c.graceful, c.reason)
		}
		if !slices.Equal(b.pastes, []string{"%11 /exit"}) || !slices.Equal(b.enters, []string{"%11"}) || !slices.Equal(b.killed, c.killed) {
			t.Errorf("%q: typed %v, Enter in %v, closed %v; want /exit into %%11, one Enter, then closed %v", c.onExit, b.pastes, b.enters, b.killed, c.killed)
		}
		if !c.graceful && took < 600*time.Millisecond {
			t.Errorf("closed after %s, before the grace", took)
		}
		if c.graceful && took > 550*time.Millisecond {
			t.Errorf("a graceful end took %s", took)
		}
	}
}

// TestAClaudeWaitingOnThePersonIsTypedNothing (🔴1): a pane at a permission
// prompt (Enter would approve it), at the AskUserQuestion picker (Enter
// would take its first option), with a draft in its box (Enter would submit
// it with "/exit" on it), or whose session status says waiting, is typed
// NOTHING and no Enter is pressed: its window is closed at once, not
// graceful, with why. Real captures of the CLI.
func TestAClaudeWaitingOnThePersonIsTypedNothing(t *testing.T) {
	for _, c := range []struct {
		name, screen, status, why string
	}{
		{"a permission prompt", capture(t, "../../cmd/ccd/testdata/pane_permission_2.1.296.txt"), mirror.StatusIdle, "its screen showed a dialog"},
		{"the picker", capture(t, "../askdrive/testdata/multi-q.txt"), mirror.StatusIdle, "its screen showed a dialog"},
		{"a draft in the box", capture(t, "../ccdialog/testdata/pane_idle_typed_2.1.289.ansi"), mirror.StatusIdle, "its input box held a draft"},
		{"waiting by its status", capture(t, "../../cmd/ccd/testdata/pane_idle_2.1.296.txt"), mirror.StatusWaiting, "it was waiting on you"},
	} {
		cl, b := closeFixture(t, "gone")
		b.screens["%11"] = c.screen
		b.windows[0].Claude.Status = c.status
		r, err, took := runClose(cl, req1)
		if err != nil || r.Graceful || !strings.Contains(r.Reason, c.why) {
			t.Errorf("%s: %+v %v; want closed, not graceful, because %q", c.name, r, err, c.why)
		}
		if len(b.pastes) != 0 || len(b.enters) != 0 {
			t.Errorf("%s: typed %v and pressed Enter in %v; want nothing typed", c.name, b.pastes, b.enters)
		}
		if !slices.Equal(b.killed, []string{"@1"}) || took > 300*time.Millisecond {
			t.Errorf("%s: closed %v after %s; want @1 closed at once", c.name, b.killed, took)
		}
	}
}

// TestADialogOpeningAsExitLandsWithholdsTheEnter (#549 round 2): the check
// before the Enter is real. A permission prompt, or a waiting status, that
// appears between the paste of /exit and its Enter: NO Enter is pressed, and
// the window is closed at once, not graceful, saying /exit was typed but its
// Enter withheld. The control: nothing appears, and the Enter is pressed.
func TestADialogOpeningAsExitLandsWithholdsTheEnter(t *testing.T) {
	permission := capture(t, "../../cmd/ccd/testdata/pane_permission_2.1.296.txt")
	for _, c := range []struct {
		name string
		flip func(b *closeBackend)
		why  string
	}{
		{"a permission prompt", func(b *closeBackend) { b.screens["%11"] = permission }, "its screen showed a dialog"},
		{"a waiting status", func(b *closeBackend) { b.windows[0].Claude.Status = mirror.StatusWaiting }, "it was waiting on you"},
	} {
		cl, b := closeFixture(t, "gone")
		b.afterPaste = c.flip
		r, err, _ := runClose(cl, req1)
		if !slices.Equal(b.pastes, []string{"%11 /exit"}) || len(b.enters) != 0 {
			t.Errorf("%s: typed %v, Enter in %v; want /exit pasted and NO Enter", c.name, b.pastes, b.enters)
		}
		want := "/exit was typed into Claude Code but Enter was withheld, because " + c.why
		if err != nil || r.Graceful || !strings.HasPrefix(r.Reason, want) || !slices.Equal(b.killed, []string{"@1"}) {
			t.Errorf("%s: %+v %v, closed %v; want closed at once, not graceful, %q", c.name, r, err, b.killed, want)
		}
	}
	cl, b := closeFixture(t, "gone")
	b.afterPaste = func(*closeBackend) {}
	if r, err, _ := runClose(cl, req1); err != nil || !r.Graceful || !slices.Equal(b.enters, []string{"%11"}) {
		t.Fatalf("control: %+v %v, Enter in %v; want the Enter pressed and a graceful end", r, err, b.enters)
	}
}

// TestAnOpenCodeWindowIsClosedAtOnce (🟡1): nothing is typed into an opencode
// pane (the paste guard refuses one, and opencode offers no exit call): its
// window is closed at once, not graceful, with that reason.
func TestAnOpenCodeWindowIsClosedAtOnce(t *testing.T) {
	cl, b := closeFixture(t, "gone")
	r, err, took := runClose(cl, mirror.AgentCloseReq{Session: "work", Window: "@2", Key: "opencode:" + ocSID2})
	if err != nil || r.Graceful || r.Reason != closeReasonOpenCode {
		t.Fatalf("%+v %v; want closed at once, not graceful", r, err)
	}
	if len(b.pastes) != 0 || !slices.Equal(b.killed, []string{"@2"}) || took > 300*time.Millisecond {
		t.Errorf("typed %v, closed %v after %s; want nothing typed and @2 closed at once", b.pastes, b.killed, took)
	}
}

// TestACloseIsOfTheConversationItNames (🟡3, 🟢2): a request whose key is not
// the conversation the window runs is refused (mismatch) with nothing typed
// or closed; and a window whose conversation changed during the grace is
// re-listed before the kill and NOT closed.
func TestACloseIsOfTheConversationItNames(t *testing.T) {
	cl, b := closeFixture(t, "gone")
	_, err, _ := runClose(cl, mirror.AgentCloseReq{Session: "work", Window: "@1", Key: ccKey4})
	var e *mirror.CloseRefusedError
	if !errors.As(err, &e) || e.Code != mirror.CloseMismatch || len(b.pastes) != 0 || len(b.killed) != 0 {
		t.Errorf("another conversation's key: %v, typed %v, closed %v; want refused mismatch, nothing done", err, b.pastes, b.killed)
	}
	cl, b = closeFixture(t, "")
	b.afterExit = func(b *closeBackend) { b.windows[0].Claude.Gen = ccKey4 }
	_, err, _ = runClose(cl, req1)
	if !errors.As(err, &e) || e.Code != mirror.CloseMismatch || len(b.killed) != 0 {
		t.Errorf("a conversation changed in the grace: %v, closed %v; want refused mismatch, not closed", err, b.killed)
	}
	// Positive control: the same fixture, its own key, ends it.
	cl, b = closeFixture(t, "gone")
	if r, err, _ := runClose(cl, req1); err != nil || !r.Graceful || len(b.pastes) != 1 {
		t.Fatalf("control: %+v %v, typed %v", r, err, b.pastes)
	}
}

// TestTheExitIsReadFromTheAgentsOwnPane (🟢1): once another pane of the
// window is active, the agent's exit is read from ITS process, not from the
// window's active pane (which runs no agent): while it runs, the window is
// closed only after the grace; once it is gone, at once.
func TestTheExitIsReadFromTheAgentsOwnPane(t *testing.T) {
	defer func(f func(int) bool) { agentProcAlive = f }(agentProcAlive)
	for _, alive := range []bool{true, false} {
		agentProcAlive = func(pid int) bool { return alive && pid == 101 }
		cl, b := closeFixture(t, "")
		// The person split the window and focused the new pane: the agent
		// runs on in %11, now one of the window's other panes.
		b.afterExit = func(b *closeBackend) {
			b.windows[0].Others = []ClaudePane{*b.windows[0].Claude}
			b.windows[0].Pane, b.windows[0].Claude, b.windows[0].Command = "%21", nil, "zsh"
		}
		r, err, took := runClose(cl, req1)
		if err != nil || r.Graceful == alive {
			t.Errorf("agent alive %t: %+v %v; want graceful %t", alive, r, err, !alive)
		}
		if alive && took < 600*time.Millisecond {
			t.Errorf("a running agent's window was closed after %s, before the grace", took)
		}
	}
}

// TestAnAgentCloseRefusesAnythingElse: a window not listed, a window of
// another session (the id is listed, the session is not its), a window with
// no agent, a request naming no conversation, a host that does not close
// windows: each is refused with its code, and NOTHING is typed or closed.
func TestAnAgentCloseRefusesAnythingElse(t *testing.T) {
	for _, c := range []struct {
		name string
		on   bool
		req  mirror.AgentCloseReq
		code string
	}{
		{"no such window", true, mirror.AgentCloseReq{Session: "work", Window: "@9", Key: ccKey1}, mirror.CloseNotListed},
		{"another session's window", true, mirror.AgentCloseReq{Session: "work", Window: "@4", Key: ccKey4}, mirror.CloseNotListed},
		{"not a window id", true, mirror.AgentCloseReq{Session: "work", Window: "work:1", Key: ccKey1}, mirror.CloseNotListed},
		{"a plain shell", true, mirror.AgentCloseReq{Session: "work", Window: "@3", Key: ccKey1}, mirror.CloseNoAgent},
		{"no conversation named", true, mirror.AgentCloseReq{Session: "work", Window: "@1"}, mirror.CloseMismatch},
		{"a host that does not close", false, req1, mirror.CloseUnavailable},
	} {
		cl, b := closeFixture(t, "gone")
		_, err := (&hostSide{c: cl, ctx: context.Background(), version: 2}).closeAgent(context.Background(), c.on, c.req)
		var e *mirror.CloseRefusedError
		if !errors.As(err, &e) || e.Code != c.code {
			t.Errorf("%s: %v; want refused %s", c.name, err, c.code)
		}
		if len(b.pastes) != 0 || len(b.killed) != 0 {
			t.Errorf("%s: typed %v and closed %v; want nothing", c.name, b.pastes, b.killed)
		}
	}
	// Positive control: the same fixture closes a listed agent window.
	cl, b := closeFixture(t, "gone")
	if _, err := (&hostSide{c: cl, ctx: context.Background(), version: 2}).closeAgent(context.Background(), true,
		mirror.AgentCloseReq{Session: "other", Window: "@4", Key: ccKey4}); err != nil || len(b.pastes) != 1 {
		t.Fatalf("control: %v, typed %v", err, b.pastes)
	}
}

// TestOnlyAnInteractiveHostThatCanAdvertisesAgentClose: the hello's
// AgentClose is an -interactive v2 host's whose backend lists, pastes and
// closes.
func TestOnlyAnInteractiveHostThatCanAdvertisesAgentClose(t *testing.T) {
	cl, _ := closeFixture(t, "gone")
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
