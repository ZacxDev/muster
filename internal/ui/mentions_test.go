package ui

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/cctranscript"
)

// mentionFixture is an index of: two agents — one slug distinctive
// (fix-login), one a plain English word (scout) — and four mirrored windows:
// one with a codename (mango:2), two sharing a codename across hosts (pear:1,
// ambiguous), and an unnamed tmux session "12" (all digits, never a key).
// Tasks 7 and 1234 exist; 8 does not.
func mentionFixture() *MentionIndex { return mentionFixtureCounting(new(int), new([][]int64)) }

// mentionFixtureCounting is mentionFixture, counting its name loads and
// recording each task batch it is asked about.
func mentionFixtureCounting(nameLoads *int, taskAsks *[][]int64) *MentionIndex {
	return NewMentionIndex(
		func() MentionNames {
			*nameLoads++
			return MentionNames{
				Agents: []string{"fix-login", "scout", "x<y\"z"},
				Sessions: []MirrorCardView{
					{Host: "hosta", Session: "work", Window: "@3", Label: "mango:2", Claude: true},
					{Host: "hosta", Session: "side", Window: "@4", Label: "pear:1", Claude: true},
					{Host: "hostb", Session: "side", Window: "@9", Label: "pear:1", Claude: true},
					{Host: "hostc", Session: "12", Claude: true},
				},
			}
		},
		func(ids []int64) map[int64]bool {
			*taskAsks = append(*taskAsks, append([]int64(nil), ids...))
			out := map[int64]bool{}
			for _, id := range ids {
				out[id] = id == 7 || id == 1234
			}
			return out
		},
	)
}

const (
	mangoA    = `<a href="/mirror/view?host=hosta&amp;session=work&amp;window=%403" data-mention="session" hx-boost="false" class="text-accent underline decoration-accent/40 underline-offset-2 hover:text-fg">mango:2</a>`
	loginA    = `<a href="/agents/fix-login" data-mention="agent" hx-boost="false" class="text-accent underline decoration-accent/40 underline-offset-2 hover:text-fg">fix-login</a>`
	scoutA    = `<a href="/agents/scout" data-mention="agent" hx-boost="false" class="text-accent underline decoration-accent/40 underline-offset-2 hover:text-fg">@scout</a>`
	task1234A = `<a href="/tasks/1234" data-mention="task" hx-boost="false" class="text-accent underline decoration-accent/40 underline-offset-2 hover:text-fg">#1234</a>`
	task7A    = `<a href="/tasks/7" data-mention="task" hx-boost="false" class="text-accent underline decoration-accent/40 underline-offset-2 hover:text-fg">#7</a>`
)

