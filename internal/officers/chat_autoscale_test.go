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

// CHAT-INSTANCE AUTOSCALING (docs/officers.md "Chat sessions", "Autoscaling
// chat instances"). A pool that runs chats may start instances past
// max_instances, up to chat_max_instances, when every lane of every up
// instance is busy; an instance with no chat turn, claim or prompt for
// chat_idle_stop is stopped, never one doing queue work; a chat whose instance
// is down or past the ceiling is re-homed onto a running one.

var autoNow = time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC)

func downInstance(o officers.Officer, n int) officers.InstanceState {
	return officers.InstanceState{N: n, Name: o.InstanceName(n)}
}

// busyRig is a clock rig whose officer runs `sessions` lanes an instance,
// with chat c-busy's turn running (blocked) on instance 1. release ends it.
func busyRig(t *testing.T, max, sessions int) (r *chatRig, clk *clock, release func()) {
	t.Helper()
	r, clk = clockRig(t, max)
	r.o = r.o.WithChatSessions(sessions)
	r.hq.AddChat(relID, "c-busy", "busy")
	_, id := r.hq.MemberSays("c-busy", "member1", "Member One", "a long question")
	r.lanes.block = make(chan struct{})
	r.lanes.started = make(chan string, 8)
	r.runner.Dispatch(context.Background(), r.o, officers.Observation{Chats: true, Open: []officers.Item{r.item(t, id)},
		Instances: []officers.InstanceState{upInstance(r.o, 1)}}, clk.Now())
	if got := <-r.lanes.started; got != "c-busy" {
		t.Fatalf("the busy turn was %s's", got)
	}
	once := false
	release = func() {
		if !once {
			once = true
			close(r.lanes.block)
			r.runner.Wait()
		}
	}
	t.Cleanup(release)
	return r, clk, release
}

// waiting adds n new chats (no lane yet) and answers their open items.
func waiting(t *testing.T, r *chatRig, n int) []officers.Item {
	t.Helper()
	var out []officers.Item
	for i := 1; i <= n; i++ {
		chat := fmt.Sprintf("c-w%d", i)
		r.hq.AddChat(relID, chat, chat)
		_, id := r.hq.MemberSays(chat, "member1", "Member One", "question "+chat)
		out = append(out, r.item(t, id))
	}
	return out
}

// TestAChatScalesUpWhenEveryLaneIsBusy: instance 1's one lane runs a turn and
// two new chats wait. The plan needs ONE more instance per chat_sessions lanes
// (here one lane, so two), the first is started past max_instances (1), and
// it is never more than the ceiling allows. With three lanes an instance, the
// same two chats need one instance, not two.
func TestAChatScalesUpWhenEveryLaneIsBusy(t *testing.T) {
	r, clk, _ := busyRig(t, 1, 1)
	insts := []officers.InstanceState{upInstance(r.o, 1), downInstance(r.o, 2), downInstance(r.o, 3)}
	obs := officers.Observation{Chats: true, Open: waiting(t, r, 2), Instances: insts}
	plan, held := r.runner.Want(context.Background(), r.o, obs, clk.Now())
	if len(held) != 0 || plan.Need != 2 || plan.Waiting != 2 || plan.Busy[1] != 1 {
		t.Fatalf("two chats waiting, instance 1's one lane busy: plan %+v held %v; want Need 2, Waiting 2, Busy[1] 1, nothing held", plan, held)
	}
	obs.ChatPlan = plan
	d := officers.Decide(r.o, obs, time.Time{}, clk.Now(), nil)
	if fmt.Sprint(d.Start) != "[2 3]" {
		t.Fatalf("scale-up: start %v, want [2 3] (past max_instances 1, within chat_max_instances 3)", d.Start)
	}
	// The ceiling: chat_max_instances 2 starts instance 2 only, though the
	// plan asks for two.
	if d := officers.Decide(r.o.WithChatMaxInstances(2), obs, time.Time{}, clk.Now(), nil); fmt.Sprint(d.Start) != "[2]" {
		t.Fatalf("chat_max_instances 2: start %v, want [2] only", d.Start)
	}
	// Three lanes an instance: the busy turn takes one, two are free, so the
	// two chats need nothing started.
	r3 := r.o.WithChatSessions(3)
	plan, _ = r.runner.Want(context.Background(), r3, obs, clk.Now())
	obs.ChatPlan = plan
	if d := officers.Decide(r3, obs, time.Time{}, clk.Now(), nil); plan.Need != 0 || len(d.Start) != 0 {
		t.Fatalf("a free lane on instance 1: plan %+v, start %v; want nothing started", plan, d.Start)
	}
}

