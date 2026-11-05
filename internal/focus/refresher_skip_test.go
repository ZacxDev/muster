package focus

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"
)

// readCounter is a source that counts tail reads per subject.
type readCounter struct {
	fakeSource
	reads map[Subject]int
}

func (c *readCounter) Tail(ctx context.Context, s Subject) ([]Entry, error) {
	if c.reads == nil {
		c.reads = map[Subject]int{}
	}
	c.reads[s]++
	return c.fakeSource.Tail(ctx, s)
}

func (c *readCounter) total(subs []Subject) int {
	n := 0
	for _, s := range subs {
		n += c.reads[s]
	}
	return n
}

// skipFixture is a refresher on a fixed clock at base, with an owner 1.
func skipFixture(base time.Time) (*Refresher, *memStore, *readCounter, *fakeSummarizer, *time.Time) {
	now := base
	st := newMem()
	st.owners = []int64{1}
	src := &readCounter{fakeSource: fakeSource{tails: map[Subject][]Entry{}}}
	sm := &fakeSummarizer{sum: Summary{Title: "Reworking the parser", Description: "Splitting the lexer out."}}
	r := &Refresher{Store: st, Sessions: src, Summarizer: sm, Now: func() time.Time { return now },
		AsOwner: func(ctx context.Context, _ int64) context.Context { return ctx }}
	return r, st, src, sm, &now
}

// unchangedRow is a stored focus whose fingerprint is entries' own: reading
// the tail again finds nothing new (the unchanged outcome).
func unchangedRow(sub Subject, entries []Entry, updated time.Time) Focus {
	_, fp := RenderTail(entries)
	return Focus{Subject: sub, Title: "Settled work " + sub.Key, Source: SourceSummarizer, UpdatedAt: updated, Fingerprint: fp}
}

// TestUnchangedSessionsDoNotStarveChangedOnes: 25 sessions, 20 whose tail has
// NOT moved (yet whose activity is newer than their focus, and newer than the
// other five's), and 5 whose tail HAS moved. The first pass reads the 20
// newest (MaxPerPass) and finds them unchanged; the SECOND pass must skip
// those 20 without a read and refresh all 5 changed ones.
//
// Regression: on origin/main an unchanged read wrote nothing, so the 20 stayed
// candidates, took every slot on every pass, and the 5 were never refreshed.
func TestUnchangedSessionsDoNotStarveChangedOnes(t *testing.T) {
	base := time.Now().Truncate(time.Minute).Add(30 * time.Second)
	r, st, src, sm, now := skipFixture(base)
	var unchanged, changed []Subject
	for i := 0; i < 20; i++ {
		s := MirrorSubject(fmt.Sprintf("idle-%02d", i))
		e := []Entry{{Role: "user", Text: "settled " + s.Key}}
		src.tails[s] = e
		st.rows[s] = unchangedRow(s, e, base.Add(-3*time.Hour))
		src.cands = append(src.cands, Candidate{s, base.Add(-time.Hour + time.Duration(i)*time.Minute)})
		unchanged = append(unchanged, s)
	}
	for i := 0; i < 5; i++ {
		s := AgentSubject(int64(500 + i))
		src.tails[s] = []Entry{{Role: "user", Text: fmt.Sprintf("new work %d", i)}}
		st.rows[s] = Focus{Subject: s, Title: "Old title", Source: SourceSummarizer, UpdatedAt: base.Add(-3 * time.Hour), Fingerprint: "stale"}
		src.cands = append(src.cands, Candidate{s, base.Add(-2 * time.Hour)})
		changed = append(changed, s)
	}
	if err := r.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Positive control: the first pass did read the 20 and found them unchanged.
	if got := src.total(unchanged); got != 20 {
		t.Fatalf("first pass read %d unchanged tails, want 20", got)
	}
	*now = base.Add(15 * time.Minute)
	if err := r.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := src.total(unchanged); got != 20 {
		t.Errorf("the second pass re-read unchanged sessions: %d reads in all, want 20", got)
	}
	for _, s := range changed {
		if st.rows[s].Title != "Reworking the parser" {
			t.Errorf("%s was starved: title %q", s, st.rows[s].Title)
		}
	}
	if sm.calls != 5 {
		t.Errorf("%d summarizer calls, want 5", sm.calls)
	}
}

// TestATailMovedInsideTheCheckedBucketIsReadAgain: activity is floored to a
// minute (mirror.ActivityBucket), so a message that lands in the same minute
// as a read, AFTER it began, reads as no newer than the read. It must still be
// read on the next pass. (Invariant guard on the new skip rule: origin/main
// had no check record and re-read everything.)
func TestATailMovedInsideTheCheckedBucketIsReadAgain(t *testing.T) {
	minute := time.Now().Truncate(time.Minute)
	r, st, src, sm, now := skipFixture(minute.Add(30 * time.Second))
	s := MirrorSubject("busy")
	e := []Entry{{Role: "user", Text: "first"}}
	src.tails[s] = e
	st.rows[s] = unchangedRow(s, e, minute.Add(-time.Hour))
	src.cands = []Candidate{{s, minute}}
	if err := r.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if src.reads[s] != 1 || sm.calls != 0 {
		t.Fatalf("first pass: reads=%d calls=%d, want 1 and 0 (unchanged)", src.reads[s], sm.calls)
	}
	// A message 20 s after the read began: floored, its activity is still `minute`.
	src.tails[s] = append(e, Entry{Role: "assistant", Text: "second"})
	*now = minute.Add(15 * time.Minute)
	if err := r.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if src.reads[s] != 2 || sm.calls != 1 || st.rows[s].Title != "Reworking the parser" {
		t.Fatalf("second pass: reads=%d calls=%d title=%q; want the moved tail read and summarized", src.reads[s], sm.calls, st.rows[s].Title)
	}
}

