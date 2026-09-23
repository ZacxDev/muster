package notes

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
)

// statusProbe is a minimal Store: it EMBEDS the interface (so it satisfies it)
// and implements only the two methods the choke point touches. Any other method
// would nil-panic, which is the point — a decorator that started calling
// something else would be caught, not silently tolerated.
type statusProbe struct {
	Store
	cur      string
	getErr   error
	setErr   error
	gets     int
	sets     int
	lastCtx  context.Context
	lastStat string
}

func (p *statusProbe) Get(_ context.Context, id int64) (Note, error) {
	p.gets++
	if p.getErr != nil {
		return Note{}, p.getErr
	}
	return Note{ID: id, Status: p.cur}, nil
}

func (p *statusProbe) SetStatus(ctx context.Context, id int64, status string) (Note, error) {
	p.sets++
	p.lastCtx = ctx
	p.lastStat = status
	if p.setErr != nil {
		return Note{}, p.setErr
	}
	p.cur = status
	return Note{ID: id, Status: status}, nil
}

func newLoggedProbe(cur string) (*statusProbe, Store, *bytes.Buffer) {
	p := &statusProbe{cur: cur}
	var buf bytes.Buffer
	return p, WithStatusLogging(p, log.New(&buf, "", 0)), &buf
}

// 🔴 NEGATIVE CONTROL FOR THE WHOLE FILE. Every assertion below reads a log
// buffer, and a buffer that can never be non-empty would make all of them pass
// vacuously. This is the paired positive control: the UNWRAPPED store writes
// NOTHING, the wrapped one writes a line. Without the pair, "the log contains X"
// is a claim about the string I typed into the format verb.
func TestUnwrappedStoreLogsNothingAndWrappedStoreLogs(t *testing.T) {
	p := &statusProbe{cur: StatusOpen}
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)

	// Bare store, same logger: the logger is reachable, and still nothing lands.
	if _, err := p.SetStatus(context.Background(), 7, StatusInProgress); err != nil {
		t.Fatalf("bare SetStatus: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("negative control: the UNWRAPPED store logged %q. Every other assertion in this "+
			"file would then pass without the decorator doing anything", buf.String())
	}

	wrapped := WithStatusLogging(p, logger)
	if _, err := wrapped.SetStatus(context.Background(), 7, StatusReadyForReview); err != nil {
		t.Fatalf("wrapped SetStatus: %v", err)
	}
	if buf.Len() == 0 {
		t.Fatal("positive control: the WRAPPED store logged nothing — the instrument is wired to nothing")
	}
}

func TestChokePointLogsIdOldNewAndWriter(t *testing.T) {
	p, st, buf := newLoggedProbe(StatusOpen)
	ctx := WithWriter(context.Background(), "dispatch-advance")

	n, err := st.SetStatus(ctx, 372, StatusInProgress)
	if err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	// Behaviour is unchanged: same note, and the delegate really ran.
	if n.ID != 372 || n.Status != StatusInProgress {
		t.Fatalf("returned note = %+v, want id 372 status in_progress", n)
	}
	if p.sets != 1 {
		t.Fatalf("delegate SetStatus called %d times, want 1", p.sets)
	}

	got := buf.String()
	for _, want := range []string{
		"task status write:", "task=372", "old=open", "new=in_progress", "writer=dispatch-advance", "result=ok",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log line %q missing %q", strings.TrimSpace(got), want)
		}
	}
	if strings.Contains(got, "DOWNGRADE") {
		t.Errorf("open→in_progress logged as a DOWNGRADE: %q", strings.TrimSpace(got))
	}
	if c := strings.Count(strings.TrimSpace(got), "\n"); c != 0 {
		t.Errorf("one status write produced %d log lines, want exactly 1:\n%s", c+1, got)
	}
}

