package db

import (
	"testing"
	"time"
)

// TestPoolConfigDefaultsWhenTheDSNSaysNothing: a DSN with no pool_* keys gets
// muster's pool size and idle timeout, not pgx's CPU-derived default — in both
// DSN forms pgx accepts. On an 8-CPU host pgx's max(4, NumCPU) default is also
// 8, so there the idle-time assertion (pgx default 30m) is the one that
// discriminates.
func TestPoolConfigDefaultsWhenTheDSNSaysNothing(t *testing.T) {
	for _, dsn := range []string{
		"postgres://u:p@127.0.0.1:5432/muster?sslmode=disable",
		"host=127.0.0.1 port=5432 user=u password=p dbname=muster sslmode=disable",
	} {
		cfg, err := poolConfig(dsn)
		if err != nil {
			t.Fatalf("poolConfig(%q): %v", dsn, err)
		}
		if cfg.MaxConns != 8 {
			t.Errorf("%q: MaxConns = %d, want 8", dsn, cfg.MaxConns)
		}
		if cfg.MaxConnIdleTime != 5*time.Minute {
			t.Errorf("%q: MaxConnIdleTime = %s, want 5m", dsn, cfg.MaxConnIdleTime)
		}
	}
}

// TestPoolConfigTheDSNWins: pool_max_conns / pool_max_conn_idle_time in the
// DSN are honoured, each independently of the other, above and below muster's
// defaults.
func TestPoolConfigTheDSNWins(t *testing.T) {
	for _, c := range []struct {
		dsn      string
		wantMax  int32
		wantIdle time.Duration
	}{
		{"postgres://u:p@127.0.0.1/muster?pool_max_conns=3&pool_max_conn_idle_time=1m", 3, time.Minute},
		{"host=127.0.0.1 user=u dbname=muster pool_max_conns=3 pool_max_conn_idle_time=1m", 3, time.Minute},
		{"postgres://u:p@127.0.0.1/muster?pool_max_conns=31", 31, 5 * time.Minute},
		{"postgres://u:p@127.0.0.1/muster?pool_max_conn_idle_time=47s", 8, 47 * time.Second},
	} {
		cfg, err := poolConfig(c.dsn)
		if err != nil {
			t.Fatalf("poolConfig(%q): %v", c.dsn, err)
		}
		if cfg.MaxConns != c.wantMax {
			t.Errorf("%q: MaxConns = %d, want %d", c.dsn, cfg.MaxConns, c.wantMax)
		}
		if cfg.MaxConnIdleTime != c.wantIdle {
			t.Errorf("%q: MaxConnIdleTime = %s, want %s", c.dsn, cfg.MaxConnIdleTime, c.wantIdle)
		}
		if _, leaked := cfg.ConnConfig.RuntimeParams["pool_max_conns"]; leaked {
			t.Errorf("%q: pool_max_conns would be sent to the server as a runtime parameter", c.dsn)
		}
	}
}

// TestPoolConfigRefusesAMalformedPoolValue: a bad value is an error, not a
// silent fall-back to the default.
func TestPoolConfigRefusesAMalformedPoolValue(t *testing.T) {
	for _, dsn := range []string{
		"postgres://u:p@127.0.0.1/muster?pool_max_conns=lots",
		"postgres://u:p@127.0.0.1/muster?pool_max_conn_idle_time=soon",
	} {
		if _, err := poolConfig(dsn); err == nil {
			t.Errorf("poolConfig(%q) = nil error, want a parse error", dsn)
		}
	}
}
