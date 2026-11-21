// Package signinreq is the store behind "Sign in with the muster app"
// (migration 0173; docs/design/qr-sign-in.md): a signed-out browser starts a
// REQUEST and shows its code as a QR; a phone already signed in to muster
// opens the code, is shown the request's details and approves or denies it;
// the browser that started it redeems the approval for a session of the
// approver.
//
// The flow, one method each:
//
//   - Create: the browser starts a request. Two secrets come back: the CODE
//     (the QR carries it) and the BINDING (the browser keeps it in an HttpOnly
//     cookie). Only their sha256s are stored.
//   - Status: the browser polls by its binding.
//   - Open: the phone opened the code (pending -> scanned).
//   - Decide: the phone approved or denied (pending|scanned -> approved|denied),
//     recording the approver and the sign-in time of the session that approved.
//   - Redeem: the browser spends the approval (approved -> redeemed). It needs
//     the code AND the binding of the SAME row, so a code read off a screen, a
//     log line or a photo cannot be redeemed by another browser.
//
// 🔴 EXPIRY IS NOT A STATE: every transition checks expires_at against the
// database's clock, so a request past it can be neither opened, decided nor
// redeemed whatever its state says (Status reports it as StateExpired).
//
// 🔴 SINGLE USE: each transition is one conditional UPDATE (or a row lock
// followed by one), so two redeems of one approval cannot both succeed, and a
// decided request cannot be decided again.
//
// 🔴 AN APPROVAL DIES WITH THE SESSION THAT GAVE IT. Redeem refuses an
// approval whose approver is disabled, or whose approving session's sign-in is
// at or before the approver's session cutoff (users.sessions_not_before) or
// latest Sign out everywhere (users.signed_out_at) — the rule a paired phone is
// held to (devices.LiveSQL), in whole seconds.
//
// The table has no owner (a request is anonymous until it is approved), so
// every query runs under authz.System through this package's one helper, sys
// (ledgered in internal/authz/ledger_test.go), and every query is keyed by the
// hash of a secret its caller holds.
package signinreq

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/users"
)

const (
	// TTL is how long a request lives, from its creation: the phone must open
	// and decide it, and the browser redeem it, within this.
	TTL = 2 * time.Minute
	// CodePrefix starts every code; the binding has none (it never leaves an
	// HttpOnly cookie).
	CodePrefix = "mqr_"
	// encodedLen is the base64url (no padding) length of 32 bytes.
	encodedLen = 43
	// MaxAgentLen and MaxIPLen bound the details stored for the phone.
	MaxAgentLen = 120
	MaxIPLen    = 64
	// sweepAfter is how long past its expiry a row is kept before an insert
	// deletes it.
	sweepAfter = time.Hour
)

// State is a request's state as Status reports it.
type State string

const (
	StatePending  State = "pending"
	StateScanned  State = "scanned"
	StateApproved State = "approved"
	StateDenied   State = "denied"
	StateRedeemed State = "redeemed"
	// StateExpired is never stored: it is any unredeemed row past expires_at.
	StateExpired State = "expired"
)

var (
	// ErrNotFound: no live request for that secret — unknown, expired, already
	// decided (for Open/Decide), already redeemed, or a code and binding that
	// are not one row's. One error for all of them.
	ErrNotFound = errors.New("signinreq: no such live sign-in request")
	// ErrNotApproved: Redeem of a request the phone has not decided yet.
	ErrNotApproved = errors.New("signinreq: the sign-in request is not approved yet")
	// ErrDenied: Redeem of a request the phone denied.
	ErrDenied = errors.New("signinreq: the sign-in request was denied")
	// ErrApproverEnded: the approver is disabled, or the session that approved
	// has been ended since (cutoff or Sign out everywhere).
	ErrApproverEnded = errors.New("signinreq: the session that approved this sign-in has been ended")
	// ErrNoApprover: Decide with no approver or no sign-in time.
	ErrNoApprover = errors.New("signinreq: an approval needs the approver and its session's sign-in time")
)

// Request is one request's details.
type Request struct {
	ID             int64
	State          State
	RequesterIP    string
	RequesterAgent string
	CreatedAt      time.Time
	ExpiresAt      time.Time
	// ApprovedBy is the approver's user id, 0 until approved.
	ApprovedBy int64
}

// Object is the audit_log `object` a request's rows carry.
func Object(id int64) string { return "signin_request:" + strconv.FormatInt(id, 10) }

