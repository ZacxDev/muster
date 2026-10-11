package connectors

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PGStore is the Postgres-backed [Store]. Secrets are sealed with the key given
// to [NewPG] before they reach a query, and opened only by [PGStore.Credential].
type PGStore struct {
	pool *pgxpool.Pool
	seal sealer
}

// NewPG builds a PGStore. key must be [KeySize] bytes.
func NewPG(pool *pgxpool.Pool, key []byte) (*PGStore, error) {
	s, err := newSealer(key)
	if err != nil {
		return nil, err
	}
	return &PGStore{pool: pool, seal: s}, nil
}

var _ Store = (*PGStore)(nil)

const connectorCols = `id, name, platform, hosts, auth_scheme, auth_header, auth_username, secret_hint,
	created_at, updated_at, rotated_at, last_used_at`

func scanConnector(row pgx.Row) (Connector, error) {
	var c Connector
	var scheme string
	err := row.Scan(&c.ID, &c.Name, &c.Platform, &c.Hosts, &scheme, &c.HeaderName, &c.Username, &c.SecretHint,
		&c.CreatedAt, &c.UpdatedAt, &c.RotatedAt, &c.LastUsedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Connector{}, ErrNotFound
	}
	c.Scheme = Scheme(scheme)
	return c, err
}

// ListConnectors returns every connector, by name.
func (s *PGStore) ListConnectors(ctx context.Context) ([]Connector, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+connectorCols+` FROM connectors ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Connector
	for rows.Next() {
		c, err := scanConnector(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetConnector returns one connector's public description.
func (s *PGStore) GetConnector(ctx context.Context, name string) (Connector, error) {
	return scanConnector(s.pool.QueryRow(ctx, `SELECT `+connectorCols+` FROM connectors WHERE name=$1`, name))
}

// CreateConnector validates, seals and inserts a connector. A duplicate name is
// ErrInvalid.
func (s *PGStore) CreateConnector(ctx context.Context, nc NewConnector) (Connector, error) {
	nc, err := nc.Validate()
	if err != nil {
		return Connector{}, err
	}
	ct, err := s.seal.seal(nc.Name, nc.Secret)
	if err != nil {
		return Connector{}, err
	}
	c, err := scanConnector(s.pool.QueryRow(ctx, `
		INSERT INTO connectors (name, platform, hosts, auth_scheme, auth_header, auth_username, secret_ciphertext, secret_hint)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING `+connectorCols,
		nc.Name, nc.Platform, nc.Hosts, string(nc.Scheme), nc.HeaderName, nc.Username, ct, Hint(nc.Secret)))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return Connector{}, fmt.Errorf("%w: a connector named %q already exists", ErrInvalid, nc.Name)
	}
	return c, err
}

// RotateSecret replaces a connector's secret. Grants are kept: rotation changes
// what is sent, not who may send it.
func (s *PGStore) RotateSecret(ctx context.Context, name, secret string) error {
	if err := ValidateSecret(secret); err != nil {
		return err
	}
	ct, err := s.seal.seal(name, secret)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE connectors SET secret_ciphertext=$2, secret_hint=$3, rotated_at=now(), updated_at=now()
		WHERE name=$1`, name, ct, Hint(secret))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteConnector removes a connector AND every grant naming it, in one
