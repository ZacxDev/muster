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
	// AsksCardErrorClass is a card's refusal line (AsksCardErrorHTML), in its
	// error slot.
	AsksCardErrorClass = "rounded-lg bg-st-error-bg px-3 py-2 text-sm text-st-error-fg ring-1 ring-inset ring-st-error-fg/40"
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

// AsksCardToggleLabel is a card head's accessible NAME: what the toggle does.
// Its visible words (who, how long) are its description (aria-describedby),
// so the head never reads as another control saying the card's words.
const AsksCardToggleLabel = "Details"

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

// AsksDraftsWaitHTML is the Open feed's empty state while reply drafts wait
// (n > 0): NOT "All clear" — something is still pending, on the Drafts tab.
// Same calm layout as AsksAllClearHTML, no check mark, and a link to the tab.
// Its count is its own element (data-asks-drafts-n), so the feed's script can
// draw it from its template (asks-drafts-wait-tpl) with the live number.
func AsksDraftsWaitHTML(n int) string {
	return `<li class="flex flex-col items-center gap-2 rounded-2xl px-4 py-10 text-center" data-asks-empty="drafts">` +
		`<p class="text-base font-semibold text-fg">Nothing else is open</p>` +
		`<p class="text-sm text-fg2" data-asks-drafts-wait>` + AsksDraftsWaitWords(n) + `</p>` +
		`<a href="/attention?tab=drafts" class="` + AsksCardLinkClass + `" data-asks-drafts-link>Review drafts</a></li>`
}

// AsksDraftsWaitWords is the drafts empty state's sentence for n drafts.
func AsksDraftsWaitWords(n int) string {
	if n == 1 {
		return `<span data-asks-drafts-n>1</span> reply draft waits for your decision.`
	}
	return `<span data-asks-drafts-n>` + strconv.Itoa(n) + `</span> reply drafts wait for your decision.`
}

// AsksDraftsMarkerHTML is the Open feed's hidden note of how many reply drafts
// wait (asksPendingOf counts them; they are drawn on the Drafts tab): the
// feed's script adds it to the cards it counts, so the header's "N open"
// after a leave or a live refresh is the server's own total. "" for none.
func AsksDraftsMarkerHTML(n int) string {
	if n <= 0 {
		return ""
	}
	return `<li hidden aria-hidden="true" data-asks-drafts="` + strconv.Itoa(n) + `"></li>`
}

// AsksCardErrorHTML is the small fragment a refused card answer or dismissal
// answers with (internal/api asksCardFailed), put in the card's error slot
// ([data-asks-card-error]) by the feed's script. role=alert: a screen reader
// says it.
func AsksCardErrorHTML(text string) string {
	return `<p role="alert" class="` + AsksCardErrorClass + `" data-asks-card-refusal>` + html.EscapeString(text) + `</p>`
}

// asksFeedScriptSrc is the feed's behaviour, a static file (static_script.go).
var asksFeedScriptSrc = staticJS("asks-feed", asksFeedJS)

// AsksFeedScriptHTML is the feed script's tag. It is written once, by the Asks
// page alone (internal/api renderAsksPage, after the page's other scripts);
// the ask page (/attention/asks/<id>) draws the card always expanded and does
// not include it.
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
	// AsksSwipeMaxScale: with the page pinch-zoomed past this
	// (visualViewport.scale), a sideways drag is the browser's pan, never a
	// tab swipe.
	AsksSwipeMaxScale = 1.01
)

// AsksLeaveFallbackMs is how long a leaving card waits for its collapse's
// transitionend before it counts the collapse as played (a transition the
// browser skipped, a card a refresh detached). Above .asks-leave's 240ms.
const AsksLeaveFallbackMs = 400

// AsksCollapsingAttr marks a feed card whose leave collapse is playing; the
// Open feed's live refresh (AttentionLiveScript) is held while one is, and
// runs on AsksCollapsedEvent, which the card's feed hears when it ends.
const (
	AsksCollapsingAttr = "data-asks-collapsing"
	AsksCollapsedEvent = "asks:collapsed"
)

// AsksSentToastPrefix leads the toast an accepted answer shows: "Sent to
// <the session's name>" (the card's data-asks-who).
const AsksSentToastPrefix = "Sent to "

