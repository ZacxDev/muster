package clankmailplugin

// hubclient.go — the ClankMail hub's HTTP API (SPEC §8), the routes this plugin
// uses. Every authenticated call signs a fresh §6.4 auth for its own exact
// request. No error built here carries a URL, a token or a response body: a
// body can echo message text, and a wrapped *url.Error prints the hub address.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// hubReadCap bounds one hub answer: a page of ≤200 tokens of ≤98 304 bytes.
const hubReadCap = 24 << 20

var errTransport = errors.New("transport error")

// HubError is a non-2xx answer from the hub: the status and its E_… code.
type HubError struct {
	Status int
	Code   string
}

func (e *HubError) Error() string { return fmt.Sprintf("hub HTTP %d %s", e.Status, e.Code) }

func hubCode(err error) string {
	var he *HubError
	if errors.As(err, &he) {
		return he.Code
	}
	return ""
}

// HubClient talks to one hub as one key.
type HubClient struct {
	Base string // HUB_URL, no trailing slash
	Name string // HUB_NAME: what every auth and identity object names
	Key  ed25519.PrivateKey
	HTTP *http.Client
	Now  func() time.Time
	UA   string
}

func (c *HubClient) do(ctx context.Context, method, path string, body []byte, contentType string, auth bool) (int, []byte, error) {
	u, err := url.Parse(c.Base + path)
	if err != nil {
		return 0, nil, errors.New("bad hub request URL")
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return 0, nil, errors.New("bad hub request")
	}
	// ONE attempt on the wire: no transparent resend by net/http, so an attempt
	// whose answer was lost surfaces as errTransport (the replier's ambiguity).
	req.GetBody = nil
	req.Header.Set("User-Agent", c.UA)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if auth {
		h, err := AuthorizationHeader(c.Key, c.Name, method, req.URL.RequestURI(), body, c.Now().UnixMilli())
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Authorization", h)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, errTransport
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, hubReadCap+1))
	if err != nil {
		return resp.StatusCode, nil, errTransport
	}
	if len(b) > hubReadCap {
		return resp.StatusCode, nil, errors.New("hub answer over the read cap")
	}
	return resp.StatusCode, b, nil
}

func hubErr(status int, b []byte) error {
	var v struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(b, &v)
	if len(v.Error) > 32 {
		v.Error = ""
	}
	return &HubError{Status: status, Code: v.Error}
}

// InboxItem is one entry of GET /v1/inbox. Thread is the hub's UNSIGNED claim
// of the derived root; this plugin never routes on it (see relay.go).
type InboxItem struct {
	Cursor string `json:"cursor"`
	Token  string `json:"token"`
	Thread string `json:"thread,omitempty"`
}

// Inbox reads one page of a queue ("inbox" or "requests") after cursor.
func (c *HubClient) Inbox(ctx context.Context, queue, after string, limit int) (items []InboxItem, next string, err error) {
	q := url.Values{"queue": {queue}, "limit": {strconv.Itoa(limit)}}
	if after != "" {
		q.Set("after", after)
	}
	st, b, err := c.do(ctx, http.MethodGet, "/v1/inbox?"+q.Encode(), nil, "", true)
	if err != nil {
		return nil, "", err
	}
	if st != http.StatusOK {
		return nil, "", hubErr(st, b)
	}
	var out struct {
		Items []InboxItem `json:"items"`
		Next  string      `json:"next"`
	}
	if json.Unmarshal(b, &out) != nil {
		return nil, "", errors.New("inbox: malformed hub answer")
	}
	return out.Items, out.Next, nil
}

// Object fetches GET /v1/objects/{id}, authenticated (a direct message is
// served only to its sender or a recipient). found=false on 404, which the hub
// also answers for an object the requester may not see.
func (c *HubClient) Object(ctx context.Context, id string) (token string, found bool, err error) {
	st, b, err := c.do(ctx, http.MethodGet, "/v1/objects/"+url.PathEscape(id), nil, "", true)
	if err != nil {
		return "", false, err
	}
	if st == http.StatusNotFound {
		return "", false, nil
	}
	if st != http.StatusOK {
		return "", false, hubErr(st, b)
	}
	var out struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(b, &out) != nil || out.Token == "" {
		return "", false, errors.New("object: malformed hub answer")
	}
	return out.Token, true, nil
}

// Submit posts one token. 200 → the id the hub stored it under (also for a
// resend of bytes it already holds: §5 answers a stored id first).
func (c *HubClient) Submit(ctx context.Context, token string) (string, error) {
	st, b, err := c.do(ctx, http.MethodPost, "/v1/submit", []byte(token), "text/plain", false)
	if err != nil {
		return "", err
	}
	if st != http.StatusOK {
		return "", hubErr(st, b)
	}
	var out struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(b, &out) != nil || out.ID == "" {
		return "", errors.New("submit: malformed hub answer")
	}
	return out.ID, nil
}

// Allow adds key to this key's allow list (POST /v1/allow).
func (c *HubClient) Allow(ctx context.Context, key string) error {
	body, _ := json.Marshal(map[string]string{"key": key})
	st, b, err := c.do(ctx, http.MethodPost, "/v1/allow", body, "application/json", true)
	if err != nil {
		return err
	}
	if st != http.StatusOK {
		return hubErr(st, b)
	}
	return nil
}

// Disallow removes key from this key's allow list (DELETE /v1/allow/{key}).
func (c *HubClient) Disallow(ctx context.Context, key string) error {
	st, b, err := c.do(ctx, http.MethodDelete, "/v1/allow/"+url.PathEscape(key), nil, "", true)
	if err != nil {
		return err
	}
	if st != http.StatusOK {
		return hubErr(st, b)
	}
	return nil
}

// AgentPair is one live grant + enroll the hub lists under a handle.
type AgentPair struct {
	Grant  string `json:"grant"`
	Enroll string `json:"enroll"`
}

// Handle reads GET /v1/handles/{name}. found=false on 404.
func (c *HubClient) Handle(ctx context.Context, name string) (handle string, agents []AgentPair, found bool, err error) {
	st, b, err := c.do(ctx, http.MethodGet, "/v1/handles/"+url.PathEscape(name), nil, "", false)
	if err != nil {
		return "", nil, false, err
	}
	if st == http.StatusNotFound {
		return "", nil, false, nil
	}
	if st != http.StatusOK {
		return "", nil, false, hubErr(st, b)
	}
	var out struct {
		Handle string      `json:"handle"`
		Agents []AgentPair `json:"agents"`
	}
	if json.Unmarshal(b, &out) != nil {
		return "", nil, false, errors.New("handles: malformed hub answer")
	}
	return out.Handle, out.Agents, true, nil
}
