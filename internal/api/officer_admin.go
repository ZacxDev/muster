package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
)

// ---------------------------------------------------------------------------
// THE BUILT-IN OFFICER STORE'S ADMIN API (`muster officer`, docs/officers-builtin.md)
//
//	GET    /api/officers/types                       the caller's types (no charters)
//	GET    /api/officers/types/{key}                 one type, charter included
//	PUT    /api/officers/types/{key}                 {name, charter, policy?} create or replace
//	DELETE /api/officers/types/{key}                 409 while an officer names it
//	GET    /api/officers/defs                        the caller's officers
//	PUT    /api/officers/defs/{officer}              {enabled?, mode?, escalate_to?} create or replace
//	DELETE /api/officers/defs/{officer}              409 while it has items or schedules
//	GET    /api/officers/defs/{officer}/items        ?status=&limit=
//	POST   /api/officers/defs/{officer}/items        {kind, subject_key, payload?, tier?, priority?, not_before?}
//	POST   /api/officers/defs/{officer}/items/{id}/resolve   {outcome, summary}
//	GET    /api/officers/defs/{officer}/schedules    the officer's cron schedules
//	PUT    /api/officers/defs/{officer}/schedules/{name}   {cron, kind, subject_key, payload?, tier?, priority?, skip_if_live?, enabled?}
//	DELETE /api/officers/defs/{officer}/schedules/{name}
//
// 🔴 OWNER-SCOPED TWICE. Every call acts as the CALLER (a personal token: its
// owner; the shared hook token: the operator, on an armed server only —
// requireArmedHookToken), and every store call both names that owner and runs
// under its principal, so row-level security and the WHERE clause agree.
//
// 🔴 A CHARTER LIVES ONLY HERE. It is written through PUT …/types/{key} (the
// CLI reads it from a file or stdin, never argv) into muster's database, and
// reaches an instance only through officer_brief. Nothing in this repository
// or the deployment's manifests holds one.
//
// 404 while the store is off (EnableOfficerStore: MUSTER_OFFICERS unset or no
// database).
// ---------------------------------------------------------------------------

const officerAdminPrefix = "/api/officers"

// officerAdminBodyLimit bounds a request body: a charter is at most 32 KiB
// (0118's CHECK), a payload a small object.
const officerAdminBodyLimit = 256 << 10

func (s *Server) registerOfficerAdminRoutes(mux Mux) {
	// Auth first, then the store's switch: a caller learns the store is off only
	// once admitted.
	h := func(f http.HandlerFunc) http.HandlerFunc { return s.requireArmedHookToken(s.requireOfficerStore(f)) }
	mux.HandleFunc("GET "+officerAdminPrefix+"/types", h(s.handleOfficerTypes))
	mux.HandleFunc("GET "+officerAdminPrefix+"/types/{key}", h(s.handleOfficerTypeGet))
	mux.HandleFunc("PUT "+officerAdminPrefix+"/types/{key}", h(s.handleOfficerTypePut))
	mux.HandleFunc("DELETE "+officerAdminPrefix+"/types/{key}", h(s.handleOfficerTypeDelete))
	mux.HandleFunc("GET "+officerAdminPrefix+"/defs", h(s.handleOfficerDefs))
	mux.HandleFunc("PUT "+officerAdminPrefix+"/defs/{officer}", h(s.handleOfficerDefPut))
	mux.HandleFunc("DELETE "+officerAdminPrefix+"/defs/{officer}", h(s.handleOfficerDefDelete))
	mux.HandleFunc("GET "+officerAdminPrefix+"/defs/{officer}/items", h(s.handleOfficerItems))
	mux.HandleFunc("POST "+officerAdminPrefix+"/defs/{officer}/items", h(s.handleOfficerItemEnqueue))
	mux.HandleFunc("POST "+officerAdminPrefix+"/defs/{officer}/items/{id}/resolve", h(s.handleOfficerItemResolve))
	mux.HandleFunc("GET "+officerAdminPrefix+"/defs/{officer}/schedules", h(s.handleOfficerSchedules))
	mux.HandleFunc("PUT "+officerAdminPrefix+"/defs/{officer}/schedules/{name}", h(s.handleOfficerSchedulePut))
	mux.HandleFunc("DELETE "+officerAdminPrefix+"/defs/{officer}/schedules/{name}", h(s.handleOfficerScheduleDelete))
}

