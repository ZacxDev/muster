package morning

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/users"
)

// THE MORNING REPORT'S ROWS (migration 0156). Every method runs as ctx's
// principal: morning_reports is under FORCE row-level security, so an owner
// reads and writes only its own reports — and every read and the push claim
// ALSO name the principal's owner in SQL (ownerIs), so a policy turned off
// (rollback/rls_off.sql) still cannot hand one owner another's report.

// ownerIs is the principal's own rows, spelled as the policy spells it.
const ownerIs = `owner_id = nullif(current_setting('muster.uid', true), '')::bigint`

// Body is what a report's row keeps of it: the lines that were true in its
// window, as the job read them. The open questions are not kept — the page
// reads them live, as the real asks, so an answered one is gone.
type Body struct {
	Done  []Done  `json:"done"`
	Stuck []Stuck `json:"stuck"`
	Usage Usage   `json:"usage"`
	// Gaps are the plain sentences saying what could not be read ("Merged
	// pull requests could not be checked just now.").
	Gaps []Gap `json:"gaps,omitempty"`
}

// Row is one day's report as stored.
type Row struct {
	Date         string
	Since, Until time.Time
	Quiet        bool
	Questions    int
	Body         Body
	CreatedAt    time.Time
	PushedAt     time.Time // zero: no push
}

// Store is the reports.
type Store interface {
	// Insert records day's report for the principal. 🔴 ONE PER OWNER PER
	// DAY: false (and nothing written) when the day already has one — the
	// caller then builds and sends nothing.
	Insert(ctx context.Context, r Row) (bool, error)
	// ClaimPush marks date's report pushed, once: true for the one caller
	// that moved pushed_at from NULL, false for every other (already pushed,
	// a quiet day, or no such report). Claimed BEFORE the send, so a send is
	// at most once.
	ClaimPush(ctx context.Context, date string, at time.Time) (bool, error)
	// Get is the principal's report of date; false when there is none.
	Get(ctx context.Context, date string) (Row, bool, error)
	// Latest is the principal's newest report; false when there is none.
	Latest(ctx context.Context) (Row, bool, error)
	// Owners is who gets a report: every enabled person, never a team (a
	// team's sessions are its members' to read) and never a disabled
	// account. Read under the system scope.
	Owners(ctx context.Context) ([]int64, error)
}

// PG is Store on Postgres.
type PG struct{ pool *pgxpool.Pool }

// NewPG is Store over pool.
func NewPG(pool *pgxpool.Pool) *PG { return &PG{pool: pool} }

var _ Store = (*PG)(nil)

// Insert implements Store.
func (p *PG) Insert(ctx context.Context, r Row) (bool, error) {
	body, err := json.Marshal(r.Body)
	if err != nil {
		return false, err
	}
	tag, err := p.pool.Exec(ctx, `INSERT INTO morning_reports (day, window_since, window_until, quiet, questions, body)
		VALUES ($1::date, $2, $3, $4, $5, $6) ON CONFLICT (owner_id, day) DO NOTHING`,
		r.Date, r.Since, r.Until, r.Quiet, r.Questions, body)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ClaimPush implements Store.
func (p *PG) ClaimPush(ctx context.Context, date string, at time.Time) (bool, error) {
	tag, err := p.pool.Exec(ctx, `UPDATE morning_reports SET pushed_at = $2
		WHERE `+ownerIs+` AND day = $1::date AND pushed_at IS NULL AND NOT quiet`, date, at)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

const rowColumns = `to_char(day, 'YYYY-MM-DD'), window_since, window_until, quiet, questions, body, created_at, pushed_at`

// Get implements Store.
func (p *PG) Get(ctx context.Context, date string) (Row, bool, error) {
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return Row{}, false, nil
	}
	return scanRow(p.pool.QueryRow(ctx, `SELECT `+rowColumns+` FROM morning_reports WHERE `+ownerIs+` AND day = $1::date`, date))
}

// Latest implements Store.
func (p *PG) Latest(ctx context.Context) (Row, bool, error) {
	return scanRow(p.pool.QueryRow(ctx, `SELECT `+rowColumns+` FROM morning_reports WHERE `+ownerIs+` ORDER BY day DESC LIMIT 1`))
}

// Owners implements Store.
func (p *PG) Owners(ctx context.Context) ([]int64, error) {
	rows, err := p.pool.Query(authz.System(ctx), `SELECT id FROM users WHERE id IN (`+users.EnabledPersonIDs+`) ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func scanRow(row pgx.Row) (Row, bool, error) {
	var r Row
	var body []byte
	var pushed *time.Time
	err := row.Scan(&r.Date, &r.Since, &r.Until, &r.Quiet, &r.Questions, &body, &r.CreatedAt, &pushed)
	if errors.Is(err, pgx.ErrNoRows) {
		return Row{}, false, nil
	}
	if err != nil {
		return Row{}, false, err
	}
	if err := json.Unmarshal(body, &r.Body); err != nil {
		return Row{}, false, err
	}
	if pushed != nil {
		r.PushedAt = *pushed
	}
	return r, true, nil
}
