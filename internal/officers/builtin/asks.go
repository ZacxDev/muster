package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ZacxDev/muster/internal/askimage"
	"github.com/ZacxDev/muster/internal/officers"
)

// --- asks: officer_ask_person on a builtin officer ----------------------------
//
// HQ's asks semantics, delivered by muster (docs/design/officers-first-class.md
// "Day-1 scope of the built-in backend"):
//
//   - the instance holding an item under a live lease asks a question;
//     muster raises it as an Attention ask to the OFFICER'S OWNER (the
//     Attention view and their phone), through an Asker — `to` is not a
//     routing input here: there is no HQ member directory, only the owner;
//   - asking ENDS the lease: the item is `waiting` with no holder, the ask
//     recorded on it (officer_items.asks, migration 0120);
//   - once no ask on it is open — each answered or withdrawn — the item is
//     re-opened with the answers on it, and the wake loop wakes an instance
//     for it; an ask that EXPIRES unanswered escalates the item to its owner
//     (Store.ResolveAsk, driven by internal/api's sweep).

// MaxAsksPerItem bounds the asks one item may carry (0120's CHECK is 20).
const MaxAsksPerItem = 20

// Asker raises, as the officer's owner, the Attention ask an instance's
// officer_ask_person becomes, and withdraws one whose item could not then be
// set waiting. internal/api implements it over its attention store. Raise's
// refusals are the contract's decisions (*officers.HQError: 400 for a bound
// the question breaks, 409 when the owner's open asks are at their cap);
// anything else is an outage.
type Asker interface {
	Raise(ctx context.Context, o officers.Officer, question string, options []string, timeout time.Duration) (askID int64, err error)
	Withdraw(ctx context.Context, o officers.Officer, askID int64)
}

// WithAsker sets the Asker b raises asks through, and returns b.
func (b *Backend) WithAsker(a Asker) *Backend { b.asker = a; return b }

var _ officers.AsksBackend = (*Backend)(nil)

