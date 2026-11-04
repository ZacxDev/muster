package ui

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/notes"
)

// TestAReferenceCardShowsItsUpstream: the detail card of a plugin task
// reference carries the upstream block (link, label, sync time, a stale badge
// past refStaleAfter), the upstream's comments ESCAPED, the queued writes with
// their state, and the muster-only label over its own comments. An ordinary
// task's card carries none of it.
func TestAReferenceCardShowsItsUpstream(t *testing.T) {
	now := time.Now()
	n := notes.Note{ID: 42, Title: "Rotate the key", Body: "b", Status: notes.StatusInProgress, UpdatedAt: now,
		Comments: []notes.Comment{{ID: 1, NoteID: 42, Author: "system", Body: "flagged", CreatedAt: now}},
		Ref: &notes.Reference{Key: "tracker:4711", PluginID: 3, SyncedAt: now.Add(-20 * time.Minute),
			Snapshot: notes.RefSnapshot{Title: "Rotate the key", URL: "https://tracker.example.invalid/t/4711",
				Status: notes.StatusInProgress, StatusLabel: "doing it", UpdatedAt: now,
				Comments: []notes.RefComment{{ID: "c1", Author: "A. Person", Body: "<script>x</script> upstream says", CreatedAt: now}}}}}
	writes := []notes.RefWrite{
		{ID: 7, NoteID: 42, Kind: notes.RefWriteComment, Body: "my reply", Author: "user", State: notes.RefWritePending},
		{ID: 8, NoteID: 42, Kind: notes.RefWriteStatus, Status: notes.StatusComplete, Author: "user", State: notes.RefWriteFailed, Error: "not allowed upstream"},
	}
	render := func(v TaskCardView) string {
		t.Helper()
		var b bytes.Buffer
		if err := RenderNoteCard(&b, v); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	out := render(TaskCardView{Note: n, Detail: true, RefWrites: writes})
	for _, want := range []string{
		`data-task-reference="tracker:4711"`, `data-task-ref-section`, `href="https://tracker.example.invalid/t/4711"`,
		`data-ref-stale`, `data-ref-comment="c1"`, `&lt;script&gt;`, `data-ref-write="7"`, `data-ref-write-state="pending"`,
		`sending…`, `data-ref-write-state="failed"`, `failed: not allowed upstream`, `data-ref-local-label`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("reference detail card lacks %s", want)
		}
	}
	if strings.Contains(out, "<script>x</script>") {
		t.Error("an upstream comment body was rendered unescaped")
	}
	// The FAILED write (8) carries the owner's Retry and Dismiss, each a form
	// that posts without JavaScript and swaps the card with it; the pending
	// one (7) carries neither.
	for _, want := range []string{
		`data-ref-write-retry="8"`, `data-ref-write-dismiss="8"`,
		`action="/tasks/42/ref-writes/8/retry"`, `hx-post="/tasks/42/ref-writes/8/retry"`,
		`action="/tasks/42/ref-writes/8/dismiss"`, `hx-post="/tasks/42/ref-writes/8/dismiss"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the failed write lacks %s", want)
		}
	}
	for _, absent := range []string{`data-ref-write-retry="7"`, `data-ref-write-dismiss="7"`, `/ref-writes/7/`} {
		if strings.Contains(out, absent) {
			t.Errorf("the PENDING write carries %s", absent)
		}
	}
	// Fresh: no stale badge.
	fresh := n
	fresh.Ref = &notes.Reference{Key: n.Ref.Key, SyncedAt: now, Snapshot: n.Ref.Snapshot}
	if strings.Contains(render(TaskCardView{Note: fresh, Detail: true}), "data-ref-stale") {
		t.Error("a reference synced just now shows the stale badge")
	}
	// The badge is ext_synced_at's age alone, at 15 minutes: either side of the
	// boundary, with the snapshot's own updated_at deliberately OLD both times
	// (the upstream's clock must not decide it).
	for _, c := range []struct {
		age   time.Duration
		stale bool
	}{{14 * time.Minute, false}, {16 * time.Minute, true}, {3 * time.Hour, true}} {
		r := n
		snap := n.Ref.Snapshot
		snap.UpdatedAt = now.Add(-48 * time.Hour)
		r.Ref = &notes.Reference{Key: n.Ref.Key, SyncedAt: now.Add(-c.age), Snapshot: snap}
		if got := strings.Contains(render(TaskCardView{Note: r, Detail: true}), "data-ref-stale"); got != c.stale {
			t.Errorf("synced %s ago: stale badge %v, want %v", c.age, got, c.stale)
		}
	}
	// An ordinary task: none of it.
	plain := n
	plain.Ref = nil
	got := render(TaskCardView{Note: plain, Detail: true})
	for _, absent := range []string{"data-task-reference", "data-task-ref-section", "data-ref-local-label"} {
		if strings.Contains(got, absent) {
			t.Errorf("an ordinary task's card carries %s", absent)
		}
	}
}
