package mirror

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/attemptcode"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/clientaddr"
	"github.com/ZacxDev/muster/internal/provision"
	"github.com/ZacxDev/muster/internal/stepup"
)

const (
	// TokenRecheck is how often a live connection's token is re-read, so a
	// revoked token ends a connection (and its open terminals) that is already up.
	TokenRecheck = 30 * time.Second
	// RecheckFailClosedAfter is how many CONSECUTIVE failed rechecks (a database
	// error, not a revocation) end the connection. One is tolerated — a blip —
	// but a token that cannot be re-verified is not kept live indefinitely.
	RecheckFailClosedAfter = 2
	// authFailLimit is how many failed token checks one client address may make
	// per authFailWindow before /mirror/connect answers 429 until the window
	// ends.
	authFailLimit  = 10
	authFailWindow = time.Minute
)

// ErrNoPrincipal: a view or attach was asked for with no user on the context.
var ErrNoPrincipal = errors.New("mirror: no user principal on this request")

// stepUpRequired is ErrStepUpRequired's type: stepup.ErrStale under this
// package's prefix. A type rather than fmt.Errorf("%w") because ErrStale's text
// names the CONFIGURED window and Errorf would freeze it at package load.
type stepUpRequired struct{}

func (stepUpRequired) Error() string { return "mirror: " + stepup.ErrStale.Error() }
func (stepUpRequired) Unwrap() error { return stepup.ErrStale }

// ErrStepUpRequired: an interactive attach, or a paste, on a session that did
// not sign in within stepup.Window.
var ErrStepUpRequired error = stepUpRequired{}

// Service is the mirror plugin as internal/api consumes it: the connect
// endpoint, the viewing principal's hosts, and terminal attach.
type Service struct {
	hub    *Hub
	store  Store
	logger *log.Logger

	// recheck is TokenRecheck; a test shortens it.
	recheck time.Duration

	limiter *failLimiter

	// connTok is which token each (owner, host) connection was made with,
	// for HostToken; gen tells a connection's own entry from its successor's.
	connMu  sync.Mutex
	connTok map[hostKey]connToken
	connGen uint64
}

type connToken struct {
	id  int64
	gen uint64
	// team and code: the connection is a team host's, presenting this attempt
	// code's hash (nil: none) — how `muster doctor` on the LIVE machine is
	// told from a held one (teamhold.go, HeldHere).
	team bool
	code *[32]byte
}

// HostToken is the id of the token the viewing principal's host is
// connected with now, if it is connected through ServeConnect. Two machines
// minted under one host name are one hub entry; the token id is what tells
// which of them holds it (`muster doctor`'s this-host, via GET /mirror/host).
func (s *Service) HostToken(ctx context.Context, host string) (int64, bool) {
	o, err := owner(ctx)
	if err != nil {
		return 0, false
	}
	s.connMu.Lock()
	defer s.connMu.Unlock()
	c, ok := s.connTok[hostKey{o, host}]
	return c.id, ok
}

// NewService builds the plugin over store.
func NewService(store Store, logger *log.Logger) *Service {
	if logger == nil {
		logger = log.Default()
	}
	s := &Service{store: store, logger: logger, recheck: TokenRecheck, hub: NewHub(logger),
		limiter: newFailLimiter(authFailLimit, authFailWindow)}
	return s
}

// spawnMarker is the store's half that persists "this token's host has
// started agents" (PGStore.MarkSpawned, mirror_tokens.spawned_at).
type spawnMarker interface {
	MarkSpawned(ctx context.Context, id int64) error
}

// markSpawnedTries is how many times markSpawned writes the mark before the
// connection is refused (ServeConnMarking fails closed).
const markSpawnedTries = 3

