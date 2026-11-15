package credentials

import (
	"context"
	"io"
	"log"
	"net/http"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/ccpool"
	"github.com/ZacxDev/muster/internal/ccusage"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/users"
)

// Fake tokens for the demoted-operator world: pairwise distinct, none equal
// to another fixture's.
const (
	poolTokAlpha = "sk-ant-oat01-FAKE-pool-alpha-5555555"
	poolTokZulu  = "sk-ant-oat01-FAKE-pool-zulu-66666666"
	ownTokAlpha  = "sk-ant-oat01-FAKE-user1-alpha-777777"
	ownTokZulu   = "sk-ant-oat01-FAKE-user1-zulu-8888888"
)

// The poller reads the pool owner off its Source (the plane): a compile-time
// pin that the plane still offers it.
var _ ccusage.PoolOwned = (*Plane)(nil)

// demotedOperatorWorld is the round 0 world: user 1 DEMOTED to member, with
// personal Claude accounts named exactly as the server pool's (alpha, zulu);
// two enabled admins, firstAdmin (the lowest ENABLED id, so the pool owner)
// and secondAdmin, and a DISABLED admin with a lower id than either; a team
// listed on the server pool. Roles are the REAL users
// store, so the pool owner is the real FirstAdmin read.
type demotedOperatorWorld struct {
	pg                                 *pgxpool.Pool
	plane                              *Plane
	store                              *ccpool.PGStore
	pool                               *ccpool.Pool
	firstAdmin, secondAdmin            int64
	team                               int64
	userCtx, firstCtx, secCtx, teamCtx context.Context
}

func newDemotedOperatorWorld(t *testing.T) *demotedOperatorWorld {
	t.Helper()
	const disabledAdmin, firstAdmin, secondAdmin, team = int64(9404), int64(9405), int64(9406), int64(9407)
	_, pg := pgStore(t)
	dsn := dbtest.DSN(t)
	dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, role) OVERRIDING SYSTEM VALUE VALUES ($1, 'admin'), ($2, 'admin') ON CONFLICT (id) DO UPDATE SET role = 'admin', disabled_at = NULL`, firstAdmin, secondAdmin)
	dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, role, kind, display_name) OVERRIDING SYSTEM VALUE VALUES ($1, 'member', 'team', 'pool owner team') ON CONFLICT (id) DO NOTHING`, team)
	dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, role, disabled_at) OVERRIDING SYSTEM VALUE VALUES ($1, 'admin', now()) ON CONFLICT (id) DO UPDATE SET role = 'admin', disabled_at = now()`, disabledAdmin)
	dbtest.OwnerExec(t, dsn, `UPDATE users SET role = 'member' WHERE id = 1`)
	t.Cleanup(func() { dbtest.OwnerExec(t, dsn, `UPDATE users SET role = 'admin' WHERE id = 1`) })
	dbtest.OwnerExec(t, dsn, `DELETE FROM cc_account_marks`)
	dbtest.OwnerExec(t, dsn, `DELETE FROM cc_account_usage`)
	dbtest.OwnerExec(t, dsn, `DELETE FROM agents WHERE owner_id IN (1, $1, $2, $3)`, firstAdmin, secondAdmin, team)

	env, err := ccpool.NewStatic(map[string]string{"alpha": poolTokAlpha, "zulu": poolTokZulu})
	if err != nil {
		t.Fatal(err)
	}
	plane := &Plane{EnvClaude: env, Roles: users.NewPG(pg), TeamsOnServerPool: map[int64]bool{team: true},
		Personal: fakePersonal{claude: map[int64]map[string]string{authz.OperatorUserID: {"alpha": ownTokAlpha, "zulu": ownTokZulu}}}}
	store := ccpool.NewPG(pg)
	pool, err := ccpool.New(plane, store)
	if err != nil {
		t.Fatal(err)
	}
	return &demotedOperatorWorld{pg: pg, plane: plane, store: store, pool: pool,
		firstAdmin: firstAdmin, secondAdmin: secondAdmin, team: team,
		userCtx: as(authz.OperatorUserID), firstCtx: as(firstAdmin), secCtx: as(secondAdmin),
		teamCtx: authz.WithPrincipal(context.Background(), authz.Principal{UserID: team, Team: true, Role: authz.RoleMember, Via: authz.ViaAgent})}
}

// marksUnder is how many cc_account_marks rows owner holds.
func (w *demotedOperatorWorld) marksUnder(t *testing.T, owner int64) int {
	t.Helper()
	var n int
	if err := w.pg.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM cc_account_marks WHERE owner_id = $1`, owner).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (w *demotedOperatorWorld) selects(t *testing.T, who string, ctx context.Context, owner int64, want string) {
	t.Helper()
	if got, err := w.pool.Select(ctx, owner, ""); err != nil || got != want {
		t.Errorf("%s's selection = %q (%v); want %q", who, got, err, want)
	}
}

