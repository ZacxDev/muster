package api

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
)

// The owner releases the chief's gate from the routed-attention ask itself
// (answer_release.go): "Release & dispatch on <target>" and "Release only".

// The literal labels, typed here rather than read from the implementation.
const (
	relGateway   = "Release & dispatch on pod/gateway, no repository"
	relCC        = "Release & dispatch on pod/claude-code, no repository"
	relOnly      = "Release only: release the gate, start nothing"
	dispGateway  = "agent_dispatch on pod/gateway: start a new agent on it"
	dispCC       = "agent_dispatch on pod/claude-code: start a new agent on it"
	relActionTag = "release_only"
)

// routeToOwner calls task_route_attention on task id as the chief: the ask.
func (f *taskRouteFixture) routeToOwner(t *testing.T, id int64) (attention.Ask, []string) {
	t.Helper()
	text := f.call(f.chief, taskRouteAttentionTool, map[string]any{"task_id": id, "note": "A user reports a crash on save."})
	var out struct {
		ID      int64    `json:"id"`
		Offered []string `json:"actions_offered"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil || out.ID <= 0 {
		t.Fatalf("task_route_attention: %s", text)
	}
	ask, err := f.srv.ext.Attention.Get(f.asOwner, out.ID)
	if err != nil {
		t.Fatal(err)
	}
	return ask, out.Offered
}

// releaseAudits is task id's gate-released-by-answer audit rows: (user_id,
// via, ask_id) each.
func (f *taskRouteFixture) releaseAudits(t *testing.T, id int64) [][3]string {
	t.Helper()
	rows, err := f.pool.Query(authz.System(context.Background()), `SELECT coalesce(user_id::text, ''), via, coalesce(detail->>'ask_id', '')
		FROM audit_log WHERE action = 'task.gate_released_by_answer' AND object = $1 ORDER BY id`, "note:"+strconv.FormatInt(id, 10))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out [][3]string
	for rows.Next() {
		var r [3]string
		if err := rows.Scan(&r[0], &r[1], &r[2]); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// as is ctx's owner through door via.
func (f *taskRouteFixture) asVia(via authz.Via) context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: f.owner, Role: authz.RoleMember, Via: via})
}

// mustStayGated: the refused answer recorded nothing — the ask is open, the
// task carries the same tags, and no release was audited or commented.
func (f *taskRouteFixture) mustStayGated(t *testing.T, ask attention.Ask, before notes.Note) {
	t.Helper()
	a, err := f.srv.ext.Attention.Get(f.asOwner, ask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != attention.Open {
		t.Errorf("a refused Release recorded the answer: ask %d is %s (%q)", ask.ID, a.Status, a.Answer)
	}
	n, err := f.notes.Get(f.asOwner, before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(n.Tags, ",") != strings.Join(before.Tags, ",") {
		t.Errorf("a refused Release changed task %d's tags: %v → %v", before.ID, before.Tags, n.Tags)
	}
	if rows := f.releaseAudits(t, before.ID); len(rows) != 0 {
		t.Errorf("a refused Release was audited: %v", rows)
	}
	if cs := f.releaseComments(t, before.ID); len(cs) != 0 {
		t.Errorf("a refused Release left a comment: %+v", cs)
	}
}

// TestTheOwnersReleaseAndDispatchTapReleasesTheGateAndBindsTheStart is the
// feature, through each of the owner's two doors (the signed-in browser and the
// paired phone): the chief files feedback gated and routes it; the ask offers
// Release & dispatch per target and Release only; before the tap the chief
// cannot start anything; the owner's tap releases the gate, audits it (the
// owner, the door, the ask) and says so on the task; the chief's agent_dispatch
// is then held to exactly the tapped target, and that one starts.
func TestTheOwnersReleaseAndDispatchTapReleasesTheGateAndBindsTheStart(t *testing.T) {
	for _, via := range []authz.Via{authz.ViaSession, authz.ViaDevice} {
		t.Run(string(via), func(t *testing.T) {
			f := newReleaseFixture(t)
			n := f.fileGated(t, "feedback: crash on save")
			ask, offered := f.routeToOwner(t, n.ID)
			if want := []string{relGateway, relOnly, relCC}; strings.Join(ask.Options, "|") != strings.Join(want, "|") {
				t.Fatalf("the ask's options %q\nwant %q", ask.Options, want)
			}
			if strings.Join(offered, ",") != agentDispatchTool+","+relActionTag+","+agentDispatchTool {
				t.Errorf("actions_offered %q", offered)
			}
			if !strings.Contains(ask.Question, "only you can release it: Release & dispatch releases the gate") {
				t.Errorf("the ask does not say the buttons release the gate: %q", ask.Question)
			}
			// Before the tap: still gated, nothing starts.
			f.mustRefuse(t, f.callRaw(f.chief, agentDispatchTool, map[string]any{"target": "pod/gateway", "task_id": n.ID}), "is gated", n.ID)

			a, err := f.srv.answerAskFrom(f.asVia(via), ask.ID, relGateway, attention.AnswerSource{})
			if err != nil {
				t.Fatalf("the owner's Release & dispatch tap: %v", err)
			}
			if a.Status != attention.Answered {
				t.Fatalf("ask %d is %s", a.ID, a.Status)
			}
			got, err := f.notes.Get(f.asOwner, n.ID)
			if err != nil {
				t.Fatal(err)
			}
			if notes.IsGated(got.Tags) {
				t.Fatalf("the tap did not release the gate: %v", got.Tags)
			}
			rows := f.releaseAudits(t, n.ID)
			if want := [][3]string{{strconv.FormatInt(f.owner, 10), string(via), strconv.FormatInt(ask.ID, 10)}}; len(rows) != 1 || rows[0] != want[0] {
				t.Errorf("audit rows %v; want %v (the owner, the door, the ask)", rows, want)
			}
			cs := f.releaseComments(t, n.ID)
			if len(cs) != 1 || !strings.Contains(cs[0].Body, "on your answer to attention #"+strconv.FormatInt(ask.ID, 10)) {
				t.Errorf("release comments %+v", cs)
			}
			// The chief is held to the tapped target, and that one starts.
			f.mustRefuse(t, f.callRaw(f.chief, agentDispatchTool, map[string]any{"target": "pod/claude-code", "task_id": n.ID}),
				"your owner chose pod/gateway for this task", n.ID)
			ag := f.startedOne(t, f.callRaw(f.chief, agentDispatchTool, map[string]any{"target": "pod/gateway", "task_id": n.ID}), n.ID)
			if ag.Kind != agents.KindGateway {
				t.Errorf("agent kind %q, want the gateway the owner tapped", ag.Kind)
			}
		})
	}
}

// TestReleaseOnlyReleasesTheGateAndStartsNothing: the other button. The gate
// goes (audited), and agent_dispatch without owner_asked is refused on the
// task, naming the owner's choice.
func TestReleaseOnlyReleasesTheGateAndStartsNothing(t *testing.T) {
	f := newReleaseFixture(t)
	n := f.fileGated(t, "feedback: a feature request")
	ask, _ := f.routeToOwner(t, n.ID)
	if _, err := f.srv.answerAskFrom(f.asOwner, ask.ID, relOnly, attention.AnswerSource{}); err != nil {
		t.Fatal(err)
	}
	got, _ := f.notes.Get(f.asOwner, n.ID)
	if notes.IsGated(got.Tags) {
		t.Fatalf("Release only did not release the gate: %v", got.Tags)
	}
	if rows := f.releaseAudits(t, n.ID); len(rows) != 1 {
		t.Errorf("audit rows %v; want one", rows)
	}
	f.mustRefuse(t, f.callRaw(f.chief, agentDispatchTool, map[string]any{"target": "pod/gateway", "task_id": n.ID}),
		"chose to start nothing", n.ID)
}

// TestOnlyTheOwnersOwnTapReleases: every other door is refused BEFORE the
// answer is recorded — the owner's mirrored session through attention_answer,
// the chief calling attention_answer by name, and the owner's own user behind
// an agent's, a plugin's, a personal API token's, the hook's or a mirror host's
// credential. Each leaves the ask open and the task gated; the control is the
// owner's session tap, which then releases.
func TestOnlyTheOwnersOwnTapReleases(t *testing.T) {
	f := newReleaseFixture(t)
	n := f.fileGated(t, "feedback: confusing docs")
	ask, _ := f.routeToOwner(t, n.ID)

	mirrored := toolCaller{asker: attention.Asker{MirrorHost: "ws", MirrorSession: "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d", Label: "ws"}}
	text := f.srv.toolAttentionAnswer(f.asVia(authz.ViaMirrorHost), mirrored, `{"id": `+strconv.FormatInt(ask.ID, 10)+`, "answer": "`+relGateway+`"}`)
	if !strings.Contains(text, `"error"`) || !strings.Contains(text, releaseNotOwnersDoor) {
		t.Errorf("attention_answer from a mirrored session: %s", text)
	}
	f.mustStayGated(t, ask, n)

	if text := f.call(f.chief, attentionAnswerTool, map[string]any{"id": ask.ID, "answer": relGateway}); !strings.Contains(text, `"error"`) {
		t.Errorf("the chief answered its own ask: %s", text)
	}
	f.mustStayGated(t, ask, n)

	// An answer whose SOURCE is attention_answer never releases, whatever the
	// principal says (defence in depth: today only a mirror-host token reaches
	// attention_answer, which attention.IsOwnerDoor refuses as well).
	mcp := attention.AnswerSource{Via: attention.ViaMCP, Host: "laptop", Session: "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"}
	if _, err := f.srv.answerAskFrom(f.asOwner, ask.ID, relGateway, mcp); err == nil || !strings.Contains(err.Error(), releaseNotOwnersDoor) {
		t.Errorf("an MCP-sourced answer under a session principal: %v; want the owner's-door refusal", err)
	}
	f.mustStayGated(t, ask, n)

	for _, via := range []authz.Via{authz.ViaAgent, authz.ViaConnect, authz.ViaPlugin, authz.ViaToken, authz.ViaHook, authz.ViaMirrorHost, authz.ViaAttention} {
		_, err := f.srv.answerAskFrom(f.asVia(via), ask.ID, relGateway, attention.AnswerSource{})
		if err == nil || !strings.Contains(err.Error(), releaseNotOwnersDoor) {
			t.Errorf("via %s: %v; want the owner's-door refusal", via, err)
		}
		f.mustStayGated(t, ask, n)
	}
	// Control: the owner's own tap releases.
	if _, err := f.srv.answerAskFrom(f.asOwner, ask.ID, relGateway, attention.AnswerSource{}); err != nil {
		t.Fatalf("control: the owner's tap: %v", err)
	}
	if got, _ := f.notes.Get(f.asOwner, n.ID); notes.IsGated(got.Tags) {
		t.Fatalf("control: the owner's tap did not release: %v", got.Tags)
	}
}

// TestAStaleReleaseFailsSafely: the task changed after the ask was filed — its
// gate already released on the board, a gate of the owner's added, the task
// completed, or dismissed. Each tap is refused with its own sentence, records
// no answer and releases nothing; the ask stays open (Dismiss still works).
func TestAStaleReleaseFailsSafely(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(t *testing.T, f *releaseFixture, n notes.Note)
		want   string
	}{
		{"released-on-the-board", func(t *testing.T, f *releaseFixture, n notes.Note) {
			if _, err := f.notes.RemoveTags(f.asOwner, n.ID, gateTags(n.Tags)); err != nil {
				t.Fatal(err)
			}
		}, releaseAlreadyFree},
		{"owners-own-gate-added", func(t *testing.T, f *releaseFixture, n notes.Note) {
			if _, err := f.notes.AddTags(f.asOwner, n.ID, []string{"gate:waiting-on-legal"}); err != nil {
				t.Fatal(err)
			}
		}, releaseNotChiefGate},
		{"completed", func(t *testing.T, f *releaseFixture, n notes.Note) {
			if _, err := f.notes.SetStatus(f.asOwner, n.ID, notes.StatusComplete); err != nil {
				t.Fatal(err)
			}
		}, "this task is complete now"},
		{"dismissed", func(t *testing.T, f *releaseFixture, n notes.Note) {
			if err := f.notes.SoftDelete(f.asOwner, n.ID); err != nil {
				t.Fatal(err)
			}
		}, releaseTaskGone},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newReleaseFixture(t)
			n := f.fileGated(t, "feedback: "+c.name)
			ask, _ := f.routeToOwner(t, n.ID)
			c.change(t, f, n)
			before, err := f.notes.Get(f.asOwner, n.ID)
			if err != nil {
				before = n // dismissed: not readable; nothing below reads its tags
			}
			_, err = f.srv.answerAskFrom(f.asOwner, ask.ID, relGateway, attention.AnswerSource{})
			if err == nil || !strings.Contains(err.Error(), c.want) || !isReleaseRefusal(err) {
				t.Fatalf("a stale Release answered %v; want a refusal carrying %q", err, c.want)
			}
			if got := attentionErrText(err, ask); !strings.Contains(got, c.want) {
				t.Errorf("the owner reads %q; want it to carry %q", got, c.want)
			}
			if c.name != "dismissed" {
				f.mustStayGated(t, ask, before)
			} else if a, _ := f.srv.ext.Attention.Get(f.asOwner, ask.ID); a.Status != attention.Open {
				t.Errorf("ask is %s", a.Status)
			}
			if _, err := f.srv.cancelAsk(f.asOwner, ask.ID); err != nil {
				t.Errorf("Dismiss after a refused Release: %v", err)
			}
		})
	}
}

// TestAnUngatedOrOwnerGatedTaskOffersNoRelease: the regression. An ungated
// task's ask offers exactly the per-target agent_dispatch options, as before;
// a gate the OWNER set offers nothing and says it is theirs to handle; and an
// ordinary answer writes no release audit.
func TestAnUngatedOrOwnerGatedTaskOffersNoRelease(t *testing.T) {
	f := newReleaseFixture(t)
	open := f.task(t, "ungated")
	ask, offered := f.routeToOwner(t, open.ID)
	if want := []string{dispGateway, dispCC}; strings.Join(ask.Options, "|") != strings.Join(want, "|") {
		t.Errorf("an ungated task's options %q, want %q", ask.Options, want)
	}
	if strings.Join(offered, ",") != agentDispatchTool {
		t.Errorf("actions_offered %q", offered)
	}
	if _, err := f.srv.answerAskFrom(f.asOwner, ask.ID, dispGateway, attention.AnswerSource{}); err != nil {
		t.Fatal(err)
	}
	if rows := f.releaseAudits(t, open.ID); len(rows) != 0 {
		t.Errorf("an ordinary answer was audited as a release: %v", rows)
	}
	// A Release label typed as free text on an ask whose OPTIONS do not carry
	// it (they are agent_dispatch's) is an ordinary answer too.
	other := f.task(t, "ungated, free text")
	ask, _ = f.routeToOwner(t, other.ID)
	if len(ask.Options) == 0 {
		t.Fatal("control: the ungated ask offers no options, so it cannot show the options are checked")
	}
	if _, err := f.srv.answerAskFrom(f.asOwner, ask.ID, relGateway, attention.AnswerSource{}); err != nil {
		t.Fatalf("a free-text answer spelling a Release label on an ungated task's ask: %v", err)
	}
	if rows := f.releaseAudits(t, other.ID); len(rows) != 0 {
		t.Errorf("audited: %v", rows)
	}

	held := f.task(t, "owner's hold", "gate:waiting-on-legal")
	ask, offered = f.routeToOwner(t, held.ID)
	if len(ask.Options) != 0 || len(offered) != 0 || !strings.Contains(ask.Question, "it is yours to handle") {
		t.Errorf("an owner's own gate: options %q offered %q question %q", ask.Options, offered, ask.Question)
	}
	// A Release label typed as FREE TEXT on an ask that did not offer it is an
	// ordinary answer: recorded, nothing released, no audit.
	if _, err := f.srv.answerAskFrom(f.asOwner, ask.ID, relOnly, attention.AnswerSource{}); err != nil {
		t.Fatalf("a free-text answer that spells a Release label: %v", err)
	}
	if got, _ := f.notes.Get(f.asOwner, held.ID); strings.Join(got.Tags, ",") != "gate:waiting-on-legal" {
		t.Errorf("the owner's hold changed: %v", got.Tags)
	}
	if rows := f.releaseAudits(t, held.ID); len(rows) != 0 {
		t.Errorf("audited: %v", rows)
	}
}

// TestAReleaseLabelOnAnAskMusterDidNotLinkReleasesNothing: the release keys on
// STRUCTURE — an ask muster linked to the task, offering the option — never on
// the words. The chief's own attention_ask carrying a Release label as an
// option, answered by the owner, releases nothing.
func TestAReleaseLabelOnAnAskMusterDidNotLinkReleasesNothing(t *testing.T) {
	f := newReleaseFixture(t)
	n := f.fileGated(t, "feedback: spoofed buttons")
	text := f.call(f.chief, attentionAskTool, map[string]any{"question": "Task #" + strconv.FormatInt(n.ID, 10) + " needs you", "options": []string{relGateway}})
	var out struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil || out.ID <= 0 {
		t.Fatalf("attention_ask: %s", text)
	}
	if _, err := f.srv.answerAskFrom(f.asOwner, out.ID, relGateway, attention.AnswerSource{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.notes.Get(f.asOwner, n.ID); !notes.IsGated(got.Tags) {
		t.Fatalf("an unlinked ask's answer released the gate: %v", got.Tags)
	}
	if rows := f.releaseAudits(t, n.ID); len(rows) != 0 {
		t.Errorf("audited: %v", rows)
	}
}

// fileGatedIn is fileGated with the task's repository set by the chief —
// written while it read the outside sender's text.
func (f *taskRouteFixture) fileGatedIn(t *testing.T, title, repo string) notes.Note {
	t.Helper()
	text := f.call(f.chief, gatedTaskToolName, map[string]any{"title": title, "body": "Work an outside sender asked for.",
		"gate_reason": "from agent mail", "repo": repo})
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
	return n
}

// TestAnMCPAnswerDoesNotUndoTheOwnersReleaseOnly is audit round 1's flip: the
// owner taps Release only; the chief routes the now-ungated task again; a
// mirrored session answers the NEW ask through attention_answer with a
// dispatch option. The owner's Release only still binds: no start. The
// control is the owner answering that new ask on their own door, which then
// binds the target they chose.
func TestAnMCPAnswerDoesNotUndoTheOwnersReleaseOnly(t *testing.T) {
	f := newReleaseFixture(t)
	n := f.fileGated(t, "feedback: release only, then MCP")
	ask, _ := f.routeToOwner(t, n.ID)
	if _, err := f.srv.answerAskFrom(f.asOwner, ask.ID, relOnly, attention.AnswerSource{}); err != nil {
		t.Fatal(err)
	}
	again, _ := f.routeToOwner(t, n.ID)
	if strings.Join(again.Options, "|") != dispGateway+"|"+dispCC {
		t.Fatalf("control: the released task's new ask offers %q", again.Options)
	}
	mirrored := toolCaller{asker: attention.Asker{MirrorHost: "ws", MirrorSession: "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d", Label: "ws"}}
	text := f.srv.toolAttentionAnswer(f.asVia(authz.ViaMirrorHost), mirrored, `{"id": `+strconv.FormatInt(again.ID, 10)+`, "answer": "`+dispGateway+`"}`)
	if strings.Contains(text, `"error"`) {
		t.Fatalf("control: the mirrored session's answer was not recorded: %s", text)
	}
	f.mustRefuse(t, f.callRaw(f.chief, agentDispatchTool, map[string]any{"target": "pod/gateway", "task_id": n.ID}),
		"chose to start nothing", n.ID)

	// Control: the owner's own answer on a newer ask does bind.
	third, _ := f.routeToOwner(t, n.ID)
	if _, err := f.srv.answerAskFrom(f.asOwner, third.ID, dispGateway, attention.AnswerSource{}); err != nil {
		t.Fatal(err)
	}
	f.startedOne(t, f.callRaw(f.chief, agentDispatchTool, map[string]any{"target": "pod/gateway", "task_id": n.ID}), n.ID)
}

// TestTheReleaseAndDispatchTapBindsTheRepositoryItShowed: the button names the
// task's repository ("repo example/app", or "no repository"), and the start is
// held to it — the chief's repo argument, chosen while reading outside text,
// cannot move the agent to another of the owner's repositories.
func TestTheReleaseAndDispatchTapBindsTheRepositoryItShowed(t *testing.T) {
	t.Run("task-names-a-repo", func(t *testing.T) {
		f := newReleaseFixture(t)
		f.withConnectedRepos(t, "example/app", "example/other")
		n := f.fileGatedIn(t, "feedback: in a repo", "example/app")
		ask, _ := f.routeToOwner(t, n.ID)
		want := "Release & dispatch on pod/gateway, repo example/app"
		if len(ask.Options) == 0 || ask.Options[0] != want {
			t.Fatalf("options %q; want first %q", ask.Options, want)
		}
		if _, err := f.srv.answerAskFrom(f.asOwner, ask.ID, want, attention.AnswerSource{}); err != nil {
			t.Fatal(err)
		}
		f.mustRefuse(t, f.callRaw(f.chief, agentDispatchTool, map[string]any{"target": "pod/gateway", "task_id": n.ID, "repo": "example/other"}),
			"with the repository example/app", n.ID)
		ag := f.startedOne(t, f.callRaw(f.chief, agentDispatchTool, map[string]any{"target": "pod/gateway", "task_id": n.ID}), n.ID)
		if ag.Kind != agents.KindGateway {
			t.Errorf("kind %q", ag.Kind)
		}
	})
	t.Run("no-repo", func(t *testing.T) {
		f := newReleaseFixture(t)
		f.withConnectedRepos(t, "example/other")
		n := f.fileGated(t, "feedback: no repo")
		ask, _ := f.routeToOwner(t, n.ID)
		if _, err := f.srv.answerAskFrom(f.asOwner, ask.ID, relGateway, attention.AnswerSource{}); err != nil {
			t.Fatal(err)
		}
		f.mustRefuse(t, f.callRaw(f.chief, agentDispatchTool, map[string]any{"target": "pod/gateway", "task_id": n.ID, "repo": "example/other"}),
			"with no repository", n.ID)
		f.startedOne(t, f.callRaw(f.chief, agentDispatchTool, map[string]any{"target": "pod/gateway", "task_id": n.ID}), n.ID)
	})
}

// cancelAfterAnswer is the attention store whose Answer, once recorded,
// cancels the request's context: a client that hung up right after.
type cancelAfterAnswer struct {
	attention.Store
	cancel context.CancelFunc
}

func (c cancelAfterAnswer) Answer(ctx context.Context, id int64, answer string) (attention.Ask, error) {
	a, err := c.Store.Answer(ctx, id, answer)
	c.cancel()
	return a, err
}

// TestAReleaseOutlivesTheRequestThatGaveIt: the answer is recorded and then
// the request is cancelled. The gate still comes off, audited and said.
func TestAReleaseOutlivesTheRequestThatGaveIt(t *testing.T) {
	f := newReleaseFixture(t)
	n := f.fileGated(t, "feedback: hung up")
	ask, _ := f.routeToOwner(t, n.ID)
	ctx, cancel := context.WithCancel(f.asOwner)
	defer cancel()
	f.srv.ext.Attention = cancelAfterAnswer{Store: f.srv.ext.Attention, cancel: cancel}
	if _, err := f.srv.answerAskFrom(ctx, ask.ID, relOnly, attention.AnswerSource{}); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() == nil {
		t.Fatal("control: the request was not cancelled")
	}
	if got, _ := f.notes.Get(f.asOwner, n.ID); notes.IsGated(got.Tags) {
		t.Fatalf("a cancelled request left the gate on: %v", got.Tags)
	}
	if rows := f.releaseAudits(t, n.ID); len(rows) != 1 {
		t.Errorf("audit rows %v; want one", rows)
	}
	if cs := f.releaseComments(t, n.ID); len(cs) != 1 || !strings.Contains(cs[0].Body, "muster released the gate") {
		t.Errorf("comments %+v", cs)
	}
}

// failingAudit is the plugin store whose release audit fails.
type failingAudit struct{ *plugins.PGStore }

func (failingAudit) RecordGateReleasedByAnswer(context.Context, int64, int64, []string, string) error {
	return errors.New("audit down")
}

// TestAReleaseThatCannotBeAuditedIsNotApplied: the audit row comes first, so
// when it cannot be written the gate stays on and the task says so.
func TestAReleaseThatCannotBeAuditedIsNotApplied(t *testing.T) {
	f := newReleaseFixture(t)
	n := f.fileGated(t, "feedback: audit down")
	ask, _ := f.routeToOwner(t, n.ID)
	f.srv.ext.Plugins = failingAudit{f.plugins}
	if _, err := f.srv.answerAskFrom(f.asOwner, ask.ID, relOnly, attention.AnswerSource{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.notes.Get(f.asOwner, n.ID); !notes.IsGated(got.Tags) {
		t.Fatalf("an unaudited release was applied: %v", got.Tags)
	}
	if cs := f.releaseComments(t, n.ID); len(cs) != 1 || !strings.Contains(cs[0].Body, "could not be released") {
		t.Errorf("comments %+v", cs)
	}
}

// TestReleaseOptionsComeFirstBesideAForward: a running agent on the gated task
// adds task_forward; the release options still lead, so Release only is one
// of the phone's two option buttons.
func TestReleaseOptionsComeFirstBesideAForward(t *testing.T) {
	f := newReleaseFixture(t)
	n := f.fileGated(t, "feedback: someone on it")
	if _, err := agents.NewPG(f.pool).Create(f.asOwner, agents.Agent{Name: "rel-fwd", Namespace: "ns-rel", Status: agents.StatusRunning, NoteID: &n.ID}); err != nil {
		t.Fatal(err)
	}
	ask, _ := f.routeToOwner(t, n.ID)
	// A live agent withholds a start, so Release only leads, then the forward.
	if len(ask.Options) != 2 || ask.Options[0] != relOnly || !strings.HasPrefix(ask.Options[1], "task_forward") {
		t.Errorf("options %q: want Release only first, then task_forward", ask.Options)
	}
}
