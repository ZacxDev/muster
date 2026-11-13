package db_test

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/db"
)

// TestMigration0121OutboundDrafts: at 0121 a plugin can be bound to an
// officer and a draft can be OUTBOUND (no message, its plugin named). The
// shape checks refuse a half-outbound row, and 0121's triggers keep what the
// officer filed fixed, let uploaded media only grow while pending, and set
// the approval's hashes only with the approval. 🔴 THE GUARD THIS FILE EXISTS
// FOR: db.RLSOn re-applies 0016, whose replyable trigger refuses a draft with
// no message; it must re-apply 0121 after it, or every outbound draft is
// refused after an --rls-on.
func TestMigration0121OutboundDrafts(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0121_outbound_drafts", 120)
	if _, err := pool.Exec(ctx, `SELECT deliver_to FROM plugins`); err == nil {
		t.Fatal("control: before 0121 plugins.deliver_to exists")
	}
	applyThroughFrom(ctx, t, pool, 121)
	agent := scanID(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace) VALUES (1, 'outbound-agent', 'ns-outbound') RETURNING id`)
	plugin := scanID(ctx, t, pool, `INSERT INTO plugins (owner_id, name, label, token_hash, data_scope, capabilities, deliver_to)
		VALUES (1, 'example-relay', 'Relay', 'h-0121', 'example', '{messages,draft_checks}', 'officer:example:main') RETURNING id`)
	outbound := func() error {
		_, err := pool.Exec(ctx, `INSERT INTO plugin_reply_drafts (owner_id, message_id, agent_id, agent_name, body, expires_at,
			check_state, check_due_at, plugin_id, venue, target_kind, target_ref, media_refs, officer)
			VALUES (1, NULL, $1, 'outbound-agent', 'a post', now() + interval '7 days', 'awaiting', now() + interval '7 days',
			        $2, 'example.forum', 'post', NULL, '{a/b}', 'example:main')`, agent, plugin)
		return err
	}
	if err := outbound(); err != nil {
		t.Fatalf("an outbound draft at 0121: %v", err)
	}
	for _, bad := range []struct{ why, sql string }{
		{"a deliver_to that is not officer:<type>:<area>", `UPDATE plugins SET deliver_to = 'chief' WHERE id = ` + itoa(plugin)},
		{"a draft with neither a message nor a plugin", `INSERT INTO plugin_reply_drafts (owner_id, agent_id, agent_name, body, expires_at)
			VALUES (1, ` + itoa(agent) + `, 'x', 'b', now() + interval '1 day')`},
		{"an outbound reply with no ref", `INSERT INTO plugin_reply_drafts (owner_id, agent_id, agent_name, body, expires_at, plugin_id, venue, target_kind, officer)
			VALUES (1, ` + itoa(agent) + `, 'x', 'b', now() + interval '1 day', ` + itoa(plugin) + `, 'v', 'reply', 'example:main')`},
		{"five media refs", `INSERT INTO plugin_reply_drafts (owner_id, agent_id, agent_name, body, expires_at, plugin_id, venue, target_kind, officer, media_refs)
			VALUES (1, ` + itoa(agent) + `, 'x', 'b', now() + interval '1 day', ` + itoa(plugin) + `, 'v', 'post', 'example:main', '{a/1,a/2,a/3,a/4,a/5}')`},
		{"rewriting an outbound draft's target", `UPDATE plugin_reply_drafts SET venue = 'elsewhere' WHERE message_id IS NULL`},
		{"approval hashes set without the approval", `UPDATE plugin_reply_drafts SET approved_body_sha256 = 'x', approved_media_sha256 = '{}' WHERE message_id IS NULL`},
	} {
		if _, err := pool.Exec(ctx, bad.sql); err == nil {
			t.Errorf("accepted %s", bad.why)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE plugin_reply_drafts SET media = media || '[{"ref":"a/b","sha256":"s"}]' WHERE message_id IS NULL`); err != nil {
		t.Fatalf("control: adding a medium to a pending draft: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE plugin_reply_drafts SET media = '[]' WHERE message_id IS NULL`); err == nil {
		t.Error("accepted removing an uploaded medium")
	}

	// 🔴 RLSOff then RLSOn: 0016's replyable trigger is re-created, then 0121's.
	if err := db.RLSOff(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := db.RLSOn(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := outbound(); err != nil {
		t.Fatalf("an outbound draft after RLSOn: %v — 0121 must be re-applied after 0016", err)
	}
	var when string
	if err := pool.QueryRow(ctx, `SELECT pg_get_triggerdef(oid) FROM pg_trigger WHERE tgname = 'plugin_reply_drafts_message_replyable'`).Scan(&when); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(when, "WHEN ((new.message_id IS NOT NULL))") {
		t.Fatalf("the replyable trigger after RLSOn: %s; want it conditional on a message", when)
	}
}
