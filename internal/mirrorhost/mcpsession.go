package mirrorhost

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// MCP SESSIONS ON /mcp: HOW A CONNECTED SESSION LEARNS ITS TOOL LIST IS STALE
// (docs/mirroring.md, "The local MCP endpoint").
//
// With the tool watch on (ToolsRefresh > 0), /mcp is a STATEFUL streamable-HTTP
// server for a Claude Code caller of this host's tmux server:
//
//   - its `initialize` answer carries a fresh Mcp-Session-Id, and every later
//     request (POST, and the GET stream) must send it back;
//   - a request with NO id is answered 400 "Bad Request: Mcp-Session-Id header
//     is required", and one with an id this process does not know (it
//     restarted, or evicted it) 404 "Session not found" — the spec's answers,
//     and the ones Claude Code (measured in 2.1.289's bundled client,
//     isMcpSessionExpiredError) treats as an EXPIRED session on a tools/call:
//     it drops the connection, initializes again — reading the CURRENT
//     capabilities and tool list — and retries the call once. The 400 is what
//     recovers a session that connected before this endpoint issued ids: its
//     transport holds no id, so a 404 would not count for it, while the 400's
//     words do;
//   - what each session last LISTED (a hash of the tools, names and schemas)
//     is kept. When what /mcp serves now differs — the watch re-read it, a
//     reconnect to muster triggered a re-read (KickTools), another session
//     listed it — that session is sent notifications/tools/list_changed ONCE:
//     on its open GET stream, or, with no stream, ahead of the answer to its
//     next request (the POST answered as an event stream: the notification,
//     then the response).
//
// An opencode caller, an outsider, and everything with the watch off are
// untouched: no ids, no checks.

// MCPSessionIDHeader is the streamable-HTTP transport's session header.
const MCPSessionIDHeader = "Mcp-Session-Id"

// maxMCPSessions bounds the session table; past it the least recently seen is
// forgotten (its next request is a 404, and it initializes again).
const maxMCPSessions = 512

// The transport's refusals, word for word as the MCP TypeScript SDK's server
// writes them: Claude Code matches these words (400) and this status (404).
const (
	mcpSessionRequired = `{"jsonrpc":"2.0","error":{"code":-32000,"message":"Bad Request: Mcp-Session-Id header is required (muster-mirror issues one at initialize; reconnect to get one)"},"id":null}` + "\n"
	mcpSessionNotFound = `{"jsonrpc":"2.0","error":{"code":-32001,"message":"Session not found (muster-mirror restarted or forgot it; initialize again)"},"id":null}` + "\n"
)

type mcpSession struct {
	// listed is the hash of the tools this session last listed ("" = none
	// yet); notified the hash it was last told list_changed about.
	listed, notified string
	seen             time.Time
}

// sessionTable is /mcp's sessions.
type sessionTable struct {
	mu sync.Mutex
	m  map[string]*mcpSession
}

// create is a new session's id.
func (t *sessionTable) create() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[string]*mcpSession{}
	}
	for len(t.m) >= maxMCPSessions {
		oldest := ""
		for k, s := range t.m {
			if oldest == "" || s.seen.Before(t.m[oldest].seen) {
				oldest = k
			}
		}
		delete(t.m, oldest)
	}
	t.m[id] = &mcpSession{seen: time.Now()}
	return id
}

// touch reports whether id is a known session, marking it seen.
func (t *sessionTable) touch(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.m[id]
	if s == nil {
		return false
	}
	s.seen = time.Now()
	return true
}

// setListed records that session id was just served the tools hashing to h.
func (t *sessionTable) setListed(id, h string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s := t.m[id]; s != nil {
		s.listed = h
	}
}

// stale reports whether session id listed a set other than current and has
// not been told about current yet. A session that never listed is not stale:
// it has nothing to refresh.
func (t *sessionTable) stale(id, current string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.m[id]
	return s != nil && current != "" && s.listed != "" && s.listed != current && s.notified != current
}

func (t *sessionTable) markNotified(id, h string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s := t.m[id]; s != nil {
		s.notified = h
	}
}

