package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// queryTracer is the tracer every pool Connect opens gets (SetQueryTracer);
// nil, the default, is none.
var queryTracer pgx.QueryTracer

// SetQueryTracer installs t on every pool Connect opens from now on:
// muster-server hands it internal/telemetry's (nil while tracing is off). It
// is a setter rather than an import so the packages that link internal/db
// into the agent pod's binary do not link the tracing SDK with it.
func SetQueryTracer(t pgx.QueryTracer) { queryTracer = t }

// StatementTimeout is injected into every connection this package opens.
//
// 🔴 IT IS SET HERE RATHER THAN LEFT TO THE SERVER'S DEFAULT (which is "no
// limit"), because a query with no bound is indistinguishable from a hung
// process at every layer above it. A caller that genuinely needs longer — the
// migration runner's own CREATE INDEX, say — must say so explicitly on its own
// connection rather than raise this for everyone.
const StatementTimeout = 10 * time.Second

// DefaultMaxConns and DefaultMaxConnIdleTime are the pool's size and idle
// timeout when the DSN does not set pool_max_conns / pool_max_conn_idle_time.
//
// 🔴 pgx's OWN DEFAULT IS max(4, runtime.NumCPU()) — the NODE's CPU count, not
// anything about the database — so the same image opens 4 connections on a
// laptop and 24 on a 24-CPU node, and holds them for pgx's 30-minute idle
// timeout. Measured: a 1Gi Postgres container was OOM-killed by ~24 idle
// backends at ~40 MB each, worst during a rollout, when two pods each hold a
// pool. muster's load is a handful of short queries, so 8 is ample.
//
// The DSN is the knob: pool_max_conns=N and pool_max_conn_idle_time=<duration>
// in DATABASE_URL always win over these (docs/configuration.md).
const (
	DefaultMaxConns        = 8
	DefaultMaxConnIdleTime = 5 * time.Minute
)

// poolConfig parses dsn into a pool config with muster's pool defaults
// applied wherever the DSN did not set its own.
//
// "Set by the DSN" cannot be read off pgxpool.ParseConfig's result: it fills
// pgx's defaults in for absent keys, so a DSN that says pool_max_conns=24 and
// one that says nothing look the same on a 24-CPU node. pgx.ParseConfig — the
// connection-level parser pgxpool itself calls first, so it reads both the URL
// and the keyword/value form exactly as pgxpool does — leaves the pool_* keys
// in RuntimeParams, which is where they are looked for.
func poolConfig(dsn string) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	raw, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	if _, set := raw.RuntimeParams["pool_max_conns"]; !set {
		cfg.MaxConns = DefaultMaxConns
	}
	if _, set := raw.RuntimeParams["pool_max_conn_idle_time"]; !set {
		cfg.MaxConnIdleTime = DefaultMaxConnIdleTime
	}
	return cfg, nil
}

// Connect opens a pool against dsn with muster's connection settings applied,
// and verifies it can actually reach the server before returning.
func Connect(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := poolConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	if _, set := cfg.ConnConfig.RuntimeParams["statement_timeout"]; !set {
		cfg.ConnConfig.RuntimeParams["statement_timeout"] =
			fmt.Sprintf("%d", StatementTimeout.Milliseconds())
	}
	if _, set := cfg.ConnConfig.RuntimeParams["application_name"]; !set {
		cfg.ConnConfig.RuntimeParams["application_name"] = "muster"
	}
	// Row-level security: every acquire acts as AppRole with the context's
	// principal; every release clears it. See scope.go.
	cfg.PrepareConn = prepareConn
	cfg.AfterRelease = afterRelease
	// A span per query and per connection wait, inside a trace — none (the
	// pool exactly as before) unless SetQueryTracer installed one.
	if queryTracer != nil {
		cfg.ConnConfig.Tracer = queryTracer
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	// 🔴 PING BEFORE RETURNING. pgxpool.New is LAZY — it returns a healthy-looking
	// pool against a server that does not exist, and the failure then surfaces at
	// the first query, attributed to whatever happened to run it.
	//
	// It pings as the MIGRATOR (the login role, no row scope): the pool may be
	// connecting to a database 0007 has not reached yet, where AppRole does not
	// exist, and a ping reads no rows.
	if err := pool.Ping(asMigrator(ctx)); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}
