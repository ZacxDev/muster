package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
)

// typedByOf is chat_messages.typed_by (0 for NULL) of agent a's user-role row
// whose content is text, waiting up to 2s for it (a POST's turn runs in the
// background).
func typedByOf(t *testing.T, pool *pgxpool.Pool, a agents.Agent, text string) (int64, bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var by *int64
		err := pool.QueryRow(authz.System(context.Background()),
			`SELECT typed_by FROM chat_messages WHERE agent_id = $1 AND role = 'user' AND content = $2 ORDER BY id DESC LIMIT 1`,
			a.ID, text).Scan(&by)
		if err == nil {
			if by == nil {
				return 0, true
			}
			return *by, true
		}
		if time.Now().After(deadline) {
			return 0, false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestTheOwnersChatDoorsStampTheTyper: a line the signed-in operator (user 1)
// sends through the agent's composer POST and through its chat socket is stored
// typed_by 1 — the provenance task_dispatch's gate release reads. Control: a
// line handed in by muster itself (a task_forward's turn, the same turn
// function the composer runs) is stored with typed_by NULL.
func TestTheOwnersChatDoorsStampTheTyper(t *testing.T) {
	f := newAttnFixture(t, func(ext *Extensions, _ *pgxpool.Pool) { ext.Gateway = &turnGateway{} })

	// The composer POST.
	const posted = "typed in the composer, dispatch it"
	rd, hdr := form(url.Values{"text": {posted}, "nonce": {"typed-by-nonce-0000001"}})
	if w := f.call(t, http.MethodPost, "/agents/"+f.op.Name+"/send", rd, merge(f.operator(), hdr)); w.Code != http.StatusNoContent {
		t.Fatalf("the send: %d %s", w.Code, w.Body)
	}
	if by, ok := typedByOf(t, f.pool, f.op, posted); !ok || by != 1 {
		t.Fatalf("the composer's line: typed_by %d (stored %v); want 1, the signed-in typer", by, ok)
	}

	// The chat socket.
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
	const socketed = "typed in the chat, go ahead"
	raw, _ := json.Marshal(map[string]string{"type": "message", "text": socketed})
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
		if m["type"] == "error" {
			t.Fatalf("the socket turn: %v", m)
		}
		if m["type"] == "done" {
			break
		}
	}
	if by, ok := typedByOf(t, f.pool, f.op, socketed); !ok || by != 1 {
		t.Fatalf("the socket's line: typed_by %d (stored %v); want 1", by, ok)
	}

	// Control: muster's own hand-in through the same turn function (a forward's
	// turn, s.taskForwardTurn) is not typed by anybody.
	const forwarded = "muster: your task changed outside muster"
	sess, err := agents.NewPG(f.pool).LatestOrCreateSession(asUser(1), f.op.ID, f.op.Name)
	if err != nil {
		t.Fatal(err)
	}
	if f.srv.taskForwardTurn == nil || !f.srv.turns.enter(f.op.ID) {
		t.Fatal("control: no forward turn, or the gate would not admit it")
	}
	f.srv.taskForwardTurn(asUser(1), f.op, sess, forwarded)
	if by, ok := typedByOf(t, f.pool, f.op, forwarded); !ok || by != 0 {
		t.Fatalf("a forward's line: typed_by %d (stored %v); want NULL", by, ok)
	}
}
