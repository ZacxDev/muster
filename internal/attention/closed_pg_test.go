package attention

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/users"
)

// closedIDs is one Closed page for ctx's principal, as ids in order.
func closedIDs(t *testing.T, s *PGStore, ctx context.Context, q ClosedPage) []int64 {
	t.Helper()
	got, err := s.Closed(ctx, q)
	if err != nil {
		t.Fatalf("closed %+v: %v", q, err)
	}
	ids := make([]int64, 0, len(got))
	for _, a := range got {
		ids = append(ids, a.ID)
	}
	return ids
}

// closeAs moves ask id to status, closed at (resolved_at), as the table's
// owner (a sweep's or an answer's write, with a chosen time).
func closeAs(t *testing.T, dsn string, id int64, status Status, at time.Time) {
	t.Helper()
	dbtest.OwnerExec(t, dsn, `UPDATE attention_asks SET status = '`+string(status)+`', resolved_at = '`+at.UTC().Format(time.RFC3339Nano)+
		`', answer = CASE WHEN '`+string(status)+`' IN ('answered', 'answered_in_session') THEN 'done' END, answered_by = CASE WHEN '`+string(status)+
		`' IN ('answered', 'answered_in_session') THEN 'operator' END WHERE id = `+strconv.FormatInt(id, 10))
}

// TestClosedListsEveryClosedAskNewestFirstWithNoWindow: answered, dismissed,
// expired (swept, and an open row past its expiry that no sweep moved) and
// answered in the session, some of them days old, are all listed, newest
// closed first; an open ask is not. RED with the old Answered read (answered
// only, the last day): the dismissed, expired and old ones were missing.
func TestClosedListsEveryClosedAskNewestFirstWithNoWindow(t *testing.T) {
	s, _, dsn := pgStore(t)
	const owner = int64(2)
	now := time.Now().UTC().Truncate(time.Second)
	mk := func(q string) Ask { return mustAsk(t, s, owner, New{Asker: session, Question: q}) }
	answered, dismissed, expired, inSession := mk("answered one"), mk("dismissed one"), mk("expired one"), mk("answered in session")
	closeAs(t, dsn, answered.ID, Answered, now.Add(-1*time.Hour))
	closeAs(t, dsn, dismissed.ID, Cancelled, now.Add(-50*time.Hour))
	closeAs(t, dsn, expired.ID, Expired, now.Add(-30*24*time.Hour))
	closeAs(t, dsn, inSession.ID, AnsweredInSession, now.Add(-3*time.Hour))
	// Past its expiry, never swept: it reads as expired, closed at its expiry.
	lapsed := mk("lapsed, not swept")
	dbtest.OwnerExec(t, dsn, `UPDATE attention_asks SET created_at = '`+now.Add(-11*time.Hour).Format(time.RFC3339)+`', expires_at = '`+
		now.Add(-10*time.Hour).Format(time.RFC3339)+`' WHERE id = `+strconv.FormatInt(lapsed.ID, 10))
	open := mk("still open")

	want := []int64{answered.ID, inSession.ID, lapsed.ID, dismissed.ID, expired.ID}
	if got := closedIDs(t, s, as(owner), ClosedPage{}); !slices.Equal(got, want) {
		t.Fatalf("closed: %v, want %v (open %d never)", got, want, open.ID)
	}
	got, _ := s.Closed(as(owner), ClosedPage{})
	if got[2].Status != Expired || !got[2].ClosedAt().Equal(now.Add(-10*time.Hour)) {
		t.Errorf("the lapsed ask reads %s closed at %v, want expired at its expiry", got[2].Status, got[2].ClosedAt())
	}
}

