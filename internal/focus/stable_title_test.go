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

// TestAMarkdownTitleOrACodeFenceIsRefused: an answer in markdown — a code
// fence anywhere, or a title that is a heading, **bold** or a [link](…) — is
// refused, the previous title kept, and the reason is its OWN ("answered in
// markdown"), not "repeated the session's text". Red before: stored as
// written; and at 1e4636b4 the reason was the copy one.
func TestAMarkdownTitleOrACodeFenceIsRefused(t *testing.T) {
	for _, sum := range []Summary{
		{Title: "Second pass, both findings **minor**", Description: "Closing out."},
		{Title: "## Export repair", Description: "Wiring it."},
		{Title: "Export repair per [the runbook](https://example.invalid/rb)", Description: "Wiring it."},
		{Title: "Export repair", Description: "```\nexit 1\n```"},
	} {
		r, st, sub := titledFixture("Nightly export repair", shortTail, &fakeSummarizer{sum: sum})
		if o, _ := r.Refresh(context.Background(), 1, sub); o != OutcomeFailed {
			t.Errorf("%+v: outcome %s, want failed", sum, o)
		}
		if got := st.rows[sub].Title; got != "Nightly export repair" {
			t.Errorf("%+v: the title became %q", sum, got)
		}
		if got := st.errs[sub]; got != "the summarizer answered in markdown" {
			t.Errorf("%+v: recorded reason %q", sum, got)
		}
	}
}

// TestCodeNamesAndListMarkersAreNotMarkdown: backticks around a name and a
// leading list marker are REMOVED, not refused, and "__init__" is a name, not
// emphasis. Red at 1e4636b4: all three were refused as "repeated the session's
// text".
func TestCodeNamesAndListMarkersAreNotMarkdown(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"Rework `ParseSummary` format", "Rework ParseSummary format"},
		{"- Export repair", "Export repair"},
		{"Fix __init__ circular import", "Fix __init__ circular import"},
	} {
		r, st, sub := titledFixture("Nightly export repair", shortTail, &fakeSummarizer{sum: Summary{Title: c.in, Description: "Wiring it."}})
		if o, err := r.Refresh(context.Background(), 1, sub); o != OutcomeUpdated || err != nil {
			t.Errorf("%q: %s %v, want updated", c.in, o, err)
		}
		if got := st.rows[sub].Title; got != c.want {
			t.Errorf("%q: stored %q, want %q", c.in, got, c.want)
		}
	}
}

// verbatimTail carries an assistant sentence a runaway model might copy.
var verbatimTail = []Entry{
	{Role: "user", Text: "please review the second audit round"},
	{Role: "assistant", Text: "Second round came back with two findings, both minor, and the reviewer marked the first one as acceptable as disclosed."},
}

