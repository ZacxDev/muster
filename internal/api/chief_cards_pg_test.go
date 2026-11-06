package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/net/html"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/ui"
)

const testAskArgs = `{"question":"Where should I run it?","options":[{"label":"Claude"},{"label":"deepseek/deepseek-v3"}]}`

// TestAskOwnerIsTheChiefsOverMCPAndOffersItsSchema: ask_owner is on the chief's
// /mcp tool list (a claude-code chief reaches it there) and on no worker's; its
// schema requires a question and options, each option a label; a chief's call
// answers that it was asked and that the turn is over, a malformed one is
// refused with the reason, and a worker's call by name is refused.
func TestAskOwnerIsTheChiefsOverMCPAndOffersItsSchema(t *testing.T) {
	f := newAttnFixture(t)
	if !f.listed(t, f.chiefTok)[askOwnerTool] {
		t.Fatal("the chief's /mcp tools/list has no ask_owner")
	}
	if f.listed(t, f.workerTok)[askOwnerTool] {
		t.Fatal("a worker's /mcp tools/list offers ask_owner")
	}
	def := askOwnerToolDef()
	req, _ := def.Parameters["required"].([]string)
	if strings.Join(req, ",") != "question,options" {
		t.Errorf("required = %v, want [question options]", def.Parameters["required"])
	}
	props := def.Parameters["properties"].(map[string]any)
	items := props["options"].(map[string]any)["items"].(map[string]any)
	if r, _ := items["required"].([]string); strings.Join(r, ",") != "label" {
		t.Errorf("an option requires %v, want [label]", items["required"])
	}
	if _, ok := props["multi"]; !ok {
		t.Error("no multi property")
	}

	text, isErr, rpcErr := f.tool(t, f.chiefTok, askOwnerTool, json.RawMessage(testAskArgs))
	if rpcErr != nil || isErr || !strings.Contains(text, `"asked":true`) || !strings.Contains(text, "END YOUR TURN NOW") {
		t.Fatalf("the chief's ask_owner: %s (error %v, rpc %v)", text, isErr, rpcErr)
	}
	text, isErr, _ = f.tool(t, f.chiefTok, askOwnerTool, map[string]any{"question": "?"})
	if !isErr || !strings.Contains(text, "options is required") {
		t.Errorf("a malformed ask_owner: %s (error %v)", text, isErr)
	}
	text, isErr, rpcErr = f.tool(t, f.workerTok, askOwnerTool, json.RawMessage(testAskArgs))
	if rpcErr == nil && !isErr {
		t.Errorf("a worker called ask_owner: %s", text)
	}
}

// endingGateway plays a gateway runtime that calls ask_owner through the
// turn's own dispatch closure and records whether the turn was then ENDED —
// what agents.RunToolLoop reads to run no further tool.
type endingGateway struct {
	args    string
	out     string
	ended   bool
	marked  bool
	entered bool
}

func (g *endingGateway) Chat(context.Context, agents.Agent, string, string, func(string)) (string, error) {
	return "", errors.New("endingGateway: the tool-less path is not expected here")
}

func (g *endingGateway) ChatWithTools(ctx context.Context, _ agents.Agent, _, _, _ string, tools []agents.ToolDef, dispatch agents.ToolDispatch, _ agents.StreamEmit) (string, error) {
	g.entered = true
	for _, d := range tools {
		if d.Name == askOwnerTool {
			g.marked = true
		}
	}
	g.out = dispatch(askOwnerTool, g.args)
	g.ended = agents.TurnEnded(ctx)
	return "", nil
}

// TestAskOwnerEndsAGatewayChiefsTurn: in a gateway chief's turn (its tools run
// in this process) a valid ask_owner ENDS the turn — the mark the tool loop
// stops on is set on the very context the turn's dispatch closure was built
// from. A refused call does not end it. Pinned through chatTurn, the one turn
// function, so the wiring (WithTurnEnd before the closure) is what is tested.
func TestAskOwnerEndsAGatewayChiefsTurn(t *testing.T) {
	gw := &endingGateway{}
	f := newAttnFixture(t, func(ext *Extensions, _ *pgxpool.Pool) { ext.Gateway = gw })
	ctx := asUser(f.member)
	for _, c := range []struct {
		args  string
		ended bool
	}{{testAskArgs, true}, {`{"question":"no options"}`, false}} {
		gw.args, gw.ended, gw.entered = c.args, false, false
		if _, err := f.srv.chatTurn(ctx, f.chief, "sess-end-"+strconv.FormatBool(c.ended), "pick", nil); err != nil {
			t.Fatal(err)
		}
		if !gw.entered || !gw.marked {
			t.Fatalf("the turn did not reach the runtime with ask_owner offered (entered %v, offered %v)", gw.entered, gw.marked)
		}
		if gw.ended != c.ended {
			t.Errorf("args %s: turn ended = %v, want %v (tool said %s)", c.args, gw.ended, c.ended, gw.out)
		}
	}
}

