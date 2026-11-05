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
	"math"
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
func Secrets(s string) string { return secrets(s, true, nil) }

// HasSecretValue reports whether s holds a secret-shaped VALUE: THE
// secret-shape test for a refusal of an officer's SHIP content (its pull
// request diff, title, body and comments; internal/officership's draft gate).
//
// It refuses on EITHER of two rules:
//
//   - THE VALUE (Secrets, with one narrowing). Every shape Secrets knows by its
//     VALUE (a token prefix, a PEM block, a JWT, a Stripe, age, Google,
//     Hugging Face or Discord key, an AWS secret) refuses as it redacts. A
//     shape Secrets knows only by a credential NAME (a header, "the password
//     is …", a URL query parameter, and the key forms below) refuses on this
//     rule when the value is itself secret-shaped (literalSecret). A URL's
//     userinfo refuses when it holds a password that is not a reference.
//   - 🔴 THE KEY (keyLiteral). A credential-like KEY (one ending in password,
//     passwd, passphrase, pwd, secret, token, api_key/apikey, private_key,
//     secret_key, credential or credentials) holding a LITERAL refuses,
//     whatever the literal looks like: `password: hunter2`, a diceware
//     passphrase, a letters-only word, a block scalar. Not a literal: an empty
//     or null value, a boolean, a number, a reference (${…}, $(…), $VAR,
//     {{…}}, valueFrom, a flow mapping, an alias, a <placeholder>), a SOPS
//     ENC[…] value, a path or a URL — and a key that itself NAMES a reference
//     (existingSecret…, passwordSecret, tokenSecret, …SecretName, …SecretRef,
//     …secretKeyRef). So `automountServiceAccountToken: false`,
//     `existingSecret: x`, `passwordSecret: x` and `password: ${DB_PASSWORD}`
//     pass.
func HasSecretValue(s string) bool { return HasSecretValueIn("", s) }

// HasSecretValueIn is HasSecretValue for content of the file at path. In a
// SOURCE file (sourceExts: Go, TypeScript, Python, …) an unquoted value is an
// expression (`Password: cfg.Password,`), so the key rule judges only a
// QUOTED string literal there; anywhere else (YAML, JSON, shell, .env, prose)
// every value is.
func HasSecretValueIn(path, s string) bool {
	return secrets(s, true, literalSecret) != s || keyLiteral(s, isSource(path))
}

// HasSecretPairIn reports whether lines (a manifest's lines, in order) hold a
// list item whose credential-like `name:` has a sibling `value:` literal,
// either line marked in changed (nil: every line). Not implemented yet.
func HasSecretPairIn(path string, lines []string, changed []bool) bool { return false }

// credKeyWords are the endings of a credential-like key, compared with the
// key lowercased and its _ - . separators removed.
var credKeyWords = []string{"password", "passwd", "passphrase", "pwd", "secret", "token", "apikey", "privatekey", "secretkey",
	"credential", "credentials"}

// refKeyEndings are keys that NAME a reference to a secret held elsewhere
// (a Secret's name, a key selector), compared the same way; a key starting
// with "existingsecret" is one too.
var refKeyEndings = []string{"existingsecret", "passwordsecret", "tokensecret", "credentialssecret", "credentialsecret", "secretname",
	"secretref", "secretkeyref", "keyref", "secretrefname"}

// sourceExts are the source-code file extensions (HasSecretValueIn).
var sourceExts = []string{".go", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".py", ".rb", ".java", ".kt", ".kts", ".rs", ".c", ".h",
	".cc", ".cpp", ".hpp", ".cs", ".swift", ".php", ".scala", ".lua", ".dart", ".ex", ".exs"}

func isSource(path string) bool {
	for _, e := range sourceExts {
		if strings.HasSuffix(path, e) {
			return true
		}
	}
	return false
}

