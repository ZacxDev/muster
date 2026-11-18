package askattempt

import (
	"sort"
	"strconv"
	"time"
)

// Rate is a success rate: of Total counted attempts, OK worked. A no-op
// (OutcomeNoop) is not counted, and neither is a Dismiss: the rate is of
// answers.
type Rate struct {
	OK, Total int
}

// Percent is the rate in whole percent, rounded down; 0 for none.
func (r Rate) Percent() int {
	if r.Total == 0 {
		return 0
	}
	return r.OK * 100 / r.Total
}

func (r *Rate) add(a Attempt) {
	if a.Action != ActionSubmit || a.Outcome == OutcomeNoop {
		return
	}
	r.Total++
	if a.Outcome == OutcomeOK {
		r.OK++
	}
}

// ShapeKey names a shape for grouping: its question count, whether its
// questions are single- or multi-select (or mixed), and whether "Other" was
// used. Never its text.
func ShapeKey(s Shape) string {
	n := s.Questions()
	k := strconv.Itoa(n) + " question"
	if n != 1 {
		k += "s"
	}
	multi, single := false, false
	for _, m := range s.Multi {
		if m {
			multi = true
		} else {
			single = true
		}
	}
	switch {
	case multi && single:
		k += ", mixed"
	case multi:
		k += ", multi-select"
	case single:
		k += ", single-select"
	}
	if s.Other {
		k += ", Other"
	}
	return k
}

// Keyed is a rate under a key.
type Keyed struct {
	Key  string
	Rate Rate
}

// ReasonCount is how many attempts ended with Reason.
type ReasonCount struct {
	Reason string
	Count  int
}

// Summary is one window's numbers.
type Summary struct {
	Overall   Rate
	ByRuntime []Keyed
	ByShape   []Keyed
	// Failures is every reason that was not OK (refused, failed), with its
	// count, most first.
	Failures []ReasonCount
}

// Summarise is the summary of the attempts in as at or after since.
func Summarise(as []Attempt, since time.Time) Summary {
	var s Summary
	rt, sh := map[string]*Rate{}, map[string]*Rate{}
	fails := map[string]int{}
	for _, a := range as {
		if a.At.Before(since) {
			continue
		}
		s.Overall.add(a)
		if a.Action == ActionSubmit {
			for _, g := range []struct {
				m map[string]*Rate
				k string
			}{{rt, a.Runtime}, {sh, ShapeKey(a.Shape)}} {
				if g.m[g.k] == nil {
					g.m[g.k] = &Rate{}
				}
				g.m[g.k].add(a)
			}
		}
		if a.Outcome == OutcomeRefused || a.Outcome == OutcomeFailed {
			fails[a.Reason]++
		}
	}
	keyed := func(m map[string]*Rate) []Keyed {
		out := make([]Keyed, 0, len(m))
		for k, r := range m {
			out = append(out, Keyed{k, *r})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
		return out
	}
	s.ByRuntime, s.ByShape = keyed(rt), keyed(sh)
	for r, n := range fails {
		s.Failures = append(s.Failures, ReasonCount{r, n})
	}
	sort.Slice(s.Failures, func(i, j int) bool {
		if s.Failures[i].Count != s.Failures[j].Count {
			return s.Failures[i].Count > s.Failures[j].Count
		}
		return s.Failures[i].Reason < s.Failures[j].Reason
	})
	return s
}
