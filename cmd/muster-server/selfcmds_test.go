package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestIsHealthcheckMatchesOnlyTheExactSpelling(t *testing.T) {
	cases := map[string]bool{
		"healthcheck":  true,
		"-healthcheck": false,
		"health":       false,
		"invite":       false, // the account commands still get everything else
	}
	for arg, want := range cases {
		if got := isHealthcheck([]string{arg}); got != want {
			t.Errorf("isHealthcheck(%q) = %v, want %v", arg, got, want)
		}
	}
	if isHealthcheck(nil) || isHealthcheck([]string{"healthcheck", "extra"}) {
		t.Error("isHealthcheck must be false for no args (the server itself) and for a trailing arg")
	}
}

// portEnv serves srv's port as MUSTER_PORT.
func portEnv(t *testing.T, rawURL string) func(string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(u.Host)
	return func(k string) string {
		if k == envPort {
			return port
		}
		return ""
	}
}

func TestHealthcheckIsZeroOnlyWhenReadyAndSignInIsServed(t *testing.T) {
	var hits []string
	ready, login := http.StatusOK, http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		switch r.URL.Path {
		case "/readyz":
			w.WriteHeader(ready)
		case "/login":
			w.WriteHeader(login)
			_, _ = w.Write([]byte("fixture: the password is not set"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	getenv := portEnv(t, srv.URL)
	run := func() (int, string) {
		hits = nil
		var out, errb bytes.Buffer
		rc := runHealthcheck(context.Background(), getenv, &out, &errb)
		return rc, errb.String()
	}

	if rc, e := run(); rc != 0 {
		t.Fatalf("ready + sign-in served: rc = %d, stderr %q", rc, e)
	}
	if strings.Join(hits, ",") != "/readyz,/login" {
		t.Fatalf("probed %v, want [/readyz /login]", hits)
	}

	// gotrue mode's /login may redirect; only a 503 is a refusal.
	login = http.StatusSeeOther
	if rc, e := run(); rc != 0 {
		t.Fatalf("a 303 from /login: rc = %d, stderr %q", rc, e)
	}

	// The fail-closed web tier: /readyz 200, /login 503 — must be unhealthy.
	login = http.StatusServiceUnavailable
	if rc, e := run(); rc != 1 || !strings.Contains(e, envUIPassword) || len(e) > 1024 {
		t.Fatalf("/login 503: rc = %d, stderr %q; want 1, naming %s, short enough for Docker's health log", rc, e, envUIPassword)
	}

	ready, login = http.StatusServiceUnavailable, http.StatusOK
	if rc, e := run(); rc != 1 || !strings.Contains(e, "503") {
		t.Fatalf("/readyz 503: rc = %d, stderr %q", rc, e)
	}
	if strings.Join(hits, ",") != "/readyz" {
		t.Fatalf("after a failed /readyz probed %v, want only /readyz", hits)
	}
}

func TestHealthcheckFailsOnARefusedConnectionAndABadPort(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	getenv := portEnv(t, srv.URL)
	srv.Close() // nothing listens there now
	var out, errb bytes.Buffer
	if rc := runHealthcheck(context.Background(), getenv, &out, &errb); rc != 1 {
		t.Fatalf("closed port: rc = %d, want 1", rc)
	}
	bad := func(k string) string {
		if k == envPort {
			return "not-a-port"
		}
		return ""
	}
	errb.Reset()
	if rc := runHealthcheck(context.Background(), bad, &out, &errb); rc != 1 || !strings.Contains(errb.String(), envPort) {
		t.Fatalf("bad port: rc = %d, stderr %q", rc, errb.String())
	}
}
