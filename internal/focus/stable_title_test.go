package focus

import (
	"context"
	"testing"
	"unicode/utf8"
)

// STABLE, SHORT, NEVER-SESSION-TEXT TITLES, through the one writer (Refresh).
// Every fixture here is synthetic. Reasons are compared as LITERALS: the
// strings are what fleet_list and the database show, so a renamed constant that
// changed the text must fail here.

// titledFixture is a Refresher whose subject already has the focus prev, over
// a tail of the given entries, with summarizer s.
func titledFixture(prev string, tail []Entry, s Summarizer) (*Refresher, *memStore, Subject) {
	sub := AgentSubject(5)
	st := newMem()
	if prev != "" {
		st.rows[sub] = Focus{Subject: sub, Title: prev, Description: "Earlier step.", Source: SourceSummarizer, Fingerprint: "an-older-tail"}
	}
	src := &fakeSource{tails: map[Subject][]Entry{sub: tail}}
	return &Refresher{Store: st, Sessions: src, Summarizer: s}, st, sub
}

var shortTail = []Entry{{Role: "user", Text: "the nightly export job is failing again"}, {Role: "assistant", Text: "looking at the scheduler config"}}

// TestAnOffFormatAnswerKeepsThePreviousTitle: a model that ignores the
// Title/Description format — here a sentence in transcript voice — stores
// NOTHING: the session keeps its title, and the reason is recorded. Red
// before: the parser's fallback stored the sentence's first clause as the
// title.
func TestAnOffFormatAnswerKeepsThePreviousTitle(t *testing.T) {
	for _, answer := range []string{
		"The session is working on repairing the nightly export. The scheduler config is next.",
		"Repairing the nightly export\nThe scheduler config is next.",
	} {
		srv, c := server(t, 200, completion("m", scaledownContent(answer)))
		r, st, sub := titledFixture("Nightly export repair", shortTail, client(srv.URL))
		o, err := r.Refresh(context.Background(), 7, sub)
		if o != OutcomeFailed || err == nil {
			t.Fatalf("%q: outcome %s, err %v; want a failed refresh", answer, o, err)
		}
		if c.calls != 1 {
			t.Fatalf("%q: %d summarizer request(s), want 1", answer, c.calls)
		}
		if got := st.rows[sub].Title; got != "Nightly export repair" {
			t.Errorf("%q: the title became %q; the previous one must be kept", answer, got)
		}
		if got := st.errs[sub]; got != "the summarizer ignored the format" {
			t.Errorf("%q: recorded reason %q", answer, got)
		}
	}
}

// TestALongTitleIsCutToSixtyAtAWordBoundary: the 60-character bound is
// enforced in code, not only asked for — a 90-character title is stored cut at
// the last whole word inside 60, with no ellipsis. Red before: stored whole.
func TestALongTitleIsCutToSixtyAtAWordBoundary(t *testing.T) {
	long := "Repairing the nightly export scheduler and backfilling the three missed partitions"
	if n := utf8.RuneCountInString(long); n <= 60 {
		t.Fatalf("instrument check: the fixture is %d runes, want > 60", n)
	}
	sm := &fakeSummarizer{sum: Summary{Title: long, Description: "Backfill is running."}}
	r, st, sub := titledFixture("", shortTail, sm)
	if o, err := r.Refresh(context.Background(), 1, sub); err != nil || o != OutcomeUpdated {
		t.Fatalf("refresh: %s %v", o, err)
	}
	const want = "Repairing the nightly export scheduler and backfilling the" // 58 runes: the cut fell inside "three"
	if got := st.rows[sub].Title; got != want {
		t.Errorf("stored title %q (%d runes), want %q", got, utf8.RuneCountInString(got), want)
	}
}

