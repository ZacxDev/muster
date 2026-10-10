package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAFailureAfterThePasteIsNeverNotReady: muster re-sends a `503 not_ready`
// because it promises nothing was pasted. A pane that dies between the paste and
// its Enter must therefore NOT surface as errPaneNotLive (which the server maps
// to not_ready), or the re-send pastes the prompt twice.
func TestAFailureAfterThePasteIsNeverNotReady(t *testing.T) {
	enterErr := fmt.Errorf("%w: pane %%3 is dead", errPaneNotLive)
	got := afterPaste(enterErr)
	if errors.Is(got, errPaneNotLive) {
		t.Fatalf("a post-paste failure still reads as errPaneNotLive (-> not_ready): %v", got)
	}
	if !errors.Is(got, errPastedNotSubmitted) {
		t.Fatalf("a post-paste failure is not marked as pasted-not-submitted: %v", got)
	}
	if afterPaste(nil) != nil {
		t.Fatal("a successful Enter became an error")
	}
	// Control: the pre-paste check's own failure IS errPaneNotLive.
	if !errors.Is(enterErr, errPaneNotLive) {
		t.Fatal("control: the fixture is not errPaneNotLive")
	}
}

// fakeTmux is a tmux stand-in: display-message answers "live" on its first call
// and "dead" after (dead from the start with a `deadfirst` file; EXITS 1 from the
// second call with a `failsecond` file), the buffer commands succeed, and every
// invocation is logged.
const fakeTmux = `#!/bin/sh
dir="$(dirname "$0")"
echo "$*" >> "$dir/calls"
case "$*" in
  *display-message*)
    n=$(cat "$dir/n" 2>/dev/null || echo 0); echo $((n+1)) > "$dir/n"
    if [ "$n" = 0 ] && [ ! -e "$dir/deadfirst" ]; then echo "4242 1791600000 %1 0  "
    elif [ -e "$dir/failsecond" ]; then echo "server exited unexpectedly" >&2; exit 1
    else echo "4242 1791600000 %1 1 3 "; fi ;;
  *load-buffer*) cat >/dev/null ;;
esac
exit 0
`

func fakeTerm(t *testing.T, deadFirst bool) (tmuxTerminal, string) {
	return fakeTermMode(t, deadFirst, false)
}

// fakeTermMode: failSecond makes the second display-message EXIT NON-ZERO — a
// transient tmux failure on a pane that may well still be alive.
func fakeTermMode(t *testing.T, deadFirst, failSecond bool) (tmuxTerminal, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "tmux")
	if err := os.WriteFile(bin, []byte(fakeTmux), 0o755); err != nil {
		t.Fatal(err)
	}
	if deadFirst {
		if err := os.WriteFile(filepath.Join(dir, "deadfirst"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if failSecond {
		if err := os.WriteFile(filepath.Join(dir, "failsecond"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return tmuxTerminal{bin: bin}, dir
}

// TestPasteReportsADeathAfterThePasteAsPastedNotSubmitted drives the real
// tmuxTerminal.Paste (against a fake tmux): the pane is live for the pre-paste
// check and gone for the Enter's. The error must not be errPaneNotLive (the
// server's 503 not_ready, which muster re-sends), and no Enter is sent. The
// control: dead before the paste IS errPaneNotLive, and nothing is pasted.
func TestPasteReportsADeathAfterThePasteAsPastedNotSubmitted(t *testing.T) {
	ref := paneRef{Server: "4242 1791600000", Pane: "%1"}

	term, dir := fakeTerm(t, false)
	err := term.Paste(context.Background(), ref, "hello")
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if !strings.Contains(string(calls), "paste-buffer") {
		t.Fatalf("instrument check: nothing was pasted:\n%s", calls)
	}
	if err == nil || errors.Is(err, errPaneNotLive) || !errors.Is(err, errPastedNotSubmitted) {
		t.Fatalf("death after the paste returned %v; want errPastedNotSubmitted and NOT errPaneNotLive", err)
	}
	if strings.Contains(string(calls), "send-keys") {
		t.Fatalf("an Enter was sent to a dead pane:\n%s", calls)
	}

	term, dir = fakeTerm(t, true)
	err = term.Paste(context.Background(), ref, "hello")
	calls, _ = os.ReadFile(filepath.Join(dir, "calls"))
	if !errors.Is(err, errPaneNotLive) || strings.Contains(string(calls), "paste-buffer") {
		t.Fatalf("control: dead before the paste returned %v (pasted: %v)", err, strings.Contains(string(calls), "paste-buffer"))
	}
}

// TestATransientTmuxFailureAfterThePasteIsNotNotReady is the case afterPaste
// exists for: the post-paste liveness read FAILS (tmux exits non-zero) rather
// than reporting a dead pane. state wraps that as errPaneNotLive, and the pane
// — with the pasted text in its input box — may still be alive, so a not_ready
// here would make muster paste the prompt a second time.
func TestATransientTmuxFailureAfterThePasteIsNotNotReady(t *testing.T) {
	term, dir := fakeTermMode(t, false, true)
	err := term.Paste(context.Background(), paneRef{Server: "4242 1791600000", Pane: "%1"}, "hello")
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if !strings.Contains(string(calls), "paste-buffer") {
		t.Fatalf("instrument check: nothing was pasted:\n%s", calls)
	}
	// Instrument check: the failing read must be what produced this error —
	// otherwise the script fell through to "dead" and the dead-pane test is all
	// this one would be repeating.
	if err == nil || !strings.Contains(err.Error(), "server exited unexpectedly") {
		t.Fatalf("instrument check: the non-zero display-message was not reached: %v", err)
	}
	if errors.Is(err, errPaneNotLive) || !errors.Is(err, errPastedNotSubmitted) {
		t.Fatalf("a transient tmux failure after the paste returned %v; want errPastedNotSubmitted, NOT errPaneNotLive", err)
	}
	if strings.Contains(string(calls), "send-keys") {
		t.Fatalf("an Enter was sent after a failed liveness read:\n%s", calls)
	}
}
