package mirrorhost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/askdrive"
	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/mirror"
)

// THE HOST'S HALF OF ANSWERING A PICKER (ask.go), over a real tunnel client
// against a hand-driven muster: what it advertises, which pane it types into,
// and how each kind of failure is named back.

// askingBackend is windowsBackend that can also type into a picker: it records
// each AnswerAsk (and the prior it was handed), and answers with keys and err.
type askingBackend struct {
	*windowsBackend
	mu     sync.Mutex
	calls  []string
	priors []*askq.Progress
	keys   int
	err    error
}

func (b *askingBackend) AnswerAsk(_ context.Context, pane string, qs []askq.Question, cs []askq.Choice, prior *askq.Progress) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, fmt.Sprintf("%s %s %v", pane, qs[0].Question, cs))
	b.priors = append(b.priors, prior)
	return b.keys, b.err
}

// answer1 is b's AnswerAsk with nothing typed before, its keys dropped.
func answer1(b AskBackend, ctx context.Context, pane string, qs []askq.Question, cs []askq.Choice) error {
	_, err := b.AnswerAsk(ctx, pane, qs, cs, nil)
	return err
}

func (b *askingBackend) snapshotCalls() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.calls...)
}

// TestTheHostContinuesWhatMusterSaysWasLeft: the answer's Prior reaches the
// backend's AnswerAsk, and the keys it reports go back with its refusal
// (Msg.Keys) — and not with a success, which leaves nothing to continue.
func TestTheHostContinuesWhatMusterSaysWasLeft(t *testing.T) {
	b := &askingBackend{windowsBackend: &windowsBackend{windows: threeWindows}}
	d := newDrivenMuster(t)
	d.run(t, wrapped{b}, nil, true)
	waitUntil(t, "the hello", func() bool { return len(d.of(mirror.TypeHello)) == 1 })
	prior := &askq.Progress{Choices: []askq.Choice{{Picks: []int{1}}}, Keys: 2}
	answer := func(id uint32) mirror.Msg {
		t.Helper()
		d.send(t, mirror.Msg{Type: mirror.TypeAnswer, ID: id, Session: "scratch2", Window: "@7",
			Ask: &mirror.AskTyping{Questions: hostAskQs, Choices: []askq.Choice{{Picks: []int{1}}}, Prior: prior}})
		var out mirror.Msg
		waitUntil(t, "the answered", func() bool {
			for _, m := range d.of(mirror.TypeAnswered) {
				if m.ID == id {
					out = m
					return true
				}
			}
			return false
		})
		return out
	}
	b.mu.Lock()
	b.keys, b.err = 5, fmt.Errorf("x: %w", askdrive.ErrLost)
	b.mu.Unlock()
	if a := answer(1); a.Refusal != mirror.AskRefusalLost || a.Keys != 5 {
		t.Fatalf("lost after 5 keys: answered %+v", a)
	}
	b.mu.Lock()
	b.keys, b.err = 7, nil
	got := append([]*askq.Progress(nil), b.priors...)
	b.mu.Unlock()
	if a := answer(2); a.Error != "" || a.Keys != 0 {
		t.Fatalf("a success: answered %+v", a)
	}
	if len(got) != 1 || !reflect.DeepEqual(got[0], prior) {
		t.Fatalf("the backend was handed %+v, want %+v", got, prior)
	}
}

// cancellingBackend is askingBackend that can dismiss a picker
// (AskCanceller): it records each CancelAsk — the pane, all it is handed —
// and answers with stays (the picker still showing) and cerr.
type cancellingBackend struct {
	*askingBackend
	cancels []string
	stays   bool
	cerr    error
}

func (b *cancellingBackend) CancelAsk(_ context.Context, pane string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cancels = append(b.cancels, pane)
	return !b.stays && b.cerr == nil, b.cerr
}

func (b *cancellingBackend) snapshotCancels() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.cancels...)
}

// stuckPicker is a backend whose CancelAsk is askdrive.Cancel on a pane that
// shows a real picker capture and never changes — a terminal that does not
// take the Escape.
type stuckPicker struct {
	*windowsBackend
	screen string
	sent   []string
}

func (s *stuckPicker) Capture(context.Context) (string, error) { return s.screen, nil }
func (s *stuckPicker) Send(_ context.Context, k string) error {
	s.sent = append(s.sent, k)
	return nil
}
func (s *stuckPicker) AnswerAsk(context.Context, string, []askq.Question, []askq.Choice, *askq.Progress) (int, error) {
	return 0, errors.New("not answering")
}
func (s *stuckPicker) CancelAsk(ctx context.Context, _ string) (bool, error) {
	return askdrive.Cancel(ctx, s, askdrive.Options{Settle: 100 * time.Millisecond, Poll: 10 * time.Millisecond})
}

