package builtin

import (
	"bytes"
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

	"github.com/ZacxDev/muster/internal/officers"
)

// Backend is officers.Backend for one officer over the Store: the core verbs,
// answering in the contract's shapes and errors (a decision is
// *officers.HQError with a {code, error} body; a database failure wraps
// officers.ErrHQUnavailable), as HQ does.
type Backend struct {
	store   *Store
	officer officers.Officer
	now     func() time.Time
	scope   func(context.Context) context.Context
	// asker raises an instance's officer_ask_person as an Attention ask
	// (WithAsker); nil refuses every ask as an outage.
	asker Asker
	// faultDoubleClaim is the conformance suite's fault (export_test.go):
	// a claim of an item another instance holds under a live lease succeeds.
	faultDoubleClaim bool
}

// NewBackend is officer o's backend over store. now is the clock every lease
// is judged by (nil: time.Now); scope makes ctx act as o's owner under
// row-level security (nil: ctx unchanged).
func NewBackend(store *Store, o officers.Officer, now func() time.Time, scope func(context.Context) context.Context) *Backend {
	if now == nil {
		now = time.Now
	}
	if scope == nil {
		scope = func(ctx context.Context) context.Context { return ctx }
	}
	return &Backend{store: store, officer: o, now: now, scope: scope}
}

var (
	_ officers.Backend     = (*Backend)(nil)
	_ officers.SwitchGated = (*Backend)(nil)
)

// SwitchGatesWake implements officers.SwitchGated: the wake loop honours a
// builtin officer's switch (officer_defs.enabled), its only "off".
func (b *Backend) SwitchGatesWake() {}

// Officer implements officers.Backend.
func (b *Backend) Officer() officers.Officer { return b.officer }

// Capabilities implements officers.Backend: officers.BuiltinCapabilities,
// the one value the kickoff is written from (officers.DeclaredCapabilities).
func (b *Backend) Capabilities() officers.Capabilities { return officers.BuiltinCapabilities }

// Scrub implements officers.Backend: the built-in backend holds no credential.
func (b *Backend) Scrub(s string) string { return s }

// itemCols is what every verb reads of an item, in itemRow's scan order.
const itemCols = `id, officer, kind, subject_key, payload, status, tier, priority, not_before, blocked,
	coalesce(claimed_by, ''), claimed_at, lease_expires_at, attempts, coalesce(outcome, ''), coalesce(summary, ''),
	links, coalesce(routine_summary, ''), investigation, cost_usd::float8, coalesce(cost_from, ''), created_at, updated_at, asks`

