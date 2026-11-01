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
	// Blocked and BlockedBy are HQ's task ordering: an item whose task waits
	// on unfinished tasks says `blocked: true, blocked_by: [task ids]`, and
	// both are ABSENT otherwise (as on an HQ without ordering). A claim on a
	// blocked item is refused (409 blocked).
	Blocked   bool     `json:"blocked,omitempty"`
	BlockedBy []string `json:"blocked_by,omitempty"`
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
	status   int
	reqs     []Request
	Now      func() time.Time
	Charters map[string]string
	// PageSize, when > 0, pages the items list (next = the next offset).
	PageSize int
	// Evaluations is every POST /releases/captain accepted, in order.
	Evaluations []Evaluation
	// Disabled officers answer `enabled: false` on GET /officers/{id}.
	Disabled map[string]bool
	// Notices is every notice POSTed to /officers/{id}/notices that was
	// accepted; NoticeStatus, when non-zero, is answered instead (e.g. 503).
	Notices      []Notice
	NoticeStatus int
	// Preflight is POST /officers/{id}/preflight's answer (404 when nil).
	Preflight map[string]any
	// Before, when set, runs ahead of every request, OUTSIDE the fake's lock
	// (so it may call SetClaim, SetBlocked, …): a test changes HQ's state at
	// an exact point in a multi-request exchange. Set it before any request.
	Before func(method, path string)
	// transcripts are the transcript events stored per item, in seq order
	// (POST /officers/{id}/items/{item}/transcript); transcriptStatus, when
	// non-zero, is answered instead (e.g. 503).
	transcripts      map[string][]TranscriptEvent
	transcriptStatus int
	// failNext answers the next failN transcript posts with failCode, after
	// STORING them when failCommit (an HQ that commits and then fails to
	// answer: a lost acknowledgement).
	failN, failCode int
	failCommit      bool
}

// FailTranscript makes the next n transcript posts answer code; commit stores
// each one first (the acknowledgement is lost, the write is not).
func (s *Server) FailTranscript(code, n int, commit bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failCode, s.failN, s.failCommit = code, n, commit
}

// TranscriptEvent is one stored transcript event.
type TranscriptEvent struct {
	Seq      int64
	At       string
	Kind     string
	Text     string
	Instance string
}

// Transcript returns the events stored for item, in seq order.
func (s *Server) Transcript(item string) []TranscriptEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]TranscriptEvent(nil), s.transcripts[item]...)
}

// SetTranscriptStatus makes the transcript route answer code (0: serve).
func (s *Server) SetTranscriptStatus(code int) { s.mu.Lock(); s.transcriptStatus = code; s.mu.Unlock() }

// Complete marks item done (as an instance's officer_complete would), so a
// later transcript post for it answers 409 not_claimed.
func (s *Server) Complete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it := s.items[id]
	it.Status, it.ClaimedBy, it.LeaseExpiresAt = "done", "", nil
}

// Notice is one accepted sensitive-path notice.
type Notice struct {
	Officer string
	Body    map[string]any
}

