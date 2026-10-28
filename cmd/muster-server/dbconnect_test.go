package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/dbtest"
)

// TestBootSurvivesADatabaseThatStartsListeningLate drives buildApp — the real
// boot path — at a DSN whose port refuses connections for the first ~1.5 s and
// then forwards to the test database. That is the compose fresh-volume race:
// Postgres not yet on TCP when muster first connects. Before the retry,
// buildApp failed on the first refused connect.
func TestBootSurvivesADatabaseThatStartsListeningLate(t *testing.T) {
	oldInterval := dbConnectInterval
	dbConnectInterval = 200 * time.Millisecond
	t.Cleanup(func() { dbConnectInterval = oldInterval })

	real := dbtest.DSN(t)
	u, err := url.Parse(real)
	if err != nil {
		t.Fatal(err)
	}
	target := u.Host

	// Reserve a port, then release it: nothing listens there until the proxy starts.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	late := l.Addr().String()
	l.Close()

	started := make(chan struct{})
	go func() {
		time.Sleep(1500 * time.Millisecond)
		pl, err := net.Listen("tcp", late)
		if err != nil {
			t.Errorf("proxy listen: %v", err)
			close(started)
			return
		}
		close(started)
		t.Cleanup(func() { pl.Close() })
		for {
			c, err := pl.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				up, err := net.Dial("tcp", target)
				if err != nil {
					return
				}
				defer up.Close()
				go func() { _, _ = io.Copy(up, c) }()
				_, _ = io.Copy(c, up)
			}(c)
		}
	}()

	// Positive control on the setup: the late port really refuses at first.
	if c, err := net.DialTimeout("tcp", late, time.Second); err == nil {
		c.Close()
		t.Fatal("control FAILED: the late port accepted a connection before the proxy started")
	}

	u.Host = late
	cfg := dbConfig(t)
	cfg.Database = u.String()
	var buf bytes.Buffer
	app, err := buildApp(context.Background(), cfg, log.New(&buf, "", 0))
	<-started
	if err != nil {
		t.Fatalf("buildApp against a late-listening database: %v\nlog:\n%s", err, buf.String())
	}
	defer app.Close()
	if !strings.Contains(buf.String(), "connect attempt 1 failed") {
		t.Fatalf("expected at least one logged retry; log:\n%s", buf.String())
	}
}

// TestAMalformedDatabaseURLIsNotRetried: a bad DSN fails identically forever,
// so it must be refused at once, not after the whole budget.
func TestAMalformedDatabaseURLIsNotRetried(t *testing.T) {
	start := time.Now()
	_, err := connectDB(context.Background(), "postgres://%zz", log.New(io.Discard, "", 0))
	if err == nil || !strings.Contains(err.Error(), "parse database url") {
		t.Fatalf("err = %v, want a parse error", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %s: a malformed URL was retried", d)
	}
}
