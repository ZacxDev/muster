package transcript_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
)

func stored(r *rig, item string) []hqtest.TranscriptEvent { return r.hq.Transcript(item) }

func texts(evs []hqtest.TranscriptEvent) string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Text)
	}
	return strings.Join(out, "|")
}

// addUntimed appends a line whose record carries no timestamp (At 0).
func (s *fakeSource) addUntimed(evs ...cctranscript.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.end += 137
	s.lines = append(s.lines, cctranscript.Line{Events: evs, End: s.end})
}

func text(s string) cctranscript.Event {
	return cctranscript.Event{Kind: cctranscript.KindText, Text: s}
}

func held(item string, inst string, until time.Time) []officers.Item {
	return []officers.Item{{ID: officers.ItemID(item), Status: officers.StatusClaimed, ClaimedBy: inst, LeaseExpiresAt: ptr(until)}}
}

// TestALostAcknowledgementIsNotResentAsNew: HQ stores a batch and then fails
// to answer (a 5xx after its commit). The re-send is answered stored 0,
// last_seq = the batch's end — which is NOT a reason to re-sequence: the
// events are stored exactly once.
func TestALostAcknowledgementIsNotResentAsNew(t *testing.T) {
	r := newRig(t, true, nil)
	item := r.claim()
	r.fwd.Claimed(r.o, instance, officers.ItemID(item))
	r.tick() // the probe
	posted, skipped := counter(t, "posted"), counter(t, "skipped_by_hq")
	r.hq.FailTranscript(http.StatusBadGateway, 1, true)
	r.src.add(at(1), text("one"), text("two"))
	r.tick()                   // stored, then 502
	r.advance(5 * time.Minute) // the re-send: stored 0
	r.src.add(at(2), text("three"))
	r.advance(5 * time.Minute)
	if got := texts(stored(r, item)); got != "one|two|three" {
		t.Fatalf("stored %q, want each event once (log: %s)", got, r.log)
	}
	if d := counter(t, "posted") - posted; d != 1 {
		t.Fatalf("posted moved by %v, want 1 (only what HQ stored as new on an answered post)", d)
	}
	if d := counter(t, "skipped_by_hq") - skipped; d != 2 {
		t.Fatalf("skipped_by_hq moved by %v, want 2 (the re-send HQ already held)", d)
	}
}

// TestAResumeWithNothingAtHQForwardsOnlyNewLines: muster resumes an item HQ
// holds no transcript for (first enable, or a restart before the first
// post). The backfill window is the instance's EARLIER work — not this
// item's — and is not sent.
func TestAResumeWithNothingAtHQForwardsOnlyNewLines(t *testing.T) {
	r := newRig(t, true, nil)
	for i := 0; i < 3; i++ {
		r.src.add(r.now.Add(-time.Minute), text("another item's work"))
	}
	item := r.claim()
	r.fwd.Sync(r.o, held(item, instance, r.now.Add(time.Hour)), r.now)
	r.tick()
	r.src.add(at(1), text("new"))
	r.advance(3 * time.Second)
	if got := texts(stored(r, item)); got != "new" {
		t.Fatalf("stored %q, want only the line written after the resume (log: %s)", got, r.log)
	}
}

// TestResumingAnItemAfterAnotherCompletesStartsLive: an instance holding A
// claims B (forwarding switches to B), completes B, and a pass resumes A.
// What the transcript holds since is B's work: none of it goes to A.
func TestResumingAnItemAfterAnotherCompletesStartsLive(t *testing.T) {
	r := newRig(t, true, nil)
	a := r.claim()
	r.fwd.Claimed(r.o, instance, officers.ItemID(a))
	r.src.add(at(1), text("a1"))
	r.tick()
	b := r.claim()
	r.fwd.Claimed(r.o, instance, officers.ItemID(b))
	r.src.add(at(2), text("b1"))
	r.advance(3 * time.Second)
	r.fwd.Ended(r.o, instance, officers.ItemID(b))
	r.fwd.Sync(r.o, held(a, instance, r.now.Add(time.Hour)), r.now)
	r.advance(3 * time.Second)
	r.src.add(at(3), text("a2"))
	r.advance(3 * time.Second)
	if got := texts(stored(r, a)); got != "a1|a2" {
		t.Fatalf("item A stored %q, want a1|a2 (B's work must not land in A) (log: %s)", got, r.log)
	}
	if got := texts(stored(r, b)); got != "b1" {
		t.Fatalf("item B stored %q", got)
	}
}

