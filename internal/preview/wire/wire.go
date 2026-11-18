// Package wire is the preview tunnel's protocol, shared by the gateway
// (cmd/muster-preview), the host (muster-mirror -preview) and, in phase 2, an
// agent pod's gate, so the three cannot disagree about it. See
// docs/design/previews.md ("The host tunnel").
//
// 🔴 IT IMPORTS ONLY THE STANDARD LIBRARY. The pod's supervisor (cmd/ccd) will
// link it, and cmd/ccd must not link a websocket client
// (internal/modulegate/leafdeps_test.go pins both). The transport — the
// WebSocket carrying HTTP/2 — lives in the sibling package
// internal/preview/tunnel; nothing here may import it.
//
// The shape, in one place:
//
//   - The host dials the gateway's HostPath with VersionHeader, HostNameHeader
//     and "Authorization: Bearer mpt_…". A version the gateway does not speak
//     is refused with VersionRangeHeader naming the range it does.
//   - Over that WebSocket the host is the HTTP/2 SERVER and the gateway the
//     client. The gateway's first stream is GET OfferPath: a long-lived
//     response on which the host writes newline-delimited Offer messages
//     (offer.go). The gateway answers the host's requests with POST OpenedPath
//     and POST ClosedPath.
//   - Each upstream TCP connection is one CONNECT stream whose authority is
//     127.0.0.1:<port>. The host uses ONLY the port (ConnectPort), and either
//     answers 200 and copies bytes or refuses with a Reason (WriteRefusal).
package wire

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Version is the preview protocol version this side speaks, and
// [MinVersion, MaxVersion] the range it accepts. One version exists.
const (
	Version    = 1
	MinVersion = 1
	MaxVersion = 1
)

// Header names and paths of the protocol.
const (
	// VersionHeader carries the version the host speaks on its tunnel dial.
	VersionHeader = "X-Muster-Preview-Version"
	// VersionRangeHeader is set on a refusal of an unspoken version:
	// "<min>-<max>" (VersionRange). Its presence is what tells a host the
	// refusal was about the version, not its token.
	VersionRangeHeader = "X-Muster-Preview-Version-Range"
	// HostNameHeader names the host on its tunnel dial; the gateway binds it
	// to the host token's registered name.
	HostNameHeader = "X-Muster-Preview-Host"
	// GatewayBootHeader is on the gateway's GET OfferPath: an id that changes
	// each time the gateway process starts. A host that sees a new one
	// re-announces the opens the previous gateway had acknowledged (that
	// gateway's previews were in memory and are gone).
	GatewayBootHeader = "X-Muster-Preview-Gateway-Boot"
	// ReasonHeader carries a CONNECT refusal's Reason.
	ReasonHeader = "X-Muster-Preview-Reason"

	// TokenPrefix begins every host token (stored hashed by the gateway).
	TokenPrefix = "mpt_"

	HostPath   = "/v1/host"   // the host's tunnel WebSocket, on the gateway
	OfferPath  = "/v1/offer"  // gateway → host, GET, long-lived NDJSON response
	OpenedPath = "/v1/opened" // gateway → host, POST, an Answer
	ClosedPath = "/v1/closed" // gateway → host, POST, an Answer
)

// MaxConns is the cap on a host's maxConns, and its default: the number of
// UPSTREAM connections (CONNECT streams) a host's tunnel carries at once. The
// control streams are on top of it (internal/preview/tunnel.ControlStreams).
// Phase 1 caps it at 64, below the design's 256, so the gateway's connection
// window — sized from this cap in internal/preview/tunnel — stays
// (64+4+1) × 4 MiB. Raising it raises that window and the gateway memory
// paused readers can hold; the invariant test in internal/preview/tunnel pins
// the numbers together.
const MaxConns = 64

// PingInterval is how often each end of a tunnel sends a WebSocket ping. It
// must stay well under the shortest idle timeout on any path a tunnel takes
// (the public edge and the overlay mesh's relays both cut quiet connections).
const PingInterval = 30 * time.Second

