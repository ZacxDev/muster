package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// THE OFFICERS PAGES (officers redesign O-C; docs/officers-builtin.md "The
// Officers page")
//
//	GET  /officers                              the signed-in person's officers, and their teams'
//	GET  /officers/new                          the New-officer wizard
//	POST /officers/new                          create (createDeclared)
//	GET  /officers/{officer}                    one officer: queue, instances, asks, drafts, schedules
//	GET  /officers/{officer}/edit               its declaration as a form
//	POST /officers/{officer}/edit               replace (updateDeclared)
//	POST /officers/{officer}/pause|resume       its switch (switchDeclared)
//	POST /officers/{officer}/mode               Shadow <-> Live (updateDeclared, the record unchanged but its mode)
//	POST /officers/{officer}/run                "Run now": one item in its queue
//	POST /officers/{officer}/delete             delete (deleteDeclared)
//	POST /officers/{officer}/schedules          add or replace a schedule
//	POST /officers/{officer}/schedules/{name}/delete
//	POST /officers/{officer}/refusals/{item}/refile|drop   a refusal waiting on its owner (officer_needs_you.go)
//	POST /officers/{officer}/chat                          chat with it (officer_operator_chat.go)
//	POST /officers/{officer}/chat/cards/{card}/approve|reject   a confirm card it asked for in the chat
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
	mux.HandleFunc("POST "+officersPagePath+"/{officer}/raise-cap", s.requireOperatorWrite(s.handleOfficerRaiseCap))
	mux.HandleFunc("POST "+officersPagePath+"/{officer}/delete", s.requireOperatorWrite(s.handleOfficerDeletePage))
	mux.HandleFunc("POST "+officersPagePath+"/{officer}/schedules", s.requireOperatorWrite(s.handleOfficerSchedulePage))
	mux.HandleFunc("POST "+officersPagePath+"/{officer}/schedules/{name}/delete", s.requireOperatorWrite(s.handleOfficerScheduleDeletePage))
	mux.HandleFunc("POST "+officersPagePath+"/{officer}/refusals/{item}/refile", s.requireOperatorWrite(s.handleOfficerRefusalRefile))
	mux.HandleFunc("POST "+officersPagePath+"/{officer}/refusals/{item}/drop", s.requireOperatorWrite(s.handleOfficerRefusalDrop))
	// Chat with it, and its confirm cards (officer_operator_chat.go).
	mux.HandleFunc("POST "+officersPagePath+"/{officer}/chat", s.requireOperatorWrite(s.handleOfficerChat))
	mux.HandleFunc("POST "+officersPagePath+"/{officer}/chat/cards/{card}/approve", s.requireOperatorWrite(s.handleOfficerChatCardApprove))
	mux.HandleFunc("POST "+officersPagePath+"/{officer}/chat/cards/{card}/reject", s.requireOperatorWrite(s.handleOfficerChatCardReject))
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
	if sc, ok := s.scopeOf(r.Context(), r.FormValue("owner")); ok {
		return sc, true
	}
	s.renderNotice(w, r, http.StatusNotFound, "No such officer", officerNotFound)
	return officerScope{}, false
}

