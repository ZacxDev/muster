package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/users"
)

// TEAM DISPATCH (team sharing phase 4, slice 4.7; team_dispatch.go): the
// team-agent walk's team T (A manages it, B operates in it, V views it, N is
// in no team) with its running chief, on the server pool, opted in to chief
// dispatch (MUSTER_CHIEF_DISPATCH_OWNERS=T), a chief pod whose only working
// CLI is the shared room's lane, and a team task. The chief's agent_dispatch
// is called as the team acting principal over MCP, as the team chief's own
// token reaches it.

type teamDispatchWorld struct {
	*teamAgentWalk
	pod    *podLanes
	task   notes.Note
	pusher *payloadPusher
}

func newTeamDispatchWorld(t *testing.T) *teamDispatchWorld {
	t.Helper()
	tw := newTeamAgentWalk(t)
	w := tw.w
	w.srv.ext.TaskRouting = &TaskRouting{DispatchOwners: map[int64]bool{tw.team: true}}
	w.srv.ext.ChiefDispatch = plugins.NewPG(w.pool)
	pod := &podLanes{Gateway: w.srv.ext.Gateway}
	pod.only()
	w.srv.ext.Gateway = pod
	plane := poolPlane(t, w)
	plane.TeamsOnServerPool = map[int64]bool{tw.team: true}
	t.Cleanup(func() { plane.TeamsOnServerPool = nil })
	pusher, store := &payloadPusher{}, &memPush{}
	w.srv.ext.Push = push.NewService(store, pusher, push.VAPIDConfig{PublicKey: "k"}, log.New(io.Discard, "", 0))
	claim(t, store, tw.team, ep("team-dispatch"))
	n, err := notes.NewPG(w.pool).Create(tw.teamCtx, notes.Note{Title: "team work " + w.suffix, Body: "Do the team's work."})
	if err != nil {
		t.Fatal(err)
	}
	if n.OwnerID != tw.team {
		t.Fatalf("fixture: the team task is owned by %d; want the team %d", n.OwnerID, tw.team)
	}
	t.Cleanup(func() {
		bg := authz.System(context.Background())
		_, _ = w.pool.Exec(bg, `DELETE FROM agents WHERE owner_id = $1 AND id <> $2`, tw.team, tw.chief.ID)
		_, _ = w.pool.Exec(bg, `DELETE FROM attention_asks WHERE owner_id = $1`, tw.team)
		_, _ = w.pool.Exec(bg, `DELETE FROM notes WHERE owner_id = $1`, tw.team)
		_, _ = w.pool.Exec(bg, `DELETE FROM audit_log WHERE user_id = $1`, tw.team)
	})
	return &teamDispatchWorld{teamAgentWalk: tw, pod: pod, task: n, pusher: pusher}
}

// personCtx is uid's signed-in context through via, its sign-in fresh or two
// windows old.
func (td *teamDispatchWorld) personCtx(uid int64, via authz.Via, fresh bool) context.Context {
	bg := context.Background()
	ctx := authz.WithPrincipal(bg, td.w.srv.principalFor(bg, uid, via))
	at := time.Now()
	if !fresh {
		at = at.Add(-2 * stepup.Window())
	}
	return stepup.WithAuthTime(ctx, at)
}

// typed is uid typing text into the team chief's one shared room, through
// the browser door, with a fresh or stale sign-in — markOwnerTurn exactly as
// runEnteredTurn calls it — and the runtime accepting it in the room's lane.
// It returns the stored row and the clear the turn's end runs.
func (td *teamDispatchWorld) typed(t *testing.T, uid int64, fresh bool, text string) (agents.ChatMessage, func()) {
	t.Helper()
	msg, err := agents.NewPG(td.w.pool).AddChatMessage(td.teamCtx, agents.ChatMessage{AgentID: td.chief.ID, SessionID: td.session.ID,
		Role: "user", Content: text, TypedBy: uid})
	if err != nil {
		t.Fatal(err)
	}
	ctx, clear := td.w.srv.markOwnerTurn(td.personCtx(uid, authz.ViaSession, fresh), td.chief, msg, uid)
	agents.MarkTurnAccepted(ctx, agents.TurnAcceptance{Lane: ownerLane})
	t.Cleanup(clear)
	return msg, clear
}

