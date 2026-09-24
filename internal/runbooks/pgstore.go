package runbooks

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PGStore is the Postgres-backed runbooks Store.
type PGStore struct{ pool *pgxpool.Pool }

// NewPG constructs a Postgres-backed runbooks Store.
func NewPG(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

const runbookCols = `id, name, display_name, description, spec, created_at, updated_at`

func scanRunbook(row interface {
	Scan(dest ...any) error
}) (Runbook, error) {
	var rb Runbook
	var spec []byte
	if err := row.Scan(&rb.ID, &rb.Name, &rb.DisplayName, &rb.Description, &spec, &rb.CreatedAt, &rb.UpdatedAt); err != nil {
		return Runbook{}, err
	}
	if len(spec) > 0 {
		if err := json.Unmarshal(spec, &rb.Spec); err != nil {
			return Runbook{}, err
		}
	}
	return rb, nil
}

// CreateRunbook inserts a reusable named dispatch template.
func (s *PGStore) CreateRunbook(ctx context.Context, rb Runbook) (Runbook, error) {
	spec, err := json.Marshal(rb.Spec)
	if err != nil {
		return Runbook{}, err
	}
	return scanRunbook(s.pool.QueryRow(ctx, `
		INSERT INTO runbooks (name, display_name, description, spec)
		VALUES ($1,$2,$3,$4)
		RETURNING `+runbookCols,
		rb.Name, rb.DisplayName, rb.Description, spec))
}

// ListRunbooks returns all runbooks ordered by name.
func (s *PGStore) ListRunbooks(ctx context.Context) ([]Runbook, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+runbookCols+` FROM runbooks ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Runbook, 0)
	for rows.Next() {
		rb, err := scanRunbook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rb)
	}
	return out, rows.Err()
}

// GetRunbook fetches a runbook by id.
func (s *PGStore) GetRunbook(ctx context.Context, id int64) (Runbook, error) {
	return scanRunbook(s.pool.QueryRow(ctx, `SELECT `+runbookCols+` FROM runbooks WHERE id=$1`, id))
}

// GetRunbookByName fetches a runbook by its unique name.
func (s *PGStore) GetRunbookByName(ctx context.Context, name string) (Runbook, error) {
	return scanRunbook(s.pool.QueryRow(ctx, `SELECT `+runbookCols+` FROM runbooks WHERE name=$1`, name))
}

// DeleteRunbook removes a runbook (its runs keep their snapshot; runbook_id is
// set null by the FK).
func (s *PGStore) DeleteRunbook(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM runbooks WHERE id=$1`, id)
	return err
}

const runCols = `id, runbook_id, runbook_name, agent_id, note_id, params, rendered_body, created_at`

func scanRun(row interface {
	Scan(dest ...any) error
}) (Run, error) {
	var r Run
	var params []byte
	if err := row.Scan(&r.ID, &r.RunbookID, &r.RunbookName, &r.AgentID, &r.NoteID, &params, &r.RenderedBody, &r.CreatedAt); err != nil {
		return Run{}, err
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &r.Params); err != nil {
			return Run{}, err
		}
	}
	return r, nil
}

// CreateRun records a dispatch audit row.
func (s *PGStore) CreateRun(ctx context.Context, r Run) (Run, error) {
	params, err := json.Marshal(r.Params)
	if err != nil {
		return Run{}, err
	}
	return scanRun(s.pool.QueryRow(ctx, `
		INSERT INTO runbook_runs (runbook_id, runbook_name, agent_id, note_id, params, rendered_body)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING `+runCols,
		r.RunbookID, r.RunbookName, r.AgentID, r.NoteID, params, r.RenderedBody))
}

// ListRunsForRunbook returns a runbook's dispatch history, newest first.
func (s *PGStore) ListRunsForRunbook(ctx context.Context, runbookID int64) ([]Run, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+runCols+` FROM runbook_runs WHERE runbook_id=$1 ORDER BY created_at DESC`, runbookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Run, 0)
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LatestRunForAgent returns the newest run that dispatched agentID. The bool is
// false when no run exists for the agent (an ad-hoc/non-runbook dispatch).
func (s *PGStore) LatestRunForAgent(ctx context.Context, agentID int64) (Run, bool, error) {
	r, err := scanRun(s.pool.QueryRow(ctx,
		`SELECT `+runCols+` FROM runbook_runs WHERE agent_id=$1 ORDER BY id DESC LIMIT 1`, agentID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Run{}, false, nil
		}
		return Run{}, false, err
	}
	return r, true, nil
}

// RunStatsForRunbooks returns the run count and most-recent run time per runbook
// id in one query (replacing a per-runbook ListRunsForRunbook just to count).
// Runbooks with no runs are simply absent from the map. An empty id list skips
// the query.
func (s *PGStore) RunStatsForRunbooks(ctx context.Context, runbookIDs []int64) (map[int64]RunStats, error) {
	out := make(map[int64]RunStats, len(runbookIDs))
	if len(runbookIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT runbook_id, count(*), max(created_at)
		FROM runbook_runs WHERE runbook_id = ANY($1) GROUP BY runbook_id`, runbookIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var st RunStats
		if err := rows.Scan(&id, &st.Count, &st.LastRunAt); err != nil {
			return nil, err
		}
		out[id] = st
	}
	return out, rows.Err()
}

// DeleteRunsOlderThan removes runbook_runs created before cutoff.
func (s *PGStore) DeleteRunsOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM runbook_runs WHERE created_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// compile-time assertion.
var _ Store = (*PGStore)(nil)
