package previewgw

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ZacxDev/muster/internal/preview/tunnel"
	"github.com/ZacxDev/muster/internal/preview/wire"
)

// Periods of the gateway's two checks.
const (
	// RecheckEvery: every live tunnel's host token is checked against the
	// token file this often; a revoked one ends its tunnel at the next check.
	RecheckEvery = 30 * time.Second
	// SelfEvery: each plugin token's GET /plugin/v1/self, this often.
	SelfEvery = time.Minute
	// recheckFailLimit consecutive unreadable token files end every tunnel
	// (as muster's mirror connect does when it cannot verify a token twice).
	recheckFailLimit = 2
	// UnknownLimit consecutive /self checks without a verdict (muster
	// unreachable, a 5xx, a 404 because plugins are off) end a token's last
	// verdict: about ten minutes at one check a minute. An outage shorter
	// than that does not drop previews; a longer one stops serving an owner
	// muster can no longer vouch for. One later live verdict restores it.
	UnknownLimit = 10
)

// Listener timeouts (both listeners).
//
//   - ReadHeaderTimeout: a client has this long to send its request headers;
//     a slow-header client cannot hold a connection open for free.
//   - IdleTimeout: a keep-alive connection with no request in flight closes
//     after this long — the design's 5 min "HTTP connection idle" limit.
//
// No ReadTimeout or WriteTimeout: a host tunnel is one long-lived hijacked
// WebSocket, and so will be every streamed preview response; a whole-request
// deadline would cut them. Liveness of a tunnel is the 30 s ping's job.
const (
	listenReadHeaderTimeout = 10 * time.Second
	listenIdleTimeout       = 5 * time.Minute
)

// MusterAPI is the muster calls the gateway makes (MusterClient): /self per
// plugin token, and redeeming a hand-off code for the session page.
type MusterAPI interface {
	Self(ctx context.Context, token string) (Self, SelfVerdict, error)
	Redeem(ctx context.Context, token, code string) (Handoff, error)
}

// Options are the gateway's collaborators; zero values take real ones.
type Options struct {
	Clock    Clock
	Log      *slog.Logger
	Muster   MusterAPI // nil: NewMusterClient(cfg.MusterURL)
	DialWait time.Duration
	// Tests only: the ping interval each tunnel's client uses.
	PingInterval time.Duration
	// Tests only: the browser-leg WebSocket ping and the quiet-SSE comment
	// interval; 0 means StreamKeepalive.
	StreamKeepalive time.Duration
}

// tokenState is what the gateway last learned about one plugin token.
type tokenState struct {
	owner    int64 // from the last 200; 0 before any
	plugin   int64 // the token's plugin id, from the last 200; muster_plugin names it
	verdict  SelfVerdict
	unknowns int // consecutive checks without a verdict
}

// Gateway is the running preview gateway.
type Gateway struct {
	cfg   Config
	clock Clock
	log   *slog.Logger
	store HostTokens
	mc    MusterAPI
	sign  signer
	boot  string
	o     Options

	failMesh, failPublic *failLimiter
	ttl, idle            time.Duration
	keepalive            time.Duration
	proxy                http.Handler

	reg     *prometheus.Registry
	m       *gwMetrics
	maxBody int64 // PREVIEW_MAX_BODY

	ctx    context.Context
	cancel context.CancelFunc
	stops  []func()

	mu           sync.Mutex
	byKey        map[tunnelKey]*Tunnel
	all          map[*Tunnel]struct{}
	previews     map[string]*Preview
	previewBy    map[previewKey]string // (owner, host, window, port) → id
	codes        map[string]entryCode  // sha256(entry code) → what it admits (auth.go)
	tokens       []tokenState          // parallel to cfg.PluginTokens
	recheckFails int
}

