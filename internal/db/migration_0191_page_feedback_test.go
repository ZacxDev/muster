package db_test

import (
	"strconv"
	"strings"
	"testing"
)

// TestMigration0191PageFeedback: at 0190 page_responses refuses kind
// 'feedback' and session_wakes refuses kind 'page_feedback' and has no
// link_id (the controls). At 0191 a feedback response inserts
// (component '__feedback' only), and a page_feedback wake inserts — the
// owner's (no link) and a link viewer's (link_id) — in its one shape: words,
// a version, no decision or component. An unknown kind is still refused on
// both tables, link_id stays page_feedback's alone, and 0178's
// and 0183's shapes still insert. Re-applying 0116's and 0177's bodies (what
// --rls-on does) narrows nothing, and the header's ROLLBACK block runs as
// written and the file applies again after it.
func TestMigration0191PageFeedback(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0191_page_feedback", 190)
	exec := func(q string) error {
		_, err := pool.Exec(ctx, q)
		return err
	}
	const sid = "5e55a1d0-0000-4000-8000-00000000f33d"
	page := scanID(ctx, t, pool, `INSERT INTO pages (owner_id, title, mirror_host) VALUES (1, 'Pricing table', 'tower') RETURNING id`)
	pg := strconv.FormatInt(page, 10)
	link := scanID(ctx, t, pool, `INSERT INTO page_links (owner_id, page_id, token_sha256, state, decided_at)
		VALUES (1, `+pg+`, '`+strings.Repeat("ab", 32)+`', 'live', now()) RETURNING id`)
	lk := strconv.FormatInt(link, 10)
	feedback := func(component string) string {
		return `INSERT INTO page_responses (owner_id, page_id, link_id, by_owner, version, component, kind, data)
			VALUES (1, ` + pg + `, ` + lk + `, false, 1, '` + component + `', 'feedback', '{"text":"the second column is wrong"}') RETURNING id`
	}
	wake := func(resp, extra, vals string) string {
		return `INSERT INTO session_wakes (owner_id, kind, page_id, response_id, response_count, operator_text, mirror_host, mirror_session` + extra + `)
			VALUES (1, 'page_feedback', ` + pg + `, ` + resp + `, 3, 'make the header bold', 'tower', '` + sid + `'` + vals + `)`
	}
	if err := exec(feedback("__feedback")); err == nil {
		t.Fatal("control: 0190 accepted a feedback response")
	}
	if err := exec(wake("9001", "", "")); err == nil {
		t.Fatal("control: 0190 accepted a page_feedback wake")
	}
	if err := exec(`UPDATE session_wakes SET link_id = NULL WHERE false`); err == nil {
		t.Fatal("control: session_wakes has link_id before 0191")
	}

	applyThroughFrom(ctx, t, pool, 191)

	resp := scanID(ctx, t, pool, feedback("__feedback"))
	rs := strconv.FormatInt(resp, 10)
	if err := exec(feedback("notes")); err == nil {
		t.Error("0191 accepted a feedback response under a page's own component name")
	}
	if err := exec(wake(rs, ", link_id", ", "+lk)); err != nil {
		t.Fatalf("0191 refused a link viewer's page_feedback wake: %v", err)
	}
	if err := exec(wake("9002", "", "")); err != nil {
		t.Fatalf("0191 refused the owner's page_feedback wake (no link): %v", err)
	}
	for name, q := range map[string]string{
		"an unknown response kind": `INSERT INTO page_responses (owner_id, page_id, link_id, by_owner, version, component, kind, data)
			VALUES (1, ` + pg + `, ` + lk + `, false, 1, '__feedback', 'feedbacks', '{}')`,
		"an unknown wake kind":                  strings.Replace(wake("9003", "", ""), "'page_feedback'", "'page_feedbacks'", 1),
		"a page_feedback wake with no words":    strings.Replace(wake("9004", "", ""), "'make the header bold'", "''", 1),
		"a page_feedback wake with no version":  strings.Replace(wake("9005", "", ""), ", 3,", ", 0,", 1),
		"a page_feedback wake with a decision":  wake("9006", ", decision", ", 'approved'"),
		"a page_feedback wake with a component": wake("9007", ", component", ", 'notes'"),
		"a page_feedback wake with no response": strings.Replace(wake("9008", "", ""), ", 9008,", ", NULL,", 1),
		"a page_feedback wake with a link of 0": wake("9009", ", link_id", ", 0"),
		"a link on another kind": `INSERT INTO session_wakes (owner_id, kind, page_id, response_id, component, operator_text, link_id, mirror_host, mirror_session)
			VALUES (1, 'page_own_response', ` + pg + `, 9010, 'notes', 'yes', ` + lk + `, 'tower', '` + sid + `')`,
	} {
		if err := exec(q); err == nil {
			t.Errorf("0191 accepted %s", name)
		}
	}
	for name, q := range map[string]string{
		"a submit response": `INSERT INTO page_responses (owner_id, page_id, link_id, by_owner, version, component, kind, data)
			VALUES (1, ` + pg + `, ` + lk + `, false, 1, 'notes', 'submit', '{"a":1}')`,
		"the owner's page response wake": `INSERT INTO session_wakes (owner_id, kind, page_id, response_id, component, operator_text, mirror_host, mirror_session)
			VALUES (1, 'page_own_response', ` + pg + `, 9012, 'notes', 'yes', 'tower', '` + sid + `')`,
		"a plugin's refusal": `INSERT INTO session_wakes (owner_id, kind, draft_id, officer_item_id, decision, operator_text, mirror_host, mirror_session)
			VALUES (1, 'officer_draft_decided', 601, 223, 'plugin_refused', 'posts_per_day', 'tower', '` + sid + `')`,
		"a step-up card's": `INSERT INTO session_wakes (owner_id, kind, ask_id, decision, component, mirror_host, mirror_session)
			VALUES (1, 'stepup_decided', 77, 'confirmed', 'page_share', 'tower', '` + sid + `')`,
	} {
		if err := exec(q); err != nil {
			t.Errorf("control: %s no longer inserts at 0191: %v", name, err)
		}
	}

	mustExec(ctx, t, pool, readMigration(t, "0191_page_feedback.sql"))
	mustExec(ctx, t, pool, readMigration(t, "0116_pages.sql"))
	mustExec(ctx, t, pool, readMigration(t, "0177_session_wakes.sql"))
	if err := exec(feedback("__feedback")); err != nil {
		t.Fatalf("after 0116's and 0177's bodies ran again a feedback response was refused: %v", err)
	}
	if err := exec(wake("9013", ", link_id", ", "+lk)); err != nil {
		t.Fatalf("after 0177's body ran again a page_feedback wake was refused: %v", err)
	}

	// The header's ROLLBACK block, as written.
	mustExec(ctx, t, pool, `UPDATE public.send_queue SET state = 'cancelled', finished_at = now(),
		detail = 'cancelled: session wakes rolled back', held_until = NULL, hold_token = NULL
	  WHERE state = 'queued' AND session_wake_id IN (SELECT id FROM public.session_wakes WHERE kind = 'page_feedback')`)
	mustExec(ctx, t, pool, `DELETE FROM public.session_wakes WHERE kind = 'page_feedback'`)
	mustExec(ctx, t, pool, `DELETE FROM public.page_responses WHERE kind = 'feedback'`)
	restore := func(file, constraint string) {
		m := readMigration(t, file)
		drop := "ALTER TABLE public.session_wakes DROP CONSTRAINT IF EXISTS " + constraint + ";"
		at := strings.LastIndex(m, drop)
		if at < 0 {
			t.Fatalf("%s has no %s statements to restore", file, constraint)
		}
		end := strings.Index(m[at+len(drop):], ";")
		if end < 0 {
			t.Fatalf("%s's %s statement does not end", file, constraint)
		}
		mustExec(ctx, t, pool, m[at:at+len(drop)+end+1])
	}
	restore("0183_session_wakes_plugin_refused.sql", "session_wakes_subject")
	restore("0178_session_wakes_officer_stepup.sql", "session_wakes_kind_check")
	for _, q := range []string{
		`ALTER TABLE public.session_wakes DROP CONSTRAINT IF EXISTS session_wakes_link_feedback`,
		`ALTER TABLE public.session_wakes DROP COLUMN IF EXISTS link_id`,
		`ALTER TABLE public.page_responses DROP CONSTRAINT IF EXISTS page_responses_feedback_component`,
		`ALTER TABLE public.page_responses DROP CONSTRAINT IF EXISTS page_responses_kind_check`,
		`ALTER TABLE public.page_responses ADD CONSTRAINT page_responses_kind_check CHECK (kind IN ('submit', 'upload'))`,
		`DELETE FROM schema_migrations WHERE version = 191`,
	} {
		mustExec(ctx, t, pool, q)
	}
	if err := exec(feedback("__feedback")); err == nil {
		t.Fatal("the ROLLBACK block left a feedback response insertable")
	}
	if err := exec(wake("9014", "", "")); err == nil {
		t.Fatal("the ROLLBACK block left a page_feedback wake insertable")
	}
	applyThroughFrom(ctx, t, pool, 191)
	if err := exec(wake("9015", "", "")); err != nil {
		t.Fatalf("0191 did not re-apply after its ROLLBACK block: %v", err)
	}
}
