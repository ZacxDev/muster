package api

import (
	"context"
	"html"
	"net/http"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// /teams/{id}/plugins: A TEAM'S VIEWER PLUGINS (team sharing 2b.3b;
// docs/design/team-sharing.md F, "2b.3b"). A team session's Preview runs on
// the TEAM's viewer plugin, never a viewer's own (the operator's ruling). On
// this page the team's MANAGER mints one (its token shown once), and revokes
// one.
//
// 🔴 MANAGERS ONLY, AS THE TEAM — the hosts page's gate (teamManagerPage: the
// request's memberships, then a FRESH role read) and its step-up on every
// write, audited as the manager (team.plugin.create, team.plugin.revoke). The
// row is written AS THE TEAM with the `viewer` capability and NOTHING ELSE: no
// messages, no draft checks, no officer binding. Its token is served the
// hand-off redeem and /self alone (teamPluginRoutes), whatever its row says.
// ---------------------------------------------------------------------------

func (s *Server) registerTeamPluginPages(mux Mux) {
	mux.HandleFunc("GET /teams/{id}/plugins", s.requireSession(s.handleTeamPluginsPage))
	mux.HandleFunc("POST /teams/{id}/plugins", s.requireOperatorWrite(s.handleTeamPluginMint))
	mux.HandleFunc("POST /teams/{id}/plugins/{plugin}/revoke", s.requireOperatorWrite(s.handleTeamPluginRevoke))
}

// The page's answers, one spelling each.
const (
	TeamPluginsStepUp   = "Adding or revoking a team viewer plugin needs a recent sign-in. Confirm it's you, then try again."
	TeamPluginsOff      = "This server does not offer viewer plugins (plugins are off, or MUSTER_PLUGIN_ORIGINS is unset)."
	TeamPluginsNoPlugin = "That plugin does not exist, is not this team's, or is already revoked."
	TeamPluginsManager  = "Only a manager of the team manages its viewer plugins."
	teamPluginsAbout    = "A team viewer plugin opens a team session's previews for every member who can see the session, viewers included. Its token redeems preview hand-offs and nothing else. To serve previews, the preview gateway's admin adds this token to the gateway's PLUGIN_TOKENS_FILE and mints the team's host tokens there as team:<id>."
)

// teamPluginsWrite is the page's gate: teamManagerPage, with this page's own
// notice for a caller who is no longer a manager.
func (s *Server) teamPluginsWrite(w http.ResponseWriter, r *http.Request) (int64, context.Context, bool) {
	return s.teamManagerPage(w, r, "Team plugins", TeamPluginsManager)
}

func (s *Server) handleTeamPluginsPage(w http.ResponseWriter, r *http.Request) {
	team, tctx, ok := s.teamPluginsWrite(w, r)
	if !ok {
		return
	}
	s.renderTeamPlugins(w, r, team, tctx, http.StatusOK, "", "")
}

// teamPluginsOn reports whether viewer plugins are offered here at all.
func (s *Server) teamPluginsOn() bool { return s.ext.Plugins != nil && len(s.ext.PluginOrigins) > 0 }

// renderTeamPlugins draws the page. minted, when set, is a token just minted:
// shown ONCE, here.
func (s *Server) renderTeamPlugins(w http.ResponseWriter, r *http.Request, team int64, tctx context.Context, code int, problem, minted string) {
	id := strconv.FormatInt(team, 10)
	name := s.teamNames(r.Context(), []int64{team})[team]
	var b strings.Builder
	b.WriteString(problemNode(problem))
	b.WriteString(`<section class="` + adminCard + `"><h1 class="text-base font-semibold text-fg">Team viewer plugins · ` + html.EscapeString(name) + `</h1>
<p class="text-sm text-fg2">` + html.EscapeString(teamPluginsAbout) + `</p></section>`)
	if !s.teamPluginsOn() {
		b.WriteString(`<section class="` + adminCard + `" id="team-plugins-off"><p class="text-sm text-fg2">` + html.EscapeString(TeamPluginsOff) + `</p></section>`)
		s.writeNavDocument(w, r, code, "Team plugins", ui.NavAccount, true, `<div class="flex flex-col gap-4">`+b.String()+`</div>`)
		return
	}
	if minted != "" {
		b.WriteString(`<section class="` + adminCard + `" id="team-plugin-minted"><h2 class="text-base font-semibold text-fg">New team viewer plugin token</h2>
<p class="text-sm text-fg2">Shown once. Give it to the preview gateway's admin for its PLUGIN_TOKENS_FILE.</p>
<pre class="text-sm text-fg break-all whitespace-pre-wrap" id="team-plugin-token">` + html.EscapeString(minted) + `</pre></section>`)
	}
	list, err := s.ext.Plugins.List(tctx)
	if err != nil {
		s.logger.Printf("teams: list team %d's plugins: %v", team, err)
		s.renderNotice(w, r, http.StatusInternalServerError, "Could not load the team's plugins", "Try again in a moment.")
		return
	}
	b.WriteString(`<section class="` + adminCard + `"><h2 class="text-base font-semibold text-fg">Plugins</h2><ul class="flex flex-col gap-2" id="team-plugins">`)
	live := 0
	for _, p := range list {
		if p.RevokedAt != nil {
			continue
		}
		live++
		pid := strconv.FormatInt(p.ID, 10)
		entry := ""
		if p.EntryURL != nil {
			entry = *p.EntryURL
		}
		b.WriteString(`<li class="flex flex-wrap items-center gap-2" data-team-plugin="` + pid + `" data-rotate="` + strconv.FormatBool(p.RotateAt != nil) +
			`"><span class="text-sm text-fg">` + html.EscapeString(p.Name) +
			`</span><span class="text-sm text-fg2">plugin ` + pid + ` · ` + html.EscapeString(entry) + `</span>`)
		if p.RotateAt != nil {
			// The reason is the hosts page's: the audit row the mark's own
			// transaction wrote (teamRotateLine).
			b.WriteString(`<span class="text-sm font-medium text-fg" data-rotate-reason>` + html.EscapeString(s.teamRotateLine(r.Context(), team, *p.RotateAt)) + `</span>`)
		}
		b.WriteString(`` +
			`<form method="POST" action="/teams/` + id + `/plugins/` + pid + `/revoke"><button type="submit" class="` + adminQuiet + `">Revoke</button></form></li>`)
	}
	if live == 0 {
		b.WriteString(`<li class="text-sm text-fg2" id="team-plugins-none">This team has no viewer plugin, so its sessions' previews do not open.</li>`)
	}
	b.WriteString(`</ul></section>
<form method="POST" action="/teams/` + id + `/plugins" class="` + adminCard + `">
<h2 class="text-base font-semibold text-fg">Add a viewer plugin</h2>
<label for="team-plugin-name" class="text-sm font-medium text-fg">Name</label>
<input id="team-plugin-name" name="name" required autocomplete="off" class="` + adminInput + `">
<label for="team-plugin-origin" class="text-sm font-medium text-fg">Origin (one of this server's MUSTER_PLUGIN_ORIGINS)</label>
<input id="team-plugin-origin" name="origin" required autocomplete="off" class="` + adminInput + `">
<label for="team-plugin-entry" class="text-sm font-medium text-fg">Entry URL (under that origin)</label>
<input id="team-plugin-entry" name="entry_url" required autocomplete="off" class="` + adminInput + `">
<button type="submit" class="` + adminButton + `">Mint a token</button></form>`)
	s.writeNavDocument(w, r, code, "Team plugins", ui.NavAccount, true, `<div class="flex flex-col gap-4">`+b.String()+`</div>`)
}

// teamPluginsFresh is the step-up check of the page's writes: false has
// answered (403, with the step-up header).
func (s *Server) teamPluginsFresh(w http.ResponseWriter, r *http.Request, team int64, tctx context.Context) bool {
	if stepup.Fresh(r.Context(), s.stepUpNow()) {
		return true
	}
	if isDocumentNavigation(r) {
		w.Header().Set("X-Muster-Step-Up", "required")
		s.renderTeamPlugins(w, r, team, tctx, http.StatusForbidden, TeamPluginsStepUp, "")
		return false
	}
	refuseStale(w)
	return false
}

// handleTeamPluginMint is POST /teams/{id}/plugins (name, origin, entry_url):
// mint a team viewer plugin AS THE TEAM — capability viewer and nothing else
// — show its token once, and audit it as the manager.
func (s *Server) handleTeamPluginMint(w http.ResponseWriter, r *http.Request) {
	team, tctx, ok := s.teamPluginsWrite(w, r)
	if !ok || !s.teamPluginsFresh(w, r, team, tctx) {
		return
	}
	if !s.teamPluginsOn() {
		s.renderTeamPlugins(w, r, team, tctx, http.StatusNotImplemented, TeamPluginsOff, "")
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	origin, err := plugins.ValidViewerLink(s.ext.PluginOrigins, r.PostFormValue("origin"), r.PostFormValue("entry_url"))
	if err != nil {
		s.renderTeamPlugins(w, r, team, tctx, http.StatusBadRequest, strings.TrimPrefix(err.Error(), plugins.ErrInvalid.Error()+": "), "")
		return
	}
	tok, p, err := s.ext.Plugins.CreatePlugin(tctx, plugins.NewPlugin{Name: name, Label: name,
		Capabilities: []plugins.Capability{plugins.CapViewer}, Origin: origin, EntryURL: strings.TrimSpace(r.PostFormValue("entry_url"))})
	switch {
	case err == nil:
	case strings.HasPrefix(err.Error(), plugins.ErrInvalid.Error()):
		s.renderTeamPlugins(w, r, team, tctx, http.StatusBadRequest, strings.TrimPrefix(err.Error(), plugins.ErrInvalid.Error()+": "), "")
		return
	default:
		s.logger.Printf("teams: mint a viewer plugin for team %d: %v", team, err)
		s.renderTeamPlugins(w, r, team, tctx, http.StatusInternalServerError, "The plugin could not be minted. Try again.", "")
		return
	}
	s.auditTeamLevel(r.Context(), actorOf(r), team, "team.plugin.create", "plugin:"+strconv.FormatInt(p.ID, 10), map[string]any{"name": name, "origin": origin}, true)
	s.logger.Printf("teams: manager %d minted viewer plugin %d for team %d", actorOf(r).UserID, p.ID, team)
	s.renderTeamPlugins(w, r, team, tctx, http.StatusOK, "", tok)
}

// handleTeamPluginRevoke is POST /teams/{id}/plugins/{plugin}/revoke: revoke
// one of the team's plugins AS THE TEAM, and audit it as the manager.
func (s *Server) handleTeamPluginRevoke(w http.ResponseWriter, r *http.Request) {
	team, tctx, ok := s.teamPluginsWrite(w, r)
	if !ok || !s.teamPluginsFresh(w, r, team, tctx) {
		return
	}
	if !s.teamPluginsOn() {
		s.renderTeamPlugins(w, r, team, tctx, http.StatusNotImplemented, TeamPluginsOff, "")
		return
	}
	pid, err := strconv.ParseInt(r.PathValue("plugin"), 10, 64)
	found := false
	if err == nil && pid > 0 {
		// The team's own only: Revoke runs as the team, under the store's
		// one owner filter.
		found, err = s.ext.Plugins.Revoke(tctx, pid)
	}
	if err != nil {
		s.logger.Printf("teams: revoke plugin %d of team %d: %v", pid, team, err)
	}
	if !found {
		s.renderTeamPlugins(w, r, team, tctx, http.StatusNotFound, TeamPluginsNoPlugin, "")
		return
	}
	s.auditTeamLevel(r.Context(), actorOf(r), team, "team.plugin.revoke", "plugin:"+strconv.FormatInt(pid, 10), nil, true)
	http.Redirect(w, r, "/teams/"+strconv.FormatInt(team, 10)+"/plugins", http.StatusSeeOther)
}
