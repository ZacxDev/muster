package focus

import (
	"context"
)

// THE COMMAND PALETTE'S PREVIOUS TITLES (migration 0022's rows): what each of
// the viewer's sessions was working on before its current focus, so the
// palette can find a session by a title it no longer has (operator ask: "find
// agents by their focus titles, either current or previous").
//
// 🔴 A ROW COUNT, NOT A TIME WINDOW — the opposite of history_search.go, and
// for a different job. Those reads MATCH TEXT against history, where a count
// would make a busy session's reach shorter than a quiet one's. This one feeds
// a page that is rendered on every navigation and re-fetched every minute by
// the sidebar, so what it must be is BOUNDED: each session's newest
// TitleHistoryRows rows, one index range scan each (idx_agent_focus_history_
// subject), whatever the session's churn or the retention. The palette lists
// "what it was working on lately", and the newest rows are that.
//
// Like every Store read but the sweeps it runs under the caller's principal:
// agent_focus_history is under FORCE row-level security, so another owner's
// session reads as no rows even when its id is named in live.
//
// 🔴 AND IT NAMES THE OWNER ITSELF (h.owner_id = the principal's muster.uid),
// for the plan as much as for the isolation. The policy's USING is
// "owner_id = uid OR scope = 'all'", and that OR keeps the planner off
// idx_agent_focus_history_subject, whose first column is owner_id: measured on
// 200k rows (200 sessions), the read without the predicate walked the
// created_at index and filtered 162k rows per session — 2.3 s; with it, one
// index range scan per session — 36 ms for all 200, under 1 ms for three
// sessions one of which holds 50k rows. It also means a caller under the
// system scope reads nothing here rather than every owner's rows.

const (
	// TitleHistoryRows bounds how many of a session's newest history rows
	// PreviousTitles reads: its cost per session.
	TitleHistoryRows = 64
	// MaxPreviousTitles bounds the distinct titles PreviousTitles returns per
	// session.
	MaxPreviousTitles = 10
)

// PreviousTitles is, for each live session, the distinct titles among its
// newest TitleHistoryRows history rows, newest first (a title written twice is
// listed once, at its newest), at most MaxPreviousTitles. It INCLUDES the
// current focus's title (the newest row is the current focus): the caller
// drops it, since only it knows which title is current on the page it draws.
// A session with no rows is absent from the map.
func (s *PGStore) PreviousTitles(ctx context.Context, live Live) (map[Subject][]TitleAt, error) {
	m, a := live.nonNil()
	out := map[Subject][]TitleAt{}
	if len(m) == 0 && len(a) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT s.kind, s.key, t.title, t.at
		FROM (SELECT 'mirror'::text AS kind, k AS key FROM unnest($1::text[]) k
			UNION SELECT 'agent', id::text FROM unnest($2::bigint[]) id) s
		CROSS JOIN LATERAL (
			SELECT r.title, max(r.created_at) AS at FROM (
				SELECT h.title, h.created_at FROM agent_focus_history h
				WHERE h.owner_id = nullif(current_setting('muster.uid', true), '')::bigint
					AND h.subject_kind = s.kind AND h.subject_key = s.key
				ORDER BY h.created_at DESC, h.id DESC LIMIT $3) r
			GROUP BY r.title) t
		ORDER BY s.kind, s.key, t.at DESC, t.title`, m, a, TitleHistoryRows)
	if err != nil {
		return nil, errf("previous titles: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind, key string
		var t TitleAt
		if err := rows.Scan(&kind, &key, &t.Title, &t.At); err != nil {
			return nil, errf("previous titles: %w", err)
		}
		sub := Subject{Kind: Kind(kind), Key: key}
		if len(out[sub]) < MaxPreviousTitles {
			out[sub] = append(out[sub], t)
		}
	}
	return out, rows.Err()
}
