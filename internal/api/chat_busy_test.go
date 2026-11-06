package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
)

// ---------------------------------------------------------------------------
// A MESSAGE SENT WHILE ANOTHER TURN HOLDS THE AGENT'S SESSION (chat_busy.go).
//
// The reported case: the chief is just turned on and runs its kickoff turn in
// its default chat session; the owner's message lands on it, ccd refuses it with
// a typed 409 `busy`, and the chat showed "[error] responses HTTP 409 …".
//
// These drive the REAL agentgateway.Gateway against a fake ccd speaking ccd's
// wire (typed 409 body, SSE reply), through the real routes — so they cover the
// whole path from a door to the runtime and back, not a stubbed gateway.
// ---------------------------------------------------------------------------

// busyCCD plays ccd for one session: while busy it refuses every turn with its
// typed 409 (as cmd/ccd runTurn does when a turn holds the slot), and once
// freed it answers each turn with "answer to: <input>".
type busyCCD struct {
	mu       sync.Mutex
	busy     bool
	failType string // the refusal's error.type; "busy" unless a test sets it
	code     string
	refused  int
	answered []string
	// refusedCh is told of every refusal (buffered; never blocks the fake).
	refusedCh chan struct{}
}

func newBusyCCD() *busyCCD {
	return &busyCCD{busy: true, failType: "busy", refusedCh: make(chan struct{}, 1024)}
}

func (f *busyCCD) free() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.busy = false
}

func (f *busyCCD) counts() (refused int, answered []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refused, append([]string(nil), f.answered...)
}

func (f *busyCCD) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if r.URL.Path != "/v1/responses" {
		http.NotFound(w, r)
		return
	}
	var req struct {
		Input json.RawMessage `json:"input"`
	}
	_ = json.Unmarshal(body, &req)
	f.mu.Lock()
	if f.busy {
		f.refused++
		failType, code := f.failType, f.code
		f.mu.Unlock()
		select {
		case f.refusedCh <- struct{}{}:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		fmt.Fprintf(w, `{"error":{"type":%q,"code":%q,"message":"another turn is already being driven through this session"}}`, failType, code)
		return
	}
	in := inputText(req.Input)
	f.answered = append(f.answered, in)
	f.mu.Unlock()
	reply := "answer to: " + in
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, "event: response.output_text.delta\n")
	fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":%q}\n\n", reply)
	fmt.Fprint(w, "event: response.completed\n")
	fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\","+
		"\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":%q}]}]}}\n\n", reply)
}

// inputText is the last user text in a /v1/responses input (a string, or an
// array of message items).
func inputText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var items []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	_ = json.Unmarshal(raw, &items)
	out := ""
	for _, it := range items {
		if it.Role != "user" {
			continue
		}
		if json.Unmarshal(it.Content, &s) == nil {
			out = s
		}
	}
	return out
}

// tokenedAgents is sendAgents with a hooks token on every agent, which the real
// gateway needs to derive its bearer.
type tokenedAgents struct{ *sendAgents }

func (s tokenedAgents) GetByName(ctx context.Context, name string) (agents.Agent, error) {
	a, err := s.sendAgents.GetByName(ctx, name)
	a.HooksToken = "busy-test-hooks-" + name
	return a, err
}

// busyServer is sendServer over the real gateway pointed at f, with the busy
// wait bounded by wait (poll 10ms).
func busyServer(t *testing.T, f *busyCCD, wait time.Duration) (*Server, *httptest.Server, *sendAgents) {
	t.Helper()
	up := httptest.NewServer(f)
	t.Cleanup(up.Close)
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(up.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	gw, err := agentgateway.New(agentgateway.Config{Driver: fixedEndpoint{provision.Endpoint{Host: host, Port: port}},
		Runtime: agentgateway.HooksSHA256(), Model: "runtime-sentinel", Client: up.Client()})
	if err != nil {
		t.Fatal(err)
	}
	store := newSendAgents()
	s, ts := sendServer(t, store, gw)
	s.UseExtensions(Extensions{Notes: stubNotesStore{}, Agents: tokenedAgents{store}, SessionLiveness: stubLiveness{},
		Provisioner: newFakeTerminal(), Gateway: gw})
	s.chatBusyWait, s.chatBusyPoll = wait, 10*time.Millisecond
	return s, ts, store
}

func waitRefusals(t *testing.T, f *busyCCD, n int) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case <-f.refusedCh:
		case <-deadline:
			t.Fatalf("timed out waiting for refusal %d of %d", i+1, n)
		}
	}
}

