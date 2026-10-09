package agents

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// A FAILED KICKOFF IS ITS OWN SIGNAL (PR #37 review round 0).
//
// The deliverer stamps kicked_off BEFORE the paid turn and never retries a turn
// that fails after it, so a failed first turn turns KickoffOwed OFF. These pin the
// predicate that says it failed, and the scrub that keeps the error text it
// surfaces from carrying the operator's note.
// ---------------------------------------------------------------------------

// TestKickoffFailedTruthTable covers every cell of kicked_off × kickoff_error, with
// and without a note, and pins that KickoffFailed and KickoffOwed never hold at
// once.
//
// 🔴 ALL CELLS, BECAUSE EITHER CONJUNCT ALONE PASSES A TWO-CELL TABLE. `KickoffError
// != ""` alone would badge a never-kicked-off row whose PRE-send failure is still
// being retried (it is owed, not failed); `KickedOff` alone would badge every agent
// that ever started.
func TestKickoffFailedTruthTable(t *testing.T) {
	const errText = "responses HTTP 502: upstream reset vb27"
	for _, tt := range []struct {
		name      string
		kickedOff bool
		kerr      string
		note      string
		want      bool
	}{
		{"kicked off and a recorded failure is FAILED", true, errText, "fixture-note", true},
		{"kicked off and no failure is a delivered kickoff", true, "", "fixture-note", false},
		{"not kicked off with a failure is a pre-send retry, i.e. OWED, not failed", false, errText, "fixture-note", false},
		{"not kicked off and no failure is owed or idle", false, "", "fixture-note", false},
		{"a kicked-off failure with NO note still failed (upstream-written rows)", true, errText, "", true},
		{"no note, not kicked off, a failure", false, errText, "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a := Agent{KickedOff: tt.kickedOff, KickoffError: tt.kerr, PendingNote: tt.note}
			if got := KickoffFailed(a); got != tt.want {
				t.Errorf("KickoffFailed(kicked_off=%t, kickoff_error=%q, note=%q) = %t, want %t",
					tt.kickedOff, tt.kerr, tt.note, got, tt.want)
			}
			if KickoffFailed(a) && KickoffOwed(a) {
				t.Errorf("KickoffFailed and KickoffOwed both hold for %+v — a card would say "+
					"\"owed\" and \"failed\" about one turn", a)
			}
			if txt := KickoffFailureText(a); (txt != "") != tt.want {
				t.Errorf("KickoffFailureText = %q for a row whose KickoffFailed is %t", txt, tt.want)
			}
		})
	}
}

// noteFixture is long enough that a 512-byte snippet would truncate an echo of it,
// and contains no run that also occurs in the muster-authored prefixes below.
const noteFixture = "walk the q7 ledger, reconcile the plover counts, then post a summary to the zf4 channel"

// TestScrubNoteRemovesAWholeEcho: a runtime body quoting the whole note.
func TestScrubNoteRemovesAWholeEcho(t *testing.T) {
	in := `responses HTTP 400: {"error":"bad input","input":"` + noteFixture + `"} trailer-kd2`
	got := ScrubNote(in, noteFixture)
	if strings.Contains(got, "plover counts") || strings.Contains(got, "q7 ledger") {
		t.Fatalf("the note survived the scrub: %q", got)
	}
	want := `responses HTTP 400: {"error":"bad input","input":"` + NoteWithheld + `"} trailer-kd2`
	if got != want {
		t.Errorf("ScrubNote =\n  %q\nwant\n  %q", got, want)
	}
}

// TestScrubNoteRemovesATruncatedEcho is why ScrubNote matches RUNS rather than the
// whole note: agents' responses transport cuts a non-200 body at 512 bytes, so an
// echo of a long note arrives as a PREFIX of it, which a whole-note ReplaceAll
// never matches.
func TestScrubNoteRemovesATruncatedEcho(t *testing.T) {
	cut := noteFixture[:40] // "walk the q7 ledger, reconcile the plove"
	in := "responses HTTP 422: rejected input: " + cut + "…"
	got := ScrubNote(in, noteFixture)
	if strings.Contains(got, "reconcile the") || strings.Contains(got, "q7 ledger") {
		t.Fatalf("a TRUNCATED echo of the note survived the scrub: %q", got)
	}
	if want := "responses HTTP 422: rejected input: " + NoteWithheld + "…"; got != want {
		t.Errorf("ScrubNote = %q, want %q", got, want)
	}
}

// TestScrubNoteLeavesUnrelatedTextAlone is the control: muster's own error text
// shares short words with the note ("the", "to") and must come through intact, or
// the scrub would be destroying the error the operator needs.
func TestScrubNoteLeavesUnrelatedTextAlone(t *testing.T) {
	for _, in := range []string{
		"kickoff turn failed after it was handed to the gateway (not retried, so it is never paid twice): unexpected EOF",
		"",
	} {
		if got := ScrubNote(in, noteFixture); got != in {
			t.Errorf("ScrubNote changed text that holds no 16-byte run of the note:\n  in  %q\n  out %q", in, got)
		}
	}
	if got := ScrubNote("anything at all wq3", ""); got != "anything at all wq3" {
		t.Errorf("an empty note must scrub nothing, got %q", got)
	}
}

// TestScrubNoteRemovesAShortNoteWholeAndKeepsUTF8Valid: a note under 16 bytes is
// removed whenever it appears whole, and multi-byte text either side survives as
// valid UTF-8 (matching is per rune, never mid-rune).
func TestScrubNoteRemovesAShortNoteWholeAndKeepsUTF8Valid(t *testing.T) {
	const short = "ping ñu-7"
	in := "réponse — " + short + " — fin ✓"
	got := ScrubNote(in, short)
	if strings.Contains(got, short) {
		t.Errorf("a short note appearing whole survived: %q", got)
	}
	if want := "réponse — " + NoteWithheld + " — fin ✓"; got != want {
		t.Errorf("ScrubNote = %q, want %q", got, want)
	}
	if !utf8.ValidString(got) {
		t.Errorf("ScrubNote produced invalid UTF-8: %q", got)
	}
}
