package previewhost

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/preview/wire"
)

// The offer stream (docs/design/previews.md, "The host tunnel", step 3): the
// gateway's long-lived GET wire.OfferPath, on which the host writes its
// allowlist and caps, its window listing, its listener reports and the agent
// tools' open/close requests — the first three only when they CHANGED.

// offerStream is one GET wire.OfferPath; requests are queued to the newest.
type offerStream struct {
	out  chan wire.Offer
	done chan struct{}
}

// send queues m, false when the stream is gone or its queue is full.
func (s *offerStream) send(m wire.Offer) bool {
	select {
	case <-s.done:
		return false
	default:
	}
	select {
	case s.out <- m:
		return true
	case <-s.done:
		return false
	default:
		return false
	}
}

// snapshot is what the offer stream mirrors: each nil until known.
type snapshot struct {
	cfg       wire.Config
	windows   []string
	haveWins  bool
	listeners []wire.Listener
}

func (h *Host) snapshotLocked() snapshot {
	s := snapshot{windows: h.windows, haveWins: h.haveWins, listeners: h.reports}
	if h.disabled != "" {
		s.cfg = wire.Config{Ports: []string{}, Disabled: h.disabled}
		return s
	}
	ports := []string{}
	for _, r := range h.cfg.Ports.Ranges() {
		ports = append(ports, r.String())
	}
	s.cfg = wire.Config{Ports: ports, Offer: h.cfg.Offer, MaxOpen: h.cfg.MaxOpen, MaxConns: h.cfg.MaxConns}
	return s
}

func sameWireConfig(a, b wire.Config) bool {
	return slices.Equal(a.Ports, b.Ports) && a.Offer == b.Offer && a.MaxOpen == b.MaxOpen && a.MaxConns == b.MaxConns && a.Disabled == b.Disabled
}

// serveOffer is GET wire.OfferPath. It writes the config first, then the
// window listing and listener reports (once known), then — when the
// gateway's boot id differs from the last one seen — re-announces every open
// the previous gateway acknowledged (its previews were in memory and are
// gone), and from then on writes each change and each request as it comes.
func (h *Host) serveOffer(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "cannot stream", http.StatusInternalServerError)
		return
	}
	st := &offerStream{out: make(chan wire.Offer, 64), done: make(chan struct{})}
	boot := r.Header.Get(wire.GatewayBootHeader)

	h.mu.Lock()
	var queued []wire.Offer
	if boot != "" && h.lastBoot != "" && boot != h.lastBoot {
		queued = h.reannounceLocked()
	}
	if boot != "" {
		h.lastBoot = boot
	}
	for _, c := range h.closes {
		c := c
		queued = append(queued, wire.Offer{Type: wire.TypeClose, Request: &c})
	}
	h.closes = nil
	h.stream = st
	snap, changed := h.snapshotLocked(), h.changed
	h.mu.Unlock()
	defer func() {
		close(st.done)
		h.mu.Lock()
		if h.stream == st {
			h.stream = nil
		}
		h.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	enc := wire.NewEncoder(w)
	var sent snapshot
	first := true
	write := func(s snapshot) error {
		if first || !sameWireConfig(s.cfg, sent.cfg) {
			c := s.cfg
			if err := enc.Encode(wire.Offer{Type: wire.TypeConfig, Config: &c}); err != nil {
				return err
			}
		}
		if s.haveWins && (!sent.haveWins || !slices.Equal(s.windows, sent.windows)) {
			if err := enc.Encode(wire.Offer{Type: wire.TypeWindows, Windows: s.windows}); err != nil {
				return err
			}
		}
		if (first && len(s.listeners) > 0) || (!first && !slices.Equal(s.listeners, sent.listeners)) {
			if err := enc.Encode(wire.Offer{Type: wire.TypeListeners, Listeners: s.listeners}); err != nil {
				return err
			}
		}
		first, sent = false, s
		return nil
	}
	if write(snap) != nil {
		return
	}
	for _, m := range queued {
		if enc.Encode(m) != nil {
			return
		}
	}
	fl.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case m := <-st.out:
			if enc.Encode(m) != nil {
				return
			}
			fl.Flush()
		case <-changed:
			h.mu.Lock()
			snap, changed = h.snapshotLocked(), h.changed
			// Host-decided closes go out on the CURRENT tunnel's stream
			// only: a replaced tunnel's stream, still draining, must not
			// take them to a connection the gateway is closing.
			var closes []wire.Request
			if h.stream == st {
				closes, h.closes = h.closes, nil
			}
			h.mu.Unlock()
			if write(snap) != nil {
				return
			}
			for _, c := range closes {
				c := c
				if enc.Encode(wire.Offer{Type: wire.TypeClose, Request: &c}) != nil {
					return
				}
			}
			fl.Flush()
		}
	}
}

