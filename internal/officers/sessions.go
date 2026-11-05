package officers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// THE JUDGEMENT-TIER DAILY SESSION CAP (officers plan §3).
//
// A tiered officer's judgement pool runs on Claude; `tiers.judgement.
// max_sessions_per_day` bounds how many judgement SESSIONS it starts per UTC
// day. A session is one distinct item the judgement pool claims that day:
// re-claiming an item already counted today (a restarted instance, an item
// given back and taken again) is not a new session.
//
// Two places read the ledger, and they must agree:
//   - officer_claim (internal/api): a judgement instance's claim of a NEW item
//     once the day's cap is spent is refused before HQ — the item stays
//     queued there, never dropped;
//   - the wake loop (Decide): a capped judgement pool is not woken or
//     re-prompted for new work, only for its held items and items already
//     counted today. An instance woken for work it may not claim would spend
//     a Claude session on being refused.
//
// 🔴 PERSISTED, AND ADMITTED UNDER THE ROW LOCK (operator, #436 round 0):
// the ledger is officer_judgement_sessions (migration 0081), one row per
// (officer, UTC day) of admitted entries, shared by every muster-server
// replica. PGSessionStore.AdmitSession decides in ONE transaction holding the
// day's row lock (SELECT … FOR UPDATE), on the row's latest version — never a
// snapshot — so a restart resets nothing, two replicas cannot both admit the
// N+1th, and a concurrent claim of the SAME item sees it counted (free) and
// learns it did not insert it (round 1 Y1, Y2). There is no in-memory count:
// one rule, one place (CountSessions).
//
// Entries are HQ item ids, plus two kinds no item id can spell (HQ's ids are
// integers):
//   - `pending:<unix>:<rand>` — a claim-next in flight, holding a session
//     until HQ names its item. It stops counting after PendingTTL, so one
//     whose server died before resolving it cannot block the pool for the
//     rest of the day (round 1 Y3);
//   - `unknown:<rand>` — a claim-next whose answer HQ lost: HQ may have
//     claimed an item, so the session is kept for the day, never undone
//     blindly (round 1 Y2).
// ---------------------------------------------------------------------------

// MaxSessionsPerDayCap bounds tiers.judgement.max_sessions_per_day.
const MaxSessionsPerDayCap = 100

// SessionCap is a pool's daily judgement-session cap: 0 (no cap) unless o is
// the judgement pool of a tiered officer that declares one.
func (o Officer) SessionCap() int {
	if o.Tiers == nil || o.Tier != TierJudgement {
		return 0
	}
	return o.Tiers.Judgement.MaxSessionsPerDay
}

// HasSessionCap reports whether any pool of o is capped.
func (o Officer) HasSessionCap() bool {
	return o.Tiers != nil && o.Tiers.Judgement.MaxSessionsPerDay > 0
}

// PendingTTL is how long a claim-next's pending entry holds its session: two
// HQ request timeouts. A claim-next is answered (or has failed) well within
// one; an entry older than this was left by a server that died mid-claim.
const PendingTTL = 2 * DefaultHQTimeout

const (
	pendingPrefix = "pending:"
	unknownPrefix = "unknown:"
)

// PendingToken is a fresh pending entry for a claim-next started at now.
func PendingToken(now time.Time) string {
	return pendingPrefix + strconv.FormatInt(now.Unix(), 10) + ":" + randHex()
}

func randHex() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// CountSessions is THE counting rule: of a day's entries, how many sessions
// they hold at now — every item, every unknown: entry, and every pending
// entry younger than PendingTTL (a stale or unparseable one holds nothing) —
// and which of them are items (still work for the pool at the cap).
func CountSessions(entries []string, now time.Time) (int, map[ItemID]bool) {
	n, items := 0, map[ItemID]bool{}
	for _, e := range entries {
		switch {
		case strings.HasPrefix(e, pendingPrefix):
			if !pendingLive(e, now) {
				continue
			}
		case strings.HasPrefix(e, unknownPrefix):
		default:
			items[ItemID(e)] = true
		}
		n++
	}
	return n, items
}

