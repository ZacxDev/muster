package plugins

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/users"
)

// PLUGIN PAIRING (migration 0122, docs/plugins.md "Pairing a plugin without
// handling its token"): a plugin asks for its own token, so no `mpl_` token
// is ever typed, pasted or read by a person or an agent.
//
//	plugin ──POST /plugin/v1/pair──▶ request (pending) + code + poll secret
//	owner  ──compares the code, approves──▶ approved
//	plugin ──GET /plugin/v1/pair/{id} + poll secret──▶ the token, minted NOW, once
//
// 🔴 THE TOKEN IS MINTED WHEN IT IS COLLECTED, IN THE TRANSACTION THAT MARKS
// THE REQUEST REDEEMED (RedeemPair), with the request row locked: two polls
// cannot both receive one, and no token is stored anywhere. Only the SHA-256 of
// the code and of the poll secret are stored.
//
// 🔴 THE CODE IS WHAT TIES THE REQUEST THE OWNER APPROVES TO THE PROCESS THAT
// WILL HOLD THE TOKEN: the plugin prints it, the owner compares. A decision
// over MCP must echo it (DecidePair's requireCode); a mismatch is refused.

// PairTTL is how long a pairing request lives (to approve AND to collect).
const PairTTL = 10 * time.Minute

// MaxPairsPerWindow bounds the requests (real or decoy) the server takes in
// one PairTTL: the unauthenticated door's server-wide allowance.
const MaxPairsPerWindow = 50

// pairCodeAlphabet has no 0, 1, I, L, O or U: nothing a person misreads.
const pairCodeAlphabet = "ABCDEFGHJKMNPQRSTVWXYZ23456789"

// The pairing states.
const (
	PairPending  = "pending"
	PairApproved = "approved"
	PairDenied   = "denied"
	PairRedeemed = "redeemed"
	// PairExpired is answered, never stored: a request past its expiry.
	PairExpired = "expired"
)

var (
	// ErrPairState: the request is not pending (decided, redeemed) (409).
	ErrPairState = errors.New("plugins: the pairing request is no longer pending")
	// ErrPairExpired: past PairTTL (410).
	ErrPairExpired = errors.New("plugins: the pairing request has expired")
	// ErrPairCode: the code given is not the request's (409).
	ErrPairCode = errors.New("plugins: that is not the pairing request's code")
)

// PairIn is a plugin's pairing request as it posts it.
type PairIn struct {
	Owner        string       `json:"owner"`
	Name         string       `json:"name"`
	Label        string       `json:"label"`
	DataScope    string       `json:"data_scope"`
	Capabilities []Capability `json:"capabilities"`
	DeliverTo    string       `json:"deliver_to"`
}

// PairCreated is what the plugin is told: its request's public id, the code
// it prints, and the secret it polls with.
type PairCreated struct {
	RequestID  string    `json:"request_id"`
	Code       string    `json:"code"`
	PollSecret string    `json:"poll_secret"`
	ExpiresAt  time.Time `json:"expires_at"`
	// owner and id are the real request's (0 for a decoy): for muster's push,
	// never in the answer.
	owner, id int64
	name      string
}

// Filed is the owner, request id and plugin name of a real request (ok false
// for a decoy) — who muster tells, never what the door answers.
func (c PairCreated) Filed() (owner, id int64, name string, ok bool) {
	return c.owner, c.id, c.name, c.owner > 0
}

// PairRequest is one row of plugin_pair_requests as its owner sees it.
type PairRequest struct {
	ID           int64        `json:"id"`
	OwnerID      int64        `json:"-"`
	PublicID     string       `json:"-"`
	Name         string       `json:"name"`
	Label        string       `json:"label"`
	DataScope    string       `json:"data_scope"`
	Capabilities []Capability `json:"capabilities"`
	DeliverTo    *string      `json:"deliver_to"`
	Status       string       `json:"status"`
	CreatedAt    time.Time    `json:"created_at"`
	ExpiresAt    time.Time    `json:"expires_at"`
	DecidedAt    *time.Time   `json:"decided_at,omitempty"`
	PluginID     *int64       `json:"plugin_id,omitempty"`
	codeHash     string
	secretHash   string
}

