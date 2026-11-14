package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// THE CHAT SOCKET RENDERS THE REPLY'S MARKDOWN (chat_stream.go). The page has
// no renderer of its own any more (ui.chatMessageBody), so what these pin is
// what the owner sees while a reply streams.
// ---------------------------------------------------------------------------

// pAnchor is markdown.go's mdAnchor attributes.
const pAnchor = `" target="_blank" rel="noopener" class="text-accent underline decoration-accent/40 underline-offset-2 hover:text-fg">`

// withHTMLEvery sets the stream's render interval for one test.
func withHTMLEvery(t *testing.T, d time.Duration) {
	t.Helper()
	old := chatStreamHTMLEvery
	chatStreamHTMLEvery = d
	t.Cleanup(func() { chatStreamHTMLEvery = old })
}

// recorder is a wsChatStream writer that keeps every message as JSON.
type recorder struct {
	mu  sync.Mutex
	got []string
}

func (r *recorder) write(v any) {
	b, _ := json.Marshal(v)
	r.mu.Lock()
	r.got = append(r.got, string(b))
	r.mu.Unlock()
}

func (r *recorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.got...)
}

func jsonOf(v map[string]any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func sameMsgs(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("socket messages\n got:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// TestTheChatStreamRendersEachBubbleWithTheGoRenderer, rendering on every
// delta: each delta carries the bubble's WHOLE text so far as html, and a
// tool call starts a new bubble whose html holds only its own text.
//
// 🔴 REGRESSION: red at 1b029ed9 — the socket sent {"type":"delta","content":…}
// with no html, and the page rendered it with its own JS copy.
func TestTheChatStreamRendersEachBubbleWithTheGoRenderer(t *testing.T) {
	withHTMLEvery(t, 0)
	r := &recorder{}
	st := newWSChatStream(r.write)
	st.emit(agents.StreamEvent{Kind: "text", Text: "see **bo"})
	st.emit(agents.StreamEvent{Kind: "text", Text: "ld** and _x_"})
	st.emit(agents.StreamEvent{Kind: "tool_call", ToolID: "c1", ToolName: "Bash", ToolArgs: `{"command":"ls"}`})
	st.emit(agents.StreamEvent{Kind: "text", Text: "file_path ok"})
	st.end()
	sameMsgs(t, r.all(), []string{
		jsonOf(map[string]any{"type": "delta", "content": "see **bo", "html": `<p class="my-1">see **bo</p>`}),
		jsonOf(map[string]any{"type": "delta", "content": "ld** and _x_", "html": `<p class="my-1">see <strong>bold</strong> and <em>x</em></p>`}),
		jsonOf(map[string]any{"type": "tool_call", "id": "c1", "name": "Bash", "args": `{"command":"ls"}`, "target": "ls"}),
		jsonOf(map[string]any{"type": "delta", "content": "file_path ok", "html": `<p class="my-1">file_path ok</p>`}),
	})
}

// TestTheChatStreamLinksMentions: a reply's mentions are links WHILE it
// streams (ui/mentions.go), as the stored transcript draws them; a name the
// index does not hold stays text.
func TestTheChatStreamLinksMentions(t *testing.T) {
	withHTMLEvery(t, 0)
	r := &recorder{}
	st := newWSChatStream(r.write)
	st.mentions = ui.NewMentionIndex([]string{"fix-login"}, nil, nil)
	st.emit(agents.StreamEvent{Kind: "text", Text: "ask fix-login, not fix-logout"})
	st.end()
	sameMsgs(t, r.all(), []string{
		jsonOf(map[string]any{"type": "delta", "content": "ask fix-login, not fix-logout", "html": `<p class="my-1">ask <a href="/agents/fix-login" data-mention="agent" hx-boost="false" class="text-accent underline decoration-accent/40 underline-offset-2 hover:text-fg">fix-login</a>, not fix-logout</p>`}),
	})
}

// TestTheChatStreamRendersAtMostOncePerWindow: inside the window a delta
// carries only its text; the bubble's owed html is sent when it ends (here, at
// a thinking event and at the turn's end), so nothing streamed stays unrendered.
func TestTheChatStreamRendersAtMostOncePerWindow(t *testing.T) {
	withHTMLEvery(t, time.Hour)
	r := &recorder{}
	st := newWSChatStream(r.write)
	st.emit(agents.StreamEvent{Kind: "text", Text: "a "})
	st.emit(agents.StreamEvent{Kind: "text", Text: "**b**"})
	st.emit(agents.StreamEvent{Kind: "text", Text: " c"})
	st.emit(agents.StreamEvent{Kind: "thinking", Text: "hm"})
	st.emit(agents.StreamEvent{Kind: "text", Text: "_d_"})
	st.emit(agents.StreamEvent{Kind: "text", Text: " e"})
	st.end()
	st.end() // a second end owes nothing
	sameMsgs(t, r.all(), []string{
		jsonOf(map[string]any{"type": "delta", "content": "a ", "html": `<p class="my-1">a </p>`}),
		jsonOf(map[string]any{"type": "delta", "content": "**b**"}),
		jsonOf(map[string]any{"type": "delta", "content": " c"}),
		jsonOf(map[string]any{"type": "delta", "content": "", "html": `<p class="my-1">a <strong>b</strong> c</p>`}),
		jsonOf(map[string]any{"type": "thinking", "content": "hm"}),
		jsonOf(map[string]any{"type": "delta", "content": "_d_", "html": `<p class="my-1"><em>d</em></p>`}),
		jsonOf(map[string]any{"type": "delta", "content": " e"}),
		jsonOf(map[string]any{"type": "delta", "content": "", "html": `<p class="my-1"><em>d</em> e</p>`}),
	})
}

// TestQueuedAndAcceptedDoNotEndTheBubble: "queued" and "accepted" are words
// about the MESSAGE, not parts of the reply — the page keeps drawing the same
// text bubble across them (agentChatScript sets text = null only at thinking,
// tool_call and tool_result). So the server's bubble goes on too: the html
// after them is the whole bubble, its text before them included. They flush
// owed html first, so nothing streamed waits on them.
//
// #538 round 1 F3: before this test, `queued` ending a bubble survived every test.
func TestQueuedAndAcceptedDoNotEndTheBubble(t *testing.T) {
	withHTMLEvery(t, time.Hour)
	r := &recorder{}
	st := newWSChatStream(r.write)
	st.emit(agents.StreamEvent{Kind: "text", Text: "a"})
	st.emit(agents.StreamEvent{Kind: "text", Text: " _b_"})
	st.emit(agents.StreamEvent{Kind: "queued", Text: "waiting"})
	st.emit(agents.StreamEvent{Kind: "text", Text: " c"})
	st.emit(agents.StreamEvent{Kind: "accepted"})
	st.emit(agents.StreamEvent{Kind: "text", Text: " d"})
	st.end()
	sameMsgs(t, r.all(), []string{
		jsonOf(map[string]any{"type": "delta", "content": "a", "html": `<p class="my-1">a</p>`}),
		jsonOf(map[string]any{"type": "delta", "content": " _b_"}),
		jsonOf(map[string]any{"type": "delta", "content": "", "html": `<p class="my-1">a <em>b</em></p>`}),
		jsonOf(map[string]any{"type": "queued", "content": "waiting"}),
		jsonOf(map[string]any{"type": "delta", "content": " c"}),
		jsonOf(map[string]any{"type": "delta", "content": "", "html": `<p class="my-1">a <em>b</em> c</p>`}),
		jsonOf(map[string]any{"type": "accepted"}),
		jsonOf(map[string]any{"type": "delta", "content": " d"}),
		jsonOf(map[string]any{"type": "delta", "content": "", "html": `<p class="my-1">a <em>b</em> c d</p>`}),
	})
}

// TestTheChatStreamRendersAPausedReplyWhenItsWindowEnds: a reply that stops
// mid-bubble (the model is thinking, a tool is slow to start) is drawn
// rendered when the window ends, not left as plain text until the next event.
func TestTheChatStreamRendersAPausedReplyWhenItsWindowEnds(t *testing.T) {
	withHTMLEvery(t, 40*time.Millisecond)
	r := &recorder{}
	st := newWSChatStream(r.write)
	st.emit(agents.StreamEvent{Kind: "text", Text: "x"})
	st.emit(agents.StreamEvent{Kind: "text", Text: " **y**"})
	want := jsonOf(map[string]any{"type": "delta", "content": "", "html": `<p class="my-1">x <strong>y</strong></p>`})
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := r.all()
		if len(got) == 3 && got[2] == want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the paused bubble was never rendered; messages: %q", got)
		}
		time.Sleep(5 * time.Millisecond)
	}
	st.end()
	if got := r.all(); len(got) != 3 {
		t.Fatalf("end after the timer's render sent more: %q", got)
	}
}

// deltaCCD plays a runtime whose every reply streams as deltas, in order.
type deltaCCD struct{ deltas []string }

func (f deltaCCD) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.ReadAll(r.Body)
	if r.URL.Path != "/v1/responses" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for _, d := range f.deltas {
		fmt.Fprint(w, "event: response.output_text.delta\n")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":%q}\n\n", d)
		w.(http.Flusher).Flush()
	}
	full := strings.Join(f.deltas, "")
	fmt.Fprint(w, "event: response.completed\n")
	fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\","+
		"\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":%q}]}]}}\n\n", full)
}

