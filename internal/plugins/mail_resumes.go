package plugins

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// RESUMING AN ENDED SESSION FOR ITS MAIL — the durable record and its limits
// (migration 0056): docs/plugins.md#resuming-an-ended-session.
//
// 🔴 EVERY COUNT IS THE PRINCIPAL'S, SPELLED IN THE QUERY. The table is under
// FORCE row-level security as well, but a limit that holds only while RLS
// applies to the connecting role is a limit a superuser connection silently
// lifts; the owner is named in each WHERE so the two cannot disagree.

var (
	// ErrResumeClaimed: the message already has a resume row — a message
	// resumes a session at most once, however often it is dispatched.
	ErrResumeClaimed = errors.New("plugins: this message already resumed (or tried to resume) a session")
	// ErrResumeLimited: a rate limit refused it (per session, or per host).
	ErrResumeLimited = errors.New("plugins: too many sessions resumed in the last hour")
)

// The states of a mail_resumes row.
const (
	ResumeClaimed   = "claimed"
	ResumeSpawned   = "spawned"
	ResumeDelivered = "delivered"
	ResumeTimedOut  = "timed_out"
	ResumeRefused   = "refused"
)

// MailResumeClaim is what a resume is for.
type MailResumeClaim struct {
	MessageID     int64
	ClaudeSession string
	Sender        string
	// Via is RouteViaThread or RouteViaPR.
	Via string
}

// MailResumeStore is mail_resumes. Every method runs under the caller's
// principal.
type MailResumeStore interface {
	// ClaimMailResume records that c's message is about to resume its session:
	// ErrResumeClaimed when the message already has a row, ErrResumeLimited
	// when the session was resumed perSessionPerHour times in the last hour.
	// It answers the row's id.
	ClaimMailResume(ctx context.Context, c MailResumeClaim, perSessionPerHour int) (int64, error)
	// ReserveMailResumeHost records host as the host asked to start the
	// window for row id, now — or ErrResumeLimited when host already started
	// perHostPerHour in the last hour. A reserved row counts toward both
	// limits.
	ReserveMailResumeHost(ctx context.Context, id int64, host string, perHostPerHour int) error
	// ReleaseMailResumeHost undoes a reservation whose host certainly started
	// nothing (it answered a refusal, or was never asked), so it counts toward
	// no limit. A request with no answer keeps its reservation.
	ReleaseMailResumeHost(ctx context.Context, id int64) error
	// FinishMailResume records how row id ended: its state, the window it
	// started ("" for none) and why (cut to fit).
	FinishMailResume(ctx context.Context, id int64, state, window, detail string) error
}

// principalOwner is the principal's user id as the queries read it.
const principalOwner = `nullif(current_setting('muster.uid', true), '')::bigint`

// lockResumes serialises the limit checks of one owner within tx.
func lockResumes(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('mail_resumes'), coalesce(`+principalOwner+`, 0)::int)`)
	return err
}

// ClaimMailResume: see MailResumeStore.
func (s *PGStore) ClaimMailResume(ctx context.Context, c MailResumeClaim, perSessionPerHour int) (int64, error) {
	if c.MessageID <= 0 || c.ClaudeSession == "" || c.Sender == "" || (c.Via != RouteViaThread && c.Via != RouteViaPR) {
		return 0, invalid("not a well-formed resume claim")
	}
	var id int64
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockResumes(ctx, tx); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM mail_resumes WHERE owner_id = `+principalOwner+`
			AND claude_session = $1 AND spawned_at > now() - interval '1 hour'`, c.ClaudeSession).Scan(&n); err != nil {
			return err
		}
		if n >= perSessionPerHour {
			return ErrResumeLimited
		}
		err := tx.QueryRow(ctx, `INSERT INTO mail_resumes (plugin_message_id, claude_session, sender, via)
			VALUES ($1, $2, $3, $4) ON CONFLICT (owner_id, plugin_message_id) DO NOTHING RETURNING id`,
			c.MessageID, c.ClaudeSession, cut(c.Sender, 320), c.Via).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrResumeClaimed
		}
		return err
	})
	return id, err
}

// ReserveMailResumeHost: see MailResumeStore.
func (s *PGStore) ReserveMailResumeHost(ctx context.Context, id int64, host string, perHostPerHour int) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockResumes(ctx, tx); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM mail_resumes WHERE owner_id = `+principalOwner+`
			AND host = $1 AND spawned_at > now() - interval '1 hour'`, host).Scan(&n); err != nil {
			return err
		}
		if n >= perHostPerHour {
			return ErrResumeLimited
		}
		tag, err := tx.Exec(ctx, `UPDATE mail_resumes SET host = $2, spawned_at = now(), state = 'spawned'
			WHERE id = $1 AND owner_id = `+principalOwner+` AND state = 'claimed'`, id, host)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrNotFound
		}
		return nil
	})
}

// ReleaseMailResumeHost: see MailResumeStore.
func (s *PGStore) ReleaseMailResumeHost(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE mail_resumes SET host = NULL, spawned_at = NULL, state = 'claimed'
		WHERE id = $1 AND owner_id = `+principalOwner+` AND state = 'spawned' AND window_id IS NULL`, id)
	return err
}

// FinishMailResume: see MailResumeStore.
func (s *PGStore) FinishMailResume(ctx context.Context, id int64, state, window, detail string) error {
	switch state {
	case ResumeDelivered, ResumeTimedOut, ResumeRefused, ResumeSpawned:
	default:
		return invalid("not a resume outcome")
	}
	var w *string
	if window != "" {
		w = &window
	}
	var fin *time.Time
	if state != ResumeSpawned {
		now := time.Now()
		fin = &now
	}
	_, err := s.pool.Exec(ctx, `UPDATE mail_resumes SET state = $2, window_id = coalesce($3, window_id), detail = $4,
		finished_at = $5 WHERE id = $1 AND owner_id = `+principalOwner, id, state, w, cut(detail, 1000), fin)
	return err
}

// cut is s cut to at most n bytes on a rune boundary.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !isRuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