// TestAMessagePostedWhileATurnHoldsTheSessionIsSentWhenItEnds is the reported
// bug through the composer's POST: the runtime refuses with 409 `busy` while
// another turn (the chief's kickoff) runs; once it ends the message is sent and
// answered, and no failure — no "HTTP 409" — is ever stored.
//
// 🔴 REGRESSION: on origin/main the first 409 ends the turn, and the stored
// line is "⚠ This message was not answered: responses HTTP 409: …".
func TestAMessagePostedWhileATurnHoldsTheSessionIsSentWhenItEnds(t *testing.T) {
	f := newBusyCCD()
	s, ts, store := busyServer(t, f, time.Minute)

	if resp := postAgent(t, s, ts, "keen-wren", url.Values{"text": {"hello chief"}, "session": {"41"}}, false); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("the post = %d, want 204", resp.StatusCode)
	}
	// Refused at least twice: it is waiting, not failing.
	waitRefusals(t, f, 2)
	if got := store.transcript(41); len(got) != 1 || got[0] != "user: hello chief" {
		t.Fatalf("while the session is busy the transcript is %q; want only the user's line (no failure stored)", got)
	}
	f.free() // the kickoff turn ends
	eventuallyAPI(t, "the queued message to be answered", func() bool { return len(store.transcript(41)) == 2 })
	got := store.transcript(41)
	if got[1] != "assistant: answer to: hello chief" {
		t.Fatalf("stored transcript = %q; want the reply to the queued message", got)
	}
	for _, line := range got {
		if strings.Contains(line, "409") || strings.Contains(line, AgentTurnFailedPrefix) {
			t.Fatalf("a failure was stored for a message that was delivered: %q", got)
		}
	}
	refused, answered := f.counts()
	if refused < 2 || len(answered) != 1 || answered[0] != "hello chief" {
		t.Fatalf("the runtime refused %d time(s) and answered %q; want ≥2 refusals and the message answered exactly once", refused, answered)
	}
}

// dialChat opens keen-wren's chat socket as the signed-in owner.
func dialChat(t *testing.T, s *Server, ts *httptest.Server) (*websocket.Conn, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	h := http.Header{}
	h.Set("Cookie", sessionCookieFor(s))
	h.Set("Origin", ts.URL)
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/agents/keen-wren/ws?session=41", &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return c, ctx
}

// readUntilEnd reads socket messages until "done" or "error", returning them all.
func readUntilEnd(t *testing.T, ctx context.Context, c *websocket.Conn) []map[string]any {
	t.Helper()
	var out []map[string]any
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("socket read: %v (got so far %v)", err, out)
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		if m["type"] == "heartbeat" || m["type"] == "ping" {
			continue
		}
		out = append(out, m)
		if m["type"] == "done" || m["type"] == "error" {
			return out
		}
	}
}

func sendChat(t *testing.T, ctx context.Context, c *websocket.Conn, text string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"type": "message", "text": text})
	if err := c.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}
}

// TestTheChatSocketSaysAMessageIsQueuedAndThenStreamsItsReply: the chat page's
// own door. While the runtime is busy the socket says, once, in words, that the
// message is queued; once free the reply streams and the turn ends "done" — no
// "error" message at all.
//
// 🔴 REGRESSION: on origin/main the socket's only answer is
// {"type":"error","content":"responses HTTP 409: …"}, which the page renders as
// "[error] responses HTTP 409 …".
func TestTheChatSocketSaysAMessageIsQueuedAndThenStreamsItsReply(t *testing.T) {
	f := newBusyCCD()
	s, ts, store := busyServer(t, f, time.Minute)
	c, ctx := dialChat(t, s, ts)

	sendChat(t, ctx, c, "are you up yet")
	waitRefusals(t, f, 3)
	f.free()
	msgs := readUntilEnd(t, ctx, c)

	var queued []string
	var deltas strings.Builder
	for _, m := range msgs {
		switch m["type"] {
		case "queued":
			queued = append(queued, fmt.Sprint(m["content"]))
		case "delta":
			deltas.WriteString(fmt.Sprint(m["content"]))
		case "error":
			t.Fatalf("the socket reported an error for a message that was delivered: %v", m)
		}
	}
	if len(queued) != 1 || queued[0] != ChatQueuedNotice {
		t.Fatalf("queued notices = %q; want exactly one, %q (told once, not per refusal)", queued, ChatQueuedNotice)
	}
	if last := msgs[len(msgs)-1]["type"]; last != "done" {
		t.Fatalf("the turn ended %q, want done: %v", last, msgs)
	}
	if deltas.String() != "answer to: are you up yet" {
		t.Fatalf("streamed reply = %q", deltas.String())
	}
	eventuallyAPI(t, "the reply stored", func() bool { return len(store.transcript(41)) == 2 })
}

