package ui

import (
	"io"
	"net/url"
	"strconv"
	"strings"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// SESSION TAGS (internal/sessiontags): labels the viewer puts on a session — a
// dispatched agent, or a mirrored tmux SESSION (every window of it shares
// them). Per user and private, like pins.
//
//   - EDITED from the session switcher's ⋯ menu, "Tags…" (sessionTagsMenuItem),
//     wherever the menu is drawn: a small popover (sessionTagsEditor) of chips
//     with × and an input that adds one. The input runs the task tags'
//     normalizer (js/tag-normalize.js) as it is submitted, so the chip that
//     appears is spelled as it was saved; the server normalizes again and
//     refuses what the grammar does not allow, naming it.
//   - SHOWN in the Rendered and Terminal views' headers (sessionTagsHead): one
//     tag as its chip; more as the first chip and "+N", which opens a popover
//     listing them all (the full-title popover's look, placed by
//     floatingMenuScript).
//   - FOUND BY the command palette and the Sessions dock's filter: they are in
//     each entry's data-switcher-words (switcherItems, sessionTagWords).
//
// 🔴 ONE ANSWER UPDATES ALL THREE. An add or remove posted by htmx is answered
// with the editor's body, the header's chips out of band, and the bar's rows
// out of band (switcherRefresh) — the rows carry the words the palette and
// the dock read — so nothing waits for a reload (RenderSessionTagsAnswer).
//
// 🔴 A SESSION TAG IS A PLAIN LABEL. The task tags' reserved namespaces
// (gate:, initiative:, auto:, project:) carry no behaviour on a session, so
// every chip here is drawn in the descriptive look (sessionTagChip), never a
// routing colour that would promise an effect.

// The tag routes (internal/api session_tags.go): GET lists one subject's tags
// (JSON), POST adds one, POST …/delete removes one.
const (
	SessionTagsPath       = "/sessions/tags"
	SessionTagsRemovePath = "/sessions/tags/delete"
)

// The editor's and the header's element ids.
const (
	SessionTagsEditorID = "session-tags-editor"
	sessionTagsBodyID   = "session-tags-body"
	SessionTagsInputID  = "session-tags-input"
	SessionTagsHeadID   = "session-tags-head"
	sessionTagsAllID    = "session-tags-all"
	// SessionTagsItemID is the ⋯ menu's "Tags…" item.
	SessionTagsItemID = "switcher-tags"
	// SessionTagsMenuLabel is that item's words.
	SessionTagsMenuLabel = "Tags…"
)

// SessionTagTarget is the session a tag is for, as the editor posts it: an
// agent (AgentID), or a mirrored tmux session (Host, Session — Window is only
// the page's own window, for the switcher's rows in the answer; tags are not
// keyed on it).
type SessionTagTarget struct {
	AgentID int64
	Host    string
	Session string
	Window  string
}

// fields is the target as the editor's hidden fields.
func (t SessionTagTarget) fields() [][2]string {
	if t.AgentID > 0 {
		return [][2]string{{"kind", "agent"}, {"agent_id", strconv.FormatInt(t.AgentID, 10)}}
	}
	return [][2]string{{"kind", "mirror"}, {"host", t.Host}, {"session", t.Session}, {"window", t.Window}}
}

// Query is the target as a query string (the list route's).
func (t SessionTagTarget) Query() string {
	q := url.Values{}
	for _, f := range t.fields() {
		if f[1] != "" {
			q.Set(f[0], f[1])
		}
	}
	return q.Encode()
}

// sessionTagWords is tags as palette words: space-separated, each tag whole.
func sessionTagWords(tags []string) string { return strings.Join(tags, " ") }

// sessionTagChipClass is the chip's look: the task tags' descriptive chip
// (tagChipClass's default branch), whatever the tag's namespace.
const sessionTagChipClass = "inline-flex shrink-0 items-center gap-1 rounded-md px-1.5 py-0.5 text-xs bg-s2 text-muted ring-1 ring-inset ring-line"

// sessionTagChip is one read-only chip.
func sessionTagChip(tag string, extra string) g.Node {
	return Span(
		g.Attr("data-session-tag", tag),
		g.Attr("title", "Tag: "+tag),
		Class(sessionTagChipClass+extra),
		g.Text(tag),
	)
}

// sessionTagsHead is the header's tags: nothing visible for none (the empty
// span stays, as the out-of-band swap's target); one tag as its chip; more as
// the first chip and a "+N" button opening a popover with every tag.
//
// The chip is capped (max-w-[7rem], truncated) so the header's title keeps
// its room on a phone (e2e/tests/title-room.spec.ts).
func sessionTagsHead(tags []string, oob bool) g.Node {
	nodes := []g.Node{
		ID(SessionTagsHeadID),
		g.Attr("data-session-tags-head", strconv.Itoa(len(tags))),
		g.If(oob, hx("hx-swap-oob", "outerHTML")),
		Class("inline-flex shrink-0 items-center gap-1 empty:hidden"),
	}
	if len(tags) == 0 {
		return Span(nodes...)
	}
	nodes = append(nodes, sessionTagChip(tags[0], " max-w-[7rem] truncate block"))
	if len(tags) > 1 {
		nodes = append(nodes,
			Button(
				Type("button"),
				g.Attr("data-session-tags-more", strconv.Itoa(len(tags)-1)),
				g.Attr("popovertarget", sessionTagsAllID),
				g.Attr("aria-haspopup", "dialog"),
				g.Attr("aria-controls", sessionTagsAllID),
				g.Attr("aria-expanded", "false"),
				g.Attr("aria-label", "All "+strconv.Itoa(len(tags))+" tags"),
				g.Attr("title", strings.Join(tags, ", ")),
				Class("press inline-flex min-h-[28px] shrink-0 items-center rounded-md bg-s2 px-1.5 text-xs font-medium text-fg2 ring-1 ring-inset ring-line transition hover:bg-s3 aria-expanded:bg-s3"),
				g.Text("+"+strconv.Itoa(len(tags)-1)),
			),
			sessionTagsAllPopover(tags),
		)
	}
	return Span(nodes...)
}

// sessionTagsAllPopover is the "+N" popover: every tag as a chip, in the
// full-title popover's clothes (fullTitlePopoverClass), placed by
// floatingMenuScript.
func sessionTagsAllPopover(tags []string) g.Node {
	chips := make([]g.Node, 0, len(tags))
	for _, t := range tags {
		chips = append(chips, Li(sessionTagChip(t, "")))
	}
	return Div(
		ID(sessionTagsAllID),
		g.Attr("popover", "auto"),
		g.Attr("role", "dialog"),
		g.Attr("aria-label", "Tags"),
		g.Attr("data-floating-menu", ""),
		g.Attr("data-session-tags-all", ""),
		Class(fullTitlePopoverClass),
		Span(Class("text-xs font-semibold text-muted"), g.Text("Tags")),
		Ul(Class("flex flex-wrap gap-1.5"), g.Group(chips)),
	)
}

// sessionTagsHeadNode is what a header draws. Its "+N" popover is placed by
// floatingMenuScript, which both headers' pages load at the end of their body
// (a <script src> here would block the parser above the page's content).
func sessionTagsHeadNode(tags []string) g.Node { return sessionTagsHead(tags, false) }

// sessionTagsMenuItem is the ⋯ menu's "Tags…": it opens the editor. Without
// script it is the browser's own popover toggle (popovertarget); with it, the
// menu is closed first (sessionTagsScript), so the editor is not drawn over an
// open menu.
func sessionTagsMenuItem() g.Node {
	return Button(
		ID(SessionTagsItemID),
		Type("button"),
		g.Attr("role", "menuitem"),
		g.Attr("popovertarget", SessionTagsEditorID),
		g.Attr("aria-haspopup", "dialog"),
		g.Attr("aria-controls", SessionTagsEditorID),
		Class("press flex min-h-[44px] w-full items-center gap-2 rounded-lg px-3 text-left text-sm font-semibold text-fg transition hover:bg-s2 focus:bg-s2 focus:outline-none"),
		g.Raw(`<svg class="h-4 w-4 shrink-0" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true"><path fill-rule="evenodd" d="M4.5 2A2.5 2.5 0 0 0 2 4.5v3.879a2.5 2.5 0 0 0 .732 1.767l7.5 7.5a2.5 2.5 0 0 0 3.536 0l3.878-3.878a2.5 2.5 0 0 0 0-3.536l-7.5-7.5A2.5 2.5 0 0 0 8.38 2H4.5ZM5 6a1 1 0 1 0 0-2 1 1 0 0 0 0 2Z" clip-rule="evenodd"/></svg>`),
		Span(g.Text(SessionTagsMenuLabel)),
	)
}

// sessionTagsEditor is the editor popover (top layer, out of flow — opening it
// moves nothing): its body (sessionTagsBody), which every answer replaces.
// Placed near the top of the screen, like the palette, so a phone's keyboard
// does not cover it.
func sessionTagsEditor(t SessionTagTarget, tags []string, back string) g.Node {
	return Div(
		ID(SessionTagsEditorID),
		g.Attr("popover", "auto"),
		g.Attr("role", "dialog"),
		g.Attr("aria-label", "Tags for this session"),
		Class("inset-x-0 bottom-auto top-[max(0.5rem,10vh)] mx-auto my-0 w-[min(24rem,calc(100vw-1rem))] rounded-xl border-0 bg-s1 p-3 text-fg shadow-2xl ring-1 ring-line"),
		sessionTagsBody(t, tags, back),
	)
}

// sessionTagsBody is the editor's content: a heading, the chips (each with a
// remove form), and the add form. The forms are real forms (no script: the
// server sends the browser back to the session's page) posted by htmx.
func sessionTagsBody(t SessionTagTarget, tags []string, back string) g.Node {
	hidden := func() g.Node {
		nodes := []g.Node{Input(Type("hidden"), Name("back"), Value(back))}
		for _, f := range t.fields() {
			nodes = append(nodes, Input(Type("hidden"), Name(f[0]), Value(f[1])))
		}
		return g.Group(nodes)
	}
	chips := make([]g.Node, 0, len(tags))
	for _, tag := range tags {
		chips = append(chips, Li(
			Form(
				Method("post"),
				Action(SessionTagsRemovePath),
				hx("hx-post", SessionTagsRemovePath),
				hx("hx-target", "#"+sessionTagsBodyID),
				hx("hx-swap", "outerHTML"),
				g.Attr("data-session-tag-chip", tag),
				Class(sessionTagChipClass+" gap-1.5 py-1 pr-0.5"),
				hidden(),
				Input(Type("hidden"), Name("tag"), Value(tag)),
				Span(g.Text(tag)),
				Button(
					Type("submit"),
					g.Attr("data-session-tag-remove", tag),
					g.Attr("aria-label", "Remove tag "+tag),
					g.Attr("title", "Remove tag "+tag),
					Class("press inline-flex h-7 w-7 items-center justify-center rounded-md text-current opacity-70 transition hover:bg-s3 hover:opacity-100"),
					g.Text("×"),
				),
			),
		))
	}
	full := len(tags) >= SessionTagsMax
	return Div(
		ID(sessionTagsBodyID),
		Class("flex flex-col gap-2"),
		H2(Class("text-sm font-semibold"), g.Text("Tags")),
		g.If(len(tags) == 0, P(g.Attr("data-session-tags-none", ""), Class("text-xs text-muted"), g.Text("No tags yet. Only you see them."))),
		g.If(len(tags) > 0, Ul(g.Attr("data-session-tags-list", ""), Class("flex flex-wrap gap-1.5"), g.Group(chips))),
		Form(
			Method("post"),
			Action(SessionTagsPath),
			hx("hx-post", SessionTagsPath),
			hx("hx-target", "#"+sessionTagsBodyID),
			hx("hx-swap", "outerHTML"),
			g.Attr("data-session-tags-add", ""),
			Class("flex items-center gap-2"),
			hidden(),
			Input(
				ID(SessionTagsInputID),
				Type("text"),
				Name("tag"),
				g.Attr("aria-label", "Add a tag"),
				g.Attr("autocomplete", "off"),
				g.Attr("autocapitalize", "off"),
				g.Attr("spellcheck", "false"),
				g.Attr("enterkeyhint", "done"),
				g.Attr("maxlength", strconv.Itoa(SessionTagMaxRunes)),
				g.If(full, g.Attr("disabled", "")),
				Placeholder(addTagPlaceholder(full)),
				// text-base: 16px, so a phone does not zoom the page on focus.
				Class("min-h-[44px] min-w-0 flex-1 rounded-lg border-0 bg-bg px-3 text-base text-fg ring-1 ring-inset ring-line placeholder:text-muted focus:outline-none focus:ring-2 focus:ring-focus disabled:opacity-50"),
			),
			Button(
				Type("submit"),
				g.Attr("data-session-tags-submit", ""),
				g.If(full, g.Attr("disabled", "")),
				Class(composerSendClass),
				g.Text("Add"),
			),
		),
		P(g.Attr("data-session-tags-error", ""), g.Attr("role", "status"), g.Attr("aria-live", "polite"), g.Attr("hidden", ""),
			Class("text-xs text-st-error-fg break-words")),
	)
}

func addTagPlaceholder(full bool) string {
	if full {
		return "At most " + strconv.Itoa(SessionTagsMax) + " tags"
	}
	return "Add a tag…"
}

// SessionTagsMax and SessionTagMaxRunes are the store's bounds
// (sessiontags.MaxPerTarget / MaxTagRunes — this package does not import the
// store); internal/api's TestTheEditorSpellsTheStoresBounds pins them equal.
const (
	SessionTagsMax     = 20
	SessionTagMaxRunes = 64
)

// RenderSessionTagsAnswer writes an add's or a remove's htmx answer: the
// editor's body, the header's chips out of band, and — when s is not nil — the
// bar's rows and counts out of band (switcherRefresh), whose words the
// palette and the dock's filter read.
func RenderSessionTagsAnswer(w io.Writer, t SessionTagTarget, tags []string, back string, s *Switcher) error {
	for _, n := range []g.Node{sessionTagsBody(t, tags, back), sessionTagsHead(tags, true)} {
		if err := n.Render(w); err != nil {
			return err
		}
	}
	if s == nil {
		return nil
	}
	return switcherRefresh(s).Render(w)
}

// sessionTagsScript wires the editor: the ⋯ menu's "Tags…" closes the menu and
// opens the editor with its input focused; an add normalizes the typed tag
// with the task tags' rule (tagNormalizeJS) before it is posted, and refuses
// an empty one; a refusal's reason (the server's words) is shown in the
// editor; after an answer the input is focused again. Bound once per window,
// at the document (a swap replaces the editor's body).
func sessionTagsScript() g.Node {
	return Script(g.Raw(`
(function () {
  if (window.__musterSessionTags) return;
  window.__musterSessionTags = true;
` + tagNormalizeJS + `
  var ED = '` + SessionTagsEditorID + `', INPUT = '` + SessionTagsInputID + `';
  function byId(id) { return document.getElementById(id); }
  function isOpen(p) { try { return !!p && p.matches(':popover-open'); } catch (e) { return false; } }
  function say(msg) {
    var ed = byId(ED), p = ed && ed.querySelector('[data-session-tags-error]');
    if (!p) return;
    p.textContent = msg || '';
    p.hidden = !msg;
  }
  function focusInput() { var i = byId(INPUT); if (i && !i.disabled) { try { i.focus({ preventScroll: true }); } catch (e) { i.focus(); } } }
  document.addEventListener('click', function (e) {
    var item = e.target && e.target.closest ? e.target.closest('#` + SessionTagsItemID + `') : null;
    if (!item) return;
    var ed = byId(ED);
    if (!ed || typeof ed.showPopover !== 'function') return;
    e.preventDefault();
    var menu = item.closest('[popover]');
    try { if (menu && isOpen(menu)) menu.hidePopover(); } catch (err) {}
    try { if (!isOpen(ed)) ed.showPopover(); } catch (err) {}
    say('');
    focusInput();
  });
  document.addEventListener('submit', function (e) {
    var f = e.target;
    if (!f || !f.matches || !f.matches('[data-session-tags-add]')) return;
    var i = f.querySelector('input[name="tag"]');
    if (!i) return;
    var t = normalize(i.value);
    if (!t) { e.preventDefault(); e.stopImmediatePropagation(); say('Type a tag first.'); return; }
    i.value = t;
  }, true);
  document.addEventListener('htmx:responseError', function (e) {
    var ed = byId(ED), elt = e.detail && e.detail.elt;
    if (!ed || !elt || !ed.contains(elt)) return;
    var x = e.detail.xhr;
    say((x && x.responseText ? String(x.responseText).trim() : '') || 'Could not save the tag.');
  });
  document.addEventListener('htmx:afterSwap', function (e) {
    var t = e.detail && e.detail.target;
    if (t && t.id === '` + sessionTagsBodyID + `' && isOpen(byId(ED))) focusInput();
  });
})();
`))
}
