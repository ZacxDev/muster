package ui

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// mentions.go — a chat message that NAMES another session, agent or task draws
// that name as a link to it.
//
// 🔴 A MENTION IS LINKED ONLY WHEN IT RESOLVES. The index is built from what
// exists — the agents and mirrored sessions the switcher lists (the same
// agents list and mirror hub its entries come from, internal/api
// mentionIndex), and a task id the task store answers for — so a name that
// merely LOOKS like a codename, a slug or a task ref stays plain text. No
// pattern alone ever makes a link.
//
// 🔴 THE HREF IS THE INDEX'S, NEVER THE MESSAGE'S. The message supplies only
// the KEY looked up; what is emitted is the path the index built (AgentPath,
// MirrorCardHref, "/tasks/<id>"), escaped. The label is the matched token,
// whose characters (mentionRunRe) contain nothing the escape changes.
//
// What counts as a mention (mdMention applies it, on already-escaped inline
// text, AFTER code spans, markdown links and bare URLs are vaulted, so none of
// those is ever looked inside; fenced code never reaches mdInline at all):
//
//   - A TOKEN is a maximal run of [A-Za-z0-9_.:/@#-]. It must start at a
//     boundary — the start of the text, white space, or one of mentionLeft (an
//     opening bracket, an emphasis marker, `,`, `|`, the end of an entity `;`
//     or of a <br> `>`) — and end at one (white space, the end, mentionRight).
//     So `session=mango:2`, `a/mango:2`, `mango:2=on`, `repo#12` and the
//     `#39` of `&#39;` are never tokens of their own.
//   - Trailing `.`, `:` and `/` belong to the sentence: `see mango:2.` links
//     `mango:2`.
//   - CASE-SENSITIVE and EXACT: the token must equal a slug or codename as the
//     index holds it. A display name or human session name is not matched
//     (they are free text, often several words).
//   - A BARE token is linked only when it is DISTINCTIVE — it holds a digit or
//     one of `-_.:/` (`mango:2`, `fix-login`, `scout2`). A plain word (an agent
//     slugged `scout` or `review`) is linked only when written `@scout`: an
//     agent may be named after an English word, and every "review" in a reply
//     must not become a link to it.
//   - `#N` is task N, linked when the task exists — except right after the
//     word PR, PRs, pull, issue or issues (case-insensitive), where it is a
//     GitHub number. ⚠ That exclusion IS a word heuristic; the deterministic
//     alternative is to link only `task #N`, which would miss the bare `#N`
//     the chief writes.
//   - A key two entries share with different targets (one codename on two
//     hosts, a slug equal to a codename) is ambiguous and links nowhere.

// mentionTaskLookupCap bounds how many DISTINCT task ids one index asks the
// store about; a ref past it stays plain. Each answer is remembered, so a
// transcript that names one task a hundred times costs one read.
const mentionTaskLookupCap = 32

// MentionIndex resolves a message's mentions to links. Build it once per page
// or response (internal/api mentionIndex), not per message. A nil index links
// nothing: every renderer without one is unchanged.
type MentionIndex struct {
	names map[string]mentionTarget
	task  func(id int64) bool

	mu      sync.Mutex
	tasks   map[int64]bool
	lookups int
}

type mentionTarget struct {
	href, kind string // kind "" = ambiguous
}

// NewMentionIndex indexes agents by slug (AgentPath) and mirrored sessions by
// their codename as the switcher and the palette find them (mirrorCodeName,
// MirrorCardHref). taskExists answers whether task id exists for the viewer;
// nil links no task.
func NewMentionIndex(agentNames []string, mirrors []MirrorCardView, taskExists func(id int64) bool) *MentionIndex {
	mx := &MentionIndex{names: map[string]mentionTarget{}, task: taskExists, tasks: map[int64]bool{}}
	for _, n := range agentNames {
		mx.add(n, AgentPath(n), "agent")
	}
	for _, m := range mirrors {
		mx.add(mirrorCodeName(m), MirrorCardHref(m), "session")
	}
	return mx
}

