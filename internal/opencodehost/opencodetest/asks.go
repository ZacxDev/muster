package opencodetest

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// ASKS: the fake's permission requests and forms, driven by REAL event frames.
//
// testdata/asks-v2.0.32.sse is what a real `opencode serve` (v2.0.32+6ec5682,
// in a scratch home with no credentials) sent on GET /api/event while a
// session was given a question-tool form and a ruleset that asks, and each was
// answered, rejected or cancelled through the API — its location directory
// replaced with /home/user/proj, nothing else changed. Frames (FixtureFrames)
// hands those frames to a test, which may substitute the ids; EmitFrame sends
// one verbatim and keeps the pending lists in step with it, so the list and
// reply routes agree with the stream as the real server's do:
//
//   - GET /api/session/{id}/permission and /form: the pending ones only;
//   - POST /api/session/{id}/permission/{rid}/reply {"decision"}: 204 and a
//     permission.replied frame, 404 once settled;
//   - POST /api/session/{id}/form/{fid}/reply {"answer"}: 204 and a
//     form.replied frame, 409 once settled.

//go:embed testdata/asks-v2.0.32.sse
var fixtureSSE string

// The ids in the fixture frames.
const (
	// FixtureFormSession is the root session the two forms were raised in.
	FixtureFormSession = "ses_e5d55c179ffe6MEUqkUCZbHWA9"
	// FixtureQuestionsForm is a two-question form (a single-select with two
	// options, then a multiselect); FixtureProceedForm a one-question form
	// ("Proceed?": Yes, No).
	FixtureQuestionsForm = "frm_1a2aa3ec2001NmTTM10yDwGseq"
	FixtureProceedForm   = "frm_1a2aaa3d1001lEte0g1lyG1pFi"
	// FixturePermissionSession is the root session whose ruleset asks;
	// FixtureBashRequest its permission request (bash, "rm -rf build").
	FixturePermissionSession = "ses_e5d559cd5ffecrpBJHzHzu6wl9"
	FixtureBashRequest       = "per_1a2aa6345001HPvxtQP9FD941m"
	// FixtureChildSession is a child of FixturePermissionSession (a
	// subagent's); FixtureEditRequest its permission request (edit,
	// src/main.go).
	FixtureChildSession = "ses_e5d518e77ffen4uKUoa3YNdDXi"
	FixtureEditRequest  = "per_1a2ae71b1001G6gXQELgojLFKD"
)

// Frame is one fixture frame: its type and its JSON, as the server sent it.
type Frame struct {
	Type string
	JSON string
}

// FixtureFrames is every frame of the fixture, in the order it was sent.
func FixtureFrames() []Frame {
	var out []Frame
	sc := bufio.NewScanner(strings.NewReader(fixtureSSE))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var e struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(line), &e) == nil && e.Type != "" {
			out = append(out, Frame{Type: e.Type, JSON: line})
		}
	}
	return out
}

// FixtureFrame is the n-th (0-based) fixture frame of type typ; it panics
// when there is none (a test's own mistake).
func FixtureFrame(typ string, n int) string {
	for _, f := range FixtureFrames() {
		if f.Type == typ {
			if n == 0 {
				return f.JSON
			}
			n--
		}
	}
	panic("opencodetest: no fixture frame " + typ)
}

// Reply is one reply the fake received.
type Reply struct {
	// Path is the reply route's path; Body its JSON body.
	Path, Body string
}

// Replies is every reply received so far.
func (s *Server) Replies() []Reply {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Reply(nil), s.replies...)
}

