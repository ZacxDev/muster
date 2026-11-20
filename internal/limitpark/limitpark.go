// Package limitpark tells a STALE usage-limit menu from a live one.
//
// # The failure it answers
//
// When a claude-code turn meets its account's usage limit, the CLI raises its
// usage-limit menu ("❯ 1. Stop and wait for limit to reset") in that session.
// The menu stays until someone answers it, past the limit's reset too, and
// ccd refuses every later turn into that pane with a 409 rate_limited
// (cmd/ccd limitMenuFailure), which the gateway reads as the account's usage
// limit: it marks the account and asks the failover to move the agent.
//
// The usage poller (internal/ccusage) clears an account's mark once a LIVE
// reading shows it usable again. But a pane still on its old menu refuses the
// next turn with the same 409, and the account was marked again within
// minutes. Measured on a production deployment: seven officer instances sat
// on the menu for six hours, the poller cleared the mark every interval, the
// panes re-marked it every time, and their slots were never freed although
// the account read 21% of its weekly window. A Stop and Start cleared each.
//
// # What the board does
//
// It remembers, per agent, since when its pane has been parked on the limit
// (a [Park]: the first usage-limit failure since the agent's last completed
// turn), and, per account, the poller's latest good reading. Then:
//
//   - [Board.NoteLimit] (the gateway, on every usage-limit failure) answers
//     whether the failure is STALE: a 409 from a pane parked SINCE BEFORE a
//     newer live reading that shows the account usable for the limit the pane
//     names, with no fresh limit hit on the account after that reading. For
//     an agent muster restarts off such a menu (a pod officer's instance:
//     internal/api Server.LimitRestartable, the one rule), a stale failure
//     marks nothing and moves nobody: it says nothing the live reading did
//     not already overrule. Any other agent's still marks and fails over. Decided by timestamps, never text: a
//     429 (a turn that WAS typed and hit the limit) is always fresh, and so is
//     any failure newer than the reading.
//   - [Board.Stuck] (the officer runtime, each wake-loop pass) answers whether
//     an agent's pane is stale-parked AND was still seen on the menu after
//     that reading — the instance the wake loop then restarts (internal/
//     officers restartLimitParked), as an operator's Stop and Start would.
//
// # In memory, and why that is safe
//
// The board is per process (muster runs one replica). A restarted muster has
// no parks: the next 409 from a stuck pane parks it NOW and marks the account
// as before; the poller's next pass clears the mark with a reading newer than
// that park, and from then on the pane is stale. The cost of a restart is one
// poll interval, never a wrong verdict.
package limitpark

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/ccusage"
)

// Park is one agent's pane parked on its account's usage limit.
type Park struct {
	// Owner is the owner the account's marks and readings are kept under
	// (Board.MarksOwner of the agent's owner); Account the account's name.
	Owner   int64
	Account string
	// Detail is the latest failure message that names a subscription window
	// (ccusage.MarkedLimitWindow), "" when none did.
	Detail string
	// Since is the park's first usage-limit failure; Seen its latest.
	Since, Seen time.Time
	// staleLogged: the park's first stale refusal was logged (once per park).
	staleLogged bool
}

type acct struct {
	owner int64
	name  string
}

// reading is an account's latest good live reading, and the detail of the
// mark it was read against (when that named a window).
type reading struct {
	row    ccusage.Row
	detail string
}

// Board is the parks and readings. The zero value is not usable: use [New].
type Board struct {
	marksOwner func(ctx context.Context, owner int64) int64
	now        func() time.Time
	// Logger, when set, gets one line per park the first time a refusal from
	// it is found stale (and so marks nothing). Set it before first use.
	Logger *log.Logger

	mu    sync.Mutex
	parks map[int64]Park
	live  map[acct]reading
	// hits is each account's latest FRESH usage-limit failure (a 429, or a 409
	// NoteLimit did not find stale): evidence newer than a reading overrules it.
	hits map[acct]time.Time
}

// New builds a board. marksOwner maps an agent's owner to the owner its
// account's marks and readings are kept under (ccpool.Pool.MarksOwner); nil
// keys every account by the owner given. now is a test seam (nil: time.Now).
func New(marksOwner func(ctx context.Context, owner int64) int64, now func() time.Time) *Board {
	if now == nil {
		now = time.Now
	}
	return &Board{marksOwner: marksOwner, now: now, parks: map[int64]Park{}, live: map[acct]reading{}, hits: map[acct]time.Time{}}
}

func (b *Board) key(ctx context.Context, owner int64, account string) acct {
	if b.marksOwner != nil {
		if o := b.marksOwner(ctx, owner); o > 0 {
			owner = o
		}
	}
	return acct{owner, account}
}

