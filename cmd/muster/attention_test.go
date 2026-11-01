package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// summaryBody is a summary as the server writes it (compact, one line).
const summaryBody = `{"ts":1800000000,"nudge_mode":"live","partial":false,` +
	`"hosts":[{"host":"host-a","online":true,"status_report":true,"busy":2,"idle":1}],` +
	`"items":[{"key":"ask-5","kind":"ask","host":"","title":"chief","why":"Ship it?","since":1799999940,"age_secs":60,"offline":false,"path":"/attention/open/ask-5","url":""}]}`

// TestAttentionSummaryReadsItsTokenFromItsOwnEnvFile is the status bar's call:
// no credential flags, the URL and a personal token in a DEDICATED env file
// (--env-file), so the bar's token never has to live in the default one. The
// request is GET /api/attention/summary bearing that token; stdout is the
// server's document (indented), and the token is printed nowhere.
func TestAttentionSummaryReadsItsTokenFromItsOwnEnvFile(t *testing.T) {
	h := newHarness(t)
	h.json("GET /api/attention/summary", http.StatusOK, summaryBody)
	const barToken = "mst_bar-token-from-its-own-file-never-printed"
	envFile := filepath.Join(t.TempDir(), "bar.env")
	if err := os.WriteFile(envFile, []byte("MUSTER_API_URL="+h.srv.URL+"\nMUSTER_TOKEN="+barToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := h.runCLINoCredFlags(nil, envFile, "attention", "summary")
	if got.code != exitOK {
		t.Fatalf("exit %d, stderr %q", got.code, got.stderr)
	}
	reqs := h.nonHealthRequests()
	if len(reqs) != 1 || reqs[0].method != http.MethodGet || reqs[0].uri != "/api/attention/summary" || reqs[0].auth != "Bearer "+barToken {
		t.Fatalf("requests %+v; want one GET /api/attention/summary bearing the env file's token", reqs)
	}
	if mustCompact(t, got.stdout) != mustCompact(t, summaryBody) {
		t.Fatalf("stdout is not the server's document:\n%s", got.stdout)
	}
	if !strings.Contains(got.stdout, "\n  \"hosts\": [") {
		t.Errorf("stdout is not indented: %q", got.stdout)
	}
	if strings.Contains(got.stdout+got.stderr, barToken) {
		t.Fatal("the token was printed")
	}
}

func mustCompact(t *testing.T, s string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// TestAttentionSummaryAuthFailureExitsThree: a 401 (a hook secret, an unknown
// or revoked token) and a 403 are exit 3, the auth code a poller branches on
// to show "?" rather than "nothing waits"; nothing is written to stdout.
func TestAttentionSummaryAuthFailureExitsThree(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		h := newHarness(t)
		h.json("GET /api/attention/summary", code, `{"error":"invalid or revoked API token"}`)
		got := h.runCLI("attention", "summary")
		if got.code != exitAuth || got.stdout != "" {
			t.Errorf("HTTP %d: exit %d stdout %q; want exit %d and no stdout", code, got.code, got.stdout, exitAuth)
		}
	}
}
