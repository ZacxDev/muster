package officers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
)

// fakeLanes is officers.ChatLanes in memory: each chat's home instance and
// marks, the turns sent, and what a turn answers.
type fakeLanes struct {
	mu    sync.Mutex
	homes map[string]int
	marks map[string]officers.ChatMarks
	turns []fakeTurn
	// reply answers a turn (nil: "reply to <chat>"); block, when set, holds
	// every turn until it is closed.
	reply func(chat, text string) (string, error)
	block chan struct{}
	// started is signalled once per turn as it starts (buffered by the test).
	started chan string
	// unsupported is the instances whose runtime does not name a chat
	// session's calls (Supports).
	unsupported map[int]bool
	// rehomes is each Rehome, as "<chat>:<from>".
	rehomes []string
}

type fakeTurn struct {
	n          int
	chat, text string
}

func newLanes() *fakeLanes {
	return &fakeLanes{homes: map[string]int{}, marks: map[string]officers.ChatMarks{}}
}

func (l *fakeLanes) Home(_ context.Context, _ officers.Officer, chat string) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.homes[chat], nil
}

func (l *fakeLanes) Open(_ context.Context, _ officers.Officer, n int, chat string) (officers.ChatMarks, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.homes[chat] == 0 {
		l.homes[chat] = n
	}
	return l.marks[chat], nil
}

func (l *fakeLanes) Mark(_ context.Context, _ officers.Officer, _ int, chat string, m officers.ChatMarks) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.marks[chat] = m
	return nil
}

func (l *fakeLanes) Turn(_ context.Context, _ officers.Officer, n int, chat, text string) (string, error) {
	l.mu.Lock()
	l.turns = append(l.turns, fakeTurn{n: n, chat: chat, text: text})
	block, reply, started := l.block, l.reply, l.started
	l.mu.Unlock()
	if started != nil {
		started <- chat
	}
	if block != nil {
		<-block
	}
	if reply != nil {
		return reply(chat, text)
	}
	return "reply to " + chat, nil
}

// Rehome implements officers.ChatLanes: the chat's lane on instance from is
// set aside (its home and marks forgotten), and recorded.
func (l *fakeLanes) Rehome(_ context.Context, _ officers.Officer, chat string, from int) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rehomes = append(l.rehomes, fmt.Sprintf("%s:%d", chat, from))
	if l.homes[chat] == from {
		delete(l.homes, chat)
		delete(l.marks, chat)
	}
	return nil
}

func (l *fakeLanes) sent() []fakeTurn {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]fakeTurn(nil), l.turns...)
}

func (l *fakeLanes) marksOf(chat string) officers.ChatMarks {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.marks[chat]
}

// chatRig is a fake HQ with one officer, its backends and a chat runner over
// fakeLanes.
type chatRig struct {
	hq     *hqtest.Server
	o      officers.Officer
	lanes  *fakeLanes
	runner *officers.ChatRunner
	logs   *bytes.Buffer
}

