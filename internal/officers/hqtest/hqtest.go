// Package hqtest is an in-process fake of the HQ officers API (the officers
// contract, v1), for tests in internal/officers and internal/api. It is NOT a
// reference implementation: it implements the endpoints muster consumes, with
// the claim/lease rules the contract states, and records every request so a
// test can assert which token reached which officer.
package hqtest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
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
	// Priority is HQ's item priority (urgent|high|normal|low); absent reads
	// as normal. Only the claim-next route orders by it.
	Priority string `json:"priority,omitempty"`
	// Tier is HQ's item tier (civ-hq docs/OFFICERS.md, "Tiers and due
	// dates"): routine or judgement. Empty — an item added with AddItem — is
	// served without the field, as an HQ without tiers would, and a claim
	// naming a tier reads it as judgement (HQ's default for an unnamed kind).
	Tier string `json:"tier,omitempty"`
}

// tierOf is the tier item it reads as: its own, or judgement when unset.
func tierOf(it *Item) string {
	if it.Tier == "" {
		return "judgement"
	}
	return it.Tier
}

// Request is one recorded request. Body is the body decoded as a JSON
// object (nil when it is not one); Raw is the body's bytes, and ContentType
// its Content-Type header.
type Request struct {
	Method, Path, Query, Auth string
	Body                      map[string]any
	Raw                       []byte
	ContentType               string
}

// AskImage is one image accepted by POST /officers/{id}/ask-images.
type AskImage struct {
	ID, Officer, Filename, ContentType, SHA256 string
	Data                                       []byte
}

// Ask is one ask accepted by POST /officers/{id}/items/{item}/asks.
type Ask struct {
	ID, Officer, Item string
	Body              map[string]any
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
	// ObsCalls is every POST /officers/{id}/obs/query the fake ran, in order.
	// ObsOff makes both obs routes answer 503 obs_unconfigured (an HQ with
	// the route switched off); ObsEmpty makes a query answer empty.
	ObsCalls []ObsCall
	ObsOff   bool
	ObsEmpty bool
	// ObsFail makes a query that passes validation answer HQ's backend
	// failure instead of running (502 upstream / wrong_shape).
	ObsFail ObsFail
	// ObsRawCalls is every POST /officers/{id}/obs/raw the fake ran (past
	// the decoder and the holder check), in order; ObsRawNotInvestigating
	// makes every one answer 403 not_investigating (a type without
	// investigation: true). ObsRawAnswers are the exact bytes each 200 wrote.
	ObsRawCalls            []ObsCall
	ObsRawNotInvestigating bool
	ObsRawAnswers          [][]byte
	// ClaimNext serves POST /officers/{id}/claim-next (civ-hq docs/OFFICERS.md
	// "Priority"); false — the default — is an HQ from before it: 404.
	ClaimNext bool
	// CommitThenFail, when non-zero, makes a claim (by id or claim-next)
	// that succeeds COMMIT it and then answer this status instead: an HQ
	// that took the claim but whose answer was lost (a timeout, a 5xx).
	CommitThenFail int
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
	// The asks feature (HQ's ask-images and items/{item}/asks routes):
	// members are the GitHub logins an ask may go to; askRefusal, when set,
	// is answered by the asks route instead.
	members    map[string]bool
	askRefusal *refusal
	askImages  []AskImage
	asks       []Ask
}

type refusal struct {
	status      int
	code, error string
}

// AddMember makes login an HQ member an ask may be sent to.
func (s *Server) AddMember(login string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.members == nil {
		s.members = map[string]bool{}
	}
	s.members[login] = true
}

// RefuseAsks makes the asks route answer status {error, code} (status 0:
// serve normally).
func (s *Server) RefuseAsks(status int, code, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if status == 0 {
		s.askRefusal = nil
		return
	}
	s.askRefusal = &refusal{status: status, code: code, error: msg}
}

// AskImages returns the accepted ask images, in order.
func (s *Server) AskImages() []AskImage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]AskImage(nil), s.askImages...)
}

// Asks returns the accepted asks, in order.
func (s *Server) Asks() []Ask {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Ask(nil), s.asks...)
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

// ObsCall is one obs query the fake ran: the officer HQ resolved from the
// token, and the body.
type ObsCall struct {
	Officer string
	Body    map[string]any
}