// TestTheServerPoolsMarksNeverLandOnAPersonsOwnAccounts (round 0, item 1): with
// user 1 demoted, the server pool's marks are kept under the pool owner (the
// lowest-id enabled admin), never under user 1 — whose own accounts carry the
// pool's names.
//
//   - a second admin (not the pool owner) marks the pool's alpha: user 1's own
//     alpha is NOT pushed down (user 1 still selects alpha), while the pool owner
//     and the listed team see the mark (both select zulu); the row is under the
//     pool owner, none under user 1 or the second admin;
//   - the other direction: user 1 marks their OWN alpha: the pool's alpha is not
//     pushed down (the pool owner and the team select alpha), and user 1's mark
//     stays user 1's (user 1 selects zulu — the control that it was written).
//
// 🔴 RED at 0740e0cb: the env marks owner was authz.OperatorUserID, so the
// second admin's mark landed on user 1's personal alpha (user 1 moved to
// zulu), and user 1's own mark steered the pool (the pool owner moved to zulu).
func TestTheServerPoolsMarksNeverLandOnAPersonsOwnAccounts(t *testing.T) {
	w := newDemotedOperatorWorld(t)

	if err := w.pool.MarkFailure(w.secCtx, w.secondAdmin, "alpha", ccpool.FailureRateLimited, "weekly limit reached"); err != nil {
		t.Fatal(err)
	}
	w.selects(t, "user 1 (demoted)", w.userCtx, authz.OperatorUserID, "alpha")
	w.selects(t, "the pool owner", w.firstCtx, w.firstAdmin, "zulu")
	w.selects(t, "the listed team", w.teamCtx, w.team, "zulu")
	w.selects(t, "the second admin", w.secCtx, w.secondAdmin, "zulu")
	if u1, first, sec := w.marksUnder(t, authz.OperatorUserID), w.marksUnder(t, w.firstAdmin), w.marksUnder(t, w.secondAdmin); u1 != 0 || first != 1 || sec != 0 {
		t.Errorf("the second admin's pool mark: %d rows under user 1, %d under the pool owner, %d under the second admin; want 0, 1, 0", u1, first, sec)
	}

	dbtest.OwnerExec(t, dbtest.DSN(t), `DELETE FROM cc_account_marks`)
	if err := w.pool.MarkFailure(w.userCtx, authz.OperatorUserID, "alpha", ccpool.FailureRateLimited, "weekly limit reached"); err != nil {
		t.Fatal(err)
	}
	w.selects(t, "the pool owner", w.firstCtx, w.firstAdmin, "alpha")
	w.selects(t, "the listed team", w.teamCtx, w.team, "alpha")
	w.selects(t, "user 1 (demoted, control: their own mark stands)", w.userCtx, authz.OperatorUserID, "zulu")
	if u1, first := w.marksUnder(t, authz.OperatorUserID), w.marksUnder(t, w.firstAdmin); u1 != 1 || first != 0 {
		t.Errorf("user 1's own mark: %d rows under user 1, %d under the pool owner; want 1, 0", u1, first)
	}
}

