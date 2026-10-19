package main

import (
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/askdrive"
)

// errDialogShown: the CLI's pane shows a dialog — a permission prompt, the
// AskUserQuestion picker, any numbered select menu — so a paste or an Enter would
// ANSWER it rather than reach the model. Nothing was typed. The server answers it
// 409 `busy` with code `awaiting_approval` (server.go runTurn): `busy` because
// that is the refusal muster already reads as "nothing was pasted, send it later"
// (internal/agentgateway.Unsent).
var errDialogShown = errors.New("the CLI's pane shows a dialog that a paste or Enter would answer")

// 🔴 WHY ccd READS THE SCREEN BEFORE IT TYPES, WHEN EVERYTHING ELSE IT KNOWS
// COMES FROM HOOKS. Whether a turn may be pasted is decided from hook state
// (sessionStarted, busy, awaitingApproval), and hook state is only as true as the
// hooks: a forged Stop or SessionStart makes a session at a permission prompt
// read idle, and the next turn's paste-then-Enter would answer the prompt —
// Enter takes the highlighted option, which on the permission prompt is "1. Yes".
// The hook channel is authenticated (server.go hookHandler), but the CLI's own tools run as the
// same user as ccd and can read that credential, so hook state can still be
// forged from inside the session. The screen cannot be forged that way: this
// check holds whatever the hook channel's state. It is also what stops the
// submit-retry Enter (runTurn) from answering a dialog that opened after a
// paste whose UserPromptSubmit never arrived.
//
// ⚠ A DENY-LIST, NOT AN ALLOW-LIST, AND READ FROM ONE CAPTURE. It recognises the
// dialogs below by their structure; a dialog drawn some other way is not
// recognised. It is a check before the keys, not a lock: a dialog that opens in
// the milliseconds between the capture and the keys is not caught — which needs
// a turn running in the session at that moment, and so a session ccd already
// believes busy, or a forged hook timed to that window.

// selectCursor is the CLI's select-menu cursor on a numbered option, as the
// permission prompt and the picker draw it: "❯ 1. Yes" (a space, or a no-break
// space, after the ❯). Leading spaces are the dialog's indent.
var selectCursor = regexp.MustCompile(`^\s*❯[ \x{00a0}]+\d+\.\s`)

// isRule reports whether line is one of the CLI's horizontal rules (a run of
// U+2500 box-drawing characters, as above and below its input box).
func isRule(line string) bool {
	t := strings.TrimSpace(line)
	return utf8.RuneCountInString(t) >= 10 && strings.Trim(t, "─") == ""
}

// dialogOnScreen reports whether capture (tmux capture-pane -p -J of the CLI's
// pane) shows a dialog that keys typed into the pane would answer, and which.
//
// Recognised:
//   - the AskUserQuestion picker or its review screen — askdrive.Parse, the
//     reader muster's mirror drives that picker with, which also flags a
//     picker it could not fully read;
//   - below the screen's LAST horizontal rule: a numbered select menu's cursor
//     on an option ("❯ 1. Yes"), or the permission prompt's footer ("Esc to
//     cancel · Tab to amend …") as one of the last three lines.
//
// 🔴 WHY ONLY BELOW THE LAST RULE. At its prompt the CLI draws its input box
// between two rules with only status lines under the second, so whatever is in
// the box — an operator's half-typed "1. x", or the turn ccd has just pasted,
// whose Enter is checked here too — is above the last rule and cannot read as a
// dialog. A dialog replaces the input box, so the last rule is then the dialog's
// own top border (the permission prompt draws one) or one further up, and the
// dialog is below it either way. Read against captures of the real CLI: the
// pinned 2.1.296 at its prompt and at a Bash permission prompt
// (testdata/pane_*_2.1.296.txt, captured in the image), and 2.1.289's
// permission prompt, pickers and finished turns (internal/askdrive/testdata).
func dialogOnScreen(capture string) (what string, shown bool) {
	if askdrive.Parse(capture).Kind != askdrive.None {
		return "the AskUserQuestion picker", true
	}
	lines := strings.Split(strings.ReplaceAll(capture, "\r", ""), "\n")
	from := 0
	for i := len(lines) - 1; i >= 0; i-- {
		if isRule(lines[i]) {
			from = i + 1
			break
		}
	}
	nonEmpty := 0
	for i := len(lines) - 1; i >= from; i-- {
		t := strings.TrimSpace(lines[i])
		if t == "" {
			continue
		}
		nonEmpty++
		if nonEmpty <= 3 && strings.HasPrefix(t, "Esc to cancel") {
			return "a permission prompt", true
		}
		if selectCursor.MatchString(lines[i]) {
			return "a select menu (" + t + ")", true
		}
	}
	return "", false
}
