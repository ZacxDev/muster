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

// HostState is what a tunnel's offer stream has said, as of now.
type HostState struct {
	// Config is the host's latest config line; nil before the first.
	Config *wire.Config
	// MaxConns is the tunnel's upstream-connection limit, fixed by the first
	// config that carried one (0 before then). A later config never raises
	// it: the host's HTTP/2 server was started with its own value when it
	// dialed, and a gateway limit above that only turns waits into refusals.
	MaxConns  int
	Windows   []string
	Listeners []wire.Listener
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
		}
		t.mu.Unlock()
		if limit > 0 {
			t.client.SetMaxConns(limit)
		}
	case wire.TypeWindows:
		t.mu.Lock()
		t.state.Windows = append([]string(nil), m.Windows...)
		t.mu.Unlock()
	case wire.TypeListeners:
		t.mu.Lock()
		t.state.Listeners = append([]wire.Listener(nil), m.Listeners...)
		t.mu.Unlock()
	case wire.TypeOpen:
		a := g.openPreview(t, *m.Request)
		go g.answer(t, wire.OpenedPath, a)
	case wire.TypeClose:
		a := g.closePreview(t, *m.Request)
		go g.answer(t, wire.ClosedPath, a)
	default:
		// An unknown type from a newer host: skipped (the stream is additive).
	}
}

// openPreview records an agent's preview_open. One preview per (owner, host,
// window, port): a repeat answers the same id. r.Preview is ignored on an open
// (wire.Request): a re-announced open gets a new id, because ids are this
// gateway's to mint, never the host's to choose.
//
// ⚠ NOT YET: the probe (a loopback dial and HEAD / before answering), the
// open-preview limits and the lifecycle timers arrive with the proxy.
func (g *Gateway) openPreview(t *Tunnel, r wire.Request) wire.Answer {
	a := wire.Answer{RequestID: r.ID, Window: r.Window, Port: r.Port}
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
	for _, p := range g.previews {
		if p.OwnerID == t.key.owner && p.Host == t.key.host && p.Window == r.Window && p.Port == r.Port {
			a.Preview = p.ID
			return a
		}
	}
	id, err := newPreviewID()
	if err != nil {
		a.Error = "internal"
		return a
	}
	label := r.Label
	if len(label) > 60 {
		label = label[:60]
	}
	g.previews[id] = &Preview{ID: id, OwnerID: t.key.owner, Host: t.key.host, Window: r.Window,
		Session: r.Session, Port: r.Port, Label: label, Path: r.Path, Opened: g.clock.Now()}
	a.Preview = id
	return a
}

// closePreview ends a preview the request names, if it is this host's.
func (g *Gateway) closePreview(t *Tunnel, r wire.Request) wire.Answer {
	a := wire.Answer{RequestID: r.ID, Window: r.Window, Preview: r.Preview}
	g.mu.Lock()
	defer g.mu.Unlock()
	p, ok := g.previews[r.Preview]
	if !ok || p.OwnerID != t.key.owner || p.Host != t.key.host {
		a.Error = "not_found"
		return a
	}
	a.Port = p.Port
	delete(g.previews, r.Preview)
	return a
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
func (g *Gateway) closePreviewsLocked(owner int64, host string) int {
	n := 0
	for id, p := range g.previews {
		if p.OwnerID == owner && (host == "" || p.Host == host) {
			delete(g.previews, id)
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
