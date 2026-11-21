package api

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/attention"
)

// TestStepUpLookalikeWords pins which words stepUpLookalike flags (the file
// header of stepup_lookalike.go), one rule per case, each on both sides.
func TestStepUpLookalikeWords(t *testing.T) {
	cases := []struct {
		word string
		want bool
	}{
		// Mixed scripts in one run of letters.
		{"pаypal", true},           // Latin + Cyrillic
		{"gοogle", true},           // Latin + Greek
		{"аb", true},               // Cyrillic first
		{"Café", false},            // Latin only, a precomposed letter
		{"Café", false},           // Latin with a combining acute (not an overlay)
		{"мир", false},             // Cyrillic only
		{"English/русский", false}, // two runs, one script each
		{"日本語のテキスト", false},        // Han + Hiragana + Katakana
		{"iPhoneを", false},         // Latin + Hiragana
		{"한국語abc", false},          // Hangul + Han + Latin
		{"abcаを", true},            // Latin + Cyrillic + Hiragana: no allowed set
		// Non-ASCII in a link-like word.
		{"café.com", true},
		{"https://exämple.org/a", true},
		{"@аррӏе", true},
		{"me@café.fr", true},
		{"example.com", false}, // ASCII
		{"naïve.", false},      // a sentence's end, not a domain
		{"“café”,", false},
		// Overlay marks, ASCII around them or not.
		{"S̶ALE", true},
		{"a⃒", true},
		{"plain", false},
		// Emoji and symbols.
		{"\U0001F389\U0001F44D\U0001F3FD", false},
		{"☕", false},
		{"—", false},
	}
	for _, c := range cases {
		if got := stepUpLookalike(c.word); got != c.want {
			t.Errorf("stepUpLookalike(%q) = %v, want %v", c.word, got, c.want)
		}
	}
}

// TestStepUpMarkedHTMLKeepsTheTextAndMarksOnlyFlaggedWords: every space is
// where it was, everything is escaped, and only the flagged word is marked.
func TestStepUpMarkedHTMLKeepsTheTextAndMarksOnlyFlaggedWords(t *testing.T) {
	got := stepUpMarkedHTML("  <b>pay</b> at  pаypal.com\nnow ")
	want := "  &lt;b&gt;pay&lt;/b&gt; at  " + stepUpMarkOpen + "pаypal.com</mark>\nnow "
	if got != want {
		t.Errorf("stepUpMarkedHTML\n got %q\nwant %q", got, want)
	}
}

// TestALongPreviewIsCutAndSaysSo: a preview that is not an X thread and is
// longer than a card holds keeps its first characters, ended "…", and says so
// in its own Excerpt line — never inside the preview. One that fits is left
// whole with no line, and so is an X thread that fits (its Parts counted).
func TestALongPreviewIsCutAndSaysSo(t *testing.T) {
	long := strings.Repeat("abcdefghij", 300) // 3000 characters
	c := &attention.StepUpCard{Preview: long}
	stepUpCutPreview(c)
	if n := utf8.RuneCountInString(c.Preview); n != 2600 || !strings.HasSuffix(c.Preview, "…") || strings.Contains(c.Preview, "EXCERPT") {
		t.Errorf("the cut preview: %d characters, ends %q", n, c.Preview[len(c.Preview)-10:])
	}
	if want := "⚠ AN EXCERPT: the first 2599 of 3000 characters. Refuse unless you have read the rest in muster's web UI."; c.Excerpt != want {
		t.Errorf("the excerpt line %q, want %q", c.Excerpt, want)
	}
	short := &attention.StepUpCard{Preview: "Short.", Platform: "x", Target: "post"}
	stepUpCutPreview(short)
	if short.Preview != "Short." || short.Excerpt != "" || short.Parts != 0 {
		t.Errorf("a short post: %+v", short)
	}
	thread := &attention.StepUpCard{Preview: "One.\n---8<---\nTwo.\n---8<---\nThree.", Platform: "x", Target: "post"}
	stepUpCutPreview(thread)
	if thread.Parts != 3 || thread.Excerpt != "" || thread.Preview != "One.\n---8<---\nTwo.\n---8<---\nThree." {
		t.Errorf("a thread that fits: %+v", thread)
	}
	// An X reply is one part, cut by characters.
	reply := &attention.StepUpCard{Preview: long, Platform: "x", Target: "reply"}
	stepUpCutPreview(reply)
	if reply.Parts != 0 || !strings.Contains(reply.Excerpt, "of 3000 characters") {
		t.Errorf("a long reply: parts %d, excerpt %q", reply.Parts, reply.Excerpt)
	}
}