// mentionKeyRe is what a key must look like to be matchable at all: a whole
// token that does not end in punctuation the matcher trims, and does not start
// with the `@`/`#` sigils.
var mentionKeyRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]*$`)

func (mx *MentionIndex) add(key, href, kind string) {
	if key == "" || href == "" || !mentionKeyRe.MatchString(key) || strings.TrimRight(key, mentionTrail) != key {
		return
	}
	if prev, ok := mx.names[key]; ok && prev.href != href {
		mx.names[key] = mentionTarget{}
		return
	}
	mx.names[key] = mentionTarget{href: href, kind: kind}
}

// taskExists is mx.task, remembered per id and capped (mentionTaskLookupCap).
func (mx *MentionIndex) taskExists(id int64) bool {
	if mx.task == nil {
		return false
	}
	mx.mu.Lock()
	defer mx.mu.Unlock()
	if ok, seen := mx.tasks[id]; seen {
		return ok
	}
	if mx.lookups >= mentionTaskLookupCap {
		return false
	}
	mx.lookups++
	ok := mx.task(id)
	mx.tasks[id] = ok
	return ok
}

// resolve is token's link, or ok false.
func (mx *MentionIndex) resolve(token string) (href, kind string, ok bool) {
	switch {
	case strings.HasPrefix(token, "#"):
		id, err := strconv.ParseInt(token[1:], 10, 64)
		if err != nil || id <= 0 || token[1] == '0' || !mx.taskExists(id) {
			return "", "", false
		}
		return "/tasks/" + strconv.FormatInt(id, 10), "task", true
	case strings.HasPrefix(token, "@"):
		t := mx.names[token[1:]]
		return t.href, t.kind, t.kind != ""
	default:
		if !strings.ContainsAny(token, "0123456789-_.:/") {
			return "", "", false // a plain word: only as @word
		}
		t := mx.names[token]
		return t.href, t.kind, t.kind != ""
	}
}

var (
	// mentionRunRe is a candidate token. \x00 (the vault delimiter) is not in
	// it, so a placeholder's digits are never a token.
	mentionRunRe = regexp.MustCompile(`[A-Za-z0-9_.:/@#-]+`)
	// mentionNotGitHub is the word before a `#N` that makes it a GitHub number.
	mentionNotGitHub = map[string]bool{"pr": true, "prs": true, "pull": true, "issue": true, "issues": true}
)

const (
	// mentionTrail is trimmed off a token's end: it belongs to the sentence.
	mentionTrail = ".:/"
	// mentionLeft and mentionRight are the bytes a token may sit between
	// (besides white space and the text's ends).
	mentionLeft  = "([{*_~;>,|"
	mentionRight = ")]}*_~&,;!?<|"
)

func mentionSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }

// mdMention links s's mentions, stashing each anchor in v. s is escaped inline
// text with code spans, links and URLs already vaulted (mdInlineWith).
func mdMention(s string, v *mdVault, mx *MentionIndex) string {
	if mx == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, loc := range mentionRunRe.FindAllStringIndex(s, -1) {
		start, end := loc[0], loc[1]
		if start > 0 && !mentionSpace(s[start-1]) && !strings.ContainsRune(mentionLeft, rune(s[start-1])) {
			continue
		}
		if end < len(s) && !mentionSpace(s[end]) && !strings.ContainsRune(mentionRight, rune(s[end])) {
			continue
		}
		token := strings.TrimRight(s[start:end], mentionTrail)
		if len(token) < 2 {
			continue
		}
		if token[0] == '#' && mentionNotGitHub[strings.ToLower(mentionWordBefore(s, start))] {
			continue
		}
		href, kind, ok := mx.resolve(token)
		if !ok {
			continue
		}
		b.WriteString(s[last:start])
		b.WriteString(v.stash(mentionAnchor(href, kind, token)))
		last = start + len(token)
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// mentionWordBefore is the word ending just before s[i], across white space.
func mentionWordBefore(s string, i int) string {
	j := i
	for j > 0 && mentionSpace(s[j-1]) {
		j--
	}
	k := j
	for k > 0 && (s[k-1] >= 'a' && s[k-1] <= 'z' || s[k-1] >= 'A' && s[k-1] <= 'Z') {
		k--
	}
	return s[k:j]
}

// mentionAnchor is one mention's link. It opens IN PLACE (no target): it is a
// page of this app, unlike mdAnchor's external links. hx-boost=false makes it
// a plain navigation wherever it is drawn (the chief panel, a mirrored view),
// as the dispatch card's task link is. data-mention names what it links to.
// The classes are mdAnchor's (already in the built stylesheet).
func mentionAnchor(href, kind, label string) string {
	return `<a href="` + mdEscape(href) + `" data-mention="` + kind + `" hx-boost="false" class="text-accent underline decoration-accent/40 underline-offset-2 hover:text-fg">` +
		label + `</a>`
}
