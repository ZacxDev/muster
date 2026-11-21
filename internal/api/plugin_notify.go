package api

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE KNOCK NOTIFICATION (docs/design/meetings.md, "Core seam: knock
// notification"; decision 10; core PR 1e). An `app` plugin asks muster to push
// its owner one of a FIXED SET of notifications whose text muster writes. v1
// has one kind, `knock`: someone is waiting to join the owner's meeting.
//
//	POST   /plugin/v1/notify        {"kind": "knock", "ref": "<id>", "count": N}
//	DELETE /plugin/v1/notify/{ref}  withdraws it
//
// 🔴 THE TEXT IS MUSTER'S, NEVER THE PLUGIN'S. The title is the tab label the
// OWNER chose at mint (plugins.app_label), the body one of two fixed sentences
// (appKnockBody). The request has exactly three fields and any other is a 400
// (DisallowUnknownFields), so a guest's name, a title or a body the plugin
// sends cannot reach a payload: a guest's name is text a stranger typed, and
// muster's plugin pushes never quote what outsiders wrote (docs/push.md, the
// `plugin` event's rule).
//
// 🔴 THE ORDER OF THE ANSWERS: the door (401, and 403 without `app`), the body
// (400), push configured (503), the owner's toggle and devices (204, nothing
// sent: not an error, the plugin keeps its lobby row), the caps (429 with
// Retry-After), then the queue (202). Every answer that sends nothing after
// the body is valid is logged as a dropped push (logPushDrop).
//
// 🔴 KNOCKS HAVE THEIR OWN OWNER BUDGET (appOwnerBurst, push_routes.go): a 202
// knock is still subject to it in goNotify, and dropped (logged) past it, but
// it never spends the cap every other event shares. The 503 and 204 answers
// read muster-native push alone; for an admin owner goNotify still hands the
// knock to the router as it does every notification, but no answer depends
// on that (docs/plugins.md).
//
// 🔴 ONE TAG PER (plugin, ref): app-<plugin id>-<ref>, so a later count
// replaces the earlier one on the device and the withdrawal finds it. The tap
// opens /apps/<plugin id>?ref=<ref>, which mints at tap time like any visit.
//
// 🔴 THE RECIPIENT IS THE REQUEST PRINCIPAL, AND EVERY CAP AND THE "WHICH KNOCKS
// ARE OUT" LEDGER ARE KEYED BY IT (core PR 1h). For a personal token the
// principal is the token's owner (unchanged). For a SERVER app it is the
// person whose live grant the Muster-Act-For subject names (admitServerPlugin)
// — never the row's owner (it has none) and never installed_by. So a server
// token acting for B cannot withdraw A's knock by naming A's ref, and A's
// knocks never spend B's caps.

const (
	pluginNotifyPath = pluginAPIPrefix + "/notify"

	// appNotifyKindKnock is v1's one kind.
	appNotifyKindKnock = "knock"

	// The caps (design: "at most one send per (plugin, ref) per 30 s and 30
	// per plugin per hour").
	appNotifyRefWindow    = 30 * time.Second
	appNotifyPluginBurst  = 30
	appNotifyPluginWindow = time.Hour
	// appNotifyServerCeiling is a SERVER app's knocks per appNotifyPluginWindow
	// across everyone it acts for (core PR 1h): each person's own
	// appNotifyPluginBurst is per (app, person), under this per-app ceiling.
	appNotifyServerCeiling = 600

	// appNotifyMaxCount is the largest count a knock may carry.
	appNotifyMaxCount = 99
	// maxAppNotifyBody bounds the request body (three short fields).
	maxAppNotifyBody = 1 << 10

	// appNotifyType is payload v2's (and the browser payload's) type.
	appNotifyType = push.EventApp
)

func (s *Server) registerPluginNotifyRoutes(mux Mux) {
	mux.HandleFunc("POST "+pluginNotifyPath, s.requirePluginToken(s.handlePluginNotify, plugins.CapApp))
	mux.HandleFunc("DELETE "+pluginNotifyPath+"/{ref}", s.requirePluginToken(s.handlePluginNotifyWithdraw, plugins.CapApp))
}

// appKnockBody is a knock's body, muster's own words: count is 1-99.
func appKnockBody(count int) string {
	if count == 1 {
		return "Someone is waiting to join your meeting"
	}
	return strconv.Itoa(count) + " people are waiting to join your meeting"
}

// appNotifyTag is the tag of plugin id's notification about ref.
func appNotifyTag(plugin int64, ref string) string {
	return "app-" + strconv.FormatInt(plugin, 10) + "-" + ref
}

// appNotifyURL is where a tap on it opens: the app's page with the ref hint.
func appNotifyURL(plugin int64, ref string) string {
	return ui.AppPagePath(plugin) + "?ref=" + url.QueryEscape(ref)
}

// appNotifyRequest is the whole body a plugin may send.
type appNotifyRequest struct {
	Kind  string `json:"kind"`
	Ref   string `json:"ref"`
	Count int    `json:"count"`
}

// readAppNotify decodes and checks a POST /plugin/v1/notify body, or says why
// it is refused.
func readAppNotify(r *http.Request) (appNotifyRequest, string) {
	var in appNotifyRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, maxAppNotifyBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, `the body is {"kind": "knock", "ref": "<id>", "count": 1-99} and nothing else`
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return in, "the body is one JSON object"
	}
	switch {
	case in.Kind != appNotifyKindKnock:
		return in, `kind must be "knock"`
	case !plugins.ValidAppRef(in.Ref):
		return in, "ref must be 1-64 of A-Z a-z 0-9 _ -"
	case in.Count < 1 || in.Count > appNotifyMaxCount:
		return in, "count must be 1-" + strconv.Itoa(appNotifyMaxCount)
	}
	return in, ""
}

