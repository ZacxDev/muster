package officers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// RE-RUNNING BLOCKED ITEMS (docs/officers.md "Re-running blocked items").
//
// An officer instance whose connector grant is still pending works its item
// blind: the proxy refuses it, and it completes the item escalated ("operator:
// approve grant N") or done. Approving the grant used to change nothing for
// that item. Now muster records, at the refusal, which item(s) the instance
// held in HQ — a ConnectorBlock, keyed (grant, item) — and when THAT grant is
// approved asks HQ to re-run each one that finished ESCALATED
// (RerunBackend.RerunItem, HQ's POST /officers/{id}/items/{item_id}/rerun,
// key GrantRerunKey(grant)); one that finished done / no_action, or is still
// live, is skipped for good (operator 2026-11-17: "only escalated ones").
//
// 🔴 THE LINK IS THE GRANT ID AND THE HELD ITEM, NEVER TEXT. Nothing reads an
// item's summary or a refusal message: the refusal names its grant, and HQ's
// claimed list names the items the instance held at that moment.
//
// 🔴 BOUNDED: only blocks recorded within RerunWindow of the approval, at most
// RerunMaxPerApproval per approval (newest first). A block re-run or skipped
// once is settled and never read again; HQ answering a repeat with the
// existing re-run backs that. A block HQ could not answer (no route yet, an
// outage) stays unsettled, so the grant's next approval tries it again.
//
// Kept in muster's database (PGConnectorBlockStore, migration 0148) when it has
// one, so an approval days later, after a restart or on the other replica,
// still finds the block; else in this process's memory (MemConnectorBlockStore).
// ---------------------------------------------------------------------------

// RerunWindow is how far back a block is re-run when its grant is approved.
// Seven days: an operator approves a pending grant within a working week (the
// grant that motivated this waited about 40 hours over a weekend), and a block
// older than that is work whose subject has moved on — a newer event re-opens
// it through its own trigger.
const RerunWindow = 7 * 24 * time.Hour

// RerunMaxPerApproval caps the re-runs one approval asks for: a grant pending
// for days can collect many blocks (an instance that claims item after item
// and is refused on each), and a burst of re-runs would wake the whole pool
// at once against the officer's budget. Ten covers the largest batch measured
// (five items on one grant) twice over; the rest are logged, never sent.
const RerunMaxPerApproval = 10

// GrantRerunKey is HQ's idempotency key for re-runs caused by approving grant
// id: one re-run per (item, key) however often it is asked.
func GrantRerunKey(id int64) string { return "connector-grant:" + strconv.FormatInt(id, 10) }

// ConnectorBlock is one item an officer instance held while a connector
// refused it for want of an approved grant.
type ConnectorBlock struct {
	Officer   string
	GrantID   int64
	Item      ItemID
	BlockedAt time.Time
}

// ConnectorBlockStore keeps the blocks. ctx acts as the officer's owner (the
// database store is under row-level security).
type ConnectorBlockStore interface {
	// RecordBlock records b, unless (officer, grant, item) is already
	// recorded: the first refusal's time stands.
	RecordBlock(ctx context.Context, b ConnectorBlock) error
	// BlocksForGrant is officer's UNSETTLED blocks on grant recorded at or
	// after since, newest first, at most limit.
	BlocksForGrant(ctx context.Context, officer string, grant int64, since time.Time, limit int) ([]ConnectorBlock, error)
	// SettleBlock marks (officer, grant, item) settled at at — re-run, or
	// skipped for good — so it is never read again.
	SettleBlock(ctx context.Context, officer string, grant int64, item ItemID, at time.Time) error
}

func blockKey(officer string, grant int64, item ItemID) string {
	return officer + " " + strconv.FormatInt(grant, 10) + " " + string(item)
}

type memBlock struct {
	b       ConnectorBlock
	settled bool
}

// MemConnectorBlockStore is ConnectorBlockStore in this process's memory.
type MemConnectorBlockStore struct {
	mu   sync.Mutex
	rows map[string]*memBlock
}

// NewMemConnectorBlockStore is an empty memory store.
func NewMemConnectorBlockStore() *MemConnectorBlockStore {
	return &MemConnectorBlockStore{rows: map[string]*memBlock{}}
}

// RecordBlock implements ConnectorBlockStore.
func (m *MemConnectorBlockStore) RecordBlock(_ context.Context, b ConnectorBlock) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := blockKey(b.Officer, b.GrantID, b.Item)
	if _, ok := m.rows[k]; !ok {
		m.rows[k] = &memBlock{b: b}
	}
	return nil
}