// TestAnEscapeThatDidNotTakeIsSaid: through the real askdrive.Cancel, a picker
// still on screen after the one Escape comes back as a success with the
// still-showing note — not as "gone" — and one Escape is all that was sent.
func TestAnEscapeThatDidNotTakeIsSaid(t *testing.T) {
	b, err := os.ReadFile("../askdrive/testdata/three-q2.txt")
	if err != nil {
		t.Fatal(err)
	}
	sp := &stuckPicker{windowsBackend: &windowsBackend{windows: threeWindows}, screen: string(b)}
	d := newDrivenMuster(t)
	d.run(t, wrapped{sp}, nil, true)
	waitUntil(t, "the hello", func() bool { return len(d.of(mirror.TypeHello)) == 1 })
	d.send(t, mirror.Msg{Type: mirror.TypeCancelAsk, ID: 1, Session: "scratch2", Window: "@7"})
	waitUntil(t, "the ask-cancelled", func() bool { return len(d.of(mirror.TypeAskCancelled)) == 1 })
	if a := d.of(mirror.TypeAskCancelled)[0]; a.Error != "" || a.Text != askStillShowingNote {
		t.Fatalf("an Escape the terminal did not take: %+v, want success with %q", a, askStillShowingNote)
	}
	if !reflect.DeepEqual(sp.sent, []string{askdrive.KeyEscape}) {
		t.Fatalf("sent %q, want one Escape", sp.sent)
	}
}

// TestADismissalGoesToTheClaudePaneOnly: the host advertises AskCancel only
// when it answers too (interactive, a backend that can); a cancel-ask for the
// Claude window reaches the backend with ITS pane and the questions — the
// keys, text and choices a message might also carry reach nothing; one for a
// window that is not Claude Code is refused no-picker; a picker still showing
// after the Escape is a success with its note; each backend refusal comes back
// named; a read-only host dismisses nothing.
func TestADismissalGoesToTheClaudePaneOnly(t *testing.T) {
	for _, c := range []struct {
		name        string
		b           Backend
		interactive bool
		want        bool
	}{
		{"interactive, can dismiss", wrapped{&cancellingBackend{askingBackend: &askingBackend{windowsBackend: &windowsBackend{windows: threeWindows}}}}, true, true},
		{"read-only", wrapped{&cancellingBackend{askingBackend: &askingBackend{windowsBackend: &windowsBackend{windows: threeWindows}}}}, false, false},
		{"answers but cannot dismiss (older)", wrapped{&askingBackend{windowsBackend: &windowsBackend{windows: threeWindows}}}, true, false},
	} {
		d := newDrivenMuster(t)
		d.run(t, c.b, nil, c.interactive)
		waitUntil(t, "the hello", func() bool { return len(d.of(mirror.TypeHello)) == 1 })
		if got := d.of(mirror.TypeHello)[0].AskCancel; got != c.want {
			t.Errorf("%s: AskCancel %v, want %v", c.name, got, c.want)
		}
	}
	b := &cancellingBackend{askingBackend: &askingBackend{windowsBackend: &windowsBackend{windows: threeWindows}}}
	d := newDrivenMuster(t)
	d.run(t, wrapped{b}, nil, true)
	waitUntil(t, "the hello", func() bool { return len(d.of(mirror.TypeHello)) == 1 })
	dismiss := func(id uint32, window string) mirror.Msg {
		t.Helper()
		// Everything a message could carry besides the questions: none of it
		// is typed.
		d.send(t, mirror.Msg{Type: mirror.TypeCancelAsk, ID: id, Session: "scratch2", Window: window, Text: "rm -rf ~\r",
			Ask: &mirror.AskTyping{Questions: hostAskQs, Choices: []askq.Choice{{Other: "typed"}}}})
		var out mirror.Msg
		waitUntil(t, "the ask-cancelled", func() bool {
			for _, m := range d.of(mirror.TypeAskCancelled) {
				if m.ID == id {
					out = m
					return true
				}
			}
			return false
		})
		return out
	}
	if a := dismiss(1, "@7"); a.Error != "" || a.Text != "" {
		t.Fatalf("the Claude window's dismissal: %+v", a)
	}
	if got := b.snapshotCancels(); !reflect.DeepEqual(got, []string{"%21"}) {
		t.Fatalf("dismissed %q, want the Claude pane %%21", got)
	}
	if calls := b.snapshotCalls(); len(calls) != 0 {
		t.Fatalf("a dismissal typed an answer: %q", calls)
	}
	if a := dismiss(2, "@4"); a.Refusal != mirror.AskRefusalNoPicker || len(b.snapshotCancels()) != 1 {
		t.Fatalf("a shell window: %+v", a)
	}
	b.mu.Lock()
	b.stays = true
	b.mu.Unlock()
	if a := dismiss(3, "@7"); a.Error != "" || a.Refusal != "" || a.Text != askStillShowingNote {
		t.Fatalf("a picker still showing after the Escape: %+v, want success with its note", a)
	}
	b.mu.Lock()
	b.stays = false
	b.mu.Unlock()
	for i, c := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("x: %w", askdrive.ErrNoPicker), mirror.AskRefusalNoPicker},
		{fmt.Errorf("x: %w", askdrive.ErrUnrecognised), mirror.AskRefusalUnrecognised},
		{ErrPaneBusy, mirror.AskRefusalBusy},
		{ErrNoRawPaste, mirror.AskRefusalUnsupported},
		{errors.New("tmux broke"), ""},
	} {
		b.mu.Lock()
		b.cerr = c.err
		b.mu.Unlock()
		if a := dismiss(uint32(10+i), "@7"); a.Error == "" || a.Refusal != c.want {
			t.Errorf("backend error %v: %+v, want refusal %q", c.err, a, c.want)
		}
	}
	// A read-only host refuses, whatever muster sends.
	rb := &cancellingBackend{askingBackend: &askingBackend{windowsBackend: &windowsBackend{windows: threeWindows}}}
	ro := newDrivenMuster(t)
	ro.run(t, wrapped{rb}, nil, false)
	waitUntil(t, "the hello", func() bool { return len(ro.of(mirror.TypeHello)) == 1 })
	ro.send(t, mirror.Msg{Type: mirror.TypeCancelAsk, ID: 1, Session: "scratch2", Window: "@7"})
	waitUntil(t, "the refusal", func() bool { return len(ro.of(mirror.TypeAskCancelled)) == 1 })
	if a := ro.of(mirror.TypeAskCancelled)[0]; a.Refusal != mirror.AskRefusalUnsupported || len(rb.snapshotCancels()) != 0 {
		t.Fatalf("a read-only host: %+v, dismissed %q", a, rb.snapshotCancels())
	}
}

