package api

import (
	"net/url"
	"testing"

	"github.com/ZacxDev/muster/internal/taskstatus"
)

// TestAnUnknownStatusFilterShowsEverything pins the one property that did NOT
// survive the deletion of internal/taskstatus's "lane" layer on its own.
//
// 🔴 WHY IT HAD TO MOVE RATHER THAN JUST GO. The rule — an absent or bogus
// ?status= yields NO predicate, i.e. the whole board — used to be pinned by
// TestLaneStatusesForAnUnknownLaneIsNilNotEmpty against taskstatus.LaneStatuses.
// That function was an identity wrapper and was deleted with its layer, but the
// rule is not an identity: `Statuses` becomes `status = ANY($n)`, so nil and
// `[]string{}` are one character apart and mean opposite things. An empty
// non-nil slice matches NO rows — a typo'd filter value would silently empty the
// board while the chip row showed nothing selected, which is a filter hiding
// work. Deleting the layer without re-pinning this here would have dropped the
// guard on a live safety property.
//
// It asserts the pair the handler actually composes — queryStatus then
// statusPredicate — because that composition IS the rule; either half alone is
// satisfiable while the board still empties.
func TestAnUnknownStatusFilterShowsEverything(t *testing.T) {
	cases := []struct {
		name, raw string
		wantNil   bool
	}{
		{"absent", "", true},
		// `done` is here deliberately: it was the OLD chip value for Complete, so
		// a stale bookmark or a localStorage entry written before the collapse
		// still sends it. It must land on the whole board, not on an empty one.
		{"the retired `done` spelling", "done", true},
		{"a typo", "in-progress", true},
		{"wrong case", "Open", true},
		{"trailing space is trimmed, so this IS valid", "open ", false},
		{"a real status", taskstatus.ReadyForReview, false},
	}
	for _, tc := range cases {
		q := url.Values{}
		if tc.raw != "" {
			q.Set("status", tc.raw)
		}
		got := statusPredicate(queryStatus(q))
		if tc.wantNil {
			if got != nil {
				t.Errorf("%s (?status=%q): predicate = %#v, want nil. A non-nil slice builds "+
					"`status = ANY(...)`; an empty one matches NOTHING, so a bogus filter value "+
					"would empty the board while the chip row showed nothing selected.",
					tc.name, tc.raw, got)
			}
			continue
		}
		if len(got) != 1 {
			t.Errorf("%s (?status=%q): predicate = %#v, want exactly one status", tc.name, tc.raw, got)
			continue
		}
		if !taskstatus.Valid(got[0]) {
			t.Errorf("%s (?status=%q): predicate names %q, which is not a status — that query can "+
				"only ever return an empty board", tc.name, tc.raw, got[0])
		}
	}
}

// TestEveryStatusIsReachableThroughTheQueryFilter is the positive control for the
// test above: a guard that only ever checks that bad input yields nil is
// satisfied by a parser that rejects EVERYTHING, which would make the status
// filter permanently inert while every assertion above stayed green.
func TestEveryStatusIsReachableThroughTheQueryFilter(t *testing.T) {
	for _, s := range taskstatus.All() {
		q := url.Values{}
		q.Set("status", s)
		got := statusPredicate(queryStatus(q))
		if len(got) != 1 || got[0] != s {
			t.Errorf("?status=%s produced %#v, want [%q] — this status cannot be filtered for at "+
				"all, which is the defect the chip row was fixed for, one layer down", s, got, s)
		}
	}
}