// BlocksForGrant implements ConnectorBlockStore.
func (m *MemConnectorBlockStore) BlocksForGrant(_ context.Context, officer string, grant int64, since time.Time, limit int) ([]ConnectorBlock, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ConnectorBlock
	for _, r := range m.rows {
		if r.b.Officer == officer && r.b.GrantID == grant && !r.settled && !r.b.BlockedAt.Before(since) {
			out = append(out, r.b)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].BlockedAt.Equal(out[j].BlockedAt) {
			return out[i].BlockedAt.After(out[j].BlockedAt)
		}
		return out[i].Item > out[j].Item
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// SettleBlock implements ConnectorBlockStore.
func (m *MemConnectorBlockStore) SettleBlock(_ context.Context, officer string, grant int64, item ItemID, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[blockKey(officer, grant, item)]
	if !ok {
		return errors.New("officers: no such connector block")
	}
	r.settled = true
	return nil
}

// PGConnectorBlockStore is ConnectorBlockStore on officer_connector_blocks
// (migration 0148), under row-level security: ctx must act as the officer's
// owner.
type PGConnectorBlockStore struct{ pool *pgxpool.Pool }

// NewPGConnectorBlockStore is the store over pool.
func NewPGConnectorBlockStore(pool *pgxpool.Pool) *PGConnectorBlockStore {
	return &PGConnectorBlockStore{pool: pool}
}

// RecordBlock implements ConnectorBlockStore.
func (s *PGConnectorBlockStore) RecordBlock(ctx context.Context, b ConnectorBlock) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO officer_connector_blocks (officer, grant_id, item, blocked_at)
		VALUES ($1, $2, $3, $4) ON CONFLICT (owner_id, officer, grant_id, item) DO NOTHING`,
		b.Officer, b.GrantID, string(b.Item), b.BlockedAt)
	return err
}

// BlocksForGrant implements ConnectorBlockStore.
func (s *PGConnectorBlockStore) BlocksForGrant(ctx context.Context, officer string, grant int64, since time.Time, limit int) ([]ConnectorBlock, error) {
	rows, err := s.pool.Query(ctx, `SELECT officer, grant_id, item, blocked_at
		FROM officer_connector_blocks WHERE officer = $1 AND grant_id = $2 AND settled_at IS NULL AND blocked_at >= $3
		ORDER BY blocked_at DESC, item DESC LIMIT $4`, officer, grant, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConnectorBlock
	for rows.Next() {
		var b ConnectorBlock
		var item string
		if err := rows.Scan(&b.Officer, &b.GrantID, &item, &b.BlockedAt); err != nil {
			return nil, err
		}
		b.Item = ItemID(item)
		out = append(out, b)
	}
	return out, rows.Err()
}

// SettleBlock implements ConnectorBlockStore.
func (s *PGConnectorBlockStore) SettleBlock(ctx context.Context, officer string, grant int64, item ItemID, at time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE officer_connector_blocks SET settled_at = $4
		WHERE officer = $1 AND grant_id = $2 AND item = $3`, officer, grant, string(item), at)
	return err
}

// ---------------------------------------------------------------------------
// The HQ verb.
// ---------------------------------------------------------------------------

// RerunRequest is POST /officers/{id}/items/{item_id}/rerun's body.
type RerunRequest struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

// RerunAnswer is HQ's answer: Rerun is created (a new item now), already
// (this key re-ran it), live (the item or a live item of its subject carries
// the work) or superseded (a later item of its subject finished since); Item
// is the item that carries the work.
type RerunAnswer struct {
	Rerun string
	Item  ItemID
}

// RerunBackend re-runs one of the officer's finished items (CapRerun).
type RerunBackend interface {
	RerunItem(ctx context.Context, item ItemID, r RerunRequest) (RerunAnswer, error)
}

// RerunItem is POST /officers/{id}/items/{item_id}/rerun with the officer
// token. An HQ older than the route answers 404, as for an item it does not
// have: the caller logs it and goes on (*HQError, status 404).
func (c *Client) RerunItem(ctx context.Context, item ItemID, r RerunRequest) (RerunAnswer, error) {
	body, err := c.Do(ctx, http.MethodPost, c.itemPath(item, "rerun"), r)
	if err != nil {
		return RerunAnswer{}, err
	}
	var a struct {
		Rerun string `json:"rerun"`
		Item  struct {
			ID ItemID `json:"id"`
		} `json:"item"`
	}
	if err := json.Unmarshal(body, &a); err != nil || a.Rerun == "" || a.Item.ID == "" {
		return RerunAnswer{}, fmt.Errorf("officers: HQ's rerun answer is not {rerun, item}: %s", truncate(string(body), 200))
	}
	return RerunAnswer{Rerun: a.Rerun, Item: a.Item.ID}, nil
}
