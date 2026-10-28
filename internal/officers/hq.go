package officers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrHQUnavailable is HQ not answering usefully: a transport failure, a
// timeout, or a 5xx. The wake loop backs off on it and changes nothing; an MCP
// tool reports it as "HQ is unavailable, try again".
var ErrHQUnavailable = errors.New("officers: HQ unavailable")

// HQError is HQ answering with a 4xx: a decision, not an outage. Body is HQ's
// own JSON (bounded), passed back to the instance verbatim — a 409 on a claim
// carries `held_by` and `lease_expires_at`, which is what the instance needs
// to move on.
type HQError struct {
	Status int
	Body   []byte
}

func (e *HQError) Error() string {
	return fmt.Sprintf("officers: HQ answered %d: %s", e.Status, truncate(string(e.Body), 200))
}

// ItemID is an item id as HQ spells it. The contract does not say whether ids
// are numbers or strings, so both decode; it is always used as a path segment.
type ItemID string

// UnmarshalJSON accepts a JSON string or number.
func (id *ItemID) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*id = ItemID(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("item id %s is neither a string nor a number", b)
	}
	*id = ItemID(n.String())
	return nil
}

// Item is the part of an HQ item the wake loop decides on. The MCP tools pass
// HQ's full JSON through instead of this struct.
type Item struct {
	ID             ItemID     `json:"id"`
	OfficerID      string     `json:"officer_id"`
	Kind           string     `json:"kind"`
	SubjectKey     string     `json:"subject_key"`
	Status         string     `json:"status"`
	ClaimedBy      string     `json:"claimed_by"`
	LeaseExpiresAt *time.Time `json:"lease_expires_at"`
	UpdatedAt      *time.Time `json:"updated_at"`
}

// LeaseLive reports whether the item's lease is unexpired at now. An item with
// no lease time has no live lease.
func (it Item) LeaseLive(now time.Time) bool {
	return it.LeaseExpiresAt != nil && it.LeaseExpiresAt.After(now)
}

// Item statuses, as the contract names them.
const (
	StatusOpen      = "open"
	StatusClaimed   = "claimed"
	StatusDone      = "done"
	StatusEscalated = "escalated"
)

// Client talks to one officer's HQ with that officer's token.
//
// 🔴 IT IS THE ONLY CODE THAT READS Officer.Token(). The token goes in the
// Authorization header of requests to the officer's own HQ base URL, and
// nowhere else: not in an error, not in a log line (HQError carries HQ's body,
// never the request).
type Client struct {
	officer Officer
	http    *http.Client
}

// DefaultHQTimeout bounds one HQ request.
const DefaultHQTimeout = 15 * time.Second

// maxHQBody bounds what is read of one HQ response. A brief is size-bounded by
// the contract; this is the backstop.
const maxHQBody = 1 << 20

// NewClient returns a client for o. hc nil uses a client with DefaultHQTimeout.
//
// ⚠ REDIRECTS ARE NOT FOLLOWED. Go's client re-sends the Authorization header
// to a same-host redirect, and a misconfigured proxy answering 30x would
// otherwise decide where the officer token goes next.
func NewClient(o Officer, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: DefaultHQTimeout}
	}
	c := *hc
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{officer: o, http: &c}
}

// Officer is the officer this client acts for.
func (c *Client) Officer() Officer { return c.officer }

// Do sends one request to path under the officer's HQ base (path starts with
// '/'), with body JSON-encoded when non-nil, and returns HQ's body on a 2xx.
// A 4xx is *HQError; a transport failure, a 3xx or a 5xx wraps ErrHQUnavailable.
func (c *Client) Do(ctx context.Context, method, path string, body any) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("officers: encode request: %w", err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.officer.HQURL+path, rd)
	if err != nil {
		return nil, fmt.Errorf("officers: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.officer.Token())
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// The URL in a *url.Error is the base + path, which holds no token.
		return nil, fmt.Errorf("%w: %s %s: %v", ErrHQUnavailable, method, path, scrubURLError(err))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxHQBody))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return raw, nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return nil, &HQError{Status: resp.StatusCode, Body: raw}
	default:
		return nil, fmt.Errorf("%w: %s %s answered %d", ErrHQUnavailable, method, path, resp.StatusCode)
	}
}

func scrubURLError(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}

func (c *Client) officerPath() string { return "/officers/" + url.PathEscape(c.officer.ID) }

func (c *Client) itemPath(item ItemID, verb string) string {
	return c.officerPath() + "/items/" + url.PathEscape(string(item)) + "/" + verb
}

// ItemsPage is one page of an officer's items.
type ItemsPage struct {
	Items      []Item
	NextCursor string
	// Raw is HQ's body, for the MCP tool that passes it through.
	Raw json.RawMessage
}

// ItemsQuery is GET /officers/{id}/items's query.
type ItemsQuery struct {
	Status string
	Limit  int
	Cursor string
}

