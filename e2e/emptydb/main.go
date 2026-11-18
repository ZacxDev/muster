// Command emptydb gives a spec a FRESH, EMPTY database beside the suite's own:
// the suite's database name with "_empty" inserted before the `_e2e` suffix,
// dropped and re-created on every run, and its DSN printed on stdout. A server
// started on it migrates it at boot and has no agents, no mirrored hosts and no
// tasks — the state a brand-new install is in (onboarding.spec.ts).
//
// The seeded database cannot stand in: it holds agents, so the Agents list is
// never empty there.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ZacxDev/muster/e2e/internal/e2edb"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "emptydb:", err)
		os.Exit(1)
	}
}

func run() error {
	dsn := os.Getenv("DATABASE_URL")
	if err := e2edb.Check(dsn); err != nil {
		return err
	}
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return err
	}
	name := strings.TrimSuffix(cfg.Database, e2edb.Suffix) + "_empty" + e2edb.Suffix
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := conn.Exec(ctx, `DROP DATABASE IF EXISTS `+ident+` WITH (FORCE)`); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, `CREATE DATABASE `+ident); err != nil {
		return err
	}
	u := strings.Replace(dsn, "/"+cfg.Database, "/"+name, 1)
	if u == dsn {
		return fmt.Errorf("could not rewrite the database name in the DSN")
	}
	fmt.Println(u)
	return nil
}
