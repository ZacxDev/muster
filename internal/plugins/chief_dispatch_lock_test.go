package plugins

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// smallPoolStore is pgStore's database through a pool of n connections.
func smallPoolStore(t *testing.T, n int) (*PGStore, *pgxpool.Pool) {
	t.Helper()
	_, _, dsn := pgStore(t)
	dbtest.OwnerExec(t, dsn, `DELETE FROM audit_log WHERE action LIKE 'task.chief_%'`)
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("pool_max_conns", strconv.Itoa(n))
	u.RawQuery = q.Encode()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if got := pool.Config().MaxConns; got != int32(n) {
		t.Fatalf("control: pool of %d connections, want %d", got, n)
	}
	return NewPG(pool), pool
}

// reservationsOf is how many reservation rows owner 2 has for object.
func reservationsOf(t *testing.T, pool *pgxpool.Pool, object string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(session(2), `SELECT count(*) FROM audit_log WHERE action = $1 AND object = $2`,
		ActionChiefDispatch, object).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestWaitersForTheOwnersLockLeaveThePoolToTheHolder: on a 2-connection pool,
// while one reservation holds the owner's lock (its check blocked), three more
// reservations of the owner's wait for it. The holder's check must still get a
// pool connection for its own read — a waiter sitting on a connection while it
// waited starved it until statement_timeout (57014). Then every waiter
// reserves.
func TestWaitersForTheOwnersLockLeaveThePoolToTheHolder(t *testing.T) {
	s, pool := smallPoolStore(t, 2)
	const task, waiters = 710, 3
	inside, release := make(chan struct{}), make(chan struct{})
	var readErr error
	holder := make(chan error, 1)
	go func() {
		_, err := s.ReserveChiefDispatch(session(2), NoteObject(task), nil, func() error {
			close(inside)
			<-release
			// The holder's nested read, through the pool, as agent_dispatch's
			// check reads the owner's turn and the task.
			ctx, cancel := context.WithTimeout(session(2), 2*time.Second)
			defer cancel()
			var one int
			readErr = pool.QueryRow(ctx, `SELECT 1`).Scan(&one)
			return readErr
		})
		holder <- err
	}()
	<-inside
	errs := make([]error, waiters)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.ReserveChiefDispatch(session(2), NoteObject(task+1+int64(i)), nil, nil)
		}(i)
	}
	time.Sleep(300 * time.Millisecond) // the waiters are waiting on the lock
	close(release)
	if err := <-holder; err != nil {
		t.Fatalf("the holder's reservation: %v (its nested read: %v)", err, readErr)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("waiter %d: %v", i, err)
		}
	}
	for i := int64(0); i <= waiters; i++ {
		if n := reservationsOf(t, pool, NoteObject(task+i)); n != 1 {
			t.Errorf("task %d: %d reservations, want 1", task+i, n)
		}
	}
}

// TestAWaitPastTheBoundIsBusyAndWritesNothing: a reservation that cannot take
// the owner's lock within chiefDispatchLockWait answers ErrChiefDispatchBusy
// (what dispatch_approve answers as a clean retry) and writes nothing.
func TestAWaitPastTheBoundIsBusyAndWritesNothing(t *testing.T) {
	s, pool := smallPoolStore(t, 2)
	old := chiefDispatchLockWait
	chiefDispatchLockWait = 200 * time.Millisecond
	t.Cleanup(func() { chiefDispatchLockWait = old })
	const task = 720
	inside, release := make(chan struct{}), make(chan struct{})
	holder := make(chan error, 1)
	go func() {
		_, err := s.ReserveChiefDispatch(session(2), NoteObject(task), nil, func() error {
			close(inside)
			<-release
			return nil
		})
		holder <- err
	}()
	<-inside
	_, err := s.ReserveChiefDispatch(session(2), NoteObject(task+1), nil, nil)
	close(release)
	if !errors.Is(err, ErrChiefDispatchBusy) {
		t.Fatalf("a reservation waiting past the bound: %v, want ErrChiefDispatchBusy", err)
	}
	if err := <-holder; err != nil {
		t.Fatal(err)
	}
	if n := reservationsOf(t, pool, NoteObject(task+1)); n != 0 {
		t.Errorf("the busy reservation wrote %d rows", n)
	}
}