// markSpawned marks token id — the one THIS connection authenticated with,
// passed in by ServeConnect, never looked up by host — as a token whose host
// starts agents for the chief (PGStore.MarkSpawned). 🔴 THE MARK IS THE
// TOKEN'S, and nothing but rotating the token clears it (internal/api
// host_stepup.go): a spawned agent can read the token, so the host stopping
// to advertise -agent-spawn proves nothing about who holds it. A store with
// no such half marks nothing (an in-memory test store).
func (s *Service) markSpawned(ctx context.Context, id int64) error {
	m, ok := s.store.(spawnMarker)
	if !ok {
		return nil
	}
	var err error
	for i := 0; i < markSpawnedTries; i++ {
		wctx, cancel := context.WithTimeout(authz.System(ctx), 10*time.Second)
		err = m.MarkSpawned(wctx, id)
		cancel()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Duration(i+1) * 200 * time.Millisecond):
		}
	}
	return err
}

// SetNotify installs the Agents-view refresh nudge (internal/api's
// agents-changed broadcast), called with the OWNER of the host that changed:
// the event reaches only that owner's open views (accounts PR 3b). It carries
// no data: it only tells them to re-fetch, and each re-fetch is scoped to its
// own principal.
func (s *Service) SetNotify(fn func(owner int64)) { s.hub.SetNotify(fn) }

// SetWaitingNotify installs the hub's waiting-set hook (Hub.SetWaitingNotify).
func (s *Service) SetWaitingNotify(fn func(owner int64)) { s.hub.SetWaitingNotify(fn) }

// SetStatusNotify installs the hub's status hook (Hub.SetStatusNotify).
func (s *Service) SetStatusNotify(fn func(owner int64)) { s.hub.SetStatusNotify(fn) }

// SetSpawnReplyNotify installs the hub's late-reply hook (Hub.SetSpawnReplyNotify).
func (s *Service) SetSpawnReplyNotify(fn func(owner int64, host string, m Msg)) {
	s.hub.SetSpawnReplyNotify(fn)
}

// SetListingNotify installs the hub's listing hook (Hub.SetListingNotify).
func (s *Service) SetListingNotify(fn func(owner int64, host string, windows []Window)) {
	s.hub.SetListingNotify(fn)
}

// SetOutcomesNotify installs the hub's outcomes hook (Hub.SetOutcomesNotify).
func (s *Service) SetOutcomesNotify(fn func(owner int64, host string, seq uint64, windows []Window, outcomes map[string]WindowOutcome)) {
	s.hub.SetOutcomesNotify(fn)
}

// SpawnAgent asks one of the viewing principal's hosts to start an agent for
// host dispatch dispatch. See Hub.SpawnAgent.
func (s *Service) SpawnAgent(ctx context.Context, host string, dispatch int64, req AgentSpawnReq) (AgentSpawned, error) {
	o, err := access(ctx, authz.TeamOperator)
	if err != nil {
		return AgentSpawned{}, err
	}
	return s.hub.SpawnAgent(ctx, o, host, dispatch, req)
}

// CloseAgent asks ctx's principal's host to end the agent in window req
// (agentclose.go).
func (s *Service) CloseAgent(ctx context.Context, host string, req AgentCloseReq) (AgentCloseResult, error) {
	o, err := access(ctx, authz.TeamOperator)
	if err != nil {
		return AgentCloseResult{}, err
	}
	return s.hub.CloseAgent(ctx, o, host, req)
}

// SetNotifyDebounce overrides the nudge coalescing window (tests).
func (s *Service) SetNotifyDebounce(d time.Duration) {
	s.hub.mu.Lock()
	s.hub.debounce = d
	s.hub.mu.Unlock()
}

// SetActivityNudgeEvery overrides activityNudgeEvery, the per-owner throttle on
// activity-only nudges (tests).
func (s *Service) SetActivityNudgeEvery(d time.Duration) {
	s.hub.mu.Lock()
	s.hub.activityEvery = d
	s.hub.mu.Unlock()
}

// Hub is the service's hub (tests).
func (s *Service) Hub() *Hub { return s.hub }

// SetTokenRecheck overrides TokenRecheck (tests).
func (s *Service) SetTokenRecheck(d time.Duration) { s.recheck = d }