// TestAFocusDeletedAfterACheckIsComputedAgain: a check record only skips while
// the stored focus still carries the fingerprint the check found. A focus
// deleted under it (the retention sweep) is recomputed on the next pass.
// (Invariant guard on the new skip rule.)
func TestAFocusDeletedAfterACheckIsComputedAgain(t *testing.T) {
	base := time.Now().Truncate(time.Minute).Add(30 * time.Second)
	r, st, src, sm, now := skipFixture(base)
	s := MirrorSubject("swept")
	e := []Entry{{Role: "user", Text: "steady"}}
	src.tails[s] = e
	st.rows[s] = unchangedRow(s, e, base.Add(-3*time.Hour))
	src.cands = []Candidate{{s, base.Add(-time.Hour)}}
	if err := r.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	delete(st.rows, s)
	*now = base.Add(15 * time.Minute)
	if err := r.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if src.reads[s] != 2 || sm.calls != 1 || !st.rows[s].Has() {
		t.Fatalf("reads=%d calls=%d row=%+v; want the deleted focus recomputed", src.reads[s], sm.calls, st.rows[s])
	}
}

// TestAFailedRefreshIsRetriedOnTheNextPass: a failure records no check, so it
// does not make a session skippable. (Invariant guard.)
func TestAFailedRefreshIsRetriedOnTheNextPass(t *testing.T) {
	base := time.Now().Truncate(time.Minute).Add(30 * time.Second)
	r, st, src, sm, now := skipFixture(base)
	s := AgentSubject(7)
	src.tails[s] = []Entry{{Role: "user", Text: "moved"}}
	st.rows[s] = Focus{Subject: s, Title: "Before", Source: SourceSummarizer, UpdatedAt: base.Add(-3 * time.Hour), Fingerprint: "old"}
	src.cands = []Candidate{{s, base.Add(-time.Hour)}}
	sm.err = ErrUnreachable
	if err := r.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	sm.err = nil
	*now = base.Add(15 * time.Minute)
	if err := r.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sm.calls != 2 || st.rows[s].Title != "Reworking the parser" {
		t.Fatalf("calls=%d title=%q; want the failed refresh retried", sm.calls, st.rows[s].Title)
	}
}

// TestCheckRecordsArePrunedToTheCandidates: a session no longer listed loses
// its check record, so the map is bounded by the live sessions.
func TestCheckRecordsArePrunedToTheCandidates(t *testing.T) {
	base := time.Now().Truncate(time.Minute).Add(30 * time.Second)
	r, st, src, _, _ := skipFixture(base)
	a, b := MirrorSubject("stays"), MirrorSubject("goes")
	for _, s := range []Subject{a, b} {
		e := []Entry{{Role: "user", Text: s.Key}}
		src.tails[s] = e
		st.rows[s] = unchangedRow(s, e, base.Add(-3*time.Hour))
	}
	src.cands = []Candidate{{a, base.Add(-time.Hour)}, {b, base.Add(-time.Hour)}}
	if err := r.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(r.checked[1]); n != 2 {
		t.Fatalf("positive control: %d check records, want 2", n)
	}
	src.cands = src.cands[:1]
	if err := r.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.checked[1][b]; ok || len(r.checked[1]) != 1 {
		t.Fatalf("check records %v; want only %s", r.checked[1], a)
	}
}

// TestEveryPassLogsOneLineOfCountsAndNoTitle: a pass logs exactly one summary
// line — a quiet pass too, so a missing line means the pass did not run — and
// it carries counts, never a title.
//
// Regression: origin/main logged only passes that updated or failed something.
func TestEveryPassLogsOneLineOfCountsAndNoTitle(t *testing.T) {
	base := time.Now().Truncate(time.Minute).Add(30 * time.Second)
	r, st, src, sm, now := skipFixture(base)
	var buf bytes.Buffer
	r.Logger = log.New(&buf, "", 0)
	sm.sum = Summary{Title: "Zebra-quartz migration plan", Description: "Moving the quartz."}
	quiet, moved := MirrorSubject("quiet"), AgentSubject(3)
	e := []Entry{{Role: "user", Text: "nothing new"}}
	src.tails[quiet] = e
	st.rows[quiet] = unchangedRow(quiet, e, base.Add(-3*time.Hour))
	src.tails[moved] = []Entry{{Role: "user", Text: "fresh"}}
	src.cands = []Candidate{{quiet, base.Add(-time.Hour)}, {moved, base.Add(-2 * time.Hour)}}
	if err := r.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The second pass has nothing to do: both are skipped.
	*now = base.Add(15 * time.Minute)
	if err := r.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "focus: pass: owners=1 candidates=2 skipped=0 deferred=0 updated=1 unchanged=1 empty=0 failed=0 rate-limited=false\n" +
		"focus: pass: owners=1 candidates=2 skipped=2 deferred=0 updated=0 unchanged=0 empty=0 failed=0 rate-limited=false\n"
	if buf.String() != want {
		t.Fatalf("log:\n%s\nwant:\n%s", buf.String(), want)
	}
	if strings.Contains(buf.String(), "Zebra") || strings.Contains(buf.String(), "Settled") {
		t.Fatal("a pass line carries a title")
	}
}
