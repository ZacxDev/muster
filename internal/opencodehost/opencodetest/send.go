package opencodetest

import (
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// SENDING: the fake's prompt, session create, session read and command list,
// shaped from what a REAL `opencode serve` (v2.0.32+6ec5682, in a scratch home
// with no credentials) answered: testdata/prompt-v2.0.32.json holds its
// bodies (create, getsession, prompt1, prompt1-again — the same id posted
// again with DIFFERENT text, answered with the FIRST text — prompt2, the 404
// and 409 bodies, commands, messages), and testdata/prompt-v2.0.32.sse the
// event frames it sent meanwhile; location directories replaced with
// /srv/fixture. The fake follows them:
//
//   - POST /api/session {id, location}: 200 {data: Session.Info}, the id kept;
//   - GET /api/session/{id}: 200 {data}, or 404 SessionNotFoundError;
//   - POST /api/session/{id}/prompt {id, text, delivery}: 200 {data:
//     Session.Inbox.User}; the same id on the same session answers the record
//     stored FIRST and adds nothing; on another session 409; an unknown
//     session 404 — each body as the real server's. An admitted prompt
//     becomes the session's user message, announced by session.inbox.enqueued
//     and session.inbox.delivered frames of the real shape;
//   - GET /api/command: the real server's built-ins (init, review) for a
//     request naming a directory, none for one that names none, as measured.

//go:embed testdata/prompt-v2.0.32.json
var promptCapture []byte

// Captured is one body of testdata/prompt-v2.0.32.json, as the real server
// sent it (create, getsession, prompt1, prompt1-again, prompt2, prompt-404,
// prompt-409, commands, messages).
func Captured(name string) json.RawMessage {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(promptCapture, &all); err != nil {
		panic(err)
	}
	b, ok := all[name]
	if !ok {
		panic("no captured body " + name)
	}
	return b
}

// Prompt is one prompt the fake admitted.
type Prompt struct {
	Session, ID, Text, Delivery string
}

// Prompts is every prompt admitted, in order (a repeat of an id is not one).
func (s *Server) Prompts() []Prompt {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Prompt(nil), s.prompts...)
}

// PromptPosts is how many prompt POSTs arrived, repeats and refusals too.
func (s *Server) PromptPosts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.promptPosts
}

// DelayPromptAnswers makes every prompt POST admit the prompt at once and
// answer only after d: a send that LANDED but whose answer is late, as a
// muster-mirror that timed out sees it. 0 answers at once.
func (s *Server) DelayPromptAnswers(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.promptDelay = d
}

// FailCommands makes GET /api/command answer 500 (on).
func (s *Server) FailCommands(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failCommands = on
}

// SetVersion sets what GET /api/info names as the server's version.
func (s *Server) SetVersion(v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version = v
}

// SetCommands replaces the slash commands GET /api/command lists (for a
// request naming a directory).
func (s *Server) SetCommands(cmds []map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = cmds
}

// sessionKnown reports whether sid is a session; s.mu is held.
func (s *Server) sessionKnownLocked(sid string) (json.RawMessage, bool) {
	for _, raw := range s.sessions {
		var cur struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(raw, &cur)
		if cur.ID == sid {
			return raw, true
		}
	}
	return nil, false
}

// serveSend answers the send routes; false for any other request.
func (s *Server) serveSend(w http.ResponseWriter, r *http.Request, path string) bool {
	switch {
	case r.Method == http.MethodPost && path == "/api/session":
		s.createSession(w, r)
	case r.Method == http.MethodGet && path == "/api/command":
		s.mu.Lock()
		cmds, fail := s.commands, s.failCommands
		s.mu.Unlock()
		if fail {
			http.Error(w, `{"_tag":"UnknownError"}`, http.StatusInternalServerError)
			return true
		}
		dir := r.URL.Query().Get("location[directory]")
		if dir == "" {
			cmds = nil
		}
		writeJSON(w, map[string]any{"location": map[string]any{"directory": dir}, "data": nonNilCmds(cmds)})
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/session/") && strings.HasSuffix(path, "/prompt"):
		s.prompt(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "/api/session/"), "/prompt"))
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/session/") && !strings.Contains(strings.TrimPrefix(path, "/api/session/"), "/") &&
		strings.TrimPrefix(path, "/api/session/") != "active":
		sid := strings.TrimPrefix(path, "/api/session/")
		s.mu.Lock()
		raw, ok := s.sessionKnownLocked(sid)
		s.mu.Unlock()
		if !ok {
			notFound(w, sid)
			return true
		}
		writeJSON(w, map[string]any{"data": raw})
	default:
		return false
	}
	return true
}

