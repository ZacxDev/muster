// Command asksfeedfix is the Asks card feed's browser fixture
// (e2e/tests/asks-feed.spec.ts): one owner's queue holding one of every kind
// the Open, Answered, Drafts and Nudges tabs draw, written through the real
// attention store where there is one (internal/attention: its Clean, its
// constraints, its owner default) and as plain rows where the e2e harness has
// no door (a task event, a reply draft, a nudge decision).
//
//	DATABASE_URL=…_e2e go run ./e2e/asksfeedfix seed <owner> <agent-name>   prints JSON ids
//	DATABASE_URL=…_e2e go run ./e2e/asksfeedfix clear <owner> <agent-name>
//
// The rows (all synthetic):
//
//   - a running agent <agent-name> (no credentials: nothing here calls /mcp);
//   - its question WITH options ("Deploy the importer to staging now?"), linked
//     to a task, with two suggested replies; four lines, more than a card's
//     three-line clamp, so expanding its card shows more (its chevron drawn);
//   - its FREE-TEXT question ("Which branch should the release be cut from?");
//   - a second question with options ("Rotate the staging key?"), so answering
//     one card leaves another to move focus to;
//   - muster's NOTICE ("This session may be waiting on you");
//   - an ANSWERED question (answered "yes"), for the Answered tab;
//   - a TASK TO ROUTE: a task and its unrouted task event (needs
//     MUSTER_PLUGINS=1 and MUSTER_TASK_ROUTING=1 on the server);
//   - a pending REPLY DRAFT by the agent to a delivered plugin message;
//   - a chief NUDGE decision (shadow, "leave").
//
// 🔴 IT REFUSES ANY DATABASE WHOSE NAME DOES NOT END IN "_e2e" (e2edb.Check).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/e2e/internal/e2edb"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/db"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "asksfeedfix:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	dsn := os.Getenv("DATABASE_URL")
	if err := e2edb.Check(dsn); err != nil {
		return err
	}
	if len(args) != 3 {
		return fmt.Errorf("usage: asksfeedfix seed|clear <owner> <agent-name>")
	}
	owner, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil || owner <= 0 {
		return fmt.Errorf("owner %q is not a user id", args[1])
	}
	name := args[2]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	role := authz.RoleMember
	if owner == authz.OperatorUserID {
		role = authz.RoleAdmin
	}
	// AS the owner: row-level security confines every write to their rows.
	as := authz.WithPrincipal(ctx, authz.Principal{UserID: owner, Role: role, Via: authz.ViaSession})
	switch args[0] {
	case "clear":
		return clear(as, pool, owner, name)
	case "seed":
		if err := clear(as, pool, owner, name); err != nil {
			return err
		}
		return seed(as, pool, owner, name)
	}
	return fmt.Errorf("unknown command %q", args[0])
}

func clear(ctx context.Context, pool *pgxpool.Pool, owner int64, name string) error {
	for _, q := range []string{
		`DELETE FROM attention_asks WHERE owner_id = $1 AND (agent_id IN (SELECT id FROM agents WHERE owner_id = $1 AND name = $2) OR (system_asker AND question LIKE 'feed: %'))`,
		`DELETE FROM plugins WHERE owner_id = $1 AND name = $2 || '-inbox'`,
		`DELETE FROM chief_nudges WHERE owner_id = $1 AND session_key = $2`,
		`DELETE FROM notes WHERE owner_id = $1 AND title LIKE 'feed: %' AND $2 <> ''`,
		`DELETE FROM agents WHERE owner_id = $1 AND name = $2`,
	} {
		if _, err := pool.Exec(ctx, q, owner, name); err != nil {
			return fmt.Errorf("%s: %w", q, err)
		}
	}
	return nil
}