func newChatRig(t *testing.T, max int) *chatRig {
	t.Helper()
	hq := hqtest.New(map[string]string{relTok: relID})
	t.Cleanup(hq.Close)
	o := officers.Officer{ID: relID, HQURL: hq.BaseURL(), MaxInstances: max, IdleAfter: time.Minute, PollInterval: time.Minute, Owner: 1}.WithToken(relTok)
	r := &chatRig{hq: hq, o: o, lanes: newLanes(), logs: &bytes.Buffer{}}
	backends := officers.NewBackends(nil)
	runner, err := officers.NewChatRunner(officers.ChatRunnerConfig{Lanes: r.lanes, Backend: backends.Backend, Logger: log.New(r.logs, "", 0),
		HeartbeatEvery: time.Hour, ReplyRetries: []time.Duration{time.Millisecond, time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	r.runner = runner
	return r
}

// item is the HQ item id as the loop reads it.
func (r *chatRig) item(t *testing.T, id string) officers.Item {
	t.Helper()
	raw, err := json.Marshal(r.hq.Item(id))
	if err != nil {
		t.Fatal(err)
	}
	var it officers.Item
	if err := json.Unmarshal(raw, &it); err != nil {
		t.Fatal(err)
	}
	return it
}

func (r *chatRig) officerReplies(chat string) []hqtest.ChatMessage {
	var out []hqtest.ChatMessage
	for _, m := range r.hq.ChatMessages(chat) {
		if m.Role == "officer" {
			out = append(out, m)
		}
	}
	return out
}

func chatItem(id, chat string, seq int) officers.Item {
	return officers.Item{ID: officers.ItemID(id), Kind: officers.ItemKindChat, SubjectKey: "chat:" + chat, Status: officers.StatusOpen,
		Payload: json.RawMessage(fmt.Sprintf(`{"chat_id":%q,"seq":%d}`, chat, seq))}
}

func upInstance(o officers.Officer, n int) officers.InstanceState {
	return officers.InstanceState{N: n, Name: o.InstanceName(n), Exists: true, Up: true}
}

// TestAChatItemIsNotQueueWork: an open chat item, its officer's chats on
// (obs.Chats), an up idle instance whose idle window has long run out — the
// item is not Claimable, wakes no re-prompt of the primary, and still keeps the
// pool busy (no scale-down). The control: with chats off the same item is
// ordinary queue work, as before chat sessions existed.
func TestAChatItemIsNotQueueWork(t *testing.T) {
	now := time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC)
	o := officers.Officer{ID: relID, MaxInstances: 1, IdleAfter: time.Minute}
	obs := officers.Observation{Chats: true, Open: []officers.Item{chatItem("7", "c-1", 1)}, Instances: []officers.InstanceState{upInstance(o, 1)}}
	d := officers.Decide(o, obs, now.Add(-time.Hour), now, nil)
	if d.Claimable != 0 || len(d.Reprompt) != 0 || len(d.Start) != 0 {
		t.Fatalf("a chat item counted as queue work: claimable %d, reprompt %v, start %v", d.Claimable, d.Reprompt, d.Start)
	}
	if d.ChatPending != 1 || d.Idle || len(d.Stop) != 0 {
		t.Fatalf("a pending chat: ChatPending %d, idle %v, stop %v; want 1, false, none (the chat's instance stays up)", d.ChatPending, d.Idle, d.Stop)
	}
	obs.Chats = false
	d = officers.Decide(o, obs, now.Add(-time.Hour), now, nil)
	if d.Claimable != 1 || len(d.Reprompt) != 1 || d.ChatPending != 0 {
		t.Fatalf("control: with chats off a chat-kind item is queue work: claimable %d, reprompt %v, ChatPending %d", d.Claimable, d.Reprompt, d.ChatPending)
	}
}

// TestAPendingChatStartsTheInstanceItNeeds: nothing up, a chat pending, and
// the runner says it needs instance 2 (its home): instance 2 is started — not
// the lowest-numbered down instance queue work would start.
func TestAPendingChatStartsTheInstanceItNeeds(t *testing.T) {
	now := time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC)
	o := officers.Officer{ID: relID, MaxInstances: 3, IdleAfter: time.Minute}
	down := []officers.InstanceState{{N: 1, Name: o.InstanceName(1)}, {N: 2, Name: o.InstanceName(2)}, {N: 3, Name: o.InstanceName(3)}}
	obs := officers.Observation{Chats: true, Open: []officers.Item{chatItem("7", "c-1", 1)}, Instances: down, ChatPlan: officers.ChatPlan{Homes: []int{2}}}
	d := officers.Decide(o, obs, time.Time{}, now, nil)
	if fmt.Sprint(d.Start) != "[2]" {
		t.Fatalf("a chat whose lane lives on instance 2: start %v, want [2]", d.Start)
	}
	// A home past the chat ceiling (3, the default) is not started: the chat
	// is re-homed, so the lowest down instance is started instead.
	obs.ChatPlan = officers.ChatPlan{Need: 1, Homes: []int{4}}
	if d := officers.Decide(o, obs, time.Time{}, now, nil); fmt.Sprint(d.Start) != "[1]" {
		t.Fatalf("a home past the chat ceiling: start %v, want [1]", d.Start)
	}
}

// TestAChatClaimIsNeitherThePrimarysNorReleased: a live chat claim held as
// ChatHolder(instance 1) beside an open queue item. The instance still counts
// as holding NOTHING of the queue — it is re-prompted for the queue item — and
// the chat claim is not released (its holder name is no instance), not held
// (Held 0) and not overrun; it keeps the pool busy (ChatHeld).
func TestAChatClaimIsNeitherThePrimarysNorReleased(t *testing.T) {
	now := time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC)
	o := officers.Officer{ID: relID, MaxInstances: 1, IdleAfter: time.Minute}
	exp := now.Add(time.Minute)
	held := chatItem("7", "c-1", 1)
	held.Status, held.ClaimedBy, held.LeaseExpiresAt = officers.StatusClaimed, officers.ChatHolder(o.InstanceName(1)), &exp
	queue := officers.Item{ID: "8", Kind: "release.evaluate", Status: officers.StatusOpen}
	obs := officers.Observation{Chats: true, Open: []officers.Item{queue}, Claimed: []officers.Item{held}, Instances: []officers.InstanceState{upInstance(o, 1)}}
	d := officers.Decide(o, obs, time.Time{}, now, nil)
	if fmt.Sprint(d.Reprompt) != "[1]" || d.Held != 0 || d.ChatHeld != 1 || len(d.Release) != 0 || len(d.Overrun) != 0 {
		t.Fatalf("a chat claim beside queue work: reprompt %v, held %d, ChatHeld %d, release %v, overrun %v; want [1], 0, 1, none, none",
			d.Reprompt, d.Held, d.ChatHeld, d.Release, d.Overrun)
	}
	// With nothing else, the chat claim alone keeps the instance up.
	obs.Open = nil
	if d := officers.Decide(o, obs, now.Add(-time.Hour), now, nil); d.Idle || len(d.Stop) != 0 || len(d.Reprompt) != 0 {
		t.Fatalf("only a chat claim held: idle %v, stop %v, reprompt %v; want busy, nothing moved", d.Idle, d.Stop, d.Reprompt)
	}
}

