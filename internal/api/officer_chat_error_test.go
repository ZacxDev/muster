package api

import (
	"errors"
	"net/http"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
)

// TestAChatTurnsRateLimitIsNotABusyLane pins how a chat lane's gateway error
// reaches the runner (chatTurnError). 🔴 The 409 rate_limited of a pane parked
// on the CLI's usage-limit menu is unsent like a busy lane — but rate-limited,
// which a busy lane never is, so the runner bounds it instead of retrying it
// every pass; a 429 rate_limited (the turn was typed) is rate-limited and NOT
// unsent. The rest keep their old reading.
//
// RED before the fix on the first row: the parked pane's 409 was a busy lane
// (ErrChatLaneUnavailable alone).
func TestAChatTurnsRateLimitIsNotABusyLane(t *testing.T) {
	limit := "You've hit your weekly limit · resets Nov 23, 1am (UTC)"
	for _, c := range []struct {
		name                             string
		err                              error
		unsent, unavailable, broken, rlm bool
	}{
		{"409 rate_limited (limit menu)", &agents.RuntimeError{Status: http.StatusConflict, Type: "rate_limited", Code: "rate_limit", Message: limit}, true, true, false, true},
		{"429 rate_limited (typed turn)", &agents.RuntimeError{Status: http.StatusTooManyRequests, Type: "rate_limited", Code: "rate_limit", Message: limit}, false, false, false, true},
		{"409 busy", &agents.RuntimeError{Status: http.StatusConflict, Type: "busy"}, true, true, false, false},
		{"503 capacity", &agents.RuntimeError{Status: http.StatusServiceUnavailable, Type: agents.FailureCapacity}, true, true, false, false},
		{"401 unauthorized", &agents.RuntimeError{Status: http.StatusUnauthorized, Type: "unauthorized"}, true, false, true, false},
		{"502 turn_failed", &agents.RuntimeError{Status: http.StatusBadGateway, Type: "turn_failed"}, false, false, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, unsent := chatTurnError(c.err)
			if unsent != c.unsent || errors.Is(got, officers.ErrChatLaneUnavailable) != c.unavailable ||
				errors.Is(got, officers.ErrChatLaneBroken) != c.broken || errors.Is(got, officers.ErrChatRateLimited) != c.rlm {
				t.Fatalf("chatTurnError(%v) = %v (unsent %v); want unsent %v, unavailable %v, broken %v, rate-limited %v",
					c.err, got, unsent, c.unsent, c.unavailable, c.broken, c.rlm)
			}
		})
	}
	if got, unsent := chatTurnError(nil); got != nil || unsent {
		t.Fatalf("chatTurnError(nil) = %v, %v; want nil, false", got, unsent)
	}
}
