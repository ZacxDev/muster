package previewgw

// lifecycle.go — how a preview comes and goes: the open limits, checked in
// ONE place (newPreviewLocked) for both openers (an agent's preview_open on
// the offer stream, and Open on an offer on the session page); the limit on
// one preview's requests in flight (useBegin); the idle, TTL and window-gone
// closes; and the gateway telling a host about a close it made itself
// (docs/design/previews.md, "Lifecycle" and "Limits and quotas").
//
// 🔴 A CLOSE IS IMMEDIATE. deletePreviewLocked ends the preview's context,
// which every proxied request and relayed WebSocket of it runs under, so a
// close cuts them at once; and the owner cookie names the preview id, which
// no longer resolves, so the next request is 404. With 8 h owner cookies
// that is the only immediate cut-off the owner has.

import (
	"context"
	"slices"
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
	// MaxConnsPerPreview is the most requests and WebSockets one preview may
	// have in flight at once (the design's 64 "concurrent client
	// connections" per preview). One more WAITS for a slot, for at most the
	// dial wait (Options.DialWait, the same bound the host's own upstream
	// slots use), and is then refused 429 too_many_connections — before
	// anything is dialled. It binds when the host's own upstream slots
	// (maxConns, at most 64) are full and requests queue for one: without it
	// a page could queue without bound.
	MaxConnsPerPreview = 64
)

// WindowGoneAfter closes a preview whose window has been absent from its
// host's window listing (wire.TypeWindows) this long.
const WindowGoneAfter = 10 * time.Minute

// LifecycleEvery is how often the idle, TTL and window-gone closes are
// checked; a preview closes at most this long after its limit passed.
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
// port) for opener, or answers codeMaxOpen past a limit. The caller has
// checked that t is its key's current tunnel and that no preview of the key
// exists.
func (g *Gateway) newPreviewLocked(t *Tunnel, opener, window, session string, port int, label, path string) (string, string) {
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
	p := &Preview{ID: id, OwnerID: t.key.owner, Host: t.key.host, Window: window, Session: session,
		Port: port, Label: label, Path: path, Opened: now, lastUsed: now, ctx: ctx, cancel: cancel,
		slots: make(chan struct{}, g.o.MaxConnsPerPreview)}
	// A window the host's listing already lacks is gone from now.
	t.mu.Lock()
	if t.state.HaveWindows && !slices.Contains(t.state.Windows, window) {
		p.goneSince = now
	}
	t.mu.Unlock()
	g.previews[id] = p
	g.m.opened(opener)
	return id, ""
}

// windowsListed applies a window listing from t (the WHOLE current set) to
// the previews of t's (owner, host): a preview whose window is absent starts
// (or keeps) its gone-since time, one whose window is listed clears it.
func (g *Gateway) windowsListed(t *Tunnel, windows []string) {
	now := g.clock.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, p := range g.previews {
		if p.OwnerID != t.key.owner || p.Host != t.key.host {
			continue
		}
		switch {
		case slices.Contains(windows, p.Window):
			p.goneSince = time.Time{}
		case p.goneSince.IsZero():
			p.goneSince = now
		}
	}
}

// sweep closes every preview past the TTL, idle (no request in flight and
// none for the idle period), or whose window has been gone from its host's
// listing for WindowGoneAfter, and tells each one's host. A host that is
// offline sends no listing, so it neither starts nor clears a gone-since
// time: the last listing it sent stands.
func (g *Gateway) sweep() {
	now := g.clock.Now()
	g.mu.Lock()
	var closed []Preview
	for _, p := range g.previews {
		reason := ""
		switch {
		case now.Sub(p.Opened) >= g.ttl:
			reason = closeTTL
		case p.active == 0 && now.Sub(p.lastUsed) >= g.idle:
			reason = closeIdle
		case !p.goneSince.IsZero() && now.Sub(p.goneSince) >= WindowGoneAfter:
			reason = closeWindowGone
		}
		if reason != "" {
			g.deletePreviewLocked(p, reason)
			closed = append(closed, *p)
		}
	}
	g.mu.Unlock()
	for _, p := range closed {
		g.tellHostClosed(p)
	}
	if len(closed) > 0 {
		g.log.Info("closed idle, expired or window-gone previews", "previews", len(closed))
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
		g.deletePreviewLocked(p, closePage)
		snap = *p
	}
	g.mu.Unlock()
	if ok {
		g.tellHostClosed(snap)
	}
	return ok
}

// useBegin marks one proxied request (or relayed WebSocket) of id in flight
// and returns what it needs, or a refusal code: codePreviewClosed when the
// preview is not open, its owner is not served, or it closes while the
// request waits; codeTooManyConns when it has MaxConnsPerPreview in flight
// and none ends within the dial wait; left (and no code) when ctx — the
// browser's request — ends first. useEnd(p) must follow a use begun (code ""
// and !left), and only one: a refusal or an abandoned wait holds nothing.
//
// The wait holds no gateway lock: a slot is a token in p.slots, taken by a
// send that blocks only this request. A waiter that gives up never took one,
// so it cannot leak one; a close ends p.ctx, which wakes every waiter at once.
func (g *Gateway) useBegin(ctx context.Context, id string) (p Preview, code string, left bool) {
	g.mu.Lock()
	pp, ok := g.previews[id]
	if !ok || !g.liveOwnerLocked(pp.OwnerID) {
		g.mu.Unlock()
		return Preview{}, codePreviewClosed, false
	}
	slots, pctx := pp.slots, pp.ctx
	g.mu.Unlock()

	select {
	case slots <- struct{}{}:
	default:
		wait := time.NewTimer(g.o.DialWait)
		defer wait.Stop()
		select {
		case slots <- struct{}{}:
		case <-pctx.Done():
			return Preview{}, codePreviewClosed, false
		case <-ctx.Done():
			return Preview{}, "", true
		case <-wait.C:
			return Preview{}, codeTooManyConns, false
		}
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	// A close between the lookup and the slot (one that lost the select to a
	// freed slot) — every close, an owner's included, deletes the preview.
	if cur, ok := g.previews[id]; !ok || cur != pp {
		<-slots
		return Preview{}, codePreviewClosed, false
	}
	pp.active++
	pp.lastUsed = g.clock.Now()
	return *pp, "", false
}

// useEnd ends one in-flight use of p, begun by useBegin, and frees its slot.
// active drops before the slot frees, so a waiter that takes it never counts
// one more than the cap.
func (g *Gateway) useEnd(p Preview) {
	g.mu.Lock()
	// The preview may have closed meanwhile; then nothing reads it again.
	if pp, ok := g.previews[p.ID]; ok {
		pp.active--
		pp.lastUsed = g.clock.Now()
	}
	g.mu.Unlock()
	<-p.slots
}