// marked reports whether the team chief's owner-turn marker is set.
func (td *teamDispatchWorld) marked(t *testing.T) agents.OwnerTurn {
	t.Helper()
	turn, err := agents.NewPG(td.w.pool).OwnerTurnInFlight(td.teamCtx, td.chief.ID)
	if err != nil {
		t.Fatal(err)
	}
	return turn
}

// dispatch is the team chief's agent_dispatch, over MCP, as the team.
func (td *teamDispatchWorld) dispatch(in agentDispatchIn) string {
	return td.w.srv.toolAgentDispatch(td.teamCtx, td.chief, in)
}

func (td *teamDispatchWorld) onTask(key string, ownerAsked bool) agentDispatchIn {
	return agentDispatchIn{Target: "pod/" + agents.KindClaudeCode, TaskID: td.task.ID, RequestKey: key, OwnerAsked: ownerAsked}
}

// teamAgentsOnTask is every agent on the team task.
func (td *teamDispatchWorld) teamAgentsOnTask(t *testing.T) []agents.Agent {
	t.Helper()
	rows, err := td.w.pool.Query(td.w.sys, `SELECT id, owner_id, name FROM agents WHERE note_id = $1 ORDER BY id`, td.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []agents.Agent
	for rows.Next() {
		var a agents.Agent
		if err := rows.Scan(&a.ID, &a.OwnerID, &a.Name); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

// reservation is the team task's task.chief_dispatch detail and the row's
// user_id; ok false when there is none.
func (td *teamDispatchWorld) reservation(t *testing.T) (map[string]any, int64, bool) {
	t.Helper()
	var raw []byte
	var by int64
	err := td.w.pool.QueryRow(td.w.sys, `SELECT detail, user_id FROM audit_log WHERE action = $1 AND object = $2 ORDER BY id DESC LIMIT 1`,
		plugins.ActionChiefDispatch, plugins.NoteObject(td.task.ID)).Scan(&raw, &by)
	if err != nil {
		return nil, 0, false
	}
	var d map[string]any
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	return d, by, true
}

// heldCard is the ask id of a held answer, 0 when text is no held answer.
func heldCard(text string) int64 {
	var out struct {
		Held bool  `json:"held"`
		Ask  int64 `json:"ask_id"`
	}
	if json.Unmarshal([]byte(text), &out) != nil || !out.Held {
		return 0
	}
	return out.Ask
}

func (td *teamDispatchWorld) removeB(t *testing.T) {
	t.Helper()
	if err := users.NewPG(td.w.pool).RemoveTeamMember(td.w.sys, users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession},
		td.team, td.w.b.uid); err != nil {
		t.Fatal(err)
	}
}

// TestAnOperatorsFreshTypedTurnStartsATeamAgent: B, an operator of T, signed
// in within the step-up window, types into the team chief's room, and during
// that turn the chief calls agent_dispatch with owner_asked on the team task.
// The agent starts OWNED BY THE TEAM (never B), the reservation's audit row is
// the team's and names B (requested_by_user), and the push names B and the
// team.
//
// 🔴 RED at origin/main dc2fba34: every team dispatch was refused
// (AgentDispatchTeamLater), and markOwnerTurn never marked a member's turn.
func TestAnOperatorsFreshTypedTurnStartsATeamAgent(t *testing.T) {
	td := newTeamDispatchWorld(t)
	msg, _ := td.typed(t, td.w.b.uid, true, "start a Claude agent on the team task")
	if turn := td.marked(t); turn.MessageID != msg.ID || turn.By != td.w.b.uid {
		t.Fatalf("B's fresh typed turn marked %+v; want message %d by B %d", turn, msg.ID, td.w.b.uid)
	}
	text := td.dispatch(td.onTask("k-typed", true))
	if strings.Contains(text, `"error"`) {
		t.Fatalf("B's fresh typed turn did not start the agent: %s", truncate(text, 400))
	}
	on := td.teamAgentsOnTask(t)
	if len(on) != 1 || on[0].OwnerID != td.team {
		t.Fatalf("agents on the team task: %+v; want one, owned by the team %d", on, td.team)
	}
	d, by, ok := td.reservation(t)
	if !ok || by != td.team {
		t.Fatalf("the reservation row: %v (user %d); want one owned by the team %d", d, by, td.team)
	}
	if d[DetailRequestedByUser] != float64(td.w.b.uid) || d["owner_message"] != float64(msg.ID) {
		t.Errorf("the reservation's detail %v; want requested_by_user %d and owner_message %d", d, td.w.b.uid, msg.ID)
	}
	want := teamDispatchPushBody(td.w.b.name+td.w.suffix, "agents team "+td.w.suffix, on[0].Name, td.task.ID)
	if !strings.HasPrefix(want, td.w.b.name+td.w.suffix+" asked agents team "+td.w.suffix+"'s chief to start ") {
		t.Fatalf("instrument: the push sentence is %q", want)
	}
	eventuallyAPI(t, "the team dispatch push naming B", func() bool {
		for _, p := range td.pusher.to(ep("team-dispatch")) {
			raw, _ := json.Marshal(p)
			if strings.Contains(string(raw), want) {
				return true
			}
		}
		return false
	})
}

// TestAStaleTypedTurnFallsBackToTheCardAndTheCardsDoorIsGuarded: B types with a
// sign-in OLDER than the step-up window. The line marks nothing, so the
// chief's owner_asked call puts an approval card in front of the team instead
// of starting. The card is then refused, before anything is recorded, when
// answered: from B's paired phone; from a browser without a fresh sign-in;
// by V (a viewer). Answered by B in a browser with a fresh sign-in, it is
// recorded, and the chief's next call with the same request_key starts the
// agent, its reservation naming B.
//
// 🔴 RED at origin/main dc2fba34: the call was refused outright
// (AgentDispatchTeamLater): no card.
func TestAStaleTypedTurnFallsBackToTheCardAndTheCardsDoorIsGuarded(t *testing.T) {
	td := newTeamDispatchWorld(t)
	td.typed(t, td.w.b.uid, false, "start a Claude agent on the team task")
	if turn := td.marked(t); turn.MessageID != 0 {
		t.Fatalf("B's STALE typed turn marked %+v; want no marker", turn)
	}
	in := td.onTask("k-card", true)
	in.Prompt = "Do the team's work, as B asked."
	text := td.dispatch(in)
	ask := heldCard(text)
	if ask <= 0 {
		t.Fatalf("a stale typed turn: %s; want an approval card", truncate(text, 400))
	}
	if on := td.teamAgentsOnTask(t); len(on) != 0 {
		t.Fatalf("a stale typed turn started %+v", on)
	}
	answer := func(ctx context.Context) error {
		_, err := td.w.srv.answerAskFrom(ctx, ask, approveLabel, attention.AnswerSource{})
		return err
	}
	stillOpen := func(what string) {
		t.Helper()
		a, err := td.w.srv.ext.Attention.Get(td.teamCtx, ask)
		if err != nil || a.Status != attention.Open {
			t.Fatalf("%s: the card is %q (%v); want it still open", what, a.Status, err)
		}
	}
	for _, c := range []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"B's paired phone", td.personCtx(td.w.b.uid, authz.ViaDevice, true), TeamCardNotFromPhone},
		{"B's browser, stale sign-in", td.personCtx(td.w.b.uid, authz.ViaSession, false), TeamCardStepUp},
		{"V (viewer), fresh", td.personCtx(td.v.uid, authz.ViaSession, true), TeamCardOperatorOnly},
	} {
		if err := answer(c.ctx); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s answering the team's card: %v; want %q", c.name, err, c.want)
		}
		stillOpen(c.name)
	}
	if again := td.dispatch(in); !strings.Contains(again, agentDispatchHeldWaiting) {
		t.Fatalf("the chief's call while the card is open: %s; want %q", truncate(again, 300), agentDispatchHeldWaiting)
	}
	if err := answer(td.personCtx(td.w.b.uid, authz.ViaSession, true)); err != nil {
		t.Fatalf("B approving in a browser with a fresh sign-in: %v", err)
	}
	text = td.dispatch(in)
	if strings.Contains(text, `"error"`) {
		t.Fatalf("the approved card did not start the agent: %s", truncate(text, 400))
	}
	on := td.teamAgentsOnTask(t)
	if len(on) != 1 || on[0].OwnerID != td.team {
		t.Fatalf("agents on the team task: %+v; want one owned by the team", on)
	}
	if d, _, _ := td.reservation(t); d[DetailRequestedByUser] != float64(td.w.b.uid) || d["approved_on_card"] != float64(ask) {
		t.Errorf("the reservation's detail %v; want requested_by_user B %d approved on card %d", d, td.w.b.uid, ask)
	}
}