// 🔴 THE FINDING THIS FEATURE EXISTS FOR. An unattributed status write must be
// LOUD, not an empty field — `writer=` reads as a formatting bug and gets
// scrolled past.
func TestUnlabelledWriteSaysSoLoudly(t *testing.T) {
	_, st, buf := newLoggedProbe(StatusOpen)

	if _, err := st.SetStatus(context.Background(), 41, StatusInProgress); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	got := strings.TrimSpace(buf.String())
	if !strings.Contains(got, "writer="+UnlabelledWriter) {
		t.Fatalf("unlabelled write logged %q, want writer=%s", got, UnlabelledWriter)
	}
	if strings.Contains(got, "writer= ") || strings.HasSuffix(got, "writer=") {
		t.Fatalf("unlabelled write printed an EMPTY writer field: %q", got)
	}
}

// A whitespace-only label is the same hazard wearing a disguise: it would store
// a context that claims to be labelled and print a blank field.
func TestBlankLabelIsTreatedAsUnlabelled(t *testing.T) {
	for _, label := range []string{"", "   ", "\t\n"} {
		_, st, buf := newLoggedProbe(StatusOpen)
		ctx := WithWriter(context.Background(), label)
		if _, err := st.SetStatus(ctx, 5, StatusInProgress); err != nil {
			t.Fatalf("SetStatus: %v", err)
		}
		if !strings.Contains(buf.String(), "writer="+UnlabelledWriter) {
			t.Errorf("label %q logged %q, want writer=%s", label, strings.TrimSpace(buf.String()), UnlabelledWriter)
		}
	}
}

// 🔴 AC 3: a DOWNGRADE is WARN and NAMES BOTH statuses.
func TestDowngradeIsWarnAndNamesBothStatuses(t *testing.T) {
	// The exact transition that lost an agent's completion signal.
	p, st, buf := newLoggedProbe(StatusReadyForReview)
	ctx := WithWriter(context.Background(), "agent_set_task_status")

	if _, err := st.SetStatus(ctx, 372, StatusInProgress); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if p.sets != 1 {
		t.Fatalf("delegate SetStatus called %d times, want 1 — the WARNING must not suppress the write", p.sets)
	}
	got := strings.TrimSpace(buf.String())
	for _, want := range []string{
		"WARNING", "DOWNGRADE", "task=372",
		"old=" + StatusReadyForReview, // both statuses, named
		"new=" + StatusInProgress,
		"writer=agent_set_task_status",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("downgrade line %q missing %q", got, want)
		}
	}
}

func TestForwardTransitionsAreNotWarned(t *testing.T) {
	for _, c := range []struct{ from, to string }{
		{StatusOpen, StatusInProgress},
		{StatusInProgress, StatusReadyForReview},
		{StatusReadyForReview, StatusComplete},
		{StatusInProgress, StatusInProgress},
	} {
		_, st, buf := newLoggedProbe(c.from)
		if _, err := st.SetStatus(WithWriter(context.Background(), "human-ui"), 1, c.to); err != nil {
			t.Fatalf("SetStatus %s→%s: %v", c.from, c.to, err)
		}
		if strings.Contains(buf.String(), "DOWNGRADE") {
			t.Errorf("%s→%s logged as a DOWNGRADE: %q", c.from, c.to, strings.TrimSpace(buf.String()))
		}
	}
}

// A FAILED write must not be advertised as a landed downgrade: the row never
// moved, and a WARNING about a state the task is not in poisons the grep.
func TestFailedWriteIsLoggedAsFailedAndNeverAsADowngrade(t *testing.T) {
	p := &statusProbe{cur: StatusReadyForReview, setErr: errors.New("boom")}
	var buf bytes.Buffer
	st := WithStatusLogging(p, log.New(&buf, "", 0))

	_, err := st.SetStatus(WithWriter(context.Background(), "human-ui"), 9, StatusOpen)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error = %v, want the delegate's error forwarded verbatim", err)
	}
	got := strings.TrimSpace(buf.String())
	if !strings.Contains(got, "result=FAILED") || !strings.Contains(got, "boom") {
		t.Errorf("failed write logged %q, want result=FAILED with the cause", got)
	}
	if strings.Contains(got, "DOWNGRADE") {
		t.Errorf("a write that FAILED was logged as a DOWNGRADE: %q", got)
	}
}

