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
)

// SelfAsker is the one muster call the gateway makes (MusterClient.Self).
type SelfAsker interface {
	Self(ctx context.Context, token string) (Self, SelfVerdict, error)
}

// Options are the gateway's collaborators; zero values take real ones.
type Options struct {
	Clock    Clock
	Log      *slog.Logger
	Muster   SelfAsker // nil: NewMusterClient(cfg.MusterURL)
	DialWait time.Duration
	// Tests only: the ping interval each tunnel's client uses.
	PingInterval time.Duration
}

// tokenState is what the gateway last learned about one plugin token.
type tokenState struct {
	owner   int64 // from the last 200; 0 before any
	verdict SelfVerdict
}

// Gateway is the running preview gateway.
type Gateway struct {
	cfg   Config
	clock Clock
	log   *slog.Logger
	store HostTokens
	mc    SelfAsker
	boot  string
	o     Options

	failMesh, failPublic *failLimiter

	reg          *prometheus.Registry
	tunnelsGauge *prometheus.GaugeVec

	ctx    context.Context
	cancel context.CancelFunc
	stops  []func()

	mu           sync.Mutex
	byKey        map[tunnelKey]*Tunnel
	all          map[*Tunnel]struct{}
	previews     map[string]*Preview
	tokens       []tokenState // parallel to cfg.PluginTokens
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
	if o.Muster == nil {
		o.Muster = NewMusterClient(cfg.MusterURL, "muster-preview/"+Version)
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	reg := prometheus.NewRegistry()
	gauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "muster_preview_tunnels",
		Help: "Live host tunnels, draining ones included, by the listener they arrived on.",
	}, []string{"path"})
	reg.MustRegister(gauge)
	for _, p := range []Path{PathMesh, PathPublic} {
		gauge.WithLabelValues(string(p)) // both series exist from the start, at 0
	}
	ctx, cancel := context.WithCancel(context.Background())
	g := &Gateway{
		cfg: cfg, clock: o.Clock, log: o.Log, store: HostTokens{Dir: cfg.StateDir}, mc: o.Muster,
		boot: hex.EncodeToString(b), o: o,
		failMesh:     newFailLimiter(FailLimit, 0, FailWindow, o.Clock.Now),
		failPublic:   newFailLimiter(FailLimit, PublicFailLimit, FailWindow, o.Clock.Now),
		reg:          reg,
		tunnelsGauge: gauge,
		ctx:          ctx, cancel: cancel,
		byKey:    map[tunnelKey]*Tunnel{},
		all:      map[*Tunnel]struct{}{},
		previews: map[string]*Preview{},
		tokens:   make([]tokenState, len(cfg.PluginTokens)),
	}
	return g, nil
}

// Version is the gateway's version string.
const Version = "0.1.0"

// Boot is this process's boot id (wire.GatewayBootHeader).
func (g *Gateway) Boot() string { return g.boot }

// Registry is the gateway's own metrics registry.
func (g *Gateway) Registry() *prometheus.Registry { return g.reg }

// Start asks /self for every plugin token once, then starts the minute-by-
// minute /self check and the 30 s host-token recheck.
func (g *Gateway) Start(ctx context.Context) {
	g.CheckOwners(ctx)
	g.stops = append(g.stops,
		every(g.clock, SelfEvery, func() { g.CheckOwners(g.ctx) }),
		every(g.clock, RecheckEvery, g.Recheck))
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
// verdict.
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
			if a.Err != nil {
				g.log.Warn("plugin token check failed; keeping its last verdict", "token", i+1, "err", a.Err.Error())
			}
			continue
		case SelfLive:
			g.tokens[i] = tokenState{owner: a.Self.OwnerID, verdict: SelfLive}
		case SelfDead:
			if a.Self.OwnerID > 0 {
				g.tokens[i].owner = a.Self.OwnerID
			}
			g.tokens[i].verdict = SelfDead
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
		np += g.closePreviewsLocked(owner, "")
	}
	g.mu.Unlock()
	for _, owner := range gone {
		ts := g.closeTunnelsWhere(func(t *Tunnel) bool { return t.key.owner == owner })
		g.log.Info("owner no longer served: closed its tunnels and previews", "owner", owner,
			"tunnels", len(ts), "previews", np)
	}
}

// Recheck re-reads the host-token file and ends every tunnel whose token is
// gone, or no longer names the tunnel's owner and host; the previews of a
// revoked host close with it.
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
	revoked := g.closeTunnelsWhere(func(t *Tunnel) bool {
		rec, ok := ix[t.tokenHash]
		return !ok || rec.Host != t.key.host || rec.OwnerID != t.key.owner
	})
	if len(revoked) == 0 {
		return
	}
	g.mu.Lock()
	for _, t := range revoked {
		g.closePreviewsLocked(t.key.owner, t.key.host)
	}
	g.mu.Unlock()
	for _, t := range revoked {
		g.log.Info("host token revoked: tunnel closed", "host", t.key.host, "path", string(t.path))
	}
}

// ---------------------------------------------------------------- serving

// PublicHandler serves the public listener (PREVIEW_LISTEN): the host tunnel
// on tunnel.<domain>; every other name in the zone is 404 for now (the
// session page and the previews arrive with the proxy). A request whose Host
// is not in the zone at all (an in-cluster scrape) may read /metrics and
// /healthz; the edge only ever forwards names in the zone.
func (g *Gateway) PublicHandler() http.Handler {
	metrics := promhttp.HandlerFor(g.reg, promhttp.HandlerOpts{})
	host := g.hostTunnel(PathPublic)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := requestHost(r)
		switch {
		case name == "tunnel."+g.cfg.Domain:
			if r.URL.Path == wire.HostPath {
				host.ServeHTTP(w, r)
				return
			}
		case name == g.cfg.Domain || strings.HasSuffix(name, "."+g.cfg.Domain):
		case r.URL.Path == "/metrics":
			metrics.ServeHTTP(w, r)
			return
		case r.URL.Path == "/healthz":
			_, _ = w.Write([]byte("ok\n"))
			return
		}
		http.NotFound(w, r)
	})
}

// MeshHandler serves the mesh listener (PREVIEW_MESH_LISTEN): the host tunnel
// only, whatever the Host.
func (g *Gateway) MeshHandler() http.Handler {
	host := g.hostTunnel(PathMesh)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case wire.HostPath:
			host.ServeHTTP(w, r)
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
		t := &Tunnel{key: tunnelKey{rec.OwnerID, rec.Host}, tokenHash: rec.Hash, path: p, client: cl, ctx: g.ctx}
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

// Run serves the public listener and, when configured, the mesh listener
// until ctx ends.
func (g *Gateway) Run(ctx context.Context) error {
	g.Start(ctx)
	defer g.Close()
	type srv struct {
		s    *http.Server
		addr string
	}
	servers := []srv{{&http.Server{Addr: g.cfg.Listen, Handler: g.PublicHandler(), ReadHeaderTimeout: 10 * time.Second}, g.cfg.Listen}}
	if g.cfg.MeshListen != "" {
		servers = append(servers, srv{&http.Server{Addr: g.cfg.MeshListen, Handler: g.MeshHandler(), ReadHeaderTimeout: 10 * time.Second}, g.cfg.MeshListen})
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