// TestARemovedTyperOrAnswererStartsNothing: B's fresh typed turn is marked,
// and B is removed from T before the chief calls: refused with its sentence
// (without a request_key; with one the call would fall back to a card),
// nothing started.
// Then B approves a card with a fresh sign-in and is removed before the chief
// completes it: refused, nothing started. CONTROL (the first case): the same
// call with B still a member starts (TestAnOperatorsFreshTypedTurnStartsATeamAgent).
//
// 🔴 RED at origin/main dc2fba34 for the wrong reason: every team dispatch was
// refused (AgentDispatchTeamLater); the mutation table isolates the guards.
func TestARemovedTyperOrAnswererStartsNothing(t *testing.T) {
	td := newTeamDispatchWorld(t)
	_, turnOver := td.typed(t, td.w.b.uid, true, "start it")
	td.removeB(t)
	// No request_key: the refusal itself (with one, the call falls back to
	// an approval card, as any call whose typed turn does not hold up).
	text := td.dispatch(td.onTask("", true))
	if !strings.Contains(text, ownerAskedNoTaskTyperNotOperator) {
		t.Errorf("a removed typer's turn: %s; want %q", truncate(text, 400), ownerAskedNoTaskTyperNotOperator)
	}
	if on := td.teamAgentsOnTask(t); len(on) != 0 {
		t.Fatalf("a removed typer's turn started %+v", on)
	}

	// The card: B's turn over, B back as an operator, approves, then is
	// removed.
	turnOver()
	if err := users.NewPG(td.w.pool).SetTeamMember(td.w.sys, users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession},
		td.team, td.w.b.uid, authz.TeamOperator); err != nil {
		t.Fatal(err)
	}
	in := td.onTask("k-answerer", true)
	in.Prompt = "Do the team's work."
	held := td.dispatch(in)
	ask := heldCard(held)
	if ask <= 0 {
		t.Fatalf("fixture: no card was filed: %s", truncate(held, 300))
	}
	if _, err := td.w.srv.answerAskFrom(td.personCtx(td.w.b.uid, authz.ViaSession, true), ask, approveLabel, attention.AnswerSource{}); err != nil {
		t.Fatalf("B approving: %v", err)
	}
	td.removeB(t)
	if text := td.dispatch(in); !strings.Contains(text, agentDispatchTeamCardAnswerer) {
		t.Errorf("a removed answerer's card: %s; want %q", truncate(text, 400), agentDispatchTeamCardAnswerer)
	}
	if on := td.teamAgentsOnTask(t); len(on) != 0 {
		t.Fatalf("a removed answerer's card started %+v", on)
	}
}