// scopeOf is scopeFor's rule over an `owner` value raw, answering nothing:
// "" (or the person's own id) is the person's own officers; a team they are
// in is that team's, managed when they manage it; anything else is ok false.
func (s *Server) scopeOf(ctx context.Context, raw string) (officerScope, bool) {
	p := authz.MustFrom(ctx)
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == strconv.FormatInt(p.UserID, 10) {
		return officerScope{Owner: p.UserID, Manage: true}, true
	}
	team, err := strconv.ParseInt(raw, 10, 64)
	if err == nil && team > 0 {
		if role, in := p.TeamScope().RoleIn(team); in {
			return officerScope{Owner: team, Team: true, Name: s.teamNames(ctx, []int64{team})[team], Manage: role == authz.TeamManager}, true
		}
	}
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

// plainWhere is where an officer runs, in a person's words (no runtime
// names): on a named machine, on muster's own servers, wherever muster can,
// or nowhere yet. whereItRuns is the technical line, behind the head's ⓘ.
func plainWhere(v declaredOfficerView) string {
	on, host := v.RunsOn, v.RunsOnHost
	if on == "" {
		on, host = v.Runtime, v.Host
	}
	switch on {
	case officers.RuntimePod:
		return "on muster's own servers"
	case officers.RuntimeHost:
		if host != "" {
			return "on " + host
		}
		return "on one of your machines"
	}
	if v.ManagedBy == managedByDef {
		return "nowhere yet"
	}
	return "wherever muster can run it"
}

// plainNotRunning is why an officer does not run, in a person's words, from
// where it is declared to run — the server's own reason (v.Reason, which
// names settings and flags) is behind the head's ⓘ.
func plainNotRunning(v declaredOfficerView) string {
	switch {
	case v.ManagedBy == managedByDef:
		return "nothing is set up to run it."
	case v.Runtime == officers.RuntimeHost && v.Host != "":
		return "it runs on " + v.Host + ", and this server can't start it on that machine right now."
	case v.Runtime == officers.RuntimeHost:
		return "it runs on one of your machines, and this server can't start it on any of them right now."
	case v.Runtime == officers.RuntimePod:
		return "it runs on muster's own servers, and this server can't start it there."
	}
	return "this server has nowhere it can start it right now."
}

// officerHeadSheet is the officer head's ⓘ: its id, the technical
// where-it-runs line and, when it does not run, the server's own reason.
func officerHeadSheet(v declaredOfficerView) ui.InfoSheet {
	code := "officer " + v.ID + " · runs " + whereItRuns(v)
	if v.Repo != "" {
		code += " · in " + v.Repo
	} else if v.ManagedBy != managedByDef {
		code += " · in a scratch directory"
	}
	switch v.ManagedBy {
	case officers.ManagedByEnv:
		code += " · declared by MUSTER_OFFICERS"
	case managedByDef:
		code += " · a def in the built-in store only (muster officer create declares where it runs)"
	}
	if !v.Running && v.Reason != "" {
		code += " · not running: " + v.Reason
	}
	return ui.InfoSheet{ID: "officer-head-info", Label: "Technical details of this officer", Heading: "Details",
		Lead: "What muster records for this officer:", Code: code, Inline: true}
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
	// Each team the person is in: its officers, read by every member.
	type teamList struct {
		sc   officerScope
		list []declaredOfficerView
	}
	var teams []int64
	roles := map[int64]authz.TeamRole{}
	for _, m := range p.TeamScope().List() {
		teams = append(teams, m.Team)
		roles[m.Team] = m.Role
	}
	names := s.teamNames(ctx, teams)
	var teamLists []teamList
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
		teamLists = append(teamLists, teamList{officerScope{Owner: t, Team: true, Name: names[t], Manage: manage}, list})
	}
	// What waits on the person, and what is new, for every officer listed:
	// the drafts and asks read once (readNeeds), the "new" counts in one
	// query (UnreadCounts).
	rd := s.readNeeds(ctx)
	open := rd.asks
	owners := []int64{p.UserID}
	for _, tl := range teamLists {
		owners = append(owners, tl.sc.Owner)
	}
	unread, err := s.officerStore.UnreadCounts(ctx, owners, s.officerNow())
	if err != nil {
		s.logger.Printf("officers: the list's new counts: %v (drawn without them)", err)
	}
	extra := map[builtin.OfficerRef]listExtra{}
	var links []needsLink
	collect := func(sc officerScope, list []declaredOfficerView) {
		for _, v := range list {
			ref := builtin.OfficerRef{Owner: sc.Owner, Officer: v.ID}
			n := s.officerNeedsOf(ctx, rd, sc.Owner, v.ID, v.Backend == officers.BackendBuiltin).count()
			extra[ref] = listExtra{needs: n, unread: unread[ref]}
			name := v.Name
			if name == "" {
				name = v.ID
			}
			if sc.Team {
				name += " (" + sc.Name + ")"
			}
			links = append(links, needsLink{name: name, href: officerHref(sc, v.ID, ""), n: n})
		}
	}
	collect(officerScope{Owner: p.UserID, Manage: true}, mine)
	for _, tl := range teamLists {
		collect(tl.sc, tl.list)
	}
	var b strings.Builder
	if n, ok := officerListNotices[r.URL.Query().Get("notice")]; ok {
		b.WriteString(`<p role="status" class="rounded-lg bg-s2 px-3 py-2 text-sm text-fg" id="officer-notice">` + html.EscapeString(n) + `</p>`)
	}
	if len(links) > 0 {
		b.WriteString(needsSummaryHTML(links))
	}
	b.WriteString(`<section class="` + adminCard + `" id="officers-mine"><div class="flex flex-wrap items-center gap-2"><h1 class="flex-1 text-base font-semibold text-fg">Officers</h1>
<a href="` + officersPagePath + `/new" id="officer-new" class="` + adminButton + ` inline-flex items-center">New officer</a></div>`)
	if len(mine) == 0 {
		b.WriteString(`<p class="text-sm text-fg2" id="officers-empty">An officer is a Claude Code agent with standing instructions that muster wakes when there is work for it — on a schedule, or when you hand it something — and that asks you before anything leaves.</p>`)
	} else {
		b.WriteString(s.officerList(ctx, officerScope{Owner: p.UserID, Manage: true}, mine, open, extra))
	}
	b.WriteString(`</section>`)
	for _, tl := range teamLists {
		sc, list := tl.sc, tl.list
		manage := sc.Manage
		id := strconv.FormatInt(sc.Owner, 10)
		b.WriteString(`<section class="` + adminCard + `" id="officers-team-` + id + `" data-officers-team="` + id + `"><div class="flex flex-wrap items-center gap-2"><h2 class="flex-1 text-base font-semibold text-fg">` +
			html.EscapeString(sc.Name) + `'s officers</h2>`)
		if manage {
			b.WriteString(`<a href="` + officersPagePath + `/new` + sc.ownerQuery() + `" class="` + adminQuiet + ` inline-flex items-center">New team officer</a>`)
		}
		b.WriteString(`</div>`)
		if len(list) == 0 {
			b.WriteString(`<p class="text-sm text-fg2">This team has no officers yet.</p>`)
		} else {
			b.WriteString(s.officerList(ctx, sc, list, open, extra))
		}
		b.WriteString(`</section>`)
	}
	b.WriteString(s.runtimeAvailability(ctx, p.UserID, "officers-runtimes"))
	s.writeNavDocument(w, r, http.StatusOK, "Officers", ui.NavOfficers, false, `<div class="flex flex-col gap-4">`+b.String()+`</div>`, navWide)
}

// officerListNotices are the words the list's ?notice= shows (a closed set).
var officerListNotices = map[string]string{
	"deleted": "Deleted, with its queue and schedules; its instances are being stopped.",
}

// listExtra is what the list adds to an officer's row: how many decisions
// wait on the viewer, and how many of its "What it did" entries are new.
type listExtra struct {
	needs, unread int
}

