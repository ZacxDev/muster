package taskstatus

// LANES: the three buckets the Tasks board's status filter offers.
//
// WHY THEY LIVE HERE. The board used to render three SECTIONS whose membership
// rule was open-coded in internal/ui (groupTasks). The board is now one flat
// list ordered by activity, and the same three buckets survive only as a FILTER
// — which means the rule now has to be applied in SQL (a `status = ANY(...)`
// predicate) as well as in the renderer's chip row. Two call sites for one rule
// is exactly how a predicate gets fixed in one place and stays wrong in the
// other, so the mapping is defined once, in the package that already owns the
// status vocabulary and imports nothing.
//
// 🔴 THE MAPPING IS ONE TABLE, AND EVERYTHING ELSE IS DERIVED FROM IT. LaneOf
// walks laneMembers rather than switching on the constants, for the same reason
// Valid walks `all`: a switch is a second enumeration, and a fifth status added
// to the vocabulary but not to the switch would silently belong to NO lane —
// i.e. it would be invisible under every filter chip including the ones that
// claim to cover the whole board. TestEveryStatusBelongsToExactlyOneLane pins
// that: adding a status without giving it a lane reds the suite.
//
// WHAT CHANGED FROM groupTasks, stated plainly rather than buried: the old
// grouping PROMOTED an `open` task with a provisioning/running agent into the
// "In progress" section. A lane is a property of the TASK's own status and
// nothing else, because the filter is a SQL predicate over `notes.status` and a
// join-dependent lane would make "which chip is this card under" unanswerable
// from the row. The agent's state is still on the card (agentStatusChip), and
// agent activity still FLOATS the task to the top of the list — see the
// activity ordering in internal/notes. Only the bucket rule narrowed.
const (
	LaneOpen       = "open"
	LaneInProgress = "in_progress"
	LaneDone       = "done"
)

// laneOrder is the order the chip row renders lanes in (lifecycle order), kept
// separate from laneMembers because Go map iteration is randomised and a filter
// row whose chips move between renders is a usability bug, not a nit.
var laneOrder = [...]string{LaneOpen, LaneInProgress, LaneDone}

// laneMembers is the SINGLE definition of which statuses each lane covers.
//
// ready_for_review sits in "In progress" because it is work that is not
// finished — that is where the old In-progress section put it too, so a user's
// muscle memory for where a review-ready task appears is unchanged.
var laneMembers = map[string][]string{
	LaneOpen:       {Open},
	LaneInProgress: {InProgress, ReadyForReview},
	LaneDone:       {Complete},
}

// laneLabels is the human copy for each lane. Kept beside the mapping so a lane
// can never be added without one, and read by the renderer so the chip label and
// the filter value cannot drift.
var laneLabels = map[string]string{
	LaneOpen:       "Open",
	LaneInProgress: "In progress",
	LaneDone:       "Done",
}

// Lanes returns the lane keys in lifecycle order. It returns a COPY, for the
// same reason All does: a caller that sorts or truncates the result must not be
// able to reorder the vocabulary for every other caller in the process.
func Lanes() []string {
	return append([]string(nil), laneOrder[:]...)
}

// LaneStatuses returns the statuses a lane covers, or nil for an unknown lane.
//
// 🔴 nil is the "no status predicate" answer, which is what makes an unknown or
// absent lane degrade to "all tasks" rather than to "no tasks". A filter that
// silently matches NOTHING on a typo is the trap tasksFilteredEmpty exists to
// avoid; answering "no predicate" keeps a bad lane value honest — the board
// shows everything and the chip row shows nothing selected.
//
// It returns a COPY: the slice goes straight into a pgx query argument, and a
// caller appending to it would mutate the table.
func LaneStatuses(lane string) []string {
	m, ok := laneMembers[lane]
	if !ok {
		return nil
	}
	return append([]string(nil), m...)
}

// LaneOf returns the lane a status belongs to, or "" for an unknown status.
func LaneOf(status string) string {
	for _, lane := range laneOrder {
		for _, s := range laneMembers[lane] {
			if s == status {
				return lane
			}
		}
	}
	return ""
}

// LaneLabel returns a lane's human label, or "" for an unknown lane.
func LaneLabel(lane string) string { return laneLabels[lane] }

// ValidLane reports whether lane names one of the three buckets.
func ValidLane(lane string) bool {
	_, ok := laneMembers[lane]
	return ok
}
