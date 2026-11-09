package previewtunnel

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Env is one full path: client → EdgeSim → gateway → tunnel (WS through
// EdgeSim) → host → loopback upstream.
type Env struct {
	Transport Transport
	Up        *Upstream
	GW        *Gateway
	Host      *Host
	EdgeTun   *EdgeSim
	EdgePrev  *EdgeSim
	Base      string     // http://<edgePrev>
	HostDone  chan error // receives when the host's Run returns
	cancel    func()
	// Header is sent on every client request (remote mode: the gateway secret).
	Header http.Header
	// Gzip makes CheckC5's client ask for compression the way a browser does.
	Gzip bool
}

// http1Client is an HTTP/1.1-only client (a bare Transport never negotiates h2).
func http1Client() *http.Client { return &http.Client{Transport: &http.Transport{}} }

func (e *Env) dialOpts() *websocket.DialOptions {
	return &websocket.DialOptions{HTTPHeader: e.Header.Clone(), HTTPClient: http1Client()}
}

func (e *Env) newReq(ctx context.Context, url string) *http.Request {
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	for k, v := range e.Header {
		req.Header[k] = v
	}
	return req
}

type EnvOpts struct {
	Transport Transport
	Faults    Faults
	EdgeIdle  time.Duration // 0 = edge never kills idle conns
	Ping      time.Duration
}

func NewEnv(o EnvOpts) (*Env, error) {
	if o.Ping == 0 {
		o.Ping = 30 * time.Second
	}
	up, err := StartUpstream()
	if err != nil {
		return nil, err
	}
	gw := &Gateway{Faults: o.Faults, PingInterval: o.Ping, Port: up.Port}
	if err := gw.Start(); err != nil {
		return nil, err
	}
	et := &EdgeSim{Target: gw.TunAddr, Idle: o.EdgeIdle}
	eta, err := et.Start()
	if err != nil {
		return nil, err
	}
	ep := &EdgeSim{Target: gw.PrevAddr, Idle: o.EdgeIdle}
	epa, err := ep.Start()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &Host{Transport: o.Transport, Allow: map[int]bool{up.Port: true}, Faults: o.Faults, PingInterval: o.Ping}
	e := &Env{Transport: o.Transport, Up: up, GW: gw, Host: h, EdgeTun: et, EdgePrev: ep,
		Base: "http://" + epa, HostDone: make(chan error, 1), cancel: cancel}
	go func() { e.HostDone <- h.Run(ctx, "ws://"+eta+"/v1/host") }()
	select {
	case <-gw.ready:
	case err := <-e.HostDone:
		cancel()
		return nil, fmt.Errorf("host: %w", err)
	case <-time.After(5 * time.Second):
		cancel()
		return nil, errors.New("tunnel not up in 5s")
	}
	return e, nil
}

func (e *Env) Close() {
	e.cancel()
	if e.GW != nil { // nil in remote mode: the gateway runs elsewhere
		e.GW.Close()
		e.EdgeTun.Close()
		e.EdgePrev.Close()
	}
	e.Up.Close()
}

func (e *Env) wsURL(path string) string { return "ws" + strings.TrimPrefix(e.Base, "http") + path }

func pct(d []time.Duration, p float64) time.Duration {
	s := slices.Clone(d)
	slices.Sort(s)
	i := int(float64(len(s)-1) * p)
	return s[i]
}

// EchoRTTs sends n 64-byte messages one at a time and records each round trip.
func EchoRTTs(ctx context.Context, url string, n int, opts *websocket.DialOptions) ([]time.Duration, error) {
	c, _, err := websocket.Dial(ctx, url, opts)
	if err != nil {
		return nil, err
	}
	defer c.CloseNow()
	msg := bytes.Repeat([]byte("h"), 64)
	out := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		t0 := time.Now()
		if err := c.Write(ctx, websocket.MessageText, msg); err != nil {
			return out, err
		}
		_, p, err := c.Read(ctx)
		if err != nil {
			return out, err
		}
		if !bytes.Equal(p, msg) {
			return out, errors.New("echo mismatch")
		}
		out = append(out, time.Since(t0))
		time.Sleep(2 * time.Millisecond) // interactive, not a flood
	}
	return out, nil
}

// ---------------------------------------------------------------- criterion 1

type C1 struct {
	BareP50, BareP95, TunP50, TunP95, OverP50, OverP95 time.Duration
	N                                                  int
}

