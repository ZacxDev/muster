package notes

import "fmt"

// Task merging is SUPERSEDE, not absorb.
//
// 🔴 The alternative — "absorb the loser, then delete it" — was considered and
// REJECTED on measured grounds, and the reasons are load-bearing enough to record
// where the merge logic lives:
//
//   - Deleting a task goes through the SAME dismissTask helper as the human
//     dismiss, which TEARS DOWN the task's live agent pod (helm uninstall +
//     namespace delete). A merge is a bookkeeping action; it must not kill a
//     running agent. 🔴 This reason is UNAFFECTED by the 2026-08 soft delete and is
//     on its own sufficient: the teardown was never the recoverable part.
//   - agents.note_id is `ON DELETE SET NULL` and there is NO
//     store method to re-point it, so a HARD delete permanently orphans the
//     loser's agent history.
//   - note_attachments and note_comments CASCADE on notes(id), so under a hard
//     delete the loser's comment thread and files would be destroyed, not moved.
//   - Migration 0019 added `notes.deleted_at`, so a DISMISS is now recoverable
//     (Store.Restore) — but recovery is DB/store-level with no route, an undelete
//     restores the loser as a separate task rather than merging anything, and
//     Store.Delete (the hard purge, which is still what the last two bullets
//     describe) remains available. None of that turns "absorb + delete" into a
//     bookkeeping-safe operation; the first bullet still forbids it.
//
// Supersede therefore keeps BOTH rows: the winner unions in the loser's tags and
// records what it absorbed; the loser is marked complete and records what
// superseded it. It needs no migration and no new Store method — SetStatus,
// AddTags and AddComment already exist — and every step is individually
// reversible by hand.

// MergeTaskTags computes the winner's post-merge tag set for a supersede merge:
// the set UNION of both tasks' tags, validated as a whole.
//
// 🔴 The union has to be validated, which is the entire reason this function
// exists rather than a bare MergeTags call at the call site. MergeTags normalizes
// and de-duplicates but does NOT validate, so two individually-legal tag sets can
// union into an ILLEGAL one in two distinct ways, both silent:
//
//   - PROJECT CONFLICT. `project:` is at-most-one-per-task (ValidateTags), but
//     merging project:a with project:b yields BOTH. Nothing downstream would
//     error: ProjectName resolves via TagValue, which returns whichever project
//     sorts first — so the merged task would silently land in the alphabetically
//     smaller project. This function refuses instead, NAMING both projects,
//     because picking one for the user is exactly the silent data decision the
//     tags feature was built to avoid. The user resolves it by moving one task
//     into the other's project first (an ordinary edit).
//   - TAG CAP. MaxTags is 20 per task, so two 15-tag tasks union to 24 and every
//     write path would reject the result. Refusing here turns that into a clear,
//     actionable message naming the count and the cap, instead of a merge that
//     half-applies and reports a generic failure.
//
// Each condition returns its OWN distinctly-worded error so a caller (and a
// mutation test) can tell which guard fired. ValidateTags runs last as a
// backstop, so a future grammar rule is enforced on the merged set for free
// rather than being silently exempt from merges. All three failures are
// client-fixable → the caller maps them to 400.
func MergeTaskTags(winnerTags, loserTags []string) ([]string, error) {
	// The project check reads the INPUTS, not the union: once unioned, "which task
	// contributed which project" is unrecoverable and the error could not name the
	// conflict usefully.
	wp, wok := ProjectName(winnerTags)
	lp, lok := ProjectName(loserTags)
	if wok && lok && wp != lp {
		return nil, fmt.Errorf(
			"cannot merge: the two tasks are in different projects (%q and %q) — move one into the other's project first",
			truncTag(wp), truncTag(lp))
	}
	merged := MergeTags(winnerTags, loserTags)
	if len(merged) > MaxTags {
		return nil, fmt.Errorf(
			"cannot merge: the merged task would carry %d tags (max %d) — remove some tags first",
			len(merged), MaxTags)
	}
	if err := ValidateTags(merged); err != nil {
		return nil, fmt.Errorf("cannot merge: %w", err)
	}
	return merged, nil
}