func nonNilCmds(c []map[string]string) []map[string]string {
	if c == nil {
		return []map[string]string{}
	}
	return c
}

// notFound is the real server's 404 for an unknown session.
func notFound(w http.ResponseWriter, sid string) {
	var body map[string]any
	_ = json.Unmarshal(Captured("prompt-404"), &body)
	body["sessionID"] = sid
	body["message"] = "Session not found: " + sid
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(body)
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID       string `json:"id"`
		Location struct {
			Directory string `json:"directory"`
		} `json:"location"`
	}
	b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err := json.Unmarshal(b, &in); err != nil || !strings.HasPrefix(in.ID, "ses") || !strings.HasPrefix(in.Location.Directory, "/") {
		http.Error(w, `{"_tag":"InvalidRequestError"}`, http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	_, exists := s.sessionKnownLocked(in.ID)
	s.mu.Unlock()
	if exists {
		http.Error(w, `{"_tag":"ConflictError"}`, http.StatusConflict)
		return
	}
	m := map[string]any{"id": in.ID, "projectID": "prj_fixture", "cost": 0,
		"tokens":   map[string]any{"input": 0, "output": 0, "reasoning": 0, "cache": map[string]any{"read": 0, "write": 0}},
		"time":     map[string]any{"created": s.now().UnixMilli(), "updated": s.now().UnixMilli()},
		"location": map[string]any{"directory": in.Location.Directory}}
	raw, _ := json.Marshal(m)
	s.mu.Lock()
	s.sessions = append(s.sessions, raw)
	s.mu.Unlock()
	s.Emit(in.ID, "session.created", map[string]any{})
	writeJSON(w, map[string]any{"data": json.RawMessage(raw)})
}

func (s *Server) prompt(w http.ResponseWriter, r *http.Request, sid string) {
	var in struct {
		ID       string `json:"id"`
		Text     string `json:"text"`
		Delivery string `json:"delivery"`
	}
	b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	s.mu.Lock()
	s.promptPosts++
	delay := s.promptDelay
	s.mu.Unlock()
	if err := json.Unmarshal(b, &in); err != nil || in.Text == "" || (in.ID != "" && !strings.HasPrefix(in.ID, "msg_")) {
		http.Error(w, `{"_tag":"InvalidRequestError"}`, http.StatusBadRequest)
		return
	}
	if in.Delivery == "" {
		in.Delivery = "steer"
	}
	s.mu.Lock()
	if _, ok := s.sessionKnownLocked(sid); !ok {
		s.mu.Unlock()
		notFound(w, sid)
		return
	}
	for _, p := range s.prompts {
		if p.ID != in.ID {
			continue
		}
		s.mu.Unlock()
		if p.Session != sid {
			var body map[string]any
			_ = json.Unmarshal(Captured("prompt-409"), &body)
			body["message"] = "Prompt message ID conflicts with an existing durable record: " + in.ID
			body["resource"] = in.ID
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(body)
			return
		}
		// The record stored FIRST, whatever this post carries (measured).
		writeJSON(w, map[string]any{"data": inboxUser(p, s.now())})
		return
	}
	p := Prompt{Session: sid, ID: in.ID, Text: in.Text, Delivery: in.Delivery}
	s.prompts = append(s.prompts, p)
	s.mu.Unlock()
	s.Emit(sid, "session.inbox.enqueued", map[string]any{"inboxID": in.ID,
		"item": map[string]any{"type": "user", "payload": map[string]any{"text": in.Text}, "delivery": in.Delivery}})
	s.PutMessage(sid, map[string]any{"id": in.ID, "type": "user", "text": in.Text, "time": map[string]any{"created": s.now().UnixMilli()}})
	s.Emit(sid, "session.inbox.delivered", map[string]any{"inboxID": in.ID})
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	writeJSON(w, map[string]any{"data": inboxUser(p, s.now())})
}

// inboxUser is Session.Inbox.User for p, as the real server shapes it.
func inboxUser(p Prompt, now time.Time) map[string]any {
	return map[string]any{"id": p.ID, "sessionID": p.Session, "time": map[string]any{"created": now.UnixMilli()},
		"type": "user", "payload": map[string]any{"text": p.Text}, "delivery": p.Delivery}
}
