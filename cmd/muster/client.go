package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// buildVersion is the muster SERVER version this CLI was built against. It is
// overridable at link time (-ldflags "-X main.buildVersion=1.2.3"), mirroring
// how the server stamps api.BuildVersion.
//
// 🔴 THE TWO DEFAULTS MUST AGREE, AND A TEST — NOT DISCIPLINE — IS WHAT HOLDS
// THEM TOGETHER. warnSkew compares this literal against what /health reports,
// so a value that drifts from the server's makes the note fire on every command
// on every host forever, which trains a reader to ignore the one signal that
// catches a genuinely stale client. TestTheCLIBuildVersionDefaultMatchesTheServers
// imports internal/api and fails when they diverge. That import is TEST-ONLY:
// the shipped binary does not link the server.
//
// 🔴 ITS VALUE NAMESPACE IS THE SERVER'S — A SEMVER — AND A GIT REVISION MUST
// NEVER BE STAMPED HERE. Both readers below put this value next to the server's
// own: warnSkew prints "server 0.2.2, muster built for X" and the route-absent
// 404 renders "this client was built against X" on the one surface where an
// operator is reasoning about compatibility. A revision in X turns both into a
// comparison that cannot be made — "0.2.2" against "ba6698e" is not older,
// newer, or equal — and it reads as a real version rather than the obvious
// placeholder "dev" is. buildRevision below exists so provenance has its own
// variable instead of borrowing this one.
var buildVersion = "dev"

// buildRevision is the git revision of the source tree THIS BINARY was linked
// from — provenance, NOT a version, and deliberately a second variable.
//
// 🔴 IT ANSWERS A DIFFERENT QUESTION FROM buildVersion AND IS NEVER COMPARED TO
// THE SERVER. "Which muster is installed here?" is asked during incidents by
// someone holding the artefact and not the nix store, and before this existed
// the only answer was `readlink -f` on the binary: flake.nix records the
// revision in the DERIVATION NAME (`muster-cli-ba6698e`), which the program
// could not reach. flake.nix now stamps it here instead
// (-ldflags "-X main.buildRevision=<rev>"), cliVersion composes it into what
// `--version` prints, and tests/cli-version-stamp.sh asserts it against the
// built artefact's own output.
//
// ⚠ EMPTY IS THE SUPPORTED DEFAULT, not a defect: a plain `go build` — which is
// what `make build`, `make verb-ledger` and every CI job on a runner with no nix
// produce — stamps nothing, and cliVersion omits the clause rather than printing
// an empty one.
var buildRevision = ""

// cliVersion is the string cobra reports for `--version`. It is the ONE place
// the two values above are allowed to meet, and it LABELS them rather than
// letting one stand in for the other.
//
// 🔴 THE LABEL IS THE POINT. `muster version dev (rev ba6698e)` says which
// server version the client was built against AND which tree produced the
// binary; `muster version ba6698e` — this field carrying whichever value the
// build system happened to stamp — reproduces inside `--version` exactly the
// ambiguity the split above removes.
//
// ⚠ IT MUST NEVER RETURN "". cobra DISABLES the --version flag entirely when
// Version is empty, so an empty return would delete the flag rather than print
// nothing. buildVersion's default is non-empty, and a build that blanks it
// still leaves the "(rev …)" clause.
func cliVersion() string {
	if buildRevision == "" {
		return buildVersion
	}
	return buildVersion + " (rev " + buildRevision + ")"
}

// emptyPathParamMsg is the guard's own error text. Tests assert on this exact
// string so that a mutation which removes the guard fails with THIS error and
// cannot be mistaken for some other check firing.
const emptyPathParamMsg = "refusing to build a request path"

// expandPath substitutes {name} placeholders in a route template and REFUSES to
// produce a path when any placeholder resolves to empty.
//
// 🔴 This guard is the fix for a whole class of HTTP 301. net/http.ServeMux
// redirects any request path that differs from its cleaned form, and an empty
// shell variable interpolated into "/api/tasks/$ID/status" produces
// "/api/tasks//status", which cleans to "/api/tasks/status" — hence 301 Moved
// Permanently, from a request that was malformed before it left the caller.
// Normalising the path client-side would be the WRONG fix: it would silently
// address SOME OTHER resource. The right behaviour is to never emit the request
// at all and exit 2.
func expandPath(tmpl string, params map[string]string) (string, error) {
	var b strings.Builder
	rest := tmpl
	for {
		i := strings.IndexByte(rest, '{')
		if i < 0 {
			b.WriteString(rest)
			break
		}
		j := strings.IndexByte(rest[i:], '}')
		if j < 0 {
			return "", failf(exitUsage, "%s: malformed route template %q", emptyPathParamMsg, tmpl)
		}
		name := rest[i+1 : i+j]
		b.WriteString(rest[:i])
		v, ok := params[name]
		if !ok || strings.TrimSpace(v) == "" {
			return "", failf(exitUsage,
				"%s: path parameter {%s} is empty (route %q). An empty value here would produce a doubled slash and a 301 redirect to a DIFFERENT route, so no request was sent.",
				emptyPathParamMsg, name, tmpl)
		}
		// PathEscape also escapes "/", so a value can never inject extra path
		// segments into the route.
		b.WriteString(url.PathEscape(strings.TrimSpace(v)))
		rest = rest[i+j+1:]
	}
	return b.String(), nil
}