func (r C1) String() string {
	return fmt.Sprintf("n=%d bare p50=%v p95=%v | tunnel p50=%v p95=%v | overhead p50=%v p95=%v",
		r.N, r.BareP50, r.BareP95, r.TunP50, r.TunP95, r.OverP50, r.OverP95)
}

const (
	C1MaxP50 = 50 * time.Millisecond
	C1MaxP95 = 150 * time.Millisecond
)

func CheckC1(e *Env, n int) (C1, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	bare, err := EchoRTTs(ctx, e.wsURL("/__bare_echo"), n, e.dialOpts())
	if err != nil {
		return C1{}, fmt.Errorf("bare: %w", err)
	}
	tun, err := EchoRTTs(ctx, e.wsURL("/ws"), n, e.dialOpts())
	if err != nil {
		return C1{}, fmt.Errorf("tunnel: %w", err)
	}
	return c1From(bare, tun)
}

func c1From(bare, tun []time.Duration) (C1, error) {
	r := C1{N: len(tun), BareP50: pct(bare, .5), BareP95: pct(bare, .95), TunP50: pct(tun, .5), TunP95: pct(tun, .95)}
	r.OverP50, r.OverP95 = r.TunP50-r.BareP50, r.TunP95-r.BareP95
	if r.OverP50 > C1MaxP50 || r.OverP95 > C1MaxP95 {
		return r, fmt.Errorf("C1 FAIL: overhead p50 %v (max %v) p95 %v (max %v)", r.OverP50, C1MaxP50, r.OverP95, C1MaxP95)
	}
	return r, nil
}

// ---------------------------------------------------------------- criterion 2

type C2 struct {
	Bytes        int64
	ByteExact    bool
	PeakHeapGrow int64 // max live-heap growth (whole process) during the pause
	PeakHostHeld int64 // max bytes the host held between upstream and tunnel
	Bound        int64
	Elapsed      time.Duration
}

func (r C2) String() string {
	return fmt.Sprintf("bytes=%d exact=%v peakHeapGrow=%.2fMiB peakHostHeld=%.2fMiB bound(2×window)=%.0fMiB elapsed=%v",
		r.Bytes, r.ByteExact, mib(r.PeakHeapGrow), mib(r.PeakHostHeld), mib(r.Bound), r.Elapsed.Round(time.Millisecond))
}

func mib(n int64) float64 { return float64(n) / (1 << 20) }

func liveHeap() int64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapAlloc)
}

// PausedDownload starts GET /blob of size bytes, reads 1 MiB, and returns a
// handle whose Resume reads the rest and verifies sha256.
type PausedDownload struct {
	resp *http.Response
	h    interface {
		io.Writer
		Sum([]byte) []byte
	}
	n    int64
	size int64
	seed uint64
}

func StartPausedDownload(e *Env, size int64, seed uint64) (*PausedDownload, error) {
	cl := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	resp, err := cl.Do(e.newReq(context.Background(), fmt.Sprintf("%s/blob?n=%d&seed=%d", e.Base, size, seed)))
	if err != nil {
		return nil, err
	}
	d := &PausedDownload{resp: resp, h: sha256.New(), size: size, seed: seed}
	n, err := io.CopyN(d.h, resp.Body, 1<<20)
	d.n = n
	return d, err
}

func (d *PausedDownload) Resume() (bool, int64, error) {
	defer d.resp.Body.Close()
	n, err := io.Copy(d.h, d.resp.Body)
	d.n += n
	want := BlobSHA(d.seed, d.size)
	return bytes.Equal(d.h.Sum(nil), want[:]) && d.n == d.size, d.n, err
}

func CheckC2(e *Env, size int64, pause time.Duration) (C2, error) {
	r := C2{Bound: 2 * StreamWindow}
	t0 := time.Now()
	base := liveHeap()
	e.Host.Stats.PeakHeld.Store(0)
	d, err := StartPausedDownload(e, size, 7)
	if err != nil {
		return r, err
	}
	end := time.Now().Add(pause)
	for time.Now().Before(end) {
		time.Sleep(time.Second)
		r.PeakHeapGrow = max(r.PeakHeapGrow, liveHeap()-base)
	}
	ok, n, err := d.Resume()
	r.Bytes, r.ByteExact, r.Elapsed = n, ok, time.Since(t0)
	r.PeakHostHeld = e.Host.Stats.PeakHeld.Load()
	if !ok || err != nil {
		return r, fmt.Errorf("C2 FAIL: not byte-exact (%d of %d bytes, sha match=%v, read err=%v)", n, size, ok, err)
	}
	if r.PeakHeapGrow > r.Bound {
		return r, fmt.Errorf("C2 FAIL: heap grew %.1f MiB > bound %.0f MiB", mib(r.PeakHeapGrow), mib(r.Bound))
	}
	return r, nil
}

