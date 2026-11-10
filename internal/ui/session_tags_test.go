package ui

import (
	"regexp"
	"strings"
	"testing"
)

// taggedFixture is switcherFixture with the viewer's tags on agent 1 and on the
// tmux session "w" (both of its windows).
func taggedFixture() ([]AgentCardView, []MirrorCardView) {
	list, mirrors := switcherFixture()
	list[0].Tags = []string{"needs-review"}
	for i := range mirrors {
		mirrors[i].Tags = []string{"billing", "nightly"}
	}
	return list, mirrors
}

// TestTagsAreInTheSwitchersWords: every entry's data-switcher-words — what the
// command palette (paletteJS's entries) and the Sessions dock's filter
// (musterPaletteMatch over the same attributes) match — carries the viewer's
// tags on it, each whole: an agent's on the agent, a window's on its tmux
// session. An untagged entry's words are unchanged.
func TestTagsAreInTheSwitchersWords(t *testing.T) {
	list, mirrors := taggedFixture()
	s := NewSwitcher(list, mirrors, nil, SwitcherCurrent{})
	words := map[string]string{}
	for _, it := range s.All {
		words[it.Code] = it.Words
	}
	for code, want := range map[string]string{
		"ag-one":  "cluster needs-review",
		"mango:1": "claude code w tower billing nightly",
		"mango:2": "w tower billing nightly",
		"ag-two":  "cluster",
	} {
		if got := words[code]; !strings.HasSuffix(got, want) {
			t.Errorf("%s's words = %q, want them to end %q", code, got, want)
		}
	}
	page := renderString(t, switcherBar(s, true))
	if !regexp.MustCompile(`data-switcher-words="[^"]*cluster needs-review"`).MatchString(page) || strings.Count(page, `tower billing nightly"`) < 2 {
		t.Errorf("the bar's entries do not carry the tags in data-switcher-words:\n%s", page)
	}
}

// TestTheMoreMenuHasTagsAndItsEditor: wherever the ⋯ menu is drawn (a page
// whose session is among the entries) it holds "Tags…", which opens the
// editor; the editor holds the session's tags as removable chips and an add
// form, each posting the session's own fields (an agent's id; a window's host
// and SESSION, its window only along). No page session: no menu, no editor.
func TestTheMoreMenuHasTagsAndItsEditor(t *testing.T) {
	list, mirrors := taggedFixture()
	for _, c := range []struct {
		name   string
		cur    SwitcherCurrent
		chips  []string
		fields []string
	}{
		{"an agent", SwitcherCurrent{AgentID: 1}, []string{"needs-review"},
			[]string{`name="kind" value="agent"`, `name="agent_id" value="1"`}},
		{"a window", SwitcherCurrent{Mirror: MirrorRef{Host: "tower", Session: "w", Window: "@2"}}, []string{"billing", "nightly"},
			[]string{`name="kind" value="mirror"`, `name="host" value="tower"`, `name="session" value="w"`, `name="window" value="@2"`}},
	} {
		page := renderString(t, switcherBar(NewSwitcher(list, mirrors, nil, c.cur), true))
		menu := regexp.MustCompile(`(?s)<div id="switcher-more" popover=""[^>]*>(.*?)</div>`).FindStringSubmatch(page)
		if menu == nil || !strings.Contains(menu[1], `<button id="switcher-tags" type="button" role="menuitem" popovertarget="session-tags-editor"`) ||
			!strings.Contains(menu[1], `<span>Tags…</span>`) {
			t.Errorf("%s: the ⋯ menu has no Tags… item:\n%v", c.name, menu)
		}
		ed := page[strings.Index(page, `<div id="session-tags-editor"`):]
		for _, tag := range c.chips {
			if !strings.Contains(ed, `data-session-tag-chip="`+tag+`"`) || !strings.Contains(ed, `aria-label="Remove tag `+tag+`"`) {
				t.Errorf("%s: the editor has no removable chip %q", c.name, tag)
			}
		}
		add := regexp.MustCompile(`(?s)<form method="post" action="/sessions/tags" data-session-tags-add=""[^>]*>(.*?)</form>`).FindStringSubmatch(ed)
		if add == nil || !strings.Contains(add[1], `<input id="session-tags-input" type="text" name="tag" aria-label="Add a tag"`) {
			t.Fatalf("%s: no add form with its input:\n%s", c.name, ed)
		}
		for _, f := range c.fields {
			if !strings.Contains(add[1], f) {
				t.Errorf("%s: the add form lacks %s", c.name, f)
			}
		}
		if !strings.Contains(ed, `maxlength="64"`) {
			t.Errorf("%s: the input does not carry the store's length bound", c.name)
		}
		// The editor normalizes with the task tags' one rule (tag-normalize.js).
		// Every submit is posted by the editor's script (htmx.ajax), never also by
		// htmx's own binding of a form it may not have bound yet.
		if strings.Contains(ed[:strings.Index(ed, "</div>")], "hx-post") || !strings.Contains(page, "window.htmx.ajax('POST', f.getAttribute('action')") {
			t.Errorf("%s: the editor's forms are bound by htmx, or the script does not post them", c.name)
		}
		if !strings.Contains(page, tagNormalizeJS) || !strings.Contains(page, "var t = normalize(i.value);") {
			t.Errorf("%s: the editor's script does not run the task tags' normalizer", c.name)
		}
	}
	// No page session (its session is not an entry): no menu and no editor.
	page := renderString(t, switcherBar(NewSwitcher(list, mirrors, nil, SwitcherCurrent{}), true))
	if strings.Contains(page, "switcher-tags") || strings.Contains(page, "session-tags-editor") {
		t.Error("a bar with no page session draws the Tags… item or the editor")
	}
}