// ObsRegistry is the fake's named-query registry, in the shape of HQ's
// GET /officers/{id}/obs/queries entries (civ-hq internal/api/officer_obs.go
// ObsQueryInfo): each entry's `args` are the only request fields it takes,
// besides query_id. Two entries — a series one and a lines one — so a test
// can send an argument one takes and the other refuses.
var ObsRegistry = []map[string]any{
	{"id": "fixture_calls_by_procedure", "backend": "prometheus", "title": "calls by procedure", "claims": "how many calls each procedure served in the window",
		"args": []string{"start", "end", "step", "procedures"}, "group_by": []string{"procedure"}, "single_step": false, "lines": false},
	{"id": "fixture_error_lines", "backend": "loki", "title": "error lines", "claims": "a redacted sample of error lines, never a count",
		"args": []string{"start", "end", "limit"}, "group_by": []string{}, "single_step": false, "lines": true},
}

// obsStringFields and obsOtherFields are HQ's POST /obs/query body fields
// (civ-hq internal/api/officer_obs.go OfficerObsQueryRequest) by JSON type.
var (
	obsStringFields = map[string]bool{"query_id": true, "start": true, "end": true, "step": true, "tag": true, "name": true, "path": true}
	obsIntFields    = map[string]bool{"min_calls": true, "limit": true}
)

// obsBodyShape is HQ's 400 invalid text for a body its decoder refuses.
const obsBodyShape = `body must be one JSON object {"query_id", "start", "end", "step"?, "tag"?, "procedures"?, "min_calls"?, "name"?, "path"?, "limit"?}`

// ObsFail, when Code is set, is the answer a query that passes validation
// gets instead of running: HQ's 502 upstream (the backend read failed) or
// 502 wrong_shape (the backend answered in the wrong shape).
type ObsFail struct {
	Status int
	Code   string
}

// obsDecodes reports whether body would pass HQ's decoder: one JSON object
// of the known fields (unknown ones refused), each of its type.
func obsDecodes(body map[string]any) bool {
	if body == nil {
		return false
	}
	for k, v := range body {
		switch {
		case obsStringFields[k]:
			if _, ok := v.(string); !ok {
				return false
			}
		case obsIntFields[k]:
			if f, ok := v.(float64); !ok || f != float64(int(f)) {
				return false
			}
		case k == "procedures":
			list, ok := v.([]any)
			if !ok {
				return false
			}
			for _, e := range list {
				if _, ok := e.(string); !ok {
					return false
				}
			}
		default:
			return false
		}
	}
	return true
}

// obsTime is HQ's obsTime: required, RFC 3339, whole seconds.
func obsTime(field string, body map[string]any) string {
	v, _ := body[field].(string)
	if v == "" {
		return field + " is required (RFC 3339)"
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return field + " " + strconv.Quote(v) + " is not an RFC 3339 time"
	}
	if t.Nanosecond() != 0 {
		return field + " " + strconv.Quote(v) + " is not a whole second"
	}
	return ""
}

