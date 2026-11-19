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
//
// asks:changed: on the Asks page the shell's SSE extension is not what hears
// the queue — the page's own stream is (AttentionLiveScript), which says so
// with asks:changed. Without it the More badge on the Asks page kept its old
// count until the minute's poll while the page's "N open" had moved on (the
// one open count, internal/api asks_count.go; e2e asks-feed.spec "one count").
const attentionBadgeTrigger = "load, sse:attention_queue.changed from:body delay:50ms, asks:changed from:document delay:50ms, every 60s"

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

// askBusyFunc is THE "the owner is typing an answer" rule, in JavaScript:
// focus in a field of the list, any non-blank answer box in it, or a mic of
// the list opening the microphone, recording or transcribing (dictate.go's
// data-state). Every live
// refresh of an answer form asks it first, so a refresh never eats a
// half-typed answer (AttentionLiveScript).
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

// AttentionLiveScript keeps the element listID live: on the queue's SSE event
// (event: internal/api's EventAttentionChanged) it re-fetches src into it —
// unless the owner is typing an answer (askBusyFunc), in which case it waits
// until they leave the field, or an Asks card is collapsing out
// (AsksCollapsingAttr), in which case it waits for the collapse to end. The /attention page's list and a mirrored
// window's asks badge (AskerAsksLive) use it; each is its own document (no
// htmx), so it opens its own stream.
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
    if (busy(list) || list.querySelector('[` + AsksCollapsingAttr + `]')) { pending = true; return; }
    pending = false;
    fetch(src, { credentials: 'same-origin' }).then(function (r) {
      return r.ok ? r.text() : null;
    }).then(function (html) {
      if (html !== null && !busy(list) && !list.querySelector('[` + AsksCollapsingAttr + `]')) {
        list.innerHTML = html;
        // No htmx swap, so no htmx:load: the mics the new content draws are
        // shown by this event (dictate.go's DictateRevealEvent).
        try { list.dispatchEvent(new Event(` + strconv.Quote(DictateRevealEvent) + `, { bubbles: true })); } catch (e) {}
      } else if (html !== null) pending = true;
    }).catch(function () {});
  }
  list.addEventListener('focusout', function () { if (pending) setTimeout(refresh, 0); });
  // A refresh held back while an Asks card collapsed out (AsksCollapsingAttr)
  // runs once it has.
  list.addEventListener(` + strconv.Quote(AsksCollapsedEvent) + `, function () { if (pending) setTimeout(refresh, 0); });
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

// A SESSION PAGE'S ASKS BADGE (internal/api attention_asker.go): an agent's
// page (the chief's included) and a mirrored window's rendered view draw, in
// their header, a small count of the asker's OPEN asks; a tap opens the Asks
// page filtered to them. 🔴 THERE IS NO ASKS BLOCK ON THE PAGE (operator
// decision): the block that listed them above the composer, with their answer
// forms, took too much of the screen — they are answered on the Asks page or
// from their notification's card.

// AskerAsksBadgeID is the badge's slot: the element its live refresh fills.
const AskerAsksBadgeID = "asker-asks-badge"

// askerAsksEvent is the SSE event the badge refreshes on (internal/api's
// EventAttentionChanged; its TestTheBadgeListensForTheEventTheServerSends pins
// the spelling).
const askerAsksEvent = "attention_queue.changed"

// askerAsksTrigger is the agent page's refresh of the badge (askerAsksRefresh):
// on the queue's SSE event, debounced as the Asks badge is
// (attentionBadgeTrigger), and once a minute.
const askerAsksTrigger = "sse:" + askerAsksEvent + " delay:50ms, every 60s"

// AskerAsksBadgeHTML is the badge's content for n asks waiting on the reader:
// a link to href (the Asks page filtered to them), the count in a small pill;
// no link at zero, so the slot (AskerAsksBadge) is hidden and takes no room
// in the header.
//
// open: ANY ask from the session is open — a question to the chief, a notice
// or one the reader only watches included, none of which n counts. It draws
// AskerAsksOpenMark, the mark the rendered view's suggested replies hide by
// (suggest_row.go blocked(), the server's own rule: suggest.go withholds them
// while any ask from the session is open).
//
// A full navigation (hx-boost="false"): /attention is a standalone document,
// never a body swap of the agent page's shell (attentionLink's rule).
func AskerAsksBadgeHTML(n int, href string, open bool) string {
	mark := ""
	if open || n > 0 {
		mark = AskerAsksOpenMark
	}
	if n <= 0 {
		return mark
	}
	c := strconv.Itoa(n)
	label := "Open the " + c + " questions waiting on your answer"
	if n == 1 {
		label = "Open the question waiting on your answer"
	}
	return mark + `<a href="` + html.EscapeString(href) + `" hx-boost="false" data-asker-asks-link data-count="` + c + `" aria-label="` + label + `" title="` + label + `"` +
		// A 44px tap target, drawn as the small pill inside it.
		` class="press group inline-flex h-11 shrink-0 items-center justify-center px-0.5">` +
		`<span class="inline-flex h-6 min-w-[1.5rem] items-center justify-center gap-1 rounded-full bg-accent/15 px-1.5 text-xs font-semibold tabular-nums text-accent ring-1 ring-inset ring-accent/40 transition group-hover:bg-accent/25">` +
		`<span class="h-1.5 w-1.5 shrink-0 rounded-full bg-accent" aria-hidden="true"></span><span data-asker-asks-count>` + c + `</span></span></a>`
}

