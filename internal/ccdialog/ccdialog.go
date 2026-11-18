// Package ccdialog reads a Claude Code pane's screen for a dialog that keys
// typed into the pane would ANSWER rather than reach the model: a permission
// prompt, the AskUserQuestion picker, any numbered select menu.
//
// It is the one copy of that reading. ccd checks it before each of its own
// keys (cmd/ccd/dialog.go), and muster-mirror checks it before it pastes a
// message into a mirrored Claude Code pane (internal/mirrorhost, the Claude
// prompt guard). It was ccd's (#163) and moved here unchanged when the mirror
// host needed the same check.
package ccdialog

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/askdrive"
	"github.com/ZacxDev/muster/internal/termscreen"
)

// ⚠ A DENY-LIST, NOT AN ALLOW-LIST, AND READ FROM ONE CAPTURE. It recognises the
// dialogs below by their structure; a dialog drawn some other way is not
// recognised. It is a check before the keys, not a lock: a dialog that opens in
// the milliseconds between the capture and the keys is not caught.

// selectCursor is the CLI's select-menu cursor on a numbered option, as the
// permission prompt and the picker draw it: "❯ 1. Yes" (a space, or a no-break
// space, after the ❯). Leading spaces are the dialog's indent.
var selectCursor = regexp.MustCompile(`^\s*❯[ \x{00a0}]+\d+\.\s`)

// IsRule reports whether line is one of the CLI's horizontal rules (a run of
// U+2500 box-drawing characters, as above and below its input box).
func IsRule(line string) bool {
	t := strings.TrimSpace(line)
	return utf8.RuneCountInString(t) >= 10 && strings.Trim(t, "─") == ""
}

// screenLines is capture's lines as text, and the same lines with every cell
// drawn dim (SGR 2) blanked. capture is `tmux capture-pane -p -J`, with or
// without -e: a capture with no escape sequence is split as it is (both
// results the same), one with them is read by termscreen.ParseScreen — the one
// SGR reader, which carries a style across rows as tmux's capture does.
func screenLines(capture string) (text, undimmed []string) {
	if !strings.ContainsRune(capture, 0x1b) {
		l := strings.Split(strings.ReplaceAll(capture, "\r", ""), "\n")
		return l, l
	}
	for _, row := range termscreen.ParseScreen(capture).Rows {
		var t, u strings.Builder
		for _, c := range row {
			t.WriteRune(c.R)
			if c.Dim {
				u.WriteRune(' ')
			} else {
				u.WriteRune(c.R)
			}
		}
		text, undimmed = append(text, t.String()), append(undimmed, u.String())
	}
	return text, undimmed
}

