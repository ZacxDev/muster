package db_test

import (
	"strings"
	"testing"
)

// TestMigration0183PluginRefusedWake: at 0182 session_wakes refuses an
// officer_draft_decided wake whose decision is plugin_refused (the control).
// At 0183 it inserts, with the plugin's reason in operator_text, and 0178's
// shapes still hold: plugin_refused on any other kind is refused, an officer
// wake still needs its item, and the owner's approve and reject still insert.
// Re-applying 0177's body (what --rls-on does) narrows nothing, and the
// header's ROLLBACK block runs as written and the file applies again after it.
func TestMigration0183PluginRefusedWake(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0183_plugin_refused", 182)
	exec := func(q string) error {
		_, err := pool.Exec(ctx, q)
		return err
	}
	const sid = "5e55a1d0-0000-4000-8000-00000000a11c"
	refused := func(draft string) string {
		return `INSERT INTO session_wakes (owner_id, kind, draft_id, officer_item_id, decision, operator_text, mirror_host, mirror_session)
			VALUES (1, 'officer_draft_decided', ` + draft + `, 223, 'plugin_refused', 'posts_per_day: 3 of 3', 'tower', '` + sid + `')`
	}
	if err := exec(refused("501")); err == nil {
		t.Fatal("control: 0182 accepted a plugin_refused officer wake")
	}
	if err := exec(`UPDATE session_wakes SET retry_at = NULL WHERE false`); err == nil {
		t.Fatal("control: session_wakes has retry_at before 0183")
	}

	applyThroughFrom(ctx, t, pool, 183)

	if err := exec(refused("501")); err != nil {
		t.Fatalf("0183 refused a plugin_refused officer wake: %v", err)
	}
	if err := exec(`INSERT INTO session_wakes (owner_id, kind, draft_id, officer_item_id, decision, operator_text, retry_at, mirror_host, mirror_session)
		VALUES (1, 'officer_draft_decided', 510, 223, 'plugin_refused', 'cadence', '2000-01-03T00:00:00Z', 'tower', '` + sid + `')`); err != nil {
		t.Fatalf("0183 refused a plugin_refused wake with its retry_at: %v", err)
	}
	if err := exec(`INSERT INTO session_wakes (owner_id, kind, draft_id, officer_item_id, decision, retry_at, mirror_host, mirror_session)
		VALUES (1, 'officer_draft_decided', 511, 223, 'approved', '2000-01-03T00:00:00Z', 'tower', '` + sid + `')`); err == nil {
		t.Error("0183 accepted a retry_at on an approval's wake")
	}
	for name, q := range map[string]string{
		"the owner's approval": `INSERT INTO session_wakes (owner_id, kind, draft_id, officer_item_id, decision, mirror_host, mirror_session)
			VALUES (1, 'officer_draft_decided', 502, 223, 'approved', 'tower', '` + sid + `')`,
		"the owner's rejection": `INSERT INTO session_wakes (owner_id, kind, draft_id, officer_item_id, decision, operator_text, mirror_host, mirror_session)
			VALUES (1, 'officer_draft_decided', 502, 223, 'rejected', 'not now', 'tower', '` + sid + `')`,
	} {
		if err := exec(q); err != nil {
			t.Fatalf("control: %s no longer inserts at 0183: %v", name, err)
		}
	}
	for name, q := range map[string]string{
		"plugin_refused on an outcome": `INSERT INTO session_wakes (owner_id, kind, draft_id, officer_item_id, decision, response_count, mirror_host, mirror_session)
			VALUES (1, 'officer_draft_outcome', 503, 223, 'plugin_refused', 1, 'tower', '` + sid + `')`,
		"plugin_refused on a reply draft's decision": `INSERT INTO session_wakes (owner_id, kind, draft_id, decision, mirror_host, mirror_session)
			VALUES (1, 'draft_decided', 504, 'plugin_refused', 'tower', '` + sid + `')`,
		"plugin_refused without its item": strings.Replace(strings.Replace(refused("505"), ", 223,", ", NULL,", 1), "501", "505", 1),
		"plugin_refused with a component": `INSERT INTO session_wakes (owner_id, kind, draft_id, officer_item_id, decision, component, mirror_host, mirror_session)
			VALUES (1, 'officer_draft_decided', 506, 223, 'plugin_refused', 'x', 'tower', '` + sid + `')`,
	} {
		if err := exec(q); err == nil {
			t.Errorf("0183 accepted %s", name)
		}
	}

	applyThroughFrom(ctx, t, pool, 183)
	mustExec(ctx, t, pool, readMigration(t, "0183_session_wakes_plugin_refused.sql"))
	mustExec(ctx, t, pool, readMigration(t, "0177_session_wakes.sql"))
	if err := exec(refused("507")); err != nil {
		t.Fatalf("after 0177's body and 0183's ran again a plugin_refused wake was refused: %v", err)
	}

	// The header's ROLLBACK block, as written.
	mustExec(ctx, t, pool, `UPDATE public.send_queue SET state = 'cancelled', finished_at = now(),
		detail = 'cancelled: session wakes rolled back', held_until = NULL, hold_token = NULL
	  WHERE state = 'queued' AND session_wake_id IN (SELECT id FROM public.session_wakes WHERE decision = 'plugin_refused')`)
	mustExec(ctx, t, pool, `DELETE FROM public.session_wakes WHERE decision = 'plugin_refused'`)
	m178 := readMigration(t, "0178_session_wakes_officer_stepup.sql")
	for _, c := range []string{"session_wakes_decision_check", "session_wakes_subject"} {
		drop := "ALTER TABLE public.session_wakes DROP CONSTRAINT IF EXISTS " + c + ";"
		at := strings.LastIndex(m178, drop)
		if at < 0 {
			t.Fatalf("0178 has no %s statements to restore", c)
		}
		end := strings.Index(m178[at+len(drop):], ";")
		if end < 0 {
			t.Fatalf("0178's %s statement does not end", c)
		}
		mustExec(ctx, t, pool, m178[at:at+len(drop)+end+1])
	}
	mustExec(ctx, t, pool, `ALTER TABLE public.session_wakes DROP CONSTRAINT IF EXISTS session_wakes_retry_at_plugin_refused`)
	mustExec(ctx, t, pool, `ALTER TABLE public.session_wakes DROP COLUMN IF EXISTS retry_at`)
	mustExec(ctx, t, pool, `DELETE FROM schema_migrations WHERE version = 183`)
	if err := exec(refused("508")); err == nil {
		t.Fatal("the ROLLBACK block left a plugin_refused wake insertable")
	}
	applyThroughFrom(ctx, t, pool, 183)
	if err := exec(refused("509")); err != nil {
		t.Fatalf("0183 did not re-apply after its ROLLBACK block: %v", err)
	}
}
