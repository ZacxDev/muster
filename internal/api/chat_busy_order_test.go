package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
)

// ---------------------------------------------------------------------------
// A WAITING MESSAGE'S STATE, GATE AND ORDER (chat_busy.go, turn_gate.go): the
// round-1 findings on #494. Same harness as chat_busy_test.go — the real
// gateway against busyCCD, through the real routes.
// ---------------------------------------------------------------------------

// releaseOnce closes hold now (the returned func) or at the test's end,
// whichever is first: a failing test must not leave the fake's handler blocked,
// or the server's Close waits on it for ever.
func releaseOnce(t *testing.T, hold chan struct{}) func() {
	var once sync.Once
	release := func() { once.Do(func() { close(hold) }) }
	t.Cleanup(release)
	return release
}

// phaseOf is agent name's live phase in the notification panel's registry.
func phaseOf(s *Server, name string) string {
	s.activeAgentsMu.Lock()
	defer s.activeAgentsMu.Unlock()
	return s.activeAgents[name].phase
}

// TestAQueuedMessageSaysItWasAcceptedBeforeItsReply: while the message waits
// the panel says "queued"; the moment the runtime takes it (its 103) the socket
// says "accepted" and the panel "thinking" — BEFORE any part of the reply, so
// the page drops its "queued" words while the turn runs.
//
// 🔴 REGRESSION: at 24845c1e there is no "accepted" and the panel says
// "thinking" for the whole wait.
func TestAQueuedMessageSaysItWasAcceptedBeforeItsReply(t *testing.T) {
	f := newBusyCCD()
	s, ts, _ := busyServer(t, f, time.Minute)
	c, ctx := dialChat(t, s, ts)
	sendChat(t, ctx, c, "accepted when?")
	waitRefusals(t, f, 2)
	if got := phaseOf(s, "keen-wren"); got != "queued" {
		t.Fatalf("panel phase while the message waits = %q, want queued", got)
	}
	f.mu.Lock()
	f.hold, f.arrivedCh = make(chan struct{}), make(chan struct{}, 4)
	f.mu.Unlock()
	release := releaseOnce(t, f.hold) // also on a failure, so the fake's handler can end
	f.free()
	<-f.arrivedCh // the runtime has the text and has not answered
	var before []string
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("socket read: %v (got %q)", err, before)
		}
		var m map[string]any
		_ = json.Unmarshal(data, &m)
		if m["type"] == nil {
			continue // a heartbeat
		}
		before = append(before, fmt.Sprint(m["type"]))
		if m["type"] == "accepted" {
			break
		}
		if m["type"] == "delta" || m["type"] == "done" || m["type"] == "error" {
			t.Fatalf("the socket sent %q before any \"accepted\": %q", m["type"], before)
		}
	}
	if got := phaseOf(s, "keen-wren"); got != "thinking" {
		t.Fatalf("panel phase once the runtime took the text = %q, want thinking", got)
	}
	release()
	msgs := readUntilEnd(t, ctx, c)
	if end := msgs[len(msgs)-1]["type"]; end != "done" {
		t.Fatalf("the turn ended %v", msgs)
	}
	for _, m := range msgs {
		if m["type"] == "accepted" || m["type"] == "queued" {
			t.Fatalf("a second %q after the message was accepted: %v", m["type"], msgs)
		}
	}
}

// TestAWaitingMessageDoesNotHoldTheTurnGate: a message asleep between asks of a
// busy runtime does not refuse a chief replacement or restart (closeFor); the
// close wakes it, and it gives up at once with the "replaced" words stored,
// never asking the runtime again.
//
// 🔴 REGRESSION: at 24845c1e closeFor answers gateBusy for the whole wait (up
// to 10 minutes).
func TestAWaitingMessageDoesNotHoldTheTurnGate(t *testing.T) {
	f := newBusyCCD() // never freed
	s, ts, store := busyServer(t, f, time.Minute)
	// A long poll: the waiter is asleep nearly all the time, and only the close
	// itself (not its next poll) can end the wait within the second below.
	s.chatBusyPoll = 3 * time.Second
	c, ctx := dialChat(t, s, ts)
	sendChat(t, ctx, c, "waiting when the chief is replaced")
	waitRefusals(t, f, 1)
	var ok bool
	var why string
	for deadline := time.Now().Add(3 * time.Second); !ok && time.Now().Before(deadline); {
		if ok, why = s.turns.closeFor(7); !ok {
			time.Sleep(5 * time.Millisecond) // caught mid-ask: that one may refuse
		}
	}
	if !ok {
		t.Fatalf("closeFor while the message waits = false (%s); want the waiting message not to hold the gate", why)
	}
	defer s.turns.reopen(7)
	closedAt := time.Now()
	refusedAtClose, _ := f.counts()
	msgs := readUntilEnd(t, ctx, c)
	if d := time.Since(closedAt); d > time.Second {
		t.Fatalf("the waiting message gave up %s after the close; want at once (the close wakes it)", d)
	}
	end := msgs[len(msgs)-1]
	if end["type"] != "error" || end["unsent"] != true || end["content"] != chatBusyReplaced {
		t.Fatalf("the waiting message ended %v; want unsent with %q", end, chatBusyReplaced)
	}
	eventuallyAPI(t, "the replaced give-up stored", func() bool { return len(store.transcript(41)) == 2 })
	if got := store.transcript(41)[1]; got != "assistant: "+AgentTurnFailedPrefix+chatBusyReplaced {
		t.Fatalf("stored = %q", got)
	}
	time.Sleep(300 * time.Millisecond)
	if refused, _ := f.counts(); refused != refusedAtClose {
		t.Fatalf("the runtime was asked again after the close (%d → %d)", refusedAtClose, refused)
	}
}

