package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/db"
	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/hqtest"
)

// laneChat is an api.Gateway recording each Chat with its session key and the
// lane headers its route carries.
type laneChat struct {
	stubGateway
	mu    sync.Mutex
	turns []laneTurn
	// reply answers each turn (nil: "lane reply").
	reply func() (string, error)
}

type laneTurn struct {
	agent, sessionKey, text string
	session, pinned         string
	ephemeral, toolless     string
	newConversation         string
	// mcpSession is the route's X-Muster-CC-MCP-Session: "1" asks ccd to
	// start the lane with an MCP config naming its session.
	mcpSession string
}

func (g *laneChat) Chat(ctx context.Context, a agents.Agent, sessionKey, message string, _ func(string)) (string, error) {
	t := laneTurn{agent: a.Name, sessionKey: sessionKey, text: message}
	if rt, ok := agents.TurnRouteFrom(ctx); ok {
		t.session, t.pinned = rt.Header.Get(agents.HeaderCCSession), rt.Header.Get(agents.HeaderCCPinned)
		t.ephemeral, t.toolless = rt.Header.Get(agents.HeaderCCEphemeral), rt.Header.Get(agents.HeaderCCToolless)
		t.newConversation = rt.Header.Get(agents.HeaderCCNewConversation)
		t.mcpSession = rt.Header.Get("X-Muster-CC-MCP-Session")
	}
	g.mu.Lock()
	g.turns = append(g.turns, t)
	reply := g.reply
	g.mu.Unlock()
	if reply != nil {
		return reply()
	}
	return "lane reply", nil
}

func (g *laneChat) sent() []laneTurn {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]laneTurn(nil), g.turns...)
}

// chatPG is a real store, a fake HQ serving one chat, and a server running
// officer o — a fresh_session_per_item judgement pool, so a turn sent under
// the PRIMARY's turn context would carry agents.HeaderCCNewConversation.
type chatPG struct {
	ctx  context.Context
	pg   *agents.PGStore
	hq   *hqtest.Server
	s    *Server
	rec  *officers.Reconciler
	o    officers.Officer
	row  agents.Agent
	chat *laneChat
}

