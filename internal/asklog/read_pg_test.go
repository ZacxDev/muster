package asklog

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// THE READER on a real Postgres (read.go): the owner's own rows, filtered and
// paged newest first, and the summary's literal counts, percentiles and
// repeated questions — with another owner's rows in the same table, so a
// read that is not scoped by row-level security changes every number.

// readFixture is user 2's seven asks and user 3's two. Every latency is
// distinct (10, 20, 40, 80, 160 s for user 2; 1000 and 3000 s for user 3), so
// a percentile that picks the wrong rows, or another owner's, lands on a
// value none of the expected ones equal.
type readFixture struct {
	p   *PG
	ids map[string]int64 // user 2's rows by name
	u3  []int64
}

func newReadFixture(t *testing.T) readFixture {
	t.Helper()
	p, _ := pgStore(t)
	f := readFixture{p: p, ids: map[string]int64{}}
	type row struct {
		name string
		a    Ask
		r    *Resolution
		lat  time.Duration
	}
	h := time.Hour
	ans := func(outcome, by, text, prov string) *Resolution {
		return &Resolution{Outcome: outcome, AnsweredBy: by, Answer: text, Provenance: prov}
	}
	mine := []row{
		{"cont1", Ask{Kind: KindPlainText, Runtime: RuntimeClaudeCode, Host: "alpha", TmuxSession: "s1", ClaudeSession: "cc-1", Repo: "acme/widgets",
			Question: "Tests pass.\n\nShall I continue?", AskedAt: t0.Add(1 * h)}, ans(OutcomeAnswered, ByOwner, "Yes", ProvPane), 10 * time.Second},
		{"cont2", Ask{Kind: KindPlainText, Runtime: RuntimeClaudeCode, Host: "alpha", TmuxSession: "s1", ClaudeSession: "cc-1", Repo: "acme/widgets",
			Question: "shall I continue?", AskedAt: t0.Add(2 * h)}, ans(OutcomeAnswered, ByOwner, " yes ", ProvMusterSend), 20 * time.Second},
		{"cont3", Ask{Kind: KindPlainText, Runtime: RuntimeClaudeCode, Host: "beta", TmuxSession: "s2", ClaudeSession: "cc-2", Repo: "acme/gadgets",
			Question: "Shall I continue?", AskedAt: t0.Add(3 * h)}, ans(OutcomeAnswered, ByChief, "go on", ProvNudge), 40 * time.Second},
		{"auq", Ask{Kind: KindAskUserQuestion, Runtime: RuntimeClaudeCode, Host: "alpha", TmuxSession: "s1", SourceRef: "q-1", Repo: "acme/widgets",
			Question: "Which database?", AskedAt: t0.Add(4 * h)}, ans(OutcomeAnswered, ByOwner, "postgres", ""), 80 * time.Second},
		{"ship", Ask{Kind: KindAttention, Runtime: RuntimeAgent, AgentID: 41, SourceRef: "a-1", Repo: "acme/widgets",
			Question: "Ship the migration now?", AskedAt: t0.Add(5 * h)}, ans(OutcomeDeclined, ByOwner, "", ""), 160 * time.Second},
		{"merge", Ask{Kind: KindAttention, Runtime: RuntimeAgent, AgentID: 41, SourceRef: "a-2", Repo: "acme/widgets",
			Question: "Merge #12?", AskedAt: t0.Add(6 * h)}, &Resolution{Outcome: OutcomeExpired}, time.Hour},
		{"perm", Ask{Kind: KindOpenCodePermission, Runtime: RuntimeOpenCode, Host: "beta", TmuxSession: "s2", ClaudeSession: "oc-9", SourceRef: "p-1",
			Question: "Allow a write?", AskedAt: t0.Add(7 * h)}, nil, 0},
	}
	open := func(ctx context.Context, r row) int64 {
		id, _, err := p.Open(ctx, r.a)
		if err != nil {
			t.Fatalf("open %s: %v", r.name, err)
		}
		if r.r != nil {
			res := *r.r
			res.At = r.a.AskedAt.Add(r.lat)
			if _, ok, err := p.Resolve(ctx, id, res); !ok || err != nil {
				t.Fatalf("resolve %s: ok=%v err=%v", r.name, ok, err)
			}
		}
		return id
	}
	for _, r := range mine {
		f.ids[r.name] = open(asUser(2), r)
	}
	// User 3: the same repeated question, answered by its owner, with latencies
	// far from user 2's — and asked LATER than all of user 2's rows, so a leak
	// is also the first row of an unscoped page.
	for i, lat := range []time.Duration{1000 * time.Second, 3000 * time.Second} {
		f.u3 = append(f.u3, open(asUser(3), row{"u3", Ask{Kind: KindPlainText, Runtime: RuntimeClaudeCode, Host: "alpha", TmuxSession: "s1",
			Repo: "acme/widgets", Question: "Shall I continue?", AskedAt: t0.Add(time.Duration(10+i) * h)},
			ans(OutcomeAnswered, ByOwner, "yes", ProvPane), lat}))
	}
	return f
}