// TestMentionsLinkOnlyWhatResolves pins the WHOLE rendered paragraph for each
// case, so a link that appears where none should (or vanishes) fails it.
func TestMentionsLinkOnlyWhatResolves(t *testing.T) {
	p := func(inner string) string { return `<p class="my-1">` + inner + `</p>` }
	cases := []struct{ name, src, want string }{
		{"codename", "look at mango:2 now", p("look at " + mangoA + " now")},
		// Round 1: GitHub numbers in every form the chief writes them.
		{"pull request number", "see pull request #7 now", p("see pull request #7 now")},
		{"pull requests number", "pull requests #7", p("pull requests #7")},
		{"PR list joined by and", "PR #1234 and #7", p("PR #1234 and #7")},
		{"PRs list joined by commas", "PRs #1234, #7", p("PRs #1234, #7")},
		{"issues list joined by or", "issues #1234 or #7", p("issues #1234 or #7")},
		{"PR with a colon", "PR: #7", p("PR: #7")},
		{"bold PR", "**PR** #7", p("<strong>PR</strong> #7")},
		{"merged", "merged #7 today", p("merged #7 today")},
		{"merge", "merge #7 next", p("merge #7 next")},
		// The control: a list with no GitHub word links every ref in it.
		{"task list without a PR word", "see #1234 and #7", p("see " + task1234A + " and " + task7A)},
		// A PR word does not reach past a word that is not part of a ref list.
		{"PR word then other words", "PR #1234 fixed by #7", p("PR #1234 fixed by " + task7A)},
		// Round 1: an all-digit key is never a name.
		{"all-digit session name", "I changed 12 files", p("I changed 12 files")},
		{"all-digit session name as @mention", "ask @12 now", p("ask @12 now")},
		// Round 1: wide boundaries and italics.
		{"curly apostrophe", "mango:2’s turn", p(mangoA + "’s turn")},
		{"em dash", "mango:2—done", p(mangoA + "—done")},
		{"italics", "_mango:2_ next", p("<em>" + mangoA + "</em> next")},
		{"agent slug", "fix-login is done", p(loginA + " is done")},
		{"task that exists", "see #7 for it", p("see " + task7A + " for it")},
		{"task that does not exist", "see #8 for it", p("see #8 for it")},
		{"unknown codename", "look at kiwi:4 now", p("look at kiwi:4 now")},
		{"unknown slug", "fix-logout is done", p("fix-logout is done")},
		{"ambiguous codename", "look at pear:1 now", p("look at pear:1 now")},
		{"case differs", "look at Mango:2 now", p("look at Mango:2 now")},
		{"plain-word agent name stays a word", "please scout the repo", p("please scout the repo")},
		{"plain-word agent name as @mention", "ask @scout first", p("ask " + scoutA + " first")},
		{"code span", "run `mango:2` and `#7`", p(`run <code class="rounded bg-bg/60 px-1 py-0.5 font-mono text-[0.85em]">mango:2</code> and <code class="rounded bg-bg/60 px-1 py-0.5 font-mono text-[0.85em]">#7</code>`)},
		// The name sits on a boundary (a space) INSIDE the span, so only the
		// span's exclusion keeps it text, not the boundary rule.
		{"code span holding a sentence", "run `ssh mango:2 #7 now` ok", p(`run <code class="rounded bg-bg/60 px-1 py-0.5 font-mono text-[0.85em]">ssh mango:2 #7 now</code> ok`)},
		{"markdown link label and url", "[mango:2](https://x.example/?n=mango:2)", p(`<a href="https://x.example/?n=mango:2" target="_blank" rel="noopener" class="text-accent underline decoration-accent/40 underline-offset-2 hover:text-fg">mango:2</a>`)},
		{"bare url holding a codename", "at https://x.example/s?n=mango:2 ok", p(`at <a href="https://x.example/s?n=mango:2" target="_blank" rel="noopener" class="text-accent underline decoration-accent/40 underline-offset-2 hover:text-fg">https://x.example/s?n=mango:2</a> ok`)},
		{"inside a path or query", "a/mango:2 and k=mango:2", p("a/mango:2 and k=mango:2")},
		{"followed by a non-boundary", "mango:2=on and mango:2`x`", p(`mango:2=on and mango:2<code class="rounded bg-bg/60 px-1 py-0.5 font-mono text-[0.85em]">x</code>`)},
		{"trailing full stop", "see mango:2.", p("see " + mangoA + ".")},
		{"trailing colon and comma", "mango:2: done, fix-login, #7!", p(mangoA + ": done, " + loginA + ", " + task7A + "!")},
		{"brackets", "(mango:2) [fix-login]", p("(" + mangoA + ") [" + loginA + "]")},
		{"bold", "**mango:2**", p("<strong>" + mangoA + "</strong>")},
		{"quoted", `"mango:2"`, p("&quot;" + mangoA + "&quot;")},
		{"possessive", "mango:2's turn", p(mangoA + "&#39;s turn")},
		{"github PR number", "opened PR #7 and pr #7", p("opened PR #7 and pr #7")},
		{"repo-qualified number", "ZacxDev/muster#7", p("ZacxDev/muster#7")},
		{"leading zero", "#07", p("#07")},
		{"entity is no task ref", "it's", p("it&#39;s")},
		{"second line of a paragraph", "one\nmango:2", p("one<br>" + mangoA)},
		{"name with markup is never a key", `x<y"z`, p(`x&lt;y&quot;z`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := markdownHTMLWith(c.src, mentionFixture()); got != c.want {
				t.Errorf("markdownHTMLWith(%q)\n got: %s\nwant: %s", c.src, got, c.want)
			}
		})
	}
}

// TestMentionsSkipFencedCode: a fence never goes through inline rendering.
func TestMentionsSkipFencedCode(t *testing.T) {
	got := markdownHTMLWith("```\nmango:2 #7 fix-login\n```", mentionFixture())
	if strings.Contains(got, "data-mention") {
		t.Errorf("a fenced block was linked: %s", got)
	}
	if !strings.Contains(got, "mango:2 #7 fix-login") {
		t.Errorf("the fence's text changed: %s", got)
	}
}

