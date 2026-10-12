package credentials

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/ccpool"
)

// PGStore is the Postgres-backed credential store (table user_credentials,
// migration 0010).
//
// 🔴 EVERY READ AND WRITE NAMES ITS OWNER EXPLICITLY (`owner_id = $n`), AS THE
// CONNECTOR STORE DOES. Row-level security already scopes a user principal to
// its own rows; the explicit predicate is what keeps a scope-all caller (the
// pod-spec path may run under authz.System) from ever resolving "a" credential
// across users. Writes take the owner from the acting principal and never from
// an argument: a member can only ever write their own.
type PGStore struct {
	pool   *pgxpool.Pool
	seal   sealer
	minter Minter
	logger *log.Logger
	now    func() time.Time
}

// NewPG builds the store. key is the connector store's 32-byte key; this store
// derives its own from it (seal.go). minter mints OpenRouter inference keys from
// a management key (nil: [OpenRouterMinter] against openrouter.ai).
func NewPG(pool *pgxpool.Pool, key []byte, minter Minter, logger *log.Logger) (*PGStore, error) {
	s, err := newSealer(key)
	if err != nil {
		return nil, err
	}
	if minter == nil {
		minter = OpenRouterMinter{}
	}
	if logger == nil {
		logger = log.New(log.Writer(), "", log.LstdFlags)
	}
	return &PGStore{pool: pool, seal: s, minter: minter, logger: logger, now: time.Now}, nil
}

// errNoOwner: writes need a user principal (never System, never none).
var errNoOwner = errors.New("credentials: a credential belongs to a signed-in user; this request has none")

// writer is the acting user, for writes.
func writer(ctx context.Context) (int64, error) {
	p, ok := authz.From(ctx)
	if !ok || p.All() || p.UserID <= 0 {
		return 0, errNoOwner
	}
	return p.UserID, nil
}

// List returns the acting user's credentials, Claude accounts by name then
// the OpenRouter key. No secret leaves this method.
func (s *PGStore) List(ctx context.Context) ([]Summary, error) {
	owner, err := writer(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT kind, name, fingerprint, created_at, updated_at FROM user_credentials
		WHERE owner_id = $1 ORDER BY kind, name`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Summary
	for rows.Next() {
		var c Summary
		if err := rows.Scan(&c.Kind, &c.Name, &c.Fingerprint, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PutClaude adds (or replaces) the acting user's Claude account `name`.
func (s *PGStore) PutClaude(ctx context.Context, name, token string) error {
	owner, err := writer(ctx)
	if err != nil {
		return err
	}
	if err := ValidateAccountName(name); err != nil {
		return err
	}
	if err := ValidateClaudeToken(token); err != nil {
		return err
	}
	ct, err := s.seal.seal(binding{Owner: owner, Kind: KindClaude, Name: name, Slot: "secret"}, token)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO user_credentials (owner_id, kind, name, secret_ciphertext, fingerprint)
		VALUES ($1, 'claude', $2, $3, $4)
		ON CONFLICT (owner_id, kind, name) DO UPDATE SET secret_ciphertext = EXCLUDED.secret_ciphertext,
			fingerprint = EXCLUDED.fingerprint, updated_at = now()`,
		owner, name, ct, ccpool.Fingerprint(token))
	return err
}

// Delete removes the acting user's credential (kind, name). Deleting the
// OpenRouter key also revokes the inference key minted from it (best-effort).
// Reports whether a row existed.
func (s *PGStore) Delete(ctx context.Context, kind, name string) (bool, error) {
	owner, err := writer(ctx)
	if err != nil {
		return false, err
	}
	if kind == KindOpenRouter {
		name = OpenRouterName
		if mgmt, ref, ok, err := s.openRouterRow(ctx, owner); err == nil && ok {
			s.revoke(ctx, owner, mgmt, ref)
		}
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM user_credentials WHERE owner_id = $1 AND kind = $2 AND name = $3`, owner, kind, name)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// PutOpenRouter stores the acting user's OpenRouter MANAGEMENT key, minting the
// inference key their agents will use. A previous key's minted key is revoked
// after the new one is saved (best-effort).
//
// ⚠ AN AGENT ALREADY RUNNING KEEPS THE KEY ITS POD WAS GIVEN, and revoking the
// old minted key stops it working: Stop then Start the agent to re-apply its
// spec with the new key (agentprovision re-resolves on every build).
func (s *PGStore) PutOpenRouter(ctx context.Context, managementKey string) error {
	owner, err := writer(ctx)
	if err != nil {
		return err
	}
	if err := ValidateOpenRouterKey(managementKey); err != nil {
		return err
	}
	oldMgmt, oldRef, hadOld, err := s.openRouterRow(ctx, owner)
	if err != nil && !errors.Is(err, ErrDecrypt) {
		return err
	}
	key, ref, err := s.minter.Mint(ctx, managementKey, fmt.Sprintf("muster user %d", owner))
	if err != nil {
		return err
	}
	b := binding{Owner: owner, Kind: KindOpenRouter, Name: OpenRouterName}
	b.Slot = "secret"
	sct, err := s.seal.seal(b, managementKey)
	if err != nil {
		return err
	}
	b.Slot = "derived"
	dct, err := s.seal.seal(b, key)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO user_credentials (owner_id, kind, name, secret_ciphertext, fingerprint, derived_ciphertext, derived_ref)
		VALUES ($1, 'openrouter', 'default', $2, $3, $4, $5)
		ON CONFLICT (owner_id, kind, name) DO UPDATE SET secret_ciphertext = EXCLUDED.secret_ciphertext,
			fingerprint = EXCLUDED.fingerprint, derived_ciphertext = EXCLUDED.derived_ciphertext,
			derived_ref = EXCLUDED.derived_ref, updated_at = now()`,
		owner, sct, ccpool.Fingerprint(managementKey), dct, ref)
	if err != nil {
		// The minted key would otherwise be orphaned on the member's account.
		s.revoke(ctx, owner, managementKey, ref)
		return err
	}
	if hadOld && oldRef != ref {
		s.revoke(ctx, owner, oldMgmt, oldRef)
	}
	return nil
}

