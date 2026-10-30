package connectors_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/connectors"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// The postgres connector's guards, against the package's own test database as
// the TARGET database: the executor logs in as pgTestRole — a dedicated LOGIN
// role, as docs/connectors.md recommends — which is GRANTED INSERT on the probe
// table, so a refused write is refused by the read-only transaction and not by
// a missing privilege.

const (
	pgTestRole     = "muster_pgconn_probe"
	pgTestPassword = "pgconn-probe-Pw-0123456789"
	pgProbeTable   = "pgconn_probe"
)

type pgFixture struct {
	ctx    context.Context
	pool   *pgxpool.Pool
	dsn    string
	store  *connectors.PGStore
	exec   *connectors.PostgresExecutor
	logs   *bytes.Buffer
	caller connectors.Caller
	target *pgconn.Config
}

// newPGFixture builds the store, the probe role and table, and a postgres
// connector "pgprobe" (sslmode disable: CI's server has no TLS) with settings.
func newPGFixture(t *testing.T, ps connectors.PostgresSettings) *pgFixture {
	t.Helper()
	ctx, pool := testPool(t)
	dsn := dbtest.DSN(t)
	target, err := pgconn.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	ensureProbeRole(t, dsn, target.Database)
	f := &pgFixture{ctx: ctx, pool: pool, dsn: dsn, store: newStore(t, pool, key(9)), logs: &bytes.Buffer{}, target: target}
	f.exec = &connectors.PostgresExecutor{Store: f.store, Logger: log.New(f.logs, "", 0), RatePerMinute: 6000, Burst: 1000}
	t.Cleanup(f.exec.Close)
	id := seedAgent(t, ctx, pool, "pg-agent", "pg-agent-token")
	f.caller = connectors.Caller{AgentID: id, AgentName: "pg-agent"}
	if ps.SSLMode == "" {
		ps.SSLMode = "disable"
	}
	ps.Database = target.Database
	if _, err := f.store.CreateConnector(ctx, connectors.NewConnector{Name: "pgprobe", Scheme: connectors.SchemePostgres,
		Hosts: []string{net.JoinHostPort(target.Host, strconv.Itoa(int(target.Port)))}, Username: pgTestRole,
		Secret: pgTestPassword, Postgres: &ps}); err != nil {
		t.Fatalf("create the postgres connector: %v", err)
	}
	return f
}

// ensureProbeRole creates (or resets) the probe login role and the probe table
// it may INSERT into. Roles are cluster-wide, so a concurrent package doing
// the same is tolerated.
func ensureProbeRole(t *testing.T, dsn, database string) {
	t.Helper()
	dbtest.OwnerExec(t, dsn, `DO $$ BEGIN
		CREATE ROLE `+pgTestRole+` LOGIN;
	EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL; END $$`)
	dbtest.OwnerExec(t, dsn, `ALTER ROLE `+pgTestRole+` LOGIN PASSWORD '`+pgTestPassword+`'`)
	dbtest.OwnerExec(t, dsn, `CREATE TABLE IF NOT EXISTS public.`+pgProbeTable+` (id int)`)
	dbtest.OwnerExec(t, dsn, `TRUNCATE public.`+pgProbeTable)
	dbtest.OwnerExec(t, dsn, `GRANT USAGE ON SCHEMA public TO `+pgTestRole)
	dbtest.OwnerExec(t, dsn, `GRANT SELECT, INSERT ON public.`+pgProbeTable+` TO `+pgTestRole)
}

func (f *pgFixture) query(t *testing.T, sql string, params ...any) (connectors.PGResult, error) {
	t.Helper()
	var raw []json.RawMessage
	for _, p := range params {
		b, _ := json.Marshal(p)
		raw = append(raw, b)
	}
	return f.exec.Query(f.ctx, f.caller, "pgprobe", connectors.PGQuery{SQL: sql, Params: raw})
}

func pgState(err error) string {
	var pe *connectors.PGError
	if errors.As(err, &pe) {
		return pe.SQLState
	}
	return ""
}

func pgClass(err error) string {
	var pe *connectors.PGError
	if errors.As(err, &pe) {
		return pe.Class
	}
	return ""
}

