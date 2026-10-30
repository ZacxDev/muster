package connectors

// The postgres connector kind: read-only SQL an agent runs against a database
// WITHOUT EVER HOLDING ITS CREDENTIAL.
//
// 🔴 muster-server MAKES THE CONNECTION, NOT THE POD. The password is sealed
// like every connector secret (seal.go; the database, sslmode and TLS
// requirement are bound into its additional data alongside the host and user),
// opened only by [Store.Credential] here, and spent on a connection pool that
// lives in this process. An agent sends SQL text and gets back columns and rows
// as JSON; no route, tool result, log line, audit row or error string carries
// the password or a DSN (redactPG scrubs every error that leaves).
//
// 🔴 THE GUARDS ARE muster-server's, WHATEVER THE DATABASE ROLE ALLOWS. Every
// call, on a pooled connection:
//
//  1. a per-agent rate limit (token bucket, in memory);
//  2. BEGIN READ ONLY (pgBeginReadOnly) — a write is SQLSTATE 25006;
//  3. SET LOCAL statement_timeout / lock_timeout /
//     idle_in_transaction_session_timeout (the connector's timeout, capped at
//     PGMaxStatementTimeout);
//  4. the agent's SQL as ONE extended-protocol Parse/Bind/Execute — never the
//     simple protocol, so a string holding two statements is refused by the
//     server's parser ("cannot insert multiple commands into a prepared
//     statement", 42601) before anything runs;
//  5. a row cap and a byte cap on what is returned (truncated is set when
//     either cuts the result);
//  6. ROLLBACK, always — and a connection whose transaction the statement
//     ended (COMMIT, ROLLBACK) is closed instead of returned to the pool, so
//     no session state an agent set can outlive its own call;
//  7. a pool of at most PGPoolMaxConns connections per connector.
//
// The database role is the second wall, not the first: docs/connectors.md
// recommends a LOGIN role with no table grants, CONNECTION LIMIT 2, a 5s
// statement_timeout and default_transaction_read_only on.

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/authz"
)

// The postgres connector's hard limits and defaults. A connector may lower a
// cap (PostgresSettings); nothing raises one past these.
const (
	PGDefaultPort             = 5432
	PGDefaultStatementTimeout = 5 * time.Second
	PGMinStatementTimeout     = 100 * time.Millisecond
	PGMaxStatementTimeout     = 30 * time.Second
	// PGLockTimeout is lock_timeout, or the statement timeout when that is
	// shorter.
	PGLockTimeout     = 2 * time.Second
	PGDefaultMaxRows  = 200
	PGMaxRowsCap      = 1000
	PGDefaultMaxBytes = 256 << 10
	PGMinMaxBytes     = 1 << 10
	PGMaxBytesCap     = 1 << 20
	// PGPoolMaxConns is the per-connector connection pool's size.
	PGPoolMaxConns  = 2
	PGMaxSQLBytes   = 16 << 10
	PGMaxParams     = 64
	PGMaxParamBytes = 8 << 10
	// PGDefaultRatePerMinute and PGDefaultBurst are the per-agent token
	// bucket: a sustained rate and how many calls may arrive at once.
	PGDefaultRatePerMinute = 30
	PGDefaultBurst         = 10

	pgAcquireTimeout = 5 * time.Second
	pgConnectTimeout = 5
	pgAppName        = "muster-connector"
)

// pgBeginReadOnly opens every query's transaction. 🔴 It is THE read-only
// guard; TestAPostgresWriteIsRefusedInsideTheReadOnlyTransaction pins it.
const pgBeginReadOnly = "BEGIN" // MUTANT: READ ONLY removed

// PGSSLModes are the sslmode values a postgres connector may use. allow is
// left out (it tries plaintext first); verify-ca and verify-full verify against
// the system roots — muster-server holds no client certificate or root file.
var PGSSLModes = []string{"disable", "prefer", "require", "verify-ca", "verify-full"}

