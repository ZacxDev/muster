package opencodetest

import (
	_ "embed"
	"encoding/json"
	"fmt"
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

// promptSSE is the event stream the same real server sent meanwhile: the
// fake's session-create and prompt routes emit ITS frames (realFrame), so a
// test of what the host does with them runs on the shape opencode sends.
//
//go:embed testdata/prompt-v2.0.32.sse
var promptSSE string

// PromptFrame is the n-th (0-based) captured frame of type typ from
// testdata/prompt-v2.0.32.sse, verbatim; it panics when there is none.
func PromptFrame(typ string, n int) string { return frameOf(promptSSE, typ, n) }

// realFrame is captured frame typ (the first of its type) rewritten for this
// server's session sid: data.sessionID and durable.aggregateID become sid, the
// location directory dir, each of set's keys is set in data, and the frame's
// id and time are this server's. Everything else is the real frame's.
func (s *Server) realFrame(typ, sid, dir string, set map[string]any) string {
	var f map[string]any
	if err := json.Unmarshal([]byte(PromptFrame(typ, 0)), &f); err != nil {
		panic(err)
	}
	data, _ := f["data"].(map[string]any)
	data["sessionID"] = sid
	for k, v := range set {
		data[k] = v
	}
	if loc, ok := data["location"].(map[string]any); ok && dir != "" {
		loc["directory"] = dir
	}
	if loc, ok := f["location"].(map[string]any); ok && dir != "" {
		loc["directory"] = dir
	}
	if d, ok := f["durable"].(map[string]any); ok {
		d["aggregateID"] = sid
	}
	s.mu.Lock()
	s.seq++
	f["id"] = fmt.Sprintf("evt_fixture%06d", s.seq)
	f["created"] = s.now().UnixMilli()
	s.mu.Unlock()
	b, _ := json.Marshal(f)
	return string(b)
}

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

// DelayFirstPromptAnswer is DelayPromptAnswers for the NEXT prompt POST only:
// its prompt is admitted at once and its answer comes after d; later posts
// answer at once (a lost answer whose retry is answered).
func (s *Server) DelayFirstPromptAnswer(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.promptDelayOnce = d
}

// HangPromptAnswers is DelayPromptAnswers for EVERY prompt POST, a repeat of an
// id included (DelayPromptAnswers answers a repeat at once): a server that
// admits and never answers in time.
func (s *Server) HangPromptAnswers(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.promptHang = d
}

// RefusePrompts makes every prompt POST answer 400, admitting nothing (on).
func (s *Server) RefusePrompts(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refusePrompts = on
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

// THE HOST SPAWN'S ROUTES (docs/design/chief-dispatch.md 3.6), shaped from
// the same real server (v2.0.32+6ec5682, with an OpenRouter credential):
// testdata/spawn-v2.0.32.json holds its bodies — a create carrying model
// {providerID, id} answered with the session naming that model (and the same
// for an id no provider lists: the server does NOT check it), an OpenRouter
// entry of GET /api/model, and DELETE /api/session/{id}'s 204 then 404.
//
//   - POST /api/session records each session's model (SessionModel), and
//     counts creates (Creates);
//   - GET /api/model answers the list a test sets (SetModels), none by default;
//   - DELETE /api/session/{id}: 204 and the session is gone, else 404.
//
// Every create, prompt and delete is also recorded in order (Log), so a test
// can say which came first.

//go:embed testdata/spawn-v2.0.32.json
var spawnCapture []byte

// SpawnCaptured is one body of testdata/spawn-v2.0.32.json (create-model,
// create-unknown-model, model-openrouter, delete-404).
func SpawnCaptured(name string) json.RawMessage {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(spawnCapture, &all); err != nil {
		panic(err)
	}
	b, ok := all[name]
	if !ok {
		panic("no captured body " + name)
	}
	return b
}

// Model is a model the fake lists (GET /api/model); Disabled lists it with
// "enabled": false. Context, when above 0, is its limit.context (the
// context readout's window); NoLimit lists it with no limit at all. Neither:
// the captured entry's own limit.
type Model struct {
	ProviderID, ID string
	Disabled       bool
	Context        int64
	NoLimit        bool
}

// SetModels sets what GET /api/model lists, each entry shaped as the real
// server's OpenRouter entry (SpawnCaptured("model-openrouter")).
func (s *Server) SetModels(ms ...Model) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.models = append([]Model(nil), ms...)
}

// SessionModel is the model session sid was created with ({providerID, id}),
// nil for none.
func (s *Server) SessionModel(sid string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionModels[sid]
}

// Creates is how many sessions POST /api/session created.
func (s *Server) Creates() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.creates
}

// LogEntry is one create, prompt or delete, in the order the fake saw them.
type LogEntry struct {
	Op, Session string
	At          time.Time
}

// Log is every create, prompt (admitted or not) and delete, in order.
func (s *Server) Log() []LogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]LogEntry(nil), s.log...)
}

