package previewhost

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/preview/wire"
)

// Window is one mirrored tmux window as previews need it: its id ("@12") and
// the root process of each of its panes (the listener walk's targets).
type Window struct {
	ID       string
	PanePIDs []int
}

// Open is one preview the gateway has acknowledged open on this host: by an
// agent's request (Label and Path as asked) or by the owner clicking an offer
// (the gateway tells the host unsolicited, so preview_list includes it).
type Open struct {
	Preview string
	Window  string
	Port    int
	Label   string
	Path    string
	// Opened is when the host first recorded it. A re-announce to a
	// restarted gateway keeps it (so a restart does not renew an open), and
	// an open older than MaxOpenAge is dropped instead of re-announced.
	Opened time.Time
}

// MaxOpenAge bounds how long the host keeps an open alive across gateway
// restarts: the design's default preview TTL and its offline close (24 h).
// The host cannot read the gateway's PREVIEW_TTL, so this is its own bound —
// and it is also what ends opens across a long disconnect: an open recorded
// before a disconnect of more than a day is older than this on reconnect.
// It is measured in WALL time, so a host suspended for a day has had its
// opens for a day (UNVERIFIED by a test: no test can suspend the process).
const MaxOpenAge = 24 * time.Hour

// maxPending bounds the agent requests awaiting an answer (re-announces are
// apart: at most one per open, so at most maxOpen): a gateway that never
// answers cannot grow the host's memory. Each expires after AnswerWait, or —
// once the agent was told gateway_timeout — after lateGrace.
const maxPending = 64

// lateGrace is how long a request the agent gave up on (gateway_timeout) is
// remembered, so a late "opened" for it is answered with a close rather than
// leaving a preview the gateway keeps and the host has forgotten.
const lateGrace = time.Minute

// HostConfig configures a Host.
type HostConfig struct {
	// Config is previews.json as loaded at start (Load); File is re-read every
	// Poll, and a change takes effect at once: previews on a port that left
	// the list close, and a new maxConns reconnects the tunnel (its HTTP/2
	// SETTINGS are fixed for the tunnel's life).
	Config Config
	File   string
	// Refused is the always-refused ports (muster-mirror's -mcp-listen and the
	// opencode server's), checked before the allowlist on every CONNECT.
	Refused AlwaysRefused
	// UID is this user's: the foreign_listener check and the reports.
	UID int
	// Proc is the proc root ("" = "/proc"), Procs the process table the
	// report walk reads.
	Proc  string
	Procs ProcTable
	// Poll is how often previews.json is re-read, the windows are listed and
	// the listeners are scanned (default 5 s, the mirror's own refresh).
	Poll time.Duration
	// Windows lists the mirrored tmux windows, every Poll: previews' ONLY
	// window source, so they work whether or not muster is connected. nil =
	// no listing (no window line is sent, and no listener is attributed).
	Windows func(context.Context) ([]Window, error)
	// AnswerWait bounds how long an open or close request waits for the
	// gateway's answer (default 5 s).
	AnswerWait time.Duration
	Logger     *log.Logger
	// Now is the clock (nil = time.Now).
	Now func() time.Time

	// Tests only: the loopback dial of a CONNECT (nil = dialLoopback).
	dial func(context.Context, int) (net.Conn, error)
}

// Errors of RequestOpen and RequestClose, by their tool error code.
var (
	ErrGatewayNotConnected = errors.New("gateway_not_connected")
	ErrGatewayTimeout      = errors.New("gateway_timeout")
)

// Host is the host end of previews: what the tunnel's handler (Handler)
// serves, and the state it serves from. It is safe for concurrent use; one
// Host outlives every tunnel it serves.
type Host struct {
	hc      HostConfig
	scanner *Scanner
	logger  *log.Logger

	mu       sync.Mutex
	cfg      Config
	disabled string // non-empty: previews are off, and why
	windows  []string
	panes    map[int]string
	haveWins bool // a window listing was read (none sent before: empty is not unknown)
	reports  []wire.Listener
	changed  chan struct{} // closed and replaced on every change
	stream   *offerStream  // the newest offer stream: requests go there
	lastBoot string
	seq      uint64
	pending  map[string]pendingReq // request id → what was asked
	opens    map[string]Open       // preview id → open
	closes   []wire.Request        // host-decided closes not yet sent
	conns    map[*upstream]struct{}
}

