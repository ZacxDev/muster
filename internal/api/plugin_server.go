package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/users"
)

// SERVER-INSTALLED APP PLUGINS at the plugin door (docs/design/meetings.md,
// "Core seam: server-installed app plugins"; internal/plugins server.go;
// docs/plugins.md#server-apps). A server app's token is the same mpl_ shape
// and authenticates through the same door; what it is SERVED is this file's.
//
// 🔴 THE DOOR, NOT THE ROW, DECIDES WHAT A SERVER TOKEN IS SERVED.
// serverPluginRoutes lists every /plugin/v1 route, each served or refused, and
// how it acts. A route not marked served is 403 ServerPluginRouteRefusal
// WHATEVER THE ROW'S CAPABILITIES SAY, and a served route checks the
// capability the TABLE names for a server app, never the personal ledger's
// (so a server row holding `messages` past its CHECK still reaches nothing a
// messages token does).
//
// WHO EACH ROUTE ACTS AS. /self, the hand-off redeem and owners/check act as
// NOBODY: they carry no request principal at all (authz.WithPrincipal panics
// on one with no user, and a server app has no user to borrow — never
// installed_by). The act-for routes (notify, its withdrawal, tasks) take one
// header, Muster-Act-For: <subject>; the door looks the subject up through
// plugins.Store LiveServerGrants (THE gate) keyed on the authenticated app,
// and the handler then runs as THAT PERSON — authz.ViaServerPlugin, under the
// person's row-level security — built from the grant row, never from the
// header.

// MusterActForHeader names the person a server app acts for, by the pairwise
// subject its redeem gave it.
const MusterActForHeader = "Muster-Act-For"

const (
	pluginOwnersCheckPath = pluginAPIPrefix + "/owners/check"

	// ServerPluginRouteRefusal answers a server app's token on a route the
	// door does not serve it.
	ServerPluginRouteRefusal = "this is a server app's token: it reads /self, redeems its own hand-offs, checks its people and knocks or files tasks for one of them, and nothing else"
	// ActForOnPersonalRefusal answers a personal plugin token carrying
	// Muster-Act-For, on every route.
	ActForOnPersonalRefusal = "Muster-Act-For is a server app's header: a personal plugin token acts as its own owner and names no one"
	// ActForShapeRefusal answers a server app's act-for route with the header
	// missing or doubled, and any other served route carrying it.
	ActForShapeRefusal = "this route needs exactly one Muster-Act-For header naming a subject (and only an act-for route takes one)"
	// ServerActForNotFound is THE ONE BODY of every act-for refusal of a
	// subject — unknown, another app's, expired, hidden, disabled, signed out
	// everywhere since, a retired origin — so the route is no oracle for
	// whether a person exists or what became of them.
	ServerActForNotFound = "no such person for this app"
)

// serverRoute is how the door serves a server app's token on one route.
type serverRoute struct {
	// served: the route is served at all; false is 403 at the door.
	served bool
	// actFor: the route acts FOR ONE PERSON, named by Muster-Act-For.
	actFor bool
	// need is what the server row must hold here (pluginTokenHas).
	need plugins.Capability
}

// serverPluginRoutes is EVERY /plugin/v1 route, as a SERVER app's token is
// served it. The pairing routes take no token and are not here.
// TestServerPluginRoutesCoverEveryPluginRoute pins this table to
// routes.golden, failing when it grows or shrinks.
var serverPluginRoutes = map[string]serverRoute{
	"GET /plugin/v1/self":                          {served: true, need: pluginCapAny},
	"POST /plugin/v1/handoffs/redeem":              {served: true, need: plugins.CapApp},
	"POST /plugin/v1/owners/check":                 {served: true, need: pluginCapServer},
	"POST /plugin/v1/notify":                       {served: true, actFor: true, need: plugins.CapApp},
	"DELETE /plugin/v1/notify/{ref}":               {served: true, actFor: true, need: plugins.CapApp},
	"POST /plugin/v1/tasks":                        {served: true, actFor: true, need: plugins.CapTasks},
	"PUT /plugin/v1/chief/instructions":            {},
	"GET /plugin/v1/drafts/checks":                 {},
	"GET /plugin/v1/tasks":                         {},
	"POST /plugin/v1/drafts/claim":                 {},
	"POST /plugin/v1/drafts/{id}/check":            {},
	"POST /plugin/v1/drafts/{id}/media":            {},
	"POST /plugin/v1/drafts/{id}/renew":            {},
	"POST /plugin/v1/drafts/{id}/result":           {},
	"POST /plugin/v1/messages":                     {},
	"POST /plugin/v1/messages/{id}/owner-answered": {},
	"POST /plugin/v1/task-writes/claim":            {},
	"POST /plugin/v1/task-writes/{id}/result":      {},
}

