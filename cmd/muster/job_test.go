package main

import (
	"net/http"
	"testing"
)

// TestJobVerbsReachTheirRoutesWithTheirFlags: `job list`, `job runs <name>`
// and `job run <name>` each send exactly one request to their own route, the
// name in the path and --host / --limit under the server's own parameter
// names, with no body (no verb sends a command), and print the server's JSON
// indented. A 409 (a name declared on several hosts) is a failure, not a
// success.
func TestJobVerbsReachTheirRoutesWithTheirFlags(t *testing.T) {
	for _, c := range []struct {
		name    string
		args    []string
		route   string
		wantURI string
		method  string
	}{
		{"list", []string{"job", "list"}, "GET /api/host-jobs", "/api/host-jobs", http.MethodGet},
		{"runs", []string{"job", "runs", "nightly-report", "--host", "build-02", "--limit", "7"}, "GET /api/host-jobs/nightly-report/runs",
			"/api/host-jobs/nightly-report/runs?host=build-02&limit=7", http.MethodGet},
		{"runs, no flags", []string{"job", "runs", "nightly-report"}, "GET /api/host-jobs/nightly-report/runs", "/api/host-jobs/nightly-report/runs", http.MethodGet},
		{"run", []string{"job", "run", "nightly-report", "--host", "build-02"}, "POST /api/host-jobs/nightly-report/run",
			"/api/host-jobs/nightly-report/run?host=build-02", http.MethodPost},
		{"forget", []string{"job", "forget", "build-02", "nightly-report"}, "DELETE /api/host-jobs/nightly-report",
			"/api/host-jobs/nightly-report?host=build-02", http.MethodDelete},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.json(c.route, http.StatusOK, `{"answer":"`+c.name+`"}`)
			got := h.runCLI(c.args...)
			if got.code != exitOK {
				t.Fatalf("exit %d: %s", got.code, got.stderr)
			}
			reqs := h.nonHealthRequests()
			if len(reqs) != 1 {
				t.Fatalf("%d requests; want 1: %+v", len(reqs), reqs)
			}
			if reqs[0].method != c.method || reqs[0].uri != c.wantURI {
				t.Fatalf("sent %s %s; want %s %s", reqs[0].method, reqs[0].uri, c.method, c.wantURI)
			}
			if reqs[0].body != "" {
				t.Fatalf("sent a body %q; a job verb sends none", reqs[0].body)
			}
			if want := "{\n  \"answer\": \"" + c.name + "\"\n}\n"; got.stdout != want {
				t.Fatalf("stdout %q; want %q", got.stdout, want)
			}
		})
	}

	h := newHarness(t)
	h.json("POST /api/host-jobs/nightly-report/run", http.StatusConflict, `{"error":"job \"nightly-report\" is declared on several hosts (build-01, build-02): name one with --host"}`)
	if got := h.runCLI("job", "run", "nightly-report"); got.code == exitOK || got.stdout != "" {
		t.Fatalf("a 409: exit %d stdout %q; want a failure and no stdout", got.code, got.stdout)
	}
}
