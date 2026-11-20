package officers_test

import (
	"fmt"
	"testing"

	"github.com/ZacxDev/muster/internal/officers"
)

// A 429 IS NOT ALWAYS THE ACCOUNTS' USAGE LIMIT. The CLI gives a transient
// request rate limit ("API Error: Request rate limited") the same type and
// code as "You've hit your weekly limit", and the runner used to answer both
// with "its Claude accounts are at their usage limit". internal/api's
// chatTurnError now wraps ErrChatUsageLimit only on a usage limit (ccd's
// usage_limit), so a plain 429 arrives as ErrChatRateLimited alone.

// transientBody is ccd's answer for a transient 429: rate_limited, the CLI's
// code, no usage_limit, and the CLI's own text for it.
const transientBody = `{"error":{"type":"rate_limited","code":"rate_limit","upstream_status":429,"message":"API Error: Request rate limited"}}`

func transientRateLimit() error {
	return fmt.Errorf("%w: responses HTTP 429: %s (API Error: Request rate limited)", officers.ErrChatRateLimited, transientBody)
}

// TestATransientRateLimitIsNotToldTheAccountsAreAtTheirLimit: three typed
// turns in a row failing a plain 429 (no ErrChatUsageLimit) back off and are
// answered — the same bound as a usage limit — but with the rate-limited
// reply, which neither says the accounts are at their usage limit nor quotes
// a reset.
//
// RED before the fix: the third 429 was answered "(The officer can't reply
// right now: its Claude accounts are at their usage limit. Ask again
// later.)".
func TestATransientRateLimitIsNotToldTheAccountsAreAtTheirLimit(t *testing.T) {
	r, clk := clockRig(t, 1)
	r.hq.AddChat(relID, "c-42", "triage")
	seq, id := r.hq.MemberSays("c-42", "member2", "Member Two", "Is the queue draining?")
	r.lanes.reply = func(string, string) (string, error) { return "", transientRateLimit() }
	ran := passes(t, r, clk, id, 40)
	if st := r.hq.Item(id).Status; st != "done" {
		t.Fatalf("after %d passes the rate-limited chat's item is %s with %d replies; want it answered and done (log:\n%s)", ran, st, len(r.officerReplies("c-42")), r.logs)
	}
	want := "(The officer is being rate-limited right now. Ask again in a few minutes.)"
	replies := r.officerReplies("c-42")
	if len(replies) != 1 || replies[0].Text != want || replies[0].InReplyTo != seq {
		t.Fatalf("replies %+v; want one, %q, in reply to %d (a transient 429 is not the accounts' usage limit)", replies, want, seq)
	}
	if n := len(r.lanes.sent()); n != 3 {
		t.Fatalf("%d turns sent to the lane; want 3 (the bound) before the member is told", n)
	}
}

// TestTheRateLimitedReplyFollowsTheUsageLimitMark: the reply is chosen by
// ErrChatUsageLimit alone — the same text with and without it gets the two
// different replies, so neither the text nor the outer error decides.
func TestTheRateLimitedReplyFollowsTheUsageLimitMark(t *testing.T) {
	text := "responses HTTP 429: You've hit your weekly limit · resets Nov 23, 1am (UTC)"
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"usage limit", fmt.Errorf("%w: %w: %s", officers.ErrChatRateLimited, officers.ErrChatUsageLimit, text),
			"(The officer can't reply right now: its Claude accounts are at their usage limit (resets Nov 23, 1am (UTC)). Ask again later.)"},
		{"unmarked rate limit", fmt.Errorf("%w: %s", officers.ErrChatRateLimited, text),
			"(The officer is being rate-limited right now. Ask again in a few minutes.)"},
		{"unmarked, parked (409)", fmt.Errorf("%w: %w (nothing was typed): %s", officers.ErrChatLaneUnavailable, officers.ErrChatRateLimited, text),
			"(The officer is being rate-limited right now. Ask again in a few minutes.)"},
	} {
		if got := officers.ChatRateLimitedReplyFor(c.err); got != c.want {
			t.Errorf("%s: ChatRateLimitedReplyFor(%v) = %q; want %q", c.name, c.err, got, c.want)
		}
	}
}