// handlePluginNotify is POST /plugin/v1/notify (the file header).
func (s *Server) handlePluginNotify(w http.ResponseWriter, r *http.Request) {
	p := pluginFromContext(r.Context())
	in, refused := readAppNotify(r)
	if refused != "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": refused})
		return
	}
	if !s.pushServed() {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": pushUnconfigured})
		return
	}
	if p.AppLabel == nil || *p.AppLabel == "" {
		// The capability without its tab (no row 0168 allows): nothing to title it by.
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": "this plugin has no app tab to notify from"})
		return
	}
	owner, tag := notifyRecipient(r), appNotifyTag(p.ID, in.Ref)
	// 🔴 A SERVER APP LEARNS NOTHING ABOUT A PERSON'S PUSH SETUP (audit round
	// 1): its caps are spent FIRST, and a person who muted app pushes or has
	// no device is answered exactly as one who has both — 202, dropped and
	// logged. A personal app plugin is its owner's own and keeps the 204.
	server := p.IsServer()
	if server && !s.allowKnock(w, p, owner, in.Ref, tag) {
		return
	}
	queued := func() { s.writeJSON(w, http.StatusAccepted, map[string]any{"queued": true, "tag": tag}) }
	if s.pushMuted(r.Context(), owner, push.EventApp) {
		s.logPushDrop(tag, push.EventApp, owner, pushDropMuted)
		if server {
			queued()
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	devs, err := s.ext.Push.Store().Devices(r.Context())
	if err != nil {
		s.logger.Printf("push: list devices for owner=%d (app notify): %v", owner, err)
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "the owner's devices could not be read"})
		return
	}
	if len(devs) == 0 {
		s.logPushDrop(tag, push.EventApp, owner, pushDropNoDevice)
		if server {
			queued()
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !server && !s.allowKnock(w, p, owner, in.Ref, tag) {
		return
	}
	s.goNotify(owner, push.EventApp, RouterNotification{
		Type: appNotifyType, ID: tag, Tag: tag,
		// The label is the owner's own words, already held to letters, digits,
		// spaces and - . & ' (plugins.ValidAppLabel): kept VERBATIM, since the
		// title rule would read "1. Calls" as a list item and send "Calls".
		Title: *p.AppLabel, TitleKeep: *p.AppLabel, Body: appKnockBody(in.Count),
		Data: map[string]string{"url": appNotifyURL(p.ID, in.Ref)},
	}, nil)
	s.writeJSON(w, http.StatusAccepted, map[string]any{"queued": true, "tag": tag})
}

// handlePluginNotifyWithdraw is DELETE /plugin/v1/notify/{ref}: the existing
// withdrawal (a `resolved` under the notification's tag: the browser closes
// it, a paired phone gets payload v2's silent `resolved`).
//
// 🔴 IT SENDS ONLY FOR A KNOCK THAT WENT OUT (audit round 1 of #924). A
// resolved shows nothing but still reaches every device, so a DELETE for a
// ref this plugin never knocked about — or already withdrew, or last knocked
// more than appNotifyPluginWindow ago — answers 204 and sends nothing. Those
// ghost calls are capped (appWithdrawGhostBurst per plugin per window: 429
// with Retry-After); a withdrawal of a live knock is never refused, since
// refusing it would strand the very notification it clears, and each knock
// allows at most one.
func (s *Server) handlePluginNotifyWithdraw(w http.ResponseWriter, r *http.Request) {
	p := pluginFromContext(r.Context())
	ref := r.PathValue("ref")
	if !plugins.ValidAppRef(ref) {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "ref must be 1-64 of A-Z a-z 0-9 _ -"})
		return
	}
	if !s.pushServed() {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": pushUnconfigured})
		return
	}
	owner, tag := notifyRecipient(r), appNotifyTag(p.ID, ref)
	live, wait, ok := s.appNotifies.withdraw(p.ID, owner, ref, s.now())
	switch {
	case !ok:
		s.logger.Printf("plugin notify: refused a withdrawal tag=%s owner=%d: the plugin's cap of %d withdrawals of nothing per %s is reached",
			tag, owner, appWithdrawGhostBurst, appNotifyPluginWindow)
		w.Header().Set("Retry-After", strconv.Itoa(wait))
		s.writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "too many withdrawals of notifications that are not out", "retry_after": wait})
		return
	case !live:
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.goNotify(owner, push.EventResolved, RouterNotification{Type: push.TypeResolved, ID: tag, Tag: tag}, nil)
	s.writeJSON(w, http.StatusAccepted, map[string]any{"queued": true, "tag": tag})
}

