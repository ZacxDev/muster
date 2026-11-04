# Design: the switcher dock (a vertical session list from the bottom bar)

**Status: proposed. Nothing here is implemented.** The operator decisions
below, including the three answers to this page's first open questions
(decisions 5–7), are **decided**. Every other section is the recommended design. Where
this page says how muster works today, it cites `file:line` at `main`
`723832da`, and the code is the authority (see [decisions.md](../decisions.md)).
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
| What it is | A native modal `<dialog id="switcher-dock">`, drawn **once per document** beside the command palette, laid out as a bottom sheet on touch/narrow screens and as a centred panel on desktop. The same pattern as the agent page's focus history dialog (`internal/ui/focus_history.go:139-174`) |
| How it opens | Touch: a swipe up that starts on the bar's row, or a tap on the bar's view button, which **is** the handle (decision 5). Desktop: today's view menu, plus a new chevron button |
| Snap points | **Half**, 60% of the visual viewport, and **full**, the visual viewport less the top safe area and an 8px gap |
| How it closes | A drag down on the sheet's header, or on the list when the list is scrolled to its top; a tap on the backdrop; Escape; system Back (`history.pushState` on open, close on `popstate`) |
| Content | Tabs Recent / Waiting / Pins / All, a filter box under them, then vertical rows: whole name, host, current focus title, waiting badge (stalled and needs-you badges in Phase 5). Recent lists up to 30 in the dock, 8 on the bar (decision 6). A row tap is a real load, like a pill |
| Where the data comes from | **The bar already on the page.** Its four rows are server-rendered with every session page; the dock draws its rows from them when it opens. No new request, no second copy of the markup. Two new attributes per pill (host, focus title), and an ordered href list for Recent's entries 9–30 (each one already a pill in the All row) |
| Filter | The palette's rule (`window.musterPaletteMatch` / `musterPaletteRank`) over the shown tab, plus the palette's server search of previous titles, shared through one extracted helper |
| Gesture rules | A drag starts **only** on the bar's row or the sheet's header. Never on xterm, the composer, or anywhere while the soft keyboard is up. No sideways swipe changes a tab, in the dock or anywhere |
| Phases | 1 dock + tap to open + Back + Recent to 30; 2 gestures; 3 filter; 4 desktop chevron; 5 session state for Waiting and the badges. Each ships alone |

## Operator decisions

1. **Half sheet, drag to full.** A swipe up on the bar or a tap on a small grab
   handle opens a sheet over about 60% of the screen; dragging up makes it full
   height. A swipe down on its top edge or handle, or a tap on the backdrop,
   closes it. Android back / system back closes it too.
2. **Content.** The four views as tabs at the top, a filter box under them,
   using the palette's matching (names, current and previous focus titles).
   Rows are vertical: session name, host, current focus title, waiting/stalled
   badge. A row tap opens the session exactly as the pill does today.
3. **Where.** Touch/mobile only: below the `lg` breakpoint or on a touch
   device. Desktop keeps today's horizontal bar, plus a small chevron that
   opens the same dock by click.
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
`data-switcher-prev` (recent previous focus titles). The host is only in the
host icon's `title`/`aria-label` (`"Host: …"`, `internal/ui/host_icons.go:135-160`).
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
- never changes a tab by a swipe (dock tabs change by tap or arrow keys only).

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
  So a page-level `history.pushState` is how a web overlay gets Back, provided
  `doUpdateVisitedHistory` fires for a same-document push. That is the normal
  Chromium WebView behaviour but **UNVERIFIED** here; it is an acceptance check
  of Phase 1.
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
        data-own-swipe data-snap="half">
  <div data-dock-header>                      <!-- the drag zone -->
    <div data-dock-grip aria-hidden="true"></div>   <!-- 36×4 visual handle -->
    <h2 id="switcher-dock-title" class="sr-only">Switch session</h2>
    <div role="tablist" aria-label="Session views">
      <button role="tab" id="dock-tab-recent" aria-controls="dock-list"
              aria-selected="true">Recent <span>8</span></button>
      … Waiting, Pins, All …
    </div>
    <input type="search" id="switcher-dock-filter" aria-controls="dock-list"
           aria-label="Filter sessions by name or focus" enterkeyhint="go"
           class="text-base">                  <!-- 16px: no zoom on focus -->
    <button data-dock-close aria-label="Close">×</button>
  </div>
  <ul id="dock-list" role="tabpanel" aria-labelledby="dock-tab-recent"
      data-dock-list>                         <!-- the only vertical scroller -->
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