// Evaluation is one accepted captain-route post: the officer HQ resolved from
// the token, and the body.
type Evaluation struct {
	Officer string
	Body    map[string]any
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

// SetStatus makes every request answer code (0 = serve normally): 401/403 as
// a revoked or wrong token would, 404 as an HQ with the officer API off.
func (s *Server) SetStatus(code int) { s.mu.Lock(); s.status = code; s.mu.Unlock() }

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

// SetBlocked marks item id blocked by the tasks by (none: unblocked, both
// fields absent from what the fake serves).
func (s *Server) SetBlocked(id string, by ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it := s.items[id]
	it.Blocked, it.BlockedBy = len(by) > 0, append([]string(nil), by...)
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

// SetDisabled sets whether officer answers `enabled: false`.
func (s *Server) SetDisabled(officer string, disabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Disabled == nil {
		s.Disabled = map[string]bool{}
	}
	s.Disabled[officer] = disabled
}

// SetNoticeStatus makes the notices route answer code (0: accept).
func (s *Server) SetNoticeStatus(code int) { s.mu.Lock(); s.NoticeStatus = code; s.mu.Unlock() }

// SetPreflight sets the preflight route's answer (nil: 404).
func (s *Server) SetPreflight(body map[string]any) { s.mu.Lock(); s.Preflight = body; s.mu.Unlock() }

// NoticesPosted returns the accepted notices.
func (s *Server) NoticesPosted() []Notice {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Notice(nil), s.Notices...)
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
	if s.Before != nil {
		s.Before(r.Method, r.URL.Path)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, Request{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Auth: r.Header.Get("Authorization"), Body: body})
	if s.down {
		s.write(w, http.StatusServiceUnavailable, map[string]any{"error": "down"})
		return
	}
	if s.status != 0 {
		s.write(w, s.status, map[string]any{"error": "forced", "code": "forced"})
		return
	}
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	caller, ok := s.tokens[tok]
	if !ok {
		s.write(w, http.StatusUnauthorized, map[string]any{"error": "bad token"})
		return
	}
	if r.URL.Path == "/api/v1/releases/captain" && r.Method == http.MethodPost {
		// The captain route accepts the officer scope for a RELEASE officer only
		// (the contract), recording which officer and instance wrote it.
		if !strings.HasPrefix(caller, "release:") {
			s.write(w, http.StatusForbidden, map[string]any{"error": "only a release officer may post a captain evaluation"})
			return
		}
		// HQ's body (civ-hq docs/RELEASES.md §9.3): {service, tag, evaluation?,
		// report?, instance}; unknown fields are a 400, and so is a body with
		// neither evaluation nor report, or one missing service/tag/instance.
		for k := range body {
			switch k {
			case "service", "tag", "evaluation", "report", "instance":
			default:
				s.write(w, http.StatusBadRequest, map[string]any{"error": "unknown field " + k})
				return
			}
		}
		if body["service"] == nil || body["tag"] == nil || body["instance"] == nil || (body["evaluation"] == nil && body["report"] == nil) {
			s.write(w, http.StatusBadRequest, map[string]any{"error": "service, tag, instance and one of evaluation/report are required"})
			return
		}
		s.Evaluations = append(s.Evaluations, Evaluation{Officer: caller, Body: body})
		s.write(w, http.StatusOK, map[string]any{"recorded": true, "officer": caller, "instance": body["instance"]})
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
			"officer":    map[string]any{"id": officer},
			"type":       map[string]any{"key": strings.SplitN(officer, ":", 2)[0], "charter": s.Charters[officer], "charter_version": 1},
			"open_items": s.list(officer, "open", now), "recent_items": []any{}, "cairn": map[string]any{"scopes": []any{}},
			"generated_at": now,
		})
	case len(parts) == 2 && parts[1] == "items" && r.Method == http.MethodGet:
		// HQ's envelope (civ-hq docs/OFFICERS.md): {items, next}, next null on
		// the last page. PageSize > 0 pages the list so a test can see paging.
		items := s.list(officer, r.URL.Query().Get("status"), now)
		start := 0
		if c := r.URL.Query().Get("cursor"); c != "" {
			start, _ = strconv.Atoi(c)
		}
		if start > len(items) {
			start = len(items)
		}
		items = items[start:]
		var next any
		if s.PageSize > 0 && len(items) > s.PageSize {
			items = items[:s.PageSize]
			next = strconv.Itoa(start + s.PageSize)
		}
		s.write(w, http.StatusOK, map[string]any{"items": items, "next": next})
	case len(parts) == 4 && parts[1] == "items" && r.Method == http.MethodPost:
		s.itemVerb(w, officer, parts[2], parts[3], body, now)
	case len(parts) == 1 && r.Method == http.MethodGet:
		// GET /officers/{id}: the officer row, `enabled` as HQ spells it.
		s.write(w, http.StatusOK, map[string]any{"id": officer, "enabled": !s.Disabled[officer]})
	case len(parts) == 2 && parts[1] == "notices" && r.Method == http.MethodPost:
		if s.NoticeStatus != 0 {
			s.write(w, s.NoticeStatus, map[string]any{"error": "notice refused by the fake"})
			return
		}
		s.Notices = append(s.Notices, Notice{Officer: officer, Body: body})
		s.write(w, http.StatusCreated, map[string]any{"posted": true})
	case len(parts) == 2 && parts[1] == "preflight" && r.Method == http.MethodPost:
		if s.Preflight == nil {
			s.write(w, http.StatusNotFound, map[string]any{"error": "not found"})
			return
		}
		s.write(w, http.StatusOK, s.Preflight)
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
		// As HQ does (civ-hq docs/OFFICERS.md, lease semantics): ANY live lease
		// refuses a claim — including the caller's own ("claiming your own live
		// lease is a 409 too (use heartbeat)").
		if live {
			s.write(w, http.StatusConflict, map[string]any{"error": "held", "code": "held", "held_by": it.ClaimedBy, "lease_expires_at": it.LeaseExpiresAt})
			return
		}
		// HQ's task ordering: a blocked item is not claimable; nothing changes.
		if it.Blocked {
			s.write(w, http.StatusConflict, map[string]any{"error": "blocked", "code": "blocked", "blocked_by": it.BlockedBy})
			return
		}
		exp := now.Add(time.Duration(lease) * time.Second)
		it.Status, it.ClaimedBy, it.LeaseExpiresAt, it.UpdatedAt = "claimed", instance, &exp, now
		it.Attempts++
		s.write(w, http.StatusOK, map[string]any{"item": *it, "lease_expires_at": exp})
	case "heartbeat":
		if !live || it.ClaimedBy != instance {
			s.write(w, http.StatusConflict, map[string]any{"error": "lease lost", "code": "lease_lost"})
			return
		}
		exp := now.Add(time.Duration(lease) * time.Second)
		it.LeaseExpiresAt, it.UpdatedAt = &exp, now
		s.write(w, http.StatusOK, map[string]any{"item": *it, "lease_expires_at": exp})
	case "complete":
		if !live || it.ClaimedBy != instance {
			s.write(w, http.StatusConflict, map[string]any{"error": "lease lost", "code": "lease_lost"})
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
	case "transcript":
		s.transcript(w, officer, it, instance, live, body)
	case "release":
		if it.Status != "claimed" || it.ClaimedBy != instance {
			s.write(w, http.StatusConflict, map[string]any{"error": "lease lost", "code": "lease_lost"})
			return
		}
		it.Status, it.ClaimedBy, it.LeaseExpiresAt, it.UpdatedAt = "open", "", nil, now
		s.write(w, http.StatusOK, map[string]any{"item": *it})
	default:
		s.write(w, http.StatusNotFound, map[string]any{"error": "no such verb"})
	}
}