// officerList is the officers in list, each one row: name, where it runs, its
// status, what is open on it, what waits on the viewer and what is new.
func (s *Server) officerList(ctx context.Context, sc officerScope, list []declaredOfficerView, open []attention.Ask, extra map[builtin.OfficerRef]listExtra) string {
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
		sheet := officerHeadSheet(v)
		sheet.ID = "officer-row-" + strconv.FormatInt(sc.Owner, 10) + "-" + strings.NewReplacer(":", "-").Replace(v.ID) + "-info"
		b.WriteString(`</div><div class="flex items-center gap-1"><span class="min-w-0 flex-1 text-xs text-fg2" data-officer-where>runs ` + html.EscapeString(plainWhere(v)) + `</span>` +
			ui.InfoSheetHTML(sheet) + `</div>`)
		// 🔴 NO "LAST PASS" HERE (round 0's D2): the wake loop's memory of its
		// last pass is per replica and forgotten by a restart, so it is not
		// drawn until a shared record of passes exists.
		var facts []string
		if v.Backend == officers.BackendBuiltin {
			facts = append(facts, plural(openItems, "open item"))
		}
		facts = append(facts, plural(asks, "ask"))
		x := extra[builtin.OfficerRef{Owner: sc.Owner, Officer: v.ID}]
		if x.needs > 0 {
			facts = append(facts, strconv.Itoa(x.needs)+" waiting on you")
		}
		if w := unreadWords(x.unread); w != "" {
			facts = append(facts, w)
		}
		b.WriteString(`<span class="text-xs text-fg2" data-officer-facts>` + html.EscapeString(strings.Join(facts, " · ")) + `</span>`)
		if !v.Running && v.Reason != "" {
			b.WriteString(`<span class="text-xs text-st-error-fg" data-officer-reason>Not running: ` + html.EscapeString(plainNotRunning(v)) + `</span>`)
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

// The templates the wizard's first step offers are officer_templates.go's
// catalogue (officerTemplates, templateNamed).

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
	// ModeWas is the stored mode the edit form was drawn with (its hidden
	// mode_was field): a save that leaves the mode at it writes no mode.
	ModeWas string
	// Paused is the stored switch (edit only: the form never changes it).
	Paused bool
	// Policy is the stored type policy (edit only), kept but for its
	// capabilities.
	Policy json.RawMessage
	// Kept is the stored declaration (edit only): the fields the form does
	// not show — max instances, idle, poll, model — are replaced with their
	// stored values, never reset to the defaults by a save.
	Kept *officers.Declaration
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
	{builtin.CapSessionMinder, "Mind my stopped sessions: muster queues each one as an item for it, and my chief stops nudging me while it is switched on and running (needs session state; my stop notifications still arrive)"},
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
		Caps: r.Form["cap"], Connectors: r.FormValue("connectors"), ReadRepos: r.FormValue("read_repos"), Mode: r.FormValue("mode"),
		ModeWas: r.FormValue(editModeWasField)}
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
	if k := f.Kept; k != nil {
		in.MaxInstances, in.Model = k.MaxInstances, k.Model
		if k.IdleAfter > 0 {
			in.IdleAfter = k.IdleAfter.String()
		}
		if k.PollInterval > 0 {
			in.PollInterval = k.PollInterval.String()
		}
	}
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
		case field == "policy":
			field = "cap" // the policy is the permissions step's boxes
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
	// refreshed: the wizard was redrawn for another owner (its "Whose
	// officer" choice changed), so it opens at that step.
	refreshed bool
	// picked: the wizard was redrawn with a template's starting values (its
	// "Start from" choice), so it opens at the step after it.
	picked bool
}

// escalationChoice is one person (or the team) an escalation can also ask.
type escalationChoice struct {
	ID    int64
	Label string
	// Out: a chosen id outside the reach, which the save refuses.
	Out bool
}

// escalationChoices are who an officer of sc's can also ask on an escalation
// (F6: chosen, never typed as numbers): EXACTLY the server's reach
// (escalationReach, the one rule the save is checked against) — for a team
// officer the team itself, then its members; for a personal one the people on
// the owner's teams — less a personal officer's owner, who is always asked.
//
// An id already chosen that is outside the reach (stored before Q1's rule, or
// typed into a crafted form) is drawn ticked and named only by number, marked
// as one the save will refuse, so the person sees it and unticks it: an edit
// never drops a recipient silently. It is drawn the same whether the id names
// someone or no one.
func (s *Server) escalationChoices(ctx context.Context, sc officerScope, chosen []int64) []escalationChoice {
	me := authz.MustFrom(ctx).UserID
	reach, err := s.escalationReach(ctx, sc.Owner, sc.Team)
	if err != nil {
		s.logger.Printf("officers: read who officer owner %d's escalations reach: %v", sc.Owner, err)
	}
	seen := map[int64]bool{}
	var out []escalationChoice
	for _, t := range reach {
		seen[t.ID] = true
		switch {
		case t.Self:
			continue // always asked
		case t.Team:
			out = append(out, escalationChoice{ID: t.ID, Label: "The whole team (" + sc.Name + "): one ask any member can answer"})
			continue
		}
		label := t.Label
		if t.ID == me {
			label += " (you)"
		}
		out = append(out, escalationChoice{ID: t.ID, Label: label})
	}
	for _, c := range chosen {
		if !seen[c] {
			seen[c] = true
			out = append(out, escalationChoice{ID: c, Label: "user #" + strconv.FormatInt(c, 10) + " — cannot be asked: not someone who can see this officer (untick to save)", Out: true})
		}
	}
	return out
}

