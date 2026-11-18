package db_test

import (
	"strconv"
	"strings"
	"testing"
)

// TestMigration0062AdmitsOwnerAnsweredWithItsTime: at version 61 the state
// owner_answered is refused by session_routes_state_check (the control); after
// 0062 it is stored WITH owner_replied_at, refused without it, and
// owner_replied_at is refused on any other state. Existing rows keep their
// states, and plugin_messages.human_conversation defaults to false on a row
// written before it existed.
func TestMigration0062AdmitsOwnerAnsweredWithItsTime(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0062_owner_answered", 61)
	agent := scanID(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace) VALUES (1, 'owner-answered-agent', 'ns-oa') RETURNING id`)
	plugin := scanID(ctx, t, pool, `INSERT INTO plugins (owner_id, name, label, token_hash, data_scope)
		VALUES (1, 'plugin-oa', 'Example inbox', 'pluginhash-oa', 'example') RETURNING id`)
	n := 0
	message := func() int64 {
		n++
		return scanID(ctx, t, pool, `INSERT INTO plugin_messages (owner_id, plugin_id, external_id, body, status, agent_id)
			VALUES (1, $1, $2, 'hello', 'delivering', $3) RETURNING id`, plugin, "ext-oa-"+strconv.Itoa(n), agent)
	}
	route := func(state string, withTime bool) error {
		m := message()
		key := "oa-route-" + strconv.FormatInt(m, 10)
		_, err := pool.Exec(ctx, `INSERT INTO session_routes (owner_id, route_id, plugin_message_id, agent_id, target_kind, target_key,
			timeout_at, state, finished_at) VALUES (1, 'rt_' || substr(md5($1), 1, 24), $2, $3, 'mirror', 'sess-oa', now(), $4, now())`,
			key, m, agent, state)
		if err != nil || !withTime {
			return err
		}
		tag, err := pool.Exec(ctx, `UPDATE session_routes SET owner_replied_at = now() WHERE route_id = 'rt_' || substr(md5($1), 1, 24)`, key)
		if err == nil && tag.RowsAffected() != 1 {
			t.Fatalf("instrument: the update reached %d rows, want the route just written", tag.RowsAffected())
		}
		return err
	}
	old := message()
	if err := route("owner_answered", false); err == nil || !strings.Contains(err.Error(), "session_routes_state_check") {
		t.Fatalf("control: at version 61 owner_answered was stored (or refused for another reason): %v", err)
	}
	if err := route("timed_out", false); err != nil {
		t.Fatalf("control: at version 61 a timed_out route was refused: %v", err)
	}

	applyThroughFrom(ctx, t, pool, 62)

	var human bool
	if err := pool.QueryRow(ctx, `SELECT human_conversation FROM plugin_messages WHERE id = $1`, old).Scan(&human); err != nil || human {
		t.Fatalf("a message from before 0062: human_conversation %v (%v); want false", human, err)
	}
	var timedOut int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM session_routes WHERE state = 'timed_out'`).Scan(&timedOut); err != nil || timedOut != 1 {
		t.Fatalf("the route from before 0062: %d timed_out (%v); want it kept", timedOut, err)
	}
	// Inserting with the state and no time, then adding the time, is refused at
	// the insert: the check holds on every row, not only after an update.
	if err := route("owner_answered", false); err == nil || !strings.Contains(err.Error(), "session_routes_owner_replied_check") {
		t.Fatalf("owner_answered with no owner_replied_at: %v; want refused by session_routes_owner_replied_check", err)
	}
	_, err := pool.Exec(ctx, `INSERT INTO session_routes (owner_id, route_id, plugin_message_id, agent_id, target_kind, target_key,
		timeout_at, state, finished_at, owner_replied_at) VALUES (1, 'rt_' || substr(md5('oa-ok'), 1, 24), $1, $2, 'mirror', 'sess-oa',
		now(), 'owner_answered', now(), now())`, message(), agent)
	if err != nil {
		t.Fatalf("owner_answered with owner_replied_at: %v; want stored", err)
	}
	for _, st := range []string{"timed_out", "failed"} {
		if err := route(st, true); err == nil || !strings.Contains(err.Error(), "session_routes_owner_replied_check") {
			t.Errorf("owner_replied_at on a %s route: %v; want refused by session_routes_owner_replied_check", st, err)
		}
	}
}
