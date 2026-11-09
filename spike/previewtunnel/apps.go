package previewtunnel

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// ------------------------------------------------------------ upstream app

// Blob returns a deterministic pseudo-random stream of n bytes for seed.
func Blob(seed uint64, n int64) io.Reader {
	var key [16]byte
	binary.BigEndian.PutUint64(key[:], seed)
	blk, _ := aes.NewCipher(key[:])
	zero := zeroReader{}
	return io.LimitReader(cipher.StreamReader{S: cipher.NewCTR(blk, make([]byte, 16)), R: zero}, n)
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// BlobSHA is the expected sha256 of Blob(seed, n).
func BlobSHA(seed uint64, n int64) [32]byte {
	h := sha256.New()
	_, _ = io.Copy(h, Blob(seed, n))
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// Upstream is the loopback "dev server" the host exposes.
type Upstream struct {
	Addr string
	Port int
	srv  *http.Server
	// SSESent receives the wall time each SSE event was written+flushed.
	SSESent chan time.Time
}

func StartUpstream() (*Upstream, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	u := &Upstream{Addr: ln.Addr().String(), Port: ln.Addr().(*net.TCPAddr).Port, SSESent: make(chan time.Time, 64)}
	mux := http.NewServeMux()
	mux.HandleFunc("/blob", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.ParseInt(r.URL.Query().Get("n"), 10, 64)
		seed, _ := strconv.ParseUint(r.URL.Query().Get("seed"), 10, 64)
		if d, _ := time.ParseDuration(r.URL.Query().Get("delay")); d > 0 {
			time.Sleep(d) // lets a page's assets overlap, so they are concurrent streams
		}
		w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.Copy(w, Blob(seed, n))
	})
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		time.Sleep(200 * time.Millisecond) // the event is written well after the headers
		fmt.Fprintf(w, "data: hello\n\n")
		w.(http.Flusher).Flush()
		select {
		case u.SSESent <- time.Now():
		default:
		}
		hold, _ := time.ParseDuration(r.URL.Query().Get("hold"))
		select { // hold the stream open, as an SSE server does
		case <-time.After(hold):
		case <-r.Context().Done():
		}
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		echo(r.Context(), c)
	})
	u.srv = &http.Server{Handler: mux}
	go u.srv.Serve(ln)
	return u, nil
}

func (u *Upstream) Close() { u.srv.Close() }

func echo(ctx context.Context, c *websocket.Conn) {
	for {
		t, p, err := c.Read(ctx)
		if err != nil {
			return
		}
		if c.Write(ctx, t, p) != nil {
			return
		}
	}
}

// ------------------------------------------------------------------ gateway

// Gateway accepts one host tunnel and serves previews of one upstream port.
type Gateway struct {
	Faults       Faults
	PingInterval time.Duration
	Port         int // upstream port every preview request maps to

	mu       sync.Mutex
	tun      Tunnel
	ready    chan struct{}
	tunSrv   *http.Server
	prevSrv  *http.Server
	TunAddr  string
	PrevAddr string
}

func (g *Gateway) Start() error {
	g.ready = make(chan struct{})
	tl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	pl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	g.TunAddr, g.PrevAddr = tl.Addr().String(), pl.Addr().String()
	g.tunSrv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t, err := AcceptTunnel(w, r, g.Faults, g.PingInterval)
		if err != nil {
			return
		}
		g.mu.Lock()
		g.tun = t
		g.mu.Unlock()
		close(g.ready)
	})}
	go g.tunSrv.Serve(tl)

	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-g.ready
		g.mu.Lock()
		t := g.tun
		g.mu.Unlock()
		return t.Open(ctx, g.Port)
	}
	upTr := &http.Transport{DialContext: dial, MaxIdleConnsPerHost: 100, DisableCompression: true}
	target, _ := url.Parse("http://127.0.0.1:" + strconv.Itoa(g.Port))
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.Transport = upTr
	wsClient := &http.Client{Transport: &http.Transport{DialContext: dial}}

	mux := http.NewServeMux()
	// Baseline: a bare WebSocket echo served by the gateway itself.
	mux.HandleFunc("/__bare_echo", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		echo(r.Context(), c)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "websocket" {
			g.relayWS(w, r, wsClient)
			return
		}
		rp.ServeHTTP(w, r)
	})
	g.prevSrv = &http.Server{Handler: mux}
	go g.prevSrv.Serve(pl)
	return nil
}

// relayWS terminates the browser leg and the upstream leg and copies message
// by message, pinging the browser leg (design: "Zone and deployment").
func (g *Gateway) relayWS(w http.ResponseWriter, r *http.Request, hc *http.Client) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	up, _, err := websocket.Dial(ctx, "ws://127.0.0.1:"+strconv.Itoa(g.Port)+r.URL.Path, &websocket.DialOptions{HTTPClient: hc})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer up.CloseNow()
	br, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer br.CloseNow()
	if !g.Faults.NoPings {
		go pingLoop(ctx, br, g.PingInterval)
	}
	cp := func(dst, src *websocket.Conn) {
		defer cancel()
		for {
			t, p, err := src.Read(ctx)
			if err != nil {
				return
			}
			if dst.Write(ctx, t, p) != nil {
				return
			}
		}
	}
	go cp(up, br)
	cp(br, up)
}

func (g *Gateway) Close() {
	g.tunSrv.Close()
	g.prevSrv.Close()
	g.mu.Lock()
	if g.tun != nil {
		g.tun.Close()
	}
	g.mu.Unlock()
}