// allowKnock spends the knock caps for (p, owner, ref), or answers the 429
// with Retry-After (logged as a dropped push) and reports false.
func (s *Server) allowKnock(w http.ResponseWriter, p plugins.Plugin, owner int64, ref, tag string) bool {
	wait, ok := s.appNotifies.allow(p.ID, owner, ref, p.IsServer(), s.now())
	if ok {
		return true
	}
	s.logPushDrop(tag, push.EventApp, owner, pushDropAppCap)
	w.Header().Set("Retry-After", strconv.Itoa(wait))
	s.writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": pushDropSentence(pushDropAppCap), "retry_after": wait})
	return false
}

// notifyRecipient is whom a knock or its withdrawal is for: THE REQUEST
// PRINCIPAL's user — a personal token's owner, or the person a server app's
// act-for grant names. The one spelling, for both handlers.
func notifyRecipient(r *http.Request) int64 { return authz.MustFrom(r.Context()).UserID }

// appWithdrawGhostBurst is how many withdrawals of nothing (no live knock) one
// plugin may make per appNotifyPluginWindow — per (plugin, person).
const appWithdrawGhostBurst = 60

// appNotifyLimit is the knock caps: at most one send per (plugin, person,
// ref) per appNotifyRefWindow, appNotifyPluginBurst per (plugin, person) in
// any appNotifyPluginWindow (a sliding window: the sends' own times), and for
// a server app appNotifyServerCeiling per plugin across everyone. It also
// remembers which knocks are OUT (sent and not withdrawn, for at most
// appNotifyPluginWindow after the last send), and counts each (plugin,
// person)'s withdrawals of nothing. For a personal plugin the person is
// always its owner, so each per-(plugin, person) cap is the per-plugin cap it
// always was.
type appNotifyLimit struct {
	mu      sync.Mutex
	refs    map[appNotifyKey]time.Time   // the last send per (plugin, person, ref)
	sends   map[appNotifyWho][]time.Time // each (plugin, person)'s sends in the last window, oldest first
	ceiling map[int64][]time.Time        // each server app's sends in the last window, oldest first
	live    map[appNotifyKey]time.Time   // knocks out: the last send of each, until withdrawn
	ghosts  map[appNotifyWho][]time.Time // each (plugin, person)'s withdrawals of nothing in the last window
}

// appNotifyWho is one plugin acting for one person.
type appNotifyWho struct {
	plugin, owner int64
}

// appNotifyKey is one knock: a plugin, the person it is for, and the ref.
type appNotifyKey struct {
	plugin, owner int64
	ref           string
}

