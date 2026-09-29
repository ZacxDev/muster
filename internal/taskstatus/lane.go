package taskstatus

// LANES: the buckets the Tasks board's status filter offers.
//
// WHY THEY LIVE HERE. The board used to render sections whose membership rule
// was open-coded in internal/ui (groupTasks). The board is now one flat list
// ordered by activity, and the buckets survive only as a FILTER — which means
// the rule now has to be applied in SQL (a `status = ANY(...)` predicate) as
// well as in the renderer's chip row. Two call sites for one rule is exactly how
// a predicate gets fixed in one place and stays wrong in the other, so the
// mapping is defined once, in the package that already owns the status
// vocabulary and imports nothing.
//
// 🔴 A LANE IS A STATUS. THAT IS THE WHOLE DEFINITION, AND IT IS A FIX.
//
// This file used to carry a hand-written THREE-lane table that folded
// ready_for_review into "In progress" and renamed complete to "done". Measured
// live on a 525-task board: `open 215 · complete 163 · ready_for_review 106 ·
// in_progress 41`, and clicking "In progress" loaded 30 ready_for_review cards
// beside 20 in_progress ones. So a card the per-card <select> labelled "Ready
// for review" matched NO chip of that name, could not be isolated by any chip,
// and 20% of the board was unreachable by the filter — while the chip row
// silently claimed to cover it. The lane vocabulary and the status vocabulary
// were two enumerations of one thing, and they had drifted.
//
// Deriving lanes from All() makes that unrepresentable rather than merely
// tested: a fifth status is a fifth chip the day it is added, LaneStatuses
// answers with it, and the only thing a new status still needs from this file is
// a LABEL — which laneLabels is exhaustively checked for by
// TestEveryLaneHasALabel, so forgetting one is a red suite rather than a
// blank chip.
//
// WHAT CHANGED FROM groupTasks, stated plainly rather than buried: the old
// grouping PROMOTED an `open` task with a provisioning/running agent into the
// "In progress" section. A lane is a property of the TASK's own status and
// nothing else, because the filter is a SQL predicate over `notes.status` and a
// join-dependent lane would make "which chip is this card under" unanswerable
// from the row. The agent's state is still on the card (agentStatusChip), and
// agent activity still FLOATS the task to the top of the list — see the
// activity ordering in internal/notes. Only the bucket rule narrowed.
//
// ⚠ THE `done` LANE VALUE IS GONE, and a URL is the thing that notices. The old
// chip wrote `?status=done`; the chip now writes `?status=complete`. An old
// bookmark or a stale localStorage value naming `done` is not a valid lane, and
// queryStatusLane already answers an unknown lane with "" — i.e. it degrades to
// the unfiltered board, which is the safe direction (see LaneStatuses).
const (
	LaneOpen           = Open
	LaneInProgress     = InProgress
	LaneReadyForReview = ReadyForReview
	LaneDone           = Complete
)

// laneLabels is the human copy for each lane. It is the ONLY per-lane data left:
// everything else is derived from the status vocabulary above.
//
// "Done" rather than "Complete" for the last one is deliberate — it is the word
// the board has always used on that chip, and the chip's VALUE (which is what
// travels in the URL and the SQL) is the status either way.
var laneLabels = map[string]string{
	Open:           "Open",
	InProgress:     "In progress",
	ReadyForReview: "Ready for review",
	Complete:       "Done",
}

// Lanes returns the lane keys in lifecycle order. It returns a COPY, for the
// same reason All does: a caller that sorts or truncates the result must not be
// able to reorder the vocabulary for every other caller in the process.
func Lanes() []string { return All() }

// LaneStatuses returns the statuses a lane covers, or nil for an unknown lane.
//
// 🔴 nil is the "no status predicate" answer, which is what makes an unknown or
// absent lane degrade to "all tasks" rather than to "no tasks". A filter that
// silently matches NOTHING on a typo is the trap tasksFilteredEmpty exists to
// avoid; answering "no predicate" keeps a bad lane value honest — the board
// shows everything and the chip row shows nothing selected.
//
// It returns a fresh slice: the result goes straight into a pgx query argument,
// and a caller appending to it must not be able to reach anything shared.
func LaneStatuses(lane string) []string {
	if !Valid(lane) {
		return nil
	}
	return []string{lane}
}

// LaneOf returns the lane a status belongs to, or "" for an unknown status.
func LaneOf(status string) string {
	if !Valid(status) {
		return ""
	}
	return status
}

// LaneLabel returns a lane's human label, or "" for an unknown lane.
func LaneLabel(lane string) string { return laneLabels[lane] }

// ValidLane reports whether lane names one of the buckets.
func ValidLane(lane string) bool { return Valid(lane) }
