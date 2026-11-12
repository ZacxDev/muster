# Shareable pages

An agent can publish a small web page for you: a showcase of something it
built, or a page that collects things from someone else — answers, a choice,
form fields, files, an approval, or whatever its own script sends. A page is
private until you approve a **share link**; anyone with a live link can open it
without a muster account. What viewers send comes back to the agent as data,
and you are notified.

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
| `page_share_request{page_id, reason?}` | Asks you for a share link (below). Returns the link's `url` — **once** — with `link_id` and `ask_id`. |
| `page_responses{page_id, after_id?, wait_seconds?}` | What viewers sent, oldest first, after `after_id`, as one fenced block of JSON lines. `wait_seconds` (≤ 600) blocks until something arrives. At most 2 waits per caller at once. |
| `page_link_revoke{page_id, link_id}` | Revokes a pending or live link at once. No tool makes a link live. |

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

🔴 **muster stores only the token's sha256, so a link is shown once**, to
whoever made it: the agent, in `page_share_request`'s result, or you, on the
page right after **Make a new link**. A lookup hashes the presented token and
compares the hashes in constant time. Neither the Pages screen, `page_get` nor
the database can show a link again. If one is lost, make a new one and revoke
the old. ⚠ An agent's link **is** in that agent's transcript, and so with its
model provider: `page_share_request`'s result carries it. Treat a link an agent
asked for as shared with whatever sees that session.

- **An agent asks** with `page_share_request`. That files a **pending** link
  and a question for you on the Asks page and your phone: *“<agent> asks to
  share your page …”*, saying the link always shows the page's latest
  published version — a version the agent publishes after you approve is
  shown with no new approval — with the preview's address and **Approve link** /
  **Not now**. Approve link makes the link live; Not now declines it for good.
  Only you can decide it, in muster or on your paired phone: a mirrored
  session's `attention_answer` is refused, and so is any answer that is not one
  of the two. A dispatched agent is told your answer as a new message; a
  mirrored session reads it with `attention_wait{id: ask_id}`. The agent got
  the link's `url` when it asked; it opens the page once you approve.
- **You make one** with **Make a new link** on the page's screen. It is live
  at once, and shown to you there, that once.
- **Revoking** (on the page's screen, or `page_link_revoke`) stops it at once
  and withdraws its card if it was still waiting.

At most 10 links per page may be pending or live together.

## The Pages screen

`/pages` lists your pages, newest activity first, with their counts. A page's
screen (`/pages/{id}`) has:

- **Preview** (`/pages/{id}/preview`, any version with `?v=`): the page as a
  viewer sees it, in the same sandbox, recording nothing.
- **Links**: each link's state, who asked, and a short label — when it was
  made and the first 8 hex digits of its token's hash (`created 2026-11-11
  14:02 UTC · 3fa9c2e1`), which tells links apart and opens nothing; Revoke;
  Make a new link (its address shown once, on the answer).
- **Versions**: each version, its preview, and **Show this version** (a
  rollback: the version becomes current, every link shows it, nothing is
  deleted).
- **Responses**: everything viewers sent, newest first, as JSON, and each file
  as **Download the file** (`/uploads/{id}/file`: an attachment, never shown
  inline as a page).

- **Delete this page**: deletes the page, every version, link and response,
  and every file sent through it. It cannot be undone.

The page's screen can show a just-made link, so it loads no front-end
telemetry and is never cached.

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

## Telemetry

`muster_page_link_requests_total{route,outcome}` (`served`, `not_found`,
`throttled`), `muster_page_responses_total{kind,outcome}` (`recorded`,
`invalid`, `throttled`, `full`, `not_found`, `error`) and
`muster_page_share_decisions_total{decision}` (`requested`, `approved`,
`denied`, `minted`, `revoked`) and `muster_page_upload_rollback_failures_total`. No token, page id or viewer data is a label.

## Not done

- **A turn pushed into the agent's session per response** is a planned
  follow-up. Today an agent reads responses with `page_responses` (and may
  block in it), the contract a mirrored session already has for answers, and
  you are pushed.
- **Require the viewer's email** per link.
- **Page assets** uploaded once and referenced; today images are `data:` URIs.
