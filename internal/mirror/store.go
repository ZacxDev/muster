package mirror

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/users"
)

// ErrBadToken is the refusal for a mirror token that is unknown or revoked, or
// whose owner is neither an enabled person nor a team (a disabled user).
var ErrBadToken = errors.New("mirror: unknown or revoked mirror token")

// Token is one minted mirror token's record. The token itself is never stored:
// only its sha256, so a database read cannot be replayed as a credential.
type Token struct {
	ID int64 `json:"id"`
	// OwnerID is the user who minted it; the host's sessions are visible only to
	// that user's principal.
	OwnerID   int64      `json:"ownerId"`
	Host      string     `json:"host"`
	CreatedAt time.Time  `json:"createdAt"`
	RevokedAt *time.Time `json:"revokedAt,omitempty"`
	LastUsed  *time.Time `json:"lastUsedAt,omitempty"`
	// RotateAt is when the token was marked rotate (team sharing 2b.2,
	// mirror_tokens.rotate_at): an operator or a manager of its team left,
	// dropped to viewer or was disabled. Nil for a personal token, and for a
	// team token nobody has left since it was minted.
	RotateAt *time.Time `json:"rotateAt,omitempty"`
	// SpawnedAt is when this token's host first advertised -agent-spawn or had
	// a host dispatch reserved on it (mirror_tokens.spawned_at, 0150). Nothing
	// clears it: a host-spawned agent can read the token, so the mark ends
	// only with the token (rotate it). Nil for a token whose host never did.
	SpawnedAt *time.Time `json:"spawnedAt,omitempty"`
	// Team: the owner is a TEAM (users.kind = 'team'), so the host is a team
	// host: its connection is pinned (Hub: a second connection waits for a
	// manager). Set by Authenticate only.
	Team bool `json:"team,omitempty"`
}

// Store is the plugin's persistence: tokens, and nothing else.
//
// 🔴 EVERY METHOD RUNS UNDER THE CALLER'S PRINCIPAL, AND mirror_tokens IS UNDER
// ROW-LEVEL SECURITY (migration 0008). Authenticate and TokenLive must find a
// token before anyone is known, so internal/mirror's Service calls them under
// authz.System; CreateToken, ListTokens and RevokeToken run as the minting user
// (authz.Operator for `muster-server mirror token …`), so a token lands owned by
// them and they see and revoke only their own.
type Store interface {
	// Authenticate resolves a presented token to its record, or ErrBadToken.
	Authenticate(ctx context.Context, token string) (Token, error)
	// TokenLive reports whether token id is still unrevoked and its owner
	// still an enabled person or a team (Authenticate's rule).
	TokenLive(ctx context.Context, id int64) (bool, error)
	// CreateToken mints a token for host, owned by ctx's user, and returns it
	// ONCE, with its record.
	CreateToken(ctx context.Context, host string) (string, Token, error)
	ListTokens(ctx context.Context) ([]Token, error)
	// RevokeToken revokes token id, reporting whether it was live.
	RevokeToken(ctx context.Context, id int64) (bool, error)
}

// HashToken is the stored form of a token.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// NewToken mints a fresh token: TokenPrefix + 43 base64url characters (256 bits).
func NewToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mirror: mint a token: %w", err)
	}
	return TokenPrefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// TokenShapeOK reports whether token is shaped like one NewToken mints:
