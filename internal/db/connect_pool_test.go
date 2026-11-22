package db_test

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestConnectAppliesThePoolDefaults pins the Connect -> poolConfig seam: a pool
// opened through db.Connect with a DSN that names no pool_* key gets muster's
// size and idle timeout. poolConfig's own tests cannot see Connect going back
// to pgxpool.ParseConfig(dsn); this one can.
func TestConnectAppliesThePoolDefaults(t *testing.T) {
	dsn := dbtest.DSN(t) // skips, or fails, per MUSTER_TEST_REQUIRE_DB
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse test DSN: %v", err)
	}
	for k := range u.Query() {
		if strings.HasPrefix(k, "pool_") {
			t.Fatalf("test DSN sets %s; this test needs a DSN with no pool params", k)
		}
	}

	ctx, cancel := context.WithTimeout(db.AsMigratorForTest(context.Background()), time.Minute)
	defer cancel()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	if got := pool.Config().MaxConns; got != 8 {
		t.Errorf("MaxConns = %d, want 8", got)
	}
	if got := pool.Config().MaxConnIdleTime; got != 5*time.Minute {
		t.Errorf("MaxConnIdleTime = %s, want 5m", got)
	}
}
