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

// Batch is one route_many batch as the sweep reads it: its members, oldest
// first (the order route_many made them); Size, the member count route_many
// SEALED it with once every route of the call was made (SealBatch; 0 while
// unsealed); and Owed, whether any member still owes its chief a hand-back —
// the digest not yet delivered.
type Batch struct {
	Members []Route
	Size    int
	Owed    bool
}

// Sealed reports whether b was sealed and every member it was sealed with is
// there.
func (b Batch) Sealed() bool { return b.Size > 0 && len(b.Members) == b.Size }

// BatchRoutes is the principal's batch batchID, in two queries whatever its
// size. Row-level security scopes it.
func (s *PGStore) BatchRoutes(ctx context.Context, batchID string) (Batch, error) {
	if !ValidBatchID(batchID) {
		return Batch{}, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT `+routeCols+` FROM session_routes WHERE batch_id = $1 ORDER BY id`, batchID)
	if err != nil {
		return Batch{}, err
	}
	members, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Route, error) { return scanRoute(row) })
	if err != nil {
		return Batch{}, err
	}
	b := Batch{Members: members}
	err = s.pool.QueryRow(ctx, `SELECT coalesce(max(batch_size), 0), coalesce(bool_or(handback IS NOT NULL), false)
		FROM session_routes WHERE batch_id = $1`, batchID).Scan(&b.Size, &b.Owed)
	return b, err
}

// SealBatch closes batch batchID once route_many has made every route it
// will: routes of the batch NOT in routeIDs (a route made and then refused,
// its paste impossible) leave the batch — they were refused in the call's own
// result, so the digest neither counts nor lists them — and every member
// records the batch's size, len(routeIDs). Until it is sealed a batch is
// never digested, so a member that ends while the call is still making the
// rest cannot trigger a digest of part of the batch. One transaction.
func (s *PGStore) SealBatch(ctx context.Context, batchID string, routeIDs []string) error {
	if !ValidBatchID(batchID) || len(routeIDs) == 0 {
		return nil
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE session_routes SET batch_id = NULL WHERE batch_id = $1 AND NOT route_id = ANY($2)`,
			batchID, routeIDs); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE session_routes SET batch_size = $2 WHERE batch_id = $1`, batchID, len(routeIDs))
		return err
	})
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
