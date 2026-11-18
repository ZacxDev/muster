package hqtest

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// THE CHAT ROUTES (muster docs/officers.md "Chat sessions"): a chat's
// messages, and the officer's reply. A member turn (MemberSays) enqueues — or
// re-opens — the chat's ONE item, kind "chat", subject "chat:<chat_id>",
// payload {chat_id, seq}, priority high.

// ChatMessage is one fake chat message, as the route serves it.
type ChatMessage struct {
	Seq         int64     `json:"seq"`
	Role        string    `json:"role"`
	AuthorLogin string    `json:"author_login,omitempty"`
	AuthorName  string    `json:"author_name,omitempty"`
	Text        string    `json:"text"`
	At          time.Time `json:"at"`
	// InReplyTo is the member seq an officer reply answers.
	InReplyTo int64 `json:"in_reply_to,omitempty"`
}

type fakeChat struct {
	officer, name string
	msgs          []ChatMessage
	item          string
	closed        bool
}

// CloseChat closes chat id as HQ does: its live item ends done, and a reply
// to it is 409 chat_closed.
func (s *Server) CloseChat(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.chats[id]
	c.closed = true
	if it := s.items[c.item]; it != nil && (it.Status == "open" || it.Status == "claimed") {
		it.Status, it.Outcome, it.ClaimedBy, it.LeaseExpiresAt = "done", "no_action", "", nil
	}
}

// AddChat opens chat id of officer, named name, with no message yet.
func (s *Server) AddChat(officer, id, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.chats == nil {
		s.chats = map[string]*fakeChat{}
	}
	s.chats[id] = &fakeChat{officer: officer, name: name}
}

// MemberSays appends a member turn to chat id and enqueues (or re-opens) its
// item. It answers the turn's seq and the item's id.
func (s *Server) MemberSays(id, login, display, text string) (int64, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.chats[id]
	seq := int64(len(c.msgs) + 1)
	c.msgs = append(c.msgs, ChatMessage{Seq: seq, Role: "member", AuthorLogin: login, AuthorName: display, Text: text, At: s.Now()})
	payload, _ := json.Marshal(map[string]any{"chat_id": id, "seq": seq})
	if it, ok := s.items[c.item]; ok && (it.Status == "open" || it.Status == "claimed") {
		it.Payload = payload
		return seq, c.item
	}
	if it, ok := s.items[c.item]; ok {
		// The one-live-item rule: the chat's item is re-opened.
		it.Status, it.ClaimedBy, it.LeaseExpiresAt, it.Payload, it.UpdatedAt = "open", "", nil, payload, s.Now()
		return seq, c.item
	}
	s.nextID++
	item := strconv.Itoa(s.nextID)
	s.items[item] = &Item{ID: item, OfficerID: c.officer, Kind: "chat", SubjectKey: "chat:" + id, Status: "open", Priority: "high",
		Payload: payload, UpdatedAt: s.Now()}
	c.item = item
	return seq, item
}

// ChatMessages is chat id's messages, in seq order.
func (s *Server) ChatMessages(id string) []ChatMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ChatMessage(nil), s.chats[id].msgs...)
}

// chatRoute serves GET /officers/{id}/chats/{chat}/messages?after= and POST
// …/reply {text, in_reply_to}. Called with s.mu held.
func (s *Server) chatRoute(w http.ResponseWriter, officer, chat, verb, method, after string, body map[string]any, now time.Time) {
	c, ok := s.chats[chat]
	if !ok || c.officer != officer {
		s.write(w, http.StatusNotFound, map[string]any{"error": "no such chat", "code": "not_found"})
		return
	}
	switch {
	case verb == "messages" && method == http.MethodGet:
		// HQ's view: {chat, messages[seq > after], more, state}; more is true
		// when the page (ChatPageSize, 0 = all) left later messages out.
		n, _ := strconv.ParseInt(after, 10, 64)
		out := []ChatMessage{}
		more := false
		for _, m := range c.msgs {
			if m.Seq <= n {
				continue
			}
			if s.ChatPageSize > 0 && len(out) == s.ChatPageSize {
				more = true
				break
			}
			out = append(out, m)
		}
		state := "idle"
		if c.closed {
			state = "closed"
		}
		s.write(w, http.StatusOK, map[string]any{"chat": map[string]any{"id": chat, "name": c.name, "state": state}, "messages": out, "more": more, "state": state})
	case verb == "reply" && method == http.MethodPost:
		for k := range body {
			if k != "text" && k != "in_reply_to" {
				s.write(w, http.StatusBadRequest, map[string]any{"error": "unknown field " + k, "code": "invalid"})
				return
			}
		}
		if rf := s.replyRefusal; rf != nil && rf.n > 0 {
			rf.n--
			s.write(w, rf.status, map[string]any{"error": "refused by the fake", "code": rf.code})
			return
		}
		text, _ := body["text"].(string)
		if len(text) > 64<<10 {
			s.write(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "an officer reply is at most 64 KiB", "code": "too_large"})
			return
		}
		f, _ := body["in_reply_to"].(float64)
		inReplyTo := int64(f)
		if text == "" || inReplyTo < 1 || strings.ContainsRune(text, 0) {
			s.write(w, http.StatusBadRequest, map[string]any{"error": "text (no NUL) and in_reply_to are required", "code": "invalid"})
			return
		}
		it := s.items[c.item]
		for _, m := range c.msgs {
			if m.Role == "officer" && m.InReplyTo == inReplyTo {
				// Idempotent: the reply it holds, and — when the item is held
				// again — the item completed, as the first reply did.
				if it != nil && it.Status == "claimed" {
					it.Status, it.Outcome, it.ClaimedBy, it.LeaseExpiresAt, it.UpdatedAt = "done", "done", "", nil, now
				}
				s.write(w, http.StatusOK, map[string]any{"seq": m.Seq, "replayed": true})
				return
			}
		}
		if c.closed {
			s.write(w, http.StatusConflict, map[string]any{"error": "the chat is closed", "code": "chat_closed"})
			return
		}
		var latest int64
		for _, m := range c.msgs {
			if m.Role == "member" && m.Seq > latest {
				latest = m.Seq
			}
		}
		if inReplyTo != latest {
			s.write(w, http.StatusConflict, map[string]any{"error": "not the latest member turn", "code": "stale_turn"})
			return
		}
		if it == nil || it.Status != "claimed" || it.LeaseExpiresAt == nil || !it.LeaseExpiresAt.After(now) {
			s.write(w, http.StatusConflict, map[string]any{"error": "the chat's item is not claimed", "code": "not_claimed"})
			return
		}
		seq := int64(len(c.msgs) + 1)
		c.msgs = append(c.msgs, ChatMessage{Seq: seq, Role: "officer", Text: text, At: now, InReplyTo: inReplyTo})
		it.Status, it.Outcome, it.ClaimedBy, it.LeaseExpiresAt, it.UpdatedAt = "done", "done", "", nil, now
		s.write(w, http.StatusOK, map[string]any{"seq": seq})
	default:
		s.write(w, http.StatusNotFound, map[string]any{"error": "no such route"})
	}
}

// RefuseReplies makes the next n reply posts answer status with code, as a
// refusing HQ would (nothing written).
func (s *Server) RefuseReplies(status int, code string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replyRefusal = &chatRefusal{status: status, code: code, n: n}
}

type chatRefusal struct {
	status int
	code   string
	n      int
}

// SetCharterVersion sets the charter_version officer's brief answers (1 by
// default).
func (s *Server) SetCharterVersion(officer string, v int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.charterVersions == nil {
		s.charterVersions = map[string]int{}
	}
	s.charterVersions[officer] = v
}
