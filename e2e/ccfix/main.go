// Command ccfix is the chief-mobile browser spec's fixture
// (e2e/tests/chief-mobile.spec.ts): a running claude-code agent with an
// account failover on record, the two rows the e2e harness cannot make through
// a real route — a failover needs a gateway reporting a rate limit from a real
// runtime, and the noop driver runs none.
//
//		DATABASE_URL=…_e2e go run ./e2e/ccfix agent <owner-id> <name> <account> <token>   → prints the agent id
//		DATABASE_URL=…_e2e go run ./e2e/ccfix failover <owner-id> <name> <from> <reason> <resets-RFC3339>
//		DATABASE_URL=…_e2e go run ./e2e/ccfix ask <owner-id> <name> <question> <option>…   → prints the ask id
//		DATABASE_URL=…_e2e go run ./e2e/ccfix rmagent <owner-id> <name>
//
//	  - agent: a RUNNING claude-code agent on <account>, as a dispatch leaves it.
//	  - failover: one cc_account_failovers row (internal/ccfailover PGStore.Record
//	    writes the same columns) from <from> to the agent's own account.
//	  - ask: an OPEN attention_ask from the agent WITH its options — the row its
//	    /mcp attention_ask call stores (pluginfix ask stores none).
//	  - rmagent: deletes the agent's asks, then the agent (its failovers cascade).
//
// 🔴 IT REFUSES ANY DATABASE WHOSE NAME DOES NOT END IN "_e2e" (e2edb.Check):
// it writes as the login role with row-level security's scope set to all, which
// nothing outside a fixture may do. Every value a spec sends is invented.
package main

import (
	"context"
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
		fmt.Fprintln(os.Stderr, "ccfix:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	dsn := os.Getenv("DATABASE_URL")
	if err := e2edb.Check(dsn); err != nil {
		return err
	}
	if len(args) < 3 {
		return fmt.Errorf("usage: ccfix agent|failover|ask|rmagent <owner-id> <name> …")
	}
	owner, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return fmt.Errorf("owner id: %w", err)
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
	switch args[0] {
	case "agent":
		if len(args) != 5 {
			return fmt.Errorf("usage: agent <owner-id> <name> <account> <token>")
		}
		var id int64
		if err := conn.QueryRow(ctx, `INSERT INTO agents (owner_id, name, namespace, display_name, status, hooks_token, kicked_off, kind, cc_account)
			VALUES ($1, $2, $3, $2, 'running', $4, true, 'claude-code', $5) RETURNING id`,
			owner, args[2], "devpod-"+args[2], args[4], args[3]).Scan(&id); err != nil {
			return err
		}
		fmt.Println(id)
	case "failover":
		if len(args) != 6 {
			return fmt.Errorf("usage: failover <owner-id> <name> <from> <reason> <resets-RFC3339>")
		}
		resets, err := time.Parse(time.RFC3339, args[5])
		if err != nil {
			return fmt.Errorf("resets: %w", err)
		}
		tag, err := conn.Exec(ctx, `INSERT INTO cc_account_failovers (owner_id, agent_id, agent_name, from_account, to_account, reason, resets_at)
			SELECT owner_id, id, name, $3, cc_account, $4, $5 FROM agents WHERE owner_id = $1 AND name = $2`,
			owner, args[2], args[3], args[4], resets)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("no agent %q of owner %d", args[2], owner)
		}
	case "ask":
		if len(args) < 5 {
			return fmt.Errorf("usage: ask <owner-id> <name> <question> <option>…")
		}
		opts, err := json.Marshal(args[4:])
		if err != nil {
			return err
		}
		var id int64
		if err := conn.QueryRow(ctx, `INSERT INTO attention_asks (owner_id, agent_id, asker_label, question, options, expires_at)
			SELECT $1, id, name, $3, $4::jsonb, now() + interval '1 hour' FROM agents WHERE owner_id = $1 AND name = $2 RETURNING id`,
			owner, args[2], args[3], string(opts)).Scan(&id); err != nil {
			return fmt.Errorf("ask from %s: %w", args[2], err)
		}
		fmt.Println(id)
	case "rmagent":
		// Its asks first, by name: the queue is shared by the whole suite.
		if _, err := conn.Exec(ctx, `DELETE FROM attention_asks WHERE owner_id = $1
			AND agent_id IN (SELECT id FROM agents WHERE owner_id = $1 AND name = $2)`, owner, args[2]); err != nil {
			return err
		}
		if _, err := conn.Exec(ctx, `DELETE FROM agents WHERE owner_id = $1 AND name = $2`, owner, args[2]); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
	return nil
}