// serveServerPlugin is requirePluginToken's SERVER ARM, for a live server
// app p: the route table, the capability the table names, the header's
// shape, then admitServerPlugin. Every refusal here reaches no handler and
// no store call but the token lookup.
func (s *Server) serveServerPlugin(w http.ResponseWriter, r *http.Request, next http.HandlerFunc, p plugins.Plugin, actFor []string) {
	route := serverPluginRoutes[r.Pattern]
	if !route.served {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": ServerPluginRouteRefusal})
		return
	}
	if !pluginTokenHas(p, route.need) {
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": PluginCapabilityRefusal(route.need)})
		return
	}
	if route.actFor != (len(actFor) == 1) || len(actFor) > 1 {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": ActForShapeRefusal})
		return
	}
	// 🔴 AN EMPTY OR BLANK VALUE IS A SHAPE ERROR on an act-for route (audit
	// round 1): it must never reach the acts-as-nobody path, whose handlers
	// have no principal to act as. The door branches on the ROUTE, not on the
	// subject being empty.
	subject := ""
	if route.actFor {
		if subject = actFor[0]; strings.TrimSpace(subject) == "" {
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": ActForShapeRefusal})
			return
		}
	}
	r = r.WithContext(contextWithPlugin(r.Context(), p))
	s.admitServerPlugin(w, r, next, p, route.actFor, subject)
}

// admitServerPlugin is the door's second entry, beside admit: on a route that
// does not act for anyone (actFor false) it runs admit's own pre-handler refusals and
// calls next with NO principal — any store call that needs one fails closed.
// On an act-for route it asks THE gate (LiveServerGrants, one subject) and, for a
// live grant, calls admit with that person's principal through
// authz.ViaServerPlugin; anything else is the one 404, ServerActForNotFound.
func (s *Server) admitServerPlugin(w http.ResponseWriter, r *http.Request, next http.HandlerFunc, p plugins.Plugin, actFor bool, subject string) {
	if !actFor {
		nobody := authz.Principal{Via: authz.ViaServerPlugin}
		if s.refuseCrossOriginCookieWrite(w, r, nobody) || s.refusePluginTokenElsewhere(w, r, nobody.Via) ||
			s.refuseDeviceTokenElsewhere(w, r, nobody.Via) {
			return
		}
		if s.serverNobodyProbe != nil {
			s.serverNobodyProbe(r)
		}
		next(w, r)
		return
	}
	if !plugins.ValidSubject(subject) {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": ServerActForNotFound})
		return
	}
	live, err := s.ext.Plugins.LiveServerGrants(r.Context(), p, s.ext.PluginOrigins, []string{subject}, s.now())
	if err != nil {
		s.logger.Printf("plugins: server app %d: act-for lookup: %v", p.ID, err)
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "could not check who this app acts for"})
		return
	}
	// LIVE ONLY: expired and revoked are the same 404 here.
	g := live[subject]
	if g.State != plugins.GrantLive || g.Owner <= 0 {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": ServerActForNotFound})
		return
	}
	owner := g.Owner
	// The principal comes from THE ROW, never from the header. serverGrantLive
	// admits only an enabled person; a team here would be a bug, and admit
	// refuses it (403) before any handler.
	s.admit(w, r, next, s.principalFor(r.Context(), owner, authz.ViaServerPlugin))
}

// contextWithPlugin is ctx carrying the authenticated plugin p, for
// pluginFromContext.
func contextWithPlugin(ctx context.Context, p plugins.Plugin) context.Context {
	return context.WithValue(ctx, pluginCtxKey{}, p)
}

// serverPluginSelf is GET /plugin/v1/self for a server app: no person, no
// count of people, no list.
type serverPluginSelf struct {
	PluginID     int64                `json:"plugin_id"`
	PluginName   string               `json:"plugin_name"`
	Scope        string               `json:"scope"`
	OwnerID      *int64               `json:"owner_id"`
	OwnerLogin   *string              `json:"owner_login"`
	OwnerKind    string               `json:"owner_kind"`
	Capabilities []plugins.Capability `json:"capabilities"`
	DeliverTo    *string              `json:"deliver_to"`
	ActForHours  int                  `json:"act_for_hours"`
}