// TestAScaleUpStartsNoMoreThanTheChatsNeed: four chats wait, every lane of
// instance 1 busy, two lanes an instance: two instances, not three — and an
// instance the queue starts this pass (within the ceiling) counts toward it.
func TestAScaleUpStartsNoMoreThanTheChatsNeed(t *testing.T) {
	o := officers.Officer{ID: relID, MaxInstances: 2, IdleAfter: time.Hour}.WithChatSessions(2).WithChatMaxInstances(5)
	insts := []officers.InstanceState{upInstance(o, 1)}
	for n := 2; n <= 5; n++ {
		insts = append(insts, downInstance(o, n))
	}
	obs := officers.Observation{Chats: true, Open: []officers.Item{chatItem("c1", "c-1", 1), chatItem("c2", "c-2", 1), chatItem("c3", "c-3", 1), chatItem("c4", "c-4", 1)},
		Instances: insts, ChatPlan: officers.ChatPlan{Need: 2, Waiting: 4, Unplaced: 4, Busy: map[int]int{1: 2}}}
	if d := officers.Decide(o, obs, time.Time{}, autoNow, nil); fmt.Sprint(d.Start) != "[2 3]" {
		t.Fatalf("Need 2: start %v, want [2 3]", d.Start)
	}
	// A queue item wants instance 2 started anyway: it gives the chats lanes
	// too, so one more (3), not two more.
	obs.Open = append(obs.Open, officers.Item{ID: "q1", Kind: "release.evaluate", Status: officers.StatusOpen},
		officers.Item{ID: "q2", Kind: "release.evaluate", Status: officers.StatusOpen})
	if d := officers.Decide(o, obs, time.Time{}, autoNow, nil); fmt.Sprint(d.Start) != "[2 3]" {
		t.Fatalf("Need 2 beside a queue start of 2: start %v, want [2 3]", d.Start)
	}
	// Prefer: a down home is started before the lowest free number.
	obs.Open = obs.Open[:4]
	obs.ChatPlan.Need, obs.ChatPlan.Prefer = 1, []int{4}
	if d := officers.Decide(o, obs, time.Time{}, autoNow, nil); fmt.Sprint(d.Start) != "[4]" {
		t.Fatalf("Need 1 preferring instance 4 (a waiting chat's home): start %v, want [4]", d.Start)
	}
	// A preferred home past the ceiling is not started; the lowest free one is.
	obs.ChatPlan.Prefer = []int{7}
	if d := officers.Decide(o, obs, time.Time{}, autoNow, nil); fmt.Sprint(d.Start) != "[2]" {
		t.Fatalf("Need 1 preferring instance 7 (past chat_max_instances 5): start %v, want [2]", d.Start)
	}
}

// TestAChatOnlyInstanceTakesNoQueueWork: instance 3 (past max_instances 2) is
// up for chats. It is never re-prompted for queue work, and it does not count
// as queue capacity: two claimable items start instances 1 and 2.
func TestAChatOnlyInstanceTakesNoQueueWork(t *testing.T) {
	o := officers.Officer{ID: relID, MaxInstances: 2, IdleAfter: time.Hour}
	q := func(id string) officers.Item {
		return officers.Item{ID: officers.ItemID(id), Kind: "release.evaluate", Status: officers.StatusOpen}
	}
	obs := officers.Observation{Chats: true, Open: []officers.Item{q("q1"), q("q2")},
		Instances: []officers.InstanceState{downInstance(o, 1), downInstance(o, 2), upInstance(o, 3)}}
	d := officers.Decide(o, obs, time.Time{}, autoNow, nil)
	if fmt.Sprint(d.Start) != "[1 2]" || len(d.Reprompt) != 0 {
		t.Fatalf("two claimable, chat-only instance 3 up: start %v reprompt %v; want [1 2] and no re-prompt", d.Start, d.Reprompt)
	}
	obs.Instances = []officers.InstanceState{upInstance(o, 1), upInstance(o, 2), upInstance(o, 3)}
	obs.Open = append(obs.Open, q("q3"))
	if d := officers.Decide(o, obs, time.Time{}, autoNow, nil); fmt.Sprint(d.Reprompt) != "[1 2]" {
		t.Fatalf("three claimable, all up: reprompt %v, want [1 2] (never chat-only instance 3)", d.Reprompt)
	}
}