// AskerAsksBadge is the badge's slot in a session page's header, holding inner
// (AskerAsksBadgeHTML). It carries no refresh of its own: a header is outside
// the agent page's SSE section, where the sse extension registers nothing
// (askerAsksRefresh does it from inside), and the mirrored view has no htmx
// (AskerAsksLive). Without a link inside it the slot is hidden (it may still
// hold AskerAsksOpenMark), so it takes no room and no gap in the header.
func AskerAsksBadge(inner string) g.Node {
	return Span(ID(AskerAsksBadgeID), Class(AskerAsksSlotClass), g.Attr("aria-live", "polite"), g.Raw(inner))
}

// AskerAsksSlotClass is the badge slot's class: hidden while it holds no link.
const AskerAsksSlotClass = "inline-flex shrink-0 [&:not(:has([data-asker-asks-link]))]:hidden"

// AskerAsksOpenMark marks, inside the badge's slot, that an ask from the
// session is open (AskerAsksBadgeHTML's open).
const AskerAsksOpenMark = `<span hidden data-asker-asks-open></span>`

// askerAsksRefresh keeps the agent page's badge live: a hidden element INSIDE
// the chat pane's SSE section (agentChatPane), re-reading src into the
// header's slot on the queue's event.
//
// 🔴 IT IS ALSO WHAT RELAYS THE EVENT TO <body> ON THIS PAGE. The sse
// extension (web/static/vendor/sse.js) registers an `sse:` trigger only on an
// element under an sse-connect, and dispatches the event ON that element, from
// where it bubbles to <body> — which is how the sidebar's `from:body`
// listeners (its agent list, its ☰ waiting badge, the Asks count) hear it.
// The asks block that this replaced was that element; without one the
// sidebar's attention counts on the agent page stopped hearing the queue.
func askerAsksRefresh(src string) g.Node {
	return Span(g.Attr("hidden", ""), g.Attr("data-asker-asks-refresh", ""),
		hx("hx-get", src), hx("hx-trigger", askerAsksTrigger), hx("hx-target", "#"+AskerAsksBadgeID), hx("hx-swap", "innerHTML"))
}

// AskerAsksLive is what keeps the mirrored view's badge live: the Asks page's
// own script, on the badge's slot (nothing without a source).
func AskerAsksLive(src string) g.Node {
	if src == "" {
		return nil
	}
	return g.Raw(AttentionLiveScript(AskerAsksBadgeID, src, askerAsksEvent))
}

// AsksFilterHTML is the Asks page's line while its Open list is filtered to
// one session's asks (internal/api's ?from=): "Showing asks from <name>" and a
// link to the whole list (all).
func AsksFilterHTML(name, all string) string {
	return `<p class="flex min-w-0 flex-wrap items-center gap-x-2 px-1 text-sm text-fg2" data-testid="asks-filter">` +
		`<span class="min-w-0 truncate">Showing asks from <strong class="font-semibold text-fg" data-asks-filter-name>` + html.EscapeString(name) + `</strong></span>` +
		`<span aria-hidden="true">·</span>` +
		`<a class="inline-flex min-h-[44px] items-center font-medium text-accent underline-offset-2 hover:underline" href="` + html.EscapeString(all) + `" data-asks-filter-all>Show all</a></p>`
}

// THE COMPACT ANSWER FORM's classes: an ask's card (internal/api's
// attentionItemsHTMLTo — the Asks queue's, an ask's own page's and an
// officer's page's, one form for all) draws its options, its answer box and its Answer / Dismiss
// row with these. They live here, not in internal/api, because the stylesheet
// scans internal/ui: a class written only in internal/api is never emitted.
//
// The box starts ONE line and grows with what is typed (AskFormFitClass,
// web/css/input.css: field-sizing, capped, scrolling inside past the cap; a
// browser without field-sizing keeps the one line and scrolls). The options
// are ANSWERS, not calls to action: as wide as their words, left-aligned, in
// normal-weight small text, wrapping naturally (a live card's two
// "agent_dispatch on pod/…: start a new agent on it" options were bold,
// centred, full-width two-line buttons that pushed Answer below the fold); Answer (primary) and Dismiss (quiet)
// share one row. Dismiss SAYS "Dismiss" and is NAMED "Dismiss without
// answering" (aria-label, title): the long words on the button squeezed
// Answer to "Answe" in the queue's 286px card. Every target stays 44px (e2e layout.spec.ts).
const (
	AskFormFitClass     = "answer-fit"
	AskFormClass        = "flex flex-col gap-1.5"
	AskFormOptionsClass = "flex flex-wrap gap-1.5"
	AskFormOptionClass  = "press min-h-[44px] rounded-md bg-bg px-2.5 py-1.5 text-left text-sm font-normal leading-snug text-fg ring-1 ring-inset ring-edge transition hover:bg-s2"
	AskFormBoxRowClass  = "flex items-end gap-2"
	AskFormInputClass   = AskFormFitClass + " min-h-[44px] min-w-0 flex-1 resize-none rounded-md bg-bg px-3 py-2 text-base text-fg ring-1 ring-inset ring-edge placeholder:text-muted focus:outline-none focus:ring-2 focus:ring-focus"
	AskFormActionsClass = "flex items-center gap-2"
	AskFormAnswerClass  = "press min-h-[44px] flex-1 rounded-md bg-accent px-3 py-2 text-base font-semibold text-on-accent transition hover:bg-accent/90"
	AskFormDismissClass = "press min-h-[44px] shrink-0 whitespace-nowrap rounded-md px-3 py-2 text-sm font-medium text-fg2 transition hover:bg-s2 hover:text-fg"
)