// PostgresSettings is a postgres connector's non-secret configuration.
type PostgresSettings struct {
	Database string `json:"database"`
	SSLMode  string `json:"sslmode"`
	// RequireTLS refuses a connection that is not TLS, whatever the sslmode
	// negotiated (checked on the live connection after it is made).
	RequireTLS bool `json:"require_tls"`
	// StatementTimeoutMS is statement_timeout in milliseconds; 0 is
	// PGDefaultStatementTimeout. Validated within
	// [PGMinStatementTimeout, PGMaxStatementTimeout].
	StatementTimeoutMS int `json:"statement_timeout_ms,omitempty"`
	// MaxRows and MaxBytes cap one result; 0 is the default.
	MaxRows  int `json:"max_rows,omitempty"`
	MaxBytes int `json:"max_bytes,omitempty"`
}

// StatementTimeout is the effective statement_timeout, clamped to the caps.
func (p PostgresSettings) StatementTimeout() time.Duration {
	d := time.Duration(p.StatementTimeoutMS) * time.Millisecond
	switch {
	case d <= 0:
		return PGDefaultStatementTimeout
	case d < PGMinStatementTimeout:
		return PGMinStatementTimeout
	case d > PGMaxStatementTimeout:
		return PGMaxStatementTimeout
	}
	return d
}

// LockTimeout is the effective lock_timeout.
func (p PostgresSettings) LockTimeout() time.Duration {
	return min(PGLockTimeout, p.StatementTimeout())
}

// RowCap is the effective row cap.
func (p PostgresSettings) RowCap() int {
	if p.MaxRows <= 0 || p.MaxRows > PGMaxRowsCap {
		return min(PGDefaultMaxRows, PGMaxRowsCap)
	}
	return p.MaxRows
}

// ByteCap is the effective byte cap.
func (p PostgresSettings) ByteCap() int {
	if p.MaxBytes <= 0 || p.MaxBytes > PGMaxBytesCap {
		return PGDefaultMaxBytes
	}
	return max(p.MaxBytes, PGMinMaxBytes)
}

