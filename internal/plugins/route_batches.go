package plugins

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"time"

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
// 🔴 ONE DIGEST PER BATCH, AND THE SCHEMA HOLDS IT. ClaimBatchDigest claims
// it by updating EVERY member row in one statement, only while none is
// claimed (or a claim has lapsed); a racing second claim re-reads each row
// after the first commits and matches none. FinishBatchDigest makes the claim
// permanent ('infinity') in the statement that clears the hand-backs.

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

// ClaimBatchDigest claims batchID's digest for ttl, true when this caller won
// it: no member was claimed, or the claim had lapsed (its holder died before
// finishing), and the digest is not yet delivered (FinishBatchDigest's
// 'infinity'). Of two callers racing, exactly one wins.
func (s *PGStore) ClaimBatchDigest(ctx context.Context, batchID string, ttl time.Duration) (bool, error) {
	if !ValidBatchID(batchID) {
		return false, nil
	}
	tag, err := s.pool.Exec(ctx, `UPDATE session_routes SET batch_digest_until = now() + make_interval(secs => $2)
		WHERE batch_id = $1 AND (batch_digest_until IS NULL OR batch_digest_until <= now())`, batchID, ttl.Seconds())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ReleaseBatchDigest lets go of batchID's claim when the digest was not
// delivered (its chief could not take it now): the next sweep may claim it.
// A delivered digest ('infinity') is left as it is.
func (s *PGStore) ReleaseBatchDigest(ctx context.Context, batchID string) error {
	if !ValidBatchID(batchID) {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE session_routes SET batch_digest_until = NULL
		WHERE batch_id = $1 AND batch_digest_until <> 'infinity'`, batchID)
	return err
}

// FinishBatchDigest records batchID's digest as delivered: the claim becomes
// permanent and every member's owed hand-back is cleared, in one statement.
func (s *PGStore) FinishBatchDigest(ctx context.Context, batchID string) error {
	if !ValidBatchID(batchID) {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE session_routes SET batch_digest_until = 'infinity', handback = NULL WHERE batch_id = $1`, batchID)
	return err
}