// itemRow is an item as the contract spells it (the JSON an instance reads).
type itemRow struct {
	ID             int64           `json:"id"`
	OfficerID      string          `json:"officer_id"`
	Kind           string          `json:"kind"`
	SubjectKey     string          `json:"subject_key"`
	Payload        json.RawMessage `json:"payload"`
	Status         string          `json:"status"`
	Tier           string          `json:"tier"`
	Priority       string          `json:"priority"`
	NotBefore      *time.Time      `json:"not_before,omitempty"`
	Blocked        bool            `json:"blocked,omitempty"`
	ClaimedBy      string          `json:"claimed_by"`
	ClaimedAt      *time.Time      `json:"claimed_at,omitempty"`
	LeaseExpiresAt *time.Time      `json:"lease_expires_at"`
	Attempts       int             `json:"attempts"`
	Outcome        string          `json:"outcome,omitempty"`
	Summary        string          `json:"summary,omitempty"`
	Links          []string        `json:"links,omitempty"`
	RoutineSummary string          `json:"routine_summary,omitempty"`
	Investigation  json.RawMessage `json:"investigation,omitempty"`
	CostUSD        *float64        `json:"cost_usd,omitempty"`
	CostFrom       string          `json:"cost_from,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	// Asks are the Attention asks raised on the item (officer_ask_person,
	// migration 0120), with their answers once given.
	Asks json.RawMessage `json:"asks"`
}

func scanItem(row pgx.Row) (itemRow, error) {
	var it itemRow
	err := row.Scan(&it.ID, &it.OfficerID, &it.Kind, &it.SubjectKey, &it.Payload, &it.Status, &it.Tier, &it.Priority, &it.NotBefore, &it.Blocked,
		&it.ClaimedBy, &it.ClaimedAt, &it.LeaseExpiresAt, &it.Attempts, &it.Outcome, &it.Summary,
		&it.Links, &it.RoutineSummary, &it.Investigation, &it.CostUSD, &it.CostFrom, &it.CreatedAt, &it.UpdatedAt, &it.Asks)
	return it, err
}

// live reports whether it is held under an unexpired lease at now.
func (it itemRow) live(now time.Time) bool {
	return it.Status == officers.StatusClaimed && it.LeaseExpiresAt != nil && it.LeaseExpiresAt.After(now)
}

// refuse is the contract's decision: status, with body {error, code, …extra}.
func refuse(status int, code, msg string, extra map[string]any) error {
	body := map[string]any{"error": msg, "code": code}
	for k, v := range extra {
		body[k] = v
	}
	raw, _ := json.Marshal(body)
	return &officers.HQError{Status: status, Body: raw}
}

// outage is a database failure as the contract's retryable outage.
func outage(verb string, err error) error {
	return fmt.Errorf("%w: builtin %s: %v", officers.ErrHQUnavailable, verb, err)
}

func noSuchItem() error { return refuse(http.StatusNotFound, "not_found", "no such item", nil) }

func leaseLost() error { return refuse(http.StatusConflict, "lease_lost", "lease lost", nil) }

func answer(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("builtin: encode: %v", err)
	}
	return raw, nil
}

// itemID is id as the store's key; an id that is not one is no item.
func itemID(id officers.ItemID) (int64, bool) {
	n, err := strconv.ParseInt(string(id), 10, 64)
	return n, err == nil && n > 0
}

func validTier(t string) bool { return t == officers.TierRoutine || t == officers.TierJudgement }

// tx runs fn in one transaction acting as the officer's owner.
func (b *Backend) tx(ctx context.Context, verb string, fn func(pgx.Tx) ([]byte, error)) ([]byte, error) {
	ctx = b.scope(ctx)
	tx, err := b.store.pool.Begin(ctx)
	if err != nil {
		return nil, outage(verb, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	out, err := fn(tx)
	if err != nil {
		var dec *officers.HQError
		if errors.As(err, &dec) {
			return nil, err
		}
		return nil, outage(verb, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, outage(verb, err)
	}
	return out, nil
}

// lockItem reads item id of this officer FOR UPDATE: no such item of this
// officer is 404 (another officer's item included, as on HQ).
func (b *Backend) lockItem(ctx context.Context, tx pgx.Tx, id officers.ItemID) (itemRow, error) {
	n, ok := itemID(id)
	if !ok {
		return itemRow{}, noSuchItem()
	}
	it, err := scanItem(tx.QueryRow(ctx, `SELECT `+itemCols+` FROM officer_items WHERE owner_id = $1 AND officer = $2 AND id = $3 FOR UPDATE`,
		b.officer.Owner, b.officer.ID, n))
	if errors.Is(err, pgx.ErrNoRows) {
		return itemRow{}, noSuchItem()
	}
	return it, err
}

// Claim implements officers.Backend. HQ's precedence: gone, held, wrong
// tier, blocked; then not yet due and waiting on an ask.
func (b *Backend) Claim(ctx context.Context, item officers.ItemID, instance string, leaseSeconds int, tier string) ([]byte, error) {
	if tier != "" && !validTier(tier) {
		return nil, refuse(http.StatusBadRequest, "bad_request", "tier must be routine or judgement", nil)
	}
	return b.tx(ctx, "claim", func(tx pgx.Tx) ([]byte, error) {
		it, err := b.lockItem(ctx, tx, item)
		if err != nil {
			return nil, err
		}
		now := b.now()
		switch {
		case it.Status == officers.StatusDone || it.Status == officers.StatusEscalated:
			return nil, refuse(http.StatusGone, "gone", "the item is finished", nil)
		case it.live(now) && !(b.faultDoubleClaim && it.ClaimedBy != instance):
			return nil, refuse(http.StatusConflict, "held", "held", map[string]any{"held_by": it.ClaimedBy, "lease_expires_at": it.LeaseExpiresAt})
		case tier != "" && tier != it.Tier:
			return nil, refuse(http.StatusConflict, "wrong_tier", "wrong tier", map[string]any{"item_tier": it.Tier})
		case it.Blocked:
			return nil, refuse(http.StatusConflict, "blocked", "blocked", nil)
		case it.NotBefore != nil && it.NotBefore.After(now):
			return nil, refuse(http.StatusConflict, "not_due", "not due yet", map[string]any{"not_before": it.NotBefore})
		case it.Status == "waiting":
			return nil, refuse(http.StatusConflict, "waiting", waitingWhy(it), nil)
		}
		exp := now.Add(time.Duration(officers.ClampLease(leaseSeconds)) * time.Second)
		it, err = scanItem(tx.QueryRow(ctx, `UPDATE officer_items SET status = 'claimed', claimed_by = $2, claimed_at = $3, lease_expires_at = $4,
			attempts = attempts + 1, updated_at = $3 WHERE id = $1 RETURNING `+itemCols, it.ID, instance, now, exp))
		if err != nil {
			return nil, err
		}
		return answer(map[string]any{"item": it, "lease_expires_at": exp})
	})
}

// claimable is THE predicate of an item claim-next may take at $now ($3):
// open or a lapsed claim, not blocked, due, and of tier $4 when it is not "".
const claimable = `owner_id = $1 AND officer = $2 AND NOT blocked AND (not_before IS NULL OR not_before <= $3)
	AND (status = 'open' OR (status = 'claimed' AND lease_expires_at <= $3)) AND ($4 = '' OR tier = $4)`

// queueOrder is the contract's queue order: priority, then due, then age.
const queueOrder = `CASE priority WHEN 'urgent' THEN 0 WHEN 'high' THEN 1 WHEN 'normal' THEN 2 ELSE 3 END, not_before NULLS FIRST, id`

// ClaimNext implements officers.Backend: ONE statement, the row picked under
// FOR UPDATE SKIP LOCKED, so concurrent callers (two replicas included) never
// take one item. {item: null} when nothing is claimable.
func (b *Backend) ClaimNext(ctx context.Context, instance string, leaseSeconds int, tier string) ([]byte, error) {
	if tier != "" && !validTier(tier) {
		return nil, refuse(http.StatusBadRequest, "bad_request", "tier must be routine or judgement", nil)
	}
	now := b.now()
	exp := now.Add(time.Duration(officers.ClampLease(leaseSeconds)) * time.Second)
	it, err := scanItem(b.store.pool.QueryRow(b.scope(ctx), `UPDATE officer_items SET status = 'claimed', claimed_by = $5, claimed_at = $3,
		lease_expires_at = $6, attempts = attempts + 1, updated_at = $3
		WHERE id = (SELECT id FROM officer_items WHERE `+claimable+` ORDER BY `+queueOrder+` FOR UPDATE SKIP LOCKED LIMIT 1)
		RETURNING `+itemCols, b.officer.Owner, b.officer.ID, now, tier, instance, exp))
	if errors.Is(err, pgx.ErrNoRows) {
		return answer(map[string]any{"item": nil, "lease_expires_at": nil})
	}
	if err != nil {
		return nil, outage("claim-next", err)
	}
	return answer(map[string]any{"item": it, "lease_expires_at": exp})
}

// Heartbeat implements officers.Backend: only the holder of a live lease
// renews it (409 lease_lost otherwise).
func (b *Backend) Heartbeat(ctx context.Context, item officers.ItemID, instance string, leaseSeconds int) ([]byte, error) {
	return b.tx(ctx, "heartbeat", func(tx pgx.Tx) ([]byte, error) {
		it, err := b.lockItem(ctx, tx, item)
		if err != nil {
			return nil, err
		}
		now := b.now()
		if !it.live(now) || it.ClaimedBy != instance {
			return nil, leaseLost()
		}
		exp := now.Add(time.Duration(officers.ClampLease(leaseSeconds)) * time.Second)
		it, err = scanItem(tx.QueryRow(ctx, `UPDATE officer_items SET lease_expires_at = $2, updated_at = $3 WHERE id = $1 RETURNING `+itemCols,
			it.ID, exp, now))
		if err != nil {
			return nil, err
		}
		return answer(map[string]any{"item": it, "lease_expires_at": exp})
	})
}

// Complete implements officers.Backend. The arguments are checked first
// (400), then the holder's live lease (409 lease_lost). needs_judgement hands
// a ROUTINE item to judgement in place (open, no holder, the summary kept as
// routine_summary); links are refused with it, and it is refused on a
// judgement item.
func (b *Backend) Complete(ctx context.Context, item officers.ItemID, instance string, done officers.Completion) ([]byte, error) {
	switch done.Outcome {
	case "done", "no_action", "escalated", officers.OutcomeNeedsJudgement:
	default:
		return nil, refuse(http.StatusBadRequest, "invalid", "outcome must be one of done, escalated, no_action, needs_judgement", nil)
	}
	if utf8.RuneCountInString(done.Summary) > officers.MaxSummaryChars {
		return nil, refuse(http.StatusBadRequest, "invalid", fmt.Sprintf("summary is over %d characters", officers.MaxSummaryChars), nil)
	}
	if len(done.Links) > officers.MaxLinks {
		return nil, refuse(http.StatusBadRequest, "invalid", fmt.Sprintf("at most %d links", officers.MaxLinks), nil)
	}
	if done.Outcome == officers.OutcomeNeedsJudgement && len(done.Links) > 0 {
		return nil, refuse(http.StatusBadRequest, "invalid", "links are not kept for outcome needs_judgement: put them in the summary", nil)
	}
	var inv json.RawMessage
	switch {
	case done.Investigation != nil:
		inv, _ = json.Marshal(done.Investigation)
	case len(bytes.TrimSpace(done.InvestigationJSON)) > 0 && !bytes.Equal(bytes.TrimSpace(done.InvestigationJSON), []byte("null")):
		inv = bytes.TrimSpace(done.InvestigationJSON)
	}
	if inv != nil {
		if msg := investigationInvalid(inv, done.Outcome); msg != "" {
			return nil, refuse(http.StatusBadRequest, "invalid", msg, nil)
		}
	}
	var costUSD *float64
	var costFrom *string
	if c := done.Cost; c != nil {
		if c.USD < 0 || c.USD > officers.MaxItemCostUSD || strings.TrimSpace(c.From) == "" || len(c.From) > 100 {
			return nil, refuse(http.StatusBadRequest, "invalid", fmt.Sprintf("cost_usd must be 0..%d with a cost_from", officers.MaxItemCostUSD), nil)
		}
		costUSD, costFrom = &c.USD, &c.From
	}
	links := done.Links
	if links == nil {
		links = []string{}
	}
	return b.tx(ctx, "complete", func(tx pgx.Tx) ([]byte, error) {
		it, err := b.lockItem(ctx, tx, item)
		if err != nil {
			return nil, err
		}
		now := b.now()
		if it.Status == officers.StatusWaiting {
			return nil, waitingRefusal(it)
		}
		if !it.live(now) || it.ClaimedBy != instance {
			return nil, leaseLost()
		}
		if done.Outcome == officers.OutcomeNeedsJudgement {
			if it.Tier != officers.TierRoutine {
				return nil, refuse(http.StatusBadRequest, "invalid", "outcome needs_judgement is for a routine item only: this item is "+it.Tier+
					" tier — complete it done, no_action or escalated", nil)
			}
			it, err = scanItem(tx.QueryRow(ctx, `UPDATE officer_items SET tier = 'judgement', status = 'open', claimed_by = NULL,
				lease_expires_at = NULL, routine_summary = $2, cost_usd = coalesce($3, cost_usd), cost_from = coalesce($4, cost_from), updated_at = $5
				WHERE id = $1 RETURNING `+itemCols, it.ID, done.Summary, costUSD, costFrom, now))
			if err != nil {
				return nil, err
			}
			return answer(map[string]any{"item": it})
		}
		status := officers.StatusDone
		notices := json.RawMessage(`[]`)
		if done.Outcome == "escalated" {
			status = officers.StatusEscalated
			// 🔴 WHO IS NOTIFIED IS WRITTEN IN THIS TRANSACTION: an escalation
			// that commits has its notices, and one that does not has none.
			if notices, err = noticesFor(ctx, tx, b.officer.Owner, b.officer.ID); err != nil {
				return nil, err
			}
		}
		it, err = scanItem(tx.QueryRow(ctx, `UPDATE officer_items SET status = $2, outcome = $3, summary = $4, links = $5, claimed_by = NULL,
			lease_expires_at = NULL, investigation = coalesce($6, investigation), cost_usd = coalesce($7, cost_usd),
			cost_from = coalesce($8, cost_from), updated_at = $9,
			notices = CASE WHEN $2 = 'escalated' THEN $10::jsonb ELSE notices END
			WHERE id = $1 RETURNING `+itemCols, it.ID, status, done.Outcome, done.Summary, links, inv, costUSD, costFrom, now, notices))
		if err != nil {
			return nil, err
		}
		return answer(map[string]any{"item": it})
	})
}

// Release implements officers.Backend: the holder gives its claimed item
// back (open, no holder); anyone else is 409 lease_lost.
func (b *Backend) Release(ctx context.Context, item officers.ItemID, instance string) ([]byte, error) {
	return b.tx(ctx, "release", func(tx pgx.Tx) ([]byte, error) {
		it, err := b.lockItem(ctx, tx, item)
		if err != nil {
			return nil, err
		}
		if it.Status == officers.StatusWaiting {
			return nil, waitingRefusal(it)
		}
		if it.Status != officers.StatusClaimed || it.ClaimedBy != instance {
			return nil, leaseLost()
		}
		it, err = scanItem(tx.QueryRow(ctx, `UPDATE officer_items SET status = 'open', claimed_by = NULL, lease_expires_at = NULL, updated_at = $2
			WHERE id = $1 RETURNING `+itemCols, it.ID, b.now()))
		if err != nil {
			return nil, err
		}
		return answer(map[string]any{"item": it})
	})
}

// Items implements officers.Backend: {items, next}, oldest first; next is
// the cursor of the following page (null on the last).
func (b *Backend) Items(ctx context.Context, q officers.ItemsQuery) (officers.ItemsPage, error) {
	limit := q.Limit
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	if q.Tier != "" && !validTier(q.Tier) {
		return officers.ItemsPage{}, refuse(http.StatusBadRequest, "bad_request", "tier must be routine or judgement", nil)
	}
	var after int64
	if q.Cursor != "" {
		n, err := strconv.ParseInt(q.Cursor, 10, 64)
		if err != nil || n < 0 {
			return officers.ItemsPage{}, refuse(http.StatusBadRequest, "bad_request", "cursor is not one this backend gave", nil)
		}
		after = n
	}
	rows, err := b.store.pool.Query(b.scope(ctx), `SELECT `+itemCols+` FROM officer_items
		WHERE owner_id = $1 AND officer = $2 AND ($3 = '' OR status = $3) AND ($4 = '' OR tier = $4) AND id > $5
		ORDER BY id LIMIT $6`, b.officer.Owner, b.officer.ID, q.Status, q.Tier, after, limit+1)
	if err != nil {
		return officers.ItemsPage{}, outage("items", err)
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (itemRow, error) { return scanItem(r) })
	if err != nil {
		return officers.ItemsPage{}, outage("items", err)
	}
	var next *string
	if len(list) > limit {
		list = list[:limit]
		c := strconv.FormatInt(list[limit-1].ID, 10)
		next = &c
	}
	if list == nil {
		list = []itemRow{}
	}
	raw, err := answer(map[string]any{"items": list, "next": next})
	if err != nil {
		return officers.ItemsPage{}, err
	}
	page := officers.ItemsPage{Raw: raw}
	if next != nil {
		page.NextCursor = *next
	}
	var env struct {
		Items []officers.Item `json:"items"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return officers.ItemsPage{}, fmt.Errorf("builtin: items: %v", err)
	}
	page.Items = env.Items
	return page, nil
}