// TestAMessageThatOutwaitsTheBusyBoundIsToldInWordsAndKeepsItsText: a session
// that stays busy past the bound gives up — and says so as a person would, never
// as "HTTP 409", and hands the typed text back (socket) / keeps it in the
// conversation (POST).
func TestAMessageThatOutwaitsTheBusyBoundIsToldInWordsAndKeepsItsText(t *testing.T) {
	f := newBusyCCD() // never freed
	s, ts, store := busyServer(t, f, 80*time.Millisecond)

	c, ctx := dialChat(t, s, ts)
	sendChat(t, ctx, c, "still there?")
	msgs := readUntilEnd(t, ctx, c)
	end := msgs[len(msgs)-1]
	if end["type"] != "error" || end["unsent"] != true || end["text"] != "still there?" {
		t.Fatalf("the socket's last message = %v; want an unsent error carrying the text", end)
	}
	content := fmt.Sprint(end["content"])
	if strings.Contains(content, "409") || strings.Contains(content, "HTTP") || !strings.Contains(content, "send it again") {
		t.Fatalf("the person was told %q; want words, not a status code", content)
	}

	if resp := postAgent(t, s, ts, "keen-wren", url.Values{"text": {"posted while busy"}, "session": {"41"}}, false); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("the post = %d", resp.StatusCode)
	}
	eventuallyAPI(t, "the give-up to be stored", func() bool {
		got := store.transcript(41)
		return len(got) == 3 && strings.HasPrefix(got[2], "assistant: ")
	})
	got := store.transcript(41)
	if got[1] != "user: posted while busy" {
		t.Fatalf("the posted message was not kept: %q", got)
	}
	if !strings.HasPrefix(got[2], "assistant: "+AgentTurnFailedPrefix+"It was still busy") || strings.Contains(got[2], "409") {
		t.Fatalf("stored give-up = %q; want the busy notice in words", got[2])
	}
}

// TestOnlyATypedBusyIsWaitedOn is the negative control: a 409 `rate_limited`
// (the CLI's usage-limit menu, a reset hours away) is NOT a turn in flight and
// fails at once, after ONE request. Without it the tests above pass over a wait
// that retries every 409.
func TestOnlyATypedBusyIsWaitedOn(t *testing.T) {
	f := newBusyCCD()
	f.failType = "rate_limited"
	s, ts, _ := busyServer(t, f, time.Minute)
	c, ctx := dialChat(t, s, ts)
	sendChat(t, ctx, c, "limit?")
	msgs := readUntilEnd(t, ctx, c)
	if end := msgs[len(msgs)-1]; end["type"] != "error" || end["unsent"] == true {
		t.Fatalf("a rate_limited refusal ended %v; want an ordinary error", end)
	}
	for _, m := range msgs {
		if m["type"] == "queued" {
			t.Fatalf("a rate_limited refusal was queued: %v", msgs)
		}
	}
	if refused, _ := f.counts(); refused != 1 {
		t.Fatalf("the runtime was asked %d time(s) for a rate_limited refusal, want 1", refused)
	}
}

// TestABusyForADialogSaysSo: ccd's `busy` with code awaiting_approval is a
// dialog in the terminal, not a turn — the queued notice names what it waits for.
func TestABusyForADialogSaysSo(t *testing.T) {
	f := newBusyCCD()
	f.code = agentgateway.CodeAwaitingApproval
	s, ts, _ := busyServer(t, f, time.Minute)
	c, ctx := dialChat(t, s, ts)
	sendChat(t, ctx, c, "after the dialog")
	waitRefusals(t, f, 2)
	f.free()
	msgs := readUntilEnd(t, ctx, c)
	if msgs[0]["type"] != "queued" || msgs[0]["content"] != ChatQueuedForDialogNotice {
		t.Fatalf("first socket message = %v; want the dialog notice", msgs[0])
	}
}
