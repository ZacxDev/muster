package main

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/clientaddr"
)

// syncBuf is a log sink the server's goroutines may write concurrently.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestClientAddressSettings: MUSTER_TRUSTED_PROXIES and MUSTER_CLIENT_IP_HEADER,
// read by loadConfig, are refused when only one is set, are announced in the
// boot banner, and reach the handler every listener serves (the seam the api
// package's hand-built Extensions cannot see). The handler's own warnings are
// the observable: a trusted peer sending no header, and a forwarding header
// from an untrusted peer. httptest's peer is 192.0.2.1.
func TestClientAddressSettings(t *testing.T) {
	env := func(trusted, header string) func(string) string {
		return func(k string) string {
			switch k {
			case envTrustedProxies:
				return trusted
			case envClientIPHeader:
				return header
			}
			return ""
		}
	}
	for _, c := range []struct{ trusted, header, want string }{
		{"192.0.2.0/24", "", "MUSTER_TRUSTED_PROXIES is set but MUSTER_CLIENT_IP_HEADER is not"},
		{"", "X-Forwarded-For", "MUSTER_TRUSTED_PROXIES is empty"},
		{"192.0.2.0/24", "Forwarded", "MUSTER_CLIENT_IP_HEADER"},
		{"not-a-range", "X-Real-IP", "MUSTER_TRUSTED_PROXIES"},
	} {
		if _, err := loadConfig(env(c.trusted, c.header)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("loadConfig(%q, %q) = %v, want a refusal naming %q", c.trusted, c.header, err, c.want)
		}
	}

	build := func(trusted, header string) (*app, *syncBuf) {
		t.Helper()
		loaded, err := loadConfig(env(trusted, header))
		if err != nil {
			t.Fatalf("loadConfig: %v", err)
		}
		cfg := testConfig()
		cfg.ClientAddr = loaded.ClientAddr
		cfg.ConnectorEncKey = bytes.Repeat([]byte{1}, 32)
		cfg.ConnectPort = freePort(t)
		cfg.BrowserPort = freePort(t)
		buf := &syncBuf{}
		a, err := buildApp(context.Background(), cfg, log.New(buf, "", 0))
		if err != nil {
			t.Fatalf("buildApp: %v", err)
		}
		t.Cleanup(a.Close)
		if a.connect == nil || a.browser == nil {
			t.Fatal("instrument: the connect and browser listeners were not built")
		}
		return a, buf
	}
	get := func(h http.Handler, xff string) {
		r := httptest.NewRequest(http.MethodGet, "/health", nil)
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	listeners := func(a *app) map[string]http.Handler {
		return map[string]http.Handler{"main": a.http.Handler, "connect": a.connect.Handler, "browser": a.browser.Handler}
	}

	// Each listener gets its own app: the warnings are rate-limited per handler,
	// and the three listeners share one.
	for _, name := range []string{"main", "connect", "browser"} {
		on, onLog := build("192.0.2.0/24", "x-forwarded-for")
		if !strings.Contains(onLog.String(), "client address: TRUSTED PROXIES — when the TCP peer is in 192.0.2.0/24") {
			t.Fatalf("banner with both set: no TRUSTED PROXIES line:\n%s", onLog)
		}
		get(listeners(on)[name], "")
		if !strings.Contains(onLog.String(), "trusted proxy 192.0.2.1 sent no usable X-Forwarded-For") {
			t.Errorf("%s listener: the trusted policy did not reach the handler (no trusted-peer warning):\n%s", name, onLog)
		}

		off, offLog := build("", "")
		if !strings.Contains(offLog.String(), "client address: STRICT") {
			t.Fatalf("banner with both unset: no STRICT line:\n%s", offLog)
		}
		get(listeners(off)[name], "198.51.100.7")
		if !strings.Contains(offLog.String(), "ignored X-Forwarded-For from 192.0.2.1, which is not a trusted proxy (MUSTER_TRUSTED_PROXIES)") {
			t.Errorf("%s listener: the strict policy did not log a forwarding header from an untrusted peer:\n%s", name, offLog)
		}
	}
}

func mustClientAddr(t *testing.T, trusted, header string) *clientaddr.Policy {
	t.Helper()
	p, err := clientaddr.New(clientaddr.Config{TrustedProxies: trusted, Header: header,
		TrustedVar: envTrustedProxies, HeaderVar: envClientIPHeader})
	if err != nil {
		t.Fatal(err)
	}
	return p
}