// requireOfficerStore answers 404 while the store is off.
func (s *Server) requireOfficerStore(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.officerStore == nil {
			s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "the built-in officer store is off: it needs MUSTER_OFFICERS set (an empty [] turns it on) and a database"})
			return
		}
		next(w, r)
	}
}

// adminOwner is who an admin call acts for: the caller's own user (the
// operator for the shared hook token).
func adminOwner(r *http.Request) int64 {
	if o := requestOwner(r.Context()); o > 0 {
		return o
	}
	return machineUserID()
}

// decodeAdmin reads r's JSON body strictly into v (an unknown field is a 400).
func (s *Server) decodeAdmin(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, officerAdminBodyLimit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the body is not the JSON this route takes: " + err.Error()})
		return false
	}
	return true
}

// adminStoreErr answers a store error: 404, 409, 400 for the store's own
// refusals and a contract decision's status; 500 (logged, not echoed) for
// anything else.
func (s *Server) adminStoreErr(w http.ResponseWriter, what string, err error) {
	var dec *officers.HQError
	switch {
	case errors.Is(err, builtin.ErrNotFound):
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such " + what})
	case errors.Is(err, builtin.ErrInUse):
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": what + " is in use: " + err.Error()})
	case errors.Is(err, builtin.ErrInvalid):
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
	case errors.As(err, &dec):
		var body map[string]any
		_ = json.Unmarshal(dec.Body, &body)
		s.writeJSON(w, dec.Status, body)
	default:
		s.logger.Printf("officers: admin %s: %v", what, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the built-in officer store failed; see muster's log"})
	}
}

func (s *Server) handleOfficerTypes(w http.ResponseWriter, r *http.Request) {
	list, err := s.officerStore.ListTypes(r.Context(), adminOwner(r))
	if err != nil {
		s.adminStoreErr(w, "type", err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"types": list})
}

func (s *Server) handleOfficerTypeGet(w http.ResponseWriter, r *http.Request) {
	t, err := s.officerStore.GetType(r.Context(), adminOwner(r), r.PathValue("key"))
	if err != nil {
		s.adminStoreErr(w, "type", err)
		return
	}
	s.writeJSON(w, http.StatusOK, t)
}

func (s *Server) handleOfficerTypePut(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name    string          `json:"name"`
		Charter string          `json:"charter"`
		Policy  json.RawMessage `json:"policy"`
	}
	if !s.decodeAdmin(w, r, &in) {
		return
	}
	t, err := s.officerStore.PutTypeReturning(r.Context(), adminOwner(r), builtin.Type{Key: r.PathValue("key"), Name: in.Name, Charter: in.Charter, Policy: in.Policy})
	if err != nil {
		s.adminStoreErr(w, "type", err)
		return
	}
	s.logger.Printf("officers: user %d put officer type %s (charter version %d)", adminOwner(r), t.Key, t.CharterVersion)
	t.Charter = "" // the caller has it; the answer is the version
	s.writeJSON(w, http.StatusOK, t)
}

func (s *Server) handleOfficerTypeDelete(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if err := s.officerStore.DeleteType(r.Context(), adminOwner(r), key); err != nil {
		s.adminStoreErr(w, "type", err)
		return
	}
	s.logger.Printf("officers: user %d deleted officer type %s", adminOwner(r), key)
	s.writeJSON(w, http.StatusOK, map[string]any{"deleted": key})
}

// officerDefView is an officer as the admin API answers it: the store's row
// and whether MUSTER_OFFICERS declares it on the built-in backend (only then
// does anything run it).
type officerDefView struct {
	builtin.DefInfo
	Declared bool `json:"declared"`
}