// bearer is the request's `Authorization: Bearer` value.
func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if v, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// clientAddr is the failure counter's key: the request's client address
// (internal/clientaddr, resolved by muster's handler): the TCP peer unless
// that peer is a configured trusted proxy.
//
// ⚠ Behind a proxy that is NOT configured as trusted, every client shares ONE
// address and one failure count — which is safe ONLY because the count never
// refuses a VALID token (see ServeConnect): a flood of bad tokens from anyone
// earns those bad tokens 429s, and a legitimate host's reconnect is unaffected.
func clientAddr(r *http.Request) string {
	return clientaddr.Of(r)
}

// ServeConnect is GET /mirror/connect: authenticate the host's token, upgrade,
// and run the connection.
//
// 🔴 THE TOKEN IS THE ONLY DOOR, AND IT IS A MIRROR TOKEN OR NOTHING. The route
// is not behind the browser session or the hook token: a host is neither. It is
// looked up by hash in mirror_tokens and nowhere else, so the hook, service and
// agent tokens are refused here. A mirror token also opens the AskUserQuestion
// hook's routes (/mirror/ask, internal/api's mirror_ask.go) and POST
// /mirror/mcp (mirror_mcp.go): the attention and task tools of that host's own
// Claude Code sessions — asks, and only the tasks those sessions created.
//
// 🔴 THE LOOKUP RUNS UNDER authz.System: mirror_tokens is under row-level
// security and nobody is known until the token is found. What the token yields
// — its OWNER and HOST — is everything the connection is then allowed: its
// sessions are registered under that owner only.
//
// 🔴 THE TOKEN IS LOOKED UP FIRST, AND A VALID TOKEN IS NEVER REFUSED BY THE
// FAILURE LIMIT. The limit used to be checked before the lookup, keyed on the
// TCP peer — which behind the public path is one address for everyone — so ~10
// bad tokens a minute from anyone kept every legitimate host's reconnect
// refused. Now:
//   - a string that is not even SHAPED like a mirror token (TokenShapeOK) is
//     refused without touching the database — that, not the limiter, is what
//     keeps junk off the database;
//   - a well-shaped token is always looked up (one indexed read by unique hash);
//     a valid one proceeds whatever the failure count says;
//   - only FAILURES count, and an address past the limit gets 429 instead of
//     401 for its failures — a signal to a misconfigured host that it is
//     hammering, not a lock on anyone. The secret's strength (256 bits) is the
//     guessing defence.
func (s *Service) ServeConnect(w http.ResponseWriter, r *http.Request) {
	tok, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	// No OriginPatterns: a host sends no Origin, and a browser page cannot set
	// the Authorization header on a WebSocket, so a cross-site page never gets
	// this far. Accept still refuses a cross-origin Origin if one is sent.
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.logger.Printf("mirror: accept %s: %v", tok.Host, err)
		return
	}
	defer ws.CloseNow()

	key := hostKey{tok.OwnerID, tok.Host}
	// 🔴 A TEAM HOST IS PINNED (teamhold.go): a second connection for a live
	// (team, host) is refused as held — BEFORE it touches connTok or the hub,
	// so the live connection's token stays the one tracked, and the held one
	// is sent nothing — until a manager confirms it.
	peer := clientAddr(r)
	code := attemptCodeOf(r)
	if tok.Team {
		if admit, announce := s.hub.admitPinned(key, peer, code); !admit {
			s.refuseHeld(r, ws, tok, peer, code, announce)
			return
		}
	}

	// A hijacked connection's request context does not end with the TCP
	// connection, and nothing else ends this one on revocation, so its life is
	// its own context: cancelled by the token check below or by the read loop.
	ctx, cancel := context.WithCancelCause(context.WithoutCancel(r.Context()))
	defer cancel(nil)
	go s.watchToken(ctx, cancel, tok)
	s.connMu.Lock()
	if s.connTok == nil {
		s.connTok = map[hostKey]connToken{}
	}
	s.connGen++
	mine := connToken{id: tok.ID, gen: s.connGen, team: tok.Team, code: code}
	s.connTok[key] = mine
	s.connMu.Unlock()
	defer func() {
		s.connMu.Lock()
		if s.connTok[key] == mine {
			delete(s.connTok, key)
		}
		s.connMu.Unlock()
	}()
	err = s.hub.ServeConnMarking(ctx, tok.OwnerID, tok.Host, ws, func(ctx context.Context) error { return s.markSpawned(ctx, tok.ID) })
	s.logger.Printf("mirror: host %s connection ended: %v", tok.Host, err)
}

