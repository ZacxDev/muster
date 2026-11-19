package officers_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
)

// A CHAT THAT CANNOT BE ANSWERED IS TOLD SO, NEVER RETRIED FOR EVER.
//
// In production an officer's chat lane hit its Claude account's weekly limit:
// the turn failed 429 rate_limited, its CLI then sat on the usage-limit menu,
// and every later turn was refused 409 rate_limited before anything was typed,
// which the runner read as a busy lane — so the chat's item was claimed and
// released every pass, for as long as the limit lasted, and the member never
// got a reply or an error.

// The runtime's answers as internal/api's chatTurnError hands them on: a
// turn that was typed and failed on the limit, then a pane parked on the
// usage-limit menu refusing the paste.
const limitBody = `{"error":{"type":"rate_limited","code":"rate_limit","upstream_status":429,"message":"You've hit your weekly limit · resets Nov 23, 1am (UTC)"}}`

func typedRateLimit() error {
	return fmt.Errorf("%w: responses HTTP 429: %s", officers.ErrChatRateLimited, limitBody)
}

func parkedRateLimit() error {
	return fmt.Errorf("%w: %w (nothing was typed): responses HTTP 409: %s", officers.ErrChatLaneUnavailable, officers.ErrChatRateLimited, limitBody)
}

// passes runs up to max wake-loop passes a minute apart — each one Dispatch
// over the chat item while HQ still has it open — and answers how many ran
// before the item was done (max when it never was).
func passes(t *testing.T, r *chatRig, clk *clock, id string, max int) int {
	t.Helper()
	for i := 0; i < max; i++ {
		if r.hq.Item(id).Status == "done" {
			return i
		}
		obs := officers.Observation{Chats: true, Open: []officers.Item{r.item(t, id)}, Instances: []officers.InstanceState{upInstance(r.o, 1)}}
		r.runner.Dispatch(context.Background(), r.o, obs, clk.Now())
		r.runner.Wait()
		clk.Advance(time.Minute)
	}
	return max
}

// TestARateLimitedChatIsAnsweredWithWhy: the production sequence — one typed
// turn failing 429 rate_limited, then the parked pane's 409s. The chat backs
// off, and its third rate-limited turn in a row is answered with the pinned
// error reply, in reply to the member's turn: HQ completes the item and no
// later pass claims it or sends the lane anything.
//
// RED before the fix: the 409s read as a busy lane, the item is released and
// claimed every pass, and after 40 passes it is still open with no reply.
func TestARateLimitedChatIsAnsweredWithWhy(t *testing.T) {
	r, clk := clockRig(t, 1)
	r.hq.AddChat(relID, "c-41", "triage")
	seq, id := r.hq.MemberSays("c-41", "member1", "Member One", "Is web healthy?")
	calls := 0
	r.lanes.reply = func(string, string) (string, error) {
		calls++
		if calls == 1 {
			return "", typedRateLimit()
		}
		return "", parkedRateLimit()
	}
	ran := passes(t, r, clk, id, 40)
	if st := r.hq.Item(id).Status; st != "done" {
		t.Fatalf("after %d passes the rate-limited chat's item is %s with %d replies; want it answered and done (log:\n%s)", ran, st, len(r.officerReplies("c-41")), r.logs)
	}
	want := "(The officer can't reply right now: its Claude accounts are at their usage limit (resets Nov 23, 1am (UTC)). Ask again later.)"
	replies := r.officerReplies("c-41")
	if len(replies) != 1 || replies[0].Text != want || replies[0].InReplyTo != seq {
		t.Fatalf("replies %+v; want one, %q, in reply to %d", replies, want, seq)
	}
	if n := len(r.lanes.sent()); n != 3 {
		t.Fatalf("%d turns sent to the lane; want 3 (the bound) before the member is told", n)
	}
	if m := r.lanes.marksOf("c-41"); m.Seen != m.Answered {
		t.Fatalf("marks after the chat was answered with why: %+v; want the turn marked answered (no later claim asks the lane to answer it again)", m)
	}
	// The member asks again later and the limit has reset: the lane answers.
	r.lanes.reply = nil
	seq2, id2 := r.hq.MemberSays("c-41", "member1", "Member One", "And now?")
	clk.Advance(time.Hour)
	passes(t, r, clk, id2, 3)
	replies = r.officerReplies("c-41")
	if len(replies) != 2 || replies[1].InReplyTo != seq2 || replies[1].Text != "reply to c-41" {
		t.Fatalf("after the limit reset, replies %+v; want the lane's reply to %d", replies, seq2)
	}
}

