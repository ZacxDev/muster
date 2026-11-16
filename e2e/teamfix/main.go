// Command teamfix is the team-sharing browser spec's fixture
// (e2e/tests/team-ux.spec.ts): the one thing that spec needs and the e2e
// harness cannot make through a real route — a TEAM's end card. The real one
// is filed by the team's chief (end_session_propose) on an operator's fresh
// request, which needs an agent runtime the harness does not run; the card
// and its proposal are what that call stores (internal/api end_session.go,
// internal/plugins.CreateProposal), owned by the team.
//
//	DATABASE_URL=…_e2e go run ./e2e/teamfix endcard <team-id>   → prints the card's ask id
//	DATABASE_URL=…_e2e go run ./e2e/teamfix cleanup <team-id>
//
//   - endcard: an OPEN owner card of the team's ("End …?", options End it and
//     Not now) asked about a mirrored session, and the team's open end_session
//     proposal naming it.
//   - cleanup: removes every proposal and ask the team owns, so later specs on
//     the shared database see none.
//
// Every value is invented fixture data. 🔴 IT REFUSES ANY DATABASE WHOSE NAME
// DOES NOT END IN "_e2e" (e2edb.Check): it writes as the login role, under
// no row-level security, which nothing outside a fixture may do.
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/e2e/internal/e2edb"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "teamfix:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	dsn := os.Getenv("DATABASE_URL")
	if err := e2edb.Check(dsn); err != nil {
		return err
	}
	if len(args) != 2 {
		return fmt.Errorf("usage: teamfix endcard|cleanup <team-id>")
	}
	team, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil || team <= 0 {
		return fmt.Errorf("bad team id %q", args[1])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	var kind string
	if err := conn.QueryRow(ctx, `SELECT kind FROM users WHERE id = $1`, team).Scan(&kind); err != nil || kind != "team" {
		return fmt.Errorf("user %d is not a team (%q, %v)", team, kind, err)
	}
	switch args[0] {
	case "endcard":
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		const host, sess = "e2e-team-host", "e2e-team-sess-0001"
		var ask int64
		if err := tx.QueryRow(ctx, `INSERT INTO attention_asks (owner_id, mirror_host, mirror_session, question, options, owner_card, expires_at)
			VALUES ($1, $2, $3, $4, '["End it","Not now"]'::jsonb, true, now() + interval '1 hour') RETURNING id`,
			team, host, sess, "End “e2e-team:1”?\n"+host+" · tmux session e2e-team · window @1").Scan(&ask); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO chief_proposals (owner_id, kind, chief_agent, owner_message, subject_key, subject_runtime,
			subject_host, subject_tmux_session, subject_tmux_window, ask_id)
			VALUES ($1, 'end_session', 1, 1, $2, 'claude', $3, 'e2e-team', '@1', $4)`, team, sess, host, ask); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		fmt.Println(ask)
	case "cleanup":
		for _, q := range []string{`DELETE FROM chief_proposals WHERE owner_id = $1`, `DELETE FROM attention_asks WHERE owner_id = $1`} {
			if _, err := conn.Exec(ctx, q, team); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
	return nil
}
