package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
)

// markerGateway plays an agent runtime that either ACCEPTS a turn's text
// (agents.MarkTurnAccepted, as ccd's 103 does) or refuses it (409 busy, as ccd
// answers while its CLI is still running a turn it gave up on), and records,
// DURING the turn, the owner-turn marker the agent carried.
type markerGateway struct {
	pool   *pgxpool.Pool
	accept bool
	mu     sync.Mutex
	during map[string]agents.OwnerTurn // text → marker seen mid-turn
}

func (g *markerGateway) Chat(context.Context, agents.Agent, string, string, func(string)) (string, error) {
	return "", errors.New("markerGateway: the tool-less path is not expected here")
}

func (g *markerGateway) ChatWithTools(ctx context.Context, a agents.Agent, _, _, text string, _ []agents.ToolDef, _ agents.ToolDispatch, _ agents.StreamEmit) (string, error) {
	if g.accept {
		agents.MarkTurnAccepted(ctx)
	}
	turn, _ := agents.NewPG(g.pool).OwnerTurnInFlight(authz.System(context.Background()), a.ID)
	g.mu.Lock()
	if g.during == nil {
		g.during = map[string]agents.OwnerTurn{}
	}
	g.during[text] = turn
	g.mu.Unlock()
	if !g.accept {
		return "", &agents.RuntimeError{Status: http.StatusConflict, Type: "busy", Message: "the session is mid-turn"}
	}
	return "ok", nil
}

func (g *markerGateway) seen(t *testing.T, text string) agents.OwnerTurn {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		g.mu.Lock()
		turn, ok := g.during[text]
		g.mu.Unlock()
		if ok {
			return turn
		}
		if time.Now().After(deadline) {
			t.Fatalf("no turn ran for %q", text)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// markerAfter is agent a's marker once the turn has returned (waiting up to 2s
// for a background turn to finish and clear it).
func markerAfter(t *testing.T, pool *pgxpool.Pool, a agents.Agent) agents.OwnerTurn {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		turn, err := agents.NewPG(pool).OwnerTurnInFlight(authz.System(context.Background()), a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if turn.MessageID == 0 || time.Now().After(deadline) {
			return turn
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestTheOwnersChatDoorsMarkOnlyAnAcceptedOwnerTurn: a line the signed-in
// owner (user 1, who owns the agent) sends through the composer POST and
// through the chat socket marks the owner's turn in flight WHILE the runtime
// runs it — naming that line's own row — and the mark is gone once the turn
// returns. A line the runtime REFUSED (409 busy) is stored but never marks.
// Control: a forward through the same turn function, under the same signed-in
// principal, never marks.
func TestTheOwnersChatDoorsMarkOnlyAnAcceptedOwnerTurn(t *testing.T) {
	gw := &markerGateway{accept: true}
	f := newAttnFixture(t, func(ext *Extensions, pool *pgxpool.Pool) { gw.pool = pool; ext.Gateway = gw })
	rowOf := func(text string) int64 {
		var id int64
		if err := f.pool.QueryRow(authz.System(context.Background()),
			`SELECT id FROM chat_messages WHERE agent_id = $1 AND role = 'user' AND content = $2 ORDER BY id DESC LIMIT 1`,
			f.op.ID, text).Scan(&id); err != nil {
			t.Fatalf("the row for %q: %v", text, err)
		}
		return id
	}

	// The composer POST, accepted.
	const posted = "typed in the composer, dispatch it"
	rd, hdr := form(url.Values{"text": {posted}, "nonce": {"owner-turn-nonce-00001"}})
	if w := f.call(t, http.MethodPost, "/agents/"+f.op.Name+"/send", rd, merge(f.operator(), hdr)); w.Code != http.StatusNoContent {
		t.Fatalf("the send: %d %s", w.Code, w.Body)
	}
	if got := gw.seen(t, posted); got.By != 1 || got.MessageID != rowOf(posted) {
		t.Fatalf("during the composer's turn the marker is %+v; want the owner (1) and that line's row", got)
	}
	if got := markerAfter(t, f.pool, f.op); got.MessageID != 0 {
		t.Fatalf("the marker outlived the composer's turn: %+v", got)
	}

	// The chat socket, accepted.
	ts := httptest.NewServer(f.h)
	t.Cleanup(ts.Close)
	h := http.Header{}
	h.Set("Cookie", sessionCookieFor(f.srv))
	h.Set("Origin", ts.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/agents/"+f.op.Name+"/ws", &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	send := func(text string) {
		raw, _ := json.Marshal(map[string]string{"type": "message", "text": text})
		if err := c.Write(ctx, websocket.MessageText, raw); err != nil {
			t.Fatal(err)
		}
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]string
			_ = json.Unmarshal(data, &m)
			if m["type"] == "error" || m["type"] == "done" {
				return
			}
		}
	}
	const socketed = "typed in the chat, go ahead"
	send(socketed)
	if got := gw.seen(t, socketed); got.By != 1 || got.MessageID != rowOf(socketed) {
		t.Fatalf("during the socket's turn the marker is %+v; want the owner (1) and that line's row", got)
	}
	if got := markerAfter(t, f.pool, f.op); got.MessageID != 0 {
		t.Fatalf("the marker outlived the socket's turn: %+v", got)
	}

	// Refused by the runtime (finding 1): the line is stored, nothing marks.
	gw.accept = false
	const refused = "typed while the CLI was busy"
	send(refused)
	_ = rowOf(refused) // stored
	if got := gw.seen(t, refused); got.MessageID != 0 {
		t.Fatalf("a refused line marked the owner's turn: %+v", got)
	}
	gw.accept = true

	// Control: muster's own hand-in through the same turn function.
	const forwarded = "muster: your task changed outside muster"
	sess, err := agents.NewPG(f.pool).LatestOrCreateSession(asUser(1), f.op.ID, f.op.Name)
	if err != nil {
		t.Fatal(err)
	}
	if f.srv.taskForwardTurn == nil || !f.srv.turns.enter(f.op.ID) {
		t.Fatal("control: no forward turn, or the gate would not admit it")
	}
	f.srv.taskForwardTurn(asUser(1), f.op, sess, forwarded)
	if got := gw.seen(t, forwarded); got.MessageID != 0 {
		t.Fatalf("a forward marked an owner turn: %+v", got)
	}
}