// TestATeamTaskTakesNoBareAnswerAsInstructions: on a team task, a prompt
// that is only an Attention answer's words is not taken as an agent's
// instructions (muster records no sign-in time on an answer): without a
// request_key refused with its sentence; with one, the card. INVARIANT GUARD
// at origin/main dc2fba34 (every team dispatch was refused there).
func TestATeamTaskTakesNoBareAnswerAsInstructions(t *testing.T) {
	td := newTeamDispatchWorld(t)
	in := agentDispatchIn{Target: "pod/" + agents.KindClaudeCode, TaskID: td.task.ID, Prompt: "the member's answer"}
	if text := td.dispatch(in); !strings.Contains(text, agentDispatchTeamAnswerNotInstructions) {
		t.Errorf("a bare answer on a team task: %s; want %q", truncate(text, 300), agentDispatchTeamAnswerNotInstructions)
	}
	in.RequestKey = "k-answer"
	if ask := heldCard(td.dispatch(in)); ask <= 0 {
		t.Errorf("with a request_key: no card")
	}
	if on := td.teamAgentsOnTask(t); len(on) != 0 {
		t.Fatalf("started %+v", on)
	}
}

// TestATeamAgentsTerminalOpensForAFreshOperator: the terminal socket's gate
// (teamAgentTerminal) lets B (operator) through with a fresh sign-in, and
// refuses B with a stale one (asking for the step-up), V (viewer) and — as
// the control on the role — nobody else at a fresh sign-in.
//
// 🔴 RED at origin/main dc2fba34: the terminal was refused to every member
// (teamAgentRefused), B's fresh sign-in included.
func TestATeamAgentsTerminalOpensForAFreshOperator(t *testing.T) {
	td := newTeamDispatchWorld(t)
	gate := func(ctx context.Context) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/agents/"+td.chief.Name+"/terminal/ws", nil).WithContext(ctx)
		r.Pattern = "GET /agents/{name}/terminal/ws"
		r.SetPathValue("name", td.chief.Name)
		rec := httptest.NewRecorder()
		if !td.w.srv.refuseTeamAgentRoute(rec, r) {
			rec.Code = 0 // passed the gate
		}
		return rec
	}
	if rec := gate(td.personCtx(td.w.b.uid, authz.ViaSession, true)); rec.Code != 0 {
		t.Errorf("B (operator), fresh: %d %q; want through the gate", rec.Code, rec.Body.String())
	}
	if rec := gate(td.personCtx(td.w.a.uid, authz.ViaSession, true)); rec.Code != 0 {
		t.Errorf("A (manager), fresh: %d %q; want through the gate", rec.Code, rec.Body.String())
	}
	if rec := gate(td.personCtx(td.w.b.uid, authz.ViaSession, false)); rec.Code != http.StatusForbidden ||
		!strings.Contains(rec.Body.String(), TeamAgentTerminalStepUp) || rec.Header().Get("X-Muster-Step-Up") != "required" {
		t.Errorf("B (operator), stale: %d %q (step-up %q); want 403 asking for a fresh sign-in", rec.Code, rec.Body.String(), rec.Header().Get("X-Muster-Step-Up"))
	}
	if rec := gate(td.personCtx(td.v.uid, authz.ViaSession, true)); rec.Code != http.StatusForbidden ||
		!strings.Contains(rec.Body.String(), TeamAgentTerminalOperator) {
		t.Errorf("V (viewer), fresh: %d %q; want 403 %q", rec.Code, rec.Body.String(), TeamAgentTerminalOperator)
	}
}

