package ui

import (
	"strconv"
	"time"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// SUGGESTED REPLIES' CHIP ROW (docs/suggested-replies.md): up to three short
// replies a model wrote for a mirrored Claude Code window's turn end, in a row
// directly above the Rendered view's composer. The session switcher sits
// below the input; the two never compete.
//
// 🔴 A TAP FILLS, IT NEVER SENDS. A chip is a type="button" outside the form:
// it puts its text in the message box — the whole box when it is empty, or on
// a new line below a draft already there, never over it — as an EDIT
// (insertText, so Ctrl+Z gives the draft back), and focuses the box with the
// caret at the end. Send is still the operator's, with its own step-up gate.
//
// 🔴 LABELLED AI. The row is a group named SuggestRowLabel, it starts with an
// "AI" mark, and each chip's accessible name says it is an AI-suggested
// reply. Every string from the server is drawn with textContent.
//
// 🔴 NO LAYOUT SHIFT, AND ROOM ONLY WHILE IT IS IN USE. The row is out of flow
// (absolute, bottom-full of the pinned #mv-compose), so showing it moves
// nothing in the composer; its 44px are reserved — added to the pinned
// composer's measured room (composerPinScript), which shrinks the transcript
// from the bottom — only while a set is pending or shown. Hidden, it takes
// none. Without script it is never shown (it is drawn hidden).
//
// 🔴 WITHIN SuggestCutoff OF THE TURN END OR NOT AT ALL. The socket says how
// long ago the turn ended (pending/ready); a set that is not on screen by the
// cutoff is dropped, and the row with it. (A page newly opened on an idle
// turn is a turn end of its own, within the cutoff of the open; a reload
// resumes its socket and shows no row for the turn it reloaded into.)
//
// What it reports (SuggestEventPath): "shown" once per set, "filled" on a tap,
// "sent" when the box is sent after a tap — with the EDIT DISTANCE between
// what the tap left in the box and what was sent. Numbers and the set's
// fingerprint only: never any text.

// SuggestCutoff is how long after a turn ends its suggestions may still
// appear; past it they never do (and the server stops before spending).
const SuggestCutoff = 8 * time.Second

// SuggestPath is the window's current set (JSON); SuggestEventPath where the
// row reports what happened to it.
const (
	SuggestPath      = "/mirror/view/suggest"
	SuggestEventPath = "/mirror/view/suggest/event"
)

// SuggestRowLabel names the row (its group's accessible name).
const SuggestRowLabel = "AI-suggested replies"

// suggestRow is the chip row of a composer whose Suggest is set (its set's
// URL), drawn hidden; nil for none.
func suggestRow(src string) g.Node {
	if src == "" {
		return nil
	}
	return Div(
		ID("mv-suggest"),
		g.Attr("hidden", ""),
		g.Attr("role", "group"),
		g.Attr("aria-label", SuggestRowLabel),
		g.Attr("data-src", src),
		g.Attr("data-event", SuggestEventPath),
		g.Attr("data-cutoff-ms", strconv.FormatInt(SuggestCutoff.Milliseconds(), 10)),
		Class("absolute inset-x-0 bottom-full z-20 flex h-11 items-center gap-1 border-t border-line bg-bg pl-3"),
		Span(
			g.Attr("title", "Written by a model from this conversation"),
			Class("shrink-0 rounded-md bg-s2 px-1.5 py-0.5 text-[10px] font-semibold tracking-wide text-fg2 ring-1 ring-inset ring-line"),
			g.Text("AI"),
		),
		Div(ID("mv-suggest-chips"), Class("flex h-11 min-w-0 flex-1 items-center gap-1 overflow-x-auto overflow-y-hidden")),
		Button(
			ID("mv-suggest-x"),
			Type("button"),
			g.Attr("aria-label", "Hide suggestions for this turn"),
			g.Attr("title", "Hide suggestions for this turn"),
			Class("press inline-flex h-11 w-11 shrink-0 items-center justify-center rounded-lg text-fg2 transition hover:bg-s2"),
			g.Raw(`<svg class="h-4 w-4" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true"><path d="M6.28 5.22a.75.75 0 00-1.06 1.06L8.94 10l-3.72 3.72a.75.75 0 101.06 1.06L10 11.06l3.72 3.72a.75.75 0 101.06-1.06L11.06 10l3.72-3.72a.75.75 0 00-1.06-1.06L10 8.94 6.28 5.22z"/></svg>`),
		),
	)
}

// SuggestPausedText is the composer's line while the viewer's suggested
// replies are auto-paused.
const SuggestPausedText = "Suggested replies are paused: you rarely tapped them."

// suggestPausedNote is that line, with the way back on.
func suggestPausedNote() g.Node {
	return P(ID("mv-suggest-paused"), Class("flex flex-wrap items-center gap-x-1 px-3 text-[11px] text-muted"),
		g.Text(SuggestPausedText+" "),
		A(Href("/account#suggest"), Class("inline-flex min-h-[44px] items-center text-accent underline"), g.Text("Turn them back on")))
}

// suggestChipClass is a chip: a 44px tap target around a pill at most 32px
// tall, its text cut with an ellipsis (the whole text is its title and name).
const (
	suggestChipClass = "press inline-flex min-h-[44px] max-w-[16rem] shrink-0 items-center"
	suggestPillClass = "block max-h-8 truncate rounded-full bg-s2 px-3 py-1.5 text-sm leading-5 text-fg ring-1 ring-inset ring-line hover:bg-s3"
)

// READING A LONG CHIP (docs/suggested-replies.md). A chip is cut at 16rem, so
// a long suggestion's end — usually the part that decides it — was unreadable
// (measured at 412px: a 138-character suggestion showed 232 of its 965px).
// Each chip therefore sits in an item with, at its END, an expand button
// (SuggestMoreLabel) that opens the whole text in a popover.
//
//   - A SIBLING, NEVER INSIDE THE CHIP: a button in a button is invalid, and
//     the chip's own tap (fill) must stay exactly what it was.
//   - SHOWN ONLY WHILE THE TEXT IS CUT. The pill is one nowrap line with
//     overflow hidden, so scrollWidth > clientWidth says exactly whether it is
//     cut; a ResizeObserver on each pill re-measures on a resize, a rotation or
//     a late web font. It cannot oscillate: the item and the chip are
//     shrink-0 in a row that scrolls sideways, so showing the button never
//     changes the pill's width. (Always showing it would cost every chip that
//     fits 32–44px of a row that is already short on a phone, for a popover
//     repeating what is on screen.)
//   - 44px TALL ALWAYS (the row's height), 44px WIDE ON A COARSE POINTER,
//     32px with a mouse.
//   - THE POPOVER IS fullTitlePopover's PATTERN: a native popover="auto" (top
//     layer — the row's sideways scroll does not clip it; an outside tap or
//     Escape closes it) placed by floatingMenuScript beside its button, height
//     capped to the viewport. Only the text scrolls, so its × and "Use this"
//     stay on screen for any length. The focus moves to the panel on open and
//     back to the button on Escape or × (the browser's, for a popover its
//     button opened). No animation, so nothing for reduced motion to remove.
//   - "Use this" IS THE CHIP'S TAP: the same take() (fill, never send; the
//     same "filled" report), after closing the popover.
//   - CLEARED UNDER AN OPEN POPOVER (a send, the next set): it is closed first,
//     floatingMenuScript is told, and the focus goes to the box (clearChips).
//   - NO POPOVER API (iOS < 17): the button is never shown, so it cannot sit
//     there doing nothing; the chip's tap still fills.
const (
	suggestItemClass = "inline-flex shrink-0 items-center"
	suggestMoreClass = "press inline-flex h-11 w-8 shrink-0 items-center justify-center rounded-lg text-fg2 transition hover:bg-s2 hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus aria-expanded:bg-s2 [@media(pointer:coarse)]:w-11 [&[hidden]]:hidden"
	suggestFullClass = "invisible hidden [&:popover-open]:flex m-0 w-80 max-w-[calc(100vw-16px)] overscroll-contain flex-col gap-2 rounded-xl border border-line bg-s1 p-3 text-fg shadow-2xl ring-1 ring-black/20 focus:outline-none"
	suggestFullText  = "min-h-0 overflow-y-auto whitespace-pre-wrap [overflow-wrap:anywhere] text-sm leading-6"
	suggestFullClose = "press inline-flex h-11 w-11 shrink-0 items-center justify-center rounded-lg text-fg2 transition hover:bg-s2 hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus"
	suggestFullUse   = "press inline-flex min-h-[44px] items-center justify-center rounded-xl bg-accent px-4 text-sm font-semibold text-on-accent transition hover:bg-accent/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus"
)

// SuggestMoreLabel names a chip's expand button; SuggestFullLabel its popover.
const (
	SuggestMoreLabel = "Show full suggestion"
	SuggestFullLabel = "Full suggestion"
)

// suggestRowScript drives the row: the socket's news (window.musterSuggest,
// called by mirrorViewScript), the fetch of the set, the taps, the × and the
// reports. See the file's comment for the rules.
func suggestRowScript() g.Node {
	return Script(g.Raw(`
(function () {
  var row = document.getElementById('mv-suggest');
  var input = document.getElementById('mv-input');
  if (!row || !input) return;
  var chips = document.getElementById('mv-suggest-chips');
  var x = document.getElementById('mv-suggest-x');
  var src = row.getAttribute('data-src'), evURL = row.getAttribute('data-event');
  var cutoff = +row.getAttribute('data-cutoff-ms') || 8000;
  // gen invalidates whatever an earlier state started (a fetch, a timer);
  // fp is the set on screen; fill what the last tap left in the box.
  var gen = 0, deadline = 0, timer = 0, fp = '', fill = null;
  // countable: the set answers a turn that ENDED while this view was open (an
  // open on an idle session is shown but not reported); seen: "shown" was
  // reported for the set on screen; inView: the row is in the viewport.
  var countable = false, seen = false, inView = false;
  var io = typeof IntersectionObserver !== 'undefined' ? new IntersectionObserver(function (es) {
    es.forEach(function (en) { inView = en.isIntersecting && en.intersectionRatio > 0; });
    seenNow();
  }, { threshold: [0, 0.5] }) : null;
  var DKEY = 'mv-suggest-dismissed';
  function dismissedSet(f) { try { return !!f && localStorage.getItem(DKEY) === f; } catch (e) { return false; } }
  function dismiss(f) { try { localStorage.setItem(DKEY, f); } catch (e) {} }
  function measure() { if (window.musterPinMeasure) window.musterPinMeasure(); }
  function state() { return row.getAttribute('data-state') || ''; }
  function hide() {
    gen++;
    clearTimeout(timer);
    deadline = 0; fp = '';
    if (io) io.unobserve(row);
    inView = false;
    if (row.hidden && !state()) return;
    row.hidden = true;
    row.removeAttribute('data-state');
    if (fitRO) fitRO.disconnect();
    clearChips();
    measure();
  }
  // clearChips empties the row. An expand popover open in it is CLOSED first,
  // and its close is told to floatingMenuScript, which stops observing it and
  // drops its inline position only on a toggle it sees at the document. A
  // popover merely removed from the page fires no toggle at all; and the one
  // hidePopover() queues fires a task LATER, on the element already detached
  // here, so it never reaches the document either (measured: none seen in
  // Chromium). So the closed toggle is dispatched now, while it is still in
  // the page. A focus inside the row, which would fall to BODY, goes to the box.
  function clearChips() {
    var had = chips.contains(document.activeElement);
    var ps = chips.querySelectorAll('[popover]');
    for (var i = 0; i < ps.length; i++) {
      var open = false;
      try { open = ps[i].matches(':popover-open'); if (open) ps[i].hidePopover(); } catch (e) {}
      if (!open) continue;
      var ev;
      try { ev = new ToggleEvent('toggle', { oldState: 'open', newState: 'closed' }); } catch (e) { ev = new Event('toggle'); }
      ps[i].dispatchEvent(ev);
    }
    chips.textContent = '';
    if (had) input.focus();
  }
  // fit shows an item's expand button exactly while its chip's text is cut,
  // and closes its popover when it no longer is (see suggest_row.go) — and
  // never without the Popover API (iOS < 17), where the button would do
  // nothing; the chip's own tap still fills.
  var POPOVER = typeof HTMLElement !== 'undefined' && HTMLElement.prototype.hasOwnProperty('popover');
  function fit(item) {
    var p = item.querySelector('[data-suggest-chip] > span'), m = item.querySelector('[data-suggest-more]');
    if (!p || !m) return;
    var cut = p.scrollWidth > p.clientWidth + 1;
    if (cut) { if (m.hidden && POPOVER) m.hidden = false; return; }
    if (!m.hidden) m.hidden = true;
    var pop = document.getElementById(m.getAttribute('popovertarget'));
    try { if (pop && pop.matches(':popover-open')) pop.hidePopover(); } catch (e) {}
  }
  var fitRO = typeof ResizeObserver !== 'undefined' ? new ResizeObserver(function (es) {
    es.forEach(function (en) { var it = en.target.closest('[data-suggest-item]'); if (it) fit(it); });
  }) : null;
  var SVG = 'http://www.w3.org/2000/svg';
  function icon(d) {
    var v = document.createElementNS(SVG, 'svg');
    v.setAttribute('class', 'h-4 w-4');
    v.setAttribute('viewBox', '0 0 20 20');
    v.setAttribute('fill', 'currentColor');
    v.setAttribute('aria-hidden', 'true');
    var path = document.createElementNS(SVG, 'path');
    path.setAttribute('d', d);
    v.appendChild(path);
    return v;
  }
  // The expand icon (two corner arrows) and the popover's ×.
  var EXPAND = 'M3.75 3a.75.75 0 00-.75.75v3.5a.75.75 0 001.5 0V5.56l3.22 3.22a.75.75 0 001.06-1.06L5.56 4.5h1.69a.75.75 0 000-1.5h-3.5zM12.75 3a.75.75 0 000 1.5h1.69l-3.22 3.22a.75.75 0 001.06 1.06l3.22-3.22v1.69a.75.75 0 001.5 0v-3.5a.75.75 0 00-.75-.75h-3.5zM8.78 12.28a.75.75 0 00-1.06-1.06L4.5 14.44v-1.69a.75.75 0 00-1.5 0v3.5c0 .414.336.75.75.75h3.5a.75.75 0 000-1.5H5.56l3.22-3.22zM12.28 11.22a.75.75 0 00-1.06 1.06l3.22 3.22h-1.69a.75.75 0 000 1.5h3.5a.75.75 0 00.75-.75v-3.5a.75.75 0 00-1.5 0v1.69l-3.22-3.22z';
  var CLOSE = 'M6.28 5.22a.75.75 0 00-1.06 1.06L8.94 10l-3.72 3.72a.75.75 0 101.06 1.06L10 11.06l3.72 3.72a.75.75 0 101.06-1.06L11.06 10l3.72-3.72a.75.75 0 00-1.06-1.06L10 8.94 6.28 5.22z';
  // full builds chip i's expand button and its popover: the whole text
  // (textContent), a × and "Use this", which is the chip's own tap.
  function full(s, i, chip) {
    var id = 'mv-suggest-full-' + i;
    var m = document.createElement('button');
    m.type = 'button';
    m.hidden = true;
    m.className = '` + suggestMoreClass + `';
    m.setAttribute('data-suggest-more', String(i));
    m.setAttribute('popovertarget', id);
    m.setAttribute('aria-haspopup', 'dialog');
    m.setAttribute('aria-controls', id);
    m.setAttribute('aria-expanded', 'false');
    m.setAttribute('aria-label', '` + SuggestMoreLabel + `');
    m.title = '` + SuggestMoreLabel + `';
    m.appendChild(icon(EXPAND));
    var pop = document.createElement('div');
    pop.id = id;
    pop.setAttribute('popover', 'auto');
    pop.setAttribute('role', 'dialog');
    pop.setAttribute('aria-label', '` + SuggestFullLabel + `');
    pop.setAttribute('data-floating-menu', '');
    pop.tabIndex = -1;
    pop.className = '` + suggestFullClass + `';
    var head = document.createElement('div');
    head.className = 'flex items-center gap-2';
    // Still labelled AI (the row's rule), in words: a second "AI" mark would
    // make the row's own one ambiguous to anything that finds it by text.
    var cap = document.createElement('span');
    cap.className = 'min-w-0 flex-1 text-xs font-medium text-muted';
    cap.textContent = 'AI-suggested reply';
    var x = document.createElement('button');
    x.type = 'button';
    x.className = '` + suggestFullClose + `';
    x.setAttribute('popovertarget', id);
    x.setAttribute('popovertargetaction', 'hide');
    x.setAttribute('aria-label', 'Close');
    x.title = 'Close';
    x.appendChild(icon(CLOSE));
    head.appendChild(cap); head.appendChild(x);
    var t = document.createElement('p');
    t.className = '` + suggestFullText + `';
    t.setAttribute('data-suggest-full-text', '');
    t.textContent = s.text;
    var acts = document.createElement('div');
    acts.className = 'flex justify-end';
    var use = document.createElement('button');
    use.type = 'button';
    use.className = '` + suggestFullUse + `';
    use.setAttribute('data-suggest-use', String(i));
    use.textContent = 'Use this';
    // The chip's tap: close first (the browser gives the focus back to the
    // button), then take() moves it to the box — or, from a keyboard, to the
    // chip, so a held Enter cannot go on to send the box.
    use.addEventListener('click', function (e) {
      try { pop.hidePopover(); } catch (err) {}
      take(s.text, e.detail === 0 ? chip : null);
    });
    use.addEventListener('keydown', function (e) { if (e.repeat && (e.key === 'Enter' || e.key === ' ')) e.preventDefault(); });
    acts.appendChild(use);
    pop.appendChild(head); pop.appendChild(t); pop.appendChild(acts);
    // The focus moves in on open (the panel, so the text is read first).
    pop.addEventListener('toggle', function (e) { if (e.newState === 'open' && !pop.contains(document.activeElement)) pop.focus(); });
    return [m, pop];
  }
  // blocked: what the server excludes, seen on the page too — a question card
  // drawn, an attention ask (shown or hidden on this device: hidden is not
  // answered).
  function blocked() {
    var q = document.getElementById('mv-asks');
    if (q && !q.hidden && q.querySelector('form, button')) return true;
    var a = document.getElementById('asker-asks');
    if (a && a.querySelector('[id^="ask-"]')) return true;
    return false;
  }
  function post(ev, extra) {
    if (!fp && !(extra && extra.fp)) return;
    try {
      var b = new URLSearchParams(src.split('?')[1] || '');
      b.set('fingerprint', (extra && extra.fp) || fp);
      b.set('event', ev);
      if (extra && typeof extra.dist === 'number') b.set('dist', String(extra.dist));
      fetch(evURL, { method: 'POST', body: b, credentials: 'same-origin',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded', 'X-Requested-With': 'XMLHttpRequest' } }).catch(function () {});
    } catch (e) {}
  }
  // reserve: a set is being made for a turn that ended ms ago. The row takes
  // its room now (empty, inert) and gives it back at the cutoff.
  function reserve(ms, open) {
    hide();
    countable = !open;
    if (blocked()) return;
    deadline = Date.now() + cutoff - (ms || 0);
    if (deadline <= Date.now()) { deadline = 0; return; }
    row.setAttribute('data-state', 'pending');
    row.hidden = false;
    measure();
    var g = gen;
    timer = setTimeout(function () { if (g === gen && state() === 'pending') hide(); }, deadline - Date.now());
  }
  function load() {
    if (state() !== 'pending') return;
    var g = gen;
    fetch(src, { credentials: 'same-origin', headers: { 'Accept': 'application/json' } })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (v) {
        if (g !== gen) return;
        var list = v && v.suggestions ? v.suggestions.slice(0, 3) : [];
        if (!list.length || Date.now() > deadline || blocked() || dismissedSet(v.fingerprint)) { hide(); return; }
        show(v.fingerprint, list);
      }, function () { if (g === gen) hide(); });
  }
  function show(f, list) {
    clearTimeout(timer);
    if (fitRO) fitRO.disconnect();
    clearChips();
    fp = f;
    list.forEach(function (s, i) {
      var b = document.createElement('button');
      b.type = 'button';
      b.className = '` + suggestChipClass + `';
      b.setAttribute('data-suggest-chip', String(i));
      b.setAttribute('data-kind', s.kind || '');
      b.setAttribute('aria-label', 'AI-suggested reply: ' + s.text);
      b.title = s.text;
      var p = document.createElement('span');
      p.className = '` + suggestPillClass + `';
      p.textContent = s.text;
      b.appendChild(p);
      // detail 0: a keyboard activation (Enter, Space) — the focus stays on
      // the chip, so a held Enter cannot go on to send the box.
      b.addEventListener('click', function (e) { take(s.text, e.detail === 0 ? b : null); });
      // A held Enter or Space fills ONCE (a button repeats its activation).
      b.addEventListener('keydown', function (e) { if (e.repeat && (e.key === 'Enter' || e.key === ' ')) e.preventDefault(); });
      // The item: the chip, then its expand button and popover (full).
      var item = document.createElement('span');
      item.className = '` + suggestItemClass + `';
      item.setAttribute('data-suggest-item', String(i));
      item.appendChild(b);
      full(s, i, b).forEach(function (n) { item.appendChild(n); });
      chips.appendChild(item);
    });
    row.setAttribute('data-state', 'shown');
    row.hidden = false;
    // Measured now (laid out, before the first paint) and on every resize.
    chips.querySelectorAll('[data-suggest-item]').forEach(function (it) {
      fit(it);
      var p = it.querySelector('[data-suggest-chip] > span');
      if (fitRO && p) fitRO.observe(p);
    });
    measure();
    seen = false;
    if (io) io.observe(row); else { inView = true; seenNow(); }
  }
  // seenNow reports "shown" once per set, and only when it was really seen:
  // the page visible AND the row in the viewport (a hidden tab, a row off
  // screen, are not an impression the auto-pause may count).
  function seenNow() {
    if (!countable || seen || !fp || state() !== 'shown' || !inView || document.visibilityState !== 'visible') return;
    seen = true;
    post('shown');
  }
  document.addEventListener('visibilitychange', seenNow);
  // take fills the box with text — below a draft, never over it — as an edit.
  // keyEl: the chip a keyboard activated, which gets the focus back.
  function take(text, keyEl) {
    var cur = input.value;
    var draft = cur.trim() !== '';
    var add = draft ? (/\n$/.test(cur) ? '' : '\n') + text : text;
    input.focus();
    input.setSelectionRange(draft ? cur.length : 0, cur.length);
    var done = false;
    try { done = !!(document.execCommand && document.execCommand('insertText', false, add)); } catch (e) {}
    if (!done || input.value === cur) {
      input.setRangeText(add, draft ? cur.length : 0, cur.length, 'end');
      input.dispatchEvent(new Event('input', { bubbles: true }));
    }
    var end = input.value.length;
    input.setSelectionRange(end, end);
    if (window.musterComposerFit) window.musterComposerFit(input);
    if (keyEl) keyEl.focus();
    fill = { fp: fp, after: input.value, countable: countable };
    if (countable) {
      // A tap is seen by definition.
      if (!seen) { seen = true; post('shown'); }
      post('filled');
    }
  }
  // dist is the edit distance between a and b (Levenshtein, over at most
  // 4000 characters of each): how much was changed after a tap. A number
  // is all that leaves the page.
  function dist(a, b) {
    a = a.slice(0, 4000); b = b.slice(0, 4000);
    if (a === b) return 0;
    var prev = new Array(b.length + 1), cur = new Array(b.length + 1), i, j;
    for (j = 0; j <= b.length; j++) prev[j] = j;
    for (i = 1; i <= a.length; i++) {
      cur[0] = i;
      for (j = 1; j <= b.length; j++) {
        cur[j] = Math.min(prev[j] + 1, cur[j - 1] + 1, prev[j - 1] + (a.charCodeAt(i - 1) === b.charCodeAt(j - 1) ? 0 : 1));
      }
      var t = prev; prev = cur; cur = t;
    }
    return prev[b.length];
  }
  x.addEventListener('click', function () {
    if (fp) dismiss(fp);
    hide();
    input.focus();
  });
  // A message went (Send or Queue, mirrorComposerScript): a new turn starts,
  // and a tap before it is reported with how much it was edited.
  document.addEventListener('mv-sent', function (e) {
    var sent = e.detail && typeof e.detail.text === 'string' ? e.detail.text : null;
    if (fill && fill.countable && sent !== null) post('sent', { fp: fill.fp, dist: dist(fill.after, sent) });
    fill = null;
    hide();
  });
  // A question card or an attention ask that arrives while chips are up
  // takes them away (the server's exclusions run only when a set is made or
  // read).
  if (typeof MutationObserver !== 'undefined') {
    var mo = new MutationObserver(function () { if (state() && blocked()) hide(); });
    ['mv-asks', 'asker-asks'].forEach(function (id) {
      var el = document.getElementById(id);
      if (el) mo.observe(el, { childList: true, subtree: true, attributes: true, attributeFilter: ['hidden'] });
    });
  }
  window.musterSuggest = function (m) {
    if (!m) return;
    if (m.state === 'pending') reserve(m.ms, m.open);
    else if (m.state === 'ready') load();
    else hide();
  };
  window.musterSuggestDist = dist;
})();
`))
}
