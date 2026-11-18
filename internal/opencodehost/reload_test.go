package opencodehost

import (
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/opencodehost/opencodetest"
)

// A READER of the token file (a muster-mirror run with -opencode-read-only,
// docs/design/multi-tenant.md): it follows the renewals another daemon
// writes, and never mints a token itself.

// TestAReaderFollowsTheFileAndNeverMints: a token DUE for renewal is never
// renewed by Reload or KeepReloaded (zero /api/pair requests); a new token
// written to the file by the renewer is picked up and used. The positive
// control for "zero": Renew on the same due token does mint.
func TestAReaderFollowsTheFileAndNeverMints(t *testing.T) {
	srv := opencodetest.New(fixturePassword)
	defer srv.Close()
	c, tokFile := pairedCredentials(t, srv)
	// Due: 25 days on (5 left, inside RenewBefore), on the server's clock too.
	later := time.Now().Add(25 * 24 * time.Hour)
	srv.SetNow(func() time.Time { return later })
	pairs := func() int {
		n := 0
		for _, r := range srv.Requests() {
			if r.Path == "/api/pair" {
				n++
			}
		}
		return n
	}
	before := pairs()

	if changed, err := c.Reload(); changed || err != nil {
		t.Fatalf("an unchanged file: changed %v, %v", changed, err)
	}
	// The renewer (another daemon) writes a new token.
	renewer, err := LoadCredentials(tokFile)
	if err != nil {
		t.Fatal(err)
	}
	if renewed, err := renewer.Renew(context.Background(), later); !renewed || err != nil {
		t.Fatalf("the control: the renewer did not renew: %v %v", renewed, err)
	}
	if pairs() != before+1 {
		t.Fatalf("the control: the renewer made %d /api/pair request(s), want 1", pairs()-before)
	}
	before = pairs()

	var logs syncBuf
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { KeepReloaded(ctx, c, log.New(&logs, "", 0), 10*time.Millisecond); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for c.token() != renewer.token() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if c.token() != renewer.token() {
		t.Fatal("the reader never picked up the renewed token")
	}
	if n := pairs() - before; n != 0 {
		t.Fatalf("the reader made %d /api/pair request(s), want 0", n)
	}
	out := logs.String()
	if !strings.Contains(out, "changed: token") || strings.Contains(out, c.token()) {
		t.Fatalf("log %q", out)
	}
}
