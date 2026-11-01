package transcript_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
	"github.com/ZacxDev/muster/internal/officers/transcript"
)

// ---------------------------------------------------------------------------
// The transcript forwarder against the fake HQ (hqtest), with a fake
// transcript source standing in for the instance pod's ccd.
// ---------------------------------------------------------------------------

const (
	officerID    = "release:example-web"
	officerToken = "tok-officer-SECRET-0123456789"
)

var instance = officers.InstanceName(officerID, 1)

// fakeSource is one instance's transcript: complete lines, each with its end
// offset, as ccd serves them (cctranscript.ReadLines' Line).
type fakeSource struct {
	mu      sync.Mutex
	session string
	lines   []cctranscript.Line
	end     int64
	reads   int
	// others are other instances' transcripts (each its own file).
	others map[string]*fakeSource
}

func newSource() *fakeSource {
	return &fakeSource{session: "11111111-2222-3333-4444-555555555555"}
}

// add appends one line carrying evs, 137 bytes long (an odd width, so no
// arithmetic in the seq formula lines up with a power of two by accident).
func (s *fakeSource) add(at time.Time, evs ...cctranscript.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.end += 137
	s.lines = append(s.lines, cctranscript.Line{Events: evs, End: s.end, At: at.UnixMilli()})
}

func (s *fakeSource) newSession(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.session, s.lines, s.end = id, nil, 0
}

func (s *fakeSource) Read(_ context.Context, o officers.Officer, inst string, q transcript.Query) (cctranscript.Page, error) {
	s.mu.Lock()
	if other := s.others[inst]; other != nil && inst != instance {
		s.mu.Unlock()
		return other.Read(context.Background(), o, instance, q)
	}
	defer s.mu.Unlock()
	s.reads++
	if o.ID != officerID || inst != instance {
		return cctranscript.Page{}, io.ErrUnexpectedEOF
	}
	from := q.After
	if q.Session != "" && q.Session != s.session {
		from = 0 // a new conversation: read it from its start
	}
	if from < 0 {
		from = s.end - int64(q.Backfill)
		if from < 0 {
			from = 0
		}
	}
	p := cctranscript.Page{Session: s.session, Next: s.end}
	for _, l := range s.lines {
		if l.End > from {
			p.Lines = append(p.Lines, l)
		}
	}
	return p, nil
}

type rig struct {
	t   *testing.T
	hq  *hqtest.Server
	src *fakeSource
	o   officers.Officer
	fwd *transcript.Forwarder
	now time.Time
	log *bytes.Buffer
	cfg transcript.Config
}

func officer(t *testing.T, hq *hqtest.Server, enabled bool) officers.Officer {
	t.Helper()
	tr := ""
	if enabled {
		tr = `,"transcript":{"enabled":true}`
	}
	list, err := officers.Parse(`[{"id":"`+officerID+`","hq_url":"`+hq.BaseURL()+`","max_instances":2`+tr+`}]`,
		func(k string) string {
			if k == officers.TokenEnvPrefix+officers.TokenEnvSuffix(officerID) {
				return officerToken
			}
			return ""
		}, "muster-agent-")
	if err != nil {
		t.Fatal(err)
	}
	return list[0]
}

func newRig(t *testing.T, enabled bool, tweak func(*transcript.Config)) *rig {
	t.Helper()
	hq := hqtest.New(map[string]string{officerToken: officerID})
	t.Cleanup(hq.Close)
	r := &rig{t: t, hq: hq, src: newSource(), now: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC), log: &bytes.Buffer{}}
	hq.Now = func() time.Time { return r.now }
	r.o = officer(t, hq, enabled)
	r.cfg = transcript.Config{
		Source: r.src,
		Poster: transcript.NewClientPoster(officers.NewClientHQ(nil)),
		Logger: log.New(r.log, "", 0),
		Now:    func() time.Time { return r.now },
	}
	if tweak != nil {
		tweak(&r.cfg)
	}
	r.restart()
	return r
}

// restart is a fresh Forwarder over the same source and HQ: what a muster
// restart leaves.
func (r *rig) restart() {
	f, err := transcript.New(r.cfg)
	if err != nil {
		r.t.Fatal(err)
	}
	r.fwd = f
}

