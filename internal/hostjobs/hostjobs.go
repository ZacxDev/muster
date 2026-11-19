// Package hostjobs is the store of SCHEDULED HOST JOBS (migration 0157;
// docs/host-jobs.md): each job as its host last reported it, its cron's next
// fire, and its runs. The protocol is internal/mirror's hostjobs.go, the host
// half internal/mirrorhost's, and what a fire and a result DO — the trigger,
// the page version, the Attention item — internal/api's hostjobs.go.
//
// 🔴 A JOB IS WRITTEN HERE ONLY FROM ITS HOST'S REPORT (Report). Nothing in
// this package takes a command from anyone else, and nothing reads one back
// to send it anywhere: the command column is for display.
//
// 🔴 EVERY METHOD RUNS UNDER THE CALLER'S PRINCIPAL (row-level security,
// owner_id defaulting to the principal) — except the scheduler's FireDue and
// the sweep's LostRuns, which read every owner's rows through this package's
// one System helper (sys) and hand each row back with its owner, to be acted
// on as THAT owner.
package hostjobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/cronspec"
	"github.com/ZacxDev/muster/internal/mirror"
)

// Run statuses (0157's CHECK).
const (
	StatusRunning = "running"
	// StatusOK: the command exited 0 and its output was a valid page — a
	// version written, or none because it was unchanged.
	StatusOK = "ok"
	// StatusFailed: a non-zero exit, a stdout over the bound, or a page that
	// could not be written.
	StatusFailed = "failed"
	// StatusTimeout: the host stopped it at its timeout.
	StatusTimeout = "timeout"
	// StatusInvalid: it exited 0 and its output is not a valid page.
	StatusInvalid = "invalid"
	// StatusOffline: the host was not connected when it was due.
	StatusOffline = "offline"
	// StatusSkipped: it was due while its previous run was still going.
	StatusSkipped = "skipped"
	// StatusRefused: the host refused the job-run (Detail says why).
	StatusRefused = "refused"
	// StatusLost: muster never got its result (LostRuns).
	StatusLost = "lost"
)

// Triggers (0157's CHECK).
const (
	TriggerCron   = "cron"
	TriggerManual = "manual"
)

var (
	// ErrNotFound is a job or run that is not the caller's, or not there.
	ErrNotFound = errors.New("hostjobs: no such job or run")
)

// Job is one job as its host last reported it.
type Job struct {
	ID          int64      `json:"id"`
	Owner       int64      `json:"-"`
	Host        string     `json:"host"`
	Name        string     `json:"name"`
	Cron        string     `json:"cron"`
	Command     []string   `json:"command"`
	PageID      int64      `json:"page_id,omitempty"`
	MadePageID  int64      `json:"made_page_id,omitempty"`
	Title       string     `json:"title,omitempty"`
	Output      string     `json:"output"`
	TimeoutS    int        `json:"timeout_seconds"`
	Declared    bool       `json:"declared"`
	NextFireAt  *time.Time `json:"next_fire_at"`
	LastFiredAt *time.Time `json:"last_fired_at"`
	ReportedAt  time.Time  `json:"reported_at"`
}

// Decl is j as its host declared it (mirror.HostJob).
func (j Job) Decl() mirror.HostJob {
	return mirror.HostJob{Name: j.Name, Cron: j.Cron, Command: j.Command, Page: j.PageID, Title: j.Title, Output: j.Output, TimeoutSeconds: j.TimeoutS}
}

// TargetPage is the page j writes: the one its host named, else the one
// muster made for it (0: none yet).
func (j Job) TargetPage() int64 {
	if j.PageID > 0 {
		return j.PageID
	}
	return j.MadePageID
}

// Run is one run of a job.
type Run struct {
	ID          int64      `json:"id"`
	Owner       int64      `json:"-"`
	JobID       int64      `json:"job_id"`
	Host        string     `json:"host"`
	Job         string     `json:"job"`
	Trigger     string     `json:"trigger"`
	Status      string     `json:"status"`
	TimeoutS    int        `json:"timeout_seconds"`
	FiredAt     time.Time  `json:"fired_at"`
	StartedAt   *time.Time `json:"started_at"`
	EndedAt     *time.Time `json:"ended_at"`
	ExitCode    *int       `json:"exit_code"`
	DurationMs  *int64     `json:"duration_ms"`
	PageID      int64      `json:"page_id,omitempty"`
	PageVersion int        `json:"page_version,omitempty"`
	StderrTail  string     `json:"stderr_tail,omitempty"`
	Detail      string     `json:"detail,omitempty"`
	NoticeID    int64      `json:"notice_id,omitempty"`
}