// TestTheHeaderDrawsTheSessionsTags: none is an empty slot (the out-of-band
// target, hidden by :empty); one is its chip; several are the first chip and a
// "+N" button opening a popover with every tag, placed by floatingMenuScript.
// Chips are the descriptive look whatever the namespace (a session tag carries
// no behaviour).
func TestTheHeaderDrawsTheSessionsTags(t *testing.T) {
	none := renderString(t, sessionTagsHead(SessionTagTarget{AgentID: 1}, nil, false))
	if none != `<span id="session-tags-head" data-session-tags-head="0" data-session-tags-for="agent:1" class="inline-flex shrink-0 items-center gap-1 empty:hidden"></span>` {
		t.Errorf("no tags: %s", none)
	}
	one := renderString(t, sessionTagsHead(SessionTagTarget{AgentID: 1}, []string{"gate:blocked"}, false))
	if !strings.Contains(one, `<span data-session-tag="gate:blocked" title="Tag: gate:blocked" class="`+sessionTagChipClass) ||
		strings.Contains(one, "data-session-tags-more") || strings.Contains(one, "st-error") {
		t.Errorf("one tag is not its plain chip alone: %s", one)
	}
	three := renderString(t, sessionTagsHead(SessionTagTarget{AgentID: 1}, []string{"alpha", "beta", "gamma"}, false))
	more := regexp.MustCompile(`<button type="button" data-session-tags-more="2" popovertarget="session-tags-all" aria-haspopup="dialog" aria-controls="session-tags-all" aria-expanded="false" aria-label="All 3 tags"[^>]*>\+2</button>`)
	if !strings.Contains(three, `data-session-tag="alpha"`) || !more.MatchString(three) {
		t.Errorf("three tags are not the first chip and +2: %s", three)
	}
	pop := regexp.MustCompile(`(?s)<div id="session-tags-all" popover="auto" role="dialog" aria-label="Tags" data-floating-menu=""[^>]*>(.*)</div>`).FindStringSubmatch(three)
	if pop == nil || strings.Count(pop[1], `data-session-tag="`) != 3 {
		t.Errorf("the +N popover does not list all three: %v", pop)
	}
	if !strings.Contains(renderString(t, sessionTagsHead(SessionTagTarget{AgentID: 1}, []string{"a"}, true)), `hx-swap-oob="outerHTML"`) {
		t.Error("the out-of-band copy is not marked for the swap")
	}
}

