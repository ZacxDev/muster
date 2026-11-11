package notes

import (
	"context"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
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
	mark := UpstreamReleaseMark(n.Ref.Snapshot.Rev)
	for _, c := range n.Ref.Snapshot.Claimants {
		who := c.who()
		if who == "" || upstreamReleased(n.Sessions, c.AgentSession, mark) {
			continue
		}
		out = append(out, Claim{Source: ClaimUpstream, Who: who, SessionID: c.AgentSession, Agent: c.Agent})
	}
	return out
}

// UpstreamReleaseMark is the mark a release of an upstream claim records for
// a snapshot at rev (task_sessions.upstream_released_rev): never "", so an
// unreleased link can never read as released.
func UpstreamReleaseMark(rev string) string { return "rev:" + rev }

// upstreamReleased reports whether session sid's upstream claim was released
// at the snapshot revision mark (its session ended there): the claimant is
// then no claim until the snapshot moves to another revision.
func upstreamReleased(links []SessionLink, sid, mark string) bool {
	for _, l := range links {
		if l.SessionID == sid {
			return l.UpstreamReleasedRev == mark
		}
	}
	return false
}

// who is the name an upstream claimant is shown under: its display name, else
// its agent, else its session's prefix — "" for a claimant WITHOUT a session,
// which is no claim (round 1: a claim is a session's pickup; an agent alone
// names no session that is working the task).
func (c RefClaimant) who() string {
	switch {
	case c.AgentSession == "":
		return ""
	case c.Name != "":
		return c.Name
	case c.Agent != "":
		return c.Agent
	}
	return SessionClaimName(c.AgentSession)
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
// name by (cut at MaxClaimedByRunes), on l.ClaimHost (the host the mirror
// listed it on, "" for none): the link is upserted as `worked` (LinkSession's
// rules: a live task only, the role only moves up) and then carries
// claimed_at — kept from an earlier claim, so a repeat claim does not move it
// — claimed_by and claim_host. changed is false for a repeat claim under the
// same name and host. A dismissed or unknown task is pgx.ErrNoRows.
func (s *PGStore) ClaimByLink(ctx context.Context, noteID int64, l SessionLink, by string, detailSeen bool) (bool, error) {
	l.Role = RoleWorked
	if _, _, err := s.LinkSession(ctx, noteID, l, detailSeen); err != nil {
		return false, err
	}
	if utf8.RuneCountInString(by) > MaxClaimedByRunes {
		by = string([]rune(by)[:MaxClaimedByRunes])
	}
	tag, err := s.pool.Exec(ctx, `UPDATE task_sessions SET claimed_at = coalesce(claimed_at, now()), claimed_by = $3, claim_host = $4
		WHERE note_id = $1 AND session_id = $2 AND (claimed_at IS NULL OR claimed_by <> $3 OR claim_host <> $4)`,
		noteID, l.SessionID, by, l.ClaimHost)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ClaimedSession is one claim the ended-session sweep can judge: whose, on
// which task, which session, and the host muster saw it on ("" when none).
// Upstream is "" for a session's own claim (claimed_at), and for an upstream
// claimant's linked session the release mark of the snapshot's current
// revision (UpstreamReleaseMark).
type ClaimedSession struct {
	OwnerID, NoteID           int64
	SessionID, Host, Upstream string
}

// ClaimedSessions is the claims ctx may see on live tasks, at most limit:
// every session claim (claimed_at set), and every upstream claimant whose
// session is linked to its task with a host and is not released at the
// snapshot's current revision. The claim sweep passes the system scope (its
// caller is on internal/authz's ledger), then judges and releases each as its
// owner.
func (s *PGStore) ClaimedSessions(ctx context.Context, limit int) ([]ClaimedSession, error) {
	rows, err := s.pool.Query(ctx, `SELECT * FROM (
		SELECT ts.owner_id, ts.note_id, ts.session_id, ts.claim_host, '' AS upstream
		  FROM task_sessions ts JOIN notes n ON n.id = ts.note_id
		 WHERE ts.claimed_at IS NOT NULL AND n.`+liveOnly+`
		UNION ALL
		SELECT ts.owner_id, ts.note_id, ts.session_id, ts.claim_host, 'rev:' || coalesce(n.ext_snapshot->>'rev', '')
		  FROM task_sessions ts JOIN notes n ON n.id = ts.note_id
		 WHERE n.ext_key IS NOT NULL AND n.`+liveOnly+` AND ts.claim_host <> ''
		   AND EXISTS (SELECT 1 FROM jsonb_array_elements(coalesce(n.ext_snapshot->'claimants', '[]'::jsonb)) c
		               WHERE c->>'agent_session' = ts.session_id)
		   AND ts.upstream_released_rev <> 'rev:' || coalesce(n.ext_snapshot->>'rev', '')
		) claims ORDER BY owner_id, note_id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (ClaimedSession, error) {
		var c ClaimedSession
		err := row.Scan(&c.OwnerID, &c.NoteID, &c.SessionID, &c.Host, &c.Upstream)
		return c, err
	})
}

// SetClaimHost records host as the host the mirror listed session sessionID
// on, for task noteID (an upstream claimant's linked session).
func (s *PGStore) SetClaimHost(ctx context.Context, noteID int64, sessionID, host string) error {
	_, err := s.pool.Exec(ctx, `UPDATE task_sessions SET claim_host = $3
		WHERE note_id = $1 AND session_id = $2 AND claim_host <> $3`, noteID, sessionID, host)
	return err
}

// ReleaseUpstreamClaim records that session sessionID's upstream claim on task
// noteID is released at the snapshot revision mark (UpstreamReleaseMark): the
// session ended. It reports whether the mark changed.
func (s *PGStore) ReleaseUpstreamClaim(ctx context.Context, noteID int64, sessionID, mark string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE task_sessions SET upstream_released_rev = $3
		WHERE note_id = $1 AND session_id = $2 AND upstream_released_rev <> $3`, noteID, sessionID, mark)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ReleaseSessionClaim clears session sessionID's claim on task noteID (the
// session ended), reporting whether there was one.
func (s *PGStore) ReleaseSessionClaim(ctx context.Context, noteID int64, sessionID string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE task_sessions SET claimed_at = NULL, claimed_by = ''
		WHERE note_id = $1 AND session_id = $2 AND claimed_at IS NOT NULL`, noteID, sessionID)
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
