package plugins

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/ZacxDev/muster/internal/dbtest"
)

// HELD DISPATCHES (dispatch_hold.go) over a real Postgres: a hold is one audit
// row of its owner's, open for DispatchHoldTTL, and consumed by the
// reservation that names it.

func fileHold(t *testing.T, s *PGStore, uid int64, key string) DispatchHold {
	t.Helper()
	h, err := s.FileDispatchHold(session(uid), map[string]any{"label": key, "target": "pod/claude-code", "prompt": "p-" + key})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// TestAHoldIsOpenUntilAReservationNamesIt: a fresh hold is open, carries its
// detail and expires DispatchHoldTTL after it was filed; a reservation of
// ANOTHER object that names nothing leaves it open; the reservation naming it
// consumes it.
func TestAHoldIsOpenUntilAReservationNamesIt(t *testing.T) {
	s, _, dsn := pgStore(t)
	dbtest.OwnerExec(t, dsn, `DELETE FROM audit_log WHERE action LIKE 'chief.dispatch_%' OR action LIKE 'task.chief_%'`)
	h := fileHold(t, s, 2, "k-open")
	if !ValidHoldID(h.ID) || !h.Open() || h.ConsumedBy != 0 || h.Expired {
		t.Fatalf("a fresh hold: %+v; want an open hold with a hold id", h)
	}
	if got := h.ExpiresAt.Sub(h.At); got != DispatchHoldTTL {
		t.Errorf("expires %v after it was filed, want %v", got, DispatchHoldTTL)
	}
	var d map[string]any
	if err := json.Unmarshal(h.Detail, &d); err != nil || d["prompt"] != "p-k-open" {
		t.Errorf("detail %s: want the request as filed", h.Detail)
	}
	other, _ := NewDispatchObject()
	if _, err := s.ReserveChiefDispatch(session(2), other, map[string]any{"target": "pod/claude-code"}, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := s.DispatchHold(session(2), h.ID); err != nil || !got.Open() {
		t.Fatalf("after an unrelated reservation: %+v %v; want still open", got, err)
	}
	obj, _ := NewDispatchObject()
	res, err := s.ReserveChiefDispatch(session(2), obj, map[string]any{DetailHeld: h.ID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.DispatchHold(session(2), h.ID)
	if err != nil || got.Open() || got.ConsumedBy != res.ReservationID {
		t.Fatalf("after the reservation naming it: %+v %v; want consumed by reservation %d", got, err, res.ReservationID)
	}
}

// TestAHoldExpiresByTheDatabasesClock: a hold filed DispatchHoldTTL and a
// minute ago reads Expired (not open); one filed a minute short of it does not.
func TestAHoldExpiresByTheDatabasesClock(t *testing.T) {
	s, _, dsn := pgStore(t)
	old, fresh := fileHold(t, s, 2, "k-old"), fileHold(t, s, 2, "k-fresh")
	dbtest.OwnerExec(t, dsn, `UPDATE audit_log SET at = at - interval '16 minutes' WHERE object = '`+old.ID+`'`)
	dbtest.OwnerExec(t, dsn, `UPDATE audit_log SET at = at - interval '14 minutes' WHERE object = '`+fresh.ID+`'`)
	if got, err := s.DispatchHold(session(2), old.ID); err != nil || !got.Expired || got.Open() {
		t.Errorf("a hold 16 minutes old: %+v %v; want expired", got, err)
	}
	if got, err := s.DispatchHold(session(2), fresh.ID); err != nil || got.Expired || !got.Open() {
		t.Errorf("a hold 14 minutes old: %+v %v; want open", got, err)
	}
}

// TestAHoldIsItsOwnersOnly: another owner reads it as missing, its owner
// reads it; a malformed id is missing without a query.
func TestAHoldIsItsOwnersOnly(t *testing.T) {
	s, _, _ := pgStore(t)
	h := fileHold(t, s, 2, "k-mine")
	if _, err := s.DispatchHold(session(3), h.ID); !errors.Is(err, ErrHoldNotFound) {
		t.Errorf("owner 3 reading owner 2's hold: %v; want ErrHoldNotFound", err)
	}
	if got, err := s.DispatchHold(session(2), h.ID); err != nil || got.ID != h.ID {
		t.Errorf("owner 2 reading its own hold: %+v %v", got, err)
	}
	for _, bad := range []string{"", "held:", "held:XYZ", "dispatch:" + h.ID[5:], h.ID + "0"} {
		if _, err := s.DispatchHold(session(2), bad); !errors.Is(err, ErrHoldNotFound) {
			t.Errorf("hold id %q: %v; want ErrHoldNotFound", bad, err)
		}
	}
}

// TestAHoldIsTheCallersWithoutRLS: with row-level security off (audit_log
// included), another owner's hold still reads as missing, and another owner's
// reservation naming the caller's hold does not consume it — the reads keep to
// the caller's rows in SQL.
func TestAHoldIsTheCallersWithoutRLS(t *testing.T) {
	s, pool, dsn := pgStore(t)
	mine, theirs := fileHold(t, s, 2, "k-rls-mine"), fileHold(t, s, 3, "k-rls-theirs")
	obj, _ := NewDispatchObject()
	if _, err := s.ReserveChiefDispatch(session(3), obj, map[string]any{DetailHeld: mine.ID}, nil); err != nil {
		t.Fatal(err)
	}
	rlsOffForTest(t, pool, dsn, true)
	if _, err := s.DispatchHold(session(2), theirs.ID); !errors.Is(err, ErrHoldNotFound) {
		t.Errorf("owner 2 reading owner 3's hold with RLS off: %v; want ErrHoldNotFound", err)
	}
	if got, err := s.DispatchHold(session(2), mine.ID); err != nil || !got.Open() {
		t.Errorf("owner 2's hold, named by owner 3's reservation, RLS off: %+v %v; want still open", got, err)
	}
}

// TestAnApprovalIsRecordedOnTheHold: RecordDispatchApproved writes one
// ActionDispatchApproved row of the owner's whose object is the hold id.
func TestAnApprovalIsRecordedOnTheHold(t *testing.T) {
	s, pool, _ := pgStore(t)
	h := fileHold(t, s, 2, "k-approved")
	if err := s.RecordDispatchApproved(session(2), h.ID, map[string]any{"agent": "a-1"}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(session(2), `SELECT count(*) FROM audit_log WHERE action = $1 AND object = $2 AND user_id = 2
		AND detail->>'agent' = 'a-1'`, ActionDispatchApproved, h.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("approval rows %d %v; want 1", n, err)
	}
	if err := s.RecordDispatchApproved(session(2), "nope", nil); !errors.Is(err, ErrHoldNotFound) {
		t.Errorf("approval of a malformed hold id: %v; want ErrHoldNotFound", err)
	}
}
