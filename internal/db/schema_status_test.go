package db_test

import (
	"testing"

	"github.com/ZacxDev/muster/internal/db"
)

// TestSchemaStatusReadsTheTableNotTheBoot: a migrated database is current, and
// the SAME database with its newest row deleted is not — the status follows
// schema_migrations, so a database behind the binary is caught even though the
// process that would have migrated it is not the one asking.
func TestSchemaStatusReadsTheTableNotTheBoot(t *testing.T) {
	ctx, pool := migrated(t)

	st, err := db.SchemaStatus(ctx, pool)
	if err != nil {
		t.Fatalf("SchemaStatus: %v", err)
	}
	if st.Latest < 56 {
		t.Fatalf("Latest = %d, want the newest embedded migration (>= 56); the embed is not being read", st.Latest)
	}
	if !st.Current() || st.Applied != st.Latest {
		t.Fatalf("a freshly migrated database reads %+v, want Applied == Latest", st)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = $1`, st.Latest); err != nil {
		t.Fatalf("delete newest row: %v", err)
	}
	behind, err := db.SchemaStatus(ctx, pool)
	if err != nil {
		t.Fatalf("SchemaStatus: %v", err)
	}
	if behind.Current() || behind.Applied != st.Latest-1 {
		t.Fatalf("with the newest row gone it reads %+v, want Applied %d and not current", behind, st.Latest-1)
	}
	// Put it back so nothing after this test in the package sees a gap.
	if _, err := pool.Exec(ctx, `INSERT INTO schema_migrations (version, name) VALUES ($1, 'restored-by-test')`, st.Latest); err != nil {
		t.Fatalf("restore row: %v", err)
	}
}

// TestASchemaAheadOfTheBinaryIsNotCurrent: an older image against a newer
// database does not know that schema.
func TestASchemaAheadOfTheBinaryIsNotCurrent(t *testing.T) {
	if (db.SchemaState{Applied: 60, Latest: 56}).Current() {
		t.Fatal("a database ahead of the binary reads as current")
	}
	if !(db.SchemaState{Applied: 56, Latest: 56}).Current() {
		t.Fatal("an equal pair reads as not current")
	}
}