// refuseHeld answers a held team host connection (teamhold.go): its hello
// first, as every refusal reads it, so the host reads the refusal as muster's
// answer to its registration. Logged, and the team's views nudged, only when
// announce (at most once per heldAnnounceEvery per host): a machine in a
// tight retry loop is counted on the card, not announced each time. The log
// names the attempt's code by its fingerprint only (internal/attemptcode).
func (s *Service) refuseHeld(r *http.Request, ws *websocket.Conn, tok Token, peer string, code *[32]byte, announce bool) {
	_, _ = readHello(r.Context(), ws)
	if code == nil {
		refuse(r.Context(), ws, ErrHeldNoCode)
	} else {
		refuse(r.Context(), ws, ErrHeldForManager)
	}
	if announce {
		which := "with no attempt code (it cannot be confirmed)"
		if code != nil {
			which = "with attempt code " + attemptcode.Fingerprint(*code) + " (fingerprint)"
		}
		s.logger.Printf("mirror: team host %s (owner %d): a second connection from %s %s is held", tok.Host, tok.OwnerID, peer, which)
		s.hub.changed(tok.OwnerID)
	}
}

// authenticate checks the request's mirror token (ServeConnect's rules: the
// shape first, the lookup under authz.System, only FAILURES counted against the
// address) and answers the refusal itself when there is one.
func (s *Service) authenticate(w http.ResponseWriter, r *http.Request) (Token, bool) {
	addr := clientAddr(r)
	refuse := func() {
		s.limiter.fail(addr)
		if s.limiter.blocked(addr) {
			http.Error(w, "mirror: too many failed token checks from this address; wait a minute", http.StatusTooManyRequests)
			return
		}
		http.Error(w, ErrBadToken.Error(), http.StatusUnauthorized)
	}
	presented := bearer(r)
	if !TokenShapeOK(presented) {
		refuse()
		return Token{}, false
	}
	tok, err := s.store.Authenticate(authz.System(r.Context()), presented)
	if err != nil {
		if errors.Is(err, ErrBadToken) {
			refuse()
			return Token{}, false
		}
		s.logger.Printf("mirror: authenticate: %v", err)
		http.Error(w, "mirror: could not check the token", http.StatusServiceUnavailable)
		return Token{}, false
	}
	return tok, true
}

// AuthenticateHost is the ASK HOOK's door (`muster-mirror ask`, internal/api's
// mirror_ask.go): the request's mirror token, checked exactly as
// /mirror/connect checks it, yielding the OWNER and HOST it was minted for.
// ok false: the refusal (401, 429 or 503) has been written.
//
// 🔴 A MIRROR TOKEN OPENS THIS AND /mirror/connect, AND NOTHING ELSE. The ask
// route registers a question under the token's own owner and host, and
// nowhere else: muster shows it only in a window that host lists as running
// that Claude session, and only to that owner.
func (s *Service) AuthenticateHost(w http.ResponseWriter, r *http.Request) (owner int64, host string, ok bool) {
	tok, ok := s.authenticate(w, r)
	if !ok {
		return 0, "", false
	}
	return tok.OwnerID, tok.Host, true
}

// Nudge tells owner's open views to re-fetch (the hub's own debounced nudge).
func (s *Service) Nudge(owner int64) { s.hub.changed(owner) }

