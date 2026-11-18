package agents

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// OFFICER CHAT LANES (migration 0149; internal/officers chat.go). A chat a
// team member holds with an officer runs in a Claude Code session of its own
// on one of the officer's instances: a chat_sessions row on that instance's
// agent, on the claude-code runtime, keyed by the chat (officers.
// ChatSessionKey). The row is how the chat finds its instance again (the
// lane's conversation is on that instance's volume), and it carries the
// chat's marks: the highest chat seq the lane has been given (seen) and the
// last one HQ took a reply for (answered).

// OfficerChatStore is the store's 0149 half, when it has one.
type OfficerChatStore interface {
	// SessionByKeyAnyAgent is the session keyed sessionKey, whichever agent
	// (the caller's row scope permitting) it is on. pgx.ErrNoRows when none.
	SessionByKeyAnyAgent(ctx context.Context, sessionKey string) (ChatSession, error)
	// EnsureClaudeCodeSession is agent agentID's session keyed sessionKey on
	// the claude-code runtime (no model), created with title when it does not
	// exist. ErrSessionKeyElsewhere when the key is another agent's.
	EnsureClaudeCodeSession(ctx context.Context, agentID int64, sessionKey, title string) (ChatSession, error)
	// OfficerChatMarks is session sessionID's chat marks.
	OfficerChatMarks(ctx context.Context, sessionID int64) (ChatLaneMarks, error)
	// SetOfficerChatMarks writes session sessionID's chat marks.
	SetOfficerChatMarks(ctx context.Context, sessionID int64, m ChatLaneMarks) error
	// ChatLaneSecret is the secret of agent agentID's chat lane keyed
	// sessionKey, "" when it has none (or no such session).
	ChatLaneSecret(ctx context.Context, agentID int64, sessionKey string) (string, error)
	// EnsureChatLaneSecret is session sessionID's lane secret, minted (32
	// random bytes, hex) and kept the first time it is asked for, and the
	// same one every time after.
	EnsureChatLaneSecret(ctx context.Context, sessionID int64) (string, error)
	// SetAsideOfficerChat sets agent agentID's session keyed sessionKey aside
	// when its chat moves to another instance: its key becomes
	// OfficerChatMovedPrefix + "<session id>:" + sessionKey (so the chat's
	// key is free for its new session) and its lane secret is cleared (no
	// call can name it as a lane again); its marks and history stay on the
	// row. moved is false when the agent has no such session.
	SetAsideOfficerChat(ctx context.Context, agentID int64, sessionKey string) (moved bool, err error)
}

// OfficerChatMovedPrefix prefixes a set-aside officer chat session's key
// (SetAsideOfficerChat). It is not the chat key prefix
// (officers.ChatSessionKeyPrefix, "officer-chat:"), so no lookup of a chat's
// session finds it and no MCP call naming it is a chat lane's.
const OfficerChatMovedPrefix = "officer-chat-moved:"

// ChatLaneMarks is a chat lane's marks (internal/officers ChatMarks): the
// highest chat seq it has seen, the last one HQ took a reply for, and the
// charter version it last loaded ("" for none).
type ChatLaneMarks struct {
	Seen, Answered int64
	Charter        string
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
func (s *PGStore) OfficerChatMarks(ctx context.Context, sessionID int64) (ChatLaneMarks, error) {
	var m ChatLaneMarks
	err := s.pool.QueryRow(ctx, `SELECT officer_chat_seen, officer_chat_answered, officer_chat_charter FROM chat_sessions WHERE id = $1`, sessionID).
		Scan(&m.Seen, &m.Answered, &m.Charter)
	return m, err
}

// SetOfficerChatMarks implements OfficerChatStore.
func (s *PGStore) SetOfficerChatMarks(ctx context.Context, sessionID int64, m ChatLaneMarks) error {
	tag, err := s.pool.Exec(ctx, `UPDATE chat_sessions SET officer_chat_seen = $2, officer_chat_answered = $3, officer_chat_charter = $4 WHERE id = $1`,
		sessionID, m.Seen, m.Answered, m.Charter)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	return nil
}

// ChatLaneSecret implements OfficerChatStore. No such session is "", nil:
// the caller refuses an empty secret either way.
func (s *PGStore) ChatLaneSecret(ctx context.Context, agentID int64, sessionKey string) (string, error) {
	var secret string
	err := s.pool.QueryRow(ctx, `SELECT officer_chat_secret FROM chat_sessions WHERE agent_id = $1 AND session_key = $2`, agentID, sessionKey).Scan(&secret)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return secret, err
}

// EnsureChatLaneSecret implements OfficerChatStore: one UPDATE that writes
// the fresh secret only where none is kept, then the one kept — so two
// callers racing keep ONE secret.
func (s *PGStore) EnsureChatLaneSecret(ctx context.Context, sessionID int64) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	if _, err := s.pool.Exec(ctx, `UPDATE chat_sessions SET officer_chat_secret = $2 WHERE id = $1 AND officer_chat_secret = ''`, sessionID, hex.EncodeToString(b)); err != nil {
		return "", err
	}
	var secret string
	if err := s.pool.QueryRow(ctx, `SELECT officer_chat_secret FROM chat_sessions WHERE id = $1`, sessionID).Scan(&secret); err != nil {
		return "", err
	}
	if secret == "" {
		return "", fmt.Errorf("agents: session %d keeps no lane secret", sessionID)
	}
	return secret, nil
}

// SetAsideOfficerChat implements OfficerChatStore: one UPDATE, so a racing
// second call finds no row under the old key and moves nothing.
func (s *PGStore) SetAsideOfficerChat(ctx context.Context, agentID int64, sessionKey string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE chat_sessions SET session_key = $3::text || id::text || ':' || session_key, officer_chat_secret = ''
		WHERE agent_id = $1 AND session_key = $2`, agentID, sessionKey, OfficerChatMovedPrefix)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