// TestMentionsReachEveryInlineBlock: headings, lists, quotes and table cells
// all go through the same inline pass.
func TestMentionsReachEveryInlineBlock(t *testing.T) {
	for _, src := range []string{"# mango:2", "- mango:2", "1. mango:2", "> mango:2", "| a |\n| - |\n| mango:2 |"} {
		if got := markdownHTMLWith(src, mentionFixture()); !strings.Contains(got, mangoA) {
			t.Errorf("%q: no mention link in %s", src, got)
		}
	}
}

// TestNoIndexIsTheOldRenderer: renderMarkdown, and a nil index, link nothing.
func TestNoIndexIsTheOldRenderer(t *testing.T) {
	src := "mango:2 fix-login #7 @scout"
	if got, want := markdownHTMLWith(src, nil), markdownHTML(src); got != want {
		t.Errorf("nil index: %s, want %s", got, want)
	}
	if strings.Contains(markdownHTML(src), "data-mention") {
		t.Error("the index-free renderer linked a mention")
	}
}

// TestTaskLookupsAreBatchedAndCapped: every ref a render names is asked about
// in ONE call, each id once, and none past the cap.
func TestTaskLookupsAreBatchedAndCapped(t *testing.T) {
	var asks [][]int64
	mx := NewMentionIndex(nil, func(ids []int64) map[int64]bool {
		asks = append(asks, append([]int64(nil), ids...))
		out := map[int64]bool{}
		for _, id := range ids {
			out[id] = true
		}
		return out
	})
	var b strings.Builder
	for i := 0; i < 3; i++ {
		b.WriteString("#5 ")
	}
	for i := 1; i <= mentionTaskLookupCap+10; i++ {
		b.WriteString("#1000" + strconv.Itoa(i) + " ")
	}
	got := markdownHTMLWith(b.String(), mx)
	if len(asks) != 1 || len(asks[0]) != mentionTaskLookupCap {
		t.Fatalf("task asks = %d (first of %d ids), want 1 of %d", len(asks), len(asks[0]), mentionTaskLookupCap)
	}
	if n := strings.Count(got, `data-mention="task"`); n != 3+mentionTaskLookupCap-1 {
		t.Errorf("task links = %d, want %d", n, 3+mentionTaskLookupCap-1)
	}
}

// TestTheIndexIsLazy: an index reads nothing for text that names nothing; it
// asks about tasks only for a #N, loads the names only for a token that could
// be one, and each at most once.
func TestTheIndexIsLazy(t *testing.T) {
	loads, asks := 0, [][]int64{}
	mx := mentionFixtureCounting(&loads, &asks)
	// "12" is all digits: not a name to look up (an unnamed tmux session is).
	_ = markdownHTMLWith("just plain words here, 12 files, nothing else", mx)
	if loads != 0 || len(asks) != 0 {
		t.Fatalf("plain words: %d name loads, %d task asks; want none", loads, len(asks))
	}
	_ = markdownHTMLWith("see #7", mx)
	if loads != 0 || len(asks) != 1 {
		t.Fatalf("a task ref: %d name loads, %d task asks; want 0 and 1", loads, len(asks))
	}
	_ = markdownHTMLWith("see mango:2 and fix-login, then #7 again", mx)
	_ = markdownHTMLWith("and mango:2 once more", mx)
	if loads != 1 || len(asks) != 1 {
		t.Fatalf("names twice, #7 again: %d name loads, %d task asks; want 1 and 1", loads, len(asks))
	}
}

// TestAPrimedPageAsksAboutItsTasksOnce: with the whole log primed (what
// internal/api withChatLogLive does), two replies naming two tasks cost ONE
// ask, not one per reply.
func TestAPrimedPageAsksAboutItsTasksOnce(t *testing.T) {
	msgs := []ChatLine{{Role: "assistant", Content: "first #7", ID: 1}, {Role: "assistant", Content: "then #1234", ID: 2}}
	for _, primed := range []bool{false, true} {
		loads, asks := 0, [][]int64{}
		mx := mentionFixtureCounting(&loads, &asks)
		if primed {
			mx.Prime(msgs[0].Content, msgs[1].Content)
		}
		body, _, err := ChatLogBody(AgentDetailView{Name: "chief", Status: "running", Mentions: mx, Messages: msgs})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), task7A) || !strings.Contains(string(body), task1234A) {
			t.Fatalf("primed=%v: the refs are not linked: %s", primed, body)
		}
		want := 2
		if primed {
			want = 1
		}
		if len(asks) != want {
			t.Errorf("primed=%v: %d task asks %v, want %d", primed, len(asks), asks, want)
		}
	}
}

