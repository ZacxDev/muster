package api

import (
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// postgres_query over POST /mcp, against this package's own test database as
// the target: only an agent whose owner approved ITS grant for the connector
// gets rows.

const (
	pgqRole     = "muster_pgconn_probe"
	pgqPassword = "pgconn-probe-Pw-0123456789"
)

// newPGQueryFixture is connFixture plus a postgres connector "pgro" that logs
// in to the test database as a dedicated probe role.
func newPGQueryFixture(t *testing.T) *connFixture {
	t.Helper()
	f := newConnFixture(t)
	dsn := dbtest.DSN(t)
	target, err := pgconn.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.OwnerExec(t, dsn, `DO $$ BEGIN
		CREATE ROLE `+pgqRole+` LOGIN;
	EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL; END $$`)
	dbtest.OwnerExec(t, dsn, `ALTER ROLE `+pgqRole+` LOGIN PASSWORD '`+pgqPassword+`'`)
	if _, err := f.store.CreateConnector(f.ctx, connectors.NewConnector{Name: "pgro", Scheme: connectors.SchemePostgres,
		Hosts: []string{net.JoinHostPort(target.Host, strconv.Itoa(int(target.Port)))}, Username: pgqRole, Secret: pgqPassword,
		Postgres: &connectors.PostgresSettings{Database: target.Database, SSLMode: "disable"}}); err != nil {
		t.Fatalf("create the postgres connector: %v", err)
	}
	return f
}

func (f *connFixture) pgTool(t *testing.T, tok string, args map[string]any) (string, bool) {
	t.Helper()
	r, code := f.mcp(t, tok, mcpToolCall("postgres_query", args), nil)
	if r == nil || r.Error != nil {
		t.Fatalf("postgres_query: HTTP %d %+v", code, r)
	}
	return toolText(t, r)
}

// TestAnUngrantedAgentsPostgresQueryIsRefused: two agents of one owner; only
// one is approved. The other's query is refused naming the missing grant (and
// files a pending request, as the HTTP proxy's first use does), and runs
// nothing; the approved one's returns rows — the positive control that the
// refusal is the grant check and not a broken executor.
func TestAnUngrantedAgentsPostgresQueryIsRefused(t *testing.T) {
	f := newPGQueryFixture(t)
	grantedID := f.seedAgent(t, "pg-granted", "pg-granted-token")
	ungrantedID := f.seedAgent(t, "pg-ungranted", "pg-ungranted-token")
	g, _, err := f.store.RequestGrant(f.ctx, grantedID, "pgro", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DecideGrant(f.ctx, g.ID, true, "operator", connectors.SourceStored); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"connector": "pgro", "sql": "SELECT current_user AS who, $1::int AS n", "params": []any{7}}

	text, isErr := f.pgTool(t, "pg-ungranted-token", args)
	if !isErr || !strings.Contains(text, "no approved grant for connector pgro") || strings.Contains(text, pgqRole) {
		t.Errorf("an ungranted agent's postgres_query: isError %v, %s", isErr, text)
	}
	if lg, err := f.store.LatestGrant(f.ctx, ungrantedID, "pgro"); err != nil || lg.Status != connectors.StatusPending {
		t.Errorf("the ungranted agent's grant after its query = %+v (%v), want a pending request", lg, err)
	}
	// Still refused while it is pending.
	if text, isErr := f.pgTool(t, "pg-ungranted-token", args); !isErr || !strings.Contains(text, "grant: pending") {
		t.Errorf("a pending agent's postgres_query: isError %v, %s", isErr, text)
	}

	text, isErr = f.pgTool(t, "pg-granted-token", args)
	if isErr {
		t.Fatalf("the approved agent's postgres_query failed: %s", text)
	}
	var res connectors.PGResult
	if err := json.Unmarshal([]byte(text), &res); err != nil {
		t.Fatalf("result is not a PGResult: %s", text)
	}
	if res.RowCount != 1 || *res.Rows[0][0] != pgqRole || *res.Rows[0][1] != "7" {
		t.Errorf("the approved agent's result = %s", text)
	}
	if strings.Contains(text, pgqPassword) {
		t.Errorf("the tool result carries the password: %s", text)
	}
	// The agent's view of the connector teaches the tool, not the proxy.
	st, _, _ := f.pgToolStatus(t, "pg-granted-token")
	if !strings.Contains(st, "postgres_query") || strings.Contains(st, "/connect/pgro") {
		t.Errorf("connector_status for a postgres connector = %s", st)
	}
}

func (f *connFixture) pgToolStatus(t *testing.T, tok string) (string, bool, *mcpError) {
	t.Helper()
	r, code := f.mcp(t, tok, mcpToolCall("connector_status", map[string]any{"name": "pgro"}), nil)
	if r == nil {
		t.Fatalf("connector_status: HTTP %d", code)
	}
	text, isErr := toolText(t, r)
	return text, isErr, nil
}