func (s *Server) defView(d builtin.DefInfo) officerDefView {
	v := officerDefView{DefInfo: d}
	if s.officerRT != nil {
		if o, err := s.officerRT.reg.Get(d.Officer); err == nil && o.IsBuiltin() {
			v.Declared = true
		}
	}
	return v
}

func (s *Server) handleOfficerDefs(w http.ResponseWriter, r *http.Request) {
	list, err := s.officerStore.ListDefs(r.Context(), adminOwner(r))
	if err != nil {
		s.adminStoreErr(w, "officer", err)
		return
	}
	out := make([]officerDefView, 0, len(list))
	for _, d := range list {
		out = append(out, s.defView(d))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"defs": out})
}

func (s *Server) handleOfficerDefPut(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Enabled    *bool   `json:"enabled"`
		Mode       string  `json:"mode"`
		EscalateTo []int64 `json:"escalate_to"`
	}
	if !s.decodeAdmin(w, r, &in) {
		return
	}
	on := true
	if in.Enabled != nil {
		on = *in.Enabled
	}
	for _, u := range in.EscalateTo {
		if u <= 0 {
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "escalate_to holds user ids (positive numbers)"})
			return
		}
	}
	// Every recipient must be a user muster can ask: an id that is not one
	// would be an escalation nobody can ever receive. Read in the system
	// scope — users is self-read under row-level security — and answering
	// only which of the caller's own ids are missing.
	if missing, err := s.officerStore.UsersExist(authz.System(r.Context()), in.EscalateTo); err != nil {
		s.adminStoreErr(w, "officer", err)
		return
	} else if len(missing) > 0 {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "escalate_to names users that do not exist", "missing": missing})
		return
	}
	d, err := s.officerStore.PutDefReturning(r.Context(), adminOwner(r), builtin.Def{Officer: r.PathValue("officer"), Enabled: on, Mode: in.Mode, EscalateTo: in.EscalateTo})
	if err != nil {
		s.adminStoreErr(w, "officer type (define it first: PUT "+officerAdminPrefix+"/types/<type>)", err)
		return
	}
	s.logger.Printf("officers: user %d put officer %s (enabled %v, mode %s)", adminOwner(r), d.Officer, d.Enabled, d.Mode)
	s.writeJSON(w, http.StatusOK, s.defView(d))
}

func (s *Server) handleOfficerDefDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("officer")
	if err := s.officerStore.DeleteDef(r.Context(), adminOwner(r), id); err != nil {
		s.adminStoreErr(w, "officer", err)
		return
	}
	s.logger.Printf("officers: user %d deleted officer %s", adminOwner(r), id)
	s.writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

func (s *Server) handleOfficerItems(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	status := r.URL.Query().Get("status")
	if status != "" && !validItemStatus(status) {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "status must be one of " + strings.Join(officers.ItemStatuses, ", ")})
		return
	}
	list, err := s.officerStore.ListItems(r.Context(), adminOwner(r), r.PathValue("officer"), status, limit)
	if err != nil {
		s.adminStoreErr(w, "officer", err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"items": list})
}

func validItemStatus(s string) bool {
	for _, v := range officers.ItemStatuses {
		if v == s {
			return true
		}
	}
	return false
}

func (s *Server) handleOfficerItemEnqueue(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Kind       string          `json:"kind"`
		SubjectKey string          `json:"subject_key"`
		Payload    json.RawMessage `json:"payload"`
		Tier       string          `json:"tier"`
		Priority   string          `json:"priority"`
		NotBefore  *time.Time      `json:"not_before"`
	}
	if !s.decodeAdmin(w, r, &in) {
		return
	}
	officer := r.PathValue("officer")
	id, created, err := s.officerStore.EnqueueChecked(r.Context(), adminOwner(r), builtin.NewItem{Officer: officer, Kind: in.Kind, SubjectKey: in.SubjectKey,
		Payload: in.Payload, Tier: in.Tier, Priority: in.Priority, NotBefore: in.NotBefore}, s.officerNow())
	if err != nil {
		s.adminStoreErr(w, "officer", err)
		return
	}
	code := http.StatusOK
	if created {
		code = http.StatusCreated
		s.logger.Printf("officers: user %d enqueued item %d (%s %s) for %s", adminOwner(r), id, in.Kind, in.SubjectKey, officer)
	}
	s.writeJSON(w, code, map[string]any{"id": id, "created": created})
}