// TokenPrefix + 43 base64url characters. It is a FILTER, not authentication:
// it lets the connect route refuse junk without a database read.
func TokenShapeOK(token string) bool {
	rest, ok := strings.CutPrefix(token, TokenPrefix)
	if !ok || len(rest) != 43 {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(rest)
	return err == nil && len(b) == 32
}

// PGStore is Store over Postgres (migration 0008).
type PGStore struct{ pool *pgxpool.Pool }

// NewPG builds the Postgres store.
func NewPG(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

const tokenCols = `id, owner_id, host, created_at, revoked_at, last_used_at, rotate_at, spawned_at`

func scanToken(row pgx.Row) (Token, error) {
	var t Token
	err := row.Scan(&t.ID, &t.OwnerID, &t.Host, &t.CreatedAt, &t.RevokedAt, &t.LastUsed, &t.RotateAt, &t.SpawnedAt)
	return t, err
}

// Authenticate looks the token up by hash among unrevoked tokens and stamps its
// last use.
//
// 🔴 THE OWNER SET IS users.EnabledPersonOrTeamIDs: AN ENABLED PERSON, OR A
// TEAM (team sharing phase 2b.2). A token whose owner is a DISABLED person is
// refused as if unknown (#681), on every call. A disable also revokes the
// person's rows (users.SetDisabledAs), but this read is the guarantee — it
// holds for a token that revoke missed, and it is the same read TokenLive
// makes, so a disable also ends the owner's live connections.
//
// A TEAM-OWNED token is a TEAM HOST's: its host's sessions register under the
// team, and its /mirror/mcp calls act as the team acting principal through
// the reduced tool set (internal/api teamHostTools). A team has no
// disabled_at; a team token kept by someone who left is the rotate mark's
// case (rotate_at), never this filter's. Team reports which kind it is.
//
// ⚠ A STRING WITHOUT THE PREFIX IS REFUSED BEFORE THE QUERY. That is not the
// security property — the hash lookup is, and no other credential's hash is in
// this table — it only keeps a hook token pasted into the wrong config from
// costing a database round trip per reconnect.
func (s *PGStore) Authenticate(ctx context.Context, token string) (Token, error) {
	if !strings.HasPrefix(token, TokenPrefix) {
		return Token{}, ErrBadToken
	}
	var t Token
	err := s.pool.QueryRow(ctx, `
		UPDATE mirror_tokens SET last_used_at = now()
		WHERE token_hash = $1 AND revoked_at IS NULL
			AND owner_id IN (`+users.EnabledPersonOrTeamIDs+`)
		RETURNING `+tokenCols+`, EXISTS (SELECT 1 FROM users u WHERE u.id = mirror_tokens.owner_id AND u.kind = 'team')`, HashToken(token)).
		Scan(&t.ID, &t.OwnerID, &t.Host, &t.CreatedAt, &t.RevokedAt, &t.LastUsed, &t.RotateAt, &t.SpawnedAt, &t.Team)
	if errors.Is(err, pgx.ErrNoRows) {
		return Token{}, ErrBadToken
	}
	return t, err
}

// TokenLive is the live connection's recheck (Service.watchToken): whether
// token id would still authenticate. It applies Authenticate's owner rule
// (users.EnabledPersonOrTeamIDs), so a connection whose owner is a disabled
// person ends at its next recheck, exactly as a revoked one does (#681).
func (s *PGStore) TokenLive(ctx context.Context, id int64) (bool, error) {
	var live bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mirror_tokens WHERE id = $1 AND revoked_at IS NULL
			AND owner_id IN (`+users.EnabledPersonOrTeamIDs+`))`, id).Scan(&live)
	return live, err
}

func (s *PGStore) CreateToken(ctx context.Context, host string) (string, Token, error) {
	if err := ValidHost(host); err != nil {
		return "", Token{}, err
	}
	tok, err := NewToken()
	if err != nil {
		return "", Token{}, err
	}
	// owner_id is the column default: the acting principal's user (0008).
	t, err := scanToken(s.pool.QueryRow(ctx, `
		INSERT INTO mirror_tokens (host, token_hash) VALUES ($1, $2) RETURNING `+tokenCols,
		host, HashToken(tok)))
	if err != nil {
		return "", Token{}, err
	}
	return tok, t, nil
}

// ErrNotATeam: CreateTeamToken was handed an id that names no team.
var ErrNotATeam = errors.New("mirror: no such team")

// CreateTeamToken mints a TEAM HOST token for host, owned by team, and writes
// the team's audit row (team.host.token.create, as by) in the SAME
// transaction: `muster-server mirror token create <host> --team <id>`, the
// headless bootstrap of a team host (team sharing 2b.2). ctx is
// authz.System's — the server's own shell, which can already do anything —
// so the owner is written explicitly, and only when it names a team
// (ErrNotATeam otherwise: never a person's token through this path).
//
// A manager mints from /teams/{id}/hosts instead, AS THE TEAM (CreateToken
// under the team acting principal), with the step-up.
func (s *PGStore) CreateTeamToken(ctx context.Context, by users.Actor, team int64, host string) (string, Token, error) {
	if err := ValidHost(host); err != nil {
		return "", Token{}, err
	}
	tok, err := NewToken()
	if err != nil {
		return "", Token{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", Token{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	t, err := scanToken(tx.QueryRow(ctx, `
		INSERT INTO mirror_tokens (owner_id, host, token_hash)
		SELECT id, $2, $3 FROM users WHERE id = $1 AND kind = 'team'
		RETURNING `+tokenCols, team, host, HashToken(tok)))
	if errors.Is(err, pgx.ErrNoRows) {
		return "", Token{}, ErrNotATeam
	}
	if err != nil {
		return "", Token{}, err
	}
	t.Team = true
	if err := users.AuditTeamTx(ctx, tx, by, team, "team.host.token.create", fmt.Sprintf("mirror_token:%d", t.ID), map[string]any{"host": host}); err != nil {
		return "", Token{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", Token{}, err
	}
	return tok, t, nil
}

func (s *PGStore) ListTokens(ctx context.Context) ([]Token, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+tokenCols+` FROM mirror_tokens ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Token{}
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *PGStore) RevokeToken(ctx context.Context, id int64) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE mirror_tokens SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// MarkSpawned records that live token id's host has started agents
// (mirror_tokens.spawned_at, 0150): once, keeping the first time. A revoked
// token is left alone. Service calls it under authz.System when the host's
// hello advertises -agent-spawn, before the welcome (Service.markSpawned).
func (s *PGStore) MarkSpawned(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE mirror_tokens SET spawned_at = now() WHERE id = $1 AND revoked_at IS NULL AND spawned_at IS NULL`, id)
	return err
}
