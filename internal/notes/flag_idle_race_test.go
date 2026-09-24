package notes

import (
	"sync"
	"testing"
	"time"
)

// The two FlagIdle properties that a SEQUENTIAL test cannot see. Both are
// PG-gated through flagIdleStore (flag_idle_test.go), for the same reason the
// rest of that file is: they are claims about what Postgres does, so only
// Postgres can be asked.
//
//	MUSTER_TEST_DATABASE_URL='postgres://postgres:x@localhost:55432/postgres?sslmode=disable' \
//	  go test ./internal/notes -run FlagIdle

// TestFlagIdleWritesOnceUnderConcurrency is the CONCURRENT regression test for
// the race FlagIdle's own comment used to claim it was immune to.
//
// 🔴 The retired claim was that Postgres's EvalPlanQual re-check makes the
// `NOT EXISTS` guard race-safe on its own: "the loser's re-check sees the
// winner's comment". It does not. EvalPlanQual re-evaluates the qual against the
// updated `notes` TUPLE, but the `NOT EXISTS` subplan reads `note_comments` on
// the STATEMENT's original snapshot — Postgres documents exactly this ("it can
// see the effects of concurrent updating commands on the same rows it is trying
// to update, but it does not see effects of those commands on other rows"). So
// the loser's re-check looked at a snapshot taken before the winner committed,
// found no comment, and wrote a second one. Measured against real Postgres 16 at
// d7967b76c: concurrent callers duplicated the comment on every trial.
//
// The fix is the explicit `SELECT … FOR UPDATE` that FlagIdle now issues as its
// OWN command before the CTE: the loser blocks on the row lock, and the CTE that
// follows is a NEW command, so under READ COMMITTED it takes a fresh snapshot
// taken AFTER the winner committed — and the marker is then visible to it.
//
// Concurrency, not repetition: TestFlagIdleWritesAtMostOncePerIdlePeriod already
// covers the sequential case, and it passes at every revision of this method,
// including the racy one.
func TestFlagIdleWritesOnceUnderConcurrency(t *testing.T) {
	store, ctx := flagIdleStore(t)

	const (
		trials  = 12
		workers = 4 // MaxConns is 8; each worker holds one for its transaction.
	)
	dupTrials, worstComments, worstWriters := 0, 0, 0
	for trial := 0; trial < trials; trial++ {
		n, err := store.Create(ctx, Note{Directory: "/work/orbit", Body: "an abandoned task"})
		if err != nil {
			t.Fatalf("trial %d create: %v", trial, err)
		}
		defer store.Delete(ctx, n.ID)

		start := make(chan struct{})
		var wg sync.WaitGroup
		wrote := make([]bool, workers)
		errs := make([]error, workers)
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				<-start
				wrote[w], errs[w] = store.FlagIdle(ctx, n.ID, "stale",
					"Task idle — tagged **stale** by the retention sweep.")
			}(w)
		}
		close(start)
		wg.Wait()

		writers := 0
		for w := 0; w < workers; w++ {
			if errs[w] != nil {
				t.Fatalf("trial %d worker %d: FlagIdle: %v", trial, w, errs[w])
			}
			if wrote[w] {
				writers++
			}
		}
		cs, err := store.ListComments(ctx, n.ID)
		if err != nil {
			t.Fatalf("trial %d ListComments: %v", trial, err)
		}
		if writers != 1 || len(cs) != 1 {
			dupTrials++
			if writers > worstWriters {
				worstWriters = writers
			}
			if len(cs) > worstComments {
				worstComments = len(cs)
			}
		}
	}
	if dupTrials > 0 {
		t.Errorf("%d of %d trials with %d concurrent FlagIdle calls on ONE never-reaped task "+
			"double-wrote: the worst trial reported %d writers and left %d comments in the "+
			"thread (want exactly 1 of each, in every trial).\n"+
			"The `NOT EXISTS` clause inside the UPDATE's WHERE is NOT sufficient on its own: "+
			"EvalPlanQual re-checks the qual against the updated `notes` tuple, but the subplan "+
			"on `note_comments` still reads the statement's ORIGINAL snapshot, so the loser "+
			"never sees the winner's just-committed comment. FlagIdle must take the note's row "+
			"lock in a SEPARATE, EARLIER command (SELECT … FOR UPDATE) so that the CTE which "+
			"follows runs on a snapshot taken after the lock was granted.",
			dupTrials, trials, workers, worstWriters, worstComments)
	}
}

