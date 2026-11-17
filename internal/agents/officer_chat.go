package agents

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// OFFICER CHAT LANES (migration 0148; internal/officers chat.go). A chat a
// team member holds with an officer runs in a Claude Code session of its own
// on one of the officer's instances: a chat_sessions row on that instance's
// agent, on the claude-code runtime, keyed by the chat (officers.
// ChatSessionKey). The row is how the chat finds its instance again (the
// lane's conversation is on that instance's volume), and it carries the
// chat's marks: the highest chat seq the lane has been given (seen) and the
// last one HQ took a reply for (answered).

// OfficerChatStore is the store's 0148 half, when it has one.
type OfficerChatStore interface {
	// SessionByKeyAnyAgent is the session keyed sessionKey, whichever agent
	// (the caller's row scope permitting) it is on. pgx.ErrNoRows when none.
	SessionByKeyAnyAgent(ctx context.Context, sessionKey string) (ChatSession, error)
	// EnsureClaudeCodeSession is agent agentID's session keyed sessionKey on
	// the claude-code runtime (no model), created with title when it does not
	// exist. ErrSessionKeyElsewhere when the key is another agent's.
	EnsureClaudeCodeSession(ctx context.Context, agentID int64, sessionKey, title string) (ChatSession, error)
	// OfficerChatMarks is session sessionID's chat marks.
	OfficerChatMarks(ctx context.Context, sessionID int64) (seen, answered int64, err error)
	// SetOfficerChatMarks writes session sessionID's chat marks.
	SetOfficerChatMarks(ctx context.Context, sessionID, seen, answered int64) error
}

var _ OfficerChatStore = (*PGStore)(nil)

// ErrSessionKeyElsewhere is a session key held by another agent's session:
// session keys are globally unique (0001).
var ErrSessionKeyElsewhere = errors.New("agents: that session key belongs to another agent's session")

// SessionByKeyAnyAgent implements OfficerChatStore.
func (s *PGStore) SessionByKeyAnyAgent(ctx context.Context, sessionKey string) (ChatSession, error) {
	return scanSession(s.pool.QueryRow(ctx, `SELECT `+sessionCols+` FROM chat_sessions WHERE session_key = $1`, sessionKey))
}

// EnsureClaudeCodeSession implements OfficerChatStore. The INSERT does nothing
// on a key that exists, so two callers racing make one row; the row read back
// is checked to be agentID's.
func (s *PGStore) EnsureClaudeCodeSession(ctx context.Context, agentID int64, sessionKey, title string) (ChatSession, error) {
	c, err := scanSession(s.pool.QueryRow(ctx, `
		INSERT INTO chat_sessions (agent_id, owner_id, session_key, title, runtime, model) VALUES ($1, `+ownerOfAgent+`, $2, $3, $4, '')
		ON CONFLICT (session_key) DO NOTHING
		RETURNING `+sessionCols, agentID, sessionKey, title, SessionRuntimeClaudeCode))
	if errors.Is(err, pgx.ErrNoRows) {
		c, err = s.SessionByKeyAnyAgent(ctx, sessionKey)
	}
	if err != nil {
		return ChatSession{}, err
	}
	if c.AgentID != agentID {
		return ChatSession{}, fmt.Errorf("%w (session %d, agent %d)", ErrSessionKeyElsewhere, c.ID, c.AgentID)
	}
	if c.Runtime != SessionRuntimeClaudeCode {
		return ChatSession{}, fmt.Errorf("agents: session %d keyed %q is not on the %s runtime", c.ID, sessionKey, SessionRuntimeClaudeCode)
	}
	return c, nil
}

// OfficerChatMarks implements OfficerChatStore.
func (s *PGStore) OfficerChatMarks(ctx context.Context, sessionID int64) (seen, answered int64, err error) {
	err = s.pool.QueryRow(ctx, `SELECT officer_chat_seen, officer_chat_answered FROM chat_sessions WHERE id = $1`, sessionID).Scan(&seen, &answered)
	return seen, answered, err
}

// SetOfficerChatMarks implements OfficerChatStore.
func (s *PGStore) SetOfficerChatMarks(ctx context.Context, sessionID, seen, answered int64) error {
	tag, err := s.pool.Exec(ctx, `UPDATE chat_sessions SET officer_chat_seen = $2, officer_chat_answered = $3 WHERE id = $1`, sessionID, seen, answered)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	return nil
}
