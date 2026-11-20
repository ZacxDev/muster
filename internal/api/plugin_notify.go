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
// 🔴 ONE TAG PER (plugin, ref): app-<plugin id>-<ref>, so a later count
// replaces the earlier one on the device and the withdrawal finds it. The tap
// opens /apps/<plugin id>?ref=<ref>, which mints at tap time like any visit.

const (
	pluginNotifyPath = pluginAPIPrefix + "/notify"

	// appNotifyKindKnock is v1's one kind.
	appNotifyKindKnock = "knock"

	// The caps (design: "at most one send per (plugin, ref) per 30 s and 30
	// per plugin per hour").
	appNotifyRefWindow    = 30 * time.Second
	appNotifyPluginBurst  = 30
	appNotifyPluginWindow = time.Hour

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
	owner, tag := p.OwnerID, appNotifyTag(p.ID, in.Ref)
	if s.pushMuted(r.Context(), owner, push.EventApp) {
		s.logPushDrop(tag, push.EventApp, owner, pushDropMuted)
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
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if wait, ok := s.appNotifies.allow(p.ID, in.Ref, s.now()); !ok {
		s.logPushDrop(tag, push.EventApp, owner, pushDropAppCap)
		w.Header().Set("Retry-After", strconv.Itoa(wait))
		s.writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": pushDropSentence(pushDropAppCap), "retry_after": wait})
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
// it, a paired phone gets payload v2's silent `resolved`). It needs no device
// check and spends no cap: a clear is never throttled (pushAllowed).
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
	tag := appNotifyTag(p.ID, ref)
	s.goNotify(p.OwnerID, push.EventResolved, RouterNotification{Type: push.TypeResolved, ID: tag, Tag: tag}, nil)
	s.writeJSON(w, http.StatusAccepted, map[string]any{"queued": true, "tag": tag})
}

// appNotifyLimit is the knock caps: at most one send per (plugin, ref) per
// appNotifyRefWindow, and appNotifyPluginBurst per plugin in any
// appNotifyPluginWindow (a sliding window: the sends' own times).
type appNotifyLimit struct {
	mu    sync.Mutex
	refs  map[appNotifyKey]time.Time // the last send per (plugin, ref)
	sends map[int64][]time.Time      // each plugin's sends in the last window, oldest first
}

type appNotifyKey struct {
	plugin int64
	ref    string
}

// allow reports whether plugin may send about ref at now, and records it when
// it may; refused, wait is the whole seconds until it may (Retry-After). A
// refused send is not recorded and spends neither cap.
func (l *appNotifyLimit) allow(plugin int64, ref string, now time.Time) (wait int, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.refs == nil {
		l.refs, l.sends = map[appNotifyKey]time.Time{}, map[int64][]time.Time{}
	}
	// Forget what has rolled out of both windows, so the maps hold only live
	// plugins and refs.
	for k, at := range l.refs {
		if now.Sub(at) >= appNotifyRefWindow {
			delete(l.refs, k)
		}
	}
	for id, ts := range l.sends {
		i := 0
		for i < len(ts) && now.Sub(ts[i]) >= appNotifyPluginWindow {
			i++
		}
		if i == len(ts) {
			delete(l.sends, id)
		} else {
			l.sends[id] = ts[i:]
		}
	}
	var until time.Duration
	key := appNotifyKey{plugin, ref}
	if at, seen := l.refs[key]; seen {
		until = max(until, appNotifyRefWindow-now.Sub(at))
	}
	if ts := l.sends[plugin]; len(ts) >= appNotifyPluginBurst {
		until = max(until, appNotifyPluginWindow-now.Sub(ts[len(ts)-appNotifyPluginBurst]))
	}
	if until > 0 {
		return int(math.Ceil(until.Seconds())), false
	}
	l.refs[key] = now
	l.sends[plugin] = append(l.sends[plugin], now)
	return 0, true
}