// reannounceLocked builds an open request for every acknowledged open, each
// naming the previous gateway's preview id. The gateway's answer is
// authoritative: an opened answer replaces the id, an error or a closed
// answer drops the open, and no answer within AnswerWait drops it too
// (expirePendingLocked). An open older than MaxOpenAge is dropped, not
// re-announced. Opens are at most maxOpen (recordOpenLocked), so this is
// bounded too.
func (h *Host) reannounceLocked() []wire.Offer {
	var out []wire.Offer
	now := h.now()
	h.expirePendingLocked()
	// One re-announce per open at a time: a gateway restarting again (or a
	// GET with a new boot id, repeated) does not add another, so pending
	// stays at most maxOpen re-announces plus maxPending requests.
	inFlight := map[string]bool{}
	for _, p := range h.pending {
		if p.reannoun != "" {
			inFlight[p.reannoun] = true
		}
	}
	for _, o := range h.sortedOpensLocked() {
		if inFlight[o.Preview] {
			continue
		}
		// WALL time (Round(0) drops the monotonic reading): a host asleep
		// for a day has had its opens for a day.
		if now.Round(0).Sub(o.Opened.Round(0)) >= MaxOpenAge {
			delete(h.opens, o.Preview)
			h.logger.Printf("muster-mirror: previews: dropped preview %s: open longer than %v", o.Preview, MaxOpenAge)
			continue
		}
		h.seq++
		req := wire.Request{ID: "a" + strconv.FormatUint(h.seq, 10), Window: o.Window, Port: o.Port, Label: o.Label, Path: o.Path, Preview: o.Preview}
		h.pending[req.ID] = pendingReq{req: req, typ: wire.TypeOpen, reannoun: o.Preview, deadline: now.Add(h.hc.AnswerWait)}
		out = append(out, wire.Offer{Type: wire.TypeOpen, Request: &req})
	}
	return out
}

func (h *Host) sortedOpensLocked() []Open {
	out := make([]Open, 0, len(h.opens))
	for _, o := range h.opens {
		out = append(out, o)
	}
	slices.SortFunc(out, func(a, b Open) int {
		switch {
		case a.Preview < b.Preview:
			return -1
		case a.Preview > b.Preview:
			return 1
		}
		return 0
	})
	return out
}

// maxAnswerBytes bounds an Answer body: a few short fields.
const maxAnswerBytes = 4 << 10

// Bounds on what an answer may make the host record (recordOpenLocked).
const (
	// maxPreviewID: a preview id becomes the p-<id> host label, and a DNS
	// label is at most 63 bytes.
	maxPreviewID = 61
	// maxWindowID: a tmux window id is "@" and an unsigned int (at most 10
	// digits).
	maxWindowID = 11
)