// TestAnAnswerThatIsTheTranscriptVerbatimIsRefused: a title that is mostly
// the tail's assistant lines word for word is refused, the previous title kept.
// The CONTROL: an answer in the summarizer's own words that shares a short
// phrase with the tail is accepted — the test is about copying, not about
// naming the same thing. Red before: the copies were stored.
func TestAnAnswerThatIsTheTranscriptVerbatimIsRefused(t *testing.T) {
	for _, sum := range []Summary{
		{Title: "Second round came back with two findings, both minor", Description: "Closing the audit."},
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

// TestACopiedDescriptionIsDroppedAndTheTitleKept: a description that is
// mostly a reply word for word is dropped — the title, in the summarizer's own
// words, is stored. Red at 1e4636b4: the whole answer was refused, so a
// session with no title got none.
func TestACopiedDescriptionIsDroppedAndTheTitleKept(t *testing.T) {
	sm := &fakeSummarizer{sum: Summary{Title: "Audit follow-up", Description: "Second round came back with two findings, both minor, and the reviewer marked the first one as acceptable."}}
	r, st, sub := titledFixture("", verbatimTail, sm)
	if o, err := r.Refresh(context.Background(), 1, sub); o != OutcomeUpdated || err != nil {
		t.Fatalf("refresh: %s %v, want updated", o, err)
	}
	if got := st.rows[sub]; got.Title != "Audit follow-up" || got.Description != "" {
		t.Errorf("stored (%q, %q), want (\"Audit follow-up\", \"\")", got.Title, got.Description)
	}
}

// TestThePersonsOwnWordsAreNotACopy: the copy test judges only the tail's
// ASSISTANT lines. A title naming the job in the words the person asked for it
// with is accepted. Red at 1e4636b4: refused (the whole tail was judged).
func TestThePersonsOwnWordsAreNotACopy(t *testing.T) {
	tail := []Entry{{Role: "user", Text: "please fix the flaky upload test in the importer worker"}, {Role: "assistant", Text: "looking now"}}
	sm := &fakeSummarizer{sum: Summary{Title: "Fix the flaky upload test in importer worker", Description: "Reading the retry loop."}}
	r, st, sub := titledFixture("", tail, sm)
	if o, err := r.Refresh(context.Background(), 1, sub); o != OutcomeUpdated || err != nil {
		t.Fatalf("refresh: %s %v, want updated", o, err)
	}
	if got := st.rows[sub].Title; got != "Fix the flaky upload test in importer worker" {
		t.Errorf("stored %q", got)
	}
}

// TestAKeptTitleIsNeverJudgedACopy: the title the summarizer was asked to keep
// word for word is kept even when a reply repeats it. Red at 1e4636b4: refused
// as a copy, though it was exactly what the prompt asked for.
func TestAKeptTitleIsNeverJudgedACopy(t *testing.T) {
	prev := "Stable focus titles with fed back previous title"
	tail := []Entry{{Role: "user", Text: "status?"}, {Role: "assistant", Text: "The stable focus titles with fed back previous title work is done."}}
	sm := &fakeSummarizer{sum: Summary{Title: prev, Description: "Done; waiting on review."}}
	r, st, sub := titledFixture(prev, tail, sm)
	if o, err := r.Refresh(context.Background(), 1, sub); o != OutcomeUpdated || err != nil {
		t.Fatalf("refresh: %s %v, want updated", o, err)
	}
	if got := st.rows[sub].Title; got != prev {
		t.Errorf("stored %q, want the kept title", got)
	}
}

// refusing is a summarizer whose answer is always refused for its content
// (markdown), counting its calls.
func refusing() *fakeSummarizer {
	return &fakeSummarizer{sum: Summary{Title: "## Export repair", Description: "Wiring it."}}
}

// TestARefusedTailIsNotSentAgainUntilItChanges: an answer refused for its
// CONTENT records the tail it was for, and that tail is not sent again: 3
// refreshes over one fixed tail make 1 call. A changed tail is sent. The
// control: a TRANSPORT failure says nothing about the tail and is retried
// every time (3 calls). Red at 1e4636b4: 3 calls for the refused tail.
func TestARefusedTailIsNotSentAgainUntilItChanges(t *testing.T) {
	sm := refusing()
	r, _, sub := titledFixture("Nightly export repair", shortTail, sm)
	for i := 0; i < 3; i++ {
		_, _ = r.Refresh(context.Background(), 1, sub)
	}
	if sm.calls != 1 {
		t.Errorf("a refused, unchanged tail cost %d calls over 3 refreshes, want 1", sm.calls)
	}
	r.Sessions.(*fakeSource).tails[sub] = append(append([]Entry(nil), shortTail...), Entry{Role: "assistant", Text: "found the cron typo"})
	_, _ = r.Refresh(context.Background(), 1, sub)
	if sm.calls != 2 {
		t.Errorf("a changed tail was not sent: %d calls, want 2", sm.calls)
	}
	down := &fakeSummarizer{err: ErrUnreachable}
	r2, _, sub2 := titledFixture("Nightly export repair", shortTail, down)
	for i := 0; i < 3; i++ {
		_, _ = r2.Refresh(context.Background(), 1, sub2)
	}
	if down.calls != 3 {
		t.Errorf("a transport failure was retried %d times over 3 refreshes, want 3", down.calls)
	}
}