// rawlessBackend is askingBackend whose tmux pastes raw keys as paste says
// (Tmux.KeyPaste).
type rawlessBackend struct {
	*askingBackend
	paste KeyPaste
}

func (b *rawlessBackend) KeyPaste(context.Context) KeyPaste { return b.paste }

var hostAskQs = []askq.Question{{Question: "Which color?", Options: []askq.Option{{Label: "Red"}, {Label: "Blue"}}}}

func TestTheHostAdvertisesAnsweringOnlyWhenItCan(t *testing.T) {
	for _, c := range []struct {
		name        string
		b           Backend
		interactive bool
		want        bool
	}{
		{"interactive, can type", wrapped{&askingBackend{windowsBackend: &windowsBackend{windows: threeWindows}}}, true, true},
		{"read-only", wrapped{&askingBackend{windowsBackend: &windowsBackend{windows: threeWindows}}}, false, false},
		{"a backend that cannot type", wrapped{&windowsBackend{windows: threeWindows}}, true, false},
		{"a tmux that cannot paste raw (server rejects -S yet reports 3.7+)", wrapped{&rawlessBackend{askingBackend: &askingBackend{windowsBackend: &windowsBackend{windows: threeWindows}}, paste: KeyPasteNone}}, true, false},
		{"a tmux with paste-buffer -S", wrapped{&rawlessBackend{askingBackend: &askingBackend{windowsBackend: &windowsBackend{windows: threeWindows}}, paste: KeyPasteRawFlag}}, true, true},
		{"a tmux before 3.7 (plain paste-buffer is raw)", wrapped{&rawlessBackend{askingBackend: &askingBackend{windowsBackend: &windowsBackend{windows: threeWindows}}, paste: KeyPastePlain}}, true, true},
		{"a tmux not known yet (no server)", wrapped{&rawlessBackend{askingBackend: &askingBackend{windowsBackend: &windowsBackend{windows: threeWindows}}}}, true, true},
	} {
		d := newDrivenMuster(t)
		d.run(t, c.b, nil, c.interactive)
		waitUntil(t, "the hello", func() bool { return len(d.of(mirror.TypeHello)) == 1 })
		if got := d.of(mirror.TypeHello)[0].AskAnswer; got != c.want {
			t.Errorf("%s: AskAnswer %v, want %v", c.name, got, c.want)
		}
	}
}

