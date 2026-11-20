package flows

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// PUBLIC LINKS IN AN OFFICER'S OUTBOUND DRAFT (OutboundTextRefusal).
//
// An outbound draft may link to a public host (the host-name pattern is not in
// its screen), but the DEPLOYMENT'S OWN NAMES (MUSTER_FLOW_AGENT_DENY_WORDS)
// are still refused anywhere in the text — and a project's own repository link
// usually spells one of them: `github.com/<owner>/muster` carries "muster". So
// the screen first takes out every link on the outbound ALLOWLIST and screens
// what is left:
//
//   - An allowlist entry is `host[/segment…]` (parsePublicLink): a public host
//     and an optional path prefix. muster builds the list per draft: each of
//     the filing officer's standing-read repositories as
//     `github.com/<owner>/<repo>` (officers.Officer.StandingReadRepos — the
//     repositories the deployment itself declares public).
//   - A link in the text is a run `[http[s]://][www.]host[/path…]`. It is on
//     the allowlist when its host equals an entry's host and its first path
//     segments equal the entry's segments (both compared without case; a
//     segment may end in `.git` or a sentence's `.`), and the character before
//     it cannot continue a URL (no `user@host`, no `x.host`, no `…/https://`).
//   - 🔴 ONLY THE ALLOWLISTED PREFIX IS EXEMPT. What follows it in the link (a
//     deeper path: `/blob/main/<name>`) stays in the text and is screened like
//     every other word. A link the allowlist does not name — another repository
//     of the same owner included — is not exempt at all.
//   - A REPOSITORY ENTRY's name may also stand alone. An entry of exactly the
//     shape `host/<owner>/<repo>` declares <repo> public (publicRepoNames), and
//     each STANDALONE occurrence of <repo> in the draft (without case) is
//     masked for the scan of THAT name (MaskBareNames): "<repo> has a new
//     logo." passes though <repo> is a deny word. Standalone is an ALLOWLIST
//     (proseBeside): each neighbour is the start or end of the text,
//     whitespace, one of , ; ! ? ' ’ " “ ” ( ) [ ], or — after the name — a
//     `.` or `:` followed by whitespace or the end. Anything else joins the
//     name to a longer one, which is screened as ever: `<repo>-prod`,
//     `<repo>–prod`, `<repo>.internal`, `<repo>:5432`, `ops@<repo>`,
//     `…/<repo>-private`. Every OTHER deny word is scanned against the
//     unmasked text, so `<repo> prod` stays refused when it is a deny word.
//     Only the repository name: never <owner>, never a
//     name that is also an entry's owner, never a host-only or deeper entry's
//     last segment.
//   - Everything else in the screen is unchanged: an IP address, an internal
//     host name, a credential- or token-shaped string, an infrastructure word
//     or any other deployment name anywhere OUTSIDE an allowlisted link is
//     refused.

var (
	// publicLinkRE is an allowlist entry, after parsePublicLink took off a
	// scheme and a trailing slash: a host of two or more labels and optional
	// path segments.
	publicLinkRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)+(?:/[A-Za-z0-9._~-]+)*$`)
	// linkRunRE is a link-shaped run in text: optional scheme and www., a
	// dotted host, then any path segments.
	linkRunRE = regexp.MustCompile(`(?i)(?:https?://)?(?:www\.)?[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)+(?:/[A-Za-z0-9._~-]*)*`)
	// linkPrefixRE is the scheme and www. linkRunRE may begin with.
	linkPrefixRE = regexp.MustCompile(`(?i)^(?:https?://)?(?:www\.)?`)
)

// parsePublicLink is entry as an allowlist entry — lower-case host, path
// segments as given, no scheme, no trailing slash — or why it is refused: not
// host[/path] shaped, or a host the screen would refuse anyway (an IP address,
// an internal host name), which no allowlist may exempt.
func parsePublicLink(entry string) (string, error) {
	e := strings.TrimSpace(entry)
	e = linkPrefixRE.ReplaceAllString(e, "")
	e = strings.TrimRight(e, "/")
	host, path, _ := strings.Cut(e, "/")
	host = strings.ToLower(host)
	if path != "" {
		e = host + "/" + path
	} else {
		e = host
	}
	switch {
	case !publicLinkRE.MatchString(e):
		return "", fmt.Errorf("%q is not a public link: want host[/path], e.g. github.com/example/project", entry)
	case ipAddressDeny.re.MatchString(host), internalHostDeny.re.MatchString(host):
		return "", fmt.Errorf("%q names an IP address or an internal host: never a public link", entry)
	}
	return e, nil
}