// PairPoll is what a poll answers.
type PairPoll struct {
	Status       string       `json:"status"`
	ExpiresAt    *time.Time   `json:"expires_at,omitempty"`
	Token        string       `json:"token,omitempty"`
	PluginID     int64        `json:"plugin_id,omitempty"`
	PluginName   string       `json:"plugin_name,omitempty"`
	Capabilities []Capability `json:"capabilities,omitempty"`
	DeliverTo    *string      `json:"deliver_to,omitempty"`
}

const pairCols = `id, owner_id, public_id, name, label, data_scope, capabilities, deliver_to, status, created_at, expires_at,
	decided_at, plugin_id, code_hash, poll_secret_hash`

func scanPair(row pgx.Row) (PairRequest, error) {
	var p PairRequest
	err := row.Scan(&p.ID, &p.OwnerID, &p.PublicID, &p.Name, &p.Label, &p.DataScope, &p.Capabilities, &p.DeliverTo, &p.Status,
		&p.CreatedAt, &p.ExpiresAt, &p.DecidedAt, &p.PluginID, &p.codeHash, &p.secretHash)
	return p, err
}

// PairObject is the audit_log object naming a pairing request.
func PairObject(id int64) string { return "plugin_pair:" + strconv.FormatInt(id, 10) }

// NewPairCode is a fresh code, XXXX-XXXX from pairCodeAlphabet (~39 bits).
func NewPairCode() (string, error) {
	var b strings.Builder
	for i := 0; i < 8; i++ {
		if i == 4 {
			b.WriteByte('-')
		}
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(pairCodeAlphabet))))
		if err != nil {
			return "", err
		}
		b.WriteByte(pairCodeAlphabet[n.Int64()])
	}
	return b.String(), nil
}

// NormalizePairCode is a code as it is hashed: upper case, without dashes or
// spaces, so "abcd-efgh" and "ABCDEFGH" are the same code.
func NormalizePairCode(code string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' {
			return -1
		}
		return r
	}, strings.ToUpper(strings.TrimSpace(code)))
}

func pairCodeHash(code string) string { return HashToken(NormalizePairCode(code)) }

// validate checks a pairing request's fields (everything but the owner).
func (in *PairIn) validate() error {
	if in.DataScope == "" {
		in.DataScope = in.Name
	}
	if err := ValidName(in.Name); err != nil {
		return err
	}
	in.Label = strings.TrimSpace(in.Label)
	if err := ValidLabel(in.Label); err != nil {
		return err
	}
	if err := ValidScope(in.DataScope); err != nil {
		return err
	}
	if !slices.Contains(in.Capabilities, CapMessages) {
		return invalid("capabilities must include messages")
	}
	for _, c := range in.Capabilities {
		if c != CapMessages && c != CapDraftChecks {
			return invalid("a paired plugin may have messages and draft_checks only")
		}
	}
	slices.Sort(in.Capabilities)
	in.Capabilities = slices.Compact(in.Capabilities)
	if err := ValidDeliverTo(in.DeliverTo); err != nil {
		return err
	}
	if strings.TrimSpace(in.Owner) == "" || len(in.Owner) > 320 {
		return invalid("owner is the owner's login (their email)")
	}
	return nil
}