var (
	// reKeyColon: a YAML or JSON `key: value` line (key optionally quoted,
	// optionally a list item).
	reKeyColon = regexp.MustCompile(`^\s*(?:-\s+)?["']?([A-Za-z0-9_.$-]+)["']?\s*:(?:\s+(.*)|\s*)$`)
	// reJSONPair: every "key": value pair of a JSON object on one line.
	reJSONPair = regexp.MustCompile(`"([A-Za-z0-9_.$-]+)"\s*:\s*("(?:[^"\\]|\\.)*"|[^,}\s]+)`)
	// reKeyAssign: NAME=value / NAME = 'value' / name := "value" (export
	// optional).
	reKeyAssign = regexp.MustCompile(`(?:^|[\s;(])(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*:?=\s*("[^"]*"|'[^']*'|` + "`[^`]*`" + `|[^\s;&]+)`)
	// reKeyFlag: a credential flag and its value.
	reKeyFlag = regexp.MustCompile(`(?:^|\s)--(password|passwd|pass|token|access-token|auth-token|api-key|apikey|secret|client-secret|private-key)(?:=|\s+)("[^"]*"|'[^']*'|\S+)`)
	// reNumber: a number, optionally with a duration or size unit.
	reNumber = regexp.MustCompile(`^[+-]?[0-9]+(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?(?:ns|us|ms|s|m|h|d|w|Ki|Mi|Gi|Ti|K|M|G|T|k)?$`)
	// reAnchor: a YAML anchor before a value (&name value).
	reAnchor = regexp.MustCompile(`^&[^\s]+\s+`)
)

// credKey reports whether key is credential-like and does not name a
// reference.
func credKey(key string) bool {
	k := strings.NewReplacer("_", "", "-", "", ".", "", "$", "").Replace(strings.ToLower(key))
	if strings.HasPrefix(k, "existingsecret") {
		return false
	}
	for _, e := range refKeyEndings {
		if strings.HasSuffix(k, e) {
			return false
		}
	}
	for _, w := range credKeyWords {
		if strings.HasSuffix(k, w) {
			return true
		}
	}
	return false
}

// keyLiteral reports whether s has a credential-like key (credKey) holding a
// LITERAL: any value except an empty or null one, a boolean, a number (with a
// duration or size unit), a reference (${…}, $(…), $VAR, {{…}}, %{…}, a
// <placeholder>, a YAML alias or flow mapping, anything mentioning
// valueFrom), a SOPS ENC[…] value, a path (/…, ./…, ~/…) or a URL. A YAML
// block scalar (| or >) is a literal. In code, only a quoted value is judged.
func keyLiteral(s string, code bool) bool {
	judge := func(key, v string) bool {
		if !credKey(key) {
			return false
		}
		v = strings.TrimSpace(v)
		quoted := len(v) >= 2 && strings.ContainsRune(`"'`+"`", rune(v[0])) && v[len(v)-1] == v[0]
		if !quoted {
			v = strings.TrimSuffix(strings.TrimSpace(v), ",")
			quoted = len(v) >= 2 && strings.ContainsRune(`"'`+"`", rune(v[0])) && v[len(v)-1] == v[0]
		}
		if code && !quoted {
			return false
		}
		if !quoted {
			if i := strings.Index(v, " #"); i >= 0 {
				v = strings.TrimSpace(v[:i])
			}
		} else {
			v = v[1 : len(v)-1]
		}
		return isLiteral(v, quoted)
	}
	for _, line := range strings.Split(s, "\n") {
		for _, m := range reJSONPair.FindAllStringSubmatch(line, -1) {
			if judge(m[1], m[2]) {
				return true
			}
		}
		if m := reKeyColon.FindStringSubmatch(line); m != nil && judge(m[1], m[2]) {
			return true
		}
		for _, m := range reKeyAssign.FindAllStringSubmatch(line, -1) {
			if judge(m[1], m[2]) {
				return true
			}
		}
		for _, m := range reKeyFlag.FindAllStringSubmatch(line, -1) {
			if judge(m[1], m[2]) {
				return true
			}
		}
	}
	return false
}

