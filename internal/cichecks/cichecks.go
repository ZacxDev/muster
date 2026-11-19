// Package cichecks is THE ONE reading of a commit's CI: its commit statuses
// and its check runs, judged by name. Two callers share it — the shipping
// officer's merge gate (internal/officership gates.go checksGreen, which asks
// "is every REQUIRED check green?") and the fleet's CI read (internal/api
// fleet_ci.go, which asks "has every check finished, and how did each end?").
// One implementation, so the two never disagree about what green means.
//
// The rule, per check name:
//
//   - a commit status of that name (its LATEST state, as GitHub's combined
//     status gives it) decides: success is green; pending is not finished;
//     failure and error are finished and failed;
//   - with no status of that name, the NEWEST check run of that name (the
//     highest id: a re-run is a new run, so a newer attempt supersedes an older
//     one, whichever way each ended) decides: queued or in_progress is not
//     finished; completed/success is green; completed/neutral or skipped is
//     finished and neither green nor failed; any other conclusion is failed;
//   - neither: missing.
package cichecks

import (
	"sort"
	"time"
)

// Run is one check run (GitHub's check-runs API).
type Run struct {
	ID          int64
	Name        string
	Status      string // queued, in_progress, completed
	Conclusion  string // success, failure, neutral, skipped, cancelled, timed_out, action_required, stale
	CompletedAt time.Time
}

// Status is one commit status context's latest state (GitHub's combined
// status), and when it was last set.
type Status struct {
	State     string // success, pending, failure, error
	UpdatedAt time.Time
}

// Checks is everything muster read of one commit's CI.
type Checks struct {
	Statuses map[string]Status
	Runs     []Run
	// Truncated: the read stopped at its cap with more left unread, so a
	// check not read may still be running — the CI is never done (or green).
	Truncated bool
}

// The verdicts of one check (Verdict.State).
const (
	Green   = "green"
	Pending = "pending"
	Failed  = "failed"
	Neutral = "neutral" // finished, neither green nor failed (neutral, skipped)
	Missing = "missing"
)

// Verdict is one check name's reading.
type Verdict struct {
	Name  string
	State string
	// Detail is the raw state: a status's ("failure") or a run's
	// status/conclusion ("completed/failure").
	Detail string
	// FinishedAt is when it finished (zero while pending or unknown).
	FinishedAt time.Time
}

// Of is the verdict for check name in c.
func Of(c Checks, name string) Verdict {
	if st, ok := c.Statuses[name]; ok {
		v := Verdict{Name: name, Detail: st.State}
		switch st.State {
		case "success":
			v.State, v.FinishedAt = Green, st.UpdatedAt
		case "pending":
			v.State = Pending
		default:
			v.State, v.FinishedAt = Failed, st.UpdatedAt
		}
		return v
	}
	r, ok := newest(c.Runs, name)
	if !ok {
		return Verdict{Name: name, State: Missing, Detail: "missing"}
	}
	v := Verdict{Name: name, Detail: r.Status + "/" + r.Conclusion}
	if r.Status != "completed" {
		v.State = Pending
		return v
	}
	v.FinishedAt = r.CompletedAt
	switch r.Conclusion {
	case "success":
		v.State = Green
	case "neutral", "skipped":
		v.State = Neutral
	default:
		v.State = Failed
	}
	return v
}

// newest is runs' newest run named name (the highest id).
func newest(runs []Run, name string) (Run, bool) {
	var best Run
	found := false
	for _, r := range runs {
		if r.Name == name && (!found || r.ID > best.ID) {
			best, found = r, true
		}
	}
	return best, found
}

// Names is every check name c holds (statuses and runs), sorted.
func Names(c Checks) []string {
	seen := map[string]bool{}
	var out []string
	for n := range c.Statuses {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for _, r := range c.Runs {
		if !seen[r.Name] {
			seen[r.Name] = true
			out = append(out, r.Name)
		}
	}
	sort.Strings(out)
	return out
}

// Summary is every check of a commit, judged.
type Summary struct {
	Verdicts []Verdict
	// Counts per state.
	Green, Pending, Failed, Neutral int
	// LastFinished is when the last check to finish finished.
	LastFinished time.Time
	// Truncated: Checks.Truncated (not every check was read).
	Truncated bool
}

// Summarize judges every check c holds.
func Summarize(c Checks) Summary {
	s := Summary{Truncated: c.Truncated}
	for _, n := range Names(c) {
		v := Of(c, n)
		s.Verdicts = append(s.Verdicts, v)
		switch v.State {
		case Green:
			s.Green++
		case Pending:
			s.Pending++
		case Failed:
			s.Failed++
		case Neutral:
			s.Neutral++
		}
		if v.FinishedAt.After(s.LastFinished) {
			s.LastFinished = v.FinishedAt
		}
	}
	return s
}

// Done reports whether every check finished: at least one check, none pending,
// and every check read (a truncated read is pending, never done). No checks is
// not done (there is nothing to have finished).
func (s Summary) Done() bool { return len(s.Verdicts) > 0 && s.Pending == 0 && !s.Truncated }

// AllGreen reports whether CI is green: done, nothing failed, at least one
// green (a commit whose every check was skipped is not green).
func (s Summary) AllGreen() bool { return s.Done() && s.Failed == 0 && s.Green > 0 }

// FailedNames is the failed checks' names, sorted.
func (s Summary) FailedNames() []string {
	var out []string
	for _, v := range s.Verdicts {
		if v.State == Failed {
			out = append(out, v.Name)
		}
	}
	return out
}
