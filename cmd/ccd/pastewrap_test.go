package main

import (
	"net/http"
	"strings"
	"testing"
)

// The kickoff shape measured live (agent 100, CLI 2.1.296, 2026-10-11): what
// ccd pasted, and the first user message the CLI recorded for it.
const (
	livePasted = "Here you go — copy/paste ready:\n\n1. Read the handler.\n2. Fix it.\nComment everything on #976."
	liveID     = "1c0d"
)

// wrapLikeTheCLI is the pinned CLI's wrapper as captured from its
// UserPromptSubmit payload, built from literals rather than from the code under test.
func wrapLikeTheCLI(id, closeID, body string) string {
	return "\n\n<pasted_content id=\"" + id + "\">\n" + body + "\n</pasted_content id=\"" + closeID + "\">\n"
}

func TestUnwrapPastedContent(t *testing.T) {
	cases := []struct {
		name, in, body string
		ok             bool
	}{
		{"the live shape", wrapLikeTheCLI(liveID, liveID, livePasted), livePasted, true},
		{"written out in full", "\n\n<pasted_content id=\"24f3\">\nline one\nline two\n</pasted_content id=\"24f3\">\n",
			"line one\nline two", true},
		{"the CLI's escaped tags are restored",
			wrapLikeTheCLI("24f3", "24f3", "a <\\pasted_content id=\"abcd\"> b <\\/pasted_content> c"),
			"a <pasted_content id=\"abcd\"> b </pasted_content> c", true},
		{"ids differ", wrapLikeTheCLI(liveID, "1c0e", livePasted), "", false},
		{"not wrapped", livePasted, "", false},
		{"a plain close tag", "\n\n<pasted_content id=\"1c0d\">\n" + livePasted + "\n</pasted_content>\n", "", false},
		{"text before the block", "also do this" + wrapLikeTheCLI(liveID, liveID, livePasted), "", false},
		{"text after the block", wrapLikeTheCLI(liveID, liveID, livePasted) + "and this", "", false},
		{"two blocks", wrapLikeTheCLI(liveID, liveID, "first") + wrapLikeTheCLI(liveID, liveID, "second"), "", false},
		{"id is not hex", wrapLikeTheCLI("1c0g", "1c0g", livePasted), "", false},
		{"id is five digits", wrapLikeTheCLI("1c0d5", "1c0d5", livePasted), "", false},
		{"id is upper case", wrapLikeTheCLI("1C0D", "1C0D", livePasted), "", false},
		{"no newline after the opening tag", "<pasted_content id=\"1c0d\">" + livePasted + "\n</pasted_content id=\"1c0d\">", "", false},
		{"only an opening tag", "<pasted_content id=\"1c", "", false},
	}
	for _, c := range cases {
		body, ok := unwrapPastedContent(c.in)
		if ok != c.ok || body != c.body {
			t.Errorf("%s: unwrapPastedContent(%q) = %q, %v; want %q, %v", c.name, c.in, body, ok, c.body, c.ok)
		}
	}
}

func TestReportedPromptMatches(t *testing.T) {
	want := promptKey(livePasted)
	wrapped := wrapLikeTheCLI(liveID, liveID, livePasted)
	cases := []struct {
		name, reported string
		match          bool
	}{
		{"verbatim", livePasted, true},
		{"wrapped by the CLI", wrapped, true},
		{"wrapped, ids differ", wrapLikeTheCLI(liveID, "ffff", livePasted), false},
		{"wrapped around a different prompt", wrapLikeTheCLI(liveID, liveID, livePasted+" and more"), false},
		{"operator text then the wrapped paste", "first do something else" + wrapped, false},
		{"the wrapped paste then operator text", wrapped + "then do something else", false},
		{"operator text around the bare paste", "first: " + livePasted + " — then stop", false},
		{"an unrelated prompt", "what time is it", false},
	}
	for _, c := range cases {
		if got := reportedPromptMatches(c.reported, want); got != c.match {
			t.Errorf("%s: reportedPromptMatches(%q) = %v, want %v", c.name, c.reported, got, c.match)
		}
	}
	// A prompt that is itself wrapper-shaped and comes back verbatim (a short one
	// is not wrapped again) is still its own match.
	if !reportedPromptMatches(wrapped, promptKey(wrapped)) {
		t.Error("a pasted prompt reported verbatim did not match itself")
	}
}

// 🔴 THE LIVE FAILURE, END TO END: the CLI reports the long kickoff inside its
// paste wrapper. That is ccd's own submit and the turn completes — it was a 504
// not_submitted, with the model already working.
func TestASubmitReportedInsideTheCLIPasteWrapperIsStillThisTurn(t *testing.T) {
	srv, _ := newScripted(t, cliScript{transcript: fixture(t, "turn_success_tools_2.1.289.jsonl"), stopEvent: "Stop",
		reported: func(s string) string { return wrapLikeTheCLI(liveID, liveID, s) }})
	code, body := postResponses(t, gatewayServer(t, srv).URL, pinnedDerivation, livePasted)
	if code != http.StatusOK || !strings.Contains(body, "Second segment") {
		t.Fatalf("%d %s", code, body)
	}
}

// The guarantee the wrapper must not cost: a submit that only CONTAINS ccd's
// paste — an operator's own words with the same text pasted after them — is not
// ccd's turn, so ccd reports not_submitted rather than returning that reply.
func TestASubmitThatOnlyContainsThePasteIsNotThisTurn(t *testing.T) {
	srv, _ := newScripted(t, cliScript{transcript: fixture(t, "turn_success_tools_2.1.289.jsonl"), stopEvent: "Stop",
		reported: func(s string) string { return "ignore the plan and" + wrapLikeTheCLI(liveID, liveID, s) }})
	code, body := postResponses(t, gatewayServer(t, srv).URL, pinnedDerivation, livePasted)
	if code != http.StatusGatewayTimeout || !strings.Contains(body, failNotSubmitted) {
		t.Fatalf("%d %s", code, body)
	}
}