// obs serves the two obs routes as HQ does (civ-hq internal/api/
// officer_obs.go), refusals in HQ's order and with HQ's codes: the decoder
// (400 invalid: not one object of the known fields, a field of the wrong
// type), the registry (400 unknown_query), then the entry's arguments (400
// invalid: start/end required, RFC 3339, whole seconds; an argument the
// entry does not take; one it needs and lacks).
func (s *Server) obs(w http.ResponseWriter, officer, verb, method string, body map[string]any) {
	if s.ObsOff {
		s.write(w, http.StatusServiceUnavailable, map[string]any{"error": "the obs query route is off on this HQ (CIVHQ_OFFICER_OBS)", "code": "obs_unconfigured"})
		return
	}
	invalid := func(msg string) { s.write(w, http.StatusBadRequest, map[string]any{"error": msg, "code": "invalid"}) }
	switch {
	case verb == "queries" && method == http.MethodGet:
		s.write(w, http.StatusOK, map[string]any{"queries": ObsRegistry, "limits": map[string]any{"max_range_seconds": 604800, "max_lines": 1000, "per_minute": 30}})
	case verb == "query" && method == http.MethodPost:
		if !obsDecodes(body) {
			invalid(obsBodyShape)
			return
		}
		id, _ := body["query_id"].(string)
		var entry map[string]any
		var ids []string
		for _, e := range ObsRegistry {
			ids = append(ids, e["id"].(string))
			if e["id"] == id {
				entry = e
			}
		}
		if entry == nil {
			sort.Strings(ids)
			s.write(w, http.StatusBadRequest, map[string]any{"error": "no registry entry " + strconv.Quote(id) + "; the entries are: " + strings.Join(ids, ", "), "code": "unknown_query"})
			return
		}
		for _, f := range []string{"start", "end"} {
			if msg := obsTime(f, body); msg != "" {
				invalid(msg)
				return
			}
		}
		takes := map[string]bool{}
		for _, a := range entry["args"].([]string) {
			takes[a] = true
		}
		for _, f := range []string{"step", "min_calls"} {
			if takes[f] && body[f] == nil {
				invalid("query " + id + " needs " + f)
				return
			}
		}
		for _, f := range []string{"step", "tag", "procedures", "name", "path", "limit", "min_calls"} {
			if _, given := body[f]; given && !takes[f] {
				invalid("query " + id + " takes no " + f)
				return
			}
		}
		if s.ObsFail.Code != "" {
			s.write(w, s.ObsFail.Status, map[string]any{"error": "the backend read failed in the fake", "code": s.ObsFail.Code})
			return
		}
		s.ObsCalls = append(s.ObsCalls, ObsCall{Officer: officer, Body: body})
		ans := map[string]any{"audit_id": len(s.ObsCalls), "query_id": id, "backend": entry["backend"], "claims": entry["claims"],
			"status": "ok", "empty": false, "result_count": 1, "series": []any{map[string]any{"key": "fixture.procedure", "total": 42}}, "lines": []any{}}
		if s.ObsEmpty {
			ans["status"], ans["empty"], ans["result_count"], ans["series"] = "empty", true, 0, []any{}
			ans["hint"] = "an empty answer is not a zero: run the positive control first"
		}
		s.write(w, http.StatusOK, ans)
	case verb == "raw" && method == http.MethodPost:
		s.obsRaw(w, officer, body)
	default:
		s.write(w, http.StatusNotFound, map[string]any{"error": "not found"})
	}
}

// obsRawStringFields are HQ's POST /obs/raw string fields (civ-hq
// internal/api/officer_obs_raw.go OfficerObsRawRequest); item_id and limit
// are its integers.
var obsRawStringFields = map[string]bool{"instance": true, "backend": true, "query": true, "start": true, "end": true, "step": true}

