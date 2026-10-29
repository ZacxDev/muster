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

// The stamp's conversation does not match its message's (SettleOwnerAnswered):
// both are refused with the message's route left exactly as it is.
var (
	// ErrStampNoThread: the message was stored with no thread, so no stamp
	// can name its conversation and none is taken.
	ErrStampNoThread = errors.New("plugins: the message has no thread, so a stamp cannot be bound to its conversation")
	// ErrStampThreadMismatch: the stamp names another conversation than the
	// one the message arrived on.
	ErrStampThreadMismatch = errors.New("plugins: the stamp's thread is not the message's")
)

// SettleOwnerAnswered: see RouteStore. The message must be the principal's
// (row-level security) AND plugin pluginID's own, else ErrNotFound — the same
// answer as a message that does not exist, so a plugin learns nothing about
// another plugin's messages. Its LATEST route is the one judged, under its
// lock: settled only when it is still routed (owner_replied_at is muster's
// now, the audit row session_route.owner_answered). Any other route is
// returned as it is with settled false, and a message never routed returns
// the zero Route (RouteID ""). A repeat of the stamp finds the route already
// owner_answered and changes nothing.
//
// 🔴 THE STAMP IS BOUND TO ITS CONVERSATION. thread is the conversation the
// stamped reply was SENT to, as the sending tool knew it; it must equal the
// thread the message was stored with (plugin_messages.thread), compared
// exactly. A message stored with no thread is ErrStampNoThread, a different
// one ErrStampThreadMismatch — each returned with the latest route as it is,
// unchanged — so a stamp naming the wrong message id cannot settle a route
// of another conversation.
//
// ⚠ A `fallback` route — the session answered through route_reply and its
// draft awaits the owner's approval — is NOT settled (it is not routed): the
// stamp is refused and the draft stays pending. Approving that draft later
// would send the sender a SECOND reply; rejecting it is the owner's call.
func (s *PGStore) SettleOwnerAnswered(ctx context.Context, pluginID, messageID int64, thread string) (Route, bool, error) {
	var r Route
	settled := false
	var refused error
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var owner int64
		var stored *string
		err := tx.QueryRow(ctx, `SELECT plugin_id, thread FROM plugin_messages WHERE id = $1`, messageID).Scan(&owner, &stored)
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
			switch {
			case stored == nil:
				refused = ErrStampNoThread
			case *stored != thread:
				refused = ErrStampThreadMismatch
			}
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
		switch {
		case stored == nil:
			refused = ErrStampNoThread
			return nil
		case *stored != thread:
			refused = ErrStampThreadMismatch
			return nil
		}
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
	return r, settled, refused
}
