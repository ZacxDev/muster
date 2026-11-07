package asklog

import "testing"

// TestNormaliseLiteralPairs: the normal form, by literal input/output pairs —
// never computed from Normalise itself. Each pair exercises one rule, and the
// placeholders are checked to replace only their own token.
func TestNormaliseLiteralPairs(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"Shall I continue?", "shall i continue"},
		{"  Shall   I\tcontinue?\n", "shall i continue"},
		// The last paragraph only: the report before the question is dropped.
		{"I ran the tests and they pass.\n\nShall I merge #482?", "shall i merge {pr}"},
		{"Done.\n \nReady to merge https://github.com/acme/widgets/pull/17 ?", "ready to merge {pr}"},
		{"Push to `feature/x` now?", "push to {code} now"},
		{"Edit internal/api/ask_log.go next?", "edit {path} next"},
		{"Reset to 3f9a2c1e and retry?", "reset to {sha} and retry"},
		// A hex run with no digit is a word, and a digit run with no letter a
		// number — neither is a sha.
		{"The defaced file again?", "the defaced file again"},
		{"Retry 1234567 times?", "retry {n} times"},
		{"Run step 3 of 12?", "run step {n} of {n}"},
		// Leading and trailing punctuation goes; a placeholder's braces stay.
		{"**Should I open #9?**", "should i open {pr}"},
		{"— want me to push `main`?", "want me to push {code}"},
		// NFC: a decomposed é is the composed one.
		{"Café or not?", "café or not"},
		{"", ""},
	} {
		if got := Normalise(c.in); got != c.want {
			t.Errorf("Normalise(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestPrefilterLiteralTable: the pre-filter keeps a last paragraph holding a
// '?' or opening with one of the design's phrases, and drops everything else —
// including a '?' that is only in an EARLIER paragraph (the report, not the
// ask).
func TestPrefilterLiteralTable(t *testing.T) {
	kept := []string{
		"Shall I continue?",
		"All green.\n\nShall I merge it?",
		"shall i go ahead and push",
		"Should I open the pull request",
		"Want me to rebase first.",
		"**Do you want** the long version",
		"Ready to merge when you are",
		"Let me know if the copy works for you",
		"- Let me know which one",
	}
	dropped := []string{
		"",
		"Done. The tests pass.",
		"Was it flaky? It was not.\n\nThe suite is green now.",
		"I will merge it when CI is green.",
		"Readying the release notes.",
		"Should've been caught earlier, fixed now.",
	}
	for _, s := range kept {
		if !Prefilter(s) {
			t.Errorf("Prefilter(%q) dropped it, want kept", s)
		}
	}
	for _, s := range dropped {
		if Prefilter(s) {
			t.Errorf("Prefilter(%q) kept it, want dropped", s)
		}
	}
}

// TestLastParagraph: blank lines (with stray spaces) split paragraphs; CRLF is
// a newline.
func TestLastParagraph(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"a\n\nb", "b"},
		{"a\r\n\r\nb\r\n", "b"},
		{"a\n \t\nb\nc\n\n\n", "b\nc"},
		{"only", "only"},
		{"\n\n", ""},
	} {
		if got := LastParagraph(c.in); got != c.want {
			t.Errorf("LastParagraph(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
