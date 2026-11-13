package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// --- schedules: a builtin officer's cron (officer_schedules, 0118) ------------
//
// A schedule enqueues one item for its officer each time its cron comes due
// (UTC). The rules, each pinned by a test:
//
//   - ONCE ACROSS REPLICAS. A fire is a compare-and-set on next_fire_at
//     (UPDATE … WHERE id = $1 AND next_fire_at = <the value read>): of two
//     replicas reading the same due schedule exactly one moves it, and only
//     that one enqueues — in the SAME transaction, so a fire and its item
//     commit together or not at all.
//   - ONCE ACROSS A RESTART. next_fire_at is the database's, so a restarted
//     muster reads the schedule as already advanced.
//   - MISSED FIRES COLLAPSE TO ONE. The next fire is computed from NOW, not
//     from the missed time: a schedule that was due ten times while muster
//     was down fires once, then at its next future time.
//   - skip_if_live (default): the item's subject is the schedule's subject,
//     so a fire while an item of that kind and subject is still live (open,
//     claimed or waiting) enqueues nothing — the live-subject dedupe. With it
//     off, each fire's subject is the schedule's suffixed with the fire's
//     minute (`<subject>@2026-11-13T06:00Z`), so every fire is a new item.
//   - A SWITCHED-OFF OFFICER (officer_defs.enabled false) is not fed: its
//     schedules still advance, so switching it on does not release a backlog.

// Schedule is one schedule as the admin API puts and lists it.
type Schedule struct {
	Name        string          `json:"name"`
	Cron        string          `json:"cron"`
	Kind        string          `json:"kind"`
	SubjectKey  string          `json:"subject_key"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	Tier        string          `json:"tier"`
	Priority    string          `json:"priority"`
	SkipIfLive  bool            `json:"skip_if_live"`
	Enabled     bool            `json:"enabled"`
	NextFireAt  *time.Time      `json:"next_fire_at"`
	LastFiredAt *time.Time      `json:"last_fired_at"`
}

// PutSchedule creates or replaces owner's officer's schedule s.Name and
// answers it: its cron is parsed (ErrInvalid naming the field) and its next
// fire computed from now — so a replaced schedule never fires a time it was
// due under its old cron. A cron with no time in the next five years is
// refused. ErrNotFound when the officer is not one of owner's.
func (s *Store) PutSchedule(ctx context.Context, owner int64, officer string, sc Schedule, now time.Time) (Schedule, error) {
	c, err := ParseCron(sc.Cron)
	if err != nil {
		return Schedule{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	next, ok := c.Next(now)
	if !ok {
		return Schedule{}, fmt.Errorf("%w: cron %q never comes due", ErrInvalid, sc.Cron)
	}
	payload := sc.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	tier, prio := sc.Tier, sc.Priority
	if tier == "" {
		tier = "judgement"
	}
	if prio == "" {
		prio = "normal"
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO officer_schedules (owner_id, officer, name, cron, kind, subject_key, payload, tier, priority, skip_if_live, enabled, next_fire_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (owner_id, officer, name) DO UPDATE SET cron = EXCLUDED.cron, kind = EXCLUDED.kind, subject_key = EXCLUDED.subject_key,
			payload = EXCLUDED.payload, tier = EXCLUDED.tier, priority = EXCLUDED.priority, skip_if_live = EXCLUDED.skip_if_live,
			enabled = EXCLUDED.enabled, next_fire_at = EXCLUDED.next_fire_at, updated_at = now()`,
		owner, officer, sc.Name, sc.Cron, sc.Kind, sc.SubjectKey, payload, tier, prio, sc.SkipIfLive, sc.Enabled, next)
	if err != nil {
		return Schedule{}, classify(err)
	}
	list, err := s.ListSchedules(ctx, owner, officer)
	if err != nil {
		return Schedule{}, err
	}
	for _, x := range list {
		if x.Name == sc.Name {
			return x, nil
		}
	}
	return Schedule{}, ErrNotFound
}

// ListSchedules is owner's officer's schedules, by name.
func (s *Store) ListSchedules(ctx context.Context, owner int64, officer string) ([]Schedule, error) {
	rows, err := s.pool.Query(ctx, `SELECT name, cron, kind, subject_key, payload, tier, priority, skip_if_live, enabled, next_fire_at, last_fired_at
		FROM officer_schedules WHERE owner_id = $1 AND officer = $2 ORDER BY name`, owner, officer)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Schedule, error) {
		var x Schedule
		err := r.Scan(&x.Name, &x.Cron, &x.Kind, &x.SubjectKey, &x.Payload, &x.Tier, &x.Priority, &x.SkipIfLive, &x.Enabled, &x.NextFireAt, &x.LastFiredAt)
		return x, err
	})
	if out == nil {
		out = []Schedule{}
	}
	return out, err
}

