// Command muster-migrate applies muster's migrations to the database named by
// DATABASE_URL and exits.
//
// 🔴 IT IS A CI AND DEVELOPMENT FIXTURE, NOT A DEPLOY STEP. The server migrates
// itself at boot. This exists so the TEMPLATE database that internal/dbtest
// copies can be migrated ONCE, before the test binaries start — otherwise the
// first Postgres-backed package to run pays the whole bootstrap under an
// advisory lock while every other package queues behind it.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/ZacxDev/muster/internal/db"
)

const budget = 5 * time.Minute

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "muster-migrate: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		// 🔴 AN EMPTY DSN IS A REFUSAL, NOT A DEFAULT. Falling back to a local
		// server would let a CI step that forgot to set the variable migrate
		// something other than the database the tests will use, and report
		// success for it.
		return fmt.Errorf("DATABASE_URL is unset; refusing to guess a database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	return db.Migrate(ctx, pool, log.New(os.Stdout, "", 0))
}
