package fleet

import (
	"testing"
	"time"
)

// The fixed texts and muster's attribution line, pinned as literals (never
// derived from the constants they test).

func TestTheFixedTextsByteForByte(t *testing.T) {
	for name, c := range map[string]struct{ got, want string }{
		"continue": {ContinueText, "Yes, continue with what you proposed. Ask with attention_ask before anything destructive, outward or outside this task."},
		"stop_note": {StopNoteText, "Stop after this step and wait for your owner's next message. Do not start anything new; ask with attention_ask if something cannot wait."},
		"handoff merged": {HandoffText(4321, PRMerged),
			"Your PR #4321 is merged and nothing is in flight. Write your handoff (what is done, what is left, where), then stop."},
		"handoff closed": {HandoffText(87, PRClosed),
			"Your PR #87 is closed and nothing is in flight. Write your handoff (what is done, what is left, where), then stop."},
		"handoff none": {HandoffText(0, ""), "Write your handoff (what is done, what is left, where), then stop."},
		"reassign": {ReassignHandoffText(613, "lint sweep"),
			"Task #613 has moved to another session (lint sweep). Stop work on it. Write your handoff (what is done, what is left, where), then stop."},
	} {
		if c.got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", name, c.got, c.want)
		}
	}
}

// TestTheAttributionLine: "[Chief, approved by <owner> at HH:MM UTC]", the
// owner's name on one line with no brackets of its own, the time in UTC.
func TestTheAttributionLine(t *testing.T) {
	at := time.Date(2001, 2, 3, 19, 7, 0, 0, time.FixedZone("x", -5*3600)) // 00:07 UTC the next day
	for name, c := range map[string]struct{ owner, want string }{
		"plain":    {"Rowan Vale", "[Chief, approved by Rowan Vale at 00:07 UTC]"},
		"brackets": {"Ivy [ops]\n Lund", "[Chief, approved by Ivy (ops) Lund at 00:07 UTC]"},
		"blank":    {"  ", "[Chief, approved by your owner at 00:07 UTC]"},
	} {
		if got := Header(c.owner, at); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
	if got := Body("Rowan Vale", at, "Go on."); got != "[Chief, approved by Rowan Vale at 00:07 UTC]\nGo on." {
		t.Errorf("body: %q", got)
	}
	if HeaderPrefix != "[Chief, approved by " {
		t.Errorf("HeaderPrefix %q", HeaderPrefix)
	}
}
