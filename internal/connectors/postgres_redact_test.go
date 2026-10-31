package connectors

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestAPostgresErrorIsRedactedOfItsDSN: a DSN injected into an error — as a
// URL, as keyword pairs, as the bare password, inside a server error's message
// and detail — comes out of classify with no trace of the password or the URL
// credentials, whichever error shape carried it.
func TestAPostgresErrorIsRedactedOfItsDSN(t *testing.T) {
	const secret = "s3cr3t-Pa55word-xyz"
	dsns := []string{
		"postgres://muster_ro:" + secret + "@db.example.com:5432/app?sslmode=require",
		"postgresql://muster_ro:" + secret + "@db.example.com/app",
		"host=db.example.com user=muster_ro password=" + secret + " dbname=app",
		"host=db.example.com user=muster_ro password='" + secret + " with space' dbname=app",
		"the password is " + secret,
	}
	for _, dsn := range dsns {
		for name, err := range map[string]error{
			"plain":     fmt.Errorf("dial %s: connection refused", dsn),
			"wrapped":   fmt.Errorf("pool: %w", errors.New("connect "+dsn)),
			"pg error":  &pgconn.PgError{Severity: "FATAL", Code: "28P01", Message: "bad login for " + dsn, Detail: dsn},
			"connected": fmt.Errorf("failed to connect: %w", &pgconn.PgError{Severity: "FATAL", Code: "08006", Message: dsn}),
		} {
			got := classify(err, secret).Message
			if strings.Contains(got, secret) || strings.Contains(got, "muster_ro:") {
				t.Errorf("%s error from %q kept a credential: %q", name, dsn, got)
			}
			// Positive control: the redaction did not just drop the message.
			if !strings.Contains(got, "[redacted]") {
				t.Errorf("%s error from %q shows no redaction marker: %q", name, dsn, got)
			}
		}
	}
	// Control: an error with nothing secret in it is passed through intact.
	if got := classify(&pgconn.PgError{Severity: "ERROR", Code: "42P01", Message: `relation "x" does not exist`}, secret).Message; got !=
		`ERROR: relation "x" does not exist (SQLSTATE 42P01)` {
		t.Errorf("clean error = %q", got)
	}
	// The password field of a URL with no user, and with an empty secret
	// argument (the audit line's call).
	if got := redactPG("postgres://:" + secret + "@h/db"); strings.Contains(got, secret) {
		t.Errorf("URL with no user: %q", got)
	}
}