// TestARoutinePoolRunsNoChat: a tiered officer's routine pool neither works a
// chat item as queue work nor counts it as its chat work.
func TestARoutinePoolRunsNoChat(t *testing.T) {
	now := time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC)
	o := officers.Officer{ID: relID, MaxInstances: 1, IdleAfter: time.Minute, Tier: officers.TierRoutine}
	obs := officers.Observation{Chats: true, Open: []officers.Item{chatItem("7", "c-1", 1)}, Instances: []officers.InstanceState{upInstance(o, 1)}}
	d := officers.Decide(o, obs, now.Add(-time.Hour), now, nil)
	if d.Claimable != 0 || d.ChatPending != 0 || len(d.Reprompt) != 0 {
		t.Fatalf("the routine pool and a chat item: claimable %d, ChatPending %d, reprompt %v; want 0, 0, none", d.Claimable, d.ChatPending, d.Reprompt)
	}
}

// TestTheChatTurnTextIsTheContractsFrame pins the whole turn a lane is sent:
// the frame line, the first-turn charter line, and each message under who
// typed it (display name and login).
func TestTheChatTurnTextIsTheContractsFrame(t *testing.T) {
	o := officers.Officer{ID: relID, MaxInstances: 1}
	msgs := []officers.ChatMessage{{Seq: 1, Role: "member", AuthorLogin: "member1", AuthorName: "Member One", Text: "Is web healthy?"},
		{Seq: 2, Role: "system", Text: "the chat was renamed"}}
	got := officers.ChatTurnText(o, 1, "triage", msgs, true, false)
	want := "A team member is chatting with you in session triage; reply in plain text; this is not a queue item.\n" +
		"You are officer release:example-web (instance 1, officer-release-example-web-1). Call officer_brief once to load your charter before you answer.\n" +
		"\nMember One (@member1) wrote:\nIs web healthy?\n" +
		"\n[system]\nthe chat was renamed\n"
	if got != want {
		t.Fatalf("turn text:\n%q\nwant\n%q", got, want)
	}
	got = officers.ChatTurnText(o, 1, "triage", nil, false, true)
	want = "A team member is chatting with you in session triage; reply in plain text; this is not a queue item.\n" +
		"\nYour reply to the last message above did not reach the team member (muster lost it to a restart or a failed post). Reply to it again.\n"
	if got != want {
		t.Fatalf("retry text:\n%q\nwant\n%q", got, want)
	}
}

// TestAChatTurnClaimsAsTheChatHolderAndReplies: one member turn, run end to
// end against the fake HQ — claimed as ChatHolder (never the instance's own
// name), its message sent to the lane under who typed it, the lane's text
// posted as the reply to that seq, the item done, the marks moved past the
// reply.
func TestAChatTurnClaimsAsTheChatHolderAndReplies(t *testing.T) {
	r := newChatRig(t, 1)
	r.hq.AddChat(relID, "c-41", "triage")
	seq, id := r.hq.MemberSays("c-41", "member1", "Member One", "Is web healthy?")
	if got := r.runner.Run(context.Background(), r.o, 1, r.item(t, id)); got != officers.ChatReplied {
		t.Fatalf("Run = %s, want %s; log:\n%s", got, officers.ChatReplied, r.logs)
	}
	var claimedAs string
	for _, q := range r.hq.Requests() {
		if strings.HasSuffix(q.Path, "/items/"+id+"/claim") {
			claimedAs, _ = q.Body["instance"].(string)
		}
	}
	if want := officers.InstanceName(relID, 1) + "/chat"; claimedAs != want {
		t.Fatalf("claimed as %q, want %q", claimedAs, want)
	}
	turns := r.lanes.sent()
	if len(turns) != 1 || turns[0].n != 1 || turns[0].chat != "c-41" || !strings.Contains(turns[0].text, "Member One (@member1) wrote:\nIs web healthy?") ||
		!strings.Contains(turns[0].text, "in session triage;") {
		t.Fatalf("lane turns %+v; want one on instance 1 carrying the member's message under who typed it", turns)
	}
	replies := r.officerReplies("c-41")
	if len(replies) != 1 || replies[0].InReplyTo != seq || replies[0].Text != "reply to c-41" {
		t.Fatalf("replies %+v; want one, in reply to %d", replies, seq)
	}
	if st := r.hq.Item(id).Status; st != "done" {
		t.Fatalf("item %s is %s after the reply, want done", id, st)
	}
	if m := r.lanes.marksOf("c-41"); m.Seen != replies[0].Seq || m.Answered != replies[0].Seq {
		t.Fatalf("marks %+v; want seen = answered = the reply's seq %d", m, replies[0].Seq)
	}
}