// inReach is ids less any outside sc's escalation reach (the wizard's owner
// change: a choice made for one owner is not carried to another's officer).
func (s *Server) inReach(ctx context.Context, sc officerScope, ids []int64) []int64 {
	reach, err := s.escalationReach(ctx, sc.Owner, sc.Team)
	if err != nil {
		return nil
	}
	in := map[int64]bool{}
	for _, t := range reach {
		in[t.ID] = true
	}
	var out []int64
	for _, id := range ids {
		if in[id] {
			out = append(out, id)
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

// wizardStepsShown is the numbers of the steps the form draws: edit's four;
// the wizard's six, less "Start from" while there is only one template to
// start from (round 0's D1 — O-D's catalogue brings the step back).
func wizardStepsShown(edit bool) []int {
	switch {
	case edit:
		return []int{2, 3, 5, 6}
	case len(officerTemplates) < 2:
		return []int{2, 3, 4, 5, 6}
	}
	return []int{1, 2, 3, 4, 5, 6}
}

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
		first = wizardStepsShown(fp.edit)[0]
		switch {
		case fp.top != "":
			first = 6
		case fp.refreshed:
			first = 2 // the owner choice's own step
		case fp.picked:
			first = 2 // the step after "Start from"
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
	b.WriteString(`<form method="POST" action="` + esc(action) + `" id="officer-form" class="flex flex-col gap-4" data-wizard="` + strconv.FormatBool(!fp.edit) + `" data-first-step="` + strconv.Itoa(first) + `" data-template="` + esc(f.Template) + `">`)
	if fp.edit {
		b.WriteString(fp.sc.ownerField())
		if f.ModeWas != "" {
			b.WriteString(`<input type="hidden" name="` + editModeWasField + `" value="` + esc(f.ModeWas) + `">`)
		}
	}
	// The steps drawn: each step keeps its number (data-step, stepOfField's),
	// and the person reads them numbered as they are drawn.
	steps := wizardStepsShown(fp.edit)
	shown := map[int]int{}
	for i, n := range steps {
		shown[n] = i + 1
	}
	if !fp.edit {
		b.WriteString(`<ol class="flex flex-wrap gap-x-3 gap-y-1 text-xs text-fg2" id="wizard-progress" hidden>`)
		for i, n := range steps {
			b.WriteString(`<li data-progress="` + strconv.Itoa(n) + `">` + strconv.Itoa(i+1) + `. ` + esc(wizardSteps[n-1]) + `</li>`)
		}
		b.WriteString(`</ol>`)
	}
	section := func(n int, legend string) {
		b.WriteString(`<fieldset class="flex flex-col gap-3" data-step="` + strconv.Itoa(n) + `" id="step-` + strconv.Itoa(n) + `"><legend class="mb-1 text-sm font-semibold text-fg">`)
		if !fp.edit {
			b.WriteString(strconv.Itoa(shown[n]) + `. `)
		}
		b.WriteString(esc(legend) + `</legend>`)
	}
	// 1. template — 🔴 only when there is a choice (round 0's D1): with one
	// template the step is a question with one answer, so its value rides in
	// a hidden field and the wizard starts at the name.
	if !fp.edit && len(officerTemplates) < 2 {
		b.WriteString(`<input type="hidden" name="template" value="` + esc(officerTemplates[0].Key) + `">`)
	}
	if !fp.edit && len(officerTemplates) >= 2 {
		section(1, wizardSteps[0])
		for i, t := range officerTemplates {
			on := f.Template == t.Key || f.Template == "" && i == 0
			b.WriteString(`<label class="flex items-start gap-2 rounded-lg p-3 ring-1 ring-inset ring-edge"><input type="radio" name="template" value="` + esc(t.Key) + `"` + checkedIf(on) + ` class="mt-1">
<span class="flex flex-col"><span class="text-sm font-semibold text-fg">` + esc(t.Name) + `</span><span class="text-xs text-fg2">` + esc(t.Summary) + `</span></span></label>`)
		}
		// Without the script, this redraws the form with the chosen
		// template's starting values; the script submits it when Next leaves
		// this step with another template chosen, and removes the button.
		b.WriteString(`<p class="text-xs text-fg2">A template only fills in the next steps: you can change everything it sets.</p>
<button type="submit" name="` + wizardTemplateField + `" value="1" formnovalidate class="` + adminQuiet + ` self-start" id="officer-template-pick">Use this starting point</button>`)
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
		// Without the script, this redraws the form for the owner chosen (its
		// machines, what auto picks, whom it can ask); the script submits it on
		// a change and removes the button.
		b.WriteString(`</select><button type="submit" name="` + wizardRefreshField + `" value="1" formnovalidate class="` + adminQuiet + ` self-start" id="officer-owner-refresh">Show choices for this owner</button>`)
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
		if c.Key == builtin.CapSessionMinder {
			b.WriteString(`<p class="pl-6 text-xs text-fg2" data-minder-note>Only one session minder is used per owner (the first by id), and it has no effect on a team's officers.</p>`)
		}
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
		b.WriteString(`<p class="text-xs text-fg2" id="officer-escalate-none">Nobody else to choose: an escalation asks only people who can see the officer, and the people on your teams appear here.</p>`)
	}
	for _, who := range fp.people {
		on := false
		for _, e := range f.Escalate {
			on = on || e == who.ID
		}
		out := ""
		if who.Out {
			out = ` data-escalate-out`
		}
		b.WriteString(`<label class="flex items-start gap-2"` + out + `><input type="checkbox" name="escalate_to" value="` + strconv.FormatInt(who.ID, 10) + `"` + checkedIf(on) + ` class="mt-1"><span class="text-sm text-fg">` + esc(who.Label) + `</span></label>`)
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
// "Officers in the Compose stack (host sessions)"): drawn on the permissions
// step and on a host officer's page.
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
	// 🔴 THE CHOSEN MACHINE IS ALWAYS AN OPTION (round 0's 🟡). The hub lists
	// only the hosts connected to THIS server now; a host officer whose machine
	// is offline, or connected to another replica, is still on it. Its machine
	// is drawn selected (and the host choice enabled), so a save never moves it
	// to another machine or to auto.
	stored := runtime == officers.RuntimeHost && f.Host != "" && !slices.ContainsFunc(opts.Hosts, func(h hostOption) bool { return h.Host == f.Host })
	hostOff := ""
	hostWhy := "A Claude Code window on a machine of yours that muster mirrors."
	switch {
	case stored:
		hostWhy = f.Host + " is not connected to this server now; it stays on it."
	case opts.HostReason != "":
		hostOff, hostWhy = " disabled", "Not available here: "+opts.HostReason+"."
	case len(opts.Hosts) == 0:
		hostOff, hostWhy = " disabled", "No machine of yours is mirrored: run muster-mirror with -interactive -agent-spawn on one, and it appears here."
	}
	b.WriteString(`<label class="flex items-start gap-2"><input type="radio" name="runtime" value="host"` + checkedIf(runtime == officers.RuntimeHost) + hostOff + ` class="mt-1" data-label="On one of my machines">
<span class="flex flex-col"><span class="text-sm font-semibold text-fg">On one of my machines</span><span class="text-xs text-fg2" id="runtime-host-why">` + esc(hostWhy) + `</span></span></label>`)
	if stored || len(opts.Hosts) > 0 && opts.HostReason == "" {
		b.WriteString(`<div class="pl-5 flex flex-col gap-1"><label for="officer-host" class="text-xs text-fg2">Machine</label><select id="officer-host" name="host" class="w-full min-w-0 ` + adminInput + `"` + describedBy(fp.errs, "host") + `>`)
		picked := f.Host
		if picked == "" {
			for _, h := range opts.Hosts {
				if h.Qualifies {
					picked = h.Host
					break
				}
			}
		}
		if stored {
			b.WriteString(`<option value="` + esc(f.Host) + `" selected data-qualifies="false" data-offline>` + esc(f.Host+" — offline: not connected to this server now") + `</option>`)
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
  var at = Math.max(0, steps.findIndex(function(s){ return s.dataset.step === form.dataset.firstStep; }));
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
  var pick = document.getElementById('officer-template-pick');
  if (pick) pick.remove();
  next.addEventListener('click', function(){
    // Leaving "Start from" with another template chosen redraws the form with
    // its starting values, opening at the next step.
    if (steps[at].dataset.step === '1' && val('template') !== form.dataset.template) {
      var t = document.createElement('input'); t.type = 'hidden'; t.name = '` + wizardTemplateField + `'; t.value = '1';
      form.appendChild(t); form.submit(); return;
    }
    if (stepValid(at)) { show(at + 1); var f = steps[at].querySelector('input:not([type=hidden]), textarea, select'); if (f) f.focus(); } });
  back.addEventListener('click', function(){ show(at - 1); });
  form.addEventListener('change', hostRisk);
  var owner = document.getElementById('officer-owner'), refresh = document.getElementById('officer-owner-refresh');
  if (owner && refresh) {
    refresh.remove();
    owner.addEventListener('change', function(){
      var f = document.createElement('input'); f.type = 'hidden'; f.name = '` + wizardRefreshField + `'; f.value = '1';
      form.appendChild(f); form.submit();
    });
  }
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
	f.Caps = append([]string(nil), t.Values.Caps...)
	f.Template = t.Key
	s.renderOfficerForm(w, r, http.StatusOK, s.formPageFor(r.Context(), sc, f, nil, "", false, ""))
}

// formPageFor is the form page's data for scope sc: where an officer of the
// scope's owner could run, and whom its escalations can ask — both the
// OWNER's, so a change of owner redraws them (wizardRefresh).
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

// wizardRefreshField is the wizard's "redraw for this owner" submission: the
// form posted with it set re-renders for the owner it names — that owner's
// machines, what auto picks for it, whom its escalations can ask — and
// creates nothing. Its button is the no-JS path; the wizard's script submits
// it when the owner select changes (round 0's 🟡: the rest of the form
// followed the owner the page was drawn for).
const wizardRefreshField = "refresh_owner"

// wizardRefresh is the wizard redrawn for sc with what was typed: a host and
// escalation choices of another owner's are dropped (they are not this
// owner's to choose), everything else kept.
func (s *Server) wizardRefresh(w http.ResponseWriter, r *http.Request, sc officerScope) {
	ctx := r.Context()
	f := readForm(r)
	fp := s.formPageFor(ctx, sc, formValues{}, nil, "", false, "")
	if !slices.ContainsFunc(fp.opts.Hosts, func(h hostOption) bool { return h.Host == f.Host }) {
		f.Host = ""
	}
	f.Escalate = s.inReach(ctx, sc, f.Escalate)
	fp.f, fp.people, fp.refreshed = f, s.escalationChoices(ctx, sc, f.Escalate), true
	s.renderOfficerForm(w, r, http.StatusOK, fp)
}

// wizardTemplateField is the wizard's "start from this template" submission:
// the form posted with it set re-renders with the chosen template's starting
// values (for the owner it names) and creates nothing. Its button is the
// no-JS path; the wizard's script submits it when Next leaves "Start from"
// with a template other than the one the form was drawn with.
const wizardTemplateField = "pick_template"

// wizardPickTemplate is the wizard redrawn for sc with the chosen template's
// starting values, opening at the step after "Start from". 🔴 THE TEMPLATE'S
// VALUES REPLACE what was typed (a new starting point), all but the owner,
// which the scope already carries.
func (s *Server) wizardPickTemplate(w http.ResponseWriter, r *http.Request, sc officerScope) {
	t := templateNamed(r.FormValue("template"))
	f := t.Values
	f.Caps = append([]string(nil), t.Values.Caps...)
	f.Template = t.Key
	fp := s.formPageFor(r.Context(), sc, f, nil, "", false, "")
	fp.picked = true
	s.renderOfficerForm(w, r, http.StatusOK, fp)
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
	if r.FormValue(wizardRefreshField) != "" {
		s.wizardRefresh(w, r, sc)
		return
	}
	if r.FormValue(wizardTemplateField) != "" {
		s.wizardPickTemplate(w, r, sc)
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
		if sc.Team {
			s.auditTeam(ctx, actorOf(r), sc.Owner, "officer.schedule.put", "officer_def:"+row.Officer, map[string]any{"schedule": "pass", "cron": cron})
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
// 🔴 An error reading its type is answered, never drawn as an empty charter
// and policy: a save of that form would write them as `{}` (round 0's 🟢).
func (s *Server) declaredFormOf(ctx context.Context, owner int64, row builtin.DeclaredRow) (formValues, error) {
	f := formValues{Name: row.TypeName, Runtime: row.Fields.Runtime, Host: row.Fields.Host, Repo: row.Fields.Repo,
		Connectors: strings.Join(row.Fields.Connectors, ", "), ReadRepos: strings.Join(row.Fields.ReadRepos, ", "),
		Escalate: row.EscalateTo, Mode: row.Mode, ModeWas: row.Mode, Paused: !row.Enabled, Kept: &row.Fields}
	t, err := s.officerStore.GetType(ctx, owner, row.TypeKey)
	if err != nil {
		return formValues{}, fmt.Errorf("officers: read officer %s's type %s: %w", row.Officer, row.TypeKey, err)
	}
	f.Charter, f.Policy, f.Caps = t.Charter, t.Policy, policyCaps(t.Policy)
	return f, nil
}

// formReadFailed answers a form whose stored officer could not be read.
func (s *Server) formReadFailed(w http.ResponseWriter, r *http.Request, err error) {
	s.logger.Printf("officers: %v", err)
	s.renderNotice(w, r, http.StatusInternalServerError, "Could not load the officer", "Try again in a moment.")
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
		if code == http.StatusInternalServerError {
			s.logger.Printf("officers: read officer %s of %d: %v", id, sc.Owner, err)
		}
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
	f, err := s.declaredFormOf(r.Context(), sc.Owner, row)
	if err != nil {
		s.formReadFailed(w, r, err)
		return
	}
	s.renderOfficerForm(w, r, http.StatusOK, s.formPageFor(r.Context(), sc, f, nil, "", true, row.Officer))
}

// editModeWasField is the edit form's hidden copy of the mode it was drawn
// with: a save whose mode is still that one leaves the stored mode alone.
const editModeWasField = "mode_was"

func (s *Server) handleOfficerEdit(w http.ResponseWriter, r *http.Request) {
	sc, row, ok := s.editableRow(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	stored, err := s.declaredFormOf(ctx, sc.Owner, row)
	if err != nil {
		s.formReadFailed(w, r, err)
		return
	}
	f := readForm(r)
	f.ID, f.Paused, f.Policy, f.Kept = row.Officer, stored.Paused, stored.Policy, stored.Kept
	// 🔴 NEVER MOVED BY A FORM THAT DID NOT SAY WHERE (round 0's 🟡). A form
	// with no runtime (its choice was drawn disabled, so the browser sent
	// none) keeps the stored runtime and host; a host officer's form with no
	// machine keeps the stored machine — never auto, never another host.
	if f.Runtime == "" {
		f.Runtime, f.Host = stored.Runtime, stored.Host
	}
	if f.Runtime == officers.RuntimeHost && f.Host == "" && stored.Runtime == officers.RuntimeHost {
		f.Host = stored.Host
	}
	in, errs := declaredInOf(f, true)
	if len(errs) > 0 {
		s.renderOfficerForm(w, r, http.StatusBadRequest, s.formPageFor(ctx, sc, f, errs, "", true, row.Officer))
		return
	}
	// The switch is never the form's; the mode only when the person changed it.
	in.keepEnabled = true
	in.keepMode = f.ModeWas != "" && f.ModeWas == *in.Mode
	if err := s.updateDeclared(ctx, sc.Owner, sc.Team, row.Officer, in); err != nil {
		if fe, top := officerFieldErrs(err); len(fe) > 0 || top != "" {
			s.renderOfficerForm(w, r, http.StatusBadRequest, s.formPageFor(ctx, sc, f, fe, top, true, row.Officer))
			return
		}
		code, msg := officerPageRefusal(err)
		if code == http.StatusInternalServerError {
			s.logger.Printf("officers: save from the edit form: %v", err)
		}
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

// officerSwitch is Pause / Resume. 🔴 ON THE SWITCH ONLY, so an officer
// MUSTER_OFFICERS declares takes it too (controlTarget): a database-declared
// one's is its declared row's (switchDeclared), an env one's its def's — the
// switch `muster officer def put --enabled` sets, which the wake loop honours
// for every builtin officer (officers.SwitchGated). Its declaration is never
// written.
func (s *Server) officerSwitch(w http.ResponseWriter, r *http.Request, on bool) {
	sc, id, key, env, ok := s.controlTarget(w, r)
	if !ok {
		return
	}
	var err error
	if env {
		if err = s.officerStore.SetEnabled(r.Context(), sc.Owner, id, on); err == nil {
			s.logger.Printf("officers: user %d switched env officer %s of owner %d on=%v", authz.MustFrom(r.Context()).UserID, id, sc.Owner, on)
			if key != "" && s.officerRT.rec != nil {
				s.officerRT.rec.Due(key)
			}
		}
	} else {
		err = s.switchDeclared(r.Context(), sc.Owner, id, on)
	}
	notice := "paused"
	if on {
		notice = "resumed"
	}
	if err == nil && sc.Team {
		s.auditTeam(r.Context(), actorOf(r), sc.Owner, "officer."+strings.TrimSuffix(notice, "d"), "officer_def:"+id, nil)
	}
	s.officerActionDone(w, r, sc, id, notice, err)
}

// handleOfficerMode is the Shadow/Live switch: the mode and nothing else
// (modeDeclared), so it never rewrites the record the page read.
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
	err := s.modeDeclared(ctx, sc.Owner, row.Officer, mode)
	if err == nil && sc.Team {
		s.auditTeam(ctx, actorOf(r), sc.Owner, "officer.mode", "officer_def:"+row.Officer, map[string]any{"mode": mode})
	}
	s.officerActionDone(w, r, sc, row.Officer, "mode-"+mode, err)
}

// runNowKind is the kind of item "Run now" puts in an officer's queue.
const runNowKind = "manual.run"

// handleOfficerRunNow is Run now: one owner item in the queue. It writes the
// QUEUE only, so an officer MUSTER_OFFICERS declares takes it too
// (controlTarget). An owner item gets past a spent daily budget
// (officers.Officer.BypassesBudget): Run now is how a held officer is woken.
func (s *Server) handleOfficerRunNow(w http.ResponseWriter, r *http.Request) {
	sc, officer, key, _, ok := s.controlTarget(w, r)
	if !ok {
		return
	}
	// One live "run now" at a time: a second tap while the first is still
	// open returns that item (the store's dedupe on kind and subject).
	id, created, err := s.officerStore.EnqueueChecked(r.Context(), sc.Owner, builtin.NewItem{Officer: officer, Kind: runNowKind, SubjectKey: "run-now", Origin: officers.OriginOwner}, s.officerNow())
	notice := "queued"
	if err == nil && !created {
		notice = "already-queued"
	}
	if err == nil {
		s.logger.Printf("officers: user %d ran officer %s of owner %d now (item %d, created %v)", authz.MustFrom(r.Context()).UserID, officer, sc.Owner, id, created)
		if sc.Team {
			s.auditTeam(r.Context(), actorOf(r), sc.Owner, "officer.run", "officer_def:"+officer, map[string]any{"item": id, "created": created})
		}
		if key != "" && s.officerRT.rec != nil {
			s.officerRT.rec.Due(key)
		}
	}
	s.officerActionDone(w, r, sc, officer, notice, err)
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
	if err == nil && sc.Team {
		s.auditTeam(r.Context(), actorOf(r), sc.Owner, "officer.schedule.put", "officer_def:"+row.Officer, map[string]any{"schedule": name, "cron": cron})
	}
	s.officerActionDone(w, r, sc, row.Officer, "schedule-saved", err)
}

func (s *Server) handleOfficerScheduleDeletePage(w http.ResponseWriter, r *http.Request) {
	sc, row, ok := s.editableRow(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	err := s.officerStore.DeleteSchedule(r.Context(), sc.Owner, row.Officer, name)
	if err == nil && sc.Team {
		s.auditTeam(r.Context(), actorOf(r), sc.Owner, "officer.schedule.delete", "officer_def:"+row.Officer, map[string]any{"schedule": name})
	}
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
	// The Needs-you section's decisions (officer_needs_you.go).
	"draft-approved": "Approved: its plugin sends exactly that, once.",
	"draft-rejected": "Rejected: it is not sent, and the officer is told why.",
	"batch-decided":  "Decided: the ticked ones go to their plugin, the rest were rejected.",
	"refiled":        "Re-filed: the officer files it again at that time, and you approve the new draft.",
	"dropped":        "Dropped: the officer will not redraft it.",
	"cap-raised":     "Raised for today: it is woken for new work again, and its budget goes back at the next 00:00 UTC.",
	// The chat's (officer_operator_chat.go).
	"chat-sent":      "Sent. It answers below in a moment.",
	"card-approved":  "Approved: its plugin sends exactly what the card showed, once.",
	"card-rejected":  "Rejected: it is not sent, and the officer is told.",
	"card-dismissed": "Dismissed: nothing was changed.",
}

// officerPageNotice is the words of a notice code on an officer's page: its
// own (officerNotices), else an answer form's (composerNotices: an ask
// answered on this page that was not recorded). ok false for any other.
func officerPageNotice(code string) (string, bool) {
	if n, ok := officerNotices[code]; ok {
		return n, true
	}
	if n, ok := composerNotices[code]; ok && strings.HasPrefix(code, "ask-") {
		return n.text, true
	}
	return "", false
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
	if n, ok := officerPageNotice(r.URL.Query().Get("notice")); ok && problem == "" {
		b.WriteString(`<p role="status" class="rounded-lg bg-s2 px-3 py-2 text-sm text-fg" id="officer-notice">` + esc(n) + `</p>`)
	}
	// The head: who, where, how it is.
	b.WriteString(`<section class="` + adminCard + `" id="officer-head" data-officer="` + esc(v.ID) + `" data-managed-by="` + v.ManagedBy + `">
<p class="text-xs"><a href="` + officersPagePath + `" class="text-fg2 underline-offset-2 hover:underline">← Officers</a></p>
<div class="flex flex-wrap items-center gap-2"><h1 class="min-w-0 flex-1 break-words text-base font-semibold text-fg">` + esc(name) + `</h1>` + statusChip(status) + `</div>
`)
	// 🔴 THE TOP LAYER IS PLAIN WORDS (the operator rule): where it runs and,
	// when it does not, why — in a sentence. Its id, the technical
	// where-it-runs line and the server's own reason (which names settings)
	// are behind the ⓘ (officerHeadSheet).
	plain := ""
	if sc.Team {
		plain = sc.Name + "'s · "
	}
	plain += "runs " + plainWhere(v)
	b.WriteString(`<div class="flex items-center gap-1"><p class="min-w-0 flex-1 text-xs text-fg2" data-officer-where>` + esc(plain) + `</p>` +
		ui.InfoSheetHTML(officerHeadSheet(v)) + `</div>`)
	if !v.Running && v.Reason != "" {
		b.WriteString(`<p class="text-sm text-st-error-fg" id="officer-reason">Not running: ` + esc(plainNotRunning(v)) + `</p>`)
	}
	switch v.ManagedBy {
	case officers.ManagedByEnv:
		b.WriteString(`<p class="text-sm text-fg2" id="officer-read-only">Its setup comes from this server's own settings, so it is changed there, not here. Its decisions, Run now and Pause still work here.</p>`)
	case managedByDef:
		b.WriteString(`<p class="text-sm text-fg2" id="officer-read-only">It has instructions but nowhere to run yet, so muster does not start it.</p>`)
	}
	if v.RunsOn == officers.RuntimeHost || v.Runtime == officers.RuntimeHost {
		b.WriteString(`<p class="rounded-lg bg-st-warning-bg px-3 py-2 text-sm text-st-warning-fg" data-host-risk>` + esc(hostRiskLine) + `</p>`)
	}
	if writable {
		form := func(suffix, label, cls string, extra string) string {
			return `<form method="POST" action="` + esc(officersPagePath+"/"+url.PathEscape(v.ID)+suffix) + `">` + sc.ownerField() + extra +
				`<button type="submit" class="` + cls + `">` + label + `</button></form>`
		}
		// Run now and Pause / Resume are the Needs-you section's run
		// controls (officerRunHTML), drawn for every officer they apply to.
		b.WriteString(`<div class="flex flex-wrap gap-2" id="officer-actions">`)
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

	// Needs you, then What it did (officer_needs_you.go).
	controllable := sc.Manage && builtinBackend && (v.ManagedBy == officers.ManagedByDB || v.ManagedBy == officers.ManagedByEnv)
	needs := s.officerNeedsOf(ctx, s.readNeeds(ctx), sc.Owner, v.ID, builtinBackend)
	b.WriteString(s.needsYouHTML(ctx, sc, v, needs, s.officerRunHTML(ctx, sc, v, controllable), controllable))
	// Chat with it (officer_operator_chat.go), right below what waits on
	// the person: its confirm cards are decisions too.
	chat := s.officerChatHTML(r, sc, v)
	b.WriteString(chat)
	if len(needs.drafts) > 0 || officerChatHasOpenCard(chat) {
		// The Approve's "Confirm it's you" dialog and form script — the Asks
		// page's own (draftStepUpHTML); the server checks it either way.
		b.WriteString(s.draftStepUpHTML(r))
	}
	// 🔴 THE LOOK IS RECORDED ONLY WHEN THE PAGE IS DRAWN FOR A GET: a refusal
	// re-drawn after a POST is not the person reading what it did.
	var seen *time.Time
	if builtinBackend && r.Method == http.MethodGet && code == http.StatusOK {
		prev, err := s.officerStore.MarkSeen(ctx, sc.Owner, v.ID, s.officerNow())
		if err != nil {
			s.logger.Printf("officers: record user %d's look at %s: %v", authz.MustFrom(ctx).UserID, v.ID, err)
		}
		seen = prev
	}
	b.WriteString(s.timelineHTML(ctx, sc, v, seen))

	// The instances.
	b.WriteString(s.officerInstances(ctx, sc, v))

	// Schedules.
	if builtinBackend {
		b.WriteString(s.officerSchedules(ctx, sc, v, writable))
	}

	// The raw queue: what What it did says in words, in the store's own terms
	// — behind a closed Technical details, never in the top layer.
	b.WriteString(`<details class="` + adminCard + `" id="officer-technical"><summary class="cursor-pointer text-base font-semibold text-fg">Technical details</summary>`)
	if builtinBackend {
		items, err := s.officerItems(ctx, sc.Owner, v.ID, 50)
		b.WriteString(`<section class="flex flex-col gap-3" id="officer-queue"><h2 class="text-sm font-semibold text-fg">Queue</h2>`)
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
		b.WriteString(`<section class="flex flex-col gap-3" id="officer-queue"><h2 class="text-sm font-semibold text-fg">Queue</h2><p class="text-sm text-fg2">Its queue is in its HQ.</p></section>`)
	}
	b.WriteString(`</details>`)

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
	s.writeNavDocument(w, r, code, name, ui.NavOfficers, false, `<div class="flex flex-col gap-4">`+b.String()+`</div>`, navWide)
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
			// 🔴 READ AS THE OFFICER'S OWNER (round 0's 🟡). The host spawn store
			// reads only the acting principal's own rows (ownRowSQL), and a team
			// officer's spawns are the TEAM's: read as the viewer, every
			// instance of a team host officer said "not started". scopeFor has
			// already admitted the viewer to sc (themselves, or a team they are
			// in), so reading as sc's owner shows them nothing they may not see.
			var err error
			if spawns, err = hs.LatestHostSpawns(s.officerPrincipal(ctx, sc.Owner), v.ID); err != nil {
				s.logger.Printf("officers: read %s's host spawns for its page: %v", v.ID, err)
			}
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
