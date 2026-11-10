package previewhost

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/ZacxDev/muster/internal/preview/tunnel"
	"github.com/ZacxDev/muster/internal/preview/wire"
)

// loopbackDialTimeout bounds each loopback dial of a CONNECT.
const loopbackDialTimeout = 5 * time.Second

// loopbacks are the addresses a CONNECT dials, in order: 127.0.0.1, then ::1
// (a dev server may bind "localhost" to ::1 only). 🔴 NEVER A NAME AND NEVER
// THE REQUESTED ADDRESS: the CONNECT's authority contributes its port alone
// (wire.ConnectPort).
var loopbacks = []string{"127.0.0.1", "::1"}

// serveConnect answers one CONNECT stream (docs/design/previews.md, "What the
// host checks before it dials"): the port alone is taken from the authority;
// it must be allowed (Config.Allows, the always-refused ports first) and,
// where this host can tell (Supported), held by this user's listener
// (CheckListener, which fails closed on an unreadable row). Then 127.0.0.1,
// then ::1, is dialled and the stream spliced to it. Refusals: 403
// port_not_allowed | foreign_listener, 502 refused. A CONNECT past maxConns
// never gets here: tunnel.Serve answers it 429 max_conns.
func (h *Host) serveConnect(w http.ResponseWriter, r *http.Request) {
	port, err := wire.ConnectPort(r)
	if err != nil {
		http.Error(w, "bad CONNECT", http.StatusBadRequest)
		return
	}
	if reason, why := h.check(port); reason != "" {
		h.logger.Printf("muster-mirror: previews: refused a connection to port %d: %s (%s)", port, reason, why)
		wire.WriteRefusal(w, reason)
		return
	}
	up, err := h.hc.dial(r.Context(), port)
	if err != nil {
		h.logger.Printf("muster-mirror: previews: refused a connection to port %d: %s (%v)", port, wire.ReasonRefused, err)
		wire.WriteRefusal(w, wire.ReasonRefused)
		return
	}
	u := &upstream{port: port, conn: up}
	h.mu.Lock()
	// Re-checked under the lock that a reload takes: a port that left the
	// list between the check and here is not spliced.
	if h.disabled != "" || h.cfg.Allows(port, h.hc.Refused) != nil {
		h.mu.Unlock()
		up.Close()
		wire.WriteRefusal(w, wire.ReasonPortNotAllowed)
		return
	}
	h.conns[u] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.conns, u)
		h.mu.Unlock()
	}()
	tunnel.Splice(w, r, up)
}

// check is the host's refusal for port, "" when it may be dialled.
func (h *Host) check(port int) (wire.Reason, string) {
	cfg, on := h.config()
	if !on {
		return wire.ReasonPortNotAllowed, "previews are off"
	}
	if err := cfg.Allows(port, h.hc.Refused); err != nil {
		return wire.ReasonPortNotAllowed, err.Error()
	}
	if Supported() != nil {
		return "", "" // the foreign_listener check is off here (UNVERIFIED off Linux)
	}
	l, err := ReadListeners(h.scanner.root())
	if err != nil {
		// FAIL CLOSED: whose listener holds the port cannot be read.
		return wire.ReasonForeignListener, err.Error()
	}
	var ref *Refusal
	switch err := CheckListener(l, port, h.hc.UID); {
	case err == nil:
		return "", ""
	case errors.As(err, &ref) && ref.Code == CodeNotListening:
		return wire.ReasonRefused, ref.Why
	case errors.As(err, &ref) && ref.Code == CodeForeignListener:
		return wire.ReasonForeignListener, ref.Why
	default:
		return wire.ReasonForeignListener, err.Error()
	}
}

// dialLoopback dials port on 127.0.0.1, then on ::1.
func dialLoopback(ctx context.Context, port int) (net.Conn, error) {
	d := net.Dialer{Timeout: loopbackDialTimeout}
	var errs []error
	for _, a := range loopbacks {
		c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(a, strconv.Itoa(port)))
		if err == nil {
			return c, nil
		}
		errs = append(errs, err)
	}
	return nil, errors.Join(errs...)
}
