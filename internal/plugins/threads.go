package plugins

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// THREADS A SESSION STARTED (plugin_threads, migration 0054): when one of the
// owner's mirrored Claude Code sessions sends agent mail that starts a thread,
// its PostToolUse hook reports the thread (POST /api/plugin-threads), and a
// later inbound message on that thread is routed straight back to that session
// (internal/api's routeByThread) — the chief's route, without the chief's
// decision. docs/plugins.md#a-reply-on-a-thread-a-session-started.
//
// 🔴 THE FIRST REPORT WINS. A thread is bound to the session that STARTED it;
// the primary key (owner_id, thread) and INSERT … ON CONFLICT DO NOTHING make a
// second report — another session's, or the same one's — change nothing.

// AgentMailPlugin is the one plugin, by name, whose threads a session can be
// bound to: agent mail (operator decision), the same plugin the unrouted
// sweep judges (UnroutedAskPlugin).
const AgentMailPlugin = UnroutedAskPlugin

// RouteViaThread is RouteOrigin.Via for a route muster made because the
// message is a reply on a thread the target session started.
const RouteViaThread = "thread"

// ThreadBinding is one row of plugin_threads.
type ThreadBinding struct {
	Thread        string
	Host          string
	ClaudeSession string
	CreatedAt     time.Time
}

// BindThread binds thread to the mirrored Claude Code session claudeSession on
// host, for the principal. created is false — and the stored binding is
// returned unchanged — when the thread was already bound.
func (s *PGStore) BindThread(ctx context.Context, thread, host, claudeSession string) (ThreadBinding, bool, error) {
	if !ValidThread(thread) {
		return ThreadBinding{}, false, invalid("thread is 1-%d bytes with no spaces, control or invisible characters", MaxThreadLen)
	}
	var b ThreadBinding
	created := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO plugin_threads (thread, host, claude_session) VALUES ($1, $2, $3)
			ON CONFLICT (owner_id, thread) DO NOTHING RETURNING thread, host, claude_session, created_at`,
			thread, cutRunes(host, 200), claudeSession).Scan(&b.Thread, &b.Host, &b.ClaudeSession, &b.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			b, err = boundThread(ctx, tx, thread)
			return err
		}
		if err != nil {
			return err
		}
		created = true
		return audit(ctx, tx, "plugin.thread.bound", "plugin_thread:"+thread, map[string]any{
			"host": b.Host, "claude_session": b.ClaudeSession})
	})
	return b, created, err
}

// BoundThread is the principal's binding for thread, or ErrNotFound.
func (s *PGStore) BoundThread(ctx context.Context, thread string) (ThreadBinding, error) {
	return boundThread(ctx, s.pool, thread)
}

func boundThread(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, thread string) (ThreadBinding, error) {
	var b ThreadBinding
	err := q.QueryRow(ctx, `SELECT thread, host, claude_session, created_at FROM plugin_threads WHERE thread = $1`, thread).
		Scan(&b.Thread, &b.Host, &b.ClaudeSession, &b.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ThreadBinding{}, ErrNotFound
	}
	return b, err
}
