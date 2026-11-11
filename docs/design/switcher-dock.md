# Design: the switcher dock (a vertical session list from the bottom bar)

**Status: Phase 1 and Phase 3 (the filter) are implemented, with decision 8
(one snap, the grip closes, a swipe on the dock's list changes its tab); the
header drag of Phase 2 is moot, and Phases 4 and 5 are proposed.** Where an
older section below still describes the full snap or "no swipe changes a dock
tab", decision 8 supersedes it, and the code (`internal/ui/switcher_dock.go`)
is the authority. The operator decisions
below, including the answers to this page's open questions (decisions 5–7,
and the amendment to decision 3), are **decided**. Every other section is the recommended design. Where
this page says how muster works today, it cites `file:line` at `main`
`723832da` (session-state cites at `6897dcdc`, after #430), and the code is the
authority (see [decisions.md](../decisions.md)).
Where it says how the Android shell behaves, it cites
[ZacxDev/muster-android][android] at `7af3250b`. Claims nothing here has
measured are marked **UNVERIFIED**, and each one is an acceptance check in a
phase below.

[android]: https://github.com/ZacxDev/muster-android/tree/7af3250b750781e2360e2dfd620e32e3fb34bdda

## The ask

> "Also, the bottom bar below the message bar where I can see recent agents.
> There are often many recent agents, and I have to scroll horizontally, which
> is crowded on mobile. Scope and propose how we could cleanly integrate a
> swipe up motion to expand and update the layout so that I can vertically
> scroll in a. New dock component that is fixed to the bottom of the screen
> and sits on top of the current content. I can then select an agent from that
> list or I can swipe down from the edge to close it."

## Summary

| | Proposal |
|---|---|
| What it is | A native modal `<dialog id="switcher-dock">`, drawn **once per document** beside the command palette, laid out as a bottom sheet in the **touch layout** (a coarse primary pointer) and as a centred panel in the **mouse layout**. It always opens in the **top** document, also when the bar that opens it is inside the view-swap frame. The same pattern as the agent page's focus history dialog (`internal/ui/focus_history.go:139-174`) |
| How it opens | Touch: a swipe up that starts on the bar's row, or a tap on the bar's view button, which **is** the handle (decision 5). Desktop: today's view menu, plus a new chevron button |
| Snap points | **One: half**, 60% of the visual viewport, never less than the header and three rows; the list scrolls inside it (decision 8: no full snap) |
| How it closes | A tap on the grip at its top; a tap on the backdrop; the × button; Escape (after a first Escape has cleared a typed filter); Android back. **The dock touches no history**: back reaches it through the modal dialog's own close watcher in a browser, and through a small `back-claim` bridge message in the Android shell. The list only scrolls; it never moves the sheet |
| Content | A filter box under the grip, tabs Recent / Waiting / Pins / All under it (a sideways swipe on the list walks them), then vertical rows: whole name, host, current focus title, waiting badge (stalled and needs-you badges in Phase 5). Recent lists up to 30 in the dock, 8 on the bar (decision 6). A row tap is a real load, like a pill |
| Where the data comes from | **The bar already on the page.** Its four rows are server-rendered with every session page; the dock draws its rows from them when it opens. No new request, no second copy of the markup. Two new attributes per pill (host, focus title), and an ordered href list for Recent's entries 9–30 (each one already a pill in the All row) |
| Filter | The palette's in-page rule (`window.musterPaletteRank`) over the shown tab, previous titles included through `data-switcher-prev`. No server search: the palette stays the full-history search |
| Gesture rules | The touch layout gets gestures; the mouse layout gets the chevron. A drag starts **only** on the bar's row or the sheet's header. Never on xterm, the composer, or anywhere while the soft keyboard is up. A sideways swipe changes a tab **only on the dock's list** (decision 8), nowhere else. Release snaps by position only |
| Phases | 1 dock + tap to open + Back + Recent to 30 (shipped); 2 gestures (the header drag is moot: one snap); 3 filter (shipped); 4 desktop chevron; 5 session state for Waiting and the badges. Each ships alone |

## Operator decisions

1. **Half sheet, drag to full.** A swipe up on the bar or a tap on a small grab
   handle opens a sheet over about 60% of the screen; dragging up makes it full
   height. A swipe down on its top edge or handle, or a tap on the backdrop,
   closes it. Android back / system back closes it too.
2. **Content.** The four views as tabs at the top, a filter box under them,
   using the palette's matching (names, current and previous focus titles).
   Rows are vertical: session name, host, current focus title, waiting/stalled
   badge. A row tap opens the session exactly as the pill does today.
3. **Where.** Touch devices only: the touch layout is chosen by
   `(pointer: coarse)` alone, at any width. *(Amended by the owner after round
   1; it first read "below the `lg` breakpoint or on a touch device".)* A
   fine-pointer window, however narrow, keeps today's horizontal bar, plus a
   small chevron that opens the same dock by click.
4. **Gestures.** A swipe starts only on the bar or the grab handle, never in
   the terminal (xterm) or the message box, and is ignored while the
   on-screen keyboard is up.
5. **The handle is the view button.** On touch screens the bar's view button
   becomes the dock's handle: a tap opens the dock, with the view tabs inside,
   replacing the small upward view menu. A swipe up on the bar also opens it.
   There is no separate handle, and the tap target stays at least 44px.
   Desktop keeps today's view menu and adds the chevron.
6. **Recent: 8 on the bar, up to 30 in the dock**, from the same recents
   source ([Recent's extra 22](#recents-extra-22)).
7. **Waiting ships with today's predicate.** `listEntry.waiting` drives the
   Waiting tab and the waiting badge at first. A later phase
   ([Phase 5](#phase-5-session-state-for-waiting-and-the-badges)) switches
   the Waiting tab and the stalled / needs-you badges to session state once
   #426's model lands, giving one definition everywhere.
8. **One snap; the grip closes; the dock's tabs swipe.** *(Owner, after
   Phase 1 shipped.)* Drop full screen entirely: the dock is always the half
   snap (60% of the visual viewport, never less than the header and three
   rows) and its list scrolls inside it. A tap on the grip at its top
   **closes** the dock (it used to toggle half and full), and is named "Close
   sessions". A horizontal swipe on the dock's list moves to the next or
   previous tab ([below](#the-docks-tab-swipe)). This amends decision 1's
   "dragging up makes it full height" and the "no sideways swipe changes a
   tab" rule, for the dock's list only.

## Current state

### The bar

`switcherBar` (`internal/ui/switcher.go:422-520`) draws a `<nav
id="session-switcher">` holding, left to right:

- the **view button** (`switcher-toggle`, `:461-492`), a 44×44 target showing
  only the shown view's icon, which opens the **view menu** popover
  (`switcherViewMenu`, `:554-582`) upward;
- **four rows**, one per view (`switcherRow`, `:685-698`), only the shown one
  visible, each `overflow-x-auto` so a long row scrolls sideways
  (`:94-95`, `:695`);
- the **search** button, which opens the command palette (`:494-511`);
- on a session's page, the **⋯ menu** with Pin / Unpin (`:66-70`, `switcherMoreButton`).

The header comments (`:15-101`) record constraints the dock must keep:

- **Whole names.** A label is the entry's whole name, `":<index>"` and all,
  qualified only when two entries of one view read the same
  (`switcherName`, `sessionSwitcherLabels`, `:205-252`). A pill is as wide as
  its label, with no cap and no ellipsis (`:53-60`, `:717-728`). That is the
  direct cause of the sideways scroll the ask is about.
- **"Current" marking.** In Waiting, Pins and All, the page's own session is
  drawn as `aria-current="page"`, not a link (`:44-45`, `:759-769`). Recent
  leaves it out (`:35-36`).
- **Real-load navigation.** Every entry is an `<a href hx-boost="false">`
  opening what its sidebar row opens (`:47-51`, `:770-782`).
- **No layout shift.** The bar is server-rendered, absolutely placed in room
  its container reserves, fixed height, and invisible until its inline script
  has applied the stored view (`:85-92`, `:438-457`, `:584-601`).
- **44×44 targets** for every icon button (`:529-546`), measured by
  `e2e/tests/layout.spec.ts`.
- **The menus and palette are popovers** (top layer), so opening any moves
  nothing (`:81-83`).
- **It steps aside for the soft keyboard.** `switcherKeyboardJS`
  (`:845-883`) sets `<html data-keyboard>` while a text field has focus and the
  visual viewport is at least 150px shorter than the tallest it has been at
  that width (`:603-633`). CSS then sets the bar to `display: none` where it
  sits in a marked room (`web/css/input.css:553-558`), and on any screen
  30rem tall or less for an in-flow room (`:559-566`).

### Data sources per view

`NewSwitcher` (`internal/ui/switcher.go:271-317`) resolves all four lists on
the server from what `internal/api/recents.go:73-83` passes it: the sidebar's
entries (`agentsListEntries`) and the viewer's recently opened refs.

| View | Source | Order | Cap |
|---|---|---|---|
| Recent | `internal/recents`, server-side and cross-device, newest first; the page's own session left out (`:32-36`, `:296-315`) | newest first | `switcherRecentMax` = 8 (`:141`) |
| Waiting | `listEntry.waiting` (`internal/ui/list_order.go:109-117`): an attention kind filed under the wait token, or an agent awaiting approval (`internal/ui/list_signals.go:138-157`) | `orderList` | none |
| Pins | `internal/pins` (`listEntry.pinned`, `list_order.go:28-33`) | `orderList` | none |
| All | every entry | `orderList` | none |

Each pill carries what the palette searches (`SwitcherItem`, `:175-203`;
`switcherEntry`, `:752-758`): `data-switcher-code`, `data-switcher-title`
(display name and current focus, **joined by " · "**), `data-switcher-words`,
`data-switcher-prev` (recent previous focus titles). The host is folded,
lower-cased, into `data-switcher-words` (`switcherSearchWords`, `:333`, `:342`)
and drawn in the host icon's `title`/`aria-label` (`"Host: …"`,
`internal/ui/host_icons.go:135-160`); neither gives it back as its own
display value, which is why the dock adds `data-switcher-host`.
The waiting dot is `data-switcher-wait` (`:747-748`). **There is no stalled
mark anywhere in the UI today**; see [Badge sources](#badge-sources).

### Where the bar is rendered

| Page | Call | Layout |
|---|---|---|
| A mirrored window's Rendered view | `internal/ui/mirror_compose.go:363`, inside `#mv-compose` | `fixed inset-x-0 bottom-0 z-20` composer (`:257`); the bar is `pinned`, in the composer's reserved room (`:268-284`) |
| An agent's chat page | `internal/ui/agents_detail.go:1013` | inside the page's composer pane, `pinned` false (`:949`); **not** hidden by the keyboard rule (it carries no room mark, `switcher.go:627-628`) |
| The Terminal view of either | `internal/ui/terminal.go:462` (no composer) or through the mirror composer | in flow at the foot of the column, below xterm, room mark `flow` |

### The only gesture code

`internal/ui/components.go:2214-2271` is the mobile sidebar's edge swipe:
two passive `document` listeners, `touchstart` (`:2247`) and `touchend`
(`:2263`). Below 1024px a touch that starts within 28px of the left edge
(`SB_EDGE`, `:2229`), on a closed sidebar, and not on something that owns its
sideways swipes (`ownsSideways`, `:2236-2245`: an ancestor marked
`data-own-swipe`, or one that scrolls sideways) opens it after 45px of mostly
horizontal travel. The bar is marked `data-own-swipe` (`switcher.go:435-437`)
because at a phone's width it starts inside the edge zone.

### Why the tab swipe was removed

`components.go:2222-2228`: a horizontal swipe anywhere over a tab panel used
to walk the tab order. **It was unused, and it fired on gestures meant for
something else**: swiping the recent-agents strip sideways switched the parent
tab. It was removed in `6424ce7d`. `TestNoTabSwitchSwipe`
(`internal/ui/swipe_test.go:47`) pins its absence together with the
survival of the edge swipe, and `e2e/tests/recent-agents.spec.ts:131` pins it
in the browser.

The lesson for the dock is not "no gestures", it is **a gesture must own a
zone nothing else claims, and an axis nothing else in that zone uses**. The
tab swipe owned the whole panel and the horizontal axis, which the strip was
already using. The dock therefore:

- starts a drag only on the bar's row or the sheet's own header;
- uses only the **vertical** axis there, which nothing in those zones uses
  today (the row scrolls sideways; the bar is not inside a vertical scroller);
- ~~never changes a tab by a swipe (dock tabs change by tap or arrow keys only)~~
  — superseded by decision 8: see [The dock's tab swipe](#the-docks-tab-swipe).

### The dock's tab swipe

Decision 8 brings a tab swipe back, and the lesson above is what scopes it.
It owns **one zone, the dock's list** (`#dock-list`, the tabpanel inside the
modal dialog), and **the horizontal axis there, which nothing in that zone
uses**: the list scrolls only vertically (`touch-action: pan-y`), its rows
wrap their names and cut their focus line, and no element in it scrolls
sideways (`switcher-dock.spec.ts` case S asserts that). The recents strip and
every page tabpanel are outside the dialog, so a swipe there is never seen by
it.

- **Bound to the list alone**: three passive listeners (`touchstart`,
  `touchend`, `touchcancel`) on `[data-dock-list]`, never on the document.
  `TestNoTabSwitchSwipe` (`internal/ui/swipe_test.go`) is narrowed, not
  lifted: it keeps a ledger of every touch listener `appScript` registers, on
  any receiver — the sidebar's two on `document`, the dock's three on its
  list — and fails when the set grows or shrinks, and when any of them names
  the page's tab router (`switchTab`, `TABS`, `tabFromPath`, a `#panel-`).
- **Clearly horizontal**: at least 60px across (`dockSwipeMinX`), and more
  than 1.5 times as far across as down (`dockSwipeRatio`), so a scroll of a
  long list, which always drifts sideways a little, never switches the tab.
  These are the removed page swipe's numbers.
- **The arrow keys' path**: leftwards shows the next tab, rightwards the
  previous, through `pick(view, true)`, which the Left / Right keys call: the
  same `aria-selected`, roving `tabindex`, focus on the tab, and the bar moved
  through `musterSwitcherShow`. **No wrap** (the arrow keys wrap): a swipe at
  the last tab stays there, so the edges read as edges.
- **The sidebar's edge swipe stands down** for a touch that starts anywhere in
  the dock: the dialog and the list both carry `data-own-swipe`.

### Prior art in the repo

- **The command palette** (`internal/ui/palette.go`): a popover drawn once per
  document in `#sidebar-root`, outside the sliding panel
  (`internal/ui/components.go:1017-1019`), reading its entries from the bar's
  All row (`palette.go:223-238`), matched by `paletteMatchJS`
  (`:137-212`), with a debounced, cancelled, sequence-checked server search
  (`:252-290`, `internal/api/palette.go:40-80`).
- **The focus history dialog** (`internal/ui/focus_history.go:139-174`): a
  native modal `<dialog>`, bottom sheet on a phone and centred from `sm` up,
  `overscroll-contain` list. The dock is the same component shape with snap
  points and gestures added.

### The Android shell

- **Back.** `ShellBack` is one `OnBackPressedCallback`, enabled exactly while
  the WebView `canGoBack()`, and `handleOnBackPressed` calls `goBack()`
  (`feature/shell/…/ShellBack.kt`). It re-syncs on every
  `doUpdateVisitedHistory` (`ShellWebView.kt:171-173`, `MainActivity.kt:304-306`).
  It consults the page for nothing: Back is WebView history or the system's.
  The design below therefore adds one bridge message rather than a history
  entry ([How back closes it](#how-back-closes-it)).
- **The bridge.** `musterApp` is a `WebViewCompat.addWebMessageListener`
  (`ShellWebView.kt:24`, `:116-125`); every message is a JSON string parsed by
  `BridgeMessage.parse`, which refuses anything not from the **main frame** and
  the paired origin, longer than 1024 characters, or of an unknown `type`
  (`BridgeMessage.kt`). `MainActivity.onBridgeMessage` dispatches the known
  ones (`MainActivity.kt:236-243`).
- **Insets.** The shell pads its container for system bars, cutout **and
  the IME**, and consumes them, so in the shell `env(safe-area-inset-*)` reads
  0 and the WebView itself shrinks when the keyboard opens
  (`ShellWebView.kt:190-206`).
- **Pull to refresh.** `PullGate` refuses a pull that starts on a
  fixed-position element or a dialog (`owner = overlay`), on an element whose
  `touch-action` lacks `pan-y` (`gestures`), or on the terminal and Rendered
  views at all (`PullGate.kt`, `PullProbeScript.kt:86-95`). The dock is both
  fixed and a dialog, and the bar's row will carry `touch-action: pan-x`, so
  neither can start a refresh.

## The dock component

### DOM

```html
<!-- in #sidebar-root, beside the palette: once per document -->
<dialog id="switcher-dock" aria-labelledby="switcher-dock-title"
        data-own-swipe>
  <div data-dock-header>
    <button data-dock-grip aria-label="Close sessions"></button>  <!-- full width, 44px tall; draws a 36×4 handle; a tap closes -->
    <h2 id="switcher-dock-title" class="sr-only">Switch session</h2>
    <input type="search" id="dock-filter" data-dock-filter aria-controls="dock-list"
           aria-label="Filter sessions" enterkeyhint="search"
           class="text-base">                  <!-- 16px: no zoom on focus -->
    <button data-dock-filter-clear aria-label="Clear filter" hidden>×</button>
    <div role="tablist" aria-label="Session views">
      <button role="tab" id="dock-tab-recent" aria-controls="dock-list"
              aria-selected="true">Recent <span>8</span></button>
      … Waiting, Pins, All …
    </div>
    <button data-dock-close aria-label="Close">×</button>
  </div>
  <ul id="dock-list" role="tabpanel" aria-labelledby="dock-tab-recent"
      data-dock-list data-own-swipe>          <!-- the only vertical scroller; the tab swipe's zone -->
    <li><a href="…" hx-boost="false" data-dock-row>…</a></li>
    <li><span aria-current="page" data-dock-row>…</span></li>
  </ul>
  <p data-dock-empty hidden>…</p>
</dialog>
```

**Why `#sidebar-root` and not inside the bar.** Three ancestors of the bar can
hide it, and a top-layer element is still not rendered under a `display:
none` ancestor, and still inherits `visibility: hidden`:

1. `html[data-keyboard] [data-switcher-room] > #session-switcher { display:
   none }` (`web/css/input.css:556-558`). The dock's own filter box raises the
   keyboard, which sets `data-keyboard`: a dock inside the bar would vanish
   the moment the operator typed in it.
2. The short-screen rule (`input.css:559-566`).
3. `#mv-compose` is `invisible` until it is whole (`mirror_compose.go:257`),
   and `visibility` inherits.

`#sidebar-root` already hosts the palette for the same kind of reason
(`components.go:1017-1019`), and every page with the bar has the sidebar
(`palette.go:20-22`). The dock element is drawn on every sidebar page and
stays closed and empty where there is no bar to open it from.

**Rows are built by script at open, from the bar's rows**, with
`textContent` and `setAttribute` only (the palette's rule, `palette.go:33-35`):

- line 1: the waiting or stalled badge, the **whole** label (wrapping with
  `break-words`, never truncated: the bar's whole-name rule), then the host as
  icon plus its name;
- line 2: the current focus title, one line, `truncate` (the focus is not the
  name, so the whole-name rule does not bind it);
- a 56px minimum row height, a full-width target;
- the page's own session as `aria-current="page"`, not a link, accent
  coloured, in the tabs where the bar draws it.

### Layers

| Layer | z | Element |
|---|---|---|
| Top layer (modal) | above all | `#switcher-dock` when open, with `::backdrop` |
| Top layer (popover) | above all | the palette, the view menu, the ⋯ menu |
| 50 | | toasts (`components.go:785`), modals (`agents_detail.go:629`, `chat_index.go:68`) |
| 40 | | the sidebar panel (`components.go:923`), the chief panel, the session drawer |
| 35 | | the PWA update pill (`pwa.go:57-65`) |
| 30 | | the sidebar backdrop, the FABs (`components.go:530-578`) |
| 20 | | the pinned composer `#mv-compose` (`mirror_compose.go:257`) |

A modal dialog sits above every z-index and makes the rest of the document
inert, which is what gives it a free focus trap. Consequences, all intended:

- **Opening the dock closes the view menu, the ⋯ menu and the palette** first
  (they are outside the dialog and would be inert under it). The dock has no
  search button: its filter is the search.
- **Toasts** (z-50, not top layer) draw under the backdrop while the dock is
  open. A toast is transient and the dock is a few seconds' interaction; not
  worth promoting toasts to the top layer.
- **The sidebar** cannot open over the dock: its edge swipe stands down
  because the dock is marked `data-own-swipe` (`ownsSideways`,
  `components.go:2236-2245`), and its button is inert.

### Snap points

**One snap since decision 8: half.** The full snap, its CSS rule and the
grip's "Expand" / "Shrink to half" names are gone. Measured against the
**visual** viewport, kept in CSS custom properties the script writes on
`visualViewport` `resize` and `scroll`:

- `--dock-half`: the half height below;
- `--dock-kb`: `max(0, innerHeight - visualViewport.height - visualViewport.offsetTop)`,
  the part of the layout viewport the keyboard covers (0 where the keyboard
  resizes the page: the Rendered view's `interactive-widget=resizes-content`,
  `mirror_view.go:185`, and the Android shell, which pads for the IME).

| Snap | Height |
|---|---|
| half | `round(0.6 × visualViewport.height)`, never less than the header plus 3 rows |

The sheet is `position: fixed; inset-inline: 0; bottom: var(--dock-kb)` with
that height, and the list's last row is reachable by scrolling inside it.
`--dock-kb` is 0 under a pinch zoom (a `visualViewport.scale` other than 1),
which shrinks the visual viewport without any keyboard. The keyboard reading
is the one shared helper, `keyboardViewportJS` (`internal/ui/keyboard_viewport.go`),
that the Rendered view's pinned composer and the bar's keyboard mark also use
(`TestOneKeyboardMeasure` pins all three). They measure the TOP window inside
the view switch's frame (`kbWin()`); the dock measures its own window, which is
always the top document it is drawn in, also when a frame's bar opens it
(`switcher-dock.spec.ts` case 13k checks both).

### Drag physics

> **Moot since decision 8.** With one snap there is nothing to drag between,
> so the header drag below (half ↔ full, `dockSnap`) will not be built. What
> is left of Phase 2 is the swipe up on the bar to open, if it is still wanted.
> The section is kept as the record of the design.

**Pointer events with pointer capture**, on the two drag zones only. Neither
scrolls vertically, so neither has to share an axis with a scroller. The dock
adds **no `touch*` listener anywhere**, so the sidebar test's extractor
(`internal/ui/swipe_test.go:15-33`, which reads every document-level
`touch…` listener) sees exactly what it sees today.

| Zone | `touch-action` | Events | What a vertical drag does |
|---|---|---|---|
| The bar's shown row (closed) | `pan-x` | `pointerdown/move/up/cancel`, `setPointerCapture` | up: opens, the sheet follows the finger from the bar |
| The dock header (tabs, grip, filter row) | `none` | pointer events, captured | up: toward full; down: toward half, then closed |
| The list | `pan-y` | none | scrolls natively, and nothing else: it never moves or closes the sheet |
| The backdrop | — | `click` with `target === dialog` | closes |

`pan-x` on the bar's row keeps its native sideways scroll and hands vertical
moves to script without a `pointercancel`. That one property is the whole
disambiguation between "scroll the pills" and "open the dock".

**Axis lock.** Nothing moves until the pointer has travelled 10px. Then the
gesture is the dock's only if `|dy| > 1.2 × |dx|` (the edge swipe's ratio,
`components.go:2268`); otherwise it is released to the row or ignored for the
rest of that gesture.

`overscroll-behavior: contain` on the list stops a scroll at either end from
chaining to the page behind.

**Release snaps by position only.** The decision is one pure function,
`dockSnap(from, top)`, where `top` is how much of the sheet is showing when
the finger lifts, exported like `musterPaletteMatch` so a table test can run
it (`e2e/tests/focus.spec.ts` already does this for the palette):

| From | Where it was let go | Result |
|---|---|---|
| closed (bar) | dragged up less than 48px | stays closed |
| closed (bar) | dragged up 48px or more | the nearer of half and full |
| half or full | showing less than half of the half height | closed |
| half or full | otherwise | the nearer of half and full |

No velocity: with two snap points and a close threshold, position alone gives
every outcome, and a velocity term would be one more tuned constant and one
more dimension for the tests to cover. The two thresholds (48px, and half of
the half height) are named constants beside the function, to be tuned on a
phone in Phase 2.

**A drag never clicks.** A drag that started on a pill must not open that
pill. Once the gesture is the dock's, the next `click` in the capture phase is
swallowed. A tap (no travel past the slop) is not a gesture and goes through.

### Open and close animation

`transform` and the backdrop's `opacity`, 220ms, `cubic-bezier(0.2, 0.8, 0.2,
1)`, the easing the bar's popovers use (`web/css/input.css:578-616`), with
`@starting-style` for the opening frame and `overlay`/`display`
`allow-discrete` so the close plays before the dialog leaves the top layer.

**`prefers-reduced-motion: reduce`**: no transition at all, both ways, as the
popovers do (`input.css:617-623`). A drag still follows the finger (direct
manipulation is not animation), and the release jumps to its snap.

### Semantics, focus and keys

- **A dialog, not a "sheet".** ARIA has no sheet role. `showModal()` gives
  `role=dialog`, `aria-modal`, inertness outside, and Escape. Its name is the
  visually hidden "Switch session" heading.
- **Tabs** are an ARIA tablist with roving `tabindex`; Left/Right/Home/End
  move between them. The dock opens on the bar's shown view. Activating a tab
  must also move **the bar that opened it**, in the same document, at once, and
  writing `muster.switcher.view` alone does not: the bar reads storage only as
  it is parsed, and `show()` / `choose()` are private to `switcherJS`
  (`switcher.go:908-923`). So `switcherJS` exports one hook,
  `window.musterSwitcherShow(view)`, which is `choose(view)` (show the row, swap
  the icon, update the menu's checks, store the choice). The dock calls it on
  the source bar's window (the frame's, when the frame opened it) and on the
  top window's own bar if it has one.
- **The list is a list of links**, not a listbox, so a long-press "open in new
  tab" and a screen reader's links list work. Arrow Up/Down from the filter
  move to the first row and between rows; Enter follows.
- **Initial focus.** Opened by touch: the selected tab, **not** the filter,
  so opening never raises the keyboard. Opened by the desktop chevron or a
  keyboard: the filter, so typing filters at once.
- **Escape** with a filter typed **clears the filter** and keeps the dock
  open (the dialog's keydown handler cancels the key, which stops the close
  request); the next Escape (the dialog's `cancel` event) closes through the
  same path as every other close: see [One cleanup point](#one-cleanup-point).
  Closing empties the filter, so the dock reopens unfiltered.
- **The palette's `/` is off while the dock is open.** Its document `keydown`
  (`palette.go:419-435`) already skips a text field, which covers the filter,
  but not the tabs or rows. It gains one early return, `if
  (document.getElementById('switcher-dock')?.open) return;`, so `/` cannot open
  an inert popover behind the modal.
- **The grip is a real button**: full width and 44px tall at the sheet's top,
  drawing the 36×4 handle inside it. **A tap closes the dock** (decision 8),
  through the same cleanup point as every other close; its name is "Close
  sessions".
- **Focus returns** to what opened it: the view button, the chevron, or, for a
  swipe, the view button with `preventScroll`, in whichever document that
  element lives (the frame's, when the frame's bar opened it). Never to a text
  field on touch, which would raise the keyboard.

### Safe areas

- **iOS home indicator, browsers with a gesture bar.** The sheet pads its
  list's foot by `env(safe-area-inset-bottom)` and its sides by the left and
  right insets; full height stops at `env(safe-area-inset-top)`. The bar's
  room already includes the bottom inset (`switcherPinnedPad`,
  `switcher.go:543-545`); **the drag zone is the bar's 44px row, not its
  inset padding**, so a swipe that starts in the system's home-gesture strip
  is never the dock's.
- **The Android shell** reads all insets as 0 because the native container
  already pads for them (`ShellWebView.kt:190-206`); the same CSS is correct
  there with no branch.

### The soft keyboard

**Detection reuses `<html data-keyboard>`**, the one keyboard predicate the bar
already has (`switcherKeyboardJS`, `switcher.go:845-883`): focus in a text
field **and** the visual viewport at least 150px shorter than the tallest seen
at this width, pinch-zoom excluded. The dock adds no second detector.

**The filter box does resize the sheet** (as built, with decision 8's one
snap): on every visual viewport `resize` and `scroll` the sheet is the half of
the now shorter visual viewport (never less than the header and three rows),
sitting on the keyboard (`bottom: var(--dock-kb)`), so the filter, the tabs
and the rows that fit stay above it. Pages differ in how a keyboard changes
the viewport (the agent page resizes only the visual viewport; the Rendered
view and the Android shell resize the layout viewport), and the two custom
properties make that one code path. `switcher-dock.spec.ts` case 13k emulates
a shorter visual viewport; **UNVERIFIED on a real phone keyboard**.

## The four tabs and the filter

### Reuse and new server work

| Piece | Reuse | New |
|---|---|---|
| The four lists | `NewSwitcher`: the bar's rows are the data | Recent's entries 9–30 as an href list ([below](#recents-extra-22)) |
| Name, current marking, order | the pills' `title`, `data-switcher-current`, row order | — |
| Host | — | `data-switcher-host` on every pill (`SwitcherItem.Host` is already there, `switcher.go:184-189`) |
| Current focus title | — | `data-switcher-focus` on every pill (`focusTitle`, `:351-356`); `data-switcher-title` joins it with the display name, so it cannot be split back out reliably |
| Waiting badge | `data-switcher-wait` | — (until Phase 5) |
| Stalled / needs-you badges | — | session state, Phase 5 ([below](#badge-sources)) |
| Matching | `musterPaletteRank` (`palette.go:158-212`), in-page, previous titles from `data-switcher-prev` | — |
| Server search | none in the dock; the palette keeps `/ui/palette/history` | — |

### Server-rendered with the page, or fetched on open?

**Recommendation: neither as a separate copy. Draw the dock's rows, at open,
from the bar's four rows that the page already carries.**

| Option | Cost | Verdict |
|---|---|---|
| **Read from the bar (recommended)** | Two attributes per pill (a host name and a focus title, tens of bytes each, across up to four rows); a few dozen DOM nodes built on open. Zero requests. One source of truth: the bar's bfcache refresh (`RestoreRefreshAttr`, `restore.go:94-98`) and the pin toggle's out-of-band refresh (`switcherRefresh`, `switcher.go:700-715`) already keep it current, and the dock rebuilds every time it opens | **Chosen** |
| Server-render the dock's rows too | Every entry's markup twice per session page, inline host-icon SVG included, in hidden markup the parser must still get through. The page's CLS budget was won against parser stalls inside this very bar (`switcher.go:438-457`). Two copies to keep in step on every refresh path | No |
| Fetch on open | A new route and handler; a round trip at the moment of the gesture, so the sheet either waits or opens on a skeleton and fills (exactly the empty-then-filled the bar forbids itself, `:85-88`); fails when the connection does | No |

The palette already proves the approach: it reads its entries from the bar's
All row (`palette.go:223-238`).

### Recent's extra 22

For decision 6. **The query needs no change.** `recents.PGStore.List`
(`internal/recents/recents.go:55-80`) has no `LIMIT`: it reads every one of the
viewer's rows, newest first, and the table holds at most about
`MaxPerOwner` = 50 per owner because each visit prunes past it (`:32-36`,
`:99-110`). `recentRefs` passes them all on (`internal/api/recents.go:42-64`,
"every stored entry is read so that dropping cannot leave the view short").
The cap of 8 is applied only in `NewSwitcher`'s loop
(`internal/ui/switcher.go:298-301`, `switcherRecentMax`).

**The server change**, all in `NewSwitcher`:

- a second constant, `dockRecentMax = 30`, and the loop collects up to 30
  entries (still skipping gone entries, the page's own session and repeats);
- `Switcher.Recent` stays the first 8, so the bar is unchanged;
- a new `Switcher.RecentMore []string` holds the **hrefs** of entries 9–30, in
  order;
- a Go test pins `MaxPerOwner` above `dockRecentMax`, so the store keeps
  enough rows for 30 even when some of them name sessions that are gone.

**How the dock gets them: the page carries an ordered href list, and the dock
draws those rows from the All row's pills.** Every Recent entry is resolved
against the same entries the All row draws (`byAgent` / `byMirror`,
`switcher.go:272-315`), so each of the extra 22 is already a pill in the All
row, with every attribute a dock row needs. Only their **order** is missing.
The Recent row carries it as one hidden last child,
`<span hidden data-switcher-recent-more="href\nhref…">`, a child rather than
an attribute of the row so that the bfcache refresh (which replaces a region's
children, `restore.go:94-98`) and the pin toggle's out-of-band refresh
(`switcherRefresh`) keep it in step with the rows at no extra cost. Hrefs, not
positions: the palette's server hits already name entries by href
(`musterPaletteRank`, `palette.go:178-212`), and a position would break the
moment the All row's order changed under a refresh.

| Option | Cost per session page | Verdict |
|---|---|---|
| **Href list on the Recent row (recommended)** | at most 22 hrefs, about 1.5KB of attribute text, no markup, no request; a few lines in `NewSwitcher` | **Chosen** |
| Render the extra 22 as hidden rows | 22 more full pills, each with an inline host-icon SVG: on the order of 15–20KB of hidden markup the parser must get through on every session page, inside the bar whose parser stalls were measured as layout shift (`switcher.go:438-457`) | No |
| Fetch the rest on open | a new route and handler, a round trip at the moment of the gesture, and a Recent tab that grows from 8 to 30 after it opens, under the operator's finger | No |

Two consequences, both acceptable and tested:

- **Labels.** A row drawn from an All-row pill carries All's label, which is
  qualified against every entry, not only Recent's. It can therefore be
  *more* qualified than the bar's Recent label ("mango:2 · laptop" where
  Recent alone would say "mango:2"), never less. The whole-name rule holds.
- **The dock's Recent is the bar's 8 followed by the extra 22**, so the two
  can never disagree about the first 8.

### How the filter works

1. The filter matches **the shown tab's** entries with
   `musterPaletteRank(entries, q, [])`, in-page only, so the rank is the
   palette's exactly: codename prefix, codename, title, previous title, words.
   Previous titles are matched through each pill's `data-switcher-prev` (the
   recent ones, `withPreviousTitles`), and a row matched that way shows
   "Previously: …" as the palette's does.
2. **No server search.** The command palette (the bar's magnifier, the
   sidebar's, or `/`) remains the full-history search over every title in the
   retention (`/ui/palette/history`). The dock is for picking from what is on
   the page.
3. A tab with no match says "No sessions match" and nothing more. Every
   tab's count is its matches.
4. Switching tabs keeps the query and re-filters.
5. **As built** (Phase 3): one reading of a pill's search fields,
   `window.musterPaletteEntry` (`palette.go`), shared by the palette and the
   dock, so the two match the same fields: name, display name, current and
   previous focus titles, and the words (repository, kind, tmux session,
   host, and whatever else the switcher folds into `data-switcher-words`).
   Rows are drawn best match first. Clearing: the × button, or Escape (see
   [Semantics, focus and keys](#semantics-focus-and-keys)). ⚠ Not built: the
   "Previously: …" line under a row matched by a previous title (point 1),
   and Arrow Up/Down from the filter into the rows.

### Badge sources

**Phases 1–4 ship with today's waiting predicate and the waiting badge only**
(decision 7). Phase 5 moves both to session state.

- **Waiting**: the pill's `data-switcher-wait`, that is `listEntry.waiting`
  (`list_order.go:109-117`). No change until Phase 5.
- **Stalled and needs-you (Phase 5)**: the session-state work
  (`docs/design/session-state.md` at `6897dcdc`; PR 1 #426 and PR 2 #430 are
  merged) keeps open episodes with `state` `needs_you` or `stalled`
  (`internal/sessionstate/state.go:21-25`). `OpenEpisodes` is "the caller's
  open episodes, oldest first" (`internal/sessionstate/store.go:74-75` at
  `6897dcdc`), so it is already owner-scoped; the switcher maps them to its
  entries by the sweep's `(kind, key)`. One query per page render, added to
  `Server.switcher` (`internal/api/recents.go:73-83`), and a `Stalled bool`
  on `SwitcherItem` drawn as `data-switcher-stalled`. Where there is no
  episode store, or no open episode, there is no badge.
- **One definition (Phase 5).** The change is made in `listEntry.waiting`
  itself, not beside it, so the Waiting tab, the bar's Waiting row, the
  waiting dot, the sidebar's order and its waiting mark (every reader listed
  at `list_signals.go:152-157`) all move to session state at once. A second
  predicate next to the old one would leave the sidebar and the dock
  disagreeing about who is waiting.
  **The sidebar changes when Phase 5 ships** (owner answer): its order and
  its waiting mark follow the new definition along with the bar and the
  dock, so there is one waiting definition everywhere.
- **Where the badges are drawn.** The needs-you and stalled badges are drawn
  **in the dock's rows only**. The bar's pills carry `data-switcher-stalled` as
  data for the dock to read but draw no new badge; their existing waiting dot
  stays, reading the redefined `listEntry.waiting`.
- **Precedence:** one badge per row, needs-you over stalled
  (`session-state.md:230` at `6897dcdc`).

## Gesture rules

### Start zones

In the touch layout only (the mouse layout has no drag), a drag is the dock's
only when its `pointerdown` target is inside one of:

- **closed:** the bar's shown row, `#session-switcher [data-switcher-list]:not([hidden])`,
  or the view button;
- **open:** the dock header `[data-dock-header]` (grab handle, tabs, filter
  row). Never the list.

Everything else is never a start zone, by construction rather than by a
deny-list: the composer (`#chat-input`, `#mv-input`), the quick keys, xterm,
the transcript, the sidebar.

### The grab handle on the bar (decision 5)

**On touch layouts the view button is the handle, and there is no other.** It
keeps its view icon (so the bar still says which view it shows), gains a small
up-caret, and a tap opens the dock on that view's tab instead of the upward
view menu, whose job the dock's tabs take over. Its target stays the 44×44 box
it has today (`switcher.go:529-546`). No width is taken from the pills, and
nothing on the bar moves. A swipe up anywhere on the shown row opens the dock
too. On desktop the view button and its menu are unchanged, and the chevron is
added beside them.

Mechanically, in the touch layout:

- the button's `click` calls `preventDefault()` (which stops its
  `popovertarget` from toggling the menu) and opens the dock, and
  `aria-haspopup` / `aria-controls` name the dock instead of the menu;
- its ArrowUp / ArrowDown, which open the menu today (`switcher.go:973-979`),
  open the dock instead;
- it carries **`touch-action: none`**. Without it a vertical drag that starts
  on the button is the browser's to pan, and the browser answers with a
  `pointercancel` before the dock sees the drag. The button scrolls nothing,
  so `none` costs nothing.

Without script the button still opens the menu natively.

### Coexistence

| Neighbour | Rule |
|---|---|
| The sidebar edge swipe | The bar and the dock both carry `data-own-swipe`, so `ownsSideways` stands the edge swipe down for any touch that starts on either (`components.go:2236-2261`). The dock's axis is vertical and the edge swipe's horizontal, so even a mistake cannot satisfy both |
| The bar's sideways scroll | `touch-action: pan-x` on the row: the browser scrolls it natively; the dock only sees vertical moves |
| xterm | Not a start zone. xterm owns every drag on the terminal |
| The composer | Not a start zone, and its focus is what raises `data-keyboard` |
| The soft keyboard | While `<html data-keyboard>` is set, a drag on the bar is ignored entirely. On the Rendered and Terminal views the bar is already `display: none` then; **on the agent chat page it is still drawn** (no room mark, `switcher.go:627-628`), which is where this check does the work. Inside the open dock, a drag on the header still closes it (closing blurs the filter) |
| Android system gestures | Back (edge swipe from the side) is horizontal and arrives as Back, below. Home (swipe up from the bottom edge) starts in the gesture strip, which the shell pads out of the WebView, and which a browser leaves as the safe-area inset outside the bar's row |
| Android pull to refresh | Never: the dock is fixed and a dialog, and the bar's row has `touch-action` without `pan-y` (`PullProbeScript.kt:86-95`) |
| iOS edge-swipe back | Safari's tab gesture is a history Back, so it leaves the page, dock and all, as Back does today. iOS has no system back, and an installed iOS app has no edge-swipe back, so there is nothing else to wire |

### How back closes it

**Recommendation: the dock uses no history at all** (round 1 option (a)).

**Why not a history entry.** Back from a pushed dock entry lands on the
page's own entry, and that entry is often htmx's: a page reached by a boosted
link carries `{htmx: true, …}`. The session picker (`agents_detail.go:1157-1161`)
and the notification rows (`notifications.go:134-135`) are such links, under
the body's `hx-boost` (`components.go:284`). htmx 2.0.4 then restores, from
`web/static/vendor/htmx.min.js`:

```js
window.onpopstate=function(e){if(e.state&&e.state.htmx){Wt();…}else{if(n){n(e)}}}
function Wt(e){zt();e=e||location.pathname+location.search;const t=Vt(e);
  if(t){…qe(n);Ve(r,n,o);…}else{if(Q.config.refreshOnHistoryMiss){…reload…}else{Gt(e)}}}
```

so a dock close would snapshot the page with the dock open (`zt`), then swap
the whole body from the cache (`Ve`) or with an XHR GET (`Gt`). The two other
options were rejected on that source:

- **(b) Stop the `popstate` in the capture phase, before `window.onpopstate`.**
  It works only by the at-target ordering of capture before non-capture
  listeners, it must never miss (a miss is the body swap above), and a
  Forward press then lands on a dead dock entry. Too fragile for a close.
- **(c) Copy htmx's state into the dock's entry.** The restore is decided by
  the **destination** entry's state, which is the page's, not the dock's. To
  defuse it the page's own entry would have to be rewritten on open and put
  back on close, and a row tap leaves the document before it can be put back,
  leaving a boosted entry that no longer restores.

**What closes it instead:**

| Where | Back reaches the dock by | Status |
|---|---|---|
| Any browser | Escape, the backdrop, ×, the header drag | built with the dock |
| Chrome on Android (tab or installed app) | the modal `<dialog>`'s own close watcher: Android back fires `cancel` on an open modal dialog, which closes through the same path as Escape (Chrome 120 shipped `CloseWatcher` and its `<dialog>` integration). It needs a user activation to open, which both a tap and a swipe's `pointerup` give | **UNVERIFIED** here; Phase 1 check on a phone browser |
| The Android shell | a `back-claim` bridge message ([below](#the-shell-change-muster-android)): the shell offers the press to the page first while the page holds a claim | a muster-android PR shipped alongside Phase 1 |
| iOS | no system back exists; Escape-equivalents only | — |

**A row tap is the pill's link, unchanged**: a real load, no `replace`, so
history after it is `[page, new page]` as after a pill. Because the page can be
kept in the back/forward cache, the dock is **closed without history before the
navigation** (on the row's `click`, then the browser follows the link), so the
cached page is stored closed. And on `pageshow` with `persisted` true, the dock
is force-closed (and its claim released) in case anything left it open.

**No phantom entry anywhere**: with no push, a reload or a restore has nothing
to clean up, and the "one Back press that does nothing visible" case is gone.

**htmx's history cache must never store the dock open.** htmx's save
(`zt` in `web/static/vendor/htmx.min.js`) fires `htmx:beforeHistorySave` and
then stores the history element's cloned HTML (`jt`, `_t`) in
`localStorage`, so a `<dialog open>` would come back from the cache as a
non-modal open dialog. The dock closes on `htmx:beforeHistorySave`, the same
hook `view_swap.go` uses to remove its frame before a save (`:205-211`). This
save also runs when htmx restores on Back (`Wt` calls `zt` first), so a Back
or Forward between boosted entries with the dock open is covered too.

#### One cleanup point

**The dialog's `close` event.** Every path ends in
`dialog.close()`: Escape and Android back in a browser (via `cancel`), the
backdrop, ×, the header drag, `musterBack()`, a row's `click`, `pageshow`
with `persisted`, and `htmx:beforeHistorySave`. The `close` handler alone does
the rest: post `back-claim` `false`, reset the sheet's transform and height,
release pointer capture, and return focus. No path cleans up on its own, so
none can forget a step.

#### The shell change (muster-android)

`ShellBack` today (`feature/shell/…/ShellBack.kt`) is enabled exactly while
`webView.canGoBack()`, and its handler calls `goBack()`. The contract is one
small PR in that repo, **in the plan and shipped alongside Phase 1**
([Phase 1](#phase-1-the-dock-opened-by-tap) lists its files and checks):

1. **The message.** The page posts
   `{"type":"back-claim","open":true}` when the dock opens and
   `{"type":"back-claim","open":false}` when it closes (and on `pagehide`).
   `BridgeMessage.parse` gains `BackClaim(open: Boolean)`: `open` must be a JSON
   boolean, anything else is refused. The existing main-frame and origin checks
   apply unchanged, which is why the dock always opens in the top document
   ([The view-swap frame](#the-view-swap-frame)).
   **A second claimer: the Preview panel's full screen** (in the app, the
   panel's Full screen replaces New tab; `internal/ui/plugin_chip.go`). It
   posts the same pair, through the MAIN frame's bridge also when the panel
   is in the view-swap frame, and wraps the main frame's `musterBack` so a
   press leaves full screen first and otherwise reaches the dock as before.
   The shell holds one boolean, so the two share it: everything behind a
   full-screen panel is inert, so the dock cannot be opened over it by a tap
   and the pair cannot interleave that way.
2. **The state.** `ShellBack` gains `claimed: Boolean`, and
   `isEnabled = claimed || canGoBack()`. So a dock open on the first page of
   history, where `canGoBack()` is false, still gets the press.
3. **The press.** With `claimed`, `handleOnBackPressed` calls
   `webView.evaluateJavascript("window.musterBack ? window.musterBack() : false")`.
   `musterBack()` closes the dock and returns `true` when it closed something.
   If the result is not `"true"` (an old page, a page that already closed),
   the shell clears `claimed` and does what it does today: `goBack()` if it can,
   else it disables itself and re-dispatches the press to the system.
   **A hung page cannot hold the press:** `evaluateJavascript` never answers
   while the page's main thread is stuck, so the shell posts a
   `CLAIM_TIMEOUT_MS = 300` fallback on the main `Handler` when it evaluates.
   Whichever comes first, the answer or the timeout, decides the press, once;
   the timeout clears `claimed` and falls back exactly as a `"false"` answer
   does, and a late answer after it is ignored.
4. **The reset.** `claimed` clears on every main-frame page start
   (`PageEvent.STARTED`), and when the WebView is replaced or its renderer is
   gone (`MainActivity.kt:229`, `:264`, `:535`). **Not on
   `doUpdateVisitedHistory`**: the view swap calls `history.replaceState` on
   the top window whenever its frame loads (`view_swap.go:95`, `:109`, `:171`),
   which would clear a live claim with the dock still open. Page start covers
   every cross-document navigation, a reload included. The one navigation it
   does not see is a same-document one (an htmx boosted swap) that replaces
   the body while a claim is held; but the dock closes on that swap's
   `htmx:beforeHistorySave` and posts `false`, and if that message were lost
   the stale claim still costs no press: the next Back evaluates
   `musterBack()`, the new page's closed dock answers `false`, and the shell
   falls back within the same press. **Bound:** a stale or hung claim delays
   one Back press by at most `CLAIM_TIMEOUT_MS` (300 ms); it never takes the
   press away.
5. **Tests** (Robolectric, beside `ShellBackTest`): a claim with
   `canGoBack()` false enables the callback; a press with a claim evaluates the
   script and does not call `goBack()`; a `"false"` result falls back to
   `goBack()`; **no answer within 300 ms** (Robolectric's `ShadowWebView`
   never runs the callback, so the paused main looper advanced by 300 ms is a
   hung page) falls back to `goBack()`, and an answer arriving after that does
   nothing; a page start clears the claim; a `doUpdateVisitedHistory` does
   not; a `back-claim` from a subframe
   or with `"open":"yes"` is refused (`BridgeAndNavigationTest`).

On muster's side the claim is posted only where `window.musterApp` exists
(the shell), as `appBridgeJS` already checks (`internal/api/devices.go:616-619`).

### The view-swap frame

`internal/ui/view_swap.go` lays the other view (a Terminal view over an agent
page, or the Rendered view and its Terminal) over the page as a same-origin
`<iframe>` (`:16-20`, `:126-176`), and that frame has its own bar. A dock
opened **inside** the frame would be wrong three ways: its navigation is
re-routed (`leave` assigns the parent's location, `:227-229`; a navigation is
intercepted and re-issued in the top window, `:246-276`); its
`visualViewport` is the frame's, which a keyboard never shrinks (the reason
`switcherKeyboardJS` measures the top window, `switcher.go:849-851`); and the
shell's bridge refuses anything not from the main frame.

**So the dock always opens in the top document.** The frame's bar asks the
parent through the same-origin parent call the swap already uses. **Before
calling `preventDefault`**, the view button's handler checks, with
`view_swap.go`'s own `framed` predicate (`:59-60`: a `frameElement` carrying
`data-muster-view` and a parent with `musterViews`), that
`typeof window.parent.musterDock?.open === 'function'`. If the parent has no
`musterDock` (an older parent page under a newer frame, or a cross-version
window during a deploy), the handler does nothing, so the button's
`popovertarget` opens the frame's own **view menu** as it does today, and a
swipe on the frame's bar is ignored. The frame's own dock is never the
fallback: it is exactly the dock the three problems above describe. Otherwise:
`window.parent.musterDock.open(bar)`, where `bar` is the frame's own
`#session-switcher` element and the opener is the element to return focus to.
The top window's dock reads its rows from **that** bar (the frame is the newer
document, and its pin toggle refreshes only its own bar), renders above the
frame (a modal dialog's top layer is above the frame's `z-index:2147483000`),
measures the top `visualViewport`, posts the `back-claim` from the main frame,
and navigates the top window on a row tap. A row for the top page's own
session is handed to `musterViews.showOwn(href)` first, as a frame link is
(`:227-228`), so it shows the view underneath rather than reloading it. The
frame's own dock element stays closed and unused. `musterDock` exists in every
document that draws the sidebar, and both the agent page and the Rendered view
do.

**The dock sits under an inert ancestor while the frame is shown.**
`inertAll` sets `inert` on every child of the top body except the frame
(`view_swap.go:74-82`), `#sidebar-root` included. A modal dialog opened there
still takes taps, focus and Escape: **measured on Chromium 153** (round 2 of
this PR's audit). Older Android WebViews are **UNVERIFIED**; case 20 runs in
the CI's Chromium only. If one is found that keeps the dialog inert,
`musterDock.open` lifts `inert` from its body-level ancestor while open and
restores it on `close`, the way `inertAll` already exempts an open
`#term-confirm` (`:77-79`).

## Desktop chevron

**Recommendation: the same `<dialog>`, laid out in the mouse layout as a
centred panel**, `w-[min(36rem,calc(100vw-2rem))]`, at most 70vh,
with the backdrop. Not a bottom sheet: a sheet the width of a desktop screen is
mostly empty, and the palette and the focus history dialog are already centred
there. No snap points, no drag.

- The chevron is a 44×44 icon button between the shown row and the search
  button, `aria-haspopup="dialog"`, `aria-controls="switcher-dock"`, drawn by
  the server and shown only where the mouse layout applies, by CSS media
  query alone (so it moves nothing at first paint).
- Click opens with focus in the filter; Escape, the backdrop and the close
  button close it; focus returns to the chevron.
- **No new shortcut in this design.** `/` already opens the palette, which
  finds any session. A dock shortcut is easy to add later and is not part of
  the ask.

**Which layout applies** is one media query, used by both the CSS and the
script (`matchMedia`): **touch layout when `(pointer: coarse)`, mouse layout
otherwise**, at any width (decision 3, as amended by the owner, who
re-confirmed "touch pointer only" with the spec facts below): a narrow window
driven by a mouse gets the mouse layout (view menu plus chevron, centred
panel), which works at 390px too.

**What the rule does to the existing specs** (corrected in round 2: an
earlier draft said the pointer rule left all eight untouched, which is false).
The default project is Desktop Chrome at 390px with a fine pointer, so the
specs' default and desktop runs keep the mouse layout. But seven of them also
run a phone variant with `hasTouch: true` / `isMobile: true`, where
`(pointer: coarse)` matches and the view button opens the dock, not
`#switcher-views`. Those phone halves change in Phase 1
([the list](#phase-1-the-dock-opened-by-tap)); `bfcache-restore` (no touch
context) and every desktop half stay as they are. **The touch layout gets the gestures and the mouse
layout gets the chevron, and nothing crosses over**: in the mouse layout the
bar and the dock bind no drag at all, so a mouse that drags across the bar
selects text as it does today, and the dock opens only by the chevron.

Without script the dock never opens, and the bar works as today (its
`<noscript>` rule, `switcher.go:518`).

## Testing

### The harness's blind dimension

`e2e/playwright.config.ts:55-70` runs every spec as **Desktop Chrome at
390×844: a phone's size with `hasTouch` false and a fine pointer.** That
config is structurally blind to touch (no `(pointer: coarse)`, no touch
events) and to desktop widths. The dock's specs therefore set their own
contexts, as `e2e/tests/suggested-replies.spec.ts:351-352` already does,
and run the behaviour that depends on them across a matrix:

| Project | Viewport | `hasTouch` / `isMobile` | Layout it must show |
|---|---|---|---|
| phone | 390×844 | true / true | touch: sheet, swipe |
| narrow mouse | 390×844 | false / false | **mouse** layout: view menu and chevron, centred panel (the default project's shape) |
| touch tablet | 1180×820 | true / true | touch layout, above `lg` |
| desktop | 1280×800 | false / false | mouse: chevron, centred panel |

Every touch spec first asserts `matchMedia('(pointer: coarse)').matches` is
true, and every mouse spec that it is false: the layout is decided by that
query, so a context that did not emulate it would test the wrong layout and
pass.

Drags use `e2e/lib/touch.ts`'s `touchDrag` (CDP `Input.dispatchTouchEvent`),
which goes through Chrome's own gesture handling, `touch-action` included.
Snapping is by position only, so no case depends on the drag's speed.

### Cases

Every case asserts its own **positive control first**: the swipe point is on
the element it claims (as `recent-agents.spec.ts:142-145` checks it is on the
bar), the list really overflows (`scrollHeight > clientHeight`, seeded with
40 sessions), `data-keyboard` really is set.

1. **Swipe up on the bar opens at half**: the dialog is open, its height is
   60% of the visual viewport ±2px, and `history.length` and `history.state`
   are unchanged. A swipe that starts on the view button opens it too (no
   `pointercancel`; `touch-action: none`).
2. **Tap on the view button opens** (touch layout), and does **not** open the
   view menu; ArrowUp on it opens the dock too; on desktop both still open the
   view menu. Picking the Pins tab moves the bar's row to Pins at once (via
   `musterSwitcherShow`), in the same document, without a reload.
3. **Drag the header up from half → full**; a drag that ends nearer half
   returns to half.
4. **Drag the header down from half → closed**; a short drag down from full →
   half; a long one from full → closed.
5. **Backdrop tap closes**; the close button closes; Escape closes. The grip
   is 44px tall and a tap on it closes the dock (decision 8; case G, which
   also asserts no state reaches another snap). With the dock open, `/`
   on a focused tab opens no palette.
6. **The list only scrolls**: with the list at its top, a downward drag on it
   leaves the dock open at its snap and the sheet unmoved; a drag up scrolls
   the list (`scrollTop` grew).
7. **Keyboard up ignores the swipe**, on the agent chat page (where the bar
   stays drawn): focus the composer and shrink the viewport height by 200px at
   the same width, so `switcherKeyboardJS` sets `data-keyboard` (asserted);
   a swipe up on the bar leaves the dock closed. Control: blur, restore the
   height, the same swipe opens it. On the Rendered view, the bar is hidden in
   that state (asserted).
8. **A swipe on the terminal does not open it**: a vertical drag on xterm
   leaves the dock closed. Control: the same drag on that page's bar opens it.
9. **Closing never touches history, including on a boosted page.** Arrive at
   the agent page **through the session picker** (a boosted link, so the
   entry's state has `htmx`; asserted) and put a marker element in the body.
   Open the dock and close it by Escape, the backdrop, × and the header drag in
   turn: each time the marker survives, no `htmx:historyRestore` fired (a
   listener counts them; positive control: a real `page.goBack()` to a boosted
   entry afterwards counts one), and `history.length` is unchanged. A row tap
   then navigates: `history.length` grew by exactly one, and `page.goBack()`
   lands on the agent page with the dock closed, also when the page came from
   the back/forward cache (`pageshow.persisted`, asserted true in a run that
   uses it).
9a. **Back in a phone browser** (manual, Phase 1): Chrome on Android, open the
   dock, press back: the dock closes and the page stays.
9b. **The shell's claim**: a stub `window.musterApp` records messages; opening
   posts `back-claim` `true`, every close posts `false`, `window.musterBack()`
   returns `true` while open and `false` when shut.
10. **A horizontal swipe on the bar** scrolls its row (`scrollLeft` moved) and
    leaves the dock closed.
11. **A swipe that starts on a pill does not open the pill**; a tap still
    does.
12. **The sidebar's edge swipe stands down over the dock**: a rightward drag
    from x = 4 inside the open dock leaves `#sidebar-root` closed. The
    existing edge-swipe test stays green.
13. **Filter**: by codename, by current focus title, and by a previous title
    carried in `data-switcher-prev` (shown as "Previously: …"); the dock makes
    no request to `/ui/palette/history` while filtering (a route counter reads
    0, with a positive control: the palette, typed into the same way, makes
    one).
14. **Desktop chevron**: 1280×800 shows the chevron, click opens a centred
    panel with focus in the filter, Escape closes, focus is back on the
    chevron, the view menu still works; a mouse drag up on the bar opens
    nothing; at 390×844 touch the chevron is not shown.
15. **Reduced motion**: under `page.emulateMedia({ reducedMotion: 'reduce' })`
    the dock's computed `transition-duration` is `0s` both ways. Control:
    without the emulation it is not.
16. **Layout shift**: the existing `cls*.spec.ts` stay green; a closed dock is
    `display: none` and adds nothing to first paint.
17. **Targets**: `layout.spec.ts`'s 44px rule covers the view button as the
    handle, the chevron, the close button, the tabs and the rows.
18. **Recent holds 30**: seed 35 recent visits, two of them to sessions since
    deleted. The bar's Recent row shows exactly 8, the dock's Recent tab shows
    30 in visit order, its first 8 are the bar's, and the deleted ones are
    absent. Control: with 5 visits, both show 5.
19. **The handle replaces the menu on touch only**: in the phone project a tap
    on the view button opens the dock and `#switcher-views` stays closed; in
    the desktop project the same tap opens the menu and not the dock.
20. **The dock inside the Terminal view swap**: from an agent page, open the
    Terminal view (the frame, `data-muster-view`), then open the dock from the
    frame's bar by tap and by swipe. The open dialog is in the **top**
    document and the frame's dock stays closed; its rows are the frame bar's
    (a pin toggled in the frame shows in Pins); with the keyboard emulated in
    the top window the filter snaps to full above it; a row tap navigates the
    top window with `history.length` +1 and leaves no frame; a row for the
    page's own session shows the view underneath without a load; Escape
    returns focus to the view button inside the frame; `back-claim` is posted
    by the top window only. The same from the Rendered view's Terminal swap.
21. **The history cache never holds the dock open**: on an agent page reached
    by a boosted link, open the dock, `page.goBack()` to the previous boosted
    entry (htmx restores it: one `htmx:historyRestore`, asserted), then
    `page.goForward()`: the restored page's `#switcher-dock` is not `open`
    and the body is not inert. Control: with the `htmx:beforeHistorySave`
    close removed, the restored dialog carries `open`.
22. **An older parent page**: open the Terminal view frame, then delete
    `window.musterDock` in the top window (an older parent). A tap on the
    frame's view button opens the frame's `#switcher-views` menu, no
    `<dialog>` is open in either document, and a swipe on the frame's bar
    does nothing. Control: with `musterDock` present, the same tap opens the
    top dock.

Go unit tests: the two new pill attributes (with values the fixtures cannot
produce by accident: a host and a focus title distinct from every name),
`dockSnap`'s table (both thresholds exercised just above and just below),
`TestNoTabSwitchSwipe` unchanged and green.

**Each guard is mutation-checked once**: remove the keyboard check (case 7
goes red), drop `data-own-swipe` from the dock (12), let the list drive the
sheet (6), add a `pushState` on open (9: caught by the unchanged
`history.length` assertion; a `pushState` without a matching `back` never
reaches htmx, so the restore counter would stay 0 and is not the guard), drop
the `htmx:beforeHistorySave` close (21), open the dock in the frame's own
document (20), call `preventDefault` before the `parent.musterDock` check
(22), drop the view button's `touch-action` (1), and confirm that test fails
with its own assertion.

## Phases

Each phase is one muster PR, ships alone, and leaves the bar working as today
for anything it does not touch. Phase 1 also has one companion PR in
muster-android.

### Phase 1: the dock, opened by tap

The `<dialog>` in `#sidebar-root`; tabs; rows read from the bar; the two
pill attributes; Recent to 30 (`dockRecentMax`, `RecentMore`, the href list);
the view button becomes the handle in the touch layout (decision 5); half and full
(the 44px grip toggles them); backdrop, close button, Escape; no history:
the dialog's close watcher for browser back, the `back-claim` /
`musterBack()` pair for the shell; close before a row's navigation and on
`pageshow.persisted`; the dock in the top document for the view-swap frame;
`musterSwitcherShow`; the palette's `/` off while open; the touch layout by
`(pointer: coarse)`; the close on `htmx:beforeHistorySave`; the
`parent.musterDock` check in the frame; one cleanup point (`close`); focus,
`aria`, safe areas, reduced motion.

**The seven existing specs whose phone halves change.** Each opens
`#switcher-views` from `#switcher-toggle` in a `hasTouch: true` context,
which now opens the dock. The change is the same in each: one shared helper
in `e2e/lib/`, `pickView(page, view)`, that opens the menu in the mouse layout
and the dock (then its tab, then closes it) in the touch layout, chosen by
`matchMedia('(pointer: coarse)')`. **Count assertions that read
`#switcher-views [data-switcher-count]` stay**: the menu stays in the DOM in
both layouts, and its counts are refreshed with the rows either way.

| Spec | Phone variant | What changes in it |
|---|---|---|
| `agents-list-ux.spec.ts` | the `phone 390×844 touch` matrix entry (`:264`) | the Waiting pick (`:344-348`) goes through `pickView`; the count read (`:353`) stays |
| `switcher-search.spec.ts` | `phone 390×844` (`:118`) | the menu's own behaviour (its animation, upward placement, arrow navigation and focus, `:129-178`) moves to the desktop half only; the phone half asserts instead that the toggle names the dialog (`aria-haspopup="dialog"`), opens the dock, and that a tab pick sets the bar's icon |
| `terminal.spec.ts` | file-level `hasTouch` (`:17`), and the matrix at `:363` and `:456` | the Recent and All picks (`:295-303`) go through `pickView` |
| `composer-switcher.spec.ts` | `:322`, `:413` | its local `pick` helper (`:42-45`) becomes `pickView` |
| `agents-switcher.spec.ts` | `:145`, `:496` | the picks (`:166-178`, `:543-545`) go through `pickView`; the visibility asserts on the menu (`:167`, `:169`, `:544`) apply in the mouse layout only; the `pins` count reads (`:180`, `:369-392`) stay |
| `pins.spec.ts` | `phone 390×844 touch` (`:156`) | the Pins pick (`:193-194`) goes through `pickView` |
| `uploads.spec.ts` | the `newContext` at `:335` with `hasTouch: vp.mobile` | the All pick (`:390-392`) goes through `pickView` |

`bfcache-restore.spec.ts` (no touch context; it only reads the menu's count,
`:314`) and every desktop half need no change.

**The companion muster-android PR** ([the contract](#the-shell-change-muster-android)),
all under `feature/shell/` and `app/` in [ZacxDev/muster-android][android]:

| File | Change |
|---|---|
| `feature/shell/src/main/kotlin/…/feature/shell/BridgeMessage.kt` | `BackClaim(open: Boolean)`; `"back-claim"` parsed, `open` a JSON boolean or refused; the KDoc's message list gains it |
| `feature/shell/src/main/kotlin/…/feature/shell/ShellBack.kt` | `claimed`; `isEnabled = claimed \|\| canGoBack()`; the claimed press evaluates `window.musterBack`, falling back to `goBack()` or the system on a non-`"true"` answer or after `CLAIM_TIMEOUT_MS` (300 ms) |
| `app/src/main/kotlin/…/MainActivity.kt` | `onBridgeMessage` routes `BackClaim` to `back` (`:236-243`); the claim cleared when the WebView is replaced or its renderer is gone (`:229`, `:264`, `:535`) and on `PageEvent.STARTED` in `onPageEvent` (`:293`), which `ShellWebView` already reports from `onPageStarted` (`ShellWebView.kt:145-154`), so that file needs no change |
| `feature/shell/src/test/kotlin/…/feature/shell/ShellBackTest.kt` | the claim cases: enabled with `canGoBack()` false, evaluates and does not `goBack()`, a `"false"` answer falls back, no answer in 300 ms falls back and a late answer is ignored, a page start clears it, a `doUpdateVisitedHistory` does not |
| `feature/shell/src/test/kotlin/…/feature/shell/BridgeAndNavigationTest.kt` | `back-claim` parsed; refused from a subframe, with `"open":"yes"`, and over 1024 characters |

The two PRs can land in either order: without the app change the page's
`back-claim` is refused as an unknown message and Back leaves the page as
today; without the page change the app never sees a claim and behaves as
today.

Acceptance: cases 2, 5, 9, 9a, 9b, 15, 16, 17, 18, 19, 20 green in the phone
project, and the narrow-mouse and desktop projects unchanged (the view menu
still opens); the seven specs above green with their phone halves on the
dock and their desktop halves unchanged, and `bfcache-restore` green
untouched; cases 21 and 22; the waiting badge only
(decision 7). The muster-android PR's unit tests above green in its CI. **On
an emulator with both PRs: open the dock, press Back, the dock closes and the
page stays; press Back again, the page goes back as before; open the dock on
the first page of history (`canGoBack()` false, from a push tap), press Back,
the dock closes and the app stays open.** Screen-reader pass: the dialog's
name, the tabs, the grip, the current row.

### Phase 2: gestures

> **The header drag is moot (decision 8: one snap, the grip closes).** The
> dock's tab swipe shipped with Phase 3 instead ([The dock's tab
> swipe](#the-docks-tab-swipe)). What remains below, if wanted, is the bar's
> swipe up to open.

`touch-action: pan-x` on the bar's rows; the bar and header drags, touch
layout only; position-only `dockSnap` and its table; the keyboard guard;
click suppression after a drag.

Acceptance: cases 1, 3, 4, 6, 7, 8, 10, 11, 12 green in phone and touch
tablet, each mutation-checked; the constants tuned on a real phone (the
owner's), the values recorded in the PR.

### Phase 3: the filter

> **Shipped**, with decision 8: the filter under the grip (`#dock-filter`,
> 16px, never focused on open), the per-tab counts of matches, "No sessions
> match", × and Escape to clear, emptied on close; the sheet sitting on a
> keyboard at the one snap; the grip closing the dock; the tab swipe on the
> list. Covered by `switcher-dock.spec.ts` cases G, S, 13 and 13k and the Go
> tests in `switcher_dock_test.go` and `swipe_test.go`. The palette's matching
> is shared (`musterPaletteEntry`), not changed.

The in-page filter over the shown tab (`musterPaletteRank`, no server search);
snapping to full above the keyboard. The palette is not touched.

Acceptance: case 13; the palette's own specs (`switcher-search.spec.ts`,
`focus.spec.ts`) unchanged; on a phone, typing
in the filter keeps the tabs, the filter and at least three rows above the
keyboard.

### Phase 4: the desktop chevron

The chevron and the centred layout from `lg` on a fine pointer; the mouse
layout binds no drag.

Acceptance: case 14 in the desktop project; the phone project shows no
chevron.

### Phase 5: session state for Waiting and the badges

**Gate: #426's model in the UI**, that is session-state rollout step 2
(`docs/design/session-state.md:474-489` at `6897dcdc`: states, episodes and
the Attention section's Stalled / waiting rows). **#430 (session-state PR 2)
merged that step at `6897dcdc`**, so Phase 5 is no longer blocked by it; it
still follows Phases 1–4. Until it ships, they run on today's predicate
(decision 7).

The caller's open episodes (`OpenEpisodes`, already owner-scoped) read in `Server.switcher`; `listEntry.waiting`
redefined as "session state `needs-you`", in place, so every reader moves
together (the Waiting tab and row, the waiting dot, **the sidebar's order and
mark, which change with this phase**); `SwitcherItem.Stalled` and
`data-switcher-stalled`; the needs-you and stalled badges in **the dock's rows
only** (the bar's pills draw no new badge).

Acceptance: a Go test that a stalled episode marks exactly its entry and
another owner's episode marks nothing; an idle session the classifier scores
as asking appears in Waiting (red before this phase, green after); the
sidebar's waiting mark and the dock's Waiting tab list the same entries for
one fixture; a needs-you entry with a stalled episode shows the needs-you
badge only; the bar's pills draw no stalled badge; no store, no badge, no
error.

## Open questions

None. The handle, Recent's length and the waiting definition are decided as
[decisions 5–7](#operator-decisions); the layout rule as decision 3, amended.
