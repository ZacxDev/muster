package builtin_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/officers/backendtest"
	"github.com/ZacxDev/muster/internal/officers/builtin"
)

func itemCount(t *testing.T, w *pgWorld, officer string) int {
	t.Helper()
	var n int
	if err := w.store.Pool().QueryRow(ctxOp(), `SELECT count(*) FROM officer_items WHERE officer = $1`, officer).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func putSchedule(t *testing.T, w *pgWorld, sc builtin.Schedule, now time.Time) builtin.Schedule {
	t.Helper()
	got, err := w.store.PutSchedule(ctxOp(), 1, backendtest.OfficerA, sc, now)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestAScheduleFiresOnceAcrossTwoReplicasAndARestart: two schedulers (two
// pools, as two replicas) firing the same due schedule at the same instant
// enqueue ONE item between them; a third scheduler on a fresh pool (a
// restarted muster) at the same instant enqueues nothing more; the schedule's
// next fire is the next cron time after now.
func TestAScheduleFiresOnceAcrossTwoReplicasAndARestart(t *testing.T) {
	w := newPGWorld(t, false)
	put := time.Date(2026, 11, 12, 23, 10, 0, 0, time.UTC)
	sc := putSchedule(t, w, builtin.Schedule{Name: "morning", Cron: "0 6 * * *", Kind: "digest", SubjectKey: "daily", SkipIfLive: true, Enabled: true}, put)
	if sc.NextFireAt == nil || !sc.NextFireAt.Equal(time.Date(2026, 11, 13, 6, 0, 0, 0, time.UTC)) {
		t.Fatalf("next fire after put: %v, want 2026-11-13 06:00", sc.NextFireAt)
	}
	fire := time.Date(2026, 11, 13, 6, 0, 20, 0, time.UTC)
	stores := make([]*builtin.Store, 3)
	for i := range stores {
		pool, err := db.Connect(context.Background(), dbtest.DSN(t))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		stores[i] = builtin.NewStore(pool)
	}
	sys := authz.System(context.Background())
	var wg sync.WaitGroup
	var mu sync.Mutex
	var fires []builtin.Fire
	for _, st := range stores[:2] {
		wg.Add(1)
		go func(st *builtin.Store) {
			defer wg.Done()
			f, err := st.FireDue(sys, fire, 10)
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			fires = append(fires, f...)
			mu.Unlock()
		}(st)
	}
	wg.Wait()
	if len(fires) != 1 || fires[0].Item == 0 || itemCount(t, w, backendtest.OfficerA) != 1 {
		t.Fatalf("two replicas at one instant: fires %+v, %d items; want exactly one fire enqueuing one item", fires, itemCount(t, w, backendtest.OfficerA))
	}
	if again, err := stores[2].FireDue(sys, fire, 10); err != nil || len(again) != 0 || itemCount(t, w, backendtest.OfficerA) != 1 {
		t.Fatalf("a restarted muster at the same instant: %+v (%v), want nothing", again, err)
	}
	list, _ := w.store.ListSchedules(ctxOp(), 1, backendtest.OfficerA)
	if len(list) != 1 || !list[0].NextFireAt.Equal(time.Date(2026, 11, 14, 6, 0, 0, 0, time.UTC)) || list[0].LastFiredAt == nil {
		t.Fatalf("after the fire: %+v, want next 2026-11-14 06:00 and last fired set", list)
	}
}

// TestMissedFiresCollapseToOneAndALiveItemIsNotRepeated: a schedule due many
// times over (muster down for days) fires once, and its next fire is the next
// cron time after NOW; with skip_if_live, a fire while that item is still
// live enqueues nothing; with it off, each fire is its own item.
func TestMissedFiresCollapseToOneAndALiveItemIsNotRepeated(t *testing.T) {
	w := newPGWorld(t, false)
	sys := authz.System(context.Background())
	put := time.Date(2026, 11, 1, 0, 0, 30, 0, time.UTC)
	putSchedule(t, w, builtin.Schedule{Name: "hourly", Cron: "17 * * * *", Kind: "check", SubjectKey: "hourly", SkipIfLive: true, Enabled: true}, put)
	later := time.Date(2026, 11, 4, 9, 40, 0, 0, time.UTC) // ~80 hourly fires missed
	f, err := w.store.FireDue(sys, later, 10)
	if err != nil || len(f) != 1 || f[0].Item == 0 {
		t.Fatalf("after days down: %+v (%v), want one fire", f, err)
	}
	list, _ := w.store.ListSchedules(ctxOp(), 1, backendtest.OfficerA)
	if !list[0].NextFireAt.Equal(time.Date(2026, 11, 4, 10, 17, 0, 0, time.UTC)) {
		t.Fatalf("next fire after the collapse: %v, want the next :17 after now (10:17)", list[0].NextFireAt)
	}
	f, err = w.store.FireDue(sys, time.Date(2026, 11, 4, 10, 17, 5, 0, time.UTC), 10)
	if err != nil || len(f) != 1 || f[0].Item != 0 || f[0].Skipped != "live" || itemCount(t, w, backendtest.OfficerA) != 1 {
		t.Fatalf("a fire while the item is live: %+v (%v), want skipped live and still one item", f, err)
	}

	putSchedule(t, w, builtin.Schedule{Name: "each", Cron: "*/5 * * * *", Kind: "tick", SubjectKey: "tick", SkipIfLive: false, Enabled: true},
		time.Date(2026, 11, 4, 10, 18, 0, 0, time.UTC))
	for _, at := range []time.Time{time.Date(2026, 11, 4, 10, 20, 1, 0, time.UTC), time.Date(2026, 11, 4, 10, 25, 1, 0, time.UTC)} {
		if f, err := w.store.FireDue(sys, at, 10); err != nil || len(f) != 1 || f[0].Item == 0 {
			t.Fatalf("skip_if_live off at %s: %+v (%v), want a new item each fire", at, f, err)
		}
	}
	var subjects []string
	rows, _ := w.store.Pool().Query(ctxOp(), `SELECT subject_key FROM officer_items WHERE kind = 'tick' ORDER BY id`)
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		subjects = append(subjects, s)
	}
	if len(subjects) != 2 || subjects[0] != "tick@2026-11-04T10:20Z" || subjects[1] != "tick@2026-11-04T10:25Z" {
		t.Fatalf("per-fire subjects %v, want tick@…10:20Z and tick@…10:25Z", subjects)
	}
}

// TestASwitchedOffOfficerIsNotFedButItsScheduleAdvances: a fire for an
// officer switched off enqueues nothing and still moves next_fire_at, so
// switching it on does not release a backlog. A cron that never comes due,
// or a malformed one, is refused at put.
func TestASwitchedOffOfficerIsNotFedButItsScheduleAdvances(t *testing.T) {
	w := newPGWorld(t, false)
	sys := authz.System(context.Background())
	put := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	putSchedule(t, w, builtin.Schedule{Name: "m", Cron: "30 * * * *", Kind: "k", SubjectKey: "s", SkipIfLive: true, Enabled: true}, put)
	w.SetEnabled(backendtest.OfficerA, false)
	f, err := w.store.FireDue(sys, time.Date(2026, 11, 12, 0, 30, 9, 0, time.UTC), 10)
	if err != nil || len(f) != 1 || f[0].Skipped != "switched off" || itemCount(t, w, backendtest.OfficerA) != 0 {
		t.Fatalf("switched off: %+v (%v), want skipped and no item", f, err)
	}
	list, _ := w.store.ListSchedules(ctxOp(), 1, backendtest.OfficerA)
	if !list[0].NextFireAt.Equal(time.Date(2026, 11, 12, 1, 30, 0, 0, time.UTC)) {
		t.Fatalf("next fire while off: %v, want advanced to 01:30", list[0].NextFireAt)
	}
	for _, bad := range []string{"0 0 31 2 *", "61 * * * *"} {
		if _, err := w.store.PutSchedule(ctxOp(), 1, backendtest.OfficerA, builtin.Schedule{Name: "bad", Cron: bad, Kind: "k", SubjectKey: "s", Enabled: true}, put); err == nil {
			t.Fatalf("cron %q was accepted", bad)
		}
	}
}

// TestAScheduleWhoseItemCannotBeInsertedNeitherSticksNorStarvesOthers: a
// per-fire subject too long for an item is refused at put; and a fire whose
// insert the store refuses anyway (a subject made too long behind the API's
// back) still advances its schedule, so a schedule due right after it fires
// in the same pass even with a batch of one.
func TestAScheduleWhoseItemCannotBeInsertedNeitherSticksNorStarvesOthers(t *testing.T) {
	w := newPGWorld(t, false)
	put := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	long := strings.Repeat("s", 490)
	if _, err := w.store.PutSchedule(ctxOp(), 1, backendtest.OfficerA, builtin.Schedule{Name: "long", Cron: "0 * * * *", Kind: "k",
		SubjectKey: long, SkipIfLive: false, Enabled: true}, put); err == nil || !strings.Contains(err.Error(), "at most 482") {
		t.Fatalf("a per-fire subject that cannot fit: %v, want a refusal naming 482", err)
	}
	putSchedule(t, w, builtin.Schedule{Name: "bad", Cron: "0 * * * *", Kind: "k", SubjectKey: "short", SkipIfLive: false, Enabled: true}, put)
	putSchedule(t, w, builtin.Schedule{Name: "good", Cron: "2 1 * * *", Kind: "k", SubjectKey: "good", SkipIfLive: true, Enabled: true}, put)
	if _, err := w.store.Pool().Exec(ctxOp(), `UPDATE officer_schedules SET subject_key = $1 WHERE name = 'bad'`, long); err != nil {
		t.Fatal(err)
	}
	sys := authz.System(context.Background())
	at := time.Date(2026, 11, 12, 1, 5, 0, 0, time.UTC)
	f, err := w.store.FireDue(sys, at, 1)
	if err != nil || len(f) != 1 || f[0].Schedule != "bad" || f[0].Item != 0 || !strings.HasPrefix(f[0].Skipped, "failed: ") {
		t.Fatalf("the refused fire: %+v (%v), want it recorded as failed", f, err)
	}
	f, err = w.store.FireDue(sys, at, 1)
	if err != nil || len(f) != 1 || f[0].Schedule != "good" || f[0].Item == 0 {
		t.Fatalf("the next pass with a batch of one: %+v (%v), want the good schedule fired (the failed one advanced)", f, err)
	}
}