// TestAnAnswerIsTypedIntoTheClaudePaneOnly: an answer for the Claude window is
// typed into ITS pane; one for a window whose pane is not Claude Code is
// refused as "no picker" without typing; the driver's own refusals come back
// named; a read-only host types nothing.
func TestAnAnswerIsTypedIntoTheClaudePaneOnly(t *testing.T) {
	b := &askingBackend{windowsBackend: &windowsBackend{windows: threeWindows}}
	d := newDrivenMuster(t)
	d.run(t, wrapped{b}, nil, true)
	waitUntil(t, "the hello", func() bool { return len(d.of(mirror.TypeHello)) == 1 })
	answer := func(id uint32, window string) mirror.Msg {
		t.Helper()
		d.send(t, mirror.Msg{Type: mirror.TypeAnswer, ID: id, Session: "scratch2", Window: window,
			Ask: &mirror.AskTyping{Questions: hostAskQs, Choices: []askq.Choice{{Picks: []int{1}}}}})
		var out mirror.Msg
		waitUntil(t, "the answered", func() bool {
			for _, m := range d.of(mirror.TypeAnswered) {
				if m.ID == id {
					out = m
					return true
				}
			}
			return false
		})
		return out
	}
	if a := answer(1, "@7"); a.Error != "" {
		t.Fatalf("the Claude window's answer was refused: %s", a.Error)
	}
	if got := b.snapshotCalls(); !reflect.DeepEqual(got, []string{"%21 Which color? [{[1] }]"}) {
		t.Fatalf("typed %q, want the Claude pane %%21", got)
	}
	if a := answer(2, "@4"); a.Refusal != mirror.AskRefusalNoPicker {
		t.Fatalf("a shell window: %+v, want refused no-picker", a)
	}
	if got := b.snapshotCalls(); len(got) != 1 {
		t.Fatalf("a refused answer was typed: %q", got)
	}
	for i, c := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("x: %w", askdrive.ErrNoPicker), mirror.AskRefusalNoPicker},
		{fmt.Errorf("x: %w", askdrive.ErrNotUntouched), mirror.AskRefusalTouched},
		{fmt.Errorf("x: %w", askdrive.ErrUnreadable), mirror.AskRefusalUnreadable},
		{fmt.Errorf("%w: %w", askdrive.ErrLost, askdrive.ErrUnreadable), mirror.AskRefusalUnreadableTyped},
		{fmt.Errorf("x: %w", askdrive.ErrLost), mirror.AskRefusalLost},
		{fmt.Errorf("x: %w", askdrive.ErrUnrecognised), mirror.AskRefusalUnrecognised},
		{ErrPaneBusy, mirror.AskRefusalBusy},
		{ErrNoRawPaste, mirror.AskRefusalUnsupported},
		{errors.New("tmux broke"), ""},
	} {
		b.mu.Lock()
		b.err = c.err
		b.mu.Unlock()
		a := answer(uint32(10+i), "@7")
		if a.Error == "" || a.Refusal != c.want {
			t.Errorf("driver error %v: answered %+v, want refusal %q", c.err, a, c.want)
		}
	}
	// A read-only host refuses whatever muster sends.
	rb := &askingBackend{windowsBackend: &windowsBackend{windows: threeWindows}}
	ro := newDrivenMuster(t)
	ro.run(t, wrapped{rb}, nil, false)
	waitUntil(t, "the hello", func() bool { return len(ro.of(mirror.TypeHello)) == 1 })
	ro.send(t, mirror.Msg{Type: mirror.TypeAnswer, ID: 1, Session: "scratch2", Window: "@7",
		Ask: &mirror.AskTyping{Questions: hostAskQs, Choices: []askq.Choice{{Picks: []int{1}}}}})
	waitUntil(t, "the refusal", func() bool { return len(ro.of(mirror.TypeAnswered)) == 1 })
	if a := ro.of(mirror.TypeAnswered)[0]; a.Refusal != mirror.AskRefusalUnsupported || len(rb.snapshotCalls()) != 0 {
		t.Fatalf("a read-only host: %+v, typed %q", a, rb.snapshotCalls())
	}
}