// VersionRange is the value of VersionRangeHeader for this side.
func VersionRange() string { return fmt.Sprintf("%d-%d", MinVersion, MaxVersion) }

// AcceptVersion reports whether a VersionHeader value names a version this
// side speaks, and which. Only a plain decimal integer is a version.
func AcceptVersion(h string) (int, bool) {
	if h == "" || strings.TrimLeft(h, "0123456789") != "" || len(h) > 4 {
		return 0, false
	}
	v, err := strconv.Atoi(h)
	if err != nil || v < MinVersion || v > MaxVersion {
		return 0, false
	}
	return v, true
}

// Reason is why a host refused a CONNECT. Each has exactly one status.
type Reason string

const (
	ReasonPortNotAllowed  Reason = "port_not_allowed" // 403: the port is not in previews.json
	ReasonForeignListener Reason = "foreign_listener" // 403: the listener is another user's
	ReasonRefused         Reason = "refused"          // 502: nothing is listening (both loopbacks)
	// ReasonMaxConns (429): all of the host's maxConns upstream slots stayed
	// in use. The GATEWAY emits it, when a Dial has waited its bound for a
	// free slot (internal/preview/tunnel, ClientOptions.DialWait); the host
	// sends it only as a backstop, for a CONNECT that arrives past its
	// maxConns anyway, and the gateway retries that within the same wait.
	ReasonMaxConns Reason = "max_conns"
)

var reasonStatus = map[Reason]int{
	ReasonPortNotAllowed:  http.StatusForbidden,
	ReasonForeignListener: http.StatusForbidden,
	ReasonRefused:         http.StatusBadGateway,
	ReasonMaxConns:        http.StatusTooManyRequests,
}

// Status is r's HTTP status, or 0 for a value that is not a Reason.
func (r Reason) Status() int { return reasonStatus[r] }

// WriteRefusal answers a CONNECT with r's status and ReasonHeader. It panics
// on a value that is not a Reason: a refusal with no status is a bug in the
// caller, not something to send.
func WriteRefusal(w http.ResponseWriter, r Reason) {
	st := r.Status()
	if st == 0 {
		panic(fmt.Sprintf("wire: %q is not a refusal reason", string(r)))
	}
	w.Header().Set(ReasonHeader, string(r))
	w.WriteHeader(st)
}

// RefusalOf reads a CONNECT response's refusal: its Reason when the status and
// ReasonHeader agree, else "" (an unexplained failure, e.g. a 400 for a bad
// authority, or a status the reason does not map to).
func RefusalOf(status int, h http.Header) Reason {
	r := Reason(h.Get(ReasonHeader))
	if r.Status() == 0 || r.Status() != status {
		return ""
	}
	return r
}

// ErrNotConnect is ConnectPort's error for a request that is not a CONNECT.
var ErrNotConnect = errors.New("wire: not a CONNECT request")

// ConnectPort is the port a CONNECT asks for. 🔴 IT RETURNS ONLY THE PORT: the
// authority's host part is ignored, never resolved and never dialed, because
// the host dials loopback and nothing else whatever the gateway sends. The
// port must be plain decimal, 1–65535.
func ConnectPort(r *http.Request) (int, error) {
	if r.Method != http.MethodConnect {
		return 0, ErrNotConnect
	}
	_, ps, err := net.SplitHostPort(r.Host)
	if err != nil {
		return 0, fmt.Errorf("wire: bad CONNECT authority %q: %w", r.Host, err)
	}
	if ps == "" || len(ps) > 5 || strings.TrimLeft(ps, "0123456789") != "" {
		return 0, fmt.Errorf("wire: bad CONNECT port %q", ps)
	}
	p, err := strconv.Atoi(ps)
	if err != nil || p < 1 || p > 65535 {
		return 0, fmt.Errorf("wire: CONNECT port %q out of range", ps)
	}
	return p, nil
}

// ConnectAuthority is the authority the gateway sends for port.
func ConnectAuthority(port int) string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}