// TestAPostgresQueryReturnsColumnsAndRows is the positive control every guard
// test below leans on: a parameterised SELECT through the executor answers its
// columns, types and text values.
func TestAPostgresQueryReturnsColumnsAndRows(t *testing.T) {
	f := newPGFixture(t, connectors.PostgresSettings{})
	res, err := f.query(t, `SELECT $1::int + 1 AS n, $2::text AS s, NULL::text AS z, current_user AS who`, 41, "hi")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if res.RowCount != 1 || res.Truncated || len(res.Rows) != 1 {
		t.Fatalf("result = %+v, want one untruncated row", res)
	}
	names := []string{}
	for _, c := range res.Columns {
		names = append(names, c.Name+":"+c.Type)
	}
	if got := strings.Join(names, ","); got != "n:int4,s:text,z:text,who:name" {
		t.Errorf("columns = %s", got)
	}
	row := res.Rows[0]
	if *row[0] != "42" || *row[1] != "hi" || row[2] != nil || *row[3] != pgTestRole {
		t.Errorf("row = %v %v %v %v", *row[0], *row[1], row[2], *row[3])
	}
	if res.MaxRows != connectors.PGDefaultMaxRows || res.MaxBytes != connectors.PGDefaultMaxBytes {
		t.Errorf("caps = %d rows / %d bytes, want the defaults", res.MaxRows, res.MaxBytes)
	}
}

// TestAPostgresWriteIsRefusedInsideTheReadOnlyTransaction: an INSERT the role
// is GRANTED is refused with SQLSTATE 25006 (read_only_sql_transaction), and
// the table is unchanged. Red without pgBeginReadOnly's READ ONLY.
func TestAPostgresWriteIsRefusedInsideTheReadOnlyTransaction(t *testing.T) {
	f := newPGFixture(t, connectors.PostgresSettings{})
	// Control: the role really may insert outside the executor's transaction,
	// so the refusal below can only come from READ ONLY.
	conn, err := pgconn.Connect(f.ctx, "host="+f.target.Host+" port="+strconv.Itoa(int(f.target.Port))+" dbname="+
		f.target.Database+" user="+pgTestRole+" password="+pgTestPassword+" sslmode=disable")
	if err != nil {
		t.Fatalf("control connect: %v", err)
	}
	if _, err := conn.Exec(f.ctx, `BEGIN; INSERT INTO public.`+pgProbeTable+` VALUES (7); ROLLBACK`).ReadAll(); err != nil {
		t.Fatalf("control: the probe role cannot insert at all (%v), so this test would prove nothing", err)
	}
	conn.Close(f.ctx)

	for _, sql := range []string{
		`INSERT INTO public.` + pgProbeTable + ` VALUES (1)`,
		`WITH w AS (INSERT INTO public.` + pgProbeTable + ` VALUES (2) RETURNING id) SELECT * FROM w`,
	} {
		_, err := f.query(t, sql)
		if pgState(err) != "25006" || pgClass(err) != "read_only" {
			t.Errorf("a write inside the read-only transaction (%q): got %v (state %q), want SQLSTATE 25006", sql, err, pgState(err))
		}
	}
	var n int
	if err := f.pool.QueryRow(authz.System(f.ctx), `SELECT count(*) FROM public.`+pgProbeTable).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("the probe table has %d rows after refused writes", n)
	}
}

// TestAPostgresMultiStatementStringIsRefused: the extended protocol's Parse
// refuses a string holding two statements (42601) — nothing runs, the INSERT
// smuggled after a SELECT included.
func TestAPostgresMultiStatementStringIsRefused(t *testing.T) {
	f := newPGFixture(t, connectors.PostgresSettings{})
	for _, sql := range []string{
		`SELECT 1; SELECT 2`,
		`SELECT 1; COMMIT; INSERT INTO public.` + pgProbeTable + ` VALUES (3)`,
		`SELECT 1;SET statement_timeout = 0`,
	} {
		_, err := f.query(t, sql)
		var pe *connectors.PGError
		if !errors.As(err, &pe) || pe.SQLState != "42601" || !strings.Contains(pe.Message, "multiple commands") {
			t.Errorf("multi-statement %q: got %v, want 42601 'cannot insert multiple commands'", sql, err)
		}
	}
	// Positive control: one statement with a trailing semicolon is ONE statement.
	if _, err := f.query(t, `SELECT 1;`); err != nil {
		t.Errorf("a single statement with a trailing ';' was refused: %v", err)
	}
	var n int
	if err := f.pool.QueryRow(authz.System(f.ctx), `SELECT count(*) FROM public.`+pgProbeTable).Scan(&n); err != nil || n != 0 {
		t.Errorf("probe rows = %d (%v), want 0", n, err)
	}
}