// TestARefusedLaneLeavesTheItemUnclaimed: ccd refuses the turn before typing
// anything (ErrChatLaneUnavailable: 409 busy, 503 not_ready or capacity). The
// item goes back to open, no reply is posted, and the marks are as before —
// the next pass sends the same message, not a "your reply was lost".
func TestARefusedLaneLeavesTheItemUnclaimed(t *testing.T) {
	r := newChatRig(t, 1)
	r.hq.AddChat(relID, "c-41", "triage")
	_, id := r.hq.MemberSays("c-41", "member1", "Member One", "Is web healthy?")
	r.lanes.reply = func(string, string) (string, error) {
		return "", fmt.Errorf("%w (capacity)", officers.ErrChatLaneUnavailable)
	}
	if got := r.runner.Run(context.Background(), r.o, 1, r.item(t, id)); got != officers.ChatLaneBusy {
		t.Fatalf("Run = %s, want %s", got, officers.ChatLaneBusy)
	}
	if it := r.hq.Item(id); it.Status != "open" || it.ClaimedBy != "" {
		t.Fatalf("item after a refused lane: %s by %q, want open and unheld", it.Status, it.ClaimedBy)
	}
	if n := len(r.officerReplies("c-41")); n != 0 {
		t.Fatalf("%d replies posted for a turn the lane never took", n)
	}
	if m := r.lanes.marksOf("c-41"); m.Seen != 0 || m.Answered != 0 {
		t.Fatalf("marks after a refused lane: %+v, want 0/0", m)
	}
	// The next try sends the member's message itself.
	r.lanes.reply = nil
	if got := r.runner.Run(context.Background(), r.o, 1, r.item(t, id)); got != officers.ChatReplied {
		t.Fatalf("retry: Run = %s, want %s", got, officers.ChatReplied)
	}
	if turns := r.lanes.sent(); len(turns) != 2 || !strings.Contains(turns[1].text, "Is web healthy?") || strings.Contains(turns[1].text, "did not reach") {
		t.Fatalf("the retry's turn: %q; want the member's message, not a lost-reply prompt", turns[len(turns)-1].text)
	}
}

// TestAChatNeverResendsOrSkipsAcrossARestart: the marks a restarted muster
// reads decide the turn. Seen past Answered (the lane took seq 1, the reply
// never landed) sends NO message again and asks for the answer; after the
// reply, a new member turn sends only that turn.
func TestAChatNeverResendsOrSkipsAcrossARestart(t *testing.T) {
	r := newChatRig(t, 1)
	r.hq.AddChat(relID, "c-41", "triage")
	seq1, id := r.hq.MemberSays("c-41", "member1", "Member One", "first question")
	r.lanes.marks["c-41"] = officers.ChatMarks{Seen: seq1, Answered: 0} // sent, then muster died
	r.lanes.homes["c-41"] = 1
	if got := r.runner.Run(context.Background(), r.o, 1, r.item(t, id)); got != officers.ChatReplied {
		t.Fatalf("Run = %s, want %s; log:\n%s", got, officers.ChatReplied, r.logs)
	}
	turns := r.lanes.sent()
	if len(turns) != 1 || strings.Contains(turns[0].text, "first question") || !strings.Contains(turns[0].text, "did not reach the team member") {
		t.Fatalf("after a lost reply the lane got %q; want the lost-reply prompt and no re-sent message", turns[0].text)
	}
	if rs := r.officerReplies("c-41"); len(rs) != 1 || rs[0].InReplyTo != seq1 {
		t.Fatalf("replies %+v; want one in reply to %d", rs, seq1)
	}
	seq3, id := r.hq.MemberSays("c-41", "member1", "Member One", "second question")
	if got := r.runner.Run(context.Background(), r.o, 1, r.item(t, id)); got != officers.ChatReplied {
		t.Fatalf("second turn: Run = %s, want %s", got, officers.ChatReplied)
	}
	turns = r.lanes.sent()
	last := turns[len(turns)-1].text
	if !strings.Contains(last, "second question") || strings.Contains(last, "first question") || strings.Contains(last, "Call officer_brief") {
		t.Fatalf("second turn sent %q; want only the new message (and no first-turn line)", last)
	}
	if rs := r.officerReplies("c-41"); len(rs) != 2 || rs[1].InReplyTo != seq3 {
		t.Fatalf("replies %+v; want a second in reply to %d", rs, seq3)
	}
}