// TestAMarkdownTitleOrACodeFenceIsRefused: markdown in a title, or a code
// fence anywhere, is the session's own text, not a name for it: refused, the
// previous title kept, the reason recorded. Red before: stored as written.
func TestAMarkdownTitleOrACodeFenceIsRefused(t *testing.T) {
	for _, sum := range []Summary{
		{Title: "Second pass, both findings **minor**", Description: "Closing out."},
		{Title: "Using `make lint` for the gate", Description: "Wiring it."},
		{Title: "## Export repair", Description: "Wiring it."},
		{Title: "- Export repair", Description: "Wiring it."},
		{Title: "Export repair", Description: "```\nexit 1\n```"},
	} {
		r, st, sub := titledFixture("Nightly export repair", shortTail, &fakeSummarizer{sum: sum})
		if o, _ := r.Refresh(context.Background(), 1, sub); o != OutcomeFailed {
			t.Errorf("%+v: outcome %s, want failed", sum, o)
		}
		if got := st.rows[sub].Title; got != "Nightly export repair" {
			t.Errorf("%+v: the title became %q", sum, got)
		}
		if got := st.errs[sub]; got != "the summarizer repeated the session's text" {
			t.Errorf("%+v: recorded reason %q", sum, got)
		}
	}
}

// verbatimTail carries an assistant sentence a runaway model might copy.
var verbatimTail = []Entry{
	{Role: "user", Text: "please review the second audit round"},
	{Role: "assistant", Text: "Second round came back with two findings, both minor, and the reviewer marked the first one as acceptable as disclosed."},
}

// TestAnAnswerThatIsTheTranscriptVerbatimIsRefused: a title or a description
// that is mostly the tail word for word is refused, the previous title kept.
// The CONTROL: an answer in the summarizer's own words that shares a short
// phrase with the tail is accepted — the test is about copying, not about
// naming the same thing. Red before: the copies were stored.
func TestAnAnswerThatIsTheTranscriptVerbatimIsRefused(t *testing.T) {
	for _, sum := range []Summary{
		{Title: "Second round came back with two findings, both minor", Description: "Closing the audit."},
		{Title: "Audit follow-up", Description: "Second round came back with two findings, both minor, and the reviewer marked the first one as acceptable."},
	} {
		r, st, sub := titledFixture("Audit follow-up work", verbatimTail, &fakeSummarizer{sum: sum})
		if o, _ := r.Refresh(context.Background(), 1, sub); o != OutcomeFailed {
			t.Errorf("%+v: outcome %s, want failed", sum, o)
		}
		if got := st.rows[sub].Title; got != "Audit follow-up work" {
			t.Errorf("%+v: the title became %q", sum, got)
		}
		if got := st.errs[sub]; got != "the summarizer repeated the session's text" {
			t.Errorf("%+v: recorded reason %q", sum, got)
		}
	}
	own := Summary{Title: "Closing out the second audit round", Description: "Two minor findings remain; one is accepted as disclosed."}
	r, st, sub := titledFixture("Audit follow-up work", verbatimTail, &fakeSummarizer{sum: own})
	if o, err := r.Refresh(context.Background(), 1, sub); o != OutcomeUpdated || err != nil {
		t.Fatalf("control: an answer in its own words was refused: %s %v", o, err)
	}
	if got := st.rows[sub].Title; got != own.Title {
		t.Errorf("control: stored %q", got)
	}
}

// TestAKeptTitleIsStoredByteIdentical: a model that keeps the title but
// changes its case or punctuation stores the PREVIOUS title exactly, so the
// session's name does not flicker. Red before: the re-cased title was stored.
func TestAKeptTitleIsStoredByteIdentical(t *testing.T) {
	sm := &fakeSummarizer{sum: Summary{Title: "nightly export repair.", Description: "Backfill is running."}}
	r, st, sub := titledFixture("Nightly Export Repair", shortTail, sm)
	if o, err := r.Refresh(context.Background(), 1, sub); err != nil || o != OutcomeUpdated {
		t.Fatalf("refresh: %s %v", o, err)
	}
	if got := st.rows[sub].Title; got != "Nightly Export Repair" {
		t.Errorf("stored title %q, want the previous one byte for byte", got)
	}
	if got := st.rows[sub].Description; got != "Backfill is running." {
		t.Errorf("the description %q was not updated", got)
	}
}
