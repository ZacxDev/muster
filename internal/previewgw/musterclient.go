package previewgw

// musterclient.go — the one muster route the gateway calls in this phase:
// GET /plugin/v1/self, per plugin token, at start and every minute
// (docs/plugins.md, "GET /plugin/v1/self").

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

// CapabilityViewer is the plugin capability a token must hold for the gateway
// to serve its owner.
const CapabilityViewer = "viewer"

// Self is GET /plugin/v1/self's answer.
type Self struct {
	PluginID     int64    `json:"plugin_id"`
	PluginName   string   `json:"plugin_name"`
	OwnerID      int64    `json:"owner_id"`
	OwnerLogin   string   `json:"owner_login"`
	Capabilities []string `json:"capabilities"`
}

// HasViewer reports whether the token holds the viewer capability.
func (s Self) HasViewer() bool { return slices.Contains(s.Capabilities, CapabilityViewer) }

// SelfVerdict is what one /self call says about a token.
type SelfVerdict int

const (
	// SelfUnknown: no verdict (muster unreachable, a 5xx, a 404 because
	// plugins are off, an answer that does not parse). Nothing changes.
	SelfUnknown SelfVerdict = iota
	// SelfLive: 200 with the viewer capability.
	SelfLive
	// SelfDead: 401 (revoked token, disabled owner), or 200 WITHOUT the
	// viewer capability — treated exactly like a 401 (the capability is
	// what lets the gateway serve the owner at all).
	SelfDead
)

func (v SelfVerdict) String() string {
	switch v {
	case SelfLive:
		return "live"
	case SelfDead:
		return "dead"
	}
	return "unknown"
}

// MusterClient calls muster's plugin door.
type MusterClient struct {
	Base string // MUSTER_URL, no trailing slash
	HTTP *http.Client
	UA   string
}

// NewMusterClient is a client with a bounded timeout.
func NewMusterClient(base, ua string) *MusterClient {
	return &MusterClient{Base: base, HTTP: &http.Client{Timeout: 15 * time.Second}, UA: ua}
}

// Self asks who token is. The Self is filled only on a 200. The verdict is
// SelfDead on a 401 or a 200 without viewer, SelfLive on a 200 with it, and
// SelfUnknown otherwise (with an error saying why, never quoting the token).
func (m *MusterClient) Self(ctx context.Context, token string) (Self, SelfVerdict, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.Base+"/plugin/v1/self", nil)
	if err != nil {
		return Self{}, SelfUnknown, errors.New("bad muster request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if m.UA != "" {
		req.Header.Set("User-Agent", m.UA)
	}
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return Self{}, SelfUnknown, fmt.Errorf("muster /plugin/v1/self: %s", transportReason(err))
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return Self{}, SelfUnknown, errors.New("muster /plugin/v1/self: reading the answer failed")
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return Self{}, SelfDead, nil
	default:
		return Self{}, SelfUnknown, fmt.Errorf("muster /plugin/v1/self: HTTP %d", resp.StatusCode)
	}
	var s Self
	if err := json.Unmarshal(b, &s); err != nil || s.OwnerID <= 0 {
		return Self{}, SelfUnknown, errors.New("muster /plugin/v1/self: the answer does not parse")
	}
	if !s.HasViewer() {
		return s, SelfDead, nil
	}
	return s, SelfLive, nil
}

// transportReason names a transport failure without the URL (which carries
// nothing secret, but a proxy error can echo headers).
func transportReason(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	}
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 {
		return s[i+2:]
	}
	return "transport error"
}

// ResolveOwner picks the owner a `host-token create` names, from the /self
// answers of every token in PLUGIN_TOKENS_FILE.
//
// 🔴 owner_login IS A LABEL, NOT AN IDENTIFIER (muster's email-else-display-
// name): two accounts can share one. So the login is matched against every
// LIVE answer, and the result is refused unless exactly one owner_id carries
// it. With login empty and exactly one live owner, that owner is the answer.
// A token whose answer is not live (401, no viewer, unreachable) cannot be
// named, and the error says how many were skipped.
func ResolveOwner(login string, answers []SelfAnswer) (Self, error) {
	byOwner := map[int64]Self{}
	skipped := 0
	for _, a := range answers {
		if a.Verdict != SelfLive {
			skipped++
			continue
		}
		byOwner[a.Self.OwnerID] = a.Self
	}
	note := ""
	if skipped > 0 {
		note = fmt.Sprintf(" (%d token(s) in PLUGIN_TOKENS_FILE skipped: revoked, without the %q capability, or muster did not answer)", skipped, CapabilityViewer)
	}
	if login == "" {
		if len(byOwner) == 1 {
			for _, s := range byOwner {
				return s, nil
			}
		}
		return Self{}, fmt.Errorf("name the owner: PLUGIN_TOKENS_FILE has %d live owner(s)%s", len(byOwner), note)
	}
	var match []Self
	for _, s := range byOwner {
		if s.OwnerLogin == login {
			match = append(match, s)
		}
	}
	switch len(match) {
	case 1:
		return match[0], nil
	case 0:
		return Self{}, fmt.Errorf("no live plugin token in PLUGIN_TOKENS_FILE belongs to an owner whose login is %q%s", login, note)
	}
	ids := make([]string, 0, len(match))
	for _, s := range match {
		ids = append(ids, fmt.Sprint(s.OwnerID))
	}
	slices.Sort(ids)
	return Self{}, fmt.Errorf("the login %q is ambiguous: owners %s all answer to it; a login is a label, not an identifier", login, strings.Join(ids, ", "))
}

// SelfAnswer is one token's /self result.
type SelfAnswer struct {
	Self    Self
	Verdict SelfVerdict
	Err     error
}

// SelfAll asks /self for every token, in order.
func (m *MusterClient) SelfAll(ctx context.Context, tokens []string) []SelfAnswer {
	out := make([]SelfAnswer, len(tokens))
	for i, t := range tokens {
		s, v, err := m.Self(ctx, t)
		out[i] = SelfAnswer{Self: s, Verdict: v, Err: err}
	}
	return out
}
