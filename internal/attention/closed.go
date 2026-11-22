package attention

import (
	"context"
	"time"
)

// THE OWNER'S CLOSED ASKS (the Asks page's Done tab, docs/attention.md "The
// Done tab"): every ask and notice the caller OWNS that is no longer open —
// answered, answered in the session, dismissed (cancelled) and expired, an
// open row past its expiry included (it reads as expired: cols) — newest
// closed first, with no age window and no cap but the page's.
//
// 🔴 THE CALLER'S OWN ASKS, NAMED IN THE QUERY, as Search names them:
// attention_asks is team-shared (0127), so row-level security lets a team's
// member read the team's asks too, and "what RLS lets me read" is not "mine".
// A question a session put to the chief (ToChief) IS listed once closed, as
// the Answered tab listed the chief's answers ("Your chief's answer: …",
// docs/attention.md): it never reaches the Open tab, so the Done tab is the
// one place the owner sees what the chief answered for them.
//
// 🔴 KEYSET PAGING, never OFFSET: a page is "closed before this (time, id)",
// so an ask closing while the owner pages never shifts a card onto two pages
// or off both. The key is ClosedAt (resolved_at, or for an open row past its
// expiry that no sweep has moved yet, expires_at) and the id, both descending.

// ClosedPage is one page of Closed: at most Limit asks (≤ 0 or more than
// MaxClosedPage: MaxClosedPage) closed strictly before (BeforeAt, BeforeID) in
// the order — the zero cursor is the first page.
type ClosedPage struct {
	BeforeAt time.Time
	BeforeID int64
	Limit    int
}

// MaxClosedPage bounds one page of Closed.
const MaxClosedPage = 100

// ClosedAt is when ask a closed, the Done tab's order: its resolved_at, or —
// for an open row past its expiry that no sweep has moved yet — its expiry.
// The zero time for an ask that is open.
func (a Ask) ClosedAt() time.Time {
	if !a.ResolvedAt.IsZero() {
		return a.ResolvedAt
	}
	if a.Status == Expired {
		return a.ExpiresAt
	}
	return time.Time{}
}

// closedAtSQL is ClosedAt in SQL, THE one key the order and the cursor share.
const closedAtSQL = `coalesce(resolved_at, expires_at)`

// Closed is the caller's own closed asks, one page (see the file's header).
func (s *PGStore) Closed(ctx context.Context, q ClosedPage) ([]Ask, error) {
	limit := q.Limit
	if limit <= 0 || limit > MaxClosedPage {
		limit = MaxClosedPage
	}
	var before *time.Time
	if !q.BeforeAt.IsZero() {
		t := q.BeforeAt
		before = &t
	}
	return collect(s.pool.Query(ctx, `SELECT `+cols+` FROM attention_asks
		WHERE owner_id = nullif(current_setting('muster.uid', true), '')::bigint
		  AND (status <> 'open' OR expires_at <= now())
		  AND ($1::timestamptz IS NULL OR (`+closedAtSQL+`, id) < ($1::timestamptz, $2::bigint))
		ORDER BY `+closedAtSQL+` DESC, id DESC LIMIT $3`, before, q.BeforeID, limit))
}