// TestAnIdleChatInstanceIsStoppedAndAQueueWorkerNever: the pool is busy
// (instance 1 holds a queue item). chat_idle_stop is 20m (not the default,
// so a mutant reading the default is seen). Instance 2, quiet 21m, is
// stopped; instance 3, quiet 19m, is not; instance 1, quiet for hours but
// holding the claim, is not.
func TestAnIdleChatInstanceIsStoppedAndAQueueWorkerNever(t *testing.T) {
	o := officers.Officer{ID: relID, MaxInstances: 1, IdleAfter: time.Hour}.WithChatIdleStop(20 * time.Minute)
	exp := autoNow.Add(time.Minute)
	queue := officers.Item{ID: "q1", Kind: "release.evaluate", Status: officers.StatusClaimed, ClaimedBy: o.InstanceName(1), LeaseExpiresAt: &exp}
	base := func() officers.Observation {
		return officers.Observation{Chats: true, Claimed: []officers.Item{queue},
			Instances: []officers.InstanceState{upInstance(o, 1), upInstance(o, 2), upInstance(o, 3)},
			ChatPlan: officers.ChatPlan{ActiveAt: map[int]time.Time{1: autoNow.Add(-3 * time.Hour), 2: autoNow.Add(-21 * time.Minute), 3: autoNow.Add(-19 * time.Minute)},
				Busy: map[int]int{}, PendingOn: map[int]int{}}}
	}
	d := officers.Decide(o, base(), time.Time{}, autoNow, nil)
	if fmt.Sprint(d.Stop) != "[2]" || d.Idle {
		t.Fatalf("chat-idle stop: stop %v idle %v; want [2] while busy", d.Stop, d.Idle)
	}
	// Each of these keeps instance 2 up.
	for name, mut := range map[string]func(*officers.Observation, map[int]time.Time){
		"a chat turn running on it":         func(obs *officers.Observation, _ map[int]time.Time) { obs.ChatPlan.Busy[2] = 1 },
		"a pending chat whose home it is":   func(obs *officers.Observation, _ map[int]time.Time) { obs.ChatPlan.PendingOn[2] = 1 },
		"a waiting chat with no home":       func(obs *officers.Observation, _ map[int]time.Time) { obs.ChatPlan.Unplaced = 1 },
		"started or prompted 5 minutes ago": func(_ *officers.Observation, p map[int]time.Time) { p[2] = autoNow.Add(-5 * time.Minute) },
		"a prompt owed to it":               func(obs *officers.Observation, _ map[int]time.Time) { obs.Instances[1].PromptPending = true },
		"never seen active by this process": func(obs *officers.Observation, _ map[int]time.Time) { delete(obs.ChatPlan.ActiveAt, 2) },
		"in error (it holds its slot)":      func(obs *officers.Observation, _ map[int]time.Time) { obs.Instances[1].Error = true },
		"a live chat claim held as its holder": func(obs *officers.Observation, _ map[int]time.Time) {
			obs.Claimed = append(obs.Claimed, liveChatClaim(o, 2))
		},
	} {
		obs, prompted := base(), map[int]time.Time{}
		mut(&obs, prompted)
		if d := officers.Decide(o, obs, time.Time{}, autoNow, prompted); len(d.Stop) != 0 {
			t.Errorf("%s: stop %v, want instance 2 kept up", name, d.Stop)
		}
	}
	// With no chats run by muster, nothing is stopped for chats.
	obs := base()
	obs.Chats = false
	if d := officers.Decide(o, obs, time.Time{}, autoNow, nil); len(d.Stop) != 0 {
		t.Fatalf("chats off: stop %v, want none", d.Stop)
	}
}

