package ui

import (
	"html"
	"strconv"
)

// THE ASKS CARD FEED (operator decision "Vertical card feed", docs/attention.md
// "The card feed"): every item on the Asks page is a CARD in one vertically
// scrolling feed per tab. internal/api builds the cards as HTML strings (the
// page is its document); the classes and the feed's script live here because
// the stylesheet scans internal/ui, not internal/api — a class written only
// there would ship unstyled with css-check green.
//
// A card, top to bottom:
//
//   - its HEAD, a <button data-asks-toggle aria-expanded>: the kind badge
//     (AsksKindBadge), who is waiting (the session's name, the one naming
//     rule: docs/attention.md "Every card names its session the same way"),
//     how long, and a chevron. The head is NOT a link any more: tapping it —
//     or anywhere on the card outside its controls — expands the card in place;
//   - the QUESTION (or what the session waits on), clamped to four lines while
//     the card is collapsed;
//   - the CONTEXT slot ("why it's here") and the MEDIA slot (internal/api
//     asksCardContextSlot / asksCardMediaSlot: the seam the ask-attachments
//     work fills);
//   - its CONTROLS: the option buttons, the suggested-reply chips, the answer
//     box with its microphone and Answer;
//   - the MORE region (data-asks-card-more), shown only expanded: the secondary
//     facts the card used to draw inline;
//   - its FOOTER: View in session, the task link, and Dismiss on the far end —
//     never in the head (Round 0 of #620).
//
// The motion (enter, leave, expand, the tab slide, press feedback) is CSS in
// web/css/input.css ("The Asks card feed"), every rule of it switched off under
// prefers-reduced-motion: reduce.

const (
	// AsksSwipeAreaClass is the tab panels' wrapper, the swipe area: at least
	// most of a screen tall, so a sideways swipe on the empty space under a
	// short feed still switches tabs.
	AsksSwipeAreaClass = "flex min-h-[60dvh] min-w-0 flex-col"
	// AsksFeedClass is a tab's feed: the <ul> of cards.
	AsksFeedClass = "asks-feed flex flex-col gap-3"
	// AsksFeedCardClass is one card (an <li>). data-expanded drives the more
	// region and the clamp (input.css).
	AsksFeedCardClass = "asks-card relative flex min-w-0 flex-col gap-2 rounded-2xl bg-s1 p-3 ring-1 ring-inset ring-line focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus data-[expanded=true]:ring-edge"
	// AsksCardHeadClass is the head: one full-width toggle button.
	AsksCardHeadClass = "press -m-1 flex min-h-[44px] min-w-0 items-center gap-2 rounded-lg p-1 text-left transition hover:bg-s2 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus"
	// AsksCardWhoClass wraps the head's name and its sub line.
	AsksCardWhoClass = "flex min-w-0 flex-1 flex-col"
	// AsksCardQuestionClass is the card's question, clamped while collapsed.
	AsksCardQuestionClass = "asks-clamp whitespace-pre-wrap break-words text-[15px] leading-snug text-fg"
	// AsksCardContextClass is the "why it's here" line.
	AsksCardContextClass = "asks-clamp-1 break-words text-xs text-fg2"
	// AsksCardMoreClass is the expanded-only region (its one child carries
	// AsksCardMoreInnerClass, so it can collapse to zero rows).
	AsksCardMoreClass      = "asks-more"
	AsksCardMoreInnerClass = "flex min-h-0 min-w-0 flex-col gap-2 overflow-hidden"
	// AsksCardFooterClass is the footer row: links, then Dismiss at its end.
	AsksCardFooterClass = "flex min-w-0 flex-wrap items-center gap-1"
	// AsksCardLinkClass is a footer link (View in session, the task).
	AsksCardLinkClass = "press inline-flex min-h-[44px] items-center gap-1 rounded-lg px-2 text-sm font-semibold text-accent transition hover:bg-s2 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus"
	// AsksCardChipsClass / AsksCardChipClass are the suggested replies: a tap
	// FILLS the answer box (nothing is sent before Answer).
	AsksCardChipsClass = "flex flex-wrap gap-1.5"
	AsksCardChipClass  = "press min-h-[44px] max-w-full break-words rounded-full bg-bg px-3 py-1.5 text-left text-sm text-fg ring-1 ring-inset ring-accent/40 transition hover:bg-s2"
	// AsksCardSendClass is the answer row's Answer: compact, beside the box.
	AsksCardSendClass = "press min-h-[44px] shrink-0 rounded-lg bg-accent px-4 text-sm font-semibold text-on-accent transition hover:bg-accent/90"
	// AsksCardSendQuietClass is Answer on a card whose options are its primary
	// action: outlined, not filled.
	AsksCardSendQuietClass = "press min-h-[44px] shrink-0 rounded-lg px-4 text-sm font-semibold text-fg ring-1 ring-inset ring-edge transition hover:bg-s2"
	// AsksCountClass is the page header's "N open".
	AsksCountClass = "inline-flex items-center rounded-full bg-s2 px-2 py-0.5 text-xs font-semibold tabular-nums text-fg2"
	// AsksToastClass is the Undo toast, pinned above the bottom edge.
	AsksToastClass = "asks-toast fixed inset-x-4 bottom-[calc(1rem+env(safe-area-inset-bottom))] z-40 mx-auto flex max-w-sm items-center gap-3 rounded-xl bg-s3 px-4 py-2 text-sm text-fg shadow-lg ring-1 ring-inset ring-edge"
)

