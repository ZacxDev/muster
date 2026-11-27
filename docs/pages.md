# Shareable pages

An agent can publish a small web page for you: a showcase of something it
built, or a page that collects things from someone else — answers, a choice,
form fields, files, an approval, or whatever its own script sends. You are
shown it at once — a card in the chat and a push — and it is private until it
has a live **share link**; anyone with a live link can open it without a
muster account. Sharing is one tap (**Share**), and an agent's own share
request goes live at once unless you turned off **Let my agents share pages
without asking** — your own choice, on your Account page, on until you turn
it off; off, the request waits for your approval ([below](#when-an-agent-shares)). What viewers send comes back to
the agent as data, and you are notified.

The design, with the threat model and the reasoning, is
[design/pages.md](design/pages.md). This page describes what is built.

## Turning it on

`MUSTER_PAGES=1` ([configuration](configuration.md#muster-server-optional-features)),
with a database. Set `MUSTER_PUBLIC_URL` too, or links are written as paths
that an agent cannot hand on. An `upload` block needs
[attachments](features.md#attachments); without them it is drawn disabled and
says so. The boot banner says `shareable pages: ON (MUSTER_PAGES=1) — …`.

Off, no page tool is offered, the Pages screen answers the typed
`feature_disabled` 404, and every share link answers 404.

## The agents' tools

Offered to every dispatched agent (on its gateway turns and over `POST /mcp`)
and to every mirrored Claude Code session (`POST /mirror/mcp`). A mirrored
opencode session is not offered them: its set is the attention and task tools and `notify_owner`
([attention.md](attention.md#mirrored-opencode-sessions)). A tool acts
only on pages its caller made: a dispatched agent on its own pages, a mirrored
session on the pages made from its **host** (the host is what the mirror token
proves). Any other page reads as `no such page`.

| tool | what it does |
|---|---|
| `page_create{title, blocks}` | Publishes a page (version 1). Returns `page_id` and `url`, [your own view](#the-owners-view) of it, which only you can open, signed in. (`preview_url` is the same address, kept for older callers.) You are notified: *Page ready: <title>*, under the `page_published` toggle. |
| `page_update{page_id, title?, blocks}` | Publishes the next version and makes it current. Links keep working and show it. You are notified of each version: *Page updated: <title> (v<N>)*, opening that version, under its own toggle `page_updated` — at most 6 per 10 minutes from one agent or session, the newer replacing the older on your device ([push.md](push.md#an-agent-notifies-you)). |
| `page_get{page_id}` | Title, versions, links (each one's state and label, never its address), the response count, and the page's [assets](#images-and-video) (`asset_id`, `ref`, name, type, size) with their total. |
| `page_list{}` | The caller's pages. |
| `page_share_request{page_id, reason?}` | Shares the page ([below](#when-an-agent-shares)). For an owner who turned **Let my agents share pages without asking** off: asks you, and returns a pending link's `url` — **once** — with `link_id` and `ask_id` (`status: "pending"`). For an owner who left it on (the default): the page's link is live at once; returns `status: "live"` and its `url` (the same one on every request when the page has a reusable link). |
| `page_responses{page_id, after_id?, wait_seconds?}` | What viewers sent — and what you sent from your own view — oldest first, after `after_id`, as one fenced block of JSON lines. `wait_seconds` (≤ 600) blocks until something arrives. At most 2 waits per caller at once. |
| `page_link_revoke{page_id, link_id}` | Revokes a pending or live link at once. Only the page owner's **Let my agents share pages without asking** (on unless they turned it off) lets a tool make a link live. |
| `page_asset_upload{page_id, path, content_type?}` | Attaches an image or video file to the page ([below](#images-and-video)) and returns its `ref` (`asset:<id>`), which a block then uses. A mirrored Claude Code session's only, for now: a dispatched agent is not offered it (or `page_asset_delete`). |
| `page_asset_delete{page_id, asset_id}` | Deletes an asset no version of the page uses (one a version uses is refused, naming the versions), and its bytes. |

`page_responses` hands the responses over inside a `fence` block whose preface
says they are data and not instructions (a viewer's is a stranger's input).
Each line is `{id, component, kind, version, by_owner, link_id, at, data}` —
`by_owner: true` and no `link_id` for one you sent from your own view — or,
for a file,
`file: {upload_id, name, type, size, url}`, where `url` is a signed `/f/` link
([uploads.md](uploads.md)) valid for the uploads' link lifetime.

## Blocks

A version is a list of blocks, at most 100 and 512 KiB as JSON. An unknown
field, or a field the block's type does not take, is refused with a sentence
naming it.

| block | fields | a viewer sends |
|---|---|---|
| `markdown` | `text` | — |
| `question` | `id`, `label`, `required`, `multiline`, `submit_label` | `{answer}`, at most 4000 characters |
| `choice` | `id`, `label`, `options` (≤ 20), `multiple`, `required`, `submit_label` | `{choices}`, each one of the options |
| `form` | `id`, `label`, `fields` (≤ 30: `name`, `label`, `type` — `text`, `textarea`, `email`, `number`, `date`, `select`, `checkbox` — `required`, `options` for a select), `submit_label` | `{fields}`: known names only, checked by type |
| `upload` | `id`, `label`, `multiple`, `submit_label` | files |
| `approval` | `id`, `label`, `comment` | `{decision: approve\|decline, comment?}` |
| `html` | `html` (≤ 256 KiB) | anything its script sends |
| `gallery` | `id`, `label`, `items` (1 to 200: `asset`, `caption`, `group`), `pick` (`multiple`, `required`, `submit_label`), `layout` | with `pick`: `{picks: [{index, asset, caption}]}` ([below](#gallery)) |

A component's `id` is a letter followed by up to 63 letters, digits, `_` or
`-`, unique on the page; `custom` is reserved. muster checks every submit
against the version it answers — a link's, the **current** version; yours,
the version your view shows — and stores it normalised (a form's
number as a number, a missing optional field as empty).

### Gallery

Many images to compare and pick from, on a phone:

```json
{"type": "gallery", "id": "logo", "label": "Pick the logos to keep",
 "items": [{"asset": "asset:<id>", "caption": "1a", "group": "Round 1"},
           {"asset": "asset:<id>", "caption": "1b", "group": "Round 1"}],
 "pick": {"multiple": true, "required": true, "submit_label": "Save picks"},
 "layout": "grid"}
```

- Each item's `asset` is one of the page's own image [assets](#images-and-video)
  (`asset:<id>`; a video, another page's asset or a missing one refuses the
  version). `caption` is one line of at most 120 characters: the image's alt
  text and its name on its buttons ("Open 1a full size", "Pick 1a") and in a
  pick. A caption or `group` holding anything shaped like an asset reference
  (`asset:` followed by an id) refuses the version: the page's asset
  references are swapped for their images in the rendered markup, so such a
  caption could never be saved with a pick. A heading is drawn wherever an item's `group` differs from the item
  before it, so keep a group's items together. `layout` is `grid`
  (thumbnails, the default) or `rows` (one image per row).
- A tap on a thumbnail opens it **full size**: swipe (or the arrows and ← / →
  on a desktop) to move through every item of the gallery, pinch or
  double-tap to zoom (drag to pan; swiping waits until you zoom back out).
  A small image is enlarged to fill the viewer's width or height. The counter
  ("7 / 24") and the caption are shown. **Back closes the viewer** and stays
  on the page, as do ×, Esc and a tap beside the image.
- With `pick`, each image has a **Pick** button, under it and in the viewer:
  a tap toggles it at once and sends nothing. The bar under the gallery says
  how many are picked, and its **Save** sends them in one submit — in your own
  view recorded as yours (`by_owner`) — after muster's *Send as you?* on a
  page with an `html` block — like every other component. `multiple: false` keeps one pick (a new pick replaces
  it); `required` refuses an empty Save. Unsaved picks survive a reload (per
  browser tab and page version, for up to 16 picking galleries a page); after a Save the bar says *Saved*, and a
  change can be saved again (each Save is its own response).
- A Save is stored as `{"picks": [{"index": 0, "asset": "asset:<id>",
  "caption": "1a"}]}`: `index` counts from 0 in the items' order, picks in
  that order. Very many picks with long captions can pass a submit's 32 KiB
  and are refused.

### The SDK

Every page carries a small script:

```js
muster.submit(data, {component})   // → Promise<{id}>; component defaults to "custom"
muster.upload(file, {component})   // → Promise<{id, name, size, type}>
muster.page                        // {title, version}
```

`custom` is accepted only from a version with an `html` block, and takes any
JSON up to 32 KiB (stored as `{"value": …}`). The built-in components use the
same two calls. In [your own view](#the-owners-view) of a version with an
`html` block (the only kind whose own script can call them) muster first asks
you (*Send as you?*): the call resolves once you tap **Send** (or press a
fresh **Enter**), and rejects with
`Not sent` if you cancel, or `Another send is waiting for your answer.` if
one already is. Data must be plain JSON (objects, arrays, strings, finite
numbers, booleans, null), at most 32 levels deep; `muster.upload` takes a
`File` or `Blob`.

The page's own markup is put in by this script once it is connected to
muster's shell and the document is parsed, so the page's scripts run then,
in order, like `defer` scripts, not while the document is parsed. After they
run, `DOMContentLoaded` is dispatched to `document`, and `load` to `window`
when the window has already loaded (otherwise the real one follows), once
each, so code waiting for either works. What differs from a plain page:
`document.readyState` is already `interactive` or `complete` while your
scripts run; `document.write` cannot add to the page (it replaces it, as
after load); and a `<script type="module">` runs after both events have been
dispatched, so it must not wait for either (put its setup at its top level). Talk to muster only through `muster.submit` and
`muster.upload`: a message posted to the parent window directly is ignored.

Inside the share page's frame the page has **no network**: `fetch`, images
from elsewhere and fonts from elsewhere are refused by its policy. A small
image can be inline as a `data:` URI ([which ones a markdown block
draws](#images-in-a-markdown-block)); a full-size image or a video is an
[asset](#images-and-video). The policy cannot stop a top-level navigation, so the
content is served only into a frame: a browser opening `/s/<token>/content`
on its own (`Sec-Fetch-Dest: document`) gets the link's 404, and only muster's
shell may frame it (`frame-ancestors 'self'`).

## Images and video

The page's author attaches an image or a video **once**, and its blocks refer
to it: a page can then carry full-size review media and short clips that a
`data:` URI (inside the 512 KiB a version may hold) cannot.

| | |
|---|---|
| types | PNG, JPEG, WebP, GIF, MP4, WebM: the type is **sniffed from the file's bytes**, and a declared type (the request's `Content-Type`, `page_asset_upload`'s `content_type`, or the file's extension) must be what the bytes are |
| one asset | at most 15 MiB (or the upload store's `MUSTER_UPLOAD_MAX_BYTES`, if smaller) |
| a page's assets | at most 100 MiB in all, and 200 assets |

**Attaching one**, from where the file is (muster-server never reads an
agent's disk, and the bytes never travel inside an MCP request). For now only
a **mirrored Claude Code session** can: a dispatched agent is not offered the
asset tools, and one that calls them is told so (a follow-up).

- **A mirrored Claude Code session** calls `page_asset_upload{page_id, path}`,
  `path` relative to its working directory. Its host's `muster-mirror` takes
  the path out of the call, resolves it inside the session's working directory
  (after symlinks; a hidden file, a path outside it or a file swapped while it
  is read is refused, as for an ask's attachments), checks the type and size,
  and sends the file to `POST /mirror/pages/{id}/assets` with the host's
  mirror token and the calling session; the call goes on to muster with the
  asset's id, and answers its `ref`. An older `muster-mirror` forwards the path
  and muster answers that the host must be updated.

Muster checks the bytes again whatever sent them, and only the page's
**author** may add one: the route acts for the host that made the page; any
other page — another owner's, or the same owner's made by an agent or another
host — answers 404.
A refused file stores nothing; the two caps refuse with their own codes,
`asset_too_large` (the one asset's) and `page_assets_full` (the page's),
before a byte is read when the length is known.

**Using one:** attach after `page_create` (a new page has no assets yet), then
publish a version whose blocks use the `ref`:

- markdown: `![what it shows](asset:<id>)` draws the image, or a video player
  for a video;
- html: `<img src="asset:<id>">`, `<video src="asset:<id>" controls>`, or the
  reference anywhere in the page's own script.

A version may refer only to its own page's assets: naming another page's (or
one that does not exist) refuses the version, naming it.

### Images in a markdown block

A markdown block draws exactly two kinds of image; every other image syntax
is shown as the text it was written as:

| written | drawn as |
|---|---|
| `![what it shows](asset:<id>)` | the asset: an image, or a video player for a video |
| `![what it shows](data:image/png;base64,…)` — `png`, `jpeg`, `webp` or `gif`, base64 that decodes | an `<img>` of it, at most the column's width |

Not drawn: an `http(s)` address (the frame has no network), an SVG
(`data:image/svg+xml`) or any other `data:` type, a `data:` URI that is not
base64, and the syntax inside a code span or a fenced block (shown as code,
as written). An html block may use any `data:` URI the frame's policy allows.
An inline image counts toward the 512 KiB a version may hold; a large one is
an asset.

### Written for a phone

A page is read on a phone more often than not. A markdown block's text wraps
wherever a line cannot otherwise fit (a long code span, URL or unbroken run),
images never exceed the column, and a table keeps each column at least 6rem
wide and scrolls inside its own box when it is wider than the screen.

`page_create` and `page_update` answer each published version with
`phone_notes` when a markdown block has a shape that still reads badly: a
table of more than 5 columns, an inline code span of more than 60
characters, or an image the block cannot draw. Each note names the block
(`blocks[i]`, as the call's array) and what to change. The version is
published either way; a version with nothing to say has no `phone_notes`.

**How it is shown.** The frame's policy is not widened: the page still has no
network. Muster's own shell — on muster's origin, outside the frame — fetches
each asset the version uses, through a link's token (`GET
/s/<token>/assets/<id>`, no credentials) or the owner's session (`GET
/pages/{id}/versions/{v}/assets/<id>`), and hands the bytes to the frame on
its private channel; the frame makes a `blob:` URL of each.

**The page renders at once.** Its markup goes in straight away, each
`asset:<id>` replaced by a **placeholder** — a `data:` URL of a 1×1
transparent image ending `#muster-asset-<id>` — on an element marked
`data-muster-asset="loading"`, drawn as a 16:9 box at the full width. As each
asset arrives, every `src` or `poster` still holding its placeholder is set to
the asset's `blob:` URL and the mark is removed (a `<source>`'s video is
reloaded), so the element's own `load` or `loadedmetadata` follows. An image of
another shape than 16:9 moves the layout once, when it arrives. One that
could not be loaded keeps its placeholder, marked `data-muster-asset="failed"`.
⚠ **A page's own script that reads an asset before it arrives sees the
placeholder**, not the asset: wait for the element's `load`
(`loadedmetadata` for a video), or read its `src` again after. An element the
script creates with the reference AFTER that asset has arrived keeps the
placeholder: create media from the markup, or before the page's `load`.

**Cached.** An asset's id never names other bytes, so each response is
`Cache-Control: private, max-age=31536000, immutable`, and the shell's fetch
uses the browser's cache: a repeat view does not fetch it again. Through a
link, each viewer's reads are capped (per link and client address, so one
viewer never throttles another), and so are the bytes all of a link's viewers
draw together ([Limits](#limits)); either answers 429 with `Retry-After`, in
its own words.

⚠ **On plain HTTP, an asset the owner's view cached outlives signing out.**
It is cached in that browser for up to a year (`private`: no shared cache
keeps it). Signing out sends `Clear-Site-Data: "cache"`, which empties it —
but a browser honours that only on a secure origin (HTTPS, or localhost), so
on a muster served over plain HTTP the owner's cached media stays in that
browser after sign-out. On a shared computer, serve muster over HTTPS, or
clear the browser's cache.

**Who can read one:** exactly who can read the page. A link serves only an
asset its page's **current** version uses (a revoked link serves none); the
owner's view serves an asset the version it shows uses. Another owner, and a
link of another page, get the one 404. Each response is served as every
uploaded file is ([uploads.md](uploads.md#serving-uploaded-files)), and
`Cross-Origin-Resource-Policy: same-origin`.

**How long it lives:** while any version of the page uses it — an older
version keeps its assets, which the owner's view of that version still shows
— and `page_asset_delete` refuses one in use. Deleting the page deletes every
asset with it (their bytes are under the page's own prefix, [below](#files)).

## Styling: theme and width

A page is drawn in **the server's theme**: muster's dark palette, with the
preset and accent an admin picked on Account → Theme
([ui-kit.md](ui-kit.md#the-theme)). Every document a page is served as — the
content and the shell, through a link and in the owner's view (a session's
Pages tab included) — inlines the same theme block `/ui-kit/v1/theme.css`
serves (the content's policy loads no stylesheet), so the page follows the
theme the moment it changes. The document depends on the server's theme only,
never on who is looking: a viewer's device or colour-scheme preference changes
nothing.

**Use the tokens, not colour literals.** The content defines these CSS
variables on its root, each following the theme:

| Token | For |
|---|---|
| `--bg` | the page's ground (`body` already has it) |
| `--s1`, `--s2` | a card or panel's surface; a raised one inside it |
| `--line` | borders, dividers, table rules, a stripe |
| `--edge` | an input's border (stronger than `--line`) |
| `--fg` | text (`body` already has it) |
| `--fg2` | secondary text: captions, hints |
| `--accent` | emphasis, a link, a button's fill |
| `--on-accent` | text on an `--accent` fill |
| `--good`, `--bad` | a success or an error message |

```html
<style>
  .card { background: var(--s1); border: 1px solid var(--line); border-radius: 12px; padding: 16px; }
  .card h2 { color: var(--fg); } .card small { color: var(--fg2); }
  table { width: 100%; border-collapse: collapse; }
  td, th { border-bottom: 1px solid var(--line); padding: 6px 8px; text-align: left; }
  .go { background: var(--accent); color: var(--on-accent); }
</style>
```

Don't set the page's background or its text colour with a literal: a
`background: #fff` is a white patch on a dark page, and `color: #111` with no
background of its own is unreadable on it. `page_create` and `page_update`
answer such a version with **`theme_notes`** (beside `phone_notes`), naming
the declaration and the token to use instead; the version is published either way.

**Full width.** The content is the frame's full width, for every block, with
a 16px gutter each side and room for the shell's header at the top: there is
no maximum width. A page that wants a narrower column sets one on its own
markup (`max-width: 44rem; margin: 0 auto`).

## Share links

A link is `https://<MUSTER_PUBLIC_URL>/s/<token>` (or under
`MUSTER_PAGES_PUBLIC_URL`, [for people outside your
network](#sharing-with-people-outside-your-network)), where the token is 256
random bits. It serves the page's current version until it is revoked. A link
that is unknown, waiting for approval, declined or revoked answers 404 with one
body, whichever it is.

A viewer's request is looked up by the token's **sha256**, compared in
constant time; the token itself is never the lookup.

### Sharing

**Share** — on the page's card in the chat, each row of `/pages` and the
page's screen — is one tap. It asks muster for the page's **one reusable
link** (making it on the first tap) and hands it to your phone's **share
sheet** (`navigator.share`, with the page's title); cancelling the sheet does
nothing. Where the browser has no share sheet, or it fails, the link is
**copied**, with a *Link copied* toast (and if the browser refuses the copy
too, the toast shows the link to copy by hand). Every later Share gives the
**same** link. The **⋯** menu beside it has **Revoke link** (it stops working
at once) and **New link** (the old link stops working and a new one replaces
it, in one step).

🔴 **The reusable link's token is kept encrypted at rest** (AES-256-GCM, a key
derived from `MUSTER_CONNECTOR_ENCRYPTION_KEY` and bound to the link's owner,
page and token hash) so Share can show it again; it is wiped the moment the
link is revoked. **Without that key** there are no reusable links, and Share
still works: each tap makes a new **one-off** (hash-only) live link and shares
or copies it — its address is in that answer only — so the page's screen
lists one link per tap; revoke the ones you no longer need there.

With the key, the address is fetched **as your finger goes down** on Share
(pointerdown, or focus from a keyboard), before the tap completes, so the tap
itself can open the share sheet at once — iOS Safari opens it only from inside
the tap. A press that turns into a scroll has made (or found) the page's ONE
link and given it to nobody; the card then says the page has a live link.
**Without the key there is no such fetch**: each ask would mint another
one-off live link, so a press or a tab-through would leave links nobody holds
(and fill the page's 10-link cap, which also refuses an agent's request) —
there the tap alone makes the link. If the tap still beats the fetch, the link is copied in the same
tap and the sheet is tried when it arrives; if the browser refuses it, the
toast says the link is copied, and the next tap shares at once. The Pages
screens draw the address on the button already. On a server without the key,
the ⓘ beside Share on the page's screen says so (*Sharing isn't set up on this
server*) and what Share does there; the setting it needs is in that ⓘ for an
admin only (anyone else is told to ask their admin).

**Older links** — every link made before reusable links existed, an agent's
link you approved on Asks, and every link made on a server without the key —
are **hash-only**: muster stores only the token's sha256, so such a link was
shown once, to whoever made it, and cannot be shown again. They keep working
and can be revoked; the page's screen labels them *an earlier link, which can't
be copied again*. Share does not reuse them — it makes the page's
reusable link beside them. ⚠ An agent's link **is** in that agent's
transcript, and so with its model provider: `page_share_request`'s result
carries it. Treat a link an agent shared as shared with whatever sees that
session.

### Sharing with people outside your network

A default install is reachable only over your own network (your tailnet), so
a link under `MUSTER_PUBLIC_URL` does not open for a client outside it. To
let it, turn on the **public pages listener**: a port of its own that serves
share links and nothing else.

```sh
MUSTER_PAGES_PUBLIC_LISTEN=:8108
MUSTER_PAGES_PUBLIC_URL=https://pages.example.com
```

([configuration](configuration.md#muster-server-optional-features)). Then
**you** make that port reachable from outside: a reverse proxy, a tunnel or
Tailscale Funnel pointed at it. muster only opens the port. Keep everything
else as it is: the rest of muster stays on your network.

- **What it serves**: a link's page (`/s/<token>`), its content and its
  images and video, a viewer's submit and upload, and the shell's logo and
  icon. That list is one list in the code (`api.PagesPublicRoutes`). Every
  other path is a 404 there, including `/`, `/api/`, sign-in, `/plugin/`,
  MCP, `/health` and `/metrics`.
- **No credential crosses it.** Only a short list of request headers is
  passed on (`api.PagesPublicRequestHeaders`: the body's type and length,
  `Accept*`, the caching headers, `Range`, `Origin`, `Referer`,
  `User-Agent` and `Sec-Fetch-*`, plus your `MUSTER_CLIENT_IP_HEADER`, which
  muster believes only from `MUSTER_TRUSTED_PROXIES`). Every other header,
  cookies included, is removed before muster sees the request, and no
  response sets a cookie, so your own signed-in browser gets nothing more
  there than a stranger does. There is no CORS on it.
- **A slow client is cut off.** The listener has its own timeouts: 30 s to
  read a request (a submit has 15 s for its body, an upload its own minute),
  2 minutes to answer it, and 1 minute for an idle connection.
- **The links you hand on** (Share, an agent's `page_share_request`, the
  page's screen) are written under `MUSTER_PAGES_PUBLIC_URL`. Your own view
  of a page and every other link stay under `MUSTER_PUBLIC_URL`. A token
  works on both listeners, so a link handed on earlier keeps working on your
  network.
- **The same limits apply**: the rate limits, caps and refusals in
  [Limits](#limits) are the share routes' own, on either listener. They are
  counted per client address, so behind a proxy or tunnel set
  [`MUSTER_TRUSTED_PROXIES`](configuration.md#client-addresses-behind-a-proxy),
  or every viewer shares the proxy's one address. One difference: on this
  listener an address past the failed-lookup limit is answered 429 for the
  minute **before** its link is looked up, so it cannot open even a live
  link until then. On your own network a throttled address still opens a
  live link, and only its misses are refused.
- Prefer a hostname of its own for `MUSTER_PAGES_PUBLIC_URL`, not muster's:
  a page link's shell then shares no origin with your muster.

### When an agent shares

What an agent's `page_share_request` does is **the page owner's own choice**,
the switch **Let my agents share pages without asking** in the Pages section of
their Account page. It is per user, **on for every user until they turn it
off**, and only they can flip it (a signed-in, same-origin write of their own
row; no agent or session token reaches it). It is not a server setting. A
choice already saved is kept: a user who had it off (including one whose
preferences row was written, off, before the default changed) stays off until
they turn it on. A team's pages, if a team-owned agent makes one, ALWAYS get the ask
flow, whatever is stored: nobody signs in as a team, so nobody could turn it
off for one. And a session on a machine the [step-up gate](design/officers-on-hosts.md)
covers — one that runs a host officer or starts agents for the chief, or a
session mirrored to another muster — ALWAYS asks, even with the switch on: any
process on such a machine can name your session, so its request proves
nothing about who sent it.

- **Off** — the agent asks, and you approve, below.
- **On** (everyone's default) — for your pages the link goes **live at once**, and the agent is given
  its address: the page's reusable link (the one you may already have shared,
  or a new one the agent made), or, without the key, a new hash-only link. No
  question is filed. 🔴 **No one approves it**: anything the agent publishes is
  public to whoever it gives the link, before you have looked. You are
  **told**: a push (*Page shared: <title>*, opening the page's screen), the
  chat's page card says *Shared by <agent>*, and the page's screen lists the
  link as *made live by <agent> without asking you*, with Revoke beside it.
  An agent can still share only a page it made for you — never another
  agent's page, never another member's.

While it is off:

- **An agent asks** with `page_share_request`. That files a **pending** link
  and a question for you on the Asks page and your phone: *Share the page
  “<title>”? <agent> asks to let anyone who has its link open it.*, saying the
  link always shows the page's latest version — a version the agent publishes
  after you approve is shown with no new approval — with **Open the page**
  ([your own view](#the-owners-view) of it, one tap away; the question holds no
  address) and **Approve link** /
  **Not now**. Approve link makes the link live; Not now declines it for good.
  Only you can decide it, in muster or on your paired phone: a mirrored
  session's `attention_answer` is refused, and so is any answer that is not one
  of the two. A dispatched agent is told your answer as a new message; a
  mirrored session reads it with `attention_wait{id: ask_id}`. The agent got
  the link's `url` when it asked; it opens the page once you approve.
- **You share it yourself** with **Share** (above), whatever the switch says.
- **Revoking** (on the page's screen, or `page_link_revoke`) stops it at once
  and withdraws its card if it was still waiting.

At most 10 links per page may be pending or live together.

## The page itself

A link's viewer and you see the same page under the same header: one slim
row (about 40px) with the instance's logo and name, and the page's title on
one line, cut short with an ellipsis (the whole title shows on hover). The
header slides away as the page is scrolled down and comes back on any scroll
up, whenever the page is near its top, on its own 3 seconds after the page
last scrolled further down, and at most 8 seconds after it hid, whatever the
page does — after which it hides again only on a fresh scroll down of more
than its own height; it is always there when the page opens. The page cannot hide it on its own: the frame reports its scroll on a
private channel the page's own script never holds (it is connected before any
of the page's script runs), and the shell only accepts
a number there.

### The owner's view

`/pages/{id}/view` (any version with `?v=N`) is the page as a viewer sees it,
signed in as you. **What you send from it is recorded**, as yours: a form, an
answer, a file go through the same checks, size limits and rate limits as a
link's, and show on the page's screen and in `page_responses` as yours
(`by_owner`), not a link's. A response from an older version (`?v=N`) answers
that version: it is checked against version N's components and recorded with
its number. Your own responses do not notify you.

**A page with no `html` block sends at once.** When the version you are
looking at has no `html` block, nothing but muster's own script runs in it,
so every send from it is your own press of one of muster's components:
**Approve**, an answer's **Send**, a Save — and it is recorded at once, with
no bar (the operator's decision, to make approving asks quick). It goes
through the same route, checks and size cap as below. The flip side: on such
a page one click records, so a mis-click records too (send again, or tell the
agent). muster decides this when it draws your view, from that version's
blocks; nothing in the page can turn the bar off.

🔴 **On a page with an `html` block, nothing is sent as you until you confirm
it.** Every send from your view of such a version — a form, an answer, a
file, or something the page's own script sends, the moment it opens or
later — first shows muster's own **Send as you?** bar at the bottom of the
screen (a sheet on a phone, a box at the bottom centre on a wider screen).
It says which component it is for and shows what will be sent: **every
field**, one per line, each name and value written as JSON (`"email":
"me@example.com"`), so a line break, a control or an invisible or
direction-changing character shows as its escape (`\n`, `\u202e`) instead of
faking a second line or reordering the text; the box scrolls. For files, each
file's name (written the same way) and size. Nothing is ever cut: a send too
long to show whole is refused (*This is too much to show you before sending;
nothing was sent.*). The data is shown after muster has turned it into JSON
— what JSON cannot carry is gone, a date is its ISO text — so **Send**
records exactly what you read. **Cancel** records nothing, and the page is
told *Not sent*; so does **Escape** while the keyboard is in muster's part
of the screen (if it is in the page, muster cannot see the key: tap Cancel).
**Enter** sends while the keyboard is in the bar (it takes the keyboard when
it opens): only a plain, fresh press after the bar's first half second. Any
other key in the bar — a letter, Shift, Ctrl, a held Enter — starts the half
second again, and an Enter anywhere else on the screen (the Feedback box,
the version button) never sends, so the press that opened the bar, or typing
meant for something else, cannot also confirm it. Mind the flip side: once
the bar has been open for half a second, one Enter confirms it.
Left alone for 10 minutes, the bar cancels itself. The bar is muster's,
outside the page: the page's script cannot reach it, press it or change
what it shows. Send is never focused when the bar opens, and a press of the
bar in its first half second does nothing and starts that half second
again, so a burst of taps that opened the bar cannot also press Send; Send
then takes a press that begins on it. One send waits at a time — another,
while the bar is open, is refused (*Another send is waiting for your
answer.*). A viewer through a link sees no bar: what they send is recorded
as before, turned into JSON the same way.

### Answering by keyboard

muster's own components can be answered without the mouse, on any page:

- **Tab** walks every control in page order: each option of a choice (a
  single choice is one stop; the arrow keys move between its options), the
  fields, **Approve** and **Decline**, a gallery's **Pick** buttons, then the
  component's submit. The control with the keyboard has a visible ring.
- **Space** picks the option or presses the button that has the keyboard.
  **Approve** and **Decline** send that decision at once (one press, one
  decision, as a click).
- **Ctrl+Enter** (**⌘+Enter** on a Mac) anywhere in a component — in a text
  box too — sends that component, as its submit button would; the hint beside
  each submit says so. In an approval it sends the decision that has the
  keyboard (anywhere else in it, it asks you to choose). A held key sends
  once. **Shift+Enter** in a multi-line answer is a new line.
- After a send, the keyboard moves on to the next component below that you
  have not sent from this view, scrolled into sight. For half a second its
  first control ignores a press (a double press or double tap never answers
  two asks); typing in it starts at once. On a page without an
  `html` block that is the whole loop: Space, Ctrl+Enter, the next ask. On a
  page with one, Ctrl+Enter opens **Send as you?**, and a fresh **Enter**
  after its half second sends.

Your header starts with **breadcrumbs** where the instance's name would be:
**Pages ›**, a tap back to the Pages screen, and on a team's page the team's
name after it (as words: there is no screen of one team's pages). On a narrow
screen the team's name is cut short first, then the title; the header stays
one row.

**Tap the title** to read all of it: a small popover with the whole title,
wrapped, then *Page #12 · v3 · updated today at 9:14* (the page's id, the
version you are viewing, and when the page last changed, in your own time
zone), and **Copy ID**, which puts the id on your clipboard.

Your header also has the **version chip** (`v3 ▾`). It opens a list of every
version, when it was made in words (*today at 9:14*, in your own time zone)
and which is current (the one links show); tapping
one views it. Under the list is **Page details**, the page's screen. Both
popovers are a sheet from the bottom on a phone and hang under the header on
a wider screen; one is open at a time, and Escape, a tap outside or its own
button closes it.

**Share** ends the header (the same Share as everywhere else): on a device
with a share sheet it opens it with the page's link; otherwise it copies the
link and says *Link copied*. It is drawn only for someone who may manage the
page — its owner, or on a team's page the person who started the agent that
made it (a manager of the team, once that person has left it); any other
team member does not see it here, and shares from the Pages screen instead.
A link's viewer sees none of these: their header is the instance's brand
and the title.

Beside the chip is the **session that made the page**: a button with its
name, one tap to it. For a page a mirrored Claude Code session made, that is
the session's view in muster, at the window it runs in **now** — muster
records the session (not its window, which moves) when the page is made, and
looks it up among your own machines when you open the page. For an agent's
page, the agent's page. The version list says *Made by …* with the same
link, and so does the page's screen. When the session cannot be found — it
has ended, its machine is offline, or the page was made before muster
recorded sessions — there is no button and no link, only its name on the
page's screen: muster never guesses a window. Only you see it; a link's
viewer never does.

`/pages/{id}/preview`, the view's former address, redirects to it.

## A session's pages

When you look at a session, the pages it made are a tap away. The session's
header has one **Open** button (an icon and a count on a phone) that counts
its [previews](previews.md#what-you-do), its pages and its
[open asks](attention.md#a-sessions-asks) together, and opens a panel with
three tabs, **Previews**, **Pages** and **Asks** (swipe between them on a
phone). It is drawn whenever the session has any of them. Pages need no viewer
plugin. The panel opens on a tab that has something: the one you used last on
this browser when it has, otherwise the first that has.

The **Pages** tab lists the pages the session **created**, newest first, each
with its current version and whether it has a live link. Tap one to read it in
the panel (your own view of it, with its version chip), or **Open full page**
for `/pages/{id}/view`.

Which session made a page:

- **A dispatched agent's** pages are on its page (`/agents/{name}`): the
  pages its tools made.
- **A mirrored window's** pages are every page made **in that window** on
  that machine (its tmux window), whichever Claude Code (or opencode)
  session made it, so a `/clear`, a restart or a resume in the window keeps
  them. muster records the session and the window a page came from when the
  page is made. The window also lists the pages made by the session running
  in it now, wherever that session made them.
- A window on another machine is never the same window, even with the same
  name. A tmux server restart reuses window ids, so a new window can match
  an old one of the same tmux session name on the same machine.
- **A page a mirrored session made before muster recorded sessions** has no
  session or window to match, so it is listed under no window. A page made
  after muster recorded sessions but before it recorded windows is listed
  only while its session runs in the window. Both are still on the Pages
  screen.
- Only your own pages are listed. A team session's window lists none: a
  team's pages are the team's, and its members do not see them.

## The Pages screen

**Pages** is in the sidebar (under More). `/pages` lists your pages, newest
activity first. Each row is the page's title, who made it and when it last
changed (*today at 9:14*, in your own time zone), and a badge only for what
is there: the version (**v3**, when there is more than
one), **Live** (it has a live link), **2 waiting** (links waiting for your
approval) and **3 replies** (responses). Tapping a row opens [your own
view](#the-owners-view) of the page; its **⋯** holds **Share**, **Details**
(the page's screen) and, when the page has its Share link, **Revoke link** and
**New link**. What the screen is for is behind the **ⓘ** beside its heading.

A page's screen (`/pages/{id}`) has, under the title, who made it and the
version shown:

- **Open** — [your own view](#the-owners-view) of the page — and **Share**. When
  the page has its Share link, a **⋯** beside them has **Revoke link** and
  **New link**. The screen never spells an address.
- **Links**, only when the page has one. Each live or waiting link: its state,
  who asked, when it was made in words (*Made today at 9:14*; the exact time
  is its tooltip), whether it is the page's Share link or an earlier one,
  **Copy** on the page's Share link (the one link whose address muster
  keeps, on any device), and **Revoke**. Revoked and declined links are
  folded under **Past links (n)**. (There is no separate "make a link"
  button: Share and ⋯ → New link make every link.)
- **Versions**, only when there is more than one: one row per version, newest
  first (*v2 · current · today at 9:14*; its exact time and size are the
  time's tooltip); tapping a row views that version. An older version's
  **⋯** has **Make current** (a rollback: the version becomes current, every
  link shows it, nothing is deleted).
- **Responses**, only when there is one: everything viewers sent and you sent
  from your view (marked *Sent by you*), newest first, each as labelled fields named by the page's
  own form (the version it was sent through): a form's field labels, a
  question's *Answer*, a choice's *Chose*, an approval's *Decision* and
  *Comment*; what a page's own script sent, by its keys (followed four levels
  deep, deeper is *(nested data)*; each label and value is cut at 200 and
  2000 characters, and one response draws at most about 4,000 characters,
  the rest *N more not shown here*). A response to a version that no longer
  exists is *A response to an older version*. They are shown 50 at a time,
  newest first, with **Show more** for the older ones. Each file is
  **Download the file** (`/uploads/{id}/file`: an attachment, never shown
  inline as a page). An admin also has a **Show raw** ⓘ on each: the part's
  id, the version, the link it came through and the data as stored — its
  first 2,000 characters, with **Download the full response**
  (`/pages/{id}/responses/{rid}/raw`, a JSON attachment) when it is longer. A
  response too large to keep is refused with a 413 in plain words.

- **Delete this page** (its ⓘ says what it removes) asks in place: it
  becomes *Delete this page and its responses?* with **Delete** and
  **Cancel** (Escape cancels too). Delete deletes the page, every
  version, link and response,
  and every file sent through it. It cannot be undone. From its first step
  the page takes no new link (an agent's share or a Share is refused), so
  nothing goes live while it is being deleted; a delete that fails leaves it
  so, until Delete is pressed again.

Both screens show live links (the Share buttons carry them), so they load no
front-end telemetry, and the page's screen is never cached.

## A team's pages

A page a [team's](auth.md#teams) agent makes belongs to the team. Everyone on
the team (viewer and up) sees it:

- **In Pages**, beside your own pages, marked **Team: <name>**.
- **On its screen and its view**, where you can answer its questions and
  forms. What you send is recorded as **yours**: the page's screen says
  *Sent by <your name>* to everyone else on the team, and *Sent by you* to
  you. Everyone's responses from the page's own view count together toward
  its cap (1,000 per page, the same as one link's), not 1,000 each. Files
  can't be sent from a team page yet: a file field there says so. The
  page's own [images and video](#images-and-video) are shown to everyone on
  the team, as the page is.
- **Share** works for anyone on the team: one tap makes the page's link
  public, or shows it again if it already has one. The link's row names who
  tapped. (The page's own view draws Share only for whoever may manage the
  page; the others share from the Pages screen or the page's screen.)

**The page's creator manages it**: the person who started the team agent that
made it (the person whose say-so the team chief's start was). Only they see,
and can use, **Delete this page** and **Make current** (an older version's ⋯). Anyone else on the
team gets a 403 from those routes: *this is a team's page: only the person who
started the agent that made it can delete it or change its version (a manager
of the team can when that person has left the team)*. The creator keeps this
whatever their role on the team: a creator made a viewer still manages the
page they started. Leaving the team is what ends it.

**A manager of the team steps in**:

- When the page records no creator (its agent's start named nobody, like the
  team chief itself), or its creator has **left the team**, any manager of the
  team manages it instead. Otherwise nobody could ever delete it.
- A manager can **always revoke** the page's links (a link's **Revoke**, and
  ⋯ **Revoke link**) and make a **New link** (a revoke and a Share, both
  theirs), even while the creator is on the team, so a mistaken Share is
  undone at once. Anyone else is refused: *only the person who started the
  agent that made it, or a manager of the team, can revoke its links or make
  a new one*.

**A team agent never makes a link live by itself.** Its `page_share_request`
always files a pending link and a card, whatever any setting says. *Let my
agents share pages without asking* is never read for a team. Anyone on the
team answers the card on Asks, in a browser or on a paired phone; answering
it is the same act as Share. The card records who answered.

Your own pages stay yours: nobody on your teams sees them, and a team's
pages are seen only through a signed-in browser or paired phone (never a
personal API token or your agents), like every team row. A member who leaves
the team stops seeing its pages on their next request.

Postgres holds every statement to these rules (migration 0165): the read
policies, the creator check (`muster_page_manager`), and guards that let
anyone else on the team do only three things. They can touch a page's
*updated* time when they answer. They can make a live link (Share). They can
move a pending link to live or denied (the card). A manager can also revoke a
link. Only the page's owner (its agent) names the card that decides a link.
A member's answer to the card is recorded by `muster_answer_team_page_card`,
because a viewer may not write the team's asks otherwise. That function
answers only an agent's link's card, and only with one of the card's own
options. For the same reason, a creator or manager's revoke withdraws the
link's card through `muster_cancel_team_page_card`, and their delete revokes
the page's file rows (under `pages/<id>/`) through
`muster_revoke_team_page_uploads`. Each of these functions answers one kind
of row, and only the application role may call it.

## In the chat

When an agent calls `page_create`, `page_update` or `page_share_request`, its
chat draws a **page card** under the tool's row: the title, what happened (a
new page, a new version, shared, or waiting for you), **Open** — [your own
view](#the-owners-view), the page itself as a viewer sees it — **Share**, and **⋯** (Page
settings; Revoke link and New link too while the page has a live link). It is drawn in a dispatched agent's chat (its
stored transcript and its live stream) and in a mirrored Claude Code
session's view. The card never holds a link's address: Share asks muster for
the page's current link on each tap, so a card from an hour ago never copies
a revoked link.

🔴 **The card is the page's, never the transcript's.** Only muster's own page
tools draw one (`page_create`, or `mcp__muster__page_create` from a mirrored
session — never another MCP server's tool of the same name), and only as a
reference to WHICH page: its title, whether it has a live link and who made
it are read from the page itself, as you — so a tool result cannot put words
on a card, and a page that is not yours draws no card at all. (The mirrored
view draws an empty placeholder that fills itself from muster.)

## Files

A file sent through a page's link — and the author's [assets](#images-and-video)
— is stored in the uploads bucket under the
page's own prefix, `pages/<page id>/<random id>` — **not** under `uploads/`,
whose lifecycle rule expires objects after days. It is kept until its page is
deleted, and deleting the page deletes it (the files go first: if one cannot be
deleted, the page is kept and Delete can be pressed again).

- 🔴 **Do not add a lifecycle rule on `pages/`.** It would delete files a page
  still shows. With pages on, the boot check warns about any rule that would
  expire objects there, a rule with no prefix included.
- With pages and attachments on, muster writes and deletes one object under
  `pages/` at boot, and refuses to boot when the key cannot.
- Deleting a page first revokes its links (so no upload is recorded through
  them after), then deletes its files' rows, then **every object under
  `pages/<page id>/`, by listing the bucket** — so a file that arrived while the
  page was being deleted goes too — and only then the page. A file stored but
  not recorded (its link revoked meanwhile, a cap reached) is taken back at
  once; when even that fails it is logged and counted
  (`muster_page_upload_rollback_failures_total`), and the page's deletion still
  removes it.
- A page's files are limited to `MUSTER_PAGES_UPLOAD_BYTES` in all (500 MiB by
  default); an upload that would pass it is refused, saying how much room is
  left.
- Strangers' uploads have their own slots — per link and for all pages on the
  server — apart from yours, and one minute to deliver a file: a link holder
  cannot keep your own attach busy.
- **Rolling migration 0116 back** removes the `pages/%` upload rows but not
  their objects. Delete those with the store's own tool, e.g.
  `mc rm --recursive --force <alias>/<bucket>/pages/`.
- The access key needs the same object permissions on
  `arn:aws:s3:::<bucket>/pages/*` as on `uploads/*`
  ([uploads.md](uploads.md#configuration)); the bundled store's key already has
  them.
- The signed `/f/` link an agent is given for a file still expires with the
  uploads' link lifetime; the file itself does not.

## What a viewer sees

`/s/<token>` is a thin page: your server's [branding](branding.md) (logo and
display name, when set; no muster mark) and the page's title in [one slim
header](#the-page-itself), and a frame of the page. The page itself is served at `/s/<token>/content` with exactly:

```
Content-Security-Policy: sandbox allow-scripts allow-forms; default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data: blob:; media-src data: blob:; font-src data:; connect-src 'none'; form-action 'none'; base-uri 'none'; frame-ancestors 'self'
X-Content-Type-Options: nosniff
X-Frame-Options: SAMEORIGIN
Referrer-Policy: no-referrer
Cross-Origin-Resource-Policy: same-origin
Permissions-Policy: camera=(), microphone=(), geolocation=(), display-capture=(), usb=(), payment=()
Cache-Control: no-store
```

The `sandbox` is an HTTP header, so the page has an opaque origin even when
it is opened directly: its script is never muster's, cannot read a muster
cookie, and its requests carry `Origin: null`. It reaches muster only by
posting a message to the frame's parent; the parent (muster's own script)
checks the message came from its own frame and calls `POST /s/<token>/submit`
or `/upload` with no credentials. Those routes, and the three `GET`s (the
shell, the content and an [asset](#images-and-video)), read no
cookie: the token is their only credential. (In [your own
view](#the-owners-view) the parent posts to `/pages/{id}/versions/{v}/submit`
or `/upload` instead, with your session, same-origin, as every write of
yours; those routes read no token.)

No page response sets a cookie. The access log writes every `/s/` path as
`/s/[redacted]`. Nothing about a viewer is stored but what they send; the
failed-lookup limit holds client addresses in memory only.

An uploaded file may be of any type. Its type is sniffed from its bytes, and it
is only ever served back as [uploads.md](uploads.md#serving-uploaded-files)
serves every upload: to the owner, as an attachment (images inline), under a
`sandbox` policy.

## Feedback

Every page view — [your own](#the-owners-view) and a share link's — has a
**Feedback** pill pinned to the bottom right, in a slim strip of muster's own
below the page (so it never covers anything the page draws at its bottom
edge). Tap it for a small panel: a box
for a message, **Send**, and (in your own view, on a server with
[dictation](features.md)) the microphone. **×** or Escape folds it back to
the pill and keeps what you typed; Ctrl/⌘+Enter sends. On a phone the open
panel is the screen's full width, the box on its own row and the microphone
and Send beneath it, below the header and clear of the home indicator.

The panel is part of muster's own frame around the page (the shell), never of
the page: the page's script cannot type, read or send a message.

What happens to a message (migration 0191, `internal/api/page_feedback.go`):

- it is **kept as one of the page's responses**, kind `feedback`,
  data `{"text": "…"}` — on [the page's screen](#the-pages-screen) as
  *Feedback*, and to the agent through `page_responses`, whether or not the
  session below is told;
- the [session that made the page](#notifications) gets **one line per
  message** (never a count), with the page's number and version (never its
  title) and the message fenced: yours as your words (needing your fresh sign-in, as every
  line carrying your words does), a link viewer's as a stranger's
  (`<<<viewer-feedback untrusted …>>>`, naming the link: *a viewer of share
  link #41 sent feedback on page #12 (version 3) …*);
- after **Send** the box is cleared and, in your own view, the panel says
  where it went: *Sent to the session that made this page.*; *Saved. It goes
  to the session that made this page once you sign in again.* (your sign-in
  is older than the step-up window); or *Saved with this page's responses, but
  the session that made this page isn't reachable, so it was not sent to it.*
  — a page a dispatched agent made, a team's (through its share link), or one
  whose host no longer mirrors here. A share link's viewer is always told
  *Sent. Thanks for the feedback.*: whether your session is reachable is not
  theirs to learn. The panel stays open for another message.

A share link's viewer can **type only**: the microphone posts to
`/transcribe`, which is yours alone, so their panel has none. Their message
goes through the link like a submit (`POST /s/<token>/feedback`, no cookie):
a revoked, pending, denied or expired link takes none. Each viewer of a link
(by client address, read by the same trusted-proxy rule as every other
limit) may send **5 messages a minute**, and all of a link's viewers together
**30 a minute**. A link takes at most **200 feedback messages in any 24
hours** (a rolling window, so one viewer cannot close a shared link for
good), a budget of its own: feedback never counts toward the link's 1000 form
responses, nor they toward it. Yours goes to `POST
/pages/{id}/versions/{v}/feedback` with your session: **5 a minute** per page,
no count cap. Every message is refused over **4000 characters** (refused
whole, never cut). A [team's
page](#a-teams-pages) draws no pill in a member's own view (a team's pages
wake no session), and that route refuses a member; its share link's viewers
can still send feedback.

## Notifications

- **Page ready: <title>** when an agent publishes a NEW page (`page_create`),
  opening your view of it.
- **Page updated: <title> (v<N>)** when an agent publishes a new VERSION
  (`page_update`), opening that version (`/pages/{id}/view?v=<N>`). An agent
  iterating publishes many versions in minutes, so this one is capped: at most
  6 per 10 minutes from one agent or session, and each replaces the page's last
  one on your device ([push.md](push.md#an-agent-notifies-you)).
- **Page shared: <title>** EVERY time an agent hands a page's link on without
  asking (the switch is on) — a new link, or the one you had already shared (the
  push says which) — opening the page's screen, where Revoke is.

They are three toggles on Account → Notifications: **An agent published a page
for you**, **An agent published a new version of one of your pages** (its own,
so you can mute an iterating agent's revisions and still hear of a new page)
and **An agent shared one of your pages without asking**. 🔴 The
last is its own on purpose: it is the one notice that a page went public
with nobody approving it, so muting "a page is ready" must never also mute it.
It is on by default, like every notification; muting it is its own, explicit
choice.

A response pushes you **A viewer sent something through one of your pages'
links** (its own toggle on Account → Notifications), naming the page and the
part by its label (*Through “Your details”*), never what was sent; at most
one a minute per page.

**The mirrored session that made the page is told too**, through its window's
send queue ([attention.md](attention.md#waking-a-mirrored-session)), using the
session and host recorded when the page was made:

- **your own response** (from [your own view](#the-owners-view)) is relayed
  with its content, fenced: `muster: you responded on page #12 (component
  q1), which this session made, from your own view. …` then each field as
  JSON, one per line (a file says only that you sent one);
- **anyone else's** — a link's viewer, a team member — becomes ONE line,
  `muster: page #12, which this session made, got 3 new responses from its
  viewers — read them with page_responses{page_id: 12}.`, with no part of what
  they sent. Responses that arrive before the line is queued fold into it.

The page's title is not in either line. A dispatched agent's pages wake
nothing (it reads `page_responses` itself). [Feedback](#feedback) is the one
exception to the count: each message is its own line, with its words (and
still without the title).

### If a page's notification did not arrive

Each page notification that reaches nobody is logged where it is dropped:
`push: dropped tag=page-<id>-ready …` (or `-updated`), with the reason — the
event muted in Account → Notifications, a cap reached, no device subscribed
([push.md](push.md#every-dropped-notification-is-logged)). A delivered one
logs `push: delivered tag=page-<id>-ready owner=<id> to N/M device(s)`, which
means a push service accepted it, not that the device showed it.

## Limits

| | limit |
|---|---|
| pages per owner | 200 |
| versions per page | 100 (a page muster makes for a [scheduled host job](host-jobs.md#the-page) keeps its newest 100 instead: the oldest version nothing references is dropped) |
| a version | 512 KiB, 100 blocks; an `html` block 256 KiB; a `markdown` block 64 KiB; a `gallery` 200 items, each caption 120 characters |
| pending and live links per page | 10 |
| a submit's data | 32 KiB |
| responses per link | 1000, files included ([feedback](#feedback) apart) |
| files per link | 50, each at most `MUSTER_UPLOAD_MAX_BYTES` |
| a page's files in all | `MUSTER_PAGES_UPLOAD_BYTES` (500 MiB) |
| one author asset | 15 MiB; PNG, JPEG, WebP, GIF, MP4 or WebM |
| a page's author assets | 100 MiB and 200 assets in all |
| page uploads in flight | 2 per link, 8 on the server (apart from the composer's), 1 minute each |
| submits per link | 20 a minute |
| [feedback](#feedback) messages | 5 a minute per viewer (client address) of a link, 30 a minute per link, 200 per link in any 24 hours (not counted in its 1000 responses); yours 5 a minute per page, no count cap; each at most 4000 characters |
| uploads per link | 10 a minute |
| asset reads per link and viewer (client address) | 600 a minute (a repeat view reads from the browser's cache) |
| asset bytes per link, all viewers | 2 GiB a minute |
| failed link lookups per client address | 60 a minute, then 429 |
| one MCP request (`page_create` / `page_update`) | 1088 KiB: the blocks as sent, up to 1 MiB (twice the stored 512 KiB, for an encoder that escapes more than muster's), plus 64 KiB for the rest |
| one MCP request (any other tool or method) | 64 KiB |

The MCP request bound is one constant (`mirror.MaxMCPRequestBytes`, derived
from the page limit) read by both hops a mirrored session's call crosses:
muster's MCP server and the host's local proxy in `muster-mirror`. A page past
512 KiB but inside it gets `page_create`'s own refusal ("a version holds at
most 512 KiB"); a request past it is refused 413 by whichever hop read it,
which names itself (`muster refused …` or `muster-mirror on this host
refused …`). A `muster-mirror` older than this bound still refuses any
request over 64 KiB with its old `{"error":"the request is too large"}`: a
page over 64 KiB from a mirrored session needs the host's `muster-mirror`
updated as well as the server.

The other way round, a current `muster-mirror` in front of a muster older
than v0.239.0: that muster reads at most 64 KiB per request and answers a
larger one with a bare JSON-RPC `-32700 could not read the request`. The
host proxy replaces that answer with one that names the muster, the
request's size and both limits (`muster at <host> could not read this MCP
request of N bytes: it reads at most 64 KiB per request, so it is probably
older than v0.239.0 …`). The fix is to upgrade that muster. Every muster the
host reaches counts, including any extra instance in its MCP targets file
([mirroring.md](mirroring.md#several-muster-instances)). A `markdown` block
holds at most 64 KiB on any version, so a probe of one large `markdown` block
is refused by the page tool even when the transport is fine. Use `html`
blocks to test a large page.

## Telemetry

`muster_page_link_requests_total{route,outcome}` (`route` is `shell`,
`content`, `submit`, `upload`, `feedback` or `asset`; `served`, `not_found`,
`throttled`), `muster_page_responses_total{kind,outcome}` (`kind` is `submit`,
`upload` or `feedback`; `recorded`,
`invalid`, `throttled`, `full`, `not_found`, `error`) and
`muster_page_share_decisions_total{decision}` (`requested`, `approved`,
`denied`, `minted`, `revoked`, `auto` — an agent's link made live for an owner whose switch is on) and `muster_page_upload_rollback_failures_total`. No token, page id or viewer data is a label.

## Not done

- **A turn pushed into the agent's session per response** is a planned
  follow-up. Today an agent reads responses with `page_responses` (and may
  block in it), the contract a mirrored session already has for answers, and
  you are pushed.
- **Require the viewer's email** per link.
- **An asset from an opencode session**: a mirrored opencode session is not
  offered the page tools.
- **Streaming an asset**: the shell fetches each asset whole, so a video
  does not start before it has arrived (the rest of the page shows meanwhile).
- **An asset from a dispatched agent**: the asset tools are a mirrored
  session's only, for now.
