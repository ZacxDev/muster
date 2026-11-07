package main

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/safedial"
)

// MUSTER_DIAL_DENY_PRIVATE from the environment to the dial sites.

// TestTheDialGuardIsOffByDefaultAndStrictWhenOn: unset (and every non-true
// spelling) is no guard; a true spelling is the STRICT guard, which refuses
// loopback.
func TestTheDialGuardIsOffByDefaultAndStrictWhenOn(t *testing.T) {
	for _, tc := range []struct {
		val  string
		want bool
	}{{"", false}, {"0", false}, {"off", false}, {"1", true}, {"true", true}, {"on", true}} {
		cfg, err := loadConfig(func(k string) string {
			if k == envDialDenyPrivate {
				return tc.val
			}
			return ""
		})
		if err != nil {
			t.Fatalf("%s=%q: loadConfig: %v", envDialDenyPrivate, tc.val, err)
		}
		g := cfg.dialGuard()
		if (g != nil) != tc.want {
			t.Fatalf("%s=%q: guard = %v, want on=%v", envDialDenyPrivate, tc.val, g, tc.want)
		}
		if g != nil && (g.Allowed(netip.MustParseAddr("127.0.0.1")) || g.Resolve != nil) {
			t.Fatalf("%s=%q: the guard is not the strict one: %+v", envDialDenyPrivate, tc.val, g)
		}
	}
}

// TestTheDialGuardReachesThePushSender: the service buildApp builds
// (buildPushService) dials through the guard when it is on. A raw listener on
// loopback counts connections: on, the probe is refused with safedial's error
// and the listener is never connected to; off (the control), the same probe
// CONNECTS (its TLS handshake then fails against a plain listener, which is
// fine — the connection is the observable).
func TestTheDialGuardReachesThePushSender(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var accepted atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			c.Close()
		}
	}()
	pool, err := pgxpool.New(context.Background(), "postgres://unused@127.0.0.1:1/unused")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	priv, pub, err := push.GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	sub := pushTestSubscription(t, "https://"+ln.Addr().String()+"/push/abc")
	probe := func(on bool) push.SendResult {
		cfg := config{VAPIDPublicKey: pub, VAPIDPrivateKey: priv, VAPIDSubject: "mailto:ops@example.com", DialDenyPrivate: on}
		return buildPushService(cfg, pool, log.New(io.Discard, "", 0)).Probe(context.Background(), sub, []byte(`{}`))
	}

	if res := probe(true); !errors.Is(res.Err, safedial.ErrForbiddenAddress) {
		t.Fatalf("guard on: probe = %+v; want safedial.ErrForbiddenAddress", res)
	}
	if n := accepted.Load(); n != 0 {
		t.Fatalf("guard on: the loopback listener accepted %d connection(s)", n)
	}
	if res := probe(false); res.Err == nil || errors.Is(res.Err, safedial.ErrForbiddenAddress) {
		t.Fatalf("CONTROL (guard off): probe = %+v; want a non-guard failure (a TLS handshake against a plain listener)", res)
	}
	if accepted.Load() == 0 {
		t.Fatal("CONTROL (guard off): the probe never connected to the loopback listener")
	}
}

// TestTheDialGuardBannerLine: both directions, each naming the variable.
func TestTheDialGuardBannerLine(t *testing.T) {
	on := lineNaming(renderBanner(t, config{DialDenyPrivate: true}, api.Extensions{DialGuard: &safedial.Guard{}}), envDialDenyPrivate)
	off := lineNaming(renderBanner(t, config{}, api.Extensions{}), envDialDenyPrivate)
	if !strings.Contains(on, "dial guard: ON") || !strings.Contains(off, "dial guard: OFF") {
		t.Fatalf("banner lines:\n  on:  %q\n  off: %q", on, off)
	}
}

// TestBuildAppWiresTheDialGuard: buildApp's own banner reads the WIRED
// ext.DialGuard (the field the postgres executor is built from), so ON there
// means the extension was set, not only the config.
func TestBuildAppWiresTheDialGuard(t *testing.T) {
	for _, on := range []bool{true, false} {
		var buf bytes.Buffer
		a, err := buildApp(context.Background(), config{DialDenyPrivate: on}, log.New(&buf, "", 0))
		if err != nil {
			t.Fatalf("buildApp (guard %v): %v", on, err)
		}
		a.Close()
		line := lineNaming(buf.String(), envDialDenyPrivate)
		want := map[bool]string{true: "dial guard: ON", false: "dial guard: OFF"}[on]
		if !strings.Contains(line, want) {
			t.Fatalf("buildApp with DialDenyPrivate=%v: banner line %q, want %q", on, line, want)
		}
	}
}

func pushTestSubscription(t *testing.T, endpoint string) push.Subscription {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatal(err)
	}
	var s push.Subscription
	s.Endpoint = endpoint
	s.Keys.P256dh = base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes())
	s.Keys.Auth = base64.RawURLEncoding.EncodeToString(auth)
	return s
}
