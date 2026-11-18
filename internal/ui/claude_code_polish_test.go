package ui

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/notes"
)

// TestTheBoardCardPrintsTheTimeTheBoardIsOrderedBy: the task board is ordered by
// most recent activity, so its card prints that time; the detail page keeps the
// creation age. Times are pairwise distinct (created < updated < activity).
func TestTheBoardCardPrintsTheTimeTheBoardIsOrderedBy(t *testing.T) {
	created := time.Date(2025, 4, 1, 8, 0, 0, 0, time.UTC)
	n := notes.Note{ID: 31, Body: "a task", Status: notes.StatusOpen,
		CreatedAt: created, UpdatedAt: created.Add(2 * time.Hour), ActivityAt: created.Add(30 * time.Hour)}
	ts := func(at time.Time) string { return `data-ts="` + strconv.FormatInt(at.UnixMilli(), 10) + `"` }

	board := renderNode(t, noteCard(TaskCardView{Note: n}))
	if !strings.Contains(board, ts(n.ActivityAt)) {
		t.Errorf("the board card does not print its last activity %s", ts(n.ActivityAt))
	}
	if strings.Contains(board, ts(created)) {
		t.Errorf("the board card still prints its creation time %s over an activity-ordered list", ts(created))
	}

	detail := renderNode(t, noteCard(TaskCardView{Note: n, Detail: true}))
	if !strings.Contains(detail, ts(created)) {
		t.Errorf("the detail card lost its creation age %s", ts(created))
	}

	// A single-card re-render reads the note through Get, which computes no
	// ActivityAt: the card must then print UpdatedAt, never fall back to creation.
	n.ActivityAt = time.Time{}
	rerender := renderNode(t, noteCard(TaskCardView{Note: n}))
	if !strings.Contains(rerender, ts(n.UpdatedAt)) {
		t.Errorf("a re-rendered board card without ActivityAt does not print UpdatedAt %s", ts(n.UpdatedAt))
	}
}
