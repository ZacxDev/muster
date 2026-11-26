package ui

import (
	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE MESSAGE INDEX: every message the reader sent in a mirrored
// conversation, in a sheet opened from the Rendered view's header. A tap on one
// whose bubble is on the page scrolls to it, opens it if it is clamped
// (bubbleClampScript) and highlights it briefly.
//
// 🔴 "EVERY" MEANS THE WHOLE TRANSCRIPT, NOT THE PAGE. The view loads only the
// file's last window (mirror.BackfillWindow / MaxBackfillEvents), so the list
// is the HOST's prompt index (mirror.Msg.PromptIndex: every prompt in the file,
// window.muMirrorIndex here) joined with the page's own [data-bubble="user"]
// elements for whatever streamed in after it. A prompt whose bubble is not on
// the page is listed with its text in full and an "earlier — not loaded" mark,
// never left out; when the host's bound dropped the oldest, a line says how
// many. A host that predates the index sends none: the list is then the page's
// bubbles, under a line saying that is all it can show.
//
// Ids: a bubble's id is its prompt's place in the transcript (MirrorPromptID),
// the same id the index names it by. A bubble with none (an older host) is
// given a page-unique one the first time the index sees it, never reused.
//
// 🔴 THE SHEET IS TABBED (tabs.go): "Your messages" and "Focus history" — the
// session's past focus, newest first (focus_history.go). The tab chosen is
// kept per session in localStorage (the Tabs key, which the page derives from
// the window's address), and the sheet is a FIXED height, so neither switching
// tabs nor a history page arriving moves anything outside the list.
//
// 🔴 ON A CHIEF'S SURFACES IT HAS A THIRD TAB, FIRST: "Sessions" (an operator
// ask: "make it part of the same button that opens the slide up that shows the
// focus and the user messages, and just make it another tab"). It is every
// chat of the chief's — switch, New session, each row's ⋯, a search box and
// filter chips (chiefSessionsTab, chief_chats.go) — and it replaced the
// Sessions drawer that slid in from the right. The tab choice is still
// remembered, so Sessions is the tab a first open lands on and the one a
// returning reader left on after that. The chief's PAGE has the sheet under the
// header's list icon; the chief PANEL has its own, under the list icon in the
// panel's title bar, inside the panel's column. A mirrored session's sheet has
// no Sessions tab: its sessions are not the chief's.
//
// 🔴 ONE SHEET PER SURFACE, EACH ITS OWN IDS, AND THE SCRIPT SCOPED TO THE
// SHEET IT OPENED. The page's sheet is #chat-index (its parts chat-index-*:
// what the specs of the mirrored view and the chief page read), the panel's is
// #chief-panel-index (chiefPanelIndexID). No document muster serves today holds
// both — the chief page and the mirrored view are their own documents, and the
// panel lives in the home shell only — but a boosted navigation keeps the
// script, so nothing here assumes there is one sheet: the opener names its sheet
// (aria-controls), and every part is found inside that sheet by its data mark,
// never by a document-wide id (an invariant guard for a future document, not a
// served one: TestInvariantTwoSheetsOnOneDocumentEachOpenTheirOwn; e2e
// chief-sessions-tab.spec.ts builds that document and opens each).
//
// On the chief's surfaces "Your messages" lists the user bubbles the page
// loaded (no host sends the chief an index), and says so.

// The sheets' ids: the page's (a mirrored view's, the chief page's) and the
// chief panel's.
const (
	chatIndexPageID   = "chat-index"
	chiefPanelIndexID = "chief-panel-index"
)

// chatIndexLabel is the index control's and sheet's name on a mirrored view.
const chatIndexLabel = "Your messages and what it worked on"

// chiefIndexLabel is the chief's: the sheet has its Sessions too.
const chiefIndexLabel = "Sessions, your messages and what it worked on"

// chiefIndexUnreadName is what the chief's list icon's name adds while another
// chat has an unread reply (its dot says so on screen).
const chiefIndexUnreadName = ", unread replies in other sessions"

// The data marks the script finds a sheet's parts by (inside the sheet, never
// by a document-wide id): the sheet itself, its opener, its messages list and
// its three notes.
const (
	chatIndexSheetAttr = "data-chat-index-sheet"
	chatIndexOpenAttr  = "data-chat-index-open"
	chatIndexPartAttr  = "data-chat-index-part"
)

// chatIndexButton is a mirrored view's header control that opens its index
// for the log with id logID.
func chatIndexButton(logID string) g.Node {
	return chatIndexOpener(chatIndexPageID, logID, chatIndexLabel, nil)
}

// chiefIndexButton is the chief's list icon (the page's header, the panel's
// title bar) that opens sheet sheetID — its first tab the chief's Sessions —
// for #chat-log. unread: another chat than the one on screen has an unread
// reply; the icon carries the dot that said so on the Sessions button before
// (chiefSessionsUnreadAttr), and its name says it in words.
func chiefIndexButton(sheetID string, unread bool) g.Node {
	name := chiefIndexLabel
	var dot g.Node
	if unread {
		name += chiefIndexUnreadName
		dot = Span(
			g.Attr(chiefSessionsUnreadAttr, ""),
			g.Attr("aria-hidden", "true"),
			Class("pointer-events-none absolute right-2 top-2 h-2 w-2 rounded-full bg-accent ring-2 ring-bg"),
		)
	}
	return chatIndexOpener(sheetID, "chat-log", name, dot)
}

// chatIndexOpener is a list icon opening sheet sheetID for the log with id
// logID, named name, with extra (the chief's unread dot) over the icon.
func chatIndexOpener(sheetID, logID, name string, extra g.Node) g.Node {
	return Button(
		Type("button"),
		ID(sheetID+"-open"),
		g.Attr(chatIndexOpenAttr, ""),
		g.Attr("data-chat-index-log", logID),
		g.Attr("aria-label", name),
		g.Attr("title", name),
		g.Attr("aria-haspopup", "dialog"),
		g.Attr("aria-controls", sheetID),
		g.Attr("aria-expanded", "false"),
		Class("press relative inline-flex h-11 w-11 shrink-0 items-center justify-center rounded-lg text-fg2 transition hover:bg-s2"),
		g.Raw(`<svg class="h-5 w-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M8 6h13M8 12h13M8 18h13M3 6h.01M3 12h.01M3 18h.01"/></svg>`),
		extra,
	)
}

// chatIndexSheet is a mirrored view's index sheet, closed: a bottom sheet over
// a backdrop, with two tabs — the reader's messages (built by chatIndexScript)
// and the session's focus history (historyPath, FocusHistoryPath of its newest
// page; "" = the host reports no Claude session, so there is none). tabsKey is
// what the tab choice is stored under.
func chatIndexSheet(historyPath, tabsKey string) g.Node {
	return chatIndexSheetOf(chatIndexSpec{ID: chatIndexPageID, Label: chatIndexLabel,
		HistoryPath: historyPath, TabsKey: tabsKey, Partial: chatIndexPartialMirror})
}

// The "Your messages" tab's note when the list is only what the page loaded:
// a mirrored host that sends no index, and the chief's page and panel (which no
// host indexes — they load the chat's latest messages).
const (
	chatIndexPartialMirror = "Only the messages loaded so far are listed: this computer does not send the full list."
	chatIndexPartialPage   = "Only the messages loaded on this page are listed."
	chatIndexPartialPanel  = "Only the messages loaded in this panel are listed."
)

// chatIndexSpec is one sheet: its id (every part's id is built from it), its
// name, its focus history's newest page, its tab key and its messages tab's
// only-loaded note.
type chatIndexSpec struct {
	ID, Label                     string
	HistoryPath, TabsKey, Partial string
	// Sessions, when set, is the chief's Sessions tab (chiefSessionsTab),
	// drawn FIRST; nil on a mirrored view.
	Sessions *Tab
	// InPanel: the sheet is in the chief panel body's relative column —
	// `absolute`, never `fixed`: ChiefPanel carries a transform, so a fixed
	// descendant would be placed against the panel by a rule this file does not
	// control (ChatSurface says the same of the session drawer). The column is
	// its containing block, and its overflow-hidden clips it.
	InPanel bool
}

// chatIndexSheetOf is the sheet sp describes, closed, with the scripts it
// needs (each bound once per document).
//
// 🔴 A FIXED HEIGHT, ON EVERY SURFACE: 70dvh (and in the panel never more than
// the column it is in), so switching tabs, a search narrowing the sessions or a
// history page arriving grows or shrinks the list INSIDE it and moves nothing
// else.
func chatIndexSheetOf(sp chatIndexSpec) g.Node {
	var tabs []Tab
	if sp.Sessions != nil {
		tabs = append(tabs, *sp.Sessions)
	}
	tabs = append(tabs,
		Tab{ID: "messages", Label: "Your messages", Panel: chatIndexMessages(sp.ID, sp.Partial)},
		Tab{ID: "focus", Label: FocusHistoryLabel, Panel: focusHistoryList(sp.HistoryPath)},
	)
	set := Tabs(sp.ID+"-tabs", sp.TabsKey, "Session index", tabs...)
	outer := "fixed inset-0 z-50 flex items-end justify-center sm:items-center"
	box := "relative z-10 flex h-[70dvh] w-full max-w-lg flex-col overflow-hidden rounded-t-2xl bg-s1 shadow-2xl ring-1 ring-line sm:rounded-2xl"
	backdrop := "absolute inset-0 bg-black/60"
	if sp.InPanel {
		outer = "absolute inset-0 z-30 flex items-end justify-center"
		box = "relative z-10 flex h-[70dvh] max-h-full w-full flex-col overflow-hidden rounded-t-2xl bg-s1 shadow-2xl ring-1 ring-line"
		backdrop = "absolute inset-0 rounded-lg bg-black/60"
	}
	return g.Group{Div(
		ID(sp.ID),
		g.Attr(chatIndexSheetAttr, ""),
		g.Attr("hidden", ""),
		g.Attr("role", "dialog"),
		g.Attr("aria-modal", "true"),
		g.Attr("aria-label", sp.Label),
		Class(outer),
		Div(g.Attr("data-chat-index-close", ""), Class(backdrop)),
		Div(
			Class(box),
			Div(
				Class("flex flex-none items-center gap-2 border-b border-line pl-2 pr-2"),
				// 🔴 THE TABS SCROLL SIDEWAYS, THE ROW DOES NOT GROW: three tabs and
				// the ✕ are wider than a 390px phone (and the panel's column), so
				// the tablist sits in a scroller of its own and the ✕ keeps its
				// place at the row's end.
				Div(Class("flex min-w-0 flex-1 overflow-x-auto [scrollbar-width:none] [&::-webkit-scrollbar]:hidden"), set.List),
				Button(
					Type("button"),
					g.Attr("data-chat-index-close", ""),
					g.Attr("aria-label", "Close"),
					Class("press inline-flex h-11 w-11 shrink-0 items-center justify-center rounded-lg text-muted transition hover:bg-s2 hover:text-fg"),
					g.Raw(`<svg class="h-4 w-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M18 6 6 18M6 6l12 12"/></svg>`),
				),
			),
			set.Panels,
		),
	), tabsScript(), focusHistoryScript()}
}

// chatIndexMessages is the "Your messages" tab's panel of sheet sheetID: the
// rows chatIndexScript builds, and its notes. Each part carries its data mark
// (what the script finds it by, inside its sheet) and an id built from the
// sheet's (what the specs read: #chat-index-list on a page).
func chatIndexMessages(sheetID, partial string) g.Node {
	return g.Group{
		// ⚠ Shown when the host sent no prompt index (it predates it, or its
		// scan failed, or — the chief's surfaces — there is no host): the list
		// is only what this page loaded.
		P(ID(sheetID+"-partial"), g.Attr(chatIndexPartAttr, "partial"), g.Attr("hidden", ""), Class("border-b border-line px-4 py-2 text-xs text-muted"),
			g.Text(partial)),
		// The host's bound dropped the oldest prompts: how many.
		P(ID(sheetID+"-more"), g.Attr(chatIndexPartAttr, "more"), g.Attr("hidden", ""), Class("border-b border-line px-4 py-2 text-xs text-muted")),
		Ol(ID(sheetID+"-list"), g.Attr(chatIndexPartAttr, "list"), Class("py-1")),
		P(ID(sheetID+"-empty"), g.Attr(chatIndexPartAttr, "empty"), g.Attr("hidden", ""), Class("px-4 py-6 text-center text-sm text-muted"), g.Text("You have not sent anything yet.")),
	}
}

// chatIndexScript opens and closes the sheets, builds their message rows and
// jumps. Bound once per document (window.muIndex) by delegation; every element
// is looked up when it is used, inside the sheet the opener names.
//
// Every row's text is set with textContent: no transcript text is parsed as
// markup here.
//
//   - ESCAPE IS A CAPTURE LISTENER ON document, so it runs before the chief
//     panel's (which shuts the whole panel): it takes the key (preventDefault,
//     stopPropagation) only while a sheet is open and no popover is — an open
//     ⋯ menu is the top layer, and its Escape is the browser's. The panel's
//     handler skips a prevented Escape.
//   - Tab is kept inside an open sheet (it is aria-modal).
func chatIndexScript() g.Node {
	return Script(g.Raw(`
(function () {
  if (window.muIndex) return;
  var seq = 0, watch = null, cur = null;
  var reduce = window.matchMedia && matchMedia('(prefers-reduced-motion: reduce)');
  var SHEET = '` + chatIndexSheetAttr + `', OPEN = '` + chatIndexOpenAttr + `', PART = '` + chatIndexPartAttr + `';

  function part(s, name) { return s.querySelector('[' + PART + '="' + name + '"]'); }
  function opener(s) { return s && s.id ? document.querySelector('[' + OPEN + '][aria-controls="' + s.id + '"]') : null; }
  function logEl(s) { var b = opener(s); return b && document.getElementById(b.getAttribute('data-chat-index-log')); }
  function bubbles(s) { var l = logEl(s); return l ? Array.prototype.slice.call(l.querySelectorAll('[data-bubble="user"]')) : []; }
  // A bubble's id: its prompt's (MirrorPromptID), else a page-unique one.
  function idOf(el) { if (!el.id) el.id = 'chat-umsg-' + (++seq); return el.id; }
  function firstLine(el) {
    var t = (el.innerText || el.textContent || '').split('\n');
    for (var i = 0; i < t.length; i++) { var s = t[i].trim(); if (s) return s; }
    return '';
  }
  // isOpen: s is a sheet on the page, shown.
  function isOpen(s) { return !!(s && s.isConnected && !s.hidden); }

  // rows is every sent message, oldest first: the host's index (each with its
  // bubble when the page has it), then the page's bubbles it does not name —
  // the ones that streamed in after it, or all of them from a host with none.
  function rows(s) {
    var idx = window.muMirrorIndex, l = logEl(s), out = [], named = {};
    if (idx && idx.items) {
      idx.items.forEach(function (it) {
        var el = document.getElementById(it.id);
        named[it.id] = true;
        out.push({ id: it.id, text: it.text, el: el && l && l.contains(el) ? el : null });
      });
    }
    bubbles(s).forEach(function (b) {
      if (b.id && named[b.id]) return;
      out.push({ id: idOf(b), text: firstLine(b), el: b });
    });
    return out;
  }

  function build(s) {
    var list = s && part(s, 'list');
    if (!list) return;
    var idx = window.muMirrorIndex, rs = rows(s), more = idx && idx.more > 0 ? idx.more : 0;
    var partial = part(s, 'partial');
    if (partial) partial.hidden = !!idx;
    var moreEl = part(s, 'more');
    if (moreEl) {
      moreEl.hidden = more === 0;
      moreEl.textContent = more === 0 ? '' : more + ' more earlier, not listed';
    }
    list.textContent = '';
    rs.forEach(function (r, i) {
      var li = document.createElement('li');
      var n = document.createElement('span'); n.className = 'w-6 shrink-0 text-right text-xs tabular-nums text-muted'; n.textContent = String(more + i + 1);
      var t = document.createElement('span');
      if (r.el) {
        var btn = document.createElement('button');
        btn.type = 'button';
        btn.setAttribute('data-chat-index-item', r.id);
        btn.className = 'press flex min-h-[44px] w-full items-center gap-3 px-4 py-2 text-left text-sm text-fg transition hover:bg-s2';
        t.className = 'min-w-0 flex-1 truncate'; t.textContent = r.text || '(empty)';
        btn.appendChild(n); btn.appendChild(t);
        li.appendChild(btn);
      } else {
        // Not on the page: nothing to jump to, so its whole text is here.
        li.setAttribute('data-chat-index-unloaded', r.id);
        li.className = 'flex min-h-[44px] w-full items-start gap-3 px-4 py-2 text-sm text-fg2';
        var body = document.createElement('span'); body.className = 'flex min-w-0 flex-1 flex-col gap-0.5';
        var mark = document.createElement('span'); mark.className = 'text-[11px] font-medium uppercase tracking-wide text-muted';
        mark.setAttribute('data-chat-index-mark', ''); mark.textContent = 'earlier — not loaded';
        t.className = 'whitespace-pre-wrap [overflow-wrap:anywhere]'; t.textContent = r.text || '(empty)';
        body.appendChild(mark); body.appendChild(t);
        li.appendChild(n); li.appendChild(body);
      }
      list.appendChild(li);
    });
    var empty = part(s, 'empty');
    if (empty) empty.hidden = rs.length > 0 || more > 0;
  }

  // shown: el is on screen (a hidden row, a shut panel's, is not).
  function shown(el) { return !!(el && el.getClientRects().length); }

  // open shows sheet s (shutting another first). The focus goes to the row of
  // the chat on screen when the tab open is the chief's Sessions and the row is
  // not filtered away, else to ✕.
  function open(s) {
    if (!s) return;
    if (cur && cur !== s) close({ focus: false });
    var b = opener(s);
    build(s);
    s.hidden = false;
    cur = s;
    if (b) b.setAttribute('aria-expanded', 'true');
    // Keep the list current while it is open: new messages stream in. (A
    // watch left from a sheet a swap replaced while open is dropped first.)
    if (watch) { watch.disconnect(); watch = null; }
    var l = logEl(s);
    if (l && typeof MutationObserver !== 'undefined') {
      watch = new MutationObserver(function () { if (isOpen(s)) build(s); });
      watch.observe(l, { childList: true, subtree: true });
    }
    var list = part(s, 'list');
    var last = list && list.lastElementChild;
    if (last) last.scrollIntoView({ block: 'nearest' }); // the newest is nearest the reader
    var f = s.querySelector('[role="tabpanel"]:not([hidden]) [aria-current="true"]');
    if (!shown(f)) f = s.querySelector('button[data-chat-index-close]');
    try { if (f) f.focus({ preventScroll: true }); } catch (e) {}
  }
  // close shuts the open sheet; the focus goes back to its opener unless
  // opts.focus is false (a pick that is leaving anyway).
  function close(opts) {
    var s = cur;
    cur = null;
    if (watch) { watch.disconnect(); watch = null; }
    if (!s || s.hidden) return;
    s.hidden = true;
    var b = opener(s);
    if (b) {
      b.setAttribute('aria-expanded', 'false');
      if (opts && opts.focus === false) return;
      // An opener in a closed ⋯ menu cannot take focus: its trigger does
      // (floatingMenuScript's __musterFocusBack).
      var f = window.__musterFocusBack ? window.__musterFocusBack(b) : b;
      try { if (f) f.focus({ preventScroll: true }); } catch (e) {}
    }
  }

  function jump(id) {
    var el = document.getElementById(id);
    close();
    if (!el) return;
    // Open it first WITHOUT the clamp's stick-to-bottom (the reader is going
    // to IT, not to the end), then bring its top into view: it grows below its
    // top, so the top stays where it is put.
    if (window.muClamp && el.getAttribute('data-clamp') === 'closed') window.muClamp.expand(el, { follow: false });
    el.scrollIntoView({ block: 'start', behavior: reduce && reduce.matches ? 'auto' : 'smooth' });
    el.removeAttribute('data-flash');
    void el.offsetWidth; // restart the highlight if it is still running
    el.setAttribute('data-flash', '');
    setTimeout(function () { el.removeAttribute('data-flash'); }, 1600);
  }

  // The controls of s that Tab reaches now (a shut ⋯ menu's, a hidden tab's,
  // a filtered-away row's and a roving tablist's unselected tabs are not).
  function focusables(s) {
    var all = s.querySelectorAll('a[href], button:not([disabled]), input:not([disabled]):not([type="hidden"]), select, textarea, [tabindex]:not([tabindex="-1"])');
    var out = [];
    for (var i = 0; i < all.length; i++) if (shown(all[i]) && all[i].getAttribute('tabindex') !== '-1') out.push(all[i]);
    return out;
  }

  document.addEventListener('click', function (e) {
    var t = e.target;
    if (!t || !t.closest) return;
    var o = t.closest('[' + OPEN + ']');
    if (o) {
      var s = document.getElementById(o.getAttribute('aria-controls') || '');
      if (isOpen(s)) close(); else open(s);
      return;
    }
    var item = t.closest('[data-chat-index-item]');
    if (item) { jump(item.getAttribute('data-chat-index-item')); return; }
    if (t.closest('[data-chat-index-close]')) close();
  });
  document.addEventListener('keydown', function (e) {
    if (!isOpen(cur)) return;
    if (e.key === 'Escape') {
      if (e.defaultPrevented) return;
      try { if (document.querySelector(':popover-open')) return; } catch (err) {}
      e.preventDefault();
      e.stopPropagation();
      close();
      return;
    }
    if (e.key !== 'Tab') return;
    var f = focusables(cur);
    if (!f.length) return;
    var first = f[0], last = f[f.length - 1], at = document.activeElement;
    var inside = at && cur.contains(at);
    if (e.shiftKey && (at === first || !inside)) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && (at === last || !inside)) { e.preventDefault(); first.focus(); }
  }, true);
  // The host's index arrived (or a Reset dropped it) while the sheet is open.
  document.addEventListener('muster:chat-index', function () { if (isOpen(cur)) build(cur); });

  window.muIndex = { open: open, close: close, jump: jump, current: function () { return isOpen(cur) ? cur : null; } };
})();
`))
}
