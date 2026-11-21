package api

import (
	"context"
	"errors"
	"html"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/users"
)

// /admin/apps: SERVER-INSTALLED APP PLUGINS (docs/design/meetings.md, "Who
// installs, and where"; core PR 1h.2; the store and the token's door are
// internal/plugins server.go and plugin_server.go).
//
//   - An ADMIN's page (requireAdminRole: it also serves a password-mode
//     server, whose only user is its admin), drawn only while plugins are on
//     and MUSTER_PLUGIN_ORIGINS is set (requirePluginOriginsOn, before any
//     credential is looked at).
//   - 🔴 INSTALL AND ROTATE NEED A FRESH SIGN-IN (fork 5): an install widens
//     every person's /apps page policy and mints a credential that reaches
//     every person who opens the tab; a rotate mints that credential again.
//     Their forms confirm in place (data-step-up-form, stepUpFormsHTML); the
//     handler refuses a stale sign-in whatever the page did.
//   - UNINSTALL needs a signed-in admin and a same-origin form, no fresh
//     sign-in: removing a power is safe. It goes through uninstallServerApp,
//     which withdraws every knock still out.
//   - The page lists each live app with what it may do, who installed it and
//     when, when it was last seen, and how many people hold a LIVE grant (a
//     count, never names), counted by THE gate (plugins.Store ServerApps).

const (
	adminAppsPath = "/admin/apps"

	// AdminAppsTitle is the page's title.
	AdminAppsTitle = "Apps"
)

func (s *Server) registerAdminAppRoutes(mux Mux) {
	mux.HandleFunc("GET "+adminAppsPath, s.requirePluginOriginsOn(s.requireAdminRole(s.handleAdminApps)))
	mux.HandleFunc("POST "+adminAppsPath, s.requirePluginOriginsOn(s.requireAdminRole(s.handleAdminAppInstall)))
	mux.HandleFunc("POST "+adminAppsPath+"/{id}/rotate", s.requirePluginOriginsOn(s.requireAdminRole(s.handleAdminAppRotate)))
	mux.HandleFunc("POST "+adminAppsPath+"/{id}/uninstall", s.requirePluginOriginsOn(s.requireAdminRole(s.handleAdminAppUninstall)))
}

// AdminAppsInstallWhy and AdminAppsRotateWhy are the confirm dialog's
// sentences for the page's two gated forms.
func AdminAppsInstallWhy() string {
	return "Installing an app for everyone on this server needs a sign-in from the last " + stepup.WindowText() + "."
}

func AdminAppsRotateWhy() string {
	return "Making a new token for an app needs a sign-in from the last " + stepup.WindowText() + "."
}

// AdminAppsRotateWarning is what the rotate form says before it is used.
const AdminAppsRotateWarning = "Rotate ends every call the app is in until the app is restarted with the new token."

// AdminAppsUninstallWarning is what the uninstall form says before it is used.
const AdminAppsUninstallWarning = "Uninstall removes the tab for everyone at once, and people's history in the app stops being reachable from muster."

// adminActor is the admin page's audit actor: the signed-in admin.
func adminActor(r *http.Request) users.Actor {
	p := authz.MustFrom(r.Context())
	return users.Actor{UserID: p.UserID, Via: p.Via}
}

func (s *Server) handleAdminApps(w http.ResponseWriter, r *http.Request) {
	s.renderAdminApps(w, r, http.StatusOK, "", "", "")
}