// Store is the two tables on Postgres.
type Store struct{ pool *pgxpool.Pool }

// NewPG returns a Store on pool.
func NewPG(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// sys is this package's ONE authz.System call (ledgered in
// internal/authz/ledger_test.go): the scheduler's compare-and-set of every
// owner's due jobs (FireDue) and the sweep's read of every owner's runs whose
// result is overdue (LostRuns). Each row is handed back with its owner and
// acted on as that owner.
func sys(ctx context.Context) context.Context { return authz.System(ctx) }

const jobCols = `id, owner_id, host, name, cron, command, coalesce(page_id, 0), coalesce(made_page_id, 0), title, output, timeout_s, declared, next_fire_at, last_fired_at, reported_at`

func scanJob(row pgx.Row) (Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.Owner, &j.Host, &j.Name, &j.Cron, &j.Command, &j.PageID, &j.MadePageID, &j.Title, &j.Output, &j.TimeoutS, &j.Declared, &j.NextFireAt, &j.LastFiredAt, &j.ReportedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	return j, err
}

func collectJobs(rows pgx.Rows, err error) ([]Job, error) {
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Job, error) { return scanJob(r) })
	if out == nil {
		out = []Job{}
	}
	return out, err
}

const runCols = `id, owner_id, job_id, host, job, trigger, status, timeout_s, fired_at, started_at, ended_at, exit_code, duration_ms, coalesce(page_id, 0), coalesce(page_version, 0), stderr_tail, detail, coalesce(notice_id, 0)`

func scanRun(row pgx.Row) (Run, error) {
	var r Run
	err := row.Scan(&r.ID, &r.Owner, &r.JobID, &r.Host, &r.Job, &r.Trigger, &r.Status, &r.TimeoutS, &r.FiredAt, &r.StartedAt, &r.EndedAt, &r.ExitCode, &r.DurationMs, &r.PageID, &r.PageVersion, &r.StderrTail, &r.Detail, &r.NoticeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	return r, err
}

func collectRuns(rows pgx.Rows, err error) ([]Run, error) {
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Run, error) { return scanRun(r) })
	if out == nil {
		out = []Run{}
	}
	return out, err
}

// Report records the caller's host's declared jobs (CleanJobs' list): each
// is created or updated as reported, and every other job of that host is
// UNDECLARED (kept for its runs, never fired). A job whose cron is new or
// changed, or that was undeclared, gets its next fire computed from now; an
// unchanged one keeps the fire it had, so a reconnect neither skips nor
// repeats a fire. A job whose cron does not parse is not recorded (and is
// named in the error; the rest are).
func (s *Store) Report(ctx context.Context, host string, jobs []mirror.HostJob, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var errs []error
	names := []string{}
	for _, j := range jobs {
		c, err := cronspec.Parse(j.Cron)
		if err != nil {
			errs = append(errs, fmt.Errorf("job %s: %w", j.Name, err))
			continue
		}
		next, ok := c.Next(now)
		var nextAt *time.Time
		if ok {
			nextAt = &next
		}
		var page any
		if j.Page > 0 {
			page = j.Page
		}
		names = append(names, j.Name)
		_, err = tx.Exec(ctx, `INSERT INTO host_jobs AS h (host, name, cron, command, page_id, title, output, timeout_s, declared, next_fire_at, reported_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, true, $9, $10)
			ON CONFLICT (owner_id, host, name) DO UPDATE SET
				command = EXCLUDED.command, page_id = EXCLUDED.page_id, title = EXCLUDED.title, output = EXCLUDED.output,
				timeout_s = EXCLUDED.timeout_s, reported_at = EXCLUDED.reported_at, updated_at = EXCLUDED.reported_at,
				next_fire_at = CASE WHEN h.declared AND h.cron = EXCLUDED.cron AND h.next_fire_at IS NOT NULL THEN h.next_fire_at ELSE EXCLUDED.next_fire_at END,
				cron = EXCLUDED.cron, declared = true`,
			host, j.Name, j.Cron, j.Command, page, j.Title, j.OutputFormat(), int(j.Timeout()/time.Second), nextAt, now)
		if err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE host_jobs SET declared = false, next_fire_at = NULL, updated_at = $3
		WHERE host = $1 AND declared AND NOT (name = ANY($2))`, host, names, now); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return errors.Join(errs...)
}

// Jobs is the caller's DECLARED jobs, by host then name.
func (s *Store) Jobs(ctx context.Context) ([]Job, error) {
	return collectJobs(s.pool.Query(ctx, `SELECT `+jobCols+` FROM host_jobs WHERE declared ORDER BY host, name`))
}

// JobsNamed is the caller's declared jobs named name, on host when host is
// not "" (a name may be declared on several hosts).
func (s *Store) JobsNamed(ctx context.Context, name, host string) ([]Job, error) {
	return collectJobs(s.pool.Query(ctx, `SELECT `+jobCols+` FROM host_jobs WHERE declared AND name = $1 AND ($2 = '' OR host = $2) ORDER BY host`, name, host))
}

// Job is the caller's job id.
func (s *Store) Job(ctx context.Context, id int64) (Job, error) {
	return scanJob(s.pool.QueryRow(ctx, `SELECT `+jobCols+` FROM host_jobs WHERE id = $1`, id))
}

// SetMadePage records page as the one muster made for the caller's job id.
func (s *Store) SetMadePage(ctx context.Context, id, page int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE host_jobs SET made_page_id = $2, updated_at = now() WHERE id = $1`, id, page)
	return err
}

