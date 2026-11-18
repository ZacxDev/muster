package officers_test

import (
	"context"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
)

// The second round of chat-session guards: a chat that cannot be served
// neither loops nor keeps its pool up; a reply HQ will not take is cut or
// replaced, never retried for ever; a runtime that cannot name a chat
// session's calls runs none; a charter change reaches the lane; a re-homed
// chat does not replay its whole history.

// Supports implements officers.ChatLanes: unsupported holds the answer per
// instance (absent: supported).
func (l *fakeLanes) Supports(_ context.Context, _ officers.Officer, n int) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return !l.unsupported[n], nil
}

// clockRig is a chat rig whose runner reads a test clock.
func clockRig(t *testing.T, max int) (*chatRig, *clock) {
	t.Helper()
	r := newChatRig(t, max)
	clk := &clock{t: time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC)}
	backends := officers.NewBackends(nil)
	runner, err := officers.NewChatRunner(officers.ChatRunnerConfig{Lanes: r.lanes, Backend: backends.Backend, Logger: log.New(r.logs, "", 0),
		HeartbeatEvery: time.Hour, ReplyRetries: []time.Duration{time.Millisecond}, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	r.runner = runner
	return r, clk
}

// TestAHostOfficerRunsNoChatAndKeepsNothingUp: a chat of an officer whose
// instances run on a host (no ccd, so no lane) is never claimed, says why in
// the log, and is held back — not ChatPending, so it keeps no instance up.
func TestAHostOfficerRunsNoChatAndKeepsNothingUp(t *testing.T) {
	r := newChatRig(t, 1)
	r.o.Runtime, r.o.Host = officers.RuntimeHost, "h1"
	r.hq.AddChat(relID, "c-41", "triage")
	_, id := r.hq.MemberSays("c-41", "member1", "Member One", "q")
	now := time.Now()
	obs := officers.Observation{Chats: true, Open: []officers.Item{r.item(t, id)}, Instances: []officers.InstanceState{upInstance(r.o, 1)}}
	want, held := r.runner.Want(context.Background(), r.o, obs, now)
	if len(want) != 0 || !held[officers.ItemID(id)] {
		t.Fatalf("a host officer's chat: want %v, held back %v; want nothing started and the chat held back", want, held)
	}
	r.runner.Dispatch(context.Background(), r.o, obs, now)
	r.runner.Wait()
	if it := r.hq.Item(id); it.Status != "open" {
		t.Fatalf("a host officer's chat item is %s by %q; want open, never claimed", it.Status, it.ClaimedBy)
	}
	if !strings.Contains(r.logs.String(), "host") {
		t.Fatalf("no log line says why a host officer's chat is not run:\n%s", r.logs)
	}
	obs.ChatHeldBack = held
	if d := officers.Decide(r.o, obs, now.Add(-time.Hour), now, nil); d.ChatPending != 0 || !d.Idle {
		t.Fatalf("a held-back chat: ChatPending %d, idle %v; want 0 and idle (nothing kept up)", d.ChatPending, d.Idle)
	}
}

// TestAPermanentLaneRefusalBacksOff: ccd refuses the turn for a reason that
// does not pass (ErrChatLaneBroken). The item is released, the reason logged,
// the marks untouched — and the chat is not run again, nor counted pending,
// until its backoff has passed.
func TestAPermanentLaneRefusalBacksOff(t *testing.T) {
	r, clk := clockRig(t, 1)
	r.hq.AddChat(relID, "c-41", "triage")
	_, id := r.hq.MemberSays("c-41", "member1", "Member One", "q")
	r.lanes.reply = func(string, string) (string, error) {
		return "", fmt.Errorf("%w (unauthorized)", officers.ErrChatLaneBroken)
	}
	obs := func() officers.Observation {
		return officers.Observation{Chats: true, Open: []officers.Item{r.item(t, id)}, Instances: []officers.InstanceState{upInstance(r.o, 1)}}
	}
	r.runner.Dispatch(context.Background(), r.o, obs(), clk.Now())
	r.runner.Wait()
	if n := len(r.lanes.sent()); n != 1 {
		t.Fatalf("%d turns sent, want 1", n)
	}
	if it := r.hq.Item(id); it.Status != "open" {
		t.Fatalf("after a broken lane the item is %s; want released", it.Status)
	}
	if m := r.lanes.marksOf("c-41"); m.Seen != 0 {
		t.Fatalf("marks after a broken lane %+v; want untouched (nothing was typed)", m)
	}
	if !strings.Contains(r.logs.String(), "unauthorized") {
		t.Fatalf("the refusal's reason is not logged:\n%s", r.logs)
	}
	clk.Advance(time.Second)
	r.runner.Dispatch(context.Background(), r.o, obs(), clk.Now())
	r.runner.Wait()
	if n := len(r.lanes.sent()); n != 1 {
		t.Fatalf("a broken chat was run again at once (%d turns): no backoff", n)
	}
	if _, held := r.runner.Want(context.Background(), r.o, obs(), clk.Now()); !held[officers.ItemID(id)] {
		t.Fatal("a backing-off chat is not held back: it would keep its pool up")
	}
	clk.Advance(time.Hour)
	r.lanes.reply = nil
	r.runner.Dispatch(context.Background(), r.o, obs(), clk.Now())
	r.runner.Wait()
	if st := r.hq.Item(id).Status; st != "done" {
		t.Fatalf("after the backoff the chat is %s; want answered", st)
	}
}

// TestATransientLaneRefusalSaysWhy: a capacity refusal is not a failure, but
// its reason is logged.
func TestATransientLaneRefusalSaysWhy(t *testing.T) {
	r := newChatRig(t, 1)
	r.hq.AddChat(relID, "c-41", "triage")
	_, id := r.hq.MemberSays("c-41", "member1", "Member One", "q")
	r.lanes.reply = func(string, string) (string, error) {
		return "", fmt.Errorf("%w (capacity)", officers.ErrChatLaneUnavailable)
	}
	if got := r.runner.Run(context.Background(), r.o, 1, r.item(t, id)); got != officers.ChatLaneBusy {
		t.Fatalf("Run = %s, want %s", got, officers.ChatLaneBusy)
	}
	if !strings.Contains(r.logs.String(), "capacity") {
		t.Fatalf("the lane's refusal reason is not logged:\n%s", r.logs)
	}
}

// TestAnOversizedReplyIsCutNotRefused: a reply over HQ's 64 KiB cap (HQ
// answers 413) is cut to it, with the marker, and posted once.
func TestAnOversizedReplyIsCutNotRefused(t *testing.T) {
	r := newChatRig(t, 1)
	r.hq.AddChat(relID, "c-41", "triage")
	_, id := r.hq.MemberSays("c-41", "member1", "Member One", "q")
	long := strings.Repeat("é", 40<<10) // 80 KiB of two-byte runes
	r.lanes.reply = func(string, string) (string, error) { return long, nil }
	if got := r.runner.Run(context.Background(), r.o, 1, r.item(t, id)); got != officers.ChatReplied {
		t.Fatalf("Run = %s, want %s; log:\n%s", got, officers.ChatReplied, r.logs)
	}
	rs := r.officerReplies("c-41")
	if len(rs) != 1 || len(rs[0].Text) > officers.MaxChatReplyBytes || !strings.HasSuffix(rs[0].Text, officers.ChatReplyCutMarker) {
		t.Fatalf("replies: %d, first %d bytes; want one, at most %d bytes, ending with the cut marker", len(rs), len(rs[0].Text), officers.MaxChatReplyBytes)
	}
}

// TestARefusedReplyPostsAFallbackOnce: HQ refuses the reply 400 (final). The
// turn is marked answered and the member gets a short fallback saying the
// reply could not be delivered, and why; no second claim re-asks the lane.
func TestARefusedReplyPostsAFallbackOnce(t *testing.T) {
	r := newChatRig(t, 1)
	r.hq.AddChat(relID, "c-41", "triage")
	seq, id := r.hq.MemberSays("c-41", "member1", "Member One", "q")
	r.hq.RefuseReplies(400, "invalid", 1)
	if got := r.runner.Run(context.Background(), r.o, 1, r.item(t, id)); got != officers.ChatReplyDropped {
		t.Fatalf("Run = %s, want %s; log:\n%s", got, officers.ChatReplyDropped, r.logs)
	}
	rs := r.officerReplies("c-41")
	if len(rs) != 1 || rs[0].InReplyTo != seq || !strings.Contains(rs[0].Text, "could not be delivered") || !strings.Contains(rs[0].Text, "invalid") {
		t.Fatalf("replies %+v; want one fallback, to %d, saying the reply could not be delivered and why", rs, seq)
	}
	if m := r.lanes.marksOf("c-41"); m.Answered != m.Seen || m.Seen == 0 {
		t.Fatalf("marks %+v; want the turn answered", m)
	}
}

// TestAChatWaitsForARuntimeThatNamesItsSession: an instance whose ccd does
// not advertise naming a chat session's MCP calls runs no chat: nothing is
// claimed, the log says why, and the chat is held back.
func TestAChatWaitsForARuntimeThatNamesItsSession(t *testing.T) {
	r, clk := clockRig(t, 1)
	r.lanes.unsupported = map[int]bool{1: true}
	r.hq.AddChat(relID, "c-41", "triage")
	_, id := r.hq.MemberSays("c-41", "member1", "Member One", "q")
	obs := officers.Observation{Chats: true, Open: []officers.Item{r.item(t, id)}, Instances: []officers.InstanceState{upInstance(r.o, 1)}}
	r.runner.Dispatch(context.Background(), r.o, obs, clk.Now())
	r.runner.Wait()
	if it := r.hq.Item(id); it.Status != "open" || it.ClaimedBy != "" {
		t.Fatalf("on an unsupported runtime the item is %s by %q; want never claimed", it.Status, it.ClaimedBy)
	}
	if n := len(r.lanes.sent()); n != 0 {
		t.Fatalf("%d turns sent to an unsupported runtime", n)
	}
	if !strings.Contains(r.logs.String(), "does not name") {
		t.Fatalf("no log line says the runtime cannot run a chat:\n%s", r.logs)
	}
	if _, held := r.runner.Want(context.Background(), r.o, obs, clk.Now()); !held[officers.ItemID(id)] {
		t.Fatal("a chat its runtime cannot run is not held back")
	}
}

// TestAChangedCharterIsReloadedOnTheNextTurn: the lane's next turn after the
// charter version moved on HQ's brief carries the reload line; a turn with the
// version unchanged does not.
func TestAChangedCharterIsReloadedOnTheNextTurn(t *testing.T) {
	r := newChatRig(t, 1)
	r.hq.AddChat(relID, "c-41", "triage")
	ask := func(q string) string {
		t.Helper()
		_, id := r.hq.MemberSays("c-41", "member1", "Member One", q)
		if got := r.runner.Run(context.Background(), r.o, 1, r.item(t, id)); got != officers.ChatReplied {
			t.Fatalf("Run = %s, want %s; log:\n%s", got, officers.ChatReplied, r.logs)
		}
		turns := r.lanes.sent()
		return turns[len(turns)-1].text
	}
	ask("first")
	if text := ask("second"); strings.Contains(text, "charter changed") {
		t.Fatalf("an unchanged charter asked for a reload: %q", text)
	}
	r.hq.SetCharterVersion(relID, 2)
	if text := ask("third"); !strings.Contains(text, "charter changed") || !strings.Contains(text, officers.ToolBrief) {
		t.Fatalf("after the charter moved the lane got %q; want the reload line naming %s", text, officers.ToolBrief)
	}
	if text := ask("fourth"); strings.Contains(text, "charter changed") {
		t.Fatalf("the reload line repeated after the lane reloaded: %q", text)
	}
}

// TestARehomedChatStartsAfterTheLastReply: a chat whose lane has no marks
// (its session is new: the chat moved to another instance) but whose history
// holds an officer reply sends only what came after that reply.
func TestARehomedChatStartsAfterTheLastReply(t *testing.T) {
	r := newChatRig(t, 1)
	r.hq.AddChat(relID, "c-41", "triage")
	_, id := r.hq.MemberSays("c-41", "member1", "Member One", "old question")
	if got := r.runner.Run(context.Background(), r.o, 1, r.item(t, id)); got != officers.ChatReplied {
		t.Fatalf("Run = %s, want %s", got, officers.ChatReplied)
	}
	r.lanes.mu.Lock()
	r.lanes.marks = map[string]officers.ChatMarks{} // a new session: no marks
	r.lanes.mu.Unlock()
	_, id = r.hq.MemberSays("c-41", "member1", "Member One", "new question")
	if got := r.runner.Run(context.Background(), r.o, 1, r.item(t, id)); got != officers.ChatReplied {
		t.Fatalf("Run = %s, want %s; log:\n%s", got, officers.ChatReplied, r.logs)
	}
	turns := r.lanes.sent()
	if last := turns[len(turns)-1].text; !strings.Contains(last, "new question") || strings.Contains(last, "old question") {
		t.Fatalf("the re-homed chat's turn: %q; want only what came after the last reply", last)
	}
}

// TestATooLargeHistoryFallsBackToTheTurn: a history over muster's answer cap
// (ErrHQAnswerTooLarge) does not wedge the chat: the turn the item names is
// read on its own and answered.
func TestATooLargeHistoryFallsBackToTheTurn(t *testing.T) {
	r := newChatRig(t, 1)
	r.hq.AddChat(relID, "c-41", "triage")
	big := strings.Repeat("x", 700<<10)
	r.hq.MemberSays("c-41", "member1", "Member One", big)
	r.hq.MemberSays("c-41", "member1", "Member One", big)
	seq, id := r.hq.MemberSays("c-41", "member1", "Member One", "the question")
	if got := r.runner.Run(context.Background(), r.o, 1, r.item(t, id)); got != officers.ChatReplied {
		t.Fatalf("Run = %s, want %s; log:\n%s", got, officers.ChatReplied, r.logs)
	}
	if rs := r.officerReplies("c-41"); len(rs) != 1 || rs[0].InReplyTo != seq {
		t.Fatalf("replies %+v; want one, to %d", rs, seq)
	}
}
