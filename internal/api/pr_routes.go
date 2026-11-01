package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/prroute"
)

// ---------------------------------------------------------------------------
// PULL-REQUEST → SESSION ROUTING (migration 0055):
// docs/plugins.md#a-pull-request-review-mail.
//
//	the host's PostToolUse hook (outside muster)
//	   on a successful `gh pr create` or `git push` ──▶ POST /api/pr-sessions
//	   {repo, pr, head_ref, claude_session, kind} with route reply's credential
//	   ──▶ session_prs, last_seen now (handlePRSessionBind)
//	a review mail (routeByPR) from a listed, VERIFIED agent-mail sender ──▶ its
//	   last ```json block (prroute.ParseMail) ──▶ the owner's most recent
//	   author ──▶ routeByMuster (the chief's own route, made by muster) — or,
//	   with no online window for that session, the chief's ordinary delivery
//	   with the session named in its turn (prAuthorHint)
//
// 🔴 THE REPORT IS STORED WHETHER OR NOT A WINDOW SHOWS THE SESSION, unlike a
// thread bind (409). A thread binding routes ONLY to a live window, so binding
// one nobody can see would be useless; an authorship row is ALSO what names
// the author to the chief when its window is gone (prAuthorHint), which is
// exactly the case a window check would have dropped — a pull request opened
// in a session that is not mirrored, or whose window has closed by the time
// the review lands. The host is recorded when a window shows the session.
// ---------------------------------------------------------------------------

// prSessionsPath is the hook's report route.
const prSessionsPath = "/api/pr-sessions"

// PRMail is pull-request → session routing's configuration.
type PRMail struct {
	// Senders is MUSTER_PR_MAIL_SENDERS: whose verified agent mail is read as
	// a pull-request review. Empty is the feature off.
	Senders plugins.PRMailSenders
}

// routedByPR: a route by pull request (routeByPR).
const routedByPR = "muster, because this session created or pushed the pull request the message is about"

// prRouteOn reports whether pull-request routing is on: a sender list, its
// store, and session routing (the route it makes).
func (s *Server) prRouteOn() bool {
	return s.ext.PRMail != nil && len(s.ext.PRMail.Senders) > 0 && s.ext.PRSessions != nil && routingOn(s.ext)
}

// prMailRef is the pull request message m of plugin p reviews: only with
// routing on, only from a listed sender the plugin VERIFIED, and only when its
// body's last json block names one (prroute.ParseMail).
func (s *Server) prMailRef(p plugins.Plugin, m plugins.Message) (prroute.Ref, bool) {
	if !s.prRouteOn() || !s.ext.PRMail.Senders.Allows(p.Name, m.Sender, m.SenderVerified) {
		return prroute.Ref{}, false
	}
	return prroute.ParseMail(m.Body)
}

// prName is ref as the route and its log name it: "<owner>/<repo>#<n>".
func prName(ref prroute.Ref) string { return ref.Repo + "#" + strconv.FormatInt(ref.PR, 10) }

// routeByPR routes a NEW message m of plugin p — a review of a pull request
// one of the owner's sessions authored — to that session, as routeByThread
// routes a reply (routeByMuster). It reports whether m is taken care of; false
// leaves m to the ordinary delivery to the chief, whose turn then names the
// author when there is one (prAuthorHint).
func (s *Server) routeByPR(ctx context.Context, p plugins.Plugin, m plugins.Message) bool {
	mr, ok := s.prRoute(ctx, p, m)
	if !ok {
		return false
	}
	if s.routeByMuster(ctx, p, m, mr.origin, mr.target, mr.what) {
		s.logger.Printf("prroute: message %d: %s routed to its author %s", m.ID, mr.origin.PR, mr.origin.Session)
		return true
	}
	s.logger.Printf("prroute: message %d: %s's author %s cannot take it now; to the chief, naming it", m.ID, mr.origin.PR, mr.origin.Session)
	return false
}

// prRoute is the route routeByPR makes for m: to the session that most
// recently authored the pull request it reviews, with the host recorded for
// it. false when m is not such a review or no author is recorded.
func (s *Server) prRoute(ctx context.Context, p plugins.Plugin, m plugins.Message) (musterRoute, bool) {
	if s.pluginDeliverer == nil {
		return musterRoute{}, false
	}
	ref, ok := s.prMailRef(p, m)
	if !ok {
		return musterRoute{}, false
	}
	hit, found, err := s.ext.PRSessions.ResolvePRSession(ctx, ref)
	switch {
	case err != nil:
		s.logger.Printf("prroute: message %d: resolve %s: %v; to the chief", m.ID, prName(ref), err)
		return musterRoute{}, false
	case !found:
		s.logger.Printf("prroute: message %d: no session of the owner authored %s; to the chief", m.ID, prName(ref))
		return musterRoute{}, false
	}
	return musterRoute{origin: plugins.RouteOrigin{Via: plugins.RouteViaPR, Session: hit.ClaudeSession, PR: prName(ref)}, host: hit.Host,
		target: "the session that authored pull request " + prName(ref), what: "a review of pull request " + prName(ref)}, true
}

