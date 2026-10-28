package officers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// STANDARD WEBHOOKS VERIFICATION (https://www.standardwebhooks.com)
//
// HQ signs each delivery with headers webhook-id, webhook-timestamp (unix
// seconds) and webhook-signature (space-separated `v1,<base64 HMAC-SHA256>`),
// over `<id>.<timestamp>.<body>`, keyed by the endpoint secret (`whsec_` +
// base64). A delivery is accepted when ANY listed v1 signature matches and the
// timestamp is within Tolerance of now.
//
// A verified delivery only NUDGES the wake loop: it carries ids, never
// instructions, and the loop re-reads HQ before deciding anything. So a
// replayed delivery is harmless; Dedup still drops a repeated webhook-id so a
// redelivery storm costs one pass, not one per copy.
// ---------------------------------------------------------------------------

// Tolerance is how far a delivery's timestamp may be from now.
const Tolerance = 5 * time.Minute

// Verification failures.
var (
	ErrMissingHeaders = errors.New("officers: webhook: missing webhook-id, webhook-timestamp or webhook-signature")
	ErrBadTimestamp   = errors.New("officers: webhook: timestamp outside tolerance")
	ErrBadSignature   = errors.New("officers: webhook: no matching signature")
)

// DecodeSecret reads a Standard Webhooks secret: `whsec_<base64>`, or bare
// base64.
func DecodeSecret(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "whsec_")
	if s == "" {
		return nil, errors.New("officers: webhook secret is empty")
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("officers: webhook secret is not whsec_<base64>: %w", err)
	}
	if len(b) < 16 {
		return nil, errors.New("officers: webhook secret is shorter than 16 bytes")
	}
	return b, nil
}

// Sign returns the v1 signature of (id, ts, body) under key. Used by Verify
// and by tests acting as HQ.
func Sign(key []byte, id string, ts int64, body []byte) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(id + "." + strconv.FormatInt(ts, 10) + "."))
	m.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// Verify checks a delivery and returns its webhook-id.
func Verify(key []byte, h http.Header, body []byte, now time.Time) (string, error) {
	id, tsRaw, sigs := h.Get("webhook-id"), h.Get("webhook-timestamp"), h.Get("webhook-signature")
	if id == "" || tsRaw == "" || sigs == "" {
		return "", ErrMissingHeaders
	}
	ts, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil {
		return "", ErrBadTimestamp
	}
	at := time.Unix(ts, 0)
	if at.Before(now.Add(-Tolerance)) || at.After(now.Add(Tolerance)) {
		return "", ErrBadTimestamp
	}
	want := []byte(Sign(key, id, ts, body))
	for _, s := range strings.Fields(sigs) {
		if !strings.HasPrefix(s, "v1,") {
			continue
		}
		if hmac.Equal([]byte(s), want) {
			return id, nil
		}
	}
	return "", ErrBadSignature
}

// Dedup remembers webhook-ids it has seen for Tolerance*2 (past that, Verify
// refuses the timestamp anyway), bounded to max entries.
type Dedup struct {
	mu   sync.Mutex
	seen map[string]time.Time
	max  int
}

// NewDedup returns a Dedup holding at most max ids.
func NewDedup(max int) *Dedup { return &Dedup{seen: map[string]time.Time{}, max: max} }

// First reports whether id is new at now, and records it.
func (d *Dedup) First(id string, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.seen[id]; ok {
		return false
	}
	if len(d.seen) >= d.max {
		for k, at := range d.seen {
			if now.Sub(at) > 2*Tolerance {
				delete(d.seen, k)
			}
		}
		if len(d.seen) >= d.max {
			// Still full of live ids: drop an arbitrary one. A forgotten id
			// costs one extra pass, nothing more.
			for k := range d.seen {
				delete(d.seen, k)
				break
			}
		}
	}
	d.seen[id] = now
	return true
}

// Event is a webhook delivery's body, as far as muster reads it.
type Event struct {
	Type string `json:"type"`
	Data struct {
		OfficerID string `json:"officer_id"`
		ItemID    ItemID `json:"item_id"`
		Kind      string `json:"kind"`
	} `json:"data"`
}

// EventItemCreated is the event type that wakes an officer.
const EventItemCreated = "officer.item_created"