// New builds a gateway; Start begins its checks.
func New(cfg Config, o Options) (*Gateway, error) {
	if o.Clock == nil {
		o.Clock = realClock{}
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.TTL <= 0 {
		cfg.TTL = DefaultTTL
	}
	if cfg.Idle <= 0 {
		cfg.Idle = DefaultIdle
	}
	if cfg.MaxBody <= 0 {
		cfg.MaxBody = DefaultMaxBody
	}
	if o.StreamKeepalive <= 0 {
		o.StreamKeepalive = StreamKeepalive
	}
	if o.Muster == nil {
		o.Muster = NewMusterClient(cfg.MusterURL, "muster-preview/"+Version)
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	reg := prometheus.NewRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	g := &Gateway{
		cfg: cfg, clock: o.Clock, log: o.Log, store: HostTokens{Dir: cfg.StateDir}, mc: o.Muster, sign: signer{key: cfg.SigningKey},
		boot: hex.EncodeToString(b), o: o,
		failMesh:   newFailLimiter(FailLimit, FailWindow, o.Clock.Now),
		failPublic: newFailLimiter(FailLimit, FailWindow, o.Clock.Now),
		ttl:        cfg.TTL, idle: cfg.Idle, keepalive: o.StreamKeepalive,
		reg:     reg,
		m:       newMetrics(reg),
		maxBody: cfg.MaxBody,
		ctx:     ctx, cancel: cancel,
		byKey:     map[tunnelKey]*Tunnel{},
		all:       map[*Tunnel]struct{}{},
		previews:  map[string]*Preview{},
		previewBy: map[previewKey]string{},
		codes:     map[string]entryCode{},
		tokens:    make([]tokenState, len(cfg.PluginTokens)),
	}
	g.proxy = g.newProxy()
	return g, nil
}

// Version is the gateway's version string.
const Version = "0.1.0"

// Boot is this process's boot id (wire.GatewayBootHeader).
func (g *Gateway) Boot() string { return g.boot }

// Registry is the gateway's own metrics registry.
func (g *Gateway) Registry() *prometheus.Registry { return g.reg }

// Start asks /self for every plugin token once, then starts the minute-by-
// minute /self check, the 30 s host-token recheck and the lifecycle sweep.
func (g *Gateway) Start(ctx context.Context) {
	g.CheckOwners(ctx)
	g.stops = append(g.stops,
		every(g.clock, SelfEvery, func() { g.CheckOwners(g.ctx) }),
		every(g.clock, RecheckEvery, g.Recheck),
		every(g.clock, LifecycleEvery, g.sweep))
}

// Close stops the checks and ends every tunnel.
func (g *Gateway) Close() {
	for _, s := range g.stops {
		s()
	}
	g.cancel()
	g.closeTunnelsWhere(func(*Tunnel) bool { return true })
}

// liveOwnerLocked: at least one plugin token of owner is live.
func (g *Gateway) liveOwnerLocked(owner int64) bool {
	for _, ts := range g.tokens {
		if ts.owner == owner && ts.verdict == SelfLive {
			return true
		}
	}
	return false
}

// CheckOwners asks /self for every plugin token, and closes the tunnels and
// previews of every owner that was served and no longer is: one whose token
// answered 401, or 200 without the viewer capability, with no other live
// token of that owner. A token muster did not answer for keeps its last
// verdict for UnknownLimit-1 checks; at the UnknownLimit-th in a row it is
// no longer live (its owner closes as above) until muster answers again.
func (g *Gateway) CheckOwners(ctx context.Context) {
	answers := make([]SelfAnswer, len(g.cfg.PluginTokens))
	for i, tok := range g.cfg.PluginTokens {
		s, v, err := g.mc.Self(ctx, tok)
		answers[i] = SelfAnswer{Self: s, Verdict: v, Err: err}
	}
	g.mu.Lock()
	before := map[int64]bool{}
	for _, ts := range g.tokens {
		if ts.verdict == SelfLive {
			before[ts.owner] = true
		}
	}
	for i, a := range answers {
		switch a.Verdict {
		case SelfUnknown:
			g.tokens[i].unknowns++
			n := g.tokens[i].unknowns
			reason := "no verdict"
			if a.Err != nil {
				reason = a.Err.Error()
			}
			if n >= UnknownLimit && g.tokens[i].verdict == SelfLive {
				g.tokens[i].verdict = SelfUnknown
				g.log.Warn("plugin token unverified for too long; its owner is no longer served", "token", i+1, "consecutive", n, "err", reason)
			} else if n < UnknownLimit {
				g.log.Warn("plugin token check failed; keeping its last verdict", "token", i+1, "consecutive", n, "err", reason)
			}
		case SelfLive:
			g.tokens[i] = tokenState{owner: a.Self.OwnerID, plugin: a.Self.PluginID, verdict: SelfLive}
		case SelfDead:
			if a.Self.OwnerID > 0 {
				g.tokens[i].owner = a.Self.OwnerID
				g.tokens[i].plugin = a.Self.PluginID
			}
			g.tokens[i].verdict = SelfDead
			g.tokens[i].unknowns = 0
		}
	}
	var gone []int64
	for owner := range before {
		if !g.liveOwnerLocked(owner) {
			gone = append(gone, owner)
		}
	}
	np := 0
	for _, owner := range gone {
		np += g.closePreviewsLocked(owner, "", closeOwnerGone)
	}
	g.mu.Unlock()
	for _, owner := range gone {
		ts := g.closeTunnelsWhere(func(t *Tunnel) bool { return t.key.owner == owner })
		g.log.Info("owner no longer served: closed its tunnels and previews", "owner", owner,
			"tunnels", len(ts), "previews", np)
	}
}

// Recheck re-reads the host-token file and ends every tunnel whose token is
// gone, or no longer names the tunnel's owner and host. It also closes the
// previews of every (owner, host) no stored token names any more — whether
// or not that host has a tunnel now: a host revoked while offline must not
// keep previews a new token for the same name would re-attach.
func (g *Gateway) Recheck() {
	toks, err := g.store.List()
	g.mu.Lock()
	if err != nil {
		g.recheckFails++
		n := g.recheckFails
		g.mu.Unlock()
		g.log.Error("host-token file unreadable", "err", err.Error(), "consecutive", n)
		if n >= recheckFailLimit {
			g.closeTunnelsWhere(func(*Tunnel) bool { return true })
		}
		return
	}
	g.recheckFails = 0
	g.mu.Unlock()
	ix := indexTokens(toks)
	valid := map[tunnelKey]bool{}
	for _, rec := range toks {
		valid[tunnelKey{rec.OwnerID, rec.Host}] = true
	}
	revoked := g.closeTunnelsWhere(func(t *Tunnel) bool {
		rec, ok := ix[t.tokenHash]
		return !ok || rec.Host != t.key.host || rec.OwnerID != t.key.owner
	})
	g.mu.Lock()
	n := 0
	for _, t := range revoked {
		n += g.closePreviewsLocked(t.key.owner, t.key.host, closeRevoked)
	}
	for _, p := range g.previews {
		if k := (tunnelKey{p.OwnerID, p.Host}); !valid[k] {
			n += g.closePreviewsLocked(k.owner, k.host, closeRevoked)
		}
	}
	g.mu.Unlock()
	if n > 0 {
		g.log.Info("closed the previews of hosts with no valid token", "previews", n)
	}
	for _, t := range revoked {
		g.log.Info("host token revoked: tunnel closed", "host", t.key.host, "path", string(t.path))
	}
}

// ---------------------------------------------------------------- serving

// PublicHandler serves the public listener (PREVIEW_LISTEN), by Host: the
// host tunnel on tunnel.<domain>, the session page on w.<domain>
// (session.go), each open preview on p-<id>.<domain> (auth.go), and 404 for
// everything else. It serves no /metrics or /healthz: those are on the mesh
// listener, which the edge never reaches.
func (g *Gateway) PublicHandler() http.Handler {
	host := g.hostTunnel(PathPublic)
	web := g.webHandler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch rh := requestHost(r); {
		case rh == "tunnel."+g.cfg.Domain && r.URL.Path == wire.HostPath:
			host.ServeHTTP(w, r)
		case rh == "w."+g.cfg.Domain:
			web.ServeHTTP(w, r)
		default:
			if id := g.previewIDOf(rh); id != "" {
				g.previewHandler(id).ServeHTTP(w, r)
				return
			}
			http.NotFound(w, r)
		}
	})
}