// obsRaw serves POST /officers/{id}/obs/raw as HQ does, in HQ's order: the
// decoder (400 invalid: one object of the known fields, each its type), the
// holder check (403 not_investigating: the item is the officer's, judgement,
// claimed by instance with its lease live — or the fake's type does not
// investigate), then the window (400 invalid). It does not parse the query.
func (s *Server) obsRaw(w http.ResponseWriter, officer string, body map[string]any) {
	invalid := func(msg string) { s.write(w, http.StatusBadRequest, map[string]any{"error": msg, "code": "invalid"}) }
	if body == nil {
		invalid("body must be one JSON object")
		return
	}
	for k, v := range body {
		switch {
		case obsRawStringFields[k]:
			if _, ok := v.(string); !ok {
				invalid("body field " + k + " must be a string")
				return
			}
		case k == "item_id" || k == "limit":
			if f, ok := v.(float64); !ok || f != float64(int64(f)) {
				invalid("body field " + k + " must be an integer")
				return
			}
		default:
			invalid(`body must be one JSON object {"item_id", "instance", "backend": "prometheus"|"loki", "query", "start", "end", "step"?, "limit"?}`)
			return
		}
	}
	idf, _ := body["item_id"].(float64)
	inst, _ := body["instance"].(string)
	it := s.items[strconv.FormatInt(int64(idf), 10)]
	if s.ObsRawNotInvestigating || it == nil || it.OfficerID != officer || tierOf(it) != "judgement" || it.Status != "claimed" ||
		it.ClaimedBy != inst || it.LeaseExpiresAt == nil || !it.LeaseExpiresAt.After(s.Now()) {
		s.write(w, http.StatusForbidden, map[string]any{"error": "not an investigating officer's judgement item held by that instance", "code": "not_investigating"})
		return
	}
	for _, f := range []string{"start", "end"} {
		if msg := obsTime(f, body); msg != "" {
			invalid(msg)
			return
		}
	}
	s.ObsRawCalls = append(s.ObsRawCalls, ObsCall{Officer: officer, Body: body})
	ans := map[string]any{"audit_id": 9000 + len(s.ObsRawCalls), "backend": body["backend"], "query": body["query"], "status": "ok",
		"window": map[string]any{"start": body["start"], "end": body["end"]}, "step": "", "steps": 0, "result_count": 1, "empty": false,
		"series": []any{map[string]any{"labels": map[string]any{"fixture": "raw"}, "points": []any{map[string]any{"t": 1, "v": "7"}}}},
		"lines":  []any{}, "truncated": false, "redactions": 0}
	b, _ := json.Marshal(ans)
	s.ObsRawAnswers = append(s.ObsRawAnswers, b)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
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

// AddTieredItem adds an open item of tier (routine or judgement) for officer
// and returns its id.
func (s *Server) AddTieredItem(officer, kind, subject, tier string) string {
	id := s.AddItem(officer, kind, subject)
	s.mu.Lock()
	s.items[id].Tier = tier
	s.mu.Unlock()
	return id
}

// SetTier sets item id's tier as HQ reads it now (e.g. judgement for a routine
// item HQ promoted after two lapsed leases).
func (s *Server) SetTier(id, tier string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[id].Tier = tier
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

// SetPriority sets item id's priority.
func (s *Server) SetPriority(id, priority string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[id].Priority = priority
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
	raw, _ := io.ReadAll(io.LimitReader(r.Body, maxAskImageBytes+1<<10))
	_ = json.Unmarshal(raw, &body)
	if s.Before != nil {
		s.Before(r.Method, r.URL.Path)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, Request{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Auth: r.Header.Get("Authorization"), Body: body,
		Raw: raw, ContentType: r.Header.Get("Content-Type")})
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
		if tier := r.URL.Query().Get("tier"); tier != "" {
			// `?tier=` (civ-hq docs/OFFICERS.md): the tier as items read now.
			kept := items[:0]
			for _, it := range items {
				if tierOf(&it) == tier {
					kept = append(kept, it)
				}
			}
			items = kept
		}
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
	case len(parts) == 3 && parts[1] == "obs":
		s.obs(w, officer, parts[2], r.Method, body)
	case len(parts) == 2 && parts[1] == "ask-images" && r.Method == http.MethodPost:
		s.askImage(w, officer, r, raw)
	case len(parts) == 2 && parts[1] == "claim-next" && r.Method == http.MethodPost && s.ClaimNext:
		s.claimNext(w, officer, body, now)
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

// claimNext is HQ's claim-next: the first claimable item (open or a lapsed
// claim, not blocked, and — when the body names a tier — of that tier as the
// item reads, tierOf; no tier key takes any tier) by priority, then id;
// 200 {item, lease_expires_at}, or {item: null}. A tier other than routine or
// judgement is 400, as on HQ.
func (s *Server) claimNext(w http.ResponseWriter, officer string, body map[string]any, now time.Time) {
	tier, named := "", false
	if v, ok := body["tier"]; ok {
		tier, _ = v.(string)
		named = true
		if tier != "routine" && tier != "judgement" {
			s.write(w, http.StatusBadRequest, map[string]any{"error": "tier must be routine or judgement", "code": "bad_request"})
			return
		}
	}
	rank := map[string]int{"urgent": 0, "high": 1, "": 2, "normal": 2, "low": 3}
	var best *Item
	for _, it := range s.items {
		live := it.Status == "claimed" && it.LeaseExpiresAt != nil && it.LeaseExpiresAt.After(now)
		if it.OfficerID != officer || it.Blocked || live || (it.Status != "open" && it.Status != "claimed") {
			continue
		}
		if named && tierOf(it) != tier {
			continue
		}
		if best == nil || rank[it.Priority] < rank[best.Priority] || (rank[it.Priority] == rank[best.Priority] && numID(it.ID) < numID(best.ID)) {
			best = it
		}
	}
	if best == nil {
		s.write(w, http.StatusOK, map[string]any{"item": nil, "lease_expires_at": nil})
		return
	}
	instance, _ := body["instance"].(string)
	lease := 600
	if v, ok := body["lease_seconds"].(float64); ok {
		lease = int(v)
	}
	exp := now.Add(time.Duration(lease) * time.Second)
	best.Status, best.ClaimedBy, best.LeaseExpiresAt, best.UpdatedAt = "claimed", instance, &exp, now
	best.Attempts++
	if s.CommitThenFail != 0 {
		s.write(w, s.CommitThenFail, map[string]any{"error": "committed, then failed to answer"})
		return
	}
	s.write(w, http.StatusOK, map[string]any{"item": *best, "lease_expires_at": exp})
}

func numID(id string) int { n, _ := strconv.Atoi(id); return n }

func (s *Server) itemVerb(w http.ResponseWriter, officer, id, verb string, body map[string]any, now time.Time) {
	// HQ checks an ask's input before it looks the item up: each limit is a
	// 400 invalid, whatever the item. A forced refusal (RefuseAsks) wins.
	if verb == "asks" && s.askRefusal == nil {
		if msg := askInputInvalid(body); msg != "" {
			s.write(w, http.StatusBadRequest, map[string]any{"error": msg, "code": "invalid"})
			return
		}
	}
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
		// HQ's claim body is {instance, lease_seconds?, tier?}; every body
		// refuses unknown fields (400), so a misspelled tier key is caught.
		for k := range body {
			switch k {
			case "instance", "lease_seconds", "tier":
			default:
				s.write(w, http.StatusBadRequest, map[string]any{"error": "unknown field " + k, "code": "bad_request"})
				return
			}
		}
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
		// A claim naming a tier takes only an item of that tier as it reads
		// (409 wrong_tier, nothing changed); a claim without one takes any.
		// HQ's precedence: held, wrong_tier, not_due, blocked.
		if tier, named := body["tier"]; named {
			t, _ := tier.(string)
			if t != "routine" && t != "judgement" {
				s.write(w, http.StatusBadRequest, map[string]any{"error": "tier must be routine or judgement", "code": "bad_request"})
				return
			}
			if t != tierOf(it) {
				s.write(w, http.StatusConflict, map[string]any{"error": "wrong tier", "code": "wrong_tier", "item_tier": tierOf(it)})
				return
			}
		}
		// HQ's task ordering: a blocked item is not claimable; nothing changes.
		if it.Blocked {
			s.write(w, http.StatusConflict, map[string]any{"error": "blocked", "code": "blocked", "blocked_by": it.BlockedBy})
			return
		}
		exp := now.Add(time.Duration(lease) * time.Second)
		it.Status, it.ClaimedBy, it.LeaseExpiresAt, it.UpdatedAt = "claimed", instance, &exp, now
		it.Attempts++
		if s.CommitThenFail != 0 {
			s.write(w, s.CommitThenFail, map[string]any{"error": "committed, then failed to answer"})
			return
		}
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
	case "asks":
		s.ask(w, officer, it, instance, live, body, now)
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

// maxAskImageBytes is HQ's bound on one ask image.
const maxAskImageBytes = 5 << 20

// askImage is POST /officers/{id}/ask-images?filename=…: the raw image bytes,
// Content-Type one of png/jpeg/webp and matching the bytes (415 otherwise),
// at most 5 MiB (413). 201 {id, filename, content_type, size, sha256}.
func (s *Server) askImage(w http.ResponseWriter, officer string, r *http.Request, raw []byte) {
	ct := r.Header.Get("Content-Type")
	switch ct {
	case "image/png", "image/jpeg", "image/webp":
	default:
		s.write(w, http.StatusUnsupportedMediaType, map[string]any{"error": "content type " + ct + " is not an accepted image", "code": "unsupported_media_type"})
		return
	}
	if len(raw) > maxAskImageBytes {
		s.write(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "image over 5 MiB", "code": "too_large"})
		return
	}
	if http.DetectContentType(raw) != ct {
		s.write(w, http.StatusUnsupportedMediaType, map[string]any{"error": "the bytes are not " + ct, "code": "unsupported_media_type"})
		return
	}
	name := r.URL.Query().Get("filename")
	if name == "" {
		s.write(w, http.StatusBadRequest, map[string]any{"error": "filename is required", "code": "invalid"})
		return
	}
	sum := sha256.Sum256(raw)
	img := AskImage{ID: "att-" + strconv.Itoa(len(s.askImages)+1), Officer: officer, Filename: name, ContentType: ct, SHA256: hex.EncodeToString(sum[:]), Data: append([]byte(nil), raw...)}
	s.askImages = append(s.askImages, img)
	s.write(w, http.StatusCreated, map[string]any{"id": img.ID, "filename": name, "content_type": ct, "size": len(raw), "sha256": img.SHA256})
}

// askInputInvalid is HQ's ask input limits (civ-hq createPersonAsk and
// PersonAskInput.normalize), each answered 400 invalid with HQ's message:
// question 1-2000 characters, at most 6 options of 1-40 characters, at most 4
// images, deadline_seconds a whole number from 300 to 259200 (absent: 24
// hours). "" = within them.
func askInputInvalid(body map[string]any) string {
	q, _ := body["question"].(string)
	if n := utf8.RuneCountInString(strings.TrimSpace(q)); n == 0 || n > 2000 {
		return "question must be 1-2000 characters"
	}
	opts, _ := body["options"].([]any)
	if len(opts) > 6 {
		return "at most 6 options"
	}
	for i, v := range opts {
		o, _ := v.(string)
		if n := utf8.RuneCountInString(strings.TrimSpace(o)); n == 0 || n > 40 {
			return "options[" + strconv.Itoa(i) + "] must be 1-40 characters on one line"
		}
	}
	if imgs, _ := body["images"].([]any); len(imgs) > 4 {
		return "at most 4 images"
	}
	if v, ok := body["deadline_seconds"]; ok && v != nil { // null = absent, as HQ decodes it
		f, isNum := v.(float64)
		switch {
		case !isNum || f != float64(int(f)):
			// HQ's field is an int: anything else fails its body decode.
			return `body must be one JSON object {"instance", "to", "question", "options"?: [label], "images"?: [image id], "context_url"?, "deadline_seconds"?}`
		case f <= 0:
			return "deadline_seconds must be 300-259200"
		case f < 300 || f > 259200:
			return "deadline_seconds must be 300-259200 (5 minutes to 72 hours; default 24 hours)"
		}
	}
	return ""
}

// ask is POST /officers/{id}/items/{item}/asks: 410 on a finished item, 409
// lease_lost unless the instance holds a live lease or the item is already
// waiting, 422 unknown_recipient for a login that is not a member, 400 on an
// unknown image id (the input limits come first, in itemVerb:
// askInputInvalid); then the item is WAITING with no holder, and 201 {ask, item}, the ask
// carrying its delivery (ask.delivery), as HQ nests it.
func (s *Server) ask(w http.ResponseWriter, officer string, it *Item, instance string, live bool, body map[string]any, now time.Time) {
	if f := s.askRefusal; f != nil {
		s.write(w, f.status, map[string]any{"error": f.error, "code": f.code})
		return
	}
	if it.Status == "done" || it.Status == "escalated" {
		s.write(w, http.StatusGone, map[string]any{"error": "item is " + it.Status, "code": "gone"})
		return
	}
	if !(live && it.ClaimedBy == instance && instance != "") && it.Status != "waiting" {
		s.write(w, http.StatusConflict, map[string]any{"error": "this instance does not hold a live lease on the item", "code": "lease_lost"})
		return
	}
	to, _ := body["to"].(string)
	if !s.members[to] {
		s.write(w, http.StatusUnprocessableEntity, map[string]any{"error": "no HQ member " + to, "code": "unknown_recipient"})
		return
	}
	if ids, ok := body["images"].([]any); ok {
		for _, v := range ids {
			id, _ := v.(string)
			known := false
			for _, img := range s.askImages {
				known = known || (img.ID == id && img.Officer == officer)
			}
			if !known {
				s.write(w, http.StatusBadRequest, map[string]any{"error": "unknown image " + id, "code": "invalid"})
				return
			}
		}
	}
	a := Ask{ID: "ask-" + strconv.Itoa(len(s.asks)+1), Officer: officer, Item: it.ID, Body: body}
	s.asks = append(s.asks, a)
	it.Status, it.ClaimedBy, it.LeaseExpiresAt, it.UpdatedAt = "waiting", "", nil, now
	secs := 86400
	if v, ok := body["deadline_seconds"].(float64); ok {
		secs = int(v)
	}
	s.write(w, http.StatusCreated, map[string]any{
		"ask": map[string]any{"id": a.ID, "state": "open", "deadline": now.Add(time.Duration(secs) * time.Second), "to": to,
			"delivery": map[string]any{"notice": "both", "inbox": true, "dm": "queued"}},
		"item": *it,
	})
}
