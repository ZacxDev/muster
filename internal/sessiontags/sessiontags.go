// Package sessiontags is the labels a user puts on a session (migration
// session_tags): per user, server-stored, and private — like pins and
// recents. A tag names either a dispatched agent (by id) or a mirrored tmux
// SESSION (host + session name; every window of the session shares them).
//
// 🔴 EVERY METHOD RUNS UNDER THE CALLER'S PRINCIPAL. session_tags is under FORCE
// row-level security, so a request sees, adds and removes only its own user's
// rows; there is no system method. The owner of a new row is the principal's
// user (owner_id's default), never a parameter.
//
// 🔴 ONE TAG RULE: the task tags' (internal/notes). A tag is normalized by
// notes.NormalizeTag (lower case, white space to '-'; the browser runs the same
// rule, internal/ui/js/tag-normalize.js) and must pass notes.ValidateTags'
// per-tag checks (charset, MaxTagRunes, one ':'). Because the stored tag is the
// normalized one, two spellings that differ only in case are one tag.
//
// 🔴 RESERVED NAMESPACES CARRY NO BEHAVIOUR HERE. A task's `gate:`,
// `initiative:`, `auto:` and `project:` tags route; a session's do not —
// nothing reads a session tag but its owner's own header, editor and search.
// So a session tag is never refused for its namespace (a session may carry
// two `project:` tags, which a task may not) and is drawn as a plain label.
// The grammar is otherwise the task tags', so one tag means one spelling
// everywhere.
package sessiontags

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/notes"
)

// MaxPerTarget bounds one subject's tags (a task's own cap, notes.MaxTags).
const MaxPerTarget = notes.MaxTags

// MaxTagRunes bounds one tag (notes.MaxTagRunes).
const MaxTagRunes = notes.MaxTagRunes

// Target is one subject: an agent (AgentID > 0) or a mirrored tmux session
// (Host, Session). Exactly one of the two.
type Target struct {
	AgentID int64
	Host    string
	Session string
}

// Mirror reports whether t names a mirrored session.
func (t Target) Mirror() bool { return t.AgentID == 0 }

// Valid reports whether t names exactly one subject the store can key.
func (t Target) Valid() error {
	if t.AgentID < 0 {
		return ErrBadTarget
	}
	if t.AgentID > 0 {
		if t.Host != "" || t.Session != "" {
			return ErrBadTarget
		}
		return nil
	}
	if err := mirror.ValidHost(t.Host); err != nil {
		return fmt.Errorf("%w: %v", ErrBadTarget, err)
	}
	if err := mirror.ValidSession(t.Session); err != nil {
		return fmt.Errorf("%w: %v", ErrBadTarget, err)
	}
	return nil
}

// MirrorKey is a mirrored session's key in a Set.
type MirrorKey struct{ Host, Session string }

// Set is one user's tags, by subject, each sorted.
type Set struct {
	Agents  map[int64][]string
	Mirrors map[MirrorKey][]string
}

// Agent is agent id's tags (nil for none).
func (s Set) Agent(id int64) []string { return s.Agents[id] }

// Mirror is the mirrored session's tags (nil for none).
func (s Set) Mirror(host, session string) []string {
	return s.Mirrors[MirrorKey{Host: host, Session: session}]
}

// Errors the store answers with; the API maps each to a status.
var (
	// ErrBadTarget refuses a target that names no subject, or two.
	ErrBadTarget = errors.New("sessiontags: name an agent or a mirrored session")
	// ErrNoSuchAgent refuses a tag on an agent the caller cannot see (absent,
	// or another user's — the two answer identically).
	ErrNoSuchAgent = errors.New("sessiontags: no such agent")
	// ErrTooMany refuses a tag past MaxPerTarget.
	ErrTooMany = fmt.Errorf("sessiontags: a session may carry at most %d tags; remove one first", MaxPerTarget)
	// ErrEmpty refuses a tag that normalizes to nothing.
	ErrEmpty = errors.New("sessiontags: the tag is empty")
)

// ErrInvalid wraps a tag the grammar refuses (its message names the tag).
var ErrInvalid = errors.New("sessiontags: invalid tag")

// Normalize is raw as it is stored, or an error: ErrEmpty for nothing,
// ErrInvalid (naming the tag) for what the grammar refuses.
func Normalize(raw string) (string, error) {
	t := notes.NormalizeTag(raw)
	if t == "" {
		return "", ErrEmpty
	}
	// One tag at a time through the task grammar's per-tag checks. The
	// at-most-one-`project:` rule cannot fire on a single tag, and the count
	// cap is MaxPerTarget's, checked by the store.
	if err := notes.ValidateTags([]string{t}); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return t, nil
}

// Store is the session tags' persistence.
type Store interface {
	// List is every tag of the caller's, by subject.
	List(ctx context.Context) (Set, error)
	// For is one subject's tags, sorted.
	For(ctx context.Context, t Target) ([]string, error)
	// Add normalizes raw and adds it to t; idempotent. It answers the
	// subject's tags after the add.
	Add(ctx context.Context, t Target, raw string) ([]string, error)
	// Remove takes raw (normalized) off t; idempotent. It answers the
	// subject's tags after the removal.
	Remove(ctx context.Context, t Target, raw string) ([]string, error)
}