func (s *Server) handleOfficerItemResolve(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such item"})
		return
	}
	var in struct {
		Outcome string `json:"outcome"`
		Summary string `json:"summary"`
	}
	if !s.decodeAdmin(w, r, &in) {
		return
	}
	officer := r.PathValue("officer")
	raw, err := s.officerStore.ResolveItem(r.Context(), adminOwner(r), officer, id, in.Outcome, in.Summary, s.officerNow())
	if err != nil {
		s.adminStoreErr(w, "item", err)
		return
	}
	s.logger.Printf("officers: user %d resolved item %d of %s (%s)", adminOwner(r), id, officer, in.Outcome)
	s.writeJSON(w, http.StatusOK, map[string]any{"item": json.RawMessage(raw)})
}

func (s *Server) handleOfficerSchedules(w http.ResponseWriter, r *http.Request) {
	// An officer the caller does not define is a 404, as on PUT and DELETE.
	if _, err := s.officerStore.GetDef(r.Context(), adminOwner(r), r.PathValue("officer")); err != nil {
		s.adminStoreErr(w, "officer", err)
		return
	}
	list, err := s.officerStore.ListSchedules(r.Context(), adminOwner(r), r.PathValue("officer"))
	if err != nil {
		s.adminStoreErr(w, "officer", err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"schedules": list})
}

func (s *Server) handleOfficerSchedulePut(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Cron       string          `json:"cron"`
		Kind       string          `json:"kind"`
		SubjectKey string          `json:"subject_key"`
		Payload    json.RawMessage `json:"payload"`
		Tier       string          `json:"tier"`
		Priority   string          `json:"priority"`
		SkipIfLive *bool           `json:"skip_if_live"`
		Enabled    *bool           `json:"enabled"`
	}
	if !s.decodeAdmin(w, r, &in) {
		return
	}
	skip, on := true, true
	if in.SkipIfLive != nil {
		skip = *in.SkipIfLive
	}
	if in.Enabled != nil {
		on = *in.Enabled
	}
	officer := r.PathValue("officer")
	sc, err := s.officerStore.PutSchedule(r.Context(), adminOwner(r), officer, builtin.Schedule{Name: r.PathValue("name"), Cron: in.Cron, Kind: in.Kind,
		SubjectKey: in.SubjectKey, Payload: in.Payload, Tier: in.Tier, Priority: in.Priority, SkipIfLive: skip, Enabled: on}, s.officerNow())
	if err != nil {
		s.adminStoreErr(w, "officer", err)
		return
	}
	s.logger.Printf("officers: user %d put schedule %s of %s (%s, next %v)", adminOwner(r), sc.Name, officer, sc.Cron, sc.NextFireAt)
	s.writeJSON(w, http.StatusOK, sc)
}

func (s *Server) handleOfficerScheduleDelete(w http.ResponseWriter, r *http.Request) {
	officer, name := r.PathValue("officer"), r.PathValue("name")
	if err := s.officerStore.DeleteSchedule(r.Context(), adminOwner(r), officer, name); err != nil {
		s.adminStoreErr(w, "schedule", err)
		return
	}
	s.logger.Printf("officers: user %d deleted schedule %s of %s", adminOwner(r), name, officer)
	s.writeJSON(w, http.StatusOK, map[string]any{"deleted": name})
}

// RunOfficerScheduler fires the built-in store's due schedules every
// interval until ctx ends (builtin.Store.RunScheduler); nothing while the
// store is off.
func (s *Server) RunOfficerScheduler(ctx context.Context, interval time.Duration) {
	if s.officerStore == nil {
		return
	}
	s.officerStore.RunScheduler(ctx, interval, authz.System, s.officerNow, s.logger.Printf)
}