// messagesOwner is plugin p's owner on a path only a PERSONAL plugin
// reaches: the messages, drafts, task-writes, references and mail-resume
// paths. serverPluginRoutes serves a server app none of their routes, and a
// server row can hold no `messages` (plugins_server_caps). For a server app
// it is 0 — nobody — so a push, a lookup or a lock keyed on it reaches no
// one. The one spelling of that read (the owner read-site ledger counts it).
func messagesOwner(p plugins.Plugin) int64 {
	o, _ := p.PersonalOwner()
	return o
}

// --- POST /plugin/v1/owners/check ---------------------------------------------

const (
	// ownersCheckPerMinute is one server app's owners/check calls a minute.
	ownersCheckPerMinute = 60
	// maxOwnersCheckBody bounds what is read: room for MaxOwnersCheck subjects
	// pretty-printed (each on its own indented line) with plenty to spare.
	// The count is enforced after decoding (audit round 1: a cap sized for
	// compact JSON refused an indented body of 200 valid subjects).
	maxOwnersCheckBody = 16 << 10
)

// handlePluginOwnersCheck is POST /plugin/v1/owners/check {"subjects": [...]}
// → {"states": {"<subject>": "<state>"}}: EVERY subject it was sent, once,
// and no other, with its state under THE gate now
// (LiveServerGrants, the act-for door's own): "live" (the app may act for the
// person), "expired" (the grant only ran out of time: the person's next visit
// renews it, and the plugin keeps their open meeting) or "revoked" (anything
// else: the plugin ends their sessions). One word for every state, and never
// why a subject is revoked. It says nothing about a subject the
// plugin did not already hold beyond "revoked". 1 to MaxOwnersCheck subjects,
// each a subject's shape; ownersCheckPerMinute calls a minute per app.
func (s *Server) handlePluginOwnersCheck(w http.ResponseWriter, r *http.Request) {
	p := pluginFromContext(r.Context())
	var in struct {
		Subjects []string `json:"subjects"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, maxOwnersCheckBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || len(in.Subjects) == 0 || len(in.Subjects) > plugins.MaxOwnersCheck {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the body is {\"subjects\": [1-" +
			strconv.Itoa(plugins.MaxOwnersCheck) + " subjects]}"})
		return
	}
	for _, sub := range in.Subjects {
		if !plugins.ValidSubject(sub) {
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "each subject is the 22-character subject a redeem gave"})
			return
		}
	}
	if wait, ok := s.ownersChecks.allow(p.ID, s.now()); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(wait))
		s.writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "too many owners checks this minute", "retry_after": wait})
		return
	}
	got, err := s.ext.Plugins.LiveServerGrants(r.Context(), p, s.ext.PluginOrigins, in.Subjects, s.now())
	if err != nil {
		s.logger.Printf("plugins: server app %d: owners check: %v", p.ID, err)
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "could not check the subjects"})
		return
	}
	// One entry per subject asked about, and no other: a subject this app
	// never redeemed is "revoked", as any revoked one is, so the answer cannot
	// tell "never seen" from "ended".
	states := make(map[string]plugins.GrantState, len(in.Subjects))
	for _, sub := range in.Subjects {
		st := got[sub].State
		if st != plugins.GrantLive && st != plugins.GrantExpired {
			st = plugins.GrantRevoked
		}
		states[sub] = st
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"states": states})
}

// ownersCheckLimit is ownersCheckPerMinute per app, a sliding minute.
type ownersCheckLimit struct {
	mu    sync.Mutex
	calls map[int64][]time.Time
}

func (l *ownersCheckLimit) allow(app int64, now time.Time) (int, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.calls == nil {
		l.calls = map[int64][]time.Time{}
	}
	ts := trimWindow(l.calls[app], now, time.Minute)
	if len(ts) >= ownersCheckPerMinute {
		l.calls[app] = ts
		return int(math.Ceil((time.Minute - now.Sub(ts[len(ts)-ownersCheckPerMinute])).Seconds())), false
	}
	l.calls[app] = append(ts, now)
	return 0, true
}

// --- POST /plugin/v1/tasks for a server app ----------------------------------

const (
	// serverTaskPersonBurst is one (app, person)'s task writes (a create or a
	// comment) per serverTaskWindow; serverTaskAppBurst one app's, across
	// everyone it acts for.
	serverTaskPersonBurst = 60
	serverTaskAppBurst    = 600
	serverTaskWindow      = time.Hour
)

// serverTaskLimit is a server app's task-write caps: serverTaskPersonBurst per
// (app, person) and serverTaskAppBurst per app in any serverTaskWindow. A
// write is RESERVED before the store runs and REFUNDED when it wrote nothing
// (a repeat the route's idempotency answers from the stored row spends
// nothing).
type serverTaskLimit struct {
	mu     sync.Mutex
	person map[appNotifyWho][]time.Time
	app    map[int64][]time.Time
}

// reserve takes one write for (app, owner) at now, or answers the whole
// seconds until one is free.
func (l *serverTaskLimit) reserve(app, owner int64, now time.Time) (int, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.person == nil {
		l.person, l.app = map[appNotifyWho][]time.Time{}, map[int64][]time.Time{}
	}
	l.prune(now)
	who := appNotifyWho{app, owner}
	pt, at := l.person[who], l.app[app]
	var until time.Duration
	if len(pt) >= serverTaskPersonBurst {
		until = max(until, serverTaskWindow-now.Sub(pt[len(pt)-serverTaskPersonBurst]))
	}
	if len(at) >= serverTaskAppBurst {
		until = max(until, serverTaskWindow-now.Sub(at[len(at)-serverTaskAppBurst]))
	}
	if until > 0 {
		return int(math.Ceil(until.Seconds())), false
	}
	l.person[who], l.app[app] = append(pt, now), append(at, now)
	return 0, true
}

// prune forgets every write that has rolled out of the window, and every key
// left with none, so the maps hold only who wrote in the last hour (as
// appNotifyLimit.prune does). l.mu is held.
func (l *serverTaskLimit) prune(now time.Time) {
	for k, ts := range l.person {
		if ts = trimWindow(ts, now, serverTaskWindow); len(ts) == 0 {
			delete(l.person, k)
		} else {
			l.person[k] = ts
		}
	}
	for k, ts := range l.app {
		if ts = trimWindow(ts, now, serverTaskWindow); len(ts) == 0 {
			delete(l.app, k)
		} else {
			l.app[k] = ts
		}
	}
}

// refund gives back the write reserve took at now for (app, owner).
func (l *serverTaskLimit) refund(app, owner int64, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	who := appNotifyWho{app, owner}
	drop := func(ts []time.Time) []time.Time {
		for i := len(ts) - 1; i >= 0; i-- {
			if ts[i].Equal(now) {
				return append(ts[:i:i], ts[i+1:]...)
			}
		}
		return ts
	}
	l.person[who], l.app[app] = drop(l.person[who]), drop(l.app[app])
	if len(l.person[who]) == 0 {
		delete(l.person, who)
	}
	if len(l.app[app]) == 0 {
		delete(l.app, app)
	}
}

// handleServerAppTask is POST /plugin/v1/tasks for a SERVER app, acting for
// the person its grant names (the request principal). 🔴 ONLY TASKS THIS APP
// FILED: the key resolves through plugin_server_tasks alone
// (plugins.ServerTaskLedger, notes.Store FileOnceByLedger), never through
// FindLiveByTag across the person's tasks — a global lookup would be a
// membership oracle and a comment target. A key with no ledger row, or whose
// ledgered task is no longer open, behaves exactly as a key no task carries:
// with create a new task is filed (and the ledger re-pointed), with a bare
// comment the answer is 200 {"id": null, "created": false, "commented":
// false}, the same bytes either way, and nothing is written. route_to_chief
// and reference were refused 400 before this (pluginTasksOnlyRefusal), and no
// task of a server app auto-starts anything.
func (s *Server) handleServerAppTask(w http.ResponseWriter, r *http.Request, p plugins.Plugin, in pluginTaskIn) {
	ctx := r.Context()
	owner := authz.MustFrom(ctx).UserID
	now := s.now()
	if wait, ok := s.serverTasks.reserve(p.ID, owner, now); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(wait))
		s.writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "too many task writes this hour", "retry_after": wait})
		return
	}
	wrote := false
	defer func() {
		if !wrote {
			s.serverTasks.refund(p.ID, owner, now)
		}
	}()
	var comment *notes.Comment
	if in.Comment != "" {
		comment = &notes.Comment{Author: pluginCommentAuthor(p), Body: in.Comment}
	}
	ledger := plugins.ServerTaskLedger{App: p.ID, Owner: owner, Key: in.Key}
	answer := func(code int, id *int64, created, commented bool) {
		s.writeJSON(w, code, map[string]any{"id": id, "created": created, "commented": commented})
	}
	if in.Create == nil {
		n, found, _, commented, err := s.ext.Notes.FileOnceByLedger(ctx, ledger, nil, comment)
		switch {
		case err == nil:
		case errors.Is(err, pgx.ErrNoRows):
			found = false // closed between the lookup and the comment: as if it never existed
		default:
			s.logger.Printf("plugins: server app %d task %s: comment: %v", p.ID, in.Key, err)
			s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the comment could not be added"})
			return
		}
		if !found {
			answer(http.StatusOK, nil, false, false)
			return
		}
		if commented {
			wrote = true
			s.broadcast(ctx, EventTaskChanged, strconv.FormatInt(n.ID, 10))
		}
		answer(http.StatusOK, &n.ID, false, commented)
		return
	}
	task := taskCreateInput{Title: in.Create.Title, Body: in.Create.Body, Source: "api",
		CreatedBy: notes.CreatedByPlugin(firstNonBlank(p.Label, p.Name))}
	want, err := s.taskWant(ctx, task)
	if err != nil {
		var ve errTagValidation
		if errors.As(err, &ve) {
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": ve.Error()})
			return
		}
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the task could not be filed"})
		return
	}
	n, created, commented, err := s.fileTask(ctx, task, want, nil, func(want notes.Note) (notes.Note, bool, bool, error) {
		n, _, created, commented, err := s.ext.Notes.FileOnceByLedger(ctx, ledger, &want, comment)
		return n, created, commented, err
	})
	if err != nil {
		s.logger.Printf("plugins: server app %d task %s: create: %v", p.ID, in.Key, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the task could not be filed"})
		return
	}
	wrote = created || commented
	if created {
		answer(http.StatusCreated, &n.ID, true, commented)
		return
	}
	if commented {
		s.broadcast(ctx, EventTaskChanged, strconv.FormatInt(n.ID, 10))
	}
	answer(http.StatusOK, &n.ID, false, commented)
}

// --- uninstall ------------------------------------------------------------------

// uninstallServerApp uninstalls live server app id (plugins.Store
// UninstallServerApp: revoked, every grant, ledger row and unspent code gone,
// in one transaction) and then WITHDRAWS EVERY KNOCK STILL OUT (muster's own
// ledger knows which), so no device keeps a notification whose tap would open
// a 404. The one uninstall path: the /admin/apps page and the muster-server
// app CLI (core PR 1h.2) both call it.
func (s *Server) uninstallServerApp(r *http.Request, id int64, by users.Actor) (plugins.Plugin, error) {
	p, err := s.ext.Plugins.UninstallServerApp(r.Context(), id, by)
	if err != nil {
		return plugins.Plugin{}, err
	}
	for _, k := range s.appNotifies.takeOut(p.ID, s.now()) {
		tag := appNotifyTag(k.plugin, k.ref)
		s.goNotify(k.owner, push.EventResolved, RouterNotification{Type: push.TypeResolved, ID: tag, Tag: tag}, nil)
	}
	return p, nil
}

// --- the hand-off redeem for a server app -------------------------------------

// redeemServerHandoff is POST /plugin/v1/handoffs/redeem for a server app:
// the pairwise subject and display name, never an owner id or an email, or
// the one 404. Counted in muster_plugin_server_grants_total; no audit row.
func (s *Server) redeemServerHandoff(w http.ResponseWriter, r *http.Request, p plugins.Plugin, code string) {
	h, err := s.ext.Plugins.RedeemServerHandoff(r.Context(), p, code, s.now())
	switch {
	case err == nil:
	case errors.Is(err, plugins.ErrNotFound):
		metrics.PluginServerGrants.WithLabelValues("not_found").Inc()
		metrics.PluginHandoffs.WithLabelValues("unknown", "not_found").Inc()
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": PluginHandoffNotFound})
		return
	default:
		s.logger.Printf("plugins: server app %d: hand-off redeem: %v", p.ID, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the hand-off could not be redeemed"})
		return
	}
	metrics.PluginServerGrants.WithLabelValues("granted").Inc()
	metrics.PluginHandoffs.WithLabelValues(h.Purpose, "redeemed").Inc()
	s.writeJSON(w, http.StatusOK, h)
}