// client is the HTTP surface. It never follows redirects: a session-gated route
// behind an SSO proxy answers with a redirect toward a login page, and
// following it would land the CLI on HTML which it must never try to parse as
// data.
type client struct {
	cfg    config
	http   *http.Client
	stderr io.Writer
	// progName is the name this process was invoked as, so the skew note names
	// the command the reader actually typed. Empty falls back in warnSkew.
	progName string

	skewOnce sync.Once
}

func newClient(cfg config, hc *http.Client, stderr io.Writer) *client {
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client{cfg: cfg, http: hc, stderr: stderr}
}

// request describes one call. Path is already expanded (see expandPath).
type request struct {
	method string
	path   string
	query  url.Values
	body   any
	// headers carries route-specific request headers. It exists for the
	// provenance set (see taskHeaders), which is how the server derives a
	// comment's author — the value is deliberately NOT in the body, so that a
	// producer cannot impersonate `user`/`operator` by sending an author field.
	headers map[string]string
	// cred names WHICH credential to present.
	cred credential
}

// credential names one of muster's caller-side credentials.
//
// 🔴 IT IS AN ENUM WITH TWO VALUES TODAY, AND THE HONEST READING IS THAT THIS
// COSTS NOTHING RATHER THAN THAT IT ALREADY EARNS ITS KEEP. Every verb in the
// ledger presents the hook credential; only the /health skew probe presents
// none. The upstream binary this was extracted from needed three values and the
// enum was load-bearing there. It is kept in this shape because the server
// already has a second caller tier — api.requireHookOrServiceToken admits
// MUSTER_SERVICE_TOKEN plus an asserted actor — so a verb reaching it would add
// a value rather than convert a bool, and the conversion is the step that gets
// got wrong: spelled as booleans the states can both be set, both be unset, or
// disagree, and a request that presented NEITHER credential reads as "the
// surface is broken" rather than "I sent nothing".
type credential int

const (
	// credNone sends no Authorization header. The /health skew probe only.
	credNone credential = iota
	// credHook sends MUSTER_HOOK_TOKEN. On a host that is the shared hook
	// secret; inside a managed agent pod the same variable carries that pod's
	// own row token, which is what the /agent/task* routes resolve.
	credHook
)

// do performs the call and returns the raw JSON body. The body is returned
// VERBATIM (never re-marshalled) so unknown fields from a newer server pass
// straight through to stdout — enforced structurally rather than by remembering
// not to use DisallowUnknownFields.
func (c *client) do(ctx context.Context, req request) ([]byte, error) {
	// The URL is assembled by string concatenation on purpose: url.JoinPath
	// cleans the path, which would paper over exactly the malformed paths the
	// expandPath guard exists to reject.
	raw := c.cfg.apiURL + req.path
	if len(req.query) > 0 {
		raw += "?" + req.query.Encode()
	}

	var bodyReader io.Reader
	if req.body != nil {
		buf, err := json.Marshal(req.body)
		if err != nil {
			return nil, wrapf(exitUsage, err, "could not encode request body")
		}
		bodyReader = bytes.NewReader(buf)
	}

	httpReq, err := http.NewRequestWithContext(ctx, req.method, raw, bodyReader)
	if err != nil {
		return nil, wrapf(exitUsage, err, "could not build request for %s %s", req.method, req.path)
	}
	httpReq.Header.Set("Accept", "application/json")
	if req.body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	// Applied before Authorization so that a future caller cannot displace the
	// token through this map. ⚠ No current caller sets an Authorization key, so
	// the ordering is not observable today — TestRouteHeadersDoNotClobberTheToken
	// is an INVARIANT GUARD and says so; it survives this loop being moved.
	for k, v := range req.headers {
		httpReq.Header.Set(k, v)
	}
	// 🔴 THE CREDENTIAL IS CHOSEN HERE AND NOWHERE ELSE, so a verb cannot pick a
	// secret by writing a header. A missing token is NOT an error: most of the
	// hook tier is enforce-when-set, so an unconfigured pair still works, and the
	// one fail-closed route answers 503 with its own marker (exit 9) rather than
	// a credential complaint.
	if req.cred == credHook && c.cfg.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.cfg.token)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		// 🔴 A DEADLINE IS NOT AN UNREACHABLE NETWORK. http.Client.Timeout and a
		// cancelled context both surface here as a *url.Error, and both satisfy
		// errors.Is(err, context.DeadlineExceeded) / context.Canceled; a refused
		// connection satisfies neither. Reporting an abort as exitNetwork tells the
		// caller the network is down when the request in fact arrived.
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return nil, wrapf(exitAborted, err,
				"%s %s: THIS CLIENT stopped waiting. The request left this process and its outcome is "+
					"UNKNOWN here — it is NOT a network failure and NOT a denial. Raise --timeout rather "+
					"than retrying blindly, because a write that already landed would be repeated.",
				req.method, req.path)
		}
		// The token is never in the URL, so the wrapped transport error cannot
		// leak it.
		return nil, wrapf(exitNetwork, err, "%s %s failed", req.method, req.path)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, wrapf(exitNetwork, err, "reading response body from %s %s", req.method, req.path)
	}
	return classify(req, resp, body)
}

