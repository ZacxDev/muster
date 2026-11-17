package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// THE FLEET'S ACTION LOG AND THE DIGEST'S MARKS (migration 0147). Every
// method runs as ctx's principal: both tables are under FORCE row-level
// security, so an owner reads and writes only its own rows.
//
// 🔴 PHASE 1 WRITES ONE CLASS, `notify`, FROM ONE PLACE: the episode push's
// claim (internal/api pushEpisode) — a stop of the owner's that muster pushed
// to them. The log is append-only by trigger (the migration's header); its
// one update is Settle's, finishing an action once its episode ended.

// ClassNotify is the one class Phase 1 writes.
const ClassNotify = "notify"

// ProposerSweep is a row the session-state sweep wrote (code, no model).
const ProposerSweep = "sweep_code"

// StateSent is a notify's state: the owner was told.
const StateSent = "sent"

// Notify is one notify row as RecordNotify writes it.
type Notify struct {
	// SubjectKind and SubjectKey name the session as the stall sweep keys it
	// (session_episodes' kind and key).
	SubjectKind, SubjectKey string
	Host, Window            string
	AgentID                 int64
	EpisodeID               int64
	// Evidence is references only (ids, labels), never session text.
	Evidence map[string]any
	// Reason is a muster-built line, cut to MaxReasonRunes.
	Reason string
}

// MaxReasonRunes bounds a row's reason (0147's CHECK).
const MaxReasonRunes = 300

// ErrBadNotify: a notify the store refused before it reached the database.
var ErrBadNotify = errors.New("fleet: not a valid notify")

// LookGap is how long the owner must have been away for an open of the panel
// to count as a NEW look: opens closer together than this are one look, so the
// "since you last looked" line does not reset every time the panel is
// reopened within a few minutes.
const LookGap = 10 * time.Minute

// Store is the fleet's log and marks.
type Store interface {
	// RecordNotify writes n as a sent notify, at most once per episode;
	// written false when its episode already has one.
	RecordNotify(ctx context.Context, n Notify) (written bool, err error)
	// Settle finishes every open action of the principal's whose episode has
	// ended, with the episode's end time and reason; it returns how many.
	Settle(ctx context.Context) (int, error)
	// OpenCounts is the principal's open actions per session, keyed
	// "<subject kind>:<subject key>".
	OpenCounts(ctx context.Context) (map[string]int, error)
	// Look records a look at the digest at at and returns the baseline the
	// card compares against: the previous look, when the one before at is
	// LookGap or more ago (it then becomes the baseline), else the baseline
	// kept. Zero: no look before this one.
	Look(ctx context.Context, at time.Time) (time.Time, error)
	// Baseline is the stored baseline, without recording a look.
	Baseline(ctx context.Context) (time.Time, error)
	// FleetChat is the principal's Fleet side chat's session id (0: none).
	FleetChat(ctx context.Context) (int64, error)
	// SetFleetChat records it.
	SetFleetChat(ctx context.Context, sessionID int64) error
}

// PG is Store on Postgres.
type PG struct{ pool *pgxpool.Pool }

// NewPG is Store over pool.
func NewPG(pool *pgxpool.Pool) *PG { return &PG{pool: pool} }

// RecordNotify implements Store.
func (p *PG) RecordNotify(ctx context.Context, n Notify) (bool, error) {
	if (n.SubjectKind != "mirror" && n.SubjectKind != "agent") || n.SubjectKey == "" || len(n.SubjectKey) > 200 || n.EpisodeID <= 0 {
		return false, ErrBadNotify
	}
	ev := n.Evidence
	if ev == nil {
		ev = map[string]any{}
	}
	raw, err := json.Marshal(ev)
	if err != nil || len(raw) > 4096 {
		return false, ErrBadNotify
	}
	var agent any
	if n.AgentID > 0 {
		agent = n.AgentID
	}
	tag, err := p.pool.Exec(ctx, `INSERT INTO fleet_actions (class, proposer, subject_kind, subject_key, mirror_host, mirror_window,
			agent_id, episode_id, evidence, reason, state)
		VALUES ($1, $2, $3, $4, left($5, 200), left($6, 32), $7, $8, $9, left($10, $11), $12)
		ON CONFLICT DO NOTHING`,
		ClassNotify, ProposerSweep, n.SubjectKind, n.SubjectKey, n.Host, n.Window, agent, n.EpisodeID, raw, n.Reason, MaxReasonRunes, StateSent)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// Settle implements Store. 🔴 THE EPISODE'S OWN END, NOT now(): an action
// finishes when its stop did, whenever the pass gets to it.
func (p *PG) Settle(ctx context.Context) (int, error) {
	tag, err := p.pool.Exec(ctx, `UPDATE fleet_actions fa SET finished_at = greatest(e.ended_at, fa.created_at), outcome = e.end_reason
		FROM session_episodes e
		WHERE fa.finished_at IS NULL AND fa.episode_id = e.id AND e.owner_id = fa.owner_id AND e.ended_at IS NOT NULL`)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// OpenCounts implements Store.
func (p *PG) OpenCounts(ctx context.Context) (map[string]int, error) {
	rows, err := p.pool.Query(ctx, `SELECT subject_kind, subject_key, count(*) FROM fleet_actions
		WHERE finished_at IS NULL GROUP BY 1, 2`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var kind, key string
		var n int
		if err := rows.Scan(&kind, &key, &n); err != nil {
			return nil, err
		}
		out[kind+":"+key] = n
	}
	return out, rows.Err()
}

// Look implements Store.
func (p *PG) Look(ctx context.Context, at time.Time) (time.Time, error) {
	var seen *time.Time
	err := p.pool.QueryRow(ctx, `INSERT INTO fleet_owner_marks (looked_at) VALUES ($1)
		ON CONFLICT (owner_id) DO UPDATE SET
			seen_at = CASE WHEN fleet_owner_marks.looked_at IS NOT NULL AND $1 - fleet_owner_marks.looked_at >= make_interval(secs => $2)
				THEN fleet_owner_marks.looked_at ELSE fleet_owner_marks.seen_at END,
			looked_at = greatest($1, fleet_owner_marks.looked_at), updated_at = now()
		RETURNING seen_at`, at, LookGap.Seconds()).Scan(&seen)
	if err != nil || seen == nil {
		return time.Time{}, err
	}
	return *seen, nil
}

// Baseline implements Store.
func (p *PG) Baseline(ctx context.Context) (time.Time, error) {
	var seen *time.Time
	err := p.pool.QueryRow(ctx, `SELECT seen_at FROM fleet_owner_marks`).Scan(&seen)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && seen == nil) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return *seen, nil
}

// FleetChat implements Store.
func (p *PG) FleetChat(ctx context.Context) (int64, error) {
	var id *int64
	err := p.pool.QueryRow(ctx, `SELECT fleet_chat_id FROM fleet_owner_marks`).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && id == nil) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return *id, nil
}

// SetFleetChat implements Store.
func (p *PG) SetFleetChat(ctx context.Context, sessionID int64) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO fleet_owner_marks (fleet_chat_id) VALUES ($1)
		ON CONFLICT (owner_id) DO UPDATE SET fleet_chat_id = EXCLUDED.fleet_chat_id, updated_at = now()`, sessionID)
	return err
}
