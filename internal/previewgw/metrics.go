package previewgw

// metrics.go — the gateway's Prometheus metrics, on its own registry
// (/metrics on the mesh listener), and the closed vocabularies their labels
// are drawn from (docs/design/previews.md, "Telemetry").
//
// 🔴 EVERY LABEL IS A CLOSED VOCABULARY. No preview id, host name, owner,
// window, port, path, query, header or error text is ever a label: each would
// be an unbounded series, and the first five name somebody's private work. A
// value outside a vocabulary is counted as "other" (refusals) or dropped from
// the label set by construction (the kinds below are constants).

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ZacxDev/muster/internal/preview/wire"
)

// The design's label values that exist today. kind "pod" (phase 2) and auth
// "share" (phase 3) arrive with the code that serves them.
const (
	kindHost  = "host"
	authOwner = "owner"
)

// Why a preview closed: muster_preview_closes_total{reason}.
const (
	closeHost       = "host"             // the host's preview_close
	closePage       = "page"             // Close on the session page
	closeIdle       = "idle"             // PREVIEW_IDLE with no request
	closeTTL        = "ttl"              // PREVIEW_TTL after it opened
	closeWindowGone = "window_gone"      // its window gone from the host's listing for WindowGoneAfter
	closeRevoked    = "host_revoked"     // its host's token is gone
	closeOwnerGone  = "owner_not_served" // its owner's plugin token no longer holds viewer
)

// Who opened a preview: muster_preview_opens_total{opener}.
const (
	openerAgent = "agent" // preview_open on the offer stream
	openerPage  = "page"  // Open on an offer, on the session page
)

// refusalOther is the refusals label of a code outside refusalCodes.
const refusalOther = "other"

// refusalCodes is the refusals label vocabulary: every code the gateway can
// refuse or fail a p- request or an open with. A host's CONNECT refusal is
// already one of wire's four Reasons (wire.RefusalOf), but nothing reaching
// a label is trusted to be: refusalLabel maps anything else to "other".
var refusalCodes = func() map[string]bool {
	m := map[string]bool{}
	for c := range codePages {
		m[c] = true
	}
	for _, c := range []string{codeMaxOpen, codeNotListening, codeProbeFailed, codeNoSuchOffer,
		"bad_port", "bad_path", "bad_window", "bad_session", "internal",
		string(wire.ReasonRefused)} {
		m[c] = true
	}
	return m
}()

func refusalLabel(code string) string {
	if refusalCodes[code] {
		return code
	}
	return refusalOther
}

// codeClass is an HTTP status's class label ("2xx"), "other" outside 100–599.
func codeClass(status int) string {
	if status < 100 || status > 599 {
		return "other"
	}
	return strconv.Itoa(status/100) + "xx"
}

// Dial outcomes: muster_preview_dial_seconds{outcome} and the preview.dial
// span. A host's refusal is its wire.Reason (four values).
const (
	dialOK          = "ok"
	dialHostOffline = "host_offline"
	dialTimeout     = "timeout"
	dialCanceled    = "canceled"
	dialError       = "error"
)

type gwMetrics struct {
	tunnels   *prometheus.GaugeVec
	open      *prometheus.GaugeVec
	opens     *prometheus.CounterVec
	closes    *prometheus.CounterVec
	refusals  *prometheus.CounterVec
	requests  *prometheus.CounterVec
	responses *prometheus.HistogramVec
	dials     *prometheus.HistogramVec
}

// Latency buckets, seconds: a dial on the mesh is milliseconds, the public
// path adds ~100 ms, and the upstream-headers limit is 60 s.
var latencyBuckets = []float64{0.005, 0.025, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

func newMetrics(reg *prometheus.Registry) *gwMetrics {
	m := &gwMetrics{
		tunnels: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "muster_preview_tunnels",
			Help: "Live host tunnels, draining ones included, by the listener they arrived on.",
		}, []string{"path"}),
		open: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "muster_previews_open",
			Help: "Open previews, by kind (host).",
		}, []string{"kind"}),
		opens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "muster_preview_opens_total",
			Help: "Previews opened, by opener (agent: preview_open; page: Open on an offer).",
		}, []string{"opener"}),
		closes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "muster_preview_closes_total",
			Help: "Previews closed, by reason (host, page, idle, ttl, window_gone, host_revoked, owner_not_served).",
		}, []string{"reason"}),
		refusals: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "muster_preview_refusals_total",
			Help: "Preview requests and opens the gateway refused or failed itself, by code.",
		}, []string{"reason"}),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "muster_preview_requests_total",
			Help: "Admitted preview requests (WebSockets included), by kind, auth and response status class.",
		}, []string{"kind", "auth", "code_class"}),
		responses: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "muster_preview_response_seconds",
			Help:    "Time from an admitted request to the app's response headers, by kind.",
			Buckets: latencyBuckets,
		}, []string{"kind"}),
		dials: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "muster_preview_dial_seconds",
			Help:    "Time to open an upstream stream on a host's tunnel, by kind and outcome.",
			Buckets: latencyBuckets,
		}, []string{"kind", "outcome"}),
	}
	reg.MustRegister(m.tunnels, m.open, m.opens, m.closes, m.refusals, m.requests, m.responses, m.dials)
	// The series a dashboard needs to read 0 rather than "absent" exist from
	// the start.
	for _, p := range []Path{PathMesh, PathPublic} {
		m.tunnels.WithLabelValues(string(p))
	}
	m.open.WithLabelValues(kindHost)
	return m
}

func (m *gwMetrics) refused(code string) { m.refusals.WithLabelValues(refusalLabel(code)).Inc() }

func (m *gwMetrics) opened(opener string) {
	m.opens.WithLabelValues(opener).Inc()
	m.open.WithLabelValues(kindHost).Inc()
}

func (m *gwMetrics) closed(reason string) {
	m.closes.WithLabelValues(reason).Inc()
	m.open.WithLabelValues(kindHost).Dec()
}

func (m *gwMetrics) dialed(d time.Duration, outcome string) {
	m.dials.WithLabelValues(kindHost, outcome).Observe(d.Seconds())
}
