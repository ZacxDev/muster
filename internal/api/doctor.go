package api

import (
	"context"
	"net/http"
	"sort"
	"time"
)

// DoctorPath is the server half of `muster doctor` (cmd/muster/doctor.go): the
// facts only the server can state about itself, as one JSON document.
const DoctorPath = "/api/doctor"

// DoctorReport is GET /api/doctor's body.
//
// 🔴 EVERY FIELD IS READ FROM THE SAME STATE THE FEATURE ITSELF READS, NOT FROM
// THE CONFIGURATION THAT WAS MEANT TO PRODUCE IT. Push is "the push routes
// would serve" (pushServed), dictation is "a microphone would be drawn"
// (dictatePath), the schema is the database's own schema_migrations, and each
// host's Online is the tunnel hub's — so a doctor that says PASS and a phone
// that shows the feature cannot disagree about whether it exists.
//
// ⚠ IT CARRIES NO SECRET AND NO PER-USER CONTENT: booleans, version numbers and
// the viewing principal's own host names. The route is still behind the armed
// hook token (or a personal token), so an unarmed server answers 503 with
// HookUnarmedField — which is itself what the doctor reports as "hook token:
// FAIL", read from the server rather than guessed by the client.
type DoctorReport struct {
	Version string `json:"version"`
	// AuthMode is "password" or "gotrue".
	AuthMode string `json:"auth_mode"`
	// HookTokenArmed: MUSTER_HOOK_TOKEN is set and usable (HookArmingRefusal
	// is empty). Stated even though a hook-token caller only gets here on an
	// armed server, because a PERSONAL token is admitted either way and must
	// not read as proof the shared machine tier is closed.
	HookTokenArmed bool `json:"hook_token_armed"`
	// Schema is the database's version against the migrations this binary
	// embeds; nil when the server has no database. SchemaError is why it could
	// not be read ("" when it was).
	Schema      *DoctorSchema `json:"schema"`
	SchemaError string        `json:"schema_error,omitempty"`
	// Push: the Web Push routes serve (the VAPID triple and a database).
	Push bool `json:"push"`
	// Dictation: a message box draws a microphone (MUSTER_STT_TOKEN set).
	// Whether the service actually transcribes is the client's check: it
	// sends a sample through POST /transcribe.
	Dictation bool `json:"dictation"`
	// PublicURL is MUSTER_PUBLIC_URL as configured ("" = unset).
	PublicURL string       `json:"public_url"`
	Mirror    DoctorMirror `json:"mirror"`
}

// DoctorSchema is DoctorReport.Schema.
type DoctorSchema struct {
	Applied int  `json:"applied"`
	Latest  int  `json:"latest"`
	Current bool `json:"current"`
}

// DoctorMirror is the mirror plugin as the viewing principal sees it.
type DoctorMirror struct {
	// Enabled: MUSTER_MIRROR=1 with a database (the plugin is wired).
	Enabled bool `json:"enabled"`
	// Hosts is every host the principal holds a live token for, plus any host
	// the hub lists that has none (a token revoked while it was connected),
	// sorted by name. Empty, not null, when there are none.
	Hosts []DoctorHost `json:"hosts"`
	// HostsError is why the token list could not be read ("" when it was).
	HostsError string `json:"hosts_error,omitempty"`
}

// DoctorHost is one mirrored host.
type DoctorHost struct {
	Host string `json:"host"`
	// Token: the principal holds an unrevoked mirror token for it.
	Token bool `json:"token"`
	// Online: its muster-mirror is connected now (the hub's view).
	Online bool `json:"online"`
	// Interactive: it advertised -interactive.
	Interactive bool `json:"interactive"`
}

// SchemaCheck reads the database's schema version: applied (its newest
// recorded migration) and latest (the newest this binary embeds).
type SchemaCheck func(ctx context.Context) (applied, latest int, err error)

// SetSchemaCheck installs GET /api/doctor's schema read (db.SchemaStatus).
// Unset, the report's schema is null: a server with no database.
func (s *Server) SetSchemaCheck(fn SchemaCheck) { s.schemaCheck = fn }

func (s *Server) registerDoctorRoutes(mux Mux) {
	mux.HandleFunc("GET "+DoctorPath, s.requireArmedHookToken(s.handleDoctor))
}

func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rep := DoctorReport{
		Version:        BuildVersion,
		AuthMode:       "password",
		HookTokenArmed: s.auth.HookArmingRefusal() == "",
		Push:           s.pushServed(),
		Dictation:      s.dictatePath() != "",
		PublicURL:      s.auth.PublicURL,
		Mirror:         DoctorMirror{Hosts: []DoctorHost{}},
	}
	if s.auth.goTrueMode() {
		rep.AuthMode = "gotrue"
	}
	if s.schemaCheck != nil {
		applied, latest, err := s.schemaCheck(ctx)
		if err != nil {
			s.logger.Printf("doctor: schema status: %v", err)
			rep.SchemaError = "the schema version could not be read from the database"
		} else {
			rep.Schema = &DoctorSchema{Applied: applied, Latest: latest, Current: applied == latest}
		}
	}
	if s.ext.Mirror != nil {
		rep.Mirror.Enabled = true
		rep.Mirror.Hosts, rep.Mirror.HostsError = s.doctorHosts(ctx)
	}
	s.writeJSON(w, http.StatusOK, rep)
}

// doctorHosts joins the hosts the principal holds live tokens for with the
// hosts the hub lists, so a host that never connected is reported (offline)
// rather than absent.
func (s *Server) doctorHosts(ctx context.Context) ([]DoctorHost, string) {
	byHost := map[string]*DoctorHost{}
	var order []string
	add := func(h string) *DoctorHost {
		if d, ok := byHost[h]; ok {
			return d
		}
		byHost[h] = &DoctorHost{Host: h}
		order = append(order, h)
		return byHost[h]
	}
	why := ""
	live, err := s.ext.Mirror.LiveTokenHosts(ctx)
	if err != nil {
		s.logger.Printf("doctor: mirror tokens: %v", err)
		why = "the mirror token list could not be read"
	}
	for _, h := range live {
		add(h).Token = true
	}
	for _, hv := range s.ext.Mirror.Hosts(ctx) {
		d := add(hv.Host)
		d.Online = hv.Online
		d.Interactive = hv.Interactive
	}
	sort.Strings(order)
	out := make([]DoctorHost, 0, len(order))
	for _, h := range order {
		out = append(out, *byHost[h])
	}
	return out, why
}
