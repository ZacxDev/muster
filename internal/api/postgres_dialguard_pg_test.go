package api

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/safedial"
)

// TestTheDialGuardReachesThePostgresQueryTool is the seam from
// Extensions.DialGuard (MUSTER_DIAL_DENY_PRIVATE) to the executor behind the
// postgres_query MCP tool. The package's test database is on loopback: with
// the strict guard wired, an approved agent's query is refused naming the
// guard; with none (the default, the control), the same query returns a row.
func TestTheDialGuardReachesThePostgresQueryTool(t *testing.T) {
	for _, tc := range []struct {
		name  string
		guard *safedial.Guard
	}{{"guard on", &safedial.Guard{}}, {"guard off (default)", nil}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPGQueryFixtureWith(t, func(ext *Extensions, _ *pgxpool.Pool) { ext.DialGuard = tc.guard })
			id := f.seedAgent(t, "pg-dialguard", "pg-dialguard-token")
			g, _, err := f.store.RequestGrant(f.ctx, id, "pgro", "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.DecideGrant(f.ctx, g.ID, true, "operator", connectors.SourceStored); err != nil {
				t.Fatal(err)
			}
			text, isErr := f.pgTool(t, "pg-dialguard-token", map[string]any{"connector": "pgro", "sql": "SELECT 1 AS one"})
			refused := isErr && strings.Contains(text, safedial.ErrForbiddenAddress.Error())
			if tc.guard != nil && !refused {
				t.Fatalf("guard on: postgres_query to a loopback database was not refused by the guard: isError %v, %s", isErr, text)
			}
			if tc.guard == nil && (isErr || !strings.Contains(text, `"row_count":1`)) {
				t.Fatalf("CONTROL (guard off): postgres_query to the loopback database failed: isError %v, %s", isErr, text)
			}
		})
	}
}
