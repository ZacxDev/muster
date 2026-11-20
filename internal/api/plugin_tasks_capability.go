package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/plugins"
)

// THE `tasks` CAPABILITY'S OWNER SIDE (docs/plugins.md "Capabilities";
// docs/design/meetings.md decision 11): the create form's Files tasks box and a
// live plugin's Turn on / Turn off Files tasks button. The plugin side is one
// route, POST /plugin/v1/tasks, admitted for messages OR tasks
// (pluginCapFilesTasks in plugins.go).

// pluginTasksStore is the store's Files tasks toggle (plugins.PGStore
// SetTasks). Not on plugins.Store, so the many fakes of it need nothing: a
// store without it serves no toggle (404) and draws none.
type pluginTasksStore interface {
	SetTasks(ctx context.Context, id int64, on bool) (plugins.Plugin, bool, error)
}

func (s *Server) pluginTasks() pluginTasksStore {
	ts, _ := s.ext.Plugins.(pluginTasksStore)
	return ts
}

// handlePluginTasksToggle is POST /plugins/{id}/tasks {on: "1" turns it on,
// anything else off}: Files tasks on or off for one of the owner's LIVE
// plugins, keeping its token. Gated as draft-checks is — a signed-in session
// and a same-origin form (requireOperatorWrite, and admit's central origin
// check), no fresh sign-in: filing a task reaches no outside party and starts
// nothing (a plugin's task is not dispatched until its owner does, auto-start
// needs messages). Another owner's plugin, a revoked one or none is 404, as
// revoke's. Audited plugin.capability.changed (setCapabilityTx).
func (s *Server) handlePluginTasksToggle(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	ts := s.pluginTasks()
	if err != nil || id <= 0 || ts == nil {
		s.renderPluginsPage(w, r, http.StatusNotFound, "", "No such plugin.")
		return
	}
	on := r.PostFormValue("on") == "1"
	_, changed, err := ts.SetTasks(r.Context(), id, on)
	switch {
	case err == nil:
	case errors.Is(err, plugins.ErrNotFound):
		s.renderPluginsPage(w, r, http.StatusNotFound, "", "No such plugin.")
		return
	case errors.Is(err, plugins.ErrInvalid):
		s.renderPluginsPage(w, r, http.StatusBadRequest, "", strings.TrimPrefix(err.Error(), plugins.ErrInvalid.Error()+": "))
		return
	default:
		s.logger.Printf("plugins: tasks %v for plugin %d: %v", on, id, err)
		s.renderPluginsPage(w, r, http.StatusInternalServerError, "", "Files tasks could not be changed.")
		return
	}
	if changed {
		s.logger.Printf("plugins: plugin %d: tasks turned %v by its owner", id, on)
	}
	http.Redirect(w, r, pluginsPath, http.StatusSeeOther)
}

// pluginTasksToggleHTML is a live plugin's Files tasks control on its
// /plugins card: one form posting the other state. Drawn only for a plugin
// WITHOUT messages (messages already files tasks, so the toggle would change
// nothing), and not as a Turn off for a plugin whose ONLY capability is tasks:
// a token needs one capability (the mint's rule), so it is revoked instead.
func (s *Server) pluginTasksToggleHTML(p plugins.Plugin) string {
	if s.pluginTasks() == nil || p.Has(plugins.CapMessages) || (len(p.Capabilities) == 1 && p.Has(plugins.CapTasks)) {
		return ""
	}
	on, label := "1", "Turn on Files tasks"
	if p.Has(plugins.CapTasks) {
		on, label = "0", "Turn off Files tasks"
	}
	return `<form method="POST" action="` + pluginsPath + `/` + strconv.FormatInt(p.ID, 10) + `/tasks" data-plugin-tasks>
<input type="hidden" name="on" value="` + on + `"><button type="submit" class="` + flowsQuietButton + `">` + label + `</button></form>`
}

// pluginTasksField is the create form's tasks choice (plugins.CapTasks),
// drawn whatever MUSTER_PLUGIN_ORIGINS says. Ticked, the token files tasks;
// ticked with no other capability chosen it files tasks ONLY — the box takes
// the place of the form's default, messages (pluginCreateInput). Unticked, the
// token is as before.
const pluginTasksField = `<label class="flex min-h-[44px] items-center gap-2 text-sm text-fg" data-testid="plugin-tasks"><input type="checkbox" name="tasks" value="1"> Files tasks: the plugin may file tasks for you and comment on them; with nothing else chosen, that is all it may do</label>
`
