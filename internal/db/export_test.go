package db

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AsMigratorForTest exposes the migration runner's context to this package's
// external tests (package db_test). The migration and schema tests operate as
// the DATABASE OWNER — DDL, CREATE DATABASE/ROLE/SCHEMA, raw fixture rows for
// any owner — which is exactly what the migrator context is: the login role,
// muster.scope = 'all' — PLUS muster.uid = 1, so a raw fixture INSERT that names
// no owner_id still lands as the operator, as it did before 0007 made the
// default the principal's id. Application code cannot reach it (unexported, and
// this file compiles only into this package's test binary).
func AsMigratorForTest(ctx context.Context) context.Context {
	return context.WithValue(ctx, migratorKey{}, "1")
}

// AfterReleaseForTest is the pool's AfterRelease hook, for the test that a
// released connection carries nobody's id.
var AfterReleaseForTest = afterRelease

// SkippedMigrationCheckForTest runs the runner's skipped-migration check over
// the EMBEDDED migrations plus extra (version -> file name), against pool's
// schema_migrations. extra lets a test stand in for a migration another branch
// adds (#67's 0010) before that file exists in this tree.
func SkippedMigrationCheckForTest(ctx context.Context, pool *pgxpool.Pool, extra map[int]string) error {
	ms, err := loadMigrations()
	if err != nil {
		return err
	}
	for v, name := range extra {
		ms = append(ms, migration{version: v, name: name})
	}
	sortMigrations(ms)
	var current int
	if err := pool.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return err
	}
	return checkNoSkippedMigration(ctx, pool.QueryRow, ms, current)
}
