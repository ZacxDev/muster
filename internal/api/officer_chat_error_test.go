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
		{"429 rate_limited (typed turn)", &agents.RuntimeError{Status: http.StatusTooManyRequests, Type: "rate_limited", Code: "rate_limit", Message: limit, UsageLimit: true}, false, false, false, true},
		{"409 busy", &agents.RuntimeError{Status: http.StatusConflict, Type: "busy"}, true, true, false, false},
		{"503 capacity", &agents.RuntimeError{Status: http.StatusServiceUnavailable, Type: agents.FailureCapacity}, true, true, false, false},
		{"401 unauthorized", &agents.RuntimeError{Status: http.StatusUnauthorized, Type: "unauthorized"}, true, false, true, false},
		{"502 turn_failed", &agents.RuntimeError{Status: http.StatusBadGateway, Type: "turn_failed"}, false, false, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, unsent := chatTurnError(c.err)
			if busy := c.name == "409 busy"; errors.Is(got, officers.ErrChatLaneBusy) != busy {
				t.Fatalf("chatTurnError(%v) = %v; want ErrChatLaneBusy %v (only a 409 busy is a turn in the lane)", c.err, got, busy)
			}
			if unsent != c.unsent || errors.Is(got, officers.ErrChatLaneUnavailable) != c.unavailable ||
				errors.Is(got, officers.ErrChatLaneBroken) != c.broken || errors.Is(got, officers.ErrChatRateLimited) != c.rlm {
				t.Fatalf("chatTurnError(%v) = %v (unsent %v); want unsent %v, unavailable %v, broken %v, rate-limited %v",
					c.err, got, unsent, c.unsent, c.unavailable, c.broken, c.rlm)
			}
		})
	}
	// The reset the runtime's message states reaches the runner's error text
	// (what ChatRateLimitedReply quotes), even when the body snippet is empty.
	for _, status := range []int{http.StatusConflict, http.StatusTooManyRequests} {
		got, _ := chatTurnError(&agents.RuntimeError{Status: status, Type: "rate_limited", Code: "rate_limit", Message: limit, UsageLimit: true})
		if want := "(The officer can't reply right now: its Claude accounts are at their usage limit (resets Nov 23, 1am (UTC)). Ask again later.)"; officers.ChatRateLimitedReplyFor(got) != want {
			t.Fatalf("%d rate_limited: the runner reads %q; want the reply %q", status, got.Error(), want)
		}
	}
	if got, unsent := chatTurnError(nil); got != nil || unsent {
		t.Fatalf("chatTurnError(nil) = %v, %v; want nil, false", got, unsent)
	}
}

// TestAPlain429IsNotTheAccountsUsageLimit: a typed rate_limited is the
// account's usage limit (officers.ErrChatUsageLimit) only when the runtime says
// so — ccd's usage_limit — or it is the 409 only the usage-limit menu answers;
// a 429 without usage_limit is rate-limited and nothing more, WHATEVER its code
// or text says (the CLI codes a transient "Request rate limited" rate_limit
// too). Through the runner's reply (ChatRateLimitedReplyFor) the member is
// told the accounts are at their limit only in the first case.
//
// RED before the fix: every typed rate_limited was answered "at their usage
// limit".
func TestAPlain429IsNotTheAccountsUsageLimit(t *testing.T) {
	const (
		usage    = "(The officer can't reply right now: its Claude accounts are at their usage limit (resets Nov 23, 1am (UTC)). Ask again later.)"
		usageNoR = "(The officer can't reply right now: its Claude accounts are at their usage limit. Ask again later.)"
		plain    = "(The officer is being rate-limited right now. Ask again later.)"
	)
	limit := "You've hit your weekly limit · resets Nov 23, 1am (UTC)"
	for _, c := range []struct {
		name   string
		err    *agents.RuntimeError
		usage  bool
		unsent bool
		reply  string
	}{
		{"429 usage_limit", &agents.RuntimeError{Status: http.StatusTooManyRequests, Type: "rate_limited", Code: "rate_limit", UpstreamStatus: 429, Message: limit, UsageLimit: true}, true, false, usage},
		{"429 transient, CLI-coded", &agents.RuntimeError{Status: http.StatusTooManyRequests, Type: "rate_limited", Code: "rate_limit", UpstreamStatus: 429, Message: "API Error: Request rate limited"}, false, false, plain},
		{"429 transient, status only", &agents.RuntimeError{Status: http.StatusTooManyRequests, Type: "rate_limited", UpstreamStatus: 429, Message: "too many requests"}, false, false, plain},
		// Not the code, not the text: unmarked, even this one is not a usage limit.
		{"429 unmarked, limit text", &agents.RuntimeError{Status: http.StatusTooManyRequests, Type: "rate_limited", Code: "rate_limit", Message: limit}, false, false, plain},
		{"409 limit menu", &agents.RuntimeError{Status: http.StatusConflict, Type: "rate_limited", Code: "rate_limit", Message: "the CLI's pane shows its usage-limit menu", UsageLimit: true}, true, true, usageNoR},
		{"409 limit menu, a ccd before usage_limit", &agents.RuntimeError{Status: http.StatusConflict, Type: "rate_limited", Code: "rate_limit", Message: limit}, true, true, usage},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, unsent := chatTurnError(c.err)
			if !errors.Is(got, officers.ErrChatRateLimited) || unsent != c.unsent {
				t.Fatalf("chatTurnError(%+v) = %v (unsent %v); want rate-limited, unsent %v", c.err, got, unsent, c.unsent)
			}
			if errors.Is(got, officers.ErrChatUsageLimit) != c.usage {
				t.Fatalf("chatTurnError(%+v) = %v; want usage limit %v", c.err, got, c.usage)
			}
			if r := officers.ChatRateLimitedReplyFor(got); r != c.reply {
				t.Fatalf("the member is answered %q; want %q", r, c.reply)
			}
		})
	}
}