// TestAFreshClaimSkipsUntimedLinesBeforeIt: a line with no timestamp in the
// first read cannot be shown to follow the claim, so it is not sent.
func TestAFreshClaimSkipsUntimedLinesBeforeIt(t *testing.T) {
	r := newRig(t, true, nil)
	r.src.addUntimed(text("untimed, before the claim"))
	item := r.claim()
	r.fwd.Claimed(r.o, instance, officers.ItemID(item))
	r.tick()
	r.src.add(at(1), text("after"))
	r.advance(3 * time.Second)
	if got := texts(stored(r, item)); got != "after" {
		t.Fatalf("stored %q", got)
	}
}

// TestAReclaimByAnotherInstanceLandsAboveHQsLastSeq: HQ's last_seq is per
// ITEM. Instance 1 forwards and releases; instance 2 (its own transcript,
// offsets from zero) claims the same item: its events are stored, not
// skipped as old.
func TestAReclaimByAnotherInstanceLandsAboveHQsLastSeq(t *testing.T) {
	r := newRig(t, true, nil)
	two := officers.InstanceName(officerID, 2)
	other := newSource()
	other.session = "22222222-2222-3333-4444-555555555555"
	r.src.others = map[string]*fakeSource{two: other}
	item := r.claim()
	r.fwd.Claimed(r.o, instance, officers.ItemID(item))
	for i := 0; i < 10; i++ {
		r.src.add(at(1), text("instance one"))
	}
	r.tick()
	r.fwd.Ended(r.o, instance, officers.ItemID(item))
	r.hq.SetClaim(item, two, r.now.Add(time.Hour))
	r.fwd.Claimed(r.o, two, officers.ItemID(item))
	other.add(at(2), text("instance two"))
	r.advance(3 * time.Second)
	got := stored(r, item)
	if len(got) != 11 || got[10].Text != "instance two" || got[10].Instance != two {
		t.Fatalf("stored %q (log: %s)", texts(got), r.log)
	}
}

// TestATooLargeAnswerDropsTheBatchAndHalvesTheNext: a 413 is not a decision
// about the item: that batch is dropped (counted) and the next is sent at
// half the size; forwarding goes on.
func TestATooLargeAnswerDropsTheBatchAndHalvesTheNext(t *testing.T) {
	r := newRig(t, true, nil)
	item := r.claim()
	r.fwd.Claimed(r.o, instance, officers.ItemID(item))
	r.tick() // the probe
	before := counter(t, "dropped_too_large")
	r.hq.FailTranscript(http.StatusRequestEntityTooLarge, 1, false)
	for i := 0; i < 150; i++ {
		r.src.add(at(1), text("e"+itoa(i)))
	}
	r.tick()
	if d := counter(t, "dropped_too_large") - before; d != 100 {
		t.Fatalf("dropped_too_large moved by %v, want 100", d)
	}
	posts := r.posts()
	for _, q := range posts[1:] {
		if n := len(eventsOf(q)); n > 50 {
			t.Fatalf("a post after the 413 carried %d events, want at most 50", n)
		}
	}
	got := stored(r, item)
	if len(got) != 50 || got[0].Text != "e100" {
		t.Fatalf("stored %d events after the 413, first %+v", len(got), got)
	}
	if len(r.fwd.Active()) != 1 {
		t.Fatal("a 413 stopped forwarding")
	}
}

// TestATokenCutByACapIsWithheld: ccd caps a long field with "…"; a token cut
// there leaves a PREFIX no full-token pattern matches. The line it ends is
// withheld. An ordinary capped line passes unchanged (positive control).
func TestATokenCutByACapIsWithheld(t *testing.T) {
	r := newRig(t, true, nil)
	item := r.claim()
	r.fwd.Claimed(r.o, instance, officers.ItemID(item))
	cutGitHub := "ghp_" + strings.Repeat("A1b2", 3) // 16 characters: below the full pattern's 20
	r.src.add(at(1),
		cctranscript.Event{Kind: cctranscript.KindToolResult, ToolID: "a", OK: true, Output: "first line\nthe header was " + officerToken[:12] + "…"},
		cctranscript.Event{Kind: cctranscript.KindToolResult, ToolID: "b", OK: true, Output: "clone with " + cutGitHub + "…"},
		cctranscript.Event{Kind: cctranscript.KindToolResult, ToolID: "c", OK: true, Output: "an ordinary long output, cut…"})
	r.tick()
	got := stored(r, item)
	if len(got) != 3 {
		t.Fatalf("stored %d", len(got))
	}
	if strings.Contains(got[0].Text, officerToken[:12]) || !strings.HasPrefix(got[0].Text, "first line\n") {
		t.Fatalf("a cut officer token: %q", got[0].Text)
	}
	if strings.Contains(got[1].Text, cutGitHub) {
		t.Fatalf("a cut GitHub token: %q", got[1].Text)
	}
	if got[2].Text != "an ordinary long output, cut…" {
		t.Fatalf("an ordinary capped line was changed: %q", got[2].Text)
	}
}
