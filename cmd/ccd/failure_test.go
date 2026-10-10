package main

import (
	"net/http"
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
