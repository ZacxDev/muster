package api

import (
	"context"
	"net/http"
	"strings"
)

// ---------------------------------------------------------------------------
// "SHOW TMUX SESSION IN NOTIFICATIONS" — A PER-USER NOTIFICATION PREFERENCE
// (internal/notifypref, notify_preferences, migration 0094; docs/push.md).
//
// A notification names a mirrored session by its human name (the name rule,
// internal/sessionname). With this on, it adds where the session is, as muster
// named the window before sessions had names — the host's label for it, else
// its tmux address, and the host — so its owner knows which tmux window to
// jump to: "Billing migration · mango:1 @ atlas". notifyName (names.go) is
// the one place that adds it; this file holds the switch and its read.
//
// 🔴 OFF FOR EVERY USER UNTIL THEY TURN IT ON — no row, no database, no
// principal, or a failed read are all off — and off is byte for byte the text
// every notification had before the preference existed.
// ---------------------------------------------------------------------------

// accountNotifyTmuxPath is the switch's POST (on=1|0).
const accountNotifyTmuxPath = "/account/notifications/tmux"

// Its words on the Account page.
const (
	notifyTmuxLabel = "Show tmux sessions"
	notifyTmuxHelp  = "Adds each session's tmux codename (like mango:1 @ atlas) to notifications and the session lists."
)

// notifyTmuxOn reports whether owner turned on "Show tmux session in
// notifications". ctx must act as owner (or as the system): the store refuses
// any other read, which is logged and read as off.
func (s *Server) notifyTmuxOn(ctx context.Context, owner int64) bool {
	if owner <= 0 || s.ext.NotifyPrefs == nil {
		return false
	}
	on, err := s.ext.NotifyPrefs.TmuxAddress(ctx, owner)
	if err != nil {
		s.logger.Printf("notify prefs: owner %d: read show-tmux-session: %v (off)", owner, err)
		return false
	}
	return on
}

// notifyCodenameFirstOn reports whether owner turned on "Start notification
// titles with the tmux name" (accountNotifyCodenamePath). ctx must act as owner
// (or as the system): any other read is refused, logged and read as off — as
// are no store and no row. goNotify is its one caller.
func (s *Server) notifyCodenameFirstOn(ctx context.Context, owner int64) bool {
	if owner <= 0 || s.ext.NotifyPrefs == nil {
		return false
	}
	on, err := s.ext.NotifyPrefs.CodenameFirst(ctx, owner)
	if err != nil {
		s.logger.Printf("notify prefs: owner %d: read codename-first: %v (off)", owner, err)
		return false
	}
	return on
}

// accountNotifyCodenamePath is the second switch's POST (on=1|0): "Start
// notification titles with the tmux name" (migration 0117), independent of
// "Show tmux sessions" — on alone it changes push titles only, never the
// session lists or the Asks list.
const accountNotifyCodenamePath = "/account/notifications/codename"

// Its words on the Account page.
const (
	notifyCodenameLabel = "Start notification titles with the tmux name"
	notifyCodenameHelp  = "Puts each session's tmux name and host (like mango:1@laptop) at the front of its phone and browser notifications, so it shows even when the title is cut."
)

// notifySwitch is one of the Account page's notification switches: its DOM id
// (also its anchor and data-testid), words, POST path, the ?notify= value its
// save comes back with (which draws its "Saved."), and store halves.
type notifySwitch struct {
	id, label, help, path, saved string
	get                          func(context.Context, int64) (bool, error)
	set                          func(context.Context, int64, bool) error
}

// notifySwitches is the Account page's notification switches, in page order.
// nil with no store.
func (s *Server) notifySwitches() []notifySwitch {
	p := s.ext.NotifyPrefs
	if p == nil {
		return nil
	}
	return []notifySwitch{
		{id: "notify-tmux", label: notifyTmuxLabel, help: notifyTmuxHelp, path: accountNotifyTmuxPath, saved: "saved", get: p.TmuxAddress, set: p.SetTmuxAddress},
		{id: "notify-codename", label: notifyCodenameLabel, help: notifyCodenameHelp, path: accountNotifyCodenamePath, saved: "codename-saved", get: p.CodenameFirst, set: p.SetCodenameFirst},
	}
}

