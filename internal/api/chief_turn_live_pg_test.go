package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
)

// heldGateway plays an agent runtime whose turns are HELD until the test
// releases them: entered is signalled when a turn starts, and the turn answers
// reply once release is closed. It records whether the turn's context was
// cancelled while it was held — the thing a closed socket must never do.
type heldGateway struct {
	entered chan string
	release chan struct{}
	reply   string

	mu        sync.Mutex
	cancelled bool
}

func newHeldGateway(reply string) *heldGateway {
	return &heldGateway{entered: make(chan string, 4), release: make(chan struct{}), reply: reply}
}

func (g *heldGateway) Chat(context.Context, agents.Agent, string, string, func(string)) (string, error) {
	return "", errors.New("heldGateway: the tool-less path is not expected here")
}

func (g *heldGateway) ChatWithTools(ctx context.Context, _ agents.Agent, _, _, text string, _ []agents.ToolDef, _ agents.ToolDispatch, emit agents.StreamEmit) (string, error) {
	g.entered <- text
	select {
	case <-g.release:
	case <-ctx.Done():
		g.mu.Lock()
		g.cancelled = true
		g.mu.Unlock()
		return "", ctx.Err()
	}
	if emit != nil {
		emit(agents.StreamEvent{Kind: "text", Text: g.reply})
	}
	return g.reply, nil
}

func (g *heldGateway) wasCancelled() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cancelled
}

// chiefWS opens agent name's chat socket as the signed-in operator.
func chiefWS(t *testing.T, ctx context.Context, f *connFixture, ts *httptest.Server, name string) *websocket.Conn {
	t.Helper()
	h := http.Header{}
	h.Set("Cookie", sessionCookieFor(f.srv))
	h.Set("Origin", ts.URL)
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/agents/"+name+"/ws", &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func wsSend(t *testing.T, ctx context.Context, c *websocket.Conn, text string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"type": "message", "text": text})
	if err := c.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}
}

