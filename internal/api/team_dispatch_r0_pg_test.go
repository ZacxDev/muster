package api

import (
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/users"
)

// Team dispatch, round 0's fixes (PR #682): a team's task is not its say-so
// (operator decision Q1), a team's dispatch card's push carries no answer
// actions, the typed-turn marker honours the typer's sign-out cutoffs, the
// gate-release push's team sentence, and no "Change repo" on a team's card.

// TestATeamTaskIsNotTheTeamsSaySo (operator decision Q1): on an UNGATED team
// task, the team chief's agent_dispatch with owner_asked: false and no prompt
// does NOT start an agent. Without a request_key it is refused
// (agentDispatchTeamTaskNoKey); with one, an approval card goes in front of the
// team, as a dispatch with no task does. Answered by B (operator) in a browser
// with a fresh sign-in, the chief's next call starts it: the reservation names
// B (requested_by_user, approved_on_card) and the team's push names B.
//
// 🔴 RED at a5893ba4: the call started the agent at once — the team's task
// was taken as the team's say-so.
func TestATeamTaskIsNotTheTeamsSaySo(t *testing.T) {
	td := newTeamDispatchWorld(t)
	bare := agentDispatchIn{Target: "pod/" + agents.KindClaudeCode, TaskID: td.task.ID}
	if text := td.dispatch(bare); !strings.Contains(text, agentDispatchTeamTaskNoKey) {
		t.Errorf("a team task, no owner_asked, no prompt, no request_key: %s; want %q", truncate(text, 400), agentDispatchTeamTaskNoKey)
	}
	if on := td.teamAgentsOnTask(t); len(on) != 0 {
		t.Fatalf("a team task alone started %+v", on)
	}
	in := td.onTask("k-task-only", false)
	text := td.dispatch(in)
	ask := heldCard(text)
	if ask <= 0 {
		t.Fatalf("a team task, no owner_asked, no prompt, with a request_key: %s; want an approval card", truncate(text, 400))
	}
	if on := td.teamAgentsOnTask(t); len(on) != 0 {
		t.Fatalf("a team task alone started %+v", on)
	}
	if _, _, ok := td.reservation(t); ok {
		t.Fatal("a card was raised, yet a reservation was written")
	}
	if _, err := td.w.srv.answerAskFrom(td.personCtx(td.w.b.uid, authz.ViaSession, true), ask, approveLabel, attention.AnswerSource{}); err != nil {
		t.Fatalf("B approving in a browser with a fresh sign-in: %v", err)
	}
	text = td.dispatch(in)
	if strings.Contains(text, `"error"`) {
		t.Fatalf("the approved card did not start the agent: %s", truncate(text, 400))
	}
	on := td.teamAgentsOnTask(t)
	if len(on) != 1 || on[0].OwnerID != td.team {
		t.Fatalf("agents on the team task: %+v; want one owned by the team %d", on, td.team)
	}
	d, by, _ := td.reservation(t)
	if by != td.team || d[DetailRequestedByUser] != float64(td.w.b.uid) || d["approved_on_card"] != float64(ask) {
		t.Errorf("the reservation (user %d) detail %v; want the team's, requested_by_user B %d, approved on card %d", by, d, td.w.b.uid, ask)
	}
	want := td.w.b.name + td.w.suffix + " asked agents team " + td.w.suffix + "'s chief to start " + on[0].Name + " on task #" +
		strconv.FormatInt(td.task.ID, 10)
	eventuallyAPI(t, "the team dispatch push naming B, on the card path", func() bool { return td.teamPushed(want) })
}

// TestAPersonsOwnTaskIsStillTheirSaySo: CONTROL for
// TestATeamTaskIsNotTheTeamsSaySo — the same shape (an ungated task of the
// owner's, owner_asked: false, no prompt), for a PERSON's chief, still starts
// directly, with and without a request_key: no card. INVARIANT GUARD (green at
// a5893ba4 too): it pins that the team rule did not change a person's path.
func TestAPersonsOwnTaskIsStillTheirSaySo(t *testing.T) {
	f := newReleaseFixture(t)
	for _, c := range []struct {
		name string
		args map[string]any
	}{
		{"no request_key", map[string]any{"target": "pod/claude-code"}},
		{"a request_key", map[string]any{"target": "pod/claude-code", "request_key": "k-personal-task"}},
	} {
		n := f.task(t, "personal "+c.name)
		c.args["task_id"] = n.ID
		text := f.callRaw(f.chief, agentDispatchTool, c.args)
		if heldCard(text) > 0 {
			t.Fatalf("%s: a person's own task raised a card: %s", c.name, truncate(text, 300))
		}
		f.startedOne(t, text, n.ID)
	}
}

