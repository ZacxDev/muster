package ui

import (
	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// BUBBLE CLAMP: a chat bubble taller than about fifteen lines is drawn clamped,
// in one of two directions, with a "Show more" control. A tap on the closed
// bubble or on "Show more" animates it open to its full height.
//
//   - HEAD-FIRST (the default: the reader's own messages, kickoffs): its first
//     fifteen lines, fading out at the BOTTOM, and "Show more" UNDER it.
//   - TAIL-FIRST (data-clamp-from="tail": an agent's reply): its LAST fifteen
//     lines, fading in at the TOP, and "Show more" ABOVE it. An agent's reply
//     usually ends with the thing that matters — the answer, the question, the
//     next step — so the end is what is shown. Opening it grows the earlier
//     text UPWARD and the end stays where it was on screen: the log's scroll is
//     moved by exactly what the bubble grew, on every frame (animate). That
//     holds in a SHORT transcript too, because a log's messages sit at its
//     bottom (input.css, "Chat logs are bottom-aligned"): the log can always
//     scroll by what the bubble grew.
//
// 🔴 THE DIRECTION IS DECLARED BY THE SERVER, NEVER INFERRED FROM THE TEXT.
// chatBubbleOf sets data-clamp-from="tail" from the message ROLE (clampFrom), so
// the agent chat page, the chief panel and the mirrored Claude and opencode
// views all draw a reply the same way; this script only reads the attribute.
//
// 🔴 A TAP ON THE BUBBLE ONLY EVER OPENS IT. A tap on an OPEN bubble does
// nothing to the clamp — the reader is reading it, and a stray tap must not
// fold the text away under their thumb. The ONLY way to close it is the
// control, which reads "Show less" while the bubble is open: under a head-first
// bubble, above a tail-first one. Closing does NOT anchor anything of its own:
// with the log scrolled away from its end, the bubble shrinks from its bottom
// and its control (its top) stays where it is; within stick-to-bottom's reach
// of the end (80px), the log is kept at its end instead, so the bubble's end
// stays and its top — the control with it — moves DOWN by what it shrank.
//
// 🔴 ONE SCRIPT FOR EVERY CLAMPED SURFACE, BECAUSE THE BUBBLES ARE ONE
// COMPONENT. It acts on any [data-bubble] element in the document — the mirrored
// view's (MirrorTranscript.Ops), the agent chat page's server-rendered
// transcript (chatBubbleOf) and its own live user bubbles, and the chief
// panel's (chiefPanelBody includes this script) — wherever and whenever it
// appears: a MutationObserver on <body> (which survives the chat-log refresh)
// finds new ones, and a ResizeObserver per bubble re-measures it when its width
// or content changes, so a bubble that is markdown-converted after insertion is
// measured when it actually has a size.
//
// ⚠ A STREAMING ASSISTANT REPLY IS NOT CLAMPED, deliberately: the agent chat's
// live reply bubbles carry no data-bubble (agentChatScript's ensureText), so
// text arriving at the bottom is never hidden while it streams. The persisted
// copy the chat-log refresh swaps in afterwards is clamped like any other.
//
// 🔴 A TAIL-FIRST BUBBLE IS SCROLLED TO ITS END INSIDE ITS OWN BOX. It is
// overflow:hidden, and its scrollTop is set to the end whenever it is measured
// closed and on every frame it moves (pin), in the same task that clamped it —
// so the first paint already shows the end, and nothing moves after it.
//
// 🔴 STICK-TO-BOTTOM: a bubble opened while its log is scrolled to the end keeps
// the log at the end on every animation frame and after it, so expanding the
// newest message never leaves the reader above the bottom. The page scripts'
// own "at the end" state follows, because a scroll to the bottom IS the end. A
// tail-first bubble's end-in-place covers it: keeping its end where it was keeps
// everything under it, the log's end included, where it was.
//
// 🔴 AN OPENED MESSAGE STAYS OPEN ACROSS A REDRAW. The agent chat page swaps
// #chat-log's whole content on sse:chat.reply, task:changed and agent.changed
// (chatLogTrigger), so every bubble is a NEW element after each swap. A bubble
// carrying data-msg-id (a persisted message's row id, chatBubble) is remembered
// by that id when it finishes opening or closing, and a new bubble with a
// remembered-open id is drawn open at once — no animation, no stick-to-bottom.
// A bubble with no id (a mirrored one, a live one) is not remembered.
//
// 🔴 A TAP ON A CONTROL INSIDE A BUBBLE IS THAT CONTROL'S, NOT THE BUBBLE'S. A
// tap whose target is inside a link, a <summary>/<details> (a fenced code
// block is a collapsed <details>), a button, a form field or anything marked
// [data-no-clamp] within the bubble never toggles the bubble.
//
// It is re-executable: the observers and the delegated handler are bound once
// per document, on window.muClamp, and a re-run rescans.
func bubbleClampScript() g.Node {
	return Script(g.Raw(`
(function () {
  if (window.muClamp) { window.muClamp.scan(document); return; }
  var LINES = 15;       // "about 15 lines", both directions
  // What a tap inside a bubble belongs to instead of the bubble.
  var CONTROLS = 'a, summary, details, button, input, textarea, select, label, [data-no-clamp]';
  var reduce = window.matchMedia && matchMedia('(prefers-reduced-motion: reduce)');
  var seen = new WeakSet();
  // The data-msg-id of every bubble the reader left open, for this document.
  var opened = new Set();
  function idOf(el) { return el.getAttribute('data-msg-id') || ''; }
  // remember records el's settled state under its id (none = not remembered).
  function remember(el) {
    var id = idOf(el);
    if (!id) return;
    if (el.getAttribute('data-clamp') === 'open') opened.add(id); else opened.delete(id);
  }
  // tail: the server drew el tail-first (clampFrom) — its END is what shows.
  function tail(el) { return el.getAttribute('data-clamp-from') === 'tail'; }
  // pin scrolls a tail-first bubble's own box to its end, so its last lines
  // are the ones its clamp shows. el._clampOff marks a closed bubble the
  // READER moved off its end (a code block opened mid-reply, see 'toggle'):
  // a re-measure (its width changed, its content was redrawn) leaves that
  // bubble's scroll alone instead of snapping it back.
  function pin(el) { if (tail(el)) { el.scrollTop = el.scrollHeight; el._clampOff = false; } }

  // The clamped height: LINES of the bubble's own line-height, plus its padding.
  function limit(el) {
    var s = getComputedStyle(el);
    var lh = parseFloat(s.lineHeight);
    if (!(lh > 0)) lh = parseFloat(s.fontSize) * 1.25;
    return Math.round(lh * LINES + parseFloat(s.paddingTop) + parseFloat(s.paddingBottom));
  }
  function logOf(el) { return el.closest('#mv-log, #chat-log'); }
  function atBottom(log) { return !!log && log.scrollHeight - log.scrollTop - log.clientHeight < 80; }
  // A bubble's control is its sibling: ABOVE a tail-first one (marked
  // data-clamp-toggle="tail"), UNDER a head-first one (data-clamp-toggle="").
  function toggleOf(el) {
    var n = tail(el) ? el.previousElementSibling : el.nextElementSibling;
    return n && n.getAttribute('data-clamp-toggle') === (tail(el) ? 'tail' : '') ? n : null;
  }
  function bubbleOf(b) {
    var n = b.getAttribute('data-clamp-toggle') === 'tail' ? b.nextElementSibling : b.previousElementSibling;
    return n && n.hasAttribute('data-clamp') ? n : null;
  }

  function setToggle(el, open) {
    var b = toggleOf(el);
    if (!b) {
      var t = tail(el);
      b = document.createElement('button');
      b.type = 'button';
      b.setAttribute('data-clamp-toggle', t ? 'tail' : '');
      // 44px: a thumb target, like every control (e2e/tests/layout.spec.ts).
      // Pulled toward its bubble: down onto a tail-first one, up under a head-first one.
      b.className = 'press ' + (t ? '-mb-2' : '-mt-2') + ' inline-flex min-h-[44px] items-center px-2 text-xs font-medium text-accent ' +
        (el.getAttribute('data-bubble') === 'user' ? 'self-end' : 'self-start');
      if (t) el.before(b); else el.after(b);
    }
    b.textContent = open ? 'Show less' : 'Show more';
    b.setAttribute('aria-expanded', open ? 'true' : 'false');
  }

  // measure decides whether el is clamped. An open (or moving) bubble is left
  // alone: the reader chose to see all of it.
  function measure(el) {
    var st = el.getAttribute('data-clamp');
    if (st === 'open' || st === 'moving') return;
    if (!el.isConnected || el.clientWidth === 0) return; // no box yet: measured when it gets one
    var max = limit(el);
    // A line of slack, so a bubble a few pixels over is not clamped to hide half a line.
    var need = el.scrollHeight > max + parseFloat(getComputedStyle(el).lineHeight || '20');
    if (need && opened.has(idOf(el))) {
      // A redrawn copy of a message the reader opened: open at once, unanimated.
      el.style.maxHeight = 'none';
      el.setAttribute('data-clamp', 'open');
      setToggle(el, true);
    } else if (need) {
      el.style.maxHeight = max + 'px';
      el.setAttribute('data-clamp', 'closed');
      setToggle(el, false);
      if (st !== 'closed' || !el._clampOff) pin(el);
    } else if (st === 'closed') {
      el.removeAttribute('data-clamp');
      el.style.maxHeight = '';
      var b = toggleOf(el); if (b) b.remove();
    }
  }

  var ro = typeof ResizeObserver !== 'undefined' ? new ResizeObserver(function (es) { es.forEach(function (e) { measure(e.target); }); }) : null;
  function track(el) {
    if (seen.has(el)) { measure(el); return; }
    seen.add(el);
    if (ro) ro.observe(el);
    measure(el);
  }
  function scan(root) {
    if (!root || !root.querySelectorAll) return;
    if (root.matches && root.matches('[data-bubble]')) track(root);
    root.querySelectorAll('[data-bubble]').forEach(track);
  }

  // animate moves el's max-height from its current height to target (px, or
  // null = its full height) and calls done after. While it moves, the log is
  // held one of two ways:
  //   - o.anchorEnd (a tail-first bubble opening): the log's scroll follows
  //     the bubble's growth, so the bubble's END stays where it was on screen
  //     and the earlier text opens upward;
  //   - otherwise, stick-to-bottom: kept at its end if it was there.
  // o.noFollow moves the log neither way (the message index's jump).
  function animate(el, target, done, o) {
    o = o || {};
    var log = logOf(el);
    var anchor = !o.noFollow && !!o.anchorEnd && !!log;
    var stick = !o.noFollow && !anchor && atBottom(log);
    var to = target == null ? el.scrollHeight : target;
    var h0 = el.getBoundingClientRect().height, s0 = log ? log.scrollTop : 0;
    el.setAttribute('data-clamp', 'moving');
    el.style.maxHeight = h0 + 'px';
    void el.offsetHeight; // commit the start height before the transition
    var finished = false, frame = 0;
    function follow() {
      pin(el);
      if (anchor) log.scrollTop = s0 + (el.getBoundingClientRect().height - h0);
      else if (stick && log) log.scrollTop = log.scrollHeight;
    }
    function finish() {
      if (finished) return;
      finished = true;
      cancelAnimationFrame(frame);
      el.removeEventListener('transitionend', onEnd);
      done();
      remember(el);
      follow();
    }
    function onEnd(e) { if (e.target === el && e.propertyName === 'max-height') finish(); }
    function tick() { follow(); if (!finished) frame = requestAnimationFrame(tick); }
    if (reduce && reduce.matches) { el.style.maxHeight = to + 'px'; finish(); return; }
    el.addEventListener('transitionend', onEnd);
    el.style.maxHeight = to + 'px';
    frame = requestAnimationFrame(tick);
    setTimeout(finish, 600); // a transition that never ends (display:none, no change) still finishes
  }

  // expand opens el. opts.follow === false skips stick-to-bottom and the
  // end-in-place: the message index opens a bubble it is jumping TO, not one
  // the reader is looking at.
  function expand(el, opts) {
    if (el.getAttribute('data-clamp') !== 'closed') return;
    setToggle(el, true);
    animate(el, null, function () { el.style.maxHeight = 'none'; el.setAttribute('data-clamp', 'open'); },
      { noFollow: !!(opts && opts.follow === false), anchorEnd: tail(el) });
  }
  function collapse(el) {
    if (el.getAttribute('data-clamp') !== 'open') return;
    setToggle(el, false);
    var max = limit(el);
    animate(el, max, function () { el.style.maxHeight = max + 'px'; el.setAttribute('data-clamp', 'closed'); });
  }
  // toggle is the CONTROL's action only: Show more opens, Show less closes.
  function toggle(el) {
    var st = el.getAttribute('data-clamp');
    if (st === 'closed') expand(el); else if (st === 'open') collapse(el);
  }

  document.addEventListener('click', function (e) {
    var t = e.target;
    if (!t || !t.closest) return;
    var b = t.closest('[data-clamp-toggle]');
    if (b) {
      var bub = bubbleOf(b);
      if (bub) toggle(bub);
      return;
    }
    // A tap on the bubble itself — but not on a control inside it (see the
    // comment above bubbleClampScript), and not the end of a text selection.
    // It OPENS a closed bubble and never collapses an open one: expand is a
    // no-op unless the bubble is closed.
    var el = t.closest('[data-clamp]');
    if (!el) return;
    var ctl = t.closest(CONTROLS);
    // A tap on a code block's summary in the MIDDLE of a closed tail-first
    // reply: marked off its end NOW, before the block opens — the re-measure
    // its new width triggers can run before the 'toggle' event does.
    var sum = t.closest('summary');
    if (sum && el.contains(sum) && tail(el) && el.getAttribute('data-clamp') === 'closed' &&
        sum.parentElement && sum.parentElement.tagName === 'DETAILS' && !last(sum.parentElement, el)) el._clampOff = true;
    if (ctl && el.contains(ctl)) return;
    var sel = window.getSelection && window.getSelection();
    if (sel && !sel.isCollapsed && el.contains(sel.anchorNode)) return;
    expand(el);
  });

  // last: nothing in host follows d — d holds the bubble's end.
  function last(d, host) {
    for (var n = d; n && n !== host; n = n.parentElement) if (n.nextElementSibling) return false;
    return true;
  }
  // A code block (a <details>) opened inside a closed tail-first bubble. One
  // in the MIDDLE grows below its summary and the bubble's own scroll is left
  // alone, so the summary just tapped stays exactly where it was. Only the
  // LAST block is re-pinned — it holds the end, and the code it opened would
  // otherwise be under the clamp's edge — and then never past its own summary:
  // a block taller than the clamp opens with its summary at the bubble's top.
  // 'toggle' does not bubble: captured.
  document.addEventListener('toggle', function (e) {
    var d = e.target;
    var host = d && d.closest && d.closest('[data-clamp="closed"]');
    if (!host || !tail(host)) return;
    if (!last(d, host)) { host._clampOff = true; return; }
    pin(host);
    var s = d.querySelector('summary');
    var over = s ? host.getBoundingClientRect().top - s.getBoundingClientRect().top : 0;
    if (over > 0) { host.scrollTop -= over; host._clampOff = true; }
  }, true);

  new MutationObserver(function (ms) {
    ms.forEach(function (m) {
      m.addedNodes.forEach(function (n) { if (n.nodeType === 1) scan(n); });
      // Content replaced INSIDE a bubble (the agent chat converts markdown in place).
      var host = m.target && m.target.nodeType === 1 && m.target.closest && m.target.closest('[data-bubble]');
      if (host) measure(host);
    });
  }).observe(document.body, { childList: true, subtree: true });

  window.muClamp = { scan: scan, measure: measure, expand: expand, collapse: collapse };
  scan(document);
})();
`))
}

// clampFrom is the data-clamp-from a bubble of this role is drawn with: "tail"
// for an agent's reply (its END is what a clamped bubble shows), and nothing —
// head-first — for everything else (the reader's own messages, kickoffs).
// chatBubbleOf is its one caller on the server; agentChatScript's live bubble
// spells the same rule in JS.
func clampFrom(role string) g.Node {
	return g.If(role == "assistant", g.Attr("data-clamp-from", "tail"))
}
