package hostjobs_test

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/hostjobs"
	"github.com/ZacxDev/muster/internal/mirror"
)

// THE STORE (migration 0157) on a real Postgres: a report's next fires, the
// scheduler's compare-and-set, the one-running rule, the single ending, and
// row-level security between two owners.

type world struct {
	ctx   context.Context
	one   context.Context // owner one's principal
	two   context.Context // owner two's
	store *hostjobs.Store
	dsn   string
	ids   [2]int64
}

func newWorld(t *testing.T) *world {
	t.Helper()
	dsn := dbtest.DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	w := &world{ctx: ctx, store: hostjobs.NewPG(pool), dsn: dsn}
	sys := authz.System(ctx)
	for i := range w.ids {
		if err := pool.QueryRow(sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`,
			"hostjobs-"+strconv.Itoa(i)+"-"+strconv.FormatInt(time.Now().UnixNano(), 36)).Scan(&w.ids[i]); err != nil {
			t.Fatal(err)
		}
		id := w.ids[i]
		t.Cleanup(func() {
			s := authz.System(context.Background())
			_, _ = pool.Exec(s, `DELETE FROM host_jobs WHERE owner_id = $1`, id)
			_, _ = pool.Exec(s, `DELETE FROM users WHERE id = $1`, id)
		})
	}
	as := func(id int64) context.Context {
		return authz.WithPrincipal(ctx, authz.Principal{UserID: id, Role: authz.RoleMember, Via: authz.ViaSession})
	}
	w.one, w.two = as(w.ids[0]), as(w.ids[1])
	return w
}

func decl(name, cron string) mirror.HostJob {
	return mirror.HostJob{Name: name, Cron: cron, Command: []string{"/usr/bin/env", "true"}}
}

var t0 = time.Date(2000, 3, 4, 5, 10, 0, 0, time.UTC)

// TestAReportKeepsAnUnchangedCronsNextFire: reporting the same jobs again (a
// reconnect) keeps each next fire; a changed cron gets one from the new cron;
// a job no longer reported is undeclared and never due; reported again, it is
// due from its cron again.
func TestAReportKeepsAnUnchangedCronsNextFire(t *testing.T) {
	w := newWorld(t)
	if err := w.store.Report(w.one, "build-01", []mirror.HostJob{decl("a", "0 6 * * *"), decl("b", "30 * * * *")}, t0); err != nil {
		t.Fatal(err)
	}
	next := func(name string) *time.Time {
		js, err := w.store.JobsNamed(w.one, name, "build-01")
		if err != nil || len(js) != 1 {
			return nil
		}
		return js[0].NextFireAt
	}
	if n := next("a"); n == nil || !n.Equal(time.Date(2000, 3, 4, 6, 0, 0, 0, time.UTC)) {
		t.Fatalf("a's next fire %v; want 06:00 the same day", n)
	}
	if err := w.store.Report(w.one, "build-01", []mirror.HostJob{decl("a", "0 6 * * *"), decl("b", "45 * * * *")}, t0.Add(25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Kept even though it is now past: it is still due, and fires once at the
	// next pass, rather than being skipped by the reconnect.
	if n := next("a"); n == nil || !n.Equal(time.Date(2000, 3, 4, 6, 0, 0, 0, time.UTC)) {
		t.Fatalf("a reconnect moved an unchanged cron's next fire: %v", n)
	}
	if n := next("b"); n == nil || !n.Equal(time.Date(2000, 3, 5, 6, 45, 0, 0, time.UTC)) {
		t.Fatalf("b's changed cron: next fire %v; want 06:45 the next day", n)
	}
	if err := w.store.Report(w.one, "build-01", []mirror.HostJob{decl("b", "45 * * * *")}, t0.Add(25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n := next("a"); n != nil {
		t.Fatalf("an undeclared job is still listed, next fire %v", n)
	}
	fires, err := w.store.FireDue(w.ctx, t0.Add(48*time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fires {
		if f.Job.Name == "a" && f.Job.Owner == w.ids[0] {
			t.Fatalf("an undeclared job fired: %+v", f)
		}
	}
	if err := w.store.Report(w.one, "build-01", []mirror.HostJob{decl("a", "0 6 * * *"), decl("b", "45 * * * *")}, t0.Add(72*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n := next("a"); n == nil || !n.Equal(time.Date(2000, 3, 7, 6, 0, 0, 0, time.UTC)) {
		t.Fatalf("a declared again: next fire %v; want its next from now", n)
	}
	if js, _ := w.store.Jobs(w.two); len(js) != 0 {
		t.Fatalf("owner two reads owner one's jobs: %+v", js)
	}
}

// TestADueJobFiresOnceAcrossTwoSchedulers: two stores on two pools (two
// replicas) firing the same due job at the same instant fire it ONCE between
// them, and its next fire is computed from now (missed fires collapse).
func TestADueJobFiresOnceAcrossTwoSchedulers(t *testing.T) {
	w := newWorld(t)
	host := "fire-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if err := w.store.Report(w.one, host, []mirror.HostJob{decl("a", "0 6 * * *")}, t0); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2000, 3, 9, 6, 0, 5, 0, time.UTC) // four days of fires missed
	var mu sync.Mutex
	n := 0
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		pool, err := db.Connect(w.ctx, w.dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		s := hostjobs.NewPG(pool)
		wg.Add(1)
		go func() {
			defer wg.Done()
			fires, err := s.FireDue(context.Background(), at, 100)
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			for _, f := range fires {
				if f.Job.Host == host {
					n++
				}
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if n != 1 {
		t.Fatalf("the due job fired %d times across two schedulers; want 1", n)
	}
	js, _ := w.store.JobsNamed(w.one, "a", host)
	if want := time.Date(2000, 3, 10, 6, 0, 0, 0, time.UTC); js[0].NextFireAt == nil || !js[0].NextFireAt.Equal(want) {
		t.Fatalf("next fire %v; want %v (missed fires collapse to one)", js[0].NextFireAt, want)
	}
}

// TestOneRunningRunPerJobAndOneEnding: a second start while a run is running
// is skipped; a start with the host offline is offline; a run ends ONCE (a
// second ending reports it lost the race), and its notice is set once.
func TestOneRunningRunPerJobAndOneEnding(t *testing.T) {
	w := newWorld(t)
	if err := w.store.Report(w.one, "build-01", []mirror.HostJob{decl("a", "0 6 * * *")}, t0); err != nil {
		t.Fatal(err)
	}
	j, _ := w.store.JobsNamed(w.one, "a", "build-01")
	r1, err := w.store.StartRun(w.one, j[0], hostjobs.TriggerCron, t0, true)
	if err != nil || r1.Status != hostjobs.StatusRunning {
		t.Fatalf("first start: %+v %v", r1, err)
	}
	r2, _ := w.store.StartRun(w.one, j[0], hostjobs.TriggerManual, t0, true)
	r3, _ := w.store.StartRun(w.one, j[0], hostjobs.TriggerCron, t0, false)
	if r2.Status != hostjobs.StatusSkipped || r3.Status != hostjobs.StatusSkipped {
		t.Fatalf("starts while running: %s, %s; want both skipped", r2.Status, r3.Status)
	}
	code := 0
	if _, won, err := w.store.FinishRun(w.one, r1.ID, hostjobs.Finish{Status: hostjobs.StatusOK, EndedAt: t0, ExitCode: &code}); err != nil || !won {
		t.Fatalf("first ending: won %t %v", won, err)
	}
	if _, won, _ := w.store.FinishRun(w.one, r1.ID, hostjobs.Finish{Status: hostjobs.StatusFailed, EndedAt: t0}); won {
		t.Fatal("a second ending of one run won")
	}
	if r, _ := w.store.Run(w.one, r1.ID); r.Status != hostjobs.StatusOK {
		t.Fatalf("the run after a second ending: %s; want ok", r.Status)
	}
	if _, won, _ := w.store.FinishRun(w.two, r1.ID, hostjobs.Finish{Status: hostjobs.StatusFailed, EndedAt: t0}); won {
		t.Fatal("owner two ended owner one's run")
	}
	r4, _ := w.store.StartRun(w.one, j[0], hostjobs.TriggerCron, t0, false)
	if r4.Status != hostjobs.StatusOffline {
		t.Fatalf("a start with the host offline: %s", r4.Status)
	}
	if ok, _ := w.store.SetNotice(w.one, r4.ID, 11); !ok {
		t.Fatal("the first notice was not set")
	}
	if ok, _ := w.store.SetNotice(w.one, r4.ID, 12); ok {
		t.Fatal("a second notice was set on one run")
	}
}