// deltaServer is busyServer's wiring over f (the real gateway, the real routes).
func deltaServer(t *testing.T, f http.Handler) (*Server, *httptest.Server, *sendAgents) {
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
	s.chatBusyWait, s.chatBusyPoll = time.Minute, 10*time.Millisecond
	return s, ts, store
}

// TestTheChatSocketSendsRenderedHTMLForASentMessageAndItsReply drives the real
// /agents/{name}/ws through the real gateway: the owner's message is echoed
// back as its rendered html ("sent"), and the streamed reply's last bubble
// is drawn by the Go renderer before "done" — on every case #492 measured the
// deleted JS copy getting wrong, and with author markup escaped.
//
// 🔴 REGRESSION: red at 1b029ed9 — no "sent" message, and no delta carries html.
func TestTheChatSocketSendsRenderedHTMLForASentMessageAndItsReply(t *testing.T) {
	withHTMLEvery(t, time.Hour) // one html at the first delta, the rest at the end
	f := deltaCCD{deltas: []string{"see **bo", "ld** and https://example.com/foo_", " then x_ and café_x_.txt <script>"}}
	s, ts, store := deltaServer(t, f)
	c, ctx := dialChat(t, s, ts)
	sendChat(t, ctx, c, "please **look** at <b>x</b>")
	msgs := readUntilEnd(t, ctx, c)

	var got []string
	for _, m := range msgs {
		if m["type"] == "accepted" {
			continue // the runtime took it (no 103 from this fake: said once, before the reply)
		}
		got = append(got, jsonOf(m))
	}
	sameMsgs(t, got, []string{
		jsonOf(map[string]any{"type": "sent", "html": `<p class="my-1">please <strong>look</strong> at &lt;b&gt;x&lt;/b&gt;</p>`}),
		jsonOf(map[string]any{"type": "delta", "content": "see **bo", "html": `<p class="my-1">see **bo</p>`}),
		jsonOf(map[string]any{"type": "delta", "content": "ld** and https://example.com/foo_"}),
		jsonOf(map[string]any{"type": "delta", "content": " then x_ and café_x_.txt <script>"}),
		jsonOf(map[string]any{"type": "delta", "content": "", "html": `<p class="my-1">see <strong>bold</strong> and <a href="https://example.com/foo_` + pAnchor +
			"https://example.com/foo_</a> then x_ and café_x_.txt &lt;script&gt;</p>"}),
		jsonOf(map[string]any{"type": "done"}),
	})
	eventuallyAPI(t, "the reply stored", func() bool { return len(store.transcript(41)) == 2 })
}

// TestEveryMessageTheSocketReadsIsEchoed: the page queues one bubble per
// message it sends and fills them from the echoes in order, so the socket must
// echo every message it reads — including one the turn gate refuses.
func TestEveryMessageTheSocketReadsIsEchoed(t *testing.T) {
	f := deltaCCD{deltas: []string{"ok"}}
	s, ts, _ := deltaServer(t, f)
	if ok, why := s.turns.closeFor(7); !ok {
		t.Fatalf("closeFor: %s", why)
	}
	defer s.turns.reopen(7)
	c, ctx := dialChat(t, s, ts)
	sendChat(t, ctx, c, "_refused_")
	msgs := readUntilEnd(t, ctx, c)
	if len(msgs) != 2 || jsonOf(msgs[0]) != jsonOf(map[string]any{"type": "sent", "html": `<p class="my-1"><em>refused</em></p>`}) || msgs[1]["type"] != "error" {
		t.Fatalf("a refused message's socket messages = %v; want its echo, then the error", msgs)
	}
}
