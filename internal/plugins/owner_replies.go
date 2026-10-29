package plugins

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// THE OWNER ANSWERED IT DIRECTLY (migration 0062):
// docs/plugins.md#when-the-owner-answers-it-directly. A routed message's
// sender can get their answer through the plugin's own sending path rather
// than route_reply — a session, or the owner, sending the reply directly —
// and muster, which never saw it, would time the route out and hand an
// answered message back to the chief. The tool that sent it STAMPS the send
// with the message it answers, and its plugin reports that one message
// (POST /plugin/v1/messages/{id}/owner-answered): its waiting route ends
// owner_answered. Nothing is inferred: a reply sent without the stamp
// settles nothing.

// SettleOwnerAnswered: see RouteStore. The message must be the principal's
// (row-level security) AND plugin pluginID's own, else ErrNotFound — the same
// answer as a message that does not exist, so a plugin learns nothing about
// another plugin's messages. Its LATEST route is the one judged, under its
// lock: settled only when it is still routed (owner_replied_at is muster's
// now, the audit row session_route.owner_answered). Any other route is
// returned as it is with settled false, and a message never routed returns
// the zero Route (RouteID ""). A repeat of the stamp finds the route already
// owner_answered and changes nothing.
func (s *PGStore) SettleOwnerAnswered(ctx context.Context, pluginID, messageID int64) (Route, bool, error) {
	var r Route
	settled := false
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var owner int64
		err := tx.QueryRow(ctx, `SELECT plugin_id FROM plugin_messages WHERE id = $1`, messageID).Scan(&owner)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && owner != pluginID) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var id string
		err = tx.QueryRow(ctx, `SELECT route_id FROM session_routes WHERE plugin_message_id = $1 ORDER BY id DESC LIMIT 1`,
			messageID).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		cur, err := lockRoute(ctx, tx, id)
		if err != nil {
			return err
		}
		r = cur
		if cur.State != RouteRouted {
			return nil
		}
		if r, err = scanRoute(tx.QueryRow(ctx, `UPDATE session_routes SET state = 'owner_answered', owner_replied_at = now(),
			finished_at = now() WHERE route_id = $1 RETURNING `+routeCols, id)); err != nil {
			return err
		}
		settled = true
		return audit(ctx, tx, "session_route.owner_answered", RouteObject(id), map[string]any{
			"message_id": messageID, "plugin_id": pluginID})
	})
	if err != nil {
		return Route{}, false, err
	}
	return r, settled, nil
}