func liveChatClaim(o officers.Officer, n int) officers.Item {
	exp := autoNow.Add(time.Minute)
	it := chatItem(fmt.Sprintf("ch%d", n), fmt.Sprintf("c-held-%d", n), 1)
	it.Status, it.ClaimedBy, it.LeaseExpiresAt = officers.StatusClaimed, officers.ChatHolder(o.InstanceName(n)), &exp
	return it
}

// TestTheQueuesInstanceIsNeverStoppedWhileTheQueueHasWork: instance 1 quiet
// for an hour, holding nothing, while an item is claimable: it is the queue's
// (it is re-prompted), never chat-stopped. Once the queue has nothing at all —
// the pool busy only with a chat on instance 2 — instance 1 is stopped like
// any idle instance, and chat-only instance 2 with its claim stays.
func TestTheQueuesInstanceIsNeverStoppedWhileTheQueueHasWork(t *testing.T) {
	o := officers.Officer{ID: relID, MaxInstances: 1, IdleAfter: 2 * time.Hour}.WithChatIdleStop(20 * time.Minute)
	active := map[int]time.Time{1: autoNow.Add(-time.Hour), 2: autoNow.Add(-time.Hour)}
	obs := officers.Observation{Chats: true, Open: []officers.Item{{ID: "q1", Kind: "release.evaluate", Status: officers.StatusOpen}},
		Claimed: []officers.Item{liveChatClaim(o, 2)}, Instances: []officers.InstanceState{upInstance(o, 1), upInstance(o, 2)},
		ChatPlan: officers.ChatPlan{ActiveAt: active}}
	d := officers.Decide(o, obs, time.Time{}, autoNow, nil)
	if len(d.Stop) != 0 || fmt.Sprint(d.Reprompt) != "[1]" {
		t.Fatalf("a claimable item, instance 1 quiet: stop %v reprompt %v; want no stop and instance 1 re-prompted", d.Stop, d.Reprompt)
	}
	obs.Open = nil
	if d := officers.Decide(o, obs, time.Time{}, autoNow, nil); fmt.Sprint(d.Stop) != "[1]" {
		t.Fatalf("no queue work, a chat claim on instance 2: stop %v, want [1]", d.Stop)
	}
}

// TestAChatWhoseInstanceStoppedIsRehomed: chat c-41 was answered on instance
// 2, which has been stopped; instance 1 is up with a free lane. The chat is
// not held back and needs nothing started; its next turn runs on instance 1,
// its old lane is set aside (Rehome), and the new lane starts after the
// officer's last reply.
func TestAChatWhoseInstanceStoppedIsRehomed(t *testing.T) {
	r, clk := clockRig(t, 1)
	r.hq.AddChat(relID, "c-41", "triage")
	_, id := r.hq.MemberSays("c-41", "member1", "Member One", "old question")
	if got := r.runner.Run(context.Background(), r.o, 2, r.item(t, id)); got != officers.ChatReplied {
		t.Fatalf("the first turn, on instance 2: %s; log:\n%s", got, r.logs)
	}
	_, id = r.hq.MemberSays("c-41", "member1", "Member One", "new question")
	obs := officers.Observation{Chats: true, Open: []officers.Item{r.item(t, id)}, Instances: []officers.InstanceState{upInstance(r.o, 1), downInstance(r.o, 2)}}
	plan, held := r.runner.Want(context.Background(), r.o, obs, clk.Now())
	if held[officers.ItemID(id)] || plan.Need != 0 {
		t.Fatalf("a chat whose instance stopped, a free lane on 1: plan %+v held %v; want nothing started, nothing held", plan, held)
	}
	r.runner.Dispatch(context.Background(), r.o, obs, clk.Now())
	r.runner.Wait()
	turns := r.lanes.sent()
	last := turns[len(turns)-1]
	if len(turns) != 2 || last.n != 1 || !strings.Contains(last.text, "new question") || strings.Contains(last.text, "old question") {
		t.Fatalf("turns %+v; want the second on instance 1 carrying only what came after the last reply; log:\n%s", turns, r.logs)
	}
	if fmt.Sprint(r.lanes.rehomes) != "[c-41:2]" {
		t.Fatalf("rehomes %v, want [c-41:2]", r.lanes.rehomes)
	}
	if !strings.Contains(r.logs.String(), "re-homed") {
		t.Fatalf("no log line names the re-home:\n%s", r.logs)
	}
	if st := r.hq.Item(id).Status; st != "done" {
		t.Fatalf("item %s, want done", st)
	}
}