func (f readFixture) names(t *testing.T, rows []Ask) []string {
	t.Helper()
	by := map[int64]string{}
	for n, id := range f.ids {
		by[id] = n
	}
	out := []string{}
	for _, a := range rows {
		n, ok := by[a.ID]
		if !ok {
			n = "FOREIGN"
		}
		out = append(out, n)
	}
	return out
}

// TestTheSummaryIsTheOwnersRowsOnly: literal counts, the continuous median
// (40 s) and 90th percentile (128 s: 80 + 0.6 × 80) over user 2's five
// answered rows, and the one question asked three times — "shall i continue",
// two of them answered by the owner, its median 20 s, its most frequent
// answer "yes" (twice: " yes " trims and lower-cases to it). User 3's rows,
// in the same table, change none of it; user 3's own summary is their two.
func TestTheSummaryIsTheOwnersRowsOnly(t *testing.T) {
	f := newReadFixture(t)
	s, err := f.p.Summarize(asUser(2), Filter{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.Total != 7 || s.Answered != 5 {
		t.Fatalf("total %d answered %d; want 7 and 5", s.Total, s.Answered)
	}
	if want := map[string]int{KindPlainText: 3, KindAskUserQuestion: 1, KindAttention: 2, KindOpenCodePermission: 1}; !reflect.DeepEqual(s.ByKind, want) {
		t.Errorf("by kind %v; want %v", s.ByKind, want)
	}
	if want := map[string]int{OutcomeAnswered: 4, OutcomeDeclined: 1, OutcomeExpired: 1, OutcomeOpen: 1}; !reflect.DeepEqual(s.ByOutcome, want) {
		t.Errorf("by outcome %v; want %v", s.ByOutcome, want)
	}
	if want := map[string]int{ByOwner: 4, ByChief: 1, AnsweredByNone: 2}; !reflect.DeepEqual(s.ByAnsweredBy, want) {
		t.Errorf("by answered_by %v; want %v", s.ByAnsweredBy, want)
	}
	if s.MedianAnswer != 40*time.Second || s.P90Answer != 128*time.Second {
		t.Errorf("median %s p90 %s; want 40s and 2m8s", s.MedianAnswer, s.P90Answer)
	}
	// Question: the NEWEST of its rows' own text (cont3's), as the owner saw it
	// — not cont2's lower-case "shall I continue?" nor cont1's two sentences.
	want := []TopQuestion{{Norm: "shall i continue", Question: "Shall I continue?", Count: 3, ByOwner: 2, MedianAnswer: 20 * time.Second, TopAnswer: "yes", TopAnswerCount: 2,
		LastAsked: t0.Add(3 * time.Hour)}}
	for i := range s.Top {
		s.Top[i].LastAsked = s.Top[i].LastAsked.UTC()
	}
	if !reflect.DeepEqual(s.Top, want) {
		t.Errorf("top %+v; want %+v", s.Top, want)
	}

	o, err := f.p.Summarize(asUser(3), Filter{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if o.Total != 2 || o.Answered != 2 || o.MedianAnswer != 2000*time.Second || len(o.Top) != 1 || o.Top[0].Count != 2 {
		t.Fatalf("user 3's summary: %+v; want their own two rows", o)
	}
	// No principal at all reads nothing (row-level security, not a filter).
	if n, err := f.p.Summarize(context.Background(), Filter{}, 0); err != nil || n.Total != 0 {
		t.Fatalf("no principal: total %d err %v; want 0", n.Total, err)
	}
}

// TestASummaryFilterNarrowsEveryNumber: the same summary over kind
// plain_text only — three rows, latencies 10, 20, 40 s: median 20 s, p90 36 s.
func TestASummaryFilterNarrowsEveryNumber(t *testing.T) {
	f := newReadFixture(t)
	s, err := f.p.Summarize(asUser(2), Filter{Kind: KindPlainText}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.Total != 3 || s.Answered != 3 || s.MedianAnswer != 20*time.Second || s.P90Answer != 36*time.Second ||
		!reflect.DeepEqual(s.ByAnsweredBy, map[string]int{ByOwner: 2, ByChief: 1}) {
		t.Fatalf("plain_text summary: %+v", s)
	}
}

// TestPageFiltersAndPagesTheOwnersRowsNewestFirst: each filter names exactly
// its rows; three pages of three, two and two walk every one of user 2's rows
// newest first and end; user 3 sees none of them, and a cursor naming one of
// user 2's rows is nothing to user 3.
func TestPageFiltersAndPagesTheOwnersRowsNewestFirst(t *testing.T) {
	f := newReadFixture(t)
	ctx := asUser(2)
	for _, c := range []struct {
		name string
		f    Filter
		want []string
	}{
		{"all", Filter{}, []string{"perm", "merge", "ship", "auq", "cont3", "cont2", "cont1"}},
		{"kind", Filter{Kind: KindPlainText}, []string{"cont3", "cont2", "cont1"}},
		{"runtime", Filter{Runtime: RuntimeAgent}, []string{"merge", "ship"}},
		{"outcome", Filter{Outcome: OutcomeDeclined}, []string{"ship"}},
		{"open", Filter{Outcome: OutcomeOpen}, []string{"perm"}},
		{"answered by chief", Filter{AnsweredBy: ByChief}, []string{"cont3"}},
		{"answered by none", Filter{AnsweredBy: AnsweredByNone}, []string{"perm", "merge"}},
		{"host", Filter{Host: "beta"}, []string{"perm", "cont3"}},
		{"repo", Filter{Repo: "acme/gadgets"}, []string{"cont3"}},
		{"tmux session", Filter{Session: "s1"}, []string{"auq", "cont2", "cont1"}},
		{"claude session", Filter{Session: "oc-9"}, []string{"perm"}},
		{"window", Filter{Since: t0.Add(2 * time.Hour), Until: t0.Add(5 * time.Hour)}, []string{"auq", "cont3", "cont2"}},
		{"combined", Filter{Kind: KindPlainText, Host: "alpha"}, []string{"cont2", "cont1"}},
	} {
		rows, next, err := f.p.Page(ctx, c.f, 0, 0)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := f.names(t, rows); !reflect.DeepEqual(got, c.want) || next != 0 {
			t.Errorf("%s: %v next=%d; want %v and no next page", c.name, got, next, c.want)
		}
	}

	var walked []string
	before := int64(0)
	for i, size := range []int{3, 3, 3} {
		rows, next, err := f.p.Page(ctx, Filter{}, before, size)
		if err != nil {
			t.Fatal(err)
		}
		walked = append(walked, f.names(t, rows)...)
		if i < 2 && next == 0 || i == 2 && next != 0 {
			t.Fatalf("page %d: next=%d", i, next)
		}
		before = next
		if next == 0 {
			break
		}
	}
	if want := []string{"perm", "merge", "ship", "auq", "cont3", "cont2", "cont1"}; !reflect.DeepEqual(walked, want) {
		t.Fatalf("walked %v; want %v", walked, want)
	}

	other := asUser(3)
	rows, _, err := f.p.Page(other, Filter{}, 0, 0)
	if err != nil || len(rows) != 2 || rows[0].ID != f.u3[1] || rows[1].ID != f.u3[0] {
		t.Fatalf("user 3's page: %v (%v); want their own two, newest first", f.names(t, rows), err)
	}
	if rows, _, err := f.p.Page(other, Filter{}, f.ids["ship"], 0); err != nil || len(rows) != 0 {
		t.Fatalf("user 3 paging from user 2's row: %d rows (%v); want none", len(rows), err)
	}
}

// TestARowWhoseTextExpiredIsListedWithoutItAndLeavesTheTopQuestions: after
// the sweep clears the text of the two oldest rows, both are still listed
// (with TextPurgedAt), with no question or answer; "shall i continue" is now
// held by one row only, so no question repeats; the counts are unchanged.
func TestARowWhoseTextExpiredIsListedWithoutItAndLeavesTheTopQuestions(t *testing.T) {
	f := newReadFixture(t)
	ctx := asUser(2)
	now := t0.Add(30*24*time.Hour + 2*time.Hour + 30*time.Minute) // cont1 and cont2 are past 30 days; cont3 is not
	// The cut-off is t0 + 2h30m: cont1 (1 h) and cont2 (2 h), and no other row.
	if purged, _, err := f.p.Purge(context.Background(), now, 30*24*time.Hour, RowTTL); err != nil || purged != 2 {
		t.Fatalf("purge: %d (%v); want 2", purged, err)
	}
	rows, _, err := f.p.Page(ctx, Filter{Kind: KindPlainText}, 0, 0)
	if err != nil || len(rows) != 3 {
		t.Fatalf("page: %d rows (%v)", len(rows), err)
	}
	for _, a := range rows {
		expired := a.ID == f.ids["cont1"] || a.ID == f.ids["cont2"]
		if expired != !a.TextPurgedAt.IsZero() || expired && (a.Question != "" || a.Answer != "") || !expired && a.Question == "" {
			t.Errorf("row %d: purged_at=%v question=%q answer=%q (expired=%v)", a.ID, a.TextPurgedAt, a.Question, a.Answer, expired)
		}
	}
	s, err := f.p.Summarize(ctx, Filter{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Top) != 0 || s.Total != 7 || s.Answered != 5 {
		t.Fatalf("after the sweep: top %+v total %d answered %d; want no repeated question and the same counts", s.Top, s.Total, s.Answered)
	}
}

// TestAQuestionWithNoWordsIsNotAskedMostOften: three different questions
// that hold no letter or digit outside a placeholder (synthetic shapes:
// symbols around a comma, punctuation only, code spans only) are stored with
// NO normal form, so the summary does not group them into one repeated
// question; the fixture's real repeat, "shall i continue" (asked three
// times), still is the one top question — the positive control.
func TestAQuestionWithNoWordsIsNotAskedMostOften(t *testing.T) {
	f := newReadFixture(t)
	ctx := asUser(2)
	var ids []int64
	for i, q := range []string{"→ , ←", "? , ?", "`one` , `two`?"} {
		id, _, err := f.p.Open(ctx, Ask{Kind: KindPlainText, Runtime: RuntimeClaudeCode, Host: "alpha", TmuxSession: "s1",
			Question: q, AskedAt: t0.Add(time.Duration(20+i) * time.Hour)})
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	rows, _, err := f.p.Page(ctx, Filter{Since: t0.Add(20 * time.Hour)}, 0, 0)
	if err != nil || len(rows) != 3 {
		t.Fatalf("page: %d rows (%v); want the three", len(rows), err)
	}
	for _, a := range rows {
		if a.Question == "" || a.QuestionNorm != "" {
			t.Errorf("row %d: question %q norm %q; want the question kept and no normal form", a.ID, a.Question, a.QuestionNorm)
		}
	}
	s, err := f.p.Summarize(ctx, Filter{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.Total != 10 || len(s.Top) != 1 || s.Top[0].Norm != "shall i continue" || s.Top[0].Count != 3 {
		t.Fatalf("total %d top %+v; want 10 asks and the one repeated question \"shall i continue\" ×3", s.Total, s.Top)
	}
}

// TestAFilterTheReaderDoesNotKnowIsRefused: a misspelt kind, outcome,
// runtime or answerer is an error, not an empty page that reads as "none".
func TestAFilterTheReaderDoesNotKnowIsRefused(t *testing.T) {
	for _, f := range []Filter{{Kind: "plain"}, {Outcome: "done"}, {Runtime: "codex"}, {AnsweredBy: "me"},
		{Since: t0, Until: t0}} {
		if err := f.Validate(); err == nil {
			t.Errorf("%+v: accepted", f)
		}
	}
	for _, f := range []Filter{{}, {Kind: KindAttention, Outcome: OutcomeOpen, AnsweredBy: AnsweredByNone, Runtime: RuntimeOpenCode}} {
		if err := f.Validate(); err != nil {
			t.Errorf("%+v: %v", f, err)
		}
	}
}