// CreatePairRequest stores a plugin's request for the account whose login is
// in.Owner — or, for a login naming no live account, a decoy that answers
// polls the same way and can never be approved, so the door says nothing
// about who has an account. ctx is the SYSTEM scope: nobody is signed in.
// ErrRateLimited when MaxPairsPerWindow requests arrived in the last PairTTL.
func (s *PGStore) CreatePairRequest(ctx context.Context, in PairIn, source string, now time.Time) (PairCreated, error) {
	if err := in.validate(); err != nil {
		return PairCreated{}, err
	}
	pub, err := NewToken() // 43 base64url characters; the first 22 are the id
	if err != nil {
		return PairCreated{}, err
	}
	pub = strings.TrimPrefix(pub, TokenPrefix)[:22]
	secret, err := NewToken()
	if err != nil {
		return PairCreated{}, err
	}
	secret = strings.TrimPrefix(secret, TokenPrefix)
	code, err := NewPairCode()
	if err != nil {
		return PairCreated{}, err
	}
	out := PairCreated{RequestID: pub, Code: code, PollSecret: secret, ExpiresAt: now.Add(PairTTL).UTC()}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('plugin_pair_requests.create'))`); err != nil {
			return err
		}
		var recent int
		if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM plugin_pair_requests WHERE created_at > $1)
			+ (SELECT count(*) FROM plugin_pair_decoys WHERE created_at > $1)`, now.Add(-PairTTL)).Scan(&recent); err != nil {
			return err
		}
		if recent >= MaxPairsPerWindow {
			return ErrRateLimited
		}
		if _, err := tx.Exec(ctx, `DELETE FROM plugin_pair_decoys WHERE expires_at < $1`, now.Add(-PairTTL)); err != nil {
			return err
		}
		var owner int64
		err := tx.QueryRow(ctx, `SELECT id FROM users WHERE disabled_at IS NULL
			AND lower(coalesce(nullif(email, ''), display_name)) = lower($1) ORDER BY id LIMIT 1`, strings.TrimSpace(in.Owner)).Scan(&owner)
		if errors.Is(err, pgx.ErrNoRows) {
			_, err = tx.Exec(ctx, `INSERT INTO plugin_pair_decoys (public_id, poll_secret_hash, created_at, expires_at) VALUES ($1, $2, $3, $4)`,
				pub, HashToken(secret), now, out.ExpiresAt)
			return err
		}
		if err != nil {
			return err
		}
		var id int64
		if err := tx.QueryRow(ctx, `INSERT INTO plugin_pair_requests (owner_id, public_id, name, label, data_scope, capabilities, deliver_to,
				code_hash, poll_secret_hash, source_addr, created_at, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, nullif($7, ''), $8, $9, left($10, 100), $11, $12) RETURNING id`,
			owner, pub, in.Name, in.Label, in.DataScope, in.Capabilities, in.DeliverTo, pairCodeHash(code), HashToken(secret), source,
			now, out.ExpiresAt).Scan(&id); err != nil {
			return err
		}
		out.owner, out.id, out.name = owner, id, in.Name
		return users.AuditTx(ctx, tx, users.Actor{UserID: owner, Via: authz.ViaPlugin}, "plugin.pair.requested", PairObject(id),
			map[string]any{"name": in.Name, "capabilities": in.Capabilities, "deliver_to": in.DeliverTo, "source": source})
	})
	if err != nil {
		return PairCreated{}, err
	}
	return out, nil
}

// PendingPairs is the principal's pending, unexpired pairing requests,
// newest first.
func (s *PGStore) PendingPairs(ctx context.Context, now time.Time) ([]PairRequest, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+pairCols+` FROM plugin_pair_requests
		WHERE status = 'pending' AND expires_at > $1 ORDER BY id DESC LIMIT 50`, now)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (PairRequest, error) { return scanPair(r) })
}

// DecidePair approves or denies one of the principal's pending, unexpired
// pairing requests, recording via. With requireCode (a decision over MCP) code
// must be the request's (ErrPairCode). Approving checks what minting checks:
// the name is free among the owner's live plugins, and deliver_to names an
// officer defined for them. Another owner's request, or none, is ErrNotFound.
func (s *PGStore) DecidePair(ctx context.Context, id int64, approve, requireCode bool, code, via string, now time.Time) (PairRequest, error) {
	var out PairRequest
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		cur, err := scanPair(tx.QueryRow(ctx, `SELECT `+pairCols+` FROM plugin_pair_requests WHERE id = $1 FOR UPDATE`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		out = cur
		switch {
		case cur.Status != PairPending:
			return ErrPairState
		case !cur.ExpiresAt.After(now):
			return ErrPairExpired
		case requireCode && subtle.ConstantTimeCompare([]byte(pairCodeHash(code)), []byte(cur.codeHash)) != 1:
			return ErrPairCode
		}
		status, action := PairDenied, "plugin.pair.denied"
		if approve {
			status, action = PairApproved, "plugin.pair.approved"
			if err := s.pairMintable(ctx, tx, cur); err != nil {
				return err
			}
		}
		if out, err = scanPair(tx.QueryRow(ctx, `UPDATE plugin_pair_requests SET status = $2, decided_at = $3, decided_via = $4
			WHERE id = $1 RETURNING `+pairCols, id, status, now, via)); err != nil {
			return err
		}
		return audit(ctx, tx, action, PairObject(id), map[string]any{"name": cur.Name, "decided_via": via})
	})
	return out, err
}

