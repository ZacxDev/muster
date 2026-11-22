// Package sessionwake is the store of the lines muster owes a MIRRORED Claude
// Code session because its owner decided something the session was waiting on
// (migration 0177, session_wakes; docs/attention.md "Delivery"): an ask it
// raised was answered or dismissed, a reply draft it wrote was decided and
// later sent or not, or a page it made got a response.
//
// The delivery itself — resolving the session's window, queueing the line in
// the window's send queue, the paste's gate and outcome — lives in
// internal/api (session_wake.go). This package holds the rows and their moves.
//
// 🔴 THE STATES ARE THE ANSWER DELIVERY'S (internal/attention, 0031) and every
// move is ONE conditional UPDATE on the state it leaves, so of two callers
// exactly one moves a row: queued → delivering (Hand: the send-queue row that
// carries it) → delivered | unconfirmed (Finish, only for that row); queued →
// unconfirmed (GiveUp, a day queued); delivering → queued (Requeue, the row
// provably pasted nothing); queued | delivering | unconfirmed → waited
// (MarkWaited: the session read it itself); unconfirmed → queued (Retry, the
// owner's).
//
// 🔴 THE ROW HOLDS THE PARTS OF THE LINE, NEVER THE LINE. Only internal/api's
// renderWake turns a row into text, and the one kind that carries other
// viewers' responses (KindPageResponses) holds a count and no text — the
// database refuses one with text (0177's session_wakes_count_has_no_text).
//
// PRINCIPALS. Create, AddPageResponses, Get, ForAsks, ForDrafts, Retry and
// MarkWaited run under the caller's principal (row-level security: the
// owner's rows only). Due runs under the system scope and hands each row to a
// delivery that acts as its owner; the moves run under whatever principal the
// caller holds (the owner's, in internal/api).
package sessionwake

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
)

// Kind is what a wake is about.
type Kind string

const (
	// KindAskAnswered: the owner answered an ask the session raised.
	KindAskAnswered Kind = "ask_answered"
	// KindAskDismissed: the owner dismissed it without answering.
	KindAskDismissed Kind = "ask_dismissed"
	// KindDraftDecided: the owner approved or rejected a reply draft the
	// session wrote (a route's answer).
	KindDraftDecided Kind = "draft_decided"
	// KindDraftOutcome: an approved draft's send outcome (sent, failed,
	// unconfirmed).
	KindDraftOutcome Kind = "draft_outcome"
	// KindPageOwn: the owner responded on a page the session made, from
	// their own view.
	KindPageOwn Kind = "page_own_response"
	// KindPageResponses: OTHER viewers responded on it — a count, never
	// their words.
	KindPageResponses Kind = "page_responses"
)

// State is where a wake is (0177's header).
type State string

const (
	Queued      State = "queued"
	Delivering  State = "delivering"
	Delivered   State = "delivered"
	Unconfirmed State = "unconfirmed"
	Waited      State = "waited"
)

// Decisions a draft wake carries (Wake.Decision).
const (
	DecisionApproved    = "approved"
	DecisionRejected    = "rejected"
	DecisionSent        = "sent"
	DecisionFailed      = "failed"
	DecisionUnconfirmed = "unconfirmed"
)

// MaxOperatorTextBytes bounds Wake.OperatorText (0177's CHECK).
const MaxOperatorTextBytes = 16384

// MaxDetailRunes bounds Wake.Detail (0177's CHECK).
const MaxDetailRunes = 300

