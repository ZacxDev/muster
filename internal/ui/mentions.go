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
// 🔴 A MENTION IS LINKED ONLY WHEN IT RESOLVES. The index answers from what
// exists — the agents and mirrored sessions the switcher lists (the agents
// list and the mirror hub's hosts, internal/api mentionIndex), and a task id
// the task store answers for — so a name that merely LOOKS like a codename, a
// slug or a task ref stays plain text. No pattern alone ever makes a link.
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
//     boundary — the start of the text, white space, one of mentionLeft (an
//     opening bracket, an emphasis marker, `,`, `|`, the end of an entity `;`
//     or of a <br> `>`), or a curly apostrophe or em dash — and end at one
//     (white space, the end, mentionRight, ’ or —). So `session=mango:2`,
//     `a/mango:2`, `mango:2=on`, `repo#12` and the `#39` of `&#39;` are never
//     tokens of their own. Leading `_` is italics, not the name: `_mango:2_`
//     links `mango:2`.
//   - Trailing `.`, `:`, `/` and `_` belong to the sentence: `see mango:2.`
//     links `mango:2`.
//   - CASE-SENSITIVE and EXACT: the token must equal a slug or codename as the
//     index holds it. A display name or human session name is not matched
//     (they are free text, often several words).
//   - A BARE token is linked only when it is DISTINCTIVE — it holds one of
//     `-_.:/`, or a digit AND a letter (`mango:2`, `fix-login`, `scout2`). A
//     plain word (an agent slugged `scout`) is linked only when written
//     `@scout`: every "review" in a reply must not link to an agent named
//     review. An all-digit key is never indexed at all — an unnamed tmux
//     session "12" would otherwise link "I changed 12 files".
//   - `#N` is task N, linked when the task exists — except after the word PR,
//     PRs, pull, request(s), issue(s), merge or merged (case-insensitive), where
//     it is a GitHub number. The word may be followed by `:` or `*` (`PR: #14`,
//     `**PR** #14`), and it carries across a list of refs joined by commas,
//     "and" or "or" (`PRs #12, #13 and #14`). ⚠ That exclusion IS a word
//     heuristic; the deterministic alternative is to link only `task #N`,
//     which would miss the bare `#N` the chief writes (operator's decision:
//     keep bare #N).
//   - A key two entries share with different targets (one codename on two
//     hosts, a slug equal to a codename) is ambiguous and links nowhere.
//   - A STREAMED reply's last token is not linked while it is still arriving
//     (ChatStreamHTML): "#1234" streams as #1, #12, #123, and each would be a
//     different task. It is linked once the stored reply is drawn.

// mentionTaskLookupCap bounds how many DISTINCT task ids one index asks the
// store about; a ref past it stays plain.
const mentionTaskLookupCap = 64

// MentionNames is what a mention can name besides a task: agent slugs and
// mirrored sessions (keyed by mirrorCodeName, linked to MirrorCardHref).
type MentionNames struct {
	Agents   []string
	Sessions []MirrorCardView
}

// MentionIndex resolves a message's mentions to links. Build one per page,
// response, streamed turn or socket (internal/api mentionIndex), never per
// message. A nil index links nothing: every renderer without one is unchanged.
//
// ⚡ IT IS LAZY. The names are loaded once, on the first token that could be
// one; the tasks are asked about only when a `#N` is to be resolved — every id
// primed by then (Prime) in ONE call, and each answer remembered. A
// conversation that names nothing reads nothing.
type MentionIndex struct {
	loadNames func() MentionNames
	loadTasks func(ids []int64) map[int64]bool

	// loadCards is the session cards a chief's reply can draw
	// (session_card.go), keyed by every key that names one; nil: none.
	loadCards func() map[string]SessionCard

	mu      sync.Mutex
	cards   map[string]SessionCard   // nil until loaded
	names   map[string]mentionTarget // nil until loaded
	tasks   map[int64]bool           // every id asked about, and its answer
	pending []int64                  // primed, not yet asked
	queued  map[int64]bool
}

type mentionTarget struct {
	href, kind string // kind "" = ambiguous
}

// NewMentionIndex is an index over names (called at most once) and tasks:
// given ids, which exist for the viewer (called once per batch of ids to
// resolve). Either may be nil: then nothing of that kind links.
func NewMentionIndex(names func() MentionNames, tasks func(ids []int64) map[int64]bool) *MentionIndex {
	return &MentionIndex{loadNames: names, loadTasks: tasks, tasks: map[int64]bool{}, queued: map[int64]bool{}}
}

// WithSessionCards turns session cards on (session_card.go): load is the
// viewer's own sessions' cards by key, called at most once, on the first
// reference to resolve. It returns mx. Only the chief's chat turns them on.
func (mx *MentionIndex) WithSessionCards(load func() map[string]SessionCard) *MentionIndex {
	if mx != nil {
		mx.loadCards = load
	}
	return mx
}