// revoke deletes a minted key, logging (never returning) a failure. 🔴 The log
// line names the owner and nothing secret.
func (s *PGStore) revoke(ctx context.Context, owner int64, mgmt, ref string) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := s.minter.Revoke(rctx, mgmt, ref); err != nil {
		s.logger.Printf("credentials: user %d: revoking a minted OpenRouter key failed: %v (delete it in OpenRouter's dashboard)", owner, err)
	}
}

// openRouterRow opens owner's management key and the minted key's ref.
func (s *PGStore) openRouterRow(ctx context.Context, owner int64) (mgmt, ref string, ok bool, err error) {
	var ct []byte
	err = s.pool.QueryRow(ctx, `SELECT secret_ciphertext, derived_ref FROM user_credentials
		WHERE owner_id = $1 AND kind = 'openrouter' AND name = 'default'`, owner).Scan(&ct, &ref)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	mgmt, err = s.seal.open(binding{Owner: owner, Kind: KindOpenRouter, Name: OpenRouterName, Slot: "secret"}, ct)
	if err != nil {
		return "", "", false, err
	}
	return mgmt, ref, true, nil
}

// ClaudeTokens returns owner's Claude accounts, name → setup-token. An empty
// map (never an error) means the owner has none.
//
// 🔴 ONE OF THE TWO METHODS THAT OPEN A SECRET. Its caller is [Plane], whose
// callers are pod-spec building and account selection — never a page.
func (s *PGStore) ClaudeTokens(ctx context.Context, owner int64) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT name, secret_ciphertext FROM user_credentials
		WHERE owner_id = $1 AND kind = 'claude'`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name string
		var ct []byte
		if err := rows.Scan(&name, &ct); err != nil {
			return nil, err
		}
		tok, err := s.seal.open(binding{Owner: owner, Kind: KindClaude, Name: name, Slot: "secret"}, ct)
		if err != nil {
			return nil, fmt.Errorf("credentials: Claude account %q of user %d: %w", name, owner, err)
		}
		out[name] = tok
	}
	return out, rows.Err()
}

// OpenRouterKey returns the INFERENCE key minted from owner's management key —
// the one their gateway agents are given — or ok=false when they have none.
func (s *PGStore) OpenRouterKey(ctx context.Context, owner int64) (string, bool, error) {
	var ct []byte
	err := s.pool.QueryRow(ctx, `SELECT derived_ciphertext FROM user_credentials
		WHERE owner_id = $1 AND kind = 'openrouter' AND name = 'default'`, owner).Scan(&ct)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	key, err := s.seal.open(binding{Owner: owner, Kind: KindOpenRouter, Name: OpenRouterName, Slot: "derived"}, ct)
	if err != nil {
		return "", false, fmt.Errorf("credentials: OpenRouter key of user %d: %w", owner, err)
	}
	return key, true, nil
}

// HasOpenRouterKey reports whether owner has an OpenRouter key, opening nothing.
func (s *PGStore) HasOpenRouterKey(ctx context.Context, owner int64) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM user_credentials
		WHERE owner_id = $1 AND kind = 'openrouter'`, owner).Scan(&n)
	return n > 0, err
}

// ClaudeNames returns owner's Claude account names, opening nothing.
func (s *PGStore) ClaudeNames(ctx context.Context, owner int64) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT name FROM user_credentials WHERE owner_id = $1 AND kind = 'claude' ORDER BY name`, owner)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
