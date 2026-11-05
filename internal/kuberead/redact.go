package kuberead

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// THE POD LOG SCRUB: a port of HQ's obs.RedactLine (civ-hq internal/obs/
// redact.go), the scrub HQ applies to every Loki line it hands an officer. A
// pod's log carries user data — addresses, e-mail, signed URLs, tokens — and
// officer transcripts are readable by every HQ member, so a log line reaches
// the model only through here.
//
// It is STRUCTURAL FIRST: a line that is one JSON object keeps only the
// fields logKeep names (the rest are dropped by name, never by value), and
// what survives is scrubbed. A plain-text line is scrubbed whole. Either way
// the result is capped at MaxLogLineRunes.
//
// 🔴 The scrub is a HEURISTIC: the secret shapes it knows (JWTs, bearer and
// basic credentials, URL credentials and query strings, key=value secrets,
// e-mail addresses, IPv4/IPv6 addresses, 32+ character opaque tokens). A
// secret in a shape it does not know passes; the cap bounds what can.
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

// scrubRules run in order (HQ's, unchanged): credentials inside URLs and
// JWTs before the e-mail and token shapes that would half-match them.
var scrubRules = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`eyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}`), "[jwt]"},
	{regexp.MustCompile(`(?i)\b(bearer|basic|token)\s+[A-Za-z0-9._~+/=-]{8,}`), "$1 [redacted]"},
	{regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^\s/@"'<>]+@`), "$1[redacted]@"},
	{regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://[^\s?"'<>#]*)\?[^\s"'<>#]*`), "$1?[query]"},
	{regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|client[_-]?secret|token|access[_-]?token|refresh[_-]?token|api[_-]?key|apikey|access[_-]?key|private[_-]?key|cookie|set-cookie|session(?:[_-]?id)?|sig|signature)("?\s*[:=]\s*"?)[^\s"'&,;}]+`), "$1$2[redacted]"},
	{regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`), "[email]"},
	{regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`), "[ip]"},
	{regexp.MustCompile(`(?i)\b[0-9a-f]{1,4}(?::[0-9a-f]{0,4}){3,7}\b`), "[ip]"},
	{regexp.MustCompile(`[A-Za-z0-9+/_-]{32,}={0,2}`), "[token]"},
}

func scrub(s string) (string, int) {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	n := 0
	for _, r := range scrubRules {
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
