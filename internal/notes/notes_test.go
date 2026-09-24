package notes

import "testing"

func TestValidStatus(t *testing.T) {
	valid := []string{StatusOpen, StatusInProgress, StatusReadyForReview, StatusComplete}
	for _, s := range valid {
		if !ValidStatus(s) {
			t.Errorf("ValidStatus(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "done", "closed", "in progress", "complete "} {
		if ValidStatus(s) {
			t.Errorf("ValidStatus(%q) = true, want false", s)
		}
	}
}

func TestStatusAllowedForAgent(t *testing.T) {
	// Agents may move a task through the working states...
	for _, s := range []string{StatusOpen, StatusInProgress, StatusReadyForReview} {
		if !StatusAllowedForAgent(s) {
			t.Errorf("StatusAllowedForAgent(%q) = false, want true", s)
		}
	}
	// ...but never declare it complete (operator/human only).
	if StatusAllowedForAgent(StatusComplete) {
		t.Error("StatusAllowedForAgent(complete) = true, want false (only operator/human may complete)")
	}
	if StatusAllowedForAgent("bogus") {
		t.Error("StatusAllowedForAgent(bogus) = true, want false")
	}
}

func TestGroupNoteChildren(t *testing.T) {
	notesIn := []Note{{ID: 10}, {ID: 20}, {ID: 30}}
	atts := []Attachment{
		{ID: 1, NoteID: 10, Filename: "a.txt"},
		{ID: 2, NoteID: 10, Filename: "b.txt"},
		{ID: 3, NoteID: 30, Filename: "c.txt"},
		{ID: 4, NoteID: 99, Filename: "orphan.txt"}, // unknown note -> dropped
	}
	comments := []Comment{
		{ID: 1, NoteID: 20, Body: "first"},
		{ID: 2, NoteID: 20, Body: "second"},
		{ID: 3, NoteID: 10, Body: "only"},
	}

	groupNoteChildren(notesIn, atts, comments)

	// Note 10: two attachments in input order, one comment.
	if got := len(notesIn[0].Attachments); got != 2 {
		t.Fatalf("note 10 attachments = %d, want 2", got)
	}
	if notesIn[0].Attachments[0].Filename != "a.txt" || notesIn[0].Attachments[1].Filename != "b.txt" {
		t.Fatalf("note 10 attachments out of order: %+v", notesIn[0].Attachments)
	}
	if len(notesIn[0].Comments) != 1 || notesIn[0].Comments[0].Body != "only" {
		t.Fatalf("note 10 comments = %+v, want one 'only'", notesIn[0].Comments)
	}

	// Note 20: no attachments, two comments in order.
	if len(notesIn[1].Attachments) != 0 {
		t.Fatalf("note 20 attachments = %d, want 0", len(notesIn[1].Attachments))
	}
	if len(notesIn[1].Comments) != 2 || notesIn[1].Comments[0].Body != "first" || notesIn[1].Comments[1].Body != "second" {
		t.Fatalf("note 20 comments out of order: %+v", notesIn[1].Comments)
	}

	// Note 30: one attachment, no comments.
	if len(notesIn[2].Attachments) != 1 || notesIn[2].Attachments[0].Filename != "c.txt" {
		t.Fatalf("note 30 attachments = %+v", notesIn[2].Attachments)
	}
	if len(notesIn[2].Comments) != 0 {
		t.Fatalf("note 30 comments = %d, want 0", len(notesIn[2].Comments))
	}
}

func TestGroupNoteChildrenIdempotent(t *testing.T) {
	notesIn := []Note{{ID: 1, Attachments: []Attachment{{ID: 9, NoteID: 1}}}}
	// Re-grouping with fresh children must replace, not append to, prior state.
	groupNoteChildren(notesIn, []Attachment{{ID: 1, NoteID: 1}}, nil)
	if len(notesIn[0].Attachments) != 1 || notesIn[0].Attachments[0].ID != 1 {
		t.Fatalf("expected re-group to replace attachments, got %+v", notesIn[0].Attachments)
	}
}