// renderAdminApps draws the page with, at most, a problem, a token shown once
// (and which app it is for).
func (s *Server) renderAdminApps(w http.ResponseWriter, r *http.Request, code int, problem, token, tokenFor string) {
	w.Header().Set("Cache-Control", "no-store")
	apps, err := s.ext.Plugins.ServerApps(r.Context(), s.ext.PluginOrigins, s.now())
	if err != nil {
		s.logger.Printf("admin apps: list: %v", err)
		s.renderNotice(w, r, http.StatusInternalServerError, "Could not load the apps", "Try again in a moment.")
		return
	}
	var b strings.Builder
	b.WriteString(problemNode(problem))
	if token != "" {
		b.WriteString(`<section class="` + adminCard + `" aria-labelledby="once-h"><h2 id="once-h" class="text-base font-semibold text-fg">The token for ` +
			html.EscapeString(tokenFor) + `</h2><p class="text-sm text-fg2">Give it to the app as its token. It is shown only this once.</p>` +
			`<input id="minted-server-app-token" readonly value="` + html.EscapeString(token) + `" class="` + adminInput + ` w-full font-mono text-sm" onclick="this.select()"></section>`)
	}
	b.WriteString(`<section class="` + adminCard + `"><h1 class="text-base font-semibold text-fg">Apps for everyone</h1>` +
		`<p class="text-sm text-fg2">An app installed here gets a tab for every person on this server. It learns a person only when they open its tab, and can then notify them and, if it may, file tasks for them for 8 hours. Each person can hide it on Account → Apps.</p>`)
	if len(apps) == 0 {
		b.WriteString(`<p class="text-sm text-fg2" id="admin-apps-none">No apps are installed.</p>`)
	} else {
		b.WriteString(`<ul class="flex flex-col gap-4" id="admin-apps">`)
		for _, a := range apps {
			b.WriteString(s.adminAppItem(r.Context(), a))
		}
		b.WriteString(`</ul>`)
	}
	b.WriteString(`</section>`)
	if len(apps) < plugins.MaxServerApps {
		b.WriteString(s.adminAppInstallForm())
	} else {
		b.WriteString(`<p class="text-sm text-fg2" id="admin-apps-full">This server has ` + strconv.Itoa(plugins.MaxServerApps) + ` apps, the most it may have. Uninstall one to add another.</p>`)
	}
	b.WriteString(s.stepUpFormsHTML(r, AdminAppsInstallWhy()))
	s.writeAdminDocument(w, r, code, AdminAppsTitle, adminAppsPath, b.String())
}

// adminAppItem is one installed app's entry.
func (s *Server) adminAppItem(ctx context.Context, a plugins.ServerAppInfo) string {
	p := a.App
	id := strconv.FormatInt(p.ID, 10)
	label, icon, origin, entry := "", "", "", ""
	if p.AppLabel != nil {
		label = *p.AppLabel
	}
	if p.AppIcon != nil {
		icon, _ = plugins.AppIconEmoji(*p.AppIcon)
	}
	if p.Origin != nil {
		origin = *p.Origin
	}
	if p.EntryURL != nil {
		entry = *p.EntryURL
	}
	can := "Shows its page in muster; notifies the person"
	if p.Has(plugins.CapTasks) {
		can += "; files tasks for the person"
	}
	by := "the command line"
	if a.InstalledVia == "page" && a.InstalledBy != nil {
		by = "user " + strconv.FormatInt(*a.InstalledBy, 10)
		if s.gt != nil && s.gt.Users != nil {
			if u, err := s.gt.Users.ByID(ctx, *a.InstalledBy); err == nil && u.Email != "" {
				by = u.Email
			}
		}
	}
	seen := "never"
	if p.LastSeenAt != nil {
		seen = p.LastSeenAt.UTC().Format(time.RFC3339)
	}
	var b strings.Builder
	b.WriteString(`<li class="flex flex-col gap-1" data-server-app="` + id + `">`)
	b.WriteString(`<span class="text-sm font-semibold text-fg"><span aria-hidden="true">` + html.EscapeString(icon) + `</span> ` + html.EscapeString(label) +
		` <span class="font-normal text-fg2">(` + html.EscapeString(p.Name) + `)</span></span>`)
	b.WriteString(`<span class="text-sm text-fg2 break-all">` + html.EscapeString(entry) + ` · origin ` + html.EscapeString(origin) + `</span>`)
	b.WriteString(`<span class="text-sm text-fg2">` + html.EscapeString(can) + `.</span>`)
	b.WriteString(`<span class="text-sm text-fg2">Installed by ` + html.EscapeString(by) + ` on ` + p.CreatedAt.UTC().Format(time.RFC3339) +
		`; last seen ` + html.EscapeString(seen) + `.</span>`)
	b.WriteString(`<span class="text-sm text-fg" data-live-grants="` + strconv.Itoa(a.LiveGrants) + `">` + liveGrantsLine(a.LiveGrants) + `</span>`)
	b.WriteString(`<div class="flex flex-wrap gap-2">`)
	b.WriteString(`<form method="POST" action="` + adminAppsPath + `/` + id + `/rotate" data-step-up-form data-step-up-why="` +
		html.EscapeString(AdminAppsRotateWhy()) + `"><p class="text-sm text-fg2">` + AdminAppsRotateWarning + `</p><button type="submit" class="` + adminQuiet +
		`" data-rotate="` + id + `">Rotate token</button></form>`)
	b.WriteString(`<form method="POST" action="` + adminAppsPath + `/` + id + `/uninstall"><p class="text-sm text-fg2">` + html.EscapeString(AdminAppsUninstallWarning) +
		`</p><button type="submit" class="` + adminQuiet + `" data-uninstall="` + id + `">Uninstall</button></form>`)
	b.WriteString(`</div></li>`)
	return b.String()
}