// addRows stores rows on a's latest session, as the turn would.
func addRows(t *testing.T, pool *pgxpool.Pool, owner int64, a agents.Agent, rows ...agents.ChatMessage) agents.ChatSession {
	t.Helper()
	st := agents.NewPG(pool)
	ctx := asUser(owner)
	sess, err := st.LatestOrCreateSession(ctx, a.ID, a.Name)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range rows {
		m.AgentID, m.SessionID = a.ID, sess.ID
		if _, err := st.AddChatMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	return sess
}

func parseHTML(t *testing.T, body string) *html.Node {
	t.Helper()
	doc, err := html.Parse(strings.NewReader("<div>" + body + "</div>"))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func nodeAttr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

func nodesWith(n *html.Node, key, val string) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(c *html.Node) {
		if c.Type == html.ElementNode {
			if v, ok := nodeAttr(c, key); ok && (val == "" || v == val) {
				out = append(out, c)
			}
		}
		for k := c.FirstChild; k != nil; k = k.NextSibling {
			walk(k)
		}
	}
	walk(n)
	return out
}

func nodeText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(c *html.Node) {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
		}
		for k := c.FirstChild; k != nil; k = k.NextSibling {
			walk(k)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}

func (f *attnFixture) chatLog(t *testing.T, a agents.Agent) *html.Node {
	t.Helper()
	w := f.call(t, http.MethodGet, "/ui/agents/"+a.Name+"/chat-log", nil, f.operator())
	if w.Code != http.StatusOK {
		t.Fatalf("chat-log: %d %s", w.Code, w.Body)
	}
	return parseHTML(t, w.Body.String())
}

// TestAStoredOwnerAskIsWaitingUntilTheOwnerTypesAnAnswer: the question card is
// drawn from the STORED transcript, so a reload draws it: waiting with its
// options while nothing typed follows it, and answered — with the owner's line —
// once a line they TYPED (the store's typed_by, as the chat doors write it) is
// stored after it. A line nobody typed (a plugin message) does not answer it.
func TestAStoredOwnerAskIsWaitingUntilTheOwnerTypesAnAnswer(t *testing.T) {
	f := newAttnFixture(t)
	addRows(t, f.pool, 1, f.op,
		agents.ChatMessage{Role: "user", Content: "dispatch something", TypedBy: 1},
		agents.ChatMessage{Role: "assistant", Kind: "tool_call", ToolID: "call_q", ToolName: askOwnerTool, Content: testAskArgs},
		agents.ChatMessage{Role: "assistant", Kind: "tool_result", ToolID: "call_q", ToolName: askOwnerTool, ToolOK: true, Content: `{"asked":true}`})
	state := func() (string, *html.Node) {
		cards := nodesWith(f.chatLog(t, f.op), "data-owner-ask", "")
		if len(cards) != 1 {
			t.Fatalf("%d question cards", len(cards))
		}
		st, _ := nodeAttr(cards[0], "data-owner-ask-state")
		return st, cards[0]
	}
	if st, card := state(); st != ui.OwnerAskWaiting || len(nodesWith(card, "data-owner-ask-pick", "")) != 2 {
		t.Fatalf("stored, unanswered: state %q with %d option buttons", st, len(nodesWith(card, "data-owner-ask-pick", "")))
	}
	addRows(t, f.pool, 1, f.op, agents.ChatMessage{Role: "user", Content: "a plugin's message, nobody typed it"})
	if st, _ := state(); st != ui.OwnerAskWaiting {
		t.Fatalf("an untyped line answered the card (state %q)", st)
	}
	addRows(t, f.pool, 1, f.op, agents.ChatMessage{Role: "user", Content: "deepseek/deepseek-v3", TypedBy: 1})
	st, card := state()
	if st != ui.OwnerAskAnswered {
		t.Fatalf("after the owner's typed answer: state %q", st)
	}
	if a := nodesWith(card, "data-owner-ask-answer", ""); len(a) != 1 || nodeText(a[0]) != "Your answer: deepseek/deepseek-v3" {
		t.Errorf("answer %v", a)
	}
}

// dispatchAgent creates an agent of owner's for a dispatch card, with a session.
func dispatchAgent(t *testing.T, pool *pgxpool.Pool, owner int64, a agents.Agent) (agents.Agent, int64) {
	t.Helper()
	ctx := asUser(owner)
	a.Name = a.Name + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	a.Namespace = "devpod-" + a.Name
	created, err := agents.NewPG(pool).Create(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(authz.System(context.Background()), `DELETE FROM agents WHERE id = $1`, created.ID)
	})
	sess, err := agents.NewPG(pool).LatestOrCreateSession(ctx, created.ID, created.Name)
	if err != nil {
		t.Fatal(err)
	}
	return created, sess.ID
}

