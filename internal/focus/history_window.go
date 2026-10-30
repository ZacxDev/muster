package focus

import (
	"context"
	"sort"
	"time"
)

// A WINDOW REPORT OVER FOCUS HISTORY: every session of the caller's that had
// focus activity between two instants, LIVE OR NOT — the read behind the
// chief's fleet_report (internal/api/fleet_report.go), a standup's "what did
// I work on yesterday and today".
//
// 🔴 NO LIVENESS FILTER, AND THAT IS THE ONE DIFFERENCE FROM history_search.go.
// Those reads answer "which session can I hand this to NOW", so a dead
// session's rows must not be returned; this one answers "what was worked on",
// and a session that ended last night is exactly what a standup needs. What
// does NOT change: the read runs under the caller's principal, and both tables
// it reads (agent_focus_history and agent_focus) are under FORCE row-level
// security, so another owner's sessions are never returned — the owner is
// never a parameter.
//
// ACTIVITY is either of two rows inside [since, until]:
//   - a focus history row (a focus CHANGE, Store.Upsert), and
//   - the CURRENT focus row's updated_at: a refresh rewrote it, which happens
//     only when the session's transcript moved — so a session that worked all
//     day on one unchanged focus is not missed because its history row is
//     from the day before.

// The bounds of a window read.
const (
	// MaxWindowSpan bounds until - since: a standup's previous workday and
	// today, across a weekend, is at most four days.
	MaxWindowSpan = 4 * 24 * time.Hour
	// MaxWindowSubjects bounds how many sessions one read returns, most
	// recently active first; WindowReport.Omitted counts the rest.
	MaxWindowSubjects = 200
	// MaxWindowTitles bounds the distinct titles kept per session, the newest
	// ones; WindowSubject.TitlesOmitted counts the rest.
	MaxWindowTitles = 20
)

// WindowSubject is one session's focus activity inside a window: when it was
// first and last seen there (by its focus rows), and its distinct titles
// there, oldest first, each at the newest time it was written.
type WindowSubject struct {
	Subject       Subject
	FirstAt       time.Time
	LastAt        time.Time
	Titles        []TitleAt
	TitlesOmitted int
}

// WindowReport is a window read: the sessions, most recently active first,
// and how many more MaxWindowSubjects left out.
type WindowReport struct {
	Subjects []WindowSubject
	Omitted  int
}

// errBadWindow refuses a window that is empty, inverted or too long.
var errBadWindow = errf("a window needs since before until, at most %s apart", MaxWindowSpan)

// WindowHistory is every session of the caller's with focus activity at or
// after since and at or before until (see the file comment), most recently
// active first, at most MaxWindowSubjects.
func (s *PGStore) WindowHistory(ctx context.Context, since, until time.Time) (WindowReport, error) {
	if since.IsZero() || until.IsZero() || !since.Before(until) || until.Sub(since) > MaxWindowSpan {
		return WindowReport{}, errBadWindow
	}
	rows, err := s.pool.Query(ctx, `SELECT subject_kind, subject_key, title, max(at) AS at, min(at) AS first FROM (
			SELECT h.subject_kind, h.subject_key, h.title, h.created_at AS at FROM agent_focus_history h
				WHERE h.created_at >= $1 AND h.created_at <= $2
			UNION ALL
			SELECT f.subject_kind, f.subject_key, f.title, f.updated_at AS at FROM agent_focus f
				WHERE f.title <> '' AND f.updated_at >= $1 AND f.updated_at <= $2) w
		GROUP BY subject_kind, subject_key, title
		ORDER BY subject_kind, subject_key, at`, since, until)
	if err != nil {
		return WindowReport{}, errf("window history: %w", err)
	}
	defer rows.Close()
	by := map[Subject]*WindowSubject{}
	var order []*WindowSubject
	for rows.Next() {
		var kind, key string
		var t TitleAt
		var first time.Time
		if err := rows.Scan(&kind, &key, &t.Title, &t.At, &first); err != nil {
			return WindowReport{}, errf("window history: %w", err)
		}
		sub := Subject{Kind: Kind(kind), Key: key}
		w := by[sub]
		if w == nil {
			w = &WindowSubject{Subject: sub, FirstAt: first, LastAt: t.At}
			by[sub] = w
			order = append(order, w)
		}
		// A title is grouped at its NEWEST write, so the session's first
		// activity is the minimum of each title's OLDEST write, not of these.
		if first.Before(w.FirstAt) {
			w.FirstAt = first
		}
		if t.At.After(w.LastAt) {
			w.LastAt = t.At
		}
		w.Titles = append(w.Titles, t) // oldest first per subject
	}
	if err := rows.Err(); err != nil {
		return WindowReport{}, errf("window history: %w", err)
	}
	for _, w := range order {
		if n := len(w.Titles); n > MaxWindowTitles {
			w.TitlesOmitted = n - MaxWindowTitles
			w.Titles = w.Titles[n-MaxWindowTitles:]
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if !a.LastAt.Equal(b.LastAt) {
			return a.LastAt.After(b.LastAt)
		}
		return a.Subject.String() < b.Subject.String()
	})
	out := WindowReport{Subjects: []WindowSubject{}}
	for i, w := range order {
		if i >= MaxWindowSubjects {
			out.Omitted = len(order) - MaxWindowSubjects
			break
		}
		out.Subjects = append(out.Subjects, *w)
	}
	return out, nil
}
