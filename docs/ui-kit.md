# The plugin UI kit

muster serves a small, versioned UI kit so a plugin's own page (an
[app tab](plugins.md#app-tab), a [server app](plugins.md#server-apps), a viewer)
looks like muster and follows the server's theme. The kit is three files and a
stylesheet of tokens, all under one versioned path:

| URL | what it is |
|---|---|
| `/ui-kit/v1/kit.css` | the theme's tokens, then every component class below |
| `/ui-kit/v1/theme.css` | the theme's tokens alone (for a page that styles itself) |
| `/ui-kit/v1/icons.svg` | the icon sprite: one `<symbol>` per [icon](#icons) |
| `/ui-kit/v1/kit.js` | the [behaviours](#kitjs): tooltips, toggles, focus trap, sheets, menus, toasts |

Link the kit **live**, from the muster server the plugin belongs to, and do not
copy it into your build:

```html
<link rel="stylesheet" href="https://muster.example.test/ui-kit/v1/kit.css">
<script src="https://muster.example.test/ui-kit/v1/kit.js" defer></script>
```

A theme an admin saves reaches the plugin on its next page load, with no
rebuild and no redeploy.

## Versioning

- **`v1` is additive only.** A token, a class, a modifier, an icon id, a
  `kit.js` function or option may be **added** to `v1`. None is ever removed,
  renamed, or given a different meaning, and a class keeps the element
  structure it documents here.
- **A breaking change is `v2`**, served beside `v1` at `/ui-kit/v2/`. `v1`
  keeps being served. A plugin moves when it chooses to.
- **Pin the major version, not a build.** The URL names the major version only.
  What is behind it changes with muster releases (additions) and with the
  server's theme.
- `MusterKit.version` is the full kit version (`1.<minor>.<patch>`), for a
  plugin that wants to feature-test an addition.

## What the responses carry

The kit holds nothing about any person. It is the same bytes for every caller,
signed in or not, and muster reads no session and no cookie to serve it.

| header | value |
|---|---|
| `Content-Type` | `text/css; charset=utf-8`, `image/svg+xml`, `text/javascript; charset=utf-8` |
| `ETag` | the first 16 hex digits of a sha256 over the response body. The theme is part of `kit.css` and `theme.css`, so a theme change is a new ETag |
| `Cache-Control` | `?v=<the current etag>`: `public, max-age=31536000, immutable`. No `v`, or any other `v`: `no-cache` (the current bytes, revalidated every time; the ETag answers that with a `304`) |
| `Cross-Origin-Resource-Policy` | `cross-origin`, so a page that sets `Cross-Origin-Embedder-Policy: require-corp` can still load it |
| `X-Content-Type-Options` | `nosniff` |

A plugin links the files unversioned, so every load revalidates and a new theme
shows at once. muster's own pages link the versioned URL.

**No CORS, on purpose.** A `<link rel="stylesheet">` and a `<script src>`
need no CORS header, so any page can use the kit, whether or not its origin
is in the server's `MUSTER_PLUGIN_ORIGINS`. Nothing in the kit has to be read
from script: `kit.js` carries the icon sprite inside itself and puts it in
the page (a browser will not draw `<use href>` from another origin, so a
separate `icons.svg` would need CORS). muster sends no
`Access-Control-Allow-Origin` for the kit, to anyone. `icons.svg` is still
served, for a page on muster's own origin or a tool that wants the sprite as
a file.

### Your page's Content-Security-Policy

If your page sets a CSP, add muster's origin to:

- `style-src` (kit.css),
- `script-src` (kit.js),
- `img-src` (the [brand images](branding.md)).

`kit.js` makes no request, so `connect-src` needs nothing for the kit.

```
Content-Security-Policy: default-src 'self'; style-src 'self' https://muster.example.test;
  script-src 'self' https://muster.example.test; img-src 'self' https://muster.example.test
```

## Tokens

The theme is a set of CSS custom properties on `:root`. Colour tokens are RGB
**channels**, not a colour, so an opacity can be applied:

```css
.my-thing { color: rgb(var(--mu-text)); background: rgb(var(--mu-accent) / .15); }
```

The kit is dark only (muster is). `kit.css` sets `color-scheme: dark`.

### Colour

| token | meaning |
|---|---|
| `--mu-bg` | the page background, the lowest surface |
| `--mu-s1` | a card, a panel, a sheet: one step above the page |
| `--mu-s2` | a raised element on a card: a menu, a hovered row, a secondary button |
| `--mu-s3` | the highest surface: a pressed or selected element on `s2` |
| `--mu-line` | a decorative hairline between regions (not a control's edge) |
| `--mu-edge` | a control's boundary: an input's or a secondary button's outline (3:1 on `bg` and `s1`) |
| `--mu-text` | body text and headings (4.5:1 on every surface) |
| `--mu-text2` | secondary text (4.5:1 on every surface) |
| `--mu-muted` | the least emphasis that is still text: hints, timestamps (4.5:1 on every surface) |
| `--mu-accent` | the brand colour: primary buttons, links, the selected state, a speaking tile's ring (4.5:1 on every surface) |
| `--mu-on-accent` | text and icons on an `accent` fill (4.5:1) |
| `--mu-focus` | the keyboard focus ring (3:1 on `bg`, `s1`, `s2`) |
| `--mu-danger` | destructive actions: delete, leave the call (3:1 on `s1`) |
| `--mu-on-danger` | text and icons on a `danger` fill (4.5:1) |
| `--mu-st-<status>-fg` | a status's label and glyph (4.5:1 on its own `-bg`, 3:1 on `s1`) |
| `--mu-st-<status>-bg` | a status's chip fill |

`<status>` is one of `open`, `progress`, `review`, `complete`, `error`,
`warning`, `running`: 14 tokens.

The contrast ratios in this table are **checked**: a theme that breaks any of
them cannot be saved ([the theme](#the-theme)).

### Shape, type and motion

| token | value in v1 | meaning |
|---|---|---|
| `--mu-radius-sm` | `.375rem` | inputs, buttons, chips' inner elements |
| `--mu-radius-md` | `.5rem` | menus, toasts, tiles |
| `--mu-radius-lg` | `1rem` | cards and sheets |
| `--mu-radius-full` | `9999px` | pills, avatars, icon buttons |
| `--mu-font-sans` | the system UI stack | all text |
| `--mu-font-mono` | the system monospace stack | code, ids |
| `--mu-dur-fast` | `120ms` | a press, a hover, a toggle |
| `--mu-dur-base` | `200ms` | a menu, a tooltip, a toast in |
| `--mu-dur-slow` | `320ms` | a sheet or dialog in or out |
| `--mu-ease-standard` | `cubic-bezier(.2, 0, 0, 1)` | something that moves and stays |
| `--mu-ease-enter` | `cubic-bezier(0, 0, 0, 1)` | something arriving |
| `--mu-ease-exit` | `cubic-bezier(.3, 0, 1, 1)` | something leaving |

**Reduced motion.** Under `prefers-reduced-motion: reduce`, `kit.css` sets the
three `--mu-dur-*` tokens to `0ms`, so every kit transition is instant; the
spinner stops turning and draws a still ring; a speaking tile's ring does not
pulse. Use the duration tokens in your own transitions and you inherit the
rule. `kit.js` reads the same media query and never animates a scroll.

## Components

Every class is prefixed `mu-`. A modifier is `--name`; a part is `__name`. Every
control draws its focus ring (`--mu-focus`) on `:focus-visible`, and is at least
44 × 44 px except where a `--sm` modifier says otherwise.

### Matching muster's own

The kit draws what muster's own pages draw. muster has several button styles;
the kit matches the family its account, admin and sign-in pages share, which
is also the most used one. These pairs are **locked by a browser test**
(`e2e/tests/ui-kit-parity.spec.ts`, at a phone's size and a desktop's): the
computed font size, line height, weight, padding, corner radius, border,
ring, colours and height must be equal, and the test fails when either side
changes.

| kit | muster's own |
|---|---|
| primary button, `mu-btn--primary` | the Account page's **Sign out** (`rounded-md bg-accent px-3 py-2 text-base font-semibold`) |
| secondary button, `mu-btn--secondary` | the branding section's **Reset logo** (`rounded-md bg-bg px-3 py-2 text-base font-semibold ring-1 ring-inset ring-edge`) |
| text field, `mu-input` | the branding section's **Display name** field (`rounded-md bg-bg px-3 py-2 text-base ring-1 ring-inset ring-edge`) |
| card, `mu-card` | an Account page section (`rounded-2xl border border-line bg-s1/70 px-5 py-6 ring-1 ring-line`) |
| icon button, `mu-icon-btn` | the sidebar's **Search sessions** button (`h-11 w-11 rounded-lg text-muted`) |

Matched by value but not by the browser test (muster draws its own only in
places the test cannot open): `mu-chip--<status>` (muster's status chip:
`rounded-full px-2.5 py-1 text-xs font-semibold`, the status colours and a
40% ring) and `mu-menu__item` (muster's header menu item: `rounded-lg px-3
text-sm font-medium`, `s2` on hover). The kit's other components have no muster
equivalent yet.

### Base

| class | what it is |
|---|---|
| `mu-root` | put on `<body>` (or your app's root): the background, text colour, font and `color-scheme` |
| `mu-icon` | an inline `<svg>` drawn from the sprite: `1.25em` square, `currentColor` |
| `mu-visually-hidden` | hidden on screen, read by a screen reader |

### Buttons

| class | what it is |
|---|---|
| `mu-btn` | the base button: a label, optionally a leading `mu-icon` |
| `mu-btn--primary` | `accent` fill, `on-accent` label: the one main action |
| `mu-btn--secondary` | `bg` fill, `edge` outline |
| `mu-btn--ghost` | no fill until hovered |
| `mu-btn--danger` | `danger` fill, `on-danger` label |
| `mu-btn--sm` | a compact button (32 px high, for dense rows only) |
| `mu-btn--block` | full width |
| `mu-icon-btn` | **an icon button**: one `mu-icon`, no visible text: 44 px square with `--mu-radius-md` corners, no fill and `muted` until hovered (as muster's own sidebar icon buttons). **It must carry an `aria-label`**; `kit.js` shows that label (or `data-mu-tooltip`, when set) as its tooltip, and warns in the console about one without a label |
| `mu-icon-btn--danger` | a `danger` icon button (leave the call) |
| `mu-icon-btn--lg` | a 56 px icon button (a call's main controls) |
| `mu-toggle` | **a toggle button**: a `mu-btn` or `mu-icon-btn` that also carries `aria-pressed="true"` or `"false"`. Pressed draws the `s3` fill and an `accent` ring. With `data-mu-toggle`, `kit.js` flips it on click |
| `mu-toggle--alert` | a toggle whose pressed state is a warning, not a selection (a muted microphone): pressed draws `st-error` colours |
| `mu-toolbar` | a row of buttons with `role="toolbar"`: a call's control bar |

A disabled button (`disabled` or `aria-disabled="true"`) draws at reduced
emphasis and does not react to hover.

### Chips and badges

| class | what it is |
|---|---|
| `mu-chip` | a pill label: `s2` fill, `text2` text, an optional leading `mu-icon` |
| `mu-chip--<status>` | a status chip, in that status's `-fg` on `-bg` (`<status>` as in [the tokens](#colour)) |
| `mu-badge` | a small count or dot on a corner of its parent (the parent is `position: relative`) |
| `mu-badge--accent` | the badge in `accent` |
| `mu-badge--danger` | the badge in `danger` |

A status must never be told by colour alone: put the word (or an icon whose
`aria-label` says it) in the chip.

### Cards

| class | what it is |
|---|---|
| `mu-card` | a panel on `s1` at 70%, a `line` border and ring, `--mu-radius-lg` corners (muster's own section card) |
| `mu-card--raised` | the card on `s2` |
| `mu-card__title` | the card's heading |
| `mu-card__body` | the card's text |

### Sheet and dialog

| class | what it is |
|---|---|
| `mu-backdrop` | the dimmed layer behind an open sheet |
| `mu-sheet` | a modal: a bottom sheet below 640 px wide, a centred dialog above. Hidden while it has the `hidden` attribute. Give it `role="dialog"`, `aria-modal="true"` and an `aria-labelledby` |
| `mu-sheet__header` | the title row (a heading and a close `mu-icon-btn`) |
| `mu-sheet__body` | the scrolling content |
| `mu-sheet__footer` | the action row |

Open and close it with `MusterKit.openSheet` and `closeSheet`: they show the
backdrop, trap focus, close on Escape and on the backdrop, and return focus to
what opened it.

### Menu

| class | what it is |
|---|---|
| `mu-menu` | a floating list of actions (`role="menu"`), hidden while `hidden` |
| `mu-menu__item` | one action (`role="menuitem"`, a `<button>`), an optional leading `mu-icon` |
| `mu-menu__item--danger` | a destructive action |
| `mu-menu__sep` | a separator (`role="separator"`) |

A trigger carrying `data-mu-menu="<menu id>"` opens it; `kit.js` sets
`aria-haspopup`, `aria-expanded` and `aria-controls`, moves focus with the
arrow keys, Home and End, and closes on Escape, on a choice and on a click
outside.

### Avatars and tiles

| class | what it is |
|---|---|
| `mu-avatar` | a round picture (`<img>` inside) or initials; 40 px |
| `mu-avatar--sm` | 28 px |
| `mu-avatar--lg` | 64 px |
| `mu-tile` | a 16:9 participant tile on `s2`: a `<video>` or a `mu-avatar` centred in it |
| `mu-tile--speaking` | the tile of whoever is speaking: an `accent` ring |
| `mu-tile__label` | the name, bottom left, on a legible scrim |
| `mu-tile__status` | a row of small icons, top right (a muted microphone, a raised hand, signal strength) |
| `mu-tile-grid` | a responsive grid of tiles |

### Toasts

| class | what it is |
|---|---|
| `mu-toast-region` | the fixed, `aria-live` region toasts appear in (`kit.js` creates it) |
| `mu-toast` | one toast: `s2`, a message, an optional action and a close button |
| `mu-toast--success`, `mu-toast--warning`, `mu-toast--error` | a toast of that kind (an icon and colour; the message says it in words) |

### Spinner

| class | what it is |
|---|---|
| `mu-spinner` | a turning ring in `currentColor`. Give it `role="status"` and an `aria-label` |
| `mu-spinner--lg` | a 40 px spinner |

### Inputs

| class | what it is |
|---|---|
| `mu-field` | a label, its control and its help text, stacked |
| `mu-label` | a control's label |
| `mu-input` | a text input (`bg` fill, `edge` outline) |
| `mu-select` | a `<select>` |
| `mu-textarea` | a `<textarea>` |
| `mu-check` | a checkbox or radio, at 20 px with a 44 px row |
| `mu-switch` | an on/off switch: an `<input type="checkbox" role="switch">` |
| `mu-help` | help text under a control (`muted`) |
| `mu-error` | an error under a control (`st-error-fg`). Point the control's `aria-describedby` at it and set `aria-invalid="true"`: the control then draws its outline in `danger` |

### Tooltip

| class | what it is |
|---|---|
| `mu-tooltip` | the tooltip `kit.js` draws for an element; you do not write it yourself |

## Icons

`icons.svg` holds one `<symbol>` per icon, each `id="mu-<name>"`, drawn on a
24-unit grid in `currentColor`. With `kit.js` loaded:

```html
<button class="mu-icon-btn mu-toggle mu-toggle--alert" aria-label="Mute" aria-pressed="false" data-mu-toggle>
  <svg class="mu-icon" aria-hidden="true"><use href="#mu-mic"/></svg>
</button>
```

or `MusterKit.icon("mic")`. An icon is decoration (`aria-hidden`): the button's
`aria-label`, or text beside it, says what it is.

| name | draws |
|---|---|
| `mic` | a microphone |
| `mic-off` | a struck-through microphone (muted) |
| `camera` | a video camera |
| `camera-off` | a struck-through video camera |
| `screen-share` | a screen with an arrow up |
| `screen-share-stop` | a struck-through screen |
| `leave` | a telephone handset, down: leave the call |
| `phone` | a telephone handset: join or call |
| `people` | two people: participants |
| `chat` | a speech bubble |
| `transcript` | lines of text in a document: a transcript |
| `captions` | a "CC" box: live captions |
| `settings` | a gear |
| `more` | three dots in a row |
| `more-vertical` | three dots in a column |
| `copy` | two overlapping sheets |
| `link` | two chain links: a share link |
| `share` | a box with an arrow out |
| `hand` | a raised hand |
| `check` | a tick |
| `close` | an X |
| `warning` | a triangle with an exclamation mark |
| `info` | a circle with an i |
| `error` | an octagon with an X |
| `signal-0` … `signal-4` | signal strength: four bars, 0 to 4 of them filled |
| `record` | a filled circle: recording |
| `stop` | a filled square |
| `pin` | a pushpin |
| `fullscreen` | four corners out |
| `fullscreen-exit` | four corners in |
| `volume` | a speaker with waves |
| `volume-off` | a struck-through speaker |
| `send` | a paper plane |
| `plus` | a plus |
| `minus` | a minus |
| `search` | a magnifying glass |
| `chevron-up`, `chevron-down`, `chevron-left`, `chevron-right` | a chevron |
| `external` | a box with an arrow leaving it: opens elsewhere |

## kit.js

`kit.js` defines one global, `window.MusterKit`, and wires the `data-mu-*`
attributes below in the document when it loads (and in any element you pass to
`scan`). It uses no framework and makes no request: the icon sprite is inside
it.

| member | what it does |
|---|---|
| `MusterKit.version` | the kit version, `"1.<minor>.<patch>"` |
| `MusterKit.ready` | a Promise that resolves once the icon sprite is in the page |
| `MusterKit.icon(name)` | a new `<svg class="mu-icon" aria-hidden="true">` drawing `name` |
| `MusterKit.tooltip(el, text?)` | shows `text` (default: `el`'s `data-mu-tooltip`, else its `aria-label`) as a tooltip on hover and on keyboard focus, hidden on Escape, blur and leave. Returns a function that removes it. Every `mu-icon-btn` and every `[data-mu-tooltip]` gets one automatically. The tooltip does not replace the accessible name |
| `MusterKit.toggle(el, pressed?)` | sets `el`'s `aria-pressed` to `pressed`, or flips it, and dispatches a bubbling `mu-toggle` event whose `detail.pressed` is the new state. Returns the new state. A click on a `[data-mu-toggle]` does this |
| `MusterKit.trapFocus(container)` | keeps Tab and Shift+Tab inside `container` and focuses its first focusable element. Returns a function that releases it and restores focus to what had it |
| `MusterKit.openSheet(el, {onClose})` | shows a `mu-sheet` with a `mu-backdrop`, traps focus in it, closes it on Escape and on a backdrop click (then calls `onClose`) |
| `MusterKit.closeSheet(el)` | hides it, removes the backdrop, releases focus to what opened it |
| `MusterKit.toast(message, {kind, timeout, action})` | shows a toast. `message` is drawn as **text**, never HTML. `kind` is `"info"` (default), `"success"`, `"warning"` or `"error"`; `timeout` is milliseconds (default `5000`; `0` keeps it until closed; an `error` defaults to `0`); `action` is `{label, onClick}`. Returns a function that dismisses it |
| `MusterKit.scan(root)` | wires the `data-mu-*` attributes inside `root`, for content added after load |

| attribute | effect |
|---|---|
| `data-mu-toggle` | a click flips `aria-pressed` (`MusterKit.toggle`) |
| `data-mu-tooltip="<text>"` | a tooltip with that text |
| `data-mu-menu="<id>"` | the element opens the `mu-menu` with that id |
| `data-mu-sheet="<id>"` | the element opens the `mu-sheet` with that id |
| `data-mu-close` | inside a sheet: a click closes it |

## The theme

The tokens in the kit are the server's **theme**: one of muster's presets,
optionally with the admin's own accent colour. An admin sets it on
**Account → Theme**; muster's own pages and every plugin that links the kit
follow it.

| preset | accent |
|---|---|
| `brass` | muster's own palette, the default |
| `harbor` | blue |
| `verdigris` | green |
| `ember` | coral |

Every preset keeps muster's surfaces, text and status colours; a preset
changes the accent and the two colours drawn from it (`on-accent`, `focus`).
The surfaces stay because the phone's status bar and the installed app's
colour are muster's `bg` and are read before any stylesheet.

**An accent colour** (`#RRGGBB`) replaces the preset's accent. muster derives
`on-accent` (the accent darkened to a near-black label, which for any accent
that passes the check is the better-reading one) and
`focus` (the accent, lighter) itself, then checks the whole theme against the
contrast table above. **A theme that fails any pair is refused at save**, with
the failing pairs named; nothing is stored.

`GET /api/instance` carries the theme:

```json
"theme": {
  "preset": "harbor",
  "accent": "#5AA9F0",
  "etag": "9c41d07a2be3f816",
  "css": "/ui-kit/v1/theme.css?v=9c41d07a2be3f816"
}
```

`accent` is `null` when the preset's own is used. `etag` changes whenever any
token does.