// classify turns an HTTP response into either a JSON body or a cliError with
// the right exit code.
func classify(req request, resp *http.Response, body []byte) ([]byte, error) {
	ct := resp.Header.Get("Content-Type")
	isJSON := strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "application/json")

	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		loc := resp.Header.Get("Location")
		if crossHost(resp.Request, loc) {
			return nil, failf(exitNonJSON,
				"%s %s was redirected off-host to %q and the redirect was NOT followed. That is the "+
					"signature of an SSO proxy in front of muster: a session-gated login page cannot be "+
					"scripted. Point --api-url at the service directly rather than through the proxy.",
				req.method, req.path, loc)
		}
		return nil, failf(exitNonJSON,
			"%s %s returned %d %s (Location: %q); this CLI does not follow redirects because a redirect body is not data",
			req.method, req.path, resp.StatusCode, http.StatusText(resp.StatusCode), loc)

	case (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && !isJSON:
		// 🔴 THE DISCRIMINATOR IS THE BODY, NOT THE STATUS, AND THAT IS A MEASURED
		// CORRECTION RATHER THAN A PREFERENCE. An SSO proxy does not always bounce
		// with a 3xx: given `Accept: application/json` (which this CLI sends) one
		// answers 401 + Content-Type: text/html + a cross-host Location, so the
		// "3xx with a cross-host Location" rule never fires for it.
		//
		// muster's own rejection is application/json; an edge gate's is not. A
		// non-JSON 401/403 therefore means something in FRONT of muster answered,
		// and the useful advice is to bypass the proxy — not "check your token",
		// which sends the caller hunting a credential that is fine.
		return nil, failf(exitNonJSON,
			"%s %s: %d with a non-JSON body (Content-Type %q) — an auth gate in FRONT of muster "+
				"answered, not muster. muster's own refusals are application/json. A session-gated "+
				"portal cannot be scripted; point --api-url at the service directly.",
			req.method, req.path, resp.StatusCode, ct)

	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return nil, failf(exitAuth,
			"%s %s: %d %s — the server rejected the credential this request presented. Check %s in %s (the value is never printed).",
			req.method, req.path, resp.StatusCode, http.StatusText(resp.StatusCode), envToken, defaultEnvFile)

	case resp.StatusCode == http.StatusNotFound:
		// A handler's 404 is JSON ({"error":"task not found"}); the router's own
		// 404 is text/plain "404 page not found". That difference is how a missing
		// RESOURCE (exit 4) is told apart from a missing ROUTE (exit 7, this CLI
		// being newer than the server).
		if isJSON {
			return nil, failf(exitNotFound, "%s %s: not found — %s", req.method, req.path, serverError(body))
		}
		return nil, failf(exitRouteAbsent,
			"route %s %s not found on this server; it may predate the route (this client was built against %s)",
			req.method, req.path, buildVersion)

	case resp.StatusCode == http.StatusConflict:
		return nil, failf(exitConflict, "%s %s: conflict — %s", req.method, req.path, serverError(body))

	case resp.StatusCode == http.StatusServiceUnavailable && unarmedHookSurface(body):
		// 🔴 MOST 503s ON THE HOOK TIER ARE OUTAGES; THIS ONE IS NOT, AND THE
		// DISCRIMINATOR IS THE MARKER, NOT THE STATUS. Nearly every hook-tier route
		// is enforce-when-set, so an unconfigured server SERVES them and a 503 there
		// really is a sick backend (exitServerError below is right for that).
		// api.requireArmedHookToken is the exception — the agent-steering write
		// refuses outright rather than inherit the open default — and its refusal is
		// a CONFIGURATION fact with a different remedy: arm the server.
		return nil, failf(exitUnarmed,
			"%s %s: the muster SERVER refuses to serve this write — %s.\n"+
				"This route is fail-CLOSED on purpose while the rest of the hook tier serves when "+
				"unconfigured. Arm it by setting %s on the muster deployment (an empty or missing "+
				"value produces this state), then re-run. Your own credential was not the problem "+
				"and was not checked.",
			req.method, req.path, serverError(body), envToken)

	case resp.StatusCode >= 500:
		return nil, failf(exitServerError, "%s %s: %d %s — %s", req.method, req.path, resp.StatusCode, http.StatusText(resp.StatusCode), serverError(body))

	case resp.StatusCode >= 400:
		return nil, failf(exitUsage, "%s %s: %d %s — %s", req.method, req.path, resp.StatusCode, http.StatusText(resp.StatusCode), serverError(body))
	}

	if !isJSON {
		return nil, failf(exitNonJSON,
			"%s %s returned %d with Content-Type %q, not JSON. If there is an SSO proxy in front of "+
				"muster, its session-gated pages cannot be scripted — point --api-url at the service directly.",
			req.method, req.path, resp.StatusCode, ct)
	}
	if !json.Valid(body) {
		return nil, failf(exitNonJSON, "%s %s claimed %s but the body is not valid JSON", req.method, req.path, ct)
	}
	return body, nil
}