// EmitFrame sends frame (an event's JSON, as the server sends it) verbatim to
// every open event stream, and keeps the pending lists in step: an asked or
// created frame adds its ask, a replied or cancelled one removes it.
func (s *Server) EmitFrame(frame string) {
	var e struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal([]byte(frame), &e)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.perms == nil {
		s.perms, s.forms = map[string][]json.RawMessage{}, map[string][]json.RawMessage{}
	}
	switch e.Type {
	case "permission.asked":
		var p struct {
			SessionID string `json:"sessionID"`
		}
		_ = json.Unmarshal(e.Data, &p)
		s.perms[p.SessionID] = append(s.perms[p.SessionID], e.Data)
	case "form.created":
		var d struct {
			Form json.RawMessage `json:"form"`
		}
		_ = json.Unmarshal(e.Data, &d)
		var f struct {
			SessionID string `json:"sessionID"`
		}
		_ = json.Unmarshal(d.Form, &f)
		s.forms[f.SessionID] = append(s.forms[f.SessionID], d.Form)
	case "permission.replied":
		var d struct {
			SessionID string `json:"sessionID"`
			RequestID string `json:"requestID"`
		}
		_ = json.Unmarshal(e.Data, &d)
		s.perms[d.SessionID] = dropID(s.perms[d.SessionID], d.RequestID)
	case "form.replied", "form.cancelled":
		var d struct {
			ID        string `json:"id"`
			SessionID string `json:"sessionID"`
		}
		_ = json.Unmarshal(e.Data, &d)
		s.forms[d.SessionID] = dropID(s.forms[d.SessionID], d.ID)
	}
	for ch := range s.subs {
		select {
		case ch <- frame:
		default:
		}
	}
}

// dropID is l without the entry whose "id" is id; ok reports one was there.
func dropID(l []json.RawMessage, id string) []json.RawMessage {
	out := l[:0:0]
	for _, raw := range l {
		var cur struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(raw, &cur)
		if cur.ID != id {
			out = append(out, raw)
		}
	}
	return out
}

// hasID reports whether l holds an entry whose "id" is id.
func hasID(l []json.RawMessage, id string) bool {
	return len(dropID(l, id)) != len(l)
}

// serveAsks answers the ask routes; false when path is not one.
func (s *Server) serveAsks(w http.ResponseWriter, r *http.Request, path string) bool {
	rest, ok := strings.CutPrefix(path, "/api/session/")
	if !ok {
		return false
	}
	parts := strings.Split(rest, "/")
	switch {
	case r.Method == http.MethodGet && len(parts) == 2 && (parts[1] == "permission" || parts[1] == "form"):
		s.mu.Lock()
		list := s.perms[parts[0]]
		if parts[1] == "form" {
			list = s.forms[parts[0]]
		}
		list = append([]json.RawMessage(nil), list...)
		s.mu.Unlock()
		writeJSON(w, map[string]any{"data": nonNil(list)})
		return true
	case r.Method == http.MethodPost && len(parts) == 4 && parts[3] == "reply" && (parts[1] == "permission" || parts[1] == "form"):
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		sid, kind, id := parts[0], parts[1], parts[2]
		s.mu.Lock()
		s.replies = append(s.replies, Reply{Path: path, Body: string(body)})
		pending := false
		if kind == "permission" {
			pending = hasID(s.perms[sid], id)
		} else {
			pending = hasID(s.forms[sid], id)
		}
		refuse := s.refuseReplies
		s.mu.Unlock()
		if refuse != 0 {
			w.WriteHeader(refuse)
			return true
		}
		if !pending {
			if kind == "permission" {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"_tag":"PermissionNotFoundError"}`))
			} else {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"_tag":"FormAlreadySettledError"}`))
			}
			return true
		}
		var frame []byte
		if kind == "permission" {
			var d struct {
				Decision string `json:"decision"`
			}
			_ = json.Unmarshal(body, &d)
			frame, _ = json.Marshal(map[string]any{"id": "evt_fixturereply", "type": "permission.replied", "location": map[string]any{"directory": "/srv/fixture"},
				"data": map[string]any{"sessionID": sid, "requestID": id, "reply": d.Decision}})
		} else {
			var d struct {
				Answer json.RawMessage `json:"answer"`
			}
			_ = json.Unmarshal(body, &d)
			frame, _ = json.Marshal(map[string]any{"id": "evt_fixturereply", "type": "form.replied", "location": map[string]any{"directory": "/srv/fixture"},
				"data": map[string]any{"id": id, "sessionID": sid, "answer": d.Answer}})
		}
		w.WriteHeader(http.StatusNoContent)
		s.EmitFrame(string(frame))
		return true
	}
	return false
}

// RefuseReplies makes every reply answer status (0: answer normally).
func (s *Server) RefuseReplies(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refuseReplies = status
}
