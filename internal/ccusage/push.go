package ccusage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/metrics"
	"github.com/ZacxDev/muster/internal/noredirect"
)

// PushTimeout bounds one push, connection to answer.
const PushTimeout = 10 * time.Second

// MaxPushInstance bounds the pushed source_instance label.
const MaxPushInstance = 64

// Why a push failed: muster_cc_usage_push_failures_total's `reason`, a closed
// vocabulary (never a URL, a status code or an account).
const (
	PushReasonBuild     = "build"
	PushReasonTimeout   = "timeout"
	PushReasonTransport = "transport"
	PushReason4xx       = "http_4xx"
	PushReason5xx       = "http_5xx"
	PushReasonOther     = "http_other"
)

// CheckPushURL is why u cannot be the push target, or nil. It must be an
// absolute https URL with a host and no userinfo; plain http is allowed only to
// a loopback host (a test's server), since the request carries the bearer.
func CheckPushURL(u string) error {
	p, err := url.Parse(u)
	if err != nil || p.Host == "" || p.Hostname() == "" {
		return errors.New("want an absolute https URL")
	}
	if p.User != nil {
		return errors.New("credentials in the URL are refused: the token goes in its own variable")
	}
	switch p.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopback(p.Hostname()) {
			return nil
		}
		return errors.New("plain http is refused (the request carries the bearer token): want https")
	}
	return errors.New("want an absolute https URL")
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Pusher PUTs the readings to an external dashboard once per poll cycle
// (Poller.AfterPass). It holds no state across pushes: a failed push is logged
// and counted, and the next cycle's push carries the newer readings — there is
// no retry inside a cycle.
//
// 🔴 THE TOKEN IS IN ONE HEADER AND NOWHERE ELSE, and the body is never
// logged: a failure is logged as its class (a status, timeout, transport
// error), never the URL's query, the request or the answer's body.
type Pusher struct {
	URL   string
	Token string
	// Body is the request body for a push made at now: the readings exactly as
	// GET /api/cc-accounts/usage answers them, with the instance label and
	// read_at (api.Server.CCUsagePushBody).
	Body   func(ctx context.Context, now time.Time) ([]byte, error)
	Client *http.Client
	Logger *log.Logger
	Now    func() time.Time
	// Timeout bounds one push; 0 is PushTimeout.
	Timeout time.Duration
}

func (p *Pusher) timeout() time.Duration {
	if p.Timeout > 0 {
		return p.Timeout
	}
	return PushTimeout
}

func (p *Pusher) logf(format string, args ...any) {
	if p.Logger != nil {
		p.Logger.Printf(format, args...)
	}
}

func (p *Pusher) client() *http.Client {
	c := p.Client
	if c == nil {
		c = &http.Client{}
	}
	// No redirect is followed: one would re-send the bearer elsewhere.
	return noredirect.Client(c)
}

// Push makes one push and reports its failure reason ("" for a 204). It is
// Poller.AfterPass's shape once wrapped (PushAfterPass).
func (p *Pusher) Push(ctx context.Context) string {
	now := time.Now()
	if p.Now != nil {
		now = p.Now()
	}
	reason := p.push(ctx, now)
	if reason != "" {
		metrics.CCUsagePushFailures.WithLabelValues(reason).Inc()
	}
	return reason
}

func (p *Pusher) push(ctx context.Context, now time.Time) string {
	body, err := p.Body(ctx, now)
	if err != nil {
		p.logf("ccusage: push: the readings could not be assembled: %v", err)
		return PushReasonBuild
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, p.URL, bytes.NewReader(body))
	if err != nil {
		p.logf("ccusage: push: build the request")
		return PushReasonBuild
	}
	req.Header.Set("Authorization", "Bearer "+p.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "muster-ccusage")
	resp, err := p.client().Do(req)
	if err != nil {
		var nr *noredirect.Error
		if errors.As(err, &nr) {
			p.logf("ccusage: push: the dashboard answered a redirect, which is not followed")
			return PushReasonOther
		}
		var ne interface{ Timeout() bool }
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
			p.logf("ccusage: push: failed (timeout after %s)", p.timeout())
			return PushReasonTimeout
		}
		p.logf("ccusage: push: failed (transport error)")
		return PushReasonTransport
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNoContent:
		return ""
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		p.logf("ccusage: push: the dashboard answered HTTP %d", resp.StatusCode)
		return PushReason4xx
	case resp.StatusCode >= 500 && resp.StatusCode < 600:
		p.logf("ccusage: push: the dashboard answered HTTP %d", resp.StatusCode)
		return PushReason5xx
	default:
		p.logf("ccusage: push: the dashboard answered HTTP %d, want 204", resp.StatusCode)
		return PushReasonOther
	}
}

// PushAfterPass is Push as a Poller.AfterPass.
func (p *Pusher) PushAfterPass(ctx context.Context) { p.Push(ctx) }

// PushInstance is the source_instance label: explicit (≤ MaxPushInstance
// characters, refused otherwise), else the public URL's host when it fits,
// else "muster".
func PushInstance(explicit, publicURL string) (string, error) {
	if v := strings.TrimSpace(explicit); v != "" {
		if len(v) > MaxPushInstance {
			return "", fmt.Errorf("over %d characters", MaxPushInstance)
		}
		return v, nil
	}
	if u, err := url.Parse(strings.TrimSpace(publicURL)); err == nil && u.Hostname() != "" && len(u.Hostname()) <= MaxPushInstance {
		return u.Hostname(), nil
	}
	return "muster", nil
}