// TestAnAskInFlightStillHoldsTheTurnGate is the negative control for the test
// above: while the runtime HAS the text (an ask in flight, not asleep), a close
// is refused, as before.
func TestAnAskInFlightStillHoldsTheTurnGate(t *testing.T) {
	f := newBusyCCD()
	f.busy = false
	f.hold, f.arrivedCh = make(chan struct{}), make(chan struct{}, 4)
	s, ts, _ := busyServer(t, f, time.Minute)
	c, ctx := dialChat(t, s, ts)
	release := releaseOnce(t, f.hold)
	sendChat(t, ctx, c, "in flight")
	<-f.arrivedCh
	if ok, why := s.turns.closeFor(7); ok || why != gateBusy {
		t.Fatalf("closeFor during an ask in flight = %v %q; want refused busy", ok, why)
	}
	release()
	readUntilEnd(t, ctx, c)
}

// TestQueuedMessagesFromTwoDoorsAreSentInOrder: A waits on the chat socket,
// then B is posted from the composer; the runtime frees the moment it is asked
// B. Only the oldest waiter asks, so B is never asked before A is answered, and
// the answers come A then B.
//
// 🔴 REGRESSION: at 24845c1e B polls on its own and its first ask frees the
// session for B — answered [B, A].
func TestQueuedMessagesFromTwoDoorsAreSentInOrder(t *testing.T) {
	const a, b = "A, typed first in the chat", "B, posted second from the composer"
	f := newBusyCCD()
	f.freeOn = b
	s, ts, store := busyServer(t, f, time.Minute)
	c, ctx := dialChat(t, s, ts)
	sendChat(t, ctx, c, a)
	waitRefusals(t, f, 2)
	if resp := postAgent(t, s, ts, "keen-wren", url.Values{"text": {b}, "session": {"41"}}, false); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("the post = %d", resp.StatusCode)
	}
	time.Sleep(100 * time.Millisecond) // many polls with both waiting
	f.free()
	eventuallyAPI(t, "both answered", func() bool { _, ans := f.counts(); return len(ans) == 2 })
	if _, ans := f.counts(); ans[0] != a || ans[1] != b {
		t.Fatalf("answered %q; want A then B", ans)
	}
	f.mu.Lock()
	seen := append([]string(nil), f.seen...)
	f.mu.Unlock()
	for i, in := range seen {
		if in == b {
			for _, later := range seen[i:] {
				if later == a {
					t.Fatalf("B was asked before A was answered: %q", seen)
				}
			}
			break
		}
	}
	readUntilEnd(t, ctx, c)
	eventuallyAPI(t, "four rows stored", func() bool { return len(store.transcript(41)) == 4 })
}

// handBackAgents is tokenedAgents with a session claim the route hand-back can
// take.
type handBackAgents struct{ tokenedAgents }

func (handBackAgents) ClaimKickoff(context.Context, int64, string, time.Duration) (bool, error) {
	return true, nil
}
func (handBackAgents) ReleaseKickoffClaim(context.Context, int64, string) error { return nil }

// TestARouteHandBackThatGivesUpIsWordedForTheHandBack: the hand-back is
// muster's own text, so its stored give-up does not tell the owner to "send it
// again".
//
// 🔴 REGRESSION: at 24845c1e it stores the chat wording, "… send it again …".
func TestARouteHandBackThatGivesUpIsWordedForTheHandBack(t *testing.T) {
	f := newBusyCCD() // never freed
	s, _, store := busyServer(t, f, 80*time.Millisecond)
	s.ext.Agents = handBackAgents{tokenedAgents{store}}
	chief, _ := s.ext.Agents.GetByName(context.Background(), "keen-wren")
	sess, _ := store.GetSession(context.Background(), 41)
	if !s.startClaimedChiefTurn(context.Background(), chief, sess, "route-hb1", "muster: the session did not answer") {
		t.Fatal("the hand-back did not start")
	}
	eventuallyAPI(t, "the hand-back's give-up stored", func() bool { return len(store.transcript(41)) == 2 })
	got := store.transcript(41)[1]
	if !strings.HasPrefix(got, "assistant: "+AgentTurnFailedPrefix+"The chief was still busy with another turn after ") ||
		!strings.HasSuffix(got, ", so this hand-back was not delivered to it.") || strings.Contains(got, "send it again") {
		t.Fatalf("stored hand-back give-up = %q", got)
	}
}

// TestAcceptedAfterQueuedKeepsTheCallerCallback: the 103 hook a queued ask adds
// is CHAINED to the one ctx already carried (markOwnerTurn's owner-turn marker),
// not put in its place — or a message that waited would never mark the owner's
// turn in flight when ccd took it.
func TestAcceptedAfterQueuedKeepsTheCallerCallback(t *testing.T) {
	s := turnServer(nil)
	s.activeAgents = map[string]agentPhase{}
	s.activeAgentsNow = time.Now
	var outer []string
	ctx := agents.WithTurnAccepted(context.Background(), func(a agents.TurnAcceptance) { outer = append(outer, a.Lane) })
	var events []string
	actx, _ := s.acceptedAfterQueued(ctx, agents.Agent{ID: 7, Name: "keen-wren"},
		func(ev agents.StreamEvent) { events = append(events, ev.Kind) })
	agents.MarkTurnAccepted(actx, agents.TurnAcceptance{Lane: "lane-q7"})
	if len(outer) != 1 || outer[0] != "lane-q7" {
		t.Fatalf("the caller's own 103 callback saw %q; want it called once with the lane", outer)
	}
	if len(events) != 1 || events[0] != "accepted" {
		t.Fatalf("events = %q; want one accepted", events)
	}
}
