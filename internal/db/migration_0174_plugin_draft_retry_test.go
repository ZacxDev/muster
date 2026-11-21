package db_test

import (
	"testing"

	"github.com/ZacxDev/muster/internal/db"
)

// TestMigration0174DraftRetry: at 0173 plugin_reply_drafts has no retry_of
// (the control). At 0174 a successor (retry_of set) is accepted ONLY as its
// root's approved bytes, born approved and unclaimed, after an attempt that
// FAILED with nothing_sent — so no row can re-send a post that may have gone
// out — and it carries the ROOT's decided_by_user and requested_by_user
// whatever the insert wrote. One successor per attempt, one live per chain.
// nothing_sent is a failed row's only; the result's transient is not stored.
// The trigger survives an RLS cycle.
func TestMigration0174DraftRetry(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0174_draft_retry", 173)
	if _, err := pool.Exec(ctx, `SELECT retry_of FROM plugin_reply_drafts`); err == nil {
		t.Fatal("control: before 0174 plugin_reply_drafts.retry_of exists")
	}
	applyThroughFrom(ctx, t, pool, 174)
	if _, err := pool.Exec(ctx, `SELECT transient FROM plugin_reply_drafts`); err == nil {
		t.Error("0174 stores a transient column: the result's flag decides the retry and is audited, never stored")
	}
	if _, err := pool.Exec(ctx, `SELECT nothing_sent, retry_of, attempt, not_before, retry_last_detail FROM plugin_reply_drafts`); err != nil {
		t.Fatalf("control: 0174's columns: %v", err)
	}
	approver := scanID(ctx, t, pool, `INSERT INTO users (role) VALUES ('member') RETURNING id`)
	agent := scanID(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace) VALUES (1, 'retry-agent', 'ns-retry') RETURNING id`)
	plugin := scanID(ctx, t, pool, `INSERT INTO plugins (owner_id, name, label, token_hash, data_scope, capabilities, deliver_to)
		VALUES (1, 'example-relay', 'Relay', 'h-0174', 'example', '{messages,draft_checks}', 'officer:example:main') RETURNING id`)
	const sum = `'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'`
	// The root: approved by `approver`, claimed, failed — nothing_sent off.
	root := scanID(ctx, t, pool, `INSERT INTO plugin_reply_drafts (owner_id, message_id, agent_id, agent_name, body, status, expires_at,
		decided_at, approved_at, claimed_at, lease_until, finished_at, check_state, checked_at, plugin_id, venue, target_kind, officer,
		approved_body_sha256, approved_media_sha256, decided_by_user, requested_by_user)
		VALUES (1, NULL, $1, 'retry-agent', 'a post', 'failed', now() + interval '7 days', now(), now(), now(), now() + interval '2 minutes', now(),
		        'sendable', now(), $2, 'example.forum', 'post', 'example:main', `+sum+`, '{}', $3, 1) RETURNING id`, agent, plugin, approver)
	successor := func(attempt int, set string) error {
		_, err := pool.Exec(ctx, `INSERT INTO plugin_reply_drafts (owner_id, message_id, agent_id, agent_name, body, status, expires_at,
			decided_at, approved_at, check_state, checked_at, plugin_id, venue, target_kind, officer, approved_body_sha256, approved_media_sha256,
			retry_of, attempt, not_before, decided_by_user, requested_by_user)
			SELECT owner_id, NULL, agent_id, agent_name, body, 'approved', expires_at, decided_at, approved_at, check_state, checked_at,
			       plugin_id, venue, target_kind, officer, approved_body_sha256, approved_media_sha256, id, $2, now() + interval '1 minute', 1, NULL
			  FROM plugin_reply_drafts WHERE id = $1`, root, attempt)
		if err != nil || set == "" {
			return err
		}
		_, err = pool.Exec(ctx, `UPDATE plugin_reply_drafts SET `+set+` WHERE retry_of = $1 AND attempt = $2`, root, attempt)
		return err
	}

	if err := successor(2, ""); err == nil {
		t.Fatal("accepted a successor of a failure that did not say nothing was sent (an unknown outcome)")
	}
	mustExec(ctx, t, pool, `UPDATE plugin_reply_drafts SET nothing_sent = true WHERE id = $1`, root)
	if err := successor(2, ""); err != nil {
		t.Fatalf("control: a successor of a nothing-sent failure: %v", err)
	}
	var decided, requested *int64
	var attempt int
	if err := pool.QueryRow(ctx, `SELECT decided_by_user, requested_by_user, attempt FROM plugin_reply_drafts WHERE retry_of = $1`, root).
		Scan(&decided, &requested, &attempt); err != nil {
		t.Fatal(err)
	}
	if decided == nil || *decided != approver || requested == nil || *requested != 1 || attempt != 2 {
		t.Fatalf("the successor's decided_by_user %v, requested_by_user %v, attempt %d; want the root's %d and 1, attempt 2", decided, requested, attempt, approver)
	}
	if err := successor(2, ""); err == nil {
		t.Error("accepted a second attempt 2 (one successor per attempt, one live per chain)")
	}
	if err := successor(3, ""); err == nil {
		t.Error("accepted attempt 3 while attempt 2 is approved and not failed")
	}

	for _, bad := range []struct{ why, sql string }{
		{"a not_before on a root", `UPDATE plugin_reply_drafts SET not_before = now() WHERE id = ` + itoa(root)},
		{"nothing_sent on a draft that is not failed", `UPDATE plugin_reply_drafts SET nothing_sent = true WHERE retry_of = ` + itoa(root)},
		{"attempt 2 with no retry_of", `UPDATE plugin_reply_drafts SET attempt = 2 WHERE id = ` + itoa(root)},
	} {
		if _, err := pool.Exec(ctx, bad.sql); err == nil {
			t.Errorf("accepted %s", bad.why)
		}
	}

	// Attempt 2 fails with nothing sent: attempt 3 may follow it, after the
	// RLS cycle (db.RLSOn re-applies 0016, 0127 and 0130 — never this file).
	mustExec(ctx, t, pool, `UPDATE plugin_reply_drafts SET claimed_at = now(), lease_until = now() + interval '2 minutes', status = 'failed',
		finished_at = now(), nothing_sent = true WHERE retry_of = $1 AND attempt = 2`, root)
	// Each shape refusal is reached with a valid predecessor in place (attempt
	// 2 failed, nothing sent), so it is refused for its own shape, not for
	// following nothing.
	for _, bad := range []struct{ why, sql string }{
		{"a successor with another body", `INSERT INTO plugin_reply_drafts (owner_id, agent_id, agent_name, body, status, expires_at, approved_at,
			check_state, checked_at, plugin_id, venue, target_kind, officer, approved_body_sha256, approved_media_sha256, retry_of, attempt)
			SELECT owner_id, agent_id, agent_name, 'another post', 'approved', expires_at, approved_at, check_state, checked_at, plugin_id, venue,
			       target_kind, officer, approved_body_sha256, approved_media_sha256, id, 3 FROM plugin_reply_drafts WHERE id = ` + itoa(root)},
		// Born settled with the approval's bytes, so only the status check
		// refuses it (a pending one could not carry the hashes at all).
		{"a successor born expired", `INSERT INTO plugin_reply_drafts (owner_id, agent_id, agent_name, body, status, expires_at, approved_at,
			check_state, checked_at, plugin_id, venue, target_kind, officer, approved_body_sha256, approved_media_sha256, retry_of, attempt)
			SELECT owner_id, agent_id, agent_name, body, 'expired', expires_at, approved_at, check_state, checked_at, plugin_id, venue,
			       target_kind, officer, approved_body_sha256, approved_media_sha256, id, 3 FROM plugin_reply_drafts WHERE id = ` + itoa(root)},
		{"a successor outliving its root", `INSERT INTO plugin_reply_drafts (owner_id, agent_id, agent_name, body, status, expires_at, approved_at,
			check_state, checked_at, plugin_id, venue, target_kind, officer, approved_body_sha256, approved_media_sha256, retry_of, attempt)
			SELECT owner_id, agent_id, agent_name, body, 'approved', expires_at + interval '1 day', approved_at, check_state, checked_at, plugin_id, venue,
			       target_kind, officer, approved_body_sha256, approved_media_sha256, id, 3 FROM plugin_reply_drafts WHERE id = ` + itoa(root)},
	} {
		if _, err := pool.Exec(ctx, bad.sql); err == nil {
			t.Errorf("accepted %s", bad.why)
		}
	}
	if err := db.RLSOff(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := db.RLSOn(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := successor(3, ""); err != nil {
		t.Fatalf("control: attempt 3 after a nothing-sent attempt 2, after an RLS cycle: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT decided_by_user FROM plugin_reply_drafts WHERE retry_of = $1 AND attempt = 3`, root).Scan(&decided); err != nil ||
		decided == nil || *decided != approver {
		t.Fatalf("attempt 3's decided_by_user after the RLS cycle: %v (%v); want the root's %d", decided, err, approver)
	}
}