// MeshHandler serves the mesh listener (PREVIEW_MESH_LISTEN): the host tunnel,
// /metrics and /healthz, whatever the Host.
func (g *Gateway) MeshHandler() http.Handler {
	host := g.hostTunnel(PathMesh)
	metrics := promhttp.HandlerFor(g.reg, promhttp.HandlerOpts{})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case wire.HostPath:
			host.ServeHTTP(w, r)
		case "/metrics":
			metrics.ServeHTTP(w, r)
		case "/healthz":
			_, _ = w.Write([]byte("ok\n"))
		default:
			http.NotFound(w, r)
		}
	})
}

// requestHost is r's Host, lowercase, without port or trailing dots.
func requestHost(r *http.Request) string {
	h := r.Host
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	return strings.TrimRight(strings.ToLower(h), ".")
}

// hostTunnel accepts a host's tunnel on the listener of path p.
//
// 🔴 p IS FIXED BY WHICH LISTENER'S HANDLER THIS IS, never read from the
// request: a host could otherwise claim the mesh path from the public edge.
// Checks, in order: the version (refused 426 with the range spoken), the token
// (shape, then the file; a failure counts against the peer, 429 past the
// limit, never for a valid token), the host name it registers, and the owner
// being served.
func (g *Gateway) hostTunnel(p Path) http.Handler {
	lim := g.failMesh
	if p == PathPublic {
		lim = g.failPublic
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "the host tunnel is a WebSocket GET", http.StatusMethodNotAllowed)
			return
		}
		if _, ok := wire.AcceptVersion(r.Header.Get(wire.VersionHeader)); !ok {
			w.Header().Set(wire.VersionRangeHeader, wire.VersionRange())
			http.Error(w, fmt.Sprintf("this gateway speaks preview protocol versions %s; got %q",
				wire.VersionRange(), r.Header.Get(wire.VersionHeader)), http.StatusUpgradeRequired)
			return
		}
		refuse := func() {
			if lim.fail(peerOf(p, r)) {
				http.Error(w, "too many failed host-token checks from this address; wait a minute", http.StatusTooManyRequests)
				return
			}
			http.Error(w, "not a valid host token", http.StatusUnauthorized)
		}
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !TokenShapeOK(tok) {
			refuse()
			return
		}
		toks, err := g.store.List()
		if err != nil {
			g.log.Error("host-token file unreadable", "err", err.Error())
			http.Error(w, "the gateway cannot check host tokens right now", http.StatusServiceUnavailable)
			return
		}
		rec, ok := indexTokens(toks)[HashToken(tok)]
		if !ok {
			refuse()
			return
		}
		if name := r.Header.Get(wire.HostNameHeader); name != rec.Host {
			http.Error(w, fmt.Sprintf("this host token registers the host %q only", rec.Host), http.StatusForbidden)
			return
		}
		g.mu.Lock()
		live := g.liveOwnerLocked(rec.OwnerID)
		g.mu.Unlock()
		if !live {
			http.Error(w, errOwnerNotServed.Error(), http.StatusForbidden)
			return
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		cl, err := tunnel.NewClient(g.ctx, ws, tunnel.ClientOptions{DialWait: g.o.DialWait, PingInterval: g.o.PingInterval})
		if err != nil {
			g.log.Error("tunnel start failed", "host", rec.Host, "err", err.Error())
			return
		}
		t := &Tunnel{key: tunnelKey{rec.OwnerID, rec.Host}, tokenHash: rec.Hash, path: p, client: cl, ctx: g.ctx, m: g.m,
			answers: make(chan pendingAnswer, answerQueue)}
		t.rt = newUpstreamTransport(t)
		if err := g.register(t); err != nil {
			_ = cl.Close()
			return
		}
		g.log.Info("tunnel up", "host", rec.Host, "path", string(p))
		go g.consumeOffers(t)
		<-cl.Done()
		g.unregister(t)
		g.log.Info("tunnel down", "host", rec.Host, "path", string(p), "err", errString(cl.Err()))
	})
}

