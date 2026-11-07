package db_test

import (
	"strings"
	"testing"
)

// TestMigration0092RecordsTheOwnersDoor: at version 91 'browser' is refused by
// 0034's CHECK (the control: the rule really changed); 0092 admits 'browser' and
// 'device' ONLY on an answered row naming no mirror host or session, keeps
// 'mcp' exactly as 0034 had it, and refuses any other door.
func TestMigration0092RecordsTheOwnersDoor(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0092_answer_door", 91)
	agent := scanID(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace) VALUES (1, 'door-agent', 'ns-door') RETURNING id`)
	open := func() int64 {
		return scanID(ctx, t, pool, `INSERT INTO attention_asks (owner_id, agent_id, question, options, expires_at)
			VALUES (1, $1, 'which host?', '["host-bravo","host-charlie"]', now() + interval '1 hour') RETURNING id`, agent)
	}
	answered := func() int64 {
		id := open()
		mustExec(ctx, t, pool, `UPDATE attention_asks SET status = 'answered', answer = 'host-bravo', answered_by = 'operator', resolved_at = now() WHERE id = $1`, id)
		return id
	}
	set := `UPDATE attention_asks SET answered_via = $2, answered_via_host = $3, answered_via_session = $4 WHERE id = $1`
	refused := func(what string, id int64, v, host, sess any) {
		t.Helper()
		if _, err := pool.Exec(ctx, set, id, v, host, sess); err == nil || !strings.Contains(err.Error(), "attention_asks_answered_via_check") {
			t.Errorf("%s was not refused by the door CHECK: %v", what, err)
		}
	}
	refused("control: 'browser' before 0092", answered(), "browser", nil, nil)

	applyThroughFrom(ctx, t, pool, 92)

	for _, door := range []string{"browser", "device"} {
		if _, err := pool.Exec(ctx, set, answered(), door, nil, nil); err != nil {
			t.Errorf("after 0092, an answered row through %q: %v", door, err)
		}
		refused(door+" on an open ask", open(), door, nil, nil)
		refused(door+" naming a mirror host", answered(), door, "ws", nil)
		refused(door+" naming a session", answered(), door, nil, "0b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5b")
	}
	refused("an unknown door", answered(), "telepathy", nil, nil)
	refused("mcp without a host (0034's rule, kept)", answered(), "mcp", nil, "0b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5b")
	if _, err := pool.Exec(ctx, set, answered(), "mcp", "ws", "0b2c3d4e-5f60-4718-8a9b-0c1d2e3f4a5b"); err != nil {
		t.Errorf("after 0092, an mcp answer to an agent's ask: %v", err)
	}
}

// TestMigration0092RollsBack: the header's ROLLBACK block runs as written on a
// database at 0092 holding a 'browser' answer and an owner card, and leaves
// 0034's rule: 'browser' refused again, 'mcp' kept, both new columns gone, the
// version unrecorded. It is the block the operator would paste.
func TestMigration0092RollsBack(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0092_rollback", 92)
	agent := scanID(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace) VALUES (1, 'rb-agent', 'ns-rb') RETURNING id`)
	answered := func() int64 {
		id := scanID(ctx, t, pool, `INSERT INTO attention_asks (owner_id, agent_id, question, options, expires_at, owner_card, asked_session)
			VALUES (1, $1, 'which host?', '["host-bravo"]', now() + interval '1 hour', true, 'agent:main') RETURNING id`, agent)
		mustExec(ctx, t, pool, `UPDATE attention_asks SET status = 'answered', answer = 'host-bravo', answered_by = 'operator', resolved_at = now() WHERE id = $1`, id)
		return id
	}
	mustExec(ctx, t, pool, `UPDATE attention_asks SET answered_via = 'browser' WHERE id = $1`, answered())
	rb := asksRollbackSQL(t, "0092_attention_answer_door.sql")
	if len(rb) != 5 {
		t.Fatalf("the ROLLBACK block has %d statements: %q", len(rb), rb)
	}
	for _, q := range rb {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("rollback %q: %v", q, err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE attention_asks SET answered_via = 'browser' WHERE id = (SELECT max(id) FROM attention_asks)`); err == nil ||
		!strings.Contains(err.Error(), "attention_asks_answered_via_check") {
		t.Errorf("after the rollback 'browser' was not refused: %v", err)
	}
	var cols int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'attention_asks' AND column_name IN ('owner_card', 'asked_session')`).Scan(&cols); err != nil || cols != 0 {
		t.Errorf("after the rollback %d of the new columns remain (%v)", cols, err)
	}
	var v int
	if err := pool.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&v); err != nil || v != 91 {
		t.Errorf("after the rollback the version is %d (%v)", v, err)
	}
}
