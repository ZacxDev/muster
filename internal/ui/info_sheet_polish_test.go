package ui

import (
	"strings"
	"testing"
)

// TestSentenceStartCapitalisesAFragmentNotAName: the v0.250.0 phone check
// (F4) found admin-only ⓘ sentences starting lower-case ("the VAPID keys are
// unset": a feature's reason). SentenceStart capitalises such a fragment and
// keeps a first word that is a name as written. Its use on the feature sheet
// is pinned in internal/api (TestAFeatureSheetsReasonAndNoteStartWithACapital).
//
// INVARIANT GUARD (not a regression test): a sheet's Lead is drawn exactly as
// written — a lead may start with a host or a command ("atlas runs …").
func TestSentenceStartCapitalisesAFragmentNotAName(t *testing.T) {
	got := renderString(t, InfoSheet{ID: "where", Label: "Where", Heading: "Where it runs",
		Lead: "atlas runs this session in tmux:", Code: "tmux fern:1 @ atlas"}.Node())
	if !strings.Contains(got, `<p class="text-sm text-fg2">atlas runs this session in tmux:</p>`) {
		t.Errorf("the lead is not drawn as written:\n%s", got)
	}
	for in, want := range map[string]string{
		"the VAPID keys are unset":       "The VAPID keys are unset",
		"on each host, run the mirror":   "On each host, run the mirror",
		"émigré hosts are listed":        "Émigré hosts are listed",
		"The VAPID keys are unset":       "The VAPID keys are unset",
		"MUSTER_EXAMPLE_PUSH is unset":   "MUSTER_EXAMPLE_PUSH is unset",
		"muster shows agents in tmux":    "muster shows agents in tmux",
		"muster's own GitHub identity":   "muster's own GitHub identity",
		"muster-mirror runs on each one": "muster-mirror runs on each one",
		"attention_ask over MCP":         "attention_ask over MCP",
		"iOS needs a home-screen app":    "iOS needs a home-screen app",
		"":                               "",
		"(a note)":                       "(a note)",
	} {
		if got := SentenceStart(in); got != want {
			t.Errorf("SentenceStart(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestAnInfoSheetsCodeWrapsBetweenWordsFirst: the same check found code rows
// breaking mid-word ("MUSTER_GITHUB_APP_ACCOUNTS=<lo" / "gin:id,…>"): the
// <code> was break-all, which breaks at the row's last column whatever is
// there. It is overflow-wrap:anywhere now — a space or a hyphen first, inside
// a token only when the token is wider than the row — on both code shapes.
// Red at origin/main (break-all on both).
func TestAnInfoSheetsCodeWrapsBetweenWordsFirst(t *testing.T) {
	for name, s := range map[string]InfoSheet{
		"one line":  {ID: "a", Label: "A", Heading: "A", Code: "tmux fern:1 @ atlas"},
		"multiline": {ID: "b", Label: "B", Heading: "B", Code: "MUSTER_EXAMPLE_A=1\nMUSTER_EXAMPLE_B=<a value>", Multiline: true},
	} {
		got := renderString(t, s.Node())
		i := strings.Index(got, "<code ")
		if i < 0 {
			t.Fatalf("%s: no code row:\n%s", name, got)
		}
		tag := got[i : i+strings.Index(got[i:], ">")]
		if strings.Contains(tag, "break-all") || !strings.Contains(tag, " [overflow-wrap:anywhere] ") {
			t.Errorf("%s: the code does not wrap between words first: %s", name, tag)
		}
	}
}