// Transcript route limits, as HQ states them.
const (
	maxTranscriptEvents = 100
	maxTranscriptText   = 8 << 10
)

// transcript is POST /officers/{id}/items/{item}/transcript, to the contract:
// {instance, events: [{seq, at, kind, text}]}; ≤100 events, text ≤ 8 KiB, seq
// strictly increasing within the call; idempotent on seq (a seq ≤ the highest
// stored is skipped); 409 officer_disabled, 409 not_claimed (the item is not
// claimed by this instance under a live lease), 400 on a limit. Answers
// {stored, last_seq}.
func (s *Server) transcript(w http.ResponseWriter, officer string, it *Item, instance string, live bool, body map[string]any) {
	if s.transcriptStatus != 0 {
		s.write(w, s.transcriptStatus, map[string]any{"error": "transcript refused by the fake"})
		return
	}
	if s.failN > 0 && !s.failCommit {
		s.failN--
		s.write(w, s.failCode, map[string]any{"error": "transcript failed by the fake"})
		return
	}
	for k := range body {
		if k != "instance" && k != "events" {
			s.write(w, http.StatusBadRequest, map[string]any{"error": "unknown field " + k, "code": "bad_request"})
			return
		}
	}
	if s.Disabled[officer] {
		s.write(w, http.StatusConflict, map[string]any{"error": "officer disabled", "code": "officer_disabled"})
		return
	}
	if !live || it.ClaimedBy != instance || instance == "" {
		s.write(w, http.StatusConflict, map[string]any{"error": "item not claimed by this instance", "code": "not_claimed"})
		return
	}
	raw, ok := body["events"].([]any)
	if !ok || len(raw) > maxTranscriptEvents {
		s.write(w, http.StatusBadRequest, map[string]any{"error": "events must be an array of at most 100", "code": "limits"})
		return
	}
	if s.transcripts == nil {
		s.transcripts = map[string][]TranscriptEvent{}
	}
	var last int64
	if have := s.transcripts[it.ID]; len(have) > 0 {
		last = have[len(have)-1].Seq
	}
	var batch []TranscriptEvent
	prev := int64(-1 << 62)
	for _, e := range raw {
		m, ok := e.(map[string]any)
		if !ok {
			s.write(w, http.StatusBadRequest, map[string]any{"error": "an event is not an object", "code": "limits"})
			return
		}
		seqF, _ := m["seq"].(float64)
		seq := int64(seqF)
		at, _ := m["at"].(string)
		kind, _ := m["kind"].(string)
		text, _ := m["text"].(string)
		if _, err := time.Parse(time.RFC3339, at); err != nil {
			s.write(w, http.StatusBadRequest, map[string]any{"error": "at is not RFC3339", "code": "limits"})
			return
		}
		switch kind {
		case "user", "assistant", "tool_use", "tool_result", "system":
		default:
			s.write(w, http.StatusBadRequest, map[string]any{"error": "unknown kind " + kind, "code": "limits"})
			return
		}
		if len(text) > maxTranscriptText {
			s.write(w, http.StatusBadRequest, map[string]any{"error": "text over 8 KiB", "code": "limits"})
			return
		}
		if seq <= prev {
			s.write(w, http.StatusBadRequest, map[string]any{"error": "seq not strictly increasing", "code": "limits"})
			return
		}
		prev = seq
		batch = append(batch, TranscriptEvent{Seq: seq, At: at, Kind: kind, Text: text, Instance: instance})
	}
	stored := 0
	for _, e := range batch {
		if e.Seq <= last {
			continue
		}
		s.transcripts[it.ID] = append(s.transcripts[it.ID], e)
		last = e.Seq
		stored++
	}
	if s.failN > 0 && s.failCommit {
		s.failN--
		s.write(w, s.failCode, map[string]any{"error": "committed, then failed to answer"})
		return
	}
	s.write(w, http.StatusOK, map[string]any{"stored": stored, "last_seq": last})
}
