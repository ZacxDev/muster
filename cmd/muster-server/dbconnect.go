package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/db"
)

// The boot-time database connect budget. Package variables so a test can
// shorten the interval; production never changes them.
var (
	dbConnectBudget   = 30 * time.Second
	dbConnectInterval = time.Second
)

// connectDB is db.Connect, retried for up to dbConnectBudget at boot.
//
// 🔴 WHY A RETRY AT ALL: a database that is about to accept connections is
// not a configuration error. Measured with deploy/docker-compose.yml on a
// fresh volume: Postgres's init runs a temporary socket-only server, a
// socket-level readiness probe passes during it, muster's first TCP connect is
// refused, and one failed connect used to be a Fatalf — so the container
// crash-looped and `up --wait` saw a restarting muster. The compose healthcheck
// now probes TCP too; this closes the same race for every other orchestrator.
//
// ⚠ A MALFORMED URL IS NOT RETRIED: it fails the same way forever, so it is
// refused at once, before the first attempt.
func connectDB(ctx context.Context, dsn string, logger *log.Logger) (*pgxpool.Pool, error) {
	if _, err := pgxpool.ParseConfig(dsn); err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	deadline := time.Now().Add(dbConnectBudget)
	for attempt := 1; ; attempt++ {
		pool, err := db.Connect(ctx, dsn)
		if err == nil {
			if attempt > 1 {
				logger.Printf("db: connected on attempt %d", attempt)
			}
			return pool, nil
		}
		if time.Now().Add(dbConnectInterval).After(deadline) {
			return nil, fmt.Errorf("%w (gave up after %d attempts over %s)", err, attempt, dbConnectBudget)
		}
		logger.Printf("db: connect attempt %d failed, retrying in %s: %v", attempt, dbConnectInterval, err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(dbConnectInterval):
		}
	}
}
