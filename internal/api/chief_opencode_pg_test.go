package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/ui"
	"github.com/ZacxDev/muster/internal/users"
)

// THE CHIEF'S OPENCODE SESSIONS, muster's side (session_runtime.go,
// chief_session_cap.go, mcp_session_model.go): who is offered the runtime,
// what model a session may carry, where its turns go, the budget card, and
// the model stamped on what its MCP calls write. On the spend door's fixture
// (newSessPG), whose admin chief is made a claude-code chief.

// ocPG is sessPG with the chiefs on claude-code, both runtimes offered, the
// users store's audit writer, and an opencode side chat of the admin's chief.
type ocPG struct {
	*sessPG
	keyOC string
	idOC  int64
}

func newOCPG(t *testing.T, daily float64) *ocPG {
	t.Helper()
	p := &ocPG{sessPG: newSessPGChiefs(t, daily, 0, 0, true), keyOC: "sess-pg:oc"}
	if err := p.f.pool.QueryRow(p.sys, `INSERT INTO chat_sessions (agent_id, owner_id, session_key, runtime, model)
		VALUES ($1, 1, $2, 'opencode', 'openrouter/vendor-x/model-x1') RETURNING id`, p.adminChief, p.keyOC).Scan(&p.idOC); err != nil {
		t.Fatal(err)
	}
	p.f.srv.ext.SessionRuntimes = []string{agents.SessionRuntimeClaudeCode, agents.SessionRuntimeOpencode}
	p.f.srv.ext.Users = users.NewPG(p.f.pool)
	return p
}

