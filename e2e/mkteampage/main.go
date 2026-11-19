// Command mkteampage makes, reads and removes a TEAM'S PAGE (migration 0160)
// on the e2e database, for tests/team-pages.spec.ts: a team the operator
// (user 1) is a VIEWER of, a second person who is the page's CREATOR (the
// person who started the team agent that made it), and a two-version page the
// team owns. The signed-in operator is then a member who is not the creator.
//
//	DATABASE_URL=…_e2e go run ./e2e/mkteampage create <stamp>   prints {"team","name","page","creator"} as JSON
//	DATABASE_URL=…_e2e go run ./e2e/mkteampage responses <page>  prints each owner-view response's responder id, one per line
//	DATABASE_URL=…_e2e go run ./e2e/mkteampage remove <team>     removes the team, its page and its creator
//
// The rows are the ones a team agent's page_create writes (pages, then
// page_versions) and users.CreateTeam / SetTeamMember write (a users row of
// kind 'team', its memberships), written as the login role.
//
// 🔴 IT REFUSES ANY DATABASE WHOSE NAME DOES NOT END IN "_e2e" (e2edb.Check):
// it writes users, team_members and pages as the login role, which nothing
// outside a fixture may do.
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

// Blocks is the page's one question, as the page tools would store it.
const blocks = `[{"type":"question","id":"pick","label":"Which day suits the team?","submit_label":"Send pick"}]`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "mkteampage:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	dsn := os.Getenv("DATABASE_URL")
	if err := e2edb.Check(dsn); err != nil {
		return err
	}
	if len(args) != 2 {
		return fmt.Errorf("usage: mkteampage create <stamp> | responses <page> | remove <team>")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	switch args[0] {
	case "create":
		return create(ctx, conn, args[1])
	case "responses":
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			return err
		}
		rows, err := conn.Query(ctx, `SELECT coalesce(responder_user, 0) FROM page_responses WHERE page_id = $1 AND by_owner ORDER BY id`, id)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			return err
		}
		for _, r := range ids {
			fmt.Println(r)
		}
		return nil
	case "remove":
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil || id <= 1 {
			return fmt.Errorf("not a team id: %q", args[1])
		}
		return remove(ctx, conn, id)
	}
	return fmt.Errorf("unknown verb %q", args[0])
}

func create(ctx context.Context, conn *pgx.Conn, stamp string) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	name := "E2E Pages Team " + stamp
	var team, creator, page int64
	if err := tx.QueryRow(ctx, `INSERT INTO users (kind, role, display_name) VALUES ('team', 'member', $1) RETURNING id`, name).Scan(&team); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`, "E2E Page Creator "+stamp).Scan(&creator); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO team_members (team_id, user_id, role, added_by) VALUES ($1, 1, 'viewer', 1), ($1, $2, 'operator', 1)`, team, creator); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `INSERT INTO pages (owner_id, title, current_version, mirror_host, creator_label, created_by_user)
		VALUES ($1, $2, 2, 'e2e-team-host', 'team worker', $3) RETURNING id`, team, "Team offsite "+stamp, creator).Scan(&page); err != nil {
		return err
	}
	for _, v := range []int{1, 2} {
		if _, err := tx.Exec(ctx, `INSERT INTO page_versions (owner_id, page_id, version, title, blocks, size_bytes, created_by)
			VALUES ($1, $2, $3, $4, $5::jsonb, $6, 'team worker')`, team, page, v, "Team offsite "+stamp, blocks, len(blocks)); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	out, _ := json.Marshal(map[string]any{"team": team, "name": name, "page": page, "creator": creator})
	fmt.Println(string(out))
	return nil
}

func remove(ctx context.Context, conn *pgx.Conn, team int64) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var creators []int64
	rows, err := tx.Query(ctx, `SELECT DISTINCT created_by_user FROM pages WHERE owner_id = $1 AND created_by_user IS NOT NULL`, team)
	if err != nil {
		return err
	}
	if creators, err = pgx.CollectRows(rows, pgx.RowTo[int64]); err != nil {
		return err
	}
	for _, q := range []string{`DELETE FROM pages WHERE owner_id = $1`, `DELETE FROM audit_log WHERE team_id = $1`, `DELETE FROM team_members WHERE team_id = $1`} {
		if _, err := tx.Exec(ctx, q, team); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1 AND kind = 'team'`, team); err != nil {
		return err
	}
	for _, c := range creators {
		if _, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1 AND kind = 'person' AND id <> 1`, c); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