// TestBothViewsDrawTagsInTheHeaderAndNotTheRawSession: the Rendered and
// Terminal views draw the tags after the title in the header, and neither
// draws the raw tmux session name beside a label (data-mirror-session); it is
// the title's tooltip on both, and in the Rendered view's full-title popover.
func TestBothViewsDrawTagsInTheHeaderAndNotTheRawSession(t *testing.T) {
	view := renderString(t, MirrorViewPage(MirrorViewView{Host: "tower", Session: "scratch4", Label: "mango:1", Interactive: true, Tags: []string{"alpha", "beta"}}))
	term := renderString(t, AgentTerminalPage(AgentTerminalView{Name: "scratch4", DisplayName: "mango:1", RawName: "scratch4", Badge: "Mirrored · tower",
		Tags: []string{"alpha", "beta"}}))
	for name, page := range map[string]string{"rendered": view, "terminal": term} {
		h := page[strings.Index(page, "<header"):]
		h = h[:strings.Index(h, "</header>")]
		if !strings.Contains(h, `data-session-tags-head="2"`) || !strings.Contains(h, `>+1</button>`) {
			t.Errorf("%s: the header does not draw the tags:\n%s", name, h)
		}
		if strings.Contains(page, "data-mirror-session") || strings.Contains(h, `>scratch4<`) {
			t.Errorf("%s: the raw session name is still drawn beside the label", name)
		}
		if !strings.Contains(h, `title="tmux session scratch4"`) {
			t.Errorf("%s: the title lost its tmux-session tooltip", name)
		}
	}
	if !strings.Contains(view, `mango:1 · Mirrored · tower · tmux session scratch4</span>`) {
		t.Error("the Rendered view's full-title popover does not name the tmux session")
	}
}

// TestTheRenderedViewsBoxIsCollapsibleBehindTypeAMessage: the Rendered view's
// composer draws "Type a message" (named, expanding the box it controls)
// BESIDE the mic, the box still in the form (dictation and the draft need it
// connected) marked as the collapsible field, and the form marked collapsible
// (its script collapses it);
// the reveal is the first of its scripts, before the draft's. The Terminal
// views' composers draw none of it. A disabled composer is not collapsed.
func TestTheRenderedViewsBoxIsCollapsibleBehindTypeAMessage(t *testing.T) {
	view := renderString(t, MirrorViewPage(MirrorViewView{Host: "tower", Session: "w", Interactive: true, Dictate: TranscribePath, DraftUser: 1}))
	form := view[strings.Index(view, `<form id="mv-form"`):]
	form = form[:strings.Index(form, "</form>")]
	if !strings.Contains(form, `data-mv-collapsible=""`) {
		t.Errorf("the Rendered view's form is not marked collapsible:\n%s", form)
	}
	// 🔴 The markup never collapses it: without script there is no way to
	// reveal it, so only the reveal's script sets data-mv-collapsed.
	if strings.Contains(form, `data-mv-collapsed`) || !strings.Contains(view, "form.setAttribute('data-mv-collapsed', '')") {
		t.Errorf("the box is collapsed by the markup, or the script does not collapse it")
	}
	// The collapsible field is the box's own row, holding the box.
	if !regexp.MustCompile(`<div id="` + composerInputRowID + `" data-mv-field="" class="[^"]*"><textarea id="mv-input" name="text"`).MatchString(form) {
		t.Errorf("the box is not in the form as the collapsible field:\n%s", form)
	}
	btn := regexp.MustCompile(`<button id="mv-text" type="button" aria-label="Type a message" title="Type a message" aria-expanded="false" aria-controls="mv-input"[^>]*>`).FindStringIndex(form)
	mic := strings.Index(form, `<button id="mv-dictate"`)
	if btn == nil || mic < 0 || btn[0] > mic || strings.Contains(form[btn[1]:mic], "<button") {
		t.Errorf("\"Type a message\" is not the button right before the mic:\n%s", form)
	}
	reveal, draft := strings.Index(view, "window.musterComposerReveal = function"), strings.Index(view, "window.musterDraft = {")
	if reveal < 0 || draft < 0 || reveal > draft {
		t.Errorf("the reveal's script (at %d) is not before the draft's (at %d)", reveal, draft)
	}
	term := renderString(t, AgentTerminalPage(AgentTerminalView{Name: "w",
		Composer: &ComposerView{SendPath: MirrorTerminalSendPath, Host: "tower", Session: "w", Dictate: TranscribePath}}))
	for _, bit := range []string{"data-mv-collapsible", "data-mv-field", `id="mv-text"`, "window.musterComposerReveal = function"} {
		if strings.Contains(term, bit) {
			t.Errorf("the Terminal view's composer draws %s", bit)
		}
	}
	off := renderString(t, mirrorComposer(ComposerView{SendPath: MirrorSendPath, Collapsible: true, Disabled: "read-only"}))
	if strings.Contains(off, "data-mv-collapsible") || strings.Contains(off, `id="mv-text"`) {
		t.Error("a disabled composer is collapsed")
	}
}

