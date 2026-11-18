// Command officerfix is the officer page spec's fixture
// (e2e/tests/officer-needs-you.spec.ts): the rows an officer's Needs-you and
// What-it-did sections draw, which the e2e harness cannot make through a real
// route because it runs no officer instance and no plugin.
//
//	DATABASE_URL=…_e2e go run ./e2e/officerfix seed <owner-id> <officer> <plugin-name>   → prints the ids as JSON
//	DATABASE_URL=…_e2e go run ./e2e/officerfix rmplugin <owner-id> <plugin-name>
//
//   - seed: for an officer the spec declared (through the admin API, so its
//     type and def are muster's own), a plugin of that name and, as an officer
//     instance and its plugin would leave them: one PENDING post on X the
//     plugin checked sendable (no media); two pending FOLLOWS of the same UTC
//     day (the day's outreach batch), each with a recorded person; one
//     draft.refused item its plugin refused on cadence, open; one finished
//     item with the officer's summary; and one post SENT on X for a finished
//     item, with its reference. The officer's items and people go with the
//     officer (its delete), the drafts with the plugin (rmplugin).
//   - rmplugin: removes one plugin of the owner's by name (its drafts
//     cascade) and the agent row seed made for it.
//
// 🔴 IT REFUSES ANY DATABASE WHOSE NAME DOES NOT END IN "_e2e" (e2edb.Check):
// it writes as the login role with row-level security's scope set to all,
// which nothing outside a fixture may do.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/e2e/internal/e2edb"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "officerfix:", err)
		os.Exit(1)
	}
}

// The texts the spec reads back (pinned there too).
const (
	PostBody      = "The officers page now shows what needs you first."
	SentBody      = "Shipped: every officer decision on one page."
	SentRef       = "2122780902921224250"
	RefusalDetail = "cadence: 1 days since the last post, next window tomorrow"
	DoneSummary   = "Read four replies; none needed an answer."
)

