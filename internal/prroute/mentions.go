package prroute

import (
	"regexp"
	"strconv"
	"strings"
)

// WHICH PULL REQUESTS AND BRANCHES A TEXT NAMES, for the chief's session hint
// and focus_search (internal/api's prHints): a message that names a pull
// request or branch one of the owner's online sessions created or pushed
// lists that session, whatever its repo directory or focus says.
//
// 🔴 A BARE "#N" NAMES NOTHING. "#2855" in prose is an issue, a ticket, a
// list item, a channel — and every repository has a pull request of most
// numbers. A pull request is named only with its repository: "<repo>#N",
// "<owner>/<repo>#N", or a github.com/<owner>/<repo>/pull/N URL. Even then it
// lists a session only when muster RECORDED that session creating or pushing
// that very pull request (session_prs), so "issue#12" names nothing unless a
// recorded repository is called "issue".
//
// 🔴 A BRANCH IS NAMED ONLY WHEN IT IS DISTINCTIVE (DistinctiveRef): "main",
// "master" or "develop" are spelled in ordinary prose and pushed by every
// trunk-deployed session, so a branch with no '/', '-' or '_' never matches.

// Mention is a pull request a text names. Owner is "" when the text named the
// repository alone ("infra-config#12"). Owner and Repo are lower-cased
// (GitHub's names are case-insensitive).
type Mention struct {
	Owner, Repo string
	PR          int64
}

// MaxMentions is how many pull-request mentions Mentions reads from one text:
// a message is bounded work however many it spells.
const MaxMentions = 20

var (
	// mentionRef is "<repo>#N" or "<owner>/<repo>#N", not preceded by a name
	// character (so the repo is the whole word before '#') and with N ending
	// the number.
	mentionRef = regexp.MustCompile(`(?:^|[^A-Za-z0-9_.-])(?:([A-Za-z0-9_.-]+)/)?([A-Za-z0-9_.-]+)#([0-9]{1,12})\b`)
	// mentionURL is a pull request's GitHub URL.
	mentionURL = regexp.MustCompile(`(?i)\bgithub\.com/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/pull/([0-9]{1,12})\b`)
)

// Mentions is the pull requests text names, in order of appearance (URLs
// first), distinct, at most MaxMentions. A bare "#N" is not one.
func Mentions(text string) []Mention {
	var out []Mention
	seen := map[Mention]bool{}
	add := func(owner, repo, n string) {
		pr, err := strconv.ParseInt(n, 10, 64)
		if err != nil || pr <= 0 || len(out) >= MaxMentions {
			return
		}
		m := Mention{Owner: strings.ToLower(owner), Repo: strings.ToLower(repo), PR: pr}
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	for _, g := range mentionURL.FindAllStringSubmatch(text, MaxMentions) {
		add(g[1], g[2], g[3])
	}
	for _, g := range mentionRef.FindAllStringSubmatch(text, MaxMentions) {
		add(g[1], g[2], g[3])
	}
	return out
}

// Names reports whether m names pull request pr of repo ("<owner>/<name>", as
// session_prs stores it): the same name and number, and the same owner when m
// named one.
func (m Mention) Names(repo string, pr int64) bool {
	owner, name, ok := strings.Cut(strings.ToLower(repo), "/")
	return ok && pr > 0 && m.PR == pr && m.Repo == name && (m.Owner == "" || m.Owner == owner)
}

// NamesAny reports whether any of ms names pull request pr of repo.
func NamesAny(ms []Mention, repo string, pr int64) bool {
	for _, m := range ms {
		if m.Names(repo, pr) {
			return true
		}
	}
	return false
}

// DistinctiveRef reports whether branch ref is one a message can name: a
// stored branch name (ValidRef) holding a '/', '-' or '_', at least five bytes
// long — never "main", "master", "develop" or another bare word.
func DistinctiveRef(ref string) bool {
	return validRef(ref) && len(ref) >= 5 && strings.ContainsAny(ref, "/-_")
}

// NamesBranch reports whether text spells branch ref — a DistinctiveRef — as
// a whole token: not inside a longer name ("feat/x" is not named by
// "feat/x-2" or "y/feat/x"). A trailing '.' or ',' ends a sentence, not the
// name. Case-sensitive, as git's names are.
func NamesBranch(text, ref string) bool {
	if !DistinctiveRef(ref) {
		return false
	}
	for from := 0; from <= len(text)-len(ref); {
		i := strings.Index(text[from:], ref)
		if i < 0 {
			return false
		}
		i += from
		end := i + len(ref)
		if (i == 0 || !isRefByte(text[i-1])) && (end == len(text) || !continuesRef(text[end:])) {
			return true
		}
		from = i + 1
	}
	return false
}

// isRefByte is a byte a stored branch name may hold (refPattern).
func isRefByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || strings.IndexByte("._/+-", b) >= 0
}

// continuesRef reports whether rest, the text just after a candidate branch
// name, continues that name: a name byte, except a '.' that ends a sentence
// (followed by the end, or by a byte no name holds).
func continuesRef(rest string) bool {
	if !isRefByte(rest[0]) {
		return false
	}
	if rest[0] == '.' {
		return len(rest) > 1 && isRefByte(rest[1])
	}
	return true
}
