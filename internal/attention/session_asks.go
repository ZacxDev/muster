package attention

import (
	"context"
)

// A SESSION'S ASKS (the session view's Asks tab: internal/api session_asks.go,
// docs/attention.md "A session's asks"): every question one session raised
// that is open or was answered, open ones first, newest first in each.
//
// 🔴 THE CALLER'S OWN ASKS, NAMED IN THE QUERY, as Search's (search.go):
// attention_asks is team-shared (0127), so row-level security lets a team's
// member read the team's asks too, and a team host's session can carry the
// very host name and session key a member's own does. SessionAsks names the
// owner itself (owner_id = muster.uid).
//
// Left out: a question a session put to the chief (ToChief: no page of the
// owner's draws it), a notice (it asks nothing), and an ask settled without
// an answer (dismissed or expired: there is nothing to read in it).

// SessionAsksQuery names ONE session whose asks are listed: an agent
// (AgentID), or a mirrored session on MirrorHost by any of the MCP caller keys
// its window runs (MirrorSessions, mirror.Window.MCPCallerKeys).
type SessionAsksQuery struct {
	AgentID        int64
	MirrorHost     string
	MirrorSessions []string
	// Limit is how many asks at most (≤ 0 or over it: MaxSessionAsks).
	Limit int
}

// Valid reports whether q names a session.
func (q SessionAsksQuery) Valid() bool {
	if q.AgentID > 0 {
		return q.MirrorHost == "" && len(q.MirrorSessions) == 0
	}
	return q.MirrorHost != "" && len(q.MirrorSessions) > 0
}

// MaxSessionAsks bounds one session's list.
const MaxSessionAsks = 50

// SessionAsks is the caller's own asks raised by the session q names: the
// open (unexpired) ones first, then the answered ones, newest first within
// each, at most q.Limit. None for a q that names no session.
func (s *PGStore) SessionAsks(ctx context.Context, q SessionAsksQuery) ([]Ask, error) {
	if !q.Valid() {
		return nil, nil
	}
	limit := q.Limit
	if limit <= 0 || limit > MaxSessionAsks {
		limit = MaxSessionAsks
	}
	sessions := q.MirrorSessions
	if sessions == nil {
		sessions = []string{}
	}
	return collect(s.pool.Query(ctx, `SELECT `+cols+` FROM attention_asks
		WHERE owner_id = nullif(current_setting('muster.uid', true), '')::bigint
		  AND NOT to_chief
		  AND kind = 'ask'
		  AND (($1::bigint > 0 AND agent_id = $1)
		    OR ($2::text <> '' AND mirror_host = $2 AND mirror_session = ANY($3::text[])))
		  AND ((status = 'open' AND expires_at > now()) OR status IN ('answered', 'answered_in_session'))
		ORDER BY (status = 'open') DESC, created_at DESC, id DESC
		LIMIT $4`, q.AgentID, q.MirrorHost, sessions, limit))
}