func seed(ctx context.Context, pool *pgxpool.Pool, owner int64, name string) error {
	var agentID int64
	if err := pool.QueryRow(ctx, `INSERT INTO agents (owner_id, name, namespace, display_name, status, kicked_off)
		VALUES ($1, $2, $3, $2, 'running', true) RETURNING id`, owner, name, "devpod-"+name).Scan(&agentID); err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	var taskID int64
	if err := pool.QueryRow(ctx, `INSERT INTO notes (directory, title, body, status, owner_id) VALUES ('', 'feed: ship the importer', 'feed: ship the importer', 'open', $1) RETURNING id`,
		owner).Scan(&taskID); err != nil {
		return fmt.Errorf("task: %w", err)
	}
	st := attention.NewPG(pool)
	agent := attention.Asker{AgentID: agentID, Label: name}
	out := map[string]int64{"agent": agentID, "task": taskID}
	mk := func(key string, n attention.New) (attention.Ask, error) {
		a, err := st.Create(ctx, n)
		if err != nil {
			return a, fmt.Errorf("%s: %w", key, err)
		}
		out[key] = a.ID
		return a, nil
	}
	// Oldest first: the queue is newest first within a kind.
	answered, err := mk("answered", attention.New{Asker: agent, Question: "Shall I run the migrations first?", Options: []string{"yes", "no"}})
	if err != nil {
		return err
	}
	if _, err := st.Answer(ctx, answered.ID, "yes"); err != nil {
		return fmt.Errorf("answer: %w", err)
	}
	if _, err := mk("notice", attention.New{Asker: attention.Asker{System: true, Label: attention.SystemAskerLabel}, Notice: true,
		Question: "feed: This session may be waiting on you: it stopped after asking about the schema."}); err != nil {
		return err
	}
	if _, err := mk("rotate", attention.New{Asker: agent, Question: "Rotate the staging key?", Options: []string{"Rotate it", "Leave it"}}); err != nil {
		return err
	}
	if _, err := mk("freetext", attention.New{Asker: agent, Question: "Which branch should the release be cut from?"}); err != nil {
		return err
	}
	deploy, err := mk("deploy", attention.New{Asker: agent, Question: "Deploy the importer to staging now?\n\nThe tests pass; the last deploy was two days ago.\nA rollback is one command.",
		Options: []string{"Deploy now", "Wait until tomorrow"}})
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, `UPDATE attention_asks SET task_id = $2 WHERE id = $1`, deploy.ID, taskID); err != nil {
		return fmt.Errorf("deploy task link: %w", err)
	}
	if _, _, err := st.SetSuggestions(ctx, deploy.ID, []string{"Deploy, then post the link", "Hold until the review lands"}); err != nil {
		return fmt.Errorf("suggestions: %w", err)
	}
	// The plugin a draft and a task event arrive through.
	var pluginID, msgID, draftID, eventID int64
	if err := pool.QueryRow(ctx, `INSERT INTO plugins (owner_id, name, label, token_hash, data_scope)
		VALUES ($1, $2 || '-inbox', 'Example inbox', 'e2e-fixture-' || $2 || '-' || gen_random_uuid()::text, $2 || '-inbox') RETURNING id`, owner, name).Scan(&pluginID); err != nil {
		return fmt.Errorf("plugin: %w", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO plugin_messages (owner_id, plugin_id, external_id, sender, body, status, delivered_at, agent_id)
		VALUES ($1, $2, 'feed-1', 'someone', 'Are you free at noon?', 'delivered', now(), $3) RETURNING id`, owner, pluginID, agentID).Scan(&msgID); err != nil {
		return fmt.Errorf("message: %w", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO plugin_reply_drafts (owner_id, message_id, agent_id, agent_name, body, expires_at)
		VALUES ($1, $2, $3, $4, 'Yes, noon works.', now() + interval '1 hour') RETURNING id`, owner, msgID, agentID, name).Scan(&draftID); err != nil {
		return fmt.Errorf("draft: %w", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO plugin_messages (owner_id, plugin_id, external_id, sender, body, status, task_id)
		VALUES ($1, $2, 'feed-task-1', 'tracker', 'assigned to you', 'queued', $3) RETURNING id`, owner, pluginID, taskID).Scan(&eventID); err != nil {
		return fmt.Errorf("task event: %w", err)
	}
	out["draft"], out["event"] = draftID, eventID
	var nudgeID int64
	if err := pool.QueryRow(ctx, `INSERT INTO chief_nudges (owner_id, session_kind, session_key, agent_id, title, streak_key, decision, reason, outcome, mode)
		VALUES ($1, 'agent', $2, $3, 'Importer deploy', 'feed-streak', 'leave', 'It is waiting on a review, not on you.', 'left', 'shadow') RETURNING id`,
		owner, name, agentID).Scan(&nudgeID); err != nil {
		return fmt.Errorf("nudge: %w", err)
	}
	out["nudge"] = nudgeID
	j, _ := json.Marshal(out)
	fmt.Println(string(j))
	return nil
}