// pgIdentRE is the shape accepted for a database and user name: plain
// identifiers, which keeps them out of any quoting question.
var pgIdentRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,62}$`)

// normalizePGHost validates a postgres connector's host[:port] and returns it
// lowercased with the port made explicit. Unlike an HTTP connector's host
// (NormalizeHost) a single-label name (an in-namespace Service) and an IP
// literal are accepted: no private-range rule applies, because the operator
// who creates it (an admin) names the one database it reaches.
func normalizePGHost(h string) (string, error) {
	h = strings.ToLower(strings.TrimSpace(h))
	host, port := h, strconv.Itoa(PGDefaultPort)
	if strings.Contains(h, ":") {
		var err error
		if host, port, err = net.SplitHostPort(h); err != nil {
			if ip := net.ParseIP(h); ip != nil { // a bare IPv6 literal
				host, port = h, strconv.Itoa(PGDefaultPort)
			} else {
				return "", fmt.Errorf("%w: database host %q is not host or host:port", ErrInvalid, h)
			}
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return "", fmt.Errorf("%w: database host %q has an invalid port", ErrInvalid, h)
	}
	if net.ParseIP(host) == nil {
		if host == "" || len(host) > 253 {
			return "", fmt.Errorf("%w: database host %q is not a hostname", ErrInvalid, h)
		}
		for _, l := range strings.Split(host, ".") {
			if !hostLabelRE.MatchString(l) {
				return "", fmt.Errorf("%w: database host %q is not a valid hostname (no URLs, no wildcards)", ErrInvalid, h)
			}
		}
	}
	return net.JoinHostPort(host, port), nil
}

// validatePostgres is Validate for SchemePostgres.
func (nc NewConnector) validatePostgres() (NewConnector, error) {
	if nc.Postgres == nil {
		return nc, fmt.Errorf("%w: a postgres connector needs its database settings", ErrInvalid)
	}
	var hosts []string
	for _, h := range nc.Hosts {
		if strings.TrimSpace(h) != "" {
			hosts = append(hosts, h)
		}
	}
	if len(hosts) != 1 {
		return nc, fmt.Errorf("%w: a postgres connector names exactly one database host (host or host:port)", ErrInvalid)
	}
	h, err := normalizePGHost(hosts[0])
	if err != nil {
		return nc, err
	}
	nc.Hosts = []string{h}
	nc.HeaderName = ""
	nc.Username = strings.TrimSpace(nc.Username)
	if !pgIdentRE.MatchString(nc.Username) {
		return nc, fmt.Errorf("%w: database user %q must be letters, digits, '_', '.' or '-', at most 63 characters", ErrInvalid, nc.Username)
	}
	ps := *nc.Postgres
	ps.Database = strings.TrimSpace(ps.Database)
	if !pgIdentRE.MatchString(ps.Database) {
		return nc, fmt.Errorf("%w: database name %q must be letters, digits, '_', '.' or '-', at most 63 characters", ErrInvalid, ps.Database)
	}
	ps.SSLMode = strings.ToLower(strings.TrimSpace(ps.SSLMode))
	if ps.SSLMode == "" {
		ps.SSLMode = "require"
	}
	known := false
	for _, m := range PGSSLModes {
		known = known || m == ps.SSLMode
	}
	if !known {
		return nc, fmt.Errorf("%w: sslmode %q is not one of %s", ErrInvalid, ps.SSLMode, strings.Join(PGSSLModes, ", "))
	}
	if ps.RequireTLS && ps.SSLMode == "disable" {
		return nc, fmt.Errorf("%w: require_tls cannot be combined with sslmode disable", ErrInvalid)
	}
	if ps.StatementTimeoutMS != 0 {
		d := time.Duration(ps.StatementTimeoutMS) * time.Millisecond
		if d < PGMinStatementTimeout || d > PGMaxStatementTimeout {
			return nc, fmt.Errorf("%w: statement timeout %dms is not within %s..%s", ErrInvalid, ps.StatementTimeoutMS,
				PGMinStatementTimeout, PGMaxStatementTimeout)
		}
	}
	if ps.MaxRows < 0 || ps.MaxRows > PGMaxRowsCap {
		return nc, fmt.Errorf("%w: max rows %d is not within 0..%d", ErrInvalid, ps.MaxRows, PGMaxRowsCap)
	}
	if ps.MaxBytes != 0 && (ps.MaxBytes < PGMinMaxBytes || ps.MaxBytes > PGMaxBytesCap) {
		return nc, fmt.Errorf("%w: max bytes %d is not within %d..%d", ErrInvalid, ps.MaxBytes, PGMinMaxBytes, PGMaxBytesCap)
	}
	nc.Postgres = &ps
	if nc.Platform == "" || nc.Platform == "custom" {
		nc.Platform = "postgres"
	}
	if err := ValidateSecret(nc.Secret); err != nil {
		return nc, err
	}
	return nc, nil
}

// --- redaction ----------------------------------------------------------------------

var (
	pgURLCredRE  = regexp.MustCompile(`(?i)(postgres(?:ql)?://)[^\s/@]*@`)
	pgPasswordRE = regexp.MustCompile(`(?i)(password\s*=\s*)('(?:[^'\\]|\\.)*'|"(?:[^"\\]|\\.)*"|\S+)`)
)

// redactPG returns s with every DSN credential form removed: the secret
// itself wherever it appears, a URL's user:password@, and a keyword DSN's
// password=… value. 🔴 EVERY STRING THAT LEAVES THE EXECUTOR GOES THROUGH IT —
// error messages, the audit line and the log line.
func redactPG(s string, secrets ...string) string {
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "[redacted]")
		}
	}
	s = pgURLCredRE.ReplaceAllString(s, "${1}[redacted]@")
	return pgPasswordRE.ReplaceAllString(s, "${1}[redacted]")
}

// RedactPG is redactPG for callers outside the package (the API's last-resort
// error text).
func RedactPG(s string, secrets ...string) string { return redactPG(s, secrets...) }

// --- the executor -------------------------------------------------------------------

// ErrRateLimited is a postgres query refused by the per-agent rate limit.
var ErrRateLimited = errors.New("connectors: too many queries from this agent; wait a few seconds and try again")

// ErrNotPostgres is a postgres query naming a connector of another scheme.
var ErrNotPostgres = fmt.Errorf("%w: that connector is not a postgres connector", ErrInvalid)

// PGQuery is one agent query: a single SQL statement and its positional
// parameters ($1, $2, …) as JSON values.
type PGQuery struct {
	SQL    string
	Params []json.RawMessage
}

