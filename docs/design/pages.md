# Design: shareable pages — agent-written pages a stranger can open

**Status: phase 1 is built (#606); phases 2 and 3 are proposed.** The
operator's decisions are made ([decisions](#operator-decisions)). What was
built is described in [pages.md](../pages.md), from the code; where the two
disagree, the code wins ([decisions.md](../decisions.md)). Where phase 1 came
out differently from this page, the section says **As built**.

The ask, verbatim: *"I'd like to implement the concept of shareable pages as
a feature. This probably makes sense to exist as a plugin. My requirements are
that I'd like to be able to have agents able to create and publish pages and
then have the ability to generate a shareable link. This is so that as I work
with client projects and my agents ship things, I can have them create a page
either showcasing the feature, requesting information etc. Pages actually need
to be a little bit like little apps because I'd like to be able to include
functional pieces inside the pages such as the user being able to answer
questions, upload things, fill out a form etc."*

## Summary

| | Decision |
|---|---|
| What a page is | A row the owner owns, with **versions** (immutable) and a current-version pointer. A version is a list of **blocks**: markdown, built-in components (question, choice, form, upload, approval, gallery) and raw `html` (custom HTML and JavaScript) |
| Who makes one | An agent, with MCP tools on both agent doors (`POST /mcp` and `POST /mirror/mcp`): `page_create`, `page_update`, `page_get`, `page_list`, `page_share_request`, `page_responses`, `page_link_revoke`. **As built:** a mirrored *opencode* session's narrower set (attention and task tools only, #595) does not include them |
| Who sees it | Its owner, signed in, at any time (their own view, `/pages/{id}/view`, where what they send is recorded as theirs). Anyone else only through a **share link**: `/s/<token>`, 256 random bits, revocable |
| Making a link | An agent **asks**; the owner **approves** it on the Asks page or their phone (an attention ask, owner's door only). The owner can also mint and revoke links on the Pages screen |
| Serving | From muster's own host. The page's bytes are served at `/s/<token>/content` with `Content-Security-Policy: sandbox allow-scripts allow-forms; …` as an **HTTP header** (an opaque origin even on direct navigation), framed by a thin muster shell at `/s/<token>` that draws the instance branding |
| Talking to muster | Inside the shell's frame, page script has **no network** (`connect-src 'none'`), and the content is served only into that frame. The SDK (`muster.submit`, `muster.upload`) posts a message to the shell, and the shell — muster's own fixed code — calls the capability routes with `credentials: 'omit'` |
| Responses | Rows of the page, read by the creating agent with `page_responses` (as fenced DATA, optionally long-polled) and by the owner on the Pages screen; each pushes the owner. Uploads go through muster's existing upload path |
| Shape | **In-tree, off by default** (`MUSTER_PAGES=1`, needs a database), in its own package `internal/pages` behind a narrow seam, not a plugin ([why](#core-not-a-plugin)) |

## Goals

- An agent can publish a page in one tool call, iterate on it, and ask its
  owner to share it; the owner sees it first, in their own view.
- A page can **collect** things: answers, choices, form fields, files, an
  approval, and whatever custom script sends.
- What a viewer sends reaches the agent as **data**, never as instructions.
- Agent-written HTML and JavaScript can never act as muster, read a muster
  credential, or reach muster's routes with one, whoever opens it.
- A link leaks nowhere it was not given: not in a `Referer`, not in a log.
- Republishing keeps the link; the owner can roll back.

## Non-goals

- **Hosting websites.** A page is small (512 KiB), self-contained, and has no
  external network. No custom domains.
- **Viewer accounts.** A link is a bearer capability; there is no viewer
  sign-in, and v1 records no viewer identity beyond what they submit.
- **Making page content safe.** It is untrusted and is kept away from muster.

## What exists today (that this builds on)

- **Owned tables under forced row-level security**, the `owner_isolation`
  policy keyed on `muster.uid` (migration 0089 `asks` is the template), and
  composite `(id, owner_id)` keys so a row cannot name another owner's parent.
- **A capability lookup has one System read.** A token whose row names its
  owner is looked up under `authz.System` for one store call, and the request
  then acts as that owner (`agentByToken`, `mirrorHostFor`, `uploads` `LinkTarget`).
  Each such call is in the System ledger.
- **Cookies** `muster_session`, `muster_at`, `muster_rt` are `HttpOnly`,
  `SameSite=Lax`, host-only ([previews design](previews.md#cookies-origins-and-framing)).
- **Every cookie-authenticated write refuses `Origin: null`**
  (`requireSameOrigin` → `isSameOrigin`, `internal/api/auth.go`), which is
  exactly what an opaque origin sends.
- **Uploads** stream to S3 under the caller's principal, sniff the type from
  the bytes, and are served only through `uploads.SetServeHeaders`
  (attachment unless an image; a `sandbox` CSP) ([uploads.md](../uploads.md)).
- **Attention asks** reach the owner's Asks page and phone; an **owner card**
  is answered only through the owner's own doors (browser or paired phone),
  never a mirrored session's `attention_answer` (`answerSQL`). A card's tap can
  *do* something before the answer is recorded (`ruleForAnswer`, routing rules).
- **Branding**: `ui.Brand` and the public `/brand/logo.png` ([branding.md](../branding.md)).
- **`fence`** renders untrusted text as a structural block an agent is handed.

## Core, not a plugin

muster plugins are separate processes that call muster; muster never calls
them and never proxies their bytes ([plugins.md](../plugins.md),
[decisions.md](../decisions.md#plugins-pull-muster-never-calls-out)). The
previews gateway is the precedent: because it serves bytes, it owns **its own
domain**. The operator chose to serve pages from **muster's own host**
(decision 5), so a plugin cannot do it without muster proxying its bytes —
the one thing the plugin contract forbids. Every other piece is core already:
the approval is an attention ask, uploads are muster's, branding is muster's,
and the agent tools are muster's MCP doors.

So pages are in-tree and off by default, built to be split later:

- `internal/pages` — the model (spec, validation, limits), the store
  (Postgres, RLS), the token, the renderer (the content document, the shell,
  the SDK). It imports nothing of `internal/api` or `internal/ui`; markdown is
  passed in.
- `internal/api` — the routes, the tools and the approval hook, each a thin
  layer over a `pages.Store`.
- The seam to cut for a plugin later is exactly `/s/*`: move serving to a
  separate host (the previews shape) and keep the store and tools here.

## Data model (migration 0116)

All four tables are owned, under forced row-level security, with
`owner_id DEFAULT muster.uid`, and every child names its page by
`(page_id, owner_id)`.

| table | columns (beyond `id`, `owner_id`) |
|---|---|
| `pages` | `title` (≤ 200), `current_version`, creator: `agent_id` **or** `mirror_host` (+ `creator_label`), `created_at`, `updated_at` |
| `page_versions` | `page_id`, `version` (1, 2, …; unique per page), `title`, `blocks` JSONB, `size_bytes`, `created_by` label, `created_at`. Immutable: no `UPDATE` path |
| `page_links` | `page_id`, `token_sha256` (unique, the lookup), `state` ∈ `pending`, `live`, `denied`, `revoked`; `ask_id` (the approval), `requested_by`, `created_at`, `decided_at` |
| `page_responses` | `page_id`, `link_id` (NULL for the owner's own), `by_owner` (0152), `version`, `component`, `kind` ∈ `submit`, `upload`; `data` JSONB (≤ 32 KiB), `upload_public_id`, `created_at` |

- **Only the token's sha256 is stored** (operator decision, round 0). A link
  is shown once — to the agent in `page_share_request`'s result, or to the
  owner on the answer to their Share — and a lost one is replaced, not
  recovered. A lookup hashes the presented token and compares in constant time.
  *(Superseded for the owner's Share: with a link key a page keeps ONE
  reusable link whose token is also sealed at rest, so Share shows it again —
  see the threat model and docs/pages.md#sharing. Hash-only links remain for
  agents' pending links and servers with no key.)*
- **A creator** is the calling agent (`agent_id`) or the mirrored session's
  **host** (`mirror_host`). The tools act only on pages their caller created:
  a dispatched agent on its own, a mirrored session on its host's (the host is
  what the mirror token proves; the session id is the caller's claim).
- Rollback is a pointer move (`current_version`); no version is deleted.
- **A page's files** are kept under `pages/<page id>/` in the uploads bucket,
  outside the expiring `uploads/` prefix, until the page is deleted (which
  deletes them first). 0116 widens `uploads_object_key_check` to allow that key.
- `db.RLSOff`/`RLSOn` treat the tables like every owned table.

## Routes

The routes, and who reaches each, are in [pages.md](../pages.md): the four
share-link routes under `/s/{token}` ([what a viewer sees](../pages.md#what-a-viewer-sees))
and the owner's Pages screen ([the Pages screen](../pages.md#the-pages-screen)).
A refused link is one 404 on every `/s/` route: nothing says why.

## The sandbox

This is the crux. Agent-written HTML and JavaScript run on muster's host, so
the design must hold even when the page is opened **directly**, not framed.

### The content document

`/s/{token}/content` and the owner's version route are served with exactly the
headers [pages.md lists](../pages.md#what-a-viewer-sees) (asserted whole by
the tests). What they buy:

- **`sandbox` without `allow-same-origin`, as a header.** The document gets an
  **opaque origin** however it is reached — framed, opened in a tab, or
  fetched — so its script is never muster's origin. It cannot read
  `document.cookie` (it throws), `localStorage`, or any muster frame's DOM,
  and its requests carry `Origin: null`.
- **No network** (`connect-src 'none'`, no external `img`/`font`/`media`
  source, `form-action 'none'`). Page script cannot `fetch` muster with or
  without cookies, cannot exfiltrate what a viewer types, and cannot load a
  tracker. A page is self-contained: images are `data:` URIs inside its size
  cap. Even if `connect-src` were lost, a cookie-carrying write from an opaque
  origin is refused by `requireSameOrigin` (`Origin: null`), and a read is
  unreadable (no CORS is ever answered for `null`).
- **No `allow-top-navigation`, `allow-popups`, `allow-modals`,
  `allow-downloads`**: page script cannot navigate the viewer's tab away or
  open windows.
- **`frame-ancestors 'self'`**: only muster's shell may frame it. `'self'`
  is the origin of the response's URL, not the sandboxed document's opaque
  one, so the shell on muster's origin may frame it (measured in Chromium by
  `e2e/tests/pages.spec.ts`).
- **The handler never reads a cookie** and never writes one. The route is
  outside every session wrapper; its principal comes from the link alone.

### The shell

`/s/{token}` is muster's own fixed page — the only code on muster's origin a
viewer runs. Its CSP is `default-src 'none'; script-src 'nonce-…';
style-src 'unsafe-inline'; img-src 'self'; frame-src 'self'; connect-src
'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'`, with
`Referrer-Policy: no-referrer`. It draws the instance logo and name (no
muster chrome), the page title, and
`<iframe sandbox="allow-scripts allow-forms" referrerpolicy="no-referrer">`
(the attribute repeats the header's sandbox for older browsers). Agent text
reaches it only as the HTML-escaped title.

### How the SDK talks to muster: postMessage to the shell

Two shapes were considered:

1. **CORS for `Origin: null`** on the capability routes, called by the SDK
   directly. Every opaque origin anywhere would then be allowed to read those
   responses, and page script would need `connect-src` to muster.
2. **postMessage to the shell** (chosen). The SDK posts
   `{muster: 1, id, op: "submit"|"upload", component, data|file}` to its
   parent. The shell accepts a message only when `event.source` is its own
   frame's window and `op` is one of the two; it then calls
   `POST /s/{token}/submit|upload` itself with `credentials: 'omit'`, and
   posts the result back (`{muster: 1, id, ok, result|error}`, target `*`,
   since an opaque origin can be targeted no other way; the result holds
   nothing secret).

Option 2 keeps page script with **no network** inside the frame (the content is served only into a frame), keeps muster from ever
answering CORS for `null`, and puts the only code that talks to muster in
muster's own file. The capability routes **authenticate by the link alone**
and ignore cookies, so there is no ambient credential to forge and no CSRF
surface; they therefore do not check `Origin` (a token holder can post from
anywhere, by design). If page script navigates its own frame elsewhere, the
new document is still that frame's window and could use the relay — which is
only what the page's own script could already do.

### Where this differs from the previews design, because of the same host

- Previews isolate content by **another registrable domain**; pages by an
  **opaque origin**. A sandboxed document cannot hold cookies, so the five
  same-site measures of previews (PSL, `Sec-Fetch-Site`, `__Host-` cookies,
  cookie-bomb reset) are not needed: there is no page cookie to protect, and
  page script cannot set one on muster.
- Previews' `wsAllowedOrigins` `r.Host` hole is closed there by refusing the
  preview host. Here there is no other host, and the hole does not arise:
  page requests carry `Origin: null`, never muster's.
- Previews proxy full apps (WebSockets, any method). Pages serve one static
  document per version and accept two fixed requests.
- Previews' share links are owner-minted behind a fresh sign-in; here an
  agent may **request** one and the owner approves on their own door.

## Interactivity

| block | renders | a submit's `data` | checked by muster |
|---|---|---|---|
| `markdown` | muster's markdown renderer, HTML-escaped (**as built:** its document form, `ui.MarkdownHTML`, whose headings are real `<h1>`…`<h6>`) | — | — |
| `question` | a labelled text box (`multiline` optional) | `{"answer": "…"}` | required, ≤ 4000 chars |
| `choice` | radio buttons or checkboxes (`multiple`) | `{"choices": ["…"]}` | each is one of `options`; one unless `multiple` |
| `form` | typed fields (`text`, `textarea`, `email`, `number`, `date`, `select`, `checkbox`) | `{"fields": {"name": value}}` | known names only, types, required, `select` options |
| `upload` | a file picker | — (an upload) | the link's caps; the type is sniffed |
| `approval` | Approve / Decline, optional comment | `{"decision": "approve"\|"decline", "comment": "…"}` | the two values |
| `html` | the agent's HTML and script, verbatim | `muster.submit(data)`: any JSON | ≤ 32 KiB; component `custom`; stored as `{"value": data}` |

Every component is muster-generated HTML inside the **same** sandboxed
document, wired by the SDK, so there is one security model for all of them.

The SDK, in every content document:

```js
muster.submit(data, {component})   // → Promise<{id}>
muster.upload(file, {component})   // → Promise<{id, name, size, type}>
muster.page                        // {title, version}
```

`component` defaults to `custom` and must name a block of the version (or
`custom` when the version has an `html` block).

**As built (the owner's view):** the owner's view records. Its shell is the
link's shell with the owner's chrome (a version chip and the confirm bar),
and its relay posts to `/pages/{id}/versions/{v}/submit` and `/upload` with
the owner's session, same-origin (`requireOperatorWrite`) — and, for a
version with an `html` block, **only what the owner confirmed in the shell's
own "Send as you?" bar** (the operator's decision after round 2).

**A version with no `html` block has no bar** (a later decision of the
operator's, revising that one for script-free pages, to make answering asks
quick). Only an `html` block carries the author's script: markdown is
rendered to escaped HTML (links http(s) only, images only an asset's or a
raster `data:` image), the components' and the gallery's markup is muster's
own with every author string escaped, `custom` is accepted only from a
version with an `html` block, and the theme CSS is the instance's. So in
such a version every send reaching the shell is the reader's own press of a
built-in component. The route that renders the owner's shell decides it from
the blocks of the version shown (`OwnerChrome.ScriptFree`, `!HasHTML`; its
zero value keeps the bar) and the relay then posts at once — same route,
session, checks, normalisation and size cap; nothing the frame says can turn
the bar off. *Residual:* on such a page one click records, so a mis-click
records (the author's wording could still mislead about what a built-in
button means, as it always could).

On a version that keeps the bar, it sits bottom-centre on a wider screen
and has a keyboard: Enter sends only on a trusted, non-repeating keydown
after the 500 ms quiet window (one inside it, or a repeat, restarts the
window, so a held Enter or the Enter that opened the bar never sends), and
Escape cancels; an Enter on Cancel is Cancel's. The rest of this paragraph
is the bar's. The shell checks each send on its own
structured-clone copy: a fixed field set; the data normalised by a JSON
round-trip first (round 3: for a link's viewer too, as before the confirm —
`undefined` dropped, a `Date` its ISO string, `NaN` null), then measured in
bytes against the 32 KiB cap; 1–20 `{file: Blob, name}` pairs. It shows that
copy — the component and **every** field, one per line, each name and value
as JSON with control, invisible and bidi characters escaped (round 3: a
600-character cut had hidden a later field, and a raw newline or U+202E
could fake or reorder one), never cut (a summary over 64 KiB is refused, not
shown in part), or the files' names (escaped) and sizes — and posts exactly
it only on a trusted click of the bar's Send, in muster's realm, which the
page's script cannot reach (another origin, a sandboxed frame). Send is not
focused on opening; a press anywhere on the bar within 500 ms does nothing
and restarts the 500 ms (round 3: a burst of taps had reached past a fixed
window), and Send then needs a press that starts on it; one send waits at a
time; the bar cancels itself after 10 minutes, and the SDK waits 30, so the
page is never told "No answer came back" while the bar could still send.
Escape cancels while the keyboard is in the shell; inside the frame the shell
cannot see keys, and Cancel is the reliable path. The summary is the bar's
accessible description. The row is `by_owner`, with no link;
the caps and rate limits are one link's. A response from `?v=N` answers
version N. The content document is the same for both, so it has no preview
mode.

*History:* round 0 answered Q1 with a tap gate in the page — the SDK read
`navigator.userActivation.isActive` (measured: false in the content frame on
load, true after a click inside it) and sent it as `act`. Round 1 forged
`act` through an `Object.prototype` setter on a field assigned after the
message literal; round 2 forged it again through `Array` species, which made
the built-in upload's message list a page-built Proxy. Two bypasses of one
check in the page's own realm: the gate was removed, and the decision moved
to the shell.

**As built (the session that made it):** `pages.mirror_session` (0152,
nullable) records the Claude Code session id a mirrored session's
`page_create` came from — the id its MCP request bound to — beside
`mirror_host`; an agent's page keeps `agent_id`. The owner's view resolves it
when opened (`pageSessionLink`): a mirrored session through
`claudeSessionWindow` over the viewer's own hosts — the resolver the Asks'
"View in session" uses — to its current window's rendered view; an agent
through `agentPageHref`. Unresolvable is no link, never a guessed window.
It is the owner's chrome: the header's session button, "Made by" in the
version popover and on the page's screen; never the link's shell.

**As built (the header):** the shell's header is one ~40px row over a
full-window frame and hides on a scroll down by a transform (the frame never
moves). The SDK reports the document's scroll offset to the shell on a
private `MessagePort`, and every send and answer goes on it too. The page's
markup is not in the document: it is a JSON string in the SDK, put in the
page (`createContextualFragment`, so its scripts run in order) only once the
SDK has asked the shell for the port ("hello") and taken it in a capture
listener, and the document is parsed — so no script of the page's runs
before the port is held. The functions the port and the scroll channel rest
on are saved first, and every message is a literal the SDK builds with its
own op. Round 0's D1, measured: when the markup was in the body, a page that
called `document.open()` from a `setTimeout` received the port, at both
sizes; with this order it cannot. The shell takes only the first hello and
ignores every other window message.

**As built (the gallery):** a `gallery` block's full-size viewer is the
SDK's, in the frame, but Back belongs to the top window, and an entry a
sandboxed subframe pushes is not seen by an Android WebView's own back
handling (it reports the main frame's history changes only). So the SDK posts
`{op: "viewer", open}` on the port and the shell pushes one history entry on
open and pops it on a close inside the frame; a Back that pops it is answered
`{op: "back"}` and the viewer closes. The shell decides by `history.state`
(a token of the entry it pushed), never by a timer: a queued `history.back()`
counts its target when it runs, so a push made before a slow pop lands would
be popped instead. The frame has no storage, so a gallery's unsaved picks are
kept by the shell in its own `sessionStorage` (`{op: "keep"|"recall", key,
value}`, the key the version and the gallery's id, under the shell's path).
A page's own script never holds the port, but it CAN add gallery-shaped
sections that the SDK's gallery script then keeps for; so the shell takes only
the keys the server named for the version it rendered (its pick galleries),
at most 16 under one path, 16 KiB each.

The header shows on load, near the top, and on any scroll up, and hides
only on a scroll down past its own height. It also comes back on its own 3
seconds after the last report further down (round 1: a page that scrolled
itself down and set `overflow:hidden` on `<html>` sends no scroll up; round 2:
a page re-dispatching `scroll`, or scrolling an inner element, re-reported
the same offset and kept restarting a plain idle timer — now only a report
with a larger y restarts it, and the SDK reports only the browser's own
(`isTrusted`) scroll of the document). And it comes back at most 8 seconds
after it hid, whatever the reports say, and hides again only after a fresh
scroll down of more than its own height (round 3: a page scrolling itself
1px every 2.5 s reported "further down" forever).

The saved-function layer that is left (round 3's D1) stays: the saved
`postMessage` is what keeps a page from patching it and being handed the
port as `this`, and a page holding the port could post scroll reports that
re-hide the header the moment the shell shows it — the header's guarantee
rests on the port being muster's alone.

After the page's scripts run, `DOMContentLoaded` is dispatched to them, and
`load` when it has already passed, once each; the insertion is a task after
the real `DOMContentLoaded`, so a window listener never sees both. A
`type="module"` script runs after both and must not wait for either.

## Approval flow

1. `page_share_request{page_id}` files a `pending` link and an **owner card**
   (an attention ask) from the caller: *"<agent> asks to share page “<title>”
   with anyone who has its link …"* with **Approve link** / **Not now**, and
   the address of the owner's own view of it. The owner sees it on the Asks page and their phone.
2. The card's answer runs `pageShareForAnswer` **before** it is recorded, as
   `ruleForAnswer` does: it requires the owner's own door
   (`attention.IsOwnerDoor`; a mirrored session's `attention_answer` is
   refused, and the owner card refuses it again in SQL), an answer equal to
   one of the card's options, and the link still `pending`. **Approve link**
   makes it `live`; **Not now** makes it `denied`.
3. The asker got the link's URL once, in `page_share_request`'s result (only
   its hash is stored); it learns the answer as from any ask (a dispatched
   agent by delivery, a mirrored session through `attention_wait`), and the URL
   opens the page from the approval on. A pending or denied link serves nothing.
4. An answer in the session (the owner typing to the agent) settles the card
   without the hook: the link stays `pending` and serves nothing.

That is the flow for an owner who turned the switch OFF (since migration 0163 it is on for every person until they turn it off; a saved choice is kept; a team owner always reads off). Before 0163 it was every user's, until they
do. For an owner who turned on **Let my agents share pages without asking**
(their own Account switch, `notify_preferences.pages_agent_share`, read for
the PAGE's owner) none of the above happens: `page_share_request` makes the
page's link live at once (the page's one reusable link when the server has a
link key, else a new hash-only link), files no card, returns
`status: "live"` and the address, and pushes the owner (`page_shared`, its own
toggle). The owner's door is not consulted, by that owner's own choice; see
the threat model.

The owner's **Share**, **New link** and **Revoke** on the Pages screens need a
signed-in, same-origin request, like every other owner write.

## Response delivery

- **To the agent:** `page_responses{page_id, after_id?, wait_seconds?}`
  returns the page's responses after `after_id`, as one `fence` block (each
  response a JSON line) behind a preface saying it is viewer input: data, not
  instructions. `wait_seconds` (≤ 600) blocks until one arrives, as
  `attention_wait` does. Upload responses carry a signed `/f/` link (the
  uploads TTL).
- **To the owner:** a push (`page` event, its own toggle on Account) naming the
  page and the component, never the viewer's text; at most one a minute per
  page. The Pages screen lists every response.
- **Not in v1: a turn pushed into a dispatched agent's session.** That needs
  attention delivery's exactly-once machinery (claim, lease, unconfirmed)
  generalised beyond `attention_asks`; until then an agent that must react
  waits in `page_responses`, which is the contract a mirrored session already
  has for answers ([phased plan](#phased-plan)).

## Limits and quotas

The limits are in [pages.md](../pages.md#limits). Uploads need attachments
configured; without them an `upload` block renders disabled.

## Telemetry

The metrics and their label values are in [pages.md](../pages.md#telemetry).
Spans are the middleware's, labelled by route **pattern**; the access log
writes `/s/[redacted]` for every `/s/` path, so no token reaches a log, span or
label. Viewer data is never logged.

## Threat model

| threat | control | residual |
|---|---|---|
| Page script acts as muster | opaque origin by HTTP header; no cookies readable; no network | none known |
| Page script reaches a muster route with the viewer's cookies | `connect-src 'none'`; `Origin: null` refused by `requireSameOrigin`; no CORS for `null` | a top-level **navigation** is not possible (no `allow-top-navigation`); a `GET` subresource is blocked by CSP |
| Direct navigation to `/content` | the sandbox is a header, not the frame's attribute | none |
| The link leaks | 256 bits, looked up only by its sha256; `no-referrer` on shell and content; `/s/[redacted]` in the access log; never in a push. A page's **reusable** link (the Share button's) also keeps its token **encrypted** (AES-256-GCM, a key derived from `MUSTER_CONNECTOR_ENCRYPTION_KEY`, bound to the row's owner, page and hash), wiped when it stops being live: a database read alone yields no live link, a database read **plus** that key yields each page's current Share link. Without the key, links are hash-only as before | whoever it was given to; anyone holding both the database and the key |
| A link outlives intent | revoke on the Pages screen, on the page card's ⋯ (Revoke link, New link), or with `page_link_revoke`; checked on every request | until revoked |
| Page script keeps the brand header hidden (a share link's trust signal) | scroll reports only on a private port taken before any page script, with captured primitives; the shell accepts only a finite number ≥ 0 there; visible on load, near the top and on any scroll up; focus inside it shows it | a page that scrolls its own document down is really scrolled, and hides the header until the next scroll up, 3 seconds after its last scroll further down, or 8 seconds after it hid, whichever is first (the shell's own rules); it hides again only after a fresh scroll down past its own height. A page that keeps jumping itself down by more than the header's height every 8 seconds hides it for most of the time. A page that replaces its own document (`document.open`) after the port is held loses its scroll reports (the header then stays) but never the port |
| Page script records something as the owner (their view records) | a version with no `html` block has no page script (only an `html` block carries it), so its sends post at once — the server decides that from the shown version's blocks when it renders the shell, never from the frame (the operator's decision). For a version with one: nothing from the owner's view is posted until the owner taps Send (or presses a fresh, non-repeating Enter after the quiet window) in the shell's own "Send as you?" bar, which shows every field of the shell's checked, JSON-normalised copy (escaped; never cut — too long is refused) and posts exactly it; the page's script cannot reach the shell's realm; Send is not autofocused, a press within the 500 ms quiet window restarts it, Send needs a press that starts on it; one send waits at a time; the bar gives up after 10 minutes; `by_owner` is muster's field, outside the data, handed to the agent as fenced data that authorises nothing | the owner may confirm what they did not read closely (the box scrolls; a file is its name and size, not its contents). A page can keep asking (each Cancel answers it "Not sent"). On a script-free version one click records: a mis-click records |
| An agent shares without the owner | **Owner turned it off (the default before migration 0163; since then on is every person's default, a team owner is always off):** only `page_share_request`, which files an owner card; approval only through the owner's door; the hook and `answerSQL` both refuse `attention_answer`. **Owner opted in** (every person's default since 0163; *Let my agents share pages without asking*, that owner's own switch on Account, `notify_preferences.pages_agent_share`): there is **no** owner gate for that owner's pages — the agent's request makes the page's link live at once and hands the agent its address. The controls left: the opt-in is per owner and only the owner flips it (a signed-in, same-origin write of their own row under row-level security; no agent or mirror token reaches the route); it is read for the PAGE's owner, never the caller's or the operator's; only a page the agent itself made, under its owner's row-level security (never another creator's or owner's); the owner is pushed (*Page shared*, its own toggle so muting *Page ready* never hides it), and the chat's page card and the page's screen say who made it live without asking, with Revoke one tap away | Not opted in: the owner approving what they did not preview. Opted in: **anything that owner's agent publishes can be public before the owner looks** — content, a phishing form, a leak of data the agent can read — until the owner revokes it; on by default since 0163 (the operator's decision), for that owner's own pages only, until they turn it off |
| A transcript draws a misleading page card | only muster's own page tools draw one (bare, or `mcp__muster__…`; another MCP server's same-named tool draws none), and only as a page reference: the title, share state and maker are read from the page under the viewer's row-level security, so a forged title is never drawn and a page that is not the viewer's draws no card | none known |
| A link goes live while its page is being deleted | the delete's first step marks the page (`pages.links_closed_at`) under the row lock every link maker takes; from then on no link — an agent's, the owner's Share — is made on it | a failed delete leaves the page taking no new link until Delete is pressed again |
| Prompt injection through responses | fenced DATA with a preface; the push carries no viewer text | the agent's own judgement |
| Malicious uploads | sniffed type; served only through `SetServeHeaders` (attachment, sandbox CSP, `nosniff`); never inline on the shell or content | none known |
| Exhaustion | [limits](../pages.md#limits) | bandwidth |
| User B reaches user A's pages | RLS on all four tables; composite keys; the isolation walk covers every new route | none |
| The shell's relay is abused | accepts only its own frame's messages and two ops; calls only two fixed routes, `credentials: 'omit'` | equal to the page's own power |
| A phishing page on muster's host | the shell shows the instance branding, so a page looks like the operator's; agents write pages only for their owner, who approves sharing | a page can draw a convincing form inside its frame |

## Phased plan

| phase | what |
|---|---|
| **1 (this branch)** | migration 0116, `internal/pages`, the four `/s/` routes, the owner's Pages screen and preview, versions and rollback, links (agent request + owner approval, owner mint/revoke), the seven tools on both doors, built-in components, the SDK and custom JS, responses, uploads, the owner push, docs, isolation and principal ledgers, a browser spec |
| **2** | a turn pushed into a dispatched agent's session per response (generalised exactly-once delivery); "require the viewer's email" per link |
| **3** | page assets uploaded once and referenced (instead of `data:` URIs); serving from a separate host if the operator wants a second wall |

## Open questions

Answered in round 0: the token is stored as its hash only; responses reach the
agent through `page_responses` (a pushed turn is a follow-up); a page's files
are kept under its own prefix until the page is deleted. Still open:

1. Should making a link on the Pages screen need a fresh sign-in (step-up),
   as previews' share links do? It does not.

## Operator decisions

1. **Link access:** anyone with the link — an unguessable capability URL
   (≥ 128 bits), revocable. No expiry (round 0 deleted the stored field). Optional
   "require the viewer's email" is later.
2. **Approval:** agents create and publish privately (the owner sees it
   signed in, in their own view, which records as theirs); a link outsiders can open needs the owner's approval through the
   attention ask (Asks page + phone). The owner can mint and revoke directly.
3. **Interactivity:** built-in components backed by muster, and custom
   JavaScript in a locked-down frame with a small SDK.
4. **Responses go** back to the agent as fenced data (and an agent tool), and
   notify the owner; the owner has a Pages screen with downloads.
5. **Hosting:** muster's own public host, sandboxed.
6. **Branding:** the instance branding, no muster chrome.
7. **Versions:** republishing keeps the link and prior versions; rollback.
8. **Plugin vs core:** decided from the code: [core, not a plugin](#core-not-a-plugin).
9. **Multi-tenant:** each server has its own pages; nothing crosses servers
   (each server has its own database, [multi-tenant.md](multi-tenant.md)).