// servers builds the listeners' http.Servers: the public one, and the mesh
// one when configured, with the listener timeouts above.
func (g *Gateway) servers() []*http.Server {
	mk := func(addr string, h http.Handler) *http.Server {
		return &http.Server{Addr: addr, Handler: h,
			ReadHeaderTimeout: listenReadHeaderTimeout, IdleTimeout: listenIdleTimeout}
	}
	out := []*http.Server{mk(g.cfg.Listen, g.PublicHandler())}
	if g.cfg.MeshListen != "" {
		out = append(out, mk(g.cfg.MeshListen, g.MeshHandler()))
	}
	return out
}

// Run serves the public listener and, when configured, the mesh listener
// until ctx ends.
func (g *Gateway) Run(ctx context.Context) error {
	g.Start(ctx)
	defer g.Close()
	type srv struct {
		s    *http.Server
		addr string
	}
	var servers []srv
	for _, s := range g.servers() {
		servers = append(servers, srv{s, s.Addr})
	}
	errc := make(chan error, len(servers))
	for _, s := range servers {
		ln, err := net.Listen("tcp", s.addr)
		if err != nil {
			for _, o := range servers {
				_ = o.s.Close()
			}
			return err
		}
		go func() { errc <- s.s.Serve(ln) }()
		g.log.Info("listening", "addr", ln.Addr().String())
	}
	var err error
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.s.Shutdown(sctx)
	}
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}
