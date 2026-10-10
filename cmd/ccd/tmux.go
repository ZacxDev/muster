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
// reading a leading `!` or `/` as a mode switch; neutralizeInputMode does that.
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
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tmux %s: %v: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return nil
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

// StartSession creates the session (attach-or-create, detached) running command
// in dir — idempotent if a tmux server from an earlier ccd survives.
func (t tmuxTerminal) StartSession(ctx context.Context, dir, command string) error {
	return t.run(ctx, nil, "new-session", "-A", "-d", "-s", t.target, "-x", "200", "-y", "50", "-c", dir, command)
}

func (t tmuxTerminal) Alive(ctx context.Context) error {
	return t.run(ctx, nil, "has-session", "-t", t.target)
}
