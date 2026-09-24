package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordedRequest is what the fake server saw. The RAW request URI is kept
// (r.RequestURI, not r.URL.Path) because the whole point of the
// empty-path-param guard is that a malformed path must never reach the wire —
// and r.URL.Path would already have been parsed.
type recordedRequest struct {
	method string
	uri    string
	auth   string
	body   string
	// source is the provenance header the server derives a comment's author
	// from. Recorded separately because attributing a comment to the wrong
	// producer is a silent failure, not an error.
	source string
	// headers is EVERY header the request carried. It exists for the
	// task-thread SEAM ledger (TestTaskSubcommandHeaderLedger), which has to
	// assert the exact SET a subcommand sends — a per-header field like `source`
	// above can only ever check the headers someone remembered to add a field
	// for, and the failure being guarded against is a header nobody remembered.
	headers http.Header
}

type harness struct {
	t   *testing.T
	srv *httptest.Server

	mu   sync.Mutex
	reqs []recordedRequest

	// routes maps "METHOD /path" to a responder. A request with no route gets
	// the same text/plain 404 net/http.ServeMux emits, so the exit-7 path is
	// exercised against a realistic body rather than a textbook fixture.
	routes map[string]func(w http.ResponseWriter, r *http.Request)
	// version is served by GET /health unless routes overrides it.
	version string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){}, version: buildVersion}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.reqs = append(h.reqs, recordedRequest{
			method: r.Method, uri: r.RequestURI, auth: r.Header.Get("Authorization"),
			body: string(body), source: r.Header.Get(headerSource),
			headers: r.Header.Clone(),
		})
		h.mu.Unlock()

		if fn, ok := h.routes[r.Method+" "+r.URL.Path]; ok {
			fn(w, r)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/health" {
			writeJSON(w, http.StatusOK, `{"status":"ok","version":"`+h.version+`","uptime":12.5}`)
			return
		}
		// Exactly what net/http.ServeMux writes for an unregistered pattern.
		http.NotFound(w, r)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *harness) handle(pattern string, fn func(w http.ResponseWriter, r *http.Request)) {
	h.routes[pattern] = fn
}

// json registers a static JSON responder.
func (h *harness) json(pattern string, status int, body string) {
	h.handle(pattern, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, status, body) })
}

func (h *harness) requests() []recordedRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]recordedRequest, len(h.reqs))
	copy(out, h.reqs)
	return out
}

// nonHealthRequests filters out the version-skew probe.
func (h *harness) nonHealthRequests() []recordedRequest {
	var out []recordedRequest
	for _, r := range h.requests() {
		if r.uri != "/health" {
			out = append(out, r)
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

const testToken = "s3cr3t-hook-token-never-printed"

type result struct {
	code   int
	stdout string
	stderr string
}

// testProgName is the name the harness invokes the binary as.
//
// ⚠ IT IS NOT "muster", AND THE DIFFERENCE IS A CONTROL. Every assertion that
// reads a name out of stderr or --help would pass against a hardcoded default
// if the harness used the default. Driving the tree under a DIFFERENT name
// means those assertions are measuring argv[0] propagation rather than a
// constant.
const testProgName = "harness-invoked-name"

// runCLI drives the real binary entry point. --env-file points into a temp dir
// so a developer's real config can never influence a test.
func (h *harness) runCLI(args ...string) result {
	h.t.Helper()
	return h.runCLIStdin(strings.NewReader(""), args...)
}

func (h *harness) runCLIStdin(stdin io.Reader, args ...string) result {
	h.t.Helper()
	return h.runCLIEnv(nil, stdin, args...)
}

// runCLIEnv is runCLIStdin with a controlled environment. It exists for the
// session id (the task-thread seam): the default getenv returns "" for
// everything, so without this every session-id assertion would be measuring the
// absent case only.
func (h *harness) runCLIEnv(env map[string]string, stdin io.Reader, args ...string) result {
	h.t.Helper()
	return h.runCLIAs(testProgName, env, stdin, args...)
}

// runCLIAs is runCLIEnv with an explicit argv[0]. It is the entry point the
// alias guards use.
func (h *harness) runCLIAs(argv0 string, env map[string]string, stdin io.Reader, args ...string) result {
	h.t.Helper()
	var out, errOut strings.Builder
	full := append([]string{
		"--api-url", h.srv.URL,
		"--token", testToken,
		"--env-file", filepath.Join(h.t.TempDir(), "absent.env"),
	}, args...)
	getenv := func(k string) string { return env[k] }
	code := run(context.Background(), argv0, full, stdin, &out, &errOut, getenv, h.t.TempDir())
	return result{code: code, stdout: out.String(), stderr: errOut.String()}
}

// runCLINoCredFlags drives the binary with NO --api-url / --token flags at all,
// so config comes only from the process environment and the named env file.
//
// 🔴 IT EXISTS BECAUSE THE FLAGS MASK THE THING UNDER TEST. Every other runner
// here passes credentials on the command line, which is exactly the
// configuration a dispatched agent instance does NOT have: an instance carries
// the API URL and its token in its environment and nothing else. A "the verbs
// work from an instance" claim made with --token on the command line would be
// measuring the operator shell a second time.
func (h *harness) runCLINoCredFlags(env map[string]string, envFile string, args ...string) result {
	h.t.Helper()
	var out, errOut strings.Builder
	full := append([]string{"--env-file", envFile}, args...)
	getenv := func(k string) string { return env[k] }
	code := run(context.Background(), testProgName, full, strings.NewReader(""), &out, &errOut, getenv, h.t.TempDir())
	return result{code: code, stdout: out.String(), stderr: errOut.String()}
}

// runCapturingTimeout drives the command tree and reports the duration the HTTP
// TRANSPORT was actually built with.
//
// 🔴 IT EXISTS BECAUSE THE TIMEOUT IS NOT OBSERVABLE ANY OTHER WAY WITHOUT
// WAITING FOR IT. app.api() bakes a.flagTimeout into the http.Client on FIRST
// use and the client is memoised, so the only evidence that a deadline landed
// is the value newHTTP received. It hooks app.newHTTP, the injection point the
// struct already has for exactly this.
//
// ⚠ It does NOT go through run(), because run() constructs its own app. That
// costs the exit-code path, so tests asserting an exit code use runCLI.
func (h *harness) runCapturingTimeout(args ...string) (time.Duration, int) {
	h.t.Helper()
	var out, errOut strings.Builder
	var captured time.Duration
	a := &app{
		stdout: &out, stderr: &errOut,
		getenv:   func(string) string { return "" },
		homeDir:  h.t.TempDir(),
		progName: testProgName,
		newHTTP: func(timeout time.Duration) *http.Client {
			captured = timeout
			return &http.Client{Timeout: timeout}
		},
	}
	root := newRootCmd(a)
	root.SetArgs(append([]string{
		"--api-url", h.srv.URL,
		"--token", testToken,
		"--env-file", filepath.Join(h.t.TempDir(), "absent.env"),
	}, args...))
	root.SetIn(strings.NewReader(""))
	err := root.ExecuteContext(context.Background())
	if err != nil {
		h.t.Logf("runCapturingTimeout(%v): %v\n%s", args, err, errOut.String())
	}
	return captured, exitCodeFor(err)
}

// inspectApp is a zero-value-ish app for tests that only need to WALK the
// command tree rather than run it.
func inspectApp() *app {
	return &app{
		stdout: discardWriter{}, stderr: discardWriter{},
		getenv: func(string) string { return "" }, progName: testProgName,
	}
}

// discardWriter is a local no-op writer; the command tree only needs somewhere
// to point its streams when it is being inspected rather than run.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
