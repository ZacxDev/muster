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
	// DispatchHoldTTL is how long an MCP hold (dispatch_request's, door "mcp")
	// can be approved: dispatch_approve follows its request in the same
	// session, and no card waits on it.
	DispatchHoldTTL = 15 * time.Minute
	// CardHoldTTL is how long a CARD hold (agent_dispatch's, door "card": a
	// host, model, repo, clone or approval card) can be completed — and so how
	// long its card stays in front of the owner. 🔴 WHY 24 HOURS (measured in
	// production): an approval card sat open for hours while its hold had
	// lapsed at 15 minutes, so the owner's tap approved nothing and the chief
	// had to file a new card. Operator decision: "Card expires with its hold"
	// — the hold now lives as long as a card is shown (attention's
	// DefaultTimeout), and the card is withdrawn when the hold lapses
	// (LapsedCardHolds).
	CardHoldTTL = 24 * time.Hour
	// CardAskOutlives is how much longer a card hold's attention ask is filed
	// to wait than the hold itself, so the HOLD always lapses first and the
	// sweep that withdraws its card (and tells the chief) sees it still open.
	CardAskOutlives = time.Hour
)

// CardHoldDoor is the door agent_dispatch's card holds are filed with.
const CardHoldDoor = "card"

// HoldTTL is how long a hold filed through door can be approved: CardHoldTTL
// for a card's, DispatchHoldTTL for any other. THE one rule: holdTTLSQL is the
// same choice in SQL.
func HoldTTL(door string) time.Duration {
	if door == CardHoldDoor {
		return CardHoldTTL
	}
	return DispatchHoldTTL
}

// holdTTLSQL is HoldTTL in SQL for the hold row h, the card TTL's seconds in
// $card and the other's in $mcp.
func holdTTLSQL(card, mcp string) string {
	return `(CASE WHEN h.detail->>'door' = '` + CardHoldDoor + `' THEN ` + card + `::double precision ELSE ` + mcp + `::double precision END)`
}

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
	// At is when it was filed; ExpiresAt is At + HoldTTL(its door).
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
	return s.readHold(ctx, p.UserID, `h.object = $6`, id)
}