// isLiteral reports whether v (unquoted already) is a literal value; a
// quoted one is never a boolean, a null or a block scalar.
func isLiteral(v string, quoted bool) bool {
	v = reAnchor.ReplaceAllString(strings.TrimSpace(v), "")
	if v == "" {
		return false
	}
	if !quoted {
		switch strings.ToLower(v) {
		case "null", "~", "true", "false", "yes", "no", "on", "off":
			return false
		}
		if v == "|" || v == ">" || strings.HasPrefix(v, "|") || strings.HasPrefix(v, ">") {
			return true // a block scalar: its literal is on the next lines
		}
		if reNumber.MatchString(v) {
			return false
		}
	}
	for _, p := range []string{"${", "$(", "{{", "%{", "{", "*", "<", "ENC["} {
		if strings.HasPrefix(v, p) {
			return false
		}
	}
	if len(v) > 1 && v[0] == '$' && (v[1] == '_' || v[1] >= 'A' && v[1] <= 'Z' || v[1] >= 'a' && v[1] <= 'z') {
		return false
	}
	if strings.Contains(v, "valueFrom") || strings.HasPrefix(v, "/") || strings.HasPrefix(v, "./") || strings.HasPrefix(v, "~/") {
		return false
	}
	if i := strings.Index(v, "://"); i > 0 && !strings.ContainsAny(v[:i], " \t:@") {
		return false // a URL (a password in its userinfo is the userinfo rule's)
	}
	return true
}

// minLiteralSecret is the shortest value literalSecret calls a secret.
const minLiteralSecret = 12

var (
	// reRefName: a lowercase resource-style name with at least one '-' or '.'
	// separator (db-credentials, my-release-postgresql, app.tls), or an
	// UPPER_SNAKE variable name (DB_PASSWORD, ADMIN_TOKEN_REF).
	reRefName = regexp.MustCompile(`^(?:[a-z0-9]+(?:[-.][a-z0-9]+)+|[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+)$`)
	// reUUID is a UUID, which reRefName's first form would otherwise excuse.
	reUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// literalSecret reports whether v, the value a credential-NAMED pattern
// captured, is itself secret-shaped: at least minLiteralSecret characters
// with no whitespace; not a reference, a SOPS ENC[…] value, a path or a URL;
// not a resource or
// variable name (reRefName, a UUID excepted); letters and a digit or a base64
// symbol (+ / =); and at least 2.5 bits of entropy per character.
func literalSecret(v string) bool {
	v = strings.TrimSpace(v)
	if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i]) // a YAML comment
	}
	v = strings.TrimSuffix(v, ",")
	v = strings.Trim(v, `"'`)
	if reUUID.MatchString(v) {
		return true
	}
	if strings.HasPrefix(v, "ENC[") {
		return false // a SOPS-encrypted value
	}
	if len(v) < minLiteralSecret || strings.ContainsAny(v, ws) || isReference(v) || reRefName.MatchString(v) {
		return false
	}
	if strings.HasPrefix(v, "/") || strings.HasPrefix(v, "./") || strings.HasPrefix(v, "~/") || strings.Contains(v, "://") {
		return false
	}
	var letter, other bool
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z':
			letter = true
		case r >= '0' && r <= '9' || r == '+' || r == '/' || r == '=':
			other = true
		}
	}
	return letter && other && entropy(v) >= 2.5
}

// isReference reports whether v names a value held elsewhere: a shell or
// template expansion, a YAML flow mapping, alias, anchor or block scalar, or
// a placeholder.
func isReference(v string) bool {
	if v == "" {
		return true
	}
	for _, p := range []string{"${", "$(", "{{", "{", "*", "&", "<", "|", ">", "%"} {
		if strings.HasPrefix(v, p) {
			return true
		}
	}
	if len(v) > 1 && v[0] == '$' && (v[1] == '_' || v[1] >= 'A' && v[1] <= 'Z' || v[1] >= 'a' && v[1] <= 'z') {
		return true
	}
	return strings.Contains(v, "valueFrom")
}

