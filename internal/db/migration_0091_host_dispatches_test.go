package db_test

import (
	"strings"
	"testing"
)

// TestMigration0091HostDispatches: before 0091 there is no host_dispatches
// (the control); after it a row is accepted with its defaults (pending), and
// each CHECK refuses its own bad row — ended without ended_at, ended_at on an
// open row, an orphan notice on an open row, an unknown runtime, a host the
// wire would refuse, a window id that is not one, a held id of another shape.
// send_queue gains chief_forward_dispatch.
func TestMigration0091HostDispatches(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0091_host_dispatches", 90)
	if _, err := pool.Exec(ctx, `SELECT 1 FROM host_dispatches`); err == nil || !strings.Contains(err.Error(), "host_dispatches") {
		t.Fatalf("control: before 0091 host_dispatches exists: %v", err)
	}
	applyThroughFrom(ctx, t, pool, 91)
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