// TestClosedPagesByKeyAtTheBoundary: 30 closed asks — more than the old 20 —
// paged 25 at a time by their (closed at, id) key: the first page is the 25
// newest, the next page (after the 25th) the 5 older, nothing repeats, and a
// page of exactly the rest ends there. Two asks closed at the SAME instant are
// split across the boundary by id, never both dropped or both repeated.
// RED with an OFFSET page or a cursor on the time alone (the tie).
func TestClosedPagesByKeyAtTheBoundary(t *testing.T) {
	s, _, dsn := pgStore(t)
	const owner = int64(2)
	base := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Second)
	var newestFirst []int64
	asks := make([]Ask, 30)
	for i := range asks {
		asks[i] = mustAsk(t, s, owner, New{Asker: session, Question: "paged " + strconv.Itoa(i)})
		// i 24 and 25 (the 25th and 26th newest) close at the same instant.
		at := base.Add(time.Duration(i) * time.Minute)
		if i == 4 {
			at = base.Add(5 * time.Minute)
		}
		closeAs(t, dsn, asks[i].ID, Cancelled, at)
	}
	got, err := s.Closed(as(owner), ClosedPage{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range got {
		newestFirst = append(newestFirst, a.ID)
	}
	if len(newestFirst) != 30 {
		t.Fatalf("all closed: %d, want 30", len(newestFirst))
	}
	// The tie sits on the boundary: the 25th and 26th newest share a time.
	if !got[24].ClosedAt().Equal(got[25].ClosedAt()) {
		t.Fatalf("fixture: the boundary is not a tie: %v %v", got[24].ClosedAt(), got[25].ClosedAt())
	}
	first := closedIDs(t, s, as(owner), ClosedPage{Limit: 25})
	if !slices.Equal(first, newestFirst[:25]) {
		t.Fatalf("page 1: %v, want %v", first, newestFirst[:25])
	}
	last := got[24]
	second := closedIDs(t, s, as(owner), ClosedPage{BeforeAt: last.ClosedAt(), BeforeID: last.ID, Limit: 25})
	if !slices.Equal(second, newestFirst[25:]) {
		t.Fatalf("page 2: %v, want %v", second, newestFirst[25:])
	}
	// After the last one: nothing.
	end := got[29]
	if rest := closedIDs(t, s, as(owner), ClosedPage{BeforeAt: end.ClosedAt(), BeforeID: end.ID, Limit: 25}); len(rest) != 0 {
		t.Fatalf("after the oldest: %v, want none", rest)
	}
	// A limit past the bound is the bound.
	if n := len(closedIDs(t, s, as(owner), ClosedPage{Limit: MaxClosedPage + 50})); n != 30 {
		t.Fatalf("a limit past MaxClosedPage: %d", n)
	}
}

// TestClosedIsTheOwnersOwnAsksOnly: the member reads a team's asks through
// row-level security (the control proves the team's closed ask is READABLE to
// them, so it is the query's owner condition that keeps it out), yet their
// Done page lists only their own closed asks — never the team's, never
// another owner's with the same text. Their own question to their chief is
// theirs, and listed (the Answered tab listed the chief's answers). RED with
// the owner condition removed: the team's closed ask is listed.
func TestClosedIsTheOwnersOwnAsksOnly(t *testing.T) {
	s, pool, dsn := pgStore(t)
	ctx := context.Background()
	dbtest.OwnerExec(t, dsn, `SELECT setval(pg_get_serial_sequence('public.users', 'id'), (SELECT max(id) FROM public.users))`)
	us := users.NewPG(pool)
	var member int64
	if err := pool.QueryRow(authz.System(ctx), `INSERT INTO users (role, display_name) VALUES ('member', 'Dana Done') RETURNING id`).Scan(&member); err != nil {
		t.Fatal(err)
	}
	team, err := us.CreateTeam(ctx, users.Actor{UserID: 1, Via: authz.ViaSession}, "Closers "+strconv.FormatInt(time.Now().UnixNano()%1e9, 36), member)
	if err != nil {
		t.Fatal(err)
	}
	acct, err := us.Account(authz.System(ctx), member)
	if err != nil {
		t.Fatal(err)
	}
	me := authz.WithPrincipal(ctx, authz.Principal{UserID: member, Role: acct.Role, Via: authz.ViaSession, Teams: acct.Teams})
	asTeam := authz.WithPrincipal(ctx, authz.Principal{UserID: team.ID, Role: authz.RoleMember, Via: authz.ViaAgent, Team: true})
	const text = "the shared heron word"
	create := func(c context.Context, n New) Ask {
		a, err := s.Create(c, n)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	mine := create(me, New{Asker: session, Question: text})
	teams := create(asTeam, New{Asker: session, Question: text})
	others := mustAsk(t, s, 3, New{Asker: session, Question: text})
	chief := create(me, New{Asker: session, Question: text, ToChief: true})
	at := time.Now().UTC().Add(-time.Hour)
	for i, id := range []int64{mine.ID, teams.ID, others.ID, chief.ID} {
		closeAs(t, dsn, id, Cancelled, at.Add(time.Duration(i)*time.Second))
	}
	// Control: the team's closed ask IS readable to the member.
	if a, err := s.Get(me, teams.ID); err != nil || a.ID != teams.ID {
		t.Fatalf("control: the member reads the team's ask %d: %+v %v", teams.ID, a, err)
	}
	// Control: each other owner's own Done lists its own.
	if got := closedIDs(t, s, asTeam, ClosedPage{}); !slices.Equal(got, []int64{teams.ID}) {
		t.Fatalf("control: the team's Done: %v, want %d", got, teams.ID)
	}
	if got := closedIDs(t, s, as(3), ClosedPage{}); !slices.Equal(got, []int64{others.ID}) {
		t.Fatalf("control: the other owner's Done: %v, want %d", got, others.ID)
	}
	if got := closedIDs(t, s, me, ClosedPage{}); !slices.Equal(got, []int64{chief.ID, mine.ID}) {
		t.Errorf("the member's Done: %v, want only their own: their question to the chief %d, then %d (not the team's %d or the other owner's %d)",
			got, chief.ID, mine.ID, teams.ID, others.ID)
	}
}
