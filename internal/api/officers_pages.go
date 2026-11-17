package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// THE OFFICERS PAGES (officers redesign O-C; docs/officers-builtin.md "The
// Officers page")
//
//	GET  /officers                              the signed-in person's officers, and their teams'
//	GET  /officers/new                          the New-officer wizard
//	POST /officers/new                          create (createDeclared)
//	GET  /officers/{officer}                    one officer: queue, instances, asks, drafts, schedules, last pass
//	GET  /officers/{officer}/edit               its declaration as a form
//	POST /officers/{officer}/edit               replace (updateDeclared)
//	POST /officers/{officer}/pause|resume       its switch (switchDeclared)
//	POST /officers/{officer}/mode               Shadow <-> Live (updateDeclared, the record unchanged but its mode)
//	POST /officers/{officer}/run                "Run now": one item in its queue
//	POST /officers/{officer}/delete             delete (deleteDeclared)
//	POST /officers/{officer}/schedules          add or replace a schedule
//	POST /officers/{officer}/schedules/{name}/delete
//
// 🔴 A DOOR, NOT A SECOND IMPLEMENTATION. Every write goes through the same
// core the JSON admin API does (officer_declared.go's createDeclared and the
// rest), so a declaration the page accepts is one `muster officer create`
// would, refused with the same per-field reasons.
//
// 🔴 WHOSE OFFICER: `owner=` names a TEAM (in the query, and in each form);
// absent, it is the signed-in person's own. officerScope reads the role from
// the principal's own memberships (TeamScope): a member reads a team's
// officers, only a MANAGER writes them, and Postgres refuses anyone else again
// (officer_defs and officer_types are managed: 0127). A team the person is not
// in answers exactly what a missing officer does. One person never reaches
// another's officers: every read and write names the scope's owner and runs
// under the person's own principal (row-level security).
//
// 🔴 MUSTER_OFFICERS WINS. An env officer is drawn read-only (its fields as
// the environment declares them) and every write to it is refused by the core
// (errEnvManaged), whatever the form says.
//
// ⚠ NOT A TAILWIND CONTENT ENTRY, like team_officers_pages.go: every class
// written here is also written under internal/ui or a scanned api file.
// Measured by adding this file to tailwind.config.js's content: the build
// gained only `shadow` (the WORD, from "Shadow" mode text — a leak, not a
// class this page uses). A class only this file writes renders unstyled, so
// a new one needs that entry and a css-check control.
// ---------------------------------------------------------------------------

const officersPagePath = "/officers"

func (s *Server) registerOfficerPages(mux Mux) {
	mux.HandleFunc("GET "+officersPagePath, s.requireSession(s.handleOfficersPage))
	mux.HandleFunc("GET "+officersPagePath+"/new", s.requireSession(s.handleOfficerNewPage))
	mux.HandleFunc("POST "+officersPagePath+"/new", s.requireOperatorWrite(s.handleOfficerCreate))
	mux.HandleFunc("GET "+officersPagePath+"/{officer}", s.requireSession(s.handleOfficerPage))
	mux.HandleFunc("GET "+officersPagePath+"/{officer}/edit", s.requireSession(s.handleOfficerEditPage))
	mux.HandleFunc("POST "+officersPagePath+"/{officer}/edit", s.requireOperatorWrite(s.handleOfficerEdit))
	mux.HandleFunc("POST "+officersPagePath+"/{officer}/pause", s.requireOperatorWrite(s.handleOfficerPause))
	mux.HandleFunc("POST "+officersPagePath+"/{officer}/resume", s.requireOperatorWrite(s.handleOfficerResume))
	mux.HandleFunc("POST "+officersPagePath+"/{officer}/mode", s.requireOperatorWrite(s.handleOfficerMode))
	mux.HandleFunc("POST "+officersPagePath+"/{officer}/run", s.requireOperatorWrite(s.handleOfficerRunNow))
	mux.HandleFunc("POST "+officersPagePath+"/{officer}/delete", s.requireOperatorWrite(s.handleOfficerDeletePage))
	mux.HandleFunc("POST "+officersPagePath+"/{officer}/schedules", s.requireOperatorWrite(s.handleOfficerSchedulePage))
	mux.HandleFunc("POST "+officersPagePath+"/{officer}/schedules/{name}/delete", s.requireOperatorWrite(s.handleOfficerScheduleDeletePage))
}

// officersOn reports whether this server keeps officers at all (the built-in
// store): the nav entry and every page here need it.
func (s *Server) officersOn() bool { return s.officerStore != nil }

// --- whose officers -------------------------------------------------------------

// officerScope is whose officers a request reads or writes, and what the
// person may do there.
type officerScope struct {
	// Owner is the officers' owner: the person, or a team they belong to.
	Owner int64
	// Team: Owner is a team; Name is then its name.
	Team bool
	Name string
	// Manage: the person may change these officers (their own, or a team
	// they manage).
	Manage bool
}

// ownerQuery is the scope as a URL's query ("" for the person's own).
func (sc officerScope) ownerQuery() string {
	if !sc.Team {
		return ""
	}
	return "?owner=" + strconv.FormatInt(sc.Owner, 10)
}

// ownerField is the scope as a form's hidden field ("" for the person's own).
func (sc officerScope) ownerField() string {
	if !sc.Team {
		return ""
	}
	return `<input type="hidden" name="owner" value="` + strconv.FormatInt(sc.Owner, 10) + `">`
}

// officerHref is officer id's page (suffix: "" or "/edit", …) in scope sc.
func officerHref(sc officerScope, id, suffix string) string {
	return officersPagePath + "/" + url.PathEscape(id) + suffix + sc.ownerQuery()
}

// officerNotFound is what a missing officer, a team the person is not in and
// another person's officer all answer: the same words, naming nothing the
// request named.
const officerNotFound = "There is no officer here that you can see."

// scopeFor resolves the owner named by `owner` (query or form) for the
// person. ok false: the store is off, or the owner is not theirs to read —
// answered already.
func (s *Server) scopeFor(w http.ResponseWriter, r *http.Request) (officerScope, bool) {
	if !s.officersOn() {
		s.renderNotice(w, r, http.StatusNotFound, "No officers here", "This muster keeps no officers: they need a database (DATABASE_URL).")
		return officerScope{}, false
	}
	p := authz.MustFrom(r.Context())
	raw := strings.TrimSpace(r.FormValue("owner"))
	if raw == "" || raw == strconv.FormatInt(p.UserID, 10) {
		return officerScope{Owner: p.UserID, Manage: true}, true
	}
	team, err := strconv.ParseInt(raw, 10, 64)
	if err == nil && team > 0 {
		if role, in := p.TeamScope().RoleIn(team); in {
			return officerScope{Owner: team, Team: true, Name: s.teamNames(r.Context(), []int64{team})[team], Manage: role == authz.TeamManager}, true
		}
	}
	s.renderNotice(w, r, http.StatusNotFound, "No such officer", officerNotFound)
	return officerScope{}, false
}

// managedScopes is every scope the person may create an officer in: their
// own, then each team they manage.
func (s *Server) managedScopes(ctx context.Context) []officerScope {
	p := authz.MustFrom(ctx)
	out := []officerScope{{Owner: p.UserID, Manage: true}}
	var teams []int64
	for _, m := range p.TeamScope().List() {
		if m.Role == authz.TeamManager {
			teams = append(teams, m.Team)
		}
	}
	names := s.teamNames(ctx, teams)
	for _, t := range teams {
		out = append(out, officerScope{Owner: t, Team: true, Name: names[t], Manage: true})
	}
	return out
}

// --- what an officer is, read for the pages -------------------------------------

// ManagedByDef is an officer that is only a def in the built-in store: no
// runtime is declared in the database and MUSTER_OFFICERS does not run it.
const managedByDef = "def"

// officerViews is owner's officers as the declared-officer API answers them
// (env officers read-only, then DB-declared ones), sorted by id; withDefs adds
// each def that neither declares (managedByDef), read-only, so the page
// never hides an officer row its owner has.
func (s *Server) officerViews(ctx context.Context, owner int64, withDefs bool) ([]declaredOfficerView, error) {
	rt := s.officerRT
	defs := map[string]builtin.DefInfo{}
	list, err := s.officerStore.ListDefs(ctx, owner)
	if err != nil && withDefs {
		return nil, err
	}
	for _, d := range list {
		defs[d.Officer] = d
	}
	out := []declaredOfficerView{}
	seen := map[string]bool{}
	if rt != nil {
		for _, o := range rt.env {
			if o.Owner != owner {
				continue
			}
			var dp *builtin.DefInfo
			if d, ok := defs[o.ID]; ok && o.IsBuiltin() {
				dp = &d
			}
			out = append(out, viewOfEnv(o, dp))
			seen[o.ID] = true
		}
	}
	rows, err := s.officerStore.ListDeclaredFor(ctx, owner)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		out = append(out, viewOfDeclared(s.stateOf(row)))
		seen[row.Officer] = true
	}
	if withDefs {
		for _, d := range list {
			if seen[d.Officer] {
				continue
			}
			paused := !d.Enabled
			out = append(out, declaredOfficerView{ID: d.Officer, Owner: owner, ManagedBy: managedByDef, ReadOnly: true, Backend: officers.BackendBuiltin,
				Reason:     "nothing runs it: it is a def in the built-in store with no runtime declared in the database, and MUSTER_OFFICERS does not declare it",
				Connectors: []string{}, ReadRepos: []string{}, Instances: []string{}, Paused: &paused, Mode: d.Mode, EscalateTo: d.EscalateTo})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ID < out[j].ID || out[i].ID == out[j].ID && out[i].ManagedBy > out[j].ManagedBy
	})
	return out, nil
}

// officerView is owner's officer id as officerViews answers it.
func (s *Server) officerView(ctx context.Context, owner int64, id string) (declaredOfficerView, bool, error) {
	list, err := s.officerViews(ctx, owner, true)
	if err != nil {
		return declaredOfficerView{}, false, err
	}
	for _, v := range list {
		if v.ID == id {
			return v, true, nil
		}
	}
	return declaredOfficerView{}, false, nil
}

// officerStatus is an officer's one status: misconfigured (it does not run,
// and Reason says why), paused, shadow or live.
func officerStatus(v declaredOfficerView) string {
	switch {
	case !v.Running:
		return "misconfigured"
	case v.Paused != nil && *v.Paused:
		return "paused"
	case v.Mode == "shadow":
		return "shadow"
	}
	return "live"
}

// statusChip is status drawn as a chip, with the words a person reads.
func statusChip(status string) string {
	label, tone := status, "bg-s2 text-fg2"
	switch status {
	case "live":
		label, tone = "Live", "bg-st-running-bg text-st-running-fg"
	case "shadow":
		label, tone = "Shadow", "bg-st-review-bg text-st-review-fg"
	case "paused":
		label, tone = "Paused", "bg-s2 text-fg2"
	case "misconfigured":
		label, tone = "Not running", "bg-st-error-bg text-st-error-fg"
	}
	return `<span class="inline-flex items-center rounded-full px-2 py-0.5 text-xs font-semibold ` + tone + `" data-officer-status="` + status + `">` + label + `</span>`
}

