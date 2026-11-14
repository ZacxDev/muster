package api

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/ui"
)

// chatStreamHTMLEvery is how often, at most, the chat socket re-renders a
// streamed reply's text as HTML (wsChatStream). A test sets it to 0 to render
// on every delta.
var chatStreamHTMLEvery = 100 * time.Millisecond

// wsChatStream is the server→browser half of one chat turn on the
// /agents/{name} socket: each agents.StreamEvent as the WS JSON message
// agentChatScript consumes.
//
// 🔴 THE REPLY'S MARKDOWN IS RENDERED HERE, BY THE SERVER'S ONE RENDERER. A
// "delta" carries its own text ("content") and, when rendered, "html": the
// WHOLE text of the reply's current bubble so far through ui.ChatMessageHTML
// — the function the stored transcript is drawn with. The page assigns it and
// keeps no markdown renderer of its own (agents_detail.go chatMessageBody says
// why there used to be two and why there is one).
//
// ⚡ RENDERED AT MOST EVERY chatStreamHTMLEvery, NOT ON EVERY DELTA. "html" is
// the whole bubble, and a runtime streams roughly a token per delta, so html
// on every delta is quadratic in the reply's length on the wire (a 10 KB reply
// in ~2,500 deltas would send tens of megabytes). A delta inside the window
// carries only its text — the page shows it as plain text after the last html
// — and a timer renders what the window held when it ends, so a reply that
// pauses mid-stream is still drawn rendered within the window.
//
// A bubble ends where the page starts a new one: at a thinking, tool_call or
// tool_result event (agentChatScript sets its text bubble to null there). Its
// owed html is sent first, so the page's bubble is final before the next
// part; end does the same for the turn's last bubble, before "done"/"error".
//
// All writes go through mu, the timer's included, so the socket's messages
// keep their order.
type wsChatStream struct {
	write func(v any)
	every time.Duration
	// mentions links the reply's mentions as the stored transcript will
	// (ui.ChatMessageHTMLWith); built once per turn, nil for none.
	mentions *ui.MentionIndex

	mu    sync.Mutex
	text  strings.Builder // the current bubble's text
	sent  time.Time       // when its html was last sent
	owed  bool            // text arrived since then
	timer *time.Timer
}

func newWSChatStream(write func(v any)) *wsChatStream {
	return &wsChatStream{write: write, every: chatStreamHTMLEvery}
}

// delta adds s to the current bubble.
func (w *wsChatStream) delta(s string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.text.WriteString(s)
	if wait := w.every - time.Since(w.sent); wait > 0 {
		w.write(map[string]any{"type": "delta", "content": s})
		w.owed = true
		if w.timer == nil {
			w.timer = time.AfterFunc(wait, w.flush)
		}
		return
	}
	w.sendHTMLLocked(s)
}

// sendHTMLLocked sends content with the bubble's whole text rendered.
func (w *wsChatStream) sendHTMLLocked(content string) {
	w.write(map[string]any{"type": "delta", "content": content, "html": ui.ChatMessageHTMLWith("assistant", w.text.String(), w.mentions)})
	w.sent, w.owed = time.Now(), false
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
}

// flush sends the bubble's owed html, if any (the timer's call).
func (w *wsChatStream) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.flushLocked()
}

func (w *wsChatStream) flushLocked() {
	if w.owed {
		w.sendHTMLLocked("")
	}
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
}

// endBubbleLocked finishes the current bubble: its owed html, then a fresh one.
func (w *wsChatStream) endBubbleLocked() {
	w.flushLocked()
	w.text.Reset()
	w.sent = time.Time{}
}

// event writes a non-text message, ending the current bubble first when the
// page will start a new one after it.
func (w *wsChatStream) event(endsBubble bool, v any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if endsBubble {
		w.endBubbleLocked()
	} else {
		w.flushLocked()
	}
	w.write(v)
}

// end finishes the turn's last bubble; the caller writes "done" or "error"
// after it.
func (w *wsChatStream) end() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.endBubbleLocked()
}

// emit translates one StreamEvent.
func (w *wsChatStream) emit(ev agents.StreamEvent) {
	switch ev.Kind {
	case "text":
		w.delta(ev.Text)
	case "thinking":
		w.event(true, map[string]any{"type": "thinking", "content": ev.Text})
	case "tool_call":
		w.event(true, map[string]any{"type": "tool_call", "id": ev.ToolID, "name": ev.ToolName, "args": ev.ToolArgs,
			"target": cctranscript.ToolTarget(ev.ToolName, ev.ToolArgs)})
	case "tool_result":
		w.event(true, map[string]any{"type": "tool_result", "id": ev.ToolID, "name": ev.ToolName, "ok": ev.ToolOK, "output": ev.ToolOutput})
	case "queued":
		// The message waits for the session's turn in flight (chat_busy.go):
		// the page says so in place of "working".
		w.event(false, map[string]any{"type": "queued", "content": ev.Text})
	case "accepted":
		// The runtime took a message that was queued: "working" again.
		w.event(false, map[string]any{"type": "accepted"})
	}
}

// wsSentEcho is the socket's answer to a message it read from the page: the
// message's html, drawn as its stored bubble will be, which replaces the
// plain text the page showed it with the moment it was sent.
func wsSentEcho(text string) map[string]any {
	return map[string]any{"type": "sent", "html": ui.ChatMessageHTML("user", text)}
}

// wsWriter writes v to c as JSON, ignoring a failure (a closed socket: the
// turn runs on regardless, handleAgentWS).
func wsWriter(ctx context.Context, c *websocket.Conn) func(v any) {
	return func(v any) { _ = wsWriteJSON(ctx, c, v) }
}
