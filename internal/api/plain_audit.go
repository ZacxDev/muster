package api

import (
	"html"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/ui"
	"github.com/ZacxDev/muster/internal/users"
)

// AN ACTIVITY LINE IN WORDS (the jargon audit's F105, F106): the admin's
// "Recent account activity" and a team's "Recent team activity" said
// "user.reset_link user:1 · by user 1 (session) · 2026-11-17 23:49 UTC" — an
// audit code, an object key, a user id, how they were signed in and a UTC
// timestamp. A line is now what happened, to whom, by whom and when, in
// words: "Reset link made · admin@example.test · by you · today".

// auditActionWords is what each audit action is called on an activity list.
// An action not in it is drawn by auditAction's fallback, never as its code.
var auditActionWords = map[string]string{
	"user.reset_link":           "Reset link made",
	"user.password_reset":       "Password reset",
	"user.disable":              "Account disabled",
	"user.enable":               "Account enabled",
	"user.role":                 "Role changed",
	"user.passkey_added":        "Passkey added",
	"user.passkey_removed":      "Passkey removed",
	"user.passkeys_removed":     "Passkeys removed",
	"user.ban_failed":           "Signing out a disabled account failed",
	"user.signout_failed":       "Signing out everywhere failed",
	"invite.create":             "Invite made",
	"invite.redeem":             "Invite used",
	"invite.revoke":             "Invite withdrawn",
	"team.create":               "Team created",
	"team.member.set":           "Team member added or changed",
	"team.member.remove":        "Team member removed",
	"team.host.token.create":    "Computer key made",
	"team.host.token.revoke":    "Computer key revoked",
	"team.host.replace":         "Computer replaced",
	"team.host.replace.refused": "Computer replacement refused",
	"team.plugin.create":        "Viewer plugin added",
	"team.plugin.revoke":        "Viewer plugin revoked",
	"team.plugin.handoff":       "Preview opened",
	"officer.create":            "Officer created",
	"officer.delete":            "Officer deleted",
	"officer.def.put":           "Officer changed",
	"officer.mode":              "Officer mode changed",
	"officer.replace":           "Officer replaced",
	"officer.run":               "Officer run",
	"officer.pause":             "Officer paused",
	"officer.resume":            "Officer resumed",
	"officer.schedule.put":      "Officer schedule saved",
	"officer.schedule.delete":   "Officer schedule deleted",
	"officer.spawn.share":       "Officer session shared",
	"officer.spawn.unshare":     "Officer session no longer shared",
	"officer.type.put":          "Officer type changed",
}

// auditAction is action in words: auditActionWords', else its code's words
// ("team.thing_done" → "Team thing done"), never the code itself.
func auditAction(action string) string {
	if w, ok := auditActionWords[action]; ok {
		return w
	}
	w := strings.TrimSpace(strings.NewReplacer(".", " ", "_", " ").Replace(action))
	if w == "" {
		return "Changed"
	}
	return strings.ToUpper(w[:1]) + w[1:]
}

// auditPeople names the people and teams an activity list mentions: a user
// by their email (or name), a team by its name.
type auditPeople struct {
	me    int64
	users map[int64]string
	teams map[int64]string
}

// who is user id as the line says it: "you", their email, else "someone".
func (p auditPeople) who(id int64) string {
	switch {
	case id > 0 && id == p.me:
		return "you"
	case p.users[id] != "":
		return p.users[id]
	case id <= 0:
		return "muster"
	}
	return "someone"
}

// object is what an audit row acted on, in words: a user's email ("you"),
// a team's name, "an invite" — "" for anything else.
func (p auditPeople) object(obj string) string {
	kind, raw, ok := strings.Cut(obj, ":")
	id, err := strconv.ParseInt(raw, 10, 64)
	if !ok || err != nil {
		return ""
	}
	switch kind {
	case "user":
		return p.who(id)
	case "team":
		if n := p.teams[id]; n != "" {
			return n
		}
		return "a team"
	case "invite":
		return "an invite"
	}
	return ""
}

// auditLineHTML is one activity row's inner HTML: what happened, to what, by
// whom and the day in words (ui.PlainWhenHTML, the reader's clock drawn by the
// app script).
func auditLineHTML(e users.AuditEntry, p auditPeople, now time.Time) string {
	parts := []string{`<span class="text-fg">` + html.EscapeString(auditAction(e.Action)) + `</span>`}
	if o := p.object(e.Object); o != "" {
		parts = append(parts, html.EscapeString(o))
	}
	parts = append(parts, "by "+html.EscapeString(p.who(e.UserID)), ui.PlainWhenHTML("", e.At, now))
	return strings.Join(parts, " · ")
}