// TestTheChiefsChatDrawsEachDispatchedAgentWithItsLiveState: every
// agent_dispatch result in the chief's stored transcript is drawn as the agent's
// card — its name, where it runs, its repository, its task link, and its state
// as dispatchCardState decides it — linking to the agent's page (its LIST link, as
// the sidebar's row: ui.AgentCardView.PageHref). A refusal is one compact line;
// an agent of another owner draws as removed and names nothing of it. The card's
// own refresh (GET /ui/dispatch-card) draws the same card.
func TestTheChiefsChatDrawsEachDispatchedAgentWithItsLiveState(t *testing.T) {
	f := newAttnFixture(t)
	ctx := asUser(1)
	task, err := f.notes.Create(ctx, notes.Note{Body: "Fix the flaky export test", Status: notes.StatusOpen, Repo: "example-org/widgets"})
	if err != nil {
		t.Fatal(err)
	}
	review, err := f.notes.Create(ctx, notes.Note{Body: "Done already"})
	if err != nil {
		t.Fatal(err)
	}
	// Create stores a new task open; the status is its own write.
	if review, err = f.notes.SetStatus(ctx, review.ID, notes.StatusReadyForReview); err != nil || review.Status != notes.StatusReadyForReview {
		t.Fatalf("review task: %+v %v", review, err)
	}
	type want struct {
		state, runtime string
		task           int64
	}
	cases := map[string]struct {
		a     agents.Agent
		setup func(a agents.Agent)
		want  want
	}{
		"starting": {a: agents.Agent{Name: "dc-start", Status: agents.StatusProvisioning, Kind: agents.KindGateway, Model: "deepseek/deepseek-v3", Repo: "example-org/widgets", NoteID: &task.ID},
			want: want{ui.DispatchStarting, "Gateway · deepseek/deepseek-v3", task.ID}},
		"working, first turn": {a: agents.Agent{Name: "dc-kick", Status: agents.StatusRunning, Kind: agents.KindClaudeCode, CCAccount: "acct-a", KickedOff: true},
			setup: func(a agents.Agent) {
				if won, err := agents.NewPG(f.pool).ClaimKickoff(ctx, a.ID, "test-kickoff", time.Minute); err != nil || !won {
					t.Fatalf("claim: %v %v", won, err)
				}
			},
			want: want{ui.DispatchWorking, "Claude Code", 0}},
		"working, chat turn": {a: agents.Agent{Name: "dc-chat", Status: agents.StatusRunning, Kind: agents.KindGateway, KickedOff: true},
			setup: func(a agents.Agent) {
				f.srv.turns.enter(a.ID)
				t.Cleanup(func() { f.srv.turns.leave(a.ID) })
			},
			want: want{ui.DispatchWorking, "Gateway · default model", 0}},
		"waiting": {a: agents.Agent{Name: "dc-wait", Status: agents.StatusRunning, Kind: agents.KindGateway, KickedOff: true},
			want: want{ui.DispatchWaiting, "Gateway · default model", 0}},
		"done, stopped": {a: agents.Agent{Name: "dc-stop", Status: agents.StatusStopped, Kind: agents.KindGateway, KickedOff: true},
			want: want{ui.DispatchDone, "Gateway · default model", 0}},
		"done, task in review": {a: agents.Agent{Name: "dc-rev", Status: agents.StatusRunning, Kind: agents.KindGateway, KickedOff: true, NoteID: &review.ID},
			want: want{ui.DispatchDone, "Gateway · default model", review.ID}},
		"error": {a: agents.Agent{Name: "dc-err", Status: agents.StatusError, Kind: agents.KindGateway, KickoffError: "the pod never came up"},
			want: want{ui.DispatchError, "Gateway · default model", 0}},
		"error, first turn failed": {a: agents.Agent{Name: "dc-kfail", Status: agents.StatusRunning, Kind: agents.KindGateway, KickedOff: true},
			// Create stores neither; the deliverer writes both after the stamp.
			setup: func(a agents.Agent) {
				st := agents.NewPG(f.pool)
				if err := st.SetKickedOff(ctx, a.ID, true); err != nil {
					t.Fatal(err)
				}
				if err := st.SetKickoffError(ctx, a.ID, "turn failed"); err != nil {
					t.Fatal(err)
				}
			},
			want: want{ui.DispatchError, "Gateway · default model", 0}},
	}
	type made struct {
		a    agents.Agent
		sess int64
		want want
	}
	var all []made
	var rows []agents.ChatMessage
	for label, c := range cases {
		a, sess := dispatchAgent(t, f.pool, 1, c.a)
		if c.setup != nil {
			c.setup(a)
		}
		all = append(all, made{a, sess, c.want})
		id := "call_" + strings.ReplaceAll(label, " ", "_")
		rows = append(rows,
			agents.ChatMessage{Role: "assistant", Kind: "tool_call", ToolID: id, ToolName: "mcp__muster__agent_dispatch", Content: `{"target":"pod/gateway"}`},
			agents.ChatMessage{Role: "assistant", Kind: "tool_result", ToolID: id, ToolName: "mcp__muster__agent_dispatch", ToolOK: true,
				Content: fmt.Sprintf(`{"agent_id":%d,"agent":%q,"target":"pod/gateway"}`, a.ID, a.Name)})
	}
	// Another owner's agent, and a refusal.
	theirs, _ := dispatchAgent(t, f.pool, f.member, agents.Agent{Name: "dc-theirs", Status: agents.StatusRunning})
	rows = append(rows,
		agents.ChatMessage{Role: "assistant", Kind: "tool_call", ToolID: "call_theirs", ToolName: agentDispatchTool, Content: `{}`},
		agents.ChatMessage{Role: "assistant", Kind: "tool_result", ToolID: "call_theirs", ToolName: agentDispatchTool, ToolOK: true,
			Content: fmt.Sprintf(`{"agent_id":%d,"agent":%q}`, theirs.ID, theirs.Name)},
		agents.ChatMessage{Role: "assistant", Kind: "tool_call", ToolID: "call_refused", ToolName: agentDispatchTool, Content: `{}`},
		agents.ChatMessage{Role: "assistant", Kind: "tool_result", ToolID: "call_refused", ToolName: agentDispatchTool, ToolOK: false,
			Content: `{"error":"` + agentDispatchMirroredSoon + `","targets":{"pods":[]}}`})
	addRows(t, f.pool, 1, f.op, rows...)

	doc := f.chatLog(t, f.op)
	byID := map[string]*html.Node{}
	for _, c := range nodesWith(doc, "data-dispatch-card", "") {
		id, _ := nodeAttr(c, "data-dispatch-agent")
		if st, _ := nodeAttr(c, "data-dispatch-state"); st == "refused" {
			id = "refused"
		}
		byID[id] = c
	}
	if len(byID) != len(all)+2 {
		t.Fatalf("%d cards, want %d", len(byID), len(all)+2)
	}
	check := func(label string, card *html.Node, m made) {
		t.Helper()
		if st, _ := nodeAttr(card, "data-dispatch-state"); st != m.want.state {
			t.Errorf("%s: state %q, want %q", label, st, m.want.state)
		}
		open := nodesWith(card, "data-dispatch-open", "")
		wantHref := ui.AgentCardView{Name: m.a.Name, SessionID: m.sess}.PageHref()
		if len(open) != 1 {
			t.Fatalf("%s: %d open links", label, len(open))
		}
		if h, _ := nodeAttr(open[0], "href"); h != wantHref {
			t.Errorf("%s: the card opens %q, want the agent's page %q", label, h, wantHref)
		}
		if r := nodesWith(card, "data-dispatch-runtime", ""); len(r) != 1 || nodeText(r[0]) != m.want.runtime {
			t.Errorf("%s: runtime %v, want %q", label, r, m.want.runtime)
		}
		tasks := nodesWith(card, "data-dispatch-task", "")
		if m.want.task == 0 && len(tasks) != 0 || m.want.task != 0 && (len(tasks) != 1 || func() string { v, _ := nodeAttr(tasks[0], "href"); return v }() != "/tasks/"+strconv.FormatInt(m.want.task, 10)) {
			t.Errorf("%s: task links %d, want task %d", label, len(tasks), m.want.task)
		}
	}
	for _, m := range all {
		card, ok := byID[strconv.FormatInt(m.a.ID, 10)]
		if !ok {
			t.Errorf("no card for %s", m.a.Name)
			continue
		}
		check(m.a.Name, card, m)
		// The card's own refresh draws the same card.
		w := f.call(t, http.MethodGet, ui.DispatchCardSrc(m.a.ID), nil, f.operator())
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: %d", ui.DispatchCardSrc(m.a.ID), w.Code)
		}
		refreshed := nodesWith(parseHTML(t, w.Body.String()), "data-dispatch-card", "")
		if len(refreshed) != 1 {
			t.Fatalf("the refresh drew %d cards", len(refreshed))
		}
		check(m.a.Name+" (refresh)", refreshed[0], m)
	}
	// The repository line, on the one card that has one.
	for _, m := range all {
		if m.a.Repo == "" {
			continue
		}
		if r := nodesWith(byID[strconv.FormatInt(m.a.ID, 10)], "data-dispatch-repo", ""); len(r) != 1 || nodeText(r[0]) != m.a.Repo {
			t.Errorf("%s: repo %v", m.a.Name, r)
		}
	}
	other := byID[strconv.FormatInt(theirs.ID, 10)]
	if other == nil {
		t.Fatal("no card for the other owner's agent")
	}
	if nodesWith(other, "href", "") != nil || strings.Contains(nodeText(other), theirs.Name) {
		t.Errorf("another owner's agent is linked or named: %s", nodeText(other))
	}
	if r := nodesWith(byID["refused"], "data-dispatch-refused", ""); len(r) != 1 || nodeText(r[0]) != agentDispatchMirroredSoon {
		t.Errorf("refusal %v", r)
	}
	// The live stream's refusal card, by the same renderer.
	w := f.call(t, http.MethodGet, ui.DispatchCardRefusedSrc("no target named"), nil, f.operator())
	if r := nodesWith(parseHTML(t, w.Body.String()), "data-dispatch-refused", ""); w.Code != http.StatusOK || len(r) != 1 || nodeText(r[0]) != "no target named" {
		t.Errorf("the refused card: %d %s", w.Code, w.Body)
	}
	if w := f.call(t, http.MethodGet, ui.DispatchCardPath, nil, f.operator()); w.Code != http.StatusBadRequest {
		t.Errorf("a card for nothing: %d", w.Code)
	}
}

