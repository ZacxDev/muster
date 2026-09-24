package notes

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Postgres-backed coverage for COMMENT soft delete.
//
// These are what make the in-memory fakeNotes honest at the SQL level: the API
// tests assert "a retracted comment's BODY is absent from every read, and a
// tombstone is present" against a fake, and that assertion is only worth something
// if the real queries actually redact. PG-gated like the rest of this package:
// skipped unless MUSTER_TEST_DATABASE_URL points at a disposable database.

// TestPGStoreCommentSoftDeleteTombstonesBothReads is the seam test at the store
// level: the per-note read (ListComments) and the batched board read
// (listCommentsForNotes, reached through List) must BOTH return the retracted
// comment as a body-less tombstone — still present, still in position, still
// timestamped — and both must leave the other comment untouched.
//
// 🔴 The two halves are asserted SEPARATELY on purpose. "The body is gone" alone
// is satisfied by dropping the row (the behaviour this design rejects); "the row
// is still there" alone is satisfied by not redacting at all. Only the pair pins
// the tombstone.
func TestPGStoreCommentSoftDeleteTombstonesBothReads(t *testing.T) {
	store, pool, ctx := softDeleteTestStore(t)

	n, err := store.Create(ctx, Note{Directory: "/work/orbit", Body: "comment soft delete"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	kept, err := store.AddComment(ctx, Comment{NoteID: n.ID, Author: "swift-owl", Body: "the investigation"})
	if err != nil {
		t.Fatalf("add kept comment: %v", err)
	}
	stray, err := store.AddComment(ctx, Comment{NoteID: n.ID, Author: "api", Body: "probe"})
	if err != nil {
		t.Fatalf("add stray comment: %v", err)
	}

	perNote := func() []Comment {
		cs, err := store.ListComments(ctx, n.ID)
		if err != nil {
			t.Fatalf("list comments: %v", err)
		}
		return cs
	}
	batched := func() []Comment {
		cs, err := store.listCommentsForNotes(ctx, []int64{n.ID})
		if err != nil {
			t.Fatalf("batched list comments: %v", err)
		}
		return cs
	}
	reads := map[string]func() []Comment{"ListComments": perNote, "listCommentsForNotes": batched}

	// Positive controls: both reads can see both comments, with bodies, and NOTHING
	// reads as retracted yet — so every assertion after the retraction is about the
	// retraction and not about the fixture.
	for name, read := range reads {
		got := read()
		if len(got) != 2 {
			t.Fatalf("positive control failed: %s returned %d comments before the retraction, want 2", name, len(got))
		}
		for _, c := range got {
			if c.Retracted {
				t.Fatalf("positive control failed: %s reports comment %d retracted before any retraction", name, c.ID)
			}
			if c.Body == "" {
				t.Fatalf("positive control failed: %s returned an empty body for comment %d before the retraction", name, c.ID)
			}
		}
	}

	if err := store.SoftDeleteComment(ctx, n.ID, stray.ID); err != nil {
		t.Fatalf("soft delete comment: %v", err)
	}

	for name, read := range reads {
		got := read()
		// The thread does NOT shorten.
		if len(got) != 2 {
			t.Errorf("%s after retraction returned %d comments, want 2 — a retracted comment must stay in the thread as a tombstone, not vanish", name, len(got))
			continue
		}
		// Position is preserved: oldest-first, so the kept comment is still first.
		if got[0].ID != kept.ID || got[1].ID != stray.ID {
			t.Errorf("%s after retraction returned ids [%d %d], want [%d %d] — position must be preserved", name, got[0].ID, got[1].ID, kept.ID, stray.ID)
		}
		if got[0].Retracted {
			t.Errorf("%s marked the WRONG comment retracted (the kept one)", name)
		}
		if got[0].Body != "the investigation" {
			t.Errorf("%s: the comment that was NOT retracted reads %q, want %q", name, got[0].Body, "the investigation")
		}
		if !got[1].Retracted {
			t.Errorf("%s did not report comment %d as retracted", name, stray.ID)
		}
		if got[1].Body != "" {
			t.Errorf("%s still returns the retracted body %q — the redaction must happen in SQL, not in a renderer", name, got[1].Body)
		}
		// The timestamp survives, so the tombstone can hold its place in the thread.
		if got[1].CreatedAt.IsZero() {
			t.Errorf("%s dropped the retracted comment's created_at; the tombstone has nothing to show", name)
		}
		if got[1].Author == "" {
			t.Errorf("%s dropped the retracted comment's author", name)
		}
	}

	// The ROW survives — checked WITHOUT the store, so it cannot be an artefact of
	// the store's own filtering.
	if got := rawCommentCount(t, pool, ctx, n.ID); got != 2 {
		t.Errorf("a retracted comment's row must survive: raw count = %d, want 2", got)
	}
	var deletedAt *string
	if err := pool.QueryRow(ctx, `SELECT deleted_at::text FROM note_comments WHERE id=$1`, stray.ID).Scan(&deletedAt); err != nil {
		t.Fatalf("read deleted_at: %v", err)
	}
	if deletedAt == nil {
		t.Errorf("SoftDeleteComment did not stamp note_comments.deleted_at")
	}
	// The comment that was NOT retracted must still read as live.
	var keptDeletedAt *string
	if err := pool.QueryRow(ctx, `SELECT deleted_at::text FROM note_comments WHERE id=$1`, kept.ID).Scan(&keptDeletedAt); err != nil {
		t.Fatalf("read kept deleted_at: %v", err)
	}
	if keptDeletedAt != nil {
		t.Errorf("the retraction stamped the WRONG row: kept comment has deleted_at=%v", *keptDeletedAt)
	}
}

// TestPGStoreSoftDeleteCommentErrorContract pins every ErrNoRows case against real
// Postgres — the codes the HTTP 404s rest on.
func TestPGStoreSoftDeleteCommentErrorContract(t *testing.T) {
	store, _, ctx := softDeleteTestStore(t)

	a, err := store.Create(ctx, Note{Directory: "/work/a", Body: "task a"})
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	b, err := store.Create(ctx, Note{Directory: "/work/b", Body: "task b"})
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	c, err := store.AddComment(ctx, Comment{NoteID: a.ID, Author: "api", Body: "probe"})
	if err != nil {
		t.Fatalf("add comment: %v", err)
	}

	// Cross-task: a's comment addressed through b.
	if err := store.SoftDeleteComment(ctx, b.ID, c.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("cross-task retraction = %v, want pgx.ErrNoRows", err)
	}
	// Unknown comment id.
	if err := store.SoftDeleteComment(ctx, a.ID, 999999999); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("unknown comment id = %v, want pgx.ErrNoRows", err)
	}
	// The real one succeeds (proving the failures above were not vacuous).
	if err := store.SoftDeleteComment(ctx, a.ID, c.ID); err != nil {
		t.Fatalf("retraction of a real comment = %v, want nil", err)
	}
	// Repeat retraction.
	if err := store.SoftDeleteComment(ctx, a.ID, c.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("repeat retraction = %v, want pgx.ErrNoRows", err)
	}

	// A comment on a DISMISSED task cannot be retracted.
	d, err := store.Create(ctx, Note{Directory: "/work/d", Body: "task d"})
	if err != nil {
		t.Fatalf("create d: %v", err)
	}
	dc, err := store.AddComment(ctx, Comment{NoteID: d.ID, Author: "api", Body: "on a doomed task"})
	if err != nil {
		t.Fatalf("add comment d: %v", err)
	}
	if err := store.SoftDelete(ctx, d.ID); err != nil {
		t.Fatalf("dismiss d: %v", err)
	}
	if err := store.SoftDeleteComment(ctx, d.ID, dc.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("retraction on a dismissed task = %v, want pgx.ErrNoRows", err)
	}
}

// TestPGStoreRestoreKeepsCommentRetraction pins the interaction between the two
// soft deletes: restoring a dismissed task brings its thread back with any
// individually-retracted comment STILL retracted (present, body-less). The two
// rules are independent and must compose — a restore must not un-retract.
func TestPGStoreRestoreKeepsCommentRetraction(t *testing.T) {
	store, _, ctx := softDeleteTestStore(t)

	n, err := store.Create(ctx, Note{Directory: "/work/orbit", Body: "compose"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.AddComment(ctx, Comment{NoteID: n.ID, Author: "swift-owl", Body: "keep me"}); err != nil {
		t.Fatalf("add kept: %v", err)
	}
	stray, err := store.AddComment(ctx, Comment{NoteID: n.ID, Author: "api", Body: "probe"})
	if err != nil {
		t.Fatalf("add stray: %v", err)
	}
	if err := store.SoftDeleteComment(ctx, n.ID, stray.ID); err != nil {
		t.Fatalf("retract: %v", err)
	}
	if err := store.SoftDelete(ctx, n.ID); err != nil {
		t.Fatalf("dismiss: %v", err)
	}

	restored, err := store.Restore(ctx, n.ID)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if len(restored.Comments) != 2 {
		t.Fatalf("restored thread has %d comments, want 2 (the kept one plus the retracted one's tombstone)", len(restored.Comments))
	}
	if restored.Comments[0].Retracted || restored.Comments[0].Body != "keep me" {
		t.Errorf("restored thread[0] = {body:%q retracted:%v}, want {body:%q retracted:false}",
			restored.Comments[0].Body, restored.Comments[0].Retracted, "keep me")
	}
	if !restored.Comments[1].Retracted || restored.Comments[1].Body != "" {
		t.Errorf("restored thread[1] = {body:%q retracted:%v}, want {body:\"\" retracted:true} — "+
			"restoring the TASK must not un-retract a comment that was retracted in its own right",
			restored.Comments[1].Body, restored.Comments[1].Retracted)
	}
	if restored.Comments[1].ID != stray.ID {
		t.Errorf("restored thread[1].ID = %d, want the retracted comment %d", restored.Comments[1].ID, stray.ID)
	}
}