// usableFetch answers every usage call with a reading under every limit, and
// records the tokens it was called with.
type usableFetch struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (f *usableFetch) Usage(_ context.Context, tok string) ccusage.Result {
	f.mu.Lock()
	f.seen[tok] = true
	f.mu.Unlock()
	u := 10.0
	return ccusage.Result{Status: http.StatusOK, HasReading: true,
		Reading: ccusage.Reading{Windows: map[string]ccusage.Window{ccusage.FiveHour: {Utilization: &u}}}}
}

func (f *usableFetch) Probe(ctx context.Context, tok string) ccusage.Result { return f.Usage(ctx, tok) }

// TestTheUsagePollerClearsTheMarksTheSelectionReads (round 0, item 1): the
// usage poller reads the owner the selection files the pool's marks under —
// ONE function, Plane.EnvPoolOwner, which the poller reads off its Source
// (ccusage.PoolOwned) exactly as cmd/muster-server builds it: Source the
// plane, and an Owner of user 1 that is then NOT read. With user 1 demoted, a pass under the poller's
// own scope (System, as Run) probes the POOL's tokens and never user 1's,
// clears the second admin's mark on the pool's alpha (the team selects alpha
// again), and saves its readings under the pool owner, none under user 1.
//
// 🔴 With a poller that reads a fixed user 1 (0740e0cb's Owner:
// authz.OperatorUserID, read whatever the Source), the pass probes
// user 1's personal tokens and the pool's mark is never cleared.
func TestTheUsagePollerClearsTheMarksTheSelectionReads(t *testing.T) {
	w := newDemotedOperatorWorld(t)
	if err := w.pool.MarkFailure(w.secCtx, w.secondAdmin, "alpha", ccpool.FailureRateLimited, "weekly limit reached"); err != nil {
		t.Fatal(err)
	}
	w.selects(t, "the listed team (before the pass)", w.teamCtx, w.team, "zulu")

	fetch := &usableFetch{seen: map[string]bool{}}
	now := time.Now()
	p := &ccusage.Poller{Source: w.plane, Store: ccusage.NewPG(w.pg), Fetch: fetch,
		Cooling: w.pool.Cooldown, ClearCooling: w.pool.ClearRateLimit,
		Owner:    authz.OperatorUserID,
		Interval: time.Minute, Logger: log.New(io.Discard, "", 0), Now: func() time.Time { return now }}
	if err := p.Pass(authz.System(context.Background())); err != nil {
		t.Fatal(err)
	}

	var seen []string
	for tok := range fetch.seen {
		seen = append(seen, tok)
	}
	sort.Strings(seen)
	if len(seen) != 2 || seen[0] != poolTokAlpha || seen[1] != poolTokZulu {
		t.Errorf("the pass probed %d tokens (%v); want exactly the pool's two (alpha, zulu), never user 1's own", len(seen), seen)
	}
	w.selects(t, "the listed team (after the pass)", w.teamCtx, w.team, "alpha")
	if cd, err := w.pool.Cooldown(w.firstCtx, w.firstAdmin, "alpha"); err != nil || !cd.Until.IsZero() {
		t.Errorf("after the pass the pool's alpha is still cooling (%+v, %v); the poller did not clear the mark the selection reads", cd, err)
	}
	var underOwner, underUser1 int
	if err := w.pg.QueryRow(authz.System(context.Background()), `SELECT count(*) FILTER (WHERE owner_id = $1),
		count(*) FILTER (WHERE owner_id = 1) FROM cc_account_usage`, w.firstAdmin).Scan(&underOwner, &underUser1); err != nil {
		t.Fatal(err)
	}
	if underOwner != 2 || underUser1 != 0 {
		t.Errorf("the pass saved %d readings under the pool owner and %d under user 1; want 2 and 0", underOwner, underUser1)
	}
}