// cardsOn reports whether mx draws session cards.
func (mx *MentionIndex) cardsOn() bool { return mx != nil && mx.loadCards != nil }

// sessionCard is key's card, loading the cards on first use.
func (mx *MentionIndex) sessionCard(key string) (SessionCard, bool) {
	if !mx.cardsOn() {
		return SessionCard{}, false
	}
	mx.mu.Lock()
	defer mx.mu.Unlock()
	if mx.cards == nil {
		mx.cards = mx.loadCards()
		if mx.cards == nil {
			mx.cards = map[string]SessionCard{}
		}
	}
	c, ok := mx.cards[key]
	return c, ok
}

// mentionTaskRefRe finds the task refs Prime queues: a superset of what
// mdMention resolves (no boundary or PR rule), which only costs an id asked
// about in the same one read.
var mentionTaskRefRe = regexp.MustCompile(`#([1-9][0-9]{0,17})`)

// Prime queues the `#N` refs in texts, so the first ref resolved asks about
// all of them in one call. A renderer primes what it draws; a page primes its
// whole conversation first, so the page costs one read, not one per message.
func (mx *MentionIndex) Prime(texts ...string) {
	if mx == nil || mx.loadTasks == nil {
		return
	}
	mx.mu.Lock()
	defer mx.mu.Unlock()
	for _, t := range texts {
		for _, m := range mentionTaskRefRe.FindAllStringSubmatch(t, -1) {
			if id, err := strconv.ParseInt(m[1], 10, 64); err == nil {
				mx.queueLocked(id)
			}
		}
	}
}

func (mx *MentionIndex) queueLocked(id int64) {
	if mx.queued[id] || len(mx.queued) >= mentionTaskLookupCap {
		return
	}
	mx.queued[id] = true
	mx.pending = append(mx.pending, id)
}

// taskExists answers whether task id exists, asking about every pending id
// with it.
func (mx *MentionIndex) taskExists(id int64) bool {
	if mx.loadTasks == nil {
		return false
	}
	mx.mu.Lock()
	defer mx.mu.Unlock()
	mx.queueLocked(id)
	if len(mx.pending) > 0 {
		got := mx.loadTasks(mx.pending)
		for _, p := range mx.pending {
			mx.tasks[p] = got[p]
		}
		mx.pending = nil
	}
	return mx.tasks[id]
}

// name is key's target, loading the names on first use.
func (mx *MentionIndex) name(key string) mentionTarget {
	mx.mu.Lock()
	defer mx.mu.Unlock()
	if mx.names == nil {
		mx.names = map[string]mentionTarget{}
		if mx.loadNames != nil {
			n := mx.loadNames()
			for _, a := range n.Agents {
				mx.addLocked(a, AgentPath(a), "agent")
			}
			for _, m := range n.Sessions {
				mx.addLocked(mirrorCodeName(m), MirrorCardHref(m), "session")
			}
		}
	}
	return mx.names[key]
}

// mentionKeyRe is what a key must look like to be matchable at all: a whole
// token that does not end in punctuation the matcher trims, and does not start
// with the `@`/`#` sigils or `_`.
var mentionKeyRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]*$`)

func (mx *MentionIndex) addLocked(key, href, kind string) {
	if key == "" || href == "" || !mentionKeyRe.MatchString(key) || strings.TrimRight(key, mentionTrail) != key || mentionAllDigits(key) {
		return
	}
	if prev, ok := mx.names[key]; ok && prev.href != href {
		mx.names[key] = mentionTarget{}
		return
	}
	mx.names[key] = mentionTarget{href: href, kind: kind}
}

func mentionAllDigits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

// mentionDistinctive is whether a bare token may name something without `@`.
func mentionDistinctive(token string) bool {
	if strings.ContainsAny(token, "-_.:/") {
		return true
	}
	return strings.ContainsAny(token, "0123456789") && !mentionAllDigits(token)
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
		// No all-digit name is ever a key (addLocked), so "@12" finds none.
		t := mx.name(token[1:])
		return t.href, t.kind, t.kind != ""
	default:
		if !mentionDistinctive(token) {
			return "", "", false // a plain word: only as @word
		}
		t := mx.name(token)
		return t.href, t.kind, t.kind != ""
	}
}

var (
	// mentionRunRe is a candidate token. \x00 (the vault delimiter) is not in
	// it, so a placeholder's digits are never a token.
	mentionRunRe = regexp.MustCompile(`[A-Za-z0-9_.:/@#-]+`)
	// mentionTailRe is a streamed text's last run, still arriving.
	mentionTailRe = regexp.MustCompile(`[A-Za-z0-9_.:/@#-]+$`)
	// mentionNotGitHub is the word before a `#N` that makes it a GitHub number.
	mentionNotGitHub = map[string]bool{"pr": true, "prs": true, "pull": true, "request": true, "requests": true,
		"issue": true, "issues": true, "merge": true, "merged": true}
)

