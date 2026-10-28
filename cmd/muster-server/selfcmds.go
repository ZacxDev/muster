package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// `muster-server healthcheck`: the self-contained probe a packaged deployment
// needs, asking for no database and no configuration beyond MUSTER_PORT.
//
// 🔴 IT EXISTS BECAUSE THE IMAGE IS DISTROLESS. There is no shell, curl or wget
// inside it, so a compose or Docker HEALTHCHECK has nothing to run except this
// binary. deploy/docker-compose.yml calls it; `docker compose up --wait`
// depends on it.
//
// ⚠ IT PROBES /readyz, NOT /health. /health answers 200 as soon as the listener
// is up, whatever the database says; /readyz is the one that fails while
// Postgres is unreachable or the wiring is incomplete, which is what "the stack
// is up" has to mean for `--wait`.
//
// 🔴 AND IT PROBES GET /login, WHICH /readyz DOES NOT COVER. A MUSTER_UI_PASSWORD
// that is unset or shorter than 12 characters (the .env.example's CHANGE-ME is)
// refuses the whole human web tier fail-closed — every browser surface answers
// 503 — while /readyz still answers 200. Measured with the compose stack: `up
// --wait` reported healthy and the sign-in page was a 503. A 503 from /login is
// therefore unhealthy here, with a sentence naming the variable on stderr.
// Any other status is fine: gotrue mode serves its own page there.

// isHealthcheck reports whether args are exactly `healthcheck`.
func isHealthcheck(args []string) bool {
	return len(args) == 1 && args[0] == "healthcheck"
}

// healthcheckTimeout bounds one probe. A compose healthcheck has its own
// timeout too; this one makes the binary exit rather than hang if that is unset.
const healthcheckTimeout = 5 * time.Second

// runHealthcheck GETs /readyz and /login on this host's MUSTER_PORT and exits 0
// only when /readyz is 200 and /login is not 503. Anything else, a refused
// connection or a timeout is exit 1, with the reason on stderr (Docker records
// it in the container's health log).
func runHealthcheck(ctx context.Context, getenv func(string) string, stdout, stderr io.Writer) int {
	port := defaultPort
	if v := strings.TrimSpace(getenv(envPort)); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil || p < 1 || p > 65535 {
			fmt.Fprintf(stderr, "healthcheck: invalid %s %q\n", envPort, v)
			return 1
		}
		port = p
	}
	ctx, cancel := context.WithTimeout(ctx, healthcheckTimeout)
	defer cancel()
	base := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	// No redirects: /login may redirect a signed-in browser, and the status we
	// judge is the one this server answered, not wherever it pointed.
	client := &http.Client{
		Transport:     &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	get := func(path string) (int, string, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		if err != nil {
			return 0, "", err
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, "", err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return resp.StatusCode, strings.TrimSpace(string(body)), nil
	}
	code, body, err := get("/readyz")
	if err != nil {
		fmt.Fprintf(stderr, "healthcheck: %v\n", err)
		return 1
	}
	if code != http.StatusOK {
		fmt.Fprintf(stderr, "healthcheck: %s/readyz answered %d: %s\n", base, code, body)
		return 1
	}
	code, _, err = get("/login")
	if err != nil {
		fmt.Fprintf(stderr, "healthcheck: %v\n", err)
		return 1
	}
	if code == http.StatusServiceUnavailable {
		// A FIXED sentence, not the body: the body is an HTML page whose reason
		// sits past the first few KiB, and Docker keeps only the first 4 KiB of
		// a probe's output — measured, the health log then named nothing.
		fmt.Fprintf(stderr, "healthcheck: the sign-in page is refused (%s/login answered 503): "+
			"%s is unset or shorter than 12 characters, so every browser surface is "+
			"refused. The boot log's \"human web tier\" line has the exact reason\n", base, envUIPassword)
		return 1
	}
	fmt.Fprintf(stdout, "healthcheck: %s/readyz ok, /login %d\n", base, code)
	return 0
}
