package officers_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
)

// TestChatRateLimitedReplyForQuotesThePoolsReset: a usage limit carrying the
// pool's earliest reset (officers.PoolReset) quotes THAT, not the reset the
// CLI stated for the last account tried; a PoolReset with no time falls back
// to the CLI's; and a PoolReset never turns a transient rate limit into "at
// their usage limit".
func TestChatRateLimitedReplyForQuotesThePoolsReset(t *testing.T) {
	usage := fmt.Errorf("%w: %w: rate_limited (You've hit your weekly limit · resets Nov 24, 2pm (UTC))",
		officers.ErrChatRateLimited, officers.ErrChatUsageLimit)
	transient := fmt.Errorf("%w: rate_limited (API Error: Request rate limited)", officers.ErrChatRateLimited)
	earliest := time.Date(2026, time.November, 23, 1, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"the pool's earliest reset", &officers.PoolReset{At: earliest, Err: usage},
			"(The officer can't reply right now: its Claude accounts are at their usage limit (resets Nov 23, 1am (UTC)). Ask again later.)"},
		{"wrapped again", fmt.Errorf("turn: %w", &officers.PoolReset{At: earliest, Err: usage}),
			"(The officer can't reply right now: its Claude accounts are at their usage limit (resets Nov 23, 1am (UTC)). Ask again later.)"},
		{"no pool reset: the CLI's", usage,
			"(The officer can't reply right now: its Claude accounts are at their usage limit (resets Nov 24, 2pm (UTC)). Ask again later.)"},
		{"a zero pool reset: the CLI's", &officers.PoolReset{Err: usage},
			"(The officer can't reply right now: its Claude accounts are at their usage limit (resets Nov 24, 2pm (UTC)). Ask again later.)"},
		{"a transient rate limit", &officers.PoolReset{At: earliest, Err: transient},
			"(The officer is being rate-limited right now. Ask again later.)"},
	} {
		if got := officers.ChatRateLimitedReplyFor(c.err); got != c.want {
			t.Errorf("%s: ChatRateLimitedReplyFor = %q\nwant %q", c.name, got, c.want)
		}
	}
}
