package main

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestPushExtraHostsAreParsedAtBoot: MUSTER_PUSH_EXTRA_HOSTS reaches the
// store's allowlist as exact lower-case names, and an entry that is not a bare
// host name refuses boot, naming the variable.
func TestPushExtraHostsAreParsedAtBoot(t *testing.T) {
	load := func(v string) (config, error) {
		c, err := loadConfig(func(k string) string {
			switch k {
			case envHookToken:
				return fixtureHookToken
			case envDatabase:
				return "postgres://x/y"
			case envStandalone:
				return "1"
			case envPushExtraHosts:
				return v
			}
			return ""
		})
		if err != nil {
			return c, err
		}
		return c, c.validate()
	}
	c, err := load("Ntfy.Example.org, push.example.net")
	if err != nil {
		t.Fatal(err)
	}
	a := c.pushAllowlist()
	if !a.Allows("https://ntfy.example.org/upX") || a.Allows("https://a.ntfy.example.org/upX") {
		t.Errorf("allowlist from the variable: hosts %v", a.ExtraHosts())
	}
	c, err = load("")
	if err != nil || len(c.pushAllowlist().ExtraHosts()) != 0 {
		t.Fatalf("unset: %v, hosts %v", err, c.pushAllowlist().ExtraHosts())
	}
	for _, bad := range []string{"*.example.org", "https://ntfy.example.org", "ntfy.example.org:443"} {
		if _, err := load(bad); err == nil || !strings.Contains(err.Error(), envPushExtraHosts) {
			t.Errorf("%q: err = %v; want a refusal naming %s", bad, err, envPushExtraHosts)
		}
	}
}

// TestBuildAppWiresTheExtraHostsIntoThePushStore: the service buildApp builds
// (buildPushService) holds MUSTER_PUSH_EXTRA_HOSTS in its store's allowlist —
// the one the claim and every send consult — not just in the config. The pool
// is never dialled (pgxpool connects lazily, and nothing here queries).
func TestBuildAppWiresTheExtraHostsIntoThePushStore(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://unused@127.0.0.1:1/unused")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	cfg := config{PushExtraHosts: "ntfy.example.org"}
	svc := buildPushService(cfg, pool, log.New(io.Discard, "", 0))
	if got := svc.Allowlist().ExtraHosts(); len(got) != 1 || got[0] != "ntfy.example.org" {
		t.Fatalf("the push store's operator hosts = %v; want [ntfy.example.org]", got)
	}
	if !svc.Allowlist().Allows("https://ntfy.example.org/upX") {
		t.Fatal("the push store does not admit the configured operator host")
	}
}

// TestNotifyLinkHostsAreParsedAtBoot: MUSTER_NOTIFY_LINK_HOSTS reaches the
// server as exact lower-case names, and an entry that is not a bare host name
// refuses boot, naming the variable — never a silent "no external links".
func TestNotifyLinkHostsAreParsedAtBoot(t *testing.T) {
	load := func(v string) (config, error) {
		c, err := loadConfig(func(k string) string {
			switch k {
			case envHookToken:
				return fixtureHookToken
			case envDatabase:
				return "postgres://x/y"
			case envStandalone:
				return "1"
			case envNotifyLinkHosts:
				return v
			}
			return ""
		})
		if err != nil {
			return c, err
		}
		return c, c.validate()
	}
	c, err := load("Docs.Example.com, status.example.org")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.notifyLinkHosts(), ","); got != "docs.example.com,status.example.org" {
		t.Fatalf("hosts %q", got)
	}
	if c, err := load(""); err != nil || len(c.notifyLinkHosts()) != 0 {
		t.Fatalf("unset: %v, hosts %v", err, c.notifyLinkHosts())
	}
	for _, bad := range []string{"*.example.org", "https://docs.example.com", "docs.example.com:443", "docs.example.com/x", "localhost"} {
		if _, err := load(bad); err == nil || !strings.Contains(err.Error(), envNotifyLinkHosts) {
			t.Errorf("%q: err = %v; want boot refused, naming %s", bad, err, envNotifyLinkHosts)
		}
	}
}
