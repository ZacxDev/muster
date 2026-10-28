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

	"github.com/ZacxDev/muster/internal/api"
)

func TestSelfCommandRecognisesOnlyTheExactSpellings(t *testing.T) {
	cases := map[string]string{
		"-version":    "version",
		"--version":   "version",
		"version":     "version",
		"healthcheck": "healthcheck",
		// Everything else falls through to the account commands, unchanged.
		"invite":       "",
		"-healthcheck": "",
		"versions":     "",
	}
	for arg, want := range cases {
		if got := selfCommand([]string{arg}); got != want {
			t.Errorf("selfCommand(%q) = %q, want %q", arg, got, want)
		}
	}
	if got := selfCommand(nil); got != "" {
		t.Errorf("selfCommand(nil) = %q, want \"\" (no args is the server itself)", got)
	}
	if got := selfCommand([]string{"version", "extra"}); got != "" {
		t.Errorf("selfCommand with a trailing arg = %q, want \"\"", got)
	}
}

func TestRunVersionPrintsTheStampedVersion(t *testing.T) {
	old := api.BuildVersion
	t.Cleanup(func() { api.BuildVersion = old })
	// A value the default ("dev") cannot equal, so a hardcoded literal fails.
	api.BuildVersion = "9.8.7"
	var out bytes.Buffer
	if rc := runVersion(&out); rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	if got := out.String(); got != "muster-server 9.8.7\n" {
		t.Fatalf("output = %q", got)
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
