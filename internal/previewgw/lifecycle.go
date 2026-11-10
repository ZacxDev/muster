package previewgw

// lifecycle.go — how a preview comes and goes: the open limits, checked in
// ONE place (newPreviewLocked) for both openers (an agent's preview_open on
// the offer stream, and Open on an offer on the session page); the idle and
// TTL closes; and the gateway telling a host about a close it made itself
// (docs/design/previews.md, "Lifecycle" and "Limits and quotas").
//
// 🔴 A CLOSE IS IMMEDIATE. deletePreviewLocked ends the preview's context,
// which every proxied request and relayed WebSocket of it runs under, so a
// close cuts them at once; and the owner cookie names the preview id, which
// no longer resolves, so the next request is 404. With 8 h owner cookies
// that is the only immediate cut-off the owner has.

import (
	"context"
	"time"

	"github.com/ZacxDev/muster/internal/preview/wire"
)

// The open limits. A repeat open of an existing (owner, host, window, port)
// returns its id and counts nothing.
const (
	// MaxOpenPerWindow previews per calling session (tmux window).
	MaxOpenPerWindow = 4
	// MaxOpenPerOwner previews per owner, across hosts.
	MaxOpenPerOwner = 16
	// DefaultHostMaxOpen is a host's limit before it has sent a config with
	// maxOpen (previews.json's own default). A host's maxOpen above
	// MaxOpenPerOwner never binds: one host has one owner.
	DefaultHostMaxOpen = 8
)

// LifecycleEvery is how often the idle and TTL closes are checked; a
// preview closes at most this long after its limit passed.
const LifecycleEvery = 30 * time.Second

// Refusal codes an open can answer (wire.Answer.Error, and the session
// page's open route).
const (
	codeMaxOpen     = "max_open"
	codeHostOffline = "host_offline"
)

// openRefusedLocked reports whether one more preview in t's (owner, host)
// and window would pass a limit: per window, per owner, or the host's
// maxOpen.
func (g *Gateway) openRefusedLocked(t *Tunnel, window string) bool {
	hostMax := DefaultHostMaxOpen
	t.mu.Lock()
	if c := t.state.Config; c != nil && c.MaxOpen > 0 {
		hostMax = c.MaxOpen
	}
	t.mu.Unlock()
	var owner, host, win int
	for _, p := range g.previews {
		if p.OwnerID != t.key.owner {
			continue
		}
		owner++
		if p.Host == t.key.host {
			host++
			if p.Window == window {
				win++
			}
		}
	}
	return win >= MaxOpenPerWindow || owner >= MaxOpenPerOwner || host >= hostMax
}

// newPreviewLocked records a new preview of (t's owner and host, window,
// port), or answers codeMaxOpen past a limit. The caller has checked that t
// is its key's current tunnel and that no preview of the key exists.
func (g *Gateway) newPreviewLocked(t *Tunnel, window, session string, port int, label, path string) (string, string) {
	if g.openRefusedLocked(t, window) {
		return "", codeMaxOpen
	}
	id, err := newPreviewID()
	if err != nil {
		return "", "internal"
	}
	if len(label) > maxLabel {
		label = label[:maxLabel]
	}
	now := g.clock.Now()
	ctx, cancel := context.WithCancel(g.ctx)
	g.previewBy[previewKey{t.key, window, port}] = id
	g.previews[id] = &Preview{ID: id, OwnerID: t.key.owner, Host: t.key.host, Window: window, Session: session,
		Port: port, Label: label, Path: path, Opened: now, lastUsed: now, ctx: ctx, cancel: cancel}
	return id, ""
}

// sweep closes every preview past the TTL, or idle (no request in flight and
// none for the idle period), and tells each one's host.
func (g *Gateway) sweep() {
	now := g.clock.Now()
	g.mu.Lock()
	var closed []Preview
	for _, p := range g.previews {
		if now.Sub(p.Opened) >= g.ttl || (p.active == 0 && now.Sub(p.lastUsed) >= g.idle) {
			g.deletePreviewLocked(p)
			closed = append(closed, *p)
		}
	}
	g.mu.Unlock()
	for _, p := range closed {
		g.tellHostClosed(p)
	}
	if len(closed) > 0 {
		g.log.Info("closed idle or expired previews", "previews", len(closed))
	}
}

// tellHostClosed tells p's host, unsolicited, that the gateway closed p, so
// the agent's preview_list drops it. A host with no tunnel learns nothing;
// it re-announces only what the gateway acknowledged, and a re-announce of a
// closed preview gets a new id.
func (g *Gateway) tellHostClosed(p Preview) {
	g.mu.Lock()
	t := g.byKey[tunnelKey{p.OwnerID, p.Host}]
	g.mu.Unlock()
	if t != nil {
		g.queueAnswer(t, wire.ClosedPath, wire.Answer{Preview: p.ID, Window: p.Window, Port: p.Port})
	}
}

// closeFromPage is Close on the session page: the preview must be in the
// page's window. It reports whether it closed one.
func (g *Gateway) closeFromPage(c pageClaim, id string) bool {
	g.mu.Lock()
	p, ok := g.previews[id]
	ok = ok && p.OwnerID == c.Owner && p.Host == c.Host && p.Window == c.Window
	var snap Preview
	if ok {
		g.deletePreviewLocked(p)
		snap = *p
	}
	g.mu.Unlock()
	if ok {
		g.tellHostClosed(snap)
	}
	return ok
}

// useBegin marks one proxied request (or relayed WebSocket) of id in flight
// and returns what it needs, or ok false when the preview is not open or its
// owner not served. useEnd must follow.
func (g *Gateway) useBegin(id string) (p Preview, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	pp, ok := g.previews[id]
	if !ok || !g.liveOwnerLocked(pp.OwnerID) {
		return Preview{}, false
	}
	pp.active++
	pp.lastUsed = g.clock.Now()
	return *pp, true
}

// useEnd ends one in-flight use of id, begun by useBegin.
func (g *Gateway) useEnd(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	// The preview may have closed meanwhile; then nothing reads it again.
	if pp, ok := g.previews[id]; ok {
		pp.active--
		pp.lastUsed = g.clock.Now()
	}
}
