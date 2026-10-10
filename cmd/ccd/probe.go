package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Whether the model credential WORKS is a separate question from whether one is
// configured, and the CLI's own answer to the second is no answer to the first:
// `claude auth status` reports {"loggedIn": true, "authMethod": "oauth_token"},
// exit 0, for a deliberately invalid token. So /healthz is backed by an
// authenticated round trip.
//
// THE CHECK: one headless turn through the OFFICIAL binary —
//
//	claude -p "Reply with the single word OK." --output-format json
//	       --no-session-persistence --tools "" --strict-mcp-config
//	       --setting-sources "" --model <CCD_PROBE_MODEL, default haiku>
//
// read by its JSON `is_error` / `api_error_status`, NEVER by `subtype` (which says
// "success" on a 401) or the exit code. The flags keep it small and side-effect
// free: no tools, no MCP servers, no settings files (so none of ccd's own hooks
// fire for it, and CCD_HOOK_DISABLED=1 is set as a second guard), no transcript.
//
// THE COST, so it can be budgeted rather than discovered:
//   - one smallest-model request per successful probe, on the SAME subscription
//     rate limit the operator's own sessions use. The CLI's system prompt rides
//     along, so it is a few thousand input tokens, not a handful.
//   - a probe the API REJECTS (401/429) costs no inference.
//   - a process spawn: the CLI is ~250 MB resident for the second or two it runs.
//   - cadence: once at start, then every CCD_PROBE_OK_INTERVAL (default 12h) while
//     healthy and every CCD_PROBE_RETRY_INTERVAL (default 5m) while not. Real turns
//     also update the state (a successful turn IS an authenticated round trip), so
//     the probe only carries the signal across idle periods.
//
// ⚠ A DIRECT HTTP CALL TO THE API WITH THE OAUTH TOKEN WOULD BE CHEAPER AND WAS
// NOT CHOSEN: a subscription token is for the official client, and ccd using it
// itself is exactly the credential intermediation the provider's terms rule out.

const (
	authUnknown     = "unknown"
	authOK          = "ok"
	authFailed      = failAuth
	authRateLimited = failRateLimited
	authProbeError  = "probe_error"
)

type authSnapshot struct {
	Auth       string     `json:"auth"`
	AuthDetail string     `json:"auth_detail,omitempty"`
	AuthSource string     `json:"auth_source,omitempty"` // "probe" or "turn"
	AuthAt     *time.Time `json:"auth_checked_at,omitempty"`
}

// authTracker holds the latest verdict on the model credential.
type authTracker struct {
	mu   sync.Mutex
	snap authSnapshot
	now  func() time.Time
}

func newAuthTracker() *authTracker {
	return &authTracker{snap: authSnapshot{Auth: authUnknown}, now: time.Now}
}

func (a *authTracker) snapshot() authSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.snap
}

func (a *authTracker) set(state, detail, source string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	a.snap = authSnapshot{Auth: state, AuthDetail: detail, AuthSource: source, AuthAt: &now}
}

// observeTurn records what a real turn proved: nil = it succeeded; an auth or
// rate-limit failure says the same thing about the credential a probe would.
// Other failures say nothing about the credential and leave the state alone.
func (a *authTracker) observeTurn(f *failure) {
	switch {
	case f == nil:
		a.set(authOK, "", "turn")
	case f.Type == failAuth || f.Type == failRateLimited:
		a.set(f.Type, f.Message, "turn")
	}
}

// probeResult is the subset of `claude -p --output-format json` ccd reads.
type probeResult struct {
	Type           string `json:"type"`
	Subtype        string `json:"subtype"`
	IsError        bool   `json:"is_error"`
	APIErrorStatus int    `json:"api_error_status"`
	Result         string `json:"result"`
}

// classifyProbe turns the CLI's stdout into an auth state. It reads the LAST
// line that parses as a result object, so stray output before it is harmless.
func classifyProbe(stdout []byte) (state, detail string) {
	var res *probeResult
	for _, line := range bytes.Split(stdout, []byte("\n")) {
		var p probeResult
		if json.Unmarshal(bytes.TrimSpace(line), &p) == nil && p.Type == "result" {
			p := p
			res = &p
		}
	}
	if res == nil {
		return authProbeError, "the CLI printed no result object"
	}
	if !res.IsError {
		return authOK, ""
	}
	f := classifyAPIError(apiError{Status: res.APIErrorStatus, Message: res.Result})
	switch f.Type {
	case failAuth, failRateLimited:
		return f.Type, f.Message
	}
	return authProbeError, fmt.Sprintf("is_error with api_error_status=%d: %s", res.APIErrorStatus, res.Result)
}

type cliProber struct {
	bin     string
	model   string
	dir     string
	timeout time.Duration
}

func (p cliProber) args() []string {
	a := []string{"-p", "Reply with the single word OK.", "--output-format", "json",
		"--no-session-persistence", "--tools", "", "--strict-mcp-config", "--setting-sources", ""}
	if p.model != "" {
		a = append(a, "--model", p.model)
	}
	return a
}

func (p cliProber) probe(ctx context.Context) (string, string) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.bin, p.args()...)
	cmd.Dir = p.dir
	cmd.Env = append(os.Environ(), "CCD_HOOK_DISABLED=1")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	runErr := cmd.Run()
	state, detail := classifyProbe(out.Bytes())
	if state == authProbeError && runErr != nil {
		detail = strings.TrimSpace(fmt.Sprintf("%s (%v) %s", detail, runErr, lastLine(errb.String())))
	}
	return state, detail
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// runProbes probes now and then on the schedule documented above, until ctx ends.
func runProbes(ctx context.Context, a *authTracker, probe func(context.Context) (string, string), okEvery, retryEvery time.Duration) {
	for {
		state, detail := probe(ctx)
		if ctx.Err() != nil {
			return
		}
		a.set(state, detail, "probe")
		wait := retryEvery
		if state == authOK {
			wait = okEvery
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