// TestOneDrivePerPane: while a drive has a pane, another answer for it is
// refused before tmux is touched (the Tmux here has no server at all); a
// drive of another pane, or of the same pane once released, is not.
func TestOneDrivePerPane(t *testing.T) {
	tm := Tmux{Bin: "/nonexistent/tmux", Socket: "one-drive"}
	release, ok := lockPane(strings.Join(append(tm.server(), "%5"), " "))
	if !ok {
		t.Fatal("the first drive could not take the pane")
	}
	err := answer1(tm, context.Background(), "%5", hostAskQs, []askq.Choice{{Picks: []int{0}}})
	if !errors.Is(err, ErrPaneBusy) {
		t.Fatalf("a second drive of a busy pane: %v", err)
	}
	if err := answer1(tm, context.Background(), "%6", hostAskQs, []askq.Choice{{Picks: []int{0}}}); errors.Is(err, ErrPaneBusy) {
		t.Fatalf("another pane: %v", err)
	}
	release()
	if err := answer1(tm, context.Background(), "%5", hostAskQs, []askq.Choice{{Picks: []int{0}}}); errors.Is(err, ErrPaneBusy) {
		t.Fatalf("the pane once released: %v", err)
	}
}

// fakeTmux is a shell-script tmux that answers the paste-buffer -S probe
// with probe, `display-message -p #{version}` with server (exit 1 when it is
// ""), and `-V` with version; every other command succeeds silently. Each
// call's argv lands in the returned log, one line per call.
func fakeTmux(t *testing.T, name, probe, server, version string) (Tmux, string) {
	t.Helper()
	dir := t.TempDir()
	p, log := filepath.Join(dir, name), filepath.Join(dir, "calls")
	display := "echo '" + server + "'"
	if server == "" {
		display = "echo 'no current client' >&2; exit 1"
	}
	script := "#!/bin/sh\necho \"$*\" >> '" + log + "'\ncase \"$*\" in\n" +
		"*-V*) echo '" + version + "' ;;\n" +
		"*display-message*) " + display + " ;;\n" +
		"*'paste-buffer -S -b muster-mirror-ask-probe-'*) echo '" + probe + "' >&2; exit 1 ;;\n" +
		"*load-buffer*) cat > /dev/null ;;\n" +
		"esac\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return Tmux{Bin: p}, log
}

