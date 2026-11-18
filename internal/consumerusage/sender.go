package consumerusage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/noredirect"
)

// PoolUsageSuffix is the path the pool push's URL (MUSTER_CC_USAGE_PUSH_URL)
// ends in, and ConsumerUsageSuffix the route this package PUTs to: the same
// HQ, the same base, the sibling path.
const (
	PoolUsageSuffix     = "/claude-pool/usage"
	ConsumerUsageSuffix = "/claude-pool/consumer-usage"
)

// URLFromPoolPush is the consumer-usage URL derived from the pool push's URL:
// its path's PoolUsageSuffix replaced by ConsumerUsageSuffix, scheme, host and
// base path kept. A URL whose path does not end in PoolUsageSuffix, or that
// carries a query or a fragment, is refused: there is no base to derive.
func URLFromPoolPush(poolURL string) (string, error) {
	u, err := url.Parse(poolURL)
	if err != nil {
		return "", errors.New("not a URL")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("a query or fragment leaves no base to derive the consumer-usage route from")
	}
	base, ok := strings.CutSuffix(u.Path, PoolUsageSuffix)
	if !ok {
		return "", fmt.Errorf("its path must end in %s (HQ's pool route) for the consumer-usage route (%s) to be derived from it",
			PoolUsageSuffix, ConsumerUsageSuffix)
	}
	u.Path = base + ConsumerUsageSuffix
	u.RawPath = ""
	return u.String(), nil
}

// DefaultBackoff is the waits before each retry of a push HQ answered 5xx or
// did not answer: two retries, the whole within FlushTimeout with fast
// answers.
var DefaultBackoff = []time.Duration{2 * time.Second, 5 * time.Second}

// RefusedError is HQ refusing a push (a 4xx, or a redirect, which is never
// followed). It is not retried: the same push would be refused again.
type RefusedError struct{ Status int }

func (e *RefusedError) Error() string {
	return fmt.Sprintf("HQ refused the consumer-usage push (HTTP %d)", e.Status)
}

// UnavailableError is HQ not taking a push after every retry: a 5xx (Status),
// or no answer (Status 0: a transport error or a timeout).
type UnavailableError struct {
	Status   int
	Attempts int
}

func (e *UnavailableError) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("HQ did not answer the consumer-usage push (%d attempts)", e.Attempts)
	}
	return fmt.Sprintf("HQ answered HTTP %d to the consumer-usage push (%d attempts)", e.Status, e.Attempts)
}

// Sender PUTs a Report to URL with Token as a bearer.
//
// 🔴 THE TOKEN IS IN ONE HEADER AND NOWHERE ELSE: no error carries the URL,
// the body or HQ's answer, and a redirect is never followed (it would re-send
// the bearer elsewhere).
type Sender struct {
	URL, Token string
	Client     *http.Client
	// Backoff is the wait before each retry of a 5xx or an unanswered push;
	// nil is DefaultBackoff, empty (non-nil) is no retry.
	Backoff []time.Duration
	// Sleep waits d or until ctx ends (production: a timer); a test records.
	Sleep func(ctx context.Context, d time.Duration) error
}

func (s *Sender) backoff() []time.Duration {
	if s.Backoff == nil {
		return DefaultBackoff
	}
	return s.Backoff
}

func (s *Sender) sleep(ctx context.Context, d time.Duration) error {
	if s.Sleep != nil {
		return s.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Put sends r: nil on a 2xx; *RefusedError on a 4xx or a redirect, at once;
// *UnavailableError once a 5xx or an unanswered attempt has been retried
// after each Backoff wait (or ctx ended during one).
func (s *Sender) Put(ctx context.Context, r Report) error {
	if r.Hours == nil {
		r.Hours = []Row{}
	}
	body, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("consumerusage: encode the push: %w", err)
	}
	c := noredirect.Client(s.Client)
	waits := s.backoff()
	for attempt := 1; ; attempt++ {
		status, err := s.once(ctx, c, body)
		switch {
		case err == nil && status >= 200 && status < 300:
			return nil
		case err == nil && status < 500:
			return &RefusedError{Status: status}
		}
		if errors.As(err, new(*noredirect.Error)) {
			return &RefusedError{Status: http.StatusFound}
		}
		if attempt > len(waits) || ctx.Err() != nil {
			return &UnavailableError{Status: status, Attempts: attempt}
		}
		if s.sleep(ctx, waits[attempt-1]) != nil {
			return &UnavailableError{Status: status, Attempts: attempt}
		}
	}
}

// once is one attempt: HQ's status, or an error when there was no answer
// (status 0).
func (s *Sender) once(ctx context.Context, c *http.Client, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.URL, bytes.NewReader(body))
	if err != nil {
		return 0, errors.New("consumerusage: build the request")
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "muster-consumer-usage")
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}
