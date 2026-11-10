// Package ccusage reads each pooled Claude account's SUBSCRIPTION UTILIZATION —
// the numbers Claude Code's `/usage` screen shows (the 5-hour session window,
// the 7-day window, and the per-model windows when the account has them) — and
// keeps the latest reading per (owner, account), numbers only.
//
// # The endpoint
//
// [Endpoint] is the one Claude Code's `/usage` calls. Read from the CLI bundle
// (2.1.289): `GET ${BASE_API_URL}/api/oauth/usage` with BASE_API_URL
// `https://api.anthropic.com`, the OAuth access token as `Authorization:
// Bearer`, and `anthropic-beta: oauth-2025-04-20` (the CLI's OAuth beta, sent
// beside every OAuth bearer it holds). Its answer is a JSON object whose window
// keys (`five_hour`, `seven_day`, `seven_day_opus`, `seven_day_sonnet`, …) are
// each null or `{"utilization": <percent 0-100>, "resets_at": "<RFC 3339>"}`
// (the CLI's own schema describes utilization as "Percentage of the window
// used, 0-100"), plus a `limits` array whose per-model weekly rows carry
// `scope.model.display_name`, `percent` and `resets_at`. One live call
// (a full-scope claude.ai login) answered 200 in exactly that shape,
// with `five_hour` null (no session window open) and dozens of other null keys.
//
// ⚠ UNVERIFIED FOR A `setup-token`. The pool's tokens are `claude setup-token`
// tokens, which the CLI mints inference-only (scope `user:inference`); the CLI
// itself skips this call for a token without `user:profile`. What the endpoint
// answers such a token is not measured here: a 401 or 403 is recorded on the
// account ([KindUnauthorized], [KindForbidden]) and the account is not polled
// again until its token changes, so a pool of inference-only tokens costs one
// call per account, not one per interval.
//
// 🔴 A READING HOLDS NO CREDENTIAL. The token is read from the account pool at
// the moment of the call and sent in one header; no error, log line or stored
// row carries it. A row carries the token's [ccpool.Fingerprint] only, so a
// refusal mark expires when the token is replaced.
package ccusage

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Endpoint is Claude Code's `/usage` endpoint (see the package doc).
const Endpoint = "https://api.anthropic.com/api/oauth/usage"

// BetaHeader is the anthropic-beta value the CLI sends with an OAuth bearer.
const BetaHeader = "oauth-2025-04-20"

// The two windows every subscription account has.
const (
	FiveHour = "five_hour"
	SevenDay = "seven_day"
)

// modelWindowPrefix names a per-model weekly window read from `limits[]`
// ("seven_day_model:fable").
const modelWindowPrefix = "seven_day_model:"

// Bounds on what one reading may hold, so an answer of an unexpected shape
// cannot grow a row without limit.
const (
	MaxWindows      = 24
	maxWindowName   = 64
	MaxResponseBody = 1 << 20
)

// Window is one rate-limit window: how much of it is used, in percent (0-100,
// and past 100 when usage runs past the cap), and when it resets. Either may
// be absent (nil) when the endpoint sent null.
type Window struct {
	Utilization *float64   `json:"utilization"`
	ResetsAt    *time.Time `json:"resets_at"`
}

// Reading is one answer of the endpoint: its windows, by name. A window the
// endpoint sent as null is absent.
type Reading struct {
	Windows map[string]Window
}

// ErrNotUsage is a body that is not a usage answer: not a JSON object, or an
// Anthropic error object.
var ErrNotUsage = errors.New("ccusage: not a usage answer")

var windowNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_:.-]*$`)

// Parse reads a 200 answer of [Endpoint].
//
// Every top-level key whose value is an object carrying `utilization` is a
// window (the endpoint adds windows under new names; a key that is null, or an
// object without `utilization` such as `spend`, is not; nor is one whose
// utilization and resets_at are both null, such as an `extra_usage` money
// block that is off).
// Each `limits[]` row with a model scope is a per-model weekly window,
// `seven_day_model:<display name, lowercased>`, from its `percent` — unless a
// top-level key already carries that name. At most [MaxWindows], by name.
func Parse(body []byte) (Reading, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil || top == nil {
		return Reading{}, fmt.Errorf("%w: the body is not a JSON object", ErrNotUsage)
	}
	if t, ok := top["type"]; ok && string(t) == `"error"` {
		var e struct {
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &e)
		return Reading{}, fmt.Errorf("%w: an error object (%s)", ErrNotUsage, safeWord(e.Error.Type))
	}
	out := map[string]Window{}
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !validName(k) {
			continue
		}
		var probe map[string]json.RawMessage
		if json.Unmarshal(top[k], &probe) != nil || probe == nil {
			continue // null, a number, an array
		}
		raw, ok := probe["utilization"]
		if !ok {
			continue
		}
		w, ok := window(raw, probe["resets_at"])
		if !ok || (w.Utilization == nil && w.ResetsAt == nil) {
			continue // malformed, or a window with nothing in it
		}
		out[k] = w
	}
	var limits []struct {
		Percent  json.RawMessage `json:"percent"`
		ResetsAt json.RawMessage `json:"resets_at"`
		Scope    *struct {
			Model *struct {
				DisplayName string `json:"display_name"`
			} `json:"model"`
		} `json:"scope"`
	}
	if raw, ok := top["limits"]; ok {
		_ = json.Unmarshal(raw, &limits) // a limits of another shape adds nothing
	}
	for _, l := range limits {
		if l.Scope == nil || l.Scope.Model == nil {
			continue
		}
		name := modelWindowPrefix + strings.ToLower(strings.TrimSpace(l.Scope.Model.DisplayName))
		if !validName(name) {
			continue
		}
		if _, dup := out[name]; dup {
			continue
		}
		if w, ok := window(l.Percent, l.ResetsAt); ok {
			out[name] = w
		}
	}
	if len(out) > MaxWindows {
		names := make([]string, 0, len(out))
		for n := range out {
			names = append(names, n)
		}
		sort.Strings(names)
		keep := map[string]bool{FiveHour: true, SevenDay: true}
		for _, n := range names {
			if len(keep) >= MaxWindows {
				break
			}
			keep[n] = true
		}
		for n := range out {
			if !keep[n] {
				delete(out, n)
			}
		}
	}
	return Reading{Windows: out}, nil
}

// window reads one window from its utilization and resets_at values. ok is
// false when the utilization is neither a number nor null, or resets_at is
// neither an RFC 3339 time nor null.
func window(util, resets json.RawMessage) (Window, bool) {
	var w Window
	if len(util) > 0 && string(util) != "null" {
		var f float64
		if err := json.Unmarshal(util, &f); err != nil || f < 0 || f > 1e6 {
			return Window{}, false
		}
		w.Utilization = &f
	}
	if len(resets) > 0 && string(resets) != "null" {
		var s string
		if err := json.Unmarshal(resets, &s); err != nil {
			return Window{}, false
		}
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return Window{}, false
		}
		t = t.UTC()
		w.ResetsAt = &t
	}
	return w, true
}

func validName(n string) bool { return len(n) <= maxWindowName && windowNameRE.MatchString(n) }

// safeWord keeps an upstream word printable and short: it is logged and stored
// as an error reason.
func safeWord(s string) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= 40 {
			break
		}
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}
