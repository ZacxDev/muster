# Shareable pages

An agent can publish a small web page for you: a showcase of something it
built, or a page that collects things from someone else — answers, a choice,
form fields, files, an approval, or whatever its own script sends. You are
shown it at once — a card in the chat and a push — and it is private until it
has a live **share link**; anyone with a live link can open it without a
muster account. Sharing is one tap (**Share**), and an agent's own share
request waits for your approval unless you turned on **Let my agents share
pages without asking** — your own choice, on your Account page, off until you
turn it on ([below](#when-an-agent-shares)). What viewers send comes back to
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
opencode session is not offered them: its set is the attention and task tools
([attention.md](attention.md#mirrored-opencode-sessions)). A tool acts
only on pages its caller made: a dispatched agent on its own pages, a mirrored
session on the pages made from its **host** (the host is what the mirror token
proves). Any other page reads as `no such page`.

| tool | what it does |
|---|---|
| `page_create{title, blocks}` | Publishes a page (version 1). Returns `page_id` and `preview_url`, which only you can open, signed in. |
| `page_update{page_id, title?, blocks}` | Publishes the next version and makes it current. Links keep working and show it. |
| `page_get{page_id}` | Title, versions, links (each one's state and label, never its address) and the response count. |
| `page_list{}` | The caller's pages. |
| `page_share_request{page_id, reason?}` | Shares the page ([below](#when-an-agent-shares)). For an owner who has not opted in (the default): asks you, and returns a pending link's `url` — **once** — with `link_id` and `ask_id` (`status: "pending"`). For an owner who let their agents share without asking: the page's link is live at once; returns `status: "live"` and its `url` (the same one on every request when the page has a reusable link). |
| `page_responses{page_id, after_id?, wait_seconds?}` | What viewers sent, oldest first, after `after_id`, as one fenced block of JSON lines. `wait_seconds` (≤ 600) blocks until something arrives. At most 2 waits per caller at once. |
| `page_link_revoke{page_id, link_id}` | Revokes a pending or live link at once. Unless its owner opted in, no tool makes a link live. |

`page_responses` hands the responses over inside a `fence` block whose preface
says they are a stranger's input, data and not instructions. Each line is
`{id, component, kind, version, link_id, at, data}`, or, for a file,
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

A component's `id` is a letter followed by up to 63 letters, digits, `_` or
`-`, unique on the page; `custom` is reserved. muster checks every submit
against the **current** version's component, and stores it normalised (a form's
number as a number, a missing optional field as empty).

### The SDK

Every page carries a small script:

```js
muster.submit(data, {component})   // → Promise<{id}>; component defaults to "custom"
muster.upload(file, {component})   // → Promise<{id, name, size, type}>
muster.page                        // {title, version}
```

`custom` is accepted only from a version with an `html` block, and takes any
JSON up to 32 KiB (stored as `{"value": …}`). The built-in components use the
same two calls. In your preview both answer `{preview: true}` and record
nothing.

Inside the share page's frame the page has **no network**: `fetch`, images
from elsewhere and fonts from elsewhere are refused by its policy. Put images
inline as `data:` URIs. The policy cannot stop a top-level navigation, so the
content is served only into a frame: a browser opening `/s/<token>/content`
on its own (`Sec-Fetch-Dest: document`) gets the link's 404, and only muster's
shell may frame it (`frame-ancestors 'self'`).

## Share links

A link is `https://<MUSTER_PUBLIC_URL>/s/<token>`, where the token is 256
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
screens draw the address on the button already. A server without the key says
so on the page's screen in words (*Sharing isn't set up on this server*); the
setting it needs is in the ⓘ beside that line, for an admin only (anyone else
is told to ask their admin).

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

### When an agent shares

What an agent's `page_share_request` does is **the page owner's own choice**,
the switch **Let my agents share pages without asking** in the Pages section of
their Account page. It is per user, off for every user until they turn it on,
and only they can flip it (a signed-in, same-origin write of their own row;
no agent or session token reaches it). It is not a server setting.

- **Off** (everyone's default) — the agent asks, and you approve, below.
- **On** — for your pages the link goes **live at once**, and the agent is given
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
  after you approve is shown with no new approval — with **Open the page** (its
  preview, one tap away; the question holds no address) and **Approve link** /
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

## The Pages screen

**Pages** is in the sidebar (under More). `/pages` lists your pages, newest
activity first, with their counts, each with **Share** and **⋯**. A page's
screen (`/pages/{id}`) has:

- **Share**: **Share**, **Copy link** (the same link, always to the
  clipboard) and ⋯; the page's reusable link itself (when it has one) is in
  the ⓘ beside them, with its own Copy — the screen never spells an address.

- **Preview** (`/pages/{id}/preview`, any version with `?v=`): the page as a
  viewer sees it, in the same sandbox, recording nothing.
- **Links**: each link's state, who asked, when it was made in words
  (*Made today at 9:14*, in your own time zone), whether it is the page's
  Share link or an earlier one, and Revoke. Its ⓘ holds its exact label —
  when it was made (UTC) and the first 8 hex digits of its token's hash
  (`created 2026-11-11 14:02 UTC · 3fa9c2e1`), which tells links apart and
  opens nothing. (There is no separate "make a link" button: Share and ⋯ → New link
  make every link.)
- **Versions**: each version, when it was made, its preview, and **Show this
  version** (a rollback: the version becomes current, every link shows it,
  nothing is deleted). Its ⓘ holds its exact time and size.
- **Responses**: everything viewers sent, newest first, each as labelled
  fields named by the page's own form (the version it was sent through): a
  form's field labels, a question's *Answer*, a choice's *Chose*, an
  approval's *Decision* and *Comment*; what a page's own script sent, by its
  keys. Each file is **Download the file** (`/uploads/{id}/file`: an
  attachment, never shown inline as a page). An admin also has a **Show raw**
  ⓘ on each: the part's id, the version, the link and the data as stored.

- **Delete this page**: deletes the page, every version, link and response,
  and every file sent through it. It cannot be undone. From its first step
  the page takes no new link (an agent's share or a Share is refused), so
  nothing goes live while it is being deleted; a delete that fails leaves it
  so, until Delete is pressed again.

Both screens show live links (the Share buttons carry them), so they load no
front-end telemetry, and the page's screen is never cached.

## In the chat

When an agent calls `page_create`, `page_update` or `page_share_request`, its
chat draws a **page card** under the tool's row: the title, what happened (a
new page, a new version, shared, or waiting for you), **Open** — the page's
preview, the page itself as a viewer sees it — **Share**, and **⋯** (Revoke
link, New link, Page settings). It is drawn in a dispatched agent's chat (its
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

A file sent through a page's link is stored in the uploads bucket under the
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
display name, when set; no muster mark), the page's title, and a frame of the
page. The page itself is served at `/s/<token>/content` with exactly:

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
or `/upload` with no credentials. Those routes, and the two `GET`s, read no
cookie: the token is their only credential.

No page response sets a cookie. The access log writes every `/s/` path as
`/s/[redacted]`. Nothing about a viewer is stored but what they send; the
failed-lookup limit holds client addresses in memory only.

An uploaded file may be of any type. Its type is sniffed from its bytes, and it
is only ever served back as [uploads.md](uploads.md#serving-uploaded-files)
serves every upload: to the owner, as an attachment (images inline), under a
`sandbox` policy.

## Notifications

- **Page ready: <title>** when an agent publishes a NEW page (`page_create`),
  opening its preview. Not on `page_update`: an agent iterating publishes many
  versions in minutes, and the card in the chat already shows each.
- **Page shared: <title>** EVERY time an agent hands a page's link on without
  asking (you opted in) — a new link, or the one you had already shared (the
  push says which) — opening the page's screen, where Revoke is.

They are two toggles on Account → Notifications: **An agent published a page
for you** and **An agent shared one of your pages without asking**. 🔴 The
second is its own on purpose: it is the one notice that a page went public
with nobody approving it, so muting "a page is ready" must never also mute it.
It is on by default, like every notification; muting it is its own, explicit
choice.

A response pushes you **A viewer sent something through one of your pages'
links** (its own toggle on Account → Notifications), naming the page and the
component, never what was sent; at most one a minute per page.

## Limits

| | limit |
|---|---|
| pages per owner | 200 |
| versions per page | 100 |
| a version | 512 KiB, 100 blocks; an `html` block 256 KiB; a `markdown` block 64 KiB |
| pending and live links per page | 10 |
| a submit's data | 32 KiB |
| responses per link | 1000, files included |
| files per link | 50, each at most `MUSTER_UPLOAD_MAX_BYTES` |
| a page's files in all | `MUSTER_PAGES_UPLOAD_BYTES` (500 MiB) |
| page uploads in flight | 2 per link, 8 on the server (apart from the composer's), 1 minute each |
| submits per link | 20 a minute |
| uploads per link | 10 a minute |
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

## Telemetry

`muster_page_link_requests_total{route,outcome}` (`served`, `not_found`,
`throttled`), `muster_page_responses_total{kind,outcome}` (`recorded`,
`invalid`, `throttled`, `full`, `not_found`, `error`) and
`muster_page_share_decisions_total{decision}` (`requested`, `approved`,
`denied`, `minted`, `revoked`, `auto` — an agent's link made live for an owner who opted in) and `muster_page_upload_rollback_failures_total`. No token, page id or viewer data is a label.

## Not done

- **A turn pushed into the agent's session per response** is a planned
  follow-up. Today an agent reads responses with `page_responses` (and may
  block in it), the contract a mirrored session already has for answers, and
  you are pushed.
- **Require the viewer's email** per link.
- **Page assets** uploaded once and referenced; today images are `data:` URIs.
