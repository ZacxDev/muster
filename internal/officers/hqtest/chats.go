package hqtest

import (
	"encoding/json"
	"net/http"
	"strconv"
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
		n, _ := strconv.ParseInt(after, 10, 64)
		out := []ChatMessage{}
		for _, m := range c.msgs {
			if m.Seq > n {
				out = append(out, m)
			}
		}
		s.write(w, http.StatusOK, map[string]any{"chat": map[string]any{"id": chat, "name": c.name}, "messages": out})
	case verb == "reply" && method == http.MethodPost:
		for k := range body {
			if k != "text" && k != "in_reply_to" {
				s.write(w, http.StatusBadRequest, map[string]any{"error": "unknown field " + k, "code": "invalid"})
				return
			}
		}
		text, _ := body["text"].(string)
		f, _ := body["in_reply_to"].(float64)
		inReplyTo := int64(f)
		if text == "" || inReplyTo < 1 {
			s.write(w, http.StatusBadRequest, map[string]any{"error": "text and in_reply_to are required", "code": "invalid"})
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
				s.write(w, http.StatusOK, map[string]any{"seq": m.Seq})
				return
			}
		}
		if it == nil || it.Status != "claimed" || it.LeaseExpiresAt == nil || !it.LeaseExpiresAt.After(now) {
			s.write(w, http.StatusConflict, map[string]any{"error": "the chat's item is not claimed", "code": "lease_lost"})
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
