package previewgw

// tunnels.go — the hosts' tunnels: one per (owner, host). A newer tunnel
// replaces the older at once for new streams; the older DRAINS — it closes
// when its last stream ends, or DrainTimeout after it was replaced, whichever
// is first (a host returning to the mesh from the public path is the common
// case: its public tunnel finishes what it carries, then goes).

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/preview/tunnel"
)

// DrainTimeout bounds how long a replaced tunnel keeps its open streams.
const DrainTimeout = 60 * time.Second

// ErrHostOffline is Dial's error when the (owner, host) has no live tunnel.
var ErrHostOffline = errors.New("previewgw: the host has no tunnel")

// errOwnerNotServed refuses a tunnel whose owner's plugin token is not live.
var errOwnerNotServed = errors.New("previewgw: this owner is not served (its plugin token is revoked, lacks the viewer capability, or muster has not answered yet)")

type tunnelKey struct {
	owner int64
	host  string
}

// Tunnel is one host's accepted tunnel.
type Tunnel struct {
	key       tunnelKey
	tokenHash string
	path      Path
	client    *tunnel.Client
	ctx       context.Context
	answers   chan pendingAnswer // to answerWorker; bounded (answerQueue)
	rt        *http.Transport    // the proxy's upstream connections on this tunnel (proxy.go)

	mu        sync.Mutex
	state     HostState
	streams   int
	draining  bool
	stopDrain func() bool
}

// Close ends the tunnel.
func (t *Tunnel) Close() {
	if t.rt != nil {
		t.rt.CloseIdleConnections()
	}
	_ = t.client.Close()
}

// Done is closed when the tunnel has ended.
func (t *Tunnel) Done() <-chan struct{} { return t.client.Done() }

// TunnelInfo is a tunnel as Tunnels reports it.
type TunnelInfo struct {
	Owner    int64
	Host     string
	Path     Path
	Draining bool
	Streams  int
	State    HostState
}

// Tunnels lists every live tunnel, the draining ones included.
func (g *Gateway) Tunnels() []TunnelInfo {
	g.mu.Lock()
	ts := make([]*Tunnel, 0, len(g.all))
	for t := range g.all {
		ts = append(ts, t)
	}
	g.mu.Unlock()
	out := make([]TunnelInfo, 0, len(ts))
	for _, t := range ts {
		t.mu.Lock()
		st := t.state
		st.Windows = append([]string(nil), st.Windows...)
		st.Listeners = append(st.Listeners[:0:0], st.Listeners...)
		out = append(out, TunnelInfo{Owner: t.key.owner, Host: t.key.host, Path: t.path,
			Draining: t.draining, Streams: t.streams, State: st})
		t.mu.Unlock()
	}
	return out
}

// register makes t the (owner, host)'s tunnel, draining the one it replaces.
// It refuses when the owner is not served — checked under the same lock the
// owner check's closer takes, so a tunnel cannot slip in after its owner was
// closed.
func (g *Gateway) register(t *Tunnel) error {
	g.mu.Lock()
	if !g.liveOwnerLocked(t.key.owner) {
		g.mu.Unlock()
		return errOwnerNotServed
	}
	old := g.byKey[t.key]
	g.byKey[t.key] = t
	g.all[t] = struct{}{}
	g.mu.Unlock()
	g.tunnelsGauge.WithLabelValues(string(t.path)).Inc()
	if old != nil {
		g.log.Info("tunnel replaced", "host", t.key.host, "old_path", string(old.path), "new_path", string(t.path))
		g.drain(old)
	}
	return nil
}

// unregister forgets t once it has ended.
func (g *Gateway) unregister(t *Tunnel) {
	g.mu.Lock()
	_, was := g.all[t]
	delete(g.all, t)
	if g.byKey[t.key] == t {
		delete(g.byKey, t.key)
	}
	g.mu.Unlock()
	t.mu.Lock()
	if t.stopDrain != nil {
		t.stopDrain()
	}
	t.mu.Unlock()
	if was {
		g.tunnelsGauge.WithLabelValues(string(t.path)).Dec()
	}
}

// drain stops new streams going to t (it is no longer the key's tunnel) and
// closes it when its last stream ends, or after DrainTimeout.
func (g *Gateway) drain(t *Tunnel) {
	// Idle pooled upstream connections would hold the drain open, and serve
	// nothing new: new requests go to the replacing tunnel.
	if t.rt != nil {
		t.rt.CloseIdleConnections()
	}
	t.mu.Lock()
	t.draining = true
	idle := t.streams == 0
	if !idle {
		t.stopDrain = g.clock.AfterFunc(DrainTimeout, t.Close)
	}
	t.mu.Unlock()
	if idle {
		t.Close()
	}
}

// Dial opens one upstream connection to port on the (owner, host)'s current
// tunnel. The connection holds the tunnel open while it is draining.
func (g *Gateway) Dial(ctx context.Context, owner int64, host string, port int) (net.Conn, error) {
	g.mu.Lock()
	t := g.byKey[tunnelKey{owner, host}]
	if t != nil {
		t.mu.Lock()
		t.streams++
		t.mu.Unlock()
	}
	g.mu.Unlock()
	if t == nil {
		return nil, ErrHostOffline
	}
	return t.dialHeld(ctx, port)
}

// dial opens one upstream connection to port on t itself (the proxy's
// transport for t), holding t open while it lives.
func (t *Tunnel) dial(ctx context.Context, port int) (net.Conn, error) {
	t.mu.Lock()
	t.streams++
	t.mu.Unlock()
	return t.dialHeld(ctx, port)
}

// dialHeld dials port on t for a stream already counted in t.streams.
func (t *Tunnel) dialHeld(ctx context.Context, port int) (net.Conn, error) {
	c, err := t.client.Dial(ctx, port)
	if err != nil {
		t.streamDone()
		return nil, err
	}
	return &trackedConn{Conn: c, done: t.streamDone}, nil
}

// streamDone releases one stream; a draining tunnel with none left closes.
func (t *Tunnel) streamDone() {
	t.mu.Lock()
	t.streams--
	last := t.draining && t.streams == 0
	t.mu.Unlock()
	if last {
		t.Close()
	}
}

type trackedConn struct {
	net.Conn
	once sync.Once
	done func()
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.done)
	return err
}

// CloseWrite half-closes, when the underlying stream can.
func (c *trackedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return errors.ErrUnsupported
}

// closeTunnelsWhere closes every tunnel (draining ones included) for which
// match is true, and returns them.
func (g *Gateway) closeTunnelsWhere(match func(*Tunnel) bool) []*Tunnel {
	g.mu.Lock()
	var hit []*Tunnel
	for t := range g.all {
		if match(t) {
			hit = append(hit, t)
		}
	}
	g.mu.Unlock()
	for _, t := range hit {
		t.Close()
	}
	return hit
}
