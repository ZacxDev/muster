package previewgw

// offers.go — a tunnel's offer stream (wire.OfferPath), read into memory, and
// the previews the hosts' open/close requests make. Nothing here is stored:
// a gateway restart forgets every preview, and each host, seeing the new boot
// id on the next GET /v1/offer, re-announces the opens it had acknowledged.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/preview/wire"
)

// answerTimeout bounds one POST answer to the host: the host's tool waits 5 s
// for it.
const answerTimeout = 5 * time.Second

// answerQueue is how many answers one tunnel may have waiting for its
// answer worker. Answers go out one at a time, each within answerTimeout; a
// host whose requests outrun that by this many is flooding the gateway, and
// its tunnel is closed rather than queued without bound.
const answerQueue = 64

// Bounds on what an open or close request may carry, in bytes. A request
// over one is refused, never stored.
const (
	// maxWindow: a tmux window id is "@" and a decimal number; 32 bytes is
	// far past any real one.
	maxWindow = 32
	// maxSession: the mirror's own session-name bound (internal/mirror,
	// ValidSession), so any session a host mirrors fits.
	maxSession = 128
	// maxPath: the design's bound on a preview's start path.
	maxPath = 512
	// maxLabel: the design's bound on a label (longer ones are cut).
	maxLabel = 60
)

type pendingAnswer struct {
	path string
	a    wire.Answer
}

// HostState is what a tunnel's offer stream has said, as of now.
type HostState struct {
	// Config is the host's latest config line; nil before the first.
	Config *wire.Config
	// MaxConns is the tunnel's upstream-connection limit, fixed by the first
	// config that carried one (0 before then). A later config never raises
	// it: the host's HTTP/2 server was started with its own value when it
	// dialed, and a gateway limit above that only turns waits into refusals.
	MaxConns int
	// Windows is the host's latest window listing; HaveWindows is whether it
	// has sent one (an empty listing is omitted on the wire, so a nil Windows
	// alone cannot tell "none" from "not yet").
	Windows     []string
	HaveWindows bool
	Listeners   []wire.Listener
}

// Preview is one open preview, in memory only.
type Preview struct {
	ID      string
	OwnerID int64
	Host    string
	Window  string
	Session string // display only
	Port    int
	Label   string
	Path    string
	Opened  time.Time

	lastUsed  time.Time          // the last proxied request's start or end
	active    int                // proxied requests and WebSockets in flight (at most MaxConnsPerPreview)
	slots     chan struct{}      // one token per use in flight; its capacity is the cap (useBegin)
	goneSince time.Time          // when its window left the host's listing; zero while listed
	ctx       context.Context    // ends when the preview closes; every use of it runs under it
	cancel    context.CancelFunc // ends ctx (deletePreviewLocked)
}

var previewIDEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// newPreviewID is 128 random bits as 26 lowercase base32 characters: one DNS
// label, p-<id>.
func newPreviewID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return previewIDEncoding.EncodeToString(b), nil
}

// consumeOffers reads t's offer stream until it ends, then ends the tunnel:
// a tunnel without its offer stream cannot be told what the host offers or
// asks, so the host must reconnect.
func (g *Gateway) consumeOffers(t *Tunnel) {
	defer t.Close()
	go g.answerWorker(t)
	req, err := http.NewRequestWithContext(t.ctx, http.MethodGet, "http://tunnel"+wire.OfferPath, nil)
	if err != nil {
		return
	}
	req.Header.Set(wire.GatewayBootHeader, g.boot)
	resp, err := t.client.RoundTrip(req)
	if err != nil {
		g.log.Info("offer stream did not open", "host", t.key.host, "err", err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		g.log.Info("offer stream refused", "host", t.key.host, "status", resp.StatusCode)
		return
	}
	dec := wire.NewDecoder(resp.Body)
	for {
		m, err := dec.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) && t.ctx.Err() == nil {
				g.log.Info("offer stream ended", "host", t.key.host, "err", err.Error())
			}
			return
		}
		g.applyOffer(t, m)
	}
}

// applyOffer folds one offer line into t's state.
func (g *Gateway) applyOffer(t *Tunnel, m wire.Offer) {
	switch m.Type {
	case wire.TypeConfig:
		c := *m.Config
		c.Ports = append([]string(nil), c.Ports...)
		t.mu.Lock()
		t.state.Config = &c
		limit := 0
		if c.MaxConns > 0 {
			if t.state.MaxConns == 0 {
				t.state.MaxConns = min(c.MaxConns, wire.MaxConns)
			}
			// Capped at the tunnel's own value: lowering is merely slower,
			// raising past what the host's server admits is not allowed.
			limit = min(c.MaxConns, t.state.MaxConns)
			t.connLimit = limit
		}
		t.mu.Unlock()
		if limit > 0 {
			t.client.SetMaxConns(limit)
		}
	case wire.TypeWindows:
		ws := append([]string(nil), m.Windows...)
		t.mu.Lock()
		t.state.Windows, t.state.HaveWindows = ws, true
		t.mu.Unlock()
		g.windowsListed(t, ws)
	case wire.TypeListeners:
		t.mu.Lock()
		t.state.Listeners = append([]wire.Listener(nil), m.Listeners...)
		t.mu.Unlock()
	case wire.TypeOpen:
		g.queueAnswer(t, wire.OpenedPath, g.openPreview(t, *m.Request))
	case wire.TypeClose:
		g.queueAnswer(t, wire.ClosedPath, g.closePreview(t, *m.Request))
	default:
		// An unknown type from a newer host: skipped (the stream is additive).
	}
}