// whereItRuns is where an officer runs (or is declared to), in words.
func whereItRuns(v declaredOfficerView) string {
	on := v.RunsOn
	host := v.RunsOnHost
	if on == "" {
		on, host = v.Runtime, v.Host
	}
	switch on {
	case officers.RuntimePod:
		return "in a pod"
	case officers.RuntimeHost:
		if host != "" {
			return "on " + host + " (a Claude Code window)"
		}
		return "on a host"
	case officers.RuntimeAuto, "":
		if v.ManagedBy == managedByDef {
			return "nowhere"
		}
		return "wherever it can (auto)"
	}
	return on
}

// liveItem reports whether an item status is still the officer's to work.
func liveItem(status string) bool {
	return status == officers.StatusOpen || status == officers.StatusClaimed || status == officers.StatusWaiting
}

// pageItem is the part of an item (builtin.itemRow's JSON) the pages draw.
type pageItem struct {
	ID         int64     `json:"id"`
	Kind       string    `json:"kind"`
	SubjectKey string    `json:"subject_key"`
	Status     string    `json:"status"`
	Priority   string    `json:"priority"`
	ClaimedBy  string    `json:"claimed_by"`
	Outcome    string    `json:"outcome"`
	Summary    string    `json:"summary"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// officerItems is owner's officer id's newest items (at most limit).
func (s *Server) officerItems(ctx context.Context, owner int64, id string, limit int) ([]pageItem, error) {
	raws, err := s.officerStore.ListItems(ctx, owner, id, "", limit)
	if err != nil {
		return nil, err
	}
	out := make([]pageItem, 0, len(raws))
	for _, raw := range raws {
		var it pageItem
		if json.Unmarshal(raw, &it) == nil {
			out = append(out, it)
		}
	}
	return out, nil
}

// raisedByOfficer is the open Attention asks owner's officer id raised (its
// label, officerAskLabel), from open, the person's open asks.
func raisedByOfficer(open []attention.Ask, owner int64, id string) []attention.Ask {
	var out []attention.Ask
	for _, a := range open {
		if a.Owner == owner && a.Asker.System && a.Asker.Label == officerAskLabel(id) {
			out = append(out, a)
		}
	}
	return out
}

// openAsks is the person's open Attention asks, or none (no store, a failed
// read: the pages then draw no asks rather than fail).
func (s *Server) openAsks(ctx context.Context) []attention.Ask {
	if s.ext.Attention == nil {
		return nil
	}
	list, err := s.ext.Attention.Open(ctx)
	if err != nil {
		s.logger.Printf("officers: read open asks for the officers page: %v", err)
		return nil
	}
	return list
}

// lastPass is the wake loop's last pass for owner's officer id on THIS
// server, when it has run one.
func (s *Server) lastPass(owner int64, id string) (officers.PassInfo, bool) {
	rt := s.officerRT
	if rt == nil || rt.reg == nil || rt.rec == nil {
		return officers.PassInfo{}, false
	}
	o, ok := rt.reg.ForOwner(owner, id)
	if !ok {
		return officers.PassInfo{}, false
	}
	return rt.rec.LastPass(o.Key())
}

// --- the list -------------------------------------------------------------------

func (s *Server) handleOfficersPage(w http.ResponseWriter, r *http.Request) {
	if !s.officersOn() {
		s.renderNotice(w, r, http.StatusNotFound, "No officers here", "This muster keeps no officers: they need a database (DATABASE_URL).")
		return
	}
	ctx := r.Context()
	p := authz.MustFrom(ctx)
	mine, err := s.officerViews(ctx, p.UserID, true)
	if err != nil {
		s.logger.Printf("officers: read user %d's officers: %v", p.UserID, err)
		s.renderNotice(w, r, http.StatusInternalServerError, "Could not load your officers", "Try again in a moment.")
		return
	}
	open := s.openAsks(ctx)
	var b strings.Builder
	b.WriteString(`<section class="` + adminCard + `" id="officers-mine"><div class="flex flex-wrap items-center gap-2"><h1 class="flex-1 text-base font-semibold text-fg">Officers</h1>
<a href="` + officersPagePath + `/new" id="officer-new" class="` + adminButton + ` inline-flex items-center">New officer</a></div>`)
	if len(mine) == 0 {
		b.WriteString(`<p class="text-sm text-fg2" id="officers-empty">An officer is a Claude Code agent with standing instructions that muster wakes when there is work for it — on a schedule, or when you hand it something — and that asks you before anything leaves.</p>`)
	} else {
		b.WriteString(s.officerList(ctx, officerScope{Owner: p.UserID, Manage: true}, mine, open))
	}
	b.WriteString(`</section>`)
	// Each team the person is in: its officers, read by every member.
	var teams []int64
	roles := map[int64]authz.TeamRole{}
	for _, m := range p.TeamScope().List() {
		teams = append(teams, m.Team)
		roles[m.Team] = m.Role
	}
	names := s.teamNames(ctx, teams)
	for _, t := range teams {
		list, err := s.officerViews(ctx, t, true)
		if err != nil {
			s.logger.Printf("officers: read team %d's officers: %v", t, err)
			continue
		}
		manage := roles[t] == authz.TeamManager
		if len(list) == 0 && !manage {
			continue
		}
		sc := officerScope{Owner: t, Team: true, Name: names[t], Manage: manage}
		id := strconv.FormatInt(t, 10)
		b.WriteString(`<section class="` + adminCard + `" id="officers-team-` + id + `" data-officers-team="` + id + `"><div class="flex flex-wrap items-center gap-2"><h2 class="flex-1 text-base font-semibold text-fg">` +
			html.EscapeString(sc.Name) + `'s officers</h2>`)
		if manage {
			b.WriteString(`<a href="` + officersPagePath + `/new` + sc.ownerQuery() + `" class="` + adminQuiet + ` inline-flex items-center">New team officer</a>`)
		}
		b.WriteString(`</div>`)
		if len(list) == 0 {
			b.WriteString(`<p class="text-sm text-fg2">This team has no officers yet.</p>`)
		} else {
			b.WriteString(s.officerList(ctx, sc, list, open))
		}
		b.WriteString(`</section>`)
	}
	b.WriteString(s.runtimeAvailability(ctx, p.UserID, "officers-runtimes"))
	s.writeNavDocument(w, r, http.StatusOK, "Officers", ui.NavOfficers, false, `<div class="flex flex-col gap-4">`+b.String()+`</div>`)
}

// officerList is the officers in list, each one row: name, where it runs, its
// status, its last pass, and what is open on it.
func (s *Server) officerList(ctx context.Context, sc officerScope, list []declaredOfficerView, open []attention.Ask) string {
	var b strings.Builder
	b.WriteString(`<ul class="flex flex-col divide-y divide-line" data-officer-list>`)
	for _, v := range list {
		status := officerStatus(v)
		name := v.Name
		if name == "" {
			name = v.ID
		}
		openItems := 0
		if v.Backend == officers.BackendBuiltin {
			if items, err := s.officerItems(ctx, sc.Owner, v.ID, 100); err == nil {
				for _, it := range items {
					if liveItem(it.Status) {
						openItems++
					}
				}
			}
		}
		asks := len(raisedByOfficer(open, sc.Owner, v.ID))
		b.WriteString(`<li class="flex flex-col gap-1 py-3" data-officer="` + html.EscapeString(v.ID) + `" data-managed-by="` + v.ManagedBy + `">
<div class="flex flex-wrap items-center gap-2"><a class="min-w-0 flex-1 truncate text-sm font-semibold text-fg underline-offset-2 hover:underline" href="` + officerHref(sc, v.ID, "") + `">` +
			html.EscapeString(name) + `</a>` + statusChip(status))
		if v.ReadOnly {
			b.WriteString(`<span class="text-xs text-muted" data-read-only>read-only</span>`)
		}
		b.WriteString(`</div><span class="text-xs text-fg2">` + html.EscapeString(v.ID) + ` · runs ` + html.EscapeString(whereItRuns(v)) + `</span>`)
		var facts []string
		if lp, ok := s.lastPass(sc.Owner, v.ID); ok {
			f := "last pass " + fmtTime(lp.At)
			if lp.Err != "" {
				f += " (failed)"
			}
			facts = append(facts, f)
		} else if v.Running {
			facts = append(facts, "no pass on this server yet")
		}
		if v.Backend == officers.BackendBuiltin {
			facts = append(facts, plural(openItems, "open item"))
		}
		facts = append(facts, plural(asks, "ask"))
		b.WriteString(`<span class="text-xs text-fg2" data-officer-facts>` + html.EscapeString(strings.Join(facts, " · ")) + `</span>`)
		if !v.Running && v.Reason != "" {
			b.WriteString(`<span class="text-xs text-st-error-fg" data-officer-reason>` + html.EscapeString(v.Reason) + `</span>`)
		}
		b.WriteString(`</li>`)
	}
	b.WriteString(`</ul>`)
	return b.String()
}

// runtimeAvailability draws where an officer of owner's could run on this
// install now, and why not — read from the server's own state
// (runtimeOptionsFor: the agent plane, the mirror hub, each host's readiness,
// what auto picks), never guessed.
func (s *Server) runtimeAvailability(ctx context.Context, owner int64, id string) string {
	opts := s.runtimeOptionsFor(ctx, owner, "")
	var b strings.Builder
	b.WriteString(`<section class="` + adminCard + `" id="` + id + `"><h2 class="text-base font-semibold text-fg">Where officers can run here</h2><ul class="flex flex-col gap-2 text-sm">`)
	if opts.Pod.Available {
		b.WriteString(`<li data-runtime="pod" data-available="true"><span class="font-semibold text-fg">Pods:</span> <span class="text-fg2">available — each instance runs in its own locked-down pod.</span></li>`)
	} else {
		b.WriteString(`<li data-runtime="pod" data-available="false"><span class="font-semibold text-fg">Pods:</span> <span class="text-fg2">not available: ` + html.EscapeString(opts.Pod.Reason) + `.</span></li>`)
	}
	switch {
	case opts.HostReason != "":
		b.WriteString(`<li data-runtime="host" data-available="false"><span class="font-semibold text-fg">Your machines:</span> <span class="text-fg2">not available: ` + html.EscapeString(opts.HostReason) + `.</span></li>`)
	case len(opts.Hosts) == 0:
		b.WriteString(`<li data-runtime="host" data-available="false"><span class="font-semibold text-fg">Your machines:</span> <span class="text-fg2">none is mirrored. Run muster-mirror with -interactive -agent-spawn on a machine you own, and it appears here.</span></li>`)
	default:
		b.WriteString(`<li data-runtime="host"><span class="font-semibold text-fg">Your machines:</span><ul class="mt-1 flex flex-col gap-1">`)
		for _, h := range opts.Hosts {
			why := "ready: it can start and close Claude Code windows"
			if !h.Qualifies {
				why = "not ready: " + h.Reason
			}
			b.WriteString(`<li class="text-fg2" data-host="` + html.EscapeString(h.Host) + `" data-qualifies="` + strconv.FormatBool(h.Qualifies) + `"><span class="text-fg">` +
				html.EscapeString(h.Host) + `</span> — ` + html.EscapeString(why) + `</li>`)
		}
		b.WriteString(`</ul></li>`)
	}
	b.WriteString(`<li data-runtime="auto"><span class="font-semibold text-fg">A new officer would run:</span> <span class="text-fg2" id="` + id + `-auto">` + html.EscapeString(autoWords(opts)) + `</span></li>`)
	b.WriteString(`</ul></section>`)
	return b.String()
}