// prune forgets what has rolled out of every window, so the maps hold only
// live plugins and refs. l.mu is held.
func (l *appNotifyLimit) prune(now time.Time) {
	if l.refs == nil {
		l.refs, l.sends, l.ceiling = map[appNotifyKey]time.Time{}, map[appNotifyWho][]time.Time{}, map[int64][]time.Time{}
		l.live, l.ghosts = map[appNotifyKey]time.Time{}, map[appNotifyWho][]time.Time{}
	}
	for k, at := range l.refs {
		if now.Sub(at) >= appNotifyRefWindow {
			delete(l.refs, k)
		}
	}
	for k, at := range l.live {
		if now.Sub(at) >= appNotifyPluginWindow {
			delete(l.live, k)
		}
	}
	for _, m := range []map[appNotifyWho][]time.Time{l.sends, l.ghosts} {
		for id, ts := range m {
			if ts = trimWindow(ts, now, appNotifyPluginWindow); len(ts) == 0 {
				delete(m, id)
			} else {
				m[id] = ts
			}
		}
	}
	for id, ts := range l.ceiling {
		if ts = trimWindow(ts, now, appNotifyPluginWindow); len(ts) == 0 {
			delete(l.ceiling, id)
		} else {
			l.ceiling[id] = ts
		}
	}
}

// trimWindow drops the times in ts (oldest first) at least window before now.
func trimWindow(ts []time.Time, now time.Time, window time.Duration) []time.Time {
	i := 0
	for i < len(ts) && now.Sub(ts[i]) >= window {
		i++
	}
	return ts[i:]
}

// allow reports whether plugin may send about ref for owner at now, and
// records it (and the knock as out) when it may; server is true for a server
// app, which is also held to appNotifyServerCeiling. Refused, wait is the
// whole seconds until it may (Retry-After). A refused send is not recorded and
// spends no cap.
func (l *appNotifyLimit) allow(plugin, owner int64, ref string, server bool, now time.Time) (wait int, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(now)
	var until time.Duration
	key, who := appNotifyKey{plugin, owner, ref}, appNotifyWho{plugin, owner}
	if at, seen := l.refs[key]; seen {
		until = max(until, appNotifyRefWindow-now.Sub(at))
	}
	if ts := l.sends[who]; len(ts) >= appNotifyPluginBurst {
		until = max(until, appNotifyPluginWindow-now.Sub(ts[len(ts)-appNotifyPluginBurst]))
	}
	if ts := l.ceiling[plugin]; server && len(ts) >= appNotifyServerCeiling {
		until = max(until, appNotifyPluginWindow-now.Sub(ts[len(ts)-appNotifyServerCeiling]))
	}
	if until > 0 {
		return int(math.Ceil(until.Seconds())), false
	}
	l.refs[key] = now
	l.sends[who] = append(l.sends[who], now)
	if server {
		l.ceiling[plugin] = append(l.ceiling[plugin], now)
	}
	l.live[key] = now
	return 0, true
}

// withdraw answers a withdrawal of plugin's ref for owner at now: live when a
// knock about it FOR THAT PERSON is out (it is then no longer out, and the
// caller sends the resolved); not live, a withdrawal of nothing, counted
// against (plugin, owner); ok false (wait the Retry-After) when that count is
// past appWithdrawGhostBurst. A live withdrawal is never refused.
func (l *appNotifyLimit) withdraw(plugin, owner int64, ref string, now time.Time) (live bool, wait int, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(now)
	key, who := appNotifyKey{plugin, owner, ref}, appNotifyWho{plugin, owner}
	if _, out := l.live[key]; out {
		delete(l.live, key)
		return true, 0, true
	}
	if ts := l.ghosts[who]; len(ts) >= appWithdrawGhostBurst {
		return false, int(math.Ceil((appNotifyPluginWindow - now.Sub(ts[len(ts)-appWithdrawGhostBurst])).Seconds())), false
	}
	l.ghosts[who] = append(l.ghosts[who], now)
	return false, 0, true
}

// takeOut forgets every knock of plugin still out at now and returns them:
// an uninstall withdraws each (uninstallServerApp), so no device keeps a
// notification whose tap would open a 404.
func (l *appNotifyLimit) takeOut(plugin int64, now time.Time) []appNotifyKey {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(now)
	var out []appNotifyKey
	for k := range l.live {
		if k.plugin == plugin {
			out = append(out, k)
			delete(l.live, k)
		}
	}
	return out
}