// notifySwitchFor is the switch POSTed to path.
func (s *Server) notifySwitchFor(path string) (notifySwitch, bool) {
	for _, sw := range s.notifySwitches() {
		if sw.path == path {
			return sw, true
		}
	}
	return notifySwitch{}, false
}

// accountNotifyTmuxForm is the Account page's notification switches, inside
// its Notifications section and drawn whether or not this server sends Web
// Push: "Show tmux sessions" also names sessions on the Asks list and in the
// attention summary. "" with no store (no database) or no signed-in user.
func (s *Server) accountNotifyTmuxForm(r *http.Request) string {
	owner := requestOwner(r.Context())
	if s.ext.NotifyPrefs == nil || owner <= 0 {
		return ""
	}
	var b strings.Builder
	for _, sw := range s.notifySwitches() {
		on, err := sw.get(r.Context(), owner)
		if err != nil {
			s.logger.Printf("notify prefs: account: %s: owner %d: %v", sw.id, owner, err)
			return problemNode("The notification settings could not be read.")
		}
		saved := ""
		if r.URL.Query().Get("notify") == sw.saved {
			saved = `<p role="status" class="text-sm font-semibold text-fg">Saved.</p>
`
		}
		b.WriteString(notifySwitchHTML(sw, on, saved))
	}
	return b.String()
}

// notifySwitchHTML is one switch: its state, words, and a one-button form
// that flips it.
func notifySwitchHTML(sw notifySwitch, on bool, saved string) string {
	state, v, button := "off", "1", "Turn on"
	if on {
		state, v, button = "on", "0", "Turn off"
	}
	return `<div id="` + sw.id + `" data-testid="` + sw.id + `" data-state="` + state + `" class="flex flex-col gap-2">
<p class="text-sm font-medium text-fg">` + sw.label + `: <span id="` + sw.id + `-state">` + map[bool]string{true: "On.", false: "Off."}[on] + `</span></p>
<p class="text-sm text-fg2">` + sw.help + `</p>
` + saved + `<form method="POST" action="` + sw.path + `"><input type="hidden" name="on" value="` + v + `">
<button type="submit" id="` + sw.id + `-toggle" class="press min-h-[44px] w-full rounded-md bg-bg px-3 py-2 text-base font-semibold text-fg ring-1 ring-inset ring-edge">` + button + `: ` + sw.label + `</button></form>
</div>
`
}

// handleAccountNotifySwitch is POST /account/notifications/tmux and
// /account/notifications/codename (on=1|0): the caller's own switch, back to
// the Account page at that switch.
func (s *Server) handleAccountNotifySwitch(w http.ResponseWriter, r *http.Request) {
	owner := requestOwner(r.Context())
	if owner <= 0 {
		http.Error(w, "no user", http.StatusForbidden)
		return
	}
	sw, ok := s.notifySwitchFor(r.URL.Path)
	if !ok {
		http.Error(w, "this server has no database to keep the setting in", http.StatusConflict)
		return
	}
	var on bool
	switch r.PostFormValue("on") {
	case "1":
		on = true
	case "0":
	default:
		http.Error(w, "on is 1 or 0", http.StatusBadRequest)
		return
	}
	if err := sw.set(r.Context(), owner, on); err != nil {
		s.logger.Printf("notify prefs: owner %d: save %s: %v", owner, sw.id, err)
		http.Error(w, "the setting could not be saved", http.StatusInternalServerError)
		return
	}
	// Open pages' Asks lists and badges re-read their names.
	s.attentionChanged(owner, "")
	http.Redirect(w, r, accountPath+"?notify="+sw.saved+"#"+sw.id, http.StatusSeeOther)
}
