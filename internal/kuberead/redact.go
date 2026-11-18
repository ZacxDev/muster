package kuberead

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/redact"
)

// ---------------------------------------------------------------------------
// THE POD LOG SCRUB, applied to every pod log line before it reaches the
// model. A pod's log carries user data — addresses, e-mail, signed URLs,
// tokens — and officer transcripts are readable by every HQ member, so a log
// line reaches the model only through here.
//
// It is STRUCTURAL FIRST (the shape of HQ's obs.RedactLine): a line that is
// one JSON object keeps only the fields logKeep names (the rest are dropped
// by name, never by value), and what survives is scrubbed. A plain-text line
// is scrubbed whole. Either way the result is capped at MaxLogLineRunes.
//
// 🔴 ONE REDACTOR FOR SECRETS. Every scrubbed string goes through
// internal/redact's Secrets first — the repo's one secret redactor, whose
// shapes are listed there — and then through logRules below, which hold ONLY
// what redact does not do: user data (a URL's whole query string, e-mail and
// IP addresses, a 32+ character opaque run) and the field forms a log line
// has and redact's text-oriented rules do not match (a cookie / session / sig
// field, and a credential name followed by ":" in the middle of a line). A
// new secret shape belongs in internal/redact, not here.
//
// 🔴 The scrub is a HEURISTIC: a secret in a shape neither set knows passes;
// the caps bound what can.
//
// ConfigMaps are NOT scrubbed (operator decision): this applies to pod logs
// only.
// ---------------------------------------------------------------------------

// Field caps, in runes, after scrubbing (a cut never leaves half a secret).
const (
	maxMessageRunes = 500
	maxFieldRunes   = 200
)

type keepSpec map[string]keep

type keep struct {
	msg    bool
	nested keepSpec
}

var (
	scalar  = keep{}
	message = keep{msg: true}
)

// logKeep is the CLOSED set of fields kept of a structured line (HQ's).
var logKeep = keepSpec{
	"name": scalar, "type": scalar, "pod": scalar, "path": scalar, "trpcType": scalar, "code": scalar,
	"message": message, "msg": message,
	"error": {nested: keepSpec{
		"code": scalar, "name": scalar, "message": message,
		"cause": {nested: keepSpec{"code": scalar, "name": scalar, "message": message}},
	}},
}

// RedactLine is one log line made fit to hand to the model, and how many
// substrings the scrub replaced. A structured line becomes a JSON object of
// its kept fields plus "dropped_fields" (names only).
func RedactLine(text string) (string, int) {
	n := 0
	if obj, ok := jsonObject(text); ok {
		kept := map[string]any{}
		dropped := keepFields(obj, logKeep, "", kept, &n)
		if len(dropped) > 0 {
			sort.Strings(dropped)
			kept["dropped_fields"] = dropped
		}
		b, _ := json.Marshal(kept)
		return capRunes(string(b), MaxLogLineRunes), n
	}
	s, n := scrub(text)
	return capRunes(s, MaxLogLineRunes), n
}

func keepFields(obj map[string]any, spec keepSpec, prefix string, out map[string]any, redactions *int) []string {
	var dropped []string
	for k, v := range obj {
		kf, ok := spec[k]
		if !ok {
			dropped = append(dropped, prefix+k)
			continue
		}
		switch tv := v.(type) {
		case string:
			if kf.nested != nil {
				dropped = append(dropped, prefix+k)
				continue
			}
			s, n := scrub(tv)
			*redactions += n
			lim := maxFieldRunes
			if kf.msg {
				lim = maxMessageRunes
			}
			out[k] = capRunes(s, lim)
		case float64, bool:
			if kf.nested != nil {
				dropped = append(dropped, prefix+k)
				continue
			}
			out[k] = tv
		case map[string]any:
			if kf.nested == nil {
				dropped = append(dropped, prefix+k)
				continue
			}
			nested := map[string]any{}
			dropped = append(dropped, keepFields(tv, kf.nested, prefix+k+".", nested, redactions)...)
			out[k] = nested
		default:
			dropped = append(dropped, prefix+k)
		}
	}
	return dropped
}

func jsonObject(text string) (map[string]any, bool) {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "{") {
		return nil, false
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(t), &obj); err != nil || obj == nil {
		return nil, false
	}
	return obj, true
}

// logRules run in order, after redact.Secrets (scrub), and hold only what
// redact lacks. A value already marked by redact is not matched again (the
// value classes exclude «).
var logRules = []struct {
	re   *regexp.Regexp
	repl string
}{
	// A URL's whole query string: a signed URL's parameters are user data and
	// credentials whatever they are named (redact replaces only the values of
	// parameters with credential names).
	{regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://[^\s?"'<>#]*)\?[^\s"'<>#]*`), "$1?[query]"},
	// Request-log fields redact has no name for.
	{regexp.MustCompile(`(?i)\b(cookie|set-cookie|session(?:[_-]?id)?|sig|signature)("?\s*[:=]\s*"?)[^\s"'&,;}«]+`), "$1$2[redacted]"},
	// A credential name and ":" in the MIDDLE of a line ("login failed,
	// password: …"): redact's "name: value" rule is anchored at a line's
	// start, for configuration text.
	{regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|token|api[_-]?key|apikey|access[_-]?key|private[_-]?key)("?\s*:\s*"?)[^\s"'&,;}«]+`), "$1$2[redacted]"},
	{regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`), "[email]"},
	{regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`), "[ip]"},
	{regexp.MustCompile(`(?i)\b[0-9a-f]{1,4}(?::[0-9a-f]{0,4}){3,7}\b`), "[ip]"},
	{regexp.MustCompile(`[A-Za-z0-9+/_-]{32,}={0,2}`), "[token]"},
}

// scrub is s through redact.Secrets and then logRules, and how many
// substrings were replaced.
func scrub(s string) (string, int) {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	before := strings.Count(s, redact.Mark)
	s = redact.Secrets(s)
	n := strings.Count(s, redact.Mark) - before
	if n < 0 {
		n = 0
	}
	for _, r := range logRules {
		s = r.re.ReplaceAllStringFunc(s, func(m string) string {
			n++
			return r.re.ReplaceAllString(m, r.repl)
		})
	}
	return s, n
}

// capRunes cuts s at n runes, marking the cut.
func capRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…[cut]"
}
