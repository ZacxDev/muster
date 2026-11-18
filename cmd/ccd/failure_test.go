package main

import (
	"net/http"
	"strings"
	"testing"
)

// The status/type table is the contract a muster-side account pool keys on, so it
// is pinned literally rather than derived.
func TestClassifyAPIError(t *testing.T) {
	cases := []struct {
		in     apiError
		status int
		typ    string
	}{
		{apiError{Code: "authentication_failed", Status: 401}, 502, failAuth},
		{apiError{Code: "authentication_failed"}, 502, failAuth}, // "Not logged in": no status
		{apiError{Code: "rate_limit", Status: 429}, 429, failRateLimited},
		{apiError{Code: "rate_limit"}, 429, failRateLimited},
		{apiError{Code: "billing_error", Status: 400}, 502, failBilling},
		{apiError{Code: "server_error", Status: 500}, 502, failTurn},
		{apiError{Code: "model_not_found", Status: 404}, 502, failTurn},
		{apiError{Code: "unknown"}, 502, failTurn},
		{apiError{Status: 429}, 429, failRateLimited}, // code-less fallbacks
		{apiError{Status: 401}, 502, failAuth},
		{apiError{Status: 403}, 502, failAuth},
		{apiError{}, 502, failTurn},
	}
	for _, c := range cases {
		f := classifyAPIError(c.in)
		if f.Status != c.status || f.Type != c.typ {
			t.Errorf("%+v -> %d %s, want %d %s", c.in, f.Status, f.Type, c.status, c.typ)
		}
		if f.Message == "" {
			t.Errorf("%+v -> empty message", c.in)
		}
	}
}

// 🔴 NO FAILURE MAY BE A 404 OR A 2xx: muster's client reads 404 as "this runtime
// has no /v1/responses" and retries the turn on a transport ccd does not serve,
// and any 2xx is read as an answer.
func TestNoFailureIsA404OrASuccess(t *testing.T) {
	for _, code := range []string{"authentication_failed", "rate_limit", "billing_error", "server_error", "model_not_found", "unknown", ""} {
		for _, st := range []int{0, 200, 401, 403, 404, 429, 500} {
			f := classifyAPIError(apiError{Code: code, Status: st})
			if f.Status == http.StatusNotFound || f.Status < 400 {
				t.Errorf("code=%q status=%d classified as HTTP %d", code, st, f.Status)
			}
		}
	}
}

// observedEdgeChallenge is the CLI's message for a request an edge/CDN challenge
// page answered (the CLI renders the HTML body by its <title>), verbatim from a
// failed turn. It came with error=authentication_failed and status 403.
const observedEdgeChallenge = "Please run /login · API Error: 403 Attention Required! | Cloudflare"

// 🔴 AN EDGE CHALLENGE IS NOT A CREDENTIAL REJECTION. auth_failed makes muster
// mark the account until its token is replaced; a CDN challenge page never
// reached the API, so it says nothing about the credential and must come back turn_failed — which no
// consumer marks an account for — whatever code and status the CLI attached.
func TestAnEdgeChallengePageIsNotAnAuthFailure(t *testing.T) {
	for _, in := range []apiError{
		{Code: "authentication_failed", Status: 403, Message: observedEdgeChallenge}, // the transcript record
		{Code: "authentication_failed", Message: observedEdgeChallenge},              // the StopFailure hook: no status
		{Status: 403, Message: observedEdgeChallenge},                                // code-less status fallback
		{Status: 401, Message: observedEdgeChallenge},
	} {
		f := classifyAPIError(in)
		if f.Status != http.StatusBadGateway || f.Type != failTurn {
			t.Errorf("%+v -> %d %s, want 502 %s", in, f.Status, f.Type, failTurn)
		}
		// The CLI's own code and the upstream status still reach the log line.
		if f.Code != in.Code || f.Upstream != in.Status {
			t.Errorf("%+v -> code=%q upstream=%d, want them carried through", in, f.Code, f.Upstream)
		}
		if !strings.Contains(f.Message, observedEdgeChallenge) {
			t.Errorf("%+v -> message %q lost the CLI's text", in, f.Message)
		}
	}
}

// The other side of the predicate: a real rejection still marks, including the
// cases that mention Cloudflare (or half the page title) without being the
// challenge page. Decision pinned here: only the page's exact title signature
// is an edge challenge; the word "Cloudflare" alone is not, and neither is a
// case-folded spelling (the page title is fixed text).
func TestARealAuthRejectionStillFailsAuthEvenIfItMentionsCloudflare(t *testing.T) {
	for _, in := range []apiError{
		{Code: "authentication_failed", Status: 401, Message: "Please run /login · API Error: 401 OAuth access token is invalid."},
		{Code: "authentication_failed", Status: 403, Message: `Please run /login · API Error: 403 {"type":"error","error":{"type":"permission_error","message":"OAuth token has been revoked"}}`},
		{Code: "authentication_failed", Message: "Not logged in · Please run /login"},
		{Code: "authentication_failed", Status: 401, Message: "Please run /login · API Error: 401 invalid token (served via Cloudflare)"},
		{Code: "authentication_failed", Status: 403, Message: "Please run /login · API Error: 403 Attention Required!"},
		{Code: "authentication_failed", Status: 403, Message: "Please run /login · API Error: 403 Cloudflare"},
		{Code: "authentication_failed", Status: 403, Message: "Please run /login · API Error: 403 attention required! | cloudflare"},
		{Status: 403, Message: "API Error: 403 blocked by Cloudflare"},
	} {
		if f := classifyAPIError(in); f.Status != http.StatusBadGateway || f.Type != failAuth {
			t.Errorf("%+v -> %d %s, want 502 %s", in, f.Status, f.Type, failAuth)
		}
	}
}

// The tracker behind /healthz: an edge challenge says nothing about the
// credential, so it leaves the last verdict alone — it neither flips an `ok`
// account to auth_failed nor clears a real auth_failed.
func TestAnEdgeChallengeLeavesTheAuthStateAlone(t *testing.T) {
	for _, prior := range []string{authUnknown, authOK, authFailed} {
		a := newAuthTracker()
		if prior != authUnknown {
			a.set(prior, "before")
		}
		a.observeTurn(classifyAPIError(apiError{Code: "authentication_failed", Status: 403, Message: observedEdgeChallenge}))
		if got := a.snapshot().Auth; got != prior {
			t.Errorf("auth %q -> %q after an edge challenge, want unchanged", prior, got)
		}
	}
}
