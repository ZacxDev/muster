package api

import (
	"encoding/json"
	"testing"

	"github.com/ZacxDev/muster/internal/notes"
)

// TestTheChiefsTasksListCarriesEachTasksStatusAndGate: the chief's tasks_list,
// over /mcp against REAL Postgres, reports each task's real status, its status
// filter selects on it, and a gated task carries its gate reason.
//
// The chief's list reads the whole board through notes.ListSummaries, whose
// projection once omitted `status` — so every row read `"status": ""` and the
// `open` filter returned nothing, while GET /api/tasks/{id} showed the task
// open. A worker's list reads each task through Get, which always carried the
// column, so only the chief (the whole-board path) was blind.
//
// The three tasks sit in three DIFFERENT statuses and only one is gated, so a
// row that copied a constant, or the gate onto every row, is caught.
func TestTheChiefsTasksListCarriesEachTasksStatusAndGate(t *testing.T) {
	f := newCCChiefFixture(t)
	mk := func(title string, tags []string, status string) int64 {
		t.Helper()
		n, err := f.notes.Create(f.asOwner, notes.Note{Title: title, Body: title, Tags: tags})
		if err != nil {
			t.Fatal(err)
		}
		if status != notes.StatusOpen {
			if _, err := f.notes.SetStatus(f.asOwner, n.ID, status); err != nil {
				t.Fatal(err)
			}
		}
		return n.ID
	}
	open := mk("status open", nil, notes.StatusOpen)
	busy := mk("status in progress", nil, notes.StatusInProgress)
	gated := mk("status gated", []string{"gate:signoff"}, notes.StatusReadyForReview)

	list := func(args map[string]any) map[int64]map[string]any {
		t.Helper()
		text, isErr, rpc := f.call(t, f.chiefTok, "tasks_list", args)
		if rpc != nil || isErr {
			t.Fatalf("tasks_list %v: rpc=%+v text=%s", args, rpc, text)
		}
		var res struct {
			Tasks []map[string]any `json:"tasks"`
		}
		if err := json.Unmarshal([]byte(text), &res); err != nil {
			t.Fatalf("tasks_list %v: %v: %s", args, err, text)
		}
		out := map[int64]map[string]any{}
		for _, r := range res.Tasks {
			id, _ := r["id"].(float64)
			out[int64(id)] = r
		}
		return out
	}

	all := list(map[string]any{})
	for id, want := range map[int64]string{open: "open", busy: "in_progress", gated: "ready_for_review"} {
		row, ok := all[id]
		if !ok {
			t.Fatalf("task %d missing from the chief's tasks_list", id)
		}
		if row["status"] != want {
			t.Errorf("task %d: tasks_list status %q, want %q (row %v)", id, row["status"], want, row)
		}
	}
	if g := all[gated]["gate"]; g != "signoff" {
		t.Errorf("gated task %d: tasks_list gate %v, want %q (row %v)", gated, g, "signoff", all[gated])
	}
	for _, id := range []int64{open, busy} {
		if g, has := all[id]["gate"]; has {
			t.Errorf("ungated task %d carries a gate %v; the field must be absent", id, g)
		}
	}

	onlyOpen := list(map[string]any{"status": "open"})
	if _, ok := onlyOpen[open]; !ok {
		t.Errorf("tasks_list {status: open} does not return the open task %d (got %d rows)", open, len(onlyOpen))
	}
	for _, id := range []int64{busy, gated} {
		if _, ok := onlyOpen[id]; ok {
			t.Errorf("tasks_list {status: open} returned task %d, which is not open", id)
		}
	}
}