const (
	// mentionTrail is trimmed off a token's end: it belongs to the sentence.
	mentionTrail = ".:/_"
	// mentionLeft and mentionRight are the bytes a token may sit between
	// (besides white space, the text's ends and mentionWide).
	mentionLeft  = "([{*_~;>,|"
	mentionRight = ")]}*_~&,;!?<|"
)

// mentionWide are the non-ASCII boundaries: a curly apostrophe and an em dash.
var mentionWide = []string{"’", "—"}

func mentionSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }

// mentionStartsAt is whether a token may start at s[i].
func mentionStartsAt(s string, i int) bool {
	if i == 0 || mentionSpace(s[i-1]) || strings.IndexByte(mentionLeft, s[i-1]) >= 0 {
		return true
	}
	for _, w := range mentionWide {
		if strings.HasSuffix(span(s, 0, i), w) {
			return true
		}
	}
	return false
}

// mentionEndsAt is whether a token may end just before s[j].
func mentionEndsAt(s string, j int) bool {
	if j == len(s) || mentionSpace(s[j]) || strings.IndexByte(mentionRight, s[j]) >= 0 {
		return true
	}
	for _, w := range mentionWide {
		if strings.HasPrefix(span(s, j, len(s)), w) {
			return true
		}
	}
	return false
}

// mentionCtx is one render's mentions: the index, and — for a streamed text
// that is still arriving — its last run, which is not linked yet.
type mentionCtx struct {
	mx   *MentionIndex
	tail string
	// sheets is the ⓘ sheets of the session cards drawn so far, written after
	// the message (markdownRender).
	sheets []string
}

// mentionFor is the context for rendering src with mx (nil for none),
// priming src's task refs. partial: src is a streamed reply so far.
func mentionFor(mx *MentionIndex, src string, partial bool) *mentionCtx {
	if mx == nil {
		return nil
	}
	c := &mentionCtx{mx: mx}
	if partial {
		c.tail = mentionTailRe.FindString(src)
		src = strings.TrimSuffix(src, c.tail)
	}
	mx.Prime(src)
	return c
}

// mdMention links s's mentions, stashing each anchor in v. s is escaped inline
// text with code spans, links and URLs already vaulted (mdInlineWith).
//
// ⚠ ITS SLICES ARE SPELLED s[i:j] OR GO THROUGH span: Tailwind scans this
// package's Go for class names (tailwind.config.js), and a slice bounded by
// other names, or an index like "the start, minus one" spelled as code, reads
// to it as an arbitrary property or a utility — which css-check then demands
// in web/static/app.css. (s[i:j] is one it already holds.)
func mdMention(s string, v *mdVault, mc *mentionCtx) string {
	if mc == nil {
		return s
	}
	var b strings.Builder
	done := 0
	for _, loc := range mentionRunRe.FindAllStringIndex(s, -1) {
		i, j := loc[0], loc[1]
		if !mentionStartsAt(s, i) || !mentionEndsAt(s, j) {
			continue
		}
		// Still arriving: the stream's last run, at the very end.
		if mc.tail != "" && j == len(s) && s[i:j] == mc.tail {
			continue
		}
		run := s[i:j]
		name := strings.TrimLeft(run, "_") // italics' opener
		i += len(run) - len(name)
		token := strings.TrimRight(name, mentionTrail)
		if len(token) < 2 {
			continue
		}
		if token[0] == '#' && mentionAfterGitHubWord(s, i) {
			continue
		}
		href, kind, ok := mc.mx.resolve(token)
		if !ok {
			continue
		}
		b.WriteString(span(s, done, i))
		b.WriteString(v.stash(mentionAnchor(href, kind, token)))
		done = i + len(token)
	}
	if done == 0 {
		return s
	}
	b.WriteString(span(s, done, len(s)))
	return b.String()
}

// span is s from byte i to byte j (see mdMention's note on slices).
func span(s string, i, j int) string { return s[i:j] }

// mentionAfterGitHubWord is whether the `#N` at s[at] is a GitHub number: a
// mentionNotGitHub word comes before it — past `:`, `*`, `(` and white space
// — directly or through a list of refs joined by commas, "and" or "or".
func mentionAfterGitHubWord(s string, at int) bool {
	j := at
	for {
		for j > 0 && (mentionSpace(s[j-1]) || strings.IndexByte(",:*(", s[j-1]) >= 0) {
			j--
		}
		i := j
		for i > 0 && mentionWordByte(s[i-1]) {
			i--
		}
		w := strings.ToLower(s[i:j])
		switch {
		case mentionNotGitHub[w]:
			return true
		case i < j && (w == "and" || w == "or" || mentionIsRef(w)):
			j = i
		default:
			return false
		}
	}
}

func mentionWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '#'
}

// mentionIsRef is whether w is a `#N` ref.
func mentionIsRef(w string) bool {
	return len(w) > 1 && w[0] == '#' && mentionAllDigits(strings.TrimPrefix(w, "#"))
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