// autoWords is what "auto" picks now, in words.
func autoWords(opts runtimeOptions) string {
	switch opts.Auto.Runtime {
	case officers.RuntimePod:
		return "in a pod"
	case officers.RuntimeHost:
		return "on " + opts.Auto.Host + ", as a Claude Code window"
	}
	// What auto found is the pod and host lines' own reasons together, so
	// they are not repeated here.
	return "nowhere yet: neither a pod nor one of your machines is available here"
}

// --- the form -------------------------------------------------------------------

// officerTemplate is a starting point the wizard's first step offers.
//
// 🔴 THE SEAM O-D FILLS. Templates are a catalogue (O-D); this slice has the
// one entry this PR ships, "Blank officer", and the wizard draws whatever it
// holds as the first step's choices. A template sets the form's starting
// values (formValues) and nothing else: everything it fills is still the
// person's to read and change, and the create goes through the one core.
type officerTemplate struct {
	Key, Name, Summary string
	// Values are the form's starting values ("" fields are blank).
	Values formValues
}

var officerTemplates = []officerTemplate{
	{Key: "blank", Name: "Blank officer", Summary: "Start from nothing: you write its instructions."},
}

// templateNamed is the template keyed key (the first when key is unknown).
func templateNamed(key string) officerTemplate {
	for _, t := range officerTemplates {
		if t.Key == key {
			return t
		}
	}
	return officerTemplates[0]
}

// formValues is the officer form's fields, as typed (or as stored, on edit).
type formValues struct {
	Template string
	Name, ID string
	Charter  string
	Runtime  string // auto, pod or host
	Host     string
	Repo     string
	// Wake is "schedule" or "manual" ("" until chosen); Preset a named cron or
	// "custom", Cron the custom expression.
	Wake, Preset, Cron string
	Caps               []string
	Connectors         string
	ReadRepos          string
	Escalate           []int64
	Mode               string
	// Paused is the stored switch (edit only: the form never changes it).
	Paused bool
	// Policy is the stored type policy (edit only), kept but for its
	// capabilities.
	Policy json.RawMessage
}

// schedulePresets are the wizard's named schedules (cron, UTC).
var schedulePresets = []struct{ Key, Label, Cron string }{
	{"hourly", "Every hour", "0 * * * *"},
	{"daily", "Every day at 09:00 UTC", "0 9 * * *"},
	{"weekdays", "Weekdays at 09:00 UTC", "0 9 * * 1-5"},
	{"weekly", "Mondays at 09:00 UTC", "0 9 * * 1"},
}

// presetCron is preset's cron ("" for custom or unknown).
func presetCron(key string) string {
	for _, p := range schedulePresets {
		if p.Key == key {
			return p.Cron
		}
	}
	return ""
}

// The capabilities the form offers (a type's policy), each least privilege:
// unticked by default.
var officerCaps = []struct{ Key, Label string }{
	{builtin.CapOutboundDrafts, "Draft posts and replies for your approval (outbound drafts through a bound plugin)"},
	{builtin.CapContacts, "Keep a record of the people it hears from (contacts)"},
}

// fieldErrs is a form's refusals, by field.
type fieldErrs map[string]string