// PGColumn is one result column: its name and its Postgres type name.
type PGColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// PGResult is one query's answer. Every value is Postgres's TEXT
// representation (null is JSON null).
type PGResult struct {
	Columns  []PGColumn  `json:"columns"`
	Rows     [][]*string `json:"rows"`
	RowCount int         `json:"row_count"`
	// Truncated is true when the row or byte cap cut the result; TruncatedBy
	// says which ("rows" or "bytes").
	Truncated   bool   `json:"truncated"`
	TruncatedBy string `json:"truncated_by,omitempty"`
	MaxRows     int    `json:"max_rows"`
	MaxBytes    int    `json:"max_bytes"`
	Command     string `json:"command,omitempty"`
	DurationMS  int64  `json:"duration_ms"`
}

// PGError is a refusal or a database error, carrying its class for the audit.
// Its message has been through redactPG.
type PGError struct {
	Class    string `json:"class"`
	SQLState string `json:"sqlstate,omitempty"`
	Message  string `json:"error"`
}

func (e *PGError) Error() string { return e.Message }

// PostgresExecutor runs agent queries against postgres connectors.
type PostgresExecutor struct {
	Store  Store
	Logger *log.Logger
	// RatePerMinute and Burst set the per-agent token bucket (0: the
	// defaults).
	RatePerMinute, Burst int
	// Now is the limiter's clock (tests); nil is time.Now.
	Now func() time.Time

	mu      sync.Mutex
	pools   map[int64]*pgPoolEntry
	buckets map[int64]*pgBucket
}

type pgPoolEntry struct {
	version string
	pool    *pgxpool.Pool
}

type pgBucket struct {
	tokens float64
	at     time.Time
}

// Close closes every pool.
func (e *PostgresExecutor) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, ent := range e.pools {
		ent.pool.Close()
		delete(e.pools, id)
	}
}

func (e *PostgresExecutor) logf(format string, args ...any) {
	if e.Logger != nil {
		e.Logger.Printf(format, args...)
	}
}

func (e *PostgresExecutor) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// allow takes one token from agent's bucket.
func (e *PostgresExecutor) allow(agent int64) bool {
	rate, burst := e.RatePerMinute, e.Burst
	if rate <= 0 {
		rate = PGDefaultRatePerMinute
	}
	if burst <= 0 {
		burst = PGDefaultBurst
	}
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.buckets == nil {
		e.buckets = map[int64]*pgBucket{}
	}
	b, ok := e.buckets[agent]
	if !ok {
		b = &pgBucket{tokens: float64(burst), at: now}
		e.buckets[agent] = b
	}
	b.tokens = min(float64(burst), b.tokens+now.Sub(b.at).Minutes()*float64(rate))
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Query runs q against connector for caller. The caller must already hold an
// approved grant: the executor decides HOW a query runs, the grant check (the
// api package, through Proxy.CheckAccess) decides WHETHER.
//
// An error is a *PGError (its Message safe to show the agent), ErrInvalid
// (a bad request), ErrRateLimited, ErrNotFound or ErrDecrypt.
func (e *PostgresExecutor) Query(ctx context.Context, caller Caller, connector string, q PGQuery) (PGResult, error) {
	if !e.allow(caller.AgentID) {
		e.logf("connectors: postgres query agent=%q connector=%q refused: rate limited", caller.AgentName, connector)
		return PGResult{}, ErrRateLimited
	}
	if !authz.IsAdmin(ctx) {
		// A postgres connector is an admin's (CreateConnector); one left behind
		// by a demoted owner is not served.
		return PGResult{}, &PGError{Class: "refused", Message: "postgres connectors serve admins' agents only"}
	}
	sql := q.SQL
	if strings.TrimSpace(sql) == "" {
		return PGResult{}, fmt.Errorf("%w: sql is empty", ErrInvalid)
	}
	if len(sql) > PGMaxSQLBytes || strings.ContainsRune(sql, 0) || !utf8.ValidString(sql) {
		return PGResult{}, fmt.Errorf("%w: sql must be valid UTF-8 without NUL, at most %d bytes", ErrInvalid, PGMaxSQLBytes)
	}
	params, err := pgParams(q.Params)
	if err != nil {
		return PGResult{}, err
	}
	c, secret, err := e.Store.Credential(ctx, connector)
	if err != nil {
		return PGResult{}, err
	}
	if c.Scheme != SchemePostgres || c.Postgres == nil || len(c.Hosts) != 1 {
		return PGResult{}, ErrNotPostgres
	}
	start := time.Now()
	res, perr := e.run(ctx, c, secret, sql, params)
	res.DurationMS = time.Since(start).Milliseconds()
	class := "ok"
	if perr != nil {
		class = perr.Class
	}
	e.audit(ctx, caller, c, sql, time.Since(start), res.RowCount, res.Truncated, class)
	if perr != nil {
		return PGResult{}, perr
	}
	return res, nil
}

// pgParams converts JSON parameters to Postgres text values.
func pgParams(in []json.RawMessage) ([][]byte, error) {
	if len(in) > PGMaxParams {
		return nil, fmt.Errorf("%w: at most %d params", ErrInvalid, PGMaxParams)
	}
	out := make([][]byte, len(in))
	for i, raw := range in {
		raw = json.RawMessage(strings.TrimSpace(string(raw)))
		if len(raw) > PGMaxParamBytes {
			return nil, fmt.Errorf("%w: param %d is longer than %d bytes", ErrInvalid, i+1, PGMaxParamBytes)
		}
		switch {
		case len(raw) == 0 || string(raw) == "null":
			out[i] = nil
		case raw[0] == '"':
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				return nil, fmt.Errorf("%w: param %d is not valid JSON", ErrInvalid, i+1)
			}
			if strings.ContainsRune(s, 0) {
				return nil, fmt.Errorf("%w: param %d contains NUL", ErrInvalid, i+1)
			}
			out[i] = []byte(s)
		default:
			if !json.Valid(raw) {
				return nil, fmt.Errorf("%w: param %d is not valid JSON", ErrInvalid, i+1)
			}
			// numbers, booleans, objects and arrays: their JSON text is the
			// Postgres text form (true/false, 12.5, {"a":1}).
			out[i] = []byte(raw)
		}
	}
	return out, nil
}

