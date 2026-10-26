// Package redact is THE ONE secret redactor: what looks like a credential in a
// line of text an agent or a page will be shown is replaced by [Mark].
//
// 🔴 ONE COPY, EVERY CALLER. It was internal/mirrorhost's (the pending-action
// summary a waiting session sends, below) and moved here unchanged when the
// chief's sessions_overview began quoting session transcripts
// (internal/api's sessions_overview.go); the host also runs every field of a
// rendered view's transcript stream through it (internal/mirrorhost's
// redact_stream.go). A second redactor would drift from this one — the shapes
// it knows are a list that grows — so all of them call this.
package redact

import (
	"regexp"
	"strings"
	"unicode"
)

// REDACTION BEFORE A PENDING ACTION OR A RENDERED VIEW'S TRANSCRIPT LEAVES THE
// HOST (internal/mirrorhost's summarizeTool, PendingOf and redactLines) AND
// BEFORE sessions_overview QUOTES A TRANSCRIPT.
//
// What a waiting session waits on is shown on muster's queue page and handed
// to the chief's model by attention_list, so a command line pasted there must
// not carry the credential it was typed with. Every value that LOOKS like a
// secret is replaced by «redacted» here, on the host, before muster sees it:
//
//   - an Authorization / Proxy-Authorization header's value, and a Bearer or
//     Basic credential (one with a digit in it, so prose is left alone);
//   - the value of a -p, --password, --token (and kin) flag, and of a -u whose
//     value is user:password;
//   - an assignment to a name ending in KEY, TOKEN, SECRET, PASSWORD, PASSWD
//     or PWD (FOO_KEY=…, PGPASSWORD=…);
//   - a URL's userinfo, and its query parameters named token, key, secret,
//     sig/signature, password, pass or auth;
//   - a token with a well-known prefix: ghp_ (and gho_ ghu_ ghs_ ghr_),
//     github_pat_, glpat-, sk-, xoxb-/xoxp- (and kin), AKIA…, mst_, mmt_, mpl_.
//
//   - and a second batch: a PEM/OpenSSH private key block (multi-line), a
//     Stripe sk_/rk_ live or test key, a JWT, a JSON "api_key" / "token" /
//     "secret" / "password" value, a YAML password: / token: value,
//     NAME = '…' with spaces, "the password is …", an age secret key, a
//     Google AIza… key, a Discord bot token, a hf_ token, an X-Api-Key /
//     Api-Key / X-Auth-Token header, and a 40-character AWS secret access key
//     in text that says aws or secret.
//
// ⚠ A HEURISTIC, NOT A GUARANTEE: a secret in a shape this does not know
// passes. It is a second line behind the first, which is that only one line
// of a call's input is ever summarised, and that no push carries it.

// Mark replaces each secret.
const Mark = "«redacted»"

