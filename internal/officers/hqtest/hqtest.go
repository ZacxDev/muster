// Package hqtest is an in-process fake of the HQ officers API (the officers
// contract, v1), for tests in internal/officers and internal/api. It is NOT a
// reference implementation: it implements the endpoints muster consumes, with
// the claim/lease rules the contract states, and records every request so a
// test can assert which token reached which officer.
package hqtest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Item is a fake item.
type Item struct {
	ID             string     `json:"id"`
	OfficerID      string     `json:"officer_id"`
	Kind           string     `json:"kind"`
	SubjectKey     string     `json:"subject_key"`
	Status         string     `json:"status"`
	ClaimedBy      string     `json:"claimed_by"`
	LeaseExpiresAt *time.Time `json:"lease_expires_at"`
	Attempts       int        `json:"attempts"`
	Outcome        string     `json:"outcome,omitempty"`
	Summary        string     `json:"summary,omitempty"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// Request is one recorded request.
type Request struct {
	Method, Path, Query, Auth string
	Body                      map[string]any
}

// Server is the fake.
type Server struct {
	*httptest.Server

	mu       sync.Mutex
	tokens   map[string]string // token -> officer id
	items    map[string]*Item
	nextID   int
	down     bool
	reqs     []Request
	Now      func() time.Time
	Charters map[string]string
}

// New starts a fake. tokens maps each officer token to the officer it acts
// for — the server KNOWS the caller's officer from the token, as the contract
// says HQ does.
func New(tokens map[string]string) *Server {
	s := &Server{tokens: tokens, items: map[string]*Item{}, Now: time.Now, Charters: map[string]string{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// URL is the base muster is configured with (`…/api/v1`).
func (s *Server) BaseURL() string { return s.Server.URL + "/api/v1" }

// SetDown makes every request answer 503 (true) or serve normally (false).
func (s *Server) SetDown(down bool) { s.mu.Lock(); s.down = down; s.mu.Unlock() }

// AddItem adds an open item for officer and returns its id.
func (s *Server) AddItem(officer, kind, subject string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	id := strconv.Itoa(s.nextID)
	s.items[id] = &Item{ID: id, OfficerID: officer, Kind: kind, SubjectKey: subject, Status: "open", UpdatedAt: s.Now()}
	return id
}

// SetClaim puts item id in status claimed by holder until expires.
func (s *Server) SetClaim(id, holder string, expires time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it := s.items[id]
	it.Status, it.ClaimedBy, it.LeaseExpiresAt = "claimed", holder, &expires
}

// Item returns a copy of item id.
func (s *Server) Item(id string) Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	return *s.items[id]
}

// Requests returns every request recorded so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.reqs...)
}

// ResetRequests forgets the recorded requests.
func (s *Server) ResetRequests() { s.mu.Lock(); s.reqs = nil; s.mu.Unlock() }

func (s *Server) write(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, Request{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Auth: r.Header.Get("Authorization"), Body: body})
	if s.down {
		s.write(w, http.StatusServiceUnavailable, map[string]any{"error": "down"})
		return
	}
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	caller, ok := s.tokens[tok]
	if !ok {
		s.write(w, http.StatusUnauthorized, map[string]any{"error": "bad token"})
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/api/v1/officers/")
	if !ok {
		s.write(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	parts := strings.Split(rest, "/")
	officer := parts[0]
	if officer != caller {
		s.write(w, http.StatusForbidden, map[string]any{"error": "officer token for " + caller + " may not touch " + officer})
		return
	}
	now := s.Now()
	switch {
	case len(parts) == 2 && parts[1] == "brief" && r.Method == http.MethodGet:
		s.write(w, http.StatusOK, map[string]any{
			"officer": map[string]any{"id": officer},
			"type":    map[string]any{"key": strings.SplitN(officer, ":", 2)[0], "charter": s.Charters[officer], "charter_version": 1},
			"open_items": s.list(officer, "open", now), "recent_items": []any{}, "cairn": map[string]any{"scopes": []any{}},
			"generated_at": now,
		})
	case len(parts) == 2 && parts[1] == "items" && r.Method == http.MethodGet:
		s.write(w, http.StatusOK, map[string]any{"items": s.list(officer, r.URL.Query().Get("status"), now), "next_cursor": ""})
	case len(parts) == 4 && parts[1] == "items" && r.Method == http.MethodPost:
		s.itemVerb(w, officer, parts[2], parts[3], body, now)
	default:
		s.write(w, http.StatusNotFound, map[string]any{"error": "not found"})
	}
}

// list is the officer's items in status; an expired claim lists as open
// (claimable by anyone), and as claimed too, which is how a real HQ might
// report either way.
func (s *Server) list(officer, status string, now time.Time) []Item {
	out := []Item{}
	for _, it := range s.items {
		if it.OfficerID != officer {
			continue
		}
		if status == "" || it.Status == status {
			out = append(out, *it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Server) itemVerb(w http.ResponseWriter, officer, id, verb string, body map[string]any, now time.Time) {
	it, ok := s.items[id]
	if !ok || it.OfficerID != officer {
		s.write(w, http.StatusNotFound, map[string]any{"error": "no such item"})
		return
	}
	instance, _ := body["instance"].(string)
	lease := 600
	if v, ok := body["lease_seconds"].(float64); ok {
		lease = int(v)
	}
	live := it.Status == "claimed" && it.LeaseExpiresAt != nil && it.LeaseExpiresAt.After(now)
	switch verb {
	case "claim":
		if it.Status == "done" || it.Status == "escalated" {
			s.write(w, http.StatusGone, map[string]any{"error": "done"})
			return
		}
		if live && it.ClaimedBy != instance {
			s.write(w, http.StatusConflict, map[string]any{"held_by": it.ClaimedBy, "lease_expires_at": it.LeaseExpiresAt})
			return
		}
		exp := now.Add(time.Duration(lease) * time.Second)
		it.Status, it.ClaimedBy, it.LeaseExpiresAt, it.UpdatedAt = "claimed", instance, &exp, now
		it.Attempts++
		s.write(w, http.StatusOK, map[string]any{"item": *it, "lease_expires_at": exp})
	case "heartbeat":
		if !live || it.ClaimedBy != instance {
			s.write(w, http.StatusConflict, map[string]any{"error": "lease lost"})
			return
		}
		exp := now.Add(time.Duration(lease) * time.Second)
		it.LeaseExpiresAt, it.UpdatedAt = &exp, now
		s.write(w, http.StatusOK, map[string]any{"lease_expires_at": exp})
	case "complete":
		if !live || it.ClaimedBy != instance {
			s.write(w, http.StatusConflict, map[string]any{"error": "not the holder"})
			return
		}
		outcome, _ := body["outcome"].(string)
		summary, _ := body["summary"].(string)
		st := "done"
		if outcome == "escalated" {
			st = "escalated"
		}
		it.Status, it.Outcome, it.Summary, it.ClaimedBy, it.LeaseExpiresAt, it.UpdatedAt = st, outcome, summary, "", nil, now
		s.write(w, http.StatusOK, map[string]any{"item": *it})
	case "release":
		if it.Status != "claimed" || it.ClaimedBy != instance {
			s.write(w, http.StatusConflict, map[string]any{"error": "not the holder"})
			return
		}
		it.Status, it.ClaimedBy, it.LeaseExpiresAt, it.UpdatedAt = "open", "", nil, now
		s.write(w, http.StatusOK, map[string]any{"item": *it})
	default:
		s.write(w, http.StatusNotFound, map[string]any{"error": "no such verb"})
	}
}