// prAuthorHint is the author block for message m, delivered to the chief —
// which happens when routeByPR could not route it: the session that authored
// its pull request, with what muster saw it do and whether a window shows it
// now. nil when m is not a review muster reads, or no author is recorded. ctx
// acts as m's owner.
func (s *Server) prAuthorHint(ctx context.Context, m plugins.Message) *plugins.AuthorHint {
	if !s.prRouteOn() {
		return nil
	}
	p, err := s.ext.Plugins.Plugin(ctx, m.PluginID)
	if err != nil {
		return nil
	}
	ref, ok := s.prMailRef(p, m)
	if !ok {
		return nil
	}
	hit, found, err := s.ext.PRSessions.ResolvePRSession(ctx, ref)
	if err != nil || !found {
		return nil
	}
	a := &plugins.AuthorHint{ID: focus.MirrorSubject(hit.ClaudeSession).String(), Host: hit.Host, Kind: string(hit.Kind),
		Repo: hit.Repo, PR: hit.PR, HeadRef: hit.HeadRef, LastSeen: hit.LastSeen}
	if e, ok := s.mirrorEntryForSession(ctx, hit.ClaudeSession); ok {
		a.Online, a.Host, a.Name, a.Where = true, e.Host, e.DisplayTitle(), e.Where()
	}
	if s.ext.Focus != nil {
		if f, has, err := s.ext.Focus.Get(ctx, focus.MirrorSubject(hit.ClaudeSession)); err == nil && has && f.Has() {
			a.Name = f.Title
		}
	}
	return a
}

// --- the hook's report ----------------------------------------------------------

// prSessionBadRequest is the 400 body for every refused report.
const prSessionBadRequest = `the body is {"repo": "<owner>/<repo>", "pr": <positive integer> | null, "head_ref": "<branch>" | null, ` +
	`"claude_session": "<claude session id>", "kind": "gh-pr-create" | "git-push"}: a create names its pr (and may name ` +
	`its head_ref), a push names its head_ref and no pr`

// handlePRSessionBind is POST /api/pr-sessions: a session's PostToolUse hook
// reports that it created a pull request or pushed a branch, with route
// reply's credential, acting as the owner. 201: recorded; 200: the same
// authorship again, now the most recent; 400: not a well-formed report; 404
// with pull-request routing off. No response body on success.
func (s *Server) handlePRSessionBind(w http.ResponseWriter, r *http.Request) {
	if !s.prRouteOn() {
		http.NotFound(w, r)
		return
	}
	var in struct {
		Repo          string  `json:"repo"`
		PR            *int64  `json:"pr"`
		HeadRef       *string `json:"head_ref"`
		ClaudeSession string  `json:"claude_session"`
		Kind          string  `json:"kind"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": prSessionBadRequest})
		return
	}
	sig := prroute.Signal{Kind: prroute.Kind(in.Kind), Repo: in.Repo}
	if in.PR != nil {
		if *in.PR <= 0 {
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": prSessionBadRequest})
			return
		}
		sig.PR = *in.PR
	}
	if in.HeadRef != nil {
		if *in.HeadRef == "" {
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": prSessionBadRequest})
			return
		}
		sig.HeadRef = *in.HeadRef
	}
	// A Claude session id, exactly as the thread bind takes one — and never a
	// runtime-qualified key ("hermes:<id>"): only a Claude Code hook reports.
	sub, err := focus.ParseSubject(string(focus.KindMirror) + ":" + in.ClaudeSession)
	if err != nil || strings.Contains(sub.Key, ":") || !sig.Valid() {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": prSessionBadRequest})
		return
	}
	host := ""
	if e, ok := s.mirrorEntryForSession(r.Context(), sub.Key); ok {
		host = e.Host
	}
	created, err := s.ext.PRSessions.RecordPRSession(r.Context(), sub.Key, host, sig)
	if err != nil {
		s.logger.Printf("prroute: record %s for %s: %v", sig.Kind, sub.Key, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the report could not be recorded"})
		return
	}
	what := sig.Repo + "#" + strconv.FormatInt(sig.PR, 10)
	if sig.Kind == prroute.KindPush {
		what = sig.Repo + " branch " + sig.HeadRef
	}
	s.logger.Printf("prroute: session %s %s %s (window host %q, new %v)", sub.Key, sig.Kind, what, host, created)
	if created {
		w.WriteHeader(http.StatusCreated)
		return
	}
	w.WriteHeader(http.StatusOK)
}
