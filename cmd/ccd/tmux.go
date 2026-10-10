package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// tmuxTerminal delivers prompts into the interactive session as ONE bracketed
// paste: `load-buffer` from stdin into a uniquely named buffer, `paste-buffer -p -d`
// into the pane, then Enter.
//
// Measured with the pinned CLI: "para one\n\npara two with \"quotes\" and $HOME"
// arrived in the transcript byte-for-byte this way (the screen collapsed the
// blank line; the transcript did not).
//
// ⚠ WHY A PASTE AND NOT `send-keys -l`, STATED AT ITS REAL STRENGTH: on the pinned
// CLI a TYPED LF also became a newline rather than a submit (measured), so for
// plain multi-line text the two are equivalent and the mutation battery records
// "type it instead" as an equivalent mutant, not a caught one. What the paste buys
// is that the text travels on stdin, NEVER argv — a prompt in argv is visible in
// the process table, bounded by ARG_MAX, and parsed by tmux — and that it is the
// path the fidelity measurement above exercised. The paste does NOT stop the TUI
// reading a leading `!` as shell mode; neutralizeInputMode disarms that (a leading
// `/` is let through on purpose — see there).
//
// ⚠ NOT EVERY BYTE IS PRESERVED, AND THE EXCEPTIONS ARE THE CLI'S, NOT tmux's: a
// TAB became four spaces in the same measurement. A CRLF is folded to LF by the
// caller before it gets here (see handleResponses).
type tmuxTerminal struct {
	bin    string
	socket string // -L name; "" = the default server
	conf   string // -f file for a server this process starts; "" = tmux's default
	target string // the session (and pane) to drive
	// enterDelay separates the paste from the Enter, so the TUI has ingested the
	// paste before the submit key arrives.
	enterDelay time.Duration
}

func (t tmuxTerminal) run(ctx context.Context, stdin []byte, args ...string) error {
	_, err := t.exec(ctx, stdin, args...)
	return err
}

func (t tmuxTerminal) output(ctx context.Context, args ...string) (string, error) {
	return t.exec(ctx, nil, args...)
}

func (t tmuxTerminal) exec(ctx context.Context, stdin []byte, args ...string) (string, error) {
	var full []string
	if t.socket != "" {
		full = append(full, "-L", t.socket)
	}
	if t.conf != "" {
		full = append(full, "-f", t.conf)
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, t.bin, full...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var outb, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &outb, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("tmux %s: %v: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return outb.String(), nil
}

func (t tmuxTerminal) Paste(ctx context.Context, text string) error {
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	buf := "ccd-" + hex.EncodeToString(rnd[:])
	if err := t.run(ctx, []byte(text), "load-buffer", "-b", buf, "-"); err != nil {
		return err
	}
	if err := t.run(ctx, nil, "paste-buffer", "-p", "-d", "-b", buf, "-t", t.target); err != nil {
		_ = t.run(context.Background(), nil, "delete-buffer", "-b", buf)
		return err
	}
	select {
	case <-time.After(t.enterDelay):
	case <-ctx.Done():
		return ctx.Err()
	}
	return t.Enter(ctx)
}

func (t tmuxTerminal) Enter(ctx context.Context) error {
	return t.run(ctx, nil, "send-keys", "-t", t.target, "Enter")
}

// Start creates the detached session running argv in dir and returns its pane's
// id, which the supervisor addresses from then on (the session name alone would
// resolve to whatever pane an attached operator last focused).
//
// remain-on-exit is set IN THE SAME tmux COMMAND LIST as new-session, so a CLI
// that dies at once still leaves a dead pane to read its status from, rather than
// taking the session down with it (the real-tmux crash-loop test starts exactly
// such a CLI and reads its status).
//
// ⚠ argv of ONE element is run by tmux through the shell (`sh -c`); two or more
// are exec'd directly. Both are fine for `claude` / `claude --continue`.
func (t tmuxTerminal) Start(ctx context.Context, dir string, argv []string) (string, error) {
	args := append([]string{"new-session", "-d", "-P", "-F", "#{pane_id}", "-s", t.target,
		"-x", "200", "-y", "50", "-c", dir}, argv...)
	args = append(args, ";", "set-option", "-w", "-t", t.target, "remain-on-exit", "on")
	out, err := t.output(ctx, args...)
	if err != nil {
		return "", err
	}
	pane := strings.TrimSpace(out)
	if !strings.HasPrefix(pane, "%") {
		return "", fmt.Errorf("tmux new-session printed %q, not a pane id", out)
	}
	return pane, nil
}

// Respawn runs argv in the dead pane again. Without -k it refuses a pane whose
// process is still running, so it can never kill a live CLI.
func (t tmuxTerminal) Respawn(ctx context.Context, pane, dir string, argv []string) error {
	return t.run(ctx, nil, append([]string{"respawn-pane", "-t", pane, "-c", dir}, argv...)...)
}

// Exited reads the pane's dead flag and exit status.
func (t tmuxTerminal) Exited(ctx context.Context, pane string) (bool, int, error) {
	out, err := t.output(ctx, "display-message", "-p", "-t", pane, "#{pane_dead} #{pane_dead_status}")
	if err != nil {
		return false, 0, err
	}
	var dead, status int
	if n, _ := fmt.Sscanf(strings.TrimSpace(out), "%d %d", &dead, &status); n < 1 {
		return false, 0, fmt.Errorf("tmux display-message printed %q", out)
	}
	return dead == 1, status, nil
}

func (t tmuxTerminal) Alive(ctx context.Context) error {
	return t.run(ctx, nil, "has-session", "-t", t.target)
}
