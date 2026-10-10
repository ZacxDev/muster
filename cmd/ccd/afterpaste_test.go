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
// and "dead" after (or dead from the start when deadFirst), the buffer commands
// succeed, and every invocation is logged.
const fakeTmux = `#!/usr/bin/env bash
dir="$(dirname "$0")"
echo "$*" >> "$dir/calls"
case "$*" in
  *display-message*)
    n=$(cat "$dir/n" 2>/dev/null || echo 0); echo $((n+1)) > "$dir/n"
    if [ "$n" = 0 ] && [ ! -e "$dir/deadfirst" ]; then echo "4242 1791600000 %1 0  "; else echo "4242 1791600000 %1 1 3 "; fi ;;
  *load-buffer*) cat >/dev/null ;;
esac
exit 0
`

func fakeTerm(t *testing.T, deadFirst bool) (tmuxTerminal, string) {
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