func newChatPG(t *testing.T) *chatPG {
	t.Helper()
	ctx, cancel := context.WithTimeout(dbtest.AsOperator(context.Background()), 60*time.Second)
	t.Cleanup(cancel)
	pool, err := db.Connect(ctx, dbtest.DSN(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pg := agents.NewPG(pool)
	hq := hqtest.New(map[string]string{offBTok: offB})
	t.Cleanup(hq.Close)
	hq.ClaimNext = true
	prefix := "ochat-" + time.Now().Format("150405.000") + "-"
	o := officers.Officer{ID: offB, HQURL: hq.BaseURL(), IdleAfter: time.Hour, PollInterval: time.Minute, Owner: 1,
		Tiers: &officers.Tiers{Routine: officers.TierPool{Model: "haiku", MaxInstances: 1},
			Judgement: officers.TierPool{Model: "opus", MaxInstances: 1, FreshSessionPerItem: true}}}.WithToken(offBTok)
	o.Tier, o.Model, o.MaxInstances = officers.TierJudgement, "opus", 1
	name := o.InstanceName(1)
	row, err := pg.Create(ctx, agents.Agent{Name: name, Namespace: prefix + name, Status: agents.StatusRunning,
		Kind: agents.KindClaudeCode, CCAccount: "acct-one", HooksToken: "tok-" + prefix})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := pg.SetKickedOff(ctx, row.ID, true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(dbtest.AsOperator(context.Background()), `DELETE FROM agents WHERE id=$1`, row.ID)
	})
	chat := &laneChat{}
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(&bytes.Buffer{}, "", 0))
	s.UseExtensions(Extensions{Agents: pg, Provisioner: readyProv{name: name}, Gateway: chat,
		SessionLiveness: stubLiveness{}, Kinds: stubKinds{pick: "acct-one"}, AgentNamespacePrefix: prefix})
	rec, err := s.EnableOfficers(OfficersConfig{Officers: []officers.Officer{o}, InstallSession: discardSession, NoShipResume: true,
		ChatHeartbeatEvery: time.Hour, ChatReplyRetries: []time.Duration{time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	return &chatPG{ctx: ctx, pg: pg, hq: hq, s: s, rec: rec, o: o, row: row, chat: chat}
}

// pass runs one wake-loop pass and waits for the chat turns it started.
func (c *chatPG) pass(t *testing.T) officers.PassResult {
	t.Helper()
	res := c.rec.Check(c.ctx, c.o.Key(), true)
	c.s.WaitForOfficerReprompts()
	return res
}

// TestAChatTurnIsALaneTurnAndNeverAFreshConversation is the seam the unit
// tests cannot see: the wake loop, the real store and the gateway together.
// A member turn on a fresh_session_per_item pool's officer is run in the
// chat's OWN ccd lane on the instance — X-Muster-CC-Session
// "officer-chat:<chat_id>", not pinned, not a throwaway check, and with NO
// new-conversation header (fresh_session_per_item is the primary's) — its
// reply is posted, the turn and reply are recorded on a claude-code session
// of the instance's agent (so the chat reads on its agent page), and the
// marks are kept on that row.
func TestAChatTurnIsALaneTurnAndNeverAFreshConversation(t *testing.T) {
	c := newChatPG(t)
	c.hq.AddChat(offB, "c-41", "triage")
	seq, id := c.hq.MemberSays("c-41", "member1", "Member One", "Is the API healthy?")
	c.pass(t)
	turns := c.chat.sent()
	if len(turns) != 1 {
		t.Fatalf("%d gateway turns, want 1 (the chat's): %+v", len(turns), turns)
	}
	tr := turns[0]
	if tr.agent != c.row.Name || tr.sessionKey != "officer-chat:c-41" || tr.session != "officer-chat:c-41" {
		t.Fatalf("the chat's turn went to agent %s session %q (route %q); want %s, officer-chat:c-41 on both", tr.agent, tr.sessionKey, tr.session, c.row.Name)
	}
	if tr.pinned != "" || tr.ephemeral != "" || tr.toolless != "" || tr.newConversation != "" {
		t.Fatalf("the chat lane's route carried pinned %q ephemeral %q toolless %q new-conversation %q; want none", tr.pinned, tr.ephemeral, tr.toolless, tr.newConversation)
	}
	if tr.mcpSession != "1" {
		t.Fatalf("the chat lane's route carried X-Muster-CC-MCP-Session %q; want 1, so its MCP calls name its session", tr.mcpSession)
	}
	if !strings.Contains(tr.text, "Member One (@member1) wrote:\nIs the API healthy?") {
		t.Fatalf("the lane's turn: %q", tr.text)
	}
	msgs := c.hq.ChatMessages("c-41")
	if len(msgs) != 2 || msgs[1].Role != "officer" || msgs[1].InReplyTo != seq || msgs[1].Text != "lane reply" {
		t.Fatalf("chat after the pass: %+v; want the lane's reply to %d", msgs, seq)
	}
	if st := c.hq.Item(id).Status; st != "done" {
		t.Fatalf("item %s, want done", st)
	}
	sess, err := c.pg.SessionByKeyAnyAgent(c.ctx, "officer-chat:c-41")
	if err != nil || sess.AgentID != c.row.ID || sess.Runtime != agents.SessionRuntimeClaudeCode || sess.IsDefault {
		t.Fatalf("the chat's session: %+v %v; want a non-default claude-code session of agent %d", sess, err, c.row.ID)
	}
	seen, answered, err := c.pg.OfficerChatMarks(c.ctx, sess.ID)
	if err != nil || seen != msgs[1].Seq || answered != msgs[1].Seq {
		t.Fatalf("marks %d/%d (%v); want both at the reply's seq %d", seen, answered, err, msgs[1].Seq)
	}
	stored, err := c.pg.ListChatMessages(c.ctx, sess.ID)
	if err != nil || len(stored) != 2 || stored[0].Role != "user" || stored[1].Role != "assistant" || stored[1].Content != "lane reply" {
		t.Fatalf("the session's messages: %+v %v; want the turn and the reply", stored, err)
	}
}

// TestACcdCapacityRefusalNeverFailsAChat: ccd answers the lane's turn 503
// capacity (every CCD_MAX_SESSIONS slot held). The item goes back to open —
// not failed, not dropped — nothing is recorded, and the next pass answers it.
func TestACcdCapacityRefusalNeverFailsAChat(t *testing.T) {
	c := newChatPG(t)
	c.hq.AddChat(offB, "c-41", "triage")
	seq, id := c.hq.MemberSays("c-41", "member1", "Member One", "Is the API healthy?")
	c.chat.reply = func() (string, error) {
		return "", &agents.RuntimeError{Status: 503, Type: agents.FailureCapacity, Message: "every lane is busy"}
	}
	c.pass(t)
	if it := c.hq.Item(id); it.Status != "open" || it.ClaimedBy != "" {
		t.Fatalf("after a capacity refusal the item is %s by %q; want open", it.Status, it.ClaimedBy)
	}
	if n := len(c.hq.ChatMessages("c-41")); n != 1 {
		t.Fatalf("%d chat messages after a refused turn, want only the member's", n)
	}
	c.chat.reply = nil
	c.pass(t)
	if msgs := c.hq.ChatMessages("c-41"); len(msgs) != 2 || msgs[1].InReplyTo != seq {
		t.Fatalf("the next pass: %+v; want the reply to %d", msgs, seq)
	}
	if turns := c.chat.sent(); !strings.Contains(turns[len(turns)-1].text, "Is the API healthy?") {
		t.Fatalf("the retried turn did not carry the member's message: %q", turns[len(turns)-1].text)
	}
}

// TestThePrimaryNeverClaimsAChatItem: the instance's own officer_claim — HQ's
// claim-next handing it the high-priority chat item, then a claim of it by id —
// is refused (a plain "not claimed" result), and the item released at once for
// the chat runner. The control: a queue item is claimed as before.
func TestThePrimaryNeverClaimsAChatItem(t *testing.T) {
	c := newChatPG(t)
	c.hq.AddChat(offB, "c-41", "triage")
	_, id := c.hq.MemberSays("c-41", "member1", "Member One", "Is the API healthy?")
	a, err := c.pg.GetByName(c.ctx, c.row.Name)
	if err != nil {
		t.Fatal(err)
	}
	notClaimed := func(out string) bool {
		var r map[string]any
		return json.Unmarshal([]byte(out), &r) == nil && r["claimed"] == false && r["chat"] == true
	}
	out, _ := c.s.dispatchOfficerTool(c.ctx, a, officers.ToolClaim, `{}`)
	if !notClaimed(out) {
		t.Fatalf("claim-next handed the primary the chat item: %s", out)
	}
	if it := c.hq.Item(id); it.Status != "open" || it.ClaimedBy != "" {
		t.Fatalf("after the refused claim-next the chat item is %s by %q; want open", it.Status, it.ClaimedBy)
	}
	out, _ = c.s.dispatchOfficerTool(c.ctx, a, officers.ToolClaim, `{"item_id":"`+id+`"}`)
	if !notClaimed(out) {
		t.Fatalf("a claim of the chat item by id: %s", out)
	}
	if it := c.hq.Item(id); it.Status != "open" || it.ClaimedBy != "" {
		t.Fatalf("after the refused claim by id the chat item is %s by %q; want open", it.Status, it.ClaimedBy)
	}
	q := c.hq.AddTieredItem(offB, "triage.investigate", "svc:1", officers.TierJudgement)
	out, _ = c.s.dispatchOfficerTool(c.ctx, a, officers.ToolClaim, `{"item_id":"`+q+`"}`)
	if it := c.hq.Item(q); it.Status != "claimed" || it.ClaimedBy != c.row.Name {
		t.Fatalf("control: a queue item claimed by id is %s by %q (%s); want claimed by %s", it.Status, it.ClaimedBy, out, c.row.Name)
	}
}
