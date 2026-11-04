// Command recentsfix is the switcher dock spec's fixture
// (e2e/tests/switcher-dock.spec.ts): it forgets every recently opened entry,
// so the spec's "Recent holds 30" case can count visits from none. The suite's
// other specs visit sessions on the same database, and the store keeps up to
// recents.MaxPerOwner of them, so without it a five-visit control would read
// whatever ran before.
//
//	DATABASE_URL=…_e2e go run ./e2e/recentsfix clear
//
// It prints how many rows it removed.
//
// 🔴 IT REFUSES ANY DATABASE WHOSE NAME DOES NOT END IN "_e2e" (e2edb.Check):
// it writes as the login role with row-level security's scope set to all.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/e2e/internal/e2edb"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "recentsfix:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	dsn := os.Getenv("DATABASE_URL")
	if err := e2edb.Check(dsn); err != nil {
		return err
	}
	if len(args) != 1 || args[0] != "clear" {
		return fmt.Errorf("usage: recentsfix clear")
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
	tag, err := conn.Exec(ctx, `DELETE FROM recents`)
	if err != nil {
		return err
	}
	fmt.Println(tag.RowsAffected())
	return nil
}