// TestAReplyIsPostedOnceAcrossARetry: HQ is down for the first reply post
// (an outage): the runner posts again and HQ holds ONE reply. Then an item
// re-offered for a member seq already answered sends the lane nothing and
// posts the reply idempotently (HQ answers the one it holds).
func TestAReplyIsPostedOnceAcrossARetry(t *testing.T) {
	r := newChatRig(t, 1)
	r.hq.AddChat(relID, "c-41", "triage")
	seq, id := r.hq.MemberSays("c-41", "member1", "Member One", "Is web healthy?")
	var mu sync.Mutex
	posts := 0
	r.hq.Before = func(method, path string) {
		if !strings.HasSuffix(path, "/reply") {
			return
		}
		mu.Lock()
		posts++
		r.hq.SetDown(posts == 1)
		mu.Unlock()
	}
	if got := r.runner.Run(context.Background(), r.o, 1, r.item(t, id)); got != officers.ChatReplied {
		t.Fatalf("Run = %s, want %s; log:\n%s", got, officers.ChatReplied, r.logs)
	}
	mu.Lock()
	n := posts
	mu.Unlock()
	if n != 2 {
		t.Fatalf("%d reply posts, want 2 (one refused by the outage, one taken)", n)
	}
	if rs := r.officerReplies("c-41"); len(rs) != 1 || rs[0].InReplyTo != seq {
		t.Fatalf("replies %+v; want exactly one", rs)
	}
	// HQ re-offers the item for seq 1, already answered.
	r.hq.SetPayload(id, fmt.Sprintf(`{"chat_id":"c-41","seq":%d}`, seq))
	r.hq.SetClaim(id, "", time.Now().Add(-time.Minute)) // a lapsed claim: claimable
	before := len(r.lanes.sent())
	if got := r.runner.Run(context.Background(), r.o, 1, r.item(t, id)); got != officers.ChatReplied {
		t.Fatalf("re-offered: Run = %s, want %s; log:\n%s", got, officers.ChatReplied, r.logs)
	}
	if n := len(r.lanes.sent()); n != before {
		t.Fatalf("an answered member turn was sent to the lane again (%d turns, was %d)", n, before)
	}
	if rs := r.officerReplies("c-41"); len(rs) != 1 {
		t.Fatalf("replies %+v after the re-offer; want still exactly one", rs)
	}
}