// assistantRows is agent a's stored assistant rows' content.
func assistantRows(t *testing.T, pool *pgxpool.Pool, a agents.Agent) []string {
	t.Helper()
	rows, err := pool.Query(authz.System(context.Background()),
		`SELECT content FROM chat_messages WHERE agent_id = $1 AND role = 'assistant' ORDER BY id`, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// workingShown reports whether a rendered #chat-log draws "working…" SHOWING:
// the server's in-flight mark, and the display class the script itself uses.
func workingShown(t *testing.T, body string) bool {
	t.Helper()
	i := strings.Index(body, `id="chat-working"`)
	if i < 0 {
		t.Fatalf("no #chat-working in the render:\n%s", body)
	}
	tag := body[i:]
	tag = tag[:strings.Index(tag, ">")]
	marked := strings.Contains(tag, "data-turn-in-flight")
	shown := strings.Contains(tag, `class="flex `)
	hidden := strings.Contains(tag, `class="hidden `)
	if marked != shown || shown == hidden {
		t.Fatalf("#chat-working is incoherent (marked %v, flex %v, hidden %v): %s", marked, shown, hidden, tag)
	}
	return shown
}

// TestAReloadMidTurnDrawsWorkingFromTheServer (the operator's report: "we
// reloaded the page and Chief's working indicator disappeared"): while a chief
// turn sent over the chat socket is running, every render of the conversation a
// page could load — the agent page, the chief panel and the chat-log refetch —
// draws "working…" SHOWING; once the reply is stored they draw it hidden, with
// the reply. Control: the same renders before the turn draw it hidden.
func TestAReloadMidTurnDrawsWorkingFromTheServer(t *testing.T) {
	gw := newHeldGateway("the reply, stored after the reload")
	// A fixture with ONE chief, the operator's: the panel resolves the chief by
	// its display name, so a second one (attnFixture's member chief) would be
	// the panel's subject instead.
	f := newConnFixtureWith(t, func(ext *Extensions, _ *pgxpool.Pool) { ext.Gateway = gw })
	stamp := strconv.FormatInt(time.Now().UnixNano(), 36)
	chief, err := agents.NewPG(f.pool).Create(asUser(1), agents.Agent{Name: "livechief-" + stamp, Namespace: "devpod-livechief-" + stamp,
		DisplayName: "chief", Status: agents.StatusRunning, HooksToken: "live-chief-token-" + stamp})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM agents WHERE id = $1`, chief.ID)
	})
	renders := map[string]string{
		"chat-log": "/ui/agents/" + chief.Name + "/chat-log",
		"page":     "/agents/" + chief.Name,
		"panel":    "panel",
	}
	get := func(path string) string {
		t.Helper()
		if path == "panel" {
			// The panel's ONE renderer, for this chief (writeChiefPanelBodyFor):
			// the route resolves the chief by display name, and the shared test
			// database may hold another agent of that name.
			w := httptest.NewRecorder()
			f.srv.writeChiefPanelBodyFor(asUser(1), w, chief, "")
			return w.Body.String()
		}
		w := f.call(t, http.MethodGet, path, nil, f.operator())
		if w.Code == http.StatusSeeOther { // the page names its session
			w = f.call(t, http.MethodGet, w.Header().Get("Location"), nil, f.operator())
		}
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, w.Code, w.Body)
		}
		return w.Body.String()
	}
	for name, path := range renders {
		if workingShown(t, get(path)) {
			t.Fatalf("control: %s draws working before any turn", name)
		}
	}

	ts := httptest.NewServer(f.h)
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c := chiefWS(t, ctx, f, ts, chief.Name)
	defer c.CloseNow()
	wsSend(t, ctx, c, "how is the fleet doing")
	select {
	case <-gw.entered:
	case <-ctx.Done():
		t.Fatal("the turn never started")
	}
	for name, path := range renders {
		if !workingShown(t, get(path)) {
			t.Errorf("%s, opened mid-turn, does not draw working", name)
		}
	}
	close(gw.release)
	deadline := time.Now().Add(5 * time.Second)
	for len(assistantRows(t, f.pool, chief)) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	for name, path := range renders {
		body := get(path)
		if workingShown(t, body) {
			t.Errorf("%s still draws working after the reply was stored", name)
		}
		if !strings.Contains(body, gw.reply) {
			t.Errorf("%s does not draw the stored reply", name)
		}
	}
}

// TestAChiefTurnSurvivesTheSocketClosing (operator decision 1: "closing or
// reloading the page never cancels the chief's turn"): a REAL WebSocket close
// mid-turn — what a reload does to the page's socket — leaves the turn running;
// it is not cancelled, it completes when the runtime answers, and its reply is
// stored, so the reloaded page shows it.
//
// ⚠ AN INVARIANT GUARD, NOT A REGRESSION TEST: it passes on the code before this
// change too (the hijacked socket's context is not cancelled by a close). Its
// negative control is a turn tied to the socket's lifetime — run on
// c.CloseRead's context, it goes red with "closing the socket cancelled the turn".
func TestAChiefTurnSurvivesTheSocketClosing(t *testing.T) {
	gw := newHeldGateway("finished after the page went away")
	f := newAttnFixture(t, func(ext *Extensions, _ *pgxpool.Pool) { ext.Gateway = gw })
	ts := httptest.NewServer(f.h)
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c := chiefWS(t, ctx, f.connFixture, ts, f.op.Name)
	wsSend(t, ctx, c, "start the long one")
	select {
	case <-gw.entered:
	case <-ctx.Done():
		t.Fatal("the turn never started")
	}
	// The page goes away: a normal close handshake, then the TCP connection.
	if err := c.Close(websocket.StatusGoingAway, "reload"); err != nil && !strings.Contains(err.Error(), "close") {
		t.Logf("close: %v", err)
	}
	_ = c.CloseNow()
	// Give the server every chance to notice the socket is gone and to cancel.
	time.Sleep(300 * time.Millisecond)
	if gw.wasCancelled() {
		t.Fatal("closing the socket cancelled the turn")
	}
	close(gw.release)
	deadline := time.Now().Add(5 * time.Second)
	released := time.Now()
	for time.Now().Before(deadline) {
		for _, r := range assistantRows(t, f.pool, f.op) {
			if r == gw.reply {
				t.Logf("stored %s after the runtime answered", time.Since(released).Round(time.Millisecond))
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the turn's reply was never stored (cancelled: %v; rows %q)", gw.wasCancelled(), assistantRows(t, f.pool, f.op))
}
