package api

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
)

// A TEAM'S END CARD (team sharing 2b.4; the operator's decision,
// "Operator+, fresh sign-in"): the team chief's end_session_propose files a
// card owned by the team, and an operator or a manager of the team ends the
// session from it — in a browser, with a fresh sign-in — while a viewer, a
// stale sign-in and a paired phone are refused and the card stays open. The
// session is resolved and closed AS THE TEAM: A (the manager) has a build-01
// of her own whose window carries the SAME address and conversation id, and
// it is never the one closed.

// endCardMirror is the world's mirror with two build-01s — the team's and
// A's own, one address and one conversation — and a recording agent-close.
type endCardMirror struct {
	Mirror
	team, own int64
	mu        sync.Mutex
	closes    []endCardClose
}

type endCardClose struct {
	as   int64 // the principal the close ran as
	host string
	req  mirror.AgentCloseReq
}

const endCardKey = "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"

func (m *endCardMirror) Hosts(ctx context.Context) []mirror.HostView {
	p, ok := authz.From(ctx)
	if !ok || (p.UserID != m.team && p.UserID != m.own) {
		return nil
	}
	h := mirror.HostView{Host: "build-01", Online: true, Interactive: true, AgentClose: true}
	h.Windows = mirror.NormalizeWindows([]mirror.Window{{Session: "main", ID: "@1", Index: 1, Active: true, Claude: true,
		ClaudeSession: endCardKey, Status: mirror.StatusIdle, Activity: 1_700_000_000}}, 0)
	h.Sessions, h.Claude, h.Labels, h.Activity = mirror.SessionsOf(h.Windows)
	return []mirror.HostView{h}
}

func (m *endCardMirror) CloseAgent(ctx context.Context, host string, req mirror.AgentCloseReq) (mirror.AgentCloseResult, error) {
	p, _ := authz.From(ctx)
	m.mu.Lock()
	m.closes = append(m.closes, endCardClose{as: p.UserID, host: host, req: req})
	m.mu.Unlock()
	return mirror.AgentCloseResult{Graceful: true}, nil
}

func (m *endCardMirror) closed() []endCardClose {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]endCardClose(nil), m.closes...)
}

// TestATeamsEndCardIsAnOperatorsWithAFreshSignIn: B (an operator) asks the
// team's chief, with a fresh sign-in, to end the team's build-01 session; the
// chief's end_session_propose files the team's card. Then:
//
//   - V (a viewer), fresh, in a browser: refused (TeamEndCardOperatorOnly);
//   - B with a STALE sign-in: refused (TeamEndCardStepUp);
//   - B from his paired phone: refused (TeamEndCardNotFromPhone);
//   - each leaves the card open and closes nothing;
//   - A (a manager), fresh, in a browser: the session ends — closed AS THE
//     TEAM, on the team's window — never A's own window of the same address.
//
// 🔴 RED at base: a member's tap on a team's card was proposalNotOwnersDoor
// whoever tapped, and its accept resolved the session under the TAPPER.
func TestATeamsEndCardIsAnOperatorsWithAFreshSignIn(t *testing.T) {
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
	text := td.w.srv.toolEndSessionPropose(td.teamCtx, td.chief, `{"session":"`+endCardKey+`"}`)
	var out struct {
		ID   int64 `json:"proposal_id"`
		Card int64 `json:"card_id"`
	}
	if json.Unmarshal([]byte(text), &out) != nil || out.ID <= 0 || out.Card <= 0 {
		t.Fatalf("the team chief's end_session_propose on B's fresh turn: %s; want a card", truncate(text, 400))
	}
	answer := func(ctx context.Context) error {
		_, err := td.w.srv.answerAskFrom(withOptionTap(ctx), out.Card, endSessionLabel, attention.AnswerSource{})
		return err
	}
	stillOpen := func(who string) {
		t.Helper()
		a, err := td.w.srv.ext.Attention.Get(td.teamCtx, out.Card)
		if err != nil || a.Status != attention.Open {
			t.Fatalf("%s: the card is %q (%v); want it still open", who, a.Status, err)
		}
		if n := len(m.closed()); n != 0 {
			t.Fatalf("%s: %d window(s) closed; want none", who, n)
		}
	}
	for _, c := range []struct {
		who  string
		ctx  context.Context
		want string
	}{
		{"V (viewer), fresh, browser", td.personCtx(td.v.uid, authz.ViaSession, true), TeamEndCardOperatorOnly},
		{"B (operator), stale, browser", td.personCtx(td.w.b.uid, authz.ViaSession, false), TeamEndCardStepUp},
		{"B (operator), fresh, paired phone", td.personCtx(td.w.b.uid, authz.ViaDevice, true), TeamEndCardNotFromPhone},
	} {
		if err := answer(c.ctx); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s tapping End it: %v; want %q", c.who, err, c.want)
		}
		stillOpen(c.who)
	}

	if err := answer(td.personCtx(td.w.a.uid, authz.ViaSession, true)); err != nil {
		t.Fatalf("A (manager), fresh, browser, tapping End it: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(m.closed()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	got := m.closed()
	if len(got) != 1 || got[0].as != td.team || got[0].host != "build-01" || got[0].req.Session != "main" || got[0].req.Window != "@1" {
		t.Fatalf("the close after A's tap: %+v; want ONE, run as the team %d on its build-01 main @1 (never A's own, user %d)",
			got, td.team, td.w.a.uid)
	}
	x, err := plugins.NewPG(td.w.pool).ProposalByID(td.teamCtx, out.ID)
	if err != nil || x.Decided != plugins.ProposalAccepted {
		t.Fatalf("the team's proposal after A's tap: %+v %v; want accepted", x, err)
	}
}