func validPreviewID(s string) bool {
	if s == "" || len(s) > maxPreviewID {
		return false
	}
	for _, c := range []byte(s) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func validWindowID(s string) bool {
	if len(s) < 2 || len(s) > maxWindowID || s[0] != '@' {
		return false
	}
	return strings.TrimLeft(s[1:], "0123456789") == ""
}

// recordOpenLocked records o if the host would serve it: previews on, a
// well-formed id and window, its port allowed now, and — for an id not
// already open — fewer than maxOpen opens. 🔴 THIS IS THE ONE PLACE AN OPEN
// IS RECORDED, so the gateway (whatever it sends, and however often) can
// never make the host hold more than maxOpen small records. The error is the
// refusal, as a tool error code.
func (h *Host) recordOpenLocked(o Open) string {
	switch {
	case h.disabled != "":
		return "previews_off"
	case !validPreviewID(o.Preview) || !validWindowID(o.Window):
		return "bad_answer"
	case h.cfg.Allows(o.Port, h.hc.Refused) != nil:
		return CodePortNotAllowed
	}
	if _, replacing := h.opens[o.Preview]; !replacing && len(h.opens) >= h.cfg.MaxOpen {
		return "max_open"
	}
	if o.Opened.IsZero() {
		o.Opened = h.now()
	}
	h.opens[o.Preview] = o
	return ""
}

// serveAnswer is POST wire.OpenedPath (opened) or wire.ClosedPath: the
// gateway's answer to a request (RequestID set) or, for an open the owner
// made by clicking an offer, its unsolicited notice (RequestID empty).
func (h *Host) serveAnswer(w http.ResponseWriter, r *http.Request, opened bool) {
	b, err := io.ReadAll(io.LimitReader(r.Body, maxAnswerBytes+1))
	if err != nil || len(b) > maxAnswerBytes {
		http.Error(w, "bad answer", http.StatusBadRequest)
		return
	}
	var a wire.Answer
	if err := json.Unmarshal(b, &a); err != nil {
		http.Error(w, "bad answer", http.StatusBadRequest)
		return
	}
	h.mu.Lock()
	h.expirePendingLocked()
	p, known := h.pending[a.RequestID]
	if known {
		delete(h.pending, a.RequestID)
	}
	refused := ""
	switch {
	// 🔴 An "opened" the host does not keep — refused, too late for an agent
	// that gave up, or for no request the host knows — is closed again:
	// otherwise the gateway would keep a preview the host has forgotten (and
	// would never close when its port leaves the list).
	case opened && known:
		var old Open
		if p.reannoun != "" {
			old = h.opens[p.reannoun]
			delete(h.opens, p.reannoun)
		}
		if a.Error == "" {
			if p.answer == nil && p.reannoun == "" {
				refused = "late"
			} else {
				refused = h.recordOpenLocked(Open{Preview: a.Preview, Window: p.req.Window, Port: p.req.Port, Label: p.req.Label, Path: p.req.Path, Opened: old.Opened})
			}
			if refused != "" {
				a.Error = refused // the tool sees why the host would not keep it
				h.queueCloseLocked(a.Preview, p.req.Window, p.req.Port, "the host did not keep it: "+refused)
			}
		}
	case opened && a.RequestID == "":
		// The owner clicked an offer: the gateway tells the host unasked.
		if a.Error == "" {
			if refused = h.recordOpenLocked(Open{Preview: a.Preview, Window: a.Window, Port: a.Port}); refused != "" {
				h.queueCloseLocked(a.Preview, a.Window, a.Port, "the host did not keep it: "+refused)
			}
		}
	case opened:
		// An answer to no request the host knows (expired, or never made).
		if a.Error == "" {
			refused = "unknown_request"
			h.queueCloseLocked(a.Preview, a.Window, a.Port, "an answer to no known request")
		}
	case !opened:
		// A close by the agent's request, by the gateway itself (its
		// lifecycle, the owner's Close), or its answer to a re-announce:
		// authoritative, so the open goes either way.
		if known && p.reannoun != "" {
			delete(h.opens, p.reannoun)
		}
		id := a.Preview
		if id == "" && known {
			id = p.req.Preview
		}
		if a.Error == "" && id != "" {
			delete(h.opens, id)
		}
	}
	h.mu.Unlock()
	if known && p.answer != nil {
		p.answer <- a
	}
	if refused != "" {
		h.logger.Printf("muster-mirror: previews: refused to record an open from the gateway: %s", refused)
		http.Error(w, refused, http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