// TestAPostgresStatementTimeoutCancelsALongQuery: statement_timeout is SET
// LOCAL from the connector's setting — a 5s sleep under a 300ms limit ends as
// 57014 in well under a second, and SHOW reports the limit.
func TestAPostgresStatementTimeoutCancelsALongQuery(t *testing.T) {
	f := newPGFixture(t, connectors.PostgresSettings{StatementTimeoutMS: 300})
	start := time.Now()
	_, err := f.query(t, `SELECT pg_sleep(5)`)
	if pgState(err) != "57014" || pgClass(err) != "timeout" {
		t.Fatalf("pg_sleep(5) under a 300ms timeout: got %v, want SQLSTATE 57014", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("the timed-out query took %s", d)
	}
	res, err := f.query(t, `SELECT current_setting('statement_timeout'), current_setting('lock_timeout'), current_setting('transaction_read_only')`)
	if err != nil {
		t.Fatal(err)
	}
	if got := *res.Rows[0][0] + "|" + *res.Rows[0][1] + "|" + *res.Rows[0][2]; got != "300ms|300ms|on" {
		t.Errorf("statement_timeout|lock_timeout|transaction_read_only = %s, want 300ms|300ms|on", got)
	}
}

// TestAPostgresResultIsCappedByRowsAndBytes: past the row cap, and past the
// byte cap, the result stops and says why.
func TestAPostgresResultIsCappedByRowsAndBytes(t *testing.T) {
	f := newPGFixture(t, connectors.PostgresSettings{MaxBytes: 10_000})
	res, err := f.query(t, `SELECT g FROM generate_series(1, 500) g`)
	if err != nil {
		t.Fatal(err)
	}
	if res.RowCount != connectors.PGDefaultMaxRows || len(res.Rows) != connectors.PGDefaultMaxRows || !res.Truncated ||
		res.TruncatedBy != "rows" || *res.Rows[len(res.Rows)-1][0] != "200" {
		t.Errorf("500 rows under the row cap: %d rows, truncated %v by %q", res.RowCount, res.Truncated, res.TruncatedBy)
	}
	res, err = f.query(t, `SELECT repeat('x', 1000) FROM generate_series(1, 100)`)
	if err != nil {
		t.Fatal(err)
	}
	if res.RowCount < 1 || res.RowCount >= 10 || !res.Truncated || res.TruncatedBy != "bytes" {
		t.Errorf("100 x 1000-byte rows under a 10000-byte cap: %d rows, truncated %v by %q", res.RowCount, res.Truncated, res.TruncatedBy)
	}
	// Under both caps: not truncated.
	res, err = f.query(t, `SELECT g FROM generate_series(1, 7) g`)
	if err != nil || res.RowCount != 7 || res.Truncated {
		t.Errorf("7 rows: %d rows, truncated %v, err %v", res.RowCount, res.Truncated, err)
	}
}

// TestAPostgresTransactionControlIsRefusedAndLeavesNoSessionState: a COMMIT
// that ends the executor's transaction is refused, and a session SET does not
// survive into the next query on the pool.
func TestAPostgresTransactionControlIsRefusedAndLeavesNoSessionState(t *testing.T) {
	f := newPGFixture(t, connectors.PostgresSettings{})
	for _, sql := range []string{`COMMIT`, `ROLLBACK`} {
		if _, err := f.query(t, sql); pgClass(err) != "refused" {
			t.Errorf("%s: got %v, want the transaction-control refusal", sql, err)
		}
	}
	if _, err := f.query(t, `SET application_name = 'agent-was-here'`); err != nil {
		t.Fatalf("SET: %v", err)
	}
	// The pool holds two connections: ask more than twice.
	for i := 0; i < 4; i++ {
		res, err := f.query(t, `SELECT current_setting('application_name'), current_setting('transaction_read_only')`)
		if err != nil {
			t.Fatal(err)
		}
		if got := *res.Rows[0][0] + "|" + *res.Rows[0][1]; got != "muster-connector|on" {
			t.Errorf("query %d after a session SET: application_name|read_only = %s", i, got)
		}
	}
}

// TestAPostgresQueryAuditCarriesNoParamOrPassword: one audit row and one log
// line per query, with the SQL's fingerprint and prefix, rows and class —
// and no parameter value or password anywhere.
func TestAPostgresQueryAuditCarriesNoParamOrPassword(t *testing.T) {
	f := newPGFixture(t, connectors.PostgresSettings{})
	const sentinel = "param-sentinel-7f3a"
	if _, err := f.query(t, `SELECT $1::text AS v FROM generate_series(1, 3)`, sentinel); err != nil {
		t.Fatal(err)
	}
	audit, err := f.store.ListAudit(f.ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	var q []connectors.AuditEntry
	for _, e := range audit {
		if e.Kind == "query" {
			q = append(q, e)
		}
	}
	if len(q) != 1 {
		t.Fatalf("query audit rows = %d, want 1 (%+v)", len(q), audit)
	}
	e := q[0]
	if e.AgentName != "pg-agent" || e.Connector != "pgprobe" || e.Status != 3 ||
		!strings.HasPrefix(e.Path, "sha256:") || !strings.Contains(e.Path, "SELECT $1::text AS v FROM generate_series(1, 3)") ||
		!strings.Contains(e.Detail, "rows=3") || !strings.Contains(e.Detail, "class=ok") {
		t.Errorf("audit row = %+v", e)
	}
	logs := f.logs.String()
	if !strings.Contains(logs, "postgres query agent=\"pg-agent\" connector=\"pgprobe\"") {
		t.Errorf("no query log line: %s", logs)
	}
	for _, leak := range []string{sentinel, pgTestPassword} {
		if strings.Contains(logs, leak) || strings.Contains(e.Path+e.Detail+e.Host, leak) {
			t.Errorf("%q appears in the audit or log", leak)
		}
	}
}

// TestAPostgresConnectErrorCarriesNoPassword: a database that refuses the
// password answers an auth error naming neither the password nor a DSN.
func TestAPostgresConnectErrorCarriesNoPassword(t *testing.T) {
	f := newPGFixture(t, connectors.PostgresSettings{})
	if err := f.store.RotateSecret(f.ctx, "pgprobe", "wrong-"+pgTestPassword); err != nil {
		t.Fatal(err)
	}
	_, err := f.query(t, `SELECT 1`)
	if err == nil {
		t.Fatal("a wrong password connected")
	}
	if pgClass(err) != "auth" && pgClass(err) != "connect" {
		t.Errorf("class = %q (%v), want auth or connect", pgClass(err), err)
	}
	for _, leak := range []string{pgTestPassword, "password="} {
		if strings.Contains(err.Error(), leak) || strings.Contains(f.logs.String(), leak) {
			t.Errorf("%q leaked: err %q, logs %q", leak, err, f.logs.String())
		}
	}
}

// TestAPostgresConnectorsSettingsAreBoundToItsPassword: pointing a stored
// connector at another database out of band makes its password fail to open.
func TestAPostgresConnectorsSettingsAreBoundToItsPassword(t *testing.T) {
	f := newPGFixture(t, connectors.PostgresSettings{})
	if _, _, err := f.store.Credential(f.ctx, "pgprobe"); err != nil {
		t.Fatalf("control: the untouched connector does not open: %v", err)
	}
	dbtest.OwnerExec(t, f.dsn, `UPDATE connectors SET settings = jsonb_set(settings, '{database}', '"postgres"') WHERE name = 'pgprobe'`)
	if _, _, err := f.store.Credential(f.ctx, "pgprobe"); !errors.Is(err, connectors.ErrDecrypt) {
		t.Errorf("after the database was edited out of band: %v, want ErrDecrypt", err)
	}
}

// TestTheHTTPProxyRefusesAPostgresConnector: an approved agent asking the
// HTTP proxy for a postgres connector is refused before anything is sent.
func TestTheHTTPProxyRefusesAPostgresConnector(t *testing.T) {
	f := newPGFixture(t, connectors.PostgresSettings{})
	g, _, err := f.store.RequestGrant(f.ctx, f.caller.AgentID, "pgprobe", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DecideGrant(f.ctx, g.ID, true, "operator", connectors.SourceStored); err != nil {
		t.Fatal(err)
	}
	sent := false
	p := &connectors.Proxy{Store: f.store}
	p.SetTransport(roundTripFunc(func(*http.Request) (*http.Response, error) { sent = true; return nil, errors.New("no") }))
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/connect/pgprobe/x/y", nil).WithContext(f.ctx)
	p.Serve(w, r, f.caller, "pgprobe", f.target.Host+":"+strconv.Itoa(int(f.target.Port)), "y")
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "postgres_query") || sent {
		t.Errorf("proxy for a postgres connector: %d %s (sent upstream: %v)", w.Code, w.Body, sent)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestThePostgresRateLimitIsPerAgent: past the burst an agent is refused until
// its bucket refills; another agent is not affected. (No database: the
// limiter runs before the connector is read, and an empty SQL stops there.)
func TestThePostgresRateLimitIsPerAgent(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	e := &connectors.PostgresExecutor{RatePerMinute: 60, Burst: 3, Now: func() time.Time { return now }}
	ctx := authz.Operator(context.Background())
	call := func(agent int64) error {
		_, err := e.Query(ctx, connectors.Caller{AgentID: agent}, "pg", connectors.PGQuery{SQL: " "})
		return err
	}
	for i := 0; i < 3; i++ {
		if err := call(1); !errors.Is(err, connectors.ErrInvalid) {
			t.Fatalf("call %d within the burst: %v, want the empty-SQL refusal", i, err)
		}
	}
	if err := call(1); !errors.Is(err, connectors.ErrRateLimited) {
		t.Errorf("past the burst: %v, want ErrRateLimited", err)
	}
	if err := call(2); !errors.Is(err, connectors.ErrInvalid) {
		t.Errorf("another agent: %v, want its own bucket", err)
	}
	now = now.Add(time.Second) // one token at 60/min
	if err := call(1); !errors.Is(err, connectors.ErrInvalid) {
		t.Errorf("after a refill: %v", err)
	}
	if err := call(1); !errors.Is(err, connectors.ErrRateLimited) {
		t.Errorf("after spending the refill: %v", err)
	}
}

// TestAPostgresConnectorValidates: the settings' refusals.
func TestAPostgresConnectorValidates(t *testing.T) {
	ok := func() connectors.NewConnector {
		return connectors.NewConnector{Name: "db", Scheme: connectors.SchemePostgres, Hosts: []string{"db.internal"},
			Username: "ro", Secret: "a-long-enough-password", Postgres: &connectors.PostgresSettings{Database: "app", RequireTLS: true}}
	}
	nc, err := ok().Validate()
	if err != nil {
		t.Fatalf("a valid postgres connector: %v", err)
	}
	if nc.Hosts[0] != "db.internal:5432" || nc.Postgres.SSLMode != "require" || nc.Platform != "postgres" {
		t.Errorf("normalised = %v %q %q", nc.Hosts, nc.Postgres.SSLMode, nc.Platform)
	}
	for name, mut := range map[string]func(*connectors.NewConnector){
		"no settings":          func(n *connectors.NewConnector) { n.Postgres = nil },
		"two hosts":            func(n *connectors.NewConnector) { n.Hosts = []string{"a.internal", "b.internal"} },
		"a URL host":           func(n *connectors.NewConnector) { n.Hosts = []string{"postgres://u:p@db/app"} },
		"bad port":             func(n *connectors.NewConnector) { n.Hosts = []string{"db:99999"} },
		"no user":              func(n *connectors.NewConnector) { n.Username = "" },
		"quote in user":        func(n *connectors.NewConnector) { n.Username = "ro' password=x" },
		"no database":          func(n *connectors.NewConnector) { n.Postgres.Database = "" },
		"unknown sslmode":      func(n *connectors.NewConnector) { n.Postgres.SSLMode = "allow" },
		"tls with disable":     func(n *connectors.NewConnector) { n.Postgres.SSLMode = "disable" },
		"timeout over the cap": func(n *connectors.NewConnector) { n.Postgres.StatementTimeoutMS = 60_000 },
		"rows over the cap":    func(n *connectors.NewConnector) { n.Postgres.MaxRows = 5000 },
		"short secret":         func(n *connectors.NewConnector) { n.Secret = "short" },
	} {
		n := ok()
		ps := *n.Postgres
		n.Postgres = &ps
		mut(&n)
		if _, err := n.Validate(); !errors.Is(err, connectors.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	bearer := connectors.NewConnector{Name: "x", Scheme: connectors.SchemeBearer, Hosts: []string{"api.example.com"},
		Secret: "a-long-enough-secret", Postgres: &connectors.PostgresSettings{Database: "app"}}
	if _, err := bearer.Validate(); !errors.Is(err, connectors.ErrInvalid) {
		t.Errorf("postgres settings on a bearer connector: %v, want ErrInvalid", err)
	}
}
