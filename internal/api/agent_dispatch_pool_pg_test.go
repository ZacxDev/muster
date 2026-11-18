package api

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ZacxDev/muster/internal/plugins"
)

// syncBuffer is a bytes.Buffer safe for the concurrent writes of a logger
// shared by goroutines (log.Logger serialises its own writes; this also
// serialises the read).
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestConcurrentAgentDispatchesOnASmallPoolDoNotStarve: more concurrent
// agent_dispatch starts of one owner than the pool has connections. Each
// reservation serialises on the owner's advisory lock, and the holder's check
// reads through the pool (the owner's turn in the chief's chat). A waiter that
// sits on a pool connection while it waits for the lock starves the holder of
// the connection its check needs, so nothing moves until statement_timeout
// (10 s, SQLSTATE 57014) cancels the waiters, which answer "the start could
// not be reserved".
//
// Every answer is classified: exactly MaxChiefLiveAgents starts and the rest
// the live-cap refusal, nothing logged as a failed reservation, and well
// inside one statement timeout.
func TestConcurrentAgentDispatchesOnASmallPoolDoNotStarve(t *testing.T) {
	const poolConns = 2
	const calls = 3 * poolConns
	if calls <= plugins.MaxChiefLiveAgents {
		t.Fatalf("fixture: %d calls cannot reach the live cap %d", calls, plugins.MaxChiefLiveAgents)
	}
	withPoolMaxConns(t, poolConns)
	f := newReleaseFixture(t)
	if got := f.pool.Config().MaxConns; got != poolConns {
		t.Fatalf("control: the fixture's pool has %d connections, want %d", got, poolConns)
	}
	f.ownerTurn(t, f.chief, f.owner, "Start these.")
	logs := &syncBuffer{}
	f.srv.logger = log.New(logs, "", 0)

	answers := make([]string, calls)
	var wg sync.WaitGroup
	began := time.Now()
	for i := range answers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := "pool-" + strconv.Itoa(i)
			answers[i] = f.callRaw(f.chief, agentDispatchTool, noTask(key, "pool work "+key))
		}(i)
	}
	wg.Wait()
	took := time.Since(began)

	started, capped := 0, 0
	for i, a := range answers {
		switch {
		case strings.Contains(a, `"agent_id"`) && !strings.Contains(a, `"error"`):
			started++
		case strings.Contains(a, `"error"`) && errorText(t, a) == chiefDispatchLiveCap:
			capped++
		default:
			t.Errorf("call %d answered neither a start nor the live-cap refusal: %s", i, a)
		}
	}
	if started != plugins.MaxChiefLiveAgents || capped != calls-plugins.MaxChiefLiveAgents {
		t.Errorf("%d starts and %d live-cap refusals from %d concurrent calls; want %d and %d",
			started, capped, calls, plugins.MaxChiefLiveAgents, calls-plugins.MaxChiefLiveAgents)
	}
	if l := logs.String(); strings.Contains(l, "reserve a start") {
		t.Errorf("a reservation failed:\n%s", l)
	}
	// Far inside one statement timeout: a starved holder waits the whole 10 s.
	if took > 5*time.Second {
		t.Errorf("%d concurrent calls on a %d-connection pool took %s", calls, poolConns, took)
	}
	if got := len(f.ownersAgents(t)); got != plugins.MaxChiefLiveAgents {
		t.Errorf("%d agents, want %d", got, plugins.MaxChiefLiveAgents)
	}
}

// busyReserve is a ledger whose every reservation waited out the owner's lock.
type busyReserve struct{ plugins.DispatchStore }

func (busyReserve) ReserveChiefDispatch(context.Context, string, map[string]any, func() error) (plugins.ChiefDispatch, error) {
	return plugins.ChiefDispatch{}, plugins.ErrChiefDispatchBusy
}

// TestABusyAgentDispatchIsACleanRetry: a reservation that only waited too long
// answers the chief its own retry sentence — not the logged "could not be
// reserved" error — and, nothing having been reserved, the same call with the
// same request_key then starts (a start, not a replay).
func TestABusyAgentDispatchIsACleanRetry(t *testing.T) {
	f := newReleaseFixture(t)
	f.ownerTurn(t, f.chief, f.owner, "Start it.")
	logs := &syncBuffer{}
	f.srv.logger = log.New(logs, "", 0)
	real := f.srv.ext.ChiefDispatch
	f.srv.ext.ChiefDispatch = busyReserve{real}
	text := f.callRaw(f.chief, agentDispatchTool, noTask("busy-1", "busy work"))
	if got := errorText(t, text); got != agentDispatchBusy {
		t.Fatalf("a busy reservation answered %s\nwant %q", text, agentDispatchBusy)
	}
	if l := logs.String(); l != "" {
		t.Errorf("a busy reservation logged:\n%s", l)
	}
	if got := len(f.ownersAgents(t)); got != 0 {
		t.Fatalf("%d agents after a busy reservation, want 0", got)
	}
	f.srv.ext.ChiefDispatch = real
	text = f.callRaw(f.chief, agentDispatchTool, noTask("busy-1", "busy work"))
	if strings.Contains(text, `"error"`) || strings.Contains(text, `"replayed"`) || !strings.Contains(text, `"agent_id"`) {
		t.Fatalf("the retry after a busy reservation answered %s; want a start", text)
	}
}

// TestReservationBusyNamesEveryWaitOnly: the owner's lock waited out (alone or
// with the context's own error), a statement or lock timeout and a deadline
// are a clean retry; any other failure is not.
func TestReservationBusyNamesEveryWaitOnly(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"lock wait bound", plugins.ErrChiefDispatchBusy, true},
		{"lock wait, context ended", errors.Join(plugins.ErrChiefDispatchBusy, context.Canceled), true},
		{"statement timeout", &pgconn.PgError{Code: "57014"}, true},
		{"lock timeout", &pgconn.PgError{Code: "55P03"}, true},
		{"deadline", context.DeadlineExceeded, true},
		{"a unique violation", &pgconn.PgError{Code: "23505"}, false},
		{"any other error", errors.New("boom"), false},
	} {
		if got := reservationBusy(c.err); got != c.want {
			t.Errorf("%s: reservationBusy = %v, want %v", c.name, got, c.want)
		}
	}
}
