package plugins

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/authz"
)

// ---------------------------------------------------------------------------
// HELD DISPATCHES: a start that is fully decided and checked, but waits for
// its owner's approval before anything is reserved or created
// (docs/design/chief-dispatch.md, "Held dispatches": the owner's MCP
// dispatch_request / dispatch_approve, and the sibling held dispatch of a card
// tap, #487 — which can share this store).
//
// 🔴 THE STORE IS THE AUDIT LOG, AS THE LEDGER'S IS. A hold is ONE
// ActionDispatchHeld row of its owner's: object a fresh hold id (HoldObject),
// detail the request exactly as it will start. It is CONSUMED by the
// reservation that names it (detail.held = the hold id, DetailHeld): a hold is
// open while it is at most DispatchHoldTTL old by the DATABASE's clock and no
// ActionChiefDispatch row of the owner's names it. There is no new table and no
// UPDATE: users have no UPDATE policy on audit_log, and a consumed flag written
// apart from the reservation could disagree with it.
//
// 🔴 SINGLE USE IS DECIDED UNDER THE OWNER'S RESERVATION LOCK. The approving
// caller reads the hold (DispatchHold) inside ReserveChiefDispatch's check,
// which runs holding the owner's transaction advisory lock (inChiefDispatchLock)
// and before the reservation row is inserted in the same transaction. Every
// reservation of the owner's takes that lock, so a second approval of the same hold waits for the
// first to commit and then reads the reservation that names it.
//
// ⚠ A RESERVATION WHOSE CREATE WAS THEN REFUSED STILL CONSUMES ITS HOLD
// (ReleaseRefusedChiefDispatch frees the task's start and the day's live slot,
// not the hold). An approval is one start's worth of the owner's say-so; a
// refused create asks for a new request.
// ---------------------------------------------------------------------------

const (
	// ActionDispatchHeld is a held dispatch: the request as it will start.
	ActionDispatchHeld = "chief.dispatch_held"
	// ActionDispatchApproved records an approved hold's start: the agent it
	// made, and who approved it.
	ActionDispatchApproved = "chief.dispatch_approved"
	// DetailHeld is the reservation detail key naming the hold it consumes.
	DetailHeld = "held"
	// DispatchHoldTTL is how long a hold can be approved.
	DispatchHoldTTL = 15 * time.Minute
)

// holdObjectPrefix is a hold id's prefix: "held:" and 16 random bytes in hex.
const holdObjectPrefix = "held:"

var holdObjectRE = regexp.MustCompile(`^held:[0-9a-f]{32}$`)

// ValidHoldID reports whether id has a hold id's shape.
func ValidHoldID(id string) bool { return holdObjectRE.MatchString(id) }

// newHoldObject mints a hold id. audit_log.id cannot serve (NewDispatchObject).
func newHoldObject() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return holdObjectPrefix + hex.EncodeToString(b[:]), nil
}

// ErrHoldNotFound: no hold of the principal's has that id — a missing one and
// another owner's read the same.
var ErrHoldNotFound = errors.New("plugins: no such held dispatch")

// DispatchHold is one held dispatch as read now.
type DispatchHold struct {
	// ID is the hold id (its audit row's object).
	ID string
	// RowID is its audit row.
	RowID int64
	// At is when it was filed; ExpiresAt is At + DispatchHoldTTL.
	At, ExpiresAt time.Time
	// Expired: ExpiresAt is past, by the database's clock.
	Expired bool
	// ConsumedBy is the reservation row naming it, 0 when none does.
	ConsumedBy int64
	// Detail is the request as filed.
	Detail json.RawMessage
}

// Open reports whether the hold can still be approved.
func (h DispatchHold) Open() bool { return !h.Expired && h.ConsumedBy == 0 }

// FileDispatchHold: see DispatchStore.
func (s *PGStore) FileDispatchHold(ctx context.Context, detail map[string]any) (DispatchHold, error) {
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 {
		return DispatchHold{}, errors.New("plugins: a held dispatch needs a user principal")
	}
	id, err := newHoldObject()
	if err != nil {
		return DispatchHold{}, err
	}
	if detail == nil {
		detail = map[string]any{}
	}
	err = s.inTx(ctx, func(tx pgx.Tx) error { return audit(ctx, tx, ActionDispatchHeld, id, detail) })
	if err != nil {
		return DispatchHold{}, err
	}
	return s.DispatchHold(ctx, id)
}

// DispatchHold: see DispatchStore.
//
// 🔴 THE OWNER IS IN THE WHERE (user_id = $2) on top of row-level security,
// as every ledger read is (ReserveChiefDispatch): with RLS rolled back another
// owner's hold must still read as missing.
func (s *PGStore) DispatchHold(ctx context.Context, id string) (DispatchHold, error) {
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 {
		return DispatchHold{}, errors.New("plugins: a held dispatch needs a user principal")
	}
	if !ValidHoldID(id) {
		return DispatchHold{}, ErrHoldNotFound
	}
	return s.readHold(ctx, p.UserID, `h.object = $5`, id)
}

// readHold is the newest hold of owner's matching where (whose own arguments
// start at $5).
func (s *PGStore) readHold(ctx context.Context, owner int64, where string, args ...any) (DispatchHold, error) {
	var (
		h      DispatchHold
		detail []byte
	)
	all := append([]any{ActionDispatchHeld, owner, DispatchHoldTTL.Seconds(), ActionChiefDispatch}, args...)
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT h.object, h.id, h.at, h.at + make_interval(secs => $3),
		       h.at + make_interval(secs => $3) <= now(), h.detail,
		       coalesce((SELECT r.id FROM audit_log r WHERE r.action = $4 AND r.user_id = $2
		                  AND r.detail->>'`+DetailHeld+`' = h.object ORDER BY r.id LIMIT 1), 0)
		  FROM audit_log h
		 WHERE h.action = $1 AND h.user_id = $2 AND `+where+`
		 ORDER BY h.id DESC LIMIT 1`, all...).Scan(&h.ID, &h.RowID, &h.At, &h.ExpiresAt, &h.Expired, &detail, &h.ConsumedBy)
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return DispatchHold{}, ErrHoldNotFound
	case err != nil:
		return DispatchHold{}, err
	}
	h.Detail = detail
	return h, nil
}

// RecordDispatchApproved: see DispatchStore.
func (s *PGStore) RecordDispatchApproved(ctx context.Context, holdID string, detail map[string]any) error {
	if !ValidHoldID(holdID) {
		return ErrHoldNotFound
	}
	return s.inTx(ctx, func(tx pgx.Tx) error { return audit(ctx, tx, ActionDispatchApproved, holdID, detail) })
}

// CardHoldByKey is the principal's NEWEST held dispatch filed by agent_dispatch
// for a card (detail.door "card") under request_key key — open or not; the
// caller reads Open(). A newer hold for the same key supersedes an older one
// (a host tap that re-files the hold for the model card), so only the newest
// is ever completed. ErrHoldNotFound when there is none.
func (s *PGStore) CardHoldByKey(ctx context.Context, key string) (DispatchHold, error) {
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 {
		return DispatchHold{}, errors.New("plugins: a held dispatch needs a user principal")
	}
	return s.readHold(ctx, p.UserID, `h.detail->>'door' = 'card' AND h.detail->>'request_key' = $5`, key)
}
