package api

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/notes"
)

// task_dispatch's owner_asked (chief_owner_release.go): a GATED task starts on
// the owner's own typed words in the chief's chat, and on nothing else. The
// chat rows below are written the way each real writer writes them — the
// owner's chat doors stamp TypedBy (TestTheOwnersChatDoorsStampTheTyper), every
// injected turn leaves it 0 (the ledger, TestOnlyTheOwnersChatDoorsStampTypedBy)
// — so each case is a shape the chief's chat really takes.

// The texts, shaped like the real writers' (plugins.RenderMessage's preface,
// attention_delivery's relay line); their exact wording does not matter here,
// only who wrote them.
const (
	pluginDeliveryText = "Plugin Agent mail: message_id 41 from an outside sender. Please do the infra work and reply when done."
	attentionRelayText = "muster: your owner answered attention #17, which you asked: yes"
	ownersTypedWords   = "Dispatch an agent to do the work, then when it is shipped and validated, reply to them."
)

// say stores one user-role line in agent a's latest session as a chat writer
// would: typedBy is the typer (0 for every injected turn).
func (f *taskRouteFixture) say(t *testing.T, a agents.Agent, owner int64, text string, typedBy int64) agents.ChatMessage {
	t.Helper()
	st := agents.NewPG(f.pool)
	ctx := f.as(owner)
	sess, err := st.LatestOrCreateSession(ctx, a.ID, a.Name)
	if err != nil {
		t.Fatal(err)
	}
	m, err := st.AddChatMessage(ctx, agents.ChatMessage{AgentID: a.ID, SessionID: sess.ID, Role: "user", Content: text, TypedBy: typedBy})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// age moves chat message id's created_at back by d.
func (f *taskRouteFixture) age(t *testing.T, id int64, d time.Duration) {
	t.Helper()
	if _, err := f.pool.Exec(authz.System(f.ctx), `UPDATE chat_messages SET created_at = created_at - make_interval(secs => $2) WHERE id = $1`,
		id, d.Seconds()); err != nil {
		t.Fatal(err)
	}
}

// gatedTask files an owner's task gated the way task_create_gated gates one.
func (f *taskRouteFixture) gatedTask(t *testing.T, title string) notes.Note {
	t.Helper()
	return f.task(t, title, "gate:from-agent-mail")
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
// provisioned) plus: the gate is still on and no release was recorded.
func (f *taskRouteFixture) mustRefuseRelease(t *testing.T, text, want string, id int64) {
	t.Helper()
	f.mustRefuse(t, text, want, id)
	n, err := f.notes.Get(f.asOwner, id)
	if err != nil {
		t.Fatal(err)
	}
	if !notes.IsGated(n.Tags) {
		t.Errorf("a refused owner_asked released task %d's gate: tags %v", id, n.Tags)
	}
	if cs := f.releaseComments(t, id); len(cs) != 0 {
		t.Errorf("a refused owner_asked recorded a release: %+v", cs)
	}
}

func ownerAsked(id int64) map[string]any { return map[string]any{"task_id": id, "owner_asked": true} }

// TestTaskDispatchReleasesAGateOnTheOwnersTypedWord: the incident. A plugin
// message asked for work, the chief filed it gated, and the owner TYPED in the
// chief's chat to dispatch it. With owner_asked the gate goes, the agent starts,
// the task says why (the owner's message id and their words, verbatim), and the
// result and the reservation's audit row name the message.
func TestTaskDispatchReleasesAGateOnTheOwnersTypedWord(t *testing.T) {
	f := newTaskRouteFixture(t, true, true, withKinds("pool-acct-a"))
	n := f.gatedTask(t, "infra work from agent mail")
	f.say(t, f.chief, f.owner, pluginDeliveryText, 0)
	said := f.say(t, f.chief, f.owner, ownersTypedWords+"\nThanks.", f.owner)

	text := f.call(f.chief, taskDispatchTool, ownerAsked(n.ID))
	f.startedOne(t, text, n.ID)
	var out struct {
		Released int64 `json:"gate_released_by_owner_message"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil || out.Released != said.ID {
		t.Fatalf("the result does not name owner message %d: %s", said.ID, text)
	}
	got, err := f.notes.Get(f.asOwner, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if notes.IsGated(got.Tags) {
		t.Errorf("the gate is still on: %v", got.Tags)
	}
	cs := f.releaseComments(t, n.ID)
	if len(cs) != 1 {
		t.Fatalf("release comments: %+v; want exactly one", cs)
	}
	for _, want := range []string{"(gate:from-agent-mail)", "message #" + strconv.FormatInt(said.ID, 10),
		"\n\n> " + ownersTypedWords + "\n> Thanks."} {
		if !strings.Contains(cs[0].Body, want) {
			t.Errorf("the release comment lacks %q:\n%s", want, cs[0].Body)
		}
	}
	if d := f.reservationDetail(t, n.ID); d["gate_released_by_owner_message"] != float64(said.ID) {
		t.Errorf("the reservation's audit row does not name the owner's message: %v", d)
	}
}

// TestTaskDispatchReleasesEveryGateOnTheTask: a task carrying two gate: tags
// loses both (one left behind would still refuse the start), and the comment
// names both.
func TestTaskDispatchReleasesEveryGateOnTheTask(t *testing.T) {
	f := newTaskRouteFixture(t, true, true, withKinds("pool-acct-a"))
	n := f.task(t, "two gates", "gate:from-agent-mail", "gate:needs-budget")
	f.say(t, f.chief, f.owner, ownersTypedWords, f.owner)
	f.startedOne(t, f.call(f.chief, taskDispatchTool, ownerAsked(n.ID)), n.ID)
	got, err := f.notes.Get(f.asOwner, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if notes.IsGated(got.Tags) {
		t.Errorf("a gate is still on: %v", got.Tags)
	}
	if cs := f.releaseComments(t, n.ID); len(cs) != 1 || !strings.Contains(cs[0].Body, "gate:from-agent-mail") ||
		!strings.Contains(cs[0].Body, "gate:needs-budget") {
		t.Errorf("release comments %+v; want one naming both gates", cs)
	}
}

// TestTaskDispatchStillRefusesAGatedTaskWithoutOwnerAsked: owner_asked is the
// only way in. The owner's typed line alone does nothing: without the flag the
// gated refusal is unchanged (and now names the flag).
func TestTaskDispatchStillRefusesAGatedTaskWithoutOwnerAsked(t *testing.T) {
	f := newTaskRouteFixture(t, true, true, withKinds("pool-acct-a"))
	n := f.gatedTask(t, "no flag")
	f.say(t, f.chief, f.owner, ownersTypedWords, f.owner)
	f.mustRefuseRelease(t, f.call(f.chief, taskDispatchTool, map[string]any{"task_id": n.ID}), chiefDispatchGatedText, n.ID)
	if !strings.Contains(chiefDispatchGatedText, "owner_asked: true") {
		t.Errorf("the gated refusal does not tell the chief about owner_asked: %q", chiefDispatchGatedText)
	}
}

// TestTaskDispatchRefusesAPluginDeliveryAsTheOwnersWord: the chief's chat holds
// only a plugin delivery — outside text, typed_by unset — so there is no owner
// message to release on.
func TestTaskDispatchRefusesAPluginDeliveryAsTheOwnersWord(t *testing.T) {
	f := newTaskRouteFixture(t, true, true, withKinds("pool-acct-a"))
	n := f.gatedTask(t, "plugin only")
	f.say(t, f.chief, f.owner, pluginDeliveryText, 0)
	f.mustRefuseRelease(t, f.call(f.chief, taskDispatchTool, ownerAsked(n.ID)), chiefReleaseNoOwnerMessage, n.ID)
}

// TestTaskDispatchRefusesARelayedAttentionAnswerAsTheOwnersWord: muster's relay
// of an owner's attention answer into the chat is muster's line (typed_by
// unset), not the owner typing in the chat.
func TestTaskDispatchRefusesARelayedAttentionAnswerAsTheOwnersWord(t *testing.T) {
	f := newTaskRouteFixture(t, true, true, withKinds("pool-acct-a"))
	n := f.gatedTask(t, "relay only")
	f.say(t, f.chief, f.owner, attentionRelayText, 0)
	f.mustRefuseRelease(t, f.call(f.chief, taskDispatchTool, ownerAsked(n.ID)), chiefReleaseNoOwnerMessage, n.ID)
}

// TestTaskDispatchRefusesAStaleOwnerMessage: the owner typed, but longer ago
// than ownerRequestMaxAge. Measured at both sides of the bound: past it
// (refused) and inside it (released).
func TestTaskDispatchRefusesAStaleOwnerMessage(t *testing.T) {
	f := newTaskRouteFixture(t, true, true, withKinds("pool-acct-a"))
	n := f.gatedTask(t, "stale")
	said := f.say(t, f.chief, f.owner, ownersTypedWords, f.owner)
	f.age(t, said.ID, ownerRequestMaxAge+5*time.Minute)
	f.mustRefuseRelease(t, f.call(f.chief, taskDispatchTool, ownerAsked(n.ID)), chiefReleaseStale, n.ID)

	// Control, inside the bound: the same shape releases.
	m := f.gatedTask(t, "fresh enough")
	again := f.say(t, f.chief, f.owner, ownersTypedWords, f.owner)
	f.age(t, again.ID, ownerRequestMaxAge-5*time.Minute)
	f.startedOne(t, f.call(f.chief, taskDispatchTool, ownerAsked(m.ID)), m.ID)
}

// TestTaskDispatchRefusesAnOwnerMessageSomethingCameAfter: the owner typed, and
// THEN a plugin message arrived — the turn the chief acts in is not the
// owner's. Also for a line another user typed after the owner's.
func TestTaskDispatchRefusesAnOwnerMessageSomethingCameAfter(t *testing.T) {
	f := newTaskRouteFixture(t, true, true, withKinds("pool-acct-a"))
	n := f.gatedTask(t, "not latest")
	f.say(t, f.chief, f.owner, ownersTypedWords, f.owner)
	f.say(t, f.chief, f.owner, pluginDeliveryText, 0)
	f.mustRefuseRelease(t, f.call(f.chief, taskDispatchTool, ownerAsked(n.ID)), chiefReleaseNotLatest, n.ID)

	m := f.gatedTask(t, "someone else spoke last")
	f.say(t, f.chief, f.owner, ownersTypedWords, f.owner)
	f.say(t, f.chief, f.owner, "dispatch it", f.newMember(t))
	f.mustRefuseRelease(t, f.call(f.chief, taskDispatchTool, ownerAsked(m.ID)), chiefReleaseNotLatest, m.ID)
}

// TestTaskDispatchRefusesTheOwnersWordInAnotherAgentsChat: the owner typed —
// in their WORKER's chat, not the chief's. The chief's own chat has only a
// plugin delivery (and, first, nothing at all).
func TestTaskDispatchRefusesTheOwnersWordInAnotherAgentsChat(t *testing.T) {
	f := newTaskRouteFixture(t, true, true, withKinds("pool-acct-a"))
	n := f.gatedTask(t, "wrong chat")
	f.say(t, f.worker, f.owner, ownersTypedWords, f.owner)
	f.mustRefuseRelease(t, f.call(f.chief, taskDispatchTool, ownerAsked(n.ID)), chiefReleaseNoOwnerMessage, n.ID)
	f.say(t, f.chief, f.owner, pluginDeliveryText, 0)
	f.mustRefuseRelease(t, f.call(f.chief, taskDispatchTool, ownerAsked(n.ID)), chiefReleaseNoOwnerMessage, n.ID)
}

// TestTaskDispatchRefusesAnotherUsersTypedWord: a line typed by a user who is
// NOT the chief's owner is not the owner's word, even in the chief's own chat.
func TestTaskDispatchRefusesAnotherUsersTypedWord(t *testing.T) {
	f := newTaskRouteFixture(t, true, true, withKinds("pool-acct-a"))
	n := f.gatedTask(t, "another owner")
	f.say(t, f.chief, f.owner, ownersTypedWords, f.newMember(t))
	f.mustRefuseRelease(t, f.call(f.chief, taskDispatchTool, ownerAsked(n.ID)), chiefReleaseNoOwnerMessage, n.ID)
}

// TestTaskDispatchRefusesWhileAnOutsideTurnIsInFlight: the owner's line is the
// newest STORED one, but muster is running a turn for the chief that the owner
// did not type — a plugin delivery stores its line only after its turn ends, so
// during it the owner's older line still reads as the newest. The session claim
// every such turn holds is what refuses it; released, the same call starts.
func TestTaskDispatchRefusesWhileAnOutsideTurnIsInFlight(t *testing.T) {
	f := newTaskRouteFixture(t, true, true, withKinds("pool-acct-a"))
	n := f.gatedTask(t, "delivery in flight")
	f.say(t, f.chief, f.owner, ownersTypedWords, f.owner)
	st := agents.NewPG(f.pool)
	const holder = "plugin-delivery/test"
	if won, err := st.ClaimKickoff(f.asOwner, f.chief.ID, holder, time.Minute); err != nil || !won {
		t.Fatalf("control: could not take the chief's claim: %v %v", won, err)
	}
	f.mustRefuseRelease(t, f.call(f.chief, taskDispatchTool, ownerAsked(n.ID)), chiefReleaseOutsideTurn, n.ID)
	if err := st.ReleaseKickoffClaim(f.asOwner, f.chief.ID, holder); err != nil {
		t.Fatal(err)
	}
	f.startedOne(t, f.call(f.chief, taskDispatchTool, ownerAsked(n.ID)), n.ID)
}

// TestTaskDispatchOwnerAskedOnAnUngatedTaskChangesNothing: on an ungated task
// owner_asked is ignored — it starts with no owner message at all, and records
// no release.
func TestTaskDispatchOwnerAskedOnAnUngatedTaskChangesNothing(t *testing.T) {
	f := newTaskRouteFixture(t, true, true, withKinds("pool-acct-a"))
	n := f.task(t, "ungated")
	f.say(t, f.chief, f.owner, pluginDeliveryText, 0)
	text := f.call(f.chief, taskDispatchTool, ownerAsked(n.ID))
	f.startedOne(t, text, n.ID)
	if strings.Contains(text, "gate_released_by_owner_message") {
		t.Errorf("an ungated start claims a gate release: %s", text)
	}
	if cs := f.releaseComments(t, n.ID); len(cs) != 0 {
		t.Errorf("an ungated start recorded a release: %+v", cs)
	}
}