// TestEveryWriterOfTheBoxReveals: a LEDGER of the scripts that write
// #mv-input's value, each calling the one reveal — so text is never put in a
// hidden box. It fails when a writer stops calling it, and when a new script
// starts writing the box without it (the count of value writes per script is
// pinned).
func TestEveryWriterOfTheBoxReveals(t *testing.T) {
	call := "window.musterComposerReveal("
	for name, c := range map[string]struct {
		src    string
		writes int
	}{
		"the draft (composer_draft.go)": {renderString(t, composerDraftScript()), 1},
		// shown() after a write, and focusBox() for an empty Send (it writes
		// nothing; it reveals so the focus has a box to go to).
		"the pending send (composer_pending.go)": {mirrorComposerJS, 2},
		"the mic (dictate.go)":                   {renderString(t, dictateScript(TranscribePath)), 1},
		"the / menu (composer_menu.go)":          {renderString(t, composerMenuScript("/x")), 1},
		// The insert, and the row's × (which only focuses the box).
		"the suggested replies (suggest_row.go)":  {renderString(t, suggestRowScript()), 2},
		"the send queue's kept edit (send_queue)": {renderString(t, sendQueueScript()), 1},
		"an attached file (attach.go)":            {renderString(t, attachScript(UploadPath)), 1},
	} {
		if n := strings.Count(c.src, call); n != c.writes {
			t.Errorf("%s calls the reveal %d time(s), want %d", name, n, c.writes)
		}
	}
	// The pending send routes its four writes through one helper, shown().
	if n := strings.Count(mirrorComposerJS, "shown();"); n != 4 {
		t.Errorf("the pending send reveals after %d of its writes, want 4 (restore, the hand-off, its return, the fallback)", n)
	}
}

// TestTheMoreButtonIsInTheRenderedViewsInputRow: on the Rendered view the
// session's ⋯ is in the composer's input row, AFTER Send (operator ask:
// "[input][mic][send][⋯]") — the row's last control — and not in the bar; the menu it opens (Pin, Tags…) and the editor are still drawn, once.
// On a Terminal view it stays in the bar.
func TestTheMoreButtonIsInTheRenderedViewsInputRow(t *testing.T) {
	list, mirrors := taggedFixture()
	cur := SwitcherCurrent{Mirror: MirrorRef{Host: "tower", Session: "w", Window: "@1"}}
	view := renderString(t, MirrorViewPage(MirrorViewView{Host: "tower", Session: "w", Window: "@1", Interactive: true,
		Switcher: NewSwitcher(list, mirrors, nil, cur)}))
	form := view[strings.Index(view, `<form id="mv-form"`):]
	form = form[:strings.Index(form, "</form>")]
	more, send := strings.Index(form, `<button id="switcher-more-btn"`), strings.Index(form, `id="mv-send"`)
	if more < 0 || send < 0 || more < send {
		t.Errorf("the ⋯ is not in the input row after Send (⋯ at %d, Send at %d):\n%s", more, send, form)
	}
	if more >= 0 {
		tail := form[more:]
		if after := tail[strings.Index(tail, "</button>"):]; strings.Contains(after, "<button") {
			t.Errorf("a control follows the ⋯ in the input row:\n%s", after)
		}
	}
	nav := view[strings.Index(view, `<nav id="session-switcher"`):]
	nav = nav[:strings.Index(nav, "</nav>")]
	if strings.Contains(nav, `id="switcher-more-btn"`) {
		t.Error("the Rendered view's bar still draws the ⋯")
	}
	for _, id := range []string{`id="switcher-more-btn"`, `<div id="switcher-more"`, `id="switcher-pin"`, `id="switcher-tags"`, `<div id="session-tags-editor"`} {
		if n := strings.Count(view, id); n != 1 {
			t.Errorf("the Rendered view draws %s %d times, want once", id, n)
		}
	}
	term := renderString(t, AgentTerminalPage(AgentTerminalView{Name: "w", Switcher: NewSwitcher(list, mirrors, nil, cur),
		Composer: &ComposerView{SendPath: MirrorTerminalSendPath, Host: "tower", Session: "w"}}))
	tnav := term[strings.Index(term, `<nav id="session-switcher"`):]
	tnav = tnav[:strings.Index(tnav, "</nav>")]
	if !strings.Contains(tnav, `id="switcher-more-btn"`) || strings.Count(term, `id="switcher-more-btn"`) != 1 {
		t.Error("the Terminal view's ⋯ is not in its bar (once)")
	}
}

