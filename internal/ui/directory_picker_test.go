package ui

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/notes"
)

// realisticDirs returns n paths whose LENGTH matches production.
//
// 🔴 THE LENGTH IS THE FIXTURE'S WHOLE POINT, AND IT IS NOT DECORATION. Distinct
// working directories on the deployment this was measured against averaged ~74
// characters; a fixture of "/a", "/b" makes the payload guard below pass at any
// option count, because the payload never approaches its bound. That is the
// classic fixture that cannot see the mutant it exists to catch.
//
// ⚠ SO THE SYNTHETIC PATHS BELOW ARE THE SAME LENGTH AS THE REAL ONES THEY
// REPLACE (73 characters rendered), deliberately. Genericising them to something
// shorter would have removed the private path AND silently disarmed the guard —
// a scrub that reads as cosmetic and is not.
func realisticDirs(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		// 73 characters rendered, matching the measured average. See the header:
		// shortening this disarms the payload guard below.
		out = append(out, fmt.Sprintf("/srv/projects/example-org/example-service/.worktrees/agent/agent-%08d", i))
	}
	return out
}

func renderDirectoryPicker(t *testing.T, seed []string, current string, failed bool) string {
	t.Helper()
	var b bytes.Buffer
	if err := directoryCombobox(seed, current, failed).Render(&b); err != nil {
		t.Fatalf("render: %v", err)
	}
	return b.String()
}

// TestDirectoryPickerSubmitsFreeText pins VALUE MODE, and it is the property
// most easily lost in this rewrite.
//
// 🔴 THE PICKER MUST ACCEPT A PATH IT HAS NEVER SEEN. The control it replaced
// was `<input name="directory" list="note-directories">` — a datalist offers
// suggestions but never constrains the value. The combobox component has TWO
// modes, and the other one would silently break this: with a
// [data-combobox-hidden] sibling the VISIBLE input stops being the submitted
// field, and the component's own input handler CLEARS the hidden value on every
// keystroke (see cbFilter/cbPick in components.go). A typed-but-not-picked path
// would then submit as empty — a task filed against no directory at all, with
// nothing on screen saying so.
//
// So: the visible input carries name="directory", and there is NO hidden
// companion.
func TestDirectoryPickerSubmitsFreeText(t *testing.T) {
	out := renderDirectoryPicker(t, []string{"/work/alpha"}, "", false)

	if strings.Contains(out, "data-combobox-hidden") {
		t.Fatalf("the directory picker rendered a [data-combobox-hidden] input. That puts "+
			"the combobox in hidden-value mode, where a TYPED path that is never picked "+
			"from the list submits as EMPTY.\nmarkup:\n%s", out)
	}
	if !strings.Contains(out, `name="directory"`) {
		t.Fatalf("no input carries name=\"directory\" — the field would submit nothing."+
			"\nmarkup:\n%s", out)
	}
	// The name and the combobox input must be the SAME element, or the two
	// assertions above can both hold while the submitted field is a different,
	// non-interactive input.
	if !strings.Contains(out, `data-combobox-input`) ||
		strings.Count(out, "<input") != 1 {
		t.Fatalf("expected exactly ONE input, carrying both name=\"directory\" and "+
			"data-combobox-input.\nmarkup:\n%s", out)
	}
}

// TestDirectoryPickerKeepsTheTasksCurrentDirectory pins the edit path. The edit
// modal is a FULL REPLACE of the editable columns (see handleNoteEdit), so a
// picker that rendered an empty box would silently CLEAR a task's directory the
// next time anyone saved an unrelated field.
func TestDirectoryPickerKeepsTheTasksCurrentDirectory(t *testing.T) {
	const current = "/srv/projects/example-org/example-service"
	out := renderDirectoryPicker(t, []string{"/work/alpha", "/work/beta"}, current, false)

	if !strings.Contains(out, `value="`+current+`"`) {
		t.Fatalf("the task's current directory %q is not pre-filled — saving the edit form "+
			"would clear it.\nmarkup:\n%s", current, out)
	}
}

