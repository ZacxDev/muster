package prroute

import (
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/focus"
)

// WHICH PULL REQUESTS AND BRANCHES A TEXT NAMES, for the chief's session hint
// and focus_search (internal/api's prHints): a message that names a pull
// request one of the owner's online sessions created, or a branch one pushed,
// lists that session, whatever its repo directory or focus says.
//
// 🔴 A BARE "#N" NAMES NOTHING. "#2855" in prose is an issue, a ticket, a
// list item, a channel — and every repository has a pull request of most
// numbers. A pull request is named only with its repository: "<repo>#N",
// "<owner>/<repo>#N", or a github.com/<owner>/<repo>/pull/N URL. Even then it
// lists a session only when muster RECORDED that session creating that very
// pull request (session_prs), so "issue#12" names nothing unless a recorded
// repository is called "issue".
//
// 🔴 A BRANCH IS NAMED ONLY WHEN IT IS DISTINCTIVE (DistinctiveRef): "main",
// "master" or "develop" are pushed by every trunk-deployed session, and
// "follow-up", "read-only" or "gh-pages" are ordinary words, so a branch counts
// only with a '/', a digit, or a hyphenated or underscored part that is not a
// dictionary word (focus's dictionary).

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

// MaxBranchCandidates is how many branch names BranchCandidates reads from
// one text.
const MaxBranchCandidates = 50

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
// stored branch name (ValidRef) at least five bytes long that holds a '/', a
// digit, or — split on '-' and '_' into two or more parts — a part of three or
// more letters that is not a dictionary word. Never "main", "master",
// "develop", "follow-up", "read-only" or "gh-pages".
func DistinctiveRef(ref string) bool {
	if !validRef(ref) || len(ref) < 5 {
		return false
	}
	if strings.ContainsAny(ref, "/0123456789") {
		return true
	}
	parts := strings.FieldsFunc(strings.ToLower(ref), func(r rune) bool { return r == '-' || r == '_' })
	if len(parts) < 2 {
		return false
	}
	for _, p := range parts {
		if len(p) >= 3 && isLetters(p) && !focus.IsDictWord(p) {
			return true
		}
	}
	return false
}

// isLetters reports whether s is only lower-case ASCII letters.
func isLetters(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 'a' || s[i] > 'z' {
			return false
		}
	}
	return true
}

// leftBoundaries are what may sit directly before a branch name inside a
// longer run of name bytes: a ref path, a GitHub tree URL, a compare range.
var leftBoundaries = []string{"refs/heads/", "/tree/", "..."}

// BranchCandidates is every distinctive branch name (DistinctiveRef) text
// spells as a whole token, distinct, at most MaxBranchCandidates, in order. A
// token is a run of branch-name bytes, read from its start or from just after
// "refs/heads/", "/tree/" or "...", up to its end or to a "..."; a trailing
// '.' ends a sentence, not the name. So "feat/x" is spelled by "feat/x.",
// "refs/heads/feat/x", "github.com/o/r/tree/feat/x" and "main...feat/x", but
// not by "feat/x-2", "origin/feat/x" or "feat/x.v2". Case-sensitive, as git's
// names are.
func BranchCandidates(text string) []string {
	var out []string
	add := func(s string) {
		s = strings.TrimRight(s, ".")
		if len(out) < MaxBranchCandidates && DistinctiveRef(s) && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	for i := 0; i < len(text) && len(out) < MaxBranchCandidates; {
		if !isRefByte(text[i]) {
			i++
			continue
		}
		j := i
		for j < len(text) && isRefByte(text[j]) {
			j++
		}
		run := text[i:j]
		starts := []int{0}
		for _, b := range leftBoundaries {
			for k := 0; ; {
				n := strings.Index(run[k:], b)
				if n < 0 {
					break
				}
				k += n + len(b)
				starts = append(starts, k)
			}
		}
		slices.Sort(starts)
		for _, s := range starts {
			seg := run[s:]
			if e := strings.Index(seg, "..."); e >= 0 {
				seg = seg[:e]
			}
			add(seg)
		}
		i = j
	}
	return out
}

// NamesBranch reports whether text spells branch ref as a whole token: ref is
// one of BranchCandidates(text).
func NamesBranch(text, ref string) bool {
	return slices.Contains(BranchCandidates(text), ref)
}

// isRefByte is a byte a stored branch name may hold (refPattern).
func isRefByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || strings.IndexByte("._/+-", b) >= 0
}
