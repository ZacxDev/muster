package api

import (
	"errors"
	"html"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/ui"
)

// ACCOUNT → APPS (docs/design/meetings.md, "The tab for every person", fork 4;
// core PR 1h.2): every app installed for the whole server, what each may do,
// whether it may act for you now, and Hide / Show.
//
//   - 🔴 HIDE removes the tab, makes the mint and the redeem 404, ENDS THE
//     GRANT AT ONCE (act_until = now, plugins.Store SetServerAppHidden) and
//     makes an open page's alive poll 404, so its frame goes.
//   - SHOW clears the hide only: the tab is back, but the app may not act for
//     you until you open its tab again (a fresh redeem).
//   - A same-origin form of a signed-in person (requireOperatorWrite), no
//     fresh sign-in: hiding removes a power, and showing grants none by itself.

const accountAppsPath = "/account/apps"

// AccountAppsTitle is the page's title.
const AccountAppsTitle = "Apps"

func (s *Server) registerAccountAppRoutes(mux Mux) {
	mux.HandleFunc("GET "+accountAppsPath, s.requirePluginOriginsOn(s.requireSession(s.handleAccountApps)))
	mux.HandleFunc("POST "+accountAppsPath+"/{id}/hide", s.requirePluginOriginsOn(s.requireOperatorWrite(s.handleAccountAppHide)))
	mux.HandleFunc("POST "+accountAppsPath+"/{id}/show", s.requirePluginOriginsOn(s.requireOperatorWrite(s.handleAccountAppShow)))
}

// AccountAppsNone is the page's line with no app installed.
const AccountAppsNone = "No apps are installed on this server."

// accountAppCan is what a server app may do, in a person's words.
func accountAppCan(p plugins.Plugin) string {
	can := "Can show its page in muster; can notify you"
	if p.Has(plugins.CapTasks) {
		can += "; can file tasks for you"
	}
	return can + "; it learns your display name when you open it."
}

// AccountAppsLinkedLine is a linked server app's added line on Account → Apps
// (docs/design/contacts.md, "What the admin does"): a person sees every pair
// that knows them as one person.
func AccountAppsLinkedLine(app, partner string) string {
	return app + " and " + partner + " know you as the same person."
}

func (s *Server) handleAccountApps(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	apps, err := s.ext.Plugins.AccountServerApps(r.Context(), s.ext.PluginOrigins, s.now())
	if err != nil {
		s.logger.Printf("account apps: list: %v", err)
		s.renderNotice(w, r, http.StatusInternalServerError, "Could not load the apps", "Try again in a moment.")
		return
	}
	var b strings.Builder
	b.WriteString(`<section class="` + adminCard + `"><h1 class="text-base font-semibold text-fg">` + AccountAppsTitle + `</h1>`)
	if r.URL.Query().Get("saved") == "1" {
		b.WriteString(`<p role="status" class="text-sm font-semibold text-fg">Saved.</p>`)
	}
	b.WriteString(`<p class="text-sm text-fg2">These apps were installed by an admin for everyone on this server. Hide one to remove its tab and stop it acting for you.</p>`)
	if len(apps) == 0 {
		b.WriteString(`<p class="text-sm text-fg2" id="account-apps-none">` + AccountAppsNone + `</p>`)
	} else {
		b.WriteString(`<ul class="flex flex-col gap-4" id="account-apps">`)
		for _, a := range apps {
			p := a.App
			id := strconv.FormatInt(p.ID, 10)
			label, icon := p.Name, ""
			if p.AppLabel != nil {
				label = *p.AppLabel
			}
			if p.AppIcon != nil {
				icon, _ = plugins.AppIconEmoji(*p.AppIcon)
			}
			state := "shown"
			if a.Hidden {
				state = "hidden"
			}
			b.WriteString(`<li class="flex flex-col gap-1" data-account-app="` + id + `" data-app-state="` + state + `">` +
				`<span class="text-sm font-semibold text-fg"><span aria-hidden="true">` + html.EscapeString(icon) + `</span> ` + html.EscapeString(label) + `</span>` +
				`<span class="text-sm text-fg2">` + html.EscapeString(accountAppCan(p)) + `</span>`)
			if a.LinkedWith != nil {
				b.WriteString(`<span class="text-sm text-fg2" data-app-linked="` + strconv.FormatInt(a.LinkedWith.ID, 10) + `">` +
					html.EscapeString(AccountAppsLinkedLine(label, a.LinkedWith.Label)) + `</span>`)
			}
			switch {
			case a.Hidden:
				b.WriteString(`<span class="text-sm text-fg2" data-app-grant="">Hidden: it has no tab and cannot act for you.</span>`)
			case a.LiveUntil != nil:
				b.WriteString(`<span class="text-sm text-fg" data-app-grant="` + a.LiveUntil.Format(time.RFC3339) + `">Can act for you until <time datetime="` +
					a.LiveUntil.Format(time.RFC3339) + `">` + a.LiveUntil.Format("15:04 UTC, 2 Jan") + `</time>.</span>`)
			default:
				b.WriteString(`<span class="text-sm text-fg2" data-app-grant="">Cannot act for you now; opening its tab lets it for 8 hours.</span>`)
			}
			action, button := "hide", "Hide"
			if a.Hidden {
				action, button = "show", "Show"
			}
			b.WriteString(`<form method="POST" action="` + accountAppsPath + `/` + id + `/` + action + `"><button type="submit" class="` + adminQuiet +
				`" data-app-` + action + `="` + id + `">` + button + `</button></form></li>`)
		}
		b.WriteString(`</ul>`)
	}
	b.WriteString(`</section>`)
	s.writeNavDocument(w, r, http.StatusOK, AccountAppsTitle, ui.NavAccount, false,
		`<div class="flex flex-col gap-4">`+b.String()+`<a href="/account" class="inline-flex min-h-[44px] items-center text-sm text-accent underline">Back to Account</a></div>`)
}

// handleAccountAppHide and handleAccountAppShow are POST
// /account/apps/{id}/hide and /show (named handlers, so the route walks see
// them).
func (s *Server) handleAccountAppHide(w http.ResponseWriter, r *http.Request) {
	s.setAccountAppHidden(w, r, true)
}

func (s *Server) handleAccountAppShow(w http.ResponseWriter, r *http.Request) {
	s.setAccountAppHidden(w, r, false)
}

// setAccountAppHidden is Hide (hidden) or Show for the principal.
func (s *Server) setAccountAppHidden(w http.ResponseWriter, r *http.Request, hidden bool) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if _, err := s.ext.Plugins.SetServerAppHidden(r.Context(), id, hidden, s.now()); err != nil {
		if errors.Is(err, plugins.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.logger.Printf("account apps: set app %d hidden=%v: %v", id, hidden, err)
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, accountAppsPath+"?saved=1", http.StatusSeeOther)
}
