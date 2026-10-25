package plugins

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// Threads a session started (plugin_threads, migration 0054):
// docs/plugins.md#a-reply-on-a-thread-a-session-started.

// AgentMailPlugin is the one plugin, by name, whose threads a session can be
// bound to: agent mail (operator decision), the same plugin the unrouted
// sweep judges (UnroutedAskPlugin).
const AgentMailPlugin = UnroutedAskPlugin

// RouteViaThread is RouteOrigin.Via for a route muster made because the
// message is a reply on a thread the target session started.
const RouteViaThread = "thread"

// BindThread binds thread to the mirrored Claude Code session claudeSession,
// for the principal. created is false, and nothing changes, when the thread was
// already bound: the first report wins.
func (s *PGStore) BindThread(ctx context.Context, thread, claudeSession string) (bool, error) {
	created := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO plugin_threads (thread, claude_session) VALUES ($1, $2)
			ON CONFLICT (owner_id, thread) DO NOTHING`, thread, claudeSession)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		created = true
		return audit(ctx, tx, "plugin.thread.bound", "plugin_thread:"+thread, map[string]any{"claude_session": claudeSession})
	})
	return created, err
}

// BoundThread is the Claude session the principal's thread is bound to, or
// ErrNotFound.
func (s *PGStore) BoundThread(ctx context.Context, thread string) (string, error) {
	var sess string
	err := s.pool.QueryRow(ctx, `SELECT claude_session FROM plugin_threads WHERE thread = $1`, thread).Scan(&sess)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return sess, err
}