// entropy is v's Shannon entropy in bits per byte.
func entropy(v string) float64 {
	var n [256]int
	for i := 0; i < len(v); i++ {
		n[v[i]]++
	}
	h := 0.0
	for _, c := range n {
		if c > 0 {
			p := float64(c) / float64(len(v))
			h -= p * math.Log2(p)
		}
	}
	return h
}

// secrets is Secrets; with filter false every pattern runs, which is what the
// test compares the filtered path against.
//
// isSecret, when set, is asked of every match of a credential-NAMED pattern
// with the value that name holds, and a match whose value it does not call a
// secret is left as it is (HasSecretValue). nil is Secrets: every pattern runs
// exactly as it always has (the ReplaceAllString calls below are untouched on
// that path).
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
func secrets(s string, filter bool, isSecret func(value string) bool) string {
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
	// named runs a credential-NAMED pattern: as Secrets did with isSecret
	// nil; with isSecret, a match whose value (sub[group], or the match after
	// sub[1] when group is 0) isSecret does not call a secret stays.
	named := func(re *regexp.Regexp, tmpl string, group int) {
		if isSecret == nil {
			set(re.ReplaceAllString(s, tmpl))
			return
		}
		set(re.ReplaceAllStringFunc(s, func(m string) string {
			sub := re.FindStringSubmatch(m)
			if sub == nil {
				return m
			}
			v := strings.TrimPrefix(m, sub[1])
			if group > 0 {
				v = sub[group]
			}
			if !isSecret(v) {
				return m
			}
			return Mark
		}))
	}
	if may("authorization") {
		named(reAuthHeader, "${1}"+Mark, 2)
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
			if isSecret != nil && !isSecret(sub[4]) {
				return m
			}
			return sub[1] + sub[2] + sub[3] + Mark
		}))
	}
	if may("key=", "token=", "secret=", "password=", "passwd=", "pwd=") {
		named(reAssign, "${1}="+Mark, 2)
	}
	if may("://") && strings.Contains(s, "@") {
		if isSecret == nil {
			set(reUserinfo.ReplaceAllString(s, "${1}"+Mark+"@"))
		} else {
			// A userinfo is a secret when it holds a password that is not a
			// reference (user:${PASSWORD}@ is one; user@ holds none).
			set(reUserinfo.ReplaceAllStringFunc(s, func(m string) string {
				sub := reUserinfo.FindStringSubmatch(m)
				_, pass, ok := strings.Cut(strings.TrimSuffix(strings.TrimPrefix(m, sub[1]), "@"), ":")
				if !ok || isReference(pass) {
					return m
				}
				return Mark
			}))
		}
	}
	if may("token", "key", "secret", "sig", "pass", "auth") && strings.Contains(s, "=") && (!filter ||
		afterAny(lower, queryWords, func(r string) bool { return strings.HasPrefix(strings.TrimLeft(r, nameBytes), "=") })) {
		named(reQuery, "${1}"+Mark, 0)
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
		named(reJSONSecret, `${1}"`+Mark+`"`, 0)
	}
	if may("password", "passwd", "token", "secret", "key") && strings.Contains(s, ":") && (!filter ||
		afterAny(lower, yamlWords, func(r string) bool { return strings.HasPrefix(strings.TrimLeft(r, " \t"), ":") })) {
		named(reYAMLSecret, "${1}"+Mark, 2)
	}
	if may("key", "token", "secret", "password", "passwd", "pwd") && strings.Contains(s, "=") && (!filter ||
		afterAny(lower, assignWords, quotedAssignment)) {
		named(reAssignSp, "${1}"+Mark, 2)
	}
	if may("password", "passphrase", "passwd", "secret") {
		named(reSaid, "${1}"+Mark, 2)
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
		named(reKeyHeader, "${1}"+Mark, 2)
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
