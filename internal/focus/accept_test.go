package focus

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestThePreviousTitleIsFedBack: a refresh hands the summarizer the session's
// current title to keep — but only one this format could have produced: none
// on a first refresh, none for a legacy runaway (past the 60-rune cap, or
// markdown), so a model is never told to keep transcript prose.
func TestThePreviousTitleIsFedBack(t *testing.T) {
	for _, c := range []struct {
		name, prev, want string
	}{
		{"first refresh", "", ""},
		{"a title to keep", "Nightly export repair", "Nightly export repair"},
		{"exactly at the cap", strings.Repeat("abcd ", 11) + "abcde", strings.Repeat("abcd ", 11) + "abcde"},
		{"a legacy long title", strings.Repeat("word ", 20) + "end", ""},
		{"a legacy markdown title", "Third round, both **low**", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.name == "exactly at the cap" && utf8.RuneCountInString(c.prev) != TitleCapRunes {
				t.Fatalf("instrument check: %d runes", utf8.RuneCountInString(c.prev))
			}
			sm := &fakeSummarizer{sum: Summary{Title: "Nightly export repair", Description: "Backfill is running."}}
			r, _, sub := titledFixture(c.prev, shortTail, sm)
			if _, err := r.Refresh(context.Background(), 1, sub); err != nil {
				t.Fatal(err)
			}
			if len(sm.reqs) != 1 {
				t.Fatalf("%d summarizer call(s)", len(sm.reqs))
			}
			if sm.reqs[0].PrevTitle != c.want {
				t.Errorf("PrevTitle = %q, want %q", sm.reqs[0].PrevTitle, c.want)
			}
			if !strings.Contains(sm.reqs[0].Tail, "nightly export job") {
				t.Errorf("the tail did not reach the summarizer: %q", sm.reqs[0].Tail)
			}
		})
	}
}

// TestTheSystemMessageCarriesThePreviousTitle: on the wire, the previous title
// rides in the SYSTEM message, quoted, with the keep-unless-the-goal-changed
// instruction pinned whole; with none, the system message is the bare prompt.
func TestTheSystemMessageCarriesThePreviousTitle(t *testing.T) {
	for _, c := range []struct{ prev, want string }{
		{"Nightly export repair", SummaryInstructions + "\nThe session's current title is \"Nightly export repair\". Keep exactly that title, " +
			"word for word, unless the session's overall goal has changed; a new step of the same task is not a new goal."},
		{"", SummaryInstructions},
	} {
		srv, cap := server(t, 200, completion("m", scaledownContent("Title: Nightly export repair\nDescription: Backfilling.")))
		if _, err := client(srv.URL).Summarize(context.Background(), 7, Request{Tail: "user: go", PrevTitle: c.prev}); err != nil {
			t.Fatal(err)
		}
		msgs, _ := cap.body["messages"].([]any)
		if len(msgs) != 2 {
			t.Fatalf("messages = %v", msgs)
		}
		sys, _ := msgs[0].(map[string]any)
		if sys["role"] != "system" || sys["content"] != c.want {
			t.Errorf("prev %q: system message = %q\nwant %q", c.prev, sys["content"], c.want)
		}
		if last, _ := msgs[1].(map[string]any); last["content"] != "user: go" {
			t.Errorf("prev %q: the tail is not the last message: %v", c.prev, last)
		}
	}
}

// TestTheModelIsConfigurable: ScaleDown.Model is the model asked for; unset,
// it is scaledown/summarize — the model focus always used, so nothing changes
// until an operator picks one.
func TestTheModelIsConfigurable(t *testing.T) {
	for _, c := range []struct{ model, want string }{{"", "scaledown/summarize"}, {"example/model-x", "example/model-x"}} {
		srv, cap := server(t, 200, completion("m", "Title: Nightly export repair"))
		sd := client(srv.URL)
		sd.Model = c.model
		if _, err := sd.Summarize(context.Background(), 7, Request{Tail: "t"}); err != nil {
			t.Fatal(err)
		}
		if cap.body["model"] != c.want {
			t.Errorf("Model %q: asked for %v, want %q", c.model, cap.body["model"], c.want)
		}
	}
	if DefaultModel != "scaledown/summarize" {
		t.Errorf("DefaultModel = %q", DefaultModel)
	}
}

// TestCapTitle: the cut is at the last whole word inside 60 runes, measured in
// RUNES (multi-byte fixtures), never leaves a dangling ellipsis or punctuation,
// and a single word longer than the cap is cut at the cap.
func TestCapTitle(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"Short title", "Short title"},
		{"Trailing ellipsis…", "Trailing ellipsis"},
		{"Trailing dots...", "Trailing dots"},
		// 59 runes of é-words, then a word that crosses 60: cut before it.
		{strings.Repeat("ééééé ", 10) + "crossing", strings.TrimSpace(strings.Repeat("ééééé ", 10))},
		// The 61st rune is a space: the first 60 runes are whole words.
		{strings.Repeat("abcde ", 9) + "abcdef next", strings.Repeat("abcde ", 9) + "abcdef"},
		// The cut lands after a comma: the comma goes too.
		{strings.Repeat("abcd ", 11) + "ab, longword", strings.Repeat("abcd ", 11) + "ab"},
		{strings.Repeat("x", 75), strings.Repeat("x", 60)},
		{"Ends at a comma, " + strings.Repeat("wordy ", 10), "Ends at a comma, wordy wordy wordy wordy wordy wordy wordy"},
	} {
		got := CapTitle(c.in)
		if got != c.want {
			t.Errorf("CapTitle(%q) = %q, want %q", c.in, got, c.want)
		}
		if utf8.RuneCountInString(got) > 60 {
			t.Errorf("CapTitle(%q) is %d runes", c.in, utf8.RuneCountInString(got))
		}
	}
	if TitleCapRunes != 60 {
		t.Errorf("TitleCapRunes = %d, want 60", TitleCapRunes)
	}
}

// TestTheNewReasonsAreFixedAndDistinct: a format refusal and a verbatim
// refusal each record their own short reason — not the generic "unusable" —
// even wrapped.
func TestTheNewReasonsAreFixedAndDistinct(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{ErrFormat, "the summarizer ignored the format"},
		{errors.Join(errors.New("ctx"), ErrFormat), "the summarizer ignored the format"},
		{ErrTranscriptText, "the summarizer repeated the session's text"},
		{ErrMarkdown, "the summarizer answered in markdown"},
	} {
		if got := Reason(c.err); got != c.want {
			t.Errorf("Reason(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// TestShortAnswersAreNotJudgedVerbatim: below verbatimMinWords words an
// answer is never refused as a copy — a short title can share the phrase that
// asked for the work. Fixture: a five-word title found whole in the tail.
func TestShortAnswersAreNotJudgedVerbatim(t *testing.T) {
	tail := "assistant: fix the nightly export job please"
	if _, err := Accept(Summary{Title: "Fix the nightly export job"}, tail, ""); err != nil {
		t.Errorf("a five-word title was refused: %v", err)
	}
	if _, err := Accept(Summary{Title: "Fix the nightly export job please"}, tail, ""); !errors.Is(err, ErrTranscriptText) {
		t.Errorf("a six-word copy was accepted: %v", err)
	}
}