// liveGrantsLine says how many people the app may act for now.
func liveGrantsLine(n int) string {
	switch n {
	case 0:
		return "No one has it open now."
	case 1:
		return "1 person has it open now."
	}
	return strconv.Itoa(n) + " people have it open now."
}

// adminAppInstallForm is the install form: a name, a tab label and icon, an
// origin from MUSTER_PLUGIN_ORIGINS and an entry URL under it, and Files
// tasks. No other capability is offered: a server app has no person's chief.
func (s *Server) adminAppInstallForm() string {
	var b strings.Builder
	b.WriteString(`<form method="POST" action="` + adminAppsPath + `" class="` + adminCard + `" data-step-up-form data-step-up-why="` +
		html.EscapeString(AdminAppsInstallWhy()) + `"><h2 class="text-base font-semibold text-fg">Install an app</h2>`)
	field := func(id, label, attrs string) {
		b.WriteString(`<label for="` + id + `" class="text-sm font-medium text-fg">` + label + `</label><input id="` + id + `" name="` + id +
			`" class="` + adminInput + `" ` + attrs + `>`)
	}
	field("name", "Name", `required autocomplete="off" pattern="[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?" title="`+html.EscapeString(plugins.NameRule)+`"`)
	field("app_label", "Tab label", `required maxlength="24" autocomplete="off"`)
	b.WriteString(`<label for="app_icon" class="text-sm font-medium text-fg">Tab icon</label><select id="app_icon" name="app_icon" class="` + adminInput + `">`)
	for _, i := range plugins.AppIcons {
		b.WriteString(`<option value="` + i.Key + `">` + i.Emoji + ` ` + html.EscapeString(i.Name) + `</option>`)
	}
	b.WriteString(`</select><label for="origin" class="text-sm font-medium text-fg">Origin</label><select id="origin" name="origin" class="` + adminInput + `">`)
	for _, o := range s.ext.PluginOrigins {
		b.WriteString(`<option value="` + html.EscapeString(o.String()) + `">` + html.EscapeString(o.String()) + `</option>`)
	}
	b.WriteString(`</select>`)
	field("entry_url", "Entry URL", `required type="url" autocomplete="off" placeholder="https://app.example.test/enter"`)
	b.WriteString(`<label class="flex items-center gap-2 text-sm text-fg"><input type="checkbox" name="tasks" value="1" id="tasks"> Files tasks</label>` +
		`<button type="submit" class="` + adminButton + `" id="admin-app-install">Install</button></form>`)
	return b.String()
}