// Wake is one row.
type Wake struct {
	ID         int64
	Owner      int64
	CreatedAt  time.Time
	Kind       Kind
	AskID      int64
	DraftID    int64
	PageID     int64
	ResponseID int64
	// RouteID is a draft wake's route (muster's own id, rt_…), "" otherwise.
	RouteID string
	// Decision is a draft wake's: approved or rejected (KindDraftDecided),
	// sent, failed or unconfirmed (KindDraftOutcome).
	Decision string
	// Count is how many other viewers' responses a KindPageResponses wake
	// stands for.
	Count int
	// Component is a KindPageOwn wake's page component (its own CHECKed
	// shape).
	Component string
	// OperatorText is the OWNER'S OWN WORDS the line carries, fenced: an
	// ask's answer, the reason given with a rejection, a response sent from
	// their own view. "" for every other line.
	OperatorText string
	// Host and Session are the mirrored session it goes to: its host and its
	// Claude Code session id.
	Host        string
	Session     string
	State       State
	Detail      string
	Attempts    int
	QueuedAt    time.Time
	SendQueueID int64
	DeliveredAt time.Time
}

// New is a wake to create (Create): every Wake field but the delivery's.
type New struct {
	Kind         Kind
	AskID        int64
	DraftID      int64
	PageID       int64
	ResponseID   int64
	RouteID      string
	Decision     string
	Component    string
	OperatorText string
	Host         string
	Session      string
}

var (
	// ErrInvalid refuses a wake that is not one 0177 holds.
	ErrInvalid = errors.New("sessionwake: invalid wake")
	// ErrNotFound: no such wake the caller can see.
	ErrNotFound = errors.New("sessionwake: no such wake")
	// ErrNotRetryable: Retry of a wake that is not unconfirmed.
	ErrNotRetryable = errors.New("sessionwake: only a wake whose delivery failed can be retried")
)

// Store is the wakes' store as internal/api consumes it. *PGStore.
type Store interface {
	Create(ctx context.Context, n New) (Wake, bool, error)
	AddPageResponses(ctx context.Context, pageID int64, host, session string, n int) (Wake, error)
	Get(ctx context.Context, id int64) (Wake, error)
	ForAsks(ctx context.Context, ids []int64) (map[int64]Wake, error)
	ForDrafts(ctx context.Context, ids []int64) (map[int64]Wake, error)
	Due(ctx context.Context, limit int) ([]Wake, error)
	Hold(ctx context.Context, id int64, detail string) error
	Hand(ctx context.Context, id, queueID int64) (bool, error)
	Requeue(ctx context.Context, id, queueID int64, detail string) (bool, error)
	Finish(ctx context.Context, id, queueID int64, delivered bool, detail string) (bool, error)
	GiveUp(ctx context.Context, id int64, detail string) (bool, error)
	MarkWaited(ctx context.Context, askID int64) ([]Wake, error)
	Retry(ctx context.Context, id int64) (Wake, error)
	DraftOutcomesOwed(ctx context.Context, limit int) ([]OutcomeOwed, error)
}

// PGStore is the store on Postgres.
type PGStore struct{ pool *pgxpool.Pool }