// An unreadable previous status degrades to old=UNKNOWN and still performs the
// write — the log must never be able to fail a status change.
func TestUnreadablePreviousStatusDegradesToUnknownAndStillWrites(t *testing.T) {
	p := &statusProbe{cur: StatusReadyForReview, getErr: errors.New("no rows")}
	var buf bytes.Buffer
	st := WithStatusLogging(p, log.New(&buf, "", 0))

	n, err := st.SetStatus(WithWriter(context.Background(), "human-ui"), 3, StatusOpen)
	if err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if n.Status != StatusOpen || p.sets != 1 {
		t.Fatalf("write did not happen: note=%+v sets=%d", n, p.sets)
	}
	got := strings.TrimSpace(buf.String())
	if !strings.Contains(got, "old=UNKNOWN") {
		t.Errorf("log = %q, want old=UNKNOWN when the pre-read failed", got)
	}
	if strings.Contains(got, "DOWNGRADE") {
		t.Errorf("an UNREADABLE previous status was classified as a DOWNGRADE: %q", got)
	}
}

// A BLANK previous status must reach the same sentinel as an unreadable one —
// `old=` is an empty field in the line whose whole job is attribution.
func TestBlankPreviousStatusLogsAsUnknownNotAsAnEmptyField(t *testing.T) {
	for _, cur := range []string{"", "   "} {
		_, st, buf := newLoggedProbe(cur)
		if _, err := st.SetStatus(WithWriter(context.Background(), "human-ui"), 4, StatusOpen); err != nil {
			t.Fatalf("SetStatus: %v", err)
		}
		got := strings.TrimSpace(buf.String())
		if !strings.Contains(got, "old=UNKNOWN") {
			t.Errorf("previous status %q logged %q, want old=UNKNOWN", cur, got)
		}
		if strings.Contains(got, "old= ") {
			t.Errorf("previous status %q printed an EMPTY old field: %q", cur, got)
		}
	}
}

// The label must reach the store on the SAME context the caller passed, with
// everything else on it intact — the decorator sits in the request path.
func TestWriterLabelRidesTheCallersContext(t *testing.T) {
	type k struct{}
	p, st, _ := newLoggedProbe(StatusOpen)
	ctx := context.WithValue(context.Background(), k{}, "carried")
	ctx = WithWriter(ctx, "operator_set_task_status")

	if _, err := st.SetStatus(ctx, 1, StatusInProgress); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if p.lastCtx.Value(k{}) != "carried" {
		t.Error("the caller's context value did not reach the store")
	}
	if got := WriterFrom(p.lastCtx); got != "operator_set_task_status" {
		t.Errorf("WriterFrom(store ctx) = %q, want operator_set_task_status", got)
	}
}

func TestWriterFromHandlesNilAndUnsetContexts(t *testing.T) {
	//nolint:staticcheck // deliberately probing the nil-context path
	if got := WriterFrom(nil); got != UnlabelledWriter {
		t.Errorf("WriterFrom(nil) = %q, want %s", got, UnlabelledWriter)
	}
	if got := WriterFrom(context.Background()); got != UnlabelledWriter {
		t.Errorf("WriterFrom(unset) = %q, want %s", got, UnlabelledWriter)
	}
}

// WithStatusLogging(nil) must stay nil so the "extensions are optional" nil
// checks all over internal/api keep working — wrapping nil into a non-nil
// interface would turn `s.ext.Notes == nil` false and nil-panic later.
func TestWrappingANilStoreStaysNil(t *testing.T) {
	if got := WithStatusLogging(nil, nil); got != nil {
		t.Fatalf("WithStatusLogging(nil, nil) = %#v, want nil", got)
	}
}

// Non-status methods must pass straight through to the wrapped store.
func TestDecoratorForwardsNonStatusMethods(t *testing.T) {
	p, st, buf := newLoggedProbe(StatusInProgress)
	n, err := st.Get(context.Background(), 12)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if n.ID != 12 || n.Status != StatusInProgress {
		t.Fatalf("Get returned %+v, want the wrapped store's answer", n)
	}
	if p.gets != 1 {
		t.Fatalf("delegate Get called %d times, want 1", p.gets)
	}
	if buf.Len() != 0 {
		t.Errorf("a plain Get produced a status log line: %q", buf.String())
	}
}