var (
	reAuthHeader = regexp.MustCompile(`(?i)\b((?:proxy-)?authorization\s*[:=]\s*)(?:(?:bearer|basic|token|digest)\s+)?("[^"]*"|'[^']*'|[^\s'"]+)`)
	reBearer     = regexp.MustCompile(`(?i)\b(bearer|basic)\s+([A-Za-z0-9._~+/-]{6,}=*)`)
	reFlag       = regexp.MustCompile(`(?i)(^|\s)(` + alternatives(flagWords) + `)(=|\s+)("[^"]*"|'[^']*'|[^\s'"]+)`)
	reAssign     = regexp.MustCompile(`(?i)\b([a-z0-9_]*(?:key|token|secret|password|passwd|pwd))=("[^"]*"|'[^']*'|[^\s'"&]+)`)
	reUserinfo   = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^/\s@'"]+@`)
	reQuery      = regexp.MustCompile(`(?i)([?&;][a-z0-9_.-]*(?:` + alternatives(queryWords) + `)[a-z0-9_.-]*=)[^&\s#'"]+`)
	rePrefix     = regexp.MustCompile(`\b(?:gh[pousr]_|github_pat_|glpat-|xox[abposr]-|sk-|mst_|mmt_|mpl_)[A-Za-z0-9_-]{6,}|\bAKIA[A-Z0-9]{12,}`)

	// The second batch (secrets, after rePrefix).
	rePEM        = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(?:-----END [A-Z ]*PRIVATE KEY-----|\z)`)
	reStripe     = regexp.MustCompile(`\b[sr]k_(?:live|test)_[A-Za-z0-9]{6,}`)
	reJWT        = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}`)
	reJSONSecret = regexp.MustCompile(`(?i)("[a-z0-9_-]*(?:api_?key|apikey|token|secret|password|passwd)"\s*:\s*)"[^"]*"`)
	reYAMLSecret = regexp.MustCompile(`(?im)^(\s*-?\s*[a-z0-9_.-]*(?:` + alternatives(yamlWords) + `)[ \t]*:[ \t]*)("[^"]*"|'[^']*'|[^\s"'].*)$`)
	reAssignSp   = regexp.MustCompile(`(?i)\b([a-z0-9_]*(?:` + alternatives(assignWords) + `)\s*=\s*)("[^"]+"|'[^']+')`)
	reSaid       = regexp.MustCompile(`(?i)\b((?:password|passphrase|passwd|secret)\s+(?:is|was)\s*:?\s*)("[^"]+"|'[^']+'|[^\s,;.]+)`)
	// Spelled in two parts so the leak gate's credential rule (which matches
	// the literal prefix followed by key characters) does not read the
	// pattern itself as a key.
	reAge       = regexp.MustCompile(`AGE-SECRET-` + `KEY-1[0-9A-Z]{10,}`)
	reGoogle    = regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{20,}`)
	reDiscord   = regexp.MustCompile(`\b[MNO][A-Za-z0-9_-]{23,27}\.[A-Za-z0-9_-]{6,7}\.[A-Za-z0-9_-]{27,40}\b`)
	reHF        = regexp.MustCompile(`\bhf_[A-Za-z0-9]{20,}`)
	reKeyHeader = regexp.MustCompile(`(?i)\b((?:x-api-key|api-key|x-auth-token|x-access-token)\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s'"]+)`)
	reAWSSecret = regexp.MustCompile(`[A-Za-z0-9/+=]+`)
)

// Secrets is s with every value that looks like a secret replaced by
// «redacted» (see the comment above for the shapes).
func Secrets(s string) string { return secrets(s, true) }