// pairMintable is nil when request r could be minted now for its owner: its
// name is free among the owner's live plugins, and its deliver_to (if any)
// names an officer defined for the owner.
func (s *PGStore) pairMintable(ctx context.Context, tx pgx.Tx, r PairRequest) error {
	var taken bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM plugins WHERE owner_id = $1 AND name = $2 AND revoked_at IS NULL)`,
		r.OwnerID, r.Name).Scan(&taken); err != nil {
		return err
	}
	if taken {
		return invalid("you already have a live plugin named %q; revoke it first", r.Name)
	}
	if r.DeliverTo != nil {
		return s.officerDefined(ctx, r.OwnerID, OfficerOf(*r.DeliverTo))
	}
	return nil
}

// RedeemPair is a plugin's poll of request publicID with its poll secret.
// ctx is a request context with no principal: the request is found under the
// system scope (that is how its owner is learned), and the token is minted
// acting AS ITS OWNER, in the transaction that marks it redeemed, with the
// row locked — so it is handed out at most once. A request that does not
// exist, or a wrong or missing secret, is ErrNotFound (the same answer).
func (s *PGStore) RedeemPair(ctx context.Context, publicID, secret string, now time.Time) (PairPoll, error) {
	if secret == "" || len(publicID) != 22 {
		return PairPoll{}, ErrNotFound
	}
	sys := authz.System(ctx)
	cur, err := scanPair(s.pool.QueryRow(sys, `SELECT `+pairCols+` FROM plugin_pair_requests WHERE public_id = $1`, publicID))
	if errors.Is(err, pgx.ErrNoRows) {
		var hash string
		var exp time.Time
		err := s.pool.QueryRow(sys, `SELECT poll_secret_hash, expires_at FROM plugin_pair_decoys WHERE public_id = $1`, publicID).Scan(&hash, &exp)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && subtle.ConstantTimeCompare([]byte(HashToken(secret)), []byte(hash)) != 1) {
			return PairPoll{}, ErrNotFound
		}
		if err != nil {
			return PairPoll{}, err
		}
		if !exp.After(now) {
			return PairPoll{Status: PairExpired}, nil
		}
		return PairPoll{Status: PairPending, ExpiresAt: &exp}, nil
	}
	if err != nil {
		return PairPoll{}, err
	}
	if subtle.ConstantTimeCompare([]byte(HashToken(secret)), []byte(cur.secretHash)) != 1 {
		return PairPoll{}, ErrNotFound
	}
	expired := !cur.ExpiresAt.After(now)
	switch {
	case cur.Status == PairDenied, cur.Status == PairRedeemed:
		return PairPoll{Status: cur.Status}, nil
	case expired:
		return PairPoll{Status: PairExpired}, nil
	case cur.Status == PairPending:
		exp := cur.ExpiresAt
		return PairPoll{Status: PairPending, ExpiresAt: &exp}, nil
	}
	// Approved and live: mint, as the owner, once.
	octx := authz.WithPrincipal(ctx, authz.Principal{UserID: cur.OwnerID, Role: authz.RoleMember, Via: authz.ViaPlugin})
	var out PairPoll
	err = s.inTx(octx, func(tx pgx.Tx) error {
		r, err := scanPair(tx.QueryRow(octx, `SELECT `+pairCols+` FROM plugin_pair_requests WHERE id = $1 FOR UPDATE`, cur.ID))
		if err != nil {
			return err
		}
		switch {
		case r.Status == PairRedeemed || r.Status == PairDenied:
			out = PairPoll{Status: r.Status}
			return nil
		case r.Status != PairApproved:
			return fmt.Errorf("plugins: pairing %d is %s", r.ID, r.Status)
		case !r.ExpiresAt.After(now):
			out = PairPoll{Status: PairExpired}
			return nil
		}
		if err := s.pairMintable(octx, tx, r); err != nil {
			return err
		}
		deliver := ""
		if r.DeliverTo != nil {
			deliver = *r.DeliverTo
		}
		tok, p, err := s.createPluginTx(octx, tx, NewPlugin{Name: r.Name, Label: r.Label, DataScope: r.DataScope,
			Capabilities: r.Capabilities, DeliverTo: deliver})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(octx, `UPDATE plugin_pair_requests SET status = 'redeemed', redeemed_at = $2, plugin_id = $3 WHERE id = $1`,
			r.ID, now, p.ID); err != nil {
			return err
		}
		out = PairPoll{Status: PairApproved, Token: tok, PluginID: p.ID, PluginName: p.Name, Capabilities: p.Capabilities, DeliverTo: p.DeliverTo}
		return audit(octx, tx, "plugin.pair.redeemed", PairObject(r.ID), map[string]any{"plugin_id": p.ID})
	})
	return out, err
}
