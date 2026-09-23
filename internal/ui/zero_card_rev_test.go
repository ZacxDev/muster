package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/notes"
)

// zeroUnixNano is what time.Time{}.UnixNano() actually returns. Spelled as the
// literal rather than computed, so this test states the hazard's real value: if
// the Go runtime's zero-time epoch ever changed, recomputing it would keep the
// test green while the string the renderer emits moved.
const zeroUnixNano = "-6795364578871345152"

// TestZeroUpdatedAtOmitsTheCardRev pins muster task 487 item 3.
//
// 🔴 THE FAILURE IS "THE CARD SILENTLY STOPS UPDATING", WITH NO ERROR ANYWHERE.
// data-card-rev is compared by musterRefuseStaleCardSwap, which drops a swap
// whose INCOMING rev is strictly older than what is on screen. A note reaching
// noteCard with a zero UpdatedAt renders rev -6795364578871345152, which is older
// than every real rev — so every response the server sends for that card is
// refused, forever, and only a full page reload recovers (a reload is not a swap).
//
// The direction is the subtle half and is easy to get backwards: a zero rev
// already ON SCREEN is harmless, because everything incoming then looks newer.
// It is the SERVER rendering one that wedges the card. So this test asserts on
// the rendered document, which is the side that matters.
//
// Not reachable from any STORE path — NO projection omits updated_at any more
// (ListSummaries was the last, and gained the column when the idle-task reaper
// switched to it) — and that is the reason to pin it rather than skip it: nothing
// stops a NEW projection from omitting the field and reaching this renderer, and
// the symptom names neither the projection nor the rev.
//
// 🔴 IT IS TRIVIALLY REACHABLE FROM A TEST FIXTURE, and the unqualified version
// of this sentence has already cost something. A bare notes.Note{ID: N} — the
// idiomatic "empty" fixture in this package — has a zero UpdatedAt, so it renders
// no rev; that is how the sweep's own renderEmpty document dropped out of
// TestEveryDocumentThatRendersATaskCardCarriesTheStaleGuard's reach, silently,
// because nothing pinned the count. No note the STORE can produce has a zero
// updated_at (the column is NOT NULL DEFAULT now(), migrations/0001_init.sql), so
// such a fixture is not a smaller real note — it is an impossible one.
func TestZeroUpdatedAtOmitsTheCardRev(t *testing.T) {
	zero := notes.Note{ID: 77, Title: "a task with no updated_at", Body: "b", Status: notes.StatusOpen}
	if !zero.UpdatedAt.IsZero() {
		t.Fatal("fixture error: UpdatedAt must be the zero time for this test to mean anything")
	}

	var b strings.Builder
	if err := renderDetailCard(&b, zero); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := b.String()

	if strings.Contains(out, zeroUnixNano) {
		t.Errorf("a zero UpdatedAt rendered data-card-rev=%q. Every swap of this card is then REFUSED "+
			"by musterRefuseStaleCardSwap (that rev is older than every real one) and nothing on the "+
			"page recovers short of a full reload.\n%s", zeroUnixNano, out)
	}
	if strings.Contains(out, "data-card-rev") {
		t.Errorf("a zero UpdatedAt still emitted a data-card-rev attribute; absent is the safe state "+
			"(the guard returns early when either side has none, falling back to always-apply).\n%s", out)
	}

	// 🔴 POSITIVE CONTROL. Without it, deleting the attribute unconditionally —
	// which disables the stale-swap guard for every card in the app — would pass
	// both assertions above and read as a fix.
	real := zero
	real.UpdatedAt = time.Date(2026, 9, 4, 5, 0, 0, 0, time.UTC)
	var rb strings.Builder
	if err := renderDetailCard(&rb, real); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(rb.String(), `data-card-rev="`+itoaNano(real.UpdatedAt)+`"`) {
		t.Fatalf("positive control failed: a NON-zero UpdatedAt did not render its data-card-rev, so "+
			"the guard is off for every card and the assertions above prove nothing.\n%s", rb.String())
	}
}

func itoaNano(t time.Time) string {
	const digits = "0123456789"
	n := t.UnixNano()
	if n == 0 {
		return "0"
	}
	neg := n < 0
	var buf [24]byte
	i := len(buf)
	for n != 0 {
		i--
		d := n % 10
		if d < 0 {
			d = -d
		}
		buf[i] = digits[d]
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