func (p *ocPG) agent(t *testing.T, id int64) agents.Agent {
	t.Helper()
	a, err := p.f.srv.ext.Agents.Get(p.sys, id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// TestOpencodeIsOfferedOnlyWhereTheSpendDoorServes: the runtime is offered to
// the admin's claude-code chief only while the spend door is on — the door's
// own predicates — and never to another owner's chief, nor with the door off.
func TestOpencodeIsOfferedOnlyWhereTheSpendDoorServes(t *testing.T) {
	p := newOCPG(t, 0.5)
	s := p.f.srv
	ctx := asAdmin(context.Background())
	if got := s.sessionRuntimesFor(ctx, p.agent(t, p.adminChief)); !slices.Equal(got, []string{"claude-code", "opencode"}) {
		t.Fatalf("the admin's chief is offered %v, want claude-code and opencode", got)
	}
	if got := s.sessionRuntimesFor(ctx, p.agent(t, p.memberChief)); slices.Contains(got, "opencode") {
		t.Fatalf("another owner's chief is offered %v: the spend door serves the admin's chief only", got)
	}
	cs := s.chiefSessions
	s.chiefSessions = nil
	t.Cleanup(func() { s.chiefSessions = cs })
	if got := s.sessionRuntimesFor(ctx, p.agent(t, p.adminChief)); !slices.Equal(got, []string{"claude-code"}) {
		t.Fatalf("with the spend door off the admin's chief is offered %v, want claude-code only", got)
	}
	if _, err := s.runtimeSessionModel(ctx, p.agent(t, p.adminChief), "opencode", "vendor-x/model-x1"); err == nil {
		t.Fatal("with the spend door off an opencode session was admitted")
	}
	b := s.chiefChatBarFor(ctx, p.agent(t, p.adminChief), agents.ChatSession{ID: p.idX})
	if b.OpenRouter {
		t.Fatal("with the spend door off the \"+\" menu offers OpenRouter")
	}
	s.chiefSessions = cs
	if b := s.chiefChatBarFor(ctx, p.agent(t, p.adminChief), agents.ChatSession{ID: p.idX}); !b.OpenRouter {
		t.Fatal("with the spend door on the \"+\" menu does not offer OpenRouter")
	}
}

// TestAnOpencodeSessionsModelIsACatalogueID: every door that opens one (New
// session and Continue's runtimeSessionModel, "+"'s sideChatModel) stores an
// exact catalogue id as openrouter/<id>, resolves a case variant of exactly
// one model, and refuses a near miss (naming the closest), an empty model and
// a runtime nothing offers.
func TestAnOpencodeSessionsModelIsACatalogueID(t *testing.T) {
	p := newOCPG(t, 0.5)
	s, a := p.f.srv, p.agent(t, p.adminChief)
	ctx := asAdmin(context.Background())
	for in, want := range map[string]string{"vendor-x/model-x1": "openrouter/vendor-x/model-x1",
		"openrouter/vendor-y/model-y2": "openrouter/vendor-y/model-y2", "VENDOR-P/Model-P3": "openrouter/vendor-p/model-p3"} {
		got, err := s.runtimeSessionModel(ctx, a, "opencode", in)
		if err != nil || got != want {
			t.Errorf("model %q: %q %v, want %q", in, got, err, want)
		}
		if got, err := s.sideChatModel(ctx, a, "opencode", in); err != nil || got != want {
			t.Errorf("\"+\" model %q: %q %v, want %q", in, got, err, want)
		}
	}
	if _, err := s.runtimeSessionModel(ctx, a, "opencode", "vendor-x/model-x"); err == nil || !strings.Contains(err.Error(), "vendor-x/model-x1") {
		t.Errorf("a near miss: %v, want a refusal naming the closest model", err)
	}
	if _, err := s.runtimeSessionModel(ctx, a, "opencode", ""); err == nil {
		t.Error("an opencode session with no model was admitted")
	}
	if _, err := s.sideChatModel(ctx, a, "gateway", "sonnet"); err == nil {
		t.Error("\"+\" admitted a runtime nothing offers")
	}
	if got, err := s.sideChatModel(ctx, a, "", "sonnet"); err != nil || got != "sonnet" {
		t.Errorf("control: \"+\" on a Claude model: %q %v", got, err)
	}
	sess, err := s.openSideChatOn(ctx, a, "opencode", "openrouter/vendor-x/model-x1")
	if err != nil || sess.Runtime != "opencode" || sess.Model != "openrouter/vendor-x/model-x1" {
		t.Fatalf("an opencode side chat: %+v %v", sess, err)
	}
}

// TestAnOpencodeSessionsTurnGoesToCcdsOpencodePath: its route names the
// session in the opencode headers only — no Claude lane header, never pinned —
// so the transport sends it to agents.OpencodeResponsesPath.
func TestAnOpencodeSessionsTurnGoesToCcdsOpencodePath(t *testing.T) {
	ctx := withSessionRoute(context.Background(), agents.Agent{ID: 1}, agents.ChatSession{SessionKey: "k-oc",
		Runtime: agents.SessionRuntimeOpencode, Model: "openrouter/vendor-x/model-x1", IsDefault: true})
	r, ok := agents.TurnRouteFrom(ctx)
	if !ok || r.Runtime != agents.SessionRuntimeOpencode {
		t.Fatalf("route %+v %v", r, ok)
	}
	if r.Header.Get(agents.HeaderOCSession) != "k-oc" || r.Header.Get(agents.HeaderOCModel) != "openrouter/vendor-x/model-x1" {
		t.Errorf("opencode headers %v", r.Header)
	}
	for _, h := range []string{agents.HeaderCCSession, agents.HeaderCCModel, agents.HeaderCCPinned} {
		if r.Header.Get(h) != "" {
			t.Errorf("an opencode route carries %s", h)
		}
	}
	if agents.TurnLane(ctx) != "k-oc" {
		t.Errorf("the turn's lane is %q, want the session key (ccd's /v1/clis names the lane by it)", agents.TurnLane(ctx))
	}
}

// TestASpentBudgetAnswersAnOpencodeTurnWithTheCard: with room, an opencode
// session's turn is sent (the control); once the owner's day is spent, the
// next turn sends nothing and is answered with the budget card — the spent
// and cap amounts and the 00:00 UTC reset — and a Claude session's turn is
// never carded.
func TestASpentBudgetAnswersAnOpencodeTurnWithTheCard(t *testing.T) {
	p := newOCPG(t, 0.011)
	s := p.f.srv
	gw := &countingGateway{}
	s.ext.Gateway = gw
	ctx := asAdmin(context.Background())
	a := p.agent(t, p.adminChief)
	st, _ := s.sessionRuntimeStore()
	oc, err := st.SessionByKey(ctx, a.ID, p.keyOC)
	if err != nil {
		t.Fatal(err)
	}
	if parts, err := s.runEnteredTurn(ctx, a, oc, "with room", notTyped, nil, nil); err != nil || gw.n.Load() != 1 || len(parts) == 0 || parts[0].Kind == ui.ChatKindSpendCap {
		t.Fatalf("control: with room the turn is sent: %v %d %+v", err, gw.n.Load(), parts)
	}
	for i := 0; i < 3; i++ {
		if w := p.post(t, tokSessAdminChief, p.keyX); w.Code != http.StatusOK {
			t.Fatalf("spend %d: %d", i, w.Code)
		}
	}
	parts, err := s.runEnteredTurn(ctx, a, oc, "over budget", notTyped, nil, nil)
	if err != nil || gw.n.Load() != 1 {
		t.Fatalf("over budget: %v, %d turns sent (want none past the control)", err, gw.n.Load())
	}
	if len(parts) != 1 || parts[0].Kind != ui.ChatKindSpendCap {
		t.Fatalf("over budget the turn answered %+v, want the budget card", parts)
	}
	var c ui.SpendCapCard
	if err := json.Unmarshal([]byte(parts[0].Content), &c); err != nil {
		t.Fatal(err)
	}
	tomorrow := time.Now().UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
	if !usdNear(c.DailyCapUSD, 0.011) || !usdNear(c.DaySpentUSD, 3*sessPGCost) || !c.ResetsAt.Equal(tomorrow) || c.Refusal == "" {
		t.Fatalf("the card %+v, want $0.012 of $0.011, resetting %s", c, tomorrow)
	}
	claude, err := st.SessionByKey(ctx, a.ID, p.keyClaude)
	if err != nil {
		t.Fatal(err)
	}
	if parts, err := s.runEnteredTurn(ctx, a, claude, "a Claude turn", notTyped, nil, nil); err != nil || gw.n.Load() != 2 || parts[0].Kind == ui.ChatKindSpendCap {
		t.Fatalf("a Claude session's turn over the OpenRouter budget: %v %d %+v, want it sent", err, gw.n.Load(), parts)
	}
}

// TestAnOpencodeLanesCallsNameItsModel — every write an OpenRouter session
// makes records session_model. A call carrying the opencode session's header
// leaves its chief_session.tool_call row, and that row — like every audit row
// written under the call — carries the session's model and key. A header
// naming the chief's Claude session, or no session at all, refuses the call
// and writes nothing; a call with no header writes no such row (control).
func TestAnOpencodeLanesCallsNameItsModel(t *testing.T) {
	p := newOCPG(t, 0.5)
	// Any tool the chief is offered: the row is the call's, whatever the tool.
	list, code := p.f.mcp(t, tokSessAdminChief, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
	if code != http.StatusOK || list.Error != nil {
		t.Fatalf("tools/list: %d %+v", code, list)
	}
	var tools struct {
		Tools []struct{ Name string } `json:"tools"`
	}
	if err := json.Unmarshal(list.Result, &tools); err != nil || len(tools.Tools) == 0 {
		t.Fatalf("tools/list: %s %v", list.Result, err)
	}
	tool := tools.Tools[0].Name
	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tool + `","arguments":{}}}`
	rows := func() []map[string]any {
		r, err := p.f.pool.Query(p.sys, `SELECT detail FROM audit_log WHERE action = $1 AND object = $2 ORDER BY id`,
			ChiefSessionToolCallAction, fmt.Sprintf("agent:%d", p.adminChief))
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		var out []map[string]any
		for r.Next() {
			var b []byte
			if err := r.Scan(&b); err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			out = append(out, m)
		}
		return out
	}
	if _, code := p.f.mcp(t, tokSessAdminChief, call, nil); code != http.StatusOK {
		t.Fatalf("control call: %d", code)
	}
	if n := len(rows()); n != 0 {
		t.Fatalf("control: a call naming no opencode session wrote %d rows", n)
	}
	r, code := p.f.mcp(t, tokSessAdminChief, call, map[string]string{agents.HeaderOCSession: p.keyOC})
	if code != http.StatusOK {
		t.Fatalf("opencode call: %d", code)
	}
	if r.Error != nil {
		t.Fatalf("the opencode call: JSON-RPC error %+v", *r.Error)
	}
	if text, _ := toolTextRaw(t, r); strings.Contains(text, agents.HeaderOCSession) {
		t.Fatalf("the opencode session's own call was refused: %s", text)
	}
	got := rows()
	if len(got) != 1 || got[0]["tool"] != tool || got[0][users.AuditSessionModelKey] != "openrouter/vendor-x/model-x1" ||
		got[0][users.AuditSessionKeyKey] != p.keyOC {
		t.Fatalf("the opencode call's rows %+v, want one naming the tool, the model and the session (log: %s)", got, p.logs.String())
	}
	for _, key := range []string{p.keyClaude, "sess-pg:nobody"} {
		r, code := p.f.mcp(t, tokSessAdminChief, call, map[string]string{agents.HeaderOCSession: key})
		if code != http.StatusOK {
			t.Fatalf("%s: %d", key, code)
		}
		text, isErr := toolTextRaw(t, r)
		if !isErr || !strings.Contains(text, agents.HeaderOCSession) {
			t.Errorf("a call naming %q ran: %s", key, text)
		}
	}
	if n := len(rows()); n != 1 {
		t.Fatalf("refused calls wrote rows: %d, want 1", n)
	}
}