// TestTheKeyPasteProbeReadsTmuxsAnswer: KeyPaste is -S for a tmux that
// refuses the probe as "no buffer" (it knows -S); for one that refuses the
// FLAG it is the plain paste when the server's #{version} confirms it is
// older than 3.7 (whose paste never sanitized), none when it claims 3.7+, and
// unknown when that read fails;
// with NO SERVER running it reads `tmux -V` instead — and a version it cannot
// read, or any other failure, is unknown, never "old". A fake tmux plays each.
func TestTheKeyPasteProbeReadsTmuxsAnswer(t *testing.T) {
	noServer := "no server running on /tmp/tmux-1000/default"
	unknownFlag := "command paste-buffer: unknown flag -S"
	for _, c := range []struct {
		name, probe, server, version string
		want                         KeyPaste
	}{
		{"new", "no buffer muster-mirror-ask-probe-x", "3.7c", "tmux 3.7c", KeyPasteRawFlag},
		{"old 3.4", unknownFlag, "3.4", "tmux 3.4", KeyPastePlain},
		{"old 3.2a", unknownFlag, "3.2a", "tmux 3.2a", KeyPastePlain},
		{"old 3.6b", unknownFlag, "3.6b", "tmux 3.6b", KeyPastePlain},
		// The server parses -S, so these contradict themselves: refused.
		{"rejects -S, reports 3.7c", unknownFlag, "3.7c", "tmux 3.7c", KeyPasteNone},
		{"rejects -S, reports next-3.8", unknownFlag, "next-3.8", "tmux 3.4", KeyPasteNone},
		// The version read failed (a timeout, say) or says nothing readable:
		// unknown — retried by the next answer, never "cannot".
		{"rejects -S, version read fails", unknownFlag, "", "tmux 3.4", KeyPasteUnknown},
		{"rejects -S, unreadable version", unknownFlag, "master", "tmux 3.4", KeyPasteUnknown},
		{"no server, 3.7c", noServer, "", "tmux 3.7c", KeyPasteRawFlag},
		{"no server, next-3.8", noServer, "", "tmux next-3.8", KeyPasteRawFlag},
		{"no server, 4.0", noServer, "", "tmux 4.0", KeyPasteRawFlag},
		{"no server, 3.6b", noServer, "", "tmux 3.6b", KeyPastePlain},
		{"no server, 3.4", noServer, "", "tmux 3.4", KeyPastePlain},
		{"no server, unreadable", noServer, "", "tmux master", KeyPasteUnknown},
		{"no socket yet, 3.4", "error connecting to /tmp/tmux-1000/x (No such file or directory)", "", "tmux 3.4", KeyPastePlain},
		// A 3.4 client against a 3.7c server (measured): every command fails
		// so. Not "no server": the binary's own version says nothing.
		{"a server this client cannot talk to", "server exited unexpectedly", "", "tmux 3.4", KeyPasteUnknown},
	} {
		tm, _ := fakeTmux(t, strings.ReplaceAll(c.name, " ", "_"), c.probe, c.server, c.version)
		if got := tm.KeyPaste(context.Background()); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	// A tmux that cannot paste raw is refused before any key.
	mixed, log := fakeTmux(t, "mixed", unknownFlag, "3.7c", "tmux 3.7c")
	if err := answer1(mixed, context.Background(), "%1", hostAskQs, []askq.Choice{{Picks: []int{0}}}); !errors.Is(err, ErrNoRawPaste) || !errors.Is(err, errCannotAnswer) {
		t.Fatalf("a server rejecting -S yet reporting 3.7c: %v", err)
	}
	if calls, _ := os.ReadFile(log); strings.Contains(string(calls), "capture-pane") || strings.Contains(string(calls), "load-buffer") {
		t.Fatalf("a refused answer touched the pane:\n%s", calls)
	}
	// An old tmux is NOT refused: the answer goes on to read the pane.
	old, log := fakeTmux(t, "old", unknownFlag, "3.4", "tmux 3.4")
	if err := answer1(old, context.Background(), "%1", hostAskQs, []askq.Choice{{Picks: []int{0}}}); errors.Is(err, ErrNoRawPaste) || errors.Is(err, errCannotAnswer) {
		t.Fatalf("a tmux 3.4 was refused: %v", err)
	}
	if calls, _ := os.ReadFile(log); !strings.Contains(string(calls), "capture-pane") {
		t.Fatalf("an answer on tmux 3.4 never read the pane:\n%s", calls)
	}
}

// TestSendPastesWithOrWithoutDashS: a key goes in with `paste-buffer -S` on
// a tmux that has it (and when not known yet), and with plain `paste-buffer`
// — never -S, which that tmux would refuse — on a tmux before 3.7. The bytes
// loaded are the keys exactly, whichever.
func TestSendPastesWithOrWithoutDashS(t *testing.T) {
	keys := "\x1b[B\x1b[A2\r" // Down, Up, a digit, Enter
	for _, c := range []struct {
		paste KeyPaste
		want  string
	}{
		{KeyPasteRawFlag, "paste-buffer -S -d -b muster-mirror-ask-"},
		{KeyPasteUnknown, "paste-buffer -S -d -b muster-mirror-ask-"},
		{KeyPastePlain, "paste-buffer -d -b muster-mirror-ask-"},
	} {
		tm, log := fakeTmux(t, "send", "", "", "")
		dir := filepath.Dir(log)
		// Record what load-buffer is handed on stdin.
		script := "#!/bin/sh\necho \"$*\" >> '" + log + "'\ncase \"$*\" in\n*load-buffer*) cat > '" + filepath.Join(dir, "loaded") + "' ;;\nesac\n"
		if err := os.WriteFile(tm.Bin, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := (tmuxPane{t: tm, id: "%3", paste: c.paste}).Send(context.Background(), keys); err != nil {
			t.Fatalf("%v: %v", c.paste, err)
		}
		calls, _ := os.ReadFile(log)
		var paste string
		for _, l := range strings.Split(string(calls), "\n") {
			if strings.HasPrefix(l, "paste-buffer") {
				paste = l
			}
		}
		if !strings.HasPrefix(paste, c.want) || !strings.HasSuffix(paste, " -t %3") {
			t.Errorf("%v: pasted with %q, want %q… -t %%3", c.paste, paste, c.want)
		}
		if loaded, _ := os.ReadFile(filepath.Join(dir, "loaded")); string(loaded) != keys {
			t.Errorf("%v: loaded %q, want the keys %q", c.paste, loaded, keys)
		}
	}
}