// laneBusy is the runtime's 409 busy as internal/api's chatTurnError hands it on.
func laneBusy() error {
	return fmt.Errorf("%w: %w (busy)", officers.ErrChatLaneUnavailable, officers.ErrChatLaneBusy)
}

// TestABusyLaneWithATurnInFlightIsWaitedFor: a typed turn fails (the lane has
// the member's message and owes its answer), then the lane refuses 15 turns
// in a row as busy — its own turn still running — and then answers. The
// lane's answer is posted; the member is never told the chat is unavailable.
//
// RED at 031967f3b: the 15th busy refusal posted the canned reply and marked
// the turn answered, so the lane's answer was dropped.
func TestABusyLaneWithATurnInFlightIsWaitedFor(t *testing.T) {
	r, clk := clockRig(t, 1)
	r.hq.AddChat(relID, "c-41", "triage")
	seq, id := r.hq.MemberSays("c-41", "member1", "Member One", "Is web healthy?")
	calls := 0
	r.lanes.reply = func(string, string) (string, error) {
		calls++
		switch {
		case calls == 1:
			return "", fmt.Errorf("the turn ran past its deadline")
		case calls <= 16:
			return "", laneBusy()
		}
		return "the lane's answer", nil
	}
	passes(t, r, clk, id, 40)
	replies := r.officerReplies("c-41")
	if len(replies) != 1 || replies[0].Text != "the lane's answer" || replies[0].InReplyTo != seq {
		t.Fatalf("replies %+v; want only the lane's answer, to %d (a busy lane owing a turn is waited for)", replies, seq)
	}
}

// TestABusyLaneWithATurnInFlightStillEnds: the same lane refusing busy for
// ever gives up at the first refusal chatMaxBusyWait (75 min) after the
// streak's first — not at 15 refusals, and not never.
func TestABusyLaneWithATurnInFlightStillEnds(t *testing.T) {
	r, clk := clockRig(t, 1)
	r.hq.AddChat(relID, "c-41", "triage")
	_, id := r.hq.MemberSays("c-41", "member1", "Member One", "Is web healthy?")
	calls := 0
	r.lanes.reply = func(string, string) (string, error) {
		calls++
		if calls == 1 {
			return "", fmt.Errorf("the turn ran past its deadline")
		}
		return "", laneBusy()
	}
	passes(t, r, clk, id, 120)
	replies := r.officerReplies("c-41")
	// The failed turn, its backoff's pass, then busy refusals a minute apart
	// from minute 2: the 76th refusal is the first 75 minutes after the first.
	want := "(The officer can't reply right now: its chat session refused 76 tries in a row (busy or not ready). Ask again later.)"
	if r.hq.Item(id).Status != "done" || len(replies) != 1 || replies[0].Text != want {
		t.Fatalf("after 120 passes: item %s, replies %+v; want done with %q", r.hq.Item(id).Status, replies, want)
	}
}

// TestABusyLaneOwingNoTurnIsCounted: busy with nothing typed-and-unanswered
// (an operator's turn, a dialog) is bounded by the count, 15.
func TestABusyLaneOwingNoTurnIsCounted(t *testing.T) {
	r, clk := clockRig(t, 1)
	r.hq.AddChat(relID, "c-41", "triage")
	_, id := r.hq.MemberSays("c-41", "member1", "Member One", "Is web healthy?")
	r.lanes.reply = func(string, string) (string, error) { return "", laneBusy() }
	passes(t, r, clk, id, 40)
	if n := len(r.lanes.sent()); r.hq.Item(id).Status != "done" || n != 15 {
		t.Fatalf("item %s after %d turns; want done after 15", r.hq.Item(id).Status, n)
	}
}

// TestAStreakIsForgottenWhenItsChatHasNoItem: 14 refusals, then a pass in
// which the chat has no pending item (answered or closed elsewhere): its next
// item starts a new count, so one more refusal does not give up.
//
// RED at 031967f3b: the stale count of 14 made the next item's first refusal
// the 15th.
func TestAStreakIsForgottenWhenItsChatHasNoItem(t *testing.T) {
	r, clk := clockRig(t, 1)
	r.hq.AddChat(relID, "c-41", "triage")
	_, id := r.hq.MemberSays("c-41", "member1", "Member One", "Is web healthy?")
	r.lanes.reply = func(string, string) (string, error) {
		return "", fmt.Errorf("%w (not_ready)", officers.ErrChatLaneUnavailable)
	}
	passes(t, r, clk, id, 14)
	if n := len(r.lanes.sent()); n != 14 {
		t.Fatalf("precondition: %d turns; want 14", n)
	}
	r.runner.Dispatch(context.Background(), r.o, officers.Observation{Chats: true, Instances: []officers.InstanceState{upInstance(r.o, 1)}}, clk.Now())
	r.runner.Wait()
	clk.Advance(time.Minute)
	passes(t, r, clk, id, 1)
	if st := r.hq.Item(id).Status; st != "open" || len(r.officerReplies("c-41")) != 0 {
		t.Fatalf("after its chat had no item for a pass, one refusal answered the chat (item %s): the old streak carried over", st)
	}
}

