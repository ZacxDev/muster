package ui

import (
	"html"
	"strconv"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE ATTENTION QUEUE's navigation entry: a link to /attention (a standalone
// document, rendered by internal/api) with a live count of what is waiting on
// the owner.
//
// 🔴 THE WIRING IS ON THE SLOT, THE ROUTE ANSWERS ONLY ITS CONTENTS. The slot
// (attentionBadgeSlot) carries hx-get/hx-trigger and swaps its INNER html with
// what GET AttentionBadgePath answers — AttentionBadgeHTML, which carries no
// trigger at all. The first version put the wiring on the badge itself and
// swapped it outerHTML: every swap inserted a fresh element whose `load`
// trigger fired again, a request every ~25ms for as long as the page was open
// (measured in the browser, which is how a disabled member's session got
// refused mid-test). TestTheBadgeFragmentCannotRetriggerItself pins it.

// AttentionBadgePath is the badge fragment's route.
const AttentionBadgePath = "/attention/badge"

// attentionBadgeTrigger refreshes the badge on the queue's SSE event (the
// shell's /events stream; internal/api's EventAttentionChanged), on load, and
// once a minute — the backstop on a page whose stream is not connected.
//
// 🔴 delay:50ms IS A DEBOUNCE, AND ANOTHER LISTENER IS WHY IT IS NEEDED. htmx's
// SSE extension dispatches an event on EVERY element that listens for it, and
// each dispatch bubbles to body — so with another element listening for this
// event too (the sidebar's agent list, for its rows' attention counts; first
// the retired Agents list's cards), a `from:body` listener here heard ONE event
// twice and fetched the badge twice (measured: attention.spec's
// "badge requests while nothing changed" saw 2). Debounced, the dispatches of
// one event are one fetch, and the fetch still follows the last of them.
const attentionBadgeTrigger = "load, sse:attention_queue.changed from:body delay:50ms, every 60s"

// AttentionBadgeHTML is the badge for n waiting items: hidden at zero. It is
// the slot's CONTENT and carries no htmx wiring of its own.
func AttentionBadgeHTML(n int) string {
	hidden := ""
	if n <= 0 {
		hidden = " hidden"
	}
	label := strconv.Itoa(n) + " waiting"
	return `<span id="attention-badge" data-testid="attention-badge" data-count="` + strconv.Itoa(n) + `" aria-label="` + label + `"` +
		` class="inline-flex min-w-[1.5rem] items-center justify-center rounded-full bg-accent px-1.5 text-xs font-semibold text-on-accent"` +
		hidden + `>` + strconv.Itoa(n) + `</span>`
}

// attentionBadgeSlot is the element that refreshes the badge.
func attentionBadgeSlot() g.Node {
	return Span(ID("attention-badge-slot"), Class("ml-auto"), hx("hx-get", AttentionBadgePath), hx("hx-trigger", attentionBadgeTrigger),
		hx("hx-swap", "innerHTML"), g.Raw(AttentionBadgeHTML(0)))
}

// attentionLink is the sidebar's entry, drawn only when the queue is served.
// A full navigation (hx-boost="false"): /attention is a standalone document,
// never a body swap of the shell. active/current mark it as the entry the
// page belongs to (the /attention page itself): see sidebar.
//
// 🔴 THE COUNT IS NOT ON THIS LINK: it is on the header of the sidebar's
// collapsed "More" (navMore), which holds this link — on the link it would be
// hidden whenever More is shut, and it is the one actionable count in there.
func attentionLink(feat Features, active, current bool) g.Node {
	if !feat.Attention {
		return nil
	}
	return A(ID("sidebar-attention"), Href("/attention"), hx("hx-boost", "false"), g.If(current, g.Attr("aria-current", "page")),
		Class("press flex min-h-[44px] w-full items-center gap-3 rounded-lg px-3 text-left text-sm font-medium transition "+navEntryTone(active)),
		Span(Class("text-base"), g.Text("🔔")), Span(g.Text("Asks")))
}

// AskerAsksID is the element id of a session page's asks block.
const AskerAsksID = "asker-asks"

// askerAsksTrigger refreshes the block on the queue's SSE event, debounced as
// the badge is (attentionBadgeTrigger), and once a minute.
const askerAsksTrigger = "sse:attention_queue.changed from:body delay:50ms, every 60s"

// askBusyFunc is THE "the owner is typing an answer" rule, in JavaScript:
// focus in a field of the list, any non-blank answer box in it, or a mic of
// the list opening the microphone, recording or transcribing (dictate.go's
// data-state). Every live
// refresh of an answer form asks it first, so a refresh never eats a
// half-typed answer (AttentionLiveScript; the agent page's htmx refresh,
// askerAsksBusy).
//
// 🔴 THE MIC IS PART OF "TYPING". A mic can be tapped without the box ever
// taking focus, and the recording is then for a box nothing else marks busy: a
// refresh that replaced it mid-recording left the recorder addressing a box no
// longer on the page, and the transcript was dropped (dictate.go inserts only
// into a box still connected).
const askBusyFunc = `function (list) {
    var a = document.activeElement;
    if (a && list.contains(a) && (a.tagName === 'TEXTAREA' || a.tagName === 'INPUT')) return true;
    if (list.querySelector('[data-dictate][data-state="opening"], [data-dictate][data-state="recording"], [data-dictate][data-state="busy"]')) return true;
    var fields = list.querySelectorAll('textarea');
    for (var i = 0; i < fields.length; i++) if (fields[i].value.trim() !== '') return true;
    return false;
  }`

// askerAsksBusy cancels the agent page block's refresh while the owner types.
const askerAsksBusy = "if ((" + askBusyFunc + ")(this)) event.preventDefault()"

// AttentionLiveScript keeps the element listID live: on the queue's SSE event
// (event: internal/api's EventAttentionChanged) it re-fetches src into it —
// unless the owner is typing an answer (askBusyFunc), in which case it waits
// until they leave the field. The /attention page's list and a mirrored
// window's asks block use it; each is its own document (no htmx), so it opens
// its own stream.
func AttentionLiveScript(listID, src, event string) string {
	return `<script>
(function () {
  var busy = ` + askBusyFunc + `;
  var list = document.getElementById(` + strconv.Quote(listID) + `);
  if (!list || !window.EventSource) return;
  var src = ` + strconv.Quote(src) + `;
  var pending = false;
  function refresh() {
    // The Asks page's other parts (its tabs' counts) follow the same event.
    try { document.dispatchEvent(new Event('asks:changed')); } catch (e) {}
    if (busy(list)) { pending = true; return; }
    pending = false;
    fetch(src, { credentials: 'same-origin' }).then(function (r) {
      return r.ok ? r.text() : null;
    }).then(function (html) {
      if (html !== null && !busy(list)) {
        list.innerHTML = html;
        // No htmx swap, so no htmx:load: the mics the new content draws are
        // shown by this event (dictate.go's DictateRevealEvent).
        try { list.dispatchEvent(new Event(` + strconv.Quote(DictateRevealEvent) + `, { bubbles: true })); } catch (e) {}
      } else if (html !== null) pending = true;
    }).catch(function () {});
  }
  list.addEventListener('focusout', function () { if (pending) setTimeout(refresh, 0); });
  // A refresh a mic held back runs once the mic is idle again (dictate.go's
  // DictateIdleEvent); refresh asks busy() again, so a box the recording
  // filled still holds it.
  list.addEventListener(` + strconv.Quote(DictateIdleEvent) + `, function () { if (pending) setTimeout(refresh, 0); });
  // The stream, reconnected by the page's wake (restore.go, live.go): a
  // closed one at once, an open one silent past its heartbeat bound too. A
  // reconnect catches up with one refresh — what changed meanwhile was not
  // heard.
  var es = null, lastAt = 0, beat = false, gone = false;
  function open() {
    if (es) { try { es.close(); } catch (e) {} }
    try { es = new EventSource('/events'); } catch (e) { es = null; return; }
    lastAt = Date.now(); beat = false;
    var hear = function () { lastAt = Date.now(); };
    es.addEventListener('open', hear);
    es.addEventListener(` + strconv.Quote(LivePingEvent) + `, function () { beat = true; hear(); });
    es.addEventListener(` + strconv.Quote(event) + `, function () { hear(); refresh(); });
  }
  open();
  if (!es) return;
  ` + restoreJoinJS(`{
    hide: function () { gone = true; if (es) { try { es.close(); } catch (e) {} } },
    show: function () { gone = false; open(); refresh(); },
    wake: function (why, staleMs) {
      if (gone || !es) return;
      if (es.readyState === 2 ? why !== 'tick' : (es.readyState === 1 && beat && Date.now() - lastAt > staleMs)) { open(); refresh(); }
    }
  }`) + `
  setInterval(refresh, 60000);
})();
</script>`
}

// askerAsksEvent is the SSE event the mirrored view's block refreshes on: the
// one askerAsksTrigger names (internal/api's EventAttentionChanged; its
// TestTheBadgeListensForTheEventTheServerSends pins the spelling).
const askerAsksEvent = "attention_queue.changed"

// AskerAsksBlock is a session page's block of its asker's OPEN attention asks:
// inner, the content internal/api renders (the queue's own items and answer
// forms; "" for none, and then the block takes no room), refreshed from src.
// viaHTMX: the page is in the shell, whose htmx SSE refreshes it (the agent
// page); otherwise (the mirrored view, its own document) the page carries
// internal/api's attentionLiveScript, which refreshes it by its id.
//
// Bounded and scrolling on its own (max-h, overflow-y-auto, flex-none), so on
// a phone it never pushes the conversation or the pinned composer off screen.
//
// 🔴 ONE QUESTION AT A TIME, EACH CLAMPED IN ITS OWN CARD (an operator report:
// the block "looks broken on mobile"). Seven long questions filled the block's
// scroll box with the first one's text, cut off mid-sentence at the box's
// edge, and left the conversation a sliver. Now the list (AskerAsksList)
// shows the first ask and keeps the rest behind one "Show N more"; each
// question's text is clamped to four lines (web/css/input.css, "The asker
// block") and its "Show more" is drawn INSIDE its card, opening it in place —
// see ONE AT A TIME, CLAMPED below. With the compact answer form (THE COMPACT
// ANSWER FORM) one card, its "Show N more" and the heading measure 378px at
// 412×915 — inside the 45vh cap (412px there), so the block does not scroll
// inside and no card is cut by its edge; at 40vh (366px) it did. The block is
// as tall as what it holds; the cap only bounds a card opened by "Show more"
// or a long typed answer (e2e chief-mobile.spec.ts measures both the fit and
// the conversation's room).
//
// It carries "Hide until next ask" (AskerAsksHeading) and, after it, the chip
// that reopens it (askerAsksChip) — see HIDE UNTIL NEXT ASK below.
func AskerAsksBlock(inner, src string, viaHTMX bool) g.Node {
	return g.Group{
		Section(
			ID(AskerAsksID),
			g.Attr("aria-label", "Questions waiting on your answer"),
			g.Attr("aria-live", "polite"),
			g.Attr("data-src", src),
			g.If(viaHTMX, g.Group([]g.Node{hx("hx-get", src), hx("hx-trigger", askerAsksTrigger), hx("hx-swap", "innerHTML"),
				hx("hx-target", "this"), g.Attr("hx-on::before-request", askerAsksBusy)})),
			Class("flex max-h-[45vh] min-h-0 flex-none flex-col gap-2 overflow-y-auto border-t border-line px-3 py-2 empty:hidden"),
			g.Raw(inner),
		),
		askerAsksChip(),
		askerAsksHideScript(),
	}
}

// HIDE UNTIL NEXT ASK (operator decision): the block's heading carries a
// button that hides the block IN THIS VIEW and nothing else. The asks stay
// open in the Asks queue (/attention); nothing is posted — not to the queue's
// answer or dismiss route, not to the session — and no attention item is
// cancelled. A small "N waiting" chip takes the block's place and reopens it.
//
// 🔴 IT IS PER ASK ID, AND A NEW ASK BRINGS THE BLOCK BACK. The hide records the
// ids of the asks it hid (their items' element ids, "ask-<id>") in this
// device's localStorage; the block stays hidden only while EVERY ask it holds
// is one of those. A refresh that adds an ask with an id not hidden shows the
// block again on its own; one that only drops answered asks keeps it hidden.
// Reopening by the chip forgets the ids it shows, so the next load shows it too.
// Storage that throws keeps the hide for this page only. Every write re-reads
// the stored list first (another tab's hides are merged, never overwritten),
// and a write in another tab applies here at once (the storage event).
//
// 🔴 APPLIED BEFORE THE BLOCK IS PAINTED. The script runs as the block is
// parsed (it follows it) and again on every refresh of its content (a
// MutationObserver, whose callback runs before the next paint), so a page
// that loads with its asks hidden never shows them first.

// askerAsksHideLabel is the hide button's text.
const askerAsksHideLabel = "Hide until next ask"

// askerAsksHiddenKey is where the hidden ask ids are kept (localStorage).
const askerAsksHiddenKey = "muster.asks.hidden.v1"

// AskerAsksHeading is the block's heading row: the count (n, "1 question"),
// as "<n> waiting", and the hide button. internal/api's askerAsksHTML draws it
// above the items.
//
// 🔴 ONE LINE: the heading truncates and the hide is a quiet nowrap text
// button (a 44px tap target, no border). It was an uppercase "WAITING ON YOUR
// ANSWER: 7 QUESTIONS" that wrapped to two lines beside a bordered button.
func AskerAsksHeading(n string) string {
	return `<div class="flex min-w-0 items-center gap-2"><h2 class="min-w-0 flex-1 truncate text-sm font-semibold text-fg" data-asker-asks-heading>` +
		html.EscapeString(n) + ` waiting</h2><button type="button" data-asker-asks-hide class="press -mr-2 inline-flex min-h-[44px] shrink-0 items-center whitespace-nowrap rounded-lg px-2 text-xs font-medium text-muted transition hover:bg-s2 hover:text-fg">` +
		askerAsksHideLabel + `</button></div>`
}

// AskerAsksList is the block's list: items, the queue's own <li> cards for n
// asks (internal/api's attentionItemsHTMLTo), and — when there is more than
// one — the "Show N more" that reveals the rest (ONE AT A TIME, CLAMPED).
func AskerAsksList(items string, n int) string {
	if n < 2 {
		return `<ul class="flex flex-col gap-2" data-testid="asker-asks">` + items + `</ul>`
	}
	return `<ul class="flex flex-col gap-2" data-testid="asker-asks" data-asks-collapsed>` + items + `</ul>` +
		`<button type="button" data-asks-more class="press inline-flex min-h-[44px] shrink-0 items-center self-start whitespace-nowrap rounded-lg px-2 text-xs font-semibold text-accent transition hover:bg-s2">Show ` +
		strconv.Itoa(n-1) + ` more</button>`
}

// THE COMPACT ANSWER FORM's classes: an ask's card (internal/api's
// attentionItemsHTMLTo — the Asks queue's AND a session page's asks block, one
// form for both) draws its options, its answer box and its Answer / Dismiss
// row with these. They live here, not in internal/api, because the stylesheet
// scans internal/ui: a class written only in internal/api is never emitted.
//
// The box starts ONE line and grows with what is typed (AskFormFitClass,
// web/css/input.css: field-sizing, capped, scrolling inside past the cap; a
// browser without field-sizing keeps the one line and scrolls). The options
// are as wide as their words and wrap; Answer (primary) and Dismiss (quiet)
// share one row. Dismiss SAYS "Dismiss" and is NAMED "Dismiss without
// answering" (aria-label, title): the long words on the button squeezed
// Answer to "Answe" in the queue's 286px card. Every target stays 44px (e2e layout.spec.ts).
const (
	AskFormFitClass     = "answer-fit"
	AskFormClass        = "flex flex-col gap-1.5"
	AskFormOptionsClass = "flex flex-wrap gap-2"
	AskFormOptionClass  = "press min-h-[44px] rounded-md bg-bg px-3 py-2 text-sm font-semibold text-fg ring-1 ring-inset ring-edge"
	AskFormBoxRowClass  = "flex items-end gap-2"
	AskFormInputClass   = AskFormFitClass + " min-h-[44px] min-w-0 flex-1 resize-none rounded-md bg-bg px-3 py-2 text-base text-fg ring-1 ring-inset ring-edge placeholder:text-muted focus:outline-none focus:ring-2 focus:ring-focus"
	AskFormActionsClass = "flex items-center gap-2"
	AskFormAnswerClass  = "press min-h-[44px] flex-1 rounded-md bg-accent px-3 py-2 text-base font-semibold text-on-accent transition hover:bg-accent/90"
	AskFormDismissClass = "press min-h-[44px] shrink-0 whitespace-nowrap rounded-md px-3 py-2 text-sm font-medium text-fg2 transition hover:bg-s2 hover:text-fg"
)

// ONE AT A TIME, CLAMPED (AskerAsksBlock's layout; the rules are in
// web/css/input.css under "The asker block", outside every @layer so the
// cards' own `flex` cannot outrank them):
//   - a list with data-asks-collapsed shows its FIRST card only; "Show N more"
//     (data-asks-more) drops the attribute and itself. The choice outlives the
//     block's refreshes (the script keeps it), not a reload.
//   - each question's text ([data-attention-text]) is clamped to four lines
//     until it carries data-ask-open. The script adds a "Show more" button
//     (data-ask-more-text) right after a text that IS clipped — inside its
//     <li>, so inside the card's ring — and it toggles data-ask-open in place
//     ("Show less" closes it). Which cards are open outlives a refresh too,
//     by the card's id.
// Both are applied by askerAsksHideScript, which already runs before the block
// is first painted and on every refresh of its content.

// askerAsksChip reopens a hidden block: drawn hidden, shown by
// askerAsksHideScript while the block is hidden, saying how many wait.
func askerAsksChip() g.Node {
	return Button(
		ID(AskerAsksID+"-chip"),
		Type("button"),
		g.Attr("hidden", ""),
		g.Attr("aria-controls", AskerAsksID),
		g.Attr("aria-label", "Show the questions waiting on your answer"),
		// A 44px tap target (layout.spec.ts), drawn as the small pill inside it.
		Class("press group mx-3 inline-flex h-11 flex-none items-center self-start"),
		Span(
			Class("inline-flex h-7 items-center gap-1.5 rounded-full bg-accent/15 px-3 text-xs font-semibold text-accent ring-1 ring-inset ring-accent/40 transition group-hover:bg-accent/25"),
			Span(Class("h-2 w-2 shrink-0 rounded-full bg-accent"), g.Attr("aria-hidden", "true")),
			Span(g.Attr("data-asker-asks-count", "")),
		),
	)
}

// askerAsksHideScript is the hide and the chip (see HIDE UNTIL NEXT ASK).
func askerAsksHideScript() g.Node {
	return Script(g.Raw(`
(function () {
  var block = document.getElementById('` + AskerAsksID + `');
  var chip = document.getElementById('` + AskerAsksID + `-chip');
  // 🔴 THE GUARD IS A PROPERTY, NOT AN ATTRIBUTE: htmx's history snapshot
  // carries attributes, so after Back an attribute guard made this script
  // bail on a block it had never bound — the Hide, the chip and the refresh
  // all dead. A restored block is a new element, without the property.
  if (!block || !chip || block.__musterHideBound) return;
  block.__musterHideBound = true;
  var KEY = '` + askerAsksHiddenKey + `';
  // mem is only the fallback for storage that throws (this page's own hides).
  var mem = [];
  // 🔴 READ AT EVERY USE, NEVER CACHED: another tab may have written since,
  // and a write built on a stale copy dropped its hides.
  function load() {
    try {
      var a = JSON.parse(localStorage.getItem(KEY) || '[]');
      if (Array.isArray(a)) { mem = a; return a.slice(); }
    } catch (e) {}
    return mem.slice();
  }
  // The newest 200 are kept: an id is never reused, so older ones only weigh.
  function save(a) { mem = a.slice(-200); try { localStorage.setItem(KEY, JSON.stringify(mem)); } catch (e) {} }
  function keys() {
    var out = [], items = block.querySelectorAll('[data-attention-item][id]');
    for (var i = 0; i < items.length; i++) out.push(items[i].id);
    return out;
  }
  // ONE AT A TIME, CLAMPED: whether "Show N more" was used, and which cards'
  // text is open (by the card's id) — both outlive a refresh of the content.
  var all = false, opened = {};
  function fit() {
    if (all) {
      var ul = block.querySelector('[data-asks-collapsed]');
      if (ul) ul.removeAttribute('data-asks-collapsed');
      var m = block.querySelector('[data-asks-more]');
      if (m) m.remove();
    }
    var ts = block.querySelectorAll('[data-attention-item] [data-attention-text]');
    for (var i = 0; i < ts.length; i++) {
      var t = ts[i], li = t.closest('[data-attention-item]');
      if (li && opened[li.id]) t.setAttribute('data-ask-open', '');
      var b = t.nextElementSibling && t.nextElementSibling.hasAttribute('data-ask-more-text') ? t.nextElementSibling : null;
      var open = t.hasAttribute('data-ask-open');
      // A card that is not laid out (hidden, collapsed) is measured when it is.
      if (!open && t.clientHeight === 0) continue;
      var clipped = open || t.scrollHeight > t.clientHeight + 1;
      if (!clipped) { if (b) b.remove(); continue; }
      if (!b) {
        b = document.createElement('button');
        b.type = 'button';
        b.setAttribute('data-ask-more-text', '');
        // 44px: a thumb target (e2e/tests/layout.spec.ts), drawn under the text.
        b.className = 'press -mt-1 inline-flex min-h-[44px] items-center self-start rounded-lg px-1 text-xs font-medium text-accent';
        t.after(b);
      }
      b.textContent = open ? 'Show less' : 'Show more';
      b.setAttribute('aria-expanded', open ? 'true' : 'false');
    }
  }
  function apply() {
    var k = keys(), d = load(), hide = k.length > 0;
    for (var i = 0; i < k.length && hide; i++) if (d.indexOf(k[i]) < 0) hide = false;
    block.hidden = hide;
    chip.hidden = !hide;
    if (hide) chip.querySelector('[data-asker-asks-count]').textContent = k.length + ' waiting';
    else fit();
  }
  block.addEventListener('click', function (e) {
    var t = e.target && e.target.closest ? e.target : null;
    var more = t && t.closest('[data-asks-more]');
    if (more) { all = true; fit(); return; }
    var mt = t && t.closest('[data-ask-more-text]');
    if (mt) {
      var txt = mt.previousElementSibling, li = mt.closest('[data-attention-item]');
      if (!txt) return;
      if (txt.hasAttribute('data-ask-open')) txt.removeAttribute('data-ask-open'); else txt.setAttribute('data-ask-open', '');
      if (li && li.id) { if (txt.hasAttribute('data-ask-open')) opened[li.id] = true; else delete opened[li.id]; }
      fit();
      return;
    }
    var b = t ? t.closest('[data-asker-asks-hide]') : null;
    if (!b) return;
    e.preventDefault();
    var d = load(), k = keys();
    for (var i = 0; i < k.length; i++) if (d.indexOf(k[i]) < 0) d.push(k[i]);
    save(d);
    apply();
    try { chip.focus(); } catch (err) {}
  });
  chip.addEventListener('click', function () {
    var k = keys();
    save(load().filter(function (x) { return k.indexOf(x) < 0; }));
    apply();
    var b = block.querySelector('[data-asker-asks-hide]');
    if (b) try { b.focus(); } catch (err) {}
  });
  if (typeof MutationObserver !== 'undefined') new MutationObserver(apply).observe(block, { childList: true });
  // A hide (or a reopen) in another tab applies here at once.
  window.addEventListener('storage', function (e) { if (e.key === KEY && block.isConnected) apply(); });
  // A rotation or a resize changes which texts are clipped.
  window.addEventListener('resize', function () { if (block.isConnected && !block.hidden) fit(); });
  apply();
})();
`))
}

// AskerAsksLive is what keeps a block drawn with viaHTMX false live: the
// queue page's own script, on the block (nothing without a block).
func AskerAsksLive(src string) g.Node {
	if src == "" {
		return nil
	}
	return g.Raw(AttentionLiveScript(AskerAsksID, src, askerAsksEvent))
}