type pendingReq struct {
	req      wire.Request
	typ      string
	reannoun string // a re-announced open: the previous gateway's preview id
	answer   chan wire.Answer
	deadline time.Time // past it the request is forgotten (expirePendingLocked)
}

// upstream is one spliced CONNECT, so a port leaving the allowlist can end it.
type upstream struct {
	port int
	conn net.Conn
}

// NewHost builds a Host from hc.
func NewHost(hc HostConfig) *Host {
	if hc.Poll <= 0 {
		hc.Poll = 5 * time.Second
	}
	if hc.AnswerWait <= 0 {
		hc.AnswerWait = 5 * time.Second
	}
	if hc.Logger == nil {
		hc.Logger = log.Default()
	}
	if hc.dial == nil {
		hc.dial = dialLoopback
	}
	return &Host{
		hc:      hc,
		scanner: &Scanner{Proc: hc.Proc, UID: hc.UID, Procs: hc.Procs, Now: hc.Now},
		logger:  hc.Logger,
		cfg:     hc.Config,
		changed: make(chan struct{}),
		pending: map[string]pendingReq{},
		opens:   map[string]Open{},
		conns:   map[*upstream]struct{}{},
	}
}

func (h *Host) broadcastLocked() {
	close(h.changed)
	h.changed = make(chan struct{})
}

// readWindows lists the windows (HostConfig.Windows). A failed read keeps
// the last listing: a tmux hiccup is not every window closing.
func (h *Host) readWindows(ctx context.Context) {
	if h.hc.Windows == nil {
		return
	}
	ws, err := h.hc.Windows(ctx)
	if err != nil {
		h.logger.Printf("muster-mirror: previews: list windows (keeping the last list): %v", err)
		return
	}
	ids := make([]string, 0, len(ws))
	panes := map[int]string{}
	for _, w := range ws {
		ids = append(ids, w.ID)
		for _, p := range w.PanePIDs {
			panes[p] = w.ID
		}
	}
	sort.Strings(ids)
	ids = slices.Compact(ids)
	h.mu.Lock()
	defer h.mu.Unlock()
	// Every listing wakes the offer stream, which sends it only if it
	// differs from what that stream last sent (serveOffer): one comparison,
	// in one place.
	h.panes, h.windows, h.haveWins = panes, ids, true
	h.broadcastLocked()
}

// maxConns is the maxConns a tunnel is served with now, and the channel
// closed at the next change.
func (h *Host) maxConns() (int, <-chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := h.cfg.MaxConns
	if n <= 0 {
		n = wire.MaxConns
	}
	return n, h.changed
}

// config is the allowlist in force, false while previews are off.
func (h *Host) config() (Config, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg, h.disabled == ""
}

// poll re-reads previews.json, lists the windows and scans the listeners,
// every Poll until ctx ends.
func (h *Host) poll(ctx context.Context) {
	t := time.NewTicker(h.hc.Poll)
	defer t.Stop()
	h.readWindows(ctx)
	h.scan()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.reload()
			h.readWindows(ctx)
			h.scan()
			h.mu.Lock()
			h.expirePendingLocked()
			h.mu.Unlock()
		}
	}
}

// reload re-reads previews.json. A change is applied at once: a bad file turns
// previews off (every open preview closes), and a port that left the list
// loses its previews and its live connections.
func (h *Host) reload() {
	if h.hc.File == "" {
		return
	}
	c, err := Load(h.hc.File)
	h.mu.Lock()
	defer h.mu.Unlock()
	if err != nil {
		// 🔴 The reason goes to the gateway (wire.Config.Disabled): the
		// file's absolute path — a home directory, a user name — is
		// replaced by its base name. The log keeps the full error.
		why := strings.ReplaceAll(err.Error(), h.hc.File, FileName)
		if h.disabled == why {
			return
		}
		h.disabled = why
		h.logger.Printf("muster-mirror: previews are OFF until previews.json is fixed: %v", err)
		h.closeWhereLocked(func(Open) bool { return true }, "previews are off")
		h.broadcastLocked()
		return
	}
	if h.disabled == "" && sameConfig(c, h.cfg) {
		return
	}
	was := h.disabled
	h.cfg, h.disabled = c, ""
	if was != "" {
		h.logger.Printf("muster-mirror: previews are back on (ports %s)", c.Ports)
	} else {
		h.logger.Printf("muster-mirror: previews.json changed: ports %s, maxOpen %d, maxConns %d", c.Ports, c.MaxOpen, c.MaxConns)
	}
	h.closeWhereLocked(func(o Open) bool { return c.Allows(o.Port, h.hc.Refused) != nil }, "the port left the allowlist")
	h.broadcastLocked()
}