// NewPG returns a store on pool.
func NewPG(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

const cols = `id, owner_id, created_at, kind, coalesce(ask_id, 0), coalesce(draft_id, 0), coalesce(page_id, 0),
	coalesce(response_id, 0), route_id, decision, response_count, component, operator_text, mirror_host, mirror_session,
	state, detail, attempts, queued_at, coalesce(send_queue_id, 0), coalesce(delivered_at, 'epoch'::timestamptz)`

func scan(row pgx.Row) (Wake, error) {
	var w Wake
	var kind, state string
	err := row.Scan(&w.ID, &w.Owner, &w.CreatedAt, &kind, &w.AskID, &w.DraftID, &w.PageID, &w.ResponseID, &w.RouteID,
		&w.Decision, &w.Count, &w.Component, &w.OperatorText, &w.Host, &w.Session, &state, &w.Detail, &w.Attempts,
		&w.QueuedAt, &w.SendQueueID, &w.DeliveredAt)
	w.Kind, w.State = Kind(kind), State(state)
	if w.DeliveredAt.Unix() == 0 {
		w.DeliveredAt = time.Time{}
	}
	return w, err
}

func collect(rows pgx.Rows, err error) ([]Wake, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Wake
	for rows.Next() {
		w, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// Valid reports whether n is a wake 0177 holds, checked here so a caller gets
// a reason instead of a constraint name.
func (n New) Valid() error {
	if err := mirror.ValidHost(n.Host); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if !mirror.ValidClaudeSession(n.Session) {
		return fmt.Errorf("%w: not a Claude Code session id", ErrInvalid)
	}
	if len(n.OperatorText) > MaxOperatorTextBytes {
		return fmt.Errorf("%w: the owner's text is over %d bytes", ErrInvalid, MaxOperatorTextBytes)
	}
	switch n.Kind {
	case KindAskAnswered, KindAskDismissed:
		if n.AskID <= 0 {
			return fmt.Errorf("%w: an ask wake names its ask", ErrInvalid)
		}
	case KindDraftDecided, KindDraftOutcome:
		if n.DraftID <= 0 || n.Decision == "" {
			return fmt.Errorf("%w: a draft wake names its draft and decision", ErrInvalid)
		}
	case KindPageOwn:
		if n.PageID <= 0 || n.ResponseID <= 0 {
			return fmt.Errorf("%w: an owner's response names its page and response", ErrInvalid)
		}
	default:
		// KindPageResponses is AddPageResponses' alone: its count folds.
		return fmt.Errorf("%w: kind %q", ErrInvalid, n.Kind)
	}
	return nil
}

func nullID(id int64) any {
	if id > 0 {
		return id
	}
	return nil
}

// Create records wake n for the caller's principal (its owner), queued. The
// bool is false — and the existing row is returned — when the same decision
// already has its wake (0177's one-per-decision indexes): a second answer path
// racing the first, or a repeat, owes nothing more.
func (s *PGStore) Create(ctx context.Context, n New) (Wake, bool, error) {
	if err := n.Valid(); err != nil {
		return Wake{}, false, err
	}
	w, err := scan(s.pool.QueryRow(ctx, `INSERT INTO session_wakes (kind, ask_id, draft_id, page_id, response_id, route_id,
			decision, component, operator_text, mirror_host, mirror_session)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) RETURNING `+cols,
		string(n.Kind), nullID(n.AskID), nullID(n.DraftID), nullID(n.PageID), nullID(n.ResponseID), n.RouteID, n.Decision,
		n.Component, n.OperatorText, n.Host, n.Session))
	if err == nil {
		return w, true, nil
	}
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "23505" {
		return Wake{}, false, err
	}
	var q string
	var arg any
	switch {
	case n.AskID > 0:
		q, arg = `ask_id = $1 AND kind = $2`, n.AskID
	case n.DraftID > 0:
		q, arg = `draft_id = $1 AND kind = $2`, n.DraftID
	default:
		q, arg = `response_id = $1 AND kind = $2`, n.ResponseID
	}
	w, err = scan(s.pool.QueryRow(ctx, `SELECT `+cols+` FROM session_wakes WHERE `+q, arg, string(n.Kind)))
	return w, false, err
}

// AddPageResponses records n more of OTHER viewers' responses to page pageID
// for the session that made it: folded into the page's one queued count line
// when there is one (0177's session_wakes_one_queued_count), else a new one.
// It carries a count and nothing else.
func (s *PGStore) AddPageResponses(ctx context.Context, pageID int64, host, session string, n int) (Wake, error) {
	if pageID <= 0 || n <= 0 {
		return Wake{}, fmt.Errorf("%w: a page and a positive count", ErrInvalid)
	}
	if err := mirror.ValidHost(host); err != nil {
		return Wake{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if !mirror.ValidClaudeSession(session) {
		return Wake{}, fmt.Errorf("%w: not a Claude Code session id", ErrInvalid)
	}
	return scan(s.pool.QueryRow(ctx, `INSERT INTO session_wakes (kind, page_id, response_count, mirror_host, mirror_session)
		VALUES ('page_responses', $1, $2, $3, $4)
		ON CONFLICT (page_id) WHERE kind = 'page_responses' AND state = 'queued'
		DO UPDATE SET response_count = least(session_wakes.response_count + EXCLUDED.response_count, 100000)
		RETURNING `+cols, pageID, n, host, session))
}

// Get is wake id, as the caller sees it.
func (s *PGStore) Get(ctx context.Context, id int64) (Wake, error) {
	w, err := scan(s.pool.QueryRow(ctx, `SELECT `+cols+` FROM session_wakes WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Wake{}, ErrNotFound
	}
	return w, err
}

// ForAsks is each of asks ids' wake (its answer's or its dismissal's), by ask.
func (s *PGStore) ForAsks(ctx context.Context, ids []int64) (map[int64]Wake, error) {
	if len(ids) == 0 {
		return map[int64]Wake{}, nil
	}
	ws, err := collect(s.pool.Query(ctx, `SELECT `+cols+` FROM session_wakes WHERE ask_id = ANY($1) ORDER BY id`, ids))
	if err != nil {
		return nil, err
	}
	out := map[int64]Wake{}
	for _, w := range ws {
		out[w.AskID] = w
	}
	return out, nil
}

// ForDrafts is each of drafts ids' latest wake (its outcome's once there is
// one, else its decision's), by draft.
func (s *PGStore) ForDrafts(ctx context.Context, ids []int64) (map[int64]Wake, error) {
	if len(ids) == 0 {
		return map[int64]Wake{}, nil
	}
	ws, err := collect(s.pool.Query(ctx, `SELECT `+cols+` FROM session_wakes WHERE draft_id = ANY($1) ORDER BY id`, ids))
	if err != nil {
		return nil, err
	}
	out := map[int64]Wake{}
	for _, w := range ws {
		out[w.DraftID] = w // ordered by id: the latest wins
	}
	return out, nil
}

// Due is every owner's owed (queued) and in-flight (delivering) wakes, oldest
// first, at most limit (system scope: the sweep acts on each as its owner).
func (s *PGStore) Due(ctx context.Context, limit int) ([]Wake, error) {
	return collect(s.pool.Query(authz.System(ctx), `SELECT `+cols+` FROM session_wakes
		WHERE state IN ('queued', 'delivering') ORDER BY attempts, queued_at, id LIMIT $1`, limit))
}

func cut(detail string) string {
	detail = strings.TrimSpace(detail)
	if utf8.RuneCountInString(detail) <= MaxDetailRunes {
		return detail
	}
	return string([]rune(detail)[:MaxDetailRunes-1]) + "…"
}

// Hold records why queued wake id still waits (detail), leaving it queued.
func (s *PGStore) Hold(ctx context.Context, id int64, detail string) error {
	_, err := s.pool.Exec(ctx, `UPDATE session_wakes SET detail = $2 WHERE id = $1 AND state = 'queued'`, id, cut(detail))
	return err
}

// Hand moves queued wake id to delivering on send-queue row queueID: false
// when it is no longer queued (another sweep handed it, or the session read
// it).
func (s *PGStore) Hand(ctx context.Context, id, queueID int64) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE session_wakes SET state = 'delivering', send_queue_id = $2, detail = '',
		  attempts = attempts + 1 WHERE id = $1 AND state = 'queued'`, id, queueID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// Requeue moves wake id back to queued from delivering on row queueID — that
// row provably pasted nothing — saying why (detail).
func (s *PGStore) Requeue(ctx context.Context, id, queueID int64, detail string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE session_wakes SET state = 'queued', send_queue_id = NULL, detail = $3
		WHERE id = $1 AND state = 'delivering' AND send_queue_id = $2`, id, queueID, cut(detail))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// Finish settles wake id from delivering on row queueID: delivered, or
// unconfirmed saying why. False when the wake is no longer that row's (the
// session read it meanwhile, or it already settled).
func (s *PGStore) Finish(ctx context.Context, id, queueID int64, delivered bool, detail string) (bool, error) {
	state := Unconfirmed
	if delivered {
		state, detail = Delivered, ""
	}
	tag, err := s.pool.Exec(ctx, `UPDATE session_wakes SET state = $3, detail = $4,
		  delivered_at = CASE WHEN $3 = 'delivered' THEN now() END
		WHERE id = $1 AND state = 'delivering' AND send_queue_id = $2`, id, queueID, string(state), cut(detail))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// GiveUp settles queued wake id as unconfirmed, saying why (a day queued).
func (s *PGStore) GiveUp(ctx context.Context, id int64, detail string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE session_wakes SET state = 'unconfirmed', detail = $2 WHERE id = $1 AND state = 'queued'`,
		id, cut(detail))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// MarkWaited records that the session read ask askID's answer (or dismissal)
// itself — attention_wait returned it — so its wake is never pasted: queued,
// delivering and unconfirmed all move to waited. It returns the rows that
// moved (one that was delivering names the queue row the caller cancels; the
// paste's gate refuses it anyway).
func (s *PGStore) MarkWaited(ctx context.Context, askID int64) ([]Wake, error) {
	return collect(s.pool.Query(ctx, `UPDATE session_wakes SET state = 'waited', detail = ''
		WHERE ask_id = $1 AND state IN ('queued', 'delivering', 'unconfirmed') RETURNING `+cols, askID))
}

// Retry queues unconfirmed wake id once more (the owner's Retry): ErrNotRetryable
// for any other state, with the wake as it stands.
func (s *PGStore) Retry(ctx context.Context, id int64) (Wake, error) {
	w, err := scan(s.pool.QueryRow(ctx, `UPDATE session_wakes SET state = 'queued', detail = '', queued_at = now(), send_queue_id = NULL
		WHERE id = $1 AND state = 'unconfirmed' RETURNING `+cols, id))
	if errors.Is(err, pgx.ErrNoRows) {
		cur, gerr := s.Get(ctx, id)
		if gerr != nil {
			return Wake{}, gerr
		}
		return cur, ErrNotRetryable
	}
	return w, err
}

// OutcomeOwed is one approved draft whose send outcome its session has not
// been told yet (DraftOutcomesOwed).
type OutcomeOwed struct {
	Owner   int64
	DraftID int64
}

// DraftOutcomesOwed is every owner's drafts whose approval the owner decided
// (a draft_decided wake, approved) that have since settled — sent,
// unconfirmed, or failed with no retry after it — and have no outcome wake
// yet, at most limit, over the last 8 days (system scope: the sweep files each
// as its owner). It catches every path a draft settles by — the plugin's
// result, a lease that lapsed — without a hook in each.
func (s *PGStore) DraftOutcomesOwed(ctx context.Context, limit int) ([]OutcomeOwed, error) {
	rows, err := s.pool.Query(authz.System(ctx), `SELECT w.owner_id, w.draft_id FROM session_wakes w
		JOIN plugin_reply_drafts d ON d.id = w.draft_id AND d.owner_id = w.owner_id
		WHERE w.kind = 'draft_decided' AND w.decision = 'approved' AND w.created_at > now() - interval '8 days'
		  AND (d.status IN ('sent', 'unconfirmed')
		    OR (d.status = 'failed' AND NOT EXISTS (SELECT 1 FROM plugin_reply_drafts n WHERE n.retry_of = d.id)))
		  AND NOT EXISTS (SELECT 1 FROM session_wakes o WHERE o.draft_id = w.draft_id AND o.kind = 'draft_outcome')
		ORDER BY w.id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutcomeOwed
	for rows.Next() {
		var o OutcomeOwed
		if err := rows.Scan(&o.Owner, &o.DraftID); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
