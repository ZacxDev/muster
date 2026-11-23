package officers_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
)

// orderFleet is fakeFleet recording every Stop and Start, and the forget
// (Refresh's between), in ONE log, with an injectable Stop failure.
type orderFleet struct {
	*fakeFleet
	mu      sync.Mutex
	log     []string
	stopErr map[int]error
}

func (f *orderFleet) note(e string) { f.mu.Lock(); f.log = append(f.log, e); f.mu.Unlock() }

func (f *orderFleet) Stop(ctx context.Context, o officers.Officer, n int) error {
	if err := f.stopErr[n]; err != nil {
		f.note(fmt.Sprintf("stop-failed %d", n))
		return err
	}
	f.note(fmt.Sprintf("stop %d", n))
	return f.fakeFleet.Stop(ctx, o, n)
}

func (f *orderFleet) Start(ctx context.Context, o officers.Officer, n int, kickoff string) error {
	f.note(fmt.Sprintf("start %d", n))
	return f.fakeFleet.Start(ctx, o, n, kickoff)
}

func refreshRig(t *testing.T, stopErr map[int]error) (*rig, *orderFleet, *officers.Reconciler) {
	t.Helper()
	r := newRig(t, 3, 30*time.Minute)
	of := &orderFleet{fakeFleet: r.fleet, stopErr: stopErr}
	for n := 1; n <= 3; n++ {
		r.fleet.up[n], r.fleet.exists[n] = true, true
	}
	rec, err := officers.NewReconciler(officers.Config{
		Registry: officers.NewRegistry([]officers.Officer{r.o}),
		HQ:       officers.NewBackends(nil),
		Fleet:    of,
		Logger:   log.New(r.logs, "", 0),
		Now:      r.clk.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r, of, rec
}

// TestRefreshStopsAllThenForgetsThenStarts (G2): with three instances up,
// Refresh stops ALL of them, then runs between (the forget) exactly once,
// then starts each — never a forget before a stop (a running instance could
// be re-briefed in the gap and resume its old conversation), never a start
// before the forget. The result names the three restarted.
func TestRefreshStopsAllThenForgetsThenStarts(t *testing.T) {
	r, of, rec := refreshRig(t, nil)
	res, err := rec.Refresh(context.Background(), r.o.Key(), func(context.Context) error {
		of.note("forget")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "stop 1,stop 2,stop 3,forget,start 1,start 2,start 3"
	if got := strings.Join(of.log, ","); got != want {
		t.Fatalf("the refresh ran %q; want %q", got, want)
	}
	if len(res.Restarted) != 3 || len(res.NotStopped)+len(res.NotStarted)+len(res.Unreleased) != 0 {
		t.Fatalf("result %+v; want three restarted and nothing else", res)
	}
}

// TestARefreshWhoseStopFailsStartsOnlyWhatItStopped (G2): instance 2's stop
// fails. The forget still runs after every stop was tried, instance 2 is
// NOT started (it is still running its old conversation), the others are,
// and the result says exactly that: 1 and 3 restarted, 2 not stopped.
// A failing forget starts nothing and reports the stopped ones not started.
func TestARefreshWhoseStopFailsStartsOnlyWhatItStopped(t *testing.T) {
	r, of, rec := refreshRig(t, map[int]error{2: errors.New("the provisioner refused")})
	res, err := rec.Refresh(context.Background(), r.o.Key(), func(context.Context) error {
		of.note("forget")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "stop 1,stop-failed 2,stop 3,forget,start 1,start 3"
	if got := strings.Join(of.log, ","); got != want {
		t.Fatalf("the refresh ran %q; want %q", got, want)
	}
	name := func(n int) string { return r.o.InstanceName(n) }
	if strings.Join(res.Restarted, ",") != name(1)+","+name(3) || strings.Join(res.NotStopped, ",") != name(2) || len(res.NotStarted) != 0 {
		t.Fatalf("result %+v; want %s and %s restarted, %s not stopped", res, name(1), name(3), name(2))
	}

	r2, of2, rec2 := refreshRig(t, nil)
	res2, err := rec2.Refresh(context.Background(), r2.o.Key(), func(context.Context) error {
		of2.note("forget")
		return errors.New("the database is away")
	})
	if err == nil {
		t.Fatal("a failed forget reported no error")
	}
	if got := strings.Join(of2.log, ","); got != "stop 1,stop 2,stop 3,forget" {
		t.Fatalf("after a failed forget the refresh ran %q; want the three stops, the forget, and no start", got)
	}
	if len(res2.NotStarted) != 3 || len(res2.Restarted) != 0 {
		t.Fatalf("after a failed forget: %+v; want the three stopped reported not started", res2)
	}
}
