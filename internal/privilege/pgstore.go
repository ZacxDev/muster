package privilege

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PGStore is the Postgres-backed privilege Store.
type PGStore struct{ pool *pgxpool.Pool }

// NewPG constructs a Postgres-backed privilege Store.
func NewPG(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

const requestCols = `id, agent_id, agent_name, profile, reason, status, created_at, decided_at, decided_by`

func scanRequest(row interface {
	Scan(dest ...any) error
}) (Request, error) {
	var r Request
	err := row.Scan(&r.ID, &r.AgentID, &r.AgentName, &r.Profile, &r.Reason, &r.Status,
		&r.CreatedAt, &r.DecidedAt, &r.DecidedBy)
	return r, err
}

// CreateRequest records a new pending privilege request.
func (s *PGStore) CreateRequest(ctx context.Context, r Request) (Request, error) {
	return scanRequest(s.pool.QueryRow(ctx, `
		INSERT INTO privilege_requests (agent_id, agent_name, profile, reason)
		VALUES ($1,$2,$3,$4)
		RETURNING `+requestCols,
		r.AgentID, r.AgentName, r.Profile, r.Reason))
}

// ListPendingRequests returns undecided requests, newest first.
func (s *PGStore) ListPendingRequests(ctx context.Context) ([]Request, error) {
	return s.list(ctx, `SELECT `+requestCols+` FROM privilege_requests WHERE status=$1 ORDER BY created_at DESC`, StatusPending)
}

// ListRequests returns all requests, newest first.
func (s *PGStore) ListRequests(ctx context.Context) ([]Request, error) {
	return s.list(ctx, `SELECT `+requestCols+` FROM privilege_requests ORDER BY created_at DESC`)
}

// GetRequest returns a single request by id.
func (s *PGStore) GetRequest(ctx context.Context, id int64) (Request, error) {
	return scanRequest(s.pool.QueryRow(ctx, `SELECT `+requestCols+` FROM privilege_requests WHERE id=$1`, id))
}

// DecideRequest marks a request approved/denied with the decider's name.
func (s *PGStore) DecideRequest(ctx context.Context, id int64, status, decidedBy string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE privilege_requests SET status=$2, decided_by=$3, decided_at=now() WHERE id=$1`,
		id, status, decidedBy)
	return err
}

// --- profiles ---

const profileCols = `id, name, display_name, description, spec, created_at, updated_at`

func scanProfile(row interface {
	Scan(dest ...any) error
}) (Profile, error) {
	var p Profile
	var spec []byte
	if err := row.Scan(&p.ID, &p.Name, &p.DisplayName, &p.Description, &spec, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return Profile{}, err
	}
	if len(spec) > 0 {
		if err := json.Unmarshal(spec, &p.Spec); err != nil {
			return Profile{}, err
		}
	}
	return p, nil
}

// CreateProfile inserts a reusable named access profile.
func (s *PGStore) CreateProfile(ctx context.Context, p Profile) (Profile, error) {
	spec, err := json.Marshal(p.Spec)
	if err != nil {
		return Profile{}, err
	}
	return scanProfile(s.pool.QueryRow(ctx, `
		INSERT INTO privilege_profiles (name, display_name, description, spec)
		VALUES ($1,$2,$3,$4)
		RETURNING `+profileCols,
		p.Name, p.DisplayName, p.Description, spec))
}

// ListProfiles returns all profiles ordered by name.
func (s *PGStore) ListProfiles(ctx context.Context) ([]Profile, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+profileCols+` FROM privilege_profiles ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Profile, 0)
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetProfile fetches a profile by id.
func (s *PGStore) GetProfile(ctx context.Context, id int64) (Profile, error) {
	return scanProfile(s.pool.QueryRow(ctx, `SELECT `+profileCols+` FROM privilege_profiles WHERE id=$1`, id))
}

// GetProfileByName fetches a profile by its unique name.
func (s *PGStore) GetProfileByName(ctx context.Context, name string) (Profile, error) {
	return scanProfile(s.pool.QueryRow(ctx, `SELECT `+profileCols+` FROM privilege_profiles WHERE name=$1`, name))
}

// DeleteProfile removes a profile (cascade removes its grants).
func (s *PGStore) DeleteProfile(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM privilege_profiles WHERE id=$1`, id)
	return err
}

// --- grants ---

// Grant records that an agent holds a profile. Idempotent on (agent, profile):
// a repeat grant refreshes granted_by/granted_at and returns the row.
func (s *PGStore) Grant(ctx context.Context, agentID, profileID int64, grantedBy string) (Grant, error) {
	var g Grant
	err := s.pool.QueryRow(ctx, `
		INSERT INTO agent_privileges (agent_id, profile_id, granted_by)
		VALUES ($1,$2,$3)
		ON CONFLICT (agent_id, profile_id) DO UPDATE SET granted_by=EXCLUDED.granted_by, granted_at=now()
		RETURNING id, agent_id, profile_id, granted_by, granted_at`,
		agentID, profileID, grantedBy).
		Scan(&g.ID, &g.AgentID, &g.ProfileID, &g.GrantedBy, &g.GrantedAt)
	return g, err
}

// Revoke removes a grant by agent+profile (no error if absent).
func (s *PGStore) Revoke(ctx context.Context, agentID, profileID int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM agent_privileges WHERE agent_id=$1 AND profile_id=$2`, agentID, profileID)
	return err
}

// ListGrantsForAgent returns the profiles granted to an agent, with profile name
// and the joined profile spec (env + kubeconfig + RBAC), so callers resolving an
// agent's effective access don't re-fetch each profile.
func (s *PGStore) ListGrantsForAgent(ctx context.Context, agentID int64) ([]Grant, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT ap.id, ap.agent_id, ap.profile_id, pp.name, pp.spec, ap.granted_by, ap.granted_at
		FROM agent_privileges ap JOIN privilege_profiles pp ON pp.id = ap.profile_id
		WHERE ap.agent_id=$1 ORDER BY pp.name`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Grant, 0)
	for rows.Next() {
		var g Grant
		var spec []byte
		if err := rows.Scan(&g.ID, &g.AgentID, &g.ProfileID, &g.ProfileName, &spec, &g.GrantedBy, &g.GrantedAt); err != nil {
			return nil, err
		}
		if len(spec) > 0 {
			if err := json.Unmarshal(spec, &g.Spec); err != nil {
				return nil, err
			}
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// DeleteResolvedRequestsOlderThan deletes decided (approved/denied) privilege
// requests created before cutoff. Pending requests are kept regardless of age.
func (s *PGStore) DeleteResolvedRequestsOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM privilege_requests WHERE status <> $1 AND created_at < $2`,
		StatusPending, cutoff)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (s *PGStore) list(ctx context.Context, sql string, args ...any) ([]Request, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Request, 0)
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// compile-time assertion.
var _ Store = (*PGStore)(nil)