// closeWhereLocked closes every open preview gone (and ends every live
// connection whose port is no longer allowed): the gateway is told on the
// offer stream, now or at its next connect.
func (h *Host) closeWhereLocked(gone func(Open) bool, why string) {
	ids := make([]string, 0, len(h.opens))
	for id := range h.opens {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		o := h.opens[id]
		if !gone(o) {
			continue
		}
		delete(h.opens, id)
		h.seq++
		h.closes = append(h.closes, wire.Request{ID: "c" + strconv.FormatUint(h.seq, 10), Window: o.Window, Port: o.Port, Preview: id})
		h.logger.Printf("muster-mirror: previews: closing preview %s on port %d: %s", id, o.Port, why)
	}
	for u := range h.conns {
		if h.disabled != "" || h.cfg.Allows(u.port, h.hc.Refused) != nil {
			u.conn.Close()
		}
	}
}

func sameConfig(a, b Config) bool {
	return a.Ports.String() == b.Ports.String() && a.Offer == b.Offer && a.MaxOpen == b.MaxOpen && a.MaxConns == b.MaxConns
}

// scan reads the listener reports and publishes them.
func (h *Host) scan() {
	h.mu.Lock()
	cfg, off, panes, have := h.cfg, h.disabled != "", h.panes, h.haveWins
	h.mu.Unlock()
	var out []wire.Listener
	if !off && have && cfg.Offer && Supported() == nil {
		reports, err := h.scanner.Scan(cfg, h.hc.Refused, panes)
		if err != nil {
			h.logger.Printf("muster-mirror: previews: read listeners: %v", err)
			return
		}
		for _, r := range reports {
			out = append(out, wire.Listener{Port: r.Port, Process: r.Process, Window: r.Window, FirstSeen: r.FirstSeen.UTC().Format(time.RFC3339)})
		}
	}
	// Like readWindows, every scan wakes the offer stream, which sends the
	// reports only when they differ from what it last sent.
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reports = out
	h.broadcastLocked()
}

// Opens is the previews the gateway has acknowledged open, sorted by id.
func (h *Host) Opens() []Open {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Open, 0, len(h.opens))
	for _, o := range h.opens {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Preview < out[j].Preview })
	return out
}

// WindowSummary is what one window has to preview now (Summary).
type WindowSummary struct {
	// Offered is the window's listener reports with no open preview on their
	// port: the offers the gateway's session page lists (it lists an open
	// port as its preview, not again as an offer).
	Offered int
	// Open is the window's open previews.
	Open int
}