// TestTheAgentChatHeaderDrawsItsTags: an agent's chat page draws the viewer's
// tags on the agent in its header — the slot the editor's out-of-band answer
// and the other view's copy target — keyed to the agent. Red before: the chat
// page drew no tag slot at all, so an edit there changed nothing on screen.
func TestTheAgentChatHeaderDrawsItsTags(t *testing.T) {
	page := renderString(t, AgentDetailPage(AgentDetailView{Name: "demo", ID: 9, Tags: []string{"alpha", "beta"}}, Features{}))
	// The page is hx-boost'ed: the editor opts out, or its answer would be a
	// boosted navigation that swaps the whole body.
	if ed := renderString(t, sessionTagsEditor(SessionTagTarget{AgentID: 9}, nil, "/agents/demo")); !strings.Contains(ed, `<div id="session-tags-editor" popover="auto" role="dialog" aria-label="Tags for this session" hx-boost="false"`) {
		t.Error("the tag editor is not out of the page's boost")
	}
	h := page[strings.Index(page, "<header"):]
	h = h[:strings.Index(h, "</header>")]
	if !strings.Contains(h, `<span id="session-tags-head" data-session-tags-head="2" data-session-tags-for="agent:9"`) || !strings.Contains(h, `data-session-tag="alpha"`) || !strings.Contains(h, `>+1</button>`) {
		t.Errorf("the chat page's header does not draw the agent's tags:\n%s", h)
	}
	none := renderString(t, AgentDetailPage(AgentDetailView{Name: "demo", ID: 9}, Features{}))
	if !strings.Contains(none, `<span id="session-tags-head" data-session-tags-head="0" data-session-tags-for="agent:9"`) {
		t.Error("with no tags the chat page has no slot for an edit's answer")
	}
}

// TestTheHeaderCopyIsOnlyIntoTheSameSession: the header names the session it
// is for (data-session-tags-for: the agent's id, or host and tmux session), and
// the editor copies its chips into the other live view's header only when that
// header names the same one. Red before: the copy matched any header by id.
func TestTheHeaderCopyIsOnlyIntoTheSameSession(t *testing.T) {
	for _, c := range []struct {
		t    SessionTagTarget
		want string
	}{
		{SessionTagTarget{AgentID: 7}, "agent:7"},
		{SessionTagTarget{Host: "laptop", Session: "w", Window: "@2"}, "mirror:laptop:w"},
		{SessionTagTarget{Host: "laptop", Session: "w"}, "mirror:laptop:w"},
	} {
		if got := c.t.key(); got != c.want {
			t.Errorf("%+v key = %q, want %q", c.t, got, c.want)
		}
	}
	view := renderString(t, MirrorViewPage(MirrorViewView{Host: "laptop", Session: "w", Window: "@2", Interactive: true}))
	if !strings.Contains(view, `data-session-tags-for="mirror:laptop:w"`) {
		t.Error("the Rendered view's header does not name its session")
	}
	term := renderString(t, AgentTerminalPage(AgentTerminalView{Name: "w", TagTarget: SessionTagTarget{Host: "laptop", Session: "w"}}))
	if !strings.Contains(term, `data-session-tags-for="mirror:laptop:w"`) {
		t.Error("the Terminal view's header does not name its session")
	}
	js := renderString(t, sessionTagsScript())
	if !strings.Contains(js, "o.getAttribute('data-session-tags-for') === h.getAttribute('data-session-tags-for')") {
		t.Error("the copy does not compare the two headers' sessions")
	}
}

// TestNoFocusGoesToAHiddenBox: the two places that only FOCUS the box — an
// empty Send ("Type a message first.") and the suggested replies' × — reveal
// it first on a touch screen (focus on a display:none box goes nowhere). Red
// before: each called input.focus() alone.
func TestNoFocusGoesToAHiddenBox(t *testing.T) {
	if !strings.Contains(mirrorComposerJS, "say(form.getAttribute('data-empty')); focusBox(); return;") ||
		!strings.Contains(mirrorComposerJS, "function focusBox() { if (window.musterComposerReveal) window.musterComposerReveal(input, true); else input.focus(); }") {
		t.Error("an empty Send focuses the box without revealing it")
	}
	sg := renderString(t, suggestRowScript())
	if !strings.Contains(sg, "hide();\n    if (window.musterComposerReveal) window.musterComposerReveal(input, true); else input.focus();") {
		t.Error("the suggestions' × focuses the box without revealing it")
	}
}
