package taskstatus

// Human copy for each status.
//
// 🔴 THIS FILE REPLACES A WHOLE "LANE" LAYER, AND THE DELETION IS THE POINT.
// There used to be lane.go: four Lane* constants and Lanes / LaneStatuses /
// LaneOf / ValidLane / LaneLabel, defining a SECOND vocabulary that the board's
// filter chips were built from. It existed because the chips once grouped
// statuses — `in_progress` and `ready_for_review` shared one bucket, and
// `complete` was spelled `done`.
//
// That grouping was the defect. Measured on a 525-task board: clicking "In
// progress" returned 30 `ready_for_review` cards beside 20 `in_progress` ones,
// and no chip selected either state on its own. Once a lane is required to be a
// status, every Lane* function collapses to an identity — LaneOf(s) == s,
// LaneStatuses(l) == []string{l}, ValidLane == Valid — and a layer of identities
// is not an abstraction, it is a second name for the same thing plus the
// opportunity for the two to disagree again.
//
// So the vocabulary is All() and the only per-status datum left is a label.
//
// ⚠ THE `done` LANE VALUE IS GONE, and a URL is the thing that notices. The old
// chip wrote `?status=done`; the chip now writes `?status=complete`. An old
// bookmark or a stale localStorage value naming `done` is not a valid status,
// and the query parser already answers an unknown value with "" — i.e. it
// degrades to the unfiltered board, which is the safe direction (a filter is a
// READ, so a bogus value must never be able to hide work).
var labels = map[string]string{
	Open:           "Open",
	InProgress:     "In progress",
	ReadyForReview: "Ready for review",
	// "Done" rather than "Complete" is deliberate — it is the word the board has
	// always used on that chip, and the chip's VALUE (which is what travels in
	// the URL and into the SQL) is the status either way.
	Complete: "Done",
}

// Label returns a status's human label, or "" for an unknown status.
//
// "" for unknown is load-bearing at its two call sites: the chip row renders one
// chip per All() entry, so a missing label there is a blank control
// (TestEveryStatusHasALabel refuses it), while the filtered-empty state uses ""
// to decide whether to NAME a status filter at all rather than printing an empty
// one.
func Label(status string) string { return labels[status] }