// TestATypedTurnEndsWithItsTypersSignIn: B's fresh typed turn is marked; B
// is then signed out everywhere (users.signed_out_at), or B's sessions are cut
// off (users.sessions_not_before: a disable, a password reset). The chief's
// owner_asked call is refused with ownerAskedNoTaskTyperSignedOut, nothing
// started. CONTROL: the same call with no cutoff starts
// (TestAnOperatorsFreshTypedTurnStartsATeamAgent).
//
// 🔴 RED at a5893ba4: the marker did not read the typer's cutoffs, so the
// signed-out member's turn started the agent.
func TestATypedTurnEndsWithItsTypersSignIn(t *testing.T) {
	for _, c := range []struct {
		name string
		cut  func(t *testing.T, td *teamDispatchWorld)
	}{
		{"signed out everywhere", func(t *testing.T, td *teamDispatchWorld) {
			if _, err := users.NewPG(td.w.pool).RecordSignOutEverywhere(td.w.sys, td.w.b.uid); err != nil {
				t.Fatal(err)
			}
		}},
		{"sessions cut off", func(t *testing.T, td *teamDispatchWorld) {
			if _, err := td.w.pool.Exec(td.w.sys, `UPDATE users SET sessions_not_before = clock_timestamp() WHERE id = $1`, td.w.b.uid); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			td := newTeamDispatchWorld(t)
			t.Cleanup(func() {
				_, _ = td.w.pool.Exec(td.w.sys, `UPDATE users SET signed_out_at = NULL, sessions_not_before = NULL WHERE id = $1`, td.w.b.uid)
			})
			td.typed(t, td.w.b.uid, true, "start a Claude agent on the team task")
			if turn := td.marked(t); turn.By != td.w.b.uid {
				t.Fatalf("fixture: B's fresh typed turn marked %+v", turn)
			}
			c.cut(t, td)
			if text := td.dispatch(td.onTask("", true)); !strings.Contains(text, ownerAskedNoTaskTyperSignedOut) {
				t.Errorf("a typer %s since: %s; want %q", c.name, truncate(text, 400), ownerAskedNoTaskTyperSignedOut)
			}
			if on := td.teamAgentsOnTask(t); len(on) != 0 {
				t.Fatalf("a typer %s since started %+v", c.name, on)
			}
		})
	}
}

// TestATeamDispatchCardsPushHasNoAnswerActions: a TEAM's dispatch card's
// notification — built as every ask push is (askPushItem) — carries no chips,
// no signed answer action and no options line, and its body leads with the
// literal sentence to open it in a browser. CONTROL: a team's OTHER owner card
// (an ask with options and no dispatch hold) keeps its chips, so the decision
// is the hold's, not "any team ask".
//
// 🔴 RED at a5893ba4, both assertions: askPushItem — the item every ask push
// starts from, and the only one repushAsk and officerAskFiled send — carried
// the card's chips and signed actions ("Approve and start · Change repo ·
// Don't start"), and no delivered push said to open it in a browser. (The
// approval card's FIRST push already dropped its chips there, in
// proposalCardFiled; its body did not say where to answer it.)
func TestATeamDispatchCardsPushHasNoAnswerActions(t *testing.T) {
	td := newTeamDispatchWorld(t)
	in := td.onTask("k-push", true)
	in.Prompt = "Do the team's work."
	id := heldCard(td.dispatch(in))
	if id <= 0 {
		t.Fatal("fixture: no card")
	}
	card, err := td.w.srv.ext.Attention.Get(td.teamCtx, id)
	if err != nil {
		t.Fatal(err)
	}
	n := attentionNotification(td.w.srv.askPushItem(td.teamCtx, card), false)
	const lead = "A team's dispatch card: open it in muster in a browser to approve it (not from the phone). "
	if n.Ask != nil || n.AskActions || !strings.HasPrefix(n.Body, lead) || strings.Contains(n.Body, "Options:") {
		t.Errorf("the team's dispatch card's push: ask %+v, actions %v, body %q; want no chips, no actions, body led by %q",
			n.Ask, n.AskActions, n.Body, lead)
	}
	eventuallyAPI(t, "the card's delivered push saying to open it in a browser", func() bool { return td.teamPushed(TeamCardPushBody) })

	other, err := td.w.srv.raiseOwnerCard(td.teamCtx, td.chief, "Which colour for the team's banner?", []string{"Red", "Blue"}, "")
	if err != nil {
		t.Fatal(err)
	}
	c := attentionNotification(td.w.srv.askPushItem(td.teamCtx, other), false)
	if c.Ask == nil || !c.AskActions || strings.HasPrefix(c.Body, lead) {
		t.Errorf("control: a team's non-dispatch owner card's push: ask %+v, actions %v, body %q; want its chips kept", c.Ask, c.AskActions, c.Body)
	}
}