// pendingLive reports whether pending entry e still holds its session at now.
func pendingLive(e string, now time.Time) bool {
	ts, _, ok := strings.Cut(strings.TrimPrefix(e, pendingPrefix), ":")
	if !ok {
		return false
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	return err == nil && now.Sub(time.Unix(sec, 0)) <= PendingTTL
}

// Admission is AdmitSession's answer: Admitted, the claim may go ahead;
// Inserted, THIS call added the entry — so only it may release it.
type Admission struct {
	Admitted bool
	Inserted bool
}

// SessionStore is the cap's durable ledger. day is a UTC date (2006-01-02).
type SessionStore interface {
	// AdmitSession admits entry as one of officer's sessions on day when it
	// is already there (free, Inserted false) or the day's sessions at now
	// (CountSessions) are fewer than limit (Inserted true) — atomically
	// under the day's row lock.
	AdmitSession(ctx context.Context, officer, day, entry string, limit int, now time.Time) (Admission, error)
	// ReleaseSession removes entry (its claim definitely did not happen).
	ReleaseSession(ctx context.Context, officer, day, entry string) error
	// ResolveSession replaces pending token with entry, or drops the token
	// when entry is "" or already there.
	ResolveSession(ctx context.Context, officer, day, token, entry string) error
	// Sessions is the day's entries, in admission order.
	Sessions(ctx context.Context, officer, day string) ([]string, error)
}

// ErrSessionCap is a claim the day's cap refuses; the error's text says so.
var ErrSessionCap = errors.New("officers: judgement session cap reached")

// JudgementLedger applies the cap over a SessionStore. scope gives the store
// call the principal of o's owner (the table is owner-scoped); logf reports a
// release or resolve that failed (the count is then high until it is fixed or
// the day ends). A nil ledger, or an uncapped pool, admits everything.
type JudgementLedger struct {
	store SessionStore
	scope func(ctx context.Context, o Officer) context.Context
	logf  func(format string, args ...any)
}

// NewJudgementLedger is the ledger over store; scope and logf may be nil.
func NewJudgementLedger(store SessionStore, scope func(context.Context, Officer) context.Context, logf func(string, ...any)) *JudgementLedger {
	if scope == nil {
		scope = func(ctx context.Context, _ Officer) context.Context { return ctx }
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &JudgementLedger{store: store, scope: scope, logf: logf}
}

func utcDay(t time.Time) string { return t.UTC().Format("2006-01-02") }

// Reserve admits a claim of item by pool o at now. undo releases the session
// and is to be called ONLY when the claim definitely did not happen (HQ
// refused it); it releases nothing unless this admission inserted the item,
// so a claim that lost a race for the same item never releases the winner's.
// The cap's refusal wraps ErrSessionCap; a store failure is returned as is.
func (l *JudgementLedger) Reserve(ctx context.Context, o Officer, item ItemID, now time.Time) (undo func(), err error) {
	limit := o.SessionCap()
	if l == nil || limit == 0 {
		return func() {}, nil
	}
	sctx, day := l.scope(ctx, o), utcDay(now)
	adm, err := l.store.AdmitSession(sctx, o.ID, day, string(item), limit, now)
	if err != nil {
		return nil, fmt.Errorf("officers: admit %s's judgement session: %w", o.ID, err)
	}
	if !adm.Admitted {
		return nil, capRefusal(o, limit, "item "+string(item))
	}
	if !adm.Inserted {
		return func() {}, nil
	}
	return func() {
		if err := l.store.ReleaseSession(context.WithoutCancel(sctx), o.ID, day, string(item)); err != nil {
			l.logf("officers: %s: releasing judgement session %s failed (it counts until the day ends): %v", o.ID, item, err)
		}
	}, nil
}

// ReserveNext admits a claim-next (officer_claim with no item_id: HQ picks
// the item) by pool o at now, holding one session under a pending entry.
// done(item, known) settles it: known with an item counts that item, known
// with "" (HQ claimed nothing) gives the session back, and NOT known (HQ's
// answer was lost: it may have claimed one) keeps it as an unknown: entry.
func (l *JudgementLedger) ReserveNext(ctx context.Context, o Officer, now time.Time) (done func(item ItemID, known bool), err error) {
	limit := o.SessionCap()
	if l == nil || limit == 0 {
		return func(ItemID, bool) {}, nil
	}
	sctx, day := l.scope(ctx, o), utcDay(now)
	token := PendingToken(now)
	adm, err := l.store.AdmitSession(sctx, o.ID, day, token, limit, now)
	if err != nil {
		return nil, fmt.Errorf("officers: admit %s's judgement session: %w", o.ID, err)
	}
	if !adm.Admitted {
		return nil, capRefusal(o, limit, "the next item")
	}
	return func(item ItemID, known bool) {
		entry := string(item)
		if !known {
			entry = unknownPrefix + randHex()
		}
		if err := l.store.ResolveSession(context.WithoutCancel(sctx), o.ID, day, token, entry); err != nil {
			l.logf("officers: %s: resolving a claim-next's judgement session failed (it counts for %s): %v", o.ID, PendingTTL, err)
		}
	}, nil
}

// Capped reports whether pool o has spent its cap for now's UTC day, and the
// items it counted today (still work for it at the cap), by CountSessions. An
// uncapped pool is never capped. A store failure is returned.
func (l *JudgementLedger) Capped(ctx context.Context, o Officer, now time.Time) (bool, map[ItemID]bool, error) {
	limit := o.SessionCap()
	if l == nil || limit == 0 {
		return false, nil, nil
	}
	entries, err := l.store.Sessions(l.scope(ctx, o), o.ID, utcDay(now))
	if err != nil {
		return false, nil, fmt.Errorf("officers: read %s's judgement sessions: %w", o.ID, err)
	}
	n, items := CountSessions(entries, now)
	return n >= limit, items, nil
}

// capRefusal is the cap's refusal: what was not claimed, and that the item
// stays queued in HQ.
func capRefusal(o Officer, limit int, what string) error {
	return fmt.Errorf("%w: officer %s has started all %d of its judgement sessions for today (UTC) "+
		"(tiers.judgement.max_sessions_per_day): %s stays queued in HQ and is not claimed. Do not retry today; "+
		"stop working the queue — muster wakes this pool again tomorrow", ErrSessionCap, o.ID, limit, what)
}

// PGSessionStore is SessionStore on officer_judgement_sessions (migration
// 0081), under row-level security: ctx must act as the officer's owner.
type PGSessionStore struct{ pool *pgxpool.Pool }

// NewPGSessionStore is the store over pool.
func NewPGSessionStore(pool *pgxpool.Pool) *PGSessionStore { return &PGSessionStore{pool: pool} }

// AdmitSession implements SessionStore in ONE transaction: make sure the
// day's row exists, LOCK it (SELECT … FOR UPDATE reads its latest committed
// version, after any concurrent admission commits), and decide on that:
// already there → admitted, not inserted; under the cap → append (dropping
// stale pending entries while the row is held), admitted and inserted;
// otherwise refused.
func (s *PGSessionStore) AdmitSession(ctx context.Context, officer, day, entry string, limit int, now time.Time) (Admission, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Admission{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `INSERT INTO officer_judgement_sessions (officer, day) VALUES ($1, $2::date)
		ON CONFLICT (owner_id, officer, day) DO NOTHING`, officer, day); err != nil {
		return Admission{}, err
	}
	var items []string
	if err := tx.QueryRow(ctx, `SELECT items FROM officer_judgement_sessions WHERE officer = $1 AND day = $2::date FOR UPDATE`,
		officer, day).Scan(&items); err != nil {
		return Admission{}, err
	}
	for _, e := range items {
		if e == entry {
			return Admission{Admitted: true}, tx.Commit(ctx)
		}
	}
	if n, _ := CountSessions(items, now); n >= limit {
		return Admission{}, tx.Commit(ctx)
	}
	kept := make([]string, 0, len(items)+1)
	for _, e := range items {
		if strings.HasPrefix(e, pendingPrefix) && !pendingLive(e, now) {
			continue
		}
		kept = append(kept, e)
	}
	kept = append(kept, entry)
	if _, err := tx.Exec(ctx, `UPDATE officer_judgement_sessions SET items = $3, updated_at = now() WHERE officer = $1 AND day = $2::date`,
		officer, day, kept); err != nil {
		return Admission{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Admission{}, err
	}
	return Admission{Admitted: true, Inserted: true}, nil
}

// ReleaseSession implements SessionStore.
func (s *PGSessionStore) ReleaseSession(ctx context.Context, officer, day, entry string) error {
	_, err := s.pool.Exec(ctx, `UPDATE officer_judgement_sessions SET items = array_remove(items, $3::text), updated_at = now()
		WHERE officer = $1 AND day = $2::date`, officer, day, entry)
	return err
}

// ResolveSession implements SessionStore.
func (s *PGSessionStore) ResolveSession(ctx context.Context, officer, day, token, entry string) error {
	_, err := s.pool.Exec(ctx, `UPDATE officer_judgement_sessions SET items = CASE
			WHEN $4::text = '' OR $4::text = ANY (items) THEN array_remove(items, $3::text)
			ELSE array_replace(items, $3::text, $4::text) END, updated_at = now()
		WHERE officer = $1 AND day = $2::date`, officer, day, token, entry)
	return err
}

// Sessions implements SessionStore.
func (s *PGSessionStore) Sessions(ctx context.Context, officer, day string) ([]string, error) {
	var items []string
	err := s.pool.QueryRow(ctx, `SELECT items FROM officer_judgement_sessions WHERE officer = $1 AND day = $2::date`,
		officer, day).Scan(&items)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return items, err
}
