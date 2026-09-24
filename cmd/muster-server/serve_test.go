package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// "IT SERVES" — DRIVEN, NOT READ.
//
// 🔴 THE GATE IN internal/modulegate IS THE OTHER HALF OF THIS FILE AND NEITHER
// HALF IS SUFFICIENT. That one asserts internal/api is reachable from a binary,
// which is a claim about the IMPORT GRAPH — a blank `_` import satisfies it and
// executes nothing. This one binds a real TCP port, sends a real request through
// the real handler, and reads the response. What it does NOT prove is that the
// handler is muster's rather than some other module's; the linkage gate covers
// that. Delete either and the remaining one passes over the hole.
// ---------------------------------------------------------------------------

// quietLogger keeps boot banners out of the test output without changing which
// code path runs — the banner is printed either way.
func quietLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// testConfig is the smallest configuration that assembles.
//
// ⚠ IT HAS NO DSN, AND THAT IS NOT A SHORTCUT AROUND THE DATABASE. config.validate
// refuses a DSN-less configuration and main() applies it; buildApp accepts one so
// the LISTEN path can be exercised without Postgres. /health is deliberately a
// database-free probe (see api.handleHealth), so this test asks the one question
// it can answer honestly. The database-backed readiness path is
// TestReadyzRefusesAnUndeclaredMissingRouter below.
func testConfig() config {
	return config{
		Port: 0, // ephemeral: the kernel picks, ready() reports it back
		// Long enough to clear api.minUIPasswordLen, so the browser tier is
		// CONFIGURED rather than fail-closed — otherwise every browser surface
		// answers 503 and this test would be measuring the refusal.
		UIPassword:  "correct-horse-battery-staple-not-a-real-password",
		Standalone:  true,
		RouterActor: defaultRouterActor,
	}
}

// startServer assembles and runs the app on an ephemeral port, returning its
// base URL. It fails the test rather than skipping if anything goes wrong.
func startServer(t *testing.T, cfg config) string {
	t.Helper()

	app, err := buildApp(context.Background(), cfg, quietLogger())
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	t.Cleanup(app.Close)

	ctx, cancel := context.WithCancel(context.Background())
	addrCh := make(chan net.Addr, 1)
	runErr := make(chan error, 1)
	go func() { runErr <- app.Run(ctx, func(a net.Addr) { addrCh <- a }) }()

	var addr net.Addr
	select {
	case addr = <-addrCh:
	case err := <-runErr:
		t.Fatalf("the server returned before it was ready to serve: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the server never reported a bound address")
	}

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-runErr:
			if err != nil {
				t.Errorf("Run returned %v; a cancelled context is a clean shutdown", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("the server did not shut down within 30s of its context being cancelled")
		}
	})

	return "http://" + addr.String()
}

// TestTheServerListensAndServesHealth is the behavioural proof that this module
// serves HTTP.
//
// 🔴 IT BINDS A REAL PORT. Not httptest.NewServer, which would prove only that
// api.Server.Handler() answers — the thing that was never in doubt. The
// question this repository got wrong was whether any process reaches that
// handler, so the test drives the binary's OWN listen path: buildApp assembles
// exactly what main() assembles, Run binds exactly the socket main() binds.
func TestTheServerListensAndServesHealth(t *testing.T) {
	base := startServer(t, testConfig())

	resp, err := http.Get(base + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /health = %d, want 200", resp.StatusCode)
	}

	var body struct {
		Status  string  `json:"status"`
		Version string  `json:"version"`
		Uptime  float64 `json:"uptime"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding /health: %v", err)
	}
	// 🔴 THE FIELDS ARE CHECKED, NOT JUST THE STATUS CODE. A 200 with an empty
	// body would satisfy a status-only assertion, and the thing under test is
	// that muster's OWN handler answered rather than anything at all listening
	// on that port.
	if body.Status != "ok" {
		t.Errorf("/health status = %q, want %q", body.Status, "ok")
	}
	if body.Version == "" {
		t.Error("/health carried no version; this is not muster's health handler")
	}
}

// TestTheServerServesTheDocumentRootThroughTheUILayer proves the VIEW layer is
// reached by a live request, not merely imported.
//
// 🔴 IT IS A SEPARATE CLAIM FROM /health. Every route internal/ui renders sits
// behind requireSession, so an unauthenticated GET / is answered by the auth
// middleware — which redirects a document navigation to /login. /login is
// itself rendered HTML from this module, so the assertion is on the page that
// comes back, and that page is produced by code that used to link into nothing.
func TestTheServerServesTheDocumentRootThroughTheUILayer(t *testing.T) {
	base := startServer(t, testConfig())

	// No redirect following: the 303 itself is the evidence that requireSession
	// ran, and following it would conflate two assertions.
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequest(http.MethodGet, base+"/", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	// What makes this a document navigation rather than an XHR. Without it the
	// refusal is a 401 JSON body, which is the other correct answer — see
	// api.refuseUnauthenticated.
	req.Header.Set("Sec-Fetch-Mode", "navigate")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET / (navigation, no session) = %d, want 303 to the login page", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "/login") {
		t.Fatalf("GET / redirected to %q, want the login page", loc)
	}

	// Now the login page itself, which internal/api renders and which carries
	// Tailwind classes from the stylesheet the Dockerfile's CSS stage builds.
	login, err := http.Get(base + "/login")
	if err != nil {
		t.Fatalf("GET /login: %v", err)
	}
	defer login.Body.Close()
	page, err := io.ReadAll(login.Body)
	if err != nil {
		t.Fatalf("reading /login: %v", err)
	}
	if login.StatusCode != http.StatusOK {
		t.Fatalf("GET /login = %d, want 200", login.StatusCode)
	}
	if !strings.Contains(string(page), "<form") {
		t.Errorf("GET /login returned %d bytes with no <form>; this is not the rendered login page",
			len(page))
	}
}

// TestTheServerServesTheEmbeddedStylesheet proves `web` — the embedded asset
// package — is reachable through a live request.
//
// 🔴 IT ASSERTS THE BODY IS NON-TRIVIAL, NOT MERELY 200. `GET /static/app.css`
// answering 200 with an EMPTY file is the exact failure the Dockerfile's CSS
// stage is built to prevent: Tailwind does not error on a content glob that
// matches nothing, it emits a valid smaller stylesheet and exits 0. A
// status-only assertion here would be green in precisely that case.
func TestTheServerServesTheEmbeddedStylesheet(t *testing.T) {
	base := startServer(t, testConfig())

	resp, err := http.Get(base + "/static/app.css")
	if err != nil {
		t.Fatalf("GET /static/app.css: %v", err)
	}
	defer resp.Body.Close()
	css, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the stylesheet: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /static/app.css = %d, want 200", resp.StatusCode)
	}
	// The floor is the size Tailwind emits when EVERY content glob matches
	// nothing — measured at 5,594 bytes and recorded in the Makefile's
	// css-check comment. Anything at or below that is the silent-failure case.
	const emptyGlobCeiling = 5594
	if len(css) <= emptyGlobCeiling {
		t.Errorf("the served stylesheet is %d bytes, at or below the %d bytes Tailwind emits "+
			"when every content glob matches nothing. Run `make css-check`.",
			len(css), emptyGlobCeiling)
	}
	// A control class written only in internal/ui. Its absence means the
	// stylesheet parsed but scanned nothing useful.
	if !strings.Contains(string(css), "bg-emerald-500") {
		t.Error("the served stylesheet does not contain bg-emerald-500, a class written only " +
			"in internal/ui — the build did not reach the views")
	}
}