Both measured against the **visual** viewport, kept in CSS custom properties
the script writes on `visualViewport` `resize` and `scroll`:

- `--dock-vvh`: `visualViewport.height`;
- `--dock-kb`: `max(0, innerHeight - visualViewport.height - visualViewport.offsetTop)`,
  the part of the layout viewport the keyboard covers (0 where the keyboard
  resizes the page: the Rendered view's `interactive-widget=resizes-content`,
  `mirror_view.go:185`, and the Android shell, which pads for the IME).

| Snap | Height |
|---|---|
| half | `round(0.6 × --dock-vvh)`, never less than the header plus 3 rows |
| full | `--dock-vvh − env(safe-area-inset-top) − 8px` |

The sheet is `position: fixed; inset-inline: 0; bottom: var(--dock-kb)` with
the snap's height. **At rest its height is the snap height**, so the list's
last row is reachable by scrolling at half as at full. During a drag the
sheet is set to the full height once and moved by `transform: translateY()`
only (compositor, no layout per frame); on release it animates the transform
to the target and, on `transitionend`, sets the target height with a zero
transform.

### Drag physics

**Pointer events with pointer capture** for the two drag zones that never
scroll vertically, and **touch events bound on the list element** for the
one that does.

| Zone | `touch-action` | Events | What a vertical drag does |
|---|---|---|---|
| The bar's shown row (closed) | `pan-x` | `pointerdown/move/up/cancel`, `setPointerCapture` | up: opens, the sheet follows the finger from the bar |
| The dock header (tabs, grip, filter row) | `none` | pointer events, captured | up: toward full; down: toward half, then closed |
| The list | `pan-y` | `touchstart` / non-passive `touchmove` **on `#dock-list`** | scrolls; at `scrollTop === 0` a downward move closes (below) |
| The backdrop | — | `click` with `target === dialog` | closes |

`pan-x` on the bar's row keeps its native sideways scroll and hands vertical
moves to script without a `pointercancel`. That one property is the whole
disambiguation between "scroll the pills" and "open the dock".

**Axis lock.** Nothing moves until the pointer has travelled 10px. Then the
gesture is the dock's only if `|dy| > 1.2 × |dx|` (the edge swipe's ratio,
`components.go:2268`); otherwise it is released to the row or ignored for the
rest of that gesture.

**Drag to close from the list.** At `touchstart` the script records whether
the list is at its top. On the first `touchmove` past the slop, if it was at
the top and the move is downward, it calls `preventDefault()` (the listener is
non-passive, on the list only) and drives the sheet from then on; otherwise it
does nothing and the browser scrolls. A scroll that reaches the top
mid-gesture never turns into a close: the decision is made once per gesture.
`overscroll-behavior: contain` on the list stops a scroll at either end from
chaining to the page behind.

> ⚠ **Bind the list's listeners on the list, not on `document`.** The sidebar
> test's extractor (`internal/ui/swipe_test.go:15-33`) reads every
> `document.addEventListener('touch…` up to a `{ passive: true });`
> terminator and fails on one without it. A non-passive document listener
> would break that test for a reason unrelated to what it guards.

**Release.** Velocity is the displacement over the last 100ms of samples
(px/ms). The decision is one pure function, `dockSnap(from, offset, v)`,
exported like `musterPaletteMatch` so a table test can run it
(`e2e/tests/focus.spec.ts` already does this for the palette):

| From | Gesture | Result |
|---|---|---|
| closed (bar) | up ≥ 48px, or `v ≤ −0.3` | half |
| closed (bar) | up past the half height and still moving up (`v ≤ −0.3`) | full |
| half | `v ≤ −0.5`, or up past halfway to full | full |
| half | `v ≥ 0.5`, or down past 50% of the half height | closed |
| full | `v ≥ 1.5`, or down past 75% of the way to closed | closed |
| full | `v ≥ 0.5`, or down past halfway to half | half |
| any | otherwise | the nearest snap to where it was let go |

These numbers are starting points, named constants beside the function, to be
tuned on a phone in Phase 2.

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
  move between them; activating one writes the bar's stored view
  (`muster.switcher.view`, `switcher.go:106-107`) so the bar's row follows the
  dock. The dock opens on the bar's shown view.