func run(args []string) error {
	dsn := os.Getenv("DATABASE_URL")
	if err := e2edb.Check(dsn); err != nil {
		return err
	}
	if len(args) == 0 {
		return fmt.Errorf("usage: officerfix seed|rmplugin …")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `SELECT set_config('muster.scope', 'all', false)`); err != nil {
		return err
	}
	owner, err := strconv.ParseInt(argAt(args, 1), 10, 64)
	if err != nil {
		return fmt.Errorf("%s: the owner id: %w", args[0], err)
	}
	switch args[0] {
	case "seed":
		if len(args) != 4 {
			return fmt.Errorf("usage: seed <owner-id> <officer> <plugin-name>")
		}
		out, err := seed(ctx, conn, owner, args[2], args[3])
		if err != nil {
			return err
		}
		b, _ := json.Marshal(out)
		fmt.Println(string(b))
	case "rmplugin":
		if len(args) != 3 {
			return fmt.Errorf("usage: rmplugin <owner-id> <plugin-name>")
		}
		if _, err := conn.Exec(ctx, `DELETE FROM plugins WHERE owner_id = $1 AND name = $2`, owner, args[2]); err != nil {
			return err
		}
		if _, err := conn.Exec(ctx, `DELETE FROM agents WHERE owner_id = $1 AND name = $2`, owner, "e2e-officer-"+args[2]); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
	return nil
}

func argAt(args []string, i int) string {
	if i < len(args) {
		return args[i]
	}
	return ""
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func seed(ctx context.Context, conn *pgx.Conn, owner int64, officer, plugin string) (map[string]any, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var pluginID int64
	if err := tx.QueryRow(ctx, `INSERT INTO plugins (owner_id, name, label, token_hash, data_scope, capabilities)
		VALUES ($1, $2, $2, 'e2e-fixture-' || $2 || '-' || gen_random_uuid()::text, $2, '{messages,draft_checks}') RETURNING id`, owner, plugin).Scan(&pluginID); err != nil {
		return nil, fmt.Errorf("plugin: %w", err)
	}
	item := func(kind, subject, status, outcome, summary string, payload any) (int64, error) {
		raw, _ := json.Marshal(payload)
		var id int64
		err := tx.QueryRow(ctx, `INSERT INTO officer_items (owner_id, officer, kind, subject_key, payload, status, outcome, summary)
			VALUES ($1, $2, $3, $4, $5, $6, nullif($7, ''), nullif($8, '')) RETURNING id`, owner, officer, kind, subject, raw, status, outcome, summary).Scan(&id)
		return id, err
	}
	out := map[string]any{}
	// The instance that filed them: an agent row of the owner's (a draft names
	// its agent, and the owner must own it). It holds no credential: the
	// fixture never names one (internal/agents secrets_ledger_test.go).
	agentName := "e2e-officer-" + plugin
	var agent int64
	if err := tx.QueryRow(ctx, `INSERT INTO agents (owner_id, name, namespace, display_name, status, kicked_off)
		VALUES ($1, $2, $3, $2, 'stopped', true) RETURNING id`, owner, agentName, "devpod-"+agentName).Scan(&agent); err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	// The pending post, checked sendable.
	var post int64
	if err := tx.QueryRow(ctx, `INSERT INTO plugin_reply_drafts (owner_id, agent_id, agent_name, body, expires_at, check_state, checked_at,
			plugin_id, venue, target_kind, officer)
		VALUES ($1, $6, $2, $3, now() + interval '6 days', 'sendable', now(), $4, 'x', 'post', $5) RETURNING id`,
		owner, agentName, PostBody, pluginID, officer, agent).Scan(&post); err != nil {
		return nil, fmt.Errorf("post: %w", err)
	}
	out["post"] = post
	// Two follows of today, each for a recorded person.
	var follows []int64
	for i, h := range []string{"ada-example", "bo-example"} {
		var contact int64
		if err := tx.QueryRow(ctx, `INSERT INTO officer_contacts (owner_id, officer, venue, handle, display, bio, profile_url, last_engaged)
			VALUES ($1, $2, 'x', $3, $4, $5, $6, now()) RETURNING id`, owner, officer, h, "Display "+h,
			"Builds small tools. <b>Not markup.</b>", "https://example.test/"+h).Scan(&contact); err != nil {
			return nil, fmt.Errorf("contact %s: %w", h, err)
		}
		var d int64
		if err := tx.QueryRow(ctx, `INSERT INTO plugin_reply_drafts (owner_id, agent_id, agent_name, body, expires_at, check_state, checked_at,
				plugin_id, venue, target_kind, target_ref, officer, because_ref, contact_id, outreach_reason)
			VALUES ($1, $9, $2, '', now() + interval '6 days', 'sendable', now(), $3, 'x', 'follow', $4, $5, $6, $7, $8) RETURNING id`,
			owner, agentName, pluginID, h, officer, "r-"+h, contact, "replied twice to our posts", agent).Scan(&d); err != nil {
			return nil, fmt.Errorf("follow %s: %w", h, err)
		}
		follows = append(follows, d)
		_ = i
	}
	out["follows"] = follows
	// The cadence refusal, open: the officer has not redrafted it.
	refusal, err := item("draft.refused", "draft:900001", "open", "", "", map[string]any{
		"draft_id": 900001, "venue": "x", "by": "plugin", "detail": RefusalDetail, "status": "rejected", "target": map[string]any{"kind": "post", "ref": ""}})
	if err != nil {
		return nil, fmt.Errorf("refusal: %w", err)
	}
	out["refusal"] = refusal
	// A finished item, then a sent post for another.
	done, err := item("plugin.reply", "reply-1", "done", "no_action", DoneSummary, map[string]any{})
	if err != nil {
		return nil, fmt.Errorf("done item: %w", err)
	}
	out["done"] = done
	sentItem, err := item("manual.run", "run-now-e2e", "done", "done", "posted "+SentRef, map[string]any{})
	if err != nil {
		return nil, fmt.Errorf("sent item: %w", err)
	}
	var sent int64
	if err := tx.QueryRow(ctx, `INSERT INTO plugin_reply_drafts (owner_id, agent_id, agent_name, body, expires_at, check_state, checked_at,
			plugin_id, venue, target_kind, officer, officer_item_id, status, decided_at, approved_at, approved_body_sha256, approved_media_sha256,
			claimed_at, lease_until, finished_at, external_ref)
		VALUES ($1, $9, $2, $3, now() + interval '6 days', 'sendable', now(), $4, 'x', 'post', $5, $6, 'sent', now(), now(), $7, '{}',
			now(), now() + interval '2 minutes', now(), $8) RETURNING id`,
		owner, agentName, SentBody, pluginID, officer, sentItem, sum(SentBody), SentRef, agent).Scan(&sent); err != nil {
		return nil, fmt.Errorf("sent post: %w", err)
	}
	out["sent"], out["sent_item"] = sent, sentItem
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}
