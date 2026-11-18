package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ZacxDev/muster/internal/officers"
)

// --- DB-declared officers (migration 0136; internal/api officer_declared.go) --
//
// An officer declared in the database is one officer_defs row carrying its
// runtime fields, beside its type (the charter) — written together, in ONE
// transaction, so the user's one record is never half there. Every method
// takes the owner explicitly AND runs under the caller's principal, as the
// rest of the admin side does.

// declCols are the declared columns, in DeclaredRow's scan order.
const declCols = `d.owner_id, d.officer, d.type_key, d.enabled, d.mode, d.escalate_to, d.updated_at,
	coalesce(d.runtime, ''), d.host, coalesce(d.repo, ''), d.connectors, d.read_repos, coalesce(d.max_instances, 0),
	coalesce(d.idle_after_seconds, 0), coalesce(d.poll_seconds, 0), d.model, coalesce(d.instance_slug, ''), t.name,
	d.auto_host, d.auto_host_at`

// DeclaredRow is a DB-declared officer as stored.
type DeclaredRow struct {
	Owner int64
	DefInfo
	// Fields is the runtime declaration.
	Fields officers.Declaration
	// TypeName is its type's display name.
	TypeName string
	// AutoHost is where a "runtime": "auto" officer is placed on a host ("" =
	// not on a host), and AutoHostAt when that host last qualified (zero with
	// it). THE DATABASE'S PLACEMENT (round 2's third finding): every replica
	// reads it here, and moves it only by SetAutoHost's compare-and-swap.
	AutoHost   string
	AutoHostAt time.Time
}

func scanDeclared(r pgx.Row) (DeclaredRow, error) {
	var d DeclaredRow
	var idle, poll int
	var autoAt *time.Time
	err := r.Scan(&d.Owner, &d.Officer, &d.TypeKey, &d.Enabled, &d.Mode, &d.EscalateTo, &d.UpdatedAt,
		&d.Fields.Runtime, &d.Fields.Host, &d.Fields.Repo, &d.Fields.Connectors, &d.Fields.ReadRepos, &d.Fields.MaxInstances,
		&idle, &poll, &d.Fields.Model, &d.Fields.InstanceSlug, &d.TypeName, &d.AutoHost, &autoAt)
	if autoAt != nil {
		d.AutoHostAt = *autoAt
	}
	d.Fields.ID, d.Fields.Owner = d.Officer, d.Owner
	d.Fields.IdleAfter = time.Duration(idle) * time.Second
	d.Fields.PollInterval = time.Duration(poll) * time.Second
	return d, err
}

// ListDeclared is every DB-declared officer the caller's principal reads —
// every owner's in the system scope (the roster loader), the caller's own
// otherwise — by owner then officer.
func (s *Store) ListDeclared(ctx context.Context) ([]DeclaredRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+declCols+` FROM officer_defs d
		JOIN officer_types t ON t.owner_id = d.owner_id AND t.key = d.type_key
		WHERE d.runtime IS NOT NULL ORDER BY d.owner_id, d.officer`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (DeclaredRow, error) { return scanDeclared(r) })
}