// TestAChatPastTheCeilingIsRehomedNotHeld replaces chat sessions' v1 rule
// ("held back while its home is beyond max_instances"): a chat whose lane
// lives on instance 3 of a pool whose chat ceiling is now 2 is placed on an up
// instance with a free lane, its lane on 3 set aside.
func TestAChatPastTheCeilingIsRehomedNotHeld(t *testing.T) {
	r, clk := clockRig(t, 1)
	r.o = r.o.WithChatMaxInstances(2)
	r.hq.AddChat(relID, "c-41", "triage")
	_, id := r.hq.MemberSays("c-41", "member1", "Member One", "a question")
	r.lanes.homes["c-41"] = 3
	obs := officers.Observation{Chats: true, Open: []officers.Item{r.item(t, id)}, Instances: []officers.InstanceState{upInstance(r.o, 1), downInstance(r.o, 2)}}
	if _, held := r.runner.Want(context.Background(), r.o, obs, clk.Now()); held[officers.ItemID(id)] {
		t.Fatalf("a chat whose home is past the ceiling is held back; want it re-homed")
	}
	r.runner.Dispatch(context.Background(), r.o, obs, clk.Now())
	r.runner.Wait()
	if turns := r.lanes.sent(); len(turns) != 1 || turns[0].n != 1 {
		t.Fatalf("turns %+v; want one on instance 1; log:\n%s", turns, r.logs)
	}
	if fmt.Sprint(r.lanes.rehomes) != "[c-41:3]" {
		t.Fatalf("rehomes %v, want [c-41:3]", r.lanes.rehomes)
	}
}

// TestADownHomeIsStartedOnlyWhenNoLaneIsFree: chat c-41's home (instance 2)
// is down and instance 1's one lane is busy: the plan prefers starting its
// home (its conversation is there) over the lowest free number.
func TestADownHomeIsStartedOnlyWhenNoLaneIsFree(t *testing.T) {
	r, clk, _ := busyRig(t, 1, 1)
	r.o = r.o.WithChatMaxInstances(4)
	r.hq.AddChat(relID, "c-41", "triage")
	_, id := r.hq.MemberSays("c-41", "member1", "Member One", "a question")
	r.lanes.homes["c-41"] = 3
	insts := []officers.InstanceState{upInstance(r.o, 1), downInstance(r.o, 2), downInstance(r.o, 3), downInstance(r.o, 4)}
	obs := officers.Observation{Chats: true, Open: []officers.Item{r.item(t, id)}, Instances: insts}
	plan, _ := r.runner.Want(context.Background(), r.o, obs, clk.Now())
	obs.ChatPlan = plan
	if d := officers.Decide(r.o, obs, time.Time{}, clk.Now(), nil); plan.Need != 1 || fmt.Sprint(plan.Prefer) != "[3]" || fmt.Sprint(d.Start) != "[3]" {
		t.Fatalf("a down home, no free lane: plan %+v start %v; want Need 1, Prefer [3], start [3]", plan, d.Start)
	}
}

