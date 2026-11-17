// Command mkteam makes, reads and removes a TEAM the operator (user 1)
// manages, on the e2e database — for a browser spec on the shared
// password-mode server, which has no GoTrue and so no /admin/teams page to
// make one with. The browser session's principal reads its memberships from
// team_members on every request (principalFor), so a team made here is the
// operator's at once, with no new sign-in.
//
//	DATABASE_URL=…_e2e go run ./e2e/mkteam create <name>     prints the team's id
//	DATABASE_URL=…_e2e go run ./e2e/mkteam officers <id>     prints how many officer defs and types it owns
//	DATABASE_URL=…_e2e go run ./e2e/mkteam remove <id>       removes it and its memberships
//
// The rows are the ones users.CreateTeam writes (a users row of kind 'team',
// role 'member', and the first manager's membership), without its audit row.
// remove deletes the team's audit rows with it (the seed's accounts reset
// deletes the audit log whole anyway), so it leaves nothing that reset would
// trip on.
//
// 🔴 remove DELETES NO OFFICER ROW. A team that still owns one is a failure
// of the spec that made it (a refusal that did not refuse), so remove fails
// loudly on the foreign key rather than tidying the evidence away.
//
// 🔴 IT REFUSES ANY DATABASE WHOSE NAME DOES NOT END IN "_e2e" (e2edb.Check):
// it writes users and team_members as the login role, which nothing outside a
// fixture may do.
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/e2e/internal/e2edb"
	"github.com/ZacxDev/muster/internal/branding"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "mkteam:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	dsn := os.Getenv("DATABASE_URL")
	if err := e2edb.Check(dsn); err != nil {
		return err
	}
	if len(args) != 2 {
		return fmt.Errorf("usage: mkteam create <name> | officers <id> | remove <id>")
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
	case "officers":
		id, err := teamID(args[1])
		if err != nil {
			return err
		}
		var defs, types int
		if err := conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM officer_defs WHERE owner_id = $1), (SELECT count(*) FROM officer_types WHERE owner_id = $1)`, id).Scan(&defs, &types); err != nil {
			return err
		}
		fmt.Printf("%d %d\n", defs, types)
		return nil
	case "remove":
		id, err := teamID(args[1])
		if err != nil {
			return err
		}
		return remove(ctx, conn, id)
	}
	return fmt.Errorf("unknown verb %q", args[0])
}

func teamID(v string) (int64, error) {
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil || id <= 1 {
		return 0, fmt.Errorf("not a team id: %q", v)
	}
	return id, nil
}

func create(ctx context.Context, conn *pgx.Conn, name string) error {
	// A team's name is a users.display_name: the character rule
	// users.CreateTeam applies (the display-name writer ledger's).
	if err := branding.CheckNameText(name); err != nil {
		return err
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id int64
	if err := tx.QueryRow(ctx, `INSERT INTO users (kind, role, display_name) VALUES ('team', 'member', $1) RETURNING id`, name).Scan(&id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO team_members (team_id, user_id, role, added_by) VALUES ($1, 1, 'manager', 1)`, id); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	fmt.Println(id)
	return nil
}

func remove(ctx context.Context, conn *pgx.Conn, id int64) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The team's audit rows (an officer created and deleted on its page writes
	// them, auditTeam) and its memberships go first: both reference it.
	for _, q := range []string{`DELETE FROM audit_log WHERE team_id = $1`, `DELETE FROM team_members WHERE team_id = $1`} {
		if _, err := tx.Exec(ctx, q, id); err != nil {
			return err
		}
	}
	tag, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1 AND kind = 'team'`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("no team %d to remove", id)
	}
	return tx.Commit(ctx)
}
