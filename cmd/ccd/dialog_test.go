package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// askdriveFixture is a capture of the real CLI's pane from internal/askdrive's
// testdata (Claude Code 2.1.289).
func askdriveFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "askdrive", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// What dialogOnScreen reads, against captures of the real CLI.
func TestDialogOnScreenReadsTheRealCLIsScreens(t *testing.T) {
	for _, name := range []string{
		"not-picker-permission.txt", // the permission prompt
		"single-q.txt", "multi-q.txt", "three-q1.txt", "three-q1-other.txt", "three-q2-next.txt",
		"three-review.txt", "multi-review.txt", "long-q2-clipped-80x20.txt", // the AskUserQuestion picker
	} {
		if what, shown := dialogOnScreen(askdriveFixture(t, name)); !shown {
			t.Errorf("%s: no dialog read; want one", name)
		} else {
			t.Logf("%s: %s", name, what)
		}
	}
	for _, name := range []string{
		"multi-done.txt", "single-done.txt", "three-done.txt", // the CLI at its prompt after a turn
		"not-picker-prose.txt", // prose quoting the picker's and prompt's words
	} {
		if what, shown := dialogOnScreen(askdriveFixture(t, name)); shown {
			t.Errorf("%s: read as %s; want no dialog", name, what)
		}
	}
	// The PINNED CLI (2.1.296), captured in the image: at its prompt on a fresh
	// start, and at a Bash permission prompt (a turn against a stub model API).
	if what, shown := dialogOnScreen(string(fixture(t, "pane_idle_2.1.296.txt"))); shown {
		t.Errorf("pane_idle_2.1.296.txt: read as %s; want no dialog", what)
	}
	if _, shown := dialogOnScreen(string(fixture(t, "pane_permission_2.1.296.txt"))); !shown {
		t.Error("pane_permission_2.1.296.txt: no dialog read; want the permission prompt")
	}
}

// Each of the permission prompt's two marks is enough alone, and neither counts
// above the screen's last rule — where the input box is, holding whatever an
// operator (or ccd's own paste) put there.
func TestDialogOnScreenEachMarkAloneAndOnlyBelowTheLastRule(t *testing.T) {
	rule := strings.Repeat("─", 80)
	perm := askdriveFixture(t, "not-picker-permission.txt")
	noFooter := strings.Replace(perm, "Esc to cancel", "Something else", 1)
	if _, shown := dialogOnScreen(noFooter); !shown {
		t.Error("the select cursor alone (no footer) was not read as a dialog")
	}
	noCursor := strings.Replace(perm, "❯ 1. Yes", "  1. Yes", 1)
	if _, shown := dialogOnScreen(noCursor); !shown {
		t.Error("the footer alone (no cursor) was not read as a dialog")
	}
	if _, shown := dialogOnScreen(strings.Replace(noCursor, "Esc to cancel", "Something else", 1)); shown {
		t.Error("control: with neither mark the permission prompt still read as a dialog")
	}
	// The cursor with a no-break space after it.
	if _, shown := dialogOnScreen(strings.Replace(noFooter, "❯ 1.", "❯ 1.", 1)); !shown {
		t.Error("the select cursor followed by a no-break space was not read")
	}
	// The same lines inside the input box (above the last rule) are not a dialog.
	box := "earlier output\n" + rule + "\n❯ 1. Yes\n  Esc to cancel · Tab to amend\n" + rule + "\n  ? for shortcuts\n"
	if what, shown := dialogOnScreen(box); shown {
		t.Errorf("text in the input box read as %s", what)
	}
	// Control: the same lines with no rule below them are.
	if _, shown := dialogOnScreen("earlier output\n" + rule + "\n❯ 1. Yes\n  Esc to cancel · Tab to amend\n"); !shown {
		t.Error("control: the lines below the last rule were not read as a dialog")
	}
}

// fakeTmuxScreen is fakeTmux (afterpaste_test.go) whose pane is live and whose
// capture-pane prints the file `screen`.
const fakeTmuxScreen = `#!/bin/sh
dir="$(dirname "$0")"
echo "$*" >> "$dir/calls"
case "$*" in
  *display-message*) echo "4242 1791600000 %1 0  " ;;
  *capture-pane*) cat "$dir/screen" ;;
  *load-buffer*) cat >/dev/null ;;
esac
exit 0
`

