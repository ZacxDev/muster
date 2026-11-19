package api

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
)

// stepClock is a settable clock for the wake loop.
type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *stepClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *stepClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// TestARateLimitedChatLaneIsAnsweredThroughTheWakeLoop is the seam the unit
// tests cannot see: the wake loop, the real store, the chat lanes' reading of
// the gateway's errors (chatTurnError) and the runner's bound together, on the
// production sequence — the lane's turn fails 429 rate_limited, then every
// turn is refused 409 rate_limited by a pane parked on the usage-limit menu.
// Within the bound the member is answered with the pinned error reply, the
// item is done, and no later pass sends the lane anything.
//
// RED before the fix: the 409s read as a busy lane, so the item is claimed
// and released every pass and is still open, unanswered, after 20 passes.
func TestARateLimitedChatLaneIsAnsweredThroughTheWakeLoop(t *testing.T) {
	clk := &stepClock{t: time.Now()}
	c := newChatPGAt(t, clk.now)
	c.hq.AddChat(offB, "c-41", "triage")
	seq, id := c.hq.MemberSays("c-41", "member1", "Member One", "Is the API healthy?")
	limit := "You've hit your weekly limit · resets Nov 23, 1am (UTC)"
	calls := 0
	c.chat.reply = func() (string, error) {
		calls++
		if calls == 1 {
			return "", &agents.RuntimeError{Status: http.StatusTooManyRequests, Type: "rate_limited", Code: "rate_limit", Message: limit}
		}
		return "", &agents.RuntimeError{Status: http.StatusConflict, Type: "rate_limited", Code: "rate_limit", Message: limit}
	}
	for i := 0; i < 20 && c.hq.Item(id).Status != "done"; i++ {
		c.pass(t)
		clk.add(time.Minute)
	}
	if st := c.hq.Item(id).Status; st != "done" {
		t.Fatalf("after 20 passes the rate-limited chat's item is %s (%d lane turns); want it answered and done", st, len(c.chat.sent()))
	}
	want := "(The officer can't reply right now: its Claude accounts are at their usage limit (resets Nov 23, 1am (UTC)). Ask again later.)"
	msgs := c.hq.ChatMessages("c-41")
	if len(msgs) != 2 || msgs[1].Role != "officer" || msgs[1].InReplyTo != seq || msgs[1].Text != want {
		t.Fatalf("chat after the bound: %+v; want the member's turn and one officer reply %q to %d", msgs, want, seq)
	}
	sent := len(c.chat.sent())
	if sent != 3 {
		t.Fatalf("%d lane turns; want 3 (the bound)", sent)
	}
	for i := 0; i < 3; i++ {
		c.pass(t)
		clk.add(time.Minute)
	}
	if n := len(c.chat.sent()); n != sent {
		t.Fatalf("passes after the chat was answered sent the lane %d more turns; want none", n-sent)
	}
}