// crossHost reports whether loc points at a different host than the request —
// the structural signature of an auth-portal bounce.
func crossHost(req *http.Request, loc string) bool {
	if loc == "" || req == nil || req.URL == nil {
		return false
	}
	u, err := url.Parse(loc)
	if err != nil || u.Host == "" {
		return false
	}
	return u.Host != req.URL.Host
}

// hookUnarmedField is the JSON key api.requireArmedHookToken sets on its 503.
// The server owns the spelling as api.HookUnarmedField; this binary does not
// import internal/api, so the two are pinned together by a test rather than by
// a shared constant — see TestTheUnarmedMarkerMatchesTheServer.
const hookUnarmedField = "unarmed"

// unarmedHookSurface reports whether a 503 body is the hook tier's ARMING
// refusal rather than an outage.
//
// 🔴 IT IS A *bool, NOT A bool. With a plain bool "the key is absent" and "the
// key said false" are the same value — and while that direction happens to be
// safe here, the inverse mistake (treating a missing key as present) would
// report a sick server as merely unarmed and send an operator to edit a
// deployment that is already correct.
func unarmedHookSurface(body []byte) bool {
	var out struct {
		Unarmed *bool `json:"unarmed"`
	}
	if json.Unmarshal(body, &out) != nil {
		return false
	}
	return out.Unarmed != nil && *out.Unarmed
}

// serverError pulls the server's {"error": "..."} message out of a JSON error
// body, falling back to a truncated raw body.
func serverError(body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return e.Error
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	if s == "" {
		return "(empty body)"
	}
	return s
}

type healthResponse struct {
	Status  string  `json:"status"`
	Version string  `json:"version"`
	Uptime  float64 `json:"uptime"`
}

// warnSkew probes the open /health route once per process and, when the server
// version differs from the version this CLI was built against, prints ONE note.
//
// 🔴 ONE PROBE AGAINST ONE SERVER IS THE WHOLE CLAIM, AND IT IS SOUND ONLY
// BECAUSE OF THE TWO-BINARY SPLIT. The upstream CLI had this same single probe
// while its verbs were about to straddle two services, which would have made
// the note a statement about whichever one happened to answer /health. Every
// verb here talks to the one base URL in config.apiURL — checked, not assumed,
// by TestEveryWiredRouteIsServedByThisProject — so the probe's subject and the
// verbs' subject are the same process.
//
// 🔴 The note goes to STDERR. stdout carries JSON and nothing else, so a
// pipeline into `jq` is never corrupted. A probe failure is SILENT: it must
// never turn a working command into a failing one.
func (c *client) warnSkew(ctx context.Context) {
	c.skewOnce.Do(func() {
		body, err := c.do(ctx, request{method: http.MethodGet, path: "/health", cred: credNone})
		if err != nil {
			return
		}
		var h healthResponse
		if json.Unmarshal(body, &h) != nil || h.Version == "" || h.Version == buildVersion {
			return
		}
		name := c.progName
		if name == "" {
			name = defaultProgName
		}
		fmt.Fprintf(c.stderr, "note: server %s, %s built for %s\n", h.Version, name, buildVersion)
	})
}
