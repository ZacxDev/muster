package api

import (
	"context"
	"net/http"
)

// ---------------------------------------------------------------------------
// "SHOW TMUX SESSION IN NOTIFICATIONS" — A PER-USER NOTIFICATION PREFERENCE
// (internal/notifypref, notify_preferences, migration 0094; docs/push.md).
//
// A notification names a mirrored session by its human name (the name rule,
// internal/sessionname). With this on, it adds where the session is — its tmux
// session, window index and host — so its owner knows which tmux window to
// jump to: "Billing migration · work:1 @ host-alpha". notifyName (names.go) is
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
	notifyTmuxLabel = "Show tmux session in notifications"
	notifyTmuxHelp  = "Adds the tmux session and host (like mango:1 @ atlas) after each session's name."
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

// accountNotifyTmuxForm is the Account page's switch, inside its Notifications
// section and drawn whether or not this server sends Web Push: the preference
// also names sessions on the Asks list and in the attention summary. "" with
// no store (no database) or no signed-in user.
func (s *Server) accountNotifyTmuxForm(r *http.Request) string {
	owner := requestOwner(r.Context())
	if s.ext.NotifyPrefs == nil || owner <= 0 {
		return ""
	}
	on, err := s.ext.NotifyPrefs.TmuxAddress(r.Context(), owner)
	if err != nil {
		s.logger.Printf("notify prefs: account: owner %d: %v", owner, err)
		return problemNode("The notification settings could not be read.")
	}
	saved := ""
	if r.URL.Query().Get("notify") == "saved" {
		saved = `<p role="status" class="text-sm font-semibold text-fg">Saved.</p>
`
	}
	state, v, button := "off", "1", "Turn on"
	if on {
		state, v, button = "on", "0", "Turn off"
	}
	return `<div id="notify-tmux" data-testid="notify-tmux" data-state="` + state + `" class="flex flex-col gap-2">
<p class="text-sm font-medium text-fg">` + notifyTmuxLabel + `: <span id="notify-tmux-state">` + map[bool]string{true: "On.", false: "Off."}[on] + `</span></p>
<p class="text-sm text-fg2">` + notifyTmuxHelp + `</p>
` + saved + `<form method="POST" action="` + accountNotifyTmuxPath + `"><input type="hidden" name="on" value="` + v + `">
<button type="submit" id="notify-tmux-toggle" class="press min-h-[44px] w-full rounded-md bg-bg px-3 py-2 text-base font-semibold text-fg ring-1 ring-inset ring-edge">` + button + `: ` + notifyTmuxLabel + `</button></form>
</div>
`
}

// handleAccountNotifyTmux is POST /account/notifications/tmux (on=1|0): the
// caller's own switch, back to the Account page.
func (s *Server) handleAccountNotifyTmux(w http.ResponseWriter, r *http.Request) {
	owner := requestOwner(r.Context())
	if owner <= 0 {
		http.Error(w, "no user", http.StatusForbidden)
		return
	}
	if s.ext.NotifyPrefs == nil {
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
	if err := s.ext.NotifyPrefs.SetTmuxAddress(r.Context(), owner, on); err != nil {
		s.logger.Printf("notify prefs: owner %d: save show-tmux-session: %v", owner, err)
		http.Error(w, "the setting could not be saved", http.StatusInternalServerError)
		return
	}
	// Open pages' Asks lists and badges re-read their names.
	s.attentionChanged(owner, "")
	http.Redirect(w, r, accountPath+"?notify=saved#notify-tmux", http.StatusSeeOther)
}