// askEntry is one element of officer_items.asks.
type askEntry struct {
	AskID      int64      `json:"ask_id"`
	Question   string     `json:"question"`
	Options    []string   `json:"options,omitempty"`
	AskedBy    string     `json:"asked_by"`
	AskedAt    time.Time  `json:"asked_at"`
	Status     string     `json:"status"`
	Answer     string     `json:"answer,omitempty"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
}

// The statuses of an askEntry.
const (
	AskOpen      = "open"
	AskAnswered  = "answered"
	AskWithdrawn = "withdrawn"
	AskExpired   = "expired"
)

// askDeadline is the ask's timeout from the instance's deadline_seconds
// (absent: 0, the Attention default).
func askDeadline(raw json.RawMessage) (time.Duration, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, nil
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || n <= 0 {
		return 0, refuse(http.StatusBadRequest, "invalid", "deadline_seconds must be a positive whole number of seconds", nil)
	}
	return time.Duration(n) * time.Second, nil
}

// CreateAsk implements officers.AsksBackend: the holder of a live lease on
// item asks the officer's owner a.Question (with a.Options). The ask is raised
// while the item is locked, then the item is set waiting with no holder in
// the same transaction; a commit that fails after the raise withdraws the ask.
func (b *Backend) CreateAsk(ctx context.Context, item officers.ItemID, instance string, a officers.Ask) ([]byte, error) {
	q := strings.TrimSpace(a.Question)
	switch {
	case q == "":
		return nil, refuse(http.StatusBadRequest, "invalid", "a question is required", nil)
	case len(a.Images) > 0:
		return nil, refuse(http.StatusBadRequest, "invalid", "images are not served by the built-in backend: describe what the owner must look at in the question", nil)
	}
	timeout, err := askDeadline(a.DeadlineSeconds)
	if err != nil {
		return nil, err
	}
	if b.asker == nil {
		return nil, outage("ask", errors.New("no Attention store to raise the ask in"))
	}
	var raised int64
	out, err := b.tx(ctx, "ask", func(tx pgx.Tx) ([]byte, error) {
		it, err := b.lockItem(ctx, tx, item)
		if err != nil {
			return nil, err
		}
		now := b.now()
		if !it.live(now) || it.ClaimedBy != instance {
			return nil, leaseLost()
		}
		var asks []askEntry
		_ = json.Unmarshal(it.Asks, &asks)
		if len(asks) >= MaxAsksPerItem {
			return nil, refuse(http.StatusConflict, "too_many_asks", fmt.Sprintf("the item already carries %d asks", MaxAsksPerItem), nil)
		}
		text := askQuestion(b.officer.ID, it, q)
		raised, err = b.asker.Raise(ctx, b.officer, text, a.Options, timeout)
		if err != nil {
			raised = 0
			return nil, err
		}
		entry, _ := json.Marshal([]askEntry{{AskID: raised, Question: q, Options: a.Options, AskedBy: instance, AskedAt: now, Status: AskOpen}})
		it, err = scanItem(tx.QueryRow(ctx, `UPDATE officer_items SET status = 'waiting', claimed_by = NULL, lease_expires_at = NULL,
			asks = asks || $2::jsonb, updated_at = $3 WHERE id = $1 RETURNING `+itemCols, it.ID, entry, now))
		if err != nil {
			return nil, err
		}
		return answer(map[string]any{"ask": map[string]any{"id": raised, "to": "the officer's owner", "delivery": "attention", "status": AskOpen}, "item": it})
	})
	if err != nil && raised != 0 {
		b.asker.Withdraw(context.WithoutCancel(ctx), b.officer, raised)
	}
	return out, err
}

// askQuestion is the Attention ask's text: who asks about which item, then
// the instance's question as it gave it (bounded by Attention's own limit).
func askQuestion(officer string, it itemRow, q string) string {
	head := fmt.Sprintf("Officer %s asks about item %d (%s %s):\n\n", officer, it.ID, it.Kind, it.SubjectKey)
	const max = 4000
	if utf8.RuneCountInString(head)+utf8.RuneCountInString(q) > max {
		r := []rune(q)
		q = string(r[:max-utf8.RuneCountInString(head)-1]) + "…"
	}
	return head + q
}

// UploadAskImage implements officers.AsksBackend: the built-in backend keeps
// no images (refused, never stored).
func (b *Backend) UploadAskImage(context.Context, string, string, []byte) (askimage.Record, error) {
	return askimage.Record{}, refuse(http.StatusBadRequest, "invalid", "images are not served by the built-in backend", nil)
}

// OpenAsk is one open ask on a waiting item (Store.OpenAsks).
type OpenAsk struct {
	Owner   int64
	Item    int64
	Officer string
	AskID   int64
}

// OpenAsks is every owner's open asks on waiting items: one PAGE of at most
// limit items with an id above after, oldest first (every open ask of each).
// The caller pages on with the last item's id, so asks still awaiting an
// answer can never crowd settled ones out of a sweep. ctx must act for every
// owner (the system scope): it is the sweep that settles them.
//
// 🔴 ONLY ITEMS WITH AN OPEN ASK FILL THE PAGE. An item can be waiting with
// no ask at all (on the outbound draft filed for it, outbound.go); counted
// in the page it returns no row, and the caller, reading a short page as the
// last one, stopped before the items behind it.
func (s *Store) OpenAsks(ctx context.Context, after int64, limit int) ([]OpenAsk, error) {
	rows, err := s.pool.Query(ctx, `SELECT i.owner_id, i.id, i.officer, (e->>'ask_id')::bigint
		FROM officer_items i, jsonb_array_elements(i.asks) e
		WHERE i.id IN (SELECT id FROM officer_items WHERE status = 'waiting' AND asks @> '[{"status": "open"}]'
		                AND id > $2 ORDER BY id LIMIT $1)
			AND e->>'status' = 'open' ORDER BY i.id`, limit, after)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (OpenAsk, error) {
		var a OpenAsk
		err := r.Scan(&a.Owner, &a.Item, &a.Officer, &a.AskID)
		return a, err
	})
}

// Settlement is what Store.ResolveAsk did to the item.
type Settlement string

const (
	// Settled: the ask is recorded and others on the item are still open.
	Settled Settlement = "settled"
	// Reopened: no ask is open any more; the item is open again.
	Reopened Settlement = "reopened"
	// Escalated: the ask expired unanswered; the item is escalated to its
	// owner (with its notices).
	Escalated Settlement = "escalated"
	// Stale: the ask was already settled, or the item is no longer waiting.
	Stale Settlement = "stale"
)

// ResolveAsk records the settled Attention ask askID of owner's waiting item
// — status AskAnswered (with answer), AskWithdrawn or AskExpired — and moves
// the item: re-opened once no ask on it is open, escalated when the ask
// EXPIRED (HQ's rule: an ask nobody answered in time goes to the owner).
// One transaction, each statement conditional on the item still waiting and
// the ask still open, so two replicas settle it once.
func (s *Store) ResolveAsk(ctx context.Context, owner, item, askID int64, status, answerText string, now time.Time) (Settlement, error) {
	switch status {
	case AskAnswered, AskWithdrawn, AskExpired:
	default:
		return "", fmt.Errorf("builtin: ask status %q is not a settlement", status)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var officer string
	err = tx.QueryRow(ctx, `UPDATE officer_items SET updated_at = $6, asks = (
			SELECT jsonb_agg(CASE WHEN (e->>'ask_id')::bigint = $3 AND e->>'status' = 'open'
				THEN e || jsonb_build_object('status', $4::text, 'answer', $5::text, 'resolved_at', $6::timestamptz) ELSE e END ORDER BY ord)
			FROM jsonb_array_elements(asks) WITH ORDINALITY t(e, ord))
		WHERE owner_id = $1 AND id = $2 AND status = 'waiting'
			AND asks @> jsonb_build_array(jsonb_build_object('ask_id', $3::bigint, 'status', 'open'))
		RETURNING officer`, owner, item, askID, status, answerText, now).Scan(&officer)
	if errors.Is(err, pgx.ErrNoRows) {
		return Stale, nil
	}
	if err != nil {
		return "", err
	}
	out := Settled
	if status == AskExpired {
		notices, err := noticesFor(ctx, tx, owner, officer)
		if err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx, `UPDATE officer_items SET status = 'escalated', outcome = 'escalated', notices = $3,
			summary = 'An ask to the officer''s owner expired unanswered, so the item is escalated to them.', updated_at = $4
			WHERE owner_id = $1 AND id = $2`, owner, item, notices, now); err != nil {
			return "", err
		}
		out = Escalated
	} else {
		tag, err := tx.Exec(ctx, `UPDATE officer_items SET status = 'open', updated_at = $3
			WHERE owner_id = $1 AND id = $2 AND status = 'waiting' AND NOT asks @> '[{"status": "open"}]'`, owner, item, now)
		if err != nil {
			return "", err
		}
		if tag.RowsAffected() == 1 {
			out = Reopened
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return out, nil
}

// --- escalation notices ------------------------------------------------------

// noticeEntry is one element of officer_items.notices: one recipient of an
// escalation. AttentionID is the Attention ask muster raised for it (null
// until raised); Settled is false until that ask is answered, dismissed or
// expired (or withdrawn because another recipient's answer re-opened the
// item).
type noticeEntry struct {
	Recipient   int64  `json:"recipient"`
	Suppressed  bool   `json:"suppressed"`
	AttentionID *int64 `json:"attention_id"`
	Settled     bool   `json:"settled"`
}

// noticesFor is who an escalation of officer's item notifies: its owner and
// its escalate_to, once each, suppressed (recorded, never delivered) when the
// officer is in shadow mode. Read inside the escalating transaction.
func noticesFor(ctx context.Context, tx pgx.Tx, owner int64, officer string) (json.RawMessage, error) {
	var mode string
	var esc []int64
	err := tx.QueryRow(ctx, `SELECT mode, escalate_to FROM officer_defs WHERE owner_id = $1 AND officer = $2`, owner, officer).Scan(&mode, &esc)
	if errors.Is(err, pgx.ErrNoRows) {
		mode, esc = "live", nil
	} else if err != nil {
		return nil, err
	}
	seen := map[int64]bool{}
	var out []noticeEntry
	for _, r := range append([]int64{owner}, esc...) {
		if r <= 0 || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, noticeEntry{Recipient: r, Suppressed: mode == "shadow", Settled: mode == "shadow"})
	}
	return json.Marshal(out)
}

// DueNotice is one escalation notice muster has not raised yet.
type DueNotice struct {
	Owner, Item, Recipient             int64
	Officer, Kind, SubjectKey, Summary string
}

// DueNotices is every owner's escalation notices not yet raised (and not
// suppressed) on items still escalated: one page of at most limit items with
// an id above after, oldest first (OpenAsks' paging). ctx must act for every
// owner (the system scope).
func (s *Store) DueNotices(ctx context.Context, after int64, limit int) ([]DueNotice, error) {
	rows, err := s.pool.Query(ctx, `SELECT i.owner_id, i.id, (n->>'recipient')::bigint, i.officer, i.kind, i.subject_key, coalesce(i.summary, '')
		FROM officer_items i, jsonb_array_elements(i.notices) n
		WHERE i.id IN (SELECT id FROM officer_items WHERE notices @> '[{"attention_id": null, "suppressed": false}]' AND status = 'escalated'
				AND id > $2 ORDER BY id LIMIT $1)
			AND n->'attention_id' = 'null'::jsonb AND n->'suppressed' = 'false'::jsonb
		ORDER BY i.id`, limit, after)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (DueNotice, error) {
		var d DueNotice
		err := r.Scan(&d.Owner, &d.Item, &d.Recipient, &d.Officer, &d.Kind, &d.SubjectKey, &d.Summary)
		return d, err
	})
}

// NoticeRaised records that recipient's notice of owner's item was raised as
// Attention ask attentionID — ONLY while it is still unraised, so of two
// replicas raising one notice exactly one records it (won); the loser
// withdraws its duplicate. ctx must act for every owner (the recipient need
// not be the item's owner).
func (s *Store) NoticeRaised(ctx context.Context, owner, item, recipient, attentionID int64) (won bool, err error) {
	tag, err := s.pool.Exec(ctx, `UPDATE officer_items SET notices = (
			SELECT jsonb_agg(CASE WHEN (n->>'recipient')::bigint = $3 AND n->'attention_id' = 'null'::jsonb AND n->'settled' = 'false'::jsonb
				THEN n || jsonb_build_object('attention_id', $4::bigint) ELSE n END ORDER BY ord)
			FROM jsonb_array_elements(notices) WITH ORDINALITY t(n, ord))
		WHERE owner_id = $1 AND id = $2 AND notices @> jsonb_build_array(jsonb_build_object('recipient', $3::bigint, 'attention_id', null, 'settled', false))`,
		owner, item, recipient, attentionID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// --- an escalation's answer flows back to the officer -------------------------
//
// An escalation is raised as an Attention ASK to each recipient (the owner and
// escalate_to): the officer is asking a human to act. A recipient who ANSWERS
// sends the item back to the officer — re-opened as a judgement item with the
// answer on its asks[] — and the other recipients' asks are withdrawn. A
// dismissed or expired escalation leaves the item escalated: nobody sent it
// back.

// OpenEscalation is one raised escalation ask not yet settled.
type OpenEscalation struct {
	Owner, Item, Recipient, AttentionID int64
	Officer                             string
}

// OpenEscalations is every owner's raised, unsettled escalation asks: one
// page of at most limit items with an id above after, oldest first (OpenAsks'
// paging). ctx must act for every owner (the system scope).
func (s *Store) OpenEscalations(ctx context.Context, after int64, limit int) ([]OpenEscalation, error) {
	rows, err := s.pool.Query(ctx, `SELECT i.owner_id, i.id, (n->>'recipient')::bigint, (n->>'attention_id')::bigint, i.officer
		FROM officer_items i, jsonb_array_elements(i.notices) n
		WHERE i.id IN (SELECT id FROM officer_items WHERE notices @> '[{"settled": false}]' AND id > $2
				AND EXISTS (SELECT 1 FROM jsonb_array_elements(notices) x WHERE x->'settled' = 'false'::jsonb AND x->'attention_id' <> 'null'::jsonb)
				ORDER BY id LIMIT $1)
			AND n->'settled' = 'false'::jsonb AND n->'attention_id' <> 'null'::jsonb
		ORDER BY i.id`, limit, after)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (OpenEscalation, error) {
		var e OpenEscalation
		err := r.Scan(&e.Owner, &e.Item, &e.Recipient, &e.AttentionID, &e.Officer)
		return e, err
	})
}

// SettleEscalation records how recipient's escalation ask attentionID of
// owner's item settled. answered (with answerText) re-opens the item for the
// officer — status open, judgement tier, no outcome, the answer appended to
// its asks[] — ONLY while it is still escalated and no other live item holds
// its subject; withdraw then names the other recipients' still-open asks to
// withdraw. Any other settlement (dismissed, expired) only marks the
// recipient settled. One transaction, conditional on the entry being
// unsettled, so two replicas settle it once. ctx must act for every owner
// (the recipient need not be the owner).
func (s *Store) SettleEscalation(ctx context.Context, owner, item, recipient, attentionID int64, answered bool, answerText string, now time.Time) (reopened bool, withdraw []OpenEscalation, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var officer, status string
	err = tx.QueryRow(ctx, `UPDATE officer_items SET notices = (
			SELECT jsonb_agg(CASE WHEN (n->>'recipient')::bigint = $3 AND (n->>'attention_id')::bigint = $4
				THEN n || '{"settled": true}'::jsonb ELSE n END ORDER BY ord)
			FROM jsonb_array_elements(notices) WITH ORDINALITY t(n, ord))
		WHERE owner_id = $1 AND id = $2
			AND notices @> jsonb_build_array(jsonb_build_object('recipient', $3::bigint, 'attention_id', $4::bigint, 'settled', false))
		RETURNING officer, status`, owner, item, recipient, attentionID).Scan(&officer, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	if answered && status == officers.StatusEscalated {
		entry, _ := json.Marshal([]askEntry{{AskID: attentionID, Question: "escalation", AskedBy: fmt.Sprintf("user %d", recipient),
			AskedAt: now, Status: AskAnswered, Answer: answerText, ResolvedAt: &now}})
		// A savepoint: re-opening fails on the live-subject index when another
		// live item now holds this subject, and that must not undo the
		// settlement above.
		if _, err := tx.Exec(ctx, `SAVEPOINT reopen`); err != nil {
			return false, nil, err
		}
		_, rerr := tx.Exec(ctx, `UPDATE officer_items SET status = 'open', outcome = NULL, tier = 'judgement', asks = asks || $3::jsonb,
			notices = (SELECT jsonb_agg(CASE WHEN n->'settled' = 'false'::jsonb AND n->'attention_id' = 'null'::jsonb
				THEN n || '{"settled": true}'::jsonb ELSE n END ORDER BY ord) FROM jsonb_array_elements(notices) WITH ORDINALITY t(n, ord)),
			updated_at = $4 WHERE owner_id = $1 AND id = $2`, owner, item, entry, now)
		var pg *pgconn.PgError
		switch {
		case rerr == nil:
			reopened = true
		case errors.As(rerr, &pg) && (pg.Code == "23505" || pg.Code == "23514"):
			// 23505: another live item now holds the subject; 23514: the item
			// already carries its 20 asks. Either way the re-open cannot land,
			// and the settlement above must still commit.
			if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT reopen`); err != nil {
				return false, nil, err
			}
		default:
			return false, nil, rerr
		}
		if reopened {
			rows, err := tx.Query(ctx, `SELECT (n->>'recipient')::bigint, (n->>'attention_id')::bigint FROM officer_items i, jsonb_array_elements(i.notices) n
				WHERE i.owner_id = $1 AND i.id = $2 AND n->'settled' = 'false'::jsonb AND n->'attention_id' <> 'null'::jsonb`, owner, item)
			if err != nil {
				return false, nil, err
			}
			withdraw, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (OpenEscalation, error) {
				e := OpenEscalation{Owner: owner, Item: item, Officer: officer}
				err := r.Scan(&e.Recipient, &e.AttentionID)
				return e, err
			})
			if err != nil {
				return false, nil, err
			}
			if _, err := tx.Exec(ctx, `UPDATE officer_items SET notices = (SELECT jsonb_agg(n || '{"settled": true}'::jsonb ORDER BY ord)
				FROM jsonb_array_elements(notices) WITH ORDINALITY t(n, ord)) WHERE owner_id = $1 AND id = $2`, owner, item); err != nil {
				return false, nil, err
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, nil, err
	}
	return reopened, withdraw, nil
}

