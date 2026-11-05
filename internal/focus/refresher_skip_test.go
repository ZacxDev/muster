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
// NOT moved, and 5 whose tail HAS moved, with the 20 sorted ahead of the 5 on
// every pass. Ten of the 20 are idle (their activity is newer than their focus
// but fixed); the other ten are WAITING on the person, so — as a host reports a
// waiting window — their activity is the pass's own NOW, moving every pass,
// while they have waited since two hours before. The first pass reads the 20
// newest (MaxPerPass) and finds them unchanged; every LATER pass must skip all
// 20 without a read, and the second must refresh all 5 changed ones.
//
// Regression: on origin/main an unchanged read wrote nothing, so the 20 stayed
// candidates, took every slot on every pass, and the 5 were never refreshed;
// at 8931ddcc the ten waiting ones still did, their activity always "now".
func TestUnchangedSessionsDoNotStarveChangedOnes(t *testing.T) {
	base := time.Now().Truncate(time.Minute).Add(30 * time.Second)
	r, st, src, sm, now := skipFixture(base)
	var unchanged, changed []Subject
	for i := 0; i < 20; i++ {
		s := MirrorSubject(fmt.Sprintf("idle-%02d", i))
		e := []Entry{{Role: "user", Text: "settled " + s.Key}}
		src.tails[s] = e
		st.rows[s] = unchangedRow(s, e, base.Add(-3*time.Hour))
		src.cands = append(src.cands, Candidate{Subject: s, ActiveAt: base.Add(-time.Hour + time.Duration(i)*time.Minute)})
		unchanged = append(unchanged, s)
	}
	for i := 0; i < 5; i++ {
		s := AgentSubject(int64(500 + i))
		src.tails[s] = []Entry{{Role: "user", Text: fmt.Sprintf("new work %d", i)}}
		st.rows[s] = Focus{Subject: s, Title: "Old title", Source: SourceSummarizer, UpdatedAt: base.Add(-3 * time.Hour), Fingerprint: "stale"}
		src.cands = append(src.cands, Candidate{Subject: s, ActiveAt: base.Add(-2 * time.Hour)})
		changed = append(changed, s)
	}
	// The waiting ten (idle-10..19): activity = the host's now, every pass.
	hostNow := func() {
		for i := 10; i < 20; i++ {
			src.cands[i].ActiveAt = (*now).Truncate(time.Minute)
			src.cands[i].WaitingSince = base.Add(-2 * time.Hour)
		}
	}
	hostNow()
	if err := r.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Positive control: the first pass did read the 20 and found them unchanged.
	if got := src.total(unchanged); got != 20 {
		t.Fatalf("first pass read %d unchanged tails, want 20", got)
	}
	for pass := 1; pass <= 3; pass++ {
		*now = base.Add(time.Duration(pass) * 15 * time.Minute)
		hostNow()
		if err := r.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := src.total(unchanged); got != 20 {
		t.Errorf("later passes re-read unchanged sessions: %d reads in all over 4 passes, want 20", got)
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

// TestAnEmptyTailIsNotReadAgainUntilItMoves: a session that already has a
// focus but whose tail now renders empty (its rows render nothing a focus
// keeps) is read once; an empty read matches an empty read, so later passes
// skip it while its activity has not moved. 3 passes, 1 read.
//
// Regression: at 8931ddcc the empty read recorded "" and the stored focus
// carried its older fingerprint, so the two never matched and it was read on
// every pass.
func TestAnEmptyTailIsNotReadAgainUntilItMoves(t *testing.T) {
	base := time.Now().Truncate(time.Minute).Add(30 * time.Second)
	r, st, src, sm, now := skipFixture(base)
	s := MirrorSubject("blank")
	src.tails[s] = nil
	st.rows[s] = Focus{Subject: s, Title: "Earlier work", Source: SourceSummarizer, UpdatedAt: base.Add(-3 * time.Hour), Fingerprint: "an-older-tail"}
	src.cands = []Candidate{{Subject: s, ActiveAt: base.Add(-time.Hour)}}
	for pass := 0; pass < 3; pass++ {
		*now = base.Add(time.Duration(pass) * 15 * time.Minute)
		if err := r.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if src.reads[s] != 1 || sm.calls != 0 {
		t.Errorf("reads=%d calls=%d over 3 passes, want 1 and 0", src.reads[s], sm.calls)
	}
	// It moves: read again.
	src.cands[0].ActiveAt = base.Add(40 * time.Minute)
	*now = base.Add(60 * time.Minute)
	if err := r.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if src.reads[s] != 2 {
		t.Errorf("after its activity moved: reads=%d, want 2", src.reads[s])
	}
}

// TestAWaitInsideTheCheckedBucketIsReadAgain: a window that began waiting
// LESS than a whole bucket before a read is not skipped by its wait — the
// bucket is the slack between the CLI's clock (StatusSince) and the server's
// — while one waiting since well before is. Both are "busy-now" by activity.
func TestAWaitInsideTheCheckedBucketIsReadAgain(t *testing.T) {
	base := time.Now().Truncate(time.Minute).Add(30 * time.Second)
	r, st, src, _, now := skipFixture(base)
	late, early := MirrorSubject("late"), MirrorSubject("early")
	for _, s := range []Subject{late, early} {
		e := []Entry{{Role: "user", Text: s.Key}}
		src.tails[s] = e
		st.rows[s] = unchangedRow(s, e, base.Add(-3*time.Hour))
	}
	src.cands = []Candidate{
		{Subject: late, ActiveAt: base.Truncate(time.Minute), WaitingSince: base.Add(-20 * time.Second)},
		{Subject: early, ActiveAt: base.Truncate(time.Minute), WaitingSince: base.Add(-2 * time.Hour)},
	}
	if err := r.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	*now = base.Add(15 * time.Minute)
	for i := range src.cands {
		src.cands[i].ActiveAt = (*now).Truncate(time.Minute)
	}
	if err := r.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if src.reads[late] != 2 || src.reads[early] != 1 {
		t.Errorf("reads late=%d early=%d, want 2 (inside the bucket) and 1 (waiting since before)", src.reads[late], src.reads[early])
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
	src.cands = []Candidate{{Subject: s, ActiveAt: minute}}
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
	src.cands = []Candidate{{Subject: s, ActiveAt: base.Add(-time.Hour)}}
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
	src.cands = []Candidate{{Subject: s, ActiveAt: base.Add(-time.Hour)}}
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
	src.cands = []Candidate{{Subject: a, ActiveAt: base.Add(-time.Hour)}, {Subject: b, ActiveAt: base.Add(-time.Hour)}}
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
	src.cands = []Candidate{{Subject: quiet, ActiveAt: base.Add(-time.Hour)}, {Subject: moved, ActiveAt: base.Add(-2 * time.Hour)}}
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
