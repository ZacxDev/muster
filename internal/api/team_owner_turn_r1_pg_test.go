package api

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
)

// Round 1 on #703: the team chief's say-so at its real call sites, the team
// agent the end card never stops, and a member's "Not now".

// TestATeamChiefsGateReleaseIsWordedForTheTeam: agent_dispatch's REAL call
// site on a team's chief (F3, F4):
//
//   - owner_asked on a team task the chief gated, on a STALE typed turn: the
//     gate release is refused in the team's words, never "your owner";
//   - on a team task a MEMBER gated (no task_create_gated record): the
//     not-the-chief's-gate refusal, in the team's words;
//   - a host target: refused before host_dispatch is reached
//     (agentDispatchTeamHost), so host_dispatch's own refusal words are never
//     a team chief's.
func TestATeamChiefsGateReleaseIsWordedForTheTeam(t *testing.T) {
	td := newTeamDispatchWorld(t)
	if td.w.srv.ext.Plugins == nil {
		td.w.srv.ext.Plugins = plugins.NewPG(td.w.pool)
	}
	led, ok := td.w.srv.ext.Plugins.(gatedTaskLedger)
	if !ok {
		t.Fatal("instrument check: the plugin store keeps no gated-task ledger")
	}
	gated := func(name string, filed bool) notes.Note {
		gate := "gate:" + name + "-" + td.w.suffix
		n, err := notes.NewPG(td.w.pool).Create(td.teamCtx, notes.Note{Title: name + " " + td.w.suffix, Body: "Gated.", Tags: []string{gate}})
		if err != nil {
			t.Fatal(err)
		}
		if filed {
			if err := led.RecordGatedTaskFiled(td.teamCtx, n.ID, td.chief.ID, gate); err != nil {
				t.Fatal(err)
			}
		}
		return n
	}
	chiefs, members := gated("chiefs-gate", true), gated("members-gate", false)
	td.typed(t, td.w.a.uid, false, "go ahead")
	call := func(n notes.Note) string {
		return td.dispatch(agentDispatchIn{Target: "pod/" + agents.KindClaudeCode, TaskID: n.ID, OwnerAsked: true})
	}
	if got := call(chiefs); !strings.Contains(got, "release the gate on the team's word") || strings.Contains(got, "your owner") {
		t.Errorf("owner_asked on the chief's gate, a stale turn: %s; want the team's gate refusal", truncate(got, 400))
	}
	if got := call(members); !strings.Contains(got, "a member of your team set it") || strings.Contains(got, "your owner") {
		t.Errorf("owner_asked on a member's gate: %s; want the team's not-your-gate refusal", truncate(got, 400))
	}
	if got := td.dispatch(agentDispatchIn{Target: "host/build-01/claude", TaskID: chiefs.ID, OwnerAsked: true}); !strings.Contains(got, agentDispatchTeamHost) {
		t.Errorf("a host target on the team's chief: %s; want %q (host_dispatch never reached)", truncate(got, 400), agentDispatchTeamHost)
	}
}

// TestATeamAgentIsNeverEndedFromACard: stopping and removing a team's AGENT
// is a manager's (slice 4.5), so (F2):
//
//   - the team chief's end_session_propose naming agent:<a team agent>, on an
//     operator's fresh typed turn, files NO card (endSessionTeamAgent);
//   - an end card for one filed anyway (one filed before this release) is
//     refused on an operator's tap, and the agent is still there.
func TestATeamAgentIsNeverEndedFromACard(t *testing.T) {
	td := newTeamDispatchWorld(t)
	if td.w.srv.ext.Provisioner == nil {
		td.w.srv.ext.Provisioner = newFakeTerminal()
	}
	ag, err := agents.NewPG(td.w.pool).Create(td.teamCtx, agents.Agent{Name: "team-endme-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		Namespace: "devpod-team-endme", Status: agents.StatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	if ag.OwnerID != td.team {
		t.Fatalf("fixture: the agent is owned by %d; want the team %d", ag.OwnerID, td.team)
	}
	asks := func() int {
		var n int
		if err := td.w.pool.QueryRow(td.w.sys, `SELECT count(*) FROM attention_asks WHERE owner_id = $1`, td.team).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := asks()
	td.typed(t, td.w.b.uid, true, "stop "+ag.Name)
	if got := td.w.srv.toolEndSessionPropose(td.teamCtx, td.chief, `{"session":"agent:`+ag.Name+`"}`); !strings.Contains(got, endSessionTeamAgent) {
		t.Errorf("end_session_propose naming a team agent: %s; want %q", truncate(got, 300), endSessionTeamAgent)
	}
	if n := asks(); n != before {
		t.Errorf("end_session_propose naming a team agent filed %d card(s)", n-before)
	}
	x := plugins.Proposal{ID: 1, Owner: td.team, Kind: plugins.ProposalEndSession, SubjectKey: "agent:" + ag.Name, SubjectAgent: ag.ID}
	if got := td.w.srv.acceptEndSession(td.personCtx(td.w.b.uid, authz.ViaSession, true), x, endSessionLabel); got != endSessionTeamAgent {
		t.Errorf("an operator's tap on a team agent's end card: %q; want %q", got, endSessionTeamAgent)
	}
	if _, err := td.w.srv.ext.Agents.Get(td.teamCtx, ag.ID); err != nil {
		t.Errorf("the team agent after the refused end: %v; want it still there", err)
	}
}

// TestATeamMemberDeclinesATeamsEndCard: an operator's "Not now" on the team's
// end card declines it — read and decided AS THE TEAM — and nothing is
// closed (F3).
func TestATeamMemberDeclinesATeamsEndCard(t *testing.T) {
	td := newTeamDispatchWorld(t)
	m := &endCardMirror{Mirror: td.w.srv.ext.Mirror, team: td.team, own: td.w.a.uid}
	td.w.srv.ext.Mirror = m
	if td.w.srv.ext.Focus == nil {
		td.w.srv.ext.Focus = focus.NewPG(td.w.pool)
	}
	t.Cleanup(func() {
		_, _ = td.w.pool.Exec(authz.System(context.Background()), `DELETE FROM chief_proposals WHERE owner_id = $1`, td.team)
	})
	td.typed(t, td.w.b.uid, true, "end the build-01 session")
	var out struct {
		ID   int64 `json:"proposal_id"`
		Card int64 `json:"card_id"`
	}
	text := td.w.srv.toolEndSessionPropose(td.teamCtx, td.chief, `{"session":"`+endCardKey+`"}`)
	if json.Unmarshal([]byte(text), &out) != nil || out.Card <= 0 {
		t.Fatalf("the team chief's end_session_propose: %s", truncate(text, 300))
	}
	if _, err := td.w.srv.answerAskFrom(withOptionTap(td.personCtx(td.w.b.uid, authz.ViaSession, true)), out.Card, proposalDeclineLabel,
		attention.AnswerSource{}); err != nil {
		t.Fatalf("B's Not now on the team's end card: %v", err)
	}
	x, err := plugins.NewPG(td.w.pool).ProposalByID(td.teamCtx, out.ID)
	if err != nil || x.Decided != plugins.ProposalDeclined {
		t.Fatalf("the team's proposal after B's Not now: %+v %v; want declined", x, err)
	}
	if n := len(m.closed()); n != 0 {
		t.Fatalf("a declined end card closed %d window(s)", n)
	}
}
