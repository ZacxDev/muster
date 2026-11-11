package notes

import (
	"context"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// CLAIMS (migration 0111; docs/tasks.md#claims): who is working a task.
//
// 🔴 A CLAIM IS EXPLICIT, AND BOTH SYSTEMS COUNT. A task is claimed when
//
//   - a SESSION claimed it through muster's task tools: its task_sessions row
//     carries claimed_at (a status write to in_progress that carried the
//     session's id; internal/api/task_claims.go is the writer), or
//   - its plugin task REFERENCE's upstream NAMES a claimant: the snapshot's
//     claimants carry the claiming agent and/or its session (agent,
//     agent_session). A status alone — in_progress, paused, whoever set it —
//     is NO claim: it names nobody, and an owner's own in_progress (or a
//     plugin from before claimants) must not read as someone working it.
//
// Nothing else makes a claim: not a focus title, not transcript text, not a
// session that merely read or commented on the task (role `read`/`worked`
// without claimed_at). The upstream half is READ from the stored snapshot,
// never copied: a later snapshot without it is the unclaim, and a status
// write that the upstream could not apply (queued, then failed) neither makes
// nor loses one.
//
// Claims is THE predicate. The task triage, the chief's task_route_attention
// and the board all read it, so "claimed" has one spelling.
// ---------------------------------------------------------------------------

// Claim sources.
const (
	// ClaimSession: a session's own claim (task_sessions.claimed_at).
	ClaimSession = "session"
	// ClaimUpstream: the reference's snapshot says the record is claimed.
	ClaimUpstream = "upstream"
)

// MaxClaimedByRunes bounds the name a claim is shown under (0111's CHECK).
const MaxClaimedByRunes = 200

// Claim is one claimant of a task.
type Claim struct {
	Source string `json:"source"`
	// Who is the name the board shows.
	Who string `json:"who"`
	// SessionID is the claiming session (ClaimSession), or the upstream
	// claimant's agent_session ("" when it has none).
	SessionID string `json:"sessionId,omitempty"`
	// Agent is an upstream claimant's agent id ("" when it has none).
	Agent string `json:"agent,omitempty"`
}

// Claims is every claim on n, sessions first (in thread order), then the
// upstream's. Empty means unclaimed. n must carry its thread (Get, List and
// ListPage embed it) for the session half.
func Claims(n Note) []Claim {
	var out []Claim
	for _, l := range n.Sessions {
		if l.ClaimedAt == nil {
			continue
		}
		who := l.ClaimedBy
		if who == "" {
			who = SessionClaimName(l.SessionID)
		}
		out = append(out, Claim{Source: ClaimSession, Who: who, SessionID: l.SessionID})
	}
	if n.Ref == nil {
		return out
	}
	for _, c := range n.Ref.Snapshot.Claimants {
		if who := c.who(); who != "" {
			out = append(out, Claim{Source: ClaimUpstream, Who: who, SessionID: c.AgentSession, Agent: c.Agent})
		}
	}
	return out
}

// who is the name an upstream claimant is shown under: its display name, else
// its agent, else its session's prefix; "" for a claimant that names nobody
// (no claim).
func (c RefClaimant) who() string {
	switch {
	case c.Name != "":
		return c.Name
	case c.Agent != "":
		return c.Agent
	case c.AgentSession != "":
		return SessionClaimName(c.AgentSession)
	}
	return ""
}

// SessionClaimName is the name a session's claim is shown under when it came
// with none: "session " and the id's first 8 characters.
func SessionClaimName(sessionID string) string {
	if utf8.RuneCountInString(sessionID) > 8 {
		sessionID = string([]rune(sessionID)[:8])
	}
	return "session " + sessionID
}

// ClaimByLink records that session l.SessionID CLAIMED task noteID, under the
// name by (cut at MaxClaimedByRunes): the link is upserted as `worked`
// (LinkSession's rules: a live task only, the role only moves up) and then
// carries claimed_at — kept from an earlier claim, so a repeat claim does not
// move it — and claimed_by. changed is false for a repeat claim under the same
// name. A dismissed or unknown task is pgx.ErrNoRows.
func (s *PGStore) ClaimByLink(ctx context.Context, noteID int64, l SessionLink, by string, detailSeen bool) (bool, error) {
	l.Role = RoleWorked
	if _, _, err := s.LinkSession(ctx, noteID, l, detailSeen); err != nil {
		return false, err
	}
	if utf8.RuneCountInString(by) > MaxClaimedByRunes {
		by = string([]rune(by)[:MaxClaimedByRunes])
	}
	tag, err := s.pool.Exec(ctx, `UPDATE task_sessions SET claimed_at = coalesce(claimed_at, now()), claimed_by = $3
		WHERE note_id = $1 AND session_id = $2 AND (claimed_at IS NULL OR claimed_by <> $3)`, noteID, l.SessionID, by)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ReleaseClaims clears every session claim on task noteID and reports how
// many it cleared. The links themselves stay (they record work).
func (s *PGStore) ReleaseClaims(ctx context.Context, noteID int64) (int64, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE task_sessions SET claimed_at = NULL, claimed_by = ''
		WHERE note_id = $1 AND claimed_at IS NOT NULL`, noteID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