// TestTheRunnerTracksAnInstancesLastChatActivity: the plan's ActiveAt is when
// the runner first saw the instance up, moved on by a chat turn on it and by a
// live claim of its own; an instance seen down is forgotten (seen up again, it
// starts a fresh window).
func TestTheRunnerTracksAnInstancesLastChatActivity(t *testing.T) {
	r, clk := clockRig(t, 1)
	t0 := clk.Now()
	up := officers.Observation{Chats: true, Instances: []officers.InstanceState{upInstance(r.o, 1), upInstance(r.o, 2)}}
	plan, _ := r.runner.Want(context.Background(), r.o, up, clk.Now())
	if !plan.ActiveAt[1].Equal(t0) || !plan.ActiveAt[2].Equal(t0) {
		t.Fatalf("first seen up: ActiveAt %v, want both at %s", plan.ActiveAt, t0)
	}
	clk.Advance(10 * time.Minute)
	r.hq.AddChat(relID, "c-41", "triage")
	_, id := r.hq.MemberSays("c-41", "member1", "Member One", "a question")
	if got := r.runner.Run(context.Background(), r.o, 2, r.item(t, id)); got != officers.ChatReplied {
		t.Fatalf("Run on 2: %s", got)
	}
	clk.Advance(10 * time.Minute)
	exp := clk.Now().Add(time.Hour)
	queue := officers.Item{ID: "q1", Kind: "release.evaluate", Status: officers.StatusClaimed, ClaimedBy: r.o.InstanceName(1), LeaseExpiresAt: &exp}
	withClaim := up
	withClaim.Claimed = []officers.Item{queue}
	plan, _ = r.runner.Want(context.Background(), r.o, withClaim, clk.Now())
	if !plan.ActiveAt[1].Equal(clk.Now()) {
		t.Fatalf("instance 1 holding a claim: ActiveAt %s, want now %s", plan.ActiveAt[1], clk.Now())
	}
	if !plan.ActiveAt[2].Equal(t0.Add(10 * time.Minute)) {
		t.Fatalf("instance 2 after a chat turn at +10m: ActiveAt %s, want %s", plan.ActiveAt[2], t0.Add(10*time.Minute))
	}
	down := officers.Observation{Chats: true, Instances: []officers.InstanceState{upInstance(r.o, 1), downInstance(r.o, 2)}}
	r.runner.Want(context.Background(), r.o, down, clk.Now())
	clk.Advance(time.Minute)
	plan, _ = r.runner.Want(context.Background(), r.o, up, clk.Now())
	if !plan.ActiveAt[2].Equal(clk.Now()) {
		t.Fatalf("instance 2 seen down then up: ActiveAt %s, want a fresh window at %s", plan.ActiveAt[2], clk.Now())
	}
}