// watchToken ends the connection once its token is revoked — or once it cannot
// be re-verified RecheckFailClosedAfter times in a row (fail closed). Ending the
// connection ends every terminal open on it (Hub.drop).
func (s *Service) watchToken(ctx context.Context, cancel context.CancelCauseFunc, tok Token) {
	sys := authz.System(ctx)
	t := time.NewTicker(s.recheck)
	defer t.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			live, err := s.store.TokenLive(sys, tok.ID)
			if err != nil {
				failures++
				s.logger.Printf("mirror: recheck token %d (%s) failed (%d/%d): %v", tok.ID, tok.Host, failures, RecheckFailClosedAfter, err)
				if failures >= RecheckFailClosedAfter {
					cancel(fmt.Errorf("mirror: token %d for host %s could not be re-verified %d times in a row; closing (fail closed)", tok.ID, tok.Host, failures))
					return
				}
				continue
			}
			failures = 0
			if !live {
				cancel(fmt.Errorf("%w (token %d, host %s)", ErrTokenRevoked, tok.ID, tok.Host))
				return
			}
		}
	}
}

// owner is the viewing principal's OWN user, or ErrNoPrincipal: the
// own-only methods' read (Hosts, the token and hold reads). A target owner on
// ctx (ForOwner) is not read here.
//
// 🔴 authz.System IS NOT A VIEWER. Its UserID is 0, and a host's sessions belong
// to a user; "everyone's" is not a view this plugin offers.
func owner(ctx context.Context) (int64, error) {
	p, ok := authz.From(ctx)
	if !ok || p.All() || p.UserID <= 0 {
		return 0, ErrNoPrincipal
	}
	return p.UserID, nil
}

// targetKey carries the owner a request's mirror calls are about (ForOwner).
type targetKey struct{}

// ForOwner is ctx with its mirror calls aimed at owner's hub entries rather
// than the principal's own. 🔴 IT GRANTS NOTHING: every owner-taking method
// still decides through access, which answers owner only when the principal
// IS owner or holds the method's role in owner's team (Principal.TeamScope).
// internal/api sets it in one place, its mirror route gate (mirrorTeamGate),
// for the routes mirrorTeamRoutes lets a member onto; the gate's caller
// ledger (TestOnlyTheMirrorTeamGateAimsAMirrorCall) pins that.
func ForOwner(ctx context.Context, owner int64) context.Context {
	return context.WithValue(ctx, targetKey{}, owner)
}

// TargetOwner is the owner ctx's mirror calls are aimed at (ForOwner), 0 for
// none: the principal's own.
func TargetOwner(ctx context.Context) int64 {
	o, _ := ctx.Value(targetKey{}).(int64)
	return o
}

// teamNeedsOpen is the roles a member's request may act on a TEAM host's
// entries with, in this release. Team sharing 2b.3 opened viewing (a member
// watches, reads and scrolls a team session); 2b.4 opens operating — typing
// into one: an interactive attach, a paste, an answer typed into the pane —
// at operator and above, each still behind its own fresh sign-in (Attach's
// and the paste's step-up, checked here as well as by internal/api).
//
// 🔴 A ROLE IS REACHED ONLY THROUGH A ROUTE THAT AIMS AT IT: internal/api's
// route gate (mirrorTeamGate) is ForOwner's one caller, and it aims a
// request at a team only for the routes its table lets a member onto, at the
// role each needs. So opening operator here opens the typing routes it
// lists, and no other operator method (a spawn, a close) — no route aims one
// at a team. Manager is not opened: no mirror method needs it.
var teamNeedsOpen = map[authz.TeamRole]bool{authz.TeamViewer: true, authz.TeamOperator: true}

// access is THE ONE CHOKEPOINT (team sharing 2b.3; docs/design/team-sharing.md
// A, "The chokepoint") deciding whose hub entries an owner-taking method acts
// on: the request's target (TargetOwner; none = the principal's own) when the
// principal IS that owner, or holds need in it through its team memberships
// (Principal.TeamScope: the browser session and a paired phone only), and need
// is one teamNeedsOpen opens. Anything else is ErrNoPrincipal — the error a
// missing session gets — so a refusal is no oracle for whether the team host
// exists. The team acting principal (UserID = the team) is its own owner.
// serviceNeeds (TestEveryServiceMethodSaysItsNeed) is each method's need.
func access(ctx context.Context, need authz.TeamRole) (int64, error) {
	own, err := owner(ctx)
	if err != nil {
		return 0, err
	}
	t := TargetOwner(ctx)
	if t <= 0 || t == own {
		return own, nil
	}
	p, _ := authz.From(ctx)
	role, in := p.TeamScope().RoleIn(t)
	if !in || !role.AtLeast(need) || !teamNeedsOpen[need] {
		return 0, ErrNoPrincipal
	}
	return t, nil
}