func fakeTermScreen(t *testing.T, screen string) (tmuxTerminal, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(fakeTmuxScreen), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "screen"), []byte(screen), 0o644); err != nil {
		t.Fatal(err)
	}
	return tmuxTerminal{bin: filepath.Join(dir, "tmux")}, dir
}

// 🔴 NOTHING IS TYPED INTO A PANE SHOWING A DIALOG: not the paste, and not an
// Enter (runTurn's submit retry, or the paste's own). The control: the same
// terminal with the CLI at its prompt pastes and presses Enter.
func TestTheTerminalTypesNothingIntoADialog(t *testing.T) {
	ref := paneRef{Server: "4242 1791600000", Pane: "%1"}
	term, dir := fakeTermScreen(t, askdriveFixture(t, "not-picker-permission.txt"))
	err := term.Paste(context.Background(), ref, "hello")
	if !errors.Is(err, errDialogShown) || errors.Is(err, errPaneNotLive) {
		t.Fatalf("Paste into a dialog: %v; want errDialogShown", err)
	}
	if err := term.Enter(context.Background(), ref); !errors.Is(err, errDialogShown) {
		t.Fatalf("Enter into a dialog: %v; want errDialogShown", err)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if !strings.Contains(string(calls), "capture-pane") {
		t.Fatalf("instrument check: the screen was never read:\n%s", calls)
	}
	if strings.Contains(string(calls), "paste-buffer") || strings.Contains(string(calls), "send-keys") {
		t.Fatalf("keys reached a pane showing a dialog:\n%s", calls)
	}

	term, dir = fakeTermScreen(t, askdriveFixture(t, "multi-done.txt"))
	if err := term.Paste(context.Background(), ref, "hello"); err != nil {
		t.Fatalf("control: Paste at the prompt: %v", err)
	}
	calls, _ = os.ReadFile(filepath.Join(dir, "calls"))
	if !strings.Contains(string(calls), "paste-buffer") || !strings.Contains(string(calls), "send-keys") {
		t.Fatalf("control: the prompt was not pasted and submitted:\n%s", calls)
	}
}

// A screen that cannot be read is refused before anything is typed, as
// not_ready (nothing was pasted, so the caller may send again).
func TestAnUnreadableScreenIsRefusedBeforeTyping(t *testing.T) {
	dir := t.TempDir()
	script := strings.Replace(fakeTmuxScreen, `*capture-pane*) cat "$dir/screen" ;;`,
		`*capture-pane*) echo "no server" >&2; exit 1 ;;`, 1)
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	term := tmuxTerminal{bin: filepath.Join(dir, "tmux")}
	err := term.Paste(context.Background(), paneRef{Server: "4242 1791600000", Pane: "%1"}, "hello")
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if !errors.Is(err, errPaneNotLive) || !strings.Contains(err.Error(), "no server") {
		t.Fatalf("Paste with an unreadable screen: %v; want errPaneNotLive naming the failure", err)
	}
	if strings.Contains(string(calls), "paste-buffer") {
		t.Fatalf("pasted with an unreadable screen:\n%s", calls)
	}
}

// dialogTerm is a terminal whose pane shows a dialog: tmuxTerminal's refusal.
type dialogTerm struct{ pasted bool }

func (d *dialogTerm) Paste(context.Context, paneRef, string) error {
	return errors.Join(errDialogShown, errors.New("a permission prompt"))
}
func (d *dialogTerm) Enter(context.Context, paneRef) error { return errDialogShown }
func (d *dialogTerm) Alive(context.Context, paneRef) error { return nil }

// The terminal's dialog refusal is the turn's 409 busy, code awaiting_approval
// — the refusal muster reads as "nothing was pasted".
func TestADialogOnScreenIsABusyAwaitingApprovalRefusal(t *testing.T) {
	srv := newServer(serverConfig{Bearer: "b", ConfigDir: t.TempDir()}, &dialogTerm{}, newAuthTracker())
	srv.inputPane = func() (paneRef, bool) { return fakeRef, true }
	srv.onHook(hookEvent{Event: "SessionStart", SessionID: "s"})
	_, f := srv.runTurn(context.Background(), "hello")
	if f == nil || f.Status != http.StatusConflict || f.Type != failBusy || f.Code != codeAwaitingApproval {
		t.Fatalf("runTurn with a dialog on screen: %+v; want 409 busy code %s", f, codeAwaitingApproval)
	}
}