// claim makes HQ hold item for the instance under a live lease.
func (r *rig) claim() string {
	id := r.hq.AddItem(officerID, "release", "subject-1")
	r.hq.SetClaim(id, instance, r.now.Add(24*time.Hour)) // outlives every test clock
	return id
}

func (r *rig) tick() {
	r.fwd.Tick(context.Background())
}

// advance moves the clock past any backoff and ticks.
func (r *rig) advance(d time.Duration) {
	r.now = r.now.Add(d)
	r.tick()
}

// posts is every request to the transcript route that carried events.
func (r *rig) posts() []hqtest.Request {
	var out []hqtest.Request
	for _, q := range r.hq.Requests() {
		if strings.HasSuffix(q.Path, "/transcript") && len(eventsOf(q)) > 0 {
			out = append(out, q)
		}
	}
	return out
}

// probes is every transcript post with NO events: the forwarder learning
// HQ's last_seq for an item before it sends any.
func (r *rig) probes() []hqtest.Request {
	var out []hqtest.Request
	for _, q := range r.hq.Requests() {
		if strings.HasSuffix(q.Path, "/transcript") && len(eventsOf(q)) == 0 {
			out = append(out, q)
		}
	}
	return out
}

func eventsOf(q hqtest.Request) []any {
	evs, _ := q.Body["events"].([]any)
	return evs
}

func at(min int) time.Time { return time.Date(2026, 11, 1, 12, min, 0, 0, time.UTC) }

func TestForwardsNewEventsMappedToKindsInStrictSeqOrder(t *testing.T) {
	r := newRig(t, true, nil)
	// Before the claim: the officer's earlier work, which is NOT this item's.
	r.src.add(r.now.Add(-time.Minute), cctranscript.Event{Kind: cctranscript.KindText, Text: "before the claim"})
	item := r.claim()
	r.fwd.Claimed(r.o, instance, officers.ItemID(item))

	r.src.add(at(1), cctranscript.Event{Kind: cctranscript.KindPrompt, Text: "work the queue"})
	r.src.add(at(2), cctranscript.Event{Kind: cctranscript.KindText, Text: "Looking at the release."},
		cctranscript.Event{Kind: cctranscript.KindToolCall, ToolID: "t1", Tool: "Bash", Target: "go test ./...", Input: `{"command":"go test ./..."}`})
	r.src.add(at(3), cctranscript.Event{Kind: cctranscript.KindToolResult, ToolID: "t1", OK: true, Output: "ok  \tpkg\t0.1s"})
	r.src.add(at(4), cctranscript.Event{Kind: cctranscript.KindError, Text: "rate limited"})
	r.src.add(at(5), cctranscript.Event{Kind: cctranscript.KindNotice, Notice: cctranscript.NoticeTask, Text: "background task finished"})
	r.src.add(at(5), cctranscript.Event{Kind: cctranscript.KindInjected, Text: "diagnostics"})
	r.tick()

	got := r.hq.Transcript(item)
	want := []struct{ kind, text string }{
		{"user", "work the queue"},
		{"assistant", "Looking at the release."},
		{"tool_use", "Bash: go test ./..."},
		{"tool_result", "ok  \tpkg\t0.1s"},
		{"system", "rate limited"},
		{"system", "background task finished"},
	}
	if len(got) != len(want) {
		t.Fatalf("stored %d events, want %d: %+v (log: %s)", len(got), len(want), got, r.log)
	}
	for i, w := range want {
		if got[i].Kind != w.kind || !strings.Contains(got[i].Text, w.text) {
			t.Errorf("event %d = %s %q, want %s containing %q", i, got[i].Kind, got[i].Text, w.kind, w.text)
		}
		if got[i].Instance != instance {
			t.Errorf("event %d posted for instance %q", i, got[i].Instance)
		}
		if i > 0 && got[i].Seq <= got[i-1].Seq {
			t.Errorf("seq not strictly increasing: %d then %d", got[i-1].Seq, got[i].Seq)
		}
	}
	if got[0].At != at(1).Format(time.RFC3339) {
		t.Errorf("at = %q, want the record's own time %q", got[0].At, at(1).Format(time.RFC3339))
	}
	// The officer token is the bearer, and nowhere in a body.
	for _, q := range r.posts() {
		if q.Auth != "Bearer "+officerToken {
			t.Fatalf("auth %q", q.Auth)
		}
	}

	// A second tick with nothing new posts nothing.
	n := len(r.posts())
	r.advance(3 * time.Second)
	if len(r.posts()) != n {
		t.Fatalf("a tick with no new event posted (%d -> %d)", n, len(r.posts()))
	}
}