// DeleteSchedule removes owner's officer's schedule name.
func (s *Store) DeleteSchedule(ctx context.Context, owner int64, officer, name string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM officer_schedules WHERE owner_id = $1 AND officer = $2 AND name = $3`, owner, officer, name)
	if err != nil {
		return classify(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Fire is what one due schedule did on one FireDue.
type Fire struct {
	Owner             int64
	Officer, Schedule string
	// Item is the item enqueued (0: none).
	Item int64
	// Skipped says why nothing was enqueued: "live" (an item of that subject
	// is still live), "switched off", "no next fire" (the schedule stops); ""
	// when Item is set.
	Skipped string
}

// dueSchedule is a schedule FireDue read as due.
type dueSchedule struct {
	id, owner     int64
	officer, name string
	cron          string
	nextFireAt    time.Time
}

// FireDue fires every schedule due at now, at most limit, each at most once
// (the compare-and-set above) and answers what each did. ctx must act for
// every owner (the system scope). A schedule another replica moved first is
// left out of the answer.
func (s *Store) FireDue(ctx context.Context, now time.Time, limit int) ([]Fire, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, owner_id, officer, name, cron, next_fire_at FROM officer_schedules
		WHERE enabled AND next_fire_at IS NOT NULL AND next_fire_at <= $1 ORDER BY next_fire_at, id LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	due, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (dueSchedule, error) {
		var d dueSchedule
		err := r.Scan(&d.id, &d.owner, &d.officer, &d.name, &d.cron, &d.nextFireAt)
		return d, err
	})
	if err != nil {
		return nil, err
	}
	var out []Fire
	var errs []error
	for _, d := range due {
		f, won, err := s.fireOne(ctx, d, now)
		if err != nil {
			errs = append(errs, fmt.Errorf("schedule %s/%s: %w", d.officer, d.name, err))
			continue
		}
		if won {
			out = append(out, f)
		}
	}
	return out, errors.Join(errs...)
}

// fireOne is one schedule's fire, in one transaction: the compare-and-set on
// next_fire_at, then (if this caller won it) the item.
func (s *Store) fireOne(ctx context.Context, d dueSchedule, now time.Time) (Fire, bool, error) {
	f := Fire{Owner: d.owner, Officer: d.officer, Schedule: d.name}
	var next *time.Time
	if c, err := ParseCron(d.cron); err == nil {
		if n, ok := c.Next(now); ok {
			next = &n
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return f, false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var kind, subject, tier, prio string
	var payload json.RawMessage
	var skipIfLive bool
	err = tx.QueryRow(ctx, `UPDATE officer_schedules SET next_fire_at = $3, last_fired_at = $4, updated_at = $4
		WHERE id = $1 AND enabled AND next_fire_at = $2
		RETURNING kind, subject_key, payload, tier, priority, skip_if_live`, d.id, d.nextFireAt, next, now).
		Scan(&kind, &subject, &payload, &tier, &prio, &skipIfLive)
	if errors.Is(err, pgx.ErrNoRows) {
		return f, false, nil // another replica fired it, or it changed
	}
	if err != nil {
		return f, false, err
	}
	var on bool
	if err := tx.QueryRow(ctx, `SELECT enabled FROM officer_defs WHERE owner_id = $1 AND officer = $2`, d.owner, d.officer).Scan(&on); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return f, false, err
	}
	switch {
	case !on:
		f.Skipped = "switched off"
	default:
		if !skipIfLive {
			subject = subject + "@" + d.nextFireAt.UTC().Format("2006-01-02T15:04Z")
		}
		err := tx.QueryRow(ctx, `INSERT INTO officer_items (owner_id, officer, kind, subject_key, payload, tier, priority, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)
			ON CONFLICT (owner_id, officer, kind, subject_key) WHERE status IN ('open', 'claimed', 'waiting') DO NOTHING
			RETURNING id`, d.owner, d.officer, kind, subject, payload, tier, prio, now).Scan(&f.Item)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			f.Skipped = "live"
		case err != nil:
			return f, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return f, false, err
	}
	if next == nil {
		f.Skipped = joinSkip(f.Skipped, "no next fire")
	}
	return f, true, nil
}

func joinSkip(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// DefaultScheduleInterval is how often RunScheduler looks for due schedules.
const DefaultScheduleInterval = 30 * time.Second

// scheduleBatch bounds one FireDue.
const scheduleBatch = 100

// RunScheduler fires due schedules every interval until ctx ends, logging
// each fire. sys makes a context act for every owner (authz.System); now is
// the clock (nil: time.Now).
func (s *Store) RunScheduler(ctx context.Context, interval time.Duration, sys func(context.Context) context.Context, now func() time.Time, logf func(string, ...any)) {
	if now == nil {
		now = time.Now
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fires, err := s.FireDue(sys(ctx), now(), scheduleBatch)
			if err != nil {
				logf("officers: schedules: %v", err)
			}
			for _, f := range fires {
				if f.Item != 0 {
					logf("officers: %s: schedule %s enqueued item %d", f.Officer, f.Schedule, f.Item)
				} else {
					logf("officers: %s: schedule %s fired, nothing enqueued (%s)", f.Officer, f.Schedule, f.Skipped)
				}
			}
		}
	}
}