// AnyDeclared reports whether the caller's principal reads any DB-declared
// officer: one probe of the partial unique index on instance_slug (a row
// declares a runtime iff it has a slug, officer_defs_declared_check), so a
// muster with none declared pays no list on the roster refresh.
func (s *Store) AnyDeclared(ctx context.Context) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM officer_defs WHERE instance_slug IS NOT NULL)`).Scan(&ok)
	return ok, err
}

// GetDeclared is owner's DB-declared officer; ErrNotFound when owner has no
// such officer, ErrNotDeclared when its row declares no runtime (a def an env
// officer runs on).
func (s *Store) GetDeclared(ctx context.Context, owner int64, officer string) (DeclaredRow, error) {
	return getDeclared(ctx, s.pool, owner, officer, false)
}

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func getDeclared(ctx context.Context, q queryRower, owner int64, officer string, lock bool) (DeclaredRow, error) {
	forUpdate := ""
	if lock {
		forUpdate = " FOR UPDATE OF d"
	}
	d, err := scanDeclared(q.QueryRow(ctx, `SELECT `+declCols+` FROM officer_defs d
		JOIN officer_types t ON t.owner_id = d.owner_id AND t.key = d.type_key
		WHERE d.owner_id = $1 AND d.officer = $2`+forUpdate, owner, officer))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return DeclaredRow{}, ErrNotFound
	case err != nil:
		return DeclaredRow{}, err
	case d.Fields.Runtime == "":
		return DeclaredRow{}, ErrNotDeclared
	}
	return d, nil
}

// ErrNotDeclared is an officer whose row declares no runtime: a def an env
// (MUSTER_OFFICERS) officer runs on, which the declared-officer API does not
// manage.
var ErrNotDeclared = errors.New("builtin: not declared in the database")

// ErrExists is a create of an officer owner already defines.
var ErrExists = errors.New("builtin: the officer already exists")

// ErrNoSlug is a create every instance-slug candidate of which is taken.
var ErrNoSlug = errors.New("builtin: no free instance slug")

// DeclaredWrite is a create or a whole replace: the type (Charter "" keeps an
// existing type's charter), the def, the runtime declaration.
type DeclaredWrite struct {
	TypeName string
	Charter  string
	// Policy is the type's policy; nil keeps an existing type's ({} for a new
	// one).
	Policy     json.RawMessage
	Enabled    bool
	Mode       string
	EscalateTo []int64
	Fields     officers.Declaration
	// KeepEnabled and KeepMode (UpdateDeclared only): leave the stored switch,
	// or mode, as it is rather than writing Enabled, or Mode — so a page's
	// Edit, which read them a moment ago, never undoes a Pause or a mode
	// switch made since (round 0's 🟢: the writes do not undo each other).
	KeepEnabled, KeepMode bool
}

func (w DeclaredWrite) defaults() (mode string, esc []int64) {
	mode, esc = w.Mode, w.EscalateTo
	if mode == "" {
		mode = "live"
	}
	if esc == nil {
		esc = []int64{}
	}
	return mode, esc
}

func nullInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func strs(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// putTypeTx updates (or creates) owner's type key inside tx: name always, the
// charter and policy only when given (a charter change bumps its version).
// UpdateDeclared calls it on the officer's own type only.
func putTypeTx(ctx context.Context, tx pgx.Tx, owner int64, key string, w DeclaredWrite) error {
	policy := w.Policy
	_, err := tx.Exec(ctx, `INSERT INTO officer_types (owner_id, key, name, charter, policy) VALUES ($1, $2, $3, $4, coalesce($5::jsonb, '{}'::jsonb))
		ON CONFLICT (owner_id, key) DO UPDATE SET name = EXCLUDED.name, updated_at = now(),
			policy = CASE WHEN $5::jsonb IS NULL THEN officer_types.policy ELSE EXCLUDED.policy END,
			charter = CASE WHEN $4 = '' THEN officer_types.charter ELSE EXCLUDED.charter END,
			charter_version = officer_types.charter_version + CASE WHEN $4 = '' OR officer_types.charter = EXCLUDED.charter THEN 0 ELSE 1 END`,
		owner, key, w.TypeName, w.Charter, nullJSON(policy))
	return err
}

func nullJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}

// CreateDeclared writes owner's new officer w.Fields.ID — its type, its def
// and its runtime declaration — in ONE transaction, with the first of slugs
// no other officer holds (the global unique index decides, across owners).
// ErrExists when owner already defines that officer (declared or not);
// ErrNoSlug when every candidate is taken.
//
// 🔴 ITS TYPE IS ITS OWN (round 0's F5): the type is created under the
// officer's instance slug, never the id's part before the colon, so its
// charter is private to it — creating DB officer scout:main no longer rewrites
// type scout, the charter an env officer scout:ops runs on. A candidate whose
// key owner already has a type under is passed over like a taken slug: the
// create never adopts or rewrites a type it did not make.
func (s *Store) CreateDeclared(ctx context.Context, owner int64, w DeclaredWrite, slugs []string) (DeclaredRow, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return DeclaredRow{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	id := w.Fields.ID
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM officer_defs WHERE owner_id = $1 AND officer = $2)`, owner, id).Scan(&exists); err != nil {
		return DeclaredRow{}, classify(err)
	}
	if exists {
		return DeclaredRow{}, fmt.Errorf("%w: %s", ErrExists, id)
	}
	mode, esc := w.defaults()
	f := w.Fields
	placed := false
	for _, slug := range slugs {
		sp, err := tx.Begin(ctx)
		if err != nil {
			return DeclaredRow{}, err
		}
		_, err = sp.Exec(ctx, `INSERT INTO officer_types (owner_id, key, name, charter, policy) VALUES ($1, $2, $3, $4, coalesce($5::jsonb, '{}'::jsonb))`,
			owner, slug, w.TypeName, w.Charter, nullJSON(w.Policy))
		if err == nil {
			_, err = sp.Exec(ctx, `INSERT INTO officer_defs (owner_id, officer, type_key, enabled, mode, escalate_to,
					runtime, host, repo, connectors, read_repos, max_instances, idle_after_seconds, poll_seconds, model, instance_slug)
				VALUES ($1, $2, $15, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
				owner, id, w.Enabled, mode, esc, f.Runtime, f.Host, nullStr(f.Repo), strs(f.Connectors), strs(f.ReadRepos),
				nullInt(f.MaxInstances), nullInt(int(f.IdleAfter/time.Second)), nullInt(int(f.PollInterval/time.Second)), f.Model, slug)
		}
		var pg *pgconn.PgError
		switch {
		case err == nil:
			if err := sp.Commit(ctx); err != nil {
				return DeclaredRow{}, err
			}
			placed = true
		case errors.As(err, &pg) && pg.Code == "23505" && (pg.ConstraintName == "officer_defs_instance_slug" || pg.ConstraintName == "officer_types_one_key"):
			_ = sp.Rollback(ctx)
			continue
		case errors.As(err, &pg) && pg.Code == "23505":
			_ = sp.Rollback(ctx)
			return DeclaredRow{}, fmt.Errorf("%w: %s", ErrExists, id)
		default:
			_ = sp.Rollback(ctx)
			return DeclaredRow{}, classify(err)
		}
		break
	}
	if !placed {
		return DeclaredRow{}, ErrNoSlug
	}
	d, err := getDeclared(ctx, tx, owner, id, false)
	if err != nil {
		return DeclaredRow{}, err
	}
	return d, tx.Commit(ctx)
}

// ErrTypeShared is a replace of an officer whose type is not its own: a row
// written before round 0's F5 (on the type its id's prefix names, which an
// env officer may run on), or one whose own type another def names (a def put
// on the slug's key by hand). Its name, charter and policy are left as they
// are, and the replace is refused, rather than changing another officer's
// charter.
var ErrTypeShared = errors.New("builtin: another officer runs on this officer's type")

// UpdateDeclared replaces owner's DB-declared officer's def and runtime
// declaration with w (and its OWN type's name, charter and policy as
// putTypeTx does), in one transaction; its instance slug and type key never
// change. ErrNotFound / ErrNotDeclared as GetDeclared; ErrTypeShared when
// another officer names its type.
func (s *Store) UpdateDeclared(ctx context.Context, owner int64, officer string, w DeclaredWrite) (DeclaredRow, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return DeclaredRow{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	cur, err := getDeclared(ctx, tx, owner, officer, true)
	if err != nil {
		return DeclaredRow{}, err
	}
	// 🔴 ONLY ITS OWN TYPE (F5): the type keyed by its slug, and only while
	// no other def of owner's names it. A row on any other type (one written
	// before F5, on the type its id's prefix names) shares that type with
	// whatever else runs on it — an env officer, which has no def to count —
	// so it is refused, not written through.
	var shared bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM officer_defs WHERE owner_id = $1 AND type_key = $2 AND officer <> $3)`,
		owner, cur.TypeKey, officer).Scan(&shared); err != nil {
		return DeclaredRow{}, classify(err)
	}
	if shared || cur.TypeKey != cur.Fields.InstanceSlug {
		return DeclaredRow{}, fmt.Errorf("%w: %s's type %s", ErrTypeShared, officer, cur.TypeKey)
	}
	if err := putTypeTx(ctx, tx, owner, cur.TypeKey, w); err != nil {
		return DeclaredRow{}, classify(err)
	}
	mode, esc := w.defaults()
	f := w.Fields
	if _, err := tx.Exec(ctx, `UPDATE officer_defs SET enabled = CASE WHEN $15 THEN enabled ELSE $3 END, mode = CASE WHEN $16 THEN mode ELSE $4 END,
			escalate_to = $5, runtime = $6, host = $7, repo = $8,
			connectors = $9, read_repos = $10, max_instances = $11, idle_after_seconds = $12, poll_seconds = $13, model = $14, updated_at = now(),
			auto_host = CASE WHEN $6 = 'auto' THEN auto_host ELSE '' END,
			auto_host_at = CASE WHEN $6 = 'auto' THEN auto_host_at END
		WHERE owner_id = $1 AND officer = $2`,
		owner, officer, w.Enabled, mode, esc, f.Runtime, f.Host, nullStr(f.Repo), strs(f.Connectors), strs(f.ReadRepos),
		nullInt(f.MaxInstances), nullInt(int(f.IdleAfter/time.Second)), nullInt(int(f.PollInterval/time.Second)), f.Model, w.KeepEnabled, w.KeepMode); err != nil {
		return DeclaredRow{}, classify(err)
	}
	d, err := getDeclared(ctx, tx, owner, officer, false)
	if err != nil {
		return DeclaredRow{}, err
	}
	return d, tx.Commit(ctx)
}

// SetAutoHost is the ONE writer of a "runtime": "auto" officer's placement
// (DeclaredRow.AutoHost): a COMPARE-AND-SWAP from prev to next, at time at,
// on owner's officer with that slug. It writes only while the stored host is
// still prev, so of two replicas that each decided a move from what they
// read, one wins and the other reads the winner's placement; prev == next
// refreshes when the host last qualified. next "" clears it. It reports
// whether it wrote. It never touches updated_at: a placement is not an edit.
func (s *Store) SetAutoHost(ctx context.Context, owner int64, officer, slug, prev, next string, at time.Time) (bool, error) {
	var when any
	if next != "" {
		when = at
	}
	tag, err := s.pool.Exec(ctx, `UPDATE officer_defs SET auto_host = $5, auto_host_at = $6
		WHERE owner_id = $1 AND officer = $2 AND instance_slug = $3 AND runtime = 'auto' AND auto_host = $4`,
		owner, officer, slug, prev, next, when)
	if err != nil {
		return false, classify(err)
	}
	return tag.RowsAffected() == 1, nil
}

// SetDeclaredEnabled is pause (on false) and resume (on true) of owner's
// DB-declared officer: its switch, which the wake loop honours (a switched-off
// officer's instances are stopped and none starts).
func (s *Store) SetDeclaredEnabled(ctx context.Context, owner int64, officer string, on bool) (DeclaredRow, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return DeclaredRow{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := getDeclared(ctx, tx, owner, officer, true); err != nil {
		return DeclaredRow{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE officer_defs SET enabled = $3, updated_at = now() WHERE owner_id = $1 AND officer = $2`, owner, officer, on); err != nil {
		return DeclaredRow{}, classify(err)
	}
	d, err := getDeclared(ctx, tx, owner, officer, false)
	if err != nil {
		return DeclaredRow{}, err
	}
	return d, tx.Commit(ctx)
}

// SetDeclaredMode is owner's DB-declared officer's mode ("live" or "shadow")
// and nothing else: the page's Shadow/Live switch, which must not rewrite the
// record it read (and so undo an Edit or a Pause made since).
func (s *Store) SetDeclaredMode(ctx context.Context, owner int64, officer, mode string) (DeclaredRow, error) {
	if mode != "live" && mode != "shadow" {
		return DeclaredRow{}, fmt.Errorf("%w: mode %q is not live or shadow", ErrInvalid, mode)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return DeclaredRow{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := getDeclared(ctx, tx, owner, officer, true); err != nil {
		return DeclaredRow{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE officer_defs SET mode = $3, updated_at = now() WHERE owner_id = $1 AND officer = $2`, owner, officer, mode); err != nil {
		return DeclaredRow{}, classify(err)
	}
	d, err := getDeclared(ctx, tx, owner, officer, false)
	if err != nil {
		return DeclaredRow{}, err
	}
	return d, tx.Commit(ctx)
}

// DeleteDeclared removes owner's DB-declared officer and everything that is
// only its — its schedules, items, contacts (their events cascade) and the
// def — and its type when no other officer of owner's names it, in one
// transaction. It answers the row as it was (its slug, for the caller's
// instance teardown).
func (s *Store) DeleteDeclared(ctx context.Context, owner int64, officer string) (DeclaredRow, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return DeclaredRow{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	d, err := getDeclared(ctx, tx, owner, officer, true)
	if err != nil {
		return DeclaredRow{}, err
	}
	for _, q := range []string{
		`DELETE FROM officer_schedules WHERE owner_id = $1 AND officer = $2`,
		`DELETE FROM officer_items WHERE owner_id = $1 AND officer = $2`,
		`DELETE FROM officer_contacts WHERE owner_id = $1 AND officer = $2`,
		`DELETE FROM officer_defs WHERE owner_id = $1 AND officer = $2`,
	} {
		if _, err := tx.Exec(ctx, q, owner, officer); err != nil {
			return DeclaredRow{}, classify(err)
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM officer_types t WHERE t.owner_id = $1 AND t.key = $2
		AND NOT EXISTS (SELECT 1 FROM officer_defs d WHERE d.owner_id = t.owner_id AND d.type_key = t.key)`, owner, d.TypeKey); err != nil {
		return DeclaredRow{}, classify(err)
	}
	return d, tx.Commit(ctx)
}

// ListDeclaredFor is owner's DB-declared officers, by id.
func (s *Store) ListDeclaredFor(ctx context.Context, owner int64) ([]DeclaredRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+declCols+` FROM officer_defs d
		JOIN officer_types t ON t.owner_id = d.owner_id AND t.key = d.type_key
		WHERE d.runtime IS NOT NULL AND d.owner_id = $1 ORDER BY d.officer`, owner)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (DeclaredRow, error) { return scanDeclared(r) })
}
