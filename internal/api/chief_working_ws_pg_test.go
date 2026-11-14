package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/sse"
)

// heldAnswerAttention is the attention store with its in-session answer HELD:
// AnswerInSession — the step the chat socket takes after it echoes a message
// and before the turn reaches the runtime — waits until release is closed. It
// widens the window between "the page drew working… on send" and "the turn
// runs" from a few milliseconds to as long as the test wants, so a refetch can
// be made to land inside it every time.
type heldAnswerAttention struct {
	attention.Store
	entered chan struct{}
	release chan struct{}
}

func (h heldAnswerAttention) AnswerInSession(ctx context.Context, asker attention.Asker, message string, queueID int64) ([]attention.Ask, error) {
	select {
	case h.entered <- struct{}{}:
	default:
	}
	<-h.release
	return h.Store.AnswerInSession(ctx, asker, message, queueID)
}

// TestARefetchRightAfterASocketSendDrawsWorking (the operator's report: in a
// fresh chief chat, "ping" drew "working…", and the #chat-log refetch that
// followed it re-rendered the log with the row HIDDEN while the turn ran): from
// the moment the chat socket has read a message — its "sent" echo is the page's
// proof — every render of that conversation draws "working…" SHOWING, until the
// turn ends.
//
// The window is held open deterministically: the in-session answer step, which
// the socket runs after the echo and before the turn, waits for the test. On
// origin/main the session's count began only inside chatTurnWhenFree, after that
// step, so the refetch inside the window drew the row hidden (red); it is counted
// from the read now (green). Control: before the send it draws hidden.
func TestARefetchRightAfterASocketSendDrawsWorking(t *testing.T) {
	gw := newHeldGateway("pong")
	held := heldAnswerAttention{entered: make(chan struct{}, 1), release: make(chan struct{})}
	f := newConnFixtureWith(t, func(ext *Extensions, pool *pgxpool.Pool) {
		ext.Gateway = gw
		held.Store = attention.NewPG(pool)
		ext.Attention = held
	})
	stamp := strconv.FormatInt(time.Now().UnixNano(), 36)
	chief, err := agents.NewPG(f.pool).Create(asUser(1), agents.Agent{Name: "wschief-" + stamp, Namespace: "devpod-wschief-" + stamp,
		DisplayName: "chief", Status: agents.StatusRunning, HooksToken: "ws-chief-token-" + stamp})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM agents WHERE id = $1`, chief.ID)
	})
	// A FRESH chat, as the operator's was: the page names it, the socket dials it.
	sess, err := agents.NewPG(f.pool).CreateSession(asUser(1), chief.ID, chief.Name)
	if err != nil {
		t.Fatal(err)
	}
	logPath := "/ui/agents/" + chief.Name + "/chat-log?session=" + strconv.FormatInt(sess.ID, 10)
	refetch := func() bool {
		t.Helper()
		w := f.call(t, http.MethodGet, logPath, nil, f.operator())
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", logPath, w.Code, w.Body)
		}
		return workingShown(t, w.Body.String())
	}
	if refetch() {
		t.Fatal("control: the fresh chat draws working before any send")
	}

	ts := httptest.NewServer(f.h)
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c := chiefWSSession(t, ctx, f, ts, chief.Name, sess.ID)
	defer c.CloseNow()
	wsSend(t, ctx, c, "ping")
	// The echo: the socket has read the message.
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var echo struct{ Type string }
	if json.Unmarshal(data, &echo) != nil || echo.Type != "sent" {
		t.Fatalf("first frame is not the sent echo: %s", data)
	}
	select {
	case <-held.entered:
	case <-ctx.Done():
		t.Fatal("the socket never reached the in-session answer step")
	}
	if !refetch() {
		t.Error("a refetch right after the socket read the message draws working HIDDEN while the turn is about to run")
	}
	close(held.release)
	select {
	case <-gw.entered:
	case <-ctx.Done():
		t.Fatal("the turn never started")
	}
	if !refetch() {
		t.Error("a refetch while the turn runs draws working hidden")
	}
	close(gw.release)
	deadline := time.Now().Add(5 * time.Second)
	for len(assistantRows(t, f.pool, chief)) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	for time.Now().Before(deadline) && refetch() {
		time.Sleep(20 * time.Millisecond)
	}
	if refetch() {
		t.Error("the reply is stored and the conversation still draws working")
	}
}

// chiefWSSession opens agent name's chat socket on session id, as the page does.
func chiefWSSession(t *testing.T, ctx context.Context, f *connFixture, ts *httptest.Server, name string, id int64) *websocket.Conn {
	t.Helper()
	h := http.Header{}
	h.Set("Cookie", sessionCookieFor(f.srv))
	h.Set("Origin", ts.URL)
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/agents/"+name+"/ws?session="+strconv.FormatInt(id, 10),
		&websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestATurnStartedElsewhereDrawsWorkingOnAnOpenChat: `muster chief ask` (the
// machine message door, gatewayTurn) runs a turn in the chief's primary chat;
// while it runs, a render of that chat — what an open page's chat.turn refetch
// draws — shows "working…", and once it ends it does not. On origin/main the
// machine door did not count the session at all (red); it counts through
// countSessionTurn now, which also tells open pages (EventChatTurn).
func TestATurnStartedElsewhereDrawsWorkingOnAnOpenChat(t *testing.T) {
	gw := newHeldGateway("the answer to chief ask")
	f := newConnFixtureWith(t, func(ext *Extensions, _ *pgxpool.Pool) { ext.Gateway = gw })
	stamp := strconv.FormatInt(time.Now().UnixNano(), 36)
	chief, err := agents.NewPG(f.pool).Create(asUser(1), agents.Agent{Name: "askchief-" + stamp, Namespace: "devpod-askchief-" + stamp,
		DisplayName: "chief", Status: agents.StatusRunning, HooksToken: "ask-chief-token-" + stamp})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM agents WHERE id = $1`, chief.ID)
	})
	primary, err := f.srv.primarySession(asUser(1), chief)
	if err != nil {
		t.Fatal(err)
	}
	logPath := "/ui/agents/" + chief.Name + "/chat-log?session=" + strconv.FormatInt(primary.ID, 10)
	shown := func() bool {
		t.Helper()
		w := f.call(t, http.MethodGet, logPath, nil, f.operator())
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: %d", logPath, w.Code)
		}
		return workingShown(t, w.Body.String())
	}
	if shown() {
		t.Fatal("control: working before any turn")
	}
	done := make(chan int, 1)
	go func() {
		r := httptest.NewRequest(http.MethodPost, "/api/agents/"+chief.Name+"/messages", strings.NewReader(`{"message":"how is it going"}`))
		r.Header.Set("Authorization", "Bearer "+connTestHookToken)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		f.h.ServeHTTP(w, r)
		done <- w.Code
	}()
	select {
	case <-gw.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the machine door's turn never started")
	}
	if !shown() {
		t.Error("the primary chat, open while `muster chief ask` runs its turn, draws working hidden")
	}
	close(gw.release)
	if code := <-done; code != http.StatusOK {
		t.Fatalf("chief ask answered %d", code)
	}
	if shown() {
		t.Error("the turn ended and the chat still draws working")
	}
}