// PGStore is Store on Postgres.
type PGStore struct{ pool *pgxpool.Pool }

// NewPG returns a Store on pool.
func NewPG(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

// List is the caller's tags, by subject.
func (s *PGStore) List(ctx context.Context) (Set, error) {
	set := Set{Agents: map[int64][]string{}, Mirrors: map[MirrorKey][]string{}}
	rows, err := s.pool.Query(ctx, `SELECT kind, agent_id, mirror_host, mirror_session, tag FROM session_tags ORDER BY tag`)
	if err != nil {
		return set, fmt.Errorf("sessiontags: list: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind, tag string
		var agentID *int64
		var host, session *string
		if err := rows.Scan(&kind, &agentID, &host, &session, &tag); err != nil {
			return set, fmt.Errorf("sessiontags: list: %w", err)
		}
		switch {
		case kind == "agent" && agentID != nil:
			set.Agents[*agentID] = append(set.Agents[*agentID], tag)
		case kind == "mirror" && host != nil && session != nil:
			k := MirrorKey{Host: *host, Session: *session}
			set.Mirrors[k] = append(set.Mirrors[k], tag)
		}
	}
	return set, rows.Err()
}

// For is t's tags, sorted; none is an empty (non-nil) slice.
func (s *PGStore) For(ctx context.Context, t Target) ([]string, error) {
	if err := t.Valid(); err != nil {
		return nil, err
	}
	return forTarget(ctx, s.pool, t)
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// where is t's subject as a predicate on session_tags, from arg $1.
func where(t Target) (string, []any) {
	if t.Mirror() {
		return `kind = 'mirror' AND mirror_host = $1 AND mirror_session = $2`, []any{t.Host, t.Session}
	}
	return `kind = 'agent' AND agent_id = $1`, []any{t.AgentID}
}

func forTarget(ctx context.Context, q querier, t Target) ([]string, error) {
	pred, args := where(t)
	rows, err := q.Query(ctx, `SELECT tag FROM session_tags WHERE `+pred, args...)
	if err != nil {
		return nil, fmt.Errorf("sessiontags: read: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("sessiontags: read: %w", err)
	}
	if out == nil {
		out = []string{}
	}
	sort.Strings(out)
	return out, nil
}

// Add adds raw (normalized) to t for the caller. Idempotent: a tag t already
// carries, in any case, is no new row and is not refused at the cap.
//
// The cap is checked AFTER the insert, in the same transaction, serialised per
// owner by an advisory lock (pins.insert's shape): two concurrent adds cannot
// both pass it.
func (s *PGStore) Add(ctx context.Context, t Target, raw string) ([]string, error) {
	if err := t.Valid(); err != nil {
		return nil, err
	}
	tag, err := Normalize(raw)
	if err != nil {
		return nil, err
	}
	var out []string
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('session_tags.add'),
			coalesce(nullif(current_setting('muster.uid', true), '')::int, 0))`); err != nil {
			return fmt.Errorf("sessiontags: lock: %w", err)
		}
		var q string
		var args []any
		if t.Mirror() {
			q = `INSERT INTO session_tags (kind, mirror_host, mirror_session, tag) VALUES ('mirror', $1, $2, $3)
				ON CONFLICT (owner_id, mirror_host, mirror_session, tag) WHERE kind = 'mirror' DO NOTHING`
			args = []any{t.Host, t.Session, tag}
		} else {
			q = `INSERT INTO session_tags (kind, agent_id, tag) VALUES ('agent', $1, $2)
				ON CONFLICT (owner_id, agent_id, tag) WHERE kind = 'agent' DO NOTHING`
			args = []any{t.AgentID, tag}
		}
		if _, err := tx.Exec(ctx, q, args...); err != nil {
			var pg *pgconn.PgError
			// pins.insert's mapping, measured there: an absent or another
			// owner's agent is 23514 with no constraint name (the owner
			// trigger); an agent deleted concurrently is 23503 (the FK).
			if errors.As(err, &pg) && (pg.Code == "23503" || (pg.Code == "23514" && pg.ConstraintName == "")) {
				return ErrNoSuchAgent
			}
			return fmt.Errorf("sessiontags: add: %w", err)
		}
		tags, err := forTarget(ctx, tx, t)
		if err != nil {
			return err
		}
		if len(tags) > MaxPerTarget {
			return ErrTooMany
		}
		out = tags
		return nil
	})
	return out, err
}

// Remove takes raw (normalized) off t for the caller. Idempotent. A tag that
// does not normalize is not on any subject, so removing it changes nothing.
func (s *PGStore) Remove(ctx context.Context, t Target, raw string) ([]string, error) {
	if err := t.Valid(); err != nil {
		return nil, err
	}
	pred, args := where(t)
	if tag := notes.NormalizeTag(raw); tag != "" {
		if _, err := s.pool.Exec(ctx, `DELETE FROM session_tags WHERE `+pred+fmt.Sprintf(` AND tag = $%d`, len(args)+1),
			append(args, tag)...); err != nil {
			return nil, fmt.Errorf("sessiontags: remove: %w", err)
		}
	}
	return forTarget(ctx, s.pool, t)
}