// openPreview records an agent's preview_open. One preview per (owner, host,
// window, port): a repeat answers the same id. It is refused when t is no
// longer its (owner, host)'s tunnel (closed or replaced, e.g. a line read
// after its token was revoked) or the owner is no longer served. r.Preview is ignored on an open
// (wire.Request): a re-announced open gets a new id, because ids are this
// gateway's to mint, never the host's to choose.
//
// The open limits are newPreviewLocked's (lifecycle.go). The host probed the
// port (a loopback dial and HEAD /) before it asked.
func (g *Gateway) openPreview(t *Tunnel, r wire.Request) wire.Answer {
	a := wire.Answer{RequestID: r.ID, Window: r.Window, Port: r.Port}
	if e := requestBounds(r); e != "" {
		a.Window = ""
		a.Error = e
		return a
	}
	if r.Port < 1 || r.Port > 65535 {
		a.Error = "bad_port"
		return a
	}
	if r.Path != "" && !strings.HasPrefix(r.Path, "/") {
		a.Error = "bad_path"
		return a
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.byKey[t.key] != t || !g.liveOwnerLocked(t.key.owner) {
		a.Error = codeHostOffline
		return a
	}
	pk := previewKey{t.key, r.Window, r.Port}
	if id, ok := g.previewBy[pk]; ok {
		a.Preview = id
		return a
	}
	a.Preview, a.Error = g.newPreviewLocked(t, openerAgent, r.Window, r.Session, r.Port, r.Label, r.Path)
	if a.Error != "" {
		g.m.refused(a.Error)
	}
	return a
}

// closePreview ends a preview the request names, if it is this host's.
func (g *Gateway) closePreview(t *Tunnel, r wire.Request) wire.Answer {
	a := wire.Answer{RequestID: r.ID, Window: r.Window, Preview: r.Preview}
	if e := requestBounds(r); e != "" || len(r.Preview) > 64 {
		a.Window, a.Preview, a.Error = "", "", "bad_request"
		return a
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	p, ok := g.previews[r.Preview]
	if !ok || p.OwnerID != t.key.owner || p.Host != t.key.host {
		a.Error = "not_found"
		return a
	}
	a.Port = p.Port
	g.deletePreviewLocked(p, closeHost)
	return a
}

// requestBounds is the error code for a request field over its bound, or "".
func requestBounds(r wire.Request) string {
	switch {
	case len(r.Window) > maxWindow:
		return "bad_window"
	case len(r.Session) > maxSession:
		return "bad_session"
	case len(r.Path) > maxPath:
		return "bad_path"
	}
	return ""
}

// previewKey is what makes an open a repeat: one preview per (owner, host,
// window, port).
type previewKey struct {
	tunnelKey
	window string
	port   int
}

// deletePreviewLocked forgets p and ends its context, which cuts every
// request and WebSocket of it in flight (lifecycle.go). reason is its
// muster_preview_closes_total label. Every caller passes an open preview
// (one it found in g.previews under the lock), so each close counts once.
func (g *Gateway) deletePreviewLocked(p *Preview, reason string) {
	if p.cancel != nil {
		p.cancel()
	}
	g.m.closed(reason)
	delete(g.previews, p.ID)
	key := tunnelKey{p.OwnerID, p.Host}
	delete(g.previewBy, previewKey{key, p.Window, p.Port})
	// Its pooled idle connections would hold host slots until they time out:
	// closed now, on every tunnel of the host (a draining one included). The
	// pool is per tunnel, so the host's other previews' idle ones go too and
	// are simply dialled again. Not under g.mu: closing a stream can wait on
	// the tunnel's writer, and a stalled host must not hold the gateway's lock.
	for t := range g.all {
		if t.key == key && t.rt != nil {
			go t.rt.CloseIdleConnections()
		}
	}
}

// queueAnswer hands a to t's answer worker, or closes t when its queue is
// full (see answerQueue).
func (g *Gateway) queueAnswer(t *Tunnel, path string, a wire.Answer) {
	select {
	case t.answers <- pendingAnswer{path, a}:
	default:
		g.log.Warn("host's answer queue is full: closing its tunnel", "host", t.key.host)
		t.Close()
	}
}

// answerWorker sends t's answers, one at a time, until the tunnel ends.
func (g *Gateway) answerWorker(t *Tunnel) {
	for {
		select {
		case <-t.Done():
			return
		case p := <-t.answers:
			g.answer(t, p.path, p.a)
		}
	}
}

// answer POSTs a to the host over t.
func (g *Gateway) answer(t *Tunnel, path string, a wire.Answer) {
	b, err := json.Marshal(a)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(t.ctx, answerTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://tunnel"+path, bytes.NewReader(b))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.RoundTrip(req)
	if err != nil {
		g.log.Info("answer not delivered", "host", t.key.host, "path", path, "err", err.Error())
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		g.log.Info("answer refused", "host", t.key.host, "path", path, "status", resp.StatusCode)
	}
}

// Previews lists owner's open previews (any order).
func (g *Gateway) Previews(owner int64) []Preview {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []Preview
	for _, p := range g.previews {
		if p.OwnerID == owner {
			out = append(out, *p)
		}
	}
	return out
}

// closePreviewsLocked deletes the previews matching (owner, host); host ""
// matches every host of owner. It returns how many.
func (g *Gateway) closePreviewsLocked(owner int64, host, reason string) int {
	n := 0
	for _, p := range g.previews {
		if p.OwnerID == owner && (host == "" || p.Host == host) {
			g.deletePreviewLocked(p, reason)
			n++
		}
	}
	return n
}

// errString is a stable string for logs.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprint(err)
}