// TestAChatTurnHeartbeatsItsClaim: a turn that outlasts the heartbeat cadence
// renews the chat claim, as the chat holder.
func TestAChatTurnHeartbeatsItsClaim(t *testing.T) {
	r := newChatRig(t, 1)
	backends := officers.NewBackends(nil)
	runner, err := officers.NewChatRunner(officers.ChatRunnerConfig{Lanes: r.lanes, Backend: backends.Backend, Logger: log.New(r.logs, "", 0),
		HeartbeatEvery: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	r.hq.AddChat(relID, "c-41", "triage")
	_, id := r.hq.MemberSays("c-41", "member1", "Member One", "Is web healthy?")
	r.lanes.reply = func(chat, _ string) (string, error) { time.Sleep(60 * time.Millisecond); return "ok", nil }
	if got := runner.Run(context.Background(), r.o, 1, r.item(t, id)); got != officers.ChatReplied {
		t.Fatalf("Run = %s, want %s", got, officers.ChatReplied)
	}
	beats := 0
	for _, q := range r.hq.Requests() {
		if strings.HasSuffix(q.Path, "/items/"+id+"/heartbeat") && q.Body["instance"] == officers.ChatHolder(officers.InstanceName(relID, 1)) {
			beats++
		}
	}
	if beats == 0 {
		t.Fatal("a 60ms turn at a 5ms heartbeat sent no heartbeat as the chat holder")
	}
}

// TestDispatchLeavesAChatUnclaimedWhenEveryLaneIsBusy: one instance with ONE
// chat lane and three chats pending — c-1 (no lane yet, first in HQ's order),
// c-2 (its lane already on instance 1) and c-3 (no lane yet): c-1's turn
// starts; neither other item is claimed while it runs (no failure, no drop),
// whether it is placed by its home or by the least busy instance; both are run
// once the lane is free.
func TestDispatchLeavesAChatUnclaimedWhenEveryLaneIsBusy(t *testing.T) {
	r := newChatRig(t, 1)
	r.o = r.o.WithChatSessions(1)
	var ids []string
	for _, c := range []string{"c-1", "c-2", "c-3"} {
		r.hq.AddChat(relID, c, c)
		_, id := r.hq.MemberSays(c, "member1", "Member One", "question for "+c)
		ids = append(ids, id)
	}
	r.lanes.homes["c-2"] = 1
	r.lanes.block = make(chan struct{})
	r.lanes.started = make(chan string, 4)
	open := func() []officers.Item {
		var out []officers.Item
		for _, id := range ids {
			if it := r.item(t, id); it.Status == "open" {
				out = append(out, it)
			}
		}
		return out
	}
	obs := officers.Observation{Chats: true, Open: open(), Instances: []officers.InstanceState{upInstance(r.o, 1)}}
	r.runner.Dispatch(context.Background(), r.o, obs, time.Now())
	if first := <-r.lanes.started; first != "c-1" {
		t.Fatalf("the first turn was %s's, want c-1's (HQ's order)", first)
	}
	r.runner.Dispatch(context.Background(), r.o, obs, time.Now()) // a second pass meanwhile
	for _, id := range ids[1:] {
		if it := r.hq.Item(id); it.Status != "open" || it.ClaimedBy != "" {
			t.Fatalf("with the one lane busy, item %s is %s by %q; want open and unclaimed", id, it.Status, it.ClaimedBy)
		}
	}
	close(r.lanes.block)
	r.runner.Wait()
	for i := 0; i < 2; i++ {
		obs.Open = open()
		r.runner.Dispatch(context.Background(), r.o, obs, time.Now())
		r.runner.Wait()
	}
	for _, id := range ids {
		if st := r.hq.Item(id).Status; st != "done" {
			t.Fatalf("after the lane freed: item %s is %s, want done; log:\n%s", id, st, r.logs)
		}
	}
}

// TestANewChatGoesToTheLeastBusyInstance: two instances up, one lane each,
// instance 1's lane busy with c-1: a new chat (no lane yet) runs on instance
// 2, not the lowest-numbered one.
func TestANewChatGoesToTheLeastBusyInstance(t *testing.T) {
	r := newChatRig(t, 2)
	r.o = r.o.WithChatSessions(1)
	r.hq.AddChat(relID, "c-1", "one")
	r.hq.AddChat(relID, "c-2", "two")
	_, id1 := r.hq.MemberSays("c-1", "member1", "Member One", "q1")
	_, id2 := r.hq.MemberSays("c-2", "member1", "Member One", "q2")
	r.lanes.block = make(chan struct{})
	r.lanes.started = make(chan string, 4)
	up := []officers.InstanceState{upInstance(r.o, 1), upInstance(r.o, 2)}
	r.runner.Dispatch(context.Background(), r.o, officers.Observation{Chats: true, Open: []officers.Item{r.item(t, id1)}, Instances: up}, time.Now())
	<-r.lanes.started
	r.runner.Dispatch(context.Background(), r.o, officers.Observation{Chats: true, Open: []officers.Item{r.item(t, id2)}, Instances: up}, time.Now())
	select {
	case <-r.lanes.started:
	case <-time.After(5 * time.Second):
		close(r.lanes.block)
		r.runner.Wait()
		t.Fatalf("c-2 never started while instance 2's lane was free (item %s by %q)", r.hq.Item(id2).Status, r.hq.Item(id2).ClaimedBy)
	}
	close(r.lanes.block)
	r.runner.Wait()
	byChat := map[string]int{}
	for _, tr := range r.lanes.sent() {
		byChat[tr.chat] = tr.n
	}
	if byChat["c-1"] != 1 || byChat["c-2"] != 2 {
		t.Fatalf("chats placed %v; want c-1 on instance 1 and c-2 on instance 2", byChat)
	}
}

// TestTheWakeLoopRunsAChatAndNeverPromptsThePrimary: a pass of the real loop
// over a pending chat — nothing up first: instance 1 is started for it; next
// pass the chat runs in its lane and is answered, and the primary is never
// re-prompted for it.
func TestTheWakeLoopRunsAChatAndNeverPromptsThePrimary(t *testing.T) {
	r := newRig(t, 1, 30*time.Minute)
	lanes := newLanes()
	backends := officers.NewBackends(nil)
	runner, err := officers.NewChatRunner(officers.ChatRunnerConfig{Lanes: lanes, Backend: backends.Backend, Logger: log.New(r.logs, "", 0), HeartbeatEvery: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	var observed [][]officers.Item
	rec, err := officers.NewReconciler(officers.Config{Registry: officers.NewRegistry([]officers.Officer{r.o}), HQ: backends, Fleet: r.fleet,
		Logger: log.New(r.logs, "", 0), Now: r.clk.Now, Chats: runner,
		Observed: func(_ officers.Officer, claimed []officers.Item, _ time.Time) { observed = append(observed, claimed) }})
	if err != nil {
		t.Fatal(err)
	}
	r.hq.AddChat(relID, "c-41", "triage")
	seq, id := r.hq.MemberSays("c-41", "member1", "Member One", "Is web healthy?")
	res := rec.Check(context.Background(), relID, false)
	if res.Err != nil || fmt.Sprint(res.Decision.Start) != "[1]" {
		t.Fatalf("a chat with nothing up: start %v (%v), want [1]", res.Decision.Start, res.Err)
	}
	runner.Wait()
	if n := len(lanes.sent()); n != 0 {
		t.Fatalf("a chat ran on an instance the pass saw down (%d turns)", n)
	}
	r.clk.Advance(officers.RepromptAfter + time.Minute)
	res = rec.Check(context.Background(), relID, true)
	runner.Wait()
	if len(res.Decision.Reprompt) != 0 || len(r.fleet.reprompts) != 0 {
		t.Fatalf("the primary was re-prompted for a chat: %v", r.fleet.reprompts)
	}
	if rs := r.hq.ChatMessages("c-41"); len(rs) != 2 || rs[1].Role != "officer" || rs[1].InReplyTo != seq {
		t.Fatalf("chat after the second pass: %+v; want the officer's reply to %d; log:\n%s", rs, seq, r.logs)
	}
	if st := r.hq.Item(id).Status; st != "done" {
		t.Fatalf("item %s, want done", st)
	}
	// A chat claim live at a pass beside a queue claim: the observer (the
	// transcript forwarder, the LLM proxy's sync) is told of the queue claim
	// only — the positive control is that it sees that one.
	r.hq.AddChat(relID, "c-42", "other")
	_, chatID := r.hq.MemberSays("c-42", "member1", "Member One", "still there?")
	r.hq.SetClaim(chatID, officers.ChatHolder(officers.InstanceName(relID, 1)), r.clk.Now().Add(time.Hour))
	queue := r.hq.AddItem(relID, "release.evaluate", "svc:v1")
	r.hq.SetClaim(queue, officers.InstanceName(relID, 1), r.clk.Now().Add(time.Hour))
	observed = nil
	rec.Check(context.Background(), relID, true)
	runner.Wait()
	if len(observed) != 1 || len(observed[0]) != 1 || string(observed[0][0].ID) != queue {
		t.Fatalf("the observer was told %+v; want exactly the queue claim %s (never chat claim %s)", observed, queue, chatID)
	}
}

// TestChatSessionsIsDeclaredPerOfficer: `chat_sessions` absent is the default
// (and declares nothing for the pod), 1..MaxChatSessions is taken, anything
// else refused naming the field, and a host officer refuses it.
func TestChatSessionsIsDeclaredPerOfficer(t *testing.T) {
	env := func(k string) string {
		if k == officers.TokenEnvPrefix+officers.TokenEnvSuffix(relID) {
			return relTok
		}
		return ""
	}
	parse := func(extra string) ([]officers.Officer, error) {
		return officers.Parse(`[{"id":"`+relID+`","hq_url":"https://hq.example.com/api/v1"`+extra+`}]`, env, "")
	}
	got, err := parse("")
	if err != nil || got[0].ChatSessions() != officers.DefaultChatSessions || got[0].DeclaredChatSessions() != 0 {
		t.Fatalf("no chat_sessions: %v, %d declared %d; want the default %d and none declared", err, got[0].ChatSessions(), got[0].DeclaredChatSessions(), officers.DefaultChatSessions)
	}
	got, err = parse(`,"chat_sessions":2`)
	if err != nil || got[0].ChatSessions() != 2 || got[0].DeclaredChatSessions() != 2 {
		t.Fatalf("chat_sessions 2: %v, %+v", err, got)
	}
	for _, bad := range []string{`,"chat_sessions":6`, `,"chat_sessions":-1`} {
		if _, err := parse(bad); err == nil || !strings.Contains(err.Error(), "chat_sessions") {
			t.Fatalf("%s: %v; want a refusal naming chat_sessions", bad, err)
		}
	}
	_, err = officers.Parse(`[{"id":"`+relID+`","backend":"builtin","runtime":"host","host":"h1","chat_sessions":2}]`, func(string) string { return "" }, "")
	if err == nil || !strings.Contains(err.Error(), "chat_sessions") {
		t.Fatalf("a host officer with chat_sessions: %v; want refused naming it", err)
	}
}

// TestChatOfReadsThePayloadThenTheSubject: the payload's {chat_id, seq} (a
// numeric chat id too), the subject's id when the payload names none, and
// nothing for an id muster will not put in a path.
func TestChatOfReadsThePayloadThenTheSubject(t *testing.T) {
	it := chatItem("7", "c-41", 3)
	if id, seq, ok := officers.ChatOf(it); !ok || id != "c-41" || seq != 3 {
		t.Fatalf("payload: %q %d %v", id, seq, ok)
	}
	it.Payload = json.RawMessage(`{"chat_id":9001,"seq":2}`)
	if id, seq, ok := officers.ChatOf(it); !ok || id != "9001" || seq != 2 {
		t.Fatalf("numeric chat id: %q %d %v", id, seq, ok)
	}
	it.Payload = nil
	if id, _, ok := officers.ChatOf(it); !ok || id != "c-41" {
		t.Fatalf("subject: %q %v", id, ok)
	}
	it.SubjectKey = "chat:../x"
	if _, _, ok := officers.ChatOf(it); ok {
		t.Fatal("a chat id with a path separator was accepted")
	}
	if _, _, ok := officers.ChatOf(officers.Item{Kind: "release.evaluate", SubjectKey: "chat:c-1"}); ok {
		t.Fatal("a non-chat item read as a chat")
	}
}

// THE HQ SERVER'S REPLY REFUSALS (its chat routes): 409 chat_closed and 409
// stale_turn are final for that turn — muster drops it (marks it answered, so
// no later claim asks the lane again) and never retries the post; a reply
// with a NUL (HQ's 400) is cleaned, not refused; and a messages page that says
// more: true is read to its end.

// TestAChatClosedMidTurnIsDroppedNotRetried: the member closes the chat while
// the lane answers. The reply's 409 chat_closed ends the turn: no reply, the
// marks answered (no "your reply was lost" later), the item left to HQ.
func TestAChatClosedMidTurnIsDroppedNotRetried(t *testing.T) {
	r := newChatRig(t, 1)
	r.hq.AddChat(relID, "c-41", "triage")
	_, id := r.hq.MemberSays("c-41", "member1", "Member One", "Is web healthy?")
	r.lanes.reply = func(chat, _ string) (string, error) { r.hq.CloseChat(chat); return "yes", nil }
	if got := r.runner.Run(context.Background(), r.o, 1, r.item(t, id)); got != officers.ChatReplyDropped {
		t.Fatalf("Run = %s, want %s; log:\n%s", got, officers.ChatReplyDropped, r.logs)
	}
	if m := r.lanes.marksOf("c-41"); m.Seen == 0 || m.Answered != m.Seen {
		t.Fatalf("marks after a closed chat %+v; want answered = seen (the turn dropped, never re-asked)", m)
	}
	if n := len(r.officerReplies("c-41")); n != 0 {
		t.Fatalf("%d replies on a closed chat", n)
	}
}

// TestAStaleTurnIsDroppedAndTheNewTurnAnswered: a newer member turn landed
// while the lane answered the older one; HQ answers the old reply 409
// stale_turn. That turn is dropped and the item released (it carries the new
// turn); the next run sends only the new message — no lost-reply prompt.
func TestAStaleTurnIsDroppedAndTheNewTurnAnswered(t *testing.T) {
	r := newChatRig(t, 1)
	r.hq.AddChat(relID, "c-41", "triage")
	_, id := r.hq.MemberSays("c-41", "member1", "Member One", "first question")
	var seq2 int64
	r.lanes.reply = func(chat, _ string) (string, error) {
		seq2, _ = r.hq.MemberSays(chat, "member1", "Member One", "second question")
		return "answer to the first", nil
	}
	if got := r.runner.Run(context.Background(), r.o, 1, r.item(t, id)); got != officers.ChatReplyDropped {
		t.Fatalf("Run = %s, want %s; log:\n%s", got, officers.ChatReplyDropped, r.logs)
	}
	if it := r.hq.Item(id); it.Status != "open" {
		t.Fatalf("after a stale turn the item is %s by %q; want open again (it carries the new turn)", it.Status, it.ClaimedBy)
	}
	r.lanes.reply = nil
	if got := r.runner.Run(context.Background(), r.o, 1, r.item(t, id)); got != officers.ChatReplied {
		t.Fatalf("the new turn: Run = %s, want %s; log:\n%s", got, officers.ChatReplied, r.logs)
	}
	turns := r.lanes.sent()
	last := turns[len(turns)-1].text
	if !strings.Contains(last, "second question") || strings.Contains(last, "first question") || strings.Contains(last, "did not reach") {
		t.Fatalf("the new turn sent %q; want only the second question", last)
	}
	if rs := r.officerReplies("c-41"); len(rs) != 1 || rs[0].InReplyTo != seq2 {
		t.Fatalf("replies %+v; want one, to %d", rs, seq2)
	}
}

// TestAReplyWithANulIsCleanedNotRefused: HQ refuses a NUL (400); the lane's
// text is posted without it.
func TestAReplyWithANulIsCleanedNotRefused(t *testing.T) {
	r := newChatRig(t, 1)
	r.hq.AddChat(relID, "c-41", "triage")
	_, id := r.hq.MemberSays("c-41", "member1", "Member One", "q")
	r.lanes.reply = func(string, string) (string, error) { return "ok\x00 done", nil }
	if got := r.runner.Run(context.Background(), r.o, 1, r.item(t, id)); got != officers.ChatReplied {
		t.Fatalf("Run = %s, want %s; log:\n%s", got, officers.ChatReplied, r.logs)
	}
	if rs := r.officerReplies("c-41"); len(rs) != 1 || rs[0].Text != "ok done" {
		t.Fatalf("replies %+v; want one, %q", rs, "ok done")
	}
}

// TestAMessagesPageThatSaysMoreIsReadToItsEnd: two member messages wait and
// HQ pages one at a time (more: true); the lane is sent both, and the reply
// answers the later.
func TestAMessagesPageThatSaysMoreIsReadToItsEnd(t *testing.T) {
	r := newChatRig(t, 1)
	r.hq.ChatPageSize = 1
	r.hq.AddChat(relID, "c-41", "triage")
	r.hq.MemberSays("c-41", "member1", "Member One", "part one")
	seq2, id := r.hq.MemberSays("c-41", "member1", "Member One", "part two")
	if got := r.runner.Run(context.Background(), r.o, 1, r.item(t, id)); got != officers.ChatReplied {
		t.Fatalf("Run = %s, want %s; log:\n%s", got, officers.ChatReplied, r.logs)
	}
	turns := r.lanes.sent()
	if len(turns) != 1 || !strings.Contains(turns[0].text, "part one") || !strings.Contains(turns[0].text, "part two") {
		t.Fatalf("the lane got %+v; want one turn with both messages", turns)
	}
	if rs := r.officerReplies("c-41"); len(rs) != 1 || rs[0].InReplyTo != seq2 {
		t.Fatalf("replies %+v; want one, to %d", rs, seq2)
	}
}