// secrets is Secrets; with filter false every pattern runs, which is what the
// test compares the filtered path against.
//
// 🔴 THE FILTER ONLY SKIPS A PATTERN THAT CANNOT MATCH. Go's regexp costs
// about 1 ms per pattern per 30 KiB of prose (measured at the rendered view's
// change: ~8 ms for all seven), and a rendered view's backfill is hundreds of
// such texts. Each pattern needs a literal of its own — "authorization",
// "bearer"/"basic", a flag's dash, a credential name followed by "=", "://"
// with an "@", a token prefix — so a text without that literal is not run
// through it. The literals are checked against the CURRENT text before each
// pattern (a replacement can remove one, never make one: «redacted» spells
// none), lowercased where the pattern ignores case.
func secrets(s string, filter bool) string {
	if s == "" {
		return s
	}
	lower := fold(s)
	may := func(lits ...string) bool {
		if !filter {
			return true
		}
		for _, l := range lits {
			if strings.Contains(lower, l) {
				return true
			}
		}
		return false
	}
	set := func(n string) {
		if n != s {
			s, lower = n, fold(n)
		}
	}
	if may("authorization") {
		set(reAuthHeader.ReplaceAllString(s, "${1}"+Mark))
	}
	if may("bearer", "basic") {
		set(reBearer.ReplaceAllStringFunc(s, func(m string) string {
			sub := reBearer.FindStringSubmatch(m)
			if !strings.ContainsFunc(sub[2], unicode.IsDigit) {
				return m // "basic setup", not a credential
			}
			return sub[1] + " " + Mark
		}))
	}
	if !filter || hasFlag(lower) {
		set(reFlag.ReplaceAllStringFunc(s, func(m string) string {
			sub := reFlag.FindStringSubmatch(m)
			if strings.EqualFold(sub[2], "-u") && !strings.Contains(sub[4], ":") {
				return m // -u without user:password (git add -u, ps -u root)
			}
			if strings.HasPrefix(sub[4], Mark) {
				return m
			}
			return sub[1] + sub[2] + sub[3] + Mark
		}))
	}
	if may("key=", "token=", "secret=", "password=", "passwd=", "pwd=") {
		set(reAssign.ReplaceAllString(s, "${1}="+Mark))
	}
	if may("://") && strings.Contains(s, "@") {
		set(reUserinfo.ReplaceAllString(s, "${1}"+Mark+"@"))
	}
	if may("token", "key", "secret", "sig", "pass", "auth") && strings.Contains(s, "=") && (!filter ||
		afterAny(lower, queryWords, func(r string) bool { return strings.HasPrefix(strings.TrimLeft(r, nameBytes), "=") })) {
		set(reQuery.ReplaceAllString(s, "${1}"+Mark))
	}
	// Case-sensitive, as the pattern is.
	if !filter || containsAny(s, "ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_", "glpat-",
		"xoxa-", "xoxb-", "xoxp-", "xoxo-", "xoxs-", "xoxr-", "sk-", "mst_", "mmt_", "mpl_", "AKIA") {
		set(rePrefix.ReplaceAllString(s, Mark))
	}
	// THE SECOND BATCH. An audit ran 19 realistic secret shapes through the
	// patterns above and 13 passed; these are those 13. Each still runs only
	// when a literal it needs is present (the filter's rule above).
	if may("private key") {
		set(rePEM.ReplaceAllString(s, Mark))
	}
	if may("k_live_", "k_test_") {
		set(reStripe.ReplaceAllString(s, Mark))
	}
	if may("eyj") {
		set(reJWT.ReplaceAllString(s, Mark))
	}
	if may(`key"`, `token"`, `secret"`, `password"`, `passwd"`) {
		set(reJSONSecret.ReplaceAllString(s, `${1}"`+Mark+`"`))
	}
	if may("password", "passwd", "token", "secret", "key") && strings.Contains(s, ":") && (!filter ||
		afterAny(lower, yamlWords, func(r string) bool { return strings.HasPrefix(strings.TrimLeft(r, " \t"), ":") })) {
		set(reYAMLSecret.ReplaceAllString(s, "${1}"+Mark))
	}
	if may("key", "token", "secret", "password", "passwd", "pwd") && strings.Contains(s, "=") && (!filter ||
		afterAny(lower, assignWords, quotedAssignment)) {
		set(reAssignSp.ReplaceAllString(s, "${1}"+Mark))
	}
	if may("password", "passphrase", "passwd", "secret") {
		set(reSaid.ReplaceAllString(s, "${1}"+Mark))
	}
	if may("age-secret-key-1") {
		set(reAge.ReplaceAllString(s, Mark))
	}
	if may("aiza") {
		set(reGoogle.ReplaceAllString(s, Mark))
	}
	if may(".") && (!filter || hasDiscordRun(s)) {
		set(reDiscord.ReplaceAllString(s, Mark))
	}
	if may("hf_") {
		set(reHF.ReplaceAllString(s, Mark))
	}
	if may("api-key", "auth-token", "access-token") {
		set(reKeyHeader.ReplaceAllString(s, "${1}"+Mark))
	}
	// An AWS secret access key: exactly 40 base64 characters, mixed case with
	// a digit, in text that says aws or secret (a lowercase git sha is 40
	// characters too, and is left alone).
	//
	// ⚠ "SAYS aws OR secret" IS PART OF THE RULE, NOT THE FILTER, so it holds
	// with the filter off too: any 40-character mixed-case run (a build hash,
	// a base64 blob) would otherwise be one. It went through may() until the
	// filter-equivalence test's corpus grew a long run without either word
	// and found the two redactors disagreeing on it — the unfiltered one,
	// which is only that test's reference, had been redacting what the
	// shipped one never did.
	if containsAny(lower, "aws", "secret") {
		set(reAWSSecret.ReplaceAllStringFunc(s, func(m string) string {
			if len(m) != 40 || !hasUpperLowerDigit(m) {
				return m
			}
			return Mark
		}))
	}
	return s
}

// hasUpperLowerDigit reports whether s has an ASCII upper-case letter, a
// lower-case one and a digit.
func hasUpperLowerDigit(s string) bool {
	var up, low, dig bool
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			up = true
		case r >= 'a' && r <= 'z':
			low = true
		case r >= '0' && r <= '9':
			dig = true
		}
	}
	return up && low && dig
}

// fold is s lowercased the way the patterns' (?i) matches it: an ASCII
// letter's other case and, for k and s, the two non-ASCII letters Unicode
// folds with them (U+212A KELVIN SIGN, which ToLower already makes "k", and
// U+017F LONG S, which it leaves).
func fold(s string) string {
	return strings.Map(func(r rune) rune {
		if r = unicode.ToLower(r); r == 'ſ' {
			return 's'
		}
		return r
	}, s)
}

