package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
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
// reading a leading `!` as shell mode, and a paste is only a paste for as long as
// its text cannot END it: both are the caller's job (sanitizePrompt and
// neutralizeInputMode in server.go; a leading `/` is let through on purpose).
//
// 🔴 INPUT GOES TO THE CLI's PANE ID, NEVER TO THE SESSION NAME. `-t cc` resolves
// to the session's ACTIVE pane — whichever pane an attached operator last focused —
// so a split with a shell in it would receive the prompt and run it (measured
// against this code's predecessor, which targeted the session). Paste and Enter
// take the pane id the supervisor got from new-session, and Paste refuses a pane
// that is gone or dead (errPaneNotLive) rather than pasting somewhere else.
//
// ⚠ NOT EVERY BYTE IS PRESERVED, AND THE EXCEPTIONS ARE THE CLI'S, NOT tmux's: a
// TAB became four spaces in the same measurement. CRLF and lone CR are folded to
// LF by the caller before it gets here (see sanitizePrompt).
type tmuxTerminal struct {
	bin    string
	socket string // -L name; "" = the default server
	conf   string // -f file for a server this process starts; "" = tmux's default
	// target is the SESSION name: what Start creates and Alive checks. It is never
	// an input target (see above).
	target string
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

// errPaneNotLive: the pane to paste into no longer exists or its process has
// exited. The server answers not_ready for it.
var errPaneNotLive = errors.New("the CLI's pane is gone or its process has exited")

func (t tmuxTerminal) Paste(ctx context.Context, pane, text string) error {
	if !strings.HasPrefix(pane, "%") {
		return fmt.Errorf("%w: %q is not a pane id", errPaneNotLive, pane)
	}
	// Refuse a dead or vanished pane up front. (One that dies between this check
	// and the paste is still addressed by its own id, so the paste cannot land
	// in another pane.)
	out, err := t.output(ctx, "display-message", "-p", "-t", pane, "#{pane_dead}")
	if err != nil {
		return fmt.Errorf("%w: %v", errPaneNotLive, err)
	}
	if strings.TrimSpace(out) != "0" {
		return fmt.Errorf("%w: pane %s reports pane_dead=%q", errPaneNotLive, pane, strings.TrimSpace(out))
	}
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	buf := "ccd-" + hex.EncodeToString(rnd[:])
	if err := t.run(ctx, []byte(text), "load-buffer", "-b", buf, "-"); err != nil {
		return err
	}
	if err := t.run(ctx, nil, "paste-buffer", "-p", "-d", "-b", buf, "-t", pane); err != nil {
		_ = t.run(context.Background(), nil, "delete-buffer", "-b", buf)
		return err
	}
	select {
	case <-time.After(t.enterDelay):
	case <-ctx.Done():
		return ctx.Err()
	}
	return t.Enter(ctx, pane)
}

func (t tmuxTerminal) Enter(ctx context.Context, pane string) error {
	if !strings.HasPrefix(pane, "%") {
		return fmt.Errorf("%w: %q is not a pane id", errPaneNotLive, pane)
	}
	return t.run(ctx, nil, "send-keys", "-t", pane, "Enter")
}

// Start creates the detached session running argv in dir and returns its pane's
// id, which the supervisor polls and respawns and the server pastes into from then
// on (the session name alone would resolve to whatever pane an attached operator
// last focused).
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

// paneDeadFormat is what Exited asks tmux for; parsePaneExit reads its output.
const paneDeadFormat = "#{pane_dead} #{pane_dead_status} #{pane_dead_signal}"

// Exited reads whether the pane's process has exited and, once tmux knows it, how.
func (t tmuxTerminal) Exited(ctx context.Context, pane string) (paneExit, error) {
	out, err := t.output(ctx, "display-message", "-p", "-t", pane, paneDeadFormat)
	if err != nil {
		return paneExit{}, err
	}
	return parsePaneExit(out)
}

// parsePaneExit reads "<dead> <status> <signal>", where status and signal are
// EMPTY until tmux has reaped the process.
//
// 🔴 DEAD IS NOT THE SAME AS REAPED, AND READING THEM AS ONE RECORDS A CRASH AS
// A CLEAN EXIT. tmux marks the pane dead at pty EOF and fills the status only once
// it has waited for the process, so "1  " (dead, nothing known) is a real read —
// read as status 0, it is what made the crash-loop test record 0 for a CLI that
// exits 3. It is not always brief: tmux can miss the SIGCHLD altogether
// (supervisor.run, Reap). A process killed by a signal has an EMPTY status FOR
// EVER (tmux fills pane_dead_status only for WIFEXITED) and the signal in
// pane_dead_signal instead — a NUMBER on Linux (tmux's sig2name falls back to
// the number without BSD's sys_signame; measured 9 for SIGKILL on tmux 3.3a and
// 3.7c), a name elsewhere, which is kept as text with the status unknown.
// "Wait until status is set" would hang on a signal death and on a missed
// SIGCHLD alike; the supervisor bounds the unknown case instead
// (supervisor.statusGrace).
func parsePaneExit(out string) (paneExit, error) {
	f := strings.Split(strings.TrimRight(out, "\r\n"), " ")
	if len(f) != 3 || (f[0] != "0" && f[0] != "1") {
		return paneExit{}, fmt.Errorf("tmux display-message %q printed %q", paneDeadFormat, out)
	}
	if f[0] == "0" {
		return paneExit{}, nil
	}
	e := paneExit{Dead: true}
	switch {
	case f[1] != "":
		n, err := strconv.Atoi(f[1])
		if err != nil {
			return paneExit{}, fmt.Errorf("pane_dead_status %q: %v", f[1], err)
		}
		e.Known, e.Status = true, n
	case f[2] != "":
		e.Known, e.SignalText, e.Status = true, f[2], exitUnknown
		if n, err := strconv.Atoi(f[2]); err == nil && n > 0 {
			e.Signal, e.Status = n, 128+n
		}
	}
	return e, nil
}

// Reap runs a no-op job in the tmux server. The job's own SIGCHLD makes the
// server run its waitpid(WAIT_ANY) loop, which also reaps a pane process whose
// SIGCHLD it missed (supervisor.run says when and why). `run-shell` without -b
// returns once the job has finished.
func (t tmuxTerminal) Reap(ctx context.Context) error {
	return t.run(ctx, nil, "run-shell", "true")
}

func (t tmuxTerminal) Alive(ctx context.Context) error {
	return t.run(ctx, nil, "has-session", "-t", t.target)
}