// adminAppStale answers a gated form whose sign-in is not fresh: the page's
// own 403 with the step-up header (the dialog asks, then resubmits).
func (s *Server) adminAppStale(w http.ResponseWriter, r *http.Request, what string) bool {
	if stepup.Fresh(r.Context(), s.stepUpNow()) {
		return false
	}
	w.Header().Set("X-Muster-Step-Up", "required")
	s.renderAdminApps(w, r, http.StatusForbidden, what+" needs a recent sign-in. Confirm it's you, then try again.", "", "")
	return true
}

func (s *Server) handleAdminAppInstall(w http.ResponseWriter, r *http.Request) {
	if s.adminAppStale(w, r, "Installing an app") {
		return
	}
	in := plugins.ServerAppIn{
		Name:     strings.TrimSpace(r.PostFormValue("name")),
		AppLabel: strings.TrimSpace(r.PostFormValue("app_label")),
		AppIcon:  r.PostFormValue("app_icon"),
		Origin:   strings.TrimSpace(r.PostFormValue("origin")),
		EntryURL: strings.TrimSpace(r.PostFormValue("entry_url")),
		Origins:  s.ext.PluginOrigins,
		Tasks:    r.PostFormValue("tasks") == "1",
	}
	tok, p, err := s.ext.Plugins.InstallServerApp(r.Context(), in, adminActor(r))
	if err != nil {
		if errors.Is(err, plugins.ErrInvalid) || errors.Is(err, plugins.ErrViewerOriginRetired) || strings.Contains(err.Error(), "origin") {
			s.renderAdminApps(w, r, http.StatusBadRequest, strings.TrimPrefix(err.Error(), plugins.ErrInvalid.Error()+": "), "", "")
			return
		}
		s.logger.Printf("admin apps: install: %v", err)
		s.renderAdminApps(w, r, http.StatusInternalServerError, "The app could not be installed.", "", "")
		return
	}
	s.logger.Printf("admin apps: user %d installed server app %d (%s)", adminActor(r).UserID, p.ID, p.Name)
	s.renderAdminApps(w, r, http.StatusOK, "", tok, in.AppLabel)
}

func (s *Server) handleAdminAppRotate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if s.adminAppStale(w, r, "Making a new token") {
		return
	}
	tok, p, err := s.ext.Plugins.RotateServerApp(r.Context(), id, adminActor(r))
	switch {
	case err == nil:
	case errors.Is(err, plugins.ErrNotFound):
		s.renderAdminApps(w, r, http.StatusNotFound, "There is no such app.", "", "")
		return
	default:
		s.logger.Printf("admin apps: rotate %d: %v", id, err)
		s.renderAdminApps(w, r, http.StatusInternalServerError, "The token could not be made.", "", "")
		return
	}
	label := p.Name
	if p.AppLabel != nil {
		label = *p.AppLabel
	}
	s.renderAdminApps(w, r, http.StatusOK, "", tok, label)
}

func (s *Server) handleAdminAppUninstall(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if _, err := s.uninstallServerApp(r, id, adminActor(r)); err != nil {
		if errors.Is(err, plugins.ErrNotFound) {
			s.renderAdminApps(w, r, http.StatusNotFound, "There is no such app.", "", "")
			return
		}
		s.logger.Printf("admin apps: uninstall %d: %v", id, err)
		s.renderAdminApps(w, r, http.StatusInternalServerError, "The app could not be uninstalled.", "", "")
		return
	}
	http.Redirect(w, r, adminAppsPath, http.StatusSeeOther)
}

// adminAppsServed reports whether /admin/apps is drawn: plugins on and
// MUSTER_PLUGIN_ORIGINS set (the admin tab bar links it only then).
func (s *Server) adminAppsServed() bool { return s.ext.Plugins != nil && len(s.ext.PluginOrigins) > 0 }