// pgVersion is what a connector's pool is keyed on besides its id: any change
// to where or how it connects (a rotation included) builds a new pool.
func pgVersion(c Connector) string {
	settings, _ := json.Marshal(c.Postgres)
	rot := ""
	if c.RotatedAt != nil {
		rot = c.RotatedAt.UTC().Format(time.RFC3339Nano)
	}
	return strings.Join([]string{c.UpdatedAt.UTC().Format(time.RFC3339Nano), rot, strings.Join(c.Hosts, ","), c.Username,
		string(settings)}, "|")
}

// errNoTLS is the RequireTLS refusal of a plaintext connection.
var errNoTLS = errors.New("the database connection is not TLS, and this connector requires TLS")

// pgPoolConfig builds the connector's pool configuration. The password is set
// on the parsed config, never written into a connection string.
func pgPoolConfig(c Connector, secret string) (*pgxpool.Config, error) {
	host, port, err := net.SplitHostPort(c.Hosts[0])
	if err != nil {
		return nil, err
	}
	ps := *c.Postgres
	// Every value is validated (pgIdentRE, normalizePGHost, PGSSLModes), so no
	// quoting is needed; the quotes are belt and braces.
	cs := fmt.Sprintf("host='%s' port=%s dbname='%s' user='%s' sslmode=%s connect_timeout=%d application_name=%s",
		host, port, ps.Database, c.Username, ps.SSLMode, pgConnectTimeout, pgAppName)
	cfg, err := pgxpool.ParseConfig(cs)
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.Password = secret
	cfg.ConnConfig.RuntimeParams = map[string]string{"application_name": pgAppName}
	cfg.MaxConns = PGPoolMaxConns
	cfg.MinConns = 0
	cfg.MaxConnIdleTime = time.Minute
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.HealthCheckPeriod = time.Minute
	if ps.RequireTLS {
		// sslmode prefer falls back to plaintext: drop those fallbacks, and
		// check the live connection whatever was negotiated.
		var fb []*pgconn.FallbackConfig
		for _, f := range cfg.ConnConfig.Fallbacks {
			if f.TLSConfig != nil {
				fb = append(fb, f)
			}
		}
		cfg.ConnConfig.Fallbacks = fb
		if cfg.ConnConfig.TLSConfig == nil && len(fb) == 0 {
			return nil, errNoTLS
		}
		cfg.AfterConnect = func(_ context.Context, conn *pgx.Conn) error {
			if _, ok := conn.PgConn().Conn().(*tls.Conn); !ok {
				return errNoTLS
			}
			return nil
		}
	}
	return cfg, nil
}