// Fire is one due job FireDue fired: the job (its owner set) and the time it
// was due.
type Fire struct {
	Job   Job
	DueAt time.Time
}

// FireDue advances every job due at now, at most limit, each at most once —
// a compare-and-set on next_fire_at (… WHERE id = $1 AND next_fire_at = <the
// value read>), so of two replicas reading the same due job exactly one moves
// it — and answers the jobs THIS caller moved. Missed fires collapse to one:
// the next fire is computed from now, as an officer schedule's is. ctx must
// act for every owner: it is read through sys.
func (s *Store) FireDue(ctx context.Context, now time.Time, limit int) ([]Fire, error) {
	ctx = sys(ctx)
	due, err := collectJobs(s.pool.Query(ctx, `SELECT `+jobCols+` FROM host_jobs
		WHERE declared AND next_fire_at IS NOT NULL AND next_fire_at <= $1 ORDER BY next_fire_at, id LIMIT $2`, now, limit))
	if err != nil {
		return nil, err
	}
	var out []Fire
	var errs []error
	for _, j := range due {
		var next *time.Time
		if c, err := cronspec.Parse(j.Cron); err == nil {
			if n, ok := c.Next(now); ok {
				next = &n
			}
		}
		tag, err := s.pool.Exec(ctx, `UPDATE host_jobs SET next_fire_at = $3, last_fired_at = $4, updated_at = $4
			WHERE id = $1 AND declared AND next_fire_at = $2`, j.ID, j.NextFireAt, next, now)
		if err != nil {
			errs = append(errs, fmt.Errorf("job %s on %s: %w", j.Name, j.Host, err))
			continue
		}
		if tag.RowsAffected() == 1 {
			out = append(out, Fire{Job: j, DueAt: *j.NextFireAt})
		}
	}
	return out, errors.Join(errs...)
}

// maxRunsPerJob bounds the runs kept per job: the oldest finished ones go as
// a new run is recorded.
const maxRunsPerJob = 1000