// ---------------------------------------------------------------- criterion 3

type C3 struct {
	Assets, AssetsOK   int
	AssetsMax          time.Duration // slowest asset
	PeakStreams        int64
	Echo               C1 // echo during the pause, against a quiet bare baseline
	DownloadExact      bool
	AssetsBeforeResume bool
}

func (r C3) String() string {
	return fmt.Sprintf("assets %d/%d ok, slowest %v, all-before-resume=%v, peak host streams=%d, download exact=%v | echo under load: %v",
		r.AssetsOK, r.Assets, r.AssetsMax.Round(time.Millisecond), r.AssetsBeforeResume, r.PeakStreams, r.DownloadExact, r.Echo)
}

// CheckC3: 62 asset GETs + 1 HMR WebSocket echo + the paused 100 MB reader of
// C2 = 64 concurrent streams. Every asset must finish while the reader is
// still paused, and the echo must stay within C1's bound.
func CheckC3(e *Env, size int64, pause time.Duration, echoN int) (C3, error) {
	const assets = 62
	r := C3{Assets: assets}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	bare, err := EchoRTTs(ctx, e.wsURL("/__bare_echo"), echoN, e.dialOpts())
	if err != nil {
		return r, err
	}
	e.Host.Stats.PeakStreams.Store(0)
	d, err := StartPausedDownload(e, size, 9)
	if err != nil {
		return r, err
	}
	time.Sleep(500 * time.Millisecond) // let the paused stream fill its window
	resumeAt := time.Now().Add(pause)

	var wg sync.WaitGroup
	var mu sync.Mutex
	cl := &http.Client{Transport: &http.Transport{DisableCompression: true, MaxConnsPerHost: 0, MaxIdleConnsPerHost: assets}}
	for i := 0; i < assets; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			t0 := time.Now()
			sz := int64(64<<10) * int64(1+i%8) // 64 KiB .. 512 KiB
			actx, acancel := context.WithDeadline(ctx, resumeAt)
			defer acancel()
			req, _ := http.NewRequestWithContext(actx, "GET", fmt.Sprintf("%s/blob?n=%d&seed=%d&delay=300ms", e.Base, sz, 100+i), nil)
			resp, err := cl.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			h := sha256.New()
			n, err := io.Copy(h, resp.Body)
			want := BlobSHA(uint64(100+i), sz)
			if err == nil && n == sz && bytes.Equal(h.Sum(nil), want[:]) {
				mu.Lock()
				r.AssetsOK++
				r.AssetsMax = max(r.AssetsMax, time.Since(t0))
				mu.Unlock()
			}
		}(i)
	}
	var tun []time.Duration
	var echoErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		ectx, ecancel := context.WithDeadline(ctx, resumeAt)
		defer ecancel()
		tun, echoErr = EchoRTTs(ectx, e.wsURL("/ws"), echoN, e.dialOpts())
	}()
	wg.Wait()
	r.AssetsBeforeResume = time.Now().Before(resumeAt)
	r.PeakStreams = e.Host.Stats.PeakStreams.Load()
	if w := time.Until(resumeAt); w > 0 {
		time.Sleep(w)
	}
	ok, _, derr := d.Resume()
	r.DownloadExact = ok && derr == nil
	var errs []error
	if echoErr != nil || len(tun) < echoN {
		errs = append(errs, fmt.Errorf("echo stalled: %d/%d before resume (%v)", len(tun), echoN, echoErr))
	} else {
		var e1 error
		r.Echo, e1 = c1From(bare, tun)
		errs = append(errs, e1)
	}
	if r.AssetsOK != assets {
		errs = append(errs, fmt.Errorf("only %d/%d assets completed while the reader was paused", r.AssetsOK, assets))
	}
	if !r.DownloadExact {
		errs = append(errs, errors.New("paused download not byte-exact"))
	}
	if err := errors.Join(errs...); err != nil {
		return r, fmt.Errorf("C3 FAIL: %w", err)
	}
	return r, nil
}

// ---------------------------------------------------------------- criterion 4

type C4 struct {
	Idle                        time.Duration
	EchoAfterIdle               bool
	NewStreamAfterIdle          bool
	HostTunnelAlive             bool
	EdgeKillsTun, EdgeKillsPrev int64
}

