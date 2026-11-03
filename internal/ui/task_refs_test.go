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
	// Fresh: no stale badge.
	fresh := n
	fresh.Ref = &notes.Reference{Key: n.Ref.Key, SyncedAt: now, Snapshot: n.Ref.Snapshot}
	if strings.Contains(render(TaskCardView{Note: fresh, Detail: true}), "data-ref-stale") {
		t.Error("a reference synced just now shows the stale badge")
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