func (s *Server) logOp(op, sid string) {
	s.mu.Lock()
	s.log = append(s.log, LogEntry{Op: op, Session: sid, At: time.Now()})
	s.mu.Unlock()
}

func (s *Server) listModels(w http.ResponseWriter) {
	var tmpl map[string]any
	_ = json.Unmarshal(SpawnCaptured("model-openrouter"), &tmpl)
	s.mu.Lock()
	ms := append([]Model(nil), s.models...)
	s.mu.Unlock()
	out := []map[string]any{}
	for _, m := range ms {
		e := map[string]any{}
		for k, v := range tmpl {
			e[k] = v
		}
		e["id"], e["modelID"], e["providerID"], e["name"], e["enabled"] = m.ID, m.ID, m.ProviderID, m.ID, !m.Disabled
		switch {
		case m.NoLimit:
			delete(e, "limit")
		case m.Context > 0:
			e["limit"] = map[string]any{"context": m.Context, "output": 65536}
		}
		out = append(out, e)
	}
	writeJSON(w, map[string]any{"location": map[string]any{"directory": "/srv/fixture"}, "data": out})
}

func (s *Server) deleteSession(w http.ResponseWriter, sid string) {
	s.logOp("delete", sid)
	s.mu.Lock()
	kept := s.sessions[:0]
	found := false
	for _, raw := range s.sessions {
		var cur struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(raw, &cur)
		if cur.ID == sid {
			found = true
			continue
		}
		kept = append(kept, raw)
	}
	s.sessions = kept
	s.mu.Unlock()
	if !found {
		notFound(w, sid)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// serveSend answers the send routes; false for any other request.
func (s *Server) serveSend(w http.ResponseWriter, r *http.Request, path string) bool {
	switch {
	case r.Method == http.MethodPost && path == "/api/session":
		s.createSession(w, r)
	case r.Method == http.MethodGet && path == "/api/model":
		s.listModels(w)
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/api/session/") && !strings.Contains(strings.TrimPrefix(path, "/api/session/"), "/"):
		s.deleteSession(w, strings.TrimPrefix(path, "/api/session/"))
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
		Model map[string]string `json:"model"`
	}
	b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err := json.Unmarshal(b, &in); err == nil {
		s.logOp("create", in.ID)
	}
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
	if in.Model != nil {
		// As the real server: the model named, with its default variant.
		m["model"] = map[string]any{"id": in.Model["id"], "providerID": in.Model["providerID"], "variant": "default"}
	}
	raw, _ := json.Marshal(m)
	s.mu.Lock()
	s.sessions = append(s.sessions, raw)
	s.creates++
	if in.Model != nil {
		if s.sessionModels == nil {
			s.sessionModels = map[string]map[string]string{}
		}
		s.sessionModels[in.ID] = in.Model
	}
	s.mu.Unlock()
	// The REAL server's session.created frame (testdata/prompt-v2.0.32.sse),
	// which names the session as data.sessionID and nothing about its status.
	s.EmitFrame(s.realFrame("session.created", in.ID, in.Location.Directory, nil))
	writeJSON(w, map[string]any{"data": json.RawMessage(raw)})
}

func (s *Server) prompt(w http.ResponseWriter, r *http.Request, sid string) {
	var in struct {
		ID       string `json:"id"`
		Text     string `json:"text"`
		Delivery string `json:"delivery"`
	}
	b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	s.logOp("prompt", sid)
	s.mu.Lock()
	s.promptPosts++
	delay := s.promptDelay
	if s.promptDelayOnce > 0 {
		delay, s.promptDelayOnce = s.promptDelayOnce, 0
	}
	refuse := s.refusePrompts
	if s.promptHang > 0 {
		delay = s.promptHang
	}
	s.mu.Unlock()
	if err := json.Unmarshal(b, &in); refuse || err != nil || in.Text == "" || (in.ID != "" && !strings.HasPrefix(in.ID, "msg_")) {
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
		if s.hangOf() > 0 {
			select {
			case <-time.After(s.hangOf()):
			case <-r.Context().Done():
				return
			}
		}
		writeJSON(w, map[string]any{"data": inboxUser(p, s.now())})
		return
	}
	p := Prompt{Session: sid, ID: in.ID, Text: in.Text, Delivery: in.Delivery}
	s.prompts = append(s.prompts, p)
	s.mu.Unlock()
	// The REAL server's admission frames, rewritten for this prompt.
	s.EmitFrame(s.realFrame("session.inbox.enqueued", sid, "", map[string]any{"inboxID": in.ID,
		"item": map[string]any{"type": "user", "payload": map[string]any{"text": in.Text}, "delivery": in.Delivery}}))
	s.PutMessage(sid, map[string]any{"id": in.ID, "type": "user", "text": in.Text, "time": map[string]any{"created": s.now().UnixMilli()}})
	s.EmitFrame(s.realFrame("session.inbox.delivered", sid, "", map[string]any{"inboxID": in.ID}))
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

func (s *Server) hangOf() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.promptHang
}