// Hosts is the viewing principal's OWN hosts: connected, or recently
// disconnected. Another user's hosts are never in it, and nor are its teams'.
//
// 🔴 IT STAYS OWN-ONLY (team sharing 2b.3). About fifty callers pick an entry
// from it by host NAME alone; with a team's build-01 listed beside the
// principal's own build-01 each would take whichever came first. The
// team-aware listing is VisibleHosts, for a ledgered set of callers.
//
// Each entry's Owner is 0, "the principal's own" (#690 round 1, 🟢3): an own
// entry's URLs carry no owner= (ui.MirrorRef), as before teams. Only
// VisibleHosts names owners.
func (s *Service) Hosts(ctx context.Context) []HostView {
	o, err := owner(ctx)
	if err != nil {
		return nil
	}
	hs := s.hub.Hosts(o)
	for i := range hs {
		hs[i].Owner = 0
	}
	return hs
}

// VisibleHosts is the principal's own hosts, then each of its teams' hosts
// where it is at least a viewer (Principal.TeamScope, read with the request:
// a person's browser session or phone; every other door carries no team), team
// by team in id order. Each HostView carries its Owner, and a caller acts on
// an entry it picks by that owner (ForOwner, then access) — never by its name
// alone. Its callers are a ledger (internal/api visibleHostsCallers,
// TestEveryVisibleHostsCallerIsClassified).
func (s *Service) VisibleHosts(ctx context.Context) []HostView {
	o, err := owner(ctx)
	if err != nil {
		return nil
	}
	out := s.hub.Hosts(o)
	p, _ := authz.From(ctx)
	for _, m := range p.TeamScope().List() {
		if m.Team == o || !m.Role.AtLeast(authz.TeamViewer) {
			continue
		}
		out = append(out, s.hub.Hosts(m.Team)...)
	}
	return out
}

// Attach attaches a terminal to one of the viewing principal's host sessions.
// See Hub.Attach.
//
// 🔴 AN INTERACTIVE ATTACH NEEDS A FRESH SIGN-IN (internal/stepup), checked
// HERE as well as by internal/api before its socket upgrade: the plugin does
// not rely on its caller to have asked. A context with no sign-in time is never
// fresh, so a caller that forgot to carry one gets read-only or nothing.
func (s *Service) Attach(ctx context.Context, host, session string, readOnly bool, stdin io.Reader, stdout io.Writer, resize <-chan provision.TermSize) error {
	return s.AttachWindow(ctx, host, session, "", readOnly, stdin, stdout, resize)
}

// AttachWindow is Attach on one window of the session (its tmux window id);
// window == "" is the session's active window. See Hub.AttachWindow.
func (s *Service) AttachWindow(ctx context.Context, host, session, window string, readOnly bool, stdin io.Reader, stdout io.Writer, resize <-chan provision.TermSize) error {
	need := authz.TeamOperator // live keys type into the pane
	if readOnly {
		need = authz.TeamViewer
	}
	o, err := access(ctx, need)
	if err != nil {
		return err
	}
	if !readOnly && !stepup.Fresh(ctx, s.hub.now()) {
		return ErrStepUpRequired
	}
	return s.hub.AttachWindow(ctx, o, host, session, window, readOnly, stdin, stdout, resize)
}

// Watch streams one of the viewing principal's host sessions' Claude Code
// transcript. See Hub.Watch.
func (s *Service) Watch(ctx context.Context, host, session, gen string, offset int64, out func(Msg) error) error {
	return s.WatchWindow(ctx, host, session, "", gen, offset, out)
}

