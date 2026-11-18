package api

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/notes"
)

// The reference pieces below the routes: the snapshot's caps, the route
// relay's once-only queueing, and the snapshot status's own staleness guard.

// TestASnapshotIsCappedNotRefused: the newest maxRefComments comments are kept
// (oldest first), a link that is not http(s) is dropped, a label is one line of
// plain text, an empty title falls back to the key.
func TestASnapshotIsCappedNotRefused(t *testing.T) {
	in := pluginRefIn{Title: "  ", URL: "javascript:alert(1)", Status: "open", StatusLabel: "in\nprogress\x00",
		UpdatedAt: "2026-01-02T03:04:05Z"}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < maxRefComments+7; i++ {
		in.Comments = append(in.Comments, struct {
			ID        string `json:"id"`
			Author    string `json:"author"`
			Body      string `json:"body"`
			CreatedAt string `json:"created_at"`
		}{ID: "c" + strconv.Itoa(i), Author: "someone", Body: "comment " + strconv.Itoa(i),
			// Out of order on the wire: the cut is by created_at, not position.
			CreatedAt: base.Add(time.Duration((i*7)%(maxRefComments+7)) * time.Minute).Format(time.RFC3339)})
	}
	snap := in.snapshot("tracker:4711")
	if snap.Title != "tracker:4711" || snap.URL != "" || snap.StatusLabel != "in progress" {
		t.Fatalf("title %q url %q label %q; want the key, no link, one line", snap.Title, snap.URL, snap.StatusLabel)
	}
	if len(snap.Comments) != maxRefComments {
		t.Fatalf("%d comments kept, want %d", len(snap.Comments), maxRefComments)
	}
	for i := 1; i < len(snap.Comments); i++ {
		if snap.Comments[i].CreatedAt.Before(snap.Comments[i-1].CreatedAt) {
			t.Fatalf("comments not oldest first at %d", i)
		}
	}
	// The 7 OLDEST (minutes 0..6) are the ones dropped.
	if first := snap.Comments[0].CreatedAt; !first.Equal(base.Add(7 * time.Minute)) {
		t.Fatalf("oldest kept comment at %s, want minute 7 (the newest %d kept)", first, maxRefComments)
	}
	if got := refURL("https://tracker.example.invalid/t/4711"); got != "https://tracker.example.invalid/t/4711" {
		t.Fatalf("an https link was dropped: %q", got)
	}
}

// TestARouteAnswerOnAReferenceQueuesOnce: a task route's relay is retried, and
// on a reference its answer is queued once per route (the dedupe key), never
// stored locally.
func TestARouteAnswerOnAReferenceQueuesOnce(t *testing.T) {
	f := newRefFixture(t)
	id := f.mustUpsert(t, f.key(t), refSnap("in_progress", "in progress", time.Now(), nil))
	c := notes.Comment{NoteID: id, Author: "mirror:sess via rt_1", Body: "the session's answer"}
	for i := 0; i < 2; i++ {
		wrote, queued, err := f.srv.addTaskCommentOnce(f.asOwner, commentByActor, c, "route:rt_1")
		if err != nil || wrote || !queued {
			t.Fatalf("relay #%d: wrote %v queued %v err %v; want queued, not written", i+1, wrote, queued, err)
		}
	}
	if got := f.queued(t, id); strings.Join(got, ",") != "comment:the session's answer:mirror:sess via rt_1" {
		t.Fatalf("a retried relay queued %v, want one write", got)
	}
	if _, _, _, n := f.row(t, id); n != 0 {
		t.Fatalf("the answer was stored on the reference (%d comments)", n)
	}
	// muster's own bookkeeping stays local on a reference.
	if _, q, err := f.srv.addTaskComment(f.asOwner, commentByMuster, notes.Comment{NoteID: id, Author: "system", Body: "bookkeeping"}); err != nil || q != nil {
		t.Fatalf("a muster comment on a reference: queued %v err %v; want stored", q, err)
	}
	if _, _, _, n := f.row(t, id); n != 1 {
		t.Fatalf("muster's comment on the reference: %d comments, want 1", n)
	}
}

// TestASnapshotStatusFollowsOnlyItsOwnSnapshot: the snapshot's status write is
// conditional on that snapshot still being the stored one, so an older
// snapshot's status landing after a newer snapshot was applied moves nothing.
func TestASnapshotStatusFollowsOnlyItsOwnSnapshot(t *testing.T) {
	f := newRefFixture(t)
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)
	id := f.mustUpsert(t, f.key(t), refSnap("in_progress", "in progress", t0.Add(time.Minute), nil))
	_, err := f.srv.setNoteStatus(notes.OnlyIfSnapshotAt(f.asOwner, t0), writerPluginReference, id, notes.StatusComplete)
	if !errors.Is(err, notes.ErrStatusMoved) {
		t.Fatalf("an older snapshot's status write: %v, want ErrStatusMoved", err)
	}
	if st, _, _, _ := f.row(t, id); st != notes.StatusInProgress {
		t.Fatalf("an older snapshot's status overwrote the newer one's: %q", st)
	}
	// Positive control: the stored snapshot's own instant moves it.
	if _, err := f.srv.setNoteStatus(notes.OnlyIfSnapshotAt(f.asOwner, t0.Add(time.Minute)), writerPluginReference, id, notes.StatusComplete); err != nil {
		t.Fatalf("the current snapshot's status write: %v", err)
	}
	if st, _, _, _ := f.row(t, id); st != notes.StatusComplete {
		t.Fatalf("positive control: status %q, want complete", st)
	}
}