// AllItems implements officers.Backend (officers.PageAll).
func (b *Backend) AllItems(ctx context.Context, status string) ([]officers.Item, error) {
	return officers.PageAll(ctx, b.Items, status)
}

// briefRecent bounds the brief's recent outcomes.
const briefRecent = 10

// Brief implements officers.Backend: the officer, its type's charter, its
// open items and its latest outcomes. An officer the store does not declare
// is 404.
func (b *Backend) Brief(ctx context.Context) ([]byte, error) {
	ctx = b.scope(ctx)
	var (
		key, name, charter string
		version            int
		enabled            bool
		mode               string
	)
	err := b.store.pool.QueryRow(ctx, `SELECT t.key, t.name, t.charter, t.charter_version, d.enabled, d.mode
		FROM officer_defs d JOIN officer_types t ON t.owner_id = d.owner_id AND t.key = d.type_key
		WHERE d.owner_id = $1 AND d.officer = $2`, b.officer.Owner, b.officer.ID).Scan(&key, &name, &charter, &version, &enabled, &mode)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, refuse(http.StatusNotFound, "not_found", "officer "+b.officer.ID+" is not declared in the built-in backend", nil)
	}
	if err != nil {
		return nil, outage("brief", err)
	}
	read := func(sql string) ([]itemRow, error) {
		rows, err := b.store.pool.Query(ctx, `SELECT `+itemCols+` FROM officer_items WHERE owner_id = $1 AND officer = $2 AND `+sql,
			b.officer.Owner, b.officer.ID)
		if err != nil {
			return nil, err
		}
		list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (itemRow, error) { return scanItem(r) })
		if list == nil {
			list = []itemRow{}
		}
		return list, err
	}
	open, err := read(`status = 'open' ORDER BY id LIMIT 100`)
	if err != nil {
		return nil, outage("brief", err)
	}
	recent, err := read(fmt.Sprintf(`status IN ('done', 'escalated') ORDER BY updated_at DESC, id DESC LIMIT %d`, briefRecent))
	if err != nil {
		return nil, outage("brief", err)
	}
	return answer(map[string]any{
		"officer":      map[string]any{"id": b.officer.ID, "enabled": enabled, "mode": mode},
		"type":         map[string]any{"key": key, "name": name, "charter": charter, "charter_version": version},
		"open_items":   open,
		"recent_items": recent,
		"generated_at": b.now().UTC(),
	})
}

// Enabled implements officers.Backend: the officer's switch; an officer the
// store does not define is a 404 decision, never "enabled".
func (b *Backend) Enabled(ctx context.Context) (bool, error) {
	var on bool
	err := b.store.pool.QueryRow(b.scope(ctx), `SELECT enabled FROM officer_defs WHERE owner_id = $1 AND officer = $2`,
		b.officer.Owner, b.officer.ID).Scan(&on)
	if errors.Is(err, pgx.ErrNoRows) {
		// HQ's answer for an officer it does not know: a 404 decision, which
		// the wake loop counts toward a configuration fault (not an outage).
		return false, refuse(http.StatusNotFound, "not_found", "officer "+b.officer.ID+" is not defined in the built-in backend", nil)
	}
	if err != nil {
		return false, outage("officer", err)
	}
	return on, nil
}
