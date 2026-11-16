package plugins

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"regexp"

	"github.com/jackc/pgx/v5"
)

// ROUTE BATCHES (migration 0136; internal/api route_many.go): the chief's
// route_many sends one request — the owner's, relayed under the same
// owner_asked check as route_to_session's text — to several mirrored sessions
// at once. Each target is an ordinary REQUEST route (CreateRequestRouteFrom)
// carrying the batch's id; its answer, its deadline (the batch's) and its
// hand-back are a request route's. What differs is the DELIVERY: a member's
// answer never reaches the chief by itself. Once every member has ended — all
// answered, or the deadline timed the rest out — the sweep delivers ONE
// digest of them all and clears every member's hand-back.
//
// The batch has no claim of its own: its digest is owed while any member owes
// a hand-back, and is delivered under the chief's own claims (internal/api's
// deliverRouteBatch says how); FinishBatchDigest clears the hand-backs.

var batchIDPattern = regexp.MustCompile(`^rb_[0-9a-f]{24}$`)

// ValidBatchID reports whether id is a batch id.
func ValidBatchID(id string) bool { return batchIDPattern.MatchString(id) }

// NewBatchID is a fresh, unguessable batch id.
func NewBatchID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "rb_" + hex.EncodeToString(b[:])
}

// BatchRoutes is the principal's routes of batch batchID, oldest first (the
// order route_many made them). Row-level security scopes it.
func (s *PGStore) BatchRoutes(ctx context.Context, batchID string) ([]Route, error) {
	if !ValidBatchID(batchID) {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT `+routeCols+` FROM session_routes WHERE batch_id = $1 ORDER BY id`, batchID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Route, error) { return scanRoute(row) })
}

// FinishBatchDigest records batchID's digest as delivered: every member's
// owed hand-back is cleared, in one statement. A batch that owes nothing is
// done (internal/api's deliverRouteBatch reads it so).
func (s *PGStore) FinishBatchDigest(ctx context.Context, batchID string) error {
	if !ValidBatchID(batchID) {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE session_routes SET handback = NULL WHERE batch_id = $1`, batchID)
	return err
}