// A card post's refusal when the server's own fragment is not what came back
// (the feed's script: refusal), and the feed notice's lead when the card is
// gone meanwhile (refused).
const (
	AsksRefusalOffline = "Not sent: muster could not be reached. Try again."
	// AsksRefusalSignIn: a 401 — which the shell's auth gate (components.go)
	// also turns into a navigation to the sign-in.
	AsksRefusalSignIn    = "Not sent: your sign-in has lapsed. Reload the page and sign in again."
	AsksRefusalForbidden = "Not sent: muster refused it. Reload the page and try again."
	AsksRefusalOther     = "That could not be recorded. Try again"
	AsksRefusalGone      = "Your answer was not recorded"
)

// AsksFeedNoticeHTML is the Open feed's notice line: a card's post refused
// after a refresh took the card off the page (its ask closed elsewhere), so
// there is no card to say it on. Shown by the feed's script.
const AsksFeedNoticeHTML = `<p id="asks-feed-notice" role="alert" class="` + AsksCardErrorClass + `" data-asks-feed-notice hidden></p>`

// asksFeedJS: delegated on document, bound once per window, so it works on a
// boosted navigation into the page and on cards a live refresh drew.
//
//   - EXPAND: a click on a card's head, or anywhere on the card outside its
//     controls (and not ending a text selection), toggles data-expanded. Which
//     cards are open outlives the live refresh (by id).
//   - ANSWER AND DISMISS go by htmx (the card's forms carry hx-post to the
//     queue's own routes, hx-swap none; internal/api asksCardRequest answers
//     them 204 on success, or their status with a small error fragment,
//     ui.AsksCardErrorHTML). The card starts collapsing at once; on success it
//     is removed once the collapse has played (afterLeave), the next card
//     takes focus, and an answer says "Sent to <session>" in a toast
//     (window.toast, the info tone); on a refusal it comes back with
//     the fragment in its error slot. An empty answer is not sent. A step-up
//     form (data-step-up-form) has no hx-post: it is its confirm dialog's.
//   - 🔴 A CARD THAT IS LEAVING OR WAS JUST SETTLED IS NOT REDRAWN by the live
//     refresh (which replaces the feed's innerHTML from the server, where the
//     ask may still be open until the POST lands): the reveal handler drops it
//     again in the same task, before a paint (leaving / settled, by id).
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
  var expanded = {}, lastTab = -1;
  function reduced() { try { return window.matchMedia('(prefers-reduced-motion: reduce)').matches; } catch (e) { return false; } }
  function feed() { return document.getElementById('attention-items'); }
  // allCards: every card (and stalled row) of list not on its way out,
  // SHOWN OR NOT — what is counted (a hidden tab's feed still holds its
  // cards). cardsOf: those of them on screen — where focus can move.
  function allCards(list) {
    return list ? Array.prototype.filter.call(list.querySelectorAll(CARD + ', [data-attention-episode]'), function (c) {
      return !c.hasAttribute('data-asks-leaving');
    }) : [];
  }
  function cardsOf(list) {
    return allCards(list).filter(function (c) { return c.offsetParent !== null; });
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
  // count: THE ONE OPEN COUNT (internal/api asksPendingOf), as the page has
  // it now — the Open feed's cards and stalled rows that wait on the reader
  // (not leaving; a watched team ask, data-asks-uncounted, is not counted),
  // plus the reply drafts the feed's marker says wait (data-asks-drafts).
  function count() {
    var list = feed();
    if (!list) return;
    var n = allCards(list).filter(function (c) { return !c.hasAttribute('data-asks-uncounted'); }).length;
    var m = list.querySelector('[data-asks-drafts]'), d = m ? (parseInt(m.getAttribute('data-asks-drafts'), 10) || 0) : 0;
    var el = document.querySelector('[data-asks-open-count]');
    if (el) el.textContent = (n + d) + ' open';
    var empty = list.querySelector('[data-asks-empty]');
    // The empty state only when the Open feed truly holds nothing (a card on
    // its way out still counts as there until its post lands): All clear with
    // nothing pending at all, the drafts' state while drafts wait.
    if (!list.querySelector(CARD + ', [data-attention-episode]')) {
      var want = d > 0 ? 'drafts' : '';
      if (empty && (empty.getAttribute('data-asks-empty') || '') !== want) { empty.remove(); empty = null; }
      if (!empty) {
        var tpl = document.getElementById(d > 0 ? 'asks-drafts-wait-tpl' : 'asks-empty-tpl');
        if (tpl) {
          var node = tpl.content.cloneNode(true), dn = node.querySelector('[data-asks-drafts-wait]');
          if (dn) dn.innerHTML = d === 1 ? '` + AsksDraftsWaitWords(1) + `' : '` + AsksDraftsWaitWords(0) + `'.replace('>0<', '>' + d + '<');
          list.insertBefore(node, m);
        }
      }
    } else if (empty) empty.remove();
  }
  function focusCard(c) {
    if (!c) return;
    try { c.focus({ preventScroll: true }); } catch (e) { c.focus(); }
    try { c.scrollIntoView({ block: 'nearest', behavior: reduced() ? 'auto' : 'smooth' }); } catch (e) {}
  }
  // leaving: cards whose answer or dismissal is in flight; settled: cards the
  // server has answered or dismissed (kept a minute: a refresh fetched just
  // before the POST landed still lists them). Both by id.
  var leaving = {}, settled = {};
  // leave: the card collapses out (height, then hidden), the next one moves up.
  // card.__asksLeft is the collapse's end: done once it has played (or at
  // once under reduced motion), with what waits for it (settle's removal).
  function leave(card) {
    var cards = cardsOf(card.parentElement), i = cards.indexOf(card);
    var next = cards[i + 1] || cards[i - 1] || null;
    var focused = card.contains(document.activeElement);
    var q = card.querySelector('[data-attention-text]');
    card.setAttribute('data-asks-leaving', '');
    // node: the card itself, kept so a refused post can put it back even when
    // a refresh replaced the feed meanwhile; refreshed / listed: whether a
    // refresh has run since, and whether it still listed the card.
    if (card.id) leaving[card.id] = { node: card, next: next && next.id, focus: focused, question: q ? q.textContent : '', refreshed: false, listed: false };
    var left = card.__asksLeft = { done: false, waits: [] };
    var finish = function () {
      if (left.done) return;
      left.done = true;
      // The live refresh held back while it collapsed may run now.
      var host = card.parentElement;
      card.removeAttribute('` + AsksCollapsingAttr + `');
      if (host) try { host.dispatchEvent(new Event('` + AsksCollapsedEvent + `', { bubbles: true })); } catch (e) {}
      var w = left.waits; left.waits = [];
      w.forEach(function (f) { f(); });
    };
    // The count drops as the card starts to leave (allCards skips a leaving
    // card), not once its collapse has played.
    count();
    if (reduced()) { card.style.display = 'none'; count(); finish(); return; }
    // 🔴 A LIVE REFRESH WAITS FOR THE COLLAPSE (AttentionLiveScript holds
    // while the feed has a card carrying this attribute): it replaces the
    // feed's innerHTML, and a node detached and put back mid-transition jumps
    // to its end state — the collapse cut off again, by the refresh the answer
    // itself causes.
    card.setAttribute('` + AsksCollapsingAttr + `', '');
    card.style.height = card.offsetHeight + 'px';
    void card.offsetHeight;
    card.classList.add('asks-leave');
    var end = function () {
      if (left.done || card.__asksLeft !== left) return;
      if (card.hasAttribute('data-asks-leaving')) { card.style.display = 'none'; count(); }
      finish();
    };
    card.addEventListener('transitionend', function (e) { if (e.target === card && e.propertyName === 'height') end(); });
    setTimeout(end, ` + strconv.Itoa(AsksLeaveFallbackMs) + `);
  }
  // afterLeave: run f once card's collapse has played out — at once when it
  // is not collapsing (reduced motion, a card a refresh drew, one already
  // collapsed).
  function afterLeave(card, f) {
    var left = card && card.__asksLeft;
    if (left && !left.done) left.waits.push(f); else f();
  }
  // settle: the server took it — gone for good, focus to the next card.
  // 🔴 THE CARD IS REMOVED ONLY ONCE ITS COLLAPSE HAS PLAYED: the post lands
  // in tens of milliseconds, well inside the transition, and removing it then
  // cut the collapse off so the card simply vanished.
  function settle(id) {
    var l = leaving[id]; delete leaving[id];
    settled[id] = Date.now();
    var c = document.getElementById(id) || (l && l.node);
    if (l && l.who && window.toast) window.toast('` + AsksSentToastPrefix + `' + l.who, { tone: 'info' });
    afterLeave(c, function () {
      if (c) c.remove();
      count();
      var a = document.activeElement;
      if (l && l.next && (l.focus || !a || a === document.body)) focusCard(document.getElementById(l.next));
    });
  }
  // refusal: what a failed post says, as a node for the card's error slot.
  // Only the server's own card fragment (asksCardFailed: text/html carrying
  // data-asks-card-refusal) is drawn as sent; anything else — a 401's JSON, a
  // 403, a network failure (status 0) — is one plain sentence, never raw.
  function refusal(x) {
    var st = x ? x.status : 0, ct = '';
    try { ct = (x && x.getResponseHeader('Content-Type')) || ''; } catch (e) {}
    var body = x && typeof x.responseText === 'string' ? x.responseText : '';
    if (st >= 400 && ct.indexOf('text/html') === 0 && body.indexOf('data-asks-card-refusal') >= 0) {
      var t = document.createElement('template');
      t.innerHTML = body;
      var p = t.content.querySelector('[data-asks-card-refusal]');
      if (p) return p;
    }
    var n = document.createElement('p');
    n.setAttribute('role', 'alert');
    n.setAttribute('data-asks-card-refusal', '');
    n.className = '` + AsksCardErrorClass + `';
    n.textContent = st === 0 ? '` + AsksRefusalOffline + `' : st === 401 ? '` + AsksRefusalSignIn + `' : st === 403 ? '` + AsksRefusalForbidden + `' : '` + AsksRefusalOther + ` (' + st + ').';
    return n;
  }
  // refused: the card comes back with the refusal in its error slot — the
  // node on the page, or the kept one put back where it was when a refresh
  // replaced the feed but still listed it. A refresh that no longer lists it
  // (the ask closed elsewhere) gets no card back: the feed's notice says the
  // post failed, never silently.
  function refused(id, x) {
    var l = leaving[id]; delete leaving[id];
    var why = refusal(x), list = feed();
    var c = document.getElementById(id);
    if (!c && l && list && (!l.refreshed || l.listed)) {
      c = l.node;
      var before = l.next && document.getElementById(l.next);
      if (before && before.parentElement === list) list.insertBefore(c, before); else list.appendChild(c);
    }
    if (!c) {
      var note = document.getElementById('asks-feed-notice');
      if (note) {
        note.textContent = '` + AsksRefusalGone + `' + (l && l.question ? ' (“' + l.question.trim().slice(0, 80) + '”): ' : ': ') + why.textContent;
        note.hidden = false;
      }
      count();
      return;
    }
    // Back on the page: its collapse is over and nothing waits on it.
    c.__asksLeft = null;
    c.removeAttribute('` + AsksCollapsingAttr + `');
    c.removeAttribute('data-asks-leaving');
    c.classList.remove('asks-leave');
    c.style.height = ''; c.style.display = '';
    var slot = c.querySelector('[data-asks-card-error]');
    if (slot) { slot.innerHTML = ''; slot.appendChild(why); }
    count();
    focusCard(c);
  }
  function feedCard(form) {
    var list = feed();
    return form && form.hasAttribute && form.hasAttribute('data-asks-card-form') && list && list.contains(form) ? form.closest(CARD) : null;
  }
  document.addEventListener('htmx:beforeRequest', function (e) {
    var form = e.detail && e.detail.elt, card = feedCard(form);
    if (!card) return;
    if (form.hasAttribute('data-attention-answer')) {
      var box = form.querySelector('textarea'), params = e.detail.requestConfig && e.detail.requestConfig.parameters;
      var opt = params && (params.get ? params.get('option') : params.option);
      if (!(opt && String(opt).trim()) && !(box && box.value.trim())) { e.preventDefault(); if (box) box.focus(); return; }
    }
    var slot = card.querySelector('[data-asks-card-error]');
    if (slot) slot.innerHTML = '';
    var note = document.getElementById('asks-feed-notice');
    if (note) note.hidden = true;
    leave(card);
    // 🔴 ON THE FORM ITSELF, NOT THE DOCUMENT: a live refresh that replaced
    // the feed meanwhile leaves this form detached, and htmx's afterRequest on
    // a detached element never bubbles to the document — a document listener
    // would never hear the post land (or fail).
    var id = form.getAttribute('data-asks-card-form');
    // An ANSWER (never a dismissal) says where it went once it is taken:
    // the card's session name (data-asks-who, the card head's own title).
    if (leaving[id] && form.hasAttribute('data-attention-answer')) leaving[id].who = card.getAttribute('data-asks-who') || '';
    form.addEventListener('htmx:afterRequest', function (ev) {
      if (ev.target !== form || !leaving[id]) return;
      var x = ev.detail && ev.detail.xhr;
      if (x && x.status >= 200 && x.status < 300) settle(id);
      else refused(id, x);
    }, { once: true });
  });

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

  // overlayOpen: something modal is over the page — an open dialog, any
  // shown aria-modal element, the notifications panel, the mobile sidebar.
  // The feed's keys stand down while it is.
  function overlayOpen() {
    if (document.querySelector('dialog[open]')) return true;
    var m = document.querySelectorAll('[aria-modal="true"]');
    for (var i = 0; i < m.length; i++) if (m[i].getClientRects().length > 0 && !m[i].closest('[hidden]')) return true;
    var np = document.getElementById('notif-panel');
    if (np && !np.classList.contains('hidden')) return true;
    var sb = document.getElementById('sidebar-root');
    if (sb && sb.getAttribute('data-open') === 'true' && window.innerWidth < 1024) return true;
    return false;
  }
  document.addEventListener('keydown', function (e) {
    if (e.defaultPrevented || e.ctrlKey || e.metaKey || e.altKey) return;
    if (overlayOpen()) return;
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

  // THE LIVE REFRESHES re-draw a feed (the Open feed: AttentionLiveScript; the
  // Answered feed: asksPageScript) and then dispatch the reveal event on it:
  // keep what the owner had (expanded cards, the focused card, and — on the
  // Open feed — no card that is leaving or settled), and let new cards enter.
  var known = {};
  function ids(list) { var o = {}; Array.prototype.forEach.call(list.querySelectorAll(CARD), function (c) { if (c.id) o[c.id] = true; }); return o; }
  document.addEventListener('` + DictateRevealEvent + `', function (e) {
    var list = e.target;
    if (!list || !list.matches || !list.matches('[data-asks-feed]') || !list.id) return;
    var open = list === feed();
    var focusId = list.getAttribute('data-asks-focus') || '';
    var now = Date.now();
    Object.keys(settled).forEach(function (k) { if (now - settled[k] > 60000) delete settled[k]; });
    if (open) Object.keys(leaving).forEach(function (k) { leaving[k].refreshed = true; leaving[k].listed = false; });
    Array.prototype.forEach.call(list.querySelectorAll(CARD), function (c) {
      if (open && c.id && leaving[c.id]) { leaving[c.id].listed = true; c.remove(); return; }
      if (open && c.id && settled[c.id]) { c.remove(); return; }
      if (expanded[c.id]) { c.classList.add('asks-instant'); setExpanded(c, true); }
      if (known[list.id] && !known[list.id][c.id] && !reduced()) { c.classList.add('asks-enter'); setTimeout(function () { c.classList.remove('asks-enter'); }, 400); }
    });
    requestAnimationFrame(function () { Array.prototype.forEach.call(list.querySelectorAll('.asks-instant'), function (c) { c.classList.remove('asks-instant'); }); });
    known[list.id] = ids(list);
    // The refresh is innerHTML, not an htmx swap: the new cards' hx-post forms
    // are processed here.
    if (window.htmx) try { window.htmx.process(list); } catch (err) {}
    if (focusId) { var f = document.getElementById(focusId); if (f && list.contains(f)) focusCard(f); }
    if (open) count();
  });
  // Which card had focus when a refresh replaced it (focusout fires first),
  // per feed.
  document.addEventListener('focusin', function (e) {
    var t = e.target, c = t && t.closest && t.closest(CARD), mine = c && c.closest('[data-asks-feed]');
    Array.prototype.forEach.call(document.querySelectorAll('[data-asks-feed]'), function (list) {
      if (list === mine && t === c) list.setAttribute('data-asks-focus', c.id);
      else if (t !== list) list.removeAttribute('data-asks-focus');
    });
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
  // A tab change recounts (the count is the Open feed's, whatever is shown).
  document.addEventListener('click', function (e) { if (e.target && e.target.closest && e.target.closest('#asks-tabs [role="tab"]')) setTimeout(count, 0); });
  document.addEventListener('click', function (e) { if (e.target && e.target.closest && e.target.closest('#asks-tabs [role="tab"]')) setTimeout(slide, 0); });
  document.addEventListener('keydown', function (e) { if (e.target && e.target.closest && e.target.closest('#asks-tabs [role="tab"]')) setTimeout(slide, 0); });
  function remember() { Array.prototype.forEach.call(document.querySelectorAll('[data-asks-feed][id]'), function (l) { known[l.id] = ids(l); }); }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', function () { watchTabs(); remember(); });
  else { watchTabs(); remember(); }
  document.addEventListener('htmx:load', function () { watchTabs(); remember(); });

  // THE SWIPE: sideways on the feed area moves one tab, in its direction.
  var sw = null;
  function zoomed() { var v = window.visualViewport; return !!(v && v.scale > ` + strconv.FormatFloat(AsksSwipeMaxScale, 'f', -1, 64) + `); }
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
    // Pinch-zoomed in: a sideways drag pans the zoomed page (touch-action is
    // left to the browser), never a tab swipe. Checked when the finger lands:
    // a pinch DURING a drag is two touches, which already ends the swipe.
    if (zoomed()) return;
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
