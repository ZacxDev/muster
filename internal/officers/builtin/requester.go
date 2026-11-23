package builtin

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// THE MIRRORED SESSION THAT ASKED FOR AN ITEM (migration 0178,
// officer_items.requested_host / requested_session): the Claude Code session
// whose own MCP door enqueued it (internal/api officer_item_enqueue), so the
// owner's decision on a draft the officer files for it reaches that session
// (internal/api session_wake_requests.go).
//
// 🔴 ONLY THE BOUND CALLER WRITES IT. SetItemRequester's one caller passes the
// host of the mirror token the call came in on and the session that host lists
// running in the calling window NOW — never a field of the call's arguments or
// a request header. The admin API (a personal or hook token, which names no
// session), the owner's pages, plugins and schedules never write it; `muster
// officer item enqueue` inside a mirrored session reaches it only through the
// session's own MCP door (cmd/muster officer_enqueue_session.go).
//
// 🔴 AND IT IS INHERITED, NEVER NAMED. The one other writer is
// inheritRequesterTx: the follow-up muster files when an item's draft ends
// unsent (DraftEndedTx: a plugin's refusal, a failed send, an expiry, a broken
// binding, Checks replies off, an owner rejection that could not go to the
// session) takes the requester of the item it continues, copied row to row in
// the same transaction. It is the same ask, so the decision on the redraft
// reaches the session that made it, not nobody.

// inheritRequesterTx copies, inside tx, owner's officer's item from's requester
// onto item to (a follow-up DraftEndedTx just filed), unless to already names
// one (FIRST REQUESTER WINS, as in SetItemRequester). An item no session asked
// for gives none: its empty pair is copied, a no-op.
func inheritRequesterTx(ctx context.Context, tx pgx.Tx, owner int64, officer string, from, to int64) error {
	_, err := tx.Exec(ctx, `UPDATE officer_items f SET requested_host = i.requested_host, requested_session = i.requested_session
		FROM officer_items i
		WHERE f.owner_id = $1 AND f.id = $4 AND f.requested_host = '' AND f.requested_session = ''
		  AND i.owner_id = $1 AND i.officer = $2 AND i.id = $3`, owner, officer, from, to)
	return classify(err)
}

// SetItemRequester records that the mirrored session (host, session) asked for
// owner's item id, unless the item already names one (FIRST REQUESTER WINS: an
// enqueue deduplicated onto another session's live item never takes it over).
// It reports whether it wrote.
func (s *Store) SetItemRequester(ctx context.Context, owner, id int64, host, session string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE officer_items SET requested_host = $3, requested_session = $4
		WHERE owner_id = $1 AND id = $2 AND requested_host = '' AND requested_session = ''`, owner, id, host, session)
	if err != nil {
		return false, classify(err)
	}
	return tag.RowsAffected() == 1, nil
}

// ItemRequester is the mirrored session that asked for owner's item id:
// ("", "", nil) for an item no session asked for, ErrNotFound for no such item
// the caller can see.
func (s *Store) ItemRequester(ctx context.Context, owner, id int64) (host, session string, err error) {
	err = s.pool.QueryRow(ctx, `SELECT requested_host, requested_session FROM officer_items WHERE owner_id = $1 AND id = $2`,
		owner, id).Scan(&host, &session)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotFound
	}
	return host, session, err
}
