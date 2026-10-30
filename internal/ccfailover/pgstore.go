package ccfailover

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PGStore is the Postgres [Store] (table cc_account_failovers, migration 0064).
type PGStore struct{ pool *pgxpool.Pool }

// NewPG constructs the Postgres store.
func NewPG(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

// Record inserts one rebind, owned by the agent's owner.
//
// ⚠ owner_id IS THE AGENT'S, READ IN THE INSERT, NOT THE CALLER'S: a failover
// runs under whichever principal the failing turn had (the kickoff deliverer's
// is the system's, which names no user), and the row belongs to the agent's
// owner whoever reported the failure.
func (s *PGStore) Record(ctx context.Context, e Event) (Event, error) {
	var resets *time.Time
	if !e.ResetsAt.IsZero() {
		resets = &e.ResetsAt
	}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO cc_account_failovers (owner_id, agent_id, agent_name, from_account, to_account, reason, resets_at)
		SELECT owner_id, $1, $2, $3, $4, $5, $6 FROM agents WHERE id = $1
		RETURNING id, owner_id, created_at`,
		e.AgentID, e.AgentName, e.From, e.To, e.Reason, resets).Scan(&e.ID, &e.OwnerID, &e.At)
	if err != nil {
		return Event{}, fmt.Errorf("ccfailover: record the rebind of agent %d: %w", e.AgentID, err)
	}
	return e, nil
}

// ForAgent is agentID's rebinds, newest first, at most limit (1-100).
func (s *PGStore) ForAgent(ctx context.Context, agentID int64, limit int) ([]Event, error) {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, owner_id, agent_id, agent_name, from_account, to_account, reason, resets_at, created_at
		FROM cc_account_failovers WHERE agent_id = $1 ORDER BY id DESC LIMIT $2`, agentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var resets *time.Time
		if err := rows.Scan(&e.ID, &e.OwnerID, &e.AgentID, &e.AgentName, &e.From, &e.To, &e.Reason, &resets, &e.At); err != nil {
			return nil, err
		}
		if resets != nil {
			e.ResetsAt = *resets
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