// TestTheWakeLoopScalesChatInstancesUpAndDown is the seam: the real loop
// (observe → Want → Decide → act → Dispatch). Instance 1 holds a queue item
// and its one lane runs chat c-1; chat c-2 arrives: instance 2 is started,
// with a log line saying why. c-2 runs there. Then, both chats answered and
// chat_idle_stop passed, instance 2 is stopped (logged) and instance 1, which
// holds the queue item, is not.
func TestTheWakeLoopScalesChatInstancesUpAndDown(t *testing.T) {
	r := newRig(t, 1, 3*time.Hour)
	r.o = r.o.WithChatSessions(1).WithChatIdleStop(20 * time.Minute)
	lanes := newLanes()
	lanes.block = make(chan struct{})
	lanes.started = make(chan string, 8)
	backends := officers.NewBackends(nil)
	runner, err := officers.NewChatRunner(officers.ChatRunnerConfig{Lanes: lanes, Backend: backends.Backend, Logger: log.New(r.logs, "", 0),
		HeartbeatEvery: time.Hour, Now: r.clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := officers.NewReconciler(officers.Config{Registry: officers.NewRegistry([]officers.Officer{r.o}), HQ: backends, Fleet: r.fleet,
		Logger: log.New(r.logs, "", 0), Now: r.clk.Now, Chats: runner})
	if err != nil {
		t.Fatal(err)
	}
	r.fleet.up[1], r.fleet.exists[1] = true, true
	queue := r.hq.AddItem(relID, "release.evaluate", "svc:v1")
	r.hq.SetClaim(queue, officers.InstanceName(relID, 1), r.clk.Now().Add(24*time.Hour))
	r.hq.AddChat(relID, "c-1", "one")
	r.hq.MemberSays("c-1", "member1", "Member One", "q1")
	rec.Check(context.Background(), relID, true)
	if got := <-lanes.started; got != "c-1" {
		t.Fatalf("first turn %s, want c-1", got)
	}
	r.hq.AddChat(relID, "c-2", "two")
	r.hq.MemberSays("c-2", "member1", "Member One", "q2")
	res := rec.Check(context.Background(), relID, true)
	if fmt.Sprint(res.Decision.Start) != "[2]" {
		t.Fatalf("c-2 waits, instance 1's one lane busy: start %v, want [2]; log:\n%s", res.Decision.Start, r.logs)
	}
	if !strings.Contains(r.logs.String(), "chat scale-up") {
		t.Fatalf("no log line names the chat scale-up:\n%s", r.logs)
	}
	// A second pass while instance 2 boots starts nothing more.
	if res := rec.Check(context.Background(), relID, true); len(res.Decision.Start) != 0 {
		t.Fatalf("the next pass: start %v, want none (instance 2 is on its way up)", res.Decision.Start)
	}
	close(lanes.block)
	runner.Wait()
	byChat := map[string]int{}
	for _, tr := range lanes.sent() {
		byChat[tr.chat] = tr.n
	}
	if byChat["c-1"] != 1 || byChat["c-2"] != 2 {
		t.Fatalf("turns by chat %v; want c-1 on 1 and c-2 on 2; log:\n%s", byChat, r.logs)
	}
	r.clk.Advance(21 * time.Minute)
	res = rec.Check(context.Background(), relID, true)
	runner.Wait()
	if fmt.Sprint(res.Decision.Stop) != "[2]" {
		t.Fatalf("chat_idle_stop passed: stop %v, want [2] (never instance 1, which holds the queue item); log:\n%s", res.Decision.Stop, r.logs)
	}
	if !strings.Contains(r.logs.String(), "chat scale-down") {
		t.Fatalf("no log line names the chat scale-down:\n%s", r.logs)
	}
	// The stopped instance is not started again for nothing (no flap).
	if res := rec.Check(context.Background(), relID, true); len(res.Decision.Start) != 0 || len(res.Decision.Stop) != 0 {
		t.Fatalf("the pass after the stop: start %v stop %v, want neither", res.Decision.Start, res.Decision.Stop)
	}
}

// TestChatInstancesAreTheJudgementPoolsAndAPodsOnly: the instance numbering.
// A pool that runs chats (an untiered HQ pod officer, or a tiered one's
// judgement pool) has max(max_instances, chat_max_instances) slots, the ones
// past max_instances chat-only; a routine pool, a host officer and a builtin
// one have max_instances only.
func TestChatInstancesAreTheJudgementPoolsAndAPodsOnly(t *testing.T) {
	untiered := officers.Officer{ID: relID, MaxInstances: 1}.WithChatMaxInstances(4)
	judge := untiered
	judge.Tier = officers.TierJudgement
	routine := untiered
	routine.Tier = officers.TierRoutine
	host := untiered
	host.Backend, host.Runtime, host.Host = officers.BackendBuiltin, officers.RuntimeHost, "h1"
	builtin := untiered
	builtin.Backend = officers.BackendBuiltin
	wide := officers.Officer{ID: relID, MaxInstances: 5}.WithChatMaxInstances(3)
	for _, c := range []struct {
		name  string
		o     officers.Officer
		slots int
		only  string
	}{
		{"untiered", untiered, 4, "[2 3 4]"},
		{"judgement pool", judge, 4, "[2 3 4]"},
		{"routine pool", routine, 1, "[]"},
		{"host officer", host, 1, "[]"},
		{"builtin officer", builtin, 1, "[]"},
		{"max_instances above the ceiling", wide, 5, "[]"},
		{"the default ceiling", officers.Officer{ID: relID, MaxInstances: 1}, 3, "[2 3]"},
	} {
		var only []int
		for n := 0; n <= 9; n++ {
			if c.o.ChatOnly(n) {
				only = append(only, n)
			}
		}
		if c.o.Slots() != c.slots || fmt.Sprint(only) != c.only {
			t.Errorf("%s: slots %d chat-only %v; want %d and %s", c.name, c.o.Slots(), only, c.slots, c.only)
		}
	}
	// A chat-only instance's agent name is the pool's instance; one past the
	// slots is not.
	if p, n, ok := officers.ParseInstance(untiered, untiered.InstanceName(4)); !ok || n != 4 || p.Key() != untiered.Key() {
		t.Errorf("instance 4's name: %v %d %v; want instance 4 of the officer", p.ID, n, ok)
	}
	if _, _, ok := officers.ParseInstance(untiered, untiered.InstanceName(5)); ok {
		t.Errorf("instance 5's name (past the slots) parsed as an instance")
	}
}

// TestAChatOnlyInstancesKickoffSaysItTakesNoQueueWork pins the whole first
// message a chat-only instance's primary gets; instance 1's is the queue's.
func TestAChatOnlyInstancesKickoffSaysItTakesNoQueueWork(t *testing.T) {
	o := officers.Officer{ID: relID, MaxInstances: 1}
	want := "You are officer release:example-web, instance 2 (officer-release-example-web-2). " +
		"muster started this instance for chat sessions: it runs each chat with a team member in a Claude Code session of its own beside this one, and gives this session no queue work. " +
		"Do not claim items: officer_claim refuses this instance (instance 1 works the queue). There is nothing to do in this session: end your turn now."
	if got := officers.KickoffText(o, 2); got != want {
		t.Fatalf("chat-only kickoff:\n%q\nwant\n%q", got, want)
	}
	if got := officers.KickoffText(o, 1); strings.Contains(got, "chat sessions") || !strings.Contains(got, "Work the queue") {
		t.Fatalf("instance 1's kickoff is not the queue's: %q", got)
	}
}

// TestChatMaxInstancesAndChatIdleStopAreDeclaredPerOfficer: absent is the
// default; 1..MaxChatMaxInstances and a duration in MinChatIdleStop..
// MaxChatIdleStop are taken; anything else refused naming the field; a host
// officer refuses both.
func TestChatMaxInstancesAndChatIdleStopAreDeclaredPerOfficer(t *testing.T) {
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
	if err != nil || got[0].ChatMaxInstances() != 3 || got[0].ChatIdleStop() != 15*time.Minute || got[0].Slots() != 3 {
		t.Fatalf("neither declared: %v; want ceiling 3, idle stop 15m, 3 slots", err)
	}
	got, err = parse(`,"chat_max_instances":5,"chat_idle_stop":"40m"`)
	if err != nil || got[0].ChatMaxInstances() != 5 || got[0].ChatIdleStop() != 40*time.Minute || got[0].Slots() != 5 {
		t.Fatalf("chat_max_instances 5, chat_idle_stop 40m: %v", err)
	}
	for _, bad := range []struct{ extra, field string }{
		{`,"chat_max_instances":9`, "chat_max_instances"},
		{`,"chat_max_instances":-1`, "chat_max_instances"},
		{`,"chat_idle_stop":"30s"`, "chat_idle_stop"},
		{`,"chat_idle_stop":"25h"`, "chat_idle_stop"},
		{`,"chat_idle_stop":"soon"`, "chat_idle_stop"},
	} {
		if _, err := parse(bad.extra); err == nil || !strings.Contains(err.Error(), bad.field) {
			t.Errorf("%s: %v; want a refusal naming %s", bad.extra, err, bad.field)
		}
	}
	for _, f := range []string{`"chat_max_instances":2`, `"chat_idle_stop":"20m"`} {
		_, err := officers.Parse(`[{"id":"`+relID+`","backend":"builtin","runtime":"host","host":"h1",`+f+`}]`, func(string) string { return "" }, "")
		if field := strings.SplitN(strings.Trim(f, `"`), `"`, 2)[0]; err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("a host officer with %s: %v; want refused naming %s", f, err, field)
		}
	}
}