// poolFor returns the connector's pool, building it (lazily: no connection is
// made until the first query) when there is none for its current version.
func (e *PostgresExecutor) poolFor(c Connector, secret string) (*pgxpool.Pool, error) {
	ver := pgVersion(c)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pools == nil {
		e.pools = map[int64]*pgPoolEntry{}
	}
	if ent, ok := e.pools[c.ID]; ok {
		if ent.version == ver {
			return ent.pool, nil
		}
		go ent.pool.Close() // waits for its in-flight queries
		delete(e.pools, c.ID)
	}
	cfg, err := pgPoolConfig(c, secret)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return nil, err
	}
	e.pools[c.ID] = &pgPoolEntry{version: ver, pool: pool}
	return pool, nil
}

// classify maps an error to its audit class and a message safe for the agent.
func classify(err error, secret string) *PGError {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		class := "sql_" + pgErr.Code
		switch {
		case pgErr.Code == "57014":
			class = "timeout"
		case pgErr.Code == "55P03":
			class = "lock_timeout"
		case pgErr.Code == "25006":
			class = "read_only"
		case pgErr.Code == "42601":
			class = "syntax"
		case pgErr.Code == "42501":
			class = "permission"
		case strings.HasPrefix(pgErr.Code, "28"):
			class = "auth"
		}
		msg := pgErr.Severity + ": " + pgErr.Message + " (SQLSTATE " + pgErr.Code + ")"
		if pgErr.Detail != "" {
			msg += " DETAIL: " + pgErr.Detail
		}
		if pgErr.Hint != "" {
			msg += " HINT: " + pgErr.Hint
		}
		return &PGError{Class: class, SQLState: pgErr.Code, Message: redactPG(msg, secret)}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &PGError{Class: "timeout", Message: "the query did not finish within its time limit"}
	}
	if errors.Is(err, errNoTLS) {
		return &PGError{Class: "tls", Message: errNoTLS.Error()}
	}
	return &PGError{Class: "connect", Message: "could not reach the database: " + redactPG(err.Error(), secret)}
}