// NoteReading records a good live reading of owner's account (the usage
// poller, ccusage.Poller.Reading): row as saved, taken at row.ReadAt, against
// a mark whose detail was markDetail ("" when the account was not marked).
// owner is the poller's, which is already the marks owner. A row that is not
// a good reading taken by its own attempt is ignored, as is one older than
// the account's latest.
func (b *Board) NoteReading(ctx context.Context, owner int64, account string, row ccusage.Row, markDetail string) {
	if row.ErrorKind != "" || row.ReadAt.IsZero() || !row.ReadAt.Equal(row.CheckedAt) {
		return
	}
	k := b.key(ctx, owner, account)
	b.mu.Lock()
	defer b.mu.Unlock()
	prev, had := b.live[k]
	if had && !row.ReadAt.After(prev.row.ReadAt) {
		return
	}
	r := reading{row: row, detail: prev.detail}
	if name, _ := ccusage.MarkedLimitWindow(markDetail); name != "" {
		r.detail = markDetail
	}
	b.live[k] = r
}

// NoteLimit records a usage-limit failure of agentID's turn on owner's
// account — menu: ccd's 409 refusal from a pane parked on the usage-limit
// menu (nothing typed); otherwise a typed turn that hit the limit (a 429) —
// and reports whether it is STALE, and why: a menu refusal from a pane
// parked since before a newer live reading that shows the account usable
// (see the package doc). A stale failure must not mark the account.
func (b *Board) NoteLimit(ctx context.Context, agentID, owner int64, account, detail string, menu bool) (stale bool, why string) {
	if agentID <= 0 || account == "" {
		return false, ""
	}
	k := b.key(ctx, owner, account)
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	p, had := b.parks[agentID]
	// 🔴 A TYPED TURN THAT HIT THE LIMIT STARTS A NEW PARK. A 429 is a paste
	// the pane took, so it was not on a menu before it; only a menu refusal
	// continues the park it finds.
	if !had || !menu || p.Owner != k.owner || p.Account != k.name {
		p = Park{Owner: k.owner, Account: k.name, Since: now}
	}
	p.Seen = now
	if name, _ := ccusage.MarkedLimitWindow(detail); name != "" {
		p.Detail = detail
	}
	b.parks[agentID] = p
	if menu {
		if why, ok := b.staleLocked(p, k); ok {
			if !p.staleLogged && b.Logger != nil {
				b.Logger.Printf("limitpark: agent %d: a usage-limit menu refusal on account %s is stale and marks nothing: %s", agentID, p.Account, why)
			}
			p.staleLogged = true
			b.parks[agentID] = p
			return true, why
		}
	}
	if now.After(b.hits[k]) {
		b.hits[k] = now
	}
	return false, ""
}

// staleLocked reports whether park p, on account k, is older than k's latest
// live reading and that reading shows the account usable for the limit p
// names, with no fresh limit hit on k since. b.mu is held.
func (b *Board) staleLocked(p Park, k acct) (string, bool) {
	r, ok := b.live[k]
	if !ok {
		return "", false
	}
	at := r.row.ReadAt
	// 🔴 THE PARK MUST PREDATE THE READING, STRICTLY. A pane that met the
	// limit after the account was read usable is new evidence, whatever the
	// reading said.
	if !p.Since.Before(at) {
		return "", false
	}
	// And nothing on the account hit the limit since the reading.
	if !b.hits[k].Before(at) {
		return "", false
	}
	detail := p.Detail
	if detail == "" {
		detail = r.detail
	}
	reason, ok := ccusage.LiveUsable(r.row, detail, at)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("its pane has been parked on the usage-limit menu since %s, and account %s was read usable at %s (%s)",
		p.Since.UTC().Format(time.RFC3339), p.Account, at.UTC().Format(time.RFC3339), reason), true
}

// Stuck reports whether agentID's pane is stale-parked (NoteLimit's verdict
// against the account's latest reading) AND was seen on the menu at or after
// that reading — so a restart now is what frees it — and says why.
func (b *Board) Stuck(agentID int64) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.parks[agentID]
	if !ok {
		return "", false
	}
	k := acct{p.Owner, p.Account}
	why, stale := b.staleLocked(p, k)
	if !stale || p.Seen.Before(b.live[k].row.ReadAt) {
		return "", false
	}
	return why + fmt.Sprintf("; it last refused a turn from that menu at %s", p.Seen.UTC().Format(time.RFC3339)), true
}

// Clear forgets agentID's park: a turn of it completed, ccd said a turn is
// running in it, or it was stopped or started.
func (b *Board) Clear(agentID int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.parks, agentID)
}

// Parked is agentID's park, if it has one (for tests and the log).
func (b *Board) Parked(agentID int64) (Park, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.parks[agentID]
	return p, ok
}