func TestBatchesAtMostOneHundredEventsPerCall(t *testing.T) {
	r := newRig(t, true, nil)
	item := r.claim()
	r.fwd.Claimed(r.o, instance, officers.ItemID(item))
	for i := 0; i < 250; i++ {
		r.src.add(at(1), cctranscript.Event{Kind: cctranscript.KindText, Text: "line"})
	}
	r.tick()
	posts := r.posts()
	if len(posts) != 3 {
		t.Fatalf("%d posts, want 3 (100+100+50)", len(posts))
	}
	total := 0
	for _, q := range posts {
		n := len(eventsOf(q))
		if n > 100 || n == 0 {
			t.Fatalf("a post carried %d events", n)
		}
		total += n
	}
	if total != 250 || len(r.hq.Transcript(item)) != 250 {
		t.Fatalf("posted %d, stored %d, want 250", total, len(r.hq.Transcript(item)))
	}
}

func TestTruncatesTextAtEightKiBWithAMarker(t *testing.T) {
	r := newRig(t, true, nil)
	item := r.claim()
	r.fwd.Claimed(r.o, instance, officers.ItemID(item))
	long := strings.Repeat("é", 10000) // 20000 bytes, two per rune: a cut must not split one
	r.src.add(at(1), cctranscript.Event{Kind: cctranscript.KindText, Text: long})
	r.src.add(at(2), cctranscript.Event{Kind: cctranscript.KindText, Text: "short and whole"})
	r.tick()
	got := r.hq.Transcript(item)
	if len(got) != 2 {
		t.Fatalf("stored %d events: %+v", len(got), got)
	}
	cut := got[0].Text
	if len(cut) > officers.MaxTranscriptText {
		t.Fatalf("text is %d bytes, over %d", len(cut), officers.MaxTranscriptText)
	}
	i := strings.LastIndex(cut, "… [truncated ")
	if i < 0 {
		t.Fatalf("no truncation marker in a %d-byte text", len(cut))
	}
	kept := cut[:i]
	marker := "… [truncated " + itoa(len(long)-len(kept)) + " bytes]"
	if !strings.HasSuffix(cut, marker) {
		t.Fatalf("text ends %q, want the marker %q", cut[len(cut)-40:], marker)
	}
	if !strings.HasPrefix(long, kept) || len(kept) < 8000 {
		t.Fatalf("kept %d bytes, not a prefix of the text or far too short", len(kept))
	}
	if got[1].Text != "short and whole" {
		t.Fatalf("a short text was changed: %q", got[1].Text)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestRedactsASecretInAToolResultBeforePosting(t *testing.T) {
	r := newRig(t, true, nil)
	item := r.claim()
	r.fwd.Claimed(r.o, instance, officers.ItemID(item))
	// Built at runtime: a token-shaped literal in the source trips the leak gate.
	secret := "ghp_" + strings.Repeat("A1b2", 9)
	r.src.add(at(1), cctranscript.Event{Kind: cctranscript.KindToolResult, ToolID: "t1", OK: true,
		Output: "fetched the release notes\nGITHUB_TOKEN=" + secret + "\nall 12 checks passed"})
	// The officer's own token, echoed by a tool, is never sent either — even
	// in a line no secret SHAPE matches (only the scrub can know it).
	r.src.add(at(2), cctranscript.Event{Kind: cctranscript.KindToolResult, ToolID: "t2", OK: true,
		Output: "the value was " + officerToken + " today"})
	r.tick()

	got := r.hq.Transcript(item)
	if len(got) != 2 {
		t.Fatalf("stored %d events: %+v", len(got), got)
	}
	// POSITIVE CONTROL: the ordinary lines pass unchanged, so a filter that
	// withheld everything would fail here.
	if !strings.Contains(got[0].Text, "fetched the release notes") || !strings.Contains(got[0].Text, "all 12 checks passed") {
		t.Fatalf("the clean lines did not pass unchanged: %q", got[0].Text)
	}
	if !strings.Contains(got[0].Text, "withheld") {
		t.Fatalf("the secret line was not marked withheld: %q", got[0].Text)
	}
	for _, q := range r.posts() {
		b, _ := json.Marshal(q.Body)
		if strings.Contains(string(b), secret) || strings.Contains(string(b), strings.Repeat("A1b2", 9)) {
			t.Fatalf("the secret reached HQ: %s", b)
		}
		if strings.Contains(string(b), officerToken) {
			t.Fatalf("the officer token reached HQ in a body: %s", b)
		}
	}
	if strings.Contains(r.log.String(), secret) || strings.Contains(r.log.String(), "fetched the release notes") {
		t.Fatalf("transcript text was logged: %s", r.log.String())
	}
}

func TestStopsForAnItemOnNotClaimed(t *testing.T) {
	r := newRig(t, true, nil)
	item := r.claim()
	r.fwd.Claimed(r.o, instance, officers.ItemID(item))
	r.src.add(at(1), cctranscript.Event{Kind: cctranscript.KindText, Text: "one"})
	r.tick()
	if len(r.hq.Transcript(item)) != 1 {
		t.Fatalf("first event not stored")
	}
	r.hq.Complete(item) // the item is no longer claimed by the instance
	r.src.add(at(2), cctranscript.Event{Kind: cctranscript.KindText, Text: "two"})
	r.advance(3 * time.Second)
	refused := len(r.posts())
	if refused != 2 {
		t.Fatalf("%d posts, want 2 (one stored, one answered not_claimed)", refused)
	}
	for i := 0; i < 5; i++ {
		r.src.add(at(3), cctranscript.Event{Kind: cctranscript.KindText, Text: "more"})
		r.advance(time.Minute)
	}
	if len(r.posts()) != refused {
		t.Fatalf("kept posting after not_claimed: %d posts", len(r.posts()))
	}
	if len(r.fwd.Active()) != 0 {
		t.Fatalf("still forwarding: %v", r.fwd.Active())
	}
	// A pass that still lists the item as held (HQ's list lags) does not
	// restart it: the stop holds until a new claim.
	r.fwd.Sync(r.o, []officers.Item{{ID: officers.ItemID(item), Status: officers.StatusClaimed, ClaimedBy: instance, LeaseExpiresAt: ptr(r.now.Add(time.Minute))}}, r.now)
	r.advance(time.Minute)
	if len(r.posts()) != refused {
		t.Fatalf("a pass restarted a stopped item: %d posts", len(r.posts()))
	}

	// The next claim starts again.
	next := r.claim()
	r.fwd.Claimed(r.o, instance, officers.ItemID(next))
	r.src.add(r.now.Add(time.Second), cctranscript.Event{Kind: cctranscript.KindText, Text: "the next item"})
	r.advance(3 * time.Second)
	if got := r.hq.Transcript(next); len(got) != 1 || got[0].Text != "the next item" {
		t.Fatalf("the next claim's transcript = %+v", got)
	}
}

func ptr(t time.Time) *time.Time { return &t }

func TestResumesAfterARestartWithoutDuplicates(t *testing.T) {
	r := newRig(t, true, nil)
	item := r.claim()
	r.fwd.Claimed(r.o, instance, officers.ItemID(item))
	for i := 0; i < 3; i++ {
		r.src.add(at(1+i), cctranscript.Event{Kind: cctranscript.KindText, Text: "before restart " + itoa(i)})
	}
	r.tick()
	if len(r.hq.Transcript(item)) != 3 {
		t.Fatalf("stored %d before the restart", len(r.hq.Transcript(item)))
	}

	r.restart() // muster restarts: nothing in memory
	// Written while muster was down.
	r.src.add(at(5), cctranscript.Event{Kind: cctranscript.KindText, Text: "during the restart"})
	// The wake loop's first pass sees the instance still holding the item.
	r.fwd.Sync(r.o, []officers.Item{{ID: officers.ItemID(item), Status: officers.StatusClaimed, ClaimedBy: instance, LeaseExpiresAt: ptr(r.now.Add(5 * time.Minute))}}, r.now)
	r.tick()
	r.src.add(at(6), cctranscript.Event{Kind: cctranscript.KindText, Text: "after restart"})
	r.advance(3 * time.Second)

	got := r.hq.Transcript(item)
	var texts []string
	seen := map[int64]bool{}
	for i, e := range got {
		if seen[e.Seq] {
			t.Fatalf("seq %d stored twice", e.Seq)
		}
		seen[e.Seq] = true
		if i > 0 && e.Seq <= got[i-1].Seq {
			t.Fatalf("seq went backwards")
		}
		texts = append(texts, e.Text)
	}
	want := "before restart 0|before restart 1|before restart 2|during the restart|after restart"
	if strings.Join(texts, "|") != want {
		t.Fatalf("stored %q, want %q", strings.Join(texts, "|"), want)
	}
	// The restarted forwarder learned HQ's last_seq (an empty post) and
	// sent only what HQ did not hold: nothing stored is re-sent.
	if len(r.probes()) < 2 {
		t.Fatalf("%d probes, want one per forwarder start", len(r.probes()))
	}
	sent := 0
	for _, q := range r.posts() {
		for _, e := range eventsOf(q) {
			if m, _ := e.(map[string]any); m["text"] == "before restart 0" {
				sent++
			}
		}
	}
	if sent != 1 {
		t.Fatalf("an event HQ held was sent %d times", sent)
	}
}

func TestANewSessionKeepsSeqIncreasing(t *testing.T) {
	r := newRig(t, true, nil)
	item := r.claim()
	r.fwd.Claimed(r.o, instance, officers.ItemID(item))
	for i := 0; i < 5; i++ {
		r.src.add(at(1), cctranscript.Event{Kind: cctranscript.KindText, Text: "old session"})
	}
	r.tick()
	// The CLI starts a new conversation: a new file, offsets from zero.
	r.src.newSession("99999999-2222-3333-4444-555555555555")
	r.src.add(at(2), cctranscript.Event{Kind: cctranscript.KindText, Text: "new session"})
	r.advance(3 * time.Second)
	got := r.hq.Transcript(item)
	if len(got) != 6 || got[5].Text != "new session" {
		t.Fatalf("stored %+v", got)
	}
}

func TestADisabledOfficerSendsNothing(t *testing.T) {
	r := newRig(t, false, nil)
	item := r.claim()
	r.fwd.Claimed(r.o, instance, officers.ItemID(item))
	r.fwd.Sync(r.o, []officers.Item{{ID: officers.ItemID(item), Status: officers.StatusClaimed, ClaimedBy: instance, LeaseExpiresAt: ptr(r.now.Add(time.Minute))}}, r.now)
	r.src.add(at(1), cctranscript.Event{Kind: cctranscript.KindText, Text: "x"})
	r.tick()
	r.advance(time.Minute)
	if n := len(r.posts()); n != 0 {
		t.Fatalf("a disabled officer posted %d times", n)
	}
	if r.src.reads != 0 {
		t.Fatalf("a disabled officer's transcript was read %d times", r.src.reads)
	}
	if r.o.TranscriptEnabled() {
		t.Fatal("the rig's officer reads as enabled")
	}
	// POSITIVE CONTROL: the same steps on an enabled officer do post.
	on := newRig(t, true, nil)
	item = on.claim()
	on.fwd.Claimed(on.o, instance, officers.ItemID(item))
	on.src.add(at(1), cctranscript.Event{Kind: cctranscript.KindText, Text: "x"})
	on.tick()
	if len(on.posts()) != 1 {
		t.Fatalf("the enabled control posted %d times", len(on.posts()))
	}
}

// droppedBuffer is muster_officer_transcript_events_total{result="dropped_buffer"}.
func droppedBuffer(t *testing.T) float64 { t.Helper(); return counter(t, "dropped_buffer") }

// counter is muster_officer_transcript_events_total{result=result}.
func counter(t *testing.T, result string) float64 {
	t.Helper()
	fams, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fams {
		if f.GetName() != "muster_officer_transcript_events_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "result" && l.GetValue() == result {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

func TestTheBufferIsCappedAndADropIsCounted(t *testing.T) {
	r := newRig(t, true, func(c *transcript.Config) { c.MaxBuffer = 10 })
	item := r.claim()
	r.fwd.Claimed(r.o, instance, officers.ItemID(item))
	before := droppedBuffer(t)
	r.hq.SetTranscriptStatus(http.StatusServiceUnavailable)
	for i := 0; i < 25; i++ {
		r.src.add(at(1), cctranscript.Event{Kind: cctranscript.KindText, Text: "e" + itoa(i)})
	}
	r.tick()
	if d := droppedBuffer(t) - before; d != 15 {
		t.Fatalf("dropped_buffer moved by %v, want 15 (25 read, 10 kept)", d)
	}
	if !strings.Contains(r.log.String(), "dropped") {
		t.Fatalf("a drop was not logged: %q", r.log.String())
	}
	// Backing off: an immediate tick does not post again.
	failed := len(r.posts()) + len(r.probes())
	if failed == 0 {
		t.Fatal("nothing reached HQ: the backoff below would be vacuous")
	}
	r.now = r.now.Add(10 * time.Millisecond)
	r.tick()
	if n := len(r.posts()) + len(r.probes()); n != failed {
		t.Fatalf("posted again inside the backoff (%d -> %d)", failed, n)
	}
	r.hq.SetTranscriptStatus(0)
	r.advance(5 * time.Minute)
	got := r.hq.Transcript(item)
	if len(got) != 10 || got[0].Text != "e15" || got[9].Text != "e24" {
		t.Fatalf("after recovery stored %d events, first %+v: want the newest 10 (e15..e24)", len(got), got)
	}
}

func TestRetriesAreCappedThenTheBatchIsDropped(t *testing.T) {
	r := newRig(t, true, func(c *transcript.Config) { c.MaxRetries = 2 })
	item := r.claim()
	r.fwd.Claimed(r.o, instance, officers.ItemID(item))
	r.tick() // the probe, while HQ answers
	r.hq.SetTranscriptStatus(http.StatusServiceUnavailable)
	r.src.add(at(1), cctranscript.Event{Kind: cctranscript.KindText, Text: "lost"})
	r.tick()
	for i := 0; i < 6; i++ {
		r.advance(5 * time.Minute)
	}
	if n := len(r.posts()); n != 3 {
		t.Fatalf("%d attempts, want 3 (1 + MaxRetries 2)", n)
	}
	r.hq.SetTranscriptStatus(0)
	r.src.add(at(2), cctranscript.Event{Kind: cctranscript.KindText, Text: "kept"})
	r.advance(5 * time.Minute)
	if got := r.hq.Transcript(item); len(got) != 1 || got[0].Text != "kept" {
		t.Fatalf("stored %+v, want only the event after the dropped batch (log: %s)", got, r.log)
	}
}

func TestAPassStartsAHeldItemAndStopsOneNoLongerHeld(t *testing.T) {
	r := newRig(t, true, nil)
	item := r.claim()
	held := []officers.Item{{ID: officers.ItemID(item), Status: officers.StatusClaimed, ClaimedBy: instance, LeaseExpiresAt: ptr(r.now.Add(time.Minute))}}
	// Another officer's instance, and an expired lease: neither is started.
	other := []officers.Item{
		{ID: "77", Status: officers.StatusClaimed, ClaimedBy: officers.InstanceName("other:thing", 1), LeaseExpiresAt: ptr(r.now.Add(time.Minute))},
		{ID: "78", Status: officers.StatusClaimed, ClaimedBy: officers.InstanceName(officerID, 1), LeaseExpiresAt: ptr(r.now.Add(-time.Minute))},
	}
	r.fwd.Sync(r.o, other, r.now)
	if len(r.fwd.Active()) != 0 {
		t.Fatalf("started for %v", r.fwd.Active())
	}
	r.fwd.Sync(r.o, held, r.now)
	if got := r.fwd.Active(); got[instance] != officers.ItemID(item) {
		t.Fatalf("active = %v", got)
	}
	r.now = r.now.Add(time.Second)
	r.fwd.Sync(r.o, nil, r.now)
	if len(r.fwd.Active()) != 0 {
		t.Fatalf("still active after the item left the held list: %v", r.fwd.Active())
	}
	// A claim made AFTER a pass's read is not undone by that pass.
	r.fwd.Claimed(r.o, instance, officers.ItemID(item))
	r.fwd.Sync(r.o, nil, r.now.Add(-time.Second))
	if len(r.fwd.Active()) != 1 {
		t.Fatalf("a stale pass stopped a fresh claim")
	}
}

// TestALineWithManyEventsPostsEveryOne: one user record carrying 40 parallel
// tool results is one transcript line with 40 events; every one is posted,
// in strictly increasing seq, none dropped as unsequenced.
func TestALineWithManyEventsPostsEveryOne(t *testing.T) {
	r := newRig(t, true, nil)
	item := r.claim()
	r.fwd.Claimed(r.o, instance, officers.ItemID(item))
	var evs []cctranscript.Event
	for i := 0; i < 40; i++ {
		evs = append(evs, cctranscript.Event{Kind: cctranscript.KindToolResult, ToolID: "t" + itoa(i), OK: true, Output: "result " + itoa(i)})
	}
	r.src.add(at(1), evs...)
	r.src.add(at(2), cctranscript.Event{Kind: cctranscript.KindText, Text: "the next line"})
	r.tick()
	got := r.hq.Transcript(item)
	if len(got) != 41 {
		t.Fatalf("stored %d events, want all 41 (log: %s)", len(got), r.log)
	}
	for i := 0; i < 40; i++ {
		if got[i].Text != "result "+itoa(i) {
			t.Fatalf("event %d = %q", i, got[i].Text)
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i].Seq <= got[i-1].Seq {
			t.Fatalf("seq not strictly increasing at %d: %d then %d", i, got[i-1].Seq, got[i].Seq)
		}
	}
	if got[40].Text != "the next line" {
		t.Fatalf("the following line: %q", got[40].Text)
	}
}

// TestARestartIntoANewConversationHeals: the one case the file position
// cannot order. A new conversation mid-item is shifted above the old one's
// seqs by the forwarder that saw it; a forwarder restarted INTO that
// conversation has lost the shift, so HQ skips its live events as old — and
// the forwarder re-sequences them above HQ's last_seq instead of losing every
// event until the new file outgrows the old.
func TestARestartIntoANewConversationHeals(t *testing.T) {
	r := newRig(t, true, nil)
	item := r.claim()
	r.fwd.Claimed(r.o, instance, officers.ItemID(item))
	for i := 0; i < 20; i++ {
		r.src.add(at(1), cctranscript.Event{Kind: cctranscript.KindText, Text: "old conversation"})
	}
	r.tick()
	r.src.newSession("99999999-2222-3333-4444-555555555555")
	r.src.add(at(2), cctranscript.Event{Kind: cctranscript.KindText, Text: "new conversation"})
	r.advance(3 * time.Second)
	if n := len(r.hq.Transcript(item)); n != 21 {
		t.Fatalf("stored %d before the restart, want 21", n)
	}

	r.restart()
	r.fwd.Sync(r.o, []officers.Item{{ID: officers.ItemID(item), Status: officers.StatusClaimed, ClaimedBy: instance, LeaseExpiresAt: ptr(r.now.Add(time.Hour))}}, r.now)
	r.tick() // the resume's re-read: all already stored (HQ skips it)
	r.src.add(at(3), cctranscript.Event{Kind: cctranscript.KindText, Text: "after the restart"})
	r.advance(3 * time.Second)
	r.src.add(at(4), cctranscript.Event{Kind: cctranscript.KindText, Text: "and after that"})
	r.advance(3 * time.Second)

	got := r.hq.Transcript(item)
	if len(got) != 23 || got[21].Text != "after the restart" || got[22].Text != "and after that" {
		var texts []string
		for _, e := range got {
			texts = append(texts, e.Text)
		}
		t.Fatalf("stored %d: %q (log: %s)", len(got), texts, r.log)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Seq <= got[i-1].Seq {
			t.Fatalf("seq not increasing at %d", i)
		}
	}
}
