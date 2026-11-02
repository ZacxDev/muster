package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/push"
)

// task_dispatch's owner_asked (chief_owner_release.go): a task the CHIEF filed
// gated starts only while a turn its OWNER typed is running on the chief — the
// owner-turn marker, which only the owner's chat doors set and only once the
// runtime accepted the text (TestTheOwnersChatDoorsMarkOnlyAnAcceptedOwnerTurn,
// the ledger TestOnlyTheOwnersChatDoorsMarkAnOwnerTurn). Here the marker is set
// the way markOwnerTurn sets it, to put the chief in each state.

const ownersTypedWords = "Dispatch an agent to do the work, then when it is shipped and validated, reply to them."

// withPush gives the fixture's owner one device on a recording pusher.
func withPush(pusher *payloadPusher, store *memPush) func(ext *Extensions) {
	return func(ext *Extensions) {
		ext.Push = push.NewService(store, pusher, push.VAPIDConfig{PublicKey: "k"}, log.New(io.Discard, "", 0))
	}
}

// fileGated files a task through task_create_gated as the chief: the one way a
// gate the chief may later release is set (and recorded).
func (f *taskRouteFixture) fileGated(t *testing.T, title string) notes.Note {
	t.Helper()
	text := f.call(f.chief, gatedTaskToolName, map[string]any{"title": title, "body": "Infra work an outside sender asked for.",
		"gate_reason": "from agent mail"})
	var out struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil || out.ID <= 0 {
		t.Fatalf("task_create_gated: %s", text)
	}
	n, err := f.notes.Get(f.asOwner, out.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !notes.IsGated(n.Tags) {
		t.Fatalf("control: the filed task is not gated: %v", n.Tags)
	}
	return n
}

// ownerTurn stores text as a user row in agent a's chat and marks it as the
// turn in flight, typed by `by` — what markOwnerTurn does once the runtime
// accepted an owner's text.
func (f *taskRouteFixture) ownerTurn(t *testing.T, a agents.Agent, by int64, text string) agents.ChatMessage {
	t.Helper()
	st := agents.NewPG(f.pool)
	sess, err := st.LatestOrCreateSession(f.asOwner, a.ID, a.Name)
	if err != nil {
		t.Fatal(err)
	}
	m, err := st.AddChatMessage(f.asOwner, agents.ChatMessage{AgentID: a.ID, SessionID: sess.ID, Role: "user", Content: text})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetOwnerTurn(f.asOwner, a.ID, by, m.ID, time.Minute); err != nil {
		t.Fatal(err)
	}
	return m
}

// say stores a user row in agent a's chat and marks nothing: an injected turn,
// or an owner's line the runtime refused.
func (f *taskRouteFixture) say(t *testing.T, a agents.Agent, text string) agents.ChatMessage {
	t.Helper()
	st := agents.NewPG(f.pool)
	sess, err := st.LatestOrCreateSession(f.asOwner, a.ID, a.Name)
	if err != nil {
		t.Fatal(err)
	}
	m, err := st.AddChatMessage(f.asOwner, agents.ChatMessage{AgentID: a.ID, SessionID: sess.ID, Role: "user", Content: text})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// releaseComments is the muster-written release comments on task id.
func (f *taskRouteFixture) releaseComments(t *testing.T, id int64) []notes.Comment {
	t.Helper()
	cs, err := f.notes.ListComments(f.asOwner, id)
	if err != nil {
		t.Fatal(err)
	}
	var out []notes.Comment
	for _, c := range cs {
		if c.Author == ownerReleaseAuthor {
			out = append(out, c)
		}
	}
	return out
}

// mustRefuseRelease is mustRefuse (the sentence; nothing started, reserved or
// provisioned) plus: the task's tags are unchanged and no release was recorded.
func (f *taskRouteFixture) mustRefuseRelease(t *testing.T, text, want string, before notes.Note) {
	t.Helper()
	f.mustRefuse(t, text, want, before.ID)
	n, err := f.notes.Get(f.asOwner, before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(n.Tags, ",") != strings.Join(before.Tags, ",") {
		t.Errorf("a refused owner_asked changed task %d's tags: %v → %v", before.ID, before.Tags, n.Tags)
	}
	if cs := f.releaseComments(t, before.ID); len(cs) != 0 {
		t.Errorf("a refused owner_asked recorded a release: %+v", cs)
	}
}

func ownerAsked(id int64) map[string]any { return map[string]any{"task_id": id, "owner_asked": true} }

// TestTaskDispatchReleasesTheChiefsGateDuringTheOwnersTurn: the incident. A
// plugin message asked for work, the chief filed it gated, and the owner TYPED
// in the chief's chat to dispatch it; during that turn the chief calls
// owner_asked. The agent starts, the gate goes, the task says why (the owner's
// message id and their words, verbatim), the owner gets a push quoting them,
// and the result and the reservation's audit row name the message.
func TestTaskDispatchReleasesTheChiefsGateDuringTheOwnersTurn(t *testing.T) {
	pusher, store := &payloadPusher{}, &memPush{}
	f := newTaskRouteFixture(t, true, true, withKinds("pool-acct-a"), withPush(pusher, store))
	claim(t, store, f.owner, ep("owner-release"))
	n := f.fileGated(t, "infra work from agent mail")
	f.say(t, f.chief, "Plugin Agent mail: message_id 41 from an outside sender.")
	said := f.ownerTurn(t, f.chief, f.owner, ownersTypedWords+"\nThanks.")

	text := f.call(f.chief, taskDispatchTool, ownerAsked(n.ID))
	got := f.startedOne(t, text, n.ID)
	var out struct {
		Released int64 `json:"gate_released_by_owner_message"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil || out.Released != said.ID {
		t.Fatalf("the result does not name owner message %d: %s", said.ID, text)
	}
	after, err := f.notes.Get(f.asOwner, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if notes.IsGated(after.Tags) {
		t.Errorf("the gate is still on: %v", after.Tags)
	}
	cs := f.releaseComments(t, n.ID)
	if len(cs) != 1 {
		t.Fatalf("release comments: %+v; want exactly one", cs)
	}
	for _, want := range []string{"(gate:from-agent-mail)", "started " + got.Name, "message #" + strconv.FormatInt(said.ID, 10),
		"\n\n> " + ownersTypedWords + "\n> Thanks."} {
		if !strings.Contains(cs[0].Body, want) {
			t.Errorf("the release comment lacks %q:\n%s", want, cs[0].Body)
		}
	}
	if d := f.reservationDetail(t, n.ID); d["gate_released_by_owner_message"] != float64(said.ID) {
		t.Errorf("the reservation's audit row does not name the owner's message: %v", d)
	}
	eventuallyAPI(t, "the release push", func() bool {
		for _, p := range pusher.to(ep("owner-release")) {
			raw, _ := json.Marshal(p)
			if strings.Contains(string(raw), "released the gate on task #"+strconv.FormatInt(n.ID, 10)+" on your message: Dispatch an agent") {
				return true
			}
		}
		return false
	})
}

// TestTaskDispatchRefusesWithoutAnOwnerTurnInFlight — finding 1. The owner's
// line is STORED in the chief's chat but the runtime refused it (ccd 409 busy:
// the CLI was still running a plugin turn ccd had given up on), so no marker;
// and a turn nobody marked at all (a plugin delivery, a turn Claude Code
// started by itself). Both refused; the gate stays.
func TestTaskDispatchRefusesWithoutAnOwnerTurnInFlight(t *testing.T) {
	f := newTaskRouteFixture(t, true, true, withKinds("pool-acct-a"))
	n := f.fileGated(t, "refused owner line")
	f.say(t, f.chief, "Plugin Agent mail: message_id 41 from an outside sender.")
	f.mustRefuseRelease(t, f.call(f.chief, taskDispatchTool, ownerAsked(n.ID)), chiefReleaseNoOwnerTurn, n)
	f.say(t, f.chief, ownersTypedWords) // the owner's line, refused by the runtime: stored, never marked
	f.mustRefuseRelease(t, f.call(f.chief, taskDispatchTool, ownerAsked(n.ID)), chiefReleaseNoOwnerTurn, n)
}

// TestTaskDispatchRefusesAnotherUsersOrAnotherAgentsTurn: a marker typed by a
// user who is not the chief's owner; and the owner's turn in flight on their
// WORKER, not the chief.
func TestTaskDispatchRefusesAnotherUsersOrAnotherAgentsTurn(t *testing.T) {
	f := newTaskRouteFixture(t, true, true, withKinds("pool-acct-a"))
	n := f.fileGated(t, "someone else's turn")
	f.ownerTurn(t, f.worker, f.owner, ownersTypedWords)
	f.mustRefuseRelease(t, f.call(f.chief, taskDispatchTool, ownerAsked(n.ID)), chiefReleaseNoOwnerTurn, n)
	f.ownerTurn(t, f.chief, f.newMember(t), ownersTypedWords)
	f.mustRefuseRelease(t, f.call(f.chief, taskDispatchTool, ownerAsked(n.ID)), chiefReleaseNotOwnersTurn, n)
}

// TestTaskDispatchRefusesOnceTheOwnersTurnIsOver: the marker cleared when the
// turn returned, and one left by a crash that has lapsed — both refused. The
// same marker live releases (control).
func TestTaskDispatchRefusesOnceTheOwnersTurnIsOver(t *testing.T) {
	f := newTaskRouteFixture(t, true, true, withKinds("pool-acct-a"))
	st := agents.NewPG(f.pool)
	n := f.fileGated(t, "turn over")
	said := f.ownerTurn(t, f.chief, f.owner, ownersTypedWords)
	if err := st.ClearOwnerTurn(f.asOwner, f.chief.ID, said.ID); err != nil {
		t.Fatal(err)
	}
	f.mustRefuseRelease(t, f.call(f.chief, taskDispatchTool, ownerAsked(n.ID)), chiefReleaseNoOwnerTurn, n)

	lapsed := f.say(t, f.chief, ownersTypedWords)
	if err := st.SetOwnerTurn(f.asOwner, f.chief.ID, f.owner, lapsed.ID, -time.Second); err != nil {
		t.Fatal(err)
	}
	f.mustRefuseRelease(t, f.call(f.chief, taskDispatchTool, ownerAsked(n.ID)), chiefReleaseNoOwnerTurn, n)

	f.ownerTurn(t, f.chief, f.owner, ownersTypedWords)
	f.startedOne(t, f.call(f.chief, taskDispatchTool, ownerAsked(n.ID)), n.ID)
}

// TestTaskDispatchRefusesWhileAnOutsideTurnIsInFlight: the owner's turn is in
// flight, but muster is ALSO running a turn for the chief that the owner did
// not type (its session claim is held: a plugin delivery in another lane).
// Released, the same call starts.
func TestTaskDispatchRefusesWhileAnOutsideTurnIsInFlight(t *testing.T) {
	f := newTaskRouteFixture(t, true, true, withKinds("pool-acct-a"))
	n := f.fileGated(t, "delivery in flight")
	f.ownerTurn(t, f.chief, f.owner, ownersTypedWords)
	st := agents.NewPG(f.pool)
	const holder = "plugin-delivery/test"
	if won, err := st.ClaimKickoff(f.asOwner, f.chief.ID, holder, time.Minute); err != nil || !won {
		t.Fatalf("control: could not take the chief's claim: %v %v", won, err)
	}
	f.mustRefuseRelease(t, f.call(f.chief, taskDispatchTool, ownerAsked(n.ID)), chiefReleaseOutsideTurn, n)
	if err := st.ReleaseKickoffClaim(f.asOwner, f.chief.ID, holder); err != nil {
		t.Fatal(err)
	}
	f.startedOne(t, f.call(f.chief, taskDispatchTool, ownerAsked(n.ID)), n.ID)
}

// TestTaskDispatchNeverReleasesAGateTheOwnerSet — finding 2. A hold the owner
// put on a task themselves (gate:waiting-on-legal), and a chief-filed task the
// owner ALSO put a hold on, are refused with the gate untouched — during the
// owner's own turn.
func TestTaskDispatchNeverReleasesAGateTheOwnerSet(t *testing.T) {
	f := newTaskRouteFixture(t, true, true, withKinds("pool-acct-a"))
	f.ownerTurn(t, f.chief, f.owner, ownersTypedWords)
	own := f.task(t, "the owner's hold", "gate:waiting-on-legal")
	f.mustRefuseRelease(t, f.call(f.chief, taskDispatchTool, ownerAsked(own.ID)), chiefReleaseNotChiefsGate, own)

	both := f.fileGated(t, "filed, then held")
	held, err := f.notes.AddTags(f.asOwner, both.ID, []string{"gate:waiting-on-legal"})
	if err != nil {
		t.Fatal(err)
	}
	f.mustRefuseRelease(t, f.call(f.chief, taskDispatchTool, ownerAsked(both.ID)), chiefReleaseNotChiefsGate, held)
}

// TestTheFiledGateRecordIsTheOwnersOwn: task_create_gated records the exact
// gate it set, readable by the task's owner; another user reading the same task
// id finds no record (the owner is named in the query, not left to row-level
// security).
func TestTheFiledGateRecordIsTheOwnersOwn(t *testing.T) {
	f := newTaskRouteFixture(t, true, true)
	n := f.fileGated(t, "recorded")
	if gate, ok, err := f.plugins.GatedTaskFiled(f.asOwner, n.ID); err != nil || !ok || gate != "gate:from-agent-mail" {
		t.Fatalf("the owner reads %q %v %v; want gate:from-agent-mail", gate, ok, err)
	}
	if gate, ok, err := f.plugins.GatedTaskFiled(authz.WithPrincipal(f.ctx, authz.Principal{UserID: f.newMember(t), Role: authz.RoleAdmin, Via: authz.ViaSession}), n.ID); err != nil || ok {
		t.Fatalf("another user reads %q %v %v; want no record", gate, ok, err)
	}
}

// TestTaskDispatchARefusedStartLeavesTheGateOn — finding 3. The owner's turn
// is in flight and the gate is the chief's, but the live cap refuses the
// start: the task ends gated, with no release comment.
func TestTaskDispatchARefusedStartLeavesTheGateOn(t *testing.T) {
	f := newTaskRouteFixture(t, true, true, withKinds("pool-acct-a"))
	for i := 0; i < plugins.MaxChiefLiveAgents; i++ {
		f.liveChiefAgent(t, "capped-"+strconv.Itoa(i))
	}
	n := f.fileGated(t, "capped")
	f.ownerTurn(t, f.chief, f.owner, ownersTypedWords)
	f.mustRefuseRelease(t, f.call(f.chief, taskDispatchTool, ownerAsked(n.ID)), chiefDispatchLiveCap, n)
}

// betweenDispatch is the chief-dispatch ledger with a hook run just before the
// reservation: the moment between task_dispatch's owner_asked check and its
// re-check under the lock.
type betweenDispatch struct {
	plugins.DispatchStore
	before func()
}

func (b betweenDispatch) ReserveChiefDispatch(ctx context.Context, taskID int64, detail map[string]any, check func() error) (plugins.ChiefDispatch, error) {
	if b.before != nil {
		b.before()
	}
	return b.DispatchStore.ReserveChiefDispatch(ctx, taskID, detail, check)
}

// TestTaskDispatchRechecksTheReleaseUnderTheLock: what owner_asked verified
// can change before the reservation's lock — the owner's turn ends, or the
// owner puts a hold on the task. Each is caught by the re-check under the lock
// with its own refusal: nothing starts, nothing is recorded, and the task keeps
// its gates.
func TestTaskDispatchRechecksTheReleaseUnderTheLock(t *testing.T) {
	var hook func()
	f := newTaskRouteFixture(t, true, true, withKinds("pool-acct-a"))
	f.srv.ext.ChiefDispatch = betweenDispatch{DispatchStore: f.plugins, before: func() {
		if hook != nil {
			hook()
		}
	}}
	st := agents.NewPG(f.pool)

	ended := f.fileGated(t, "turn ends in between")
	said := f.ownerTurn(t, f.chief, f.owner, ownersTypedWords)
	hook = func() { _ = st.ClearOwnerTurn(f.asOwner, f.chief.ID, said.ID) }
	f.mustRefuseRelease(t, f.call(f.chief, taskDispatchTool, ownerAsked(ended.ID)), chiefReleaseNoOwnerTurn, ended)

	// A NEW owner turn replaced the one verified: the release would quote a
	// message that is no longer the one running.
	moved := f.fileGated(t, "a new turn in between")
	f.ownerTurn(t, f.chief, f.owner, ownersTypedWords)
	hook = func() { f.ownerTurn(t, f.chief, f.owner, "something else entirely") }
	f.mustRefuseRelease(t, f.call(f.chief, taskDispatchTool, ownerAsked(moved.ID)), chiefReleaseChanged, moved)

	held := f.fileGated(t, "held in between")
	f.ownerTurn(t, f.chief, f.owner, ownersTypedWords)
	hook = func() { _, _ = f.notes.AddTags(f.asOwner, held.ID, []string{"gate:waiting-on-legal"}) }
	text := f.call(f.chief, taskDispatchTool, ownerAsked(held.ID))
	hook = nil
	after, err := f.notes.Get(f.asOwner, held.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.mustRefuseRelease(t, text, chiefReleaseChanged, after)
	if len(gateTags(after.Tags)) != 2 {
		t.Errorf("the task lost a gate: %v", after.Tags)
	}
}

// TestTaskDispatchStillRefusesAGatedTaskWithoutOwnerAsked: owner_asked is the
// only way in. The owner's turn alone does nothing: without the flag the gated
// refusal is unchanged (and now names the flag).
func TestTaskDispatchStillRefusesAGatedTaskWithoutOwnerAsked(t *testing.T) {
	f := newTaskRouteFixture(t, true, true, withKinds("pool-acct-a"))
	n := f.fileGated(t, "no flag")
	f.ownerTurn(t, f.chief, f.owner, ownersTypedWords)
	f.mustRefuseRelease(t, f.call(f.chief, taskDispatchTool, map[string]any{"task_id": n.ID}), chiefDispatchGatedText, n)
	if !strings.Contains(chiefDispatchGatedText, "owner_asked: true") {
		t.Errorf("the gated refusal does not tell the chief about owner_asked: %q", chiefDispatchGatedText)
	}
}

// TestTaskDispatchOwnerAskedOnAnUngatedTaskChangesNothing: on an ungated task
// owner_asked is ignored — it starts with no owner turn at all, and records no
// release.
func TestTaskDispatchOwnerAskedOnAnUngatedTaskChangesNothing(t *testing.T) {
	f := newTaskRouteFixture(t, true, true, withKinds("pool-acct-a"))
	n := f.task(t, "ungated")
	text := f.call(f.chief, taskDispatchTool, ownerAsked(n.ID))
	f.startedOne(t, text, n.ID)
	if strings.Contains(text, "gate_released_by_owner_message") {
		t.Errorf("an ungated start claims a gate release: %s", text)
	}
	if cs := f.releaseComments(t, n.ID); len(cs) != 0 {
		t.Errorf("an ungated start recorded a release: %+v", cs)
	}
}