func (r C4) String() string {
	return fmt.Sprintf("idle=%v ws-echo-after=%v new-stream-after=%v host-tunnel-alive=%v edge-kills tunnel=%d preview=%d",
		r.Idle, r.EchoAfterIdle, r.NewStreamAfterIdle, r.HostTunnelAlive, r.EdgeKillsTun, r.EdgeKillsPrev)
}

func CheckC4(e *Env, idle time.Duration) (C4, error) {
	r := C4{Idle: idle}
	ctx, cancel := context.WithTimeout(context.Background(), idle+30*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, e.wsURL("/ws"), e.dialOpts())
	if err != nil {
		return r, err
	}
	defer c.CloseNow()
	type msg struct {
		p   []byte
		err error
	}
	in := make(chan msg, 4)
	go func() { // keep reading so control frames (pings) are answered
		for {
			_, p, err := c.Read(ctx)
			in <- msg{p, err}
			if err != nil {
				return
			}
		}
	}()
	roundtrip := func() bool {
		if c.Write(ctx, websocket.MessageText, []byte("x")) != nil {
			return false
		}
		select {
		case m := <-in:
			return m.err == nil && string(m.p) == "x"
		case <-time.After(5 * time.Second):
			return false
		}
	}
	if !roundtrip() {
		return r, errors.New("C4: first echo failed")
	}
	time.Sleep(idle)
	r.EchoAfterIdle = roundtrip()
	gctx, gcancel := context.WithTimeout(ctx, 5*time.Second)
	defer gcancel()
	req := e.newReq(gctx, e.Base+"/blob?n=1024&seed=1")
	if resp, err := (&http.Client{Transport: &http.Transport{}}).Do(req); err == nil {
		n, _ := io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		r.NewStreamAfterIdle = resp.StatusCode == 200 && n == 1024
	}
	select {
	case <-e.HostDone:
		r.HostTunnelAlive = false
	default:
		r.HostTunnelAlive = true
	}
	if e.EdgeTun != nil { // remote mode has no simulator; the real edge's kills show as the other fields
		r.EdgeKillsTun, r.EdgeKillsPrev = e.EdgeTun.Kills.Load(), e.EdgePrev.Kills.Load()
	}
	if !r.EchoAfterIdle || !r.NewStreamAfterIdle || !r.HostTunnelAlive || r.EdgeKillsTun+r.EdgeKillsPrev > 0 {
		return r, fmt.Errorf("C4 FAIL: %v", r)
	}
	return r, nil
}

// ---------------------------------------------------------------- criterion 5

type C5 struct {
	N          int
	P50, Max   time.Duration
	Compressed bool // the response arrived compressed (only possible with Env.Gzip)
}

func (r C5) String() string {
	return fmt.Sprintf("n=%d first-event latency p50=%v max=%v compressed=%v", r.N, r.P50, r.Max, r.Compressed)
}

const C5Max = 500 * time.Millisecond

func CheckC5(e *Env, n int, hold time.Duration) (C5, error) {
	var lat []time.Duration
	cl := &http.Client{Transport: &http.Transport{DisableCompression: !e.Gzip}, Timeout: hold + 5*time.Second}
	compressed := false
	for i := 0; i < n; i++ {
		for len(e.Up.SSESent) > 0 {
			<-e.Up.SSESent
		}
		resp, err := cl.Do(e.newReq(context.Background(), fmt.Sprintf("%s/sse?hold=%s", e.Base, hold)))
		if err != nil {
			return C5{}, fmt.Errorf("C5: no response headers: %w", err)
		}
		compressed = compressed || resp.Uncompressed
		br := bufio.NewReader(resp.Body)
		var got time.Time
		for {
			line, err := br.ReadString('\n')
			if strings.HasPrefix(line, "data: hello") {
				got = time.Now()
				break
			}
			if err != nil {
				resp.Body.Close()
				return C5{}, fmt.Errorf("C5 FAIL: event not delivered before the stream ended/timed out: %v", err)
			}
		}
		sent := <-e.Up.SSESent
		resp.Body.Close()
		lat = append(lat, max(0, got.Sub(sent)))
	}
	r := C5{N: n, P50: pct(lat, .5), Max: pct(lat, 1), Compressed: compressed}
	if r.Max > C5Max {
		return r, fmt.Errorf("C5 FAIL: max first-event latency %v > %v", r.Max, C5Max)
	}
	return r, nil
}