// readHold is the newest hold of owner's matching where (whose own arguments
// start at $6).
func (s *PGStore) readHold(ctx context.Context, owner int64, where string, args ...any) (DispatchHold, error) {
	var (
		h      DispatchHold
		detail []byte
	)
	all := append([]any{ActionDispatchHeld, owner, DispatchHoldTTL.Seconds(), ActionChiefDispatch, CardHoldTTL.Seconds()}, args...)
	ttl := `make_interval(secs => ` + holdTTLSQL("$5", "$3") + `)`
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT h.object, h.id, h.at, h.at + `+ttl+`,
		       h.at + `+ttl+` <= now(), h.detail,
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

// CardHoldByAsk is the principal's newest card hold whose card is attention
// ask askID (detail.ask_id) — open or not. ErrHoldNotFound when none is.
func (s *PGStore) CardHoldByAsk(ctx context.Context, askID int64) (DispatchHold, error) {
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 {
		return DispatchHold{}, errors.New("plugins: a held dispatch needs a user principal")
	}
	return s.readHold(ctx, p.UserID, `h.detail->>'door' = 'card' AND h.detail->>'ask_id' = $6::bigint::text`, askID)
}

// ApprovalHoldCounts is how many APPROVAL card holds (detail.approval) the
// principal filed: within the last `within` by the database's clock (hour; a
// re-filed card — detail.refile, the repo card a "Change repo" tap opens — is
// the same request and is not counted again), and — of those still open:
// unexpired, unconsumed, and their card still unanswered (a declined or
// withdrawn card frees its turn at once) — how many were filed
// in chief turn `turn` (detail.turn; 0 counts none) for a request_key other
// than `key` (inTurn). The bound on how often a chief that is not in its
// owner's typed turn may put an approval card in front of them.
func (s *PGStore) ApprovalHoldCounts(ctx context.Context, within time.Duration, turn int64, key string) (hour, inTurn int, err error) {
	p, ok := authz.From(ctx)
	if !ok || p.UserID <= 0 {
		return 0, 0, errors.New("plugins: a held dispatch needs a user principal")
	}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT
		    count(*) FILTER (WHERE h.at > now() - make_interval(secs => $3) AND (h.detail->>'refile')::boolean IS NOT TRUE),
		    count(*) FILTER (WHERE $5::bigint > 0 AND h.detail->>'turn' = $5::bigint::text
		                       AND h.detail->>'request_key' IS DISTINCT FROM $6
		                       AND h.at + make_interval(secs => $7) > now()
		                       AND NOT EXISTS (SELECT 1 FROM audit_log r WHERE r.action = $4 AND r.user_id = $2
		                                        AND r.detail->>'`+DetailHeld+`' = h.object)
		                       AND EXISTS (SELECT 1 FROM attention_asks a WHERE a.owner_id = $2
		                                    AND a.id::text = h.detail->>'ask_id' AND a.status = 'open'))
		  FROM audit_log h
		 WHERE h.action = $1 AND h.user_id = $2 AND h.detail->>'door' = 'card' AND (h.detail->>'approval')::boolean IS TRUE
		   AND h.at > now() - make_interval(secs => greatest($3, $7))`,
			ActionDispatchHeld, p.UserID, within.Seconds(), ActionChiefDispatch, turn, key, CardHoldTTL.Seconds()).Scan(&hour, &inTurn)
	})
	return hour, inTurn, err
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
	return s.readHold(ctx, p.UserID, `h.detail->>'door' = 'card' AND h.detail->>'request_key' = $6`, key)
}

// LapsedCardHold is a card hold that lapsed unused while its card is still
// open: its owner, its id, the card (attention ask) and the request_key.
type LapsedCardHold struct {
	Owner      int64
	HoldID     string
	AskID      int64
	RequestKey string
	ExpiresAt  time.Time
}

// lapsedCardHoldsLookback bounds how far back LapsedCardHolds reads: no card
// is open longer than attention's MaxTimeout (7 days), so a hold filed before
// this cannot have an open card.
const lapsedCardHoldsLookback = 8 * 24 * time.Hour

// LapsedCardHolds is every owner's CARD hold (door "card") past CardHoldTTL by
// the database's clock, consumed by no reservation, whose card — the attention
// ask detail.ask_id names, the hold's owner's — is still open: what the sweep
// withdraws (internal/api's withdrawLapsedHoldCards), each AS ITS OWNER, and
// tells the chief about. Oldest first, at most limit. Under the system scope
// (internal/authz's ledger names it): owners, ids and keys only, no detail.
func (s *PGStore) LapsedCardHolds(ctx context.Context, limit int) ([]LapsedCardHold, error) {
	rows, err := s.pool.Query(authz.System(ctx), `SELECT h.user_id, h.object, a.id, coalesce(h.detail->>'request_key', ''),
	       h.at + make_interval(secs => $3)
	  FROM audit_log h
	  JOIN attention_asks a ON a.owner_id = h.user_id AND a.id::text = h.detail->>'ask_id' AND a.status = 'open'
	 WHERE h.action = $1 AND h.detail->>'door' = '`+CardHoldDoor+`'
	   AND h.at > now() - make_interval(secs => $4)
	   AND h.at + make_interval(secs => $3) <= now()
	   AND NOT EXISTS (SELECT 1 FROM audit_log r WHERE r.action = $2 AND r.user_id = h.user_id
	                    AND r.detail->>'`+DetailHeld+`' = h.object)
	 ORDER BY h.id LIMIT $5`, ActionDispatchHeld, ActionChiefDispatch, CardHoldTTL.Seconds(), lapsedCardHoldsLookback.Seconds(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LapsedCardHold
	for rows.Next() {
		var l LapsedCardHold
		if err := rows.Scan(&l.Owner, &l.HoldID, &l.AskID, &l.RequestKey, &l.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
