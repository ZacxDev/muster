package agents

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// WHICH CHAT THE OWNER WAS ON LAST (migration 0187, chat_sessions.viewed_at).
//
// The chief opens on the chat its owner opened last, on the chief page or in
// the chief panel (internal/api chiefOpenSession). Not on the chat touched
// last: updated_at moves with every message, and a delivery to the primary is
// a message, so "the latest" was the primary as soon as anything was
// delivered — the owner reopened the chief and was taken back to it from
// whichever side chat they had been reading.

// SessionViewStore is the store's 0187 half, when it has one (a test fake
// without it keeps the pre-0187 behaviour: MarkSessionRead, and the chief
// opening on its primary).
type SessionViewStore interface {
	// MarkSessionViewed stamps the session viewed (viewed_at) AND read
	// (read_at, as MarkSessionRead does), in one statement: the session was
	// drawn for its owner.
	MarkSessionViewed(ctx context.Context, sessionID int64) error
	// LastViewedSession is the agent's session with the latest viewed_at,
	// archived or not. ok is false when none of its sessions has been viewed.
	LastViewedSession(ctx context.Context, agentID int64) (sess ChatSession, ok bool, err error)
}

// MarkSessionViewed implements SessionViewStore.
func (s *PGStore) MarkSessionViewed(ctx context.Context, sessionID int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE chat_sessions SET read_at=now(), viewed_at=now() WHERE id=$1`, sessionID)
	return err
}

// LastViewedSession implements SessionViewStore. The id breaks a tie (two
// views in one clock tick): the newer session.
func (s *PGStore) LastViewedSession(ctx context.Context, agentID int64) (ChatSession, bool, error) {
	c, err := scanSession(s.pool.QueryRow(ctx,
		`SELECT `+sessionCols+` FROM chat_sessions WHERE agent_id=$1 AND viewed_at IS NOT NULL ORDER BY viewed_at DESC, id DESC LIMIT 1`, agentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ChatSession{}, false, nil
	}
	if err != nil {
		return ChatSession{}, false, err
	}
	return c, true, nil
}