// containsAny reports whether s contains any of subs.
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// THE FILTER'S SHARPER TESTS. The literals above decide most patterns, but a
// few needed literals that ordinary prose and code are full of — "key" with an
// "=" and a ":" anywhere, a ".", a "--" — so their patterns ran over nearly
// every text (MEASURED at `a8fc65c3` on a synthetic view backfill of 1,200
// fields, 431 KB: 17 ms on a fast CPU, most of it reQuery, reFlag,
// reYAMLSecret, reAssignSp and reDiscord running on text they could not match;
// 5.5 ms with these tests). Each test below is a condition its pattern CANNOT match without —
// the credential word directly followed by what the pattern needs after it —
// read off the pattern, and TestTheFilterNeverChangesWhatIsRedacted checks
// the filtered and unfiltered redactors agree. Like the literals, each runs on
// the folded text (fold) for a pattern that ignores case.

// ws is what the patterns' \s matches (RE2: [\t\n\f\r ]).
const ws = "\t\n\f\r "

// nameBytes is what reQuery's [a-z0-9_.-] matches in folded text.
const nameBytes = "abcdefghijklmnopqrstuvwxyz0123456789_.-"

// 🔴 ONE LIST PER PATTERN, AND IT BUILDS BOTH THE PATTERN AND ITS TEST. Each
// of these is its pattern's alternatives, in the pattern's order (alternatives
// joins them into it), and the same slice is what afterAny / hasFlag look
// for. They were once a second copy beside the pattern: deleting a word from
// that copy alone made the filter skip a text the pattern would have
// redacted ("--passwd hunter2" passed whole) with every test green. Shared,
// a word is in both or in neither.
var (
	queryWords  = []string{"token", "key", "secret", "sig", "signature", "password", "passwd", "pass", "auth"}
	yamlWords   = []string{"password", "passwd", "token", "secret", "api_key", "apikey"}
	assignWords = []string{"key", "token", "secret", "password", "passwd", "pwd"}
	flagWords   = []string{"-p", "-u", "--password", "--passwd", "--pass", "--token", "--access-token", "--auth-token", "--api-key",
		"--apikey", "--secret", "--client-secret"}
)

// alternatives is words as a regexp alternation, each quoted.
func alternatives(words []string) string {
	q := make([]string, len(words))
	for i, w := range words {
		q[i] = regexp.QuoteMeta(w)
	}
	return strings.Join(q, "|")
}

// afterAny reports whether, at some occurrence in s of a word of words, f
// holds of the text right after it.
func afterAny(s string, words []string, f func(rest string) bool) bool {
	for _, w := range words {
		for i := 0; i <= len(s)-len(w); {
			j := strings.Index(s[i:], w)
			if j < 0 {
				break
			}
			if f(s[i+j+len(w):]) {
				return true
			}
			i += j + 1
		}
	}
	return false
}

// quotedAssignment: reAssignSp's \s*=\s* then a quote.
func quotedAssignment(r string) bool {
	r = strings.TrimLeft(r, ws)
	if !strings.HasPrefix(r, "=") {
		return false
	}
	r = strings.TrimLeft(r[1:], ws)
	return r != "" && (r[0] == '"' || r[0] == '\'')
}

// hasFlag: reFlag's (^|\s) before one of its flags, and = or \s after it.
func hasFlag(lower string) bool {
	for _, w := range flagWords {
		for i := 0; i <= len(lower)-len(w); {
			j := strings.Index(lower[i:], w)
			if j < 0 {
				break
			}
			at := i + j
			before := at == 0 || strings.IndexByte(ws, lower[at-1]) >= 0
			if end := at + len(w); before && end < len(lower) && (lower[end] == '=' || strings.IndexByte(ws, lower[end]) >= 0) {
				return true
			}
			i = at + 1
		}
	}
	return false
}

// hasDiscordRun: reDiscord's [MNO][A-Za-z0-9_-]{23,27} directly before a
// '.' — a run of at least 24 such bytes ending at a '.'.
func hasDiscordRun(s string) bool {
	run := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
			run++
		case c == '.' && run >= 24:
			return true
		default:
			run = 0
		}
	}
	return false
}
