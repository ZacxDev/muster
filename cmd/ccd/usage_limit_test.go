package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// 🔴 A 429 IS A USAGE LIMIT ONLY WHEN THE CLI SAYS ONE WAS HIT. The CLI codes
// a transient "API Error: Request rate limited" rate_limit exactly like "You've
// hit your weekly limit", so both are rate_limited — and only the usage limit
// is marked usage_limit, which is what muster tells a member "the accounts are
// at their usage limit" on. The limit menu is always a usage limit.
func TestOnlyAUsageLimitIsMarkedUsageLimit(t *testing.T) {
	for _, c := range []struct {
		in    apiError
		usage bool
	}{
		{apiError{Code: "rate_limit", Status: 429, Message: "You've hit your weekly limit · resets Nov 23, 1am (UTC)"}, true},
		{apiError{Code: "rate_limit", Message: "Claude usage limit reached. Your limit will reset at 3pm (America/Los_Angeles)."}, true},
		{apiError{Status: 429, Message: "You've hit your session limit · resets 9:45pm (UTC)"}, true},
		{apiError{Code: "rate_limit", Status: 429, Message: "API Error: Request rate limited"}, false},
		// "limit reached" without a usage window's name is a transient limit.
		{apiError{Code: "rate_limit", Status: 429, Message: "API Error: Rate limit reached for requests per minute"}, false},
		{apiError{Status: 429, Message: "Rate limit reached"}, false},
		{apiError{Code: "rate_limit", Message: "5-hour limit reached ∙ resets 3pm"}, true},
		{apiError{Status: 429, Message: "API Error: 429 too many requests"}, false},
		{apiError{Code: "rate_limit"}, false},
		// Not rate_limited at all: never marked, whatever the text says.
		{apiError{Code: "server_error", Status: 500, Message: "You've hit your weekly limit"}, false},
	} {
		f := classifyAPIError(c.in)
		if f.UsageLimit != c.usage {
			t.Errorf("%+v -> %s usage_limit %v, want %v", c.in, f.Type, f.UsageLimit, c.usage)
		}
	}
	if f := limitMenuFailure(&limitMenuShown{what: "a menu"}); !f.UsageLimit {
		t.Errorf("the limit menu's failure %+v is not marked usage_limit", f)
	}
}

// TestMustersClientReadsTheUsageLimitMark is the wire between the two: a
// rate-limited turn through ccd, read back by muster's own client, carries
// usage_limit for the CLI's usage-limit record and not for a transient
// "Request rate limited" record of the same code and status.
func TestMustersClientReadsTheUsageLimitMark(t *testing.T) {
	limited := fixture(t, "turn_rate_limited.jsonl")
	transient := bytes.Replace(limited, []byte("You've hit your session limit · resets 9:45pm (UTC)"), []byte("API Error: Request rate limited"), 1)
	if bytes.Equal(transient, limited) {
		t.Fatal("CONTROL: the fixture's limit text was not replaced")
	}
	for _, c := range []struct {
		name       string
		transcript []byte
		usage      bool
	}{
		{"usage limit", limited, true},
		{"transient 429", transient, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, _ := newScripted(t, cliScript{transcript: c.transcript, stopEvent: "StopFailure"})
			_, err := mustersGateway(t, gatewayServer(t, srv).URL).Chat(context.Background(), contractAgent, "s", "hi", nil)
			var rt *agents.RuntimeError
			if !errors.As(err, &rt) || rt.Status != http.StatusTooManyRequests || rt.Type != failRateLimited {
				t.Fatalf("err %v; want a 429 %s", err, failRateLimited)
			}
			if rt.UsageLimit != c.usage {
				t.Fatalf("muster's client read usage_limit %v (message %q); want %v", rt.UsageLimit, rt.Message, c.usage)
			}
		})
	}
}
