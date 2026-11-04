package sessionstate

import (
	"context"
	"io"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

func pgStore(t *testing.T) *PG {
	t.Helper()
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(dbtest.AsOperator(ctx), pool, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, role) OVERRIDING SYSTEM VALUE VALUES (2, 'member'), (3, 'member') ON CONFLICT (id) DO NOTHING`)
	dbtest.OwnerExec(t, dsn, `TRUNCATE session_episodes`)
	return NewPG(pool)
}

func asUser(uid int64) context.Context {
	role := authz.RoleMember
	if uid == authz.OperatorUserID {
		role = authz.RoleAdmin
	}
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: uid, Role: role, Via: authz.ViaSession})
}

func stalledEp(key string) Episode {
	return Episode{Kind: KindMirror, Key: key, State: StateStalled, Asked: true, Host: "host-a", TmuxSession: "alpha", Window: "@3",
		Index: 3, StreakKey: "since:1"}
}

// TestOneOpenEpisodePerSessionIsTheClaim: a second Open of the same session
// while one is open claims nothing and writes nothing — the partial unique
// index is the claim — and once it ends the next Open is the session's next
// seq. Another session, and another user's same key, are their own.
func TestOneOpenEpisodePerSessionIsTheClaim(t *testing.T) {
	p := pgStore(t)
	ctx := asUser(2)
	a, claimed, err := p.Open(ctx, stalledEp("sess-a"))
	if err != nil || !claimed || a.Seq != 1 || a.State != StateStalled || !a.Open() || a.Owner != 2 || a.Index != 3 {
		t.Fatalf("first open: %+v claimed=%v err=%v", a, claimed, err)
	}
	if _, claimed, err := p.Open(ctx, stalledEp("sess-a")); err != nil || claimed {
		t.Fatalf("a second open of an open session claimed it: claimed=%v err=%v", claimed, err)
	}
	if b, claimed, err := p.Open(ctx, stalledEp("sess-b")); err != nil || !claimed || b.Seq != 1 {
		t.Fatalf("another session: %+v claimed=%v err=%v", b, claimed, err)
	}
	if c, claimed, err := p.Open(asUser(3), stalledEp("sess-a")); err != nil || !claimed || c.Seq != 1 || c.Owner != 3 {
		t.Fatalf("another user's same key: %+v claimed=%v err=%v", c, claimed, err)
	}
	if ended, err := p.End(ctx, a.ID, EndWorked, time.Now()); err != nil || !ended {
		t.Fatalf("end: %v %v", ended, err)
	}
	if ended, err := p.End(ctx, a.ID, EndGone, time.Now()); err != nil || ended {
		t.Fatalf("an ended episode ended again: %v %v", ended, err)
	}
	next, claimed, err := p.Open(ctx, stalledEp("sess-a"))
	if err != nil || !claimed || next.Seq != 2 {
		t.Fatalf("the next stop: %+v claimed=%v err=%v", next, claimed, err)
	}
	open, err := p.OpenEpisodes(ctx)
	if err != nil || len(open) != 2 {
		t.Fatalf("user 2's open episodes: %+v %v", open, err)
	}
	if theirs, _ := p.OpenEpisodes(asUser(3)); len(theirs) != 1 || theirs[0].Owner != 3 {
		t.Fatalf("row-level security: user 3 sees %+v", theirs)
	}
}

// TestRacingOpensClaimOnce: many concurrent Opens of one session — two
// replicas' passes, or one pass racing another — claim exactly one episode.
func TestRacingOpensClaimOnce(t *testing.T) {
	p := pgStore(t)
	ctx := asUser(2)
	var wg sync.WaitGroup
	var mu sync.Mutex
	claims, errs := 0, 0
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, claimed, err := p.Open(ctx, stalledEp("sess-race"))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs++
			}
			if claimed {
				claims++
			}
		}()
	}
	wg.Wait()
	if claims != 1 || errs != 0 {
		t.Fatalf("claims %d (want 1), errors %d", claims, errs)
	}
	if list := listAll(t, p, ctx); len(list) != 1 {
		t.Fatalf("rows: %+v", list)
	}
}

// TestObserveAndClassify: Observe moves the state's since-time only when the
// state changes and keeps the run of work; Classify stores the answer and the
// leave rule's input; neither touches an ended episode.
func TestObserveAndClassify(t *testing.T) {
	p := pgStore(t)
	ctx := asUser(2)
	e, _, err := p.Open(ctx, stalledEp("sess-o"))
	if err != nil {
		t.Fatal(err)
	}
	t1 := e.StateSince.Add(time.Minute)
	busy := t1.Add(-10 * time.Second)
	if err := p.Observe(ctx, e.ID, Observation{State: StateStalled, Host: "host-a", TmuxSession: "alpha", Window: "@4", Index: 4,
		StreakKey: "since:2", BusySince: busy, BusySeen: t1, At: t1}); err != nil {
		t.Fatal(err)
	}
	got := only(t, p, ctx)
	if !got.StateSince.Equal(e.StateSince) || got.Index != 4 || got.StreakKey != "since:2" || !got.BusySince.Equal(busy) || !got.BusySeen.Equal(t1) {
		t.Fatalf("same state: %+v", got)
	}
	t2 := t1.Add(time.Minute)
	if err := p.Observe(ctx, e.ID, Observation{State: StateNeedsYou, Waiting: true, StreakKey: "since:2", At: t2}); err != nil {
		t.Fatal(err)
	}
	got = only(t, p, ctx)
	if got.State != StateNeedsYou || !got.Waiting || !got.StateSince.Equal(t2) || !got.BusySince.IsZero() {
		t.Fatalf("a new state: %+v", got)
	}
	c := Classification{Label: "neither", NeedsInput: 0.1, AsksPermission: 0.05, Neither: 0.85, AskScore: 0.15, Model: "fake/jev"}
	if err := p.SetClassification(ctx, e.ID, "since:2", c, false); err != nil {
		t.Fatal(err)
	}
	got = only(t, p, ctx)
	if got.ClassifiedStreak != "since:2" || got.Jev != c || got.Asked {
		t.Fatalf("classified: %+v", got)
	}
	if _, err := p.End(ctx, e.ID, EndGone, t2); err != nil {
		t.Fatal(err)
	}
	_ = p.Observe(ctx, e.ID, Observation{State: StateStalled, At: t2.Add(time.Minute)})
	_ = p.SetClassification(ctx, e.ID, "since:9", Classification{}, true)
	got = only(t, p, ctx)
	if got.State != StateNeedsYou || got.ClassifiedStreak != "since:2" || got.EndReason != EndGone {
		t.Fatalf("an ended episode was written: %+v", got)
	}
}

func only(t *testing.T, p *PG, ctx context.Context) Episode {
	t.Helper()
	list, err := listAll(t, p, ctx), error(nil)
	if err != nil || len(list) != 1 {
		t.Fatalf("episodes: %+v %v", list, err)
	}
	return list[0]
}

// listAll is the caller's episodes, newest first: what a reader of the table
// sees (no production method lists them yet).
func listAll(t *testing.T, p *PG, ctx context.Context) []Episode {
	t.Helper()
	list, err := collect(p.pool.Query(ctx, `SELECT `+cols+` FROM session_episodes ORDER BY id DESC`))
	if err != nil {
		t.Fatal(err)
	}
	return list
}