// AsksKindBadge is a card's kind badge: the old group header, now a small
// pill on each card. kind is the badge's key (data-asks-badge), label its
// words.
func AsksKindBadge(kind, label string) string {
	cls := "bg-s2 text-fg2"
	switch kind {
	case "question":
		cls = "bg-accent/15 text-fg"
	case "waiting":
		cls = "bg-st-warning-bg text-st-warning-fg"
	case "task":
		cls = "bg-st-progress-bg text-st-progress-fg"
	case "answered":
		cls = "bg-st-complete-bg text-st-complete-fg"
	}
	return `<span class="inline-flex shrink-0 items-center rounded-full px-2 py-0.5 text-[11px] font-semibold uppercase tracking-wide ` + cls +
		`" data-asks-badge="` + html.EscapeString(kind) + `">` + html.EscapeString(label) + `</span>`
}

// AsksCardChevron is the head's expand mark: it turns when the card opens.
const AsksCardChevron = `<svg class="asks-chevron h-4 w-4 shrink-0 text-muted" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><path d="m6 9 6 6 6-6"/></svg>`

// AsksOpenCountHTML is the page header's open counter. The feed's script keeps
// it current as cards leave and as the live refresh redraws the feed.
func AsksOpenCountHTML(n int) string {
	return `<span class="` + AsksCountClass + `" data-asks-open-count aria-live="polite">` + strconv.Itoa(n) + ` open</span>`
}

// AsksAllClearHTML is the Open feed's empty state: calm, one line of words.
// Its testid and sentence are the old empty line's, so a reader (or a test)
// that looked for "Nothing is waiting on you." still finds it.
const AsksAllClearHTML = `<li class="flex flex-col items-center gap-2 rounded-2xl px-4 py-10 text-center" data-testid="attention-empty" data-asks-empty>` +
	`<svg class="h-8 w-8 text-st-complete-fg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><path d="M20 6 9 17l-5-5"/></svg>` +
	`<p class="text-base font-semibold text-fg">All clear</p><p class="text-sm text-fg2">Nothing is waiting on you.</p></li>`

// AsksToastHTML is the page's one toast: Dismiss's Undo (the feed's script
// fills and shows it).
const AsksToastHTML = `<div id="asks-toast" class="` + AsksToastClass + `" role="status" aria-live="polite" hidden data-asks-toast>` +
	`<span class="min-w-0 flex-1" data-asks-toast-text></span>` +
	`<button type="button" class="press min-h-[44px] shrink-0 rounded-lg px-3 text-sm font-semibold text-accent hover:bg-s2" data-asks-undo>Undo</button></div>`

