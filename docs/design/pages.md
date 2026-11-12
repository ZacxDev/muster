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
| What a page is | A row the owner owns, with **versions** (immutable) and a current-version pointer. A version is a list of **blocks**: markdown, built-in components (question, choice, form, upload, approval) and raw `html` (custom HTML and JavaScript) |
| Who makes one | An agent, with MCP tools on both agent doors (`POST /mcp` and `POST /mirror/mcp`): `page_create`, `page_update`, `page_get`, `page_list`, `page_share_request`, `page_responses`, `page_link_revoke`. **As built:** a mirrored *opencode* session's narrower set (attention and task tools only, #595) does not include them |
| Who sees it | Its owner, signed in, at any time (a preview). Anyone else only through a **share link**: `/s/<token>`, 256 random bits, revocable |
| Making a link | An agent **asks**; the owner **approves** it on the Asks page or their phone (an attention ask, owner's door only). The owner can also mint and revoke links on the Pages screen |
| Serving | From muster's own host. The page's bytes are served at `/s/<token>/content` with `Content-Security-Policy: sandbox allow-scripts allow-forms; …` as an **HTTP header** (an opaque origin even on direct navigation), framed by a thin muster shell at `/s/<token>` that draws the instance branding |
| Talking to muster | Inside the shell's frame, page script has **no network** (`connect-src 'none'`), and the content is served only into that frame. The SDK (`muster.submit`, `muster.upload`) posts a message to the shell, and the shell — muster's own fixed code — calls the capability routes with `credentials: 'omit'` |
| Responses | Rows of the page, read by the creating agent with `page_responses` (as fenced DATA, optionally long-polled) and by the owner on the Pages screen; each pushes the owner. Uploads go through muster's existing upload path |
| Shape | **In-tree, off by default** (`MUSTER_PAGES=1`, needs a database), in its own package `internal/pages` behind a narrow seam, not a plugin ([why](#core-not-a-plugin)) |

## Goals

- An agent can publish a page in one tool call, iterate on it, and ask its
  owner to share it; the owner previews before anyone else sees it.
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
| `page_responses` | `page_id`, `link_id`, `version`, `component`, `kind` ∈ `submit`, `upload`; `data` JSONB (≤ 32 KiB), `upload_public_id`, `created_at` |

- **Only the token's sha256 is stored** (operator decision, round 0). A link
  is shown once — to the agent in `page_share_request`'s result, or to the
  owner on the answer to **Make a new link** — and a lost one is replaced, not
  recovered. A lookup hashes the presented token and compares in constant time.
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
`custom` when the version has an `html` block). In the owner's preview the
SDK answers `{preview: true}` and records nothing.

## Approval flow

1. `page_share_request{page_id}` files a `pending` link and an **owner card**
   (an attention ask) from the caller: *"<agent> asks to share page “<title>”
   with anyone who has its link …"* with **Approve link** / **Not now**, and
   the preview's address. The owner sees it on the Asks page and their phone.
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

The owner's **Mint link** and **Revoke** on the Pages screen need a signed-in,
same-origin request, like every other owner write.

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
| The link leaks | 256 bits, stored only as its sha256 (a database read yields no live link); `no-referrer` on shell and content; `/s/[redacted]` in the access log; never in a push | whoever it was given to |
| A link outlives intent | revoke on the Pages screen or with `page_link_revoke`; checked on every request | until revoked |
| An agent shares without the owner | only `page_share_request`, which files an owner card; approval only through the owner's door; the hook and `answerSQL` both refuse `attention_answer` | the owner approving what they did not preview |
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
2. **Approval:** agents create and publish privately (the owner previews
   signed in); a link outsiders can open needs the owner's approval through the
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
