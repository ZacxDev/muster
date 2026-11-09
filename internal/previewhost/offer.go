package previewhost

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strconv"

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
			closes := h.closes
			h.closes = nil
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
// naming the previous gateway's preview id; the answer replaces it.
func (h *Host) reannounceLocked() []wire.Offer {
	var out []wire.Offer
	for _, o := range h.sortedOpensLocked() {
		h.seq++
		req := wire.Request{ID: "a" + strconv.FormatUint(h.seq, 10), Window: o.Window, Port: o.Port, Label: o.Label, Path: o.Path, Preview: o.Preview}
		h.pending[req.ID] = pendingReq{req: req, typ: wire.TypeOpen, reannoun: o.Preview}
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

// maxAnswerBytes bounds an Answer body.
const maxAnswerBytes = 64 << 10

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
	p, known := h.pending[a.RequestID]
	if known {
		delete(h.pending, a.RequestID)
	}
	switch {
	case opened && known:
		if p.reannoun != "" {
			delete(h.opens, p.reannoun)
		}
		if a.Error == "" && a.Preview != "" {
			h.opens[a.Preview] = Open{Preview: a.Preview, Window: p.req.Window, Port: p.req.Port, Label: p.req.Label, Path: p.req.Path}
		}
	case opened && a.RequestID == "":
		if a.Error == "" && a.Preview != "" && a.Window != "" && a.Port > 0 {
			h.opens[a.Preview] = Open{Preview: a.Preview, Window: a.Window, Port: a.Port}
		}
	case !opened:
		// A close by the agent's request, or one the gateway made itself
		// (its lifecycle, the owner's Close).
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
	w.WriteHeader(http.StatusNoContent)
}
