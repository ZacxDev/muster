package ccpool

import "testing"

// TestOnlyAUsageLimitIsAnAccountFailure pins the one rule every consumer of a
// typed failure takes its answer from: a rate_limited is the account's usage
// limit only when ccd marked it usage_limit or answered it with the limit
// menu's 409. A plain 429 — a transient request rate limit, or a usage limit
// from a ccd that predates the mark — says nothing about the account. An
// auth_failed always does; nothing else ever does.
func TestOnlyAUsageLimitIsAnAccountFailure(t *testing.T) {
	for _, c := range []struct {
		name       string
		typ        string
		usageLimit bool
		status     int
		want       string
	}{
		{"a plain 429", "rate_limited", false, 429, ""},
		{"a 429 marked usage_limit", "rate_limited", true, 429, FailureRateLimited},
		{"the limit menu's 409", "rate_limited", false, 409, FailureRateLimited},
		{"the limit menu's 409, marked", "rate_limited", true, 409, FailureRateLimited},
		{"auth_failed", "auth_failed", false, 502, FailureAuthFailed},
		{"busy's 409 is not a limit", "busy", false, 409, ""},
		{"a mark on another type means nothing", "turn_failed", true, 502, ""},
		{"untyped", "", true, 429, ""},
	} {
		if got := AccountFailure(c.typ, c.usageLimit, c.status); got != c.want {
			t.Errorf("%s: AccountFailure(%q, %v, %d) = %q, want %q", c.name, c.typ, c.usageLimit, c.status, got, c.want)
		}
	}
}
