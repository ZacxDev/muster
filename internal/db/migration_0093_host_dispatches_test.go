package db_test

import (
	"strings"
	"testing"
)

// TestMigration0093HostDispatches: before 0093 there is no host_dispatches
// (the control); after it a row is accepted with its defaults (pending), and
// each CHECK refuses its own bad row — ended without ended_at, ended_at on an
// open row, an orphan notice on an open row, an unknown runtime, a host the
// wire would refuse, a window id that is not one, a held id of another shape.
// send_queue gains chief_forward_dispatch.
func TestMigration0093HostDispatches(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0093_host_dispatches", 92)
	if _, err := pool.Exec(ctx, `SELECT 1 FROM host_dispatches`); err == nil || !strings.Contains(err.Error(), "host_dispatches") {
		t.Fatalf("control: before 0093 host_dispatches exists: %v", err)
	}
	applyThroughFrom(ctx, t, pool, 93)
	id := scanID(ctx, t, pool, `INSERT INTO host_dispatches (owner_id, host, runtime, repo, reservation_id) VALUES (1, 'host-bravo', 'claude', 'acme/widgets', 7) RETURNING id`)
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM host_dispatches WHERE id = $1`, id).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("a new row is %q (%v), want pending", status, err)
	}
	for _, c := range []struct{ what, sql string }{
		{"ended without ended_at", `UPDATE host_dispatches SET status = 'ended' WHERE id = $1`},
		{"ended_at on an open row", `UPDATE host_dispatches SET ended_at = now() WHERE id = $1`},
		{"an orphan notice on an open row", `UPDATE host_dispatches SET orphan_notice_id = 5 WHERE id = $1`},
		{"an unknown runtime", `UPDATE host_dispatches SET runtime = 'sh' WHERE id = $1`},
		{"a host the wire would refuse", `UPDATE host_dispatches SET host = 'Host Bravo' WHERE id = $1`},
		{"a window id that is not one", `UPDATE host_dispatches SET tmux_window = 'agents:1' WHERE id = $1`},
		{"a held id of another shape", `UPDATE host_dispatches SET held_id = 'held:xyz' WHERE id = $1`},
	} {
		if _, err := pool.Exec(ctx, c.sql, id); err == nil || !strings.Contains(err.Error(), "violates check constraint") {
			t.Errorf("%s was not refused: %v", c.what, err)
		}
	}
	mustExec(ctx, t, pool, `UPDATE host_dispatches SET status = 'ended', ended_at = now(), orphan_notice_id = 5 WHERE id = $1`, id)
	if _, err := pool.Exec(ctx, `SELECT chief_forward_dispatch FROM send_queue LIMIT 1`); err != nil {
		t.Fatalf("send_queue has no chief_forward_dispatch: %v", err)
	}
}

// TestMigration0093OneOriginCoversTheChiefForward: 0064's one-origin CHECK,
// widened by 0093, refuses a row carrying a chief forward AND another origin
// (each pair), and admits each alone. Control: before 0093 the forward column
// does not exist. Then the header's ROLLBACK block runs as written and leaves
// 0064's rule (a route plus a nudge still refused) and no forward column.
func TestMigration0093OneOriginCoversTheChiefForward(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0093_one_origin", 93)
	ins := `INSERT INTO send_queue (owner_id, mirror_host, mirror_session, mirror_window, stepup_at, after_since, body, route_id, chief_nudge_id, chief_forward_dispatch)
		VALUES (1, 'host-bravo', 'agents', '', now(), 0, 'hi', $1, $2, $3)`
	for _, c := range []struct {
		what  string
		args  []any
		taken bool
	}{
		{"a forward alone", []any{nil, nil, 7}, true},
		{"a route alone", []any{"r-1", nil, nil}, true},
		{"a forward and a route", []any{"r-2", nil, 7}, false},
		{"a forward and a nudge", []any{nil, 3, 7}, false},
		{"a route and a nudge (0064's own)", []any{"r-3", 3, nil}, false},
	} {
		_, err := pool.Exec(ctx, ins, c.args...)
		if c.taken && err != nil {
			t.Errorf("%s: %v", c.what, err)
		}
		if !c.taken && (err == nil || !strings.Contains(err.Error(), "send_queue_one_origin")) {
			t.Errorf("%s was not refused by send_queue_one_origin: %v", c.what, err)
		}
	}
	for _, q := range asksRollbackSQL(t, "0093_host_dispatches.sql") {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("rollback %q: %v", q, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO send_queue (owner_id, mirror_host, mirror_session, mirror_window, stepup_at, after_since, body, route_id, chief_nudge_id)
		VALUES (1, 'host-bravo', 'agents', '', now(), 0, 'hi', 'r-4', 3)`); err == nil || !strings.Contains(err.Error(), "send_queue_one_origin") {
		t.Errorf("after the rollback 0064's rule is gone: %v", err)
	}
	if _, err := pool.Exec(ctx, `SELECT chief_forward_dispatch FROM send_queue LIMIT 1`); err == nil {
		t.Error("after the rollback send_queue still has chief_forward_dispatch")
	}
}