// asksFeedScriptSrc is the feed's behaviour, a static file (static_script.go).
var asksFeedScriptSrc = staticJS("asks-feed", asksFeedJS)

// AsksFeedScriptHTML is the feed script's tag, once per Asks page (and on an
// ask page, whose card shares the component).
func AsksFeedScriptHTML() string { return nodeString(staticScript(asksFeedScriptSrc)) }

// AsksSwipeMinPx is the least horizontal travel that switches a tab (the
// larger of it and AsksSwipeMinFrac of the feed's width).
const (
	AsksSwipeMinPx   = 60
	AsksSwipeMinFrac = 0.18
	// AsksSwipeLockPx is how far a finger moves before the gesture is locked
	// to one axis; AsksSwipeRatio how much more sideways than vertical it
	// must be to lock horizontal. A vertical lock never switches a tab.
	AsksSwipeLockPx = 10
	AsksSwipeRatio  = 1.2
	// AsksSwipeEdgePx is the sidebar's open-swipe edge (components.go
	// SB_EDGE): a gesture starting inside it is the sidebar's, never a tab's.
	AsksSwipeEdgePx = 28
	// AsksUndoMs is how long a Dismiss can be undone before it is sent.
	AsksUndoMs = 5000
)

// asksFeedJS: delegated on document, bound once per window, so it works on a
// boosted navigation into the page and on cards a live refresh drew.
//
//   - EXPAND: a click on a card's head, or anywhere on the card outside its
//     controls (and not ending a text selection), toggles data-expanded. Which
//     cards are open outlives the live refresh (by id).
//   - ANSWER: an answer form in the Open feed is posted by fetch (the same
//     route, the same fields a plain submit sends); the card collapses out,
//     the next card takes focus. A refusal puts the card back and says why. A
//     step-up form (data-step-up-form) is left to its confirm dialog.
//   - DISMISS: the card collapses out at once and the Undo toast shows; the
//     /cancel POST is sent when the toast lapses (AsksUndoMs), or at once when
//     the page is left (keepalive). No new route: Undo simply never sends it.
//   - KEYBOARD (desktop): j/k or ↓/↑ (↓/↑ only from a card) move focus between
//     cards, Enter expands, a focuses the answer box, d dictates, Esc
//     collapses (or leaves the answer box for its card).
//   - SWIPE: a horizontal swipe on the feed area switches tabs — locked to an
//     axis after AsksSwipeLockPx, so a vertical scroll never does; ignored when
//     it starts in a field, the answer box, media, anything that scrolls
//     sideways, the sidebar's edge, or with text selected.
//   - TAB SLIDE: every tab change slides the new panel in from the side it
//     came from (data-asks-slide; CSS).
var asksFeedJS = `(function () {
  if (window.__asksFeedInit) return;
  window.__asksFeedInit = true;
  var CARD = '[data-asks-card]';
  var CONTROLS = 'a,button,input,textarea,select,label,summary,[contenteditable],[data-asks-media],[data-asks-controls]';
  var expanded = {}, pending = null, lastTab = -1;
  function reduced() { try { return window.matchMedia('(prefers-reduced-motion: reduce)').matches; } catch (e) { return false; } }
  function feed() { return document.getElementById('attention-items'); }
  function cardsOf(list) {
    return list ? Array.prototype.filter.call(list.querySelectorAll(CARD + ', [data-attention-episode]'), function (c) {
      return !c.hasAttribute('data-asks-leaving') && c.offsetParent !== null;
    }) : [];
  }
  function visibleList() {
    var p = document.querySelector('[data-asks-tabs] [role="tabpanel"]:not([hidden])');
    return p ? (p.querySelector('[data-asks-feed]') || p) : document.querySelector('[data-asks-page-card]');
  }
  function setExpanded(card, on) {
    card.setAttribute('data-expanded', on ? 'true' : 'false');
    var t = card.querySelector('[data-asks-toggle]');
    if (t) t.setAttribute('aria-expanded', on ? 'true' : 'false');
    if (card.id) { if (on) expanded[card.id] = true; else delete expanded[card.id]; }
  }
  function count() {
    var n = cardsOf(feed()).length, el = document.querySelector('[data-asks-open-count]');
    if (el) el.textContent = n + ' open';
    var list = feed();
    if (list && n === 0 && !list.querySelector('[data-asks-empty]') && !list.querySelector('[data-asks-card]')) {
      var tpl = document.getElementById('asks-empty-tpl');
      if (tpl) list.appendChild(tpl.content.cloneNode(true));
    }
  }
  function focusCard(c) {
    if (!c) return;
    try { c.focus({ preventScroll: true }); } catch (e) { c.focus(); }
    try { c.scrollIntoView({ block: 'nearest', behavior: reduced() ? 'auto' : 'smooth' }); } catch (e) {}
  }
  // leave: the card collapses out (height, then gone), the next one moves up.
  function leave(card, done) {
    var list = card.parentElement, cards = cardsOf(list), i = cards.indexOf(card);
    var next = cards[i + 1] || cards[i - 1] || null;
    card.setAttribute('data-asks-leaving', '');
    var hadFocus = card.contains(document.activeElement);
    var finish = function () {
      // Put back meanwhile (a refusal, Undo): nothing to finish.
      if (!card.hasAttribute('data-asks-leaving')) return;
      if (done) done(); else card.remove();
      count();
      // By id: a live refresh may have re-drawn the feed meanwhile, and the
      // node next named is then no longer on the page.
      var to = next && (next.isConnected ? next : (next.id && document.getElementById(next.id)));
      if (hadFocus || !document.activeElement || document.activeElement === document.body) focusCard(to);
    };
    if (reduced()) { card.style.display = 'none'; finish(); return next; }
    card.style.height = card.offsetHeight + 'px';
    void card.offsetHeight;
    card.classList.add('asks-leave');
    var ended = false, end = function () { if (ended) return; ended = true; finish(); };
    card.addEventListener('transitionend', function (e) { if (e.target === card && e.propertyName === 'height') end(); });
    setTimeout(end, 400);
    return next;
  }
  function restore(card, list, before) {
    card.removeAttribute('data-asks-leaving');
    card.classList.remove('asks-leave');
    card.style.height = ''; card.style.display = '';
    var empty = list.querySelector('[data-asks-empty]');
    if (empty) empty.remove();
    if (before && before.parentElement === list) list.insertBefore(card, before); else if (!card.parentElement) list.appendChild(card);
    if (!reduced()) { card.classList.add('asks-enter'); setTimeout(function () { card.classList.remove('asks-enter'); }, 400); }
    count();
    focusCard(card);
  }
  function toast(text, undo) {
    var t = document.getElementById('asks-toast');
    if (!t) return;
    t.querySelector('[data-asks-toast-text]').textContent = text;
    var b = t.querySelector('[data-asks-undo]');
    b.hidden = !undo;
    b.onclick = undo || null;
    t.hidden = false;
    clearTimeout(t._h);
    t._h = setTimeout(function () { t.hidden = true; }, undo ? ` + strconv.Itoa(AsksUndoMs) + ` : 4000);
  }
  function post(form, body, keep) {
    // redirect: 'manual' — a recorded answer or dismissal answers 303 to the
    // queue, which need not be fetched; a refusal answers 4xx/5xx with its page.
    return fetch(form.getAttribute('action'), { method: 'POST', body: body, credentials: 'same-origin', keepalive: !!keep, redirect: 'manual',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' } }).then(function (r) {
      return { ok: r.type === 'opaqueredirect' || (r.status >= 200 && r.status < 400), r: r };
    });
  }
  // A refusal page's sentence (problemNode's role=alert).
  function refusal(r) {
    return r.text().then(function (h) {
      var d = new DOMParser().parseFromString(h, 'text/html'), a = d.querySelector('[role="alert"]');
      return (a && a.textContent.trim()) || 'That could not be recorded.';
    }).catch(function () { return 'That could not be recorded.'; });
  }
  function flush(keep) {
    if (!pending) return;
    var p = pending; pending = null;
    clearTimeout(p.timer);
    var t = document.getElementById('asks-toast'); if (t) t.hidden = true;
    post(p.form, new URLSearchParams(new FormData(p.form)), keep).then(function (res) {
      if (!res.ok) return refusal(res.r).then(function (why) { restore(p.card, p.list, p.before); toast(why, null); });
      p.card.remove();
    }).catch(function () { restore(p.card, p.list, p.before); toast('Dismiss could not be sent.', null); });
  }
  window.addEventListener('pagehide', function () { flush(true); });
  document.addEventListener('visibilitychange', function () { if (document.visibilityState === 'hidden') flush(true); });
  document.addEventListener('htmx:beforeRequest', function () { flush(true); });

  document.addEventListener('submit', function (e) {
    var form = e.target, list = feed();
    if (!form || !list || !list.contains(form) || form.hasAttribute('data-step-up-form')) return;
    var card = form.closest(CARD);
    if (!card) return;
    var action = form.getAttribute('action') || '';
    if (/\/cancel$/.test(action)) {
      e.preventDefault(); e.stopPropagation();
      flush(false);
      var before = card.nextElementSibling;
      leave(card, function () { card.style.display = 'none'; });
      pending = { form: form, card: card, list: list, before: before };
      pending.timer = setTimeout(function () { flush(false); }, ` + strconv.Itoa(AsksUndoMs) + `);
      var mine = pending;
      toast('Dismissed.', function () {
        if (pending !== mine) return;
        clearTimeout(mine.timer); pending = null;
        document.getElementById('asks-toast').hidden = true;
        restore(card, list, before);
      });
      return;
    }
    if (!/\/answer$/.test(action)) return;
    var fd = new FormData(form, e.submitter || undefined);
    var opt = (fd.get('option') || '').toString().trim(), txt = (fd.get('answer') || '').toString().trim();
    e.preventDefault(); e.stopPropagation();
    if (!opt && !txt) { var box = form.querySelector('textarea'); if (box) box.focus(); return; }
    if (opt) fd.delete('answer'); else fd.delete('option');
    var before = card.nextElementSibling;
    leave(card, function () { card.style.display = 'none'; });
    post(form, new URLSearchParams(fd), false).then(function (res) {
      if (!res.ok) return refusal(res.r).then(function (why) { restore(card, list, before); toast(why, null); });
      card.remove(); count();
    }).catch(function () { restore(card, list, before); toast('That answer could not be sent.', null); });
  }, true);

  document.addEventListener('click', function (e) {
    var t = e.target;
    var chip = t && t.closest && t.closest('[data-asks-suggestion]');
    if (chip) {
      var c = chip.closest(CARD), box = c && c.querySelector('textarea');
      if (!box) return;
      e.preventDefault();
      var v = chip.getAttribute('data-text') || '';
      box.value = box.value.trim() ? box.value.replace(/\s+$/, '') + '\n' + v : v;
      try { box.dispatchEvent(new Event('input', { bubbles: true })); } catch (err) {}
      box.focus();
      return;
    }
    var card = t && t.closest && t.closest(CARD);
    if (!card || card.hasAttribute('data-asks-fixed')) return;
    if (!t.closest('[data-asks-toggle]')) {
      if (t.closest(CONTROLS)) return;
      var sel = window.getSelection && String(window.getSelection());
      if (sel) return;
    }
    setExpanded(card, card.getAttribute('data-expanded') !== 'true');
  });

  document.addEventListener('keydown', function (e) {
    if (e.defaultPrevented || e.ctrlKey || e.metaKey || e.altKey) return;
    var dlg = document.querySelector('dialog[open]');
    if (dlg) return;
    var t = e.target, typing = t && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName));
    var list = visibleList();
    if (!list) return;
    var cur = t && t.closest ? t.closest(CARD) : null;
    if (cur && !list.contains(cur)) cur = null;
    if (e.key === 'Escape') {
      if (!cur) return;
      if (typing) { e.preventDefault(); focusCard(cur); return; }
      if (cur.getAttribute('data-expanded') === 'true' && !cur.hasAttribute('data-asks-fixed')) { e.preventDefault(); setExpanded(cur, false); focusCard(cur); }
      return;
    }
    if (typing) return;
    var cards = cardsOf(list), i = cards.indexOf(cur), onCard = cur && t === cur;
    var k = e.key;
    if (k === 'j' || (k === 'ArrowDown' && onCard)) { e.preventDefault(); focusCard(cards[i < 0 ? 0 : Math.min(i + 1, cards.length - 1)]); return; }
    if (k === 'k' || (k === 'ArrowUp' && onCard)) { e.preventDefault(); focusCard(cards[i < 0 ? 0 : Math.max(i - 1, 0)]); return; }
    if (!cur) return;
    if (k === 'Enter' && onCard && !cur.hasAttribute('data-asks-fixed')) { e.preventDefault(); setExpanded(cur, cur.getAttribute('data-expanded') !== 'true'); return; }
    if (k === 'a') { var box = cur.querySelector('textarea'); if (box) { e.preventDefault(); box.focus(); } return; }
    if (k === 'd') { var mic = cur.querySelector('button[data-dictate]'); if (mic && !mic.hidden) { e.preventDefault(); mic.click(); } return; }
  });

  // THE LIVE REFRESH (AttentionLiveScript) re-draws the Open feed and then
  // dispatches the reveal event on it: keep what the owner had (open cards,
  // the focused card, a pending dismiss), and let new cards enter.
  var known = null;
  function ids(list) { var o = {}; Array.prototype.forEach.call(list.querySelectorAll(CARD), function (c) { if (c.id) o[c.id] = true; }); return o; }
  document.addEventListener('` + DictateRevealEvent + `', function (e) {
    var list = feed();
    if (!list || e.target !== list) return;
    var focusId = list.getAttribute('data-asks-focus') || '';
    Array.prototype.forEach.call(list.querySelectorAll(CARD), function (c) {
      if (pending && c.id && c.id === pending.card.id) { c.remove(); return; }
      if (expanded[c.id]) { c.classList.add('asks-instant'); setExpanded(c, true); }
      if (known && !known[c.id] && !reduced()) { c.classList.add('asks-enter'); setTimeout(function () { c.classList.remove('asks-enter'); }, 400); }
    });
    requestAnimationFrame(function () { Array.prototype.forEach.call(list.querySelectorAll('.asks-instant'), function (c) { c.classList.remove('asks-instant'); }); });
    known = ids(list);
    if (focusId) { var f = document.getElementById(focusId); if (f && list.contains(f)) focusCard(f); }
    count();
  });
  // Which card had focus when a refresh replaced it (focusout fires first).
  document.addEventListener('focusin', function (e) {
    var list = feed(), c = e.target && e.target.closest && e.target.closest(CARD);
    if (list && c && list.contains(c) && e.target === c) list.setAttribute('data-asks-focus', c.id); else if (list && e.target !== list) list.removeAttribute('data-asks-focus');
  });

  // THE TAB SLIDE: the panel a tab change shows slides in from its side.
  function tabs() { var l = document.getElementById('asks-tabs'); return l ? Array.prototype.slice.call(l.querySelectorAll('[role="tab"]')) : []; }
  function selIndex() { var ts = tabs(); for (var i = 0; i < ts.length; i++) if (ts[i].getAttribute('aria-selected') === 'true') return i; return -1; }
  function slide() {
    var i = selIndex();
    if (i < 0) return;
    if (lastTab >= 0 && i !== lastTab && !reduced()) {
      var p = document.getElementById(tabs()[i].getAttribute('aria-controls'));
      if (p) {
        p.setAttribute('data-asks-slide', i > lastTab ? 'left' : 'right');
        setTimeout(function () { p.removeAttribute('data-asks-slide'); }, 400);
      }
    }
    lastTab = i;
  }
  function watchTabs() { lastTab = selIndex(); }
  document.addEventListener('click', function (e) { if (e.target && e.target.closest && e.target.closest('#asks-tabs [role="tab"]')) setTimeout(slide, 0); });
  document.addEventListener('keydown', function (e) { if (e.target && e.target.closest && e.target.closest('#asks-tabs [role="tab"]')) setTimeout(slide, 0); });
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', function () { watchTabs(); known = feed() ? ids(feed()) : null; });
  else { watchTabs(); known = feed() ? ids(feed()) : null; }
  document.addEventListener('htmx:load', function () { watchTabs(); known = feed() ? ids(feed()) : null; });

  // THE SWIPE: sideways on the feed area moves one tab, in its direction.
  var sw = null;
  function ownsSideways(t) {
    for (var el = t; el && el.nodeType === 1 && el !== document.body; el = el.parentElement) {
      if (el.hasAttribute('data-own-swipe')) return true;
      if (el.scrollWidth > el.clientWidth + 1) { var ox = getComputedStyle(el).overflowX; if (ox === 'auto' || ox === 'scroll') return true; }
    }
    return false;
  }
  document.addEventListener('touchstart', function (e) {
    sw = null;
    if (!e.touches || e.touches.length !== 1) return;
    var t = e.target, area = t && t.closest && t.closest('[data-asks-swipe]');
    if (!area) return;
    var x = e.touches[0].clientX, y = e.touches[0].clientY;
    if (x <= ` + strconv.Itoa(AsksSwipeEdgePx) + `) return;
    if (t.closest('input,textarea,select,[contenteditable],[data-asks-media],[data-asks-answer-row],[data-own-swipe]')) return;
    if (ownsSideways(t)) return;
    var s = window.getSelection && String(window.getSelection());
    if (s) return;
    sw = { x: x, y: y, lock: '', area: area };
  }, { passive: true });
  document.addEventListener('touchmove', function (e) {
    if (!sw || !e.touches || !e.touches.length) return;
    var dx = e.touches[0].clientX - sw.x, dy = e.touches[0].clientY - sw.y;
    if (!sw.lock && (Math.abs(dx) > ` + strconv.Itoa(AsksSwipeLockPx) + ` || Math.abs(dy) > ` + strconv.Itoa(AsksSwipeLockPx) + `)) {
      sw.lock = Math.abs(dx) > Math.abs(dy) * ` + strconv.FormatFloat(AsksSwipeRatio, 'f', -1, 64) + ` ? 'x' : 'y';
    }
    sw.dx = dx;
  }, { passive: true });
  function endSwipe() {
    var s = sw; sw = null;
    if (!s || s.lock !== 'x') return;
    var w = s.area.clientWidth || window.innerWidth;
    if (Math.abs(s.dx || 0) < Math.max(` + strconv.Itoa(AsksSwipeMinPx) + `, w * ` + strconv.FormatFloat(AsksSwipeMinFrac, 'f', -1, 64) + `)) return;
    var ts = tabs(), i = selIndex(), n = i + (s.dx < 0 ? 1 : -1);
    if (i < 0 || n < 0 || n >= ts.length) return;
    ts[n].click();
  }
  document.addEventListener('touchend', endSwipe, { passive: true });
  document.addEventListener('touchcancel', function () { sw = null; }, { passive: true });
})();
`
