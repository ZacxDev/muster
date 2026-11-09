package previewtunnel

// Remote mode: the same harness, with the gateway deployed behind the REAL edge
// (Cloudflare -> prod Traefik -> two nginx relays -> a pod) and the host + client
// running together in one process elsewhere. Throwaway, like the rest of the spike.
//
// The gateway has no edge auth in front of it, so EVERY request — the host's
// tunnel and every client request alike — must carry SecretHeader with the
// shared secret, or it gets an empty 401 before anything else happens. There is
// no unauthenticated endpoint (probe it with a TCP check).
//
// The host serves ONLY its own in-process test upstream: its allowlist holds that
// one loopback port, and Host.dial refuses every other port with 403. The port the
// host announces (X-Spike-Port) is just what the gateway asks for; the host is the
// side that enforces it.

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	SecretHeader  = "X-Spike-Secret"
	PortHeader    = "X-Spike-Port"
	NoPingsHeader = "X-Spike-No-Pings" // "1": negative control, the gateway sends no pings on this connection
)

// RemoteGateway is the deployable gateway. One host tunnel at a time; a new
// tunnel replaces the old one.
type RemoteGateway struct {
	Secret string
	Ping   time.Duration
	Log    *log.Logger

	mu   sync.Mutex
	tun  Tunnel
	port int
	upTr *http.Transport
	rp   *httputil.ReverseProxy
	once sync.Once
}

func (g *RemoteGateway) init() {
	g.once.Do(func() {
		g.upTr = &http.Transport{DialContext: g.dial, MaxIdleConnsPerHost: 100, DisableCompression: true}
		target, _ := url.Parse("http://tunnel")
		g.rp = httputil.NewSingleHostReverseProxy(target)
		g.rp.Transport = g.upTr
		if g.Log == nil {
			g.Log = log.Default()
		}
	})
}

func (g *RemoteGateway) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	g.mu.Lock()
	t, port := g.tun, g.port
	g.mu.Unlock()
	if t == nil {
		return nil, errors.New("no host tunnel")
	}
	return t.Open(ctx, port)
}

func (g *RemoteGateway) authed(r *http.Request) bool {
	got := r.Header.Get(SecretHeader)
	return len(g.Secret) >= 32 && subtle.ConstantTimeCompare([]byte(got), []byte(g.Secret)) == 1
}

func (g *RemoteGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.init()
	if !g.authed(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	r.Header.Del(SecretHeader)
	noPings := r.Header.Get(NoPingsHeader) == "1"
	switch r.URL.Path {
	case "/v1/host":
		port, err := strconv.Atoi(r.Header.Get(PortHeader))
		if err != nil || port <= 0 || port > 65535 {
			http.Error(w, "bad port", http.StatusBadRequest)
			return
		}
		t, err := AcceptTunnel(w, r, Faults{NoPings: noPings}, g.Ping)
		if err != nil {
			g.Log.Printf("tunnel accept: %v", err)
			return
		}
		g.mu.Lock()
		old := g.tun
		g.tun, g.port = t, port
		g.mu.Unlock()
		if old != nil {
			old.Close()
		}
		g.upTr.CloseIdleConnections()
		g.Log.Printf("tunnel up transport=%s noPings=%v", r.Header.Get("X-Spike-Transport"), noPings)
	case "/__bare_echo":
		t0 := time.Now()
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if !noPings {
			go pingLoop(ctx, c, g.Ping)
		}
		err = echoErr(ctx, c)
		g.Log.Printf("bare_echo ended after %v noPings=%v: %v", time.Since(t0).Round(time.Millisecond), noPings, err)
	default:
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			g.relayWS(w, r, noPings)
			return
		}
		g.rp.ServeHTTP(w, r)
	}
}

func echoErr(ctx context.Context, c *websocket.Conn) error {
	for {
		t, p, err := c.Read(ctx)
		if err != nil {
			return err
		}
		if err := c.Write(ctx, t, p); err != nil {
			return err
		}
	}
}

func (g *RemoteGateway) relayWS(w http.ResponseWriter, r *http.Request, noPings bool) {
	t0 := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	up, _, err := websocket.Dial(ctx, "ws://tunnel"+r.URL.Path, &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: &http.Transport{DialContext: g.dial}},
	})
	if err != nil {
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer up.CloseNow()
	br, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer br.CloseNow()
	if !noPings {
		go pingLoop(ctx, br, g.Ping)
	}
	ended := make(chan string, 2)
	cp := func(name string, dst, src *websocket.Conn) {
		defer cancel()
		for {
			t, p, err := src.Read(ctx)
			if err != nil {
				ended <- fmt.Sprintf("%s read: %v", name, err)
				return
			}
			if err := dst.Write(ctx, t, p); err != nil {
				ended <- fmt.Sprintf("%s write: %v", name, err)
				return
			}
		}
	}
	go cp("browser->upstream", up, br)
	cp("upstream->browser", br, up)
	g.Log.Printf("ws relay %s ended after %v noPings=%v: %s", r.URL.Path, time.Since(t0).Round(time.Millisecond), noPings, <-ended)
}

// RemoteOpts configures NewRemoteEnv.
type RemoteOpts struct {
	Base          string // https://tunnel-spike.example
	Secret        string
	Transport     Transport
	Ping          time.Duration
	HostNoPings   bool // host sends no tunnel pings and asks the gateway to send none either
	ClientNoPings bool // ask the gateway not to ping the client's WebSocket legs
	Gzip          bool
	HostNoFlush   bool // negative control for C5: the host flushes only the response head
}

// NewRemoteEnv starts the test upstream and a host in THIS process, dials the
// remote gateway's tunnel endpoint, and returns an Env whose checks run against
// the remote Base. GW/EdgeTun/EdgePrev are nil.
func NewRemoteEnv(o RemoteOpts) (*Env, error) {
	if o.Ping == 0 {
		o.Ping = 30 * time.Second
	}
	up, err := StartUpstream()
	if err != nil {
		return nil, err
	}
	hh := http.Header{SecretHeader: {o.Secret}, PortHeader: {strconv.Itoa(up.Port)}}
	if o.HostNoPings {
		hh.Set(NoPingsHeader, "1")
	}
	h := &Host{Transport: o.Transport, Allow: map[int]bool{up.Port: true},
		Faults: Faults{NoPings: o.HostNoPings, HostNoFlush: o.HostNoFlush}, PingInterval: o.Ping, Header: hh}
	ch := http.Header{SecretHeader: {o.Secret}}
	if o.ClientNoPings {
		ch.Set(NoPingsHeader, "1")
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &Env{Transport: o.Transport, Up: up, Host: h, Base: strings.TrimRight(o.Base, "/"),
		HostDone: make(chan error, 1), cancel: cancel, Header: ch, Gzip: o.Gzip}
	go func() { e.HostDone <- h.Run(ctx, e.wsURL("/v1/host")) }()
	// The tunnel is up when a request through it succeeds.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-e.HostDone:
			e.Close()
			return nil, fmt.Errorf("host: %w", err)
		default:
		}
		rctx, rcancel := context.WithTimeout(ctx, 5*time.Second)
		resp, err := http1Client().Do(e.newReq(rctx, e.Base+"/blob?n=16&seed=1"))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				rcancel()
				return e, nil
			}
		}
		rcancel()
		time.Sleep(300 * time.Millisecond)
	}
	e.Close()
	return nil, errors.New("tunnel not usable within 15s")
}
