package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ZacxDev/muster/internal/officers"
)

// --- the owner's administration of the store (internal/api officer_admin.go,
// `muster officer`) ------------------------------------------------------------
//
// Every method takes the owner explicitly AND runs under the caller's
// principal, so row-level security (0118) and the WHERE clause agree: one
// owner can neither read nor write another's types, officers or items.

// TypeInfo is an officer type as the admin API lists it (Charter only from
// GetType).
type TypeInfo struct {
	Key            string          `json:"key"`
	Name           string          `json:"name"`
	Charter        string          `json:"charter,omitempty"`
	CharterVersion int             `json:"charter_version"`
	Policy         json.RawMessage `json:"policy,omitempty"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

// ErrNotFound is a type, officer or item the owner does not have.
var ErrNotFound = errors.New("builtin: not found")

// ErrInUse is a type an officer still names, or an officer that still has
// items or schedules: refused rather than cascaded.
var ErrInUse = errors.New("builtin: in use")

// ErrInvalid is a value the store's CHECKs refuse (its text says which).
var ErrInvalid = errors.New("builtin: invalid")

// classify maps a Postgres refusal to the store's errors: a CHECK or a
// malformed value is ErrInvalid; a foreign key naming a parent that is not
// there (an officer whose type is missing, an item for an officer not
// defined) is ErrNotFound; one refusing a delete a child still names is
// ErrInUse. Anything else is returned as it is.
func classify(err error) error {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		return err
	}
	switch pg.Code {
	case "23514", "22P02", "22007", "22008", "23502":
		return fmt.Errorf("%w: %s", ErrInvalid, pg.Message)
	case "23503":
		if strings.Contains(pg.Detail, "is still referenced") {
			return fmt.Errorf("%w: %s", ErrInUse, pg.Detail)
		}
		return fmt.Errorf("%w: %s", ErrNotFound, pg.Detail)
	}
	return err
}

// ListTypes is owner's officer types, by key (no charter).
func (s *Store) ListTypes(ctx context.Context, owner int64) ([]TypeInfo, error) {
	rows, err := s.pool.Query(ctx, `SELECT key, name, charter_version, updated_at FROM officer_types WHERE owner_id = $1 ORDER BY key`, owner)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (TypeInfo, error) {
		var t TypeInfo
		err := r.Scan(&t.Key, &t.Name, &t.CharterVersion, &t.UpdatedAt)
		return t, err
	})
	if out == nil {
		out = []TypeInfo{}
	}
	return out, err
}

// GetType is owner's type key, charter and policy included.
func (s *Store) GetType(ctx context.Context, owner int64, key string) (TypeInfo, error) {
	var t TypeInfo
	err := s.pool.QueryRow(ctx, `SELECT key, name, charter, charter_version, policy, updated_at FROM officer_types WHERE owner_id = $1 AND key = $2`,
		owner, key).Scan(&t.Key, &t.Name, &t.Charter, &t.CharterVersion, &t.Policy, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return TypeInfo{}, ErrNotFound
	}
	return t, err
}

// DeleteType removes owner's type key; ErrInUse while an officer names it.
func (s *Store) DeleteType(ctx context.Context, owner int64, key string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM officer_types WHERE owner_id = $1 AND key = $2`, owner, key)
	if err != nil {
		return classify(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DefInfo is an officer the store defines.
type DefInfo struct {
	Officer    string    `json:"officer"`
	TypeKey    string    `json:"type_key"`
	Enabled    bool      `json:"enabled"`
	Mode       string    `json:"mode"`
	EscalateTo []int64   `json:"escalate_to"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// ListDefs is owner's officers, by id.
func (s *Store) ListDefs(ctx context.Context, owner int64) ([]DefInfo, error) {
	rows, err := s.pool.Query(ctx, `SELECT officer, type_key, enabled, mode, escalate_to, updated_at FROM officer_defs WHERE owner_id = $1 ORDER BY officer`, owner)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (DefInfo, error) {
		var d DefInfo
		err := r.Scan(&d.Officer, &d.TypeKey, &d.Enabled, &d.Mode, &d.EscalateTo, &d.UpdatedAt)
		return d, err
	})
	if out == nil {
		out = []DefInfo{}
	}
	return out, err
}

// GetDef is owner's officer.
func (s *Store) GetDef(ctx context.Context, owner int64, officer string) (DefInfo, error) {
	var d DefInfo
	err := s.pool.QueryRow(ctx, `SELECT officer, type_key, enabled, mode, escalate_to, updated_at FROM officer_defs WHERE owner_id = $1 AND officer = $2`,
		owner, officer).Scan(&d.Officer, &d.TypeKey, &d.Enabled, &d.Mode, &d.EscalateTo, &d.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return DefInfo{}, ErrNotFound
	}
	return d, err
}

// DeleteDef removes owner's officer; ErrInUse while it has items or
// schedules (resolve or keep them — they are its history).
func (s *Store) DeleteDef(ctx context.Context, owner int64, officer string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM officer_defs WHERE owner_id = $1 AND officer = $2`, owner, officer)
	if err != nil {
		return classify(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListItems is owner's officer's items in status ("" for every status),
// newest first, at most limit (1..500).
func (s *Store) ListItems(ctx context.Context, owner int64, officer, status string, limit int) ([]json.RawMessage, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `SELECT `+itemCols+` FROM officer_items WHERE owner_id = $1 AND officer = $2 AND ($3 = '' OR status = $3)
		ORDER BY id DESC LIMIT $4`, owner, officer, status, limit)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (itemRow, error) { return scanItem(r) })
	if err != nil {
		return nil, err
	}
	out := make([]json.RawMessage, 0, len(list))
	for _, it := range list {
		raw, err := json.Marshal(it)
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return out, nil
}

// ResolveItem is the OWNER finishing owner's officer's item id by hand:
// outcome done, no_action or escalated, with summary. Refused (an
// *officers.HQError, 409 held) while an instance holds it under a live lease,
// and 410 gone when it is already finished. An escalation the owner makes
// notifies nobody: the owner is who it would notify.
func (s *Store) ResolveItem(ctx context.Context, owner int64, officer string, id int64, outcome, summary string, now time.Time) (json.RawMessage, error) {
	status := officers.StatusDone
	switch outcome {
	case "done", "no_action":
	case "escalated":
		status = officers.StatusEscalated
	default:
		return nil, fmt.Errorf("%w: outcome must be done, no_action or escalated", ErrInvalid)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	it, err := scanItem(tx.QueryRow(ctx, `SELECT `+itemCols+` FROM officer_items WHERE owner_id = $1 AND officer = $2 AND id = $3 FOR UPDATE`, owner, officer, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	switch {
	case it.Status == officers.StatusDone || it.Status == officers.StatusEscalated:
		return nil, refuse(http.StatusGone, "gone", "the item is finished", nil)
	case it.live(now):
		return nil, refuse(http.StatusConflict, "held", "an instance holds the item under a live lease: wait for it, or switch the officer off", map[string]any{"held_by": it.ClaimedBy})
	}
	it, err = scanItem(tx.QueryRow(ctx, `UPDATE officer_items SET status = $2, outcome = $3, summary = $4, claimed_by = NULL, lease_expires_at = NULL, updated_at = $5
		WHERE id = $1 RETURNING `+itemCols, it.ID, status, outcome, summary, now))
	if err != nil {
		return nil, classify(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return json.Marshal(it)
}

// PutTypeReturning is PutType, answering the stored type (its charter
// version included).
func (s *Store) PutTypeReturning(ctx context.Context, owner int64, t Type) (TypeInfo, error) {
	if err := s.PutType(ctx, owner, t); err != nil {
		return TypeInfo{}, classify(err)
	}
	return s.GetType(ctx, owner, t.Key)
}

// PutDefReturning is PutDef, answering the stored officer; ErrNotFound when
// its type (the id's part before the colon) is not one of owner's.
func (s *Store) PutDefReturning(ctx context.Context, owner int64, d Def) (DefInfo, error) {
	if err := s.PutDef(ctx, owner, d); err != nil {
		return DefInfo{}, classify(err)
	}
	return s.GetDef(ctx, owner, d.Officer)
}

// EnqueueChecked is Enqueue with the store's refusals classified (an officer
// the owner does not define is ErrNotFound, a bad field ErrInvalid).
func (s *Store) EnqueueChecked(ctx context.Context, owner int64, n NewItem, now time.Time) (int64, bool, error) {
	id, created, err := s.Enqueue(ctx, owner, n, now)
	return id, created, classify(err)
}