// TestATeamGateReleasePushNamesTheMember (D2): applyGateRelease on a team's
// gated task, for a release resting on B's typed message, pushes the team's
// sentence — B, the team, the agent, the task — then the gate, released on
// B's words. Pinned as a literal. The stuck branch (the tag could not be
// removed) is pinned as a literal on teamGateReleasePushBody, the one builder.
//
// INVARIANT GUARD (green at a5893ba4, with the builder shimmed in): D2 is a
// fold of a sentence built twice into one builder, which changes no output;
// this pins that output, and the branch was untested before. Mutation-checked
// (each sentence, and the team branch removed).
func TestATeamGateReleasePushNamesTheMember(t *testing.T) {
	td := newTeamDispatchWorld(t)
	gated, err := notes.NewPG(td.w.pool).Create(td.teamCtx, notes.Note{Title: "gated team work " + td.w.suffix, Body: "Gated.",
		Tags: []string{"gate:infra"}})
	if err != nil {
		t.Fatal(err)
	}
	msg, _ := td.typed(t, td.w.b.uid, true, "release it and start")
	if err := td.w.srv.applyGateRelease(td.teamCtx, td.chief, gated.ID, "agent-x", &gateRelease{gates: []string{"gate:infra"}, msg: msg}); err != nil {
		t.Fatalf("applyGateRelease: %v", err)
	}
	want := td.w.b.name + td.w.suffix + " asked agents team " + td.w.suffix + "'s chief to start agent-x on task #" + strconv.FormatInt(gated.ID, 10) +
		"; muster released its gate: release it and start"
	eventuallyAPI(t, "the team's gate-release push", func() bool { return td.teamPushed(want) })

	if got := teamGateReleasePushBody("B asked T's chief to start agent-x on task #7", "go", true); got !=
		"B asked T's chief to start agent-x on task #7, but its gate tag could not be removed" {
		t.Errorf("the stuck sentence: %q", got)
	}
	if got := teamGateReleasePushBody("B asked T's chief to start agent-x on task #7", "go\nnow", false); got !=
		"B asked T's chief to start agent-x on task #7; muster released its gate: go now" {
		t.Errorf("the released sentence: %q", got)
	}
}

// TestATeamsApprovalCardHasNoChangeRepo (D3): a team's approval card, on a
// team task with no repository (where a person's card offers "Change repo"),
// offers only Approve and Decline until slice 4.8: the repo card behind the
// button cannot list repositories for the team acting principal.
//
// 🔴 RED at a5893ba4: the team's card offered "Change repo".
func TestATeamsApprovalCardHasNoChangeRepo(t *testing.T) {
	td := newTeamDispatchWorld(t)
	if td.task.Repo != "" {
		t.Fatalf("fixture: the team task has repository %q; want none (repoChangeable)", td.task.Repo)
	}
	in := td.onTask("k-change-repo", true)
	in.Prompt = "Do it."
	id := heldCard(td.dispatch(in))
	if id <= 0 {
		t.Fatal("fixture: no card")
	}
	a, err := td.w.srv.ext.Attention.Get(td.teamCtx, id)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(a.Options, "|") != approveLabel+"|"+declineLabel {
		t.Errorf("the team's approval card offers %q; want exactly %q and %q", a.Options, approveLabel, declineLabel)
	}
}
