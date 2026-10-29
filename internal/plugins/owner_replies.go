package plugins

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// THE OWNER ANSWERED IT THEMSELVES (migration 0062):
// docs/plugins.md#when-you-answer-it-yourself. A routed message's sender can
// get their answer from the owner directly, on the upstream conversation,
// without the session ever calling route_reply — and then muster, which never
// saw the reply, would time the route out and hand an answered message back
// to the chief. The plugin, which sees the owner's own posts, reports one
// (POST /plugin/v1/owner-replies {thread, at}) and the routes it answers end
// owner_answered.

// OwnerReplyGrace is how long after a route TIMED OUT an owner reply still
// settles it. A route times out when no session answered in time, but the
// owner may still be answering the sender by hand: measured once, a direct
// reply came 37 minutes after a 30-minute route's deadline. Two hours covers
// that more than three times over, plus the plugin's own lag in seeing the
// reply (a poll cycle and a quiet window, minutes), and is short enough that
// a reply on the same conversation the next day does not rewrite a route
// whose deadline really did pass unanswered.
const OwnerReplyGrace = 2 * time.Hour

// OwnerSettled is one route SettleOwnerReplied ended: the route as it is now,
// the state it ended from (routed or timed_out), and whether it still OWED
// its chief a hand-back, which the settle withdrew (a hand-back already
// delivered stands: the chief is told the owner answered).
type OwnerSettled struct {
	Route             Route
	From              RouteState
	HandBackWithdrawn bool
}

// ownerReplyAnswers is THE predicate: whether an owner reply at `at` answers
// route r — r was made before the reply, and is still routed or timed out at
// most OwnerReplyGrace before it. An answered, relayed, failed or already
// settled route is left as it is: its answer, or its failure, is its own.
func ownerReplyAnswers(r Route, at time.Time) bool {
	if !r.CreatedAt.Before(at) {
		return false
	}
	switch r.State {
	case RouteRouted:
		return true
	case RouteTimedOut:
		return r.FinishedAt != nil && !at.After(r.FinishedAt.Add(OwnerReplyGrace))
	}
	return false
}

// SettleOwnerReplied: see RouteStore. Only the principal's routes of plugin
// pluginID's OWN messages on thread are candidates (row-level security scopes
// them to the owner; the plugin_id to the plugin), and each is judged under
// its lock by ownerReplyAnswers. A settled route records the reply's time,
// keeps no error and owes no hand-back; its session_route.owner_answered audit
// row names the state it left and whether a hand-back was withdrawn. Settling
// is idempotent: a repeat of the same report finds nothing left to settle.
func (s *PGStore) SettleOwnerReplied(ctx context.Context, pluginID int64, thread string, at time.Time) ([]OwnerSettled, error) {
	if !ValidThread(thread) {
		return nil, invalid("thread is 1-%d bytes with no spaces, control or invisible characters", MaxThreadLen)
	}
	if at.IsZero() {
		return nil, invalid("at is required")
	}
	out := []OwnerSettled{}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		out = out[:0]
		rows, err := tx.Query(ctx, `SELECT r.route_id FROM session_routes r
			  JOIN plugin_messages m ON m.id = r.plugin_message_id
			 WHERE m.plugin_id = $1 AND m.thread = $2 AND r.state IN ('routed', 'timed_out')
			 ORDER BY r.id`, pluginID, thread)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, id := range ids {
			cur, err := lockRoute(ctx, tx, id)
			if err != nil {
				return err
			}
			if !ownerReplyAnswers(cur, at) {
				continue
			}
			var owed bool
			if err := tx.QueryRow(ctx, `SELECT handback IS NOT NULL FROM session_routes WHERE id = $1`, cur.ID).Scan(&owed); err != nil {
				return err
			}
			r, err := scanRoute(tx.QueryRow(ctx, `UPDATE session_routes SET state = 'owner_answered', owner_replied_at = $2,
				finished_at = coalesce(finished_at, now()), error = '', handback = NULL
				WHERE id = $1 RETURNING `+routeCols, cur.ID, at))
			if err != nil {
				return err
			}
			if err := audit(ctx, tx, "session_route.owner_answered", RouteObject(id), map[string]any{
				"from": string(cur.State), "thread": thread, "owner_replied_at": at.UTC().Format(time.RFC3339),
				"handback_withdrawn": owed}); err != nil {
				return err
			}
			out = append(out, OwnerSettled{Route: r, From: cur.State, HandBackWithdrawn: owed})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