// OnScreen reports whether capture (tmux capture-pane -p -J of the CLI's
// pane, with or without -e) shows a dialog that keys typed into the pane would answer, and which.
//
// 🔴 A FALSE "YES" WEDGES THE SENDER, SO ONLY THE BOTTOM OF THE SCREEN COUNTS.
// Every paste is refused while this answers true, and an idle screen does not
// change by itself: text that merely LOOKS like a dialog — model output quoting a
// picker's "Review your answers" / "Submit answers" or a permission prompt's
// "❯ 1. Yes" and "Esc to cancel", or such text sitting in the input box — would
// refuse every send for as long as it is on screen. (ccd's first version ran
// askdrive.Parse and kept its Unknown kind, a guess from those words ANYWHERE on
// the screen; a transcript quoting them wedged an idle prompt.) At its prompt the
// CLI draws the input box between two rules with its status line under the
// second, so the transcript and the box are never at the bottom, nor below the
// last rule. What is recognised:
//
//   - the AskUserQuestion picker, by askdrive.Parse's STRUCTURAL kinds only
//     (Question, Review, Clipped), each anchored to the bottom of the screen: its
//     footer among the last three lines, or its review screen's Submit/Cancel rows
//     as the last two. Not Unknown. (Not "below the last rule" either: the picker
//     draws a rule of its own between its options and "Chat about this".)
//   - below the screen's LAST horizontal rule: a numbered select menu's cursor on
//     an option ("❯ 1. Yes"), or the permission prompt's footer ("Esc to cancel ·
//     Tab to amend …") as one of the last three lines. The permission prompt
//     replaces the input box and draws a rule as its top border, so it is below
//     the last one.
//
// Read against captures of the real CLI: 2.1.296 at its prompt, at a Bash
// permission prompt, and at its prompt with those words in the transcript and
// in the input box (cmd/ccd/testdata/pane_*_2.1.296.txt); and 2.1.289's
// permission prompt, pickers and finished turns (internal/askdrive/testdata).
func OnScreen(capture string) (what string, shown bool) {
	lines, _ := screenLines(capture)
	if strings.ContainsRune(capture, 0x1b) {
		capture = strings.Join(lines, "\n")
	}
	switch askdrive.Parse(capture).Kind {
	case askdrive.Question, askdrive.Review, askdrive.Clipped:
		return "the AskUserQuestion picker", true
	}
	from := 0
	for i := len(lines) - 1; i >= 0; i-- {
		if IsRule(lines[i]) {
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

// optionRow is one numbered option of a select menu, with or without the
// cursor on it: "❯ 1. Stop and wait for limit to reset", "  2. Upgrade your
// plan". Group 1 is the option's label.
var optionRow = regexp.MustCompile(`^\s*(?:❯[ \x{00a0}]+)?\d+\.\s+(.*?)\s*$`)

// limitStop is the usage-limit menu's own option: it stops the turn and waits
// for the account's limit to reset (the CLI's value "cancel"). With usage
// credits on, the CLI labels the same option just "Stop".
const limitStop = "Stop and wait for limit to reset"

// limitSiblings are the label prefixes of the usage-limit menu's OTHER
// options, as the CLI builds them (Claude Code 2.1.289's rate-limit options
// menu): read only to tell its bare "Stop" from any other menu's.
var limitSiblings = []string{
	"Wait here, then continue automatically",
	"Add funds to continue with",
	"Switch to usage",
	"Ask your admin for more usage",
	"Don’t continue automatically", "Don't continue automatically",
}

// limitLine is the CLI's own statement of the limit it hit, as it writes it
// into the transcript above the menu: "You've hit your weekly limit · resets
// Nov 2, 1am (UTC)", "You've reached your … limit".
var limitLine = regexp.MustCompile(`You[\x{2019}']ve (?:hit|reached) your .*`)

// UsageLimit reports whether capture shows Claude Code's USAGE-LIMIT menu —
// the select menu the CLI raises when the account hit its session or weekly
// limit ("❯ 1. Stop and wait for limit to reset") — and the CLI's limit line
// when the screen still shows it ("You've hit your weekly limit · resets Nov 2,
// 1am (UTC)"), "" when it does not.
//
// 🔴 IT REFINES OnScreen, NEVER WIDENS IT: a screen OnScreen does not read as a
// dialog is never the limit menu, and the option must be a numbered row below
// the screen's last rule — the same anchoring, so the menu's words quoted in
// the transcript or typed into the input box are not it. Inside that dialog it
// is the menu when one option's label is exactly [limitStop], or exactly "Stop"
// beside one of [limitSiblings]. Any other dialog is not it.
//
// 🔴 WHY IT EXISTS: a pane parked on this menu refuses every later turn as a
// dialog, which ccd answered `awaiting_approval` — so an agent that reached
// the menu without a `rate_limited` turn being seen (failover not yet shipped,
// or that turn's failure missed) reported awaiting_approval for ever and was
// never failed over. ccd answers it as the rate limit it is (cmd/ccd dialog.go).
//
// The limit line is the LAST such line on screen, from "You" to the end of its
// row. It is not anchored: once the menu is recognised, it is only the reset
// time's source, and a misread one is bounded by the account pool's own limits
// on a stated reset (internal/ccpool statedReset).
func UsageLimit(capture string) (limit string, shown bool) {
	if _, dialog := OnScreen(capture); !dialog {
		return "", false
	}
	lines, _ := screenLines(capture)
	from := 0
	for i := len(lines) - 1; i >= 0; i-- {
		if IsRule(lines[i]) {
			from = i + 1
			break
		}
	}
	stop, bareStop, sibling := false, false, false
	for _, l := range lines[from:] {
		m := optionRow.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		switch label := m[1]; {
		case label == limitStop:
			stop = true
		case label == "Stop":
			bareStop = true
		default:
			for _, p := range limitSiblings {
				if strings.HasPrefix(label, p) {
					sibling = true
				}
			}
		}
	}
	if !stop && !(bareStop && sibling) {
		return "", false
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if m := limitLine.FindString(lines[i]); m != "" {
			return strings.TrimSpace(m), true
		}
	}
	return "", true
}

// InputBox is what Claude Code's input box holds on capture, and whether the
// box was found at all. The CLI draws the box between the screen's LAST TWO
// horizontal rules, its first row starting with the prompt mark `❯`; the
// status line is under the second rule. text is the box's rows with the mark
// and the padding trimmed, joined by newlines: "" for an empty box.
//
// found is false when the bottom of the screen is not that shape — fewer than
// two rules, nothing between them, or a first row that is not the prompt — and
// a caller that needs the box empty refuses on it: an unreadable box is not
// presumed empty. Call OnScreen first: a dialog replaces the box.
//
// 🔴 A GHOST SUGGESTION IS AN EMPTY BOX. At an empty prompt Claude Code may
// draw a suggestion in the box (`Try "…"` in a fresh session, a suggested next
// prompt after a turn), DIM: `❯\u00a0` then the suggestion
// in SGR 2 (`\e[2m…\e[0m`, one run, or a run per word with plain spaces
// between). Typing replaces it; nothing of it is submitted unless accepted with
// Tab or →, which muster never sends (a bracketed paste over it, muster's
// paste, replaced it and Enter submitted only the paste, on the real 2.1.289). So with capture-pane -e a box whose every
// non-blank character is dim reads "". Only then: a box holding ANY character
// not drawn dim is read whole, dim characters included, as text — a draft is
// never taken for a suggestion because part of it is dim. Without -e no style
// can be seen, and a suggestion reads as text (the box is refused as held).
//
// Read against the real CLI (cmd/ccd/testdata/pane_*_2.1.296.txt): the empty
// prompt `❯ ` at idle, and `❯ ❯ 1. Yes` / `❯ [Pasted text #1 +4 lines]` with
// text in the box; the ghost suggestion as 2.1.289's panes draw it
// (testdata/pane_*_ghost*.ansi, synthetic text in that escape form).
func InputBox(capture string) (text string, found bool) {
	lines, undimmed := screenLines(capture)
	last, prev := -1, -1
	for i := len(lines) - 1; i >= 0; i-- {
		if !IsRule(lines[i]) {
			continue
		}
		if last < 0 {
			last = i
			continue
		}
		prev = i
		break
	}
	if prev < 0 || last-prev < 2 {
		return "", false
	}
	if !strings.HasPrefix(strings.TrimSpace(lines[prev+1]), "❯") {
		return "", false
	}
	if boxText(undimmed[prev+1:last]) == "" {
		return "", true
	}
	return boxText(lines[prev+1 : last]), true
}

// boxText is the box's rows with the prompt mark and the padding trimmed,
// joined by newlines.
func boxText(box []string) string {
	rows := []string{strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(box[0]), "❯"))}
	for _, l := range box[1:] {
		rows = append(rows, strings.TrimSpace(l))
	}
	return strings.TrimSpace(strings.Join(rows, "\n"))
}