// slugOf is name as an officer id half: lowercase letters and digits, single
// hyphens, at most 30 characters.
func slugOf(name string) string {
	var b strings.Builder
	dash := false
	for _, c := range strings.ToLower(name) {
		if b.Len() >= 30 {
			break
		}
		switch {
		case c >= 'a' && c <= 'z' || c >= '0' && c <= '9':
			b.WriteRune(c)
			dash = false
		case b.Len() > 0 && !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// splitList is a comma- or space-separated field as its entries.
func splitList(v string) []string {
	out := []string{}
	for _, f := range strings.FieldsFunc(v, func(c rune) bool { return c == ',' || c == ' ' || c == '\n' || c == '\r' || c == '\t' }) {
		out = append(out, f)
	}
	return out
}

// readForm is r's officer form.
func readForm(r *http.Request) formValues {
	f := formValues{Template: r.FormValue("template"), Name: strings.TrimSpace(r.FormValue("name")), ID: strings.TrimSpace(r.FormValue("id")),
		Charter: r.FormValue("charter"), Runtime: r.FormValue("runtime"), Host: r.FormValue("host"), Repo: strings.TrimSpace(r.FormValue("repo")),
		Wake: r.FormValue("wake"), Preset: r.FormValue("schedule_preset"), Cron: strings.TrimSpace(r.FormValue("cron")),
		Caps: r.Form["cap"], Connectors: r.FormValue("connectors"), ReadRepos: r.FormValue("read_repos"), Mode: r.FormValue("mode")}
	for _, v := range r.Form["escalate_to"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			f.Escalate = append(f.Escalate, id)
		}
	}
	return f
}

// policyWith is base (a type's policy, nil for none) with its capabilities
// set to caps (only the ones the form offers), every other key kept.
func policyWith(base json.RawMessage, caps []string) (json.RawMessage, error) {
	m := map[string]any{}
	if len(base) > 0 {
		if err := json.Unmarshal(base, &m); err != nil {
			return nil, err
		}
	}
	keep := []string{}
	for _, c := range officerCaps {
		for _, got := range caps {
			if got == c.Key {
				keep = append(keep, c.Key)
				break
			}
		}
	}
	// A capability the form does not offer, already granted, is kept: the
	// form changes only what it shows.
	if prev, ok := m["capabilities"].([]any); ok {
		for _, p := range prev {
			if ps, ok := p.(string); ok && !offeredCap(ps) {
				keep = append(keep, ps)
			}
		}
	}
	if len(keep) == 0 {
		delete(m, "capabilities")
	} else {
		m["capabilities"] = keep
	}
	if len(m) == 0 {
		return json.RawMessage(`{}`), nil
	}
	return json.Marshal(m)
}

func offeredCap(c string) bool {
	for _, o := range officerCaps {
		if o.Key == c {
			return true
		}
	}
	return false
}

// policyCaps is policy's capabilities.
func policyCaps(policy json.RawMessage) []string {
	var p struct {
		Capabilities []string `json:"capabilities"`
	}
	_ = json.Unmarshal(policy, &p)
	return p.Capabilities
}

// declaredInOf is the form f as a create's or replace's body, with the form's
// own refusals (the ones the core does not know: instructions, the wake
// choice, a schedule's cron).
func declaredInOf(f formValues, edit bool) (declaredIn, fieldErrs) {
	errs := fieldErrs{}
	id := f.ID
	if !edit && id == "" {
		if slug := slugOf(f.Name); slug != "" {
			id = slug + ":main"
		}
	}
	if f.Name == "" {
		errs["name"] = "Give it a name."
	}
	if strings.TrimSpace(f.Charter) == "" {
		errs["charter"] = "Tell it what to do: these are its standing instructions."
	}
	if !edit {
		switch f.Wake {
		case "manual":
		case "schedule":
			c := presetCron(f.Preset)
			if f.Preset == "custom" || c == "" {
				c = f.Cron
			}
			if c == "" {
				errs["cron"] = "Choose a schedule, or type one (five fields, UTC)."
			} else if _, err := builtin.ParseCron(c); err != nil {
				errs["cron"] = "That schedule is not one muster reads: " + err.Error()
			}
		default:
			// 🔴 F5: an officer nothing ever wakes is the failure this step
			// exists for — a schedule, or the explicit "only when I give it
			// work", never neither.
			errs["wake"] = "Choose when it works: on a schedule, or only when you give it work. Without either nothing ever wakes it."
		}
	}
	runtime := f.Runtime
	if runtime == "" {
		runtime = officers.RuntimeAuto
	}
	host := ""
	if runtime == officers.RuntimeHost {
		host = f.Host
	}
	mode := f.Mode
	if mode != "live" {
		mode = "shadow" // 🔴 Start in Shadow: the default, and what anything else means
	}
	paused := f.Paused
	in := declaredIn{ID: id, Name: f.Name, Charter: f.Charter, Runtime: runtime, Host: host, Repo: f.Repo,
		Connectors: splitList(f.Connectors), ReadRepos: splitList(f.ReadRepos), Mode: &mode, Paused: &paused, EscalateTo: f.Escalate}
	pol, err := policyWith(f.Policy, f.Caps)
	if err != nil {
		errs["cap"] = "Its stored policy is not JSON this form can keep; change it with `muster officer type put`."
	}
	in.Policy = pol
	return in, errs
}

// wizardCron is the cron the wizard's wake step names ("" for manual).
func wizardCron(f formValues) string {
	if f.Wake != "schedule" {
		return ""
	}
	if c := presetCron(f.Preset); f.Preset != "custom" && c != "" {
		return c
	}
	return f.Cron
}

// officerFieldErrs is a core's refusal as the form's field refusals (and the
// words for the top of the form when it names no field).
func officerFieldErrs(err error) (fieldErrs, string) {
	errs := fieldErrs{}
	var inv declaredInvalid
	if !errors.As(err, &inv) {
		return errs, ""
	}
	fs := officers.Fields(inv.err)
	if len(fs) == 0 {
		return errs, inv.err.Error()
	}
	top := ""
	for _, f := range fs {
		field := f.Field
		switch {
		case strings.HasPrefix(field, "connectors"):
			field = "connectors"
		case strings.HasPrefix(field, "read_repos"):
			field = "read_repos"
		case field == "escalate_to":
		case field == "idle_scale_to_zero_after", field == "poll_interval", field == "max_instances", field == "model":
			top = f.Err.Error()
			continue
		}
		if prev, ok := errs[field]; ok {
			errs[field] = prev + " " + f.Err.Error()
		} else {
			errs[field] = f.Err.Error()
		}
	}
	return errs, top
}

// officerPageRefusal is a core's error as a status and the words for a page.
func officerPageRefusal(err error) (int, string) {
	if errors.Is(err, errEnvManaged) {
		return http.StatusConflict, "MUSTER_OFFICERS declares this officer, so it is read-only here: change the environment and restart."
	}
	if code, msg, ok := declaredStoreRefusal(err); ok {
		if code == http.StatusNotFound {
			return code, officerNotFound
		}
		return code, strings.ToUpper(msg[:1]) + msg[1:] + "."
	}
	return http.StatusInternalServerError, "That change could not be made. Try again."
}

// formPage is everything the form page draws.
type formPage struct {
	sc     officerScope
	scopes []officerScope // the wizard's owner choices (one: no choice)
	f      formValues
	errs   fieldErrs
	top    string
	edit   bool
	id     string // edit: the officer's id
	opts   runtimeOptions
	people []escalationChoice
}

// escalationChoice is one person (or the team) an escalation can also ask.
type escalationChoice struct {
	ID    int64
	Label string
}

// escalationChoices are who an officer of sc's can also ask on an escalation
// (F6: chosen, never typed as numbers): the team itself (a team officer:
// one ask the team owns), then the people of every team the person MANAGES
// (whom they already see on that team's page) — never the owner, who is
// always asked. Ids already chosen that are none of these are kept, named by
// number, so an edit never drops one silently.
func (s *Server) escalationChoices(ctx context.Context, sc officerScope, chosen []int64) []escalationChoice {
	p := authz.MustFrom(ctx)
	seen := map[int64]bool{p.UserID: !sc.Team, sc.Owner: false}
	var out []escalationChoice
	if sc.Team {
		out = append(out, escalationChoice{ID: sc.Owner, Label: "The whole team (" + sc.Name + "): one ask any member can answer"})
		seen[sc.Owner] = true
	}
	if s.gt != nil && s.gt.Accounts != nil {
		var teams []int64
		for _, m := range p.TeamScope().List() {
			if m.Role == authz.TeamManager {
				teams = append(teams, m.Team)
			}
		}
		sort.Slice(teams, func(i, j int) bool { return teams[i] < teams[j] })
		for _, t := range teams {
			members, err := s.gt.Accounts.TeamMembers(ctx, t)
			if err != nil {
				continue
			}
			for _, m := range members {
				if seen[m.UserID] {
					continue
				}
				seen[m.UserID] = true
				label := personLabel(m.Email, m.DisplayName, m.UserID)
				if m.UserID == p.UserID {
					label += " (you)"
				}
				out = append(out, escalationChoice{ID: m.UserID, Label: label})
			}
		}
	}
	for _, c := range chosen {
		if !seen[c] {
			seen[c] = true
			out = append(out, escalationChoice{ID: c, Label: "user #" + strconv.FormatInt(c, 10)})
		}
	}
	return out
}

func checkedIf(on bool) string {
	if on {
		return " checked"
	}
	return ""
}

func fieldErrNode(errs fieldErrs, field string) string {
	msg, ok := errs[field]
	if !ok {
		return ""
	}
	return `<p class="text-sm text-st-error-fg" id="err-` + field + `" data-field-error="` + field + `">` + html.EscapeString(msg) + `</p>`
}

func describedBy(errs fieldErrs, field string) string {
	if _, ok := errs[field]; !ok {
		return ""
	}
	return ` aria-invalid="true" aria-describedby="err-` + field + `"`
}

// stepOfField is the wizard step a field is on, so a refused create opens
// at the first refusal.
func stepOfField(field string) int {
	switch field {
	case "name", "id", "charter", "owner":
		return 2
	case "runtime", "host", "repo":
		return 3
	case "wake", "cron":
		return 4
	case "connectors", "read_repos", "cap", "escalate_to":
		return 5
	}
	return 6
}

// The wizard's steps, in order (edit draws 2, 3, 5 and 6 as plain sections).
var wizardSteps = []string{"Start from", "Name and instructions", "Where it runs", "When it works", "What it may do", "Review"}

func (s *Server) renderOfficerForm(w http.ResponseWriter, r *http.Request, code int, fp formPage) {
	esc := html.EscapeString
	f, errs := fp.f, fp.errs
	action := officersPagePath + "/new"
	title := "New officer"
	if fp.edit {
		action = officerHref(fp.sc, fp.id, "/edit")
		title = "Edit officer"
	}
	first := 7
	for field := range errs {
		if st := stepOfField(field); st < first {
			first = st
		}
	}
	if first == 7 {
		first = 1
		if fp.top != "" {
			first = 6
		}
	}
	var b strings.Builder
	b.WriteString(`<section class="` + adminCard + `"><h1 class="text-base font-semibold text-fg">` + title + `</h1>`)
	if len(errs) > 0 || fp.top != "" {
		msg := "Some answers need another look: each is marked below."
		if fp.top != "" && len(errs) == 0 {
			msg = fp.top
		}
		b.WriteString(problemNode(msg))
	}
	b.WriteString(`<form method="POST" action="` + esc(action) + `" id="officer-form" class="flex flex-col gap-4" data-wizard="` + strconv.FormatBool(!fp.edit) + `" data-first-step="` + strconv.Itoa(first) + `">`)
	if fp.edit {
		b.WriteString(fp.sc.ownerField())
	}
	if !fp.edit {
		b.WriteString(`<ol class="flex flex-wrap gap-x-3 gap-y-1 text-xs text-fg2" id="wizard-progress" hidden>`)
		for i, st := range wizardSteps {
			b.WriteString(`<li data-progress="` + strconv.Itoa(i+1) + `">` + strconv.Itoa(i+1) + `. ` + esc(st) + `</li>`)
		}
		b.WriteString(`</ol>`)
	}
	section := func(n int, legend string) {
		b.WriteString(`<fieldset class="flex flex-col gap-3" data-step="` + strconv.Itoa(n) + `" id="step-` + strconv.Itoa(n) + `"><legend class="mb-1 text-sm font-semibold text-fg">`)
		if !fp.edit {
			b.WriteString(strconv.Itoa(n) + `. `)
		}
		b.WriteString(esc(legend) + `</legend>`)
	}
	// 1. template
	if !fp.edit {
		section(1, wizardSteps[0])
		for i, t := range officerTemplates {
			on := f.Template == t.Key || f.Template == "" && i == 0
			b.WriteString(`<label class="flex items-start gap-2 rounded-lg p-3 ring-1 ring-inset ring-edge"><input type="radio" name="template" value="` + esc(t.Key) + `"` + checkedIf(on) + ` class="mt-1">
<span class="flex flex-col"><span class="text-sm font-semibold text-fg">` + esc(t.Name) + `</span><span class="text-xs text-fg2">` + esc(t.Summary) + `</span></span></label>`)
		}
		b.WriteString(`</fieldset>`)
	}
	// 2. basics
	section(2, wizardSteps[1])
	if !fp.edit && len(fp.scopes) > 1 {
		b.WriteString(`<label for="officer-owner" class="text-sm font-medium text-fg">Whose officer</label><select id="officer-owner" name="owner" class="` + adminInput + `">`)
		for _, sc := range fp.scopes {
			label := "Yours"
			val := ""
			if sc.Team {
				label, val = sc.Name+" (team)", strconv.FormatInt(sc.Owner, 10)
			}
			sel := ""
			if sc.Owner == fp.sc.Owner {
				sel = " selected"
			}
			b.WriteString(`<option value="` + val + `"` + sel + `>` + esc(label) + `</option>`)
		}
		b.WriteString(`</select>`)
	} else if !fp.edit {
		b.WriteString(fp.sc.ownerField())
	}
	b.WriteString(`<label for="officer-name" class="text-sm font-medium text-fg">Name</label>
<input id="officer-name" name="name" required maxlength="80" value="` + esc(f.Name) + `" class="` + adminInput + `" placeholder="e.g. Release notes"` + describedBy(errs, "name") + `>` + fieldErrNode(errs, "name"))
	if fp.edit {
		b.WriteString(`<p class="text-xs text-fg2">Id <code>` + esc(fp.id) + `</code> (it never changes).</p>`)
	} else {
		b.WriteString(`<details class="text-sm"` + map[bool]string{true: " open", false: ""}[errs["id"] != "" || f.ID != ""] + `><summary class="cursor-pointer text-fg2">Its id (made from the name)</summary>
<label for="officer-id" class="mt-2 block text-sm font-medium text-fg">Id: <span class="font-normal text-fg2">two parts, lowercase, like <code>release-notes:main</code></span></label>
<input id="officer-id" name="id" value="` + esc(f.ID) + `" class="mt-1 w-full ` + adminInput + `" placeholder="made from the name"` + describedBy(errs, "id") + `></details>` + fieldErrNode(errs, "id"))
	}
	b.WriteString(`<label for="officer-charter" class="text-sm font-medium text-fg">Instructions</label>
<p class="text-xs text-fg2">What it does, every time it wakes. It reads these first, then the item it was woken for.</p>
<textarea id="officer-charter" name="charter" required rows="7" class="` + adminInput + `"` + describedBy(errs, "charter") + `>` + esc(f.Charter) + `</textarea>` + fieldErrNode(errs, "charter"))
	b.WriteString(`</fieldset>`)
	// 3. where
	section(3, wizardSteps[2])
	b.WriteString(s.runtimeChoices(fp))
	b.WriteString(`<label for="officer-repo" class="text-sm font-medium text-fg">Repository <span class="font-normal text-fg2">(optional)</span></label>
<p class="text-xs text-fg2">owner/name of a repository it works in. Without one, each instance starts in an empty scratch directory of its own.</p>
<input id="officer-repo" name="repo" value="` + esc(f.Repo) + `" class="` + adminInput + `" placeholder="example-org/example-repo"` + describedBy(errs, "repo") + `>` + fieldErrNode(errs, "repo"))
	b.WriteString(`</fieldset>`)
	// 4. when
	if !fp.edit {
		section(4, wizardSteps[3])
		b.WriteString(`<p class="text-xs text-fg2">An officer works only when something wakes it. Choose one.</p>`)
		b.WriteString(`<label class="flex items-start gap-2"><input type="radio" name="wake" value="schedule" required` + checkedIf(f.Wake == "schedule") + describedBy(errs, "wake") + ` class="mt-1"><span class="text-sm text-fg">On a schedule</span></label>
<div class="pl-5 flex flex-col gap-2"><label for="officer-preset" class="sr-only">Schedule</label><select id="officer-preset" name="schedule_preset" class="` + adminInput + `">`)
		for _, p := range schedulePresets {
			b.WriteString(`<option value="` + p.Key + `"` + map[bool]string{true: " selected", false: ""}[f.Preset == p.Key] + `>` + esc(p.Label) + `</option>`)
		}
		b.WriteString(`<option value="custom"` + map[bool]string{true: " selected", false: ""}[f.Preset == "custom"] + `>Custom…</option></select>
<label for="officer-cron" class="text-xs text-fg2">Custom schedule: five fields, minute hour day month weekday, in UTC</label>
<input id="officer-cron" name="cron" value="` + esc(f.Cron) + `" class="` + adminInput + `" placeholder="30 8 * * 1-5"` + describedBy(errs, "cron") + `>` + fieldErrNode(errs, "cron") + `</div>
<label class="flex items-start gap-2"><input type="radio" name="wake" value="manual"` + checkedIf(f.Wake == "manual") + ` class="mt-1"><span class="text-sm text-fg">Only when I give it work <span class="text-fg2">(Run now on its page, or an item a plugin or the CLI puts in its queue)</span></span></label>` +
			fieldErrNode(errs, "wake") + `</fieldset>`)
	}
	// 5. permissions
	section(5, wizardSteps[4])
	b.WriteString(`<p class="text-xs text-fg2">It starts with the least: its instructions, its queue, and asking you. Add only what it needs.</p>`)
	for _, c := range officerCaps {
		on := false
		for _, got := range f.Caps {
			on = on || got == c.Key
		}
		b.WriteString(`<label class="flex items-start gap-2"><input type="checkbox" name="cap" value="` + c.Key + `"` + checkedIf(on) + ` class="mt-1"><span class="text-sm text-fg">` + esc(c.Label) + `</span></label>`)
	}
	b.WriteString(fieldErrNode(errs, "cap"))
	b.WriteString(`<label for="officer-connectors" class="text-sm font-medium text-fg">Connectors it may ask for <span class="font-normal text-fg2">(optional; you approve each request)</span></label>
<input id="officer-connectors" name="connectors" value="` + esc(f.Connectors) + `" class="` + adminInput + `" placeholder="connector names, comma-separated"` + describedBy(errs, "connectors") + `>` + fieldErrNode(errs, "connectors"))
	b.WriteString(`<label for="officer-read-repos" class="text-sm font-medium text-fg">Public repositories it reads <span class="font-normal text-fg2">(optional; pods only)</span></label>
<input id="officer-read-repos" name="read_repos" value="` + esc(f.ReadRepos) + `" class="` + adminInput + `" placeholder="example-org/docs"` + describedBy(errs, "read_repos") + `>` + fieldErrNode(errs, "read_repos"))
	b.WriteString(`<fieldset class="flex flex-col gap-2" id="officer-escalate"><legend class="text-sm font-medium text-fg">Who else is asked when it escalates</legend>`)
	if fp.sc.Team {
		b.WriteString(`<p class="text-xs text-fg2">Its escalations are the team's.</p>`)
	} else {
		b.WriteString(`<p class="text-xs text-fg2">You are always asked.</p>`)
	}
	if len(fp.people) == 0 {
		b.WriteString(`<p class="text-xs text-fg2" id="officer-escalate-none">Nobody else to choose: people of the teams you manage appear here.</p>`)
	}
	for _, p := range fp.people {
		on := false
		for _, e := range f.Escalate {
			on = on || e == p.ID
		}
		b.WriteString(`<label class="flex items-start gap-2"><input type="checkbox" name="escalate_to" value="` + strconv.FormatInt(p.ID, 10) + `"` + checkedIf(on) + ` class="mt-1"><span class="text-sm text-fg">` + esc(p.Label) + `</span></label>`)
	}
	b.WriteString(fieldErrNode(errs, "escalate_to") + `</fieldset>`)
	b.WriteString(`<p class="rounded-lg bg-st-warning-bg px-3 py-2 text-sm text-st-warning-fg" id="host-risk" data-host-risk>` + esc(hostRiskLine) + `</p>`)
	b.WriteString(`</fieldset>`)
	// 6. review
	section(6, wizardSteps[5])
	if !fp.edit {
		b.WriteString(`<dl class="grid grid-cols-[auto,1fr] gap-x-3 gap-y-1 text-sm" id="wizard-review" hidden></dl>`)
	}
	mode := f.Mode
	if mode != "live" {
		mode = "shadow"
	}
	b.WriteString(`<label class="flex items-start gap-2"><input type="radio" name="mode" value="shadow"` + checkedIf(mode == "shadow") + ` class="mt-1"><span class="flex flex-col"><span class="text-sm font-semibold text-fg">Start in Shadow</span><span class="text-xs text-fg2">It works its queue, and its escalations are recorded but ask nobody. Watch it, then switch it Live.</span></span></label>
<label class="flex items-start gap-2"><input type="radio" name="mode" value="live"` + checkedIf(mode == "live") + ` class="mt-1"><span class="flex flex-col"><span class="text-sm font-semibold text-fg">Live</span><span class="text-xs text-fg2">Its escalations ask you (and whoever you chose) at once.</span></span></label>`)
	submit := "Create officer"
	if fp.edit {
		submit = "Save"
	}
	b.WriteString(`</fieldset>
<div class="flex flex-wrap gap-2" id="wizard-nav"><button type="button" class="` + adminQuiet + `" data-wizard-back hidden>Back</button><button type="button" class="` + adminButton + `" data-wizard-next hidden>Next</button>
<button type="submit" class="` + adminButton + `" id="officer-submit">` + submit + `</button></div></form></section>`)
	if !fp.edit {
		b.WriteString(wizardScript)
	}
	s.writeNavDocument(w, r, code, title, ui.NavOfficers, false, `<div class="flex flex-col gap-4">`+b.String()+`</div>`)
}

// hostRiskLine is what a host officer is, in one sentence (docs/officers-builtin.md
// "On a host"): drawn on the permissions step and on a host officer's page.
const hostRiskLine = "On one of your machines it runs as that machine's user, with that user's files, credentials and network: muster's approvals are the path it is told to take, not a wall. Run it only where you would let its instructions run unattended."

// runtimeChoices is the "where it runs" step: auto (what it picks now, and
// why), a pod and a host, each unavailable one disabled with its reason, and
// the host picker over the owner's mirrored machines.
func (s *Server) runtimeChoices(fp formPage) string {
	esc := html.EscapeString
	opts, f := fp.opts, fp.f
	runtime := f.Runtime
	if runtime == "" {
		runtime = officers.RuntimeAuto
	}
	var b strings.Builder
	b.WriteString(`<label class="flex items-start gap-2"><input type="radio" name="runtime" value="auto"` + checkedIf(runtime == officers.RuntimeAuto) + describedBy(fp.errs, "runtime") + ` class="mt-1" data-auto-runtime="` + esc(opts.Auto.Runtime) + `" data-label="` + esc("Let muster choose (right now: "+autoWords(opts)+")") + `">
<span class="flex flex-col"><span class="text-sm font-semibold text-fg">Let muster choose <span class="font-normal text-fg2">(recommended)</span></span><span class="text-xs text-fg2" id="runtime-auto-now">Right now: ` + esc(autoWords(opts)) + `. A pod when this muster has the Kubernetes agent plane, else one of your machines that is ready.</span></span></label>`)
	podOff := ""
	podWhy := "Each instance in its own locked-down pod."
	if !opts.Pod.Available {
		podOff, podWhy = " disabled", "Not available here: "+opts.Pod.Reason+"."
	}
	b.WriteString(`<label class="flex items-start gap-2"><input type="radio" name="runtime" value="pod"` + checkedIf(runtime == officers.RuntimePod) + podOff + ` class="mt-1" data-label="In a pod">
<span class="flex flex-col"><span class="text-sm font-semibold text-fg">In a pod</span><span class="text-xs text-fg2" id="runtime-pod-why">` + esc(podWhy) + `</span></span></label>`)
	hostOff := ""
	hostWhy := "A Claude Code window on a machine of yours that muster mirrors."
	switch {
	case opts.HostReason != "":
		hostOff, hostWhy = " disabled", "Not available here: "+opts.HostReason+"."
	case len(opts.Hosts) == 0:
		hostOff, hostWhy = " disabled", "No machine of yours is mirrored: run muster-mirror with -interactive -agent-spawn on one, and it appears here."
	}
	b.WriteString(`<label class="flex items-start gap-2"><input type="radio" name="runtime" value="host"` + checkedIf(runtime == officers.RuntimeHost) + hostOff + ` class="mt-1" data-label="On one of my machines">
<span class="flex flex-col"><span class="text-sm font-semibold text-fg">On one of my machines</span><span class="text-xs text-fg2" id="runtime-host-why">` + esc(hostWhy) + `</span></span></label>`)
	if len(opts.Hosts) > 0 && opts.HostReason == "" {
		b.WriteString(`<div class="pl-5 flex flex-col gap-1"><label for="officer-host" class="text-xs text-fg2">Machine</label><select id="officer-host" name="host" class="` + adminInput + `"` + describedBy(fp.errs, "host") + `>`)
		picked := f.Host
		if picked == "" {
			for _, h := range opts.Hosts {
				if h.Qualifies {
					picked = h.Host
					break
				}
			}
		}
		for _, h := range opts.Hosts {
			label := h.Host
			if h.Qualifies {
				label += " — ready"
			} else {
				label += " — not ready: " + h.Reason
			}
			b.WriteString(`<option value="` + esc(h.Host) + `"` + map[bool]string{true: " selected", false: ""}[h.Host == picked] + ` data-qualifies="` + strconv.FormatBool(h.Qualifies) + `">` + esc(label) + `</option>`)
		}
		b.WriteString(`</select></div>`)
	}
	b.WriteString(fieldErrNode(fp.errs, "runtime") + fieldErrNode(fp.errs, "host"))
	return b.String()
}

// wizardScript steps the wizard: one fieldset at a time, Next checks the
// step's own fields, Review lists the answers, and the host-risk line shows
// only where it can apply. Without it every step is on the page at once and
// the form still submits.
const wizardScript = `<script>
(function(){
  var form = document.getElementById('officer-form');
  if (!form || form.dataset.wizard !== 'true') return;
  var steps = Array.prototype.slice.call(form.querySelectorAll('fieldset[data-step]'));
  var back = form.querySelector('[data-wizard-back]'), next = form.querySelector('[data-wizard-next]');
  var submit = document.getElementById('officer-submit'), progress = document.getElementById('wizard-progress');
  var review = document.getElementById('wizard-review');
  var at = Math.max(0, Math.min(steps.length - 1, (parseInt(form.dataset.firstStep, 10) || 1) - 1));
  function val(name){ var el = form.querySelector('[name="'+name+'"]:checked') || form.querySelector('[name="'+name+'"]'); return el ? el.value : ''; }
  function label(name){ var el = form.querySelector('[name="'+name+'"]:checked'); return el ? (el.dataset.label || el.value) : ''; }
  function hostRisk(){
    var rt = val('runtime'), auto = form.querySelector('input[name=runtime][value=auto]');
    var onHost = rt === 'host' || (rt === 'auto' && auto && auto.dataset.autoRuntime === 'host');
    var line = document.getElementById('host-risk'); if (line) line.hidden = !onHost;
  }
  function fill(){
    if (!review) return;
    var sel = form.querySelector('#officer-preset');
    var when = val('wake') === 'manual' ? 'Only when you give it work' : (val('wake') === 'schedule' ? (sel && sel.value !== 'custom' ? sel.options[sel.selectedIndex].text : 'Custom: ' + val('cron')) : 'not chosen');
    var caps = Array.prototype.map.call(form.querySelectorAll('input[name=cap]:checked'), function(c){ return c.closest('label').textContent.trim(); });
    var rows = [['Name', val('name')], ['Where', label('runtime') + (val('runtime') === 'host' ? ' (' + val('host') + ')' : '')],
      ['Repository', val('repo') || 'none: a scratch directory'], ['When', when], ['May', caps.length ? caps.join('; ') : 'only its queue and asking you']];
    review.textContent = '';
    rows.forEach(function(r){ var dt = document.createElement('dt'); dt.className = 'text-fg2'; dt.textContent = r[0]; var dd = document.createElement('dd'); dd.className = 'text-fg'; dd.textContent = r[1]; review.appendChild(dt); review.appendChild(dd); });
    review.hidden = false;
  }
  function show(i){
    at = i;
    steps.forEach(function(s, j){ s.hidden = j !== i; });
    back.hidden = i === 0; next.hidden = i === steps.length - 1; submit.hidden = i !== steps.length - 1;
    if (progress) { progress.hidden = false; Array.prototype.forEach.call(progress.children, function(li, j){ if (j === i) li.setAttribute('aria-current', 'step'); else li.removeAttribute('aria-current'); li.className = j === i ? 'font-semibold text-fg' : ''; }); }
    if (i === steps.length - 1) fill();
    hostRisk();
  }
  function stepValid(i){
    var els = steps[i].querySelectorAll('input, textarea, select');
    for (var k = 0; k < els.length; k++) { if (!els[k].checkValidity()) { els[k].reportValidity(); return false; } }
    return true;
  }
  next.addEventListener('click', function(){ if (stepValid(at)) { show(at + 1); var f = steps[at].querySelector('input:not([type=hidden]), textarea, select'); if (f) f.focus(); } });
  back.addEventListener('click', function(){ show(at - 1); });
  form.addEventListener('change', hostRisk);
  show(at);
})();
</script>`

// --- the wizard and the edit ----------------------------------------------------

func (s *Server) handleOfficerNewPage(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.scopeFor(w, r)
	if !ok {
		return
	}
	if !sc.Manage {
		s.renderNotice(w, r, http.StatusForbidden, "Only a manager adds team officers", TeamOfficerManagerOnly)
		return
	}
	t := templateNamed(r.FormValue("template"))
	f := t.Values
	f.Template = t.Key
	s.renderOfficerForm(w, r, http.StatusOK, s.formPageFor(r.Context(), sc, f, nil, "", false, ""))
}

// formPageFor is the form page's data for scope sc.
func (s *Server) formPageFor(ctx context.Context, sc officerScope, f formValues, errs fieldErrs, top string, edit bool, id string) formPage {
	if errs == nil {
		errs = fieldErrs{}
	}
	fp := formPage{sc: sc, f: f, errs: errs, top: top, edit: edit, id: id, opts: s.runtimeOptionsFor(ctx, sc.Owner, ""),
		people: s.escalationChoices(ctx, sc, f.Escalate)}
	if !edit {
		fp.scopes = s.managedScopes(ctx)
	}
	return fp
}

func (s *Server) handleOfficerCreate(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.scopeFor(w, r)
	if !ok {
		return
	}
	if !sc.Manage {
		s.renderNotice(w, r, http.StatusForbidden, "Only a manager adds team officers", TeamOfficerManagerOnly)
		return
	}
	ctx := r.Context()
	f := readForm(r)
	in, errs := declaredInOf(f, false)
	if len(errs) > 0 {
		if f.ID == "" && in.ID != "" {
			f.ID = ""
		}
		s.renderOfficerForm(w, r, http.StatusBadRequest, s.formPageFor(ctx, sc, f, errs, "", false, ""))
		return
	}
	if in.ID == "" {
		errs["name"] = "Its name needs at least one letter or digit to make an id from."
		s.renderOfficerForm(w, r, http.StatusBadRequest, s.formPageFor(ctx, sc, f, errs, "", false, ""))
		return
	}
	row, err := s.createDeclared(ctx, sc.Owner, sc.Team, in)
	if err != nil {
		if fe, top := officerFieldErrs(err); len(fe) > 0 || top != "" {
			s.renderOfficerForm(w, r, http.StatusBadRequest, s.formPageFor(ctx, sc, f, fe, top, false, ""))
			return
		}
		code, msg := officerPageRefusal(err)
		if code == http.StatusInternalServerError {
			s.logger.Printf("officers: create from the wizard: %v", err)
		}
		s.renderOfficerForm(w, r, code, s.formPageFor(ctx, sc, f, nil, msg, false, ""))
		return
	}
	if sc.Team {
		s.auditTeam(ctx, actorOf(r), sc.Owner, "officer.create", "officer_def:"+row.Officer, map[string]any{"runtime": row.Fields.Runtime, "mode": row.Mode})
	}
	if cron := wizardCron(f); cron != "" {
		if _, err := s.officerStore.PutSchedule(ctx, sc.Owner, row.Officer, builtin.Schedule{Name: "pass", Cron: cron, Kind: "scheduled.pass",
			SubjectKey: "scheduled-pass", SkipIfLive: true, Enabled: true}, s.officerNow()); err != nil {
			// The officer exists; its schedule did not land. Say so on its page
			// rather than pretend.
			s.logger.Printf("officers: the wizard's schedule for %s: %v", row.Officer, err)
			http.Redirect(w, r, officerHref(sc, row.Officer, "")+joinQuery(sc, "notice=schedule-failed"), http.StatusSeeOther)
			return
		}
	}
	http.Redirect(w, r, officerHref(sc, row.Officer, "")+joinQuery(sc, "notice=created"), http.StatusSeeOther)
}

// joinQuery is q appended to a URL that does (team) or does not already
// carry a query.
func joinQuery(sc officerScope, q string) string {
	if sc.Team {
		return "&" + q
	}
	return "?" + q
}

// declaredFormOf is owner's DB-declared officer id as the edit form's values.
func (s *Server) declaredFormOf(ctx context.Context, owner int64, row builtin.DeclaredRow) formValues {
	f := formValues{Name: row.TypeName, Runtime: row.Fields.Runtime, Host: row.Fields.Host, Repo: row.Fields.Repo,
		Connectors: strings.Join(row.Fields.Connectors, ", "), ReadRepos: strings.Join(row.Fields.ReadRepos, ", "),
		Escalate: row.EscalateTo, Mode: row.Mode, Paused: !row.Enabled}
	if t, err := s.officerStore.GetType(ctx, owner, row.TypeKey); err == nil {
		f.Charter, f.Policy, f.Caps = t.Charter, t.Policy, policyCaps(t.Policy)
	}
	return f
}

// editableRow is the DB-declared officer the request names, for a write: the
// scope managed, the officer declared in the database. ok false: answered.
func (s *Server) editableRow(w http.ResponseWriter, r *http.Request) (officerScope, builtin.DeclaredRow, bool) {
	sc, ok := s.scopeFor(w, r)
	if !ok {
		return sc, builtin.DeclaredRow{}, false
	}
	id := r.PathValue("officer")
	if _, env := s.officerRT.envOfficer(sc.Owner, id); env {
		s.renderNotice(w, r, http.StatusConflict, "Read-only here", "MUSTER_OFFICERS declares this officer, so it is read-only here: change the environment and restart.")
		return sc, builtin.DeclaredRow{}, false
	}
	row, err := s.officerStore.GetDeclared(r.Context(), sc.Owner, id)
	if err != nil {
		code, msg := officerPageRefusal(err)
		s.renderNotice(w, r, code, "No such officer", msg)
		return sc, builtin.DeclaredRow{}, false
	}
	if !sc.Manage {
		s.renderNotice(w, r, http.StatusForbidden, "Only a manager changes team officers", TeamOfficerManagerOnly)
		return sc, builtin.DeclaredRow{}, false
	}
	return sc, row, true
}

func (s *Server) handleOfficerEditPage(w http.ResponseWriter, r *http.Request) {
	sc, row, ok := s.editableRow(w, r)
	if !ok {
		return
	}
	f := s.declaredFormOf(r.Context(), sc.Owner, row)
	s.renderOfficerForm(w, r, http.StatusOK, s.formPageFor(r.Context(), sc, f, nil, "", true, row.Officer))
}

func (s *Server) handleOfficerEdit(w http.ResponseWriter, r *http.Request) {
	sc, row, ok := s.editableRow(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	stored := s.declaredFormOf(ctx, sc.Owner, row)
	f := readForm(r)
	f.ID, f.Paused, f.Policy = row.Officer, stored.Paused, stored.Policy
	in, errs := declaredInOf(f, true)
	if len(errs) > 0 {
		s.renderOfficerForm(w, r, http.StatusBadRequest, s.formPageFor(ctx, sc, f, errs, "", true, row.Officer))
		return
	}
	if err := s.updateDeclared(ctx, sc.Owner, sc.Team, row.Officer, in); err != nil {
		if fe, top := officerFieldErrs(err); len(fe) > 0 || top != "" {
			s.renderOfficerForm(w, r, http.StatusBadRequest, s.formPageFor(ctx, sc, f, fe, top, true, row.Officer))
			return
		}
		code, msg := officerPageRefusal(err)
		s.renderOfficerForm(w, r, code, s.formPageFor(ctx, sc, f, nil, msg, true, row.Officer))
		return
	}
	if sc.Team {
		s.auditTeam(ctx, actorOf(r), sc.Owner, "officer.replace", "officer_def:"+row.Officer, map[string]any{"mode": *in.Mode})
	}
	http.Redirect(w, r, officerHref(sc, row.Officer, "")+joinQuery(sc, "notice=saved"), http.StatusSeeOther)
}

// --- the actions ----------------------------------------------------------------

// officerActionDone answers a write on an officer's page: back to it, or the
// page again with the refusal.
func (s *Server) officerActionDone(w http.ResponseWriter, r *http.Request, sc officerScope, id, notice string, err error) {
	if err == nil {
		http.Redirect(w, r, officerHref(sc, id, "")+joinQuery(sc, "notice="+notice), http.StatusSeeOther)
		return
	}
	code, msg := officerPageRefusal(err)
	var inv declaredInvalid
	if errors.As(err, &inv) {
		code, msg = http.StatusBadRequest, inv.err.Error()
	}
	if code == http.StatusInternalServerError {
		s.logger.Printf("officers: %s on %s: %v", notice, id, err)
	}
	if code == http.StatusNotFound {
		s.renderNotice(w, r, code, "No such officer", officerNotFound)
		return
	}
	s.renderOfficerDetail(w, r, sc, id, code, msg)
}

func (s *Server) handleOfficerPause(w http.ResponseWriter, r *http.Request) {
	s.officerSwitch(w, r, false)
}
func (s *Server) handleOfficerResume(w http.ResponseWriter, r *http.Request) {
	s.officerSwitch(w, r, true)
}

func (s *Server) officerSwitch(w http.ResponseWriter, r *http.Request, on bool) {
	sc, row, ok := s.editableRow(w, r)
	if !ok {
		return
	}
	err := s.switchDeclared(r.Context(), sc.Owner, row.Officer, on)
	notice := "paused"
	if on {
		notice = "resumed"
	}
	if err == nil && sc.Team {
		s.auditTeam(r.Context(), actorOf(r), sc.Owner, "officer."+strings.TrimSuffix(notice, "d"), "officer_def:"+row.Officer, nil)
	}
	s.officerActionDone(w, r, sc, row.Officer, notice, err)
}

// handleOfficerMode is the Shadow/Live switch: the stored record replaced
// with only its mode changed (updateDeclared: mode and paused stated).
func (s *Server) handleOfficerMode(w http.ResponseWriter, r *http.Request) {
	sc, row, ok := s.editableRow(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	mode := r.FormValue("mode")
	if mode != "live" && mode != "shadow" {
		s.renderOfficerDetail(w, r, sc, row.Officer, http.StatusBadRequest, "Choose Shadow or Live.")
		return
	}
	f := s.declaredFormOf(ctx, sc.Owner, row)
	f.ID, f.Mode = row.Officer, mode
	in, _ := declaredInOf(f, true)
	err := s.updateDeclared(ctx, sc.Owner, sc.Team, row.Officer, in)
	if err == nil && sc.Team {
		s.auditTeam(ctx, actorOf(r), sc.Owner, "officer.mode", "officer_def:"+row.Officer, map[string]any{"mode": mode})
	}
	s.officerActionDone(w, r, sc, row.Officer, "mode-"+mode, err)
}

// runNowKind is the kind of item "Run now" puts in an officer's queue.
const runNowKind = "manual.run"

func (s *Server) handleOfficerRunNow(w http.ResponseWriter, r *http.Request) {
	sc, row, ok := s.editableRow(w, r)
	if !ok {
		return
	}
	// One live "run now" at a time: a second tap while the first is still
	// open returns that item (the store's dedupe on kind and subject).
	id, created, err := s.officerStore.EnqueueChecked(r.Context(), sc.Owner, builtin.NewItem{Officer: row.Officer, Kind: runNowKind, SubjectKey: "run-now"}, s.officerNow())
	notice := "queued"
	if err == nil && !created {
		notice = "already-queued"
	}
	if err == nil {
		s.logger.Printf("officers: user %d ran officer %s of owner %d now (item %d, created %v)", authz.MustFrom(r.Context()).UserID, row.Officer, sc.Owner, id, created)
		if st, ok := s.officerRT.declaredView(sc.Owner, row.Officer); ok && st.officer != nil && s.officerRT.rec != nil {
			s.officerRT.rec.Due(st.officer.Key())
		}
	}
	s.officerActionDone(w, r, sc, row.Officer, notice, err)
}

func (s *Server) handleOfficerDeletePage(w http.ResponseWriter, r *http.Request) {
	sc, row, ok := s.editableRow(w, r)
	if !ok {
		return
	}
	if r.FormValue("confirm") != row.Officer {
		s.renderOfficerDetail(w, r, sc, row.Officer, http.StatusBadRequest, "To delete it, type its id exactly.")
		return
	}
	removed, err := s.deleteDeclared(r.Context(), sc.Owner, row.Officer)
	if err != nil {
		s.officerActionDone(w, r, sc, row.Officer, "deleted", err)
		return
	}
	if sc.Team {
		s.auditTeam(r.Context(), actorOf(r), sc.Owner, "officer.delete", "officer_def:"+row.Officer, map[string]any{"instances_removed": noneIfNil(removed)})
	}
	http.Redirect(w, r, officersPagePath+"?notice=deleted", http.StatusSeeOther)
}

func (s *Server) handleOfficerSchedulePage(w http.ResponseWriter, r *http.Request) {
	sc, row, ok := s.editableRow(w, r)
	if !ok {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		name = "pass"
	}
	cron := presetCron(r.FormValue("schedule_preset"))
	if r.FormValue("schedule_preset") == "custom" || cron == "" {
		cron = strings.TrimSpace(r.FormValue("cron"))
	}
	if _, err := builtin.ParseCron(cron); err != nil {
		s.renderOfficerDetail(w, r, sc, row.Officer, http.StatusBadRequest, "That schedule is not one muster reads: "+err.Error())
		return
	}
	_, err := s.officerStore.PutSchedule(r.Context(), sc.Owner, row.Officer, builtin.Schedule{Name: name, Cron: cron, Kind: "scheduled.pass",
		SubjectKey: "scheduled-" + name, SkipIfLive: true, Enabled: true}, s.officerNow())
	if errors.Is(err, builtin.ErrInvalid) {
		s.renderOfficerDetail(w, r, sc, row.Officer, http.StatusBadRequest, "That schedule is refused: "+strings.TrimPrefix(err.Error(), builtin.ErrInvalid.Error()+": "))
		return
	}
	s.officerActionDone(w, r, sc, row.Officer, "schedule-saved", err)
}

func (s *Server) handleOfficerScheduleDeletePage(w http.ResponseWriter, r *http.Request) {
	sc, row, ok := s.editableRow(w, r)
	if !ok {
		return
	}
	err := s.officerStore.DeleteSchedule(r.Context(), sc.Owner, row.Officer, r.PathValue("name"))
	s.officerActionDone(w, r, sc, row.Officer, "schedule-deleted", err)
}

// --- the detail page ------------------------------------------------------------

func (s *Server) handleOfficerPage(w http.ResponseWriter, r *http.Request) {
	sc, ok := s.scopeFor(w, r)
	if !ok {
		return
	}
	s.renderOfficerDetail(w, r, sc, r.PathValue("officer"), http.StatusOK, "")
}

// officerNotices are the words a redirect's ?notice= shows (a closed set: no
// request text is drawn).
var officerNotices = map[string]string{
	"created":          "Created. muster runs it from now on, with no restart.",
	"schedule-failed":  "Created, but its schedule could not be saved: add one below.",
	"saved":            "Saved.",
	"paused":           "Paused: its instances stop and none starts until you resume it.",
	"resumed":          "Resumed.",
	"mode-shadow":      "Now in Shadow: its escalations are recorded and ask nobody.",
	"mode-live":        "Now Live: its escalations ask people.",
	"queued":           "Queued: it wakes for it within a few seconds.",
	"already-queued":   "It already has a Run now waiting.",
	"schedule-saved":   "Schedule saved.",
	"schedule-deleted": "Schedule deleted.",
}

func (s *Server) renderOfficerDetail(w http.ResponseWriter, r *http.Request, sc officerScope, id string, code int, problem string) {
	esc := html.EscapeString
	ctx := r.Context()
	v, found, err := s.officerView(ctx, sc.Owner, id)
	if err != nil {
		s.logger.Printf("officers: read officer %s of %d: %v", id, sc.Owner, err)
		s.renderNotice(w, r, http.StatusInternalServerError, "Could not load the officer", "Try again in a moment.")
		return
	}
	if !found {
		s.renderNotice(w, r, http.StatusNotFound, "No such officer", officerNotFound)
		return
	}
	status := officerStatus(v)
	name := v.Name
	if name == "" {
		name = v.ID
	}
	writable := sc.Manage && v.ManagedBy == officers.ManagedByDB
	builtinBackend := v.Backend == officers.BackendBuiltin
	var b strings.Builder
	b.WriteString(problemNode(problem))
	if n, ok := officerNotices[r.URL.Query().Get("notice")]; ok && problem == "" {
		b.WriteString(`<p role="status" class="rounded-lg bg-s2 px-3 py-2 text-sm text-fg" id="officer-notice">` + esc(n) + `</p>`)
	}
	// The head: who, where, how it is.
	b.WriteString(`<section class="` + adminCard + `" id="officer-head" data-officer="` + esc(v.ID) + `" data-managed-by="` + v.ManagedBy + `">
<p class="text-xs"><a href="` + officersPagePath + `" class="text-fg2 underline-offset-2 hover:underline">← Officers</a></p>
<div class="flex flex-wrap items-center gap-2"><h1 class="min-w-0 flex-1 break-words text-base font-semibold text-fg">` + esc(name) + `</h1>` + statusChip(status) + `</div>
<p class="text-xs text-fg2">` + esc(v.ID))
	if sc.Team {
		b.WriteString(` · ` + esc(sc.Name) + `'s`)
	}
	b.WriteString(` · runs ` + esc(whereItRuns(v)))
	if v.Repo != "" {
		b.WriteString(` · in ` + esc(v.Repo))
	} else if v.ManagedBy != managedByDef {
		b.WriteString(` · in a scratch directory`)
	}
	b.WriteString(`</p>`)
	if !v.Running && v.Reason != "" {
		b.WriteString(`<p class="text-sm text-st-error-fg" id="officer-reason">Not running: ` + esc(v.Reason) + `</p>`)
	}
	switch v.ManagedBy {
	case officers.ManagedByEnv:
		b.WriteString(`<p class="text-sm text-fg2" id="officer-read-only">MUSTER_OFFICERS declares it, so it is read-only here: change the environment and restart.</p>`)
	case managedByDef:
		b.WriteString(`<p class="text-sm text-fg2" id="officer-read-only">Only a def in the built-in store: declare where it runs with <code>muster officer create</code>, or add it to MUSTER_OFFICERS.</p>`)
	}
	if v.RunsOn == officers.RuntimeHost || v.Runtime == officers.RuntimeHost {
		b.WriteString(`<p class="rounded-lg bg-st-warning-bg px-3 py-2 text-sm text-st-warning-fg" data-host-risk>` + esc(hostRiskLine) + `</p>`)
	}
	if writable {
		form := func(suffix, label, cls string, extra string) string {
			return `<form method="POST" action="` + esc(officersPagePath+"/"+url.PathEscape(v.ID)+suffix) + `">` + sc.ownerField() + extra +
				`<button type="submit" class="` + cls + `">` + label + `</button></form>`
		}
		b.WriteString(`<div class="flex flex-wrap gap-2" id="officer-actions">`)
		b.WriteString(form("/run", "Run now", adminButton, ""))
		if v.Paused != nil && *v.Paused {
			b.WriteString(form("/resume", "Resume", adminQuiet, ""))
		} else {
			b.WriteString(form("/pause", "Pause", adminQuiet, ""))
		}
		if v.Mode == "shadow" {
			b.WriteString(form("/mode", "Switch to Live", adminQuiet, `<input type="hidden" name="mode" value="live">`))
		} else {
			b.WriteString(form("/mode", "Switch to Shadow", adminQuiet, `<input type="hidden" name="mode" value="shadow">`))
		}
		b.WriteString(`<a href="` + officerHref(sc, v.ID, "/edit") + `" class="` + adminQuiet + ` inline-flex items-center" id="officer-edit">Edit</a></div>`)
	} else if v.ManagedBy == officers.ManagedByDB {
		b.WriteString(`<p class="text-sm text-fg2">` + esc(TeamOfficerManagerOnly) + `</p>`)
	}
	b.WriteString(`</section>`)

	// The last pass (this server's wake loop).
	b.WriteString(`<section class="` + adminCard + `" id="officer-pass"><h2 class="text-base font-semibold text-fg">Last pass</h2>`)
	if lp, ok := s.lastPass(sc.Owner, v.ID); ok {
		line := fmtTime(lp.At) + " · saw " + plural(lp.Claimable, "claimable item")
		if lp.Started > 0 {
			line += " · started " + plural(lp.Started, "instance")
		}
		if lp.SwitchedOff {
			line += " · switched off"
		}
		b.WriteString(`<p class="text-sm text-fg">` + esc(line) + `</p>`)
		if lp.Err != "" {
			fail := lp.Err
			if lp.Failures > 1 {
				fail += " (" + strconv.Itoa(lp.Failures) + " failed passes in a row)"
			}
			b.WriteString(`<p class="text-sm text-st-error-fg" id="officer-pass-error">` + esc(fail) + `</p>`)
		}
	} else if v.Running {
		b.WriteString(`<p class="text-sm text-fg2">No pass on this server since it started; the wake loop checks every officer within its poll interval.</p>`)
	} else {
		b.WriteString(`<p class="text-sm text-fg2">None: it is not running.</p>`)
	}
	b.WriteString(`<p class="text-xs text-muted">As this server's wake loop saw it; another replica keeps its own.</p></section>`)

	// The instances.
	b.WriteString(s.officerInstances(ctx, sc, v))

	// The queue.
	if builtinBackend {
		items, err := s.officerItems(ctx, sc.Owner, v.ID, 50)
		b.WriteString(`<section class="` + adminCard + `" id="officer-queue"><h2 class="text-base font-semibold text-fg">Queue</h2>`)
		switch {
		case err != nil:
			s.logger.Printf("officers: read %s's items: %v", v.ID, err)
			b.WriteString(`<p class="text-sm text-fg2">Its queue could not be read just now.</p>`)
		case len(items) == 0:
			b.WriteString(`<p class="text-sm text-fg2" id="officer-queue-empty">Nothing yet. Run now puts an item here; so does each schedule fire.</p>`)
		default:
			b.WriteString(`<ul class="flex flex-col divide-y divide-line">`)
			for _, it := range items {
				line := "#" + strconv.FormatInt(it.ID, 10) + " " + it.Kind + " · " + it.SubjectKey
				detail := it.Status
				if it.ClaimedBy != "" && it.Status == officers.StatusClaimed {
					detail += " by " + it.ClaimedBy
				}
				if it.Outcome != "" {
					detail += " · " + it.Outcome
				}
				detail += " · " + fmtTime(it.UpdatedAt)
				b.WriteString(`<li class="flex flex-col py-2" data-item="` + strconv.FormatInt(it.ID, 10) + `" data-item-status="` + esc(it.Status) + `"><span class="break-words text-sm text-fg">` + esc(line) + `</span><span class="text-xs text-fg2">` + esc(detail) + `</span>`)
				if it.Summary != "" {
					b.WriteString(`<span class="break-words text-xs text-fg2">` + esc(truncateRunes(it.Summary, 240)) + `</span>`)
				}
				b.WriteString(`</li>`)
			}
			b.WriteString(`</ul>`)
		}
		b.WriteString(`</section>`)
	} else {
		b.WriteString(`<section class="` + adminCard + `" id="officer-queue"><h2 class="text-base font-semibold text-fg">Queue</h2><p class="text-sm text-fg2">Its queue is in its HQ.</p></section>`)
	}

	// Asks and drafts.
	asks := raisedByOfficer(s.openAsks(ctx), sc.Owner, v.ID)
	b.WriteString(`<section class="` + adminCard + `" id="officer-asks"><h2 class="text-base font-semibold text-fg">Asks</h2>`)
	if len(asks) == 0 {
		b.WriteString(`<p class="text-sm text-fg2">Nothing it is waiting on you for.</p>`)
	} else {
		b.WriteString(`<ul class="flex flex-col gap-2">`)
		for _, a := range asks {
			b.WriteString(`<li><a class="text-sm text-fg underline-offset-2 hover:underline" href="/attention/asks/` + strconv.FormatInt(a.ID, 10) + `">` + esc(truncateRunes(a.Question, 200)) + `</a></li>`)
		}
		b.WriteString(`</ul>`)
	}
	b.WriteString(`</section>`)
	b.WriteString(s.officerDrafts(ctx, v))

	// Schedules.
	if builtinBackend {
		b.WriteString(s.officerSchedules(ctx, sc, v, writable))
	}

	// Its spend, where the server books it.
	if builtinBackend {
		if days, err := s.officerStore.ListUsage(ctx, sc.Owner, v.ID, 7, s.officerNow()); err == nil && len(days) > 0 {
			var cost float64
			var reqs int64
			for _, d := range days {
				cost += d.CostUSD
				reqs += d.Requests
			}
			b.WriteString(`<section class="` + adminCard + `" id="officer-usage"><h2 class="text-base font-semibold text-fg">Usage, last 7 days</h2><p class="text-sm text-fg">` +
				esc(fmt.Sprintf("%d requests · about $%.2f (an estimate at list price)", reqs, cost)) + `</p></section>`)
		}
	}

	if writable {
		b.WriteString(`<section class="` + adminCard + `" id="officer-delete"><h2 class="text-base font-semibold text-fg">Delete</h2>
<p class="text-sm text-fg2">Deletes it with its queue, schedules and people, and stops its instances. This cannot be undone.</p>
<form method="POST" action="` + esc(officersPagePath+"/"+url.PathEscape(v.ID)+"/delete") + `" class="flex flex-col gap-2">` + sc.ownerField() + `
<label for="officer-delete-confirm" class="text-sm text-fg">Type <code>` + esc(v.ID) + `</code> to confirm</label>
<input id="officer-delete-confirm" name="confirm" autocomplete="off" class="` + adminInput + `">
<button type="submit" class="press min-h-[44px] rounded-md bg-danger px-3 py-2 text-base font-semibold text-on-danger">Delete officer</button></form></section>`)
	}
	s.writeNavDocument(w, r, code, name, ui.NavOfficers, false, `<div class="flex flex-col gap-4">`+b.String()+`</div>`)
}

// truncateRunes is v cut to n runes, with an ellipsis when cut.
func truncateRunes(v string, n int) string {
	r := []rune(v)
	if len(r) <= n {
		return v
	}
	return string(r[:n]) + "…"
}

// officerInstances draws v's instances: for a host officer each one's newest
// spawn (its window, as a link to the mirrored view, or the host's refusal);
// for a pod officer its agent, linked when it exists.
func (s *Server) officerInstances(ctx context.Context, sc officerScope, v declaredOfficerView) string {
	esc := html.EscapeString
	var b strings.Builder
	b.WriteString(`<section class="` + adminCard + `" id="officer-instances"><h2 class="text-base font-semibold text-fg">Instances</h2>`)
	if len(v.Instances) == 0 {
		b.WriteString(`<p class="text-sm text-fg2">None: it is not running.</p></section>`)
		return b.String()
	}
	var spawns map[string]officers.HostSpawn
	if v.RunsOn == officers.RuntimeHost {
		if hs := s.officerHostSpawns(); hs != nil {
			spawns, _ = hs.LatestHostSpawns(ctx, v.ID)
		}
	}
	b.WriteString(`<ul class="flex flex-col gap-2">`)
	for _, name := range v.Instances {
		b.WriteString(`<li class="flex flex-col" data-instance="` + esc(name) + `"><span class="text-sm text-fg">` + esc(name) + `</span>`)
		switch {
		case v.RunsOn == officers.RuntimeHost:
			h, ok := spawns[name]
			switch {
			case !ok:
				b.WriteString(`<span class="text-xs text-fg2">not started: it starts when there is work</span>`)
			case h.Window != "" && h.MissingListings < officers.HostSpawnMissingToClose:
				owner := int64(0)
				if sc.Team {
					owner = sc.Owner
				}
				href := ui.MirrorRef{Owner: owner, Host: h.Host, Session: mirror.AgentSpawnSession, Window: h.Window}.ViewPath()
				b.WriteString(`<a class="text-xs text-fg underline underline-offset-2" href="` + esc(href) + `">window ` + esc(h.Window) + ` on ` + esc(h.Host) + ` · started ` + fmtTime(h.CreatedAt) + `</a>`)
			default:
				b.WriteString(`<span class="text-xs text-fg2">` + esc(spawnState(h)) + ` · ` + fmtTime(h.CreatedAt) + `</span>`)
			}
		case s.ext.Agents != nil:
			if a, err := s.ext.Agents.GetByName(ctx, name); err == nil {
				b.WriteString(`<a class="text-xs text-fg underline underline-offset-2" href="` + esc(s.agentPageHref(ctx, a)) + `">open its agent page</a>`)
			} else {
				b.WriteString(`<span class="text-xs text-fg2">not started: it starts when there is work</span>`)
			}
		}
		b.WriteString(`</li>`)
	}
	b.WriteString(`</ul></section>`)
	return b.String()
}

// officerDrafts draws the outbound drafts v's instances filed (the person's
// drafts whose agent is one of v's instances), linked to the Asks page where
// they are decided.
func (s *Server) officerDrafts(ctx context.Context, v declaredOfficerView) string {
	if s.ext.Plugins == nil || len(v.Instances) == 0 {
		return ""
	}
	cards, err := s.ext.Plugins.DraftCards(ctx, 100)
	if err != nil {
		return ""
	}
	mine := map[string]bool{}
	for _, n := range v.Instances {
		mine[n] = true
	}
	var list []plugins.DraftCard
	for _, c := range cards {
		if mine[c.Draft.AgentName] {
			list = append(list, c)
		}
	}
	esc := html.EscapeString
	var b strings.Builder
	b.WriteString(`<section class="` + adminCard + `" id="officer-drafts"><h2 class="text-base font-semibold text-fg">Drafts</h2>`)
	if len(list) == 0 {
		b.WriteString(`<p class="text-sm text-fg2">No drafts.</p></section>`)
		return b.String()
	}
	b.WriteString(`<ul class="flex flex-col gap-2">`)
	for _, c := range list {
		b.WriteString(`<li class="flex flex-col"><span class="break-words text-sm text-fg">` + esc(truncateRunes(c.Draft.Body, 200)) + `</span><span class="text-xs text-fg2">` +
			esc(string(c.Draft.Status)) + ` · ` + fmtTime(c.Draft.CreatedAt) + `</span></li>`)
	}
	b.WriteString(`</ul><p class="text-xs"><a class="text-fg2 underline underline-offset-2" href="/attention/drafts">Decide drafts on the Asks page</a></p></section>`)
	return b.String()
}

// officerSchedules draws v's schedules, with add and delete when writable.
func (s *Server) officerSchedules(ctx context.Context, sc officerScope, v declaredOfficerView, writable bool) string {
	esc := html.EscapeString
	list, err := s.officerStore.ListSchedules(ctx, sc.Owner, v.ID)
	var b strings.Builder
	b.WriteString(`<section class="` + adminCard + `" id="officer-schedules"><h2 class="text-base font-semibold text-fg">Schedules</h2>`)
	switch {
	case err != nil:
		b.WriteString(`<p class="text-sm text-fg2">Its schedules could not be read just now.</p>`)
	case len(list) == 0:
		b.WriteString(`<p class="text-sm text-fg2" id="officer-schedules-none">None: it works only when it is given work (Run now, a plugin, the CLI).</p>`)
	default:
		b.WriteString(`<ul class="flex flex-col gap-2">`)
		for _, sch := range list {
			line := sch.Name + " · " + sch.Cron + " (UTC)"
			if !sch.Enabled {
				line += " · off"
			}
			next := ""
			if sch.NextFireAt != nil {
				next = "next " + fmtTime(*sch.NextFireAt)
			}
			if sch.LastFiredAt != nil {
				if next != "" {
					next += " · "
				}
				next += "last " + fmtTime(*sch.LastFiredAt)
			}
			b.WriteString(`<li class="flex flex-wrap items-center gap-2" data-schedule="` + esc(sch.Name) + `"><span class="flex min-w-0 flex-1 flex-col"><span class="text-sm text-fg">` + esc(line) + `</span><span class="text-xs text-fg2">` + esc(next) + `</span></span>`)
			if writable {
				b.WriteString(`<form method="POST" action="` + esc(officersPagePath+"/"+url.PathEscape(v.ID)+"/schedules/"+url.PathEscape(sch.Name)+"/delete") + `">` + sc.ownerField() +
					`<button type="submit" class="` + adminQuiet + `">Delete</button></form>`)
			}
			b.WriteString(`</li>`)
		}
		b.WriteString(`</ul>`)
	}
	if writable {
		b.WriteString(`<form method="POST" action="` + esc(officersPagePath+"/"+url.PathEscape(v.ID)+"/schedules") + `" class="flex flex-col gap-2" id="officer-schedule-add">` + sc.ownerField() + `
<label for="schedule-preset" class="text-sm font-medium text-fg">Add a schedule</label><select id="schedule-preset" name="schedule_preset" class="` + adminInput + `">`)
		for _, p := range schedulePresets {
			b.WriteString(`<option value="` + p.Key + `">` + esc(p.Label) + `</option>`)
		}
		b.WriteString(`<option value="custom">Custom…</option></select>
<label for="schedule-cron" class="text-xs text-fg2">Custom: five fields, in UTC</label><input id="schedule-cron" name="cron" class="` + adminInput + `" placeholder="30 8 * * 1-5">
<label for="schedule-name" class="text-xs text-fg2">Name (one schedule per name)</label><input id="schedule-name" name="name" class="` + adminInput + `" placeholder="pass">
<button type="submit" class="` + adminQuiet + `">Save schedule</button></form>`)
	}
	b.WriteString(`</section>`)
	return b.String()
}