// TestTheTeamCardGateReadsTheHoldAsTheTeam: the gate must find the team's
// hold although the member's own principal cannot read host_dispatches'
// team rows — the reason it reads as the team. CONTROL: the member's own read
// of the same hold finds none.
func TestTheTeamCardGateReadsTheHoldAsTheTeam(t *testing.T) {
	td := newTeamDispatchWorld(t)
	in := td.onTask("k-hold", true)
	in.Prompt = "Do it."
	ask := heldCard(td.dispatch(in))
	if ask <= 0 {
		t.Fatal("fixture: no card")
	}
	bctx := td.personCtx(td.w.b.uid, authz.ViaDevice, true)
	if _, err := td.w.srv.hostDispatches().CardHoldByAsk(bctx, ask); !errors.Is(err, plugins.ErrHoldNotFound) {
		t.Fatalf("control: B's own principal reads the team's hold: %v; want none", err)
	}
	if handled, err := td.w.srv.teamCardRefusal(bctx, ask); !handled || err == nil || !strings.Contains(err.Error(), TeamCardNotFromPhone) {
		t.Errorf("the gate on B's phone: handled %v, %v; want it handled and refused", handled, err)
	}
	// The named phone-door refusal itself (device_actions.go), which phase
	// 2b.4's team phone answer route reuses.
	if err := td.w.srv.refuseTeamDispatchCardOnPhone(bctx, ask); err == nil || !strings.Contains(err.Error(), TeamCardNotFromPhone) {
		t.Errorf("refuseTeamDispatchCardOnPhone on the team's card: %v; want %q", err, TeamCardNotFromPhone)
	}
}

// TestATeamMarkerMustNameItsRowsTyper: a member's marker on the team chief
// whose chat row was NOT typed by that member (typed_by unset: muster's own
// line) is refused as unverifiable — the person a start's audit names is the
// row's typer, so the two must agree. CONTROL: the same marker on B's own row
// starts (TestAnOperatorsFreshTypedTurnStartsATeamAgent).
func TestATeamMarkerMustNameItsRowsTyper(t *testing.T) {
	td := newTeamDispatchWorld(t)
	st := agents.NewPG(td.w.pool)
	msg, err := st.AddChatMessage(td.teamCtx, agents.ChatMessage{AgentID: td.chief.ID, SessionID: td.session.ID, Role: "user", Content: "muster's line"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetOwnerTurn(td.teamCtx, td.chief.ID, td.w.b.uid, msg.ID, ownerLane, time.Minute); err != nil {
		t.Fatal(err)
	}
	if text := td.dispatch(td.onTask("", true)); !strings.Contains(text, ownerAskedNoTaskUnverifiable) {
		t.Errorf("a marker naming B on a row B did not type: %s; want %q", truncate(text, 300), ownerAskedNoTaskUnverifiable)
	}
	if on := td.teamAgentsOnTask(t); len(on) != 0 {
		t.Fatalf("started %+v", on)
	}
}