// run executes one statement under every guard.
func (e *PostgresExecutor) run(ctx context.Context, c Connector, secret, sql string, params [][]byte) (PGResult, *PGError) {
	ps := *c.Postgres
	res := PGResult{MaxRows: ps.RowCap(), MaxBytes: ps.ByteCap(), Rows: [][]*string{}, Columns: []PGColumn{}}
	pool, err := e.poolFor(c, secret)
	if err != nil {
		e.logf("connectors: postgres connector %q: pool: %s", c.Name, redactPG(err.Error(), secret))
		return res, classify(err, secret)
	}
	timeout := ps.StatementTimeout()
	actx, cancel := context.WithTimeout(ctx, pgAcquireTimeout)
	conn, err := pool.Acquire(actx)
	cancel()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return res, &PGError{Class: "busy", Message: fmt.Sprintf("the connector's %d connections stayed busy for %s; try again",
				PGPoolMaxConns, pgAcquireTimeout)}
		}
		e.logf("connectors: postgres connector %q: connect: %s", c.Name, redactPG(err.Error(), secret))
		return res, classify(err, secret)
	}
	// keep is cleared whenever the connection's state is no longer known to
	// be clean: it is then closed rather than handed to the next query.
	keep := true
	defer func() {
		if !keep {
			cctx, ccancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
			_ = conn.Conn().Close(cctx)
			ccancel()
		}
		conn.Release()
	}()
	pc := conn.Conn().PgConn()
	// The client-side deadline backs the server's statement_timeout up.
	qctx, qcancel := context.WithTimeout(ctx, timeout+2*time.Second)
	defer qcancel()

	if _, err := pc.Exec(qctx, pgBeginReadOnly).ReadAll(); err != nil {
		keep = false
		return res, classify(err, secret)
	}
	rollback := func() {
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer rcancel()
		if _, err := pc.Exec(rctx, "ROLLBACK").ReadAll(); err != nil {
			keep = false
		}
	}
	setLocal := fmt.Sprintf("SET LOCAL statement_timeout = %d; SET LOCAL lock_timeout = %d; "+
		"SET LOCAL idle_in_transaction_session_timeout = %d",
		timeout.Milliseconds(), ps.LockTimeout().Milliseconds(), (timeout + 5*time.Second).Milliseconds())
	if _, err := pc.Exec(qctx, setLocal).ReadAll(); err != nil {
		rollback()
		keep = false
		return res, classify(err, secret)
	}

	// 🔴 THE EXTENDED PROTOCOL, AND ONLY IT: one unnamed Parse/Bind/Execute.
	// Text formats both ways (nil format slices), server-inferred param types.
	rr := pc.ExecParams(qctx, sql, params, nil, nil, nil)
	tm := pgtype.NewMap()
	for _, f := range rr.FieldDescriptions() {
		typ := strconv.FormatUint(uint64(f.DataTypeOID), 10)
		if t, ok := tm.TypeForOID(f.DataTypeOID); ok {
			typ = t.Name
		}
		res.Columns = append(res.Columns, PGColumn{Name: f.Name, Type: typ})
	}
	used := 0
	for _, col := range res.Columns {
		used += len(col.Name) + len(col.Type)
	}
	for rr.NextRow() {
		if res.Truncated {
			continue // drained, not kept: bounded by statement_timeout
		}
		vals := rr.Values()
		size := 0
		for _, v := range vals {
			size += len(v) + 1
		}
		switch {
		case res.RowCount >= res.MaxRows:
			res.Truncated, res.TruncatedBy = true, "rows"
			continue
		case used+size > res.MaxBytes:
			res.Truncated, res.TruncatedBy = true, "bytes"
			continue
		}
		used += size
		row := make([]*string, len(vals))
		for i, v := range vals {
			if v != nil {
				s := string(v)
				row[i] = &s
			}
		}
		res.Rows = append(res.Rows, row)
		res.RowCount++
	}
	tag, qerr := rr.Close()
	res.Command = tag.String()
	switch st := pc.TxStatus(); {
	case pc.IsClosed():
		keep = false
	case st == 'I':
		// The statement ended our transaction (COMMIT, ROLLBACK, END): refuse it,
		// and do not reuse a connection whose session the agent then owned.
		keep = false
		if qerr == nil {
			return PGResult{}, &PGError{Class: "refused", Message: "transaction control statements (COMMIT, ROLLBACK, …) " +
				"are refused: every query already runs in its own read-only transaction"}
		}
	default:
		rollback()
	}
	if qerr != nil {
		return PGResult{}, classify(qerr, secret)
	}
	return res, nil
}

// sqlFingerprint is the audit's handle on a statement: the first 16 hex
// characters of the sha256 of its whitespace-collapsed text, and its first 120
// characters.
func sqlFingerprint(sql string) (string, string) {
	norm := strings.Join(strings.Fields(sql), " ")
	sum := sha256.Sum256([]byte(norm))
	prefix := norm
	if utf8.RuneCountInString(prefix) > 120 {
		r := []rune(prefix)
		prefix = string(r[:120]) + "…"
	}
	return "sha256:" + hex.EncodeToString(sum[:8]), prefix
}

// audit records one query: who, which connector, the statement's fingerprint
// and prefix, how long, how many rows and the outcome class. 🔴 NEVER a
// parameter value, never the password, never a DSN.
func (e *PostgresExecutor) audit(ctx context.Context, caller Caller, c Connector, sql string, d time.Duration, rows int,
	truncated bool, class string) {
	fp, prefix := sqlFingerprint(sql)
	prefix = redactPG(prefix)
	detail := fmt.Sprintf("rows=%d truncated=%t duration=%dms class=%s", rows, truncated, d.Milliseconds(), class)
	e.logf("connectors: postgres query agent=%q connector=%q sql=%s %q %s", caller.AgentName, c.Name, fp, prefix, detail)
	if err := e.Store.Audit(context.WithoutCancel(ctx), AuditEntry{Kind: "query", AgentID: caller.AgentID,
		AgentName: caller.AgentName, Connector: c.Name, Method: "SQL", Host: c.Hosts[0], Path: fp + " " + prefix,
		Status: rows, Detail: detail}); err != nil {
		e.logf("connectors: audit postgres query on %q: %v", c.Name, err)
	}
}
