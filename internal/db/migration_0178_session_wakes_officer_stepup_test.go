package db_test

import (
	"strconv"
	"strings"
	"testing"
)

// TestMigration0178RequestWakes: at 0177 session_wakes refuses the new kinds
// and officer_items has no requester (the controls). At 0178 an officer
// draft's decision and outcome and a step-up card's wake insert in their
// shapes, and each shape is held: an outcome or a card with text, a card
// without its tool, an officer wake without its item, a requester with half
// its pair or a malformed session — all refused. 0177's own rule that a count
// row carries no text still holds. A draft's decision wake is one per DECISION
// (an approval, then its withdrawal by a rejection: two rows, refused at 0177),
// an outcome still one per draft. The body re-applies cleanly, re-applying
// 0177's body (what --rls-on does) narrows neither the new CHECKs nor the
// redefined index, and the
// header's ROLLBACK block runs as written and the file applies again after it.
func TestMigration0178RequestWakes(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0178_request_wakes", 177)
	exec := func(q string) error {
		_, err := pool.Exec(ctx, q)
		return err
	}
	const sid = "5e55a1d0-0000-4000-8000-00000000a11c"
	officerDecided := `INSERT INTO session_wakes (owner_id, kind, draft_id, officer_item_id, decision, operator_text, mirror_host, mirror_session)
		VALUES (1, 'officer_draft_decided', 501, 223, 'rejected', 'not this week', 'tower', '` + sid + `')`
	stepUp := `INSERT INTO session_wakes (owner_id, kind, ask_id, decision, component, mirror_host, mirror_session)
		VALUES (1, 'stepup_decided', 701, 'confirmed', 'plugin_draft_decide', 'tower', '` + sid + `')`
	if err := exec(`INSERT INTO session_wakes (owner_id, kind, ask_id, decision, mirror_host, mirror_session)
		VALUES (1, 'stepup_decided', 700, 'confirmed', 'tower', '` + sid + `')`); err == nil {
		t.Fatal("control: 0177 accepted a stepup_decided wake")
	}
	if err := exec(`UPDATE officer_items SET requested_host = 'tower' WHERE false`); err == nil {
		t.Fatal("control: officer_items has a requester column before 0178")
	}
	// A reply draft's approval, then its withdrawal by a rejection: at 0177 the
	// second decision is a duplicate of the first (the control).
	replyDecided := func(draft int, decision string) string {
		return `INSERT INTO session_wakes (owner_id, kind, draft_id, decision, mirror_host, mirror_session)
			VALUES (1, 'draft_decided', ` + strconv.Itoa(draft) + `, '` + decision + `', 'tower', '` + sid + `')`
	}
	if err := exec(replyDecided(601, "approved")); err != nil {
		t.Fatalf("control: a reply draft's approval wake did not insert at 0177: %v", err)
	}
	if err := exec(replyDecided(601, "rejected")); err == nil {
		t.Fatal("control: 0177 accepted a second decision wake for one draft")
	}
	// 0177's own shapes, which 0178 keeps.
	if err := exec(`INSERT INTO session_wakes (owner_id, kind, ask_id, operator_text, mirror_host, mirror_session)
		VALUES (1, 'ask_answered', 699, 'go', 'tower', '` + sid + `')`); err != nil {
		t.Fatalf("control: an answered ask's wake did not insert at 0177: %v", err)
	}

	applyThroughFrom(ctx, t, pool, 178)

	for name, q := range map[string]string{"an officer decision": officerDecided, "a step-up card": stepUp,
		"an officer outcome": `INSERT INTO session_wakes (owner_id, kind, draft_id, officer_item_id, decision, response_count, mirror_host, mirror_session)
			VALUES (1, 'officer_draft_outcome', 502, 223, 'failed', 2, 'tower', '` + sid + `')`} {
		if err := exec(q); err != nil {
			t.Fatalf("%s did not insert after 0178: %v", name, err)
		}
	}
	// One decision wake PER DECISION: a withdrawn approval's rejection is its
	// own row, for a reply draft and an officer's; the same decision twice, or
	// a second outcome, is still one row.
	for name, q := range map[string]string{"a reply draft's rejection after its approval": replyDecided(601, "rejected"),
		"an officer draft's approval before its rejection": strings.Replace(officerDecided, "'rejected', 'not this week'", "'approved', ''", 1)} {
		if err := exec(q); err != nil {
			t.Fatalf("%s did not insert after 0178: %v", name, err)
		}
	}
	for name, q := range map[string]string{"a second approval of one reply draft": replyDecided(601, "approved"),
		"a second rejection of one officer draft": officerDecided,
		"a second outcome of one officer draft": `INSERT INTO session_wakes (owner_id, kind, draft_id, officer_item_id, decision, response_count, mirror_host, mirror_session)
			VALUES (1, 'officer_draft_outcome', 502, 223, 'sent', 1, 'tower', '` + sid + `')`,
		"a second outcome of one reply draft": `INSERT INTO session_wakes (owner_id, kind, draft_id, decision, mirror_host, mirror_session)
			VALUES (1, 'draft_outcome', 601, 'sent', 'tower', '` + sid + `'), (1, 'draft_outcome', 601, 'failed', 'tower', '` + sid + `')`} {
		if err := exec(q); err == nil {
			t.Errorf("%s was accepted after 0178", name)
		}
	}
	refused := map[string]string{
		"an officer outcome with text": `INSERT INTO session_wakes (owner_id, kind, draft_id, officer_item_id, decision, response_count, operator_text, mirror_host, mirror_session)
			VALUES (1, 'officer_draft_outcome', 503, 223, 'sent', 1, 'words', 'tower', '` + sid + `')`,
		"an officer outcome with no attempt": `INSERT INTO session_wakes (owner_id, kind, draft_id, officer_item_id, decision, mirror_host, mirror_session)
			VALUES (1, 'officer_draft_outcome', 504, 223, 'sent', 'tower', '` + sid + `')`,
		"an officer wake with no item": `INSERT INTO session_wakes (owner_id, kind, draft_id, decision, mirror_host, mirror_session)
			VALUES (1, 'officer_draft_decided', 505, 'approved', 'tower', '` + sid + `')`,
		"an officer decision word that is an outcome's": `INSERT INTO session_wakes (owner_id, kind, draft_id, officer_item_id, decision, mirror_host, mirror_session)
			VALUES (1, 'officer_draft_decided', 506, 223, 'sent', 'tower', '` + sid + `')`,
		"a step-up card with text": `INSERT INTO session_wakes (owner_id, kind, ask_id, decision, component, operator_text, mirror_host, mirror_session)
			VALUES (1, 'stepup_decided', 702, 'refused', 'attention_answer', 'words', 'tower', '` + sid + `')`,
		"a step-up card without its tool": `INSERT INTO session_wakes (owner_id, kind, ask_id, decision, mirror_host, mirror_session)
			VALUES (1, 'stepup_decided', 703, 'refused', 'tower', '` + sid + `')`,
		"a step-up card naming a draft": `INSERT INTO session_wakes (owner_id, kind, ask_id, draft_id, decision, component, mirror_host, mirror_session)
			VALUES (1, 'stepup_decided', 704, 9, 'refused', 'attention_answer', 'tower', '` + sid + `')`,
		"a step-up decision that is a draft's": `INSERT INTO session_wakes (owner_id, kind, ask_id, decision, component, mirror_host, mirror_session)
			VALUES (1, 'stepup_decided', 705, 'approved', 'attention_answer', 'tower', '` + sid + `')`,
		"a count with text (0177's)": `INSERT INTO session_wakes (owner_id, kind, page_id, response_count, operator_text, mirror_host, mirror_session)
			VALUES (1, 'page_responses', 9, 1, 'words', 'tower', '` + sid + `')`,
		"an ask wake naming an item": `INSERT INTO session_wakes (owner_id, kind, ask_id, officer_item_id, mirror_host, mirror_session)
			VALUES (1, 'ask_dismissed', 706, 223, 'tower', '` + sid + `')`,
	}
	for name, q := range refused {
		if err := exec(q); err == nil {
			t.Errorf("%s was accepted after 0178", name)
		}
	}
	mustExec(ctx, t, pool, `INSERT INTO officer_types (owner_id, key, charter) VALUES (1, 'm0178', 'c')`)
	mustExec(ctx, t, pool, `INSERT INTO officer_defs (owner_id, officer, type_key) VALUES (1, 'm0178:a', 'm0178')`)
	item := scanID(ctx, t, pool, `INSERT INTO officer_items (owner_id, officer, kind, subject_key) VALUES (1, 'm0178:a', 'owner.plan', 's') RETURNING id`)
	var h, s string
	if err := pool.QueryRow(ctx, `SELECT requested_host, requested_session FROM officer_items WHERE id = $1`, item).Scan(&h, &s); err != nil || h != "" || s != "" {
		t.Fatalf("a new item's requester: %q/%q %v; want empty", h, s, err)
	}
	for name, set := range map[string]string{"half a pair": `requested_host = 'tower'`, "a malformed session": `requested_host = 'tower', requested_session = 'not a session!'`,
		"a malformed host": `requested_host = 'Tower_1', requested_session = '` + sid + `'`} {
		if err := exec(`UPDATE officer_items SET ` + set + ` WHERE id = ` + strconv.FormatInt(item, 10)); err == nil {
			t.Errorf("a requester with %s was accepted", name)
		}
	}
	mustExec(ctx, t, pool, `UPDATE officer_items SET requested_host = 'tower', requested_session = '`+sid+`' WHERE id = `+strconv.FormatInt(item, 10))

	// Idempotent, and --rls-on's re-apply of 0177 does not narrow it.
	mustExec(ctx, t, pool, readMigration(t, "0178_session_wakes_officer_stepup.sql"))
	mustExec(ctx, t, pool, readMigration(t, "0177_session_wakes.sql"))
	if err := exec(strings.Replace(stepUp, "701", "707", 1)); err != nil {
		t.Fatalf("after 0177's body ran again a step-up card's wake was refused: %v", err)
	}
	if err := exec(replyDecided(602, "approved") + `, (1, 'draft_decided', 602, 'rejected', 'tower', '` + sid + `')`); err != nil {
		t.Fatalf("after 0177's body ran again a draft's two decisions were refused: %v", err)
	}

	// The header's ROLLBACK block, as written (0177's CHECK bodies restored).
	mustExec(ctx, t, pool, `DELETE FROM public.session_wakes WHERE kind IN ('officer_draft_decided', 'officer_draft_outcome', 'stepup_decided')`)
	mustExec(ctx, t, pool, `UPDATE public.send_queue SET state = 'cancelled', finished_at = now(),
		detail = 'cancelled: session wakes rolled back', held_until = NULL, hold_token = NULL
	  WHERE state = 'queued' AND session_wake_id IN (SELECT o.id FROM public.session_wakes o JOIN public.session_wakes n
		ON n.kind = 'draft_decided' AND n.draft_id = o.draft_id AND n.id > o.id WHERE o.kind = 'draft_decided')`)
	mustExec(ctx, t, pool, `DELETE FROM public.session_wakes o USING public.session_wakes n
	  WHERE o.kind = 'draft_decided' AND n.kind = 'draft_decided' AND n.draft_id = o.draft_id AND n.id > o.id`)
	mustExec(ctx, t, pool, `DROP INDEX IF EXISTS public.session_wakes_one_per_draft`)
	mustExec(ctx, t, pool, `CREATE UNIQUE INDEX session_wakes_one_per_draft ON public.session_wakes (draft_id, kind) WHERE draft_id IS NOT NULL`)
	var kept string
	if err := pool.QueryRow(ctx, `SELECT string_agg(decision, ',' ORDER BY id) FROM session_wakes WHERE kind = 'draft_decided' AND draft_id = 601`).Scan(&kept); err != nil || kept != "rejected" {
		t.Fatalf("the ROLLBACK block kept %q of draft 601's decisions (%v); want the later, rejected", kept, err)
	}
	mustExec(ctx, t, pool, `ALTER TABLE public.session_wakes DROP CONSTRAINT IF EXISTS session_wakes_subject`)
	mustExec(ctx, t, pool, `ALTER TABLE public.session_wakes ADD CONSTRAINT session_wakes_subject CHECK (
        (kind IN ('ask_answered', 'ask_dismissed') AND ask_id IS NOT NULL AND draft_id IS NULL AND page_id IS NULL AND response_id IS NULL)
        OR (kind IN ('draft_decided', 'draft_outcome') AND draft_id IS NOT NULL AND ask_id IS NULL AND page_id IS NULL AND response_id IS NULL
            AND decision <> '')
        OR (kind = 'page_own_response' AND page_id IS NOT NULL AND response_id IS NOT NULL AND ask_id IS NULL AND draft_id IS NULL)
        OR (kind = 'page_responses' AND page_id IS NOT NULL AND response_id IS NULL AND ask_id IS NULL AND draft_id IS NULL
            AND response_count >= 1))`)
	mustExec(ctx, t, pool, `ALTER TABLE public.session_wakes DROP CONSTRAINT IF EXISTS session_wakes_kind_check`)
	mustExec(ctx, t, pool, `ALTER TABLE public.session_wakes ADD CONSTRAINT session_wakes_kind_check CHECK (kind IN ('ask_answered', 'ask_dismissed', 'draft_decided',
		'draft_outcome', 'page_own_response', 'page_responses'))`)
	mustExec(ctx, t, pool, `ALTER TABLE public.session_wakes DROP CONSTRAINT IF EXISTS session_wakes_decision_check`)
	mustExec(ctx, t, pool, `ALTER TABLE public.session_wakes ADD CONSTRAINT session_wakes_decision_check
		CHECK (decision IN ('', 'approved', 'rejected', 'sent', 'failed', 'unconfirmed'))`)
	mustExec(ctx, t, pool, `ALTER TABLE public.session_wakes DROP COLUMN IF EXISTS officer_item_id`)
	mustExec(ctx, t, pool, `ALTER TABLE public.officer_items DROP CONSTRAINT IF EXISTS officer_items_requested_session_shape`)
	mustExec(ctx, t, pool, `ALTER TABLE public.officer_items DROP COLUMN IF EXISTS requested_host`)
	mustExec(ctx, t, pool, `ALTER TABLE public.officer_items DROP COLUMN IF EXISTS requested_session`)
	mustExec(ctx, t, pool, `DELETE FROM schema_migrations WHERE version = 178`)
	if err := exec(strings.Replace(stepUp, "701", "708", 1)); err == nil {
		t.Fatal("the ROLLBACK block left a step-up card's wake insertable")
	}
	if err := exec(replyDecided(601, "approved")); err == nil {
		t.Fatal("the ROLLBACK block left a draft's second decision insertable")
	}
	applyThroughFrom(ctx, t, pool, 178)
	if err := exec(strings.Replace(stepUp, "701", "709", 1)); err != nil {
		t.Fatalf("0178 did not re-apply after its ROLLBACK block: %v", err)
	}
}