- **The list is a list of links**, not a listbox, so a long-press "open in new
  tab" and a screen reader's links list work. Arrow Up/Down from the filter
  move to the first row and between rows; Enter follows.
- **Initial focus.** Opened by touch: the selected tab, **not** the filter,
  so opening never raises the keyboard. Opened by the desktop chevron or a
  keyboard: the filter, so typing filters at once.
- **Escape** (the dialog's `cancel` event) closes through the same path as
  every other close (below).
- **Focus returns** to what opened it: the view button, the chevron, or, for a
  swipe, the view button with `preventScroll`. Never to a text field on touch,
  which would raise the keyboard.

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

**The filter box does resize the sheet.** When the filter has focus and
`data-keyboard` is set, the sheet snaps to **full** of the now shorter visual
viewport, sitting on the keyboard (`bottom: var(--dock-kb)`), so the tabs, the
filter and as many rows as fit stay visible above it. When the keyboard goes,
it stays at full (it does not jump back to half under the finger). Pages
differ in how a keyboard changes the viewport (the agent page resizes only the
visual viewport; the Rendered view and the Android shell resize the layout
viewport), and the two custom properties make that one code path.

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
| Matching | `musterPaletteMatch`, `musterPaletteRank` (`palette.go:158-212`) | — |
| Server search | `/ui/palette/history` (`internal/api/palette.go`) | extract the palette's `ask` (debounce, abort, sequence check, `palette.go:252-290`) into one shared `window.musterPaletteSearch` used by both (one rule, one place) |

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

<a id="recents-extra-22"></a>
### Recent's extra 22 (decision 6)

**The query needs no change.** `recents.PGStore.List`
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
   `musterPaletteRank(entries, q, hits)`, so the rank is the palette's
   exactly: codename prefix, codename, title, previous title, words.
2. Once typing pauses (275ms) on 3 or more characters, the shared search asks
   `/ui/palette/history`; its hits (`{href, title}`) only **raise** entries
   already in the tab, and a row matched that way shows "Previously: …" as the
   palette's does.
3. If the tab has no match but All has some, the empty line says so and
   offers "N matches in All", which switches the tab. Tabs stay meaningful and
   a typed name is never lost behind the wrong tab.
4. Switching tabs keeps the query and re-filters.

### Badge sources

**Phases 1–4 ship with today's waiting predicate and the waiting badge only**
(decision 7). Phase 5 moves both to session state.

- **Waiting**: the pill's `data-switcher-wait`, that is `listEntry.waiting`
  (`list_order.go:109-117`). No change until Phase 5.
- **Stalled and needs-you (Phase 5)**: the session-state work (`docs/design/session-state.md`,
  its PR 1 merged at `723832da`) keeps open episodes with `state = stalled`
  (`internal/sessionstate/state.go:21-25`, `store.go:15-46`).
  `OpenEpisodes` (`store.go:223-226`) reads **every** owner's open episodes,
  so the switcher needs an owner-scoped read (the caller's principal, as the
  palette's server search is scoped, `internal/api/palette.go:21-26`), mapped
  to entries by the sweep's `(kind, key)`. One query per page render, added to
  `Server.switcher` (`internal/api/recents.go:73-83`), and a `Stalled bool`
  on `SwitcherItem` drawn as `data-switcher-stalled`. Where there is no
  episode store, or no open episode, there is no badge.
- **One definition (Phase 5).** The change is made in `listEntry.waiting`
  itself, not beside it, so the Waiting tab, the bar's Waiting row, the
  waiting dot, the sidebar's order and its waiting mark (every reader listed
  at `list_signals.go:152-157`) all move to session state at once. A second
  predicate next to the old one would leave the sidebar and the dock
  disagreeing about who is waiting.
- **Precedence:** one badge per row, needs-you over stalled
  (`session-state.md:230`).

## Gesture rules

### Start zones

A drag is the dock's only when its `pointerdown`/`touchstart` target is
inside one of:

- **closed:** the bar's shown row, `#session-switcher [data-switcher-list]:not([hidden])`,
  or the view button;
- **open:** the dock header `[data-dock-header]`, or the list under the
  scrolled-to-top rule above.

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

Mechanically: in the touch layout the button's `click` calls
`preventDefault()` (which stops its `popovertarget` from toggling the menu)
and opens the dock, and `aria-haspopup` / `aria-controls` name the dock
instead of the menu. Without script the button still opens the menu
natively.

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
| iOS edge-swipe back | Arrives as a history Back, so it closes the dock like Android back |

### How Android back closes it

- **Open:** `history.pushState({musterDock: 1}, '')`, same URL. In the shell
  this makes `canGoBack()` true and `ShellBack` enabled, provided the push
  reaches `doUpdateVisitedHistory` (**UNVERIFIED**, Phase 1 check).
- **Back:** `popstate` with the dock open closes it. Every other close
  (backdrop, Escape, close button, drag) calls `history.back()` and lets the
  same `popstate` handler close it, so there is exactly one close path and no
  stale entry left behind. If the push failed, a close closes directly.
- **A row tap:** `location.replace(href)`, which replaces the dock's entry
  with the new page. History ends `[page, new page]`, exactly as after a pill
  tap, and Back from the new page lands on the page with the dock closed. The
  row stays a real `<a href>` so a long-press, a middle-click or a modified
  click opens it the browser's way (and takes no `replace`).
- **Nothing else reads this state.** htmx 2.0.4 restores only an entry whose
  state has `htmx` (measured in `web/static/vendor/htmx.min.js`). The tab
  router's `popstate` calls `showForPath()` (`components.go:1786`, `:1666-1670`),
  a no-op on a session page, which has no tab panel. The dock's entry carries
  the same URL, so neither router moves.
- **A reload or bfcache restore with the dock's entry current**:
  `replaceState(null, '')` and stay closed. That leaves one Back press that
  does nothing visible, which is acceptable and rare.
- **Desktop** pushes no entry: Back on a desktop browser means "previous
  page", and a centred panel is not the kind of overlay a desktop user backs
  out of.

## Desktop chevron

**Recommendation: the same `<dialog>`, laid out from `lg` up (and on a fine
pointer) as a centred panel**, `w-[min(36rem,calc(100vw-2rem))]`, at most 70vh,
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
script (`matchMedia`): touch layout when `(max-width: 1023.98px), (pointer:
coarse)`, mouse layout otherwise. 1024px is the bar's existing `lg` line
(`components.go:2231`). **Gestures follow the pointer, not the layout**: a
`pointerType === "touch"` drag on the bar works in either layout, so a
touchscreen laptop can swipe too.

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
| narrow mouse | 800×900 | false / false | touch layout by width, opened by tap, no swipe |
| touch tablet | 1180×820 | true / true | touch layout by pointer, above `lg` |
| desktop | 1280×800 | false / false | mouse: chevron, centred panel |

Drags use `e2e/lib/touch.ts`'s `touchDrag` (CDP `Input.dispatchTouchEvent`),
which goes through Chrome's own gesture handling, `touch-action` included. A
helper variant that controls the step timing gives a known release velocity
for the fling cases.

### Cases

Every case asserts its own **positive control first**: the swipe point is on
the element it claims (as `recent-agents.spec.ts:142-145` checks it is on the
bar), the list really overflows (`scrollHeight > clientHeight`, seeded with
40 sessions), `data-keyboard` really is set.

1. **Swipe up on the bar opens at half**: the dialog is open, its height is
   60% of the visual viewport ±2px, `history.length` grew by one.
2. **Tap on the view button opens** (touch layout), and does **not** open the
   view menu; on desktop it still opens the view menu.
3. **Drag the header up from half → full**; a fling from half → full.
4. **Drag down from half → closed**; a short slow drag down from full → half;
   a fast one from full → closed.
5. **Backdrop tap closes**; the close button closes; Escape closes.
6. **The list scrolls without closing**: scroll down, then drag down with
   `scrollTop > 0` → the list scrolls up and the dock stays open; at the top,
   a drag down closes.
7. **Keyboard up ignores the swipe**, on the agent chat page (where the bar
   stays drawn): focus the composer and shrink the viewport height by 200px at
   the same width, so `switcherKeyboardJS` sets `data-keyboard` (asserted);
   a swipe up on the bar leaves the dock closed. Control: blur, restore the
   height, the same swipe opens it. On the Rendered view, the bar is hidden in
   that state (asserted).
8. **A swipe on the terminal does not open it**: a vertical drag on xterm
   leaves the dock closed. Control: the same drag on that page's bar opens it.
9. **Back closes it**: `page.goBack()` closes the dock, the URL is unchanged,
   and a marker set on `window` survives (no navigation). After a backdrop
   close, one `goBack()` reaches the previous page (no phantom entry). After a
   row tap, `goBack()` lands on the original page with the dock closed.
10. **A horizontal swipe on the bar** scrolls its row (`scrollLeft` moved) and
    leaves the dock closed.
11. **A swipe that starts on a pill does not open the pill**; a tap still
    does.
12. **The sidebar's edge swipe stands down over the dock**: a rightward drag
    from x = 4 inside the open dock leaves `#sidebar-root` closed. The
    existing edge-swipe test stays green.
13. **Filter**: by codename, by current focus title, by a previous title from
    the server search (a route stub with a hit), and "N matches in All".
14. **Desktop chevron**: 1280×800 shows the chevron, click opens a centred
    panel with focus in the filter, Escape closes, focus is back on the
    chevron, the view menu still works; at 390×844 touch the chevron is not
    shown.
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

Go unit tests: the two new pill attributes (with values the fixtures cannot
produce by accident: a host and a focus title distinct from every name),
`dockSnap`'s table, `TestNoTabSwitchSwipe` unchanged and green.

**Each guard is mutation-checked once**: remove the keyboard check (case 7
goes red), drop `data-own-swipe` from the dock (12), drop the scrolled-to-top
check (6), drop the `replace` on a row tap (9), and confirm that test fails
with its own assertion.

## Phases

Each phase is one PR, ships alone, and leaves the bar working as today for
anything it does not touch.

### Phase 1: the dock, opened by tap

The `<dialog>` in `#sidebar-root`; tabs; rows read from the bar; the two
pill attributes; Recent to 30 (`dockRecentMax`, `RecentMore`, the href list);
the view button becomes the handle in the touch layout (decision 5); half and full
(a tap on the sheet's grip toggles them); backdrop, close button, Escape;
Back by `pushState`/`popstate`; `location.replace` on a row tap; focus,
`aria`, safe areas, reduced motion.

Acceptance: cases 2, 5, 9, 15, 16, 17, 18, 19 green in the phone and
narrow-mouse projects; the waiting badge only (decision 7); desktop unchanged (the view menu still opens). **On an Android
emulator with the shell: open, press Back, the dock closes and the page
stays** (the `doUpdateVisitedHistory` check). Screen-reader pass: the dialog's
name, the tabs, the current row.

### Phase 2: gestures

`touch-action: pan-x` on the bar's rows; the bar and header drags; the
list's scrolled-to-top close; `dockSnap` and its table; the keyboard guard;
click suppression after a drag.

Acceptance: cases 1, 3, 4, 6, 7, 8, 10, 11, 12 green in phone and touch
tablet, each mutation-checked; the constants tuned on a real phone (the
owner's), the values recorded in the PR.

### Phase 3: the filter

The extracted `musterPaletteSearch` shared with the palette; the filter over
the shown tab; "N matches in All"; snapping to full above the keyboard.

Acceptance: case 13; the palette's own specs (`switcher-search.spec.ts`,
`focus.spec.ts`) unchanged and green after the extraction; on a phone, typing
in the filter keeps the tabs, the filter and at least three rows above the
keyboard.

### Phase 4: the desktop chevron

The chevron and the centred layout from `lg` on a fine pointer.

Acceptance: case 14 in the desktop project; the phone project shows no
chevron.

### Phase 5: session state for Waiting and the badges

**Starts once #426's model reaches the UI**, that is session-state rollout
step 2 (`docs/design/session-state.md:470-480`: states, episodes and the
Attention section). Until then Phases 1–4 run on today's predicate
(decision 7).

The owner-scoped open-episode read in `Server.switcher`; `listEntry.waiting`
redefined as "session state `needs-you`", in place, so every reader moves
together (the Waiting tab and row, the waiting dot, the sidebar's order and
mark); `SwitcherItem.Stalled` and `data-switcher-stalled`; the needs-you and
stalled badges in the dock's rows and the bar's pills.

Acceptance: a Go test that a stalled episode marks exactly its entry and
another owner's episode marks nothing; an idle session the classifier scores
as asking appears in Waiting (red before this phase, green after); the
sidebar's waiting mark and the dock's Waiting tab list the same entries for
one fixture; a needs-you entry with a stalled episode shows the needs-you
badge only; no store, no badge, no error.

## Open questions

None open. The three this page first asked (the handle, Recent's length, the
waiting definition) are decided as [decisions 5–7](#operator-decisions).