// sessionsOn reports whether a request is held to an MCP session: a Claude
// Code caller (the guard has already sent an outsider elsewhere) with the
// watch on.
func (p *MCPProxy) sessionsOn(r *http.Request) bool {
	return p.listChangedOn() && !OpenCodeCaller(r)
}

// requireSession is the caller's session id, or — none, or one this process
// does not know — the transport's 400 or 404 written, and ok false.
func (p *MCPProxy) requireSession(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := strings.TrimSpace(r.Header.Get(MCPSessionIDHeader))
	switch {
	case id == "":
		writeSessionRefusal(w, http.StatusBadRequest, mcpSessionRequired)
		return "", false
	case !p.sessions.touch(id):
		writeSessionRefusal(w, http.StatusNotFound, mcpSessionNotFound)
		return "", false
	}
	return id, true
}

func writeSessionRefusal(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, body)
}

// currentToolsHash is the hash of what /mcp serves a Claude Code caller now:
// the newest stored plan's ("" before any read).
func (p *MCPProxy) currentToolsHash() string {
	p.state.mu.Lock()
	defer p.state.mu.Unlock()
	if p.state.plan == nil {
		return ""
	}
	return p.state.plan.hash
}

// acceptsEventStream reports whether a POST may be answered as an event
// stream (the transport's clients send both types).
func acceptsEventStream(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/event-stream")
}

// listChangedWriter answers a POST as an event stream carrying
// notifications/tools/list_changed FIRST, then the response — when the
// response is a 200 JSON answer; anything else is relayed untouched. A
// keep-alive (leading whitespace, muster's on a long call) becomes an SSE
// comment, so the connection stays open as before.
type listChangedWriter struct {
	w           http.ResponseWriter
	wroteHeader bool
	sse         bool
	buf         bytes.Buffer
}

func (l *listChangedWriter) Header() http.Header { return l.w.Header() }

func (l *listChangedWriter) WriteHeader(code int) {
	if l.wroteHeader {
		return
	}
	l.wroteHeader = true
	h := l.w.Header()
	if code == http.StatusOK && strings.HasPrefix(strings.TrimSpace(h.Get("Content-Type")), "application/json") {
		l.sse = true
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Del("Content-Length")
		l.w.WriteHeader(code)
		_, _ = io.WriteString(l.w, listChangedEvent)
		l.Flush()
		return
	}
	l.w.WriteHeader(code)
}

func (l *listChangedWriter) Write(b []byte) (int, error) {
	if !l.wroteHeader {
		if l.w.Header().Get("Content-Type") == "" {
			l.w.Header().Set("Content-Type", http.DetectContentType(b))
		}
		l.WriteHeader(http.StatusOK)
	}
	if !l.sse {
		return l.w.Write(b)
	}
	// Whitespace before the answer is a keep-alive: an SSE comment now (finish
	// trims it from the data).
	if len(bytes.TrimSpace(l.buf.Bytes())) == 0 && len(bytes.TrimLeft(b, " \t\r\n")) < len(b) {
		if _, err := io.WriteString(l.w, ": keep-alive\n\n"); err != nil {
			return 0, err
		}
	}
	return l.buf.Write(b)
}

func (l *listChangedWriter) Flush() {
	if f, ok := l.w.(http.Flusher); ok {
		f.Flush()
	}
}

// finish writes the buffered response as the stream's message event, and
// reports whether the notification was sent.
func (l *listChangedWriter) finish() bool {
	if !l.sse {
		return false
	}
	body := bytes.TrimSpace(l.buf.Bytes())
	if len(body) > 0 {
		var out bytes.Buffer
		out.WriteString("event: message\n")
		for _, line := range bytes.Split(body, []byte("\n")) {
			out.WriteString("data: ")
			out.Write(bytes.TrimRight(line, "\r"))
			out.WriteString("\n")
		}
		out.WriteString("\n")
		_, _ = l.w.Write(out.Bytes())
		l.Flush()
	}
	return true
}