// TestAskOwnerIsOfferedWithNoStore (round 0, D1): ask_owner needs no store, so
// a chief on a server with NO attention or plugin store is still offered it —
// on its gateway list and its /mcp list — and its call still ends the turn. The
// chief's standing instructions name it unconditionally; this is the tool
// keeping that promise. Control: a worker on the same server is not offered it.
func TestAskOwnerIsOfferedWithNoStore(t *testing.T) {
	f := newConnFixture(t)
	if f.srv.attentionToolsOn() {
		t.Fatal("control: this fixture has the attention store")
	}
	sys := authz.System(context.Background())
	var member int64
	if err := f.pool.QueryRow(sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`,
		"askowner-"+strconv.FormatInt(time.Now().UnixNano(), 36)).Scan(&member); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(sys, `DELETE FROM agents WHERE owner_id = $1`, member)
		_, _ = f.pool.Exec(sys, `DELETE FROM users WHERE id = $1`, member)
	})
	chief, _ := dispatchAgent(t, f.pool, member, agents.Agent{Name: "nostore-chief", DisplayName: "chief", Status: agents.StatusRunning})
	worker, _ := dispatchAgent(t, f.pool, member, agents.Agent{Name: "nostore-worker", Status: agents.StatusRunning})
	ctx := asUser(member)
	has := func(defs []agents.ToolDef) bool {
		for _, d := range defs {
			if d.Name == askOwnerTool {
				return true
			}
		}
		return false
	}
	if !has(f.srv.AgentToolDefsFor(ctx, chief)) || !has(f.srv.MCPToolDefsFor(ctx, chief)) {
		t.Fatal("a chief on a server with no store is not offered ask_owner")
	}
	if has(f.srv.AgentToolDefsFor(ctx, worker)) || has(f.srv.MCPToolDefsFor(ctx, worker)) {
		t.Fatal("a worker is offered ask_owner")
	}
	tctx := agents.WithTurnEnd(ctx)
	if out := f.srv.dispatchAgentTool(tctx, chief, askOwnerTool, testAskArgs); !strings.Contains(out, `"asked":true`) || !agents.TurnEnded(tctx) {
		t.Fatalf("ask_owner with no store: %s (turn ended %v)", out, agents.TurnEnded(tctx))
	}
}
