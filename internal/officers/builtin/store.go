// Package builtin is the built-in officer backend: officer-backend/v1
// (officers.Backend) kept in muster's own Postgres (migration 0118), so a
// deployment can run officers with no external HQ
// (docs/design/officers-first-class.md).
//
// 🔴 NOT WIRED YET. MUSTER_OFFICERS refuses "backend": "builtin" until the
// wiring slice lands; today this package is reached only by its tests and
// the conformance suite (internal/officers/backendtest).
package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the built-in backend's tables. Every call runs under row-level
// security, so ctx must act as the officer's owner (or the operator).
type Store struct{ pool *pgxpool.Pool }

// NewStore is the store over pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Type is an officer type: its charter (versioned) and policy.
type Type struct {
	Key, Name, Charter string
	// Policy is the type's policy object; nil is {}.
	Policy json.RawMessage
}

// PutType creates or replaces type t for owner. A changed charter bumps
// charter_version; an unchanged one keeps it.
func (s *Store) PutType(ctx context.Context, owner int64, t Type) error {
	policy := t.Policy
	if len(policy) == 0 {
		policy = json.RawMessage(`{}`)
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO officer_types (owner_id, key, name, charter, policy) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (owner_id, key) DO UPDATE SET name = EXCLUDED.name, policy = EXCLUDED.policy, updated_at = now(),
			charter = EXCLUDED.charter,
			charter_version = officer_types.charter_version + CASE WHEN officer_types.charter = EXCLUDED.charter THEN 0 ELSE 1 END`,
		owner, t.Key, t.Name, t.Charter, policy)
	return err
}

// Def is an officer the built-in backend serves.
type Def struct {
	Officer string
	Enabled bool
	// Mode is live or shadow ("" is live).
	Mode string
	// EscalateTo are the muster users its escalations reach, besides its owner.
	EscalateTo []int64
}

// PutDef creates or replaces officer d for owner; its type is the id's part
// before the colon and must already exist (PutType).
func (s *Store) PutDef(ctx context.Context, owner int64, d Def) error {
	mode := d.Mode
	if mode == "" {
		mode = "live"
	}
	esc := d.EscalateTo
	if esc == nil {
		esc = []int64{}
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO officer_defs (owner_id, officer, type_key, enabled, mode, escalate_to)
		VALUES ($1, $2, split_part($2, ':', 1), $3, $4, $5)
		ON CONFLICT (owner_id, officer) DO UPDATE SET enabled = EXCLUDED.enabled, mode = EXCLUDED.mode,
			escalate_to = EXCLUDED.escalate_to, updated_at = now()`,
		owner, d.Officer, d.Enabled, mode, esc)
	return err
}

// SetEnabled flips officer's switch.
func (s *Store) SetEnabled(ctx context.Context, owner int64, officer string, on bool) error {
	tag, err := s.pool.Exec(ctx, `UPDATE officer_defs SET enabled = $3, updated_at = now() WHERE owner_id = $1 AND officer = $2`, owner, officer, on)
	if err == nil && tag.RowsAffected() == 0 {
		return fmt.Errorf("builtin: no officer %s", officer)
	}
	return err
}

// NewItem is an item to enqueue.
type NewItem struct {
	Officer, Kind, SubjectKey string
	// Payload is a JSON object; nil is {}.
	Payload json.RawMessage
	// Tier is routine or judgement ("" is judgement); Priority urgent, high,
	// normal or low ("" is normal).
	Tier, Priority string
	NotBefore      *time.Time
}

// Enqueue adds an open item, or — when the officer already has a LIVE item
// (open, claimed or waiting) of that kind and subject — returns that one
// (created false): the contract's dedupe, kept by a partial unique index.
func (s *Store) Enqueue(ctx context.Context, owner int64, n NewItem, now time.Time) (id int64, created bool, err error) {
	return s.enqueue(ctx, owner, n, now, false)
}

func (s *Store) enqueue(ctx context.Context, owner int64, n NewItem, now time.Time, retried bool) (id int64, created bool, err error) {
	payload := n.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	tier, prio := n.Tier, n.Priority
	if tier == "" {
		tier = "judgement"
	}
	if prio == "" {
		prio = "normal"
	}
	err = s.pool.QueryRow(ctx, `INSERT INTO officer_items (owner_id, officer, kind, subject_key, payload, tier, priority, not_before, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9)
		ON CONFLICT (owner_id, officer, kind, subject_key) WHERE status IN ('open', 'claimed', 'waiting') DO NOTHING
		RETURNING id`, owner, n.Officer, n.Kind, n.SubjectKey, payload, tier, prio, n.NotBefore, now).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, false, err
	}
	err = s.pool.QueryRow(ctx, `SELECT id FROM officer_items WHERE owner_id = $1 AND officer = $2 AND kind = $3 AND subject_key = $4
		AND status IN ('open', 'claimed', 'waiting')`, owner, n.Officer, n.Kind, n.SubjectKey).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) && !retried {
		// the live item finished between the insert and the read: the
		// subject is free again, so enqueue once more
		return s.enqueue(ctx, owner, n, now, true)
	}
	return id, false, err
}

// SetBlocked marks item id blocked (by unfinished work) or not.
func (s *Store) SetBlocked(ctx context.Context, owner int64, id int64, blocked bool) error {
	_, err := s.pool.Exec(ctx, `UPDATE officer_items SET blocked = $3 WHERE owner_id = $1 AND id = $2`, owner, id, blocked)
	return err
}
