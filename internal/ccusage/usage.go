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
// 🔴 A `setup-token` IS REFUSED THERE. The pool's tokens are `claude
// setup-token` tokens, which the CLI mints inference-only (scope
// `user:inference`); the endpoint answers them 403 `permission_error` ("OAuth
// token does not meet scope requirement user:profile"), measured against a
// pool token, and it rate-limits hard (429 with a Retry-After of about an
// hour). So an account's token gets ONE discovery call to it; a 2xx keeps the
// endpoint for that token, a 401/403 retires it for that token for good.
//
// # The probe (every other token)
//
// [ProbeURL] is a minimal inference call — `POST /v1/messages`, max_tokens 1,
// one user message, on [ProbeModel] — which a setup-token CAN make, and whose
// answer carries the subscription's state in `anthropic-ratelimit-unified-*`
// headers ([ParseRateLimitHeaders]): per window (`5h` five_hour, `7d`
// seven_day, `7d_oi` seven_day_overage_included, `overage`; the CLI bundle's
// own map) `-utilization` (a 0-1 FRACTION, stored here ×100 as a percent like
// the endpoint's), `-reset` (unix seconds) and `-status`, plus the overall
// `-status`, `-reset` and `-representative-claim`. A window whose headers are
// absent is absent (a measured 200 carried 7d headers and no 5h ones). It costs
// one output token of quota, so its default interval is 15 minutes, and an
// account the pool marks rate-limited is not probed until its reset.
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
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Endpoint is Claude Code's `/usage` endpoint (see the package doc).
const Endpoint = "https://api.anthropic.com/api/oauth/usage"

// ProbeURL and ProbeModel are the one-token probe (see the package doc).
const (
	ProbeURL   = "https://api.anthropic.com/v1/messages"
	ProbeModel = "claude-haiku-4-5-20251001"
	// anthropicVersion is the Messages API version the probe names.
	anthropicVersion = "2023-06-01"
)

// Where a reading came from (Row.Source).
const (
	SourceUsageAPI         = "usage_api"
	SourceRateLimitHeaders = "ratelimit_headers"
)

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
	// Status is the window's own status from the probe's headers ("allowed",
	// "allowed_warning", "rejected"); "" from the usage endpoint.
	Status string `json:"status,omitempty"`
}

// Reading is one answer: its windows, by name (a window sent as null, or
// with no headers, is absent), and — from the probe's headers only — the
// overall status, the window that decides it and when it resets.
type Reading struct {
	Windows             map[string]Window
	Status              string
	RepresentativeClaim string
	ResetsAt            *time.Time
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

// headerWindows is the CLI bundle's map from a window's name to its header
// abbreviation.
var headerWindows = [][2]string{{FiveHour, "5h"}, {SevenDay, "7d"}, {"seven_day_overage_included", "7d_oi"}, {"overage", "overage"}}

const unifiedPrefix = "anthropic-ratelimit-unified-"

// maxFraction bounds a header utilization. It is a 0-1 fraction, and the CLI
// bundle's own schema says values above 1 occur when usage runs past a
// window's cap; anything past 2 (twice the cap) is read as garbage.
const maxFraction = 2.0

// ParseRateLimitHeaders reads the `anthropic-ratelimit-unified-*` headers of a
// Messages answer (see the package doc). ok is false when the answer carries
// none of them. A garbage value is dropped (that value only): a utilization
// that is not a finite number in [0, 100], a reset that is not unix seconds, a
// status that is not a short word.
func ParseRateLimitHeaders(h http.Header) (Reading, bool) {
	r := Reading{Windows: map[string]Window{}}
	any := false
	for _, w := range headerWindows {
		var win Window
		if v, ok := headerNumber(h, unifiedPrefix+w[1]+"-utilization"); ok && v >= 0 && v <= maxFraction {
			pct := math.Round(v*100*1e4) / 1e4
			win.Utilization = &pct
		}
		if t, ok := headerUnix(h, unifiedPrefix+w[1]+"-reset"); ok {
			win.ResetsAt = &t
		}
		win.Status = headerWord(h, unifiedPrefix+w[1]+"-status")
		if win.Utilization != nil || win.ResetsAt != nil || win.Status != "" {
			r.Windows[w[0]] = win
			any = true
		}
	}
	if s := headerWord(h, unifiedPrefix+"status"); s != "" {
		r.Status, any = s, true
	}
	if c := headerWord(h, unifiedPrefix+"representative-claim"); c != "" {
		r.RepresentativeClaim, any = c, true
	}
	if t, ok := headerUnix(h, unifiedPrefix+"reset"); ok {
		r.ResetsAt, any = &t, true
	}
	return r, any
}

func headerNumber(h http.Header, name string) (float64, bool) {
	v := strings.TrimSpace(h.Get(name))
	if v == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(v, 64)
	// NaN and ±Inf parse, and fail every caller's range check (a NaN compares
	// false to both bounds).
	if err != nil {
		return 0, false
	}
	return f, true
}

// headerUnix reads unix seconds between 2000 and 2200.
func headerUnix(h http.Header, name string) (time.Time, bool) {
	f, ok := headerNumber(h, name)
	if !ok || f < 946684800 || f > 7258118400 {
		return time.Time{}, false
	}
	return time.Unix(int64(math.Round(f)), 0).UTC(), true
}

var wordRE = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)

func headerWord(h http.Header, name string) string {
	v := strings.ToLower(strings.TrimSpace(h.Get(name)))
	if !wordRE.MatchString(v) {
		return ""
	}
	return v
}
