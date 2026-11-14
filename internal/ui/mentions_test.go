package ui

import (
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/cctranscript"
)

// mentionFixture is an index of: two agents — one slug distinctive
// (fix-login), one a plain English word (scout) — and two mirrored windows,
// one with a codename (mango:2) and one sharing its codename with a window on
// another host (pear:1, ambiguous). Tasks 7 and 1234 exist; 8 does not.
func mentionFixture() *MentionIndex {
	return NewMentionIndex(
		[]string{"fix-login", "scout", "x<y\"z"},
		[]MirrorCardView{
			{Host: "hosta", Session: "work", Window: "@3", Label: "mango:2", Claude: true},
			{Host: "hosta", Session: "side", Window: "@4", Label: "pear:1", Claude: true},
			{Host: "hostb", Session: "side", Window: "@9", Label: "pear:1", Claude: true},
		},
		func(id int64) bool { return id == 7 || id == 1234 },
	)
}

const (
	mangoA = `<a href="/mirror/view?host=hosta&amp;session=work&amp;window=%403" data-mention="session" hx-boost="false" class="text-accent underline decoration-accent/40 underline-offset-2 hover:text-fg">mango:2</a>`
	loginA = `<a href="/agents/fix-login" data-mention="agent" hx-boost="false" class="text-accent underline decoration-accent/40 underline-offset-2 hover:text-fg">fix-login</a>`
	scoutA = `<a href="/agents/scout" data-mention="agent" hx-boost="false" class="text-accent underline decoration-accent/40 underline-offset-2 hover:text-fg">@scout</a>`
	task7A = `<a href="/tasks/7" data-mention="task" hx-boost="false" class="text-accent underline decoration-accent/40 underline-offset-2 hover:text-fg">#7</a>`
)

// TestMentionsLinkOnlyWhatResolves pins the WHOLE rendered paragraph for each
// case, so a link that appears where none should (or vanishes) fails it.
func TestMentionsLinkOnlyWhatResolves(t *testing.T) {
	p := func(inner string) string { return `<p class="my-1">` + inner + `</p>` }
	cases := []struct{ name, src, want string }{
		{"codename", "look at mango:2 now", p("look at " + mangoA + " now")},
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

// TestTaskLookupsAreRememberedAndCapped: one read per distinct id, and none
// past the cap.
func TestTaskLookupsAreRememberedAndCapped(t *testing.T) {
	calls := 0
	mx := NewMentionIndex(nil, nil, func(int64) bool { calls++; return true })
	var b strings.Builder
	for i := 0; i < 3; i++ {
		b.WriteString("#5 ")
	}
	for i := 1; i <= mentionTaskLookupCap+10; i++ {
		b.WriteString("#1000" + strconv.Itoa(i) + " ")
	}
	got := markdownHTMLWith(b.String(), mx)
	if calls != mentionTaskLookupCap {
		t.Errorf("task reads = %d, want %d (cap)", calls, mentionTaskLookupCap)
	}
	if n := strings.Count(got, `data-mention="task"`); n != 3+mentionTaskLookupCap-1 {
		t.Errorf("task links = %d, want %d", n, 3+mentionTaskLookupCap-1)
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

// TestChatMessageHTMLWithLinksTheAssistantOnly: the chat's renderer links an
// assistant's reply and leaves the owner's own message as typed.
func TestChatMessageHTMLWithLinksTheAssistantOnly(t *testing.T) {
	mx := mentionFixture()
	if got := ChatMessageHTMLWith("assistant", "on mango:2", mx); !strings.Contains(got, mangoA) {
		t.Errorf("assistant: %s", got)
	}
	if got := ChatMessageHTMLWith("user", "on mango:2", mx); strings.Contains(got, "data-mention") {
		t.Errorf("user: %s", got)
	}
	if got, want := ChatMessageHTMLWith("assistant", "on mango:2", nil), ChatMessageHTML("assistant", "on mango:2"); got != want {
		t.Errorf("nil index changed the chat renderer: %s vs %s", got, want)
	}
}
