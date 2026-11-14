package notes

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
)

// TestAFinishedLinkDoesNotRefuseAMove (team sharing phase 4, slice 4.7,
// closing slice 4.3's limitation (a)): a task whose only links are FINISHED —
// an ask that was answered, cancelled or expired; a session route that timed
// out or failed; a host dispatch that ended — moves into the team, and those
// rows stay their owner's. A LIVE one of each (an open ask, an open route, a
// running host dispatch) still refuses, naming it; nothing moved.
//
// 🔴 RED at origin/main dc2fba34: every row refused, finished ones included.
func TestAFinishedLinkDoesNotRefuseAMove(t *testing.T) {
	w := newTeamTaskWorld(t)
	actx := w.as(t, w.a)
	sys := authz.System(context.Background())
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := w.pool.Exec(sys, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	routeID := func() string {
		var b [12]byte
		_, _ = rand.Read(b[:])
		return "rt_" + hex.EncodeToString(b[:])
	}
	var reservation int64 = 900000
	link := func(id int64, finished bool) {
		t.Helper()
		reservation++
		if finished {
			exec(`INSERT INTO attention_asks (owner_id, mirror_host, mirror_session, question, expires_at, task_id, status, answer, answered_by, resolved_at, delivery)
				VALUES ($1, 'h', 's', 'answered?', now() + interval '1 hour', $2, 'answered', 'yes', 'operator', now(), 'pull')`, w.a, id)
			exec(`INSERT INTO attention_asks (owner_id, mirror_host, mirror_session, question, expires_at, task_id, status, resolved_at)
				VALUES ($1, 'h', 's', 'cancelled?', now() + interval '1 hour', $2, 'cancelled', now())`, w.a, id)
			exec(`INSERT INTO attention_asks (owner_id, mirror_host, mirror_session, question, created_at, expires_at, task_id)
				VALUES ($1, 'h', 's', 'lapsed?', now() - interval '2 hours', now() - interval '1 minute', $2)`, w.a, id)
			exec(`INSERT INTO session_routes (owner_id, route_id, task_id, target_kind, target_key, timeout_at, state, finished_at)
				VALUES ($1, $2, $3, 'agent', 'x', now(), 'timed_out', now())`, w.a, routeID(), id)
			exec(`INSERT INTO session_routes (owner_id, route_id, task_id, target_kind, target_key, timeout_at, state, finished_at)
				VALUES ($1, $2, $3, 'agent', 'x', now(), 'failed', now())`, w.a, routeID(), id)
			exec(`INSERT INTO host_dispatches (owner_id, host, runtime, repo, reservation_id, task_id, status, ended_at)
				VALUES ($1, 'h', 'claude', 'acme/widgets', $2, $3, 'ended', now())`, w.a, reservation, id)
			return
		}
		exec(`INSERT INTO attention_asks (owner_id, mirror_host, mirror_session, question, expires_at, task_id)
			VALUES ($1, 'h', 's', 'open?', now() + interval '1 hour', $2)`, w.a, id)
		exec(`INSERT INTO session_routes (owner_id, route_id, task_id, target_kind, target_key, timeout_at)
			VALUES ($1, $2, $3, 'agent', 'x', now() + interval '1 hour')`, w.a, routeID(), id)
		exec(`INSERT INTO host_dispatches (owner_id, host, runtime, repo, reservation_id, task_id, status)
			VALUES ($1, 'h', 'claude', 'acme/widgets', $2, $3, 'running')`, w.a, reservation, id)
	}
	t.Cleanup(func() {
		_, _ = w.pool.Exec(sys, `DELETE FROM attention_asks WHERE owner_id = $1`, w.a)
		_, _ = w.pool.Exec(sys, `DELETE FROM session_routes WHERE owner_id = $1`, w.a)
		_, _ = w.pool.Exec(sys, `DELETE FROM host_dispatches WHERE owner_id = $1`, w.a)
	})

	done, err := w.store.Create(actx, Note{Title: "finished links", Body: "b"})
	if err != nil {
		t.Fatal(err)
	}
	link(done.ID, true)
	if err := w.store.MoveToTeam(sys, done.ID, w.a, w.t); err != nil {
		t.Fatalf("a task whose links are all finished: %v; want it moved", err)
	}
	var owner int64
	_ = w.pool.QueryRow(sys, `SELECT owner_id FROM notes WHERE id = $1`, done.ID).Scan(&owner)
	if owner != w.t {
		t.Errorf("the moved task is owned by %d; want the team %d", owner, w.t)
	}
	var stayed int
	_ = w.pool.QueryRow(sys, `SELECT (SELECT count(*) FROM attention_asks WHERE task_id = $1 AND owner_id = $2)
		+ (SELECT count(*) FROM session_routes WHERE task_id = $1 AND owner_id = $2)
		+ (SELECT count(*) FROM host_dispatches WHERE task_id = $1 AND owner_id = $2)`, done.ID, w.a).Scan(&stayed)
	if stayed != 6 {
		t.Errorf("%d finished row(s) still A's and linked; want all 6, untouched", stayed)
	}

	live, err := w.store.Create(actx, Note{Title: "live links", Body: "b"})
	if err != nil {
		t.Fatal(err)
	}
	link(live.ID, false)
	err = w.store.MoveToTeam(sys, live.ID, w.a, w.t)
	var blocked *MoveBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("a task with live links: %v; want a refusal", err)
	}
	for _, k := range []string{"attention_asks.task_id", "session_routes.task_id", "host_dispatches.task_id"} {
		tbl, _, _ := strings.Cut(k, ".")
		if !strings.Contains(err.Error(), MoveRefusals[k].Reason+" ("+tbl+": 1)") {
			t.Errorf("the refusal %q does not name the live %s", err, k)
		}
	}
	_ = w.pool.QueryRow(sys, `SELECT owner_id FROM notes WHERE id = $1`, live.ID).Scan(&owner)
	if owner != w.a {
		t.Errorf("a refused move left the task owned by %d; want A %d", owner, w.a)
	}
}