// StartRun records a run of the caller's job j by trigger at now, as ONE of:
//   - skipped, when a run of j is still running (the 0157 one-running index
//     decides, whichever replica or path got there first);
//   - running, when online (the caller then sends the host its job-run);
//   - offline, when the host is not connected.
func (s *Store) StartRun(ctx context.Context, j Job, trigger string, now time.Time, online bool) (Run, error) {
	status := StatusRunning
	if !online {
		status = StatusOffline
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Run{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var running bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM host_job_runs WHERE job_id = $1 AND status = 'running')`, j.ID).Scan(&running); err != nil {
		return Run{}, err
	}
	if running {
		status = StatusSkipped
	}
	r, err := scanRun(tx.QueryRow(ctx, `INSERT INTO host_job_runs (job_id, host, job, trigger, status, timeout_s, fired_at, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (job_id) WHERE status = 'running' DO NOTHING
		RETURNING `+runCols, j.ID, j.Host, j.Name, trigger, status, j.TimeoutS, now, startDetail(status)))
	if errors.Is(err, ErrNotFound) {
		// Another path started a run between the read and the insert: this
		// one is skipped, as if it had read it.
		r, err = scanRun(tx.QueryRow(ctx, `INSERT INTO host_job_runs (job_id, host, job, trigger, status, timeout_s, fired_at, detail)
			VALUES ($1, $2, $3, $4, 'skipped', $5, $6, $7) RETURNING `+runCols, j.ID, j.Host, j.Name, trigger, j.TimeoutS, now, startDetail(StatusSkipped)))
	}
	if err != nil {
		return Run{}, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM host_job_runs WHERE job_id = $1 AND status <> 'running' AND id IN (
		SELECT id FROM host_job_runs WHERE job_id = $1 ORDER BY id DESC OFFSET $2)`, j.ID, maxRunsPerJob); err != nil {
		return Run{}, err
	}
	return r, tx.Commit(ctx)
}

// startDetail is a run's detail as StartRun records it.
func startDetail(status string) string {
	switch status {
	case StatusSkipped:
		return "its previous run was still going"
	case StatusOffline:
		return "the host was not connected"
	}
	return ""
}

// Run is the caller's run id.
func (s *Store) Run(ctx context.Context, id int64) (Run, error) {
	return scanRun(s.pool.QueryRow(ctx, `SELECT `+runCols+` FROM host_job_runs WHERE id = $1`, id))
}

// Runs is the caller's job id's runs, newest first, at most limit.
func (s *Store) Runs(ctx context.Context, job int64, limit int) ([]Run, error) {
	return collectRuns(s.pool.Query(ctx, `SELECT `+runCols+` FROM host_job_runs WHERE job_id = $1 ORDER BY id DESC LIMIT $2`, job, limit))
}

// Finish is how a run ended, as FinishRun records it.
type Finish struct {
	Status      string
	StartedAt   *time.Time
	EndedAt     time.Time
	ExitCode    *int
	DurationMs  *int64
	PageID      int64
	PageVersion int
	StderrTail  string
	Detail      string
}

// FinishRun moves the caller's RUNNING run id to f, and answers it — false
// when it was not running (finished already, by a late duplicate result or
// the lost sweep): exactly one ending is recorded, and only that caller acts
// on it.
func (s *Store) FinishRun(ctx context.Context, id int64, f Finish) (Run, bool, error) {
	var page, version any
	if f.PageID > 0 {
		page = f.PageID
	}
	if f.PageVersion > 0 {
		version = f.PageVersion
	}
	r, err := scanRun(s.pool.QueryRow(ctx, `UPDATE host_job_runs SET status = $2, started_at = $3, ended_at = $4, exit_code = $5,
		duration_ms = $6, page_id = $7, page_version = $8, stderr_tail = $9, detail = $10
		WHERE id = $1 AND status = 'running' RETURNING `+runCols,
		id, f.Status, f.StartedAt, f.EndedAt, f.ExitCode, f.DurationMs, page, version, CleanTail(f.StderrTail), CleanDetail(f.Detail)))
	if errors.Is(err, ErrNotFound) {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, err
	}
	return r, true, nil
}

// SetNotice records Attention item ask as run id's — once: false when the
// run has one already (another path raised it first; the caller withdraws
// its copy).
func (s *Store) SetNotice(ctx context.Context, id, ask int64) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE host_job_runs SET notice_id = $2 WHERE id = $1 AND notice_id IS NULL`, id, ask)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// LostRunGrace is how long past its timeout a running run waits for its
// result before LostRuns calls it lost: the host's own kill, its result's
// trip, and a reconnect.
const LostRunGrace = 5 * time.Minute

// LostRuns moves every owner's running run whose result is overdue at now
// (fired_at + its timeout + LostRunGrace) to lost, at most limit, and answers
// those this caller moved, each with its owner. Read and moved through sys:
// each is then acted on as its owner.
func (s *Store) LostRuns(ctx context.Context, now time.Time, limit int) ([]Run, error) {
	return collectRuns(s.pool.Query(sys(ctx), `UPDATE host_job_runs SET status = 'lost', ended_at = $1,
		detail = 'muster never got its result from the host'
		WHERE id IN (SELECT id FROM host_job_runs WHERE status = 'running'
			AND fired_at + make_interval(secs => timeout_s) + make_interval(secs => $2) < $1 ORDER BY id LIMIT $3)
		AND status = 'running' RETURNING `+runCols, now, LostRunGrace.Seconds(), limit))
}

// CleanTail is stderr as a run row keeps it: valid UTF-8 with no NUL, at most
// mirror.MaxJobStderrTail bytes (0157's CHECK), cut at its START (it is a
// tail).
func CleanTail(s string) string {
	s = strings.ReplaceAll(strings.ToValidUTF8(s, "?"), "\x00", "")
	for len(s) > mirror.MaxJobStderrTail {
		_, n := utf8.DecodeRuneInString(s)
		s = s[n:]
	}
	return s
}

// CleanDetail is a run's detail: valid UTF-8, at most 1000 characters
// (0157's CHECK).
func CleanDetail(s string) string {
	s = strings.ReplaceAll(strings.ToValidUTF8(s, "?"), "\x00", "")
	if utf8.RuneCountInString(s) > 1000 {
		s = string([]rune(s)[:1000])
	}
	return s
}
