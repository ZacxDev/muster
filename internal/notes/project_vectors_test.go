package notes

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// SHARED GOLDEN VECTORS — the cross-language contract for `project:<slug>`.
//
// The project rule has to be spelled once per LANGUAGE that decides it: a
// browser must decide before the wire and the server must decide again on
// arrival. Two implementations in two languages is unavoidable. What is NOT
// acceptable is that they disagree, which is exactly what shipped once: `café`
// was a loud 400 server-side and a SILENT drop client-side, `İ` and a U+0085 NEL
// were accepted server-side and dropped client-side, and a U+FEFF BOM was
// accepted client-side and 400'd server-side.
//
// testdata/project-normalization-vectors.json is the ONE place that behaviour is
// written down. It sits at the MODULE ROOT rather than beside this package so a
// future second-language implementation is driven from those bytes rather than
// from a copy of them.
//
// ⚠ ONLY THE GO SIDE EXISTS IN THIS MODULE TODAY, so the cross-language half of
// the gate is currently unarmed — this test alone cannot catch a divergence,
// because there is nothing to diverge from. It is still worth having: it pins
// the AUTHORITY's behaviour, which is the thing a second implementation will
// have to be made to match. The four divergence rows are kept for the same
// reason — they record properties of Go versus JavaScript (simple versus full
// case folding, what each calls whitespace), not properties of any one
// codebase.
// ---------------------------------------------------------------------------

// projectVector is one row of the shared table. Field docs live in the fixture's
// own `_readme`, so the contract is described where the data is.
type projectVector struct {
	Input      string `json:"input"`
	Normalized string `json:"normalized"`
	Accepted   bool   `json:"accepted"`
	Why        string `json:"why"`
}

// ProjectVectorsPath is the fixture location relative to this package. It is a
// helper rather than a literal so a move breaks in ONE place.
func projectVectorsPath(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "..", "testdata", "project-normalization-vectors.json")
}

func loadProjectVectors(t *testing.T) []projectVector {
	t.Helper()
	p := projectVectorsPath(t)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read shared golden vectors %s: %v", p, err)
	}
	var doc struct {
		Vectors []projectVector `json:"vectors"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse %s: %v", p, err)
	}
	if len(doc.Vectors) == 0 {
		t.Fatalf("%s carries no vectors — an empty table would make this whole gate vacuous", p)
	}
	return doc.Vectors
}

// TestProjectNormalizationGoldenVectors pins the SERVER half of the shared
// contract. The server is the AUTHORITY: whatever it does here is what the
// browser must be made to agree with, not the other way round.
func TestProjectNormalizationGoldenVectors(t *testing.T) {
	for _, v := range loadProjectVectors(t) {
		v := v
		t.Run(v.Input, func(t *testing.T) {
			// The browser normalizes a bare SLUG; the server normalizes a whole
			// TAG. Concatenating first is exactly what goes on the wire.
			got := NormalizeTag(NSProject + ":" + v.Input)
			if got != v.Normalized {
				t.Fatalf("NormalizeTag(%q) = %q, want %q (%s)", NSProject+":"+v.Input, got, v.Normalized, v.Why)
			}
			err := ValidateTags([]string{got})
			if v.Accepted && err != nil {
				t.Fatalf("ValidateTags(%q) = %v, want accepted (%s)", got, err, v.Why)
			}
			if !v.Accepted && err == nil {
				t.Fatalf("ValidateTags(%q) accepted it, want REJECTED (%s)", got, v.Why)
			}
			// A rejection must NAME the offending tag — a silently ignored
			// routing tag is the failure mode this feature exists to avoid.
			// The errors embed the tag with %q, so look for that exact rendering.
			if !v.Accepted && got != "" && !strings.Contains(err.Error(), fmt.Sprintf("%q", truncTag(got))) {
				t.Fatalf("rejection %q does not name the tag %q (%s)", err.Error(), got, v.Why)
			}
		})
	}
}

// TestProjectGoldenVectorsCoverTheDivergenceRows makes the table itself
// non-vacuous: a future edit that deletes the four measured cross-language
// divergence rows (and thereby makes the cross-language gate pass trivially) has
// to delete this assertion too, which is a visible act rather than a quiet one.
func TestProjectGoldenVectorsCoverTheDivergenceRows(t *testing.T) {
	// Written with \u escapes ON PURPOSE: two of these four inputs are
	// INVISIBLE, and a vector you cannot see in a diff is a vector nobody can
	// review.
	want := map[string]bool{
		"caf\u00e9": false, // server 400s; the browser used to drop it in SILENCE
		"\u0130":    true,  // Go simple-lowercase -> "i"; JS full-lowercase -> "i"+U+0307
		"a\u0085b":  true,  // U+0085 NEL is Go whitespace; JS \\s does NOT match it
		"a\ufeffb":  false, // U+FEFF is NOT Go whitespace; JS \\s DOES match it
	}
	seen := map[string]bool{}
	for _, v := range loadProjectVectors(t) {
		if acc, ok := want[v.Input]; ok {
			if v.Accepted != acc {
				t.Fatalf("vector %q: accepted = %v, want %v — this row is one of the four MEASURED divergences and its expectation must not be flipped to make a normalizer pass",
					v.Input, v.Accepted, acc)
			}
			seen[v.Input] = true
		}
	}
	for in := range want {
		if !seen[in] {
			t.Fatalf("the shared vector table no longer covers the measured divergence %q", in)
		}
	}
}