func (q ItemsQuery) encode() string {
	v := url.Values{}
	if q.Status != "" {
		v.Set("status", q.Status)
	}
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	if q.Cursor != "" {
		v.Set("cursor", q.Cursor)
	}
	if len(v) == 0 {
		return ""
	}
	return "?" + v.Encode()
}

// Items reads one page of the officer's items.
//
// The body may be `{"items":[…],"next_cursor":"…"}` or a bare array (the
// contract names the route, not the envelope; both are accepted).
func (c *Client) Items(ctx context.Context, q ItemsQuery) (ItemsPage, error) {
	raw, err := c.Do(ctx, http.MethodGet, c.officerPath()+"/items"+q.encode(), nil)
	if err != nil {
		return ItemsPage{}, err
	}
	page := ItemsPage{Raw: raw}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &page.Items); err != nil {
			return ItemsPage{}, fmt.Errorf("%w: items: %v", ErrHQUnavailable, err)
		}
		return page, nil
	}
	var env struct {
		Items      []Item `json:"items"`
		NextCursor string `json:"next_cursor"`
	}
	if err := json.Unmarshal(trimmed, &env); err != nil {
		return ItemsPage{}, fmt.Errorf("%w: items: %v", ErrHQUnavailable, err)
	}
	page.Items, page.NextCursor = env.Items, env.NextCursor
	return page, nil
}

// maxItemPages bounds AllItems: the wake loop needs a count, not a census.
const maxItemPages = 5

// AllItems reads up to maxItemPages pages of items in status.
func (c *Client) AllItems(ctx context.Context, status string) ([]Item, error) {
	var out []Item
	q := ItemsQuery{Status: status, Limit: 100}
	for i := 0; i < maxItemPages; i++ {
		page, err := c.Items(ctx, q)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if page.NextCursor == "" || len(page.Items) == 0 {
			break
		}
		q.Cursor = page.NextCursor
	}
	return out, nil
}

// Brief is GET /officers/{id}/brief, HQ's JSON passed through.
func (c *Client) Brief(ctx context.Context) ([]byte, error) {
	return c.Do(ctx, http.MethodGet, c.officerPath()+"/brief", nil)
}

// Lease bounds, from the contract (30..3600 seconds).
const (
	MinLeaseSeconds     = 30
	MaxLeaseSeconds     = 3600
	DefaultLeaseSeconds = 600
)

// ClampLease is seconds within the contract's bounds; 0 is the default.
func ClampLease(seconds int) int {
	switch {
	case seconds == 0:
		return DefaultLeaseSeconds
	case seconds < MinLeaseSeconds:
		return MinLeaseSeconds
	case seconds > MaxLeaseSeconds:
		return MaxLeaseSeconds
	}
	return seconds
}

// Claim is POST …/items/{item}/claim.
func (c *Client) Claim(ctx context.Context, item ItemID, instance string, leaseSeconds int) ([]byte, error) {
	return c.Do(ctx, http.MethodPost, c.itemPath(item, "claim"), map[string]any{"instance": instance, "lease_seconds": ClampLease(leaseSeconds)})
}

// Heartbeat is POST …/items/{item}/heartbeat.
func (c *Client) Heartbeat(ctx context.Context, item ItemID, instance string, leaseSeconds int) ([]byte, error) {
	return c.Do(ctx, http.MethodPost, c.itemPath(item, "heartbeat"), map[string]any{"instance": instance, "lease_seconds": ClampLease(leaseSeconds)})
}

// Completion is POST …/items/{item}/complete's body, less the instance.
type Completion struct {
	Outcome string   `json:"outcome"`
	Summary string   `json:"summary"`
	Links   []string `json:"links,omitempty"`
}

// Outcomes the contract accepts.
var Outcomes = []string{"done", "escalated", "no_action"}

// MaxSummaryBytes is the contract's summary bound.
const MaxSummaryBytes = 4096

// Complete is POST …/items/{item}/complete.
func (c *Client) Complete(ctx context.Context, item ItemID, instance string, done Completion) ([]byte, error) {
	return c.Do(ctx, http.MethodPost, c.itemPath(item, "complete"), map[string]any{
		"instance": instance, "outcome": done.Outcome, "summary": done.Summary, "links": done.Links,
	})
}

// Release is POST …/items/{item}/release.
func (c *Client) Release(ctx context.Context, item ItemID, instance string) ([]byte, error) {
	return c.Do(ctx, http.MethodPost, c.itemPath(item, "release"), map[string]any{"instance": instance})
}

// ReleaseEvaluation is POST /releases/captain with the officer token: the
// release captain's evaluation of one release. evaluation is the captain
// route's own body, passed through; instance is set on it so HQ records which
// instance wrote it (the officer it learns from the token).
func (c *Client) ReleaseEvaluation(ctx context.Context, instance string, evaluation map[string]json.RawMessage) ([]byte, error) {
	body := map[string]any{}
	for k, v := range evaluation {
		body[k] = v
	}
	body["instance"] = instance
	return c.Do(ctx, http.MethodPost, "/releases/captain", body)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
