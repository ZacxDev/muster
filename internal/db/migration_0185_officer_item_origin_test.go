package db_test

import (
	"strings"
	"testing"
)

// TestMigration0185OfficerItemOrigin: before 0185 neither an ask nor a draft
// has origin_* (the control). At 0185 an ask and a draft record an officer
// instance, its session, a message of that session and a time; the database
// refuses an origin agent of ANOTHER owner, a session of another agent and a
// message of another session; there is no origin_at (the row's created_at
// is when); deleting the agent clears the origin (its sessions go with it)
// and keeps the row; and the documented rollback removes
// every column, the trigger and its function.
func TestMigration0185OfficerItemOrigin(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0185_officer_item_origin", 184)
	for _, tbl := range []string{"attention_asks", "plugin_reply_drafts"} {
		if _, err := pool.Exec(ctx, `SELECT origin_agent_id FROM `+tbl); err == nil {
			t.Fatalf("control: before 0185 %s.origin_agent_id exists", tbl)
		}
	}
	applyThroughFrom(ctx, t, pool, 185)
	for _, tbl := range []string{"attention_asks", "plugin_reply_drafts"} {
		if _, err := pool.Exec(ctx, `SELECT origin_at FROM `+tbl); err == nil {
			t.Fatalf("%s.origin_at exists: when is the row's own created_at", tbl)
		}
	}

	if _, err := pool.Exec(ctx, `INSERT INTO users (id, kind, role, display_name) OVERRIDING SYSTEM VALUE VALUES (2, 'person', 'member', 'two') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	inst := scanID(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace) VALUES (1, 'officer-scout-main-1', 'ns-o1') RETURNING id`)
	other := scanID(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace) VALUES (2, 'officer-scout-main-1-two', 'ns-o2') RETURNING id`)
	sess := scanID(ctx, t, pool, `INSERT INTO chat_sessions (agent_id, owner_id, session_key) VALUES ($1, 1, 'k-0185-a') RETURNING id`, inst)
	sess2 := scanID(ctx, t, pool, `INSERT INTO chat_sessions (agent_id, owner_id, session_key) VALUES ($1, 1, 'k-0185-b') RETURNING id`, inst)
	otherSess := scanID(ctx, t, pool, `INSERT INTO chat_sessions (agent_id, owner_id, session_key) VALUES ($1, 2, 'k-0185-c') RETURNING id`, other)
	msg := scanID(ctx, t, pool, `INSERT INTO chat_messages (agent_id, owner_id, session_id, role, content) VALUES ($1, 1, $2, 'assistant', 'thinking') RETURNING id`, inst, sess)
	plugin := scanID(ctx, t, pool, `INSERT INTO plugins (owner_id, name, label, token_hash, data_scope, capabilities, deliver_to)
		VALUES (1, 'origin-relay', 'Relay', 'h-0185', 'example', '{messages,draft_checks}', 'officer:scout:main') RETURNING id`)

	ask := func(agent, session, message any) error {
		_, err := pool.Exec(ctx, `INSERT INTO attention_asks (owner_id, asker_label, question, options, expires_at, system_asker,
			origin_agent_id, origin_session_id, origin_message_id)
			VALUES (1, 'officer scout:main', 'which first?', '[]', now() + interval '1 hour', true, $1, $2, $3)`, agent, session, message)
		return err
	}
	draft := func(agent, session, message any) error {
		_, err := pool.Exec(ctx, `INSERT INTO plugin_reply_drafts (owner_id, message_id, agent_id, agent_name, body, expires_at, check_state,
			check_due_at, plugin_id, venue, target_kind, target_ref, officer, origin_agent_id, origin_session_id, origin_message_id)
			VALUES (1, NULL, $1, 'officer-scout-main-1', 'a post', now() + interval '7 days', 'awaiting', now() + interval '7 days', $2, 'x', 'post',
			        NULL, 'scout:main', $3, $4, $5)`, inst, plugin, agent, session, message)
		return err
	}
	for name, write := range map[string]func(a, s, m any) error{"ask": ask, "draft": draft} {
		if err := write(inst, sess, msg); err != nil {
			t.Fatalf("%s: the instance, its session and a message of it: %v", name, err)
		}
		if err := write(inst, sess, nil); err != nil {
			t.Fatalf("%s: a session with no message yet: %v", name, err)
		}
		if err := write(nil, nil, nil); err != nil {
			t.Fatalf("%s: no origin (an older row's shape): %v", name, err)
		}
		for _, bad := range []struct {
			why     string
			a, s, m any
		}{
			{"another owner's agent", other, otherSess, nil},
			{"another agent's session", inst, otherSess, nil},
			{"a message of another session", inst, sess2, msg},
		} {
			err := write(bad.a, bad.s, bad.m)
			if err == nil || !strings.Contains(err.Error(), "origin") {
				t.Errorf("%s: accepted %s (%v)", name, bad.why, err)
			}
		}
	}
	// The agent is deleted (its drafts first: they reference it as their
	// agent_id): the asks' origin goes, the asks stay.
	if _, err := pool.Exec(ctx, `DELETE FROM plugin_reply_drafts WHERE agent_id = $1`, inst); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM agents WHERE id = $1`, inst); err != nil {
		t.Fatalf("deleting the origin agent: %v", err)
	}
	var left int
	// Three asks were written (instance+session+message, instance+session,
	// and none): all three stay, none names anything.
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM attention_asks WHERE origin_agent_id IS NULL AND origin_session_id IS NULL AND origin_message_id IS NULL`).Scan(&left); err != nil || left != 3 {
		t.Fatalf("after the agent's delete: %d asks with the origin cleared (%v); want 3", left, err)
	}

	// ROLLBACK, as the migration's header writes it.
	for _, stmt := range []string{
		`DROP TRIGGER IF EXISTS attention_asks_origin_is_owners ON public.attention_asks`,
		`DROP TRIGGER IF EXISTS plugin_reply_drafts_origin_is_owners ON public.plugin_reply_drafts`,
		`DROP FUNCTION IF EXISTS public.officer_origin_is_owners()`,
		`ALTER TABLE public.attention_asks DROP COLUMN IF EXISTS origin_agent_id, DROP COLUMN IF EXISTS origin_session_id, DROP COLUMN IF EXISTS origin_message_id`,
		`ALTER TABLE public.plugin_reply_drafts DROP COLUMN IF EXISTS origin_agent_id, DROP COLUMN IF EXISTS origin_session_id, DROP COLUMN IF EXISTS origin_message_id`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("rollback %q: %v", stmt, err)
		}
	}
	if !strings.Contains(readMigration(t, "0185_officer_item_origin.sql"), "DROP FUNCTION IF EXISTS public.officer_origin_is_owners();") {
		t.Fatal("the migration's header no longer documents the rollback this test runs")
	}
	for _, tbl := range []string{"attention_asks", "plugin_reply_drafts"} {
		if _, err := pool.Exec(ctx, `SELECT origin_agent_id FROM `+tbl); err == nil {
			t.Fatalf("after the rollback %s.origin_agent_id still exists", tbl)
		}
	}
	if err := ask(nil, nil, nil); err == nil {
		t.Fatal("after the rollback an insert naming the origin columns still works")
	}
}