// Store is the sign-in request store.
type Store interface {
	Create(ctx context.Context, ip, agent string) (code, binding string, expiresAt time.Time, err error)
	Status(ctx context.Context, binding string) (Request, error)
	Open(ctx context.Context, code string) (Request, error)
	Decide(ctx context.Context, code string, approve bool, by users.Actor, signinAt time.Time) (Request, error)
	Redeem(ctx context.Context, code, binding string) (Request, error)
}

// PGStore is Store over Postgres (migration 0173).
type PGStore struct{ pool *pgxpool.Pool }

// NewPG returns the store over pool.
func NewPG(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

var _ Store = (*PGStore)(nil)

// sys is this package's ONE authz.System call (ledgered). See the package doc.
func sys(ctx context.Context) context.Context { return authz.System(ctx) }

func newSecret(prefix string) (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("signinreq: mint a secret: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func shapeOK(prefix, s string) bool {
	rest, ok := strings.CutPrefix(s, prefix)
	if !ok || len(rest) != encodedLen {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(rest)
	return err == nil && len(b) == 32
}

// CodeShapeOK reports whether s is shaped like a minted code.
func CodeShapeOK(s string) bool { return shapeOK(CodePrefix, s) }

// BindingShapeOK reports whether s is shaped like a minted binding.
func BindingShapeOK(s string) bool { return shapeOK("", s) }

func hash(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// clip bounds a detail to max runes, dropping invalid UTF-8 and control
// characters (they are shown on the phone).
func clip(s string, max int) string {
	var b strings.Builder
	n := 0
	for _, r := range strings.TrimSpace(s) {
		if n >= max {
			break
		}
		if r == utf8.RuneError || r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// The columns every read returns, with the effective state: an unredeemed
// row past its expiry reads as expired.
const cols = `id, CASE WHEN state <> 'redeemed' AND expires_at <= now() THEN 'expired' ELSE state END,
	requester_ip, requester_agent, created_at, expires_at, coalesce(approved_by, 0)`

func dest(r *Request) []any {
	return []any{&r.ID, &r.State, &r.RequesterIP, &r.RequesterAgent, &r.CreatedAt, &r.ExpiresAt, &r.ApprovedBy}
}

func (s *PGStore) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Create starts a request for the browser at ip with User-Agent summary agent.
func (s *PGStore) Create(ctx context.Context, ip, agent string) (string, string, time.Time, error) {
	code, err := newSecret(CodePrefix)
	if err != nil {
		return "", "", time.Time{}, err
	}
	binding, err := newSecret("")
	if err != nil {
		return "", "", time.Time{}, err
	}
	ctx = sys(ctx)
	var expires time.Time
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		// Rows long past their expiry go with each new one, so nothing else
		// needs to sweep them; an hour's are kept for a person reading the
		// audit log beside them.
		if _, err := tx.Exec(ctx, `DELETE FROM signin_requests WHERE expires_at < now() - make_interval(secs => $1)`,
			sweepAfter.Seconds()); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO signin_requests (code_sha256, binding_sha256, requester_ip, requester_agent, expires_at)
			VALUES ($1, $2, $3, $4, now() + make_interval(secs => $5)) RETURNING expires_at`,
			hash(code), hash(binding), clip(ip, MaxIPLen), clip(agent, MaxAgentLen), TTL.Seconds()).Scan(&expires)
	})
	if err != nil {
		return "", "", time.Time{}, err
	}
	return code, binding, expires, nil
}

// Status is the request the browser holding binding started, in its
// effective state.
func (s *PGStore) Status(ctx context.Context, binding string) (Request, error) {
	var r Request
	if !BindingShapeOK(binding) {
		return r, ErrNotFound
	}
	err := s.pool.QueryRow(sys(ctx), `SELECT `+cols+` FROM signin_requests WHERE binding_sha256 = $1`, hash(binding)).Scan(dest(&r)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Request{}, ErrNotFound
	}
	return r, err
}

// Open marks a live, undecided request as scanned and returns it. Opening it
// again (a reload) returns it unchanged.
func (s *PGStore) Open(ctx context.Context, code string) (Request, error) {
	var r Request
	if !CodeShapeOK(code) {
		return r, ErrNotFound
	}
	err := s.pool.QueryRow(sys(ctx), `UPDATE signin_requests SET state = 'scanned', scanned_at = coalesce(scanned_at, now())
		WHERE code_sha256 = $1 AND state IN ('pending', 'scanned') AND expires_at > now()
		RETURNING `+cols, hash(code)).Scan(dest(&r)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Request{}, ErrNotFound
	}
	return r, err
}

// Decide approves or denies a live, undecided request as by, whose session
// signed in at signinAt, and audits it in the same transaction.
func (s *PGStore) Decide(ctx context.Context, code string, approve bool, by users.Actor, signinAt time.Time) (Request, error) {
	var r Request
	if by.UserID <= 0 || signinAt.IsZero() {
		return r, ErrNoApprover
	}
	if !CodeShapeOK(code) {
		return r, ErrNotFound
	}
	state, action := StateDenied, "signin.app.deny"
	var approver *int64
	var signin *time.Time
	if approve {
		state, action = StateApproved, "signin.app.approve"
		approver, signin = &by.UserID, &signinAt
	}
	ctx = sys(ctx)
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `UPDATE signin_requests SET state = $2, decided_at = now(), approved_by = $3, approver_signin_at = $4
			WHERE code_sha256 = $1 AND state IN ('pending', 'scanned') AND expires_at > now()
			RETURNING `+cols, hash(code), string(state), approver, signin).Scan(dest(&r)...)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		return users.AuditTx(ctx, tx, by, action, Object(r.ID),
			map[string]any{"requester_ip": r.RequesterIP, "requester_agent": r.RequesterAgent})
	})
	if err != nil {
		return Request{}, err
	}
	return r, nil
}

// Redeem spends an approved request for the browser holding binding, and
// returns it (ApprovedBy is whose session to issue). code and binding must be
// the SAME row's.
//
// 🔴 IT TAKES THE APPROVER'S CREDENTIALS LOCK (users.LockCredentialsTx) before
// re-checking the approver, as a device pairing does: a disable, a reset or a
// Sign out everywhere that commits first is seen, and one that comes after
// meets a session issued after it (which it then ends).
func (s *PGStore) Redeem(ctx context.Context, code, binding string) (Request, error) {
	var r Request
	if !CodeShapeOK(code) || !BindingShapeOK(binding) {
		return r, ErrNotFound
	}
	ctx = sys(ctx)
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		// The row names the approver whose lock to take, so it is read first
		// without a lock, then locked and re-read (the users.LockCredentialsTx
		// order rule: the credentials lock before any row lock).
		var approver int64
		err := tx.QueryRow(ctx, `SELECT coalesce(approved_by, 0) FROM signin_requests WHERE code_sha256 = $1 AND binding_sha256 = $2`,
			hash(code), hash(binding)).Scan(&approver)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if approver > 0 {
			if err := users.LockCredentialsTx(ctx, tx, approver); err != nil {
				return err
			}
		}
		var signin *time.Time
		err = tx.QueryRow(ctx, `SELECT `+cols+`, approver_signin_at FROM signin_requests
			WHERE code_sha256 = $1 AND binding_sha256 = $2 FOR UPDATE`, hash(code), hash(binding)).Scan(append(dest(&r), &signin)...)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		switch r.State {
		case StatePending, StateScanned:
			return ErrNotApproved
		case StateDenied:
			return ErrDenied
		case StateApproved:
		default: // expired, redeemed
			return ErrNotFound
		}
		if r.ApprovedBy != approver || signin == nil {
			return ErrNotFound // changed between the two reads: never redeem it
		}
		var live bool
		if err := tx.QueryRow(ctx, `SELECT disabled_at IS NULL
				AND (sessions_not_before IS NULL OR floor(extract(epoch FROM $2::timestamptz)) > floor(extract(epoch FROM sessions_not_before)))
				AND (signed_out_at IS NULL OR floor(extract(epoch FROM $2::timestamptz)) > floor(extract(epoch FROM signed_out_at)))
			FROM users WHERE id = $1 AND id IN (`+users.PersonIDs+`)`, approver, *signin).Scan(&live); errors.Is(err, pgx.ErrNoRows) {
			return ErrApproverEnded
		} else if err != nil {
			return err
		}
		if !live {
			return ErrApproverEnded
		}
		if _, err := tx.Exec(ctx, `UPDATE signin_requests SET state = 'redeemed', redeemed_at = now() WHERE id = $1`, r.ID); err != nil {
			return err
		}
		r.State = StateRedeemed
		return users.AuditTx(ctx, tx, users.Actor{UserID: approver, Via: authz.ViaSession}, "signin.app.redeem", Object(r.ID),
			map[string]any{"requester_ip": r.RequesterIP, "requester_agent": r.RequesterAgent})
	})
	if err != nil {
		return Request{}, err
	}
	return r, nil
}
