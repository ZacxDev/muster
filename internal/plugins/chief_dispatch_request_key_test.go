package plugins

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// agent_dispatch's request_key in the ledger (requestKeyMatch): per owner,
// 24 hours, bound to its parameters' hash, freed by a refused create's release,
// and never subject to "one start per task" on a dispatch: object.

func keyed(key, hash string) map[string]any {
	return map[string]any{DetailRequestKey: key, DetailRequestHash: hash}
}

// TestADispatchObjectIsFreshAndHasNoPriorStart: NewDispatchObject mints
// dispatch:<32 hex>, never the same twice; two reservations on dispatch
// objects are two starts (no Prior), and a malformed object is refused.
func TestADispatchObjectIsFreshAndHasNoPriorStart(t *testing.T) {
	s, _, dsn := pgStore(t)
	dbtest.OwnerExec(t, dsn, `DELETE FROM audit_log WHERE action LIKE 'task.chief_%'`)
	a, err := NewDispatchObject()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewDispatchObject()
	if a == b || !regexp.MustCompile(`^dispatch:[0-9a-f]{32}$`).MatchString(a) {
		t.Fatalf("objects %q, %q", a, b)
	}
	for _, o := range []string{a, b} {
		r, err := s.ReserveChiefDispatch(session(2), o, nil, nil)
		if err != nil || r.Prior || r.Replayed || r.ReservationID <= 0 {
			t.Fatalf("%s: %+v %v", o, r, err)
		}
	}
	// The same dispatch object again is still no Prior: one start per task has
	// nothing to bind to there.
	if r, err := s.ReserveChiefDispatch(session(2), a, nil, nil); err != nil || r.Prior {
		t.Fatalf("again on %s: %+v %v", a, r, err)
	}
	for _, bad := range []string{"", "note:0", "dispatch", "agent:7"} {
		if _, err := s.ReserveChiefDispatch(session(2), bad, nil, nil); err == nil {
			t.Errorf("object %q was reserved", bad)
		}
	}
}

// TestARequestKeyIsPerOwnerAndBoundToItsHash: owner 2's key K replays for
// owner 2 with the same hash and is refused with another; owner 3's same K
// is a new reservation of its own; the read-only lookup agrees with the
// reservation's own check.
func TestARequestKeyIsPerOwnerAndBoundToItsHash(t *testing.T) {
	s, pool, dsn := pgStore(t)
	dbtest.OwnerExec(t, dsn, `DELETE FROM audit_log WHERE action LIKE 'task.chief_%'`)
	o2, _ := NewDispatchObject()
	first, err := s.ReserveChiefDispatch(session(2), o2, keyed("shared-key", "hash-a"), nil)
	if err != nil || first.Replayed {
		t.Fatalf("first: %+v %v", first, err)
	}
	if err := s.RecordChiefDispatchAgent(session(2), o2, 4242); err != nil {
		t.Fatal(err)
	}
	other, _ := NewDispatchObject()
	r, err := s.ReserveChiefDispatch(session(2), other, keyed("shared-key", "hash-a"), nil)
	if err != nil || !r.Replayed || r.ReservationID != first.ReservationID || r.Object != o2 || r.AgentID == nil || *r.AgentID != 4242 {
		t.Fatalf("replay: %+v %v; want reservation %d on %s with agent 4242", r, err, first.ReservationID, o2)
	}
	// A replay ran no check and wrote nothing: one reservation row only.
	if r, err := s.ReserveChiefDispatch(session(2), other, keyed("shared-key", "hash-a"), func() error {
		t.Error("a replay ran the caller's check")
		return nil
	}); err != nil || !r.Replayed {
		t.Fatalf("second replay: %+v %v", r, err)
	}
	if n := reservationRows(t, pool, 2); n != 1 {
		t.Fatalf("%d reservation rows after two replays, want 1", n)
	}
	if _, err := s.ReserveChiefDispatch(session(2), other, keyed("shared-key", "hash-b"), nil); !errors.Is(err, ErrRequestKeyReused) {
		t.Fatalf("a changed hash: %v; want ErrRequestKeyReused", err)
	}
	// Owner 3, same key, different hash: theirs alone, a fresh reservation.
	o3, _ := NewDispatchObject()
	r3, err := s.ReserveChiefDispatch(session(3), o3, keyed("shared-key", "hash-c"), nil)
	if err != nil || r3.Replayed || r3.ReservationID == first.ReservationID {
		t.Fatalf("owner 3: %+v %v; want a new reservation", r3, err)
	}
	if _, err := s.ReserveChiefDispatch(session(3), o3, keyed("shared-key", "hash-a"), nil); !errors.Is(err, ErrRequestKeyReused) {
		t.Fatalf("owner 3 with owner 2's hash: %v; want its own key's reuse refusal", err)
	}
}

