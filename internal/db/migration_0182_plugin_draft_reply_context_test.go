package db_test

import (
	"strings"
	"testing"
)

// TestMigration0182ReplyContext: at 0181 plugin_reply_drafts has no
// reply_context (the control). At 0182 an outbound REPLY may carry one — a JSON
// object with a string text — and nothing else may: not a post, not an inbound
// reply draft, not an object without text, not an array, not an oversized one.
func TestMigration0182ReplyContext(t *testing.T) {
	ctx := ctx0006(t)
	pool := freshAt(ctx, t, "ms_0182_reply_context", 181)
	if _, err := pool.Exec(ctx, `SELECT reply_context FROM plugin_reply_drafts`); err == nil {
		t.Fatal("control: before 0182 plugin_reply_drafts.reply_context exists")
	}
	applyThroughFrom(ctx, t, pool, 179)

	agent := scanID(ctx, t, pool, `INSERT INTO agents (owner_id, name, namespace) VALUES (1, 'ctx-agent', 'ns-ctx') RETURNING id`)
	plugin := scanID(ctx, t, pool, `INSERT INTO plugins (owner_id, name, label, token_hash, data_scope, capabilities, deliver_to)
		VALUES (1, 'example-relay', 'Relay', 'h-0182', 'example', '{messages,draft_checks}', 'officer:example:main') RETURNING id`)
	message := scanID(ctx, t, pool, `INSERT INTO plugin_messages (owner_id, plugin_id, external_id, sender, body)
		VALUES (1, $1, 'ext-0182', 'ada', 'hello') RETURNING id`, plugin)
	outbound := func(kind, rc string) error {
		_, err := pool.Exec(ctx, `INSERT INTO plugin_reply_drafts (owner_id, message_id, agent_id, agent_name, body, expires_at, check_state,
			check_due_at, plugin_id, venue, target_kind, target_ref, officer, reply_context)
			VALUES (1, NULL, $1, 'ctx-agent', 'a reply', now() + interval '7 days', 'awaiting', now() + interval '7 days', $2, 'x', $3,
			        CASE WHEN $3 = 'post' THEN NULL ELSE '2122495152350453825' END, 'example:main', $4::jsonb)`, agent, plugin, kind, rc)
		return err
	}
	if err := outbound("reply", `{"author": "@ada", "text": "what is muster?", "written_at": "2026-11-22T10:00:00Z", "url": "https://x.com/ada/status/1"}`); err != nil {
		t.Fatalf("control: an outbound reply with its context: %v", err)
	}
	if err := outbound("reply", `{"text": "only the text"}`); err != nil {
		t.Fatalf("control: a context of text alone: %v", err)
	}
	var text string
	if err := pool.QueryRow(ctx, `SELECT reply_context->>'text' FROM plugin_reply_drafts WHERE reply_context->>'author' = '@ada'`).Scan(&text); err != nil || text != "what is muster?" {
		t.Fatalf("the stored context's text: %q (%v)", text, err)
	}
	for _, bad := range []struct{ why, kind, rc string }{
		{"context on a new post", "post", `{"text": "x"}`},
		{"context on a like", "like", `{"text": "x"}`},
		{"a context without text", "reply", `{"author": "@ada"}`},
		{"a context whose text is a number", "reply", `{"text": 7}`},
		{"a context that is an array", "reply", `["x"]`},
		{"an oversized context", "reply", `{"text": "` + strings.Repeat("a", 131072) + `"}`},
	} {
		if err := outbound(bad.kind, bad.rc); err == nil {
			t.Errorf("accepted %s", bad.why)
		}
	}
	// An inbound reply draft (it has its message) never carries one.
	if _, err := pool.Exec(ctx, `INSERT INTO plugin_reply_drafts (owner_id, message_id, agent_id, agent_name, body, expires_at, reply_context)
		VALUES (1, $1, $2, 'ctx-agent', 'hi', now() + interval '7 days', '{"text": "x"}')`, message, agent); err == nil {
		t.Error("accepted a context on an inbound reply draft")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO plugin_reply_drafts (owner_id, message_id, agent_id, agent_name, body, expires_at)
		VALUES (1, $1, $2, 'ctx-agent', 'hi', now() + interval '7 days')`, message, agent); err != nil {
		t.Fatalf("control: an inbound reply draft without one: %v", err)
	}
}