// TestTheMachineDoorSaysChatReplyOnceTheReplyIsStored: POST
// /api/agents/{name}/messages stores the turn's reply AFTER the turn's own end
// (chat.turn) is broadcast, so an open chat that refetched on chat.turn drew no
// reply. The door now broadcasts chat.reply to the AGENT'S OWNER once the reply
// is stored — and a render on that event shows the reply. Red before the fix:
// the door published no chat.reply at all (an open chief page hid the race only
// because its in-pane context chip re-dispatched every chat event, refetching
// the transcript a second time).
//
// 🔴 THE CALLER IS NOT THE OWNER, SO THE SCOPE IS VISIBLE: the agent is owned
// by a SECOND user, the handler runs under a system-scoped principal (see the
// call below for why not the route), and both that user and the operator
// (user 1) are subscribed. The owner must get chat.reply and the operator must
// not. Mutation-checked: a CALLER-scoped broadcast (broadcast(dctx)) fails
// "never said chat.reply to the agent's OWNER"; one that also tells user 1
// fails the operator check.
func TestTheMachineDoorSaysChatReplyOnceTheReplyIsStored(t *testing.T) {
	gw := newHeldGateway("the stored answer")
	f := newConnFixtureWith(t, func(ext *Extensions, _ *pgxpool.Pool) { ext.Gateway = gw })
	bus := sse.New(512)
	f.srv.bus = bus
	sys := authz.System(context.Background())
	stamp := strconv.FormatInt(time.Now().UnixNano(), 36)
	var owner int64
	if err := f.pool.QueryRow(sys, `INSERT INTO users (role, display_name) VALUES ('member', $1) RETURNING id`,
		"replyowner-"+stamp).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(sys, `DELETE FROM agents WHERE owner_id = $1`, owner)
		_, _ = f.pool.Exec(sys, `DELETE FROM users WHERE id = $1`, owner)
	})
	_, stream := bus.Subscribe(owner)
	_, operatorStream := bus.Subscribe(1)
	chief, err := agents.NewPG(f.pool).Create(asUser(owner), agents.Agent{Name: "replychief-" + stamp, Namespace: "devpod-replychief-" + stamp,
		DisplayName: "chief", Status: agents.StatusRunning, HooksToken: "reply-chief-token-" + stamp})
	if err != nil {
		t.Fatal(err)
	}
	if chief.OwnerID != owner {
		t.Fatalf("control: the agent is owned by %d, want the second user %d", chief.OwnerID, owner)
	}
	primary, err := f.srv.primarySession(asUser(owner), chief)
	if err != nil {
		t.Fatal(err)
	}
	logPath := "/ui/agents/" + chief.Name + "/chat-log?session=" + strconv.FormatInt(primary.ID, 10)
	close(gw.release)
	// ⚠ THE HANDLER, NOT THE ROUTE: through the route the caller cannot differ
	// from the owner — the shared machine token acts as user 1, a personal token
	// as its own user, and the agent is read under that principal (row-level
	// security), so another user's agent is a 404 (measured: "no agent by that
	// name"). A SYSTEM-scoped principal is the one caller that reaches another
	// user's agent, and under it the caller-scoped broadcast's owner is 0
	// ("nobody", requestOwner) — which is what makes the owner binding visible.
	r := httptest.NewRequest(http.MethodPost, "/api/agents/"+chief.Name+"/messages", strings.NewReader(`{"message":"is it stored"}`))
	r.Header.Set("Content-Type", "application/json")
	r.SetPathValue("name", chief.Name)
	r = r.WithContext(authz.System(r.Context()))
	w := httptest.NewRecorder()
	f.srv.handleAPIAgentSendMessage(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("chief ask answered %d: %s", w.Code, w.Body.String())
	}
	want, _ := json.Marshal(map[string]any{"id": chief.Name})
	isReply := func(ev sse.Event) bool { return ev.Name == EventChatReply && ev.Data == string(want) }
	deadline := time.After(5 * time.Second)
	for got := false; !got; {
		select {
		case ev := <-stream:
			if !isReply(ev) {
				continue
			}
			got = true
			// What an open chat of the owner's reads on that event: the reply is
			// already stored.
			var n int
			if err := f.pool.QueryRow(sys, `SELECT count(*) FROM chat_messages WHERE session_id = $1 AND role = 'assistant' AND content = $2`,
				primary.ID, "the stored answer").Scan(&n); err != nil || n != 1 {
				t.Fatalf("on chat.reply the reply is not stored (%d rows, %v); %s would draw without it", n, err, logPath)
			}
		case <-deadline:
			t.Fatal("the machine door stored its reply and never said chat.reply to the agent's OWNER")
		}
	}
	// The operator (user 1) is not the owner: nothing of this agent's
	// chat.reply reaches them. Everything was published before the owner's
	// event arrived, so a drain sees it all.
	for {
		select {
		case ev := <-operatorStream:
			if isReply(ev) {
				t.Fatal("the operator (user 1, not the owner) was told chat.reply for another user's agent")
			}
		default:
			return
		}
	}
}