// TestTheRateLimitedReplyQuotesTheStatedReset: the reset the CLI stated, as it
// wrote it; none stated, none quoted.
func TestTheRateLimitedReplyQuotesTheStatedReset(t *testing.T) {
	for _, c := range []struct{ detail, want string }{
		{"responses HTTP 429: " + limitBody, "(The officer can't reply right now: its Claude accounts are at their usage limit (resets Nov 23, 1am (UTC)). Ask again later.)"},
		{"You've hit your session limit · resets 9:45pm (Europe/Berlin)", "(The officer can't reply right now: its Claude accounts are at their usage limit (resets 9:45pm (Europe/Berlin)). Ask again later.)"},
		{"responses HTTP 429: rate limited", "(The officer can't reply right now: its Claude accounts are at their usage limit. Ask again later.)"},
	} {
		if got := officers.ChatRateLimitedReply(c.detail); got != c.want {
			t.Errorf("ChatRateLimitedReply(%q) = %q; want %q", c.detail, got, c.want)
		}
	}
}

// TestALaneThatKeepsRefusingIsAnsweredWithWhy: a lane that refuses every turn
// as busy (nothing typed) is retried each pass, and its chatMaxLaneRefusals-th
// refusal in a row (15) is answered with the pinned error reply.
//
// RED before the fix: a busy refusal is retried every pass for ever.
func TestALaneThatKeepsRefusingIsAnsweredWithWhy(t *testing.T) {
	r, clk := clockRig(t, 1)
	r.hq.AddChat(relID, "c-41", "triage")
	seq, id := r.hq.MemberSays("c-41", "member1", "Member One", "Is web healthy?")
	r.lanes.reply = func(string, string) (string, error) {
		return "", fmt.Errorf("%w (busy)", officers.ErrChatLaneUnavailable)
	}
	ran := passes(t, r, clk, id, 40)
	if st := r.hq.Item(id).Status; st != "done" {
		t.Fatalf("after %d passes the chat whose lane keeps refusing is %s; want it answered and done", ran, st)
	}
	want := "(The officer can't reply right now: its chat session refused 15 tries in a row (busy or not ready). Ask again later.)"
	replies := r.officerReplies("c-41")
	if len(replies) != 1 || replies[0].Text != want || replies[0].InReplyTo != seq {
		t.Fatalf("replies %+v; want one, %q, in reply to %d", replies, want, seq)
	}
	if n := len(r.lanes.sent()); n != 15 {
		t.Fatalf("%d turns sent; want 15 (the bound)", n)
	}
	if !strings.Contains(r.logs.String(), "busy") {
		t.Fatalf("the refusal is not logged:\n%s", r.logs)
	}
}

// TestALaneRefusalStreakIsBrokenByATurnThatRan: the refusals that count are
// IN A ROW — a turn the lane took in between (here one that failed) starts the
// count again, so 14 refusals, a failed turn and 14 more are not answered
// with the error; the lane's answer is posted once it takes the turn.
func TestALaneRefusalStreakIsBrokenByATurnThatRan(t *testing.T) {
	r, clk := clockRig(t, 1)
	r.hq.AddChat(relID, "c-41", "triage")
	_, id := r.hq.MemberSays("c-41", "member1", "Member One", "Is web healthy?")
	calls := 0
	r.lanes.reply = func(string, string) (string, error) {
		calls++
		switch {
		case calls == 15:
			return "", fmt.Errorf("the turn failed")
		case calls <= 29:
			return "", fmt.Errorf("%w (busy)", officers.ErrChatLaneUnavailable)
		}
		return "the lane's answer", nil
	}
	passes(t, r, clk, id, 60)
	replies := r.officerReplies("c-41")
	if len(replies) != 1 || replies[0].Text != "the lane's answer" {
		t.Fatalf("replies %+v; want only the lane's answer (no streak reached 15 refusals in a row)", replies)
	}
}
