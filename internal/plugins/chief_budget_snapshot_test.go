package plugins

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestTheBudgetNeverMissesAStartWhoseAgentIsBeingRecorded: a start is live
// either as a pending reservation (no agent recorded yet) or as a recorded
// live agent — and the budget must see it as one or the other at EVERY
// instant, including while RecordChiefDispatchAgent commits. chiefBudget read
// those two halves as two statements under READ COMMITTED, so a read that
// straddled the commit counted the start under neither and came back one
// short. Here reads race the record, round after round, and every read must count
// the one start in flight. (A rate test by nature: on the two-statement read it
// failed within this loop on every run measured; on one statement it cannot.)
func TestTheBudgetNeverMissesAStartWhoseAgentIsBeingRecorded(t *testing.T) {
	s, pool, dsn := pgStore(t)
	dbtest.OwnerExec(t, dsn, `DELETE FROM audit_log WHERE action LIKE 'task.chief_%'`)
	store := agents.NewPG(pool)
	const starts = 60
	run := strconv.FormatInt(time.Now().UnixNano(), 36)
	for i := 0; i < starts; i++ {
		// Each round starts from an empty ledger and no live agent of the
		// owner's, so the caps never refuse the reservation and want is 1.
		dbtest.OwnerExec(t, dsn, `DELETE FROM audit_log WHERE action LIKE 'task.chief_%'`)
		dbtest.OwnerExec(t, dsn, `UPDATE agents SET status = 'stopped' WHERE owner_id = 2 AND name LIKE 'snap-%'`)
		obj, err := NewDispatchObject()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ReserveChiefDispatch(session(2), obj, nil, nil); err != nil {
			t.Fatal(err)
		}
		a, err := store.Create(session(2), agents.Agent{Name: "snap-" + run + "-" + strconv.Itoa(i), Namespace: "ns-snap-" + run + "-" + strconv.Itoa(i),
			Status: agents.StatusProvisioning})
		if err != nil {
			t.Fatal(err)
		}
		want := 1
		var (
			wg   sync.WaitGroup
			done = make(chan struct{})
		)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(done)
			if err := s.RecordChiefDispatchAgent(session(2), obj, a.ID); err != nil {
				t.Error(err)
			}
		}()
		reads := 0
		for stop := false; !stop; {
			select {
			case <-done:
				stop = true
			default:
			}
			b, err := s.ChiefDispatchBudget(session(2))
			if err != nil {
				t.Fatal(err)
			}
			reads++
			if b.Live != want {
				t.Fatalf("start %d, read %d: live %d, want %d — a start being recorded was counted as neither pending nor recorded",
					i, reads, b.Live, want)
			}
		}
		wg.Wait()
	}
}
