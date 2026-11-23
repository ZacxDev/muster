package builtin

import (
	"context"
	"errors"
	"time"

	"github.com/ZacxDev/muster/internal/itemorigin"
)

// ---------------------------------------------------------------------------
// WHERE AN OFFICER'S ASK WAS MADE FROM (migration 0185: attention_asks
// origin_*). The officer's ask is created by the attention store; its origin
// is written here, on the row it just made, by the officer runtime
// (internal/api officerAsker.Raise, raiseOfficerNotice), and read back for
// the officer page's "Show session".
//
// 🔴 THE ORIGIN COMES FROM THE CALLER'S BOUND CONTEXT ONLY (itemorigin): the
// runtime hands this what the officer tool dispatcher resolved, never a
// value from a tool's arguments. The database refuses an origin that is not
// the ask owner's own agent, that agent's session and that session's message
// (the officer_origin_is_owners trigger), and SetAskOrigin writes only an ask
// with no origin yet, so it can never move one.
// ---------------------------------------------------------------------------

// ErrOriginSet is SetAskOrigin on an ask that already has an origin, or
// that is not owner's.
var ErrOriginSet = errors.New("builtin: the ask is not owner's, or already has an origin")

// SetAskOrigin writes o as owner's ask askID's origin, once. Under ctx's
// principal (row-level security: only its own asks).
func (s *Store) SetAskOrigin(ctx context.Context, owner, askID int64, o itemorigin.Origin) error {
	args := itemorigin.Args(o, o.AgentID > 0)
	if args[0] == nil {
		return nil
	}
	tag, err := s.pool.Exec(ctx, `UPDATE attention_asks SET origin_agent_id = $3, origin_session_id = $4, origin_message_id = $5, origin_at = $6
		WHERE id = $1 AND owner_id = $2 AND origin_agent_id IS NULL`, askID, owner, args[0], args[1], args[2], args[3])
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrOriginSet
	}
	return nil
}

// AskOrigins is the origin of each of owner's asks ids that has one; an ask
// absent from the map has none (made before 0185, or by no officer call).
func (s *Store) AskOrigins(ctx context.Context, owner int64, ids []int64) (map[int64]itemorigin.Origin, error) {
	out := map[int64]itemorigin.Origin{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id, origin_agent_id, coalesce(origin_session_id, 0), coalesce(origin_message_id, 0), origin_at
		FROM attention_asks WHERE owner_id = $1 AND id = ANY($2::bigint[]) AND origin_agent_id IS NOT NULL`, owner, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var o itemorigin.Origin
		var at *time.Time
		if err := rows.Scan(&id, &o.AgentID, &o.SessionID, &o.MessageID, &at); err != nil {
			return nil, err
		}
		if at != nil {
			o.At = *at
		}
		out[id] = o
	}
	return out, rows.Err()
}