// transaction, so a connector later re-created under the same name starts with
// no approvals.
func (s *PGStore) DeleteConnector(ctx context.Context, name string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after Commit
	tag, err := tx.Exec(ctx, `DELETE FROM connectors WHERE name=$1`, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `DELETE FROM connector_grants WHERE connector_name=$1`, name); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Credential returns the connector with its decrypted secret. A secret that
// does not open under the configured key is ErrDecrypt.
func (s *PGStore) Credential(ctx context.Context, name string) (Connector, string, error) {
	var ct []byte
	var c Connector
	var scheme string
	err := s.pool.QueryRow(ctx, `SELECT `+connectorCols+`, secret_ciphertext FROM connectors WHERE name=$1`, name).
		Scan(&c.ID, &c.Name, &c.Platform, &c.Hosts, &scheme, &c.HeaderName, &c.Username, &c.SecretHint,
			&c.CreatedAt, &c.UpdatedAt, &c.RotatedAt, &c.LastUsedAt, &ct)
	if errors.Is(err, pgx.ErrNoRows) {
		return Connector{}, "", ErrNotFound
	}
	if err != nil {
		return Connector{}, "", err
	}
	c.Scheme = Scheme(scheme)
	secret, err := s.seal.open(c.Name, ct)
	if err != nil {
		return Connector{}, "", err
	}
	return c, secret, nil
}

// TouchConnector stamps last_used_at.
func (s *PGStore) TouchConnector(ctx context.Context, name string) error {
	_, err := s.pool.Exec(ctx, `UPDATE connectors SET last_used_at=now() WHERE name=$1`, name)
	return err
}

// --- grants -----------------------------------------------------------------------

const grantCols = `g.id, g.agent_id, coalesce(a.name, ''), g.connector_name, g.status, g.reason, g.requested_at,
	g.decided_at, g.decided_by, g.expires_at, g.last_used_at`

const grantFrom = ` FROM connector_grants g LEFT JOIN agents a ON a.id = g.agent_id`

func scanGrant(row pgx.Row) (Grant, error) {
	var g Grant
	err := row.Scan(&g.ID, &g.AgentID, &g.AgentName, &g.Connector, &g.Status, &g.Reason, &g.RequestedAt,
		&g.DecidedAt, &g.DecidedBy, &g.ExpiresAt, &g.LastUsedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Grant{}, ErrNotFound
	}
	return g, err
}

func (s *PGStore) queryGrants(ctx context.Context, where string, args ...any) ([]Grant, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+grantCols+grantFrom+` `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Grant
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// RequestGrant returns the live grant for (agent, connector) or files a pending
// one. An approved grant past its expiry is not live: it is marked revoked and a
// fresh pending request is filed.
func (s *PGStore) RequestGrant(ctx context.Context, agentID int64, connector, reason string) (Grant, bool, error) {
	if err := ValidateName(connector); err != nil {
		return Grant{}, false, err
	}
	if len(reason) > 2000 {
		reason = reason[:2000]
	}
	// Retire an expired approval first, so the partial unique index admits the new
	// pending row.
	if _, err := s.pool.Exec(ctx, `
		UPDATE connector_grants SET status='revoked', decided_by='expired', decided_at=now()
		WHERE agent_id=$1 AND connector_name=$2 AND status='approved' AND expires_at IS NOT NULL AND expires_at <= now()`,
		agentID, connector); err != nil {
		return Grant{}, false, err
	}
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO connector_grants (agent_id, connector_name, reason) VALUES ($1, $2, $3)
		ON CONFLICT (agent_id, connector_name) WHERE status IN ('pending', 'approved') DO NOTHING
		RETURNING id`, agentID, connector, reason).Scan(&id)
	created := true
	if errors.Is(err, pgx.ErrNoRows) {
		created = false
		err = s.pool.QueryRow(ctx, `SELECT id FROM connector_grants
			WHERE agent_id=$1 AND connector_name=$2 AND status IN ('pending', 'approved')`, agentID, connector).Scan(&id)
	}
	if err != nil {
		return Grant{}, false, err
	}
	g, err := s.GetGrant(ctx, id)
	return g, created, err
}

// LatestGrant returns the newest grant row for the pair.
func (s *PGStore) LatestGrant(ctx context.Context, agentID int64, connector string) (Grant, error) {
	return scanGrant(s.pool.QueryRow(ctx, `SELECT `+grantCols+grantFrom+`
		WHERE g.agent_id=$1 AND g.connector_name=$2 ORDER BY g.id DESC LIMIT 1`, agentID, connector))
}

// GetGrant returns one grant.
func (s *PGStore) GetGrant(ctx context.Context, id int64) (Grant, error) {
	return scanGrant(s.pool.QueryRow(ctx, `SELECT `+grantCols+grantFrom+` WHERE g.id=$1`, id))
}

// DecideGrant approves or denies a pending grant.
func (s *PGStore) DecideGrant(ctx context.Context, id int64, approve bool, by string, expiresAt *time.Time) (Grant, error) {
	status := StatusDenied
	if approve {
		status = StatusApproved
	} else {
		expiresAt = nil
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE connector_grants SET status=$2, decided_by=$3, decided_at=now(), expires_at=$4
		WHERE id=$1 AND status='pending'`, id, status, by, expiresAt)
	if err != nil {
		return Grant{}, err
	}
	if tag.RowsAffected() == 0 {
		if _, gerr := s.GetGrant(ctx, id); gerr != nil {
			return Grant{}, gerr
		}
		return Grant{}, ErrNotPending
	}
	return s.GetGrant(ctx, id)
}

// RevokeGrant ends a pending or approved grant.
func (s *PGStore) RevokeGrant(ctx context.Context, id int64, by string) (Grant, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE connector_grants SET status='revoked', decided_by=$2, decided_at=now()
		WHERE id=$1 AND status IN ('pending', 'approved')`, id, by)
	if err != nil {
		return Grant{}, err
	}
	if tag.RowsAffected() == 0 {
		if _, gerr := s.GetGrant(ctx, id); gerr != nil {
			return Grant{}, gerr
		}
		return Grant{}, ErrNotPending
	}
	return s.GetGrant(ctx, id)
}

// ListPendingGrants returns the requests awaiting the operator, oldest first.
func (s *PGStore) ListPendingGrants(ctx context.Context) ([]Grant, error) {
	return s.queryGrants(ctx, `WHERE g.status='pending' ORDER BY g.requested_at, g.id`)
}

// ListGrants returns every live (pending or approved) grant.
func (s *PGStore) ListGrants(ctx context.Context) ([]Grant, error) {
	return s.queryGrants(ctx, `WHERE g.status IN ('pending', 'approved') ORDER BY g.connector_name, a.name, g.id`)
}

// TouchGrant stamps last_used_at.
func (s *PGStore) TouchGrant(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE connector_grants SET last_used_at=now() WHERE id=$1`, id)
	return err
}

// --- audit ---------------------------------------------------------------------------

// Audit appends one entry.
func (s *PGStore) Audit(ctx context.Context, e AuditEntry) error {
	var agentID *int64
	if e.AgentID != 0 {
		agentID = &e.AgentID
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO connector_audit (kind, agent_id, agent_name, connector_name, method, host, path, status, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		e.Kind, agentID, e.AgentName, e.Connector, e.Method, e.Host, truncate(e.Path, 1024), e.Status, truncate(e.Detail, 500))
	return err
}

// ListAudit returns the newest entries first.
func (s *PGStore) ListAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, at, kind, coalesce(agent_id, 0), agent_name, connector_name, method, host, path, status, detail
		FROM connector_audit ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.At, &e.Kind, &e.AgentID, &e.AgentName, &e.Connector, &e.Method, &e.Host,
			&e.Path, &e.Status, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneAudit deletes entries older than before.
func (s *PGStore) PruneAudit(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM connector_audit WHERE at < $1`, before)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