// TestListSummariesCarriesUpdatedAtForTheReaper pins the SEAM between the store's
// cheap projection and its newest consumer.
//
// api.reapIdleTasks reads exactly two fields — ID and UpdatedAt — so it uses
// ListSummaries rather than List, which would run four queries and hydrate every
// attachment, comment and session link 48 times a day for nothing. The projection
// therefore has to carry updated_at, and 🔴 dropping it FAILS SILENTLY IN THE
// DANGEROUS DIRECTION: a Note scanned without it holds the ZERO time, which is
// before every cutoff, so the sweep would consider the ENTIRE BOARD idle and flag
// all of it. There is no error and no log line; the only symptom is a board full
// of `stale` chips.
//
// The assertion is against the row's real timestamp, not merely "non-zero", so a
// projection that filled the field from the wrong column is caught too — and the
// fixture is DELIBERATELY WRITTEN TWICE for that. On a freshly created note
// `created_at` and `updated_at` are the same value, so a mutant projecting
// `created_at` instead SURVIVED this test until the AddTags below was added; a
// fixture whose two candidate columns cannot differ cannot see that mutation.
func TestListSummariesCarriesUpdatedAtForTheReaper(t *testing.T) {
	store, ctx := flagIdleStore(t)

	n, err := store.Create(ctx, Note{Directory: "/work/orbit", Body: "a task the reaper will size up"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer store.Delete(ctx, n.ID)

	// Move updated_at OFF created_at. Without this the two are equal and the
	// assertion below is blind to the wrong column.
	time.Sleep(10 * time.Millisecond)
	if _, err := store.AddTags(ctx, n.ID, []string{"touched"}); err != nil {
		t.Fatalf("AddTags: %v", err)
	}

	want, err := store.Get(ctx, n.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if want.UpdatedAt.IsZero() {
		t.Fatal("fixture error: Get returned a zero updated_at, so the comparison would be vacuous")
	}
	if !want.UpdatedAt.After(want.CreatedAt) {
		t.Fatalf("fixture error: updated_at (%s) is not strictly after created_at (%s), so a "+
			"projection reading the WRONG column would pass this test",
			want.UpdatedAt.Format(time.RFC3339Nano), want.CreatedAt.Format(time.RFC3339Nano))
	}

	sums, err := store.ListSummaries(ctx)
	if err != nil {
		t.Fatalf("ListSummaries: %v", err)
	}
	var got *Note
	for i := range sums {
		if sums[i].ID == n.ID {
			got = &sums[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("created note %d missing from ListSummaries", n.ID)
	}
	if !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Errorf("ListSummaries returned updated_at %s for note %d, want %s (Get's value).\n"+
			"api.reapIdleTasks selects on this field. A ZERO here is BEFORE every cutoff, so "+
			"the next sweep flags the WHOLE BOARD `stale` and comments on every task — with no "+
			"error and no log line to say so. Put `updated_at` back in the projection and in "+
			"the Scan target list.",
			got.UpdatedAt.Format(time.RFC3339Nano), n.ID, want.UpdatedAt.Format(time.RFC3339Nano))
	}
}

// TestFlagIdleRecognisesAReapWrittenByThePreviousImplementation pins the claim
// this change makes about the EXISTING board: that the new predicate recognises
// reaps the OLD implementation recorded, so deploying it does not hand ~180
// already-flagged tasks a second, identical comment.
//
// 🔴 The trap is a STRICT inequality. The old reaper marked a task with AddTags
// then AddComment, and AddComment inserts the comment (created_at DEFAULT now())
// AND writes `updated_at = now()` in the SAME statement — `now()` is the
// transaction timestamp, so the two land on the IDENTICAL value. Under a marker
// of `c.created_at > notes.updated_at`, equal is not greater, so every legacy
// reap was invisible to its successor and the first sweep after deploy re-reaped
// the whole flagged population. The marker is `>=` for that reason.
//
// This replays the old implementation VERBATIM rather than hand-inserting a row,
// so the fixture cannot drift away from what production actually wrote. The tie
// is asserted explicitly first: if AddComment ever stops bumping updated_at in
// the same statement, this test would otherwise pass for the wrong reason.
func TestFlagIdleRecognisesAReapWrittenByThePreviousImplementation(t *testing.T) {
	store, ctx := flagIdleStore(t)

	n, err := store.Create(ctx, Note{Directory: "/work/orbit", Body: "an abandoned task"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer store.Delete(ctx, n.ID)

	// The PREVIOUS implementation, verbatim: AddTags then AddComment.
	if _, err := store.AddTags(ctx, n.ID, []string{"stale"}); err != nil {
		t.Fatalf("legacy AddTags: %v", err)
	}
	legacy, err := store.AddComment(ctx, Comment{
		NoteID: n.ID,
		Author: ReapCommentAuthor,
		Body:   "Task idle for >168h0m0s — tagged **stale** by the retention sweep.",
	})
	if err != nil {
		t.Fatalf("legacy AddComment: %v", err)
	}
	marked, err := store.Get(ctx, n.ID)
	if err != nil {
		t.Fatalf("get after legacy reap: %v", err)
	}
	if !legacy.CreatedAt.Equal(marked.UpdatedAt) {
		t.Fatalf("fixture no longer reproduces the TIE this test exists for: legacy reap "+
			"comment created_at=%s, notes.updated_at=%s. The old reaper's AddComment wrote both "+
			"in one statement, so they were equal; re-check AddComment before trusting this test.",
			legacy.CreatedAt.Format(time.RFC3339Nano), marked.UpdatedAt.Format(time.RFC3339Nano))
	}

	ok, err := store.FlagIdle(ctx, n.ID, "stale",
		"Task idle — tagged **stale** by the retention sweep.")
	if err != nil {
		t.Fatalf("FlagIdle: %v", err)
	}
	if ok {
		t.Errorf("FlagIdle RE-REAPED a task the PREVIOUS implementation had already reaped. " +
			"On the first sweep after deploy every one of the ~180 already-flagged tasks then " +
			"gets a second, identical comment — the opposite of what this change promises. " +
			"Cause: the legacy comment's created_at EQUALS notes.updated_at (AddComment writes " +
			"both in one statement), and a strict `>` marker does not match equal. The marker " +
			"must be `c.created_at >= notes.updated_at`.")
	}
	cs, err := store.ListComments(ctx, n.ID)
	if err != nil {
		t.Fatalf("ListComments: %v", err)
	}
	if len(cs) != 1 {
		t.Errorf("thread has %d comments after a legacy reap plus one new sweep, want 1", len(cs))
	}
}