// StripPublicLinks is text with every link on allow (parsePublicLink entries;
// an entry that does not parse is ignored) cut down to what follows its
// allowlisted prefix: the prefix becomes a space, the rest of the link stays.
func StripPublicLinks(text string, allow []string) string {
	type entry struct {
		host string
		segs []string
	}
	var es []entry
	for _, a := range allow {
		p, err := parsePublicLink(a)
		if err != nil {
			continue
		}
		host, path, _ := strings.Cut(p, "/")
		e := entry{host: host}
		if path != "" {
			e.segs = strings.Split(path, "/")
		}
		es = append(es, e)
	}
	if len(es) == 0 {
		return text
	}
	var b strings.Builder
	last := 0
	for _, m := range linkRunRE.FindAllStringIndex(text, -1) {
		start, end := m[0], m[1]
		if start > 0 && continuesURL(text[start-1]) {
			continue
		}
		run := text[start:end]
		pre := len(linkPrefixRE.FindString(run))
		host, rest, _ := strings.Cut(run[pre:], "/")
		var segs []string
		if rest != "" {
			segs = strings.Split(rest, "/")
		}
		cut := -1 // bytes of run that are the allowlisted prefix
		for _, e := range es {
			if !strings.EqualFold(host, e.host) || len(segs) < len(e.segs) {
				continue
			}
			ok := true
			for i, s := range e.segs {
				if !segmentIs(segs[i], s) {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}
			n := pre + len(host)
			for _, s := range segs[:len(e.segs)] {
				n += 1 + len(s)
			}
			if n > cut {
				cut = n
			}
		}
		if cut < 0 {
			continue
		}
		b.WriteString(text[last:start])
		b.WriteString(" ")
		last = start + cut
	}
	b.WriteString(text[last:])
	return b.String()
}

// publicRepoNames are the repository names allow declares public, lower-case:
// the <repo> of each entry (parsePublicLink) of exactly the shape
// `host/<owner>/<repo>`, a `.git` suffix dropped — less any name that is also
// an entry's <owner>, so an owner name is never exempt. An entry that does not
// parse, or has another number of path segments, declares none.
func publicRepoNames(allow []string) map[string]bool {
	names, owners := map[string]bool{}, map[string]bool{}
	for _, a := range allow {
		p, err := parsePublicLink(a)
		if err != nil {
			continue
		}
		segs := strings.Split(p, "/")[1:]
		if len(segs) != 2 {
			continue
		}
		owners[strings.ToLower(segs[0])] = true
		if n := strings.TrimSuffix(strings.ToLower(segs[1]), ".git"); n != "" {
			names[n] = true
		}
	}
	for o := range owners {
		delete(names, o)
	}
	return names
}

// MaskBareNames is text with every standalone occurrence of a name in names
// (lower-case; compared without case) replaced by as many spaces: one whose
// neighbour on each side is prose (proseBeside).
func MaskBareNames(text string, names map[string]bool) string {
	for n := range names {
		re := regexp.MustCompile(`(?i)` + regexp.QuoteMeta(n))
		var b strings.Builder
		last := 0
		for _, m := range re.FindAllStringIndex(text, -1) {
			if !proseBeside(text[:m[0]], true) || !proseBeside(text[m[1]:], false) {
				continue
			}
			b.WriteString(text[last:m[0]])
			b.WriteString(strings.Repeat(" ", m[1]-m[0]))
			last = m[1]
		}
		b.WriteString(text[last:])
		text = b.String()
	}
	return text
}

// proseAround are the characters that may stand beside a bare name on either
// side: prose punctuation that never joins two names.
const proseAround = ",;!?'\u2019\"\u201c\u201d()[]"

// proseBeside reports whether the text beside a name leaves it standing
// alone: side is what precedes the name (before) or what follows it. Only an
// ALLOWLIST does: the start or end of the text, whitespace, a proseAround
// character, or — after the name only — a `.` or `:` followed by whitespace
// or the end of the text (a sentence's end, "<name>: the new logo").
// Anything else — a letter, digit, combining mark, dash of any kind, `.` or
// `:` inside a run, `@`, `+`, `~`, `\`, `/`, `_` — joins it to a longer name.
func proseBeside(side string, before bool) bool {
	if side == "" {
		return true
	}
	if before {
		r, _ := utf8.DecodeLastRuneInString(side)
		return unicode.IsSpace(r) || strings.ContainsRune(proseAround, r)
	}
	r, n := utf8.DecodeRuneInString(side)
	switch {
	case unicode.IsSpace(r), strings.ContainsRune(proseAround, r):
		return true
	case r == '.', r == ':':
		f, _ := utf8.DecodeRuneInString(side[n:])
		return side[n:] == "" || unicode.IsSpace(f)
	}
	return false
}

// segmentIs reports whether link segment got is allowlist segment want,
// without case, allowing a `.git` suffix and a sentence's trailing dots.
func segmentIs(got, want string) bool {
	got = strings.TrimRight(got, ".")
	return strings.EqualFold(got, want) || strings.EqualFold(strings.TrimSuffix(strings.ToLower(got), ".git"), want)
}

// continuesURL reports whether c, just before a link-shaped run, would make
// that run part of something else: a longer host (x.host, or a non-ASCII
// letter: xé.host), a userinfo (user@host), a path (…/host) or a scheme or
// port (…:host).
func continuesURL(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c >= 0x80:
		return true
	}
	return strings.IndexByte("._-~%+@/:\\", c) >= 0
}