// TestAStreamedReplyDoesNotLinkItsLastToken: while "#1234" streams in as #1,
// #12, #123, none of those is looked up or linked; a token that is finished
// (text follows it) is.
func TestAStreamedReplyDoesNotLinkItsLastToken(t *testing.T) {
	loads, asks := 0, [][]int64{}
	mx := mentionFixtureCounting(&loads, &asks)
	for _, partial := range []string{"see #1", "see #12", "see #123", "ask fix-lo", "ask fix-login"} {
		if got := ChatStreamHTML("assistant", partial, mx); strings.Contains(got, "data-mention") {
			t.Errorf("%q: the arriving token was linked: %s", partial, got)
		}
	}
	if len(asks) != 0 || loads != 0 {
		t.Errorf("arriving tokens were looked up: %d name loads, task asks %v", loads, asks)
	}
	got := ChatStreamHTML("assistant", "see #1234 and fix-login then", mx)
	if !strings.Contains(got, task1234A) || !strings.Contains(got, loginA) {
		t.Errorf("finished tokens were not linked: %s", got)
	}
	// …and the partial refs were never even queued: the one ask is for 1234.
	if fmt.Sprint(asks) != "[[1234]]" {
		t.Errorf("task asks = %v, want [[1234]] (no #1, #12 or #123)", asks)
	}
	// The same text, stored (not streaming), links its last token.
	if got := ChatMessageHTML("assistant", "see #12"); strings.Contains(got, "data-mention") {
		t.Errorf("ChatMessageHTML has no index and must link nothing: %s", got)
	}
}

// TestTheChatLogLinksMentions: #chat-log — the page's, the chief panel's, the
// refetch's and the view strip's (ChatLogBody) — draws its replies with the
// view's index.
func TestTheChatLogLinksMentions(t *testing.T) {
	body, _, err := ChatLogBody(AgentDetailView{Name: "chief", Status: "running", Mentions: mentionFixture(),
		Messages: []ChatLine{{Role: "user", Content: "where is mango:2?", ID: 1}, {Role: "assistant", Content: "mango:2 has it", ID: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(body), "data-mention"); n != 1 {
		t.Errorf("mention links = %d, want 1 (the reply's, not the owner's): %s", n, body)
	}
	if !strings.Contains(string(body), mangoA+" has it") {
		t.Errorf("the reply was not linked: %s", body)
	}
}

// TestTheMirroredViewLinksMentions: a mirrored session's assistant text is
// drawn through the same renderer, with the transcript's index; a prompt is
// drawn as typed.
func TestTheMirroredViewLinksMentions(t *testing.T) {
	tr := MirrorTranscript{Mentions: mentionFixture()}
	ops := tr.Ops([]cctranscript.Event{
		{Kind: cctranscript.KindPrompt, Text: "is mango:2 done?"},
		{Kind: cctranscript.KindText, Text: "mango:2 is done"},
	})
	if len(ops) != 2 {
		t.Fatalf("ops = %d, want 2", len(ops))
	}
	if strings.Contains(ops[0].HTML, "data-mention") {
		t.Errorf("the prompt was linked: %s", ops[0].HTML)
	}
	if !strings.Contains(ops[1].HTML, mangoA+" is done") {
		t.Errorf("the reply was not linked: %s", ops[1].HTML)
	}
}

// TestChatStreamHTMLLinksTheAssistantOnly: the stream's renderer links an
// assistant's reply and leaves the owner's own message as typed; with no
// index it is the plain renderer.
func TestChatStreamHTMLLinksTheAssistantOnly(t *testing.T) {
	mx := mentionFixture()
	if got := ChatStreamHTML("assistant", "on mango:2 now", mx); !strings.Contains(got, mangoA) {
		t.Errorf("assistant: %s", got)
	}
	if got := ChatStreamHTML("user", "on mango:2 now", mx); strings.Contains(got, "data-mention") {
		t.Errorf("user: %s", got)
	}
	if got, want := ChatStreamHTML("assistant", "on mango:2 now", nil), ChatMessageHTML("assistant", "on mango:2 now"); got != want {
		t.Errorf("nil index changed the chat renderer: %s vs %s", got, want)
	}
}