// TestARefusedReleaseFreesItsRequestKey: a reservation released after a
// refused create (refused_reservation) no longer holds its key, so the same
// key and hash reserve afresh; an agent-destroy release (agent_id) of a task
// start does NOT free it.
func TestARefusedReleaseFreesItsRequestKey(t *testing.T) {
	s, _, dsn := pgStore(t)
	dbtest.OwnerExec(t, dsn, `DELETE FROM audit_log WHERE action LIKE 'task.chief_%'`)
	o, _ := NewDispatchObject()
	first, err := s.ReserveChiefDispatch(session(2), o, keyed("refused-key", "h1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if wrote, err := s.ReleaseRefusedChiefDispatch(session(2), o, first.ReservationID); err != nil || !wrote {
		t.Fatalf("refused release: %v %v", wrote, err)
	}
	o2, _ := NewDispatchObject()
	again, err := s.ReserveChiefDispatch(session(2), o2, keyed("refused-key", "h1"), nil)
	if err != nil || again.Replayed || again.ReservationID <= first.ReservationID {
		t.Fatalf("after the refused release: %+v %v; want a new reservation", again, err)
	}

	// A task start whose agent the owner destroyed: the key still replays.
	const task = 7310
	tr, err := s.ReserveChiefDispatch(session(2), NoteObject(task), keyed("task-key", "h2"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordChiefDispatchAgent(session(2), NoteObject(task), 5151); err != nil {
		t.Fatal(err)
	}
	if wrote, err := s.ReleaseChiefDispatch(session(2), task, 5151); err != nil || !wrote {
		t.Fatalf("destroy release: %v %v", wrote, err)
	}
	if r, err := s.ReserveChiefDispatch(session(2), NoteObject(task), keyed("task-key", "h2"), nil); err != nil || !r.Replayed || r.ReservationID != tr.ReservationID {
		t.Fatalf("after a destroy release: %+v %v; want the replay of reservation %d", r, err, tr.ReservationID)
	}
}

// TestAFailedStartIsMarkedForItsReplay: a reservation whose create failed
// after the unit was spent replays Failed once RecordChiefDispatchFailed names
// it — and not before, however old it is (no time window decides it).
func TestAFailedStartIsMarkedForItsReplay(t *testing.T) {
	s, _, dsn := pgStore(t)
	dbtest.OwnerExec(t, dsn, `DELETE FROM audit_log WHERE action LIKE 'task.chief_%'`)
	o, _ := NewDispatchObject()
	first, err := s.ReserveChiefDispatch(session(2), o, keyed("failed-key", "h"), nil)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.OwnerExec(t, dsn, `UPDATE audit_log SET at = at - interval '11 minutes' WHERE object = '`+o+`'`)
	other, _ := NewDispatchObject()
	if r, err := s.ReserveChiefDispatch(session(2), other, keyed("failed-key", "h"), nil); err != nil || !r.Replayed || r.Failed {
		t.Fatalf("unmarked, 11 minutes old: %+v %v; want a replay, not failed", r, err)
	}
	if err := s.RecordChiefDispatchFailed(session(2), o, first.ReservationID); err != nil {
		t.Fatal(err)
	}
	if r, err := s.ReserveChiefDispatch(session(2), other, keyed("failed-key", "h"), nil); err != nil || !r.Replayed || !r.Failed {
		t.Fatalf("marked: %+v %v; want a replay that failed", r, err)
	}
	// A marker naming ANOTHER reservation of the object marks nothing here.
	o2, _ := NewDispatchObject()
	second, _ := s.ReserveChiefDispatch(session(2), o2, keyed("other-key", "h"), nil)
	if err := s.RecordChiefDispatchFailed(session(2), o2, second.ReservationID+1000); err != nil {
		t.Fatal(err)
	}
	if r, err := s.ReserveChiefDispatch(session(2), other, keyed("other-key", "h"), nil); err != nil || !r.Replayed || r.Failed {
		t.Fatalf("a marker for another reservation: %+v %v", r, err)
	}
}

// reservationRows is owner's reservation rows.
func reservationRows(t *testing.T, pool *pgxpool.Pool, owner int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(authz.System(context.Background()),
		`SELECT count(*) FROM audit_log WHERE action = 'task.chief_dispatch' AND user_id = $1`, owner).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestAReplayedTaskStartNamesItsOwnAgent: on a task with two reservations (the
// first released by a destroy), each key replays the agent ITS reservation
// recorded, never the other's.
func TestAReplayedTaskStartNamesItsOwnAgent(t *testing.T) {
	s, _, dsn := pgStore(t)
	dbtest.OwnerExec(t, dsn, `DELETE FROM audit_log WHERE action LIKE 'task.chief_%'`)
	const task = 7320
	if _, err := s.ReserveChiefDispatch(session(2), NoteObject(task), keyed("k-one", "h"), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordChiefDispatchAgent(session(2), NoteObject(task), 6001); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReleaseChiefDispatch(session(2), task, 6001); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveChiefDispatch(session(2), NoteObject(task), keyed("k-two", "h"), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordChiefDispatchAgent(session(2), NoteObject(task), 6002); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]int64{"k-one": 6001, "k-two": 6002} {
		o, _ := NewDispatchObject()
		r, err := s.ReserveChiefDispatch(session(2), o, keyed(key, "h"), nil)
		if err != nil || !r.Replayed || r.AgentID == nil || *r.AgentID != want {
			t.Errorf("%s: %+v %v; want agent %d", key, r, err, want)
		}
	}
}