// TestDirectoryPickerIsWiredToTheSearchRoute pins the ONLY route by which the
// directories outside the seed are reachable. Without it the small seed is not
// an optimisation, it is data loss.
func TestDirectoryPickerIsWiredToTheSearchRoute(t *testing.T) {
	out := renderDirectoryPicker(t, realisticDirs(notes.DefaultDirectoryLimit), "", false)

	if !strings.Contains(out, `data-combobox-remote="`+DirectorySearchPath+`"`) {
		t.Fatalf("the picker carries no data-combobox-remote=%q. With a seed of %d and an "+
			"archive of ~1,532 directories, that makes ~1,520 of them unreachable.\nmarkup:\n%s",
			DirectorySearchPath, notes.DefaultDirectoryLimit, out)
	}
}

// TestDirectoryPickerPayloadStaysSmall is the guard on the number this whole
// redesign is about.
//
// 🔴 THE SHAPE THAT WAS REJECTED, AND WHY THE CEILING IS A BYTE COUNT. Repointing
// the query at request_history WITHOUT changing the control would have rendered
// all 1,532 live directories as <option> elements: ~154 KB shipped into the DOM
// on every modal open (114,122 bytes of path text plus 26 bytes of markup each).
// The seeded shape measures 6,789 bytes at notes.DefaultDirectoryLimit with
// production-length paths.
//
// The ceiling is deliberately loose (a per-option class string is allowed to
// grow) but far below the rejected shape, so it catches the two regressions that
// matter: raising DefaultDirectoryLimit without thinking, and passing the whole
// archive in as the "seed".
func TestDirectoryPickerPayloadStaysSmall(t *testing.T) {
	const ceilingBytes = 12_000

	seeded := renderDirectoryPicker(t, realisticDirs(notes.DefaultDirectoryLimit), "", false)
	if len(seeded) > ceilingBytes {
		t.Fatalf("the seeded directory picker renders %d bytes, over the %d-byte ceiling. "+
			"Every seeded option lands in the modal's markup on open; the shape this "+
			"replaced measured ~154,000 bytes at the live archive's size.",
			len(seeded), ceilingBytes)
	}

	// 🔴 THE POSITIVE CONTROL. A ceiling nothing can exceed is not a guard. Feed
	// the renderer the whole-archive shape and watch the number move past it — if
	// this does NOT trip, the assertion above is measuring nothing.
	whole := renderDirectoryPicker(t, realisticDirs(1532), "", false)
	if len(whole) <= ceilingBytes {
		t.Fatalf("CONTROL FAILED: rendering all 1,532 directories produced only %d bytes, "+
			"under the %d-byte ceiling — so the ceiling above cannot detect an unbounded "+
			"seed and proves nothing.", len(whole), ceilingBytes)
	}
	t.Logf("seeded (%d dirs) = %d bytes; whole archive (1532 dirs) = %d bytes",
		notes.DefaultDirectoryLimit, len(seeded), len(whole))
}

// TestDirectoryPickerDistinguishesFailedFromEmpty pins BOTH directions of the
// signal that replaced `dirs = nil // non-fatal: the picker is just empty`.
//
// One direction alone is walkable: a renderer that always says "failed" passes
// the failure case, and one that never says it passes the empty case.
func TestDirectoryPickerDistinguishesFailedFromEmpty(t *testing.T) {
	cases := []struct {
		name      string
		seed      []string
		failed    bool
		wantState string
		wantNotice
	}{
		{"a broken read", nil, true, "failed", true},
		{"a genuinely empty archive", nil, false, "ok", false},
		{"a healthy read", []string{"/work/alpha"}, false, "ok", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := renderDirectoryPicker(t, tc.seed, "", tc.failed)
			if !strings.Contains(out, `data-directories="`+tc.wantState+`"`) {
				t.Fatalf("want data-directories=%q.\nmarkup:\n%s", tc.wantState, out)
			}
			hasNotice := strings.Contains(out, "could not be loaded")
			if hasNotice != bool(tc.wantNotice) {
				t.Fatalf("failure notice present = %v, want %v.\nmarkup:\n%s",
					hasNotice, bool(tc.wantNotice), out)
			}
			// Non-fatal in EVERY case: the field still exists and still submits.
			if !strings.Contains(out, `name="directory"`) {
				t.Fatalf("the directory input is missing — a suggestion-list problem must "+
					"never remove the field.\nmarkup:\n%s", out)
			}
		})
	}
}

// wantNotice is a named bool so the table above reads at the call site rather
// than as a row of bare true/false.
type wantNotice bool