// NoticeAbandoned gives up recipient's unraised escalation entry on owner's
// item: muster could not raise it (the recipient is not a user it can ask), so
// it is marked suppressed and settled and never retried — one recipient that
// cannot be asked must not hold the others' escalations, or this one's
// retries, forever. ctx must act for every owner.
func (s *Store) NoticeAbandoned(ctx context.Context, owner, item, recipient int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE officer_items SET notices = (
			SELECT jsonb_agg(CASE WHEN (n->>'recipient')::bigint = $3 AND n->'attention_id' = 'null'::jsonb
				THEN n || '{"suppressed": true, "settled": true}'::jsonb ELSE n END ORDER BY ord)
			FROM jsonb_array_elements(notices) WITH ORDINALITY t(n, ord))
		WHERE owner_id = $1 AND id = $2 AND notices @> jsonb_build_array(jsonb_build_object('recipient', $3::bigint, 'attention_id', null))`,
		owner, item, recipient)
	return err
}

// UsersExist reports the ids among ids that are not muster users (none: nil).
// ctx must read every user (the system scope): users is self-read under
// row-level security.
func (s *Store) UsersExist(ctx context.Context, ids []int64) ([]int64, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT x FROM unnest($1::bigint[]) x WHERE NOT EXISTS (SELECT 1 FROM users u WHERE u.id = x)`, ids)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}