// WatchWindow is Watch on one window of the session; window == "" is the
// session's active window. See Hub.WatchWindow.
func (s *Service) WatchWindow(ctx context.Context, host, session, window, gen string, offset int64, out func(Msg) error) error {
	o, err := access(ctx, authz.TeamViewer)
	if err != nil {
		return err
	}
	return s.hub.WatchWindow(ctx, o, host, session, window, gen, offset, out)
}

// Paste types a message into a pane of one of the viewing principal's host
// sessions: its Claude pane, or its active pane (target). See Hub.Paste.
func (s *Service) Paste(ctx context.Context, host, session, text string, target PasteTarget) error {
	return s.PasteWindow(ctx, host, session, "", text, target)
}

// PasteWindow is Paste into one window of the session; window == "" is the
// session's active window. See Hub.PasteWindow.
func (s *Service) PasteWindow(ctx context.Context, host, session, window, text string, target PasteTarget, opts ...PasteOption) error {
	o, err := access(ctx, authz.TeamOperator)
	if err != nil {
		return err
	}
	return s.hub.PasteWindow(ctx, o, host, session, window, text, target, opts...)
}

// ScrollWindow scrolls the active pane of one window of one of the viewing
// principal's host sessions through its history (tmux copy-mode); window == ""
// is the session's active window. See Hub.ScrollWindow — and why it needs no
// fresh sign-in.
func (s *Service) ScrollWindow(ctx context.Context, host, session, window string, lines int) error {
	o, err := access(ctx, authz.TeamViewer)
	if err != nil {
		return err
	}
	return s.hub.ScrollWindow(ctx, o, host, session, window, lines)
}

// CatalogWindow is the composer menu's catalog of one window of one of the
// viewing principal's host sessions (window == "" is the session's active
// window): its commands and skills, as the host reports them. See Hub.CatalogWindow. No step-up: it reads names and descriptions,
// and types nothing.
func (s *Service) CatalogWindow(ctx context.Context, host, session, window string) (Catalog, error) {
	o, err := access(ctx, authz.TeamOperator)
	if err != nil {
		return Catalog{}, err
	}
	return s.hub.CatalogWindow(ctx, o, host, session, window)
}

// IsAttachRefusal reports whether err is one of the plugin's own refusals,
// whose text is written for the operator and is passed to the browser as is.
func IsAttachRefusal(err error) bool {
	for _, e := range []error{ErrHostOffline, ErrSessionGone, ErrWindowGone, ErrHostDisconnected, ErrOverrun,
		ErrInteractiveRefused, ErrTooManyStreams, ErrNoPrincipal, ErrStepUpRequired,
		ErrNoRenderedView, ErrNotClaude, ErrViewOverrun, ErrTooManyWatches, ErrPasteTimeout, ErrPasteRefused, ErrBadPaste,
		ErrPanePasteUnsupported, ErrScrollUnsupported, ErrScrollRefused, ErrScrollPaneInMode, ErrScrollTimeout, ErrBadScroll,
		ErrCatalogUnsupported, ErrCatalogRefused, ErrCatalogTimeout} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// failLimiter counts failed token checks per client address in fixed windows.
type failLimiter struct {
	limit  int
	window time.Duration
	now    func() time.Time

	mu sync.Mutex
	by map[string]failWindow
}

type failWindow struct {
	start time.Time
	n     int
}

func newFailLimiter(limit int, window time.Duration) *failLimiter {
	return &failLimiter{limit: limit, window: window, now: time.Now, by: map[string]failWindow{}}
}

func (l *failLimiter) blocked(addr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	w, ok := l.by[addr]
	if !ok {
		return false
	}
	if l.now().Sub(w.start) >= l.window {
		delete(l.by, addr)
		return false
	}
	return w.n >= l.limit
}

func (l *failLimiter) fail(addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.by) > 4096 { // bound the map: drop expired windows
		for a, w := range l.by {
			if now.Sub(w.start) >= l.window {
				delete(l.by, a)
			}
		}
	}
	w, ok := l.by[addr]
	if !ok || now.Sub(w.start) >= l.window {
		w = failWindow{start: now}
	}
	w.n++
	l.by[addr] = w
}