// Summary is, per tmux window id, what the host has to preview there NOW —
// what muster-mirror reports to muster so the session views show their
// Preview button only while there is something to open (docs/previews.md).
// Windows with nothing are absent.
//
// 🔴 NIL WHILE NO GATEWAY TUNNEL SERVES AN OFFER STREAM: without one the
// session page can show nothing but "the host is offline", so there is
// nothing to offer from muster. The opens the host keeps across a dropped
// tunnel (to re-announce them) come back with the tunnel. And nil while
// previews are off (every open preview was closed and no listener is
// reported then anyway).
func (h *Host) Summary() map[string]WindowSummary {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stream == nil || h.disabled != "" {
		return nil
	}
	open := map[string]map[int]bool{}
	out := map[string]WindowSummary{}
	for _, o := range h.opens {
		if open[o.Window] == nil {
			open[o.Window] = map[int]bool{}
		}
		open[o.Window][o.Port] = true
		s := out[o.Window]
		s.Open++
		out[o.Window] = s
	}
	for _, l := range h.reports {
		if open[l.Window][l.Port] {
			continue
		}
		s := out[l.Window]
		s.Offered++
		out[l.Window] = s
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// RequestOpen writes an agent's open on the offer stream and waits for the
// gateway's answer (AnswerWait). req.ID is assigned here. The answer's Error
// is the gateway's tool error ("max_open", …); an acknowledged open is
// recorded (Opens).
func (h *Host) RequestOpen(ctx context.Context, req wire.Request) (wire.Answer, error) {
	return h.request(ctx, wire.TypeOpen, req)
}

// RequestClose writes an agent's close of req.Preview and waits for the
// gateway's answer.
func (h *Host) RequestClose(ctx context.Context, req wire.Request) (wire.Answer, error) {
	return h.request(ctx, wire.TypeClose, req)
}

func (h *Host) request(ctx context.Context, typ string, req wire.Request) (wire.Answer, error) {
	ch := make(chan wire.Answer, 1)
	h.mu.Lock()
	st := h.stream
	if st == nil {
		h.mu.Unlock()
		return wire.Answer{}, ErrGatewayNotConnected
	}
	h.expirePendingLocked()
	if h.requestsPendingLocked() >= maxPending {
		h.mu.Unlock()
		return wire.Answer{}, ErrGatewayTimeout
	}
	h.seq++
	req.ID = "r" + strconv.FormatUint(h.seq, 10)
	h.pending[req.ID] = pendingReq{req: req, typ: typ, answer: ch, deadline: h.now().Add(h.hc.AnswerWait)}
	h.mu.Unlock()
	if !st.send(wire.Offer{Type: typ, Request: &req}) {
		h.forget(req.ID)
		return wire.Answer{}, ErrGatewayNotConnected
	}
	t := time.NewTimer(h.hc.AnswerWait)
	defer t.Stop()
	select {
	case a := <-ch:
		return a, nil
	case <-t.C:
		// Abandoned, not forgotten: a late "opened" is then closed again
		// (serveAnswer), since the tool has already said gateway_timeout.
		h.abandon(req.ID)
		return wire.Answer{}, ErrGatewayTimeout
	case <-ctx.Done():
		h.abandon(req.ID)
		return wire.Answer{}, context.Cause(ctx)
	}
}

// abandon keeps a request nobody waits for any more for lateGrace.
func (h *Host) abandon(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if p, ok := h.pending[id]; ok {
		p.answer, p.deadline = nil, h.now().Add(lateGrace)
		h.pending[id] = p
	}
}

// requestsPendingLocked counts the agent requests pending (not re-announces).
func (h *Host) requestsPendingLocked() int {
	n := 0
	for _, p := range h.pending {
		if p.reannoun == "" {
			n++
		}
	}
	return n
}

// queueCloseLocked asks the gateway to close a preview the host will not
// keep (an "opened" it refused, or one that came too late), on the current
// offer stream now or at the next connect. A malformed id or window cannot
// be named in a close and is only logged; the queue is bounded by
// maxPending.
func (h *Host) queueCloseLocked(preview, window string, port int, why string) {
	if !validPreviewID(preview) || !validWindowID(window) {
		h.logger.Printf("muster-mirror: previews: cannot close a malformed open from the gateway (%s)", why)
		return
	}
	if len(h.closes) >= maxPending {
		h.logger.Printf("muster-mirror: previews: close queue full; not closing preview %s (%s)", preview, why)
		return
	}
	h.seq++
	h.closes = append(h.closes, wire.Request{ID: "c" + strconv.FormatUint(h.seq, 10), Window: window, Port: port, Preview: preview})
	h.logger.Printf("muster-mirror: previews: closing preview %s: %s", preview, why)
	h.broadcastLocked()
}

func (h *Host) now() time.Time {
	if h.hc.Now != nil {
		return h.hc.Now()
	}
	return time.Now()
}

// expirePendingLocked forgets every request past its deadline. An
// unanswered re-announce also drops its open: the restarted gateway never
// confirmed it, so it is not open there.
func (h *Host) expirePendingLocked() {
	now := h.now()
	for id, p := range h.pending {
		if now.Before(p.deadline) {
			continue
		}
		delete(h.pending, id)
		if p.reannoun != "" {
			delete(h.opens, p.reannoun)
			h.logger.Printf("muster-mirror: previews: dropped preview %s: the restarted gateway never answered its re-announce", p.reannoun)
		}
	}
}

func (h *Host) forget(id string) {
	h.mu.Lock()
	delete(h.pending, id)
	h.mu.Unlock()
}

// Handler is the tunnel's handler: CONNECT (connect.go), GET wire.OfferPath,
// POST wire.OpenedPath and wire.ClosedPath. Anything else is 404.
func (h *Host) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodConnect:
			h.serveConnect(w, r)
		case r.URL.Path == wire.OfferPath && r.Method == http.MethodGet:
			h.serveOffer(w, r)
		case (r.URL.Path == wire.OpenedPath || r.URL.Path == wire.ClosedPath) && r.Method == http.MethodPost:
			h.serveAnswer(w, r, r.URL.Path == wire.OpenedPath)
		default:
			http.NotFound(w, r)
		}
	})
}
