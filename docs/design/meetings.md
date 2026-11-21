# Design: meetings — voice, video, screen share, live transcript and action items

**Status: proposed. Nothing here is built.** The operator's decisions are
recorded under [decisions](#decisions). Everything else is the recommended
design. The operator answered the first round of open questions on
2026-11-20 (recorded as decisions 6–16); what is still open is under
[open questions](#open-questions). Where
this page says how muster works today, it cites `file:line` at commit
`5fd6f7632`, and the code is the authority ([decisions.md](../decisions.md)).
A claim nothing confirmed is marked **UNVERIFIED**, and each one has a
[phase 0 spike](#phase-0-spikes) that settles it. **The spikes have run**:
their results are folded in below and summarised in
[phase 0 results](#phase-0-results); what they could not measure is listed
there as still owed.

This page proposes a **meetings plugin**: a tab in muster's sidebar where the
owner starts a call, shares a link, and later reads what was said and what was
agreed.

## Summary

- **The whole feature, from the owner's side:** Meetings tab → **New meeting**
  → copy the link → done. Guests open the link, type a name, wait in a lobby,
  and are admitted with one click. The same tab lists past meetings, each with
  its transcript, recording and action items.
- **Shape: one small core seam, then a plugin.** muster gains a reusable
  plugin capability, **`app`**, that adds a sidebar tab and opens the plugin's
  page full-width in a frame that may use the camera, the microphone and
  screen capture ([the core change](#the-core-change-the-app-capability)).
  Everything else lives in a separate repository, `ZacxDev/muster-meetings`, a
  Go program built from the [plugin template](https://github.com/ZacxDev/muster-plugin-template).
- **Media is self-hosted:** a LiveKit SFU, LiveKit's built-in TURN, and
  LiveKit Egress writing recordings to S3-compatible object storage.
- **Transcription is per participant track**, so "who said what" comes from
  the track, not from diarization. The existing `stt-cohere` service **does
  not stream** ([finding](#finding-the-existing-stt-service-does-not-stream)).
  Live captions use **NVIDIA Nemotron Speech Streaming 0.6B** (decision 19),
  served by a small NeMo websocket server on the GPU host, with an
  **utterance mode** on the existing service as the at-capacity and
  zero-new-service fallback, and faster-whisper large-v3-turbo as the named
  fallback model. (A post-meeting re-transcription pass
  was dropped in review: there is no per-track audio to feed it.)
- **Action items** come from an LLM on OpenRouter over a rolling window of
  the transcript, every 10 s, emitting only items it has not emitted before.
  The candidate is **`scaledown/extract`**, with **`deepseek/deepseek-v4.1-flash-1017`**
  as the fallback, chosen by a [labelled evaluation](#action-item-evaluation)
  with a stated pass bar. The model is one config value. Only transcript
  **text** goes to OpenRouter, never audio; on by default, off per meeting.
- **Everyone in the call sees** the live transcript and the action items.
  Recording is **on by default** (off per meeting), with a notice to every
  participant while it is on.
- **Hosted in the muster tenant,** beside muster (same namespace or Compose
  stack), on a **subdomain of muster's own host** (`meet.<muster host>`): a
  different origin from muster, which is what the frame's sandbox relies on
  ([origins](#origins-and-hosts)).
- **Two more small core seams** beside `app`: a [knock notification](#core-seam-knock-notification)
  through muster's existing push path, and a narrow [`tasks` capability](#core-seam-the-tasks-capability)
  so the plugin need not hold `messages`.
- **Proposed: server install (core PR 1h).** An admin installs the app once
  for the whole server and every person gets the tab, with no per-person
  token. The token acts as nobody, and acts for a person only within
  8 hours of a hand-off that person's own browser made
  ([design](#core-seam-server-installed-app-plugins); its forks are answered, decision 20).
- **Two core hardenings gate go-live** of the subdomain: a central origin
  check for every cookie-authenticated write (PR 1g; today many such routes
  check none, so any sibling subdomain can forge them), and `__Host-` cookie
  names (PR 1f). Both are worth shipping on their own.
- **muster still never calls out.** The plugin calls muster (hand-off redeem,
  `GET /plugin/v1/self`, `POST /plugin/v1/tasks`, `POST /plugin/v1/notify`).
  muster calls nothing.

## Goals

1. Voice and video calls, screen share, a live transcript and live action
   items, from inside muster.
2. Starting a meeting is three clicks. Nothing to configure per meeting.
3. Guests need no account: a link, a typed name, the lobby.
4. Past meetings are kept with their transcript, recording and action items,
   for a retention period the owner sets.
5. The core change is generic: any later plugin that needs a full page with
   camera or microphone (a whiteboard, a voice note recorder) uses the same
   `app` capability.
6. Every participant is told, before joining and throughout, that the meeting
   is transcribed and, when it is, recorded.

## Non-goals

- **Scheduling and calendars.** No invites, no calendar sync, no reminders. A
  meeting exists from **New meeting** until it ends.
- **Accounts for guests**, guest history, or guest access to past meetings.
- **Large meetings.** The target is up to 8 participants. Webinars,
  breakout rooms and dial-in by phone are out.
- **End-to-end encryption.** The SFU, the recorder and the transcriber all
  see the media; that is what makes recording and transcripts possible.
- **Languages other than English** in v1 (decision 15). The STT choice keeps
  a path to more languages for later (Nemotron has a multilingual streaming
  variant; Cohere covers 14).
- **Team-owned meeting tabs** in v1 (decision 14): an `app` plugin is
  person-owned. A team's manager minting one for every member is later work.
- **Chief or officer involvement.** The chief does not attend, summarise or
  act on meetings in this design. An action item becomes a task only when the
  owner clicks.
- **Native screen share in the Android app.** The app sends the meeting to
  the system browser ([Android](#the-android-app)).

## What exists today

| piece | where | what it gives this design |
|---|---|---|
| Plugin tokens and capabilities | `internal/plugins/plugins.go:151-167`; [plugins.md](../plugins.md#capabilities) | `messages`, `viewer`, `draft_checks`, `chief_extension`. A fifth slots in beside them |
| Viewer hand-off | `internal/plugins/handoffs.go`, `internal/api/plugin_handoffs.go:49-50`; [plugins.md](../plugins.md#viewer-hand-off) | a 60 s single-use code a plugin redeems to learn the owner. Today the target is only a mirrored window and the purpose only `view`; the redeem route admits only `CapViewer` |
| The preview panel's frame | `internal/ui/plugin_chip.go:100-113` | `sandbox="allow-scripts allow-same-origin allow-forms allow-popups allow-downloads allow-modals"`, `allow=""`, `referrerpolicy="no-referrer"`. No camera, microphone or capture |
| CSP and Permissions-Policy | `internal/api/middleware.go:201-305` | every response carries `Permissions-Policy: microphone=(self)` and a CSP with no `frame-src`, except the two session views, which add the viewer plugins' origins |
| `MUSTER_PLUGIN_ORIGINS` | [configuration.md](../configuration.md#muster-server-optional-features) | the admin's allowlist of plugin origins; a request whose `Host` is under one is refused `421` |
| Sidebar section links | `internal/ui/components.go:923-1190`, `internal/ui/nav_agents.go` (`navMore`), ledger `internal/ui/nav_ledger_test.go` | where a new tab is drawn, and the test that makes every new document decide whether it draws the nav |
| Plugin tasks | [plugins.md](../plugins.md#post-pluginv1tasks-one-task-per-upstream-record) | `POST /plugin/v1/tasks`, idempotent by key, behind `messages` (which also opens messages to the chief and drafts) and `MUSTER_PLUGIN_TASK_NAMESPACES` |
| Dictation's STT | `images/stt-cohere/app.py`, [dictation.md](../dictation.md) | batch transcription of an uploaded file; no streaming |
| The Android shell | `muster-android`, `feature/shell/.../ShellChromeClient.kt` | grants the microphone only to the muster origin, audio alone; denies camera and every other origin |
| Object store | [uploads.md](../uploads.md) | muster's bucket, with a key scoped to `uploads/*` and `pages/*` |
| Session cookies | `internal/api/login.go:203-216`, `login.go:279-286`, `accounts.go:879-883`, `known_device.go:61-68`, `push_routes.go:409-414` | every cookie muster sets is **host-only** (no `Domain`), `HttpOnly`, `SameSite=Lax` ([below](#origins-and-hosts)) |
| CSRF check | `internal/api/auth.go:317-331` (`requireSameOrigin`), `auth.go:363` (`isSameOrigin`), `internal/api/connectors.go:111` (`requireOperatorWrite`) | **per route, not central.** Some session-authenticated writes check the full-host **origin**; many do not (e.g. `POST /tasks`, `/tasks/{id}/edit`, `/tasks/{id}/comments`, `/tasks/{id}/tags`, `/tasks/merge` at `internal/api/notes_routes.go:43-44,134,141,161`; `/agents/{id}/start`, `/stop`, `/name`, `/model` at `internal/api/agents.go:161-171`; `/chief/provision` at `internal/api/server.go:908`). The middleware chain (`internal/api/middleware.go:50-54`) and `requireSession` (`auth.go:277`) check no `Origin`. Those routes rely on `SameSite=Lax`, which does **not** stop a sibling subdomain ([R1, below](#core-seam-a-central-origin-check-for-cookie-authenticated-writes)) |
| Web push | [push.md](../push.md) | per-event toggles on Account → Notifications, tags that replace and withdraw, muster-written text for plugin events |

### Finding: the existing STT service does not stream

`images/stt-cohere/app.py` serves one route that matters,
`POST /v1/audio/transcriptions` (`app.py:420`): a multipart upload of a whole
file, decoded with ffmpeg, run through `model.generate` once, answered as
`{"text": …}`. There is no websocket, no chunked response and no partial
result. The handler is synchronous (`def transcribe`), so concurrent requests
queue on FastAPI's threadpool and share one GPU with dictation.

Cohere's model itself (`CohereLabs/cohere-transcribe-03-2026`, 2B parameters,
Apache-2.0, Open ASR average WER 5.42, about 6 GB VRAM per `app.py:8-10`) is
an offline encoder-decoder: its own long-audio support is "automatic chunking
and reassembly" of a file. Cohere publishes no streaming endpoint for it that
we could find (a search result, not proof:
[docs](https://docs.cohere.com/docs/transcribe),
[model card](https://huggingface.co/CohereLabs/cohere-transcribe-03-2026)).

So the live transcript needs a streaming model ([choice](#choosing-the-streaming-stt-model)),
and the existing service is reused where batch is what is wanted: utterance
mode.

## User flow

### The owner

1. Opens muster and taps **Meetings** in the sidebar. The page fills the
   content area: a **New meeting** button and the list of past meetings,
   newest first.
2. Taps **New meeting**. The meeting is created and the owner is in it: their
   camera preview, a mute and camera toggle, and the link with a **Copy link**
   button. Two switches, both **on** by default: **Record**, and **AI action
   items** ("sends the transcript text, never audio, to an AI service"). Either
   can be turned off at any time; turned off, it cannot be turned back on
   once any guest has knocked (waiting, admitted, denied, or since gone), so nobody
   is recorded or sent to the AI service who knocked while it was off.
3. Pastes the link wherever they like (a chat, an email). That is all the
   setup there is.
4. When a guest knocks, a row appears at the top of the call: "**Ada** wants
   to join · Admit · Deny". One tap admits. Admit all is shown when more than
   one waits. If the owner is not on the meeting's page, their phone gets a
   push, "Someone is waiting to join your meeting"; a tap opens the meeting
   ([knock notification](#core-seam-knock-notification)).
5. During the call: **Share screen**; the live transcript in a side panel
   (a sheet below `sm`), each line under its speaker's name; **Action items**
   in a second panel, each appearing as it is found, each with
   **Make task** and **Dismiss**.
6. **End meeting** ends it for everyone, kills the link and stops the
   recording. Leaving without ending ends it automatically once the room has
   been empty for 10 minutes.
7. Back on the Meetings tab, the meeting is in the list: title (the date and
   time unless renamed), duration, who came. Opening it shows the transcript
   (download as text or WebVTT), the recording (played in the page, with a
   download), the action items, and the summary.

### A guest

1. Opens the link. The page, on the plugin's own origin, says whose meeting it
   is ("*owner label*'s meeting"), and before anything else the notice:
   "This meeting is recorded and transcribed. The transcript text (not the
   audio) is sent to an AI service (OpenRouter) to find action items." Each
   clause is shown only while its switch is on.
2. Types a name (1–40 characters), allows the camera and microphone (or
   joins without them), and taps **Ask to join**.
3. Waits: "Waiting for *owner label* to let you in." The camera preview stays
   on so they can fix their hair.
4. Admitted: they are in the call, with the **same live transcript and
   action-item panels as the owner** (decision 6; they cannot Make task or
   Dismiss), and a permanent badge naming what is on, e.g.
   "Recording · Transcribing · AI".
5. Denied, or the meeting ends while they wait: "You weren't admitted" or
   "This meeting has ended." The link answers `410` from then on.

## Architecture

```
            owner's browser (muster origin)                     guest's browser
 ┌───────────────────────────────────────────────┐        ┌─────────────────────────┐
 │ muster page /apps/<plugin id>                 │        │ https://meet.<host>/j/… │
 │  sidebar · header · <iframe sandbox allow=    │        │ (top level, no account) │
 │   "camera; microphone; display-capture …">    │        └───────┬──────────┬──────┘
 │     └─ https://meet.<host>/enter?muster_code= │                │ HTTPS    │ WebRTC
 └──────────┬────────────────────────┬───────────┘                │          │
     mint   │ POST /plugin-handoffs  │ HTTPS + WebRTC              │          │
            ▼                        ▼                             ▼          ▼
 ┌──────────────────┐   redeem, ┌─────────────────────────────┐  ┌──────────────────────┐
 │  muster-server   │◄──self,───│ muster-meetings (plugin)    │  │ LiveKit SFU          │
 │  (core: `app`    │   tasks   │  owner + guest HTTP, lobby  │──│  + built-in TURN     │
 │   capability)    │           │  LiveKit tokens, webhooks   │  │  (UDP mux, TURN/TLS) │
 └──────────────────┘           │  STT bridge · action worker │◄─│  webhooks            │
                                │  retention sweeper · SQLite │  └───┬────────────┬─────┘
                                └──┬──────────┬───────────┬───┘      │            │
                     PCM per track │          │ windows   │ objects  │ Redis      │
                                   ▼          ▼           │      ┌───▼──────┐ ┌───▼──────┐
                         ┌──────────────┐ ┌────────────┐  │      │ Egress   │ │ Redis    │
                         │ streaming    │ │ OpenRouter │  │      │ (room    │ └──────────┘
                         │ STT (GPU     │ │ (internet) │  │      │ composite│
                         │ host)        │ └────────────┘  │      │ + track) │
                         └──────────────┘                 ▼      └───┬──────┘
                                                  ┌──────────────────▼───┐
                                                  │ object store          │
                                                  │ (meetings bucket)     │
                                                  └───────────────────────┘
```

| component | runs as | owns |
|---|---|---|
| muster core | muster-server | the `app` capability, the tab, the page and frame, the hand-off |
| `muster-meetings` | one Go process, one replica | owner sessions, meetings, the lobby, guest links, LiveKit tokens, the STT bridge, the action-item worker, the retention sweeper, its SQLite database |
| LiveKit server | the upstream image, one node | rooms, media routing, TURN |
| LiveKit Egress | the upstream image | recordings (room composite, MP4), and the per-track audio feed to the STT bridge |
| Redis | the upstream image | LiveKit ↔ Egress coordination (Egress requires it) |
| Streaming STT | a GPU service on **the GPU host that already runs `stt-cohere`** (decision 18) | speech to text, one stream per participant |
| OpenRouter | the internet | action items and the summary |

### Why these boundaries

- **The plugin owns all meeting data.** muster stores no meeting, transcript
  or recording. This keeps the core change small and generic, and keeps
  muster's own schema out of a plugin's feature. It also means removing the
  plugin removes the feature cleanly.
- **One process, SQLite.** Lobby state, the SSE fan-out and the worker loops
  are all per meeting and small. A single replica with a SQLite file on a
  volume (pure Go driver, no cgo) is the simplest thing that works in both
  Kubernetes and Compose. A second replica is not a goal; Postgres is the
  migration path if it ever is.
- **LiveKit, not a hand-built SFU.** It gives rooms, simulcast, screen share,
  TURN, recording and a server API with signed tokens and webhooks.
- **Per-track transcription.** Each participant is one audio track, so each
  transcript line already has its speaker. Diarization would only re-derive,
  less reliably, what the SFU already knows.

## The core change: the `app` capability

Everything in this section is in muster, generic, and names no meetings.

### Capability

| capability | opens | carries |
|---|---|---|
| `app` | a sidebar tab, the page `/apps/{plugin id}`, minting a hand-off with target `{"app": {}}`, and `POST /plugin/v1/handoffs/redeem` for those codes | an **origin** and an **entry URL**, exactly as `viewer` does (same columns, same `ValidViewerLink` rule), plus a **tab label** and a **tab icon** |

- **Migration** `01xx_plugin_app_capability`: two nullable columns on the
  plugin row, `app_label` and `app_icon`, and a check that a token with `app`
  has an origin, an entry URL, a label and an icon. Existing tokens are
  untouched.
- **Minting.** On `/plugins`, an **App tab** checkbox beside the
  `messages`/`viewer` choice, drawn only while `MUSTER_PLUGIN_ORIGINS` is set
  (as `viewer` is). Checked, the form asks for the origin and entry URL (the
  viewer fields), a **Tab label** and a **Tab icon**. `app` may be combined
  with [`tasks`](#core-seam-the-tasks-capability) (what [Make task](#make-this-a-muster-task)
  needs), `messages` and `viewer`. Fixed at mint, as `viewer` is.
- **Not pairable.** `POST /plugin/v1/pair` refuses `app`, for the reason it
  refuses `viewer`: the origin widens muster's own pages' policies, so the
  owner picks it on `/plugins`, never a plugin.
- **The label** is the owner's, not the plugin's: 1 to 24 characters, letters,
  digits, spaces and `- . & '`, trimmed, no two spaces in a row. Rendered as
  text, never HTML.
- **The icon** is one of a fixed list of about 16 emoji the form offers as a
  picker (📹 🎙️ 🗒️ 📋 🧩 …), stored as the list's key. A fixed list avoids
  validating arbitrary grapheme clusters and keeps the sidebar's existing
  emoji-icon style (`sidebarTab`, `components.go:1161`).
- **At most 5** live `app` plugins per owner, so the sidebar cannot be
  flooded; a sixth is refused at mint.
- **Person-owned only** in v1 (decision 14). A team `app` plugin, minted on
  `/teams/<id>/plugins` so every member gets the tab, is later work.

### The tab

- One entry per live `app` plugin of the signed-in owner, in the sidebar's
  section links after the built-in ones, oldest first, as
  `sidebarTab`-shaped links to `/apps/{plugin id}`.
- **Drawn only while every gate holds:** `MUSTER_PLUGINS=1`,
  `MUSTER_PLUGIN_ORIGINS` set, the token live (not revoked, owner not
  disabled), and its origin and entry URL still passing the server's
  **current** `MUSTER_PLUGIN_ORIGINS` (the mint's own check,
  `ErrViewerOriginRetired`). With any gate closed nothing is drawn and the
  plugin store is not read beyond what the gate needs.
- `aria-current="page"` on `/apps/{id}` for its own entry, by the existing
  `activeTab`/`currentTab` split.

### The page: `GET /apps/{plugin id}`

- Signed in, the owner's own live `app` plugin; anything else (another
  owner's, revoked, retired origin, no `app`) is one `404`.
- Draws the sidebar in **overlay** mode (`sidebarIn(…, overlay=true)`), as the
  mirrored views do, so the frame has the whole width at every size. A slim
  header carries the hamburger, the tab label, and **New tab** (in the
  Android app: **Open in browser**, [below](#the-android-app)).
- **Below the header, one frame fills the rest of the viewport**
  (`100dvh` minus the header, `env(safe-area-inset-*)` respected). No message
  bar, no chief panel: this page is the plugin's.
- **Minting at load.** The page's script posts
  `POST /plugin-handoffs {"plugin": <id>, "target": {"app": {}}}` and inserts a
  **new** iframe element with its `src` set before insertion (the preview
  panel's no-history trick), so reloading muster's page re-mints. A refused
  mint (revoked since load, rate cap) is shown in place of the frame with its
  reason.
- Added to `routes.golden`, measured by `cls.spec.ts`, and listed in the nav
  ledger's `navLedgerNav`.

### The hand-off, extended

- `HandoffTarget` gains `App *AppTarget`: `{"app": {}}`, or `{"app": {"ref":
  "<ref>"}}` when the page was opened as `/apps/{id}?ref=<ref>` (a
  [knock notification's](#core-seam-knock-notification) tap). `ref` matches
  `^[A-Za-z0-9_-]{1,64}$` or is dropped; muster passes it through and does not
  interpret it, and the plugin treats it as a hint (it re-checks that the
  meeting is the owner's). Exactly one of `mirror` and `app` is set.
- **The purpose comes from the target, never the request:** a mirror target
  mints `view`, an app target mints `app`. A request that names a purpose is
  ignored for these two (the stepped-up confirm keeps its own word).
- **Capability per target:** an `app` target needs `app`; a `mirror` target
  still needs `viewer`. The redeem route admits a token holding either
  (`plugin_handoffs.go:50` today admits `CapViewer` only), and a code is
  still redeemable only by the plugin it was minted for.
- The redeem answer is unchanged in shape:
  `{"owner_id", "target": {"app": {}}, "purpose": "app", "stepped_up", "minted_at"}`.
- `muster_plugin_handoffs_total{purpose="app",…}` counts it.
- The hand-off routes are gated by `requirePluginViewersOn` today; that gate
  becomes "plugins on and `MUSTER_PLUGIN_ORIGINS` set", which is what it
  already checks, under a name that no longer says viewers.

### Core seam: the `tasks` capability

Decision 11. A token that should file tasks and nothing else.

| capability | opens |
|---|---|
| `tasks` | `POST /plugin/v1/tasks` (filing and commenting by key), nothing else |

- `messages` keeps opening the task routes, so every existing token behaves
  as before (no migration of rows; the capability set gains one value).
- A `tasks` token is refused `403` on `POST /plugin/v1/messages`, the draft
  routes, task references' write queue and chief instructions.
  `route_to_chief` on a `tasks`-only token is refused `400`: routing a notice
  to the chief is a `messages` power.
- Pairable, like `messages` (it widens no page policy). On `/plugins` it is a
  checkbox, **Files tasks**.
- The task routes' check becomes "has `messages` or `tasks`", in the one
  place `requirePluginToken` admits a capability.

### Core seam: knock notification

Decision 10. An `app` plugin may ask muster to push its owner one of a
**fixed set of notifications whose text muster writes**. v1 has one kind,
`knock`.

```
POST /plugin/v1/notify   {"kind": "knock", "ref": "<meeting id>", "count": 2}
DELETE /plugin/v1/notify/<ref>
```

| field | rule |
|---|---|
| `kind` | exactly `knock`; anything else `400` |
| `ref` | `^[A-Za-z0-9_-]{1,64}$`: the plugin's own id for what the push is about |
| `count` | 1–99, how many are waiting |

- **Needs `app`** (the tap opens the app tab; a plugin without one has
  nowhere to send the owner). `403` otherwise.
- **The text is muster's**, never the plugin's: title `<tab label>`, body
  "Someone is waiting to join your meeting" (or "<count> people are waiting
  to join your meeting"). No guest name: it is text a stranger typed, and
  muster's plugin pushes never quote what outsiders wrote (the `plugin`
  event's rule in [push.md](../push.md)).
- **Through the existing push path**: a new event, `app`, with its own toggle
  on Account → Notifications ("An app needs you", on by default), sent to the
  owner's subscribed devices and paired phones (payload v2, `type: "app"`).
  Tag `app-<plugin id>-<ref>`, so a later count replaces the earlier one on
  the device. The tap opens `/apps/<plugin id>?ref=<ref>`, which mints at tap
  time like any visit to the page.
- **Withdrawal:** `DELETE /plugin/v1/notify/<ref>` withdraws it (the
  existing withdrawal, the silent `resolved` on paired phones). The plugin
  calls it when the lobby empties or the owner opens the meeting.
- **Limits:** at most one send per `(plugin, ref)` per 30 s and 30 per plugin
  per hour; past that `429` with `Retry-After`, and the drop is logged as every
  dropped push is.
- **Answers:** `202` queued; `204` the owner has the toggle off or no device
  (not an error: the plugin keeps the lobby row either way); `503` push is not
  configured on this server.
- **When the plugin sends it:** only while no owner session holds that
  meeting's event stream open (the owner is not looking at it), on the first
  knock and when the count rises.
- muster still calls nothing: the plugin calls muster, and muster's own push
  sender does the rest.

### Core seam: `__Host-` cookie names

Decision 16, core PR 1f. **Must land before `meet.<muster host>` is live**
(with 1g). It is not
specific to meetings, and it hardens every deployment against any subdomain.

| today (`file:line`) | becomes |
|---|---|
| `muster_session` (`internal/api/auth.go:20`) | `__Host-muster_session` |
| `muster_at`, `muster_rt` (`internal/api/accounts.go:63-64`) | `__Host-muster_at`, `__Host-muster_rt` |
| `muster_device` (`internal/api/known_device.go:37`) | `__Host-muster_device` |
| `muster_push_device` (`internal/api/push_routes.go:49`) | `__Host-muster_push_device` |

- **What the prefix buys.** A browser stores a `__Host-` cookie only if it is
  `Secure`, has `Path=/` and has **no** `Domain`. A page on a subdomain can
  therefore never set one that muster would read. muster's cookies already
  meet all three, except on the loopback-http page below.
- **The name is chosen per deployment, never per request.** The `Secure`
  flag today is per request: `passwordCookieSecure`
  (`internal/api/login.go:237-239`) drops it when `loopbackHTTPPage`
  (`login.go:248-265`) sees a plain-http request whose `Origin` or `Referer`
  is loopback. A `GET` page load carries no `Origin`, and muster's
  `Referrer-Policy: no-referrer` strips the `Referer`, so the same browser
  would be "loopback" on its sign-in `POST` and "not loopback" on the next
  `GET`. A name chosen that way would be written under one name and read
  under the other: a sign-in loop. So the name comes from configuration,
  once, at boot:
  - **prefixed** (`__Host-…`) when the **resolved** secure-cookie setting,
    `secureCookies(v, publicURL)` (`cmd/muster-server/config.go:2785-2791`),
    is true **and** `MUSTER_PUBLIC_URL` is `https`;
  - **unprefixed** (today's names) otherwise. That includes a plain-http or
    loopback install, and the Compose stack, which sets
    `MUSTER_SECURE_COOKIES=1` on an `http://localhost` URL: there the
    per-request loopback exception still drops `Secure`, and a `__Host-`
    cookie without `Secure` would be refused, so it must stay unprefixed. It
    also includes any deployment where the setting resolves false
    (`MUSTER_SECURE_COOKIES=0`). The per-request `Secure` exception is
    unchanged for unprefixed deployments.

  The boot banner names the choice. One function, `cookieName(base)`, holds
  the rule, and every reader and writer calls it. A prefixed deployment
  never reads an unprefixed name.
- **Boot refusal, by registrable domain.** An unprefixed deployment gets no
  protection from cookie tossing, so **muster refuses to boot** when it
  would use unprefixed names and any `MUSTER_PLUGIN_ORIGINS` entry has the
  same **registrable domain** as `MUSTER_PUBLIC_URL`'s host (eTLD+1, by
  `golang.org/x/net/publicsuffix.EffectiveTLDPlusOne`). The comparison is
  by registrable domain, not "under muster's host": `meet.example.com` is a
  same-site sibling of muster at `app.example.com`, and it can toss cookies
  for `example.com` just as well. An IP-address public URL has no
  registrable domain and compares by exact host.
- **A prefixed deployment reached over loopback http.** An operator of an
  https deployment who opens `http://localhost:<port>` directly would get
  `__Host-` cookies that WebKit refuses over http (Chromium accepts `Secure`
  cookies on `localhost`), so sign-in would silently fail in one engine.
  Instead, on a prefixed deployment, a sign-in or step-up `POST` that arrives
  as a loopback plain-http request (`loopbackHTTPPage`) is refused with a page
  saying "This muster is set up for `<MUSTER_PUBLIC_URL>`. Sign in there."
  No cookie is set. Every other request is unchanged.
- **One sign-in for everyone.** Old names are not read after the upgrade, so
  every browser is signed out once. On its first response after the upgrade,
  muster also expires each old-named cookie (`Max-Age=-1`), so no stale
  copy lingers. The known-device cookie is lost too. Until a browser signs in once more,
  it lacks that cookie's exemption: while the wrong-password limit is
  tripped, a browser with no known-device cookie is refused
  ([testing.md](../testing.md) states the rule).
  The release notes say so.
- **Paired phones and push** are unaffected: the app's device credential is
  not a cookie. The push-device cookie only links a browser to its
  subscription. Losing it means the account page asks that browser to enable
  notifications again, and the old subscription stays until removed.
- **All seven `http.Cookie` literals**, the sets and the clears, go through
  `cookieName` ([the cookie table](#origins-and-hosts)).
- **e2e helpers** that read or set cookies by name (`e2e/lib/signin.ts`) move
  to the new names in the same PR.

### Core seam: a central origin check for cookie-authenticated writes

**Core PR 1g. Must land before `meet.<muster host>` is live**, like 1f. It
closes a gap that exists today on any deployment with a sibling host
([origins](#origins-and-hosts)), so it is worth shipping on its own, first.

- **One check, in `admit`.** `admit` (`internal/api/principal.go:200`) is,
  by its own comment, the only way an auth wrapper may call the next
  handler, so the check goes there. For every request whose method is not
  `GET`, `HEAD` or `OPTIONS` (a **method-less** mux pattern, such as
  `/connect/{connector}/{host}/{path...}` at `internal/api/connectors.go:83`
  or the LLM prefix at `internal/api/officer_llm.go:43`, counts as a write)
  and whose principal was admitted **by a cookie** (`authz.ViaSession`,
  including the GoTrue session path, `requireGoTrueSession`,
  `internal/api/accounts.go:762`), muster requires, before the handler:
  1. **`Sec-Fetch-Site` first.** `same-origin` admits. `same-site`,
     `cross-site` and `none` are refused.
  2. **Only if it is absent, `Origin`:** it must be one of muster's own
     origins (the `wsAllowedOrigins` set that `isSameOrigin`,
     `internal/api/auth.go:363`, already uses). `Origin: null` is refused.
  3. **Only if both are absent, `Referer`:** a same-origin `Referer` admits.
  4. **None of the three:** refused.

  Why `Sec-Fetch-Site` comes first: muster's default `Referrer-Policy:
  no-referrer` (`internal/api/middleware.go:238`) makes a browser send
  **`Origin: null` on a native form `POST` navigation**, while `fetch` from
  the same page sends the real origin. muster has hit this twice already and
  overrides the policy on those pages (`internal/api/composer.go:335-342`,
  `internal/api/login.go:435-441`, both measured in Chromium). An
  `Origin`-first rule would refuse muster's own no-JS forms;
  `Sec-Fetch-Site: same-origin` is sent on them regardless of the referrer
  policy. Go's `net/http.CrossOriginProtection` (Go 1.25+, and `go.mod` is
  1.26) already checks `Sec-Fetch-Site` first and then `Origin` against
  `Host`. 1g builds on it for steps 1–2, with muster's origin set in place of
  `Host`, and adds what it does not do: refusing `none`, refusing
  `Origin: null`, and refusing a request with **no** header (the standard
  library lets that through, for non-browser clients; here a cookie request
  with no header is not one of muster's pages).

  A refusal is `403` with `CrossOriginRefusal`, counted in a new
  `muster_csrf_refusals_total{route_class}` and logged without the body.
- **Who is exempt, and why that is safe.** A request authenticated by a
  bearer credential (`mst_`, `mpl_`, runner, agent, hook and service tokens)
  is not cookie-authenticated: a cross-site page cannot make a browser
  attach those. A request carrying **both** a cookie and a bearer is checked
  by the credential that admitted it. Unauthenticated routes (sign-in, the
  plugin pairing, webhooks) are listed in the ledger with their own reason.
- **What real clients send.** Current Chromium, Firefox and Safari send
  `Sec-Fetch-Site` on every request, and `Origin` on `fetch` and htmx writes;
  a native form `POST` under `no-referrer` sends `Origin: null` (above), which
  is why the rule does not start with `Origin`. Android's WebView is Chromium
  and sends `Sec-Fetch-Site`; the installed PWA is the browser. An older
  browser without `Sec-Fetch-Site` falls to `Origin`, then `Referer`. The muster CLI and
  `muster-mirror` use bearer tokens.
- **Measured (spike S7), headless Chromium 156, Firefox 157 and WebKit 27.2,
  under `Referrer-Policy: no-referrer`:** `fetch`, an htmx-style XHR and a
  native form `POST` all carried `Sec-Fetch-Site: same-origin`; the native
  form carried **`Origin: null`** in all three engines, so the
  `Sec-Fetch-Site`-first order is required, not a preference. A `POST` from a
  sibling origin (`meet.` → muster) **carried the `SameSite=Lax` session
  cookie** with `Sec-Fetch-Site: same-site`: the gap 1g closes is real, and
  step 1 refuses it. **Still owed, on devices:** the Android app's WebView,
  the installed PWA, and Safari with its tracking protections on. By
  documentation the WebView mirrors Chrome for Android (`Sec-Fetch-Site`
  since 76), and Safari's privacy settings change `Referer` and cookies, not
  `Sec-Fetch-*`; neither is measured. They are a 1g go-live check.
- **`requireSameOrigin` and `requireOperatorWrite` stay** where they are; they
  become redundant for cookie requests, and the ledger records that.
- **The route ledger.** A test enumerates every route registered on the mux
  with a method other than `GET`/`HEAD`/`OPTIONS`, **method-less patterns
  included** (from the mux itself, not a hand list), and classifies each as
  `cookie` (guarded by the check), `bearer-only` or `unauthenticated` (with a
  reason). It fails when a route is unclassified; when a `cookie` route
  reaches its handler with a sibling `Origin` or `Sec-Fetch-Site: same-site`;
  when a **`bearer-only`** route admits a **cookie-only** request with a
  sibling `Origin` (so a `cookie` route mislabelled `bearer-only` is caught);
  and when the set **grows or shrinks** without the ledger changing.
- **Size: L** (one middleware is small; the ledger over every write route,
  and the GoTrue path, are not). It touches every form, so the full browser
  suite is its gate.

### Core seam: server-installed app plugins

**Core PR 1h. Proposed; nothing here is built. The
[ten forks below](#1h-forks-for-the-operator) are answered: the operator
accepted every recommendation (decision 20).** Where this section says
how muster works today it cites `file:line` at commit `8ccee8ddc`.

**The operator's intent.** Meetings is for every user of a hosted muster,
self-serve. An admin installs the app once for the whole server, and every
user gets the Meetings tab: no per-user token, and no plugin restart per user.

**Why the current model cannot do it.** An `app` plugin today is a row of
`plugins` owned by one person (`owner_id`, under forced row-level security,
migration 0015). Its token acts as that owner (`requirePluginToken`,
`internal/api/plugins.go:219`, builds the principal from the row's
`owner_id`). The tab, the page and the alive poll are drawn only for that
owner (`liveAppPlugins`, `internal/api/plugin_app.go:59`). A hand-off can be
minted only for the principal's own plugin (`ownedByPrincipal` in
`MintHandoff`, `internal/plugins/handoffs.go:139`), and the code row's
composite foreign key `(plugin_id, owner_id)` → `plugins (id, owner_id)`
(migration 0104) means it cannot name anyone else's. Each owner may hold at
most 5 (`MaxAppPlugins`, `internal/plugins/app.go:22`). So serving N people
takes N tokens, which is why the plugin's phase 2a reads a comma-separated
list of them and restarts to add one.

#### The model in one paragraph

An admin **installs** an app on `/admin/apps`. The install is a plugin row
with **`scope = 'server'` and no owner**, holding `app` and optionally
`tasks`, and its token is shown once. Every signed-in person on the server
gets its tab. The server token **acts as nobody**. It learns a person only
from a hand-off code **that person's own browser** minted on muster's page
and handed it in the frame. Each such redeem records a **grant**: for the
next 8 hours the token may knock and file tasks **for that person, and for
nobody else**, by naming the pairwise id the redeem gave it. It has no route
that lists, searches or counts users. Uninstalling revokes the row: every
user's tab and open frame go, every grant goes, and the token is `401`
everywhere.

#### Who installs, and where

- **Admin only.** `/admin/apps`, behind `requireAdminRole`
  (`internal/api/admin.go:127`; it also serves a password-mode server, whose
  only user is its admin). A member is refused as on every `/admin/*` page.
  Drawn only while plugins are on and `MUSTER_PLUGIN_ORIGINS` is set, like
  the **App tab** box on `/plugins`.
- **The page lists** each installed app: label, icon, origin, entry URL,
  capabilities, who installed it and when, when it was last seen, and how
  many people hold a live grant (a count, never names). Each has
  **Uninstall** and **Rotate token**.
- **Install form:** a name (the `plugins_name_check` slug, unique among live
  server apps), a tab label and an icon (the same `ValidAppLabel` and
  `AppIcons` rules as at mint), an origin picked from `MUSTER_PLUGIN_ORIGINS`
  and an entry URL under it (`ValidViewerLink`), and a **Files tasks** box.
  No other capability is offered: never `messages`, `viewer`,
  `draft_checks` or `chief_extension`. Those act on one person's chief, and a
  server app has no person.
- **Gates.** Install and **Rotate token** need a **fresh step-up** (fork
  5): an install widens every user's `/apps` page policy and mints a
  credential that can reach every user who opens the tab. **Uninstall**
  needs a signed-in admin and a same-origin form, with no fresh sign-in,
  because removing a power is safe. At most **5** live server apps per
  server, so the sidebar cannot be flooded; this is apart from each
  person's own 5.
- **Not pairable.** `POST /plugin/v1/pair` cannot produce a server app,
  for the reason it refuses `app` today.
- **The command line** (fork 6): `muster-server app install|rotate|uninstall|list`,
  in the same family as `muster-server user disable`
  ([auth.md](../auth.md#users-roles-and-invites)), for deployments that
  install from a pipeline. `install` and `rotate` print the token once on
  stdout. No bearer (`mst_`) admin API is added.
- **Audit:** `plugin.server.installed`, `.rotated` and `.uninstalled`
  (actor, name, origin, entry URL, capabilities), and `plugin.server.hidden`
  and `.shown` for a person's opt-out. A redeem writes no audit row: it is
  counted in `muster_plugin_server_grants_total{outcome}`.
- **Who installed it.** From the page, `installed_by` is the admin's user id
  and the audit actor is that admin. From the command line there is no user:
  `installed_by` is NULL, `installed_via` is `cli`, and the audit row's actor
  is `users.Actor{Via: "cli"}` with no user id, as `SetUserDisabledAs`
  (`internal/api/accounts.go:943`) audits a CLI disable. The plugins store's
  own `audit()` (`internal/plugins/store.go:223`) refuses a principal with no
  user, so the CLI writes go through `users.AuditTx` with that actor.
  🔴 `installed_by` is **bookkeeping only**. No `/plugin/v1` handler reads
  it, and it is never an owner, a principal or a push recipient: a knock or a
  task reaches the person it was for, never the admin who installed the app
  ([who each handler acts as](#who-each-route-acts-as)).
- **Rotate** gives the same row a new `token_hash` and kills the old token at
  once. Subjects and grants stay, so the plugin's archive stays reachable
  (fork 7). ⚠ **Rotate ends every live call until the plugin is
  redeployed with the new token.** The running plugin's next `/self` gets
  `401`, and it then ends every owner session and open meeting it holds, as
  on an uninstall. The rotate dialog says so, and rotation belongs in the
  same change window as the plugin's redeploy.

#### The server token: what it is

The same `mpl_` shape, so the "a plugin token opens `/plugin/v1/*` only"
rule still recognises it on every other door. `refusePluginTokenElsewhere`
needs **one change**: its exemption must cover `ViaServerPlugin` as well as
`ViaPlugin`, or it refuses every act-for request
([below](#who-each-route-acts-as)). `Authenticate` (`internal/plugins/store.go:600`) gains its second
arm: a live row with `scope = 'server'` authenticates with no user join and
comes back marked `Scope: server`, `OwnerID: 0`.

🔴 **The door, not the row, decides what a server token is served.**
`requirePluginToken` checks a new table, `serverPluginRoutes`, the shape of
`teamPluginRoutes` (`internal/api/plugins.go:288`): every `/plugin/v1` route,
each marked served or refused, and how it acts. A route not marked served is
`403 ServerPluginRouteRefusal` **whatever the row's capabilities say**. The
table is pinned to `routes.golden` and fails when it grows or shrinks.

| route | served to a server token | acts as |
|---|---|---|
| `GET /plugin/v1/self` | yes | nobody: the answer names no person ([below](#self-for-a-server-token)) |
| `POST /plugin/v1/handoffs/redeem` | yes, for app codes minted **for this server app** | nobody; the answer names the person who minted it, and records the grant |
| `POST /plugin/v1/owners/check` (new) | yes | nobody: which of the subjects it sends hold a live grant |
| `POST /plugin/v1/notify`, `DELETE /plugin/v1/notify/{ref}` | yes, with `Muster-Act-For` | the person whose live grant that subject names |
| `POST /plugin/v1/tasks` | yes, if installed with **Files tasks**, with `Muster-Act-For`; create and comment only (`route_to_chief` and `reference` are `400`, as on a `tasks`-only token today), and only on tasks **this app filed** ([tasks](#tasks-for-a-server-app)) | the same |
| every other `/plugin/v1` route (messages, drafts, checks, media, task writes, `GET /plugin/v1/tasks`, chief instructions, owner-answered) | **no**, `403` | — |

#### Who each route acts as

`authz.WithPrincipal` panics on a principal with no user (`UserID <= 0`,
`internal/authz/authz.go:407`), and a password-mode server has no GoTrue
users store to build one from. So the "acts as nobody" routes **carry no
request principal at all**. They are not given a fake one, and they never
borrow `installed_by`.

| route | request principal | owner id the handler sees | where it comes from |
|---|---|---|---|
| `GET /plugin/v1/self` | **none** | none; `p.OwnerID` is `0` and the handler reads only `p` | — |
| `POST /plugin/v1/handoffs/redeem` | **none** | the code row's `owner_id` | the `UPDATE … RETURNING owner_id` on the code, run as System ([redeem](#the-owner-scoped-redeem)) |
| `POST /plugin/v1/owners/check` | **none** | none: it answers subjects, never owners | — |
| `POST /plugin/v1/notify`, `DELETE /plugin/v1/notify/{ref}` | `{UserID: <grant owner>, Role: <that user's role>, Via: ViaServerPlugin}`, built by `principalFor(grant owner, ViaServerPlugin)` | the principal's `UserID`, which is also the push recipient | the act-for grant row ([below](#acting-for-a-person-muster-act-for)) |
| `POST /plugin/v1/tasks` | the same | the same | the same |

- **The door's second entry.** `requirePluginToken`'s server arm calls a new
  `admitServerPlugin(w, r, next, actFor)` beside `admit`
  (`internal/api/principal.go:200`). It runs the same pre-handler checks
  `admit` does. With `actFor` unset it calls the handler with no principal,
  and any store call that needs one fails closed, as it does today with no
  principal. With `actFor` set it calls `admit` with the grant owner's
  principal. Those three handlers make only explicit `authz.System` calls,
  each keyed on the authenticated app's id and listed in the System ledger.
- 🔴 **Every site that branches on `ViaPlugin` must learn `ViaServerPlugin`.**
  The closed list, and the test that keeps it closed (below):
  - **`requestVias`** (`internal/authz/authz.go:210`) gains
    `ViaServerPlugin`, or the first act-for request panics in
    `WithPrincipal`. It is **not** added to the team-acting set
    (`authz.go:330`) or to `teamAgentDoors` (`internal/api/principal.go:191`):
    a server app never acts as a team.
  - **`refusePluginTokenElsewhere`** (`internal/api/plugins.go:277`) exempts
    only `via == ViaPlugin`. As written it would **`401` every act-for
    request**, because that request carries an `mpl_` token and is admitted
    as `ViaServerPlugin`. It becomes `authz.IsPluginVia(via)`, one predicate
    true for both.
  - **`ruleWriter`** (`internal/plugins/rules.go:336`) refuses only
    `ViaPlugin`, so a server app acting as a person **could write routing
    rules**. It uses `authz.IsPluginVia` too.
  - **The notify recipient** (`internal/api/plugin_notify.go:143`,
    `owner, tag := p.OwnerID, …`, and the withdrawal's `p.OwnerID` at `:205`
    and `:213`) becomes the **request principal's `UserID`** for both
    kinds. For a personal token that is `p.OwnerID`, unchanged. For a server
    token `p.OwnerID` is `0`, and the principal is the grant's person.
  - **`/self`** (`internal/api/plugins.go:332-335`) copies `p.OwnerID` and
    defaults `owner_kind` to `"person"`. It branches on the row's scope:
    for a server row it writes `owner_id: null`, `owner_login: null` and
    `owner_kind: "server"` ([below](#self-for-a-server-token)), never
    `"person"` with owner `0`.
  - **The mint's per-minute cap and prune** (`internal/plugins/handoffs.go:178-191`)
    count and delete `plugin_handoffs` rows `WHERE owner_id = p.OwnerID`,
    which is `0` for a server row: every person minting for a server app
    would share **one** bucket, owned by nobody, and nobody's old rows
    would be pruned. Both are keyed on **the minting person** (the request
    principal's `UserID`) for both kinds. For a personal plugin that is the
    same number, because the row is read under `ownedByPrincipal`. The
    insert's `owner_id` is that same value.
  - **Auto-start** (`internal/api/plugin_autostart.go:136`,
    `owner != p.OwnerID`) is **unreachable** for a server app: auto-start
    needs `messages`, `plugins_server_caps` forbids `messages` on a server
    row, and `serverPluginRoutes` serves no route that reaches it. Pinned
    by a test that inserts a server row with `messages` (refused by the
    CHECK) and one that walks `serverPluginRoutes` and finds no served
    route whose handler reaches `autostartStart`.
- **The pin is structural, not a grep.** `Plugin.OwnerID` becomes
  unexported, and the only way to read it is
  `func (p Plugin) PersonalOwner() (int64, bool)`, which answers `(0, false)`
  for a server row. So every read site must handle the server case to
  compile. Beside it, an enumerated ledger of `PersonalOwner` and
  `authz.ViaPlugin` call sites (from the Go AST, not a text search) fails
  when the set grows or shrinks. A new site must then decide what a server
  token does there.

**What it can never do:** read anything of any person's (no read route is
served); learn that a person exists before that person opens the tab; name a
person by muster's user id; act for a person whose grant has ended; post to
a chief, file a draft or write chief instructions; find, comment on or
learn of a task it did not file; auto-start an agent (that needs
`messages`); be paired.

#### The owner-scoped redeem

1. **The mint.** On `/apps/{id}` for a server app, the person's browser posts
   the same `POST /plugin-handoffs {"plugin": <id>, "target": {"app": {}}}`.
   `MintHandoff` gains its second arm: the row is a live server app, the
   principal is a **person** (never a team's acting principal), the person
   has not hidden it, and its origin still passes the current
   `MUSTER_PLUGIN_ORIGINS`. The code row is **owned by the person** and names
   the app in a new column, `server_plugin_id`, with `plugin_id` NULL
   ([data model](#data-model-and-migration-0172)). The 60-per-minute cap,
   the 60 s life and single use are unchanged.
   - **It records the minting session's sign-in time** on the code row, as
     `signed_in_at`: `stepup.AuthTime(ctx)`, the time the pairing code
     already reads for `paired_signin_at` (`internal/api/devices.go:215`;
     the session's `IssuedAt` in password mode, `internal/api/auth.go:301`).
     A session that records no sign-in time is refused `401` "sign in again",
     as pairing refuses it, rather than mint a code nothing could revoke.
   - **The whole mint transaction runs as `authz.System`** (a System-ledger
     entry), because a server row has no owner and row-level security hides
     it from every principal. Every statement names its owner explicitly:
     the code row's `owner_id` is written as the **principal's `UserID`**,
     taken from the request before the transaction, never from the column's
     `muster.uid` default, which a System context does not set.
2. **The redeem.** `POST /plugin/v1/handoffs/redeem {"code"}` with the server
   token. **One transaction, entirely as `authz.System`** (System-ledger
   entry: the code is how the person is learned, as the token is in
   `pluginFor`), every write naming `owner_id` explicitly:
   - `UPDATE plugin_handoffs SET redeemed_at = now() WHERE code_hash = $1 AND server_plugin_id = $2 AND redeemed_at IS NULL AND expires_at > now() RETURNING owner_id, signed_in_at, …`
     A code minted for a personal plugin has `server_plugin_id` NULL, so no
     server token can spend it, and a server code has `plugin_id` NULL, so no
     personal token can. Unknown, spent, expired or another app's is the one
     `404` it is today.
   - **The person is re-checked, on a fresh read, in the same transaction:**
     an enabled `person`, not hidden from this app, and the code's
     `signed_in_at` strictly after both cutoffs by the whole-second rule
     (`sec(signed_in_at) > sec(sessions_not_before)` and the same against
     `signed_out_at`: the rule the paired-device query uses,
     `internal/devices/devices.go:333-334`, and `atOrBefore` in
     `internal/api/send_queue.go:1343` refuses). A person hidden or disabled
     between the mint and the redeem, or a code minted by a session that a
     later Sign out everywhere ended, is the same `404`, and the code is
     spent all the same.
   - **The grant is upserted** on `plugin_server_users (plugin_id, owner_id)`:
     the subject is kept if the row exists, or a new random 128-bit one is
     made. `signin_at` is set to **the code's `signed_in_at`**, the sign-in
     of the session that minted the code being redeemed, and 🔴 **never
     stamped from `now()`**, on a first redeem or a later one. `granted_at =
     now()` is kept for display only and no gate reads it. `act_until =
     greatest(act_until, now() + 8 h)`, unless `hidden_at` is set, which the
     re-check above has already refused.
3. **The answer** carries no muster user id and no email (fork 2):

   ```json
   {"owner": {"subject": "AAECAwQFBgcICQoLDA0ODw", "display_name": "Ada"},
    "scope": "server", "target": {"app": {}}, "purpose": "app",
    "stepped_up": true, "minted_at": "2000-01-02T03:04:05Z",
    "act_until": "2000-01-02T11:04:05Z"}
   ```

   `subject` is exactly **22 base64url characters** (16 random bytes, no
   padding), as the data model stores it. It is **pairwise**: one person has a different subject under each
   server app, so two apps cannot join their records on it, and it is stable
   for the life of the install, so the plugin keys its data on it.
   `display_name` is the account's display name, or `""` when it has none,
   and the plugin then asks the person. A personal token's answer is unchanged
   (`owner_id` and the rest).

The plugin cannot list users because there is nothing to list from. The only
way a subject comes into existence is a redeem of a code that person's
browser minted.

#### Acting for a person: `Muster-Act-For`

A served "acts as the person" route takes one header,
`Muster-Act-For: <subject>`. The **door** checks it before the handler runs.
The handler then gets the ordinary principal of that person, through a new
`authz.ViaServerPlugin`, and a `plugins.Plugin` carrying the app's id, name
and label. So the notify and task handlers run unchanged, as that person,
under that person's row-level security.

The check, exactly, as one statement under `authz.System` (a System-ledger
entry), keyed on the authenticated app's id and never on anything else the
request says. 🔴 **The gate is one store function, `LiveServerGrants(ctx, app, origins,
subjects)`,** and both act-for (with one subject) and `owners/check`
([below](#self-for-a-server-token)) call it, so the two can never disagree
about who is live. It first checks the app row's origin and entry URL
against the **current** `MUSTER_PLUGIN_ORIGINS` (`ValidViewerLink`; a retired
origin answers no subject at all), then runs one SQL statement built on the
constant `serverGrantLive`:

```sql
-- serverGrantLive: g = plugin_server_users, p = plugins, u = users
      g.plugin_id = $1                   -- the token's own app (from Authenticate)
  AND g.act_until > now()                -- a redeem in the last 8 hours (fork 3)
  AND g.hidden_at IS NULL                -- the person has not hidden the app
  AND g.signin_at IS NOT NULL            -- a grant only a redeem creates
  AND p.revoked_at IS NULL AND p.scope = 'server'
  AND u.kind = 'person' AND u.disabled_at IS NULL
  -- the cutoffs against the SIGN-IN that minted the code, whole seconds,
  -- at-or-before refuses (devices.go:333-334's rule; send_queue.go:1343's atOrBefore)
  AND (u.signed_out_at       IS NULL OR floor(extract(epoch FROM g.signin_at)) > floor(extract(epoch FROM u.signed_out_at)))
  AND (u.sessions_not_before IS NULL OR floor(extract(epoch FROM g.signin_at)) > floor(extract(epoch FROM u.sessions_not_before)))
```

```sql
-- act-for
SELECT g.owner_id FROM plugin_server_users g
  JOIN plugins p ON p.id = g.plugin_id JOIN users u ON u.id = g.owner_id
 WHERE g.subject = $2 AND <serverGrantLive>
```

🔴 **Why the sign-in time and not the redeem time.** In GoTrue mode, the
session check that applies Sign out everywhere works from each process's
memory (the finding of this design's first audit round), so a stolen session
from before the sign-out can still pass `requireSession` on a pod that
restarted, or on another pod. Had the gate compared the cutoffs with the
**redeem** time, that session's code, redeemed after the sign-out, would
have got a fresh 8-hour grant. Comparing with the persisted sign-in time of
the minting session refuses it at the redeem, and again on every act-for
call, whatever any process remembers. The comparison is in whole seconds
with at-or-before refused, the rule the paired-device query and the send
queue already use. (The Go helper is `atOrBefore`; the SQL form is the
paired-device query's.)

Then:

- **No row:** `404 {"error": "no such person for this app"}`, **one body for
  every reason** (unknown subject, another app's subject, expired, hidden,
  disabled, signed out everywhere since the sign-in, a retired origin). So the route is no
  oracle for whether a person exists or what became of them.
- **A row:** the request proceeds as `owner_id`. The principal comes from
  **the row**, never from the header, and a header carrying a number is
  just an unknown subject.
- **Shape:** the header missing on a served act-for route, or present twice,
  is `400`. On a **personal** token the header is `400` on every route, so a
  plugin that mixes the two models fails loudly instead of acting as the
  token's owner.
- "Sign out everywhere" and the session cutoff end every grant at once,
  because each moves `signed_out_at` or `sessions_not_before` to or past the
  grant's `signin_at`. A disable ends it through `u.disabled_at`. A sign-out
  in one browser does not (fork 3).

**The notify state is keyed by person too.** Today the knock caps and the
"which knocks are out" ledger are keyed on `(plugin, ref)` (`appNotifyKey`,
`internal/api/plugin_notify.go:235`). For a server app every key gains the
acting owner, `(plugin, owner, ref)`. Without it a server token acting for B
could **withdraw A's knock** by naming A's ref, and A's knocks would spend
B's caps. The per-plugin caps (30 knocks an hour, 60 withdrawals of nothing
an hour) become **per (plugin, person)**, under a new per-app ceiling of 600
knocks an hour. The person's own knock budget (10 per 10 minutes across all
their app plugins) is unchanged. 🔴 **A server app learns nothing of a person's push
setup** (core PR 1h.1, audit round 1): its caps are spent before the toggle
and device checks, and a person who muted "An app needs you" or has no device
is answered `202` exactly as one who has both (dropped and logged). Only a
personal app plugin keeps the `204`. The knock's title is the server app's label,
the admin's words.

#### Tasks for a server app

`POST /plugin/v1/tasks` today has **no rate cap on filing or commenting**.
Its only `429` is the notice route's (`route_to_chief`), which a server app
may not send. And its key lookup is global across the owner's tasks: it
finds "the open task carrying `key` among **all** your tasks, whichever
plugin or person filed it" (`FindLiveByTag`). For a server app, acting for
someone who never chose it, both are wrong. A global lookup is a
**membership oracle** (does this person have an open task tagged
`meetings:x`?) and a **comment target** (append text to a task another
plugin or the person filed). So for a server token:

- **Only tasks this app filed.** A new ledger, `plugin_server_tasks
  (plugin_id, owner_id, key, task_id)`, `UNIQUE (plugin_id, owner_id, key)`,
  owned by `owner_id` (forced row-level security), gets a row whenever the
  app creates a task. Create and comment resolve `key` **through the ledger
  only**: `(this app, the acting person, key)` → `task_id`. That task must
  be **the open task** by the personal path's own rule, `openOnly`
  (`internal/notes/pgstore.go:832-835`): live (not dismissed), not
  complete, and not tagged `superseded-by:`. A key with no ledger row, or
  whose ledgered task is no longer open, behaves exactly as a key no task
  carries:
  - with `create`, a new task is filed, and the ledger row is **upserted**:
    inserted, or **re-pointed** at the new task when the earlier one was
    completed, dismissed or merged away. All of this happens under an
    advisory lock on `(app, person, key)`, so two retries file one task;
  - with a bare `comment`, the answer is today's
    `200 {"id": null, "created": false, "commented": false}`
    ([plugins.md](../plugins.md#post-pluginv1tasks-one-task-per-upstream-record)).
    It is the same answer whether no task carries the key at all, or only a
    task this app did not file carries it. Nothing is written.

  So a task the person or another plugin tagged with the same key is never
  found, never commented on, and never revealed. A personal token's lookup
  is unchanged.
- **Caps.** Each `(app, person)` may make 60 task writes an hour (a create
  or a comment), and each app 600 an hour across everyone. Past either, the
  answer is `429` with `Retry-After`. A repeat that the route's idempotency
  answers from the stored row spends nothing.
- The comment author is `plugin:<name>`, as today, and keys still need
  `MUSTER_PLUGIN_TASK_NAMESPACES`. No `gate:` or `auto:` tag, and no
  auto-start.

#### `/self` for a server token

```json
{"plugin_id": 9, "plugin_name": "meetings", "scope": "server",
 "owner_id": null, "owner_login": null, "owner_kind": "server",
 "capabilities": ["app", "tasks"], "deliver_to": null, "act_for_hours": 8}
```

No person, no count of people, no list. `401` once the app is uninstalled or
rotated away. **`POST /plugin/v1/owners/check {"subjects": [...]}`** (1 to
200 subjects, each the subject shape; more is `400`) answers
`{"states": {"<subject>": "live" | "expired" | "revoked"}}`, with **exactly one
entry per subject asked about and no other**:

- `live`: the grant passes **`serverGrantLive`**, the same predicate the
  act-for check uses;
- `expired`: **only** `act_until` has passed and every other condition holds
  (the person's next visit to the tab renews it, so the plugin keeps their
  open meeting);
- `revoked`: anything else: hidden, disabled, a team, signed out everywhere
  (or cut off) since the sign-in, the app uninstalled or its origin retired,
  or no grant. A subject this app never redeemed is `revoked` too, so the
  answer cannot tell "never seen" from "ended".

🔴 **Precedence: `revoked` wins.** A subject is `expired` only when the clock
is the SINGLE failing condition; if any other fails, it is `revoked` even
when `act_until` has also passed. Hide sets `act_until = now()`, so an
expiry-first answer would report a hidden person as `expired` and their
meetings would stay open. The state comes from the same statement as the
gate: `serverGrantLive` is `serverGrantInTime AND serverGrantStanding`, and
`LiveServerGrants` evaluates the two halves per row (live is both; expired is
standing alone), so the three states cannot drift from act-for. Never why a
subject is revoked beyond the one word. It can say nothing about a subject
the plugin did not already hold. A personal token is `403`. 60 calls a
minute per app.

#### The tab for every person

- **Who sees it:** every signed-in `person` while the app is live, its origin
  passes, plugins are on and `MUSTER_PLUGIN_ORIGINS` is set. That includes
  people invited after the install, with nothing for anyone to do.
- **Not drawn** for a team's acting principal (decision 14 stands: shared
  team meeting archives are later work),
  for a disabled person (refused at the session door, as today), or for a
  person who has **hidden** it (fork 4).
- **Opt-out:** Account → **Apps** lists every server app with what it may do
  ("can show its page in muster; can notify you; can file tasks for you; it
  learns your display name when you open it") and its live grant ("can act
  for you until 18:00"). **Hide** removes the tab, makes the mint and the
  redeem `404`, ends the grant at once, and makes an open page's alive poll
  `404`, so its frame goes. **Show** clears `hidden_at` only, so the tab is
  back but the plugin may not act for the person until **a fresh redeem**
  (the person opens the tab again). Shown is the default.
  - **What Hide writes:** an upsert on `plugin_server_users (plugin_id,
    owner_id)` setting `hidden_at = now()` **and `act_until = now()`**. When
    the person has never opened the app there is no row yet. Hide then
    inserts one with a new random subject, `signin_at` NULL, `act_until =
    now()` and `hidden_at = now()`. That subject is never sent anywhere, and
    `signin_at IS NULL` alone fails `serverGrantLive`, so the row can never
    act. A later Show then redeem fills `signin_at` and `act_until` as any
    redeem does, keeping that subject.
- **Order:** server apps first, oldest install first, then the person's own
  app plugins, oldest first. At most 5 + 5.
- **One predicate.** `liveAppPlugins` gains the server arm, so the tab, the
  page and the alive poll still ask one function. A server app hidden or
  retired is no tab, a `404` page and a `404` poll, as a personal one is.
- **Personal `app` plugins are unchanged** and keep their own cap of 5.
  Server apps do not count against it. Minting a personal app plugin whose
  entry URL equals a live server app's is refused ("this server already
  offers <label> to everyone"), so one person does not get two tabs into the
  same plugin under two identities (fork 9). 🔴 That check reads **server
  rows**, which row-level security hides from the minting person, so it is
  an explicit `authz.System` read (a System-ledger entry: `SELECT 1 FROM
  plugins WHERE scope = 'server' AND revoked_at IS NULL AND entry_url = $1`).
  Under the person's own principal it would see no server row and **allow
  every mint**.
- **Paths and ids:** a server app is a `plugins` row, so its id shares the
  sequence with personal plugins (fork 1). `/apps/{id}`, the push tag
  `app-<id>-<ref>` and its tap URL need no second namespace.

#### The Android path for a server app

`/enter` refuses a top-level load ([threat model](#threat-model)), so the
personal-app path, where the card's **Open** navigates to the plugin's URL
and the app hands it to a Custom Tab, cannot serve a server app. Sending
**muster's** `/apps/{id}` out instead does not work with today's app either.
`muster-android` keeps muster's own origin inside its WebView (`OriginPolicy.kt`),
and its bridge has no message that opens a URL outside it: the round-2
audit found only `pair`, `open-notification-settings`, `back-claim` and
`touch-start`. Navigating to `/apps/{id}` would therefore stay in the
WebView, where `window.musterApp` is present, so the card would be drawn
again: a loop.

- **A new bridge message, `open-external`** (a muster-android PR, row
  **A1** in the [phased plan](#phased-plan)), posted by the main frame:
  `{"type": "open-external", "url": "https://<muster host>/apps/<id>"}`.
  The app accepts it only when the URL's origin is **exactly** muster's (the
  `OriginPolicy` origin) and its path is `/apps/<digits>`, with an optional
  `?ref=` matching the ref rule. Anything else is ignored and logged. It
  opens the URL with an `ACTION_VIEW` intent in the **system browser**
  (Chrome, or a Custom Tab, which shares Chrome's cookies), never in the
  WebView. That browser frames the plugin as a desktop browser does, so
  `/enter` gets `Sec-Fetch-Dest: iframe`. A person not signed in to muster
  in that browser signs in there first. The app advertises the messages it
  supports as a list, **`window.musterAppMessages`** (muster-android A1), and
  muster's page reads that, never an app version string:
  `!!(window.musterApp && Array.isArray(window.musterAppMessages) &&
  window.musterAppMessages.indexOf('open-external') !== -1)`. An older app has
  `window.musterApp` but no list, so its card is the copy-link fallback. The
  posted URL is exactly `<muster origin>/apps/<id>`, optionally with exactly
  `?ref=<ref>` (the ref rule, raw): no other query, no fragment, no trailing
  slash. The app rebuilds the URL itself and refuses anything else.
- **The card, for a server app**, where `window.musterApp` exists: "<label>
  opens in your browser" and **Open**. Open posts `open-external` for this
  page's own URL and does nothing else. It mints no code and never
  navigates the WebView, so there is no loop.
- **An app too old for `open-external`:** the card has no Open button. It
  says "Open <muster host>/apps/<id> in your browser to use <label>", with
  **Copy link**. Nothing navigates, so there is no loop.
- **Personal app plugins are unchanged:** their card still navigates
  top-level to the minted plugin URL, which the app sends to a Custom Tab,
  because a personal plugin's `/enter` does not carry the server app's
  `Sec-Fetch-Dest` rule.
- **New tab is not drawn for a server app.** The header's **New tab**
  button (`internal/ui/app_page.go:153`) is rendered only for a personal app
  plugin. A server app's page has no top-level way into the plugin from a
  desktop browser.

#### Revocation, uninstall and the alive poll

- **Uninstall** sets `revoked_at` and deletes the app's `plugin_server_users`
  rows, its `plugin_server_tasks` rows (the tasks themselves stay in each
  person's list, with the comments the app wrote) and its unspent codes, in
  one transaction. Revocation is an `UPDATE`, so these deletes are explicit:
  the `ON DELETE CASCADE` foreign keys below fire only if the row itself is
  ever deleted. From then on the token is
  `401` on every route; every person's tab is gone on their next render;
  every open `/apps/{id}` page's 30 s alive poll answers `404` and removes its
  frame, which ends any camera, microphone or screen capture in it. This is
  muster's own enforcement and does not depend on the plugin. Every knock
  still out (muster's ledger knows which) is withdrawn, so no device keeps a
  notification whose tap would open a `404`.
- **The plugin** learns it from `/self`'s `401` within 60 s and ends every
  owner session and open meeting it holds. Per person, it learns a hidden app,
  a disable or a sign-out everywhere from `owners/check` (every 60 s, for the
  subjects with a live owner session or open meeting): a subject answered
  `revoked` ends that person's sessions and meetings; one answered `expired`
  keeps them (the grant only ran out of time).
- **Grant expiry** is not revocation. Past `act_until` a knock for that person
  is `404`, and the plugin keeps its lobby row, as on today's `204`. The
  person's next visit to the tab re-mints and extends the grant.

#### Data model and migration 0172

**`0172_plugin_server_scope.sql`** (written as 0171; `0171_attention_step_up_card`
took 0171 first, so it is 0172). 🔴 Renumber it if another takes 0172 first;
the contiguity test refuses a gap. Idempotent.

- `plugins.scope TEXT NOT NULL DEFAULT 'person'`, `CHECK (scope IN ('person', 'server'))`.
- `plugins.owner_id DROP NOT NULL`, with `plugins_scope_owner`:
  `(scope = 'person') = (owner_id IS NOT NULL)`. 🔴 **A server row has no
  owner, so row-level security shows it to no principal.** `plugins` is in
  0127's table list, so its policies are `team_read`, `team_insert`,
  `team_update` and `team_delete` (0127 replaced 0015's `owner_isolation`).
  Each admits `owner_id = muster.uid`, **or** `owner_id = ANY
  (muster_team_ids(…))`, or `muster.scope = 'all'`. A NULL `owner_id`
  matches neither of the first two, whatever teams the principal belongs to.
  Every read of a server row is a deliberate `authz.System` read in the
  System ledger. A code path that forgets the server arm sees nothing (fails
  closed), rather than seeing the admin's rows.
- `plugins.installed_by BIGINT REFERENCES users (id)` (NULL for a command-line
  install) and `plugins.installed_via TEXT` (`page` or `cli`), both set only
  on server rows: `plugins_server_installer`, `(scope = 'server') =
  (installed_via IS NOT NULL)`. Bookkeeping only (see
  [who installs](#who-installs-and-where)).
- `data_scope` is NOT NULL on every row (0015). A server row stores `none`,
  as an app-only personal token does today: a server app brings no messages
  and has no chief to scope for. `plugins_server_caps` also requires
  `data_scope = 'none'` on server rows.
- `plugins_server_caps`: `scope <> 'server' OR (capabilities <@ ARRAY['app','tasks'] AND 'app' = ANY (capabilities))`.
  **No new capability**, so `plugins_capabilities_known` is not rebuilt.
- `plugins_server_name_live`: a unique index on `(name)` where
  `scope = 'server' AND revoked_at IS NULL` (`plugins_owner_name_live` keys
  on `owner_id`, and NULLs are distinct there).
- `plugin_handoffs.server_plugin_id BIGINT REFERENCES plugins (id) ON DELETE CASCADE`,
  `plugin_id DROP NOT NULL`, and `plugin_handoffs_one_plugin`:
  `(plugin_id IS NULL) <> (server_plugin_id IS NULL)`. The composite FK
  `(plugin_id, owner_id)` stays as it is: under `MATCH SIMPLE` a NULL
  `plugin_id` skips it, and it still guards every personal code. Index on
  `(server_plugin_id)`.
- `plugin_handoffs.signed_in_at TIMESTAMPTZ`: the minting session's sign-in
  time. `plugin_handoffs_server_signin`: `server_plugin_id IS NULL OR
  signed_in_at IS NOT NULL`, so a server code can never lack it.
- **`plugin_server_users`**: `plugin_id` (→ `plugins`, cascade), `owner_id`
  (→ `users`), `subject TEXT` (22 base64url characters), `signin_at` (the
  minting session's sign-in, copied from the code at each redeem, never from
  `now()`; NULL on a row only Hide created), `granted_at` (display only),
  `act_until`, `hidden_at`, `UNIQUE (plugin_id, owner_id)` and
  `UNIQUE (plugin_id, subject)`.
- **`plugin_server_tasks`**: `plugin_id` (→ `plugins (id)`, `ON DELETE
  CASCADE`), `owner_id` (→ `users (id)`), `key`, `task_id` (→ `notes (id)`,
  `ON DELETE CASCADE`: a deleted task takes its ledger row with it), and
  `UNIQUE (plugin_id, owner_id, key)` ([tasks](#tasks-for-a-server-app)).
  The task FK is the **simple** one, as `0053_session_routes_task.sql`
  uses: a composite `(task_id, owner_id)` onto `notes (id, owner_id)` would
  depend on `notes_id_owner_key`, which 0006's body drops and re-adds
  whenever it is re-run. So the store checks, at every ledger write, that
  the task's `owner_id` is the ledger row's. A re-point (`UPDATE … SET
  task_id`) is the upsert's `ON CONFLICT (plugin_id, owner_id, key) DO
  UPDATE`.
- **Both new tables are owned:** forced row-level security under an
  owner-only policy in 0015's `owner_isolation` shape. They are **not** in
  0127's team list, because a team never owns one. So a person's Account →
  Apps reads their own rows and nobody else's, and a team member's
  principal reads no other member's. The redeem, the door and Hide's upsert
  write them under System with an explicit `owner_id`. Both join
  `db.RLSOn`'s table list and the `rls_off` rollback.

🔴 **The RLSOn caveat.** `db.RLSOn` re-applies the bodies of 0104 and 0141
(and 0015's defaults) on every RLS cycle. 0172 is written so that a
re-application cannot undo it:

- it re-states **neither** `plugins_capabilities_known` (0141, 0167, 0168)
  **nor** `plugins_viewer_link` (0104, 0168);
- 0104's `CREATE TABLE IF NOT EXISTS plugin_handoffs` is a no-op on the
  existing table, so its inline `plugin_id NOT NULL` and composite FK are
  never re-added;
- 0015's re-applied body only `SET DEFAULT`s `owner_id`, which a server
  row's explicit NULL overrides.

Each new constraint is still added in the guarded form 0168 uses
(`IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = …)`), so a
later migration that widens one is not narrowed back. The test that proves
it cycles RLS with a server row, a server code and a grant present
([tests](#1h-tests)).

**ROLLBACK** (in the migration's header): uninstall every server app first,
then roll the image back. Then delete the server rows and their codes,
restore `plugin_id NOT NULL` and `owner_id NOT NULL`, drop the new columns,
constraints, index and both tables, and `DELETE FROM schema_migrations WHERE version = 172`.

#### Threat model

The plugin now sees **every person who opens the tab**.

| it may learn | it may not learn |
|---|---|
| a pairwise subject per person, stable for the install | the person's muster user id, email, role, teams, or anything in muster |
| the display name | whether a given person exists before they open the tab |
| that, and when, the person opened the tab, and whether their step-up was fresh | anyone who never opened the tab, or who hid it |
| whatever the person does inside the plugin's own page (the frame is its origin) | the subject that person has under any other server app |
| the person's browser's network address and user agent (they talk to it directly) | another person's anything, through muster |

**How cross-user leakage is prevented, layer by layer:**

1. **No enumeration route.** Nothing a server token is served returns a
   person it did not bring. `owners/check` answers a subset of its own
   input.
2. **The person comes from the code, and the code from that person's
   browser.** Minting needs that person's own session on muster's origin
   (same-origin, and after 1g the central origin check). The code is bound
   to this app, lives 60 s and is spent once.
3. **The principal comes from the grant row,** which is keyed on the
   authenticated app and a 128-bit subject. A request can only name a
   subject; it never names a user id.
4. **Row-level security does the rest.** Once admitted, the request is that
   one person's principal, so a task lands in their rows. A key is matched
   only among the tasks **this app filed for this person** (the
   `plugin_server_tasks` ledger), never among all of theirs.
5. **Per-person keys** for every notify cap and the withdrawal ledger, so one
   person's knocks cannot be withdrawn, throttled or replaced through
   another's.
6. **Muster-written text:** the knock body and title are muster's and the
   admin's words, so the plugin cannot write one person's data into
   another's notification.
7. **Inside the plugin** (not muster's to enforce): one repository layer
   filters every owner-facing query by the session's subject, as 2a already
   does by owner id, and the plugin's mutation sweep keeps its "drop the
   owner filter" gate.

**A stolen server token** can knock for and file tasks for each person who
opened the tab in the last 8 hours and has not hidden it, **if it also holds
their subjects**, which only the plugin's database has. It reads nothing.
Uninstall or rotate ends it at once. That bound, "people active in the last
8 hours", is the reason for fork 3.

**A malicious admin** can install a hostile origin into every person's
sidebar. That is the admin's power by design (the admin already controls
`MUSTER_PLUGIN_ORIGINS`), and it is made visible: the step-up, the audit row,
and Account → Apps listing what each server app may do.

**Sign-in CSRF, now any person to any person.** A link that opens the
plugin's `/enter` with **someone else's** unspent code signs the victim in
as that person (the attacker mints a code in their own muster session, then
sends the URL within 60 s). With a personal token only that token's owner
could be planted. With one server token **any person can plant themselves
on any other**, so the victim might, for example, start a meeting and share
its link inside the attacker's account. Closed in the plugin
([2a changes](#what-changes-in-the-plugins-phase-2a)): **`/enter` refuses
unless `Sec-Fetch-Dest: iframe`**, and owner pages carry `frame-ancestors
<muster's origin>`. Only muster's own page may frame `/enter`, and muster
frames it only with a code it just minted for the signed-in person. A
top-level navigation (`Sec-Fetch-Dest: document`) and a client that sends
no `Sec-Fetch-Dest` are refused by `/enter` itself. Another site can still
frame `/enter` and get `Sec-Fetch-Dest: iframe`; the request arrives and
the code is redeemed, but `frame-ancestors` makes the browser **refuse to
render** the response, so the victim never sees or uses the session it
created. This drops the top-level entries for a server app: **New tab** is
not drawn, and in the Android app the plugin URL is never opened top
level. Its card instead opens **muster's own `/apps/{id}`** in the system
browser, which frames the plugin as any browser does (fork 10,
[the Android path](#the-android-path-for-a-server-app)).

#### 1h tests

Go, in `internal/plugins`, `internal/api` and `internal/db`. Every guard names
the mutation that must turn it red, and each is watched red before it counts.

- **The server route ledger.** `serverPluginRoutes` covers every
  `/plugin/v1` route in `routes.golden` and fails when the set grows or
  shrinks. A server token **whose row is given every capability** (a test
  fixture, past the CHECK) is `403` on each refused route, with the server
  refusal's own message. *Red if:* the door reads the row's capabilities
  instead of the table.
- **Isolation, two people.** A and B; A opens the tab and the stub redeems.
  Act-for A's subject files a task: A's tasks +1 **and** B's unchanged (the
  positive control and the negative in one case). Act-for a subject never
  redeemed, B's user id as the header, and A's subject under a **second**
  server app are each `404` with **identical bodies**. *Red if:* the
  principal is taken from the header, or the subject lookup ignores
  `plugin_id`.
- **Grant gates, one closed at a time, on BOTH consumers.** One table, run
  once against act-for (`POST /plugin/v1/notify`) and once against
  `owners/check`. Its rows: `act_until` passed, hidden, a Hide-only row
  (`signin_at` NULL), disabled, signed out everywhere after the sign-in,
  `sessions_not_before` moved past the sign-in, origin retired, and a team
  row. Each alone is `404` on act-for and **absent from
  `live`** on `owners/check`. The all-open row is `202` and present. Times
  are pinned: the boundary case redeems at `t0` and calls at
  `t0 + 8 h − 1 s` (pass) and `t0 + 8 h` (fail), with `t0` not on an hour
  boundary. *Red if:* any one predicate is dropped (one mutant per line of
  `serverGrantLive`, and one for the origin check in `LiveServerGrants`,
  each caught on both routes). **Uninstalled is not a row of this table:**
  an uninstalled app's token is `401` at the door on both routes, before
  any grant is read, and a separate case asserts that `401`.
- 🔴 **Minted before the sign-out, redeemed after.** A session signs in at
  `s0` and mints a code. Sign out everywhere follows at `s0 + 5 s`. The
  code is redeemed at `s0 + 10 s`, after the sign-out, **as if on another
  process** (the test does not use the in-memory sign-out state at all).
  The redeem is `404`, and no grant row gains a `signin_at`. Its sibling: a
  grant from a session that signed in before the sign-out but redeemed
  before it is live until the sign-out, then `404` on act-for. And the
  whole-second boundary: a sign-in in the **same second** as the cutoff is
  refused, one second later passes. *Red if:* the cutoffs are compared with
  `granted_at` or `now()` (the redeem time); `signin_at` is re-stamped
  from `now()` on a re-redeem; or the comparison is `>=` instead of
  at-or-before refused.
- **The redeem re-checks.** A person hidden, and a person disabled,
  **between the mint and the redeem**, each get a redeem `404`, and the
  code is spent. *Red if:* the redeem trusts the mint's checks.
- **One gate, pinned structurally.** The **gate queries** are pinned, not
  every mention of `act_until`: the upserts (redeem, Hide) and the Account →
  Apps display read legitimately mention it. A test asserts that the set
  of callers of `LiveServerGrants` is exactly the act-for door and the
  `owners/check` handler (from the Go AST), that `serverGrantLive` is
  referenced only inside `LiveServerGrants`, and that no other function
  decides "may this app act for this subject". So a second, drifting
  copy of the gate cannot appear. The behavioural table above is the other half: a structural check
  alone would type-check past a wrong argument.
- **The door runs first.** A spy handler records no call on any act-for
  refusal, and no store call happens but the lookup.
- **Header shape.** Missing or doubled on a server act-for route is `400`;
  present on a personal token is `400` on every route. *Red if:* a personal
  token with the header acts as its owner.
- **Codes do not cross.** A personal plugin's code is `404` to the server
  token, a server code is `404` to a personal token with the same numeric
  id in `muster_plugin`, and one server app cannot spend another's.
- **The redeem answer, whole.** Pinned as a whole string: it has no
  `owner_id` and no email. A fixture whose email, display name and subject
  are pairwise distinct, so a mutant leaking the email under `display_name`
  is caught.
- **Notify keys per person.** A's 30 knocks in an hour do not make B's 31st
  `429`, B cannot withdraw A's ref, and A's withdrawal of a ref A never
  knocked about spends A's allowance only. *Red if:* `appNotifyKey` loses
  the owner.
- **`owners/check`.** It answers only the subset of subjects it was given;
  201 subjects is `400`; a personal token is `403`.
- **Who each route acts as.** A's knock reaches **A's devices and nobody
  else's**: with A, B and the installing admin each holding a fake device,
  act-for A sends exactly one push, to A's device, and none to B's or the
  admin's (fixture user ids pairwise distinct, and the admin's id is never
  1 or the app's id). The same holds for a withdrawal. A task filed for A
  lands in A's rows and not the admin's. *Red if:* the recipient stays
  `p.OwnerID` (it is 0, so the push goes nowhere: the A-device assertion
  catches it), or becomes `installed_by` (the admin-device assertion catches
  it). `/self`, the redeem and `owners/check` run with **no** request
  principal (`authz.From` is false in a spy handler). Act-for runs as
  `ViaServerPlugin` with the grant owner's `UserID`. `ruleWriter` refuses a
  `ViaServerPlugin` principal. An act-for request is not refused by
  `refusePluginTokenElsewhere`. *Red if:* `ViaServerPlugin` is missing from
  `requestVias` (the act-for case panics), the tokens-elsewhere exemption
  stays `== ViaPlugin` (act-for gets `401`), or `ruleWriter` checks only
  `ViaPlugin`.
- **The `ViaPlugin` site ledger.** The set of `authz.ViaPlugin` and
  plugin-`OwnerID` read sites is pinned, failing when it grows or shrinks.
- **Tasks only through the ledger.** A task the person filed with key
  `meetings:x`, and one another plugin filed with it, are each invisible to
  the server app. A bare `comment` on `meetings:x` answers
  `200 {"id": null, "created": false, "commented": false}`, the **same
  body** as for a key no task carries at all (asserted byte for byte), and
  appends nothing to either task. A `create` files a **new** task and
  ledgers it. The server app's own task under that key takes its comment
  (the positive control). *Red if:* the server path calls `FindLiveByTag`
  across the person's tasks, or answers the not-ledgered case differently
  from the unknown-key case.
- **Re-filing re-points.** The app's ledgered task for `meetings:x` is
  completed; then dismissed; then merged away (`superseded-by:`). After
  each, a `create` files a new task and the ledger row now names it, and a
  `comment` lands on the new task, never the old one. *Red if:* the ledger
  uses "live" in place of `openOnly` (the completed case comments on a
  closed task), or inserts instead of upserting (the second create
  conflicts).
- **Task caps.** The 61st write in an hour for one `(app, person)` is
  `429`, and another person's 1st still passes. The app's 601st write in an
  hour, spread over 11 people, is `429`. A repeat answered from the stored
  row spends nothing.
- **The notify ceiling.** With 21 people each under their own 30-an-hour
  cap, the app's 601st knock in an hour is `429` with `Retry-After`. The
  600th passes. *Red if:* the per-app ceiling is removed.
- **Hide.** Hide on a live grant sets `act_until = now()`, after which
  act-for is `404`. Show then act-for, with no redeem in between, is still
  `404`. Show then a redeem then act-for is `202`, with the **same
  subject** as before. Hide by a person who never opened the app creates a
  row that can never act. *Red if:* Hide sets only `hidden_at` (Show revives
  the old grant).
- **Rotate keeps subjects.** After Rotate, the old token is `401` on every
  route. A redeem with the new token returns the **same subject** for a
  person who had one, and their existing grant still passes act-for. *Red
  if:* Rotate recreates the row (new id, new subjects).
- **Fork 9's System read.** With a live server app at entry URL X, a
  person's mint of a personal app plugin at X is refused, and at Y it is
  allowed (the positive control). *Red if:* the check runs under the
  person's own principal: it then sees no server row, and the X mint
  succeeds.
- **Sign-in time on the mint.** A session with no recorded sign-in time is
  refused `401` at the mint. A server code row always has `signed_in_at`
  (the 0172 CHECK, and a test that a mint stores the session's own value).
- **Mint arm.** A team's acting principal, a hidden app, a retired origin
  and an uninstalled app each refuse the mint `404`; a person mints. *Red
  if:* the System read drops `scope = 'server'` (a personal plugin of
  another owner becomes mintable: the case names one).
- **Tab gates.** The existing table gains server rows: hidden, team
  principal and uninstalled draw none; the all-open row draws exactly the
  server tab and the person's own, in that order.
- **Uninstall.** Every grant and unspent code is gone, the token is `401`,
  and each knock still out is withdrawn (a fake push sender records the
  `resolved`).
- **Migration and RLS.** `TestPluginCapabilityChecksMatchGo`'s RLS cycle, or
  a sibling test, runs with a server row, a server code and a grant
  present, and asserts every 0172 constraint is still present afterwards.
  The rollback list is checked against the previous migration.
  `TestEveryPluginRowReadSaysWhoseRowsItReads` classes each new System
  read. *Red if:* 0172's constraints are written unguarded and a re-applied
  body narrows one. **The NULL-owner invariant** (an invariant guard: no
  policy admits a NULL owner today): a server row is invisible to a person's
  principal **that carries team memberships**, so the `team_read` arm's
  `owner_id = ANY (muster_team_ids(…))` is exercised, not only the
  `muster.uid` arm. The same principal reads none of another member's
  `plugin_server_users` or `plugin_server_tasks` rows.
- **Ledgers:** `routes.golden` gains `/admin/apps*`, `POST /plugin/v1/owners/check`
  and the Account → Apps routes; the 1g route ledger classes the admin and
  Account writes `cookie` and `owners/check` `bearer-only`; the nav ledger
  lists `/admin/apps`.
- **The mint's cap is per person.** Two people mint for one server app: 60
  mints by A in a minute make A's 61st `429`, and B's 1st still mints. A's
  rows older than a day are pruned by A's next mint and B's are not. The
  code rows carry each minter's `owner_id`, never `0`. *Red if:* the cap
  or the prune stays keyed on the plugin row's owner (B's 1st is refused).
- **New tab and the Android card.** A server app's `/apps/{id}` renders no
  `data-app-newtab` button, and a personal app's still does (the positive
  control). With `window.musterApp` advertising `open-external`, the card's
  Open posts exactly one `open-external` with this page's URL, mints
  nothing and does not navigate. Without it, the card has no Open, shows the
  copy-link text, and the page makes no navigation in 5 s (the no-loop
  case). In `muster-android` (A1): `open-external` with a foreign origin, a
  path other than `/apps/<digits>`, or a userinfo URL is ignored. With
  muster's own `/apps/7` it fires one `ACTION_VIEW` intent and loads nothing
  in the WebView.
- **Playwright, `plugin-server-app.spec.ts`:** an admin installs the stub
  app (step-up dialog included). Two people in two browser contexts each see
  the tab without anyone restarting the stub, and the stub reports two
  **different** subjects. B hides it and B's open frame goes within 35 s
  while A's stays. The admin uninstalls and A's frame goes within 35 s.

Rows for the [mutation sweep](#mutation-and-negative-controls-for-the-security-gates):
the principal from the header; the subject lookup without `plugin_id`; each
grant predicate dropped; the cutoffs compared with the redeem time;
`signin_at` re-stamped from `now()`; the door reading row capabilities;
`appNotifyKey` without the owner; the notify recipient as `p.OwnerID` or as
`installed_by`; `ViaServerPlugin` missing from `refusePluginTokenElsewhere`'s
exemption or from `ruleWriter`'s refusal; the server task lookup through
`FindLiveByTag`; the per-person task cap, and each per-app ceiling,
removed; Hide without `act_until = now()`; Rotate as a new row; fork 9's
read under the person's principal; the redeem answer with `owner_id`; 0172
unguarded; the mint's System read without `scope = 'server'`. Two for the
subject: **a subject derived from the user id** (for example its decimal
or a hash of it: caught by a fixture whose user ids are known, asserting
the subject is not derivable from them and differs between two installs
with the same users), and **a subject not pairwise across apps** (one
subject per person for every app: caught by the "A's subject under a second
server app is `404`" case and by an assertion that A's two subjects
differ).

#### What changes in the plugin's phase 2a

2a today reads `MUSTER_PLUGIN_TOKEN` as a comma-separated list, one person's
token each (`main.go`), keeps a registry of them (`internal/owners`), and
keys `owners` on muster's `owner_id`. Under 1h:

- **One token.** `MUSTER_PLUGIN_TOKEN` holds exactly one server token. A
  comma, or a token whose `/self` says `scope` is not `server`, refuses to
  boot (fork 8). Adding a person is nothing at all: they open the tab.
- **Boot check:** `/self` answers `scope: server` and `app` among the
  capabilities; `tasks` decides whether **Make task** is drawn.
- **`/enter`** redeems with the one token and accepts only purpose `app`,
  an app target and `scope: server` with an `owner.subject`. `muster_plugin`
  must equal the token's own `plugin_id`; it no longer picks a token.
- 🔴 **`/enter` refuses unless `Sec-Fetch-Dest: iframe`**, before it
  redeems anything, and its pages keep `frame-ancestors <muster's origin>`.
  This closes the any-person-to-any-person sign-in CSRF
  ([threat model](#threat-model)). A request with `document`, any other
  value, or no `Sec-Fetch-Dest` header gets a plain page saying "Open
  Meetings from muster", and the code is **not** redeemed, so it stays
  spendable by the frame it was minted for. muster draws no top-level
  entry for a server app (no New tab; the Android card opens muster's own
  page in the browser: [the Android path](#the-android-path-for-a-server-app),
  fork 10), so no muster path is refused by it.
  `frame-ancestors` does not stop the request. It makes the browser refuse
  to **render** `/enter`'s response in any frame but muster's page, so a
  hostile site that frames `/enter` causes a redeem the victim never sees.
  ⚠ **Measured in Chromium only.** That `Sec-Fetch-Dest: iframe` arrives on
  a framed load was measured in Chromium; Firefox and Safari are owed in
  2d's browser runs. **Safari and iOS before 16.4 send no `Sec-Fetch-*`
  headers**, so on them `/enter` refuses every load: a dead end, not a
  hole. The page they get says "This browser is too old for Meetings.
  Update it, or open muster in a current browser." Tests: `/enter` with `Sec-Fetch-Dest: document`, with `empty`, and
  with none, is each refused and makes no redeem call to the fake muster.
  With `iframe` it redeems (the positive control). And a real-browser case
  (Playwright) loads `/enter` top-level and in muster's frame: top-level is
  refused, framed works. *Red if:* the check is removed, or accepts a
  missing header.
- **SQLite:** `owners` is keyed on `subject` and holds `display_name` (from
  the redeem), and loses `token_ref` and `owner_login`. Owner sessions,
  meetings and audit carry the subject. Nothing is migrated: meetings is not
  live (1f and 1g gate it), so 2a's schema changes in place. If a
  deployment does hold 2a data by then, that assumption is wrong, and it
  needs a one-time map from `owner_id` to subject before the switch.
- **Liveness:** the 60 s `/self` poll now covers the whole app (`401` ends
  every session and meeting). Beside it, a 60 s `owners/check` covers the
  subjects with a live owner session or open meeting: a subject answered
  `revoked` ends that person's sessions and meetings, and `expired` keeps
  them (a meeting may outlast the 8-hour grant).
- **Calls for a person:** `POST /plugin/v1/notify`, its `DELETE` and
  `POST /plugin/v1/tasks` send `Muster-Act-For: <subject>`. A `404` means
  "cannot reach this person now": the knock keeps its lobby row, and
  **Make task** says to open Meetings from muster again.
- **`internal/mustertest`** gains a server mode (subjects, grants with
  expiry, `owners/check`, the act-for header), and the plugin's mutation
  sweep gains "drop the subject filter" and "ignore an `owners/check`
  absence".
- **Docs:** the README's setup becomes "an admin installs it on
  `/admin/apps`", in place of minting a token on `/plugins`.

#### 1h forks for the operator

**All ten are answered: the operator accepted every recommendation**
(decision 20). Each is kept here with its alternatives, for the record.

1. **Where a server app lives.** (a) A `plugins` row with `scope = 'server'`
   and no owner, as above; or (b) its own table and token door.
   **Recommend (a):** one id sequence (so `/apps/{id}`, push tags, metrics
   and the mint body need no second namespace), one token door, and
   fail-closed row-level security on a NULL owner. Its cost is
   `owner_id DROP NOT NULL` and a server arm in `Authenticate` and
   `MintHandoff`. Option (b) never touches `plugins`, but two id spaces
   collide in every place keyed by plugin id (the notify caps and tags
   among them).
2. **What the plugin learns about a person** *(security)*. (a) A pairwise
   random subject and the display name, as above; or (b) muster's
   `owner_id` and `owner_login` (the email), as a personal token's redeem
   and `/self` give today. **Recommend (a):** a server app serves people who
   never chose it, so it gets the least that works. The guest page needs a
   name, not an email, and pairwise ids stop two server apps from joining
   their records.
3. **How long, and on what, the act-for grant lives** *(security)*. (a)
   8 hours from the latest redeem (decision 12's owner session), cut short
   by a disable, sign-out everywhere, hide or uninstall, as above; (b)
   24 hours, the join link's life, so a knock reaches an owner who started a
   meeting late in the window; or (c) (a) and also bound to the one browser
   session that minted the code, so a sign-out in that browser ends it (this
   needs a server-side session row per sign-in mode, and GoTrue mode has
   none today). **Recommend (a).** It bounds a stolen token to "people
   active in the last 8 hours", and the person's next look at the tab
   renews it.
4. **Per-person opt-out.** (a) **Hide** on Account → Apps, default shown,
   as above; (b) no opt-out, so every person always has it; or (c) the
   admin picks who gets it (by role or by person). **Recommend (a):** it is
   self-serve both ways, and hiding also ends the plugin's power to act for
   that person. (c) is more than the brief asks.
5. **A fresh step-up for install and rotate** *(security)*. **Recommend
   yes:** each one widens every person's `/apps` page policy, or mints a
   credential that reaches everyone, which is the class pairing-approve and
   auto-start are already gated for. Uninstall needs none.
6. **Install surfaces.** (a) `/admin/apps` and the `muster-server app` CLI,
   as above; (b) the page only; or (c) also an admin bearer API (`mst_`).
   **Recommend (a).** The CLI serves pipeline installs the way
   `user disable` does. (c) would be the first bearer route that can widen
   every person's page policy, which is too much power for a token.
7. **What an uninstall means for the plugin's data.** (a) **Rotate** keeps
   the row and its subjects, and **uninstall is final**: a later reinstall
   is a new row with new subjects, so the old archive is unreachable from
   muster and the plugin's retention sweeper purges it. Or (b) subjects
   derived from a server key and the app's name, so they survive an
   uninstall and reinstall. **Recommend (a):** uninstall is then a clean
   break, and an admin who only wants a new token has Rotate. The
   uninstall dialog says that people's meeting history in the app stops
   being reachable.
8. **Does the meetings plugin keep per-person tokens?** **Recommend no:**
   2a moves to exactly one server token, as above. The per-owner `app`
   capability stays in core for other plugins, and for a person who runs
   their own.
9. **A personal app plugin with the same entry URL as a server app.**
   (a) Refused at mint, as above; or (b) allowed, giving two tabs into one
   plugin under two identities. **Recommend (a).** A different entry URL
   (a developer's own build, say) is still allowed.
10. **Top-level entry for a server app**, given `/enter`'s
    `Sec-Fetch-Dest: iframe` rule *(security)*. (a) A server app has **no
    New tab**, and in the Android app the card opens **muster's own**
    `/apps/{id}` in the system browser through a new bridge message,
    `open-external` (muster-android PR A1), which then frames the plugin as
    a desktop browser does (a person not signed in there signs in first).
    An app without the message shows a copy-link card, with no loop
    ([the Android path](#the-android-path-for-a-server-app)). (b) Keep a
    top-level `/enter`, behind a plugin-drawn "Continue as <display name>?"
    confirm. **Recommend (a).** A confirm only asks the victim to spot a
    wrong name; it does not close the CSRF.

Decision 14 is unchanged: team-owned meeting tabs, with a shared archive,
are still later work. Under 1h every team member gets the tab as a person.

### The frame, exactly

```html
<iframe sandbox="allow-scripts allow-same-origin allow-forms allow-popups allow-downloads allow-modals"
        allow="camera <O>; microphone <O>; display-capture <O>; fullscreen <O>; autoplay <O>; clipboard-write <O>"
        referrerpolicy="no-referrer"
        src="<entry URL>?muster_code=…&muster_plugin=…">
```

`<O>` is the **entry URL's exact origin** (`https://meet.<muster host>`), never a
wildcard pattern.

- **The sandbox is the preview panel's, unchanged** (`PluginChipSandbox`). No
  `allow-top-navigation`, so the plugin cannot navigate muster's tab.
  `allow-same-origin` is safe for the reason it is there: the frame's origin
  is the plugin's, and a `Host` under `MUSTER_PLUGIN_ORIGINS` is refused `421`.
- **`allow`** delegates exactly six features to exactly one origin:
  `camera`, `microphone`, `display-capture` (screen share), `fullscreen` (a
  shared screen full screen), `autoplay` (remote audio plays without a tap),
  `clipboard-write` (**Copy link** in a cross-origin frame needs it).
- **The page's `Permissions-Policy` must allow the same,** because an
  `allow` attribute cannot delegate past the parent's header, and muster's
  header names `microphone=(self)` (`middleware.go:210`), which would block
  the frame's microphone. On `/apps/{id}` only:

  ```
  Permissions-Policy: microphone=(self "<O>"), camera=(self "<O>"), display-capture=(self "<O>"),
                      fullscreen=(self "<O>"), autoplay=(self "<O>"), clipboard-write=(self "<O>")
  ```

  **Every feature keeps `self`** (spike S5). Chromium lets a parent delegate
  only a feature it holds itself: under `camera=("<O>")` without `self` the
  frame's `getUserMedia` returned audio with **no video track and no error**,
  and `getDisplayMedia` was refused by policy. With `self` added, camera,
  microphone and screen share all worked in the frame, and both negative
  controls (no `display-capture` in `allow`; no frame origin in the header)
  still refused. Holding `self` gives muster's own page nothing in practice:
  its code never asks for a camera or a screen, and the frame is the only
  delegate. Every other response keeps exactly `microphone=(self)`.
  Firefox and Safari do not implement the `Permissions-Policy` header, so
  there the `allow` attribute is the only control, and the frame-attribute
  golden is the security-relevant test for those engines.
- **The page's CSP** gains `frame-src <O>` (no `'self'`: nothing of muster's
  is framed on this page). `frame-ancestors 'none'` and `X-Frame-Options:
  DENY` stay: nothing may frame muster's page. Every other page's CSP is
  unchanged.

### Cookies in the frame

A cross-origin frame's cookies are third-party cookies, which Safari blocks
and Chrome partitions at best. So the contract, written into
[plugins.md](../plugins.md) for any `app` plugin, is: **do not depend on a
cookie in the frame.** The meetings plugin's entry route redeems the code and
renders its page with an owner session token in the page's memory (never in
storage or a cookie); see [owner auth](#the-owner-the-hand-off). A frame
reload re-enters through muster's page, which re-mints.

### When the token is revoked

- **The tab** disappears on the next page render.
- **An open `/apps/{id}` page** polls `GET /ui/apps/{id}/alive` every 30 s
  while visible. A `404` (revoked, owner disabled, origin retired) **removes
  the frame**, which ends the plugin's page and with it the camera,
  microphone and screen capture, and shows "This app was removed." This is
  core's enforcement and does not depend on the plugin behaving.
- **The plugin** learns it from its own `GET /plugin/v1/self` poll (every 60 s)
  and ends the owner's sessions and live meetings ([failure modes](#failure-modes)).

### The Android app

The app opens every origin but muster's in the system browser, and its
`ShellChromeClient` grants only the microphone, only to the muster origin
(`ShellChromeClient.kt`, `grantsMicrophone`). A framed meeting would be denied
its camera and microphone, and Android's WebView has **no screen capture**:
by MDN's compatibility data it mirrors Chrome for Android, whose
`getDisplayMedia` is unsupported (spike S5, documented, not run on a
device). So "Open in browser" stays the v1 answer, and phase 7 can offer
camera and microphone in-app at most, never screen share.

- **v1: open in the browser.** Where `window.musterApp` exists, `/apps/{id}`
  draws no frame. It draws a card, "Meetings opens in your browser", and one
  button, **Open**. The tap mints a hand-off (the same `POST`, at tap time)
  and navigates to the minted URL top-level; the app sends that origin to a
  Custom Tab, where the browser grants camera and microphone the ordinary way
  and the plugin can use a first-party cookie. The code is single use and
  lives 60 s, so the hand-off in a Custom Tab is no wider than **New tab** in
  a browser. No app change is needed for v1 **for a personal app plugin**.
  A server-installed app (core PR 1h) needs one: its `/enter` refuses a
  top-level load, so its card sends muster's own page to the browser
  through the new `open-external` bridge message (muster-android PR A1;
  [the Android path](#the-android-path-for-a-server-app)).
- **Later (optional phase 7): in-app calls.** The main frame (muster's
  origin, already the only origin the bridge accepts) posts
  `{"type": "app-origins", "origins": ["https://meet.<muster host>"]}` on
  `/apps/{id}`. The app then grants `RESOURCE_AUDIO_CAPTURE` and
  `RESOURCE_VIDEO_CAPTURE`, and nothing else, to a request whose origin is
  exactly one of those, after asking Android for `RECORD_AUDIO` and `CAMERA`.
  Manifest gains `CAMERA` (and `ManifestPermissionsTest` with it). Screen
  share stays browser-only. The list is cleared on every main-frame
  navigation.
- **Full screen**, as the preview panel's: the frame fills the screen, the
  header hidden, an **Exit** button over it; Back leaves full screen
  (`back-claim`). In v1 it applies to the browser page, not the app.

### Mobile layout

- The page is the frame: the sidebar is an overlay at every width.
- Inside the plugin (its own CSS): below `sm` the call is one column, the
  active speaker or the shared screen on top, tiles in a horizontal strip,
  controls in a bottom bar with 44 px targets; transcript and action items are
  a bottom sheet with two tabs. From `sm` up they are side panels.
- The lobby row and the recording badge stay visible over the shared screen.

## The plugin: `muster-meetings`

A public repository, `ZacxDev/muster-meetings`, MIT, Go, built from the
template's layout (`internal/muster` client, `internal/mustertest` fake). Its
token: **`app` + `tasks`** (`tasks` only for
[Make task](#make-this-a-muster-task); without it the button is not drawn).
It never holds `messages`.
It is listed on [ecosystem.md](../ecosystem.md) once it meets the bar there.

### Origins and hosts

| host | serves | reached by |
|---|---|---|
| `meet.<muster host>` | the owner app (`/enter`, `/api/*`), the guest page (`/j/*`), the LiveKit webhook | owners (framed), guests (top level), LiveKit (in-cluster) |
| `meet-rtc.<muster host>` | LiveKit signaling (`wss`) | every participant's browser |
| `meet-turn.<muster host>` | TURN/TLS | participants behind restrictive networks |

All three are subdomains of the hosted muster's own host, deployed in the
muster tenant (decision 8). One wildcard certificate for `*.<muster host>`
covers them; single-label siblings (`meet-rtc`, not `rtc.meet`) keep it so.
The admin adds exactly `https://meet.<muster host>` to
`MUSTER_PLUGIN_ORIGINS`. The two LiveKit hosts are never framed and need no
entry. A pattern that covers muster's own host refuses to boot
([configuration.md](../configuration.md#muster-server-optional-features)),
and `meet.<muster host>` does not cover it.

**Why the plugin may not be served from muster's exact origin.** Two
reasons, each enough:

- **muster refuses it.** A request whose `Host` is under
  `MUSTER_PLUGIN_ORIGINS` is answered `421` on every muster listener, so the
  plugin's host can never be muster's. This is deliberate: muster's
  same-origin checks trust a request whose `Host` and `Origin` agree
  ([previews design](previews.md#cookies-origins-and-framing)).
- **The frame would become muster.** The frame carries `allow-same-origin`.
  Framed from muster's own origin, the plugin's script would *be* muster's
  origin: it could read muster's DOM through `window.parent`, call muster's
  APIs with the owner's session (the browser attaches it to same-origin
  requests) and pass every origin check muster has. The sandbox protects
  muster only because the frame's origin differs.

**The cookie check: host-only, so no read; but see the CSRF gap below.**
Every cookie muster sets is **host-only**:
none sets a `Domain` attribute, so the browser never sends one to
`meet.<muster host>`, and all are `HttpOnly`, so no script reads them on
muster's own origin either. The places:

| cookie | set at | attributes |
|---|---|---|
| password-mode session (`muster_session`) | `internal/api/login.go:203-216`, `login.go:279-286` | `Path=/`, `HttpOnly`, `SameSite=Lax`, `Secure` (except loopback http), no `Domain` |
| GoTrue access and refresh | `internal/api/accounts.go:879-883` (cleared `:889-892`) | `Path=/`, `HttpOnly`, `SameSite=Lax`, `Secure`, no `Domain` |
| known device | `internal/api/known_device.go:61-68` | the same, no `Domain` |
| push device | `internal/api/push_routes.go:409-414` (cleared `:417-421`) | the same, no `Domain` |

(Searched `internal/` and `cmd/`, non-test code, at `5fd6f7632`: those
**seven** `http.Cookie` literals, for five cookie names (sets and clears),
are every cookie muster-server writes; none has a
`Domain` field, and no code writes a raw `Set-Cookie` header. The only other
cookie code is the preview gateway's, `internal/previewgw/cookie.go`, a
separate process on its own host. The finding is pinned by a test,
[below](#muster-core-tests), so a later `Domain` turns it red.)

**What host-only does not cover, and why it holds anyway:**

- 🔴 **Same-site: a real CSRF gap today, closed by core PR 1g before the
  subdomain goes live.** `meet.<muster host>` is **same-site** with muster
  (site is the registrable domain), so `SameSite=Lax` does not stop the
  browser attaching muster's cookies to a request a page there makes to
  muster. `requireSameOrigin` (`internal/api/auth.go:317-331`) compares the
  full-host origin and its comment names this exact case, but it guards only
  the routes that opt in. Many cookie-authenticated writes do not
  ([what exists today](#what-exists-today)), so a script on any sibling
  subdomain can blind-POST them with the owner's session today (create or
  merge tasks, start or stop agents, provision the chief). The meetings
  page's own CSP (`connect-src`) does not list muster, but that is the
  plugin's promise, not muster's protection. PR **1g**
  ([the central origin check](#core-seam-a-central-origin-check-for-cookie-authenticated-writes))
  makes it muster's. It is also a pre-existing hardening worth shipping on
  its own: any deployment with a sibling host it does not control has the
  gap now.
- **Cookie flooding.** A sibling can set many large cookies for
  `Domain=<muster host>`, pushing the request's headers past the server's
  limit so muster answers `431` or `400` until they expire (a nuisance denial
  of service, not a read or a write). Accepted: the sibling is the
  operator's own plugin, and clearing site data recovers.
- **Cookie tossing.** A page on `meet.<muster host>` *can* set a cookie with
  `Domain=<muster host>`, which muster would then receive beside its own.
  Today muster's cookie names carry no `__Host-` prefix, so a page there
  could plant a `muster_session` of its choosing (a sign-in-CSRF shape).
  **Closed before the subdomain goes live** (decision 16): core PR **1f**
  renames every muster cookie with the `__Host-` prefix, and a browser refuses
  any `__Host-` cookie that carries a `Domain`, so no subdomain can set one
  ([the rename](#core-seam-__host--cookie-names)).
- HSTS on muster has no `includeSubDomains` (`internal/api/middleware.go:243-246`), so it
  neither helps nor hinders; the meetings hosts serve HTTPS only.

### The owner: the hand-off

1. muster's page frames `https://meet.<muster host>/enter?muster_code=…&muster_plugin=…`.
2. `/enter` calls `POST /plugin/v1/handoffs/redeem`. It refuses any answer
   whose purpose is not `app` or whose target is not `{"app": {}}`.
3. It creates an **owner session**: 32 random bytes, stored as SHA-256, bound
   to `owner_id`, valid 8 hours, and renders the app page with the token in a
   script variable. The page calls `/api/*` with `Authorization: Bearer
   mmo_<token>`; no cookie is set. `history.replaceState` removes the code,
   and the response carries `Referrer-Policy: no-referrer`.
4. A page reload inside the frame finds no token in memory and shows "Open
   Meetings from muster again"; reloading muster's page re-mints.

The plugin holds one token per owner it serves (`muster_plugin` picks it), as
previews do; one owner is the common case. Proposed core PR 1h replaces this
with one server-installed token for every person
([server install](#core-seam-server-installed-app-plugins),
[what changes in 2a](#what-changes-in-the-plugins-phase-2a)).

### Guests: links, the lobby, admission

- **The link** is `https://meet.<muster host>/j/<code>`, `<code>` 128 random bits
  (22 base64url characters), stored as SHA-256. It is a right to **knock**,
  not to join. It dies when the meeting ends (`410`), and in any case
  24 hours after the meeting was created.
- **Knock:** `POST /j/<code>/knock {"name", "notice": {"record", "ai"}}` →
  `{"ticket"}`, a second random 128-bit value held in the guest page's
  memory. `notice` is the state of the two switches **the page showed the
  guest**. If it differs from the meeting's current state (the owner turned a
  switch on after the page loaded and before anyone knocked), the knock is
  refused `409 {"notice": {current}}` and the page re-shows the notice with
  the current wording before the guest can knock again. The name is trimmed, 1–40
  characters, control and bidi characters refused; it is shown to the owner
  as text, with "(guest)" after it, never as HTML.
- **Wait:** `POST /j/<code>/wait {"ticket"}` answers an event stream (read
  with `fetch`, not `EventSource`, so the ticket travels in the body and never
  in a URL or an access log) that ends with `admitted {"livekit_url",
  "token"}`, `denied`, or `ended`.
- **Admit:** the owner's `POST /api/meetings/{id}/lobby/{pid}/admit` mints the
  guest's LiveKit token and sends it down that guest's wait stream. **A guest
  never holds a LiveKit token before admission**, so the lobby cannot be
  bypassed by talking to LiveKit directly.
- **Limits:** 20 waiting knocks per meeting, 10 knocks per source address per
  10 minutes, 8 admitted participants (owner included) by default. Over the
  limit is `429`, never a silent drop.
- **The owner can remove** an admitted guest. Removal, in this order: the
  identity is marked revoked in SQLite, LiveKit's `RemoveParticipant` drops
  the connection, and the ticket dies, so the guest must knock again and gets
  a new identity if admitted. A LiveKit JWT stays valid until its expiry, and
  LiveKit refreshes tokens for connected clients, so the token alone cannot
  be trusted to end access. Two backstops: guest tokens live **2 minutes**
  (only to connect), and on every `participant_joined` webhook the plugin
  calls `RemoveParticipant` at once for any identity that is not currently
  admitted in its own state (revoked, removed, denied, or unknown). A
  webhook can be lost, so a reconcile loop also calls `ListParticipants` for
  each live meeting every 15 s and removes any identity not admitted.

### LiveKit tokens

| who | grants | TTL |
|---|---|---|
| owner | `roomJoin`, `room=<meeting room>`, `canPublish` (camera, microphone, screen_share, screen_share_audio), `canSubscribe`, `roomAdmin`, `canPublishData` | 2 min to connect |
| guest | `roomJoin`, `room=<meeting room>`, `canPublish` (camera, microphone, screen_share, screen_share_audio), `canSubscribe`. **Not** `roomAdmin`, **not** `canPublishData`, **not** `hidden` | 2 min to connect; the `participant_joined` check [above](#guests-links-the-lobby-admission) ends any identity not admitted |
| the plugin itself | server API key, never sent to a browser | — |

Transcript lines and action items reach browsers over the plugin's own SSE,
not LiveKit data messages, so no participant can publish a fake transcript
line. Identities are `owner-<id>` and `guest-<random>`; the display name is a
LiveKit participant attribute set by the plugin when it mints the token.

### Meeting lifecycle

| state | entered by | the link | LiveKit room |
|---|---|---|---|
| `open` | **New meeting** | knocks accepted | created with `empty_timeout` 10 min, `max_participants` 8 |
| `ended` | **End meeting**, `room_finished` webhook, the 24 h cap, or the owner's token revoked | `410` | deleted (`DeleteRoom` disconnects everyone) |

Ending stops the egress, closes every STT stream, runs one last
action-item window and the summary, and refuses every wait stream with
`ended`.

### Data model (the plugin's SQLite)

| table | columns (abridged) |
|---|---|
| `owners` | `owner_id`, `owner_login` (from `/self`), `token_ref` (after 1h: `subject` and `display_name` from the redeem, in place of all three), settings: `record_default` (on), `ai_default` (on), `retention_days_recording` (30), `retention_days_text` (365) |
| `owner_sessions` | `token_sha256`, `owner_id`, `created_at`, `expires_at` |
| `meetings` | `id` (random), `owner_id`, `title`, `room`, `join_code_sha256`, `state`, `record`, `ai`, `created_at`, `started_at`, `ended_at`, `purge_recording_at`, `purge_text_at` |
| `participants` | `id`, `meeting_id`, `kind` (owner/guest), `display_name`, `identity`, `ticket_sha256`, `state` (waiting/admitted/denied/removed/left), `knocked_at`, `admitted_at`, `left_at` |
| `segments` | `id`, `meeting_id`, `participant_id`, `start_ms`, `end_ms`, `text`, `engine`, `pass` (live/final) |
| `action_items` | `id`, `meeting_id`, `text`, `assignee`, `due`, `source_segment_ids`, `window_seq`, `model`, `status` (open/dismissed/tasked), `task_id`, `created_at` |
| `windows` | `meeting_id`, `seq`, `from_segment`, `to_segment`, `model`, `latency_ms`, `tokens_in`, `tokens_out`, `cost_usd`, `outcome` |
| `recordings` | `meeting_id`, `egress_id`, `object_key`, `bytes`, `duration_s`, `state` |
| `summaries` | `meeting_id`, `text`, `model`, `created_at` |
| `audit` | `at`, `owner_id`, `meeting_id`, `event`, `detail` (no transcript text) |

Every owner-facing query is filtered by the session's `owner_id` in one
repository layer, so no handler writes its own `WHERE owner_id`.

### The plugin's HTTP API

Owner routes take `Authorization: Bearer mmo_…`; a missing or expired session
is `401`, another owner's meeting is `404`.

| route | does |
|---|---|
| `GET /enter` | redeem, create the owner session, render the app |
| `GET /api/meetings` | past and live meetings, newest first, paged |
| `POST /api/meetings {"title"?, "record"?}` | create; answers `{"id", "join_url"}` |
| `GET /api/meetings/{id}` | one meeting: participants, recording state, items, summary |
| `POST /api/meetings/{id}/token` | the owner's LiveKit token |
| `POST /api/meetings/{id}/record {"on"}` · `POST /api/meetings/{id}/ai {"on"}` | off at any time; on again only while **no guest is waiting or admitted** (and none has been) |
| `GET /api/meetings/{id}/events` | SSE: lobby changes, transcript segments, action items, recording state |
| `POST /api/meetings/{id}/lobby/{pid}/admit` · `/deny` · `DELETE …/participants/{pid}` | lobby and removal |
| `POST /api/meetings/{id}/end` | end it |
| `GET /api/meetings/{id}/transcript?format=json\|txt\|vtt` | the transcript |
| `POST /api/meetings/{id}/recording-url` | `{"url", "expires_at"}`: a URL on the plugin's own origin, `/media/<meeting id>?exp=…&sig=…` (HMAC over the path and expiry, valid 15 minutes). A `<video src>` or a download link cannot carry the bearer header, so the page asks for this URL with its bearer and hands the URL to the element. `/media/…` streams the object from the store through the plugin with `Range` support (seeking), so the store is never exposed to browsers |
| `POST /api/items/{id}/task` · `POST /api/items/{id}/dismiss` | [Make task](#make-this-a-muster-task), dismiss |
| `DELETE /api/meetings/{id}` | delete now: rows and objects |
| `GET/PUT /api/settings` | record and AI defaults, retention |

Guest and machine routes:

| route | does |
|---|---|
| `GET /j/{code}` | the guest page (`410` once ended) |
| `POST /j/{code}/knock` · `GET /j/{code}/wait` | above |
| `POST /j/{code}/events {"ticket"}` | event stream (read with `fetch`) for an admitted guest: transcript, items, and the current notice state |
| `POST /livekit/webhook` | LiveKit's events; the `Authorization` JWT is verified against the API secret and the body's SHA-256 inside it; anything else `401` |
| `GET /healthz`, `GET /metrics`, `WS /stt/ingest/<track>` | on an **internal** port only. The ingest URL is `/stt/ingest/<track>/<token>`, where `<token>` is an HMAC of the track id and an expiry under `MEETINGS_INGEST_SECRET`: Egress sends **no** `Authorization` header or any other credential on its websocket upgrade (spike S2), so the URL is the only place a secret can go. Wrong or expired token: `401`. The plugin's access log records `/stt/ingest/<track>` only, never the token; Egress's own log does record the full URL it was given, so tokens are per track and expire with the meeting |

Every page sets its own strict CSP (`default-src 'self'`; `connect-src 'self'
wss://meet-rtc.<muster host> https://meet-rtc.<muster host>`, the second for
the LiveKit client's `/rtc/validate` check on a failed connect; `media-src
'self' blob:`, for the recording via `/media/…` and the call's own streams;
`frame-ancestors <muster's origin>` on owner pages and `'none'` on guest
pages), `Referrer-Policy: no-referrer`, and `Permissions-Policy:
camera=(self), microphone=(self), display-capture=(self)`. Access logs
record the path only: no query string is logged, and no route takes a code
or ticket in its query apart from `/enter`'s one-time `muster_code`, which is
dead once redeemed.

## Media: LiveKit, TURN, Egress

- **LiveKit server**, one node, its config from a mounted file: API key and
  secret from a secret, `room.empty_timeout`, `room.max_participants`, the
  webhook URL `http://muster-meetings:…/livekit/webhook`, Redis address.
- **The public media path is TURN over TLS on 443, and nothing else**
  (decision 17). The deployment reaches the internet through a relay that
  carries **TCP only**; v1 opens **no UDP port** on the public node. So:
  - one **SNI-passthrough** route on TCP 443 at the public address that
    serves it: `meet-turn.<muster host>` → LiveKit's built-in TURN/TLS
    listener on the server host. Nothing terminates TLS on the way. Which
    public address and listener carries this route is a deployment choice
    ([operator steps](#deployment)): when measured, the relay itself had no
    443 listener, public 443 was the ingress controller on a different
    address, and whether 443 was free on the relay's address was not
    checked;
  - a **publicly trusted** certificate for `meet-turn.<muster host>`,
    mounted into LiveKit (`turn.cert_file`, `turn.key_file`). Chromium
    validates it: with a self-signed certificate the TURN/TLS connection
    failed (S1);
  - LiveKit **always advertises `turns:<turn.domain>:443`**, whatever
    `turn.tls_port` is (measured), which is exactly the port the route
    serves;
  - **TURN relay allocations stay local to the server host: UNVERIFIED**
    (derived, not measured through the relay). The reasoning: a TURN
    client's relay candidate is allocated on the server host itself, and the
    SFU reaches it there, so no relay port range needs forwarding. For that,
    `rtc.node_ip` is the server host's own address, and since LiveKit's
    built-in TURN refuses private peer addresses unless listed (measured),
    that address goes in `turn.allow_restricted_peer_cidrs`. S1 measured the
    opposite layout failing (with `node_ip` a public relay address, the SFU's
    checks to the relay candidate went out to that address and none came
    back), which is why the derivation matters. **Deploy step 6's
    real-network test is what confirms or refutes it**; if it fails, the
    TURN relay range must be narrowed and forwarded, which is a change to
    decision 17. `rtc.interfaces` is pinned (the UDP mux otherwise binds
    every interface separately).
  - Signalling (`wss://meet-rtc.<muster host>`) goes through the ordinary
    HTTPS ingress, as `meet.<muster host>` does.
- **What that costs.** Every remote participant's media is TCP inside TLS,
  relayed. On a LAN, joining took **251 ms** over forced TURN/TLS against
  **123 ms** over the UDP mux (S1, pod to pod), and TCP adds head-of-line
  delay under packet loss. Real-network round trips (a phone on a mobile
  network, a UDP-blocked laptop) are still owed: they need the route live.
  LiveKit's direct UDP mux (`7882/udp`) and TCP (`7881/tcp`) listeners are
  **not exposed** by any Service or published port in v1, in Kubernetes or
  Compose, so every client, LAN clients included, connects through
  TURN/TLS. Exposing them to the LAN, or UDP publicly, is a later decision.
- **TURN: LiveKit's built-in TURN, not coturn.** It shares LiveKit's
  credentials (no separate TURN secret to rotate) and is one fewer service.
  It worked for TURN/UDP and TURN/TLS in S1.
- **Recording: room composite Egress** (a headless Chrome rendering the
  default grid layout) to MP4 in the meetings bucket, key
  `meetings/<owner_id>/<meeting_id>/recording.mp4`. Started when the first
  guest is admitted with **Record** on, stopped at end.
- **Egress capacity: a 15-second admission hold, not a reservation**
  (spike S3). Egress admits a job if `total × 0.8 − Σ max(pending cost, last
  measured CPU)` covers the job's cost (`roomCompositeCpuCost = 4`,
  `trackCpuCost = 0.5`, `maxCpuUtilization = 0.8`,
  `pkg/config/service.go:32-39`; the check in `pkg/stats/monitor.go`). The
  cost is held **only for a job's first 15 s**; after that the job counts at
  its **measured** CPU. Measured for one 8-person meeting:

  | load | CPU (cores) | memory |
  |---|---|---|
  | recorded: room composite + 8–9 track egresses (10 min steady) | **1.41 avg, 1.69 peak** | **1.56 GiB** |
  | one track egress | **≈ 0.02 avg**, 0.07 peak | ≈ 85 MB |
  | unrecorded: track egresses only | ≈ 0.2 for nine | ≈ 350 MiB |

  The recording is H.264 720p30 at ~3 Mb/s plus AAC: **~1.4 GB per recorded
  hour**, which sizes the bucket and retention. Two refusals were measured
  on a 6-CPU Egress node (budget 4.8):
  - **A burst of starts.** At meeting start the `track_published` webhooks
    arrive together: 7 of 8 track starts beside a composite were refused,
    then all accepted 20 s later.
  - **A second recording, permanently.** A second room composite, 46 s
    later with every hold expired, was **refused**: 4.8 − ~1.3 measured =
    3.5, below the composite's cost of 4. At default costs a 6-CPU node
    takes **one recorded meeting at a time**, although two would use about
    2.8 cores.

  So the deployment sets `cpu_cost.track_cpu_cost` to **0.1** (measured
  peak 0.07) and `cpu_cost.room_composite_cpu_cost` to **2** (measured
  composite alone: 1.14 avg, 1.57 peak; not below 2), and the plugin
  retries a refused track start with backoff (1, 2, 4 … 30 s), marking a
  transcript gap until it succeeds. **Sizing:** an Egress node needs about
  `(1.7 × concurrent recordings + 0.1 × concurrent microphones) / 0.8`
  CPUs, so 6 CPUs carries two recorded 8-person meetings at once. The
  plugin's `MEETINGS_MAX_RECORDINGS` (default 2) caps concurrent recordings
  to what the node was sized for; a meeting over the cap, or whose composite
  start is refused after retries, shows "Recording unavailable" to everyone
  and is not recorded (the notice changes with it). Track egress stays the STT feed: the Go-SDK
  participant is not needed on cost grounds.
- **Audio for STT: track Egress to a websocket** (per audio track, raw PCM to
  `ws://muster-meetings:…/stt/ingest/<track>/<token>`), started by the plugin
  on every `track_published` webhook for a microphone track. Confirmed for
  31 minutes beside nine other track egresses and a composite (spike S2),
  with these rules, each measured:
  - **Format:** binary frames of **1920 bytes every 10 ms**, interleaved
    **16-bit little-endian stereo at 48 kHz**, the two channels identical
    (the mono track duplicated). No header, no text frames, no timestamps.
    The ingest takes one channel and resamples to **16 kHz mono** for both
    `nemotron` and `utterance`.
  - **No credential is sent** on the upgrade, so the secret is the URL's
    `<token>` ([the route](#the-plugins-http-api)).
  - **The ingest must not ping.** Egress never answers websocket pings and
    never reconnects: with a 20 s ping interval and 20 s timeout every stream
    died at exactly 40 s. The ingest server runs with pings and pong
    timeouts off.
  - **A dropped stream ends that egress for good,** so the plugin handles
    `egress_ended` / `egress_failed` for an STT track by starting a new track
    egress, and marks the transcript gap until audio flows again.
  - **Timestamp by sample count** from the stream's start, never by arrival:
    under host contention frames arrived in bursts after stalls of up to
    7 s (late, not lost; every stream recovered). The ingest buffers several
    seconds.

  On Kubernetes a NetworkPolicy admits only Egress to the ingest port, and
  in Compose the ingest port is on an internal network only Egress joins.
- **The object store** is a bucket of its own (`meetings`), with an access
  key scoped to `meetings/*` that only the plugin and Egress hold. muster's
  own upload key is not reused and gains nothing.

## Transcription

### Choosing the streaming STT model

**Decision 19 (operator): NVIDIA Nemotron Speech Streaming en 0.6B.** The
Kyutai model this page recommended before is not used, and its deployment is
being retired.

**The benchmark the decision rests on.** 99 real dictation recordings,
streamed live in 80 ms chunks at real time, 8 streams at once, on one 16 GB
GPU. The reference is the stored transcript from the existing batch STT,
which is itself imperfect (about 13% WER), so these are **agreement**
figures, not absolute accuracy:

| model (serving) | WER vs reference, empties included | empty outputs | first word p50 / p95 | end-of-stream flush (final after the recording ends) p50 / p95 | GPU memory |
|---|---|---|---|---|---|
| **Nemotron Speech Streaming 0.6B**, 560 ms chunks | **19.0%** | **2** | 1.61 / 2.74 s | **0.01 / 0.02 s** | ~6.8 GB (fp32, untuned) |
| faster-whisper large-v3-turbo via WhisperLive | 20.4% | 3 | 1.79 / 2.86 s | 1.52 / 1.94 s | ~2.5 GB |
| Kyutai `stt-1b-en_fr` | 31.1% | 8 | 1.58 / 2.93 s | 0.27 / 0.29 s | 3.6 GB |

Kyutai's 8 empties were recordings with clipped or DC-offset microphone audio
that it read as non-speech, which is the same failure S4 saw as "silent
starts". Nemotron is the most accurate of the three and flushes its last
words almost at once when a stream ends. That column is an **end-of-stream
flush** (each recording was one stream that then ended), not the latency of
finalising at a pause inside continuous speech, which a meeting needs and
which is measured separately ([STT evaluation](#stt-evaluation)).
faster-whisper turbo is close on accuracy and uses less memory, but its
end-of-stream flush took 1.5–1.9 s; it is the
**named fallback model** if Nemotron cannot be made to fit
([memory](#serving-nemotron)).

The vendor figures this page compared first, for context (not measured on
our hardware):

| model | streaming | latency | WER (vendor) | serving | licence |
|---|---|---|---|---|---|
| **NVIDIA Nemotron Speech Streaming en 0.6B** (chosen) | native, cache-aware FastConformer RNNT | 80–1120 ms chunks | avg 7.07 at 560 ms | NeMo; Riva/NIM (gRPC) | NVIDIA Open Model License |
| faster-whisper large-v3-turbo + WhisperLive (fallback model) | emulation (re-decodes a rolling buffer) | 1–3 s | — | websocket | MIT |
| Kyutai `stt-1b-en_fr` (not used) | native (delayed streams) | 0.5 s fixed delay | — | Rust `moshi-server` | CC-BY-4.0 |
| Mistral Voxtral Mini 4B Realtime | native | 0.16–2.4 s | FLEURS 6.73–8.72 | vLLM `/v1/realtime`; needs ≥16 GB | Apache-2.0 |
| Parakeet TDT 0.6B v2/v3 | chunked emulation | chunk-dependent | Open ASR 6.05 / 6.34 | NeMo, no bundled server | CC-BY-4.0 |
| Cohere Transcribe (existing) | **no** | per utterance | **5.42** | the existing `stt-cohere` | Apache-2.0 |

Sources: [Nemotron card](https://huggingface.co/nvidia/nemotron-speech-streaming-en-0.6b),
[WhisperLive](https://github.com/collabora/WhisperLive),
[Kyutai repo](https://github.com/kyutai-labs/delayed-streams-modeling),
[Voxtral Realtime card](https://huggingface.co/mistralai/Voxtral-Mini-4B-Realtime-2602),
[Parakeet v3 card](https://huggingface.co/nvidia/parakeet-tdt-0.6b-v3).

### Serving Nemotron

Two ways to serve it:

| | **NeMo cache-aware streaming behind a small websocket server** (v1) | Riva / NIM |
|---|---|---|
| what it is | a FastAPI websocket server in this repository's `images/stt-nemotron/` (the same shape as `images/stt-cohere/`), calling NeMo's cache-aware streaming step per stream | NVIDIA's packaged streaming ASR server, gRPC, TensorRT engines |
| memory | fp32 measured at ~6.8 GB; **bf16/fp16 weights and activations** are the first lever (UNVERIFIED how far it falls) | TensorRT fp16, typically smaller (UNVERIFIED for this model) |
| batching | ours to write: one decode step for all live streams per chunk tick | built in |
| licence and footprint | the model's licence only; a pinned Python image like `stt-cohere` | NIM for production use falls under NVIDIA's enterprise terms (UNVERIFIED for this deployment), and a large container |
| protocol | ours (below), shared with the fake and the tests | Riva's gRPC; LiveKit has a Riva STT plugin, but the plugin talks to muster-meetings, not to LiveKit Agents |

**v1 picks the NeMo websocket server.** It keeps the licence to the model's
own, keeps the image pinned and reviewable like `stt-cohere`, and puts the
protocol under our control, so the plugin's engine, the fake and the
evaluation harness all speak one thing. Riva is the path if the NeMo server
cannot batch enough streams per GPU after the bf16 work. **Measured fit
beside `stt-cohere` on the 16 GB GPU** (fp32 6.8 GB + `stt-cohere`'s ~6 GB
would leave little for activations and dictation bursts) is a **phase 3b**
item: the PR that ships the server records its memory at 1, 4 and 8 streams
in bf16, and the stream cap below is set from that measurement.

**Input: 16 kHz mono, UNVERIFIED.** It is the usual rate for NeMo ASR models
and the rate the benchmark fed, but the model card does not state it. Phase
3b confirms it two ways: the server reads `preprocessor.sample_rate` from
the `.nemo` config at start and **refuses to start** unless it is 16000, and
the 3b PR records the rate the benchmark harness actually fed. The ingest
takes one channel of Egress's S16LE stereo 48 kHz, resamples it to 16 kHz,
and sends it in 80 ms frames; the server runs the model at the **560 ms**
chunk size the benchmark used.

**Endpointing: the plugin's energy VAD, the same one the `utterance` engine
uses.** The model emits text per chunk and has no end-of-utterance output
(its card points to a separate end-of-utterance model). v1 does not add that
second model: it would be another GPU resident with no measurement behind
it. Instead the plugin, which already endpoints for the `utterance` engine,
decides where an utterance ends (energy below threshold for **700 ms**,
after removing DC offset with a high-pass filter, since clipped and
DC-offset microphones were the empties' cause) and sends the server a
`flush`; the server answers with the `final` for the text since the last
flush and keeps its streaming state. One endpointer serves both engines, so
a pause finalises the same way whichever engine a participant is on. The
end-of-utterance model is the upgrade if the evaluation's pause bar fails.

**Protocol** (`wss://<stt host>/v1/stream`, `Authorization: Bearer <token>`,
the token in the plugin's own Secret):

- client → server: binary frames, S16LE mono 16 kHz, exactly 80 ms (1280
  samples, 2560 bytes) each; a text frame `{"type": "flush"}` at each end
  of utterance the plugin's VAD finds; and `{"type": "end", "samples": N}`
  when the track ends. **The last frame is zero-padded to 1280 samples**,
  and `samples` is the true total, so the server trims the padding from its
  timestamps.
- server → client: `{"type": "partial", "text", "start_ms", "end_ms"}` as a
  chunk is decoded, and `{"type": "final", "text", "start_ms", "end_ms"}` in
  answer to each `flush` and to `end`; times are in the stream's own audio
  time (sample count), never wall time.
- **A frame of any other size** is refused: the server sends
  `{"type": "error", "code": "bad_frame"}` and closes with websocket code
  1003. The plugin treats that as a mid-stream drop (below) and logs it; it
  is a bug, never expected.
- **Capacity:** the server takes at most `STT_MAX_STREAMS` streams (set per
  GPU from the 3b measurement; 8 until then). Size it to at least
  `room.max_participants` (8) × the concurrent meetings wanted; streams over
  it fall back (below), they are never queued. A connection over the cap is
  refused **before the upgrade** with `503` and `Retry-After`, never accepted
  and then closed, so the client can tell "full" from "broken".
- The server sends no pings it requires answered (the plugin is its only
  client, and its own read loop detects a dead peer).

**When the Nemotron server is full, unreachable or drops a stream**, in this
order of precedence:

1. **At stream open, `503` or a failed connect** → that participant goes to
   the `utterance` engine at once, and the panel shows "Captions delayed".
   The plugin retries Nemotron after `max(30 s, Retry-After)` and moves the
   participant back when a stream opens.
2. **Mid-stream drop** (the socket closes or errors after the upgrade,
   `bad_frame` included) → up to 3 reconnects at 1, 2 and 4 s, with a gap
   marker for the audio not transcribed meanwhile; if all fail, step 1.
3. **A full outage** puts every participant on `utterance`, which is capped
   at 4 transcriptions in flight per plugin (it shares `stt-cohere` with
   dictation). Utterances beyond the cap queue per participant, up to 30 s
   of audio each; past that the oldest are dropped with a gap marker and the
   panel shows "Transcript paused" instead of "Captions delayed". If
   `stt-cohere` is also down, "Transcript paused" and gap markers only.

### Three engines, one interface

```go
type Engine interface {
    // Open starts one stream for one participant's track.
    Open(ctx context.Context, track TrackRef) (Stream, error)
}
type Stream interface {
    Write(pcm []int16) error            // 16 kHz mono (one channel of Egress' S16LE stereo 48 kHz, resampled)
    Segments() <-chan Segment           // partial and final, with start/end ms
    Close() error
}
```

| engine | `MEETINGS_STT_ENGINE` | output | use |
|---|---|---|---|
| `nemotron` | `nemotron` + `MEETINGS_STREAM_STT_URL` (`wss://…/v1/stream`) and `MEETINGS_STREAM_STT_TOKEN`, **and** `MEETINGS_STT_URL`/`MEETINGS_STT_TOKEN` for its fallback (all four or the plugin refuses to boot) | partials per 560 ms chunk, a final at each VAD flush | live captions (decision 19); falls back per participant to `utterance` [as above](#serving-nemotron) |
| `utterance` | `utterance` + `MEETINGS_STT_URL`/`MEETINGS_STT_TOKEN` pointed at the existing `stt-cohere` | one final per utterance, after a pause (≤15 s segments, energy-based endpointing) | zero new service; dev; Compose without a second GPU service |
| `fake` | `fake` | scripted text per participant | tests only; refuses to start unless `MEETINGS_TEST_MODE=1` |

The utterance engine sends each VAD-cut utterance to the existing
`POST /v1/audio/transcriptions`. Captions appear sentence by sentence, about
1–2 s after the speaker pauses. That is good enough for action items, which
run on 10 s windows anyway, but not word-by-word. It shares the GPU and the
threadpool with dictation, so it is capped at 4 in flight.

**No post-meeting re-transcription in v1.** The track egress streams to the
ingest and stores nothing, so there is no per-track audio to re-transcribe,
and the room composite mixes everyone into one track, which loses the
per-speaker split. Storing per-track audio would add objects to the data
model, retention, Delete now and the consent notice for one gain (archive
accuracy). Dropped as the simpler option; it can return later as its own
proposal.

### Segments

- Each segment carries its participant, its start and end in meeting time
  (milliseconds from the room's start), its text, and whether it is final.
- Partials are sent to the SSE streams and **never stored**. Finals are
  stored and sent.
- A silent or muted track sends nothing (the engine's VAD, and a
  constant-signal check for the utterance engine, as `app.py` already does).
- If no engine can take a participant's audio (the precedence in
  [serving Nemotron](#serving-nemotron)), the panel says "Transcript paused"
  and the gap is stored as a marker, so the archive shows where text is
  missing rather than appearing complete. On the fallback engine alone it
  says "Captions delayed".

## Action items

### What ScaleDown Extract is

`scaledown/extract` is a model on OpenRouter from the provider ScaleDown
([OpenRouter page](https://openrouter.ai/scaledown/extract/api); also in
OpenRouter's `/api/v1/models`, canonical slug `scaledown/extract-20260928` when
measured). It is **description-driven
entity extraction**: the caller describes the fields it wants in plain
English, and it returns **spans with character offsets into the input**, each
with a type and a confidence
([ScaleDown docs](https://docs.scaledown.ai/api-reference/extract-overview)).

- Price: $0.04 per million input tokens, output free (OpenRouter's listing).
- Context: 1M tokens. Listed parameters: `max_tokens` only; no
  `response_format`, `tools` or structured outputs through OpenRouter.
- **Request shape (spike S6):** the fields are a **record, name →
  description**, in `provider.options.scaledown.entities`:

  ```json
  {"model": "scaledown/extract",
   "messages": [{"role": "user", "content": "[s1] Ada: I'll send the deck by Friday.\n…"}],
   "provider": {"data_collection": "deny",
                "options": {"scaledown": {"entities": {"action_item": "<definition>",
                                                       "due_date": "<definition>"}}}}}
  ```

  A list is refused `400`; no entities is refused `400`. It answered under
  `data_collection: deny`.
- **Response:** `choices[0].message.content` is a JSON **string**,
  `{"entities": [{"text", "type", "confidence", "start", "end", "context"}], …}`.
- 🔴 **`text` is not always copied from the input.** In 1 of 22 entities it
  was a rewrite ("Fay will have a draft Tuesday" where `input[start:end]` was
  "I'll have a draft Tuesday"). The adapter maps spans to segments **by
  `start`/`end` only**, checks the offsets fall inside the window, and shows
  `input[start:end]`, never `text`, as the item.
- Latency 0.6–2.7 s per call, p50 0.69 s over 10 windows.
- Not context compression: that is the separate `scaledown/compress`.

**What that means here.** Extract's items are spans of the transcript. "Ada: sure, I'll send the deck Friday" becomes the span "I'll send
the deck Friday", not "Ada to send the deck by Friday". The assignee then
comes from the segment's speaker (which we know from the track), and the due
date from a second entity type. Whether that reads well enough is what the
evaluation decides. It is cheap enough that the cost is nearly zero either
way.

**The fallback: `deepseek/deepseek-v4.1-flash-1017`**, a dated snapshot, a
chat model with `response_format` (JSON schema) support, which rewrites each
item as owner, task, due.

- **Price: $0.30 / M in, $1.20 / M out at peak, $0.15 / $0.60 off-peak**
  (OpenRouter's time-of-day pricing, when measured). An earlier revision of
  this page quoted $0.036 / $0.50: the `:batch` variant is $0.036 in and
  $0.15 out. It is not used by default because its latency is untested;
  whether it is acceptable is an [open question](#open-questions).
- **Schema honoured:** strict JSON schema answered valid JSON 14 of 14 times
  in S6, including against a prompt telling it to break the shape, and a
  hidden required field came back every time, so the schema reaches the
  provider. Strict decoding on every provider is not shown at that sample
  size.
- 🔴 **The weights are pinned; the serving is not.** The dated slug is
  served by about 50 providers at mixed quantizations (fp4, mxfp4, fp8), and
  the answering provider changed on nearly every S6 request. So
  `MEETINGS_ITEMS_PROVIDER_PREFS` **pins the provider** for it
  (`order` with `allow_fallbacks: false`, or `quantizations`), and the
  evaluation runs under exactly that pin. The undated slug and the
  `~deepseek/deepseek-flash-latest` alias are not used.

### The worker

- **Window:** every `MEETINGS_ITEMS_EVERY` (default 10 s) while new final
  segments exist (no new segment, no call). Input: the final segments of the
  last 90 s (each with its id and speaker), plus the meeting's current items
  (id and text), for context.
- **Output:** new items only, each citing the segment ids it came from.
- **Deterministic guards after the model** (the model is not trusted to
  obey):
  - an item citing no segment, or a segment outside the window, is dropped;
  - an item whose normalised text (lower case, punctuation and stop words
    removed) has token Jaccard ≥ 0.7 with an existing item, or whose cited
    segments are all already cited by one existing item, is a duplicate and
    dropped;
  - at most 5 new items per window.
- **Adapters** behind one interface:
  `extract` (ScaleDown: entities `action_item`, `due_date`; spans mapped to
  segment ids by offset; assignee from the speaker, or a name in the span) and
  `chat_json` (any OpenRouter chat model with a JSON schema).
- **Config:**

  | variable | default | meaning |
  |---|---|---|
  | `MEETINGS_ITEMS_MODEL` | the evaluation's winner | an OpenRouter model slug; `off` turns action items off |
  | `MEETINGS_ITEMS_ADAPTER` | derived from the slug | `extract` or `chat_json` |
  | `MEETINGS_ITEMS_EVERY` | `10s` | window period |
  | `OPENROUTER_API_KEY` | — | required unless `off` |
  | `MEETINGS_ITEMS_PROVIDER_PREFS` | `{"data_collection": "deny"}` plus, for a multi-provider model, a provider pin (`order` + `allow_fallbacks: false`, or `quantizations`) | passed as OpenRouter's `provider` object: only providers that do not retain prompts, and for DeepSeek the one provider the evaluation ran on. Per-request `deny` makes routing independent of the account's own privacy setting |

- **Live:** each new item is sent on the meeting's SSE stream as it is stored,
  and appears in the panel with a short highlight. Its source lines are linked
  in the transcript.
- **Failures:** a 429 or 5xx backs off (1, 2, 4 … 60 s) and the next window
  covers the gap (the window is anchored on the last segment processed, not on
  time). The panel says "Action items paused" after 3 failures in a row.

### Action-item evaluation

A harness in the plugin repository, `cmd/items-eval`, run by hand or on a
schedule against OpenRouter, never in CI (it costs money and needs a key).

**The corpus**, `testdata/items-eval/`, written for the purpose (no real
meeting):

- **12 meetings, about 150 windows** of 6–12 segments each, as they would
  arrive: speaker-labelled segments with ids. Four kinds: stand-up, planning,
  a customer call, a one-to-one.
- **Labels per meeting:** each expected item with its canonical text,
  assignee, due date if stated, and the **segment ids that state it**.
- **At least 30% of windows have no item.** Hard cases, each in several
  windows: hypotheticals ("we could maybe…"), past tense ("I already
  sent it"), questions, a commitment later retracted, an assignment across
  two speakers ("can you take that?" / "sure"), the same item restated in a
  later window (dedupe), and ASR-style noise (a variant of each meeting
  passed through the real STT from synthesised speech, so the errors are real
  STT errors).

**Matching is by segments, not by wording:** an emitted item matches an
expected one when their cited segment ids intersect, in the same meeting.
This keeps the scorer deterministic; no LLM judges an LLM.

**The harness replays each meeting window by window through the real worker
code** (guards and dedupe included), so it measures what ships.

| metric | definition | pass bar |
|---|---|---|
| recall | expected items matched by ≥1 emitted item | **≥ 0.80** |
| precision | emitted items matching an expected item | **≥ 0.80** |
| duplicate rate | expected items matched by >1 emitted item | **≤ 0.05** |
| assignee accuracy | matched items with the right assignee | ≥ 0.85 (reported; not a gate for `extract`) |
| latency | per-window wall time, p50 / p95 | **p95 ≤ 4 s** |
| schema failures | responses that do not parse to the expected shape | **≤ 1%** |
| cost | USD per meeting-hour, from OpenRouter's reported usage | **≤ $0.05** |

- Each model runs **3 times**; the worst run is the score.
- **Production's routing, exactly.** The harness sends every request through
  the same client code with the same OpenRouter `provider` object as
  production (`MEETINGS_ITEMS_PROVIDER_PREFS`, default `{"data_collection":
  "deny"}`), so a model that passes is one that routes under that policy.
  The report records which provider answered each request. `scaledown/extract`
  has a **single provider** (ScaleDown), and it answered under
  `data_collection: deny` in S6; if that ever stops, every request fails in
  the harness as it would in production, and that is a failed run, not a
  skipped one. DeepSeek is evaluated under the **pinned** provider only. A model that
  passes only with looser preferences is a decision for the operator, not a
  default.
- **Smoke result, not the gate (S6):** on 10 synthetic windows with 11
  labelled items, `scaledown/extract` scored precision 0.91 / recall 0.91
  (assignee 0.80), and `deepseek/deepseek-v4.1-flash-1017` 1.00 / 1.00
  (assignee 1.00), both at 0 duplicates and 0 schema failures. Extract kept a
  retracted commitment, missed an implicit "I'll pair with you", and assigned
  "please write up your goals" to the speaker instead of the person asked;
  its confidence did not separate true from false items, so no threshold is
  used. Estimated cost per meeting-hour at these window sizes: ≈ $0.005
  (extract), ≈ $0.025 (DeepSeek at peak). n = 10, one run each: it points
  the same way as the fallback choice but decides nothing. **The full
  evaluation above is still the gate.**
- **Choice:** `scaledown/extract` if it passes every gate; else
  `deepseek/deepseek-v4.1-flash-1017` if it does; if both pass, the one with the
  better recall, then the cheaper. If neither passes, action items ship
  `off` and the result is reported to the operator.
- **The harness's own controls**, run first every time: a **null** model
  (emits nothing) must score recall 0; an **oracle** (emits the labels) must
  score 1.0 on recall and precision; a **duplicator** (emits every label
  twice) must fail the duplicate gate; a **liar** (cites segments outside the
  window) must have every item dropped by the guards. If any control does not
  score as stated, the run fails and no model is scored.
- The output is a table per model, committed as
  `docs/items-eval-<date>.md` in the plugin repository with the exact model
  slugs and dates.

### STT evaluation

`cmd/stt-eval`, run on the GPU host by hand:

- **Corpus:** a held-out subset of the AMI meeting corpus (CC-BY-4.0), using
  its **individual headset** channels, one per speaker, which is exactly what
  per-track transcription sees. About 1 hour, 4 meetings.
- **Replay:** each channel streamed in real time to the engine, all channels
  of a meeting at once.
- **A second leg, the operator's own audio:** the benchmark behind decision
  19 (99 real dictation recordings, live 80 ms chunks, 8 streams, agreement
  with the existing batch STT's stored transcripts) is re-run by the same
  harness against the deployed server. Its corpus is private and is never
  committed; only its scores are. **Bar, relative to an fp32 re-run on the
  same harness** (so the harness, not the decision's one measured point, is
  the baseline): the deployed bf16 server's agreement WER at most **1.0
  percentage point** above fp32's, empties at most **one** more, and the
  end-of-stream flush p95 ≤ 0.1 s. Agreement is not accuracy (the reference
  itself is ~13% WER), so it guards against regressions; AMI is the
  accuracy leg.
- **Pause finalisation, on continuous audio** (the AMI leg): from the end
  of a reference utterance to its `final`, p95 **≤ 1.5 s** (the 700 ms VAD
  pause plus the flush). This is the latency a meeting's transcript and its
  action-item window see; the benchmark's end-of-stream flush does not
  measure it.
- **Metrics:** WER per channel (Whisper's English normaliser), first-word
  and finalisation latency (audio time of a word's end to its final
  segment), p50/p95, GPU memory at 1, 4 and 8 concurrent streams in bf16, and
  whether latency holds at each.
- **Pass bar for live use:** WER ≤ 18% on AMI headset, p95 finalisation
  ≤ 2.5 s, 8 concurrent streams with p95 still ≤ 2.5 s, fits in the GPU memory
  left beside `stt-cohere`.
- **Controls:** a silent channel must produce no text; a channel of a known
  read passage must produce WER under 10%; the harness must report a higher
  WER when fed the wrong channel's reference (it can go red). Two of the
  three ran against the earlier candidate in S4 and behaved (silence: 0
  words; wrong reference: WER 0.90–1.21). **All three are owed against
  Nemotron**, and the known-read-passage control has **never run** on any
  model (phase 3d).
- **Silent start and empty-output check:** the harness, and production
  telemetry, flag a track with speech energy but no words for more than 20 s,
  and count empty outputs per leg. Empties were the earlier candidate's main
  failure (clipped or DC-offset audio read as non-speech), and Nemotron still
  had 2 of 99; the harness includes clipped and DC-offset fixtures for that
  reason.
- **Status:** the dictation benchmark above stands in for S4; **the AMI
  continuous-channel run, the bf16 memory measurement and all three controls
  against Nemotron are owed** (phases 3b and 3d).

## Extras: what is included and why

| extra | included | why |
|---|---|---|
| **(a) Make task** on an action item | **yes**, phase 6 | One click, and the plugin API already does it idempotently; through the new narrow `tasks` capability (decision 11) |
| **(b) Recording/transcribing notice + per-meeting toggles** | **yes, in the MVP**, not as an extra | Recording is on by default (decision 9), so the notice is not optional: it ships with the first recording and the first transcript. Toggles: **Record** and **AI action items**, both on, both off per meeting |
| **(c) Post-meeting summary** | **yes**, phase 6 | One LLM call at meeting end, on the same transcript; cheap, and it is what makes the past-meetings list scannable. Follows the **AI action items** switch: off, no summary |
| **(d) Retention** for recordings and transcripts | **yes**, phase 5 with recording | Recordings are the largest and most sensitive objects; they need an end date from day one. Defaults 30 days (recordings) and 365 days (text), decision 9 |
| Post-meeting Cohere re-transcription | **no** (dropped) | There is no per-track audio to re-transcribe; storing it would widen the data model, retention and consent for archive accuracy alone ([why](#three-engines-one-interface)) |
| Knock notification to the owner's phone | **yes**, phase 1e (core) + 2b (plugin) | Decision 10: a [core seam](#core-seam-knock-notification) on muster's existing push path, muster-written text |
| Chat in the call | **no** | Not asked for; every participant already has a chat elsewhere |

### Make this a muster task

`POST /plugin/v1/tasks` with:

```json
{"key": "meetings:<meeting id>-<item id>",
 "create": {"title": "<item text, cut at 120>",
            "body": "From the meeting \"<title>\" on <date>.\nAssignee: <assignee>\nDue: <due>\n\n> <the cited transcript lines, with speakers>"}}
```

- Needs `meetings` in the deployment's `MUSTER_PLUGIN_TASK_NAMESPACES`; when
  the route answers `503`, the button says "Tasks are not set up on this
  muster" and nothing is retried silently.
- Idempotent by key, so a double tap files one task. The answer's `id` is
  stored on the item, and the button becomes "Task #<id>".
- The body is plugin text, so muster fences it for any agent later started on
  the task ([plugins.md](../plugins.md#post-pluginv1tasks-one-task-per-upstream-record)).
  The transcript lines are guest speech: that fence is exactly right.

### Summary

At meeting end, if the meeting's **AI action items** switch is on, one
`chat_json` call (`MEETINGS_SUMMARY_MODEL`, default the DeepSeek model, `off`
allowed server-wide) over the final transcript, cut to its last
60,000 tokens: 3–6 bullet points and the decisions made. Stored, shown at the
top of the past meeting. Not regenerated automatically.

### Retention

- Per owner, in the Meetings tab's settings: **recordings** kept 30 days
  (choices: 7, 30, 90, 365, forever), **transcripts, items and summaries** kept
  365 days (choices: 30, 90, 365, forever).
- Each meeting stores its own `purge_*_at` at end, so changing the setting
  later does not shorten meetings already kept (it applies from the next
  meeting; a **Apply to existing** button does it explicitly).
- An hourly sweeper deletes objects and rows past their time, and logs a
  count, never the content. A **lifecycle rule** on the bucket (the longest
  choice below "forever", plus a day) is the backstop if the sweeper dies;
  "forever" therefore keeps that rule off for the owner's prefix, and the
  settings page says so.
- **Delete now** on a meeting removes its rows and objects at once.

## Security and privacy

- **Consent.** Recording and the AI service are on by default, so the
  notice carries the weight: it is shown to a guest before they knock, with
  one clause per switch that is on, and stays on screen for everyone as a
  badge while either is on. Either switch can be turned off at any time;
  turning one back on is refused once **any guest has knocked** (waiting,
  admitted, denied or gone), so nobody is recorded or sent to the AI service
  who knocked while it was off, including a guest still in the lobby. Because
  a switch can only go off after the first knock, the notice a guest read can
  only have shrunk by the time they are admitted; the admitted page shows the
  **current** state from the meeting's event stream, never the state cached
  at knock time. The owner sees the same badge.
- **What leaves the network.** Audio and video never do: the SFU, TURN,
  Egress and STT are self-hosted in the tenant. **Transcript text does**, by
  default (decision 7), to OpenRouter (and through it to ScaleDown or
  DeepSeek's hosting provider), in 90-second windows and once whole for the
  summary. `data_collection: deny` restricts routing to providers that do not
  retain prompts. Per meeting, **AI action items** off sends nothing; server
  wide, `MEETINGS_ITEMS_MODEL=off` and `MEETINGS_SUMMARY_MODEL=off` do the
  same for every meeting. The plugin's README says this, as ecosystem.md's
  bar requires.
- **Guests see what the owner sees, live** (decision 6): the transcript and
  the action items, over their own event stream. They can change neither,
  and the archive (past meetings, recordings) is the owner's alone.
- **Guest isolation.** A guest's ticket opens only that meeting's wait and
  event streams. Guests reach no `/api/*` route, no past meeting, no
  recording, no other meeting's transcript. Their LiveKit grant has no admin
  and no data channel.
- **Link entropy and life.** 128-bit codes and tickets, stored hashed,
  compared in constant time; the link dies with the meeting and at 24 hours
  at most. A guessed or leaked link gets someone a knock, which the owner
  sees by name and can deny.
- **Owner sessions.** Bearer only, in page memory, 8 hours, bound to one
  owner. The plugin polls `GET /plugin/v1/self` every 60 s; a `401` deletes
  every session of that owner and ends their live meetings.
- **Token scopes.** The plugin's muster token: `app` + `tasks`. It cannot
  post messages to the chief, file drafts or write chief instructions.
  The LiveKit API secret, the bucket key and the OpenRouter key are the
  plugin's alone and never reach a browser.
- **Recordings** are served only to the owner, by presigned URLs valid 5
  minutes. The bucket is private.
- **The webhook** accepts only LiveKit-signed requests.
- **Logs** never carry transcript text, item text, guest names or codes.
- **Same-site with muster.** `meet.<muster host>` is a different origin but
  the same site. muster's cookies are host-only and `HttpOnly`, so the
  plugin's pages cannot read the owner's muster session. **Today they could
  use it**: many cookie-authenticated writes check no origin. After PR 1g
  ([the central origin check](#core-seam-a-central-origin-check-for-cookie-authenticated-writes))
  they cannot, and after PR 1f ([`__Host-` names](#core-seam-__host--cookie-names))
  they cannot plant one either. Both gate go-live.
- **Knock pushes** carry muster's text only, never a guest's name, are
  capped per meeting and per plugin, and open muster's own page.
- **muster's own surface widens in exactly three places,** all on
  `/apps/{id}` only: `frame-src` names one origin, `Permissions-Policy`
  delegates six features to it, and the frame's `allow` matches. Each is
  pinned by a test that fails if it widens or appears on any other page
  ([test plan](#muster-core-tests)).

## Failure modes

| what fails | what people see | what happens |
|---|---|---|
| STT engine down or overloaded | "Captions delayed" on the fallback, "Transcript paused" when no engine can take the audio | call and recording go on; gap markers for lost audio; the precedence is in [serving Nemotron](#serving-nemotron) |
| OpenRouter 429/5xx | "Action items paused" after 3 windows | backoff; the next window covers the gap; nothing is lost |
| Egress fails to start or dies | "Recording failed" badge for everyone | the meeting goes on; the recording row says failed; the owner sees it on the past meeting |
| Object store full or unreachable | as above | egress fails; transcripts are unaffected (SQLite) |
| TURN unreachable, UDP blocked | the guest sees "Can't connect media" with a retry | TCP and TURN/TLS are tried in turn before that |
| The plugin restarts mid-meeting | a short "Reconnecting" in the panels | the room lives in LiveKit and carries on; on start the plugin rebuilds open meetings from SQLite and `ListParticipants`, re-opens STT streams; speech in the gap is not transcribed (gap marker) |
| LiveKit down | **New meeting** says "Calls are unavailable" | no room can be created; existing calls drop |
| The owner leaves without ending | nothing, at first | the room's 10-minute empty timeout fires `room_finished`; the meeting ends; the link dies |
| The owner's muster token is revoked | the tab goes; an open page loses its frame within 30 s | the plugin, within 60 s, ends that owner's meetings and deletes their sessions |
| muster is down | the tab is unreachable | a live call goes on (it needs only the plugin and LiveKit); guests can still knock and the owner, already in, can admit |
| GPU host also serving dictation | dictation slower | the utterance engine is capped at 4 in flight; the Nemotron server is its own service with its own stream cap |
| Nemotron server full or down | "Captions delayed" for the participants over the cap | `503` or a failed connect sends that participant to `utterance`, retried after `max(30 s, Retry-After)`; a mid-stream drop reconnects 3 times first ([precedence](#serving-nemotron)) |
| Nemotron and the fallback both overloaded | "Transcript paused" | `utterance` runs 4 in flight, queues 30 s per participant, then drops the oldest with gap markers |

## Deployment

Generic; the deployment's names and addresses stay out of this repository.
**Everything runs in the muster tenant** (decision 8): the same Kubernetes
namespace as the muster deployment, or the same Compose project, on
subdomains of muster's own host.

**Kubernetes (muster's namespace):**

- `muster-meetings`: a Deployment, 1 replica, `strategy: Recreate` (one
  SQLite writer), a small PVC, its `mpl_` token, the LiveKit key and secret,
  the bucket key, the OpenRouter key, `MEETINGS_INGEST_SECRET` and (for the
  `utterance` engine) the STT bearer from Secrets of its own. muster's
  Secrets are not mounted: `stt-cohere` accepts one bearer, so its value is
  **copied** into the plugin's Secret as `MEETINGS_STT_TOKEN`, and rotating it
  means updating both. It calls muster at muster's in-namespace Service.
  An ingress rule for `meet.<muster host>`, beside muster's own rule for
  `<muster host>`; an internal port for the webhook, STT ingest and metrics.
- LiveKit server: the upstream Helm chart or a plain Deployment on the
  server host, TURN/TLS on `5349` with a publicly trusted certificate for
  `meet-turn.<muster host>`, `rtc.node_ip` the server host's own address
  (listed in `turn.allow_restricted_peer_cidrs`), `rtc.interfaces` pinned,
  `cpu_cost.track_cpu_cost: 0.1` on Egress. Signalling for
  `meet-rtc.<muster host>` through the ordinary HTTPS ingress. **No public
  UDP** in v1 ([media](#media-livekit-turn-egress)); the only public media
  path is the relay route below.

**Phase-2 deploy item: the `meet-turn` route (operator steps).** Each is
outward-facing or touches shared infrastructure, so each is the operator's,
in this order:

1. **Certificate:** issue a publicly trusted certificate for
   `meet-turn.<muster host>` (a DNS-01 challenge, since nothing serves
   port 80 for that name), mount it into LiveKit as `turn.cert_file` /
   `turn.key_file`, and set up its renewal.
2. **LiveKit config:** `turn.enabled: true`, `turn.domain:
   meet-turn.<muster host>`, `turn.tls_port: 5349`, `rtc.node_ip` and
   `turn.allow_restricted_peer_cidrs` as above, `rtc.interfaces` pinned. Roll
   it out; nothing public changes yet.
3. **The 443 route:** choose the public address and listener that will
   serve TCP 443 for `meet-turn.<muster host>` (an existing ingress
   controller's 443 with an SNI-passthrough rule, or a new listener on the
   relay's address if 443 is free there; check which first, since this was
   not measured). Add **one SNI passthrough** route there: SNI
   `meet-turn.<muster host>` → the server host's TURN/TLS port `5349` over
   the existing private path. TLS is **not** terminated on the way. Every
   other SNI on that 443 is unchanged. Use that component's own
   safe-change sequence (it carries other services).
4. **DNS:** an A record for `meet-turn.<muster host>` pointing at **the
   address that serves the 443 route**, **not proxied** by any CDN (a proxy
   carries neither TURN nor raw TLS passthrough).
5. **Firewall and ledger:** any **new** listener or forward this adds (for
   example a host-network listener on the relay's address) is internet-open
   unless the host firewall says otherwise. Record it in the host firewall's
   ledger of deliberately open ports, and confirm that nothing else was
   opened: no UDP port, no TURN relay port range, and no direct-media port
   (`7881`, `7882`). If the route reuses an existing 443 listener, record
   that instead.
6. **Verify (the S1 still owed):** a phone on a mobile network (Wi-Fi off)
   and a laptop with UDP egress blocked join one room; each client's
   `getStats()` shows a selected pair of type `relay` over `tls`; pass = both
   connect and the relay-over-TLS round-trip p95 is under 300 ms. This run
   also settles the UNVERIFIED local-allocation derivation in the media
   section. Record the numbers here.
- LiveKit Egress: a Deployment, sized for the number of concurrent
  recordings. Redis: a single small instance of its own (not muster's, if
  muster has one).
- The streaming STT: the NeMo Nemotron server (`images/stt-nemotron/`), bf16,
  with `STT_MAX_STREAMS` set from the phase-3b measurement, on **the GPU host
  that runs `stt-cohere`** (decision 18), reachable only from the plugin.
  Where a tenant has no GPU, the `utterance` engine against muster's existing
  STT service is the fallback.
- NetworkPolicies: the plugin may reach muster's Service, LiveKit's API, the
  STT service, the object store and the internet (OpenRouter) only; Egress may
  reach LiveKit, the object store and the plugin's ingest port; STT is
  reachable from the plugin alone; **muster's own pods gain no egress to any
  of these** (muster still calls nothing).

**Docker Compose (muster's stack):** the plugin repository ships a fragment,
`compose.meetings.yml`, used with muster's file
(`docker compose -f docker-compose.yml -f compose.meetings.yml --profile meetings up`),
so its services join muster's project and network: `muster-meetings`,
`livekit`, `egress`, `redis-meetings`, and optionally `stt-nemotron` (NVIDIA
runtime). muster's own file is unchanged. Without a GPU service it runs the
`utterance` engine against muster's `stt-cohere`. Published on the host:
HTTPS for `meet.` and `meet-rtc.` through the stack's reverse proxy, and
TURN/TLS for `meet-turn.` as an SNI passthrough on 443 (the same shape as
the Kubernetes route above). The UDP mux and TCP media ports are not
published, so LAN clients also connect through TURN/TLS, as in Kubernetes.

**muster's side:** add exactly `https://meet.<muster host>` to
`MUSTER_PLUGIN_ORIGINS`, `meetings` to `MUSTER_PLUGIN_TASK_NAMESPACES`, turn on
web push (for knock notifications), and mint the token on `/plugins` with
**App tab** and **Files tasks** checked.

## Phased plan

Sizes: **S** ≤ 1 day, **M** 2–3 days, **L** 4–6 days, for one engineer
with the test plan's tests included.

| phase | PR | repository | size |
|---|---|---|---|
| **0: spikes** | S1–S7: **done** ([results](#phase-0-results)); what they could not measure is carried into 2c, 3b, 3d, 4c and 1g's go-live check | — | done |
| **1: core** | 1g: **central origin check for cookie-authenticated writes** (R1): the check in `admit` (`Sec-Fetch-Site` first, on `net/http.CrossOriginProtection`, plus the no-header refusal), the GoTrue path, method-less patterns, the route ledger, the sibling-origin negative controls, `muster_csrf_refusals_total`. Pre-existing hardening; ship first. **Gates the subdomain's go-live** | muster | L |
| | 1f: **`__Host-` cookie names** (decision 16): the per-deployment name rule (on the resolved secure-cookie setting), the registrable-domain boot refusal, the loopback-http refusal on prefixed deployments, all seven literals, the host-only cookie golden, expiring the old names, e2e helpers, release note. **Gates the subdomain's go-live** | muster | M |
| | 1a: migration, `app` capability, `/plugins` form, label/icon validation, pairing refusal, docs in plugins.md and configuration.md | muster | M |
| | 1b: hand-off `app` target (with `ref`) and purpose, redeem for `app`, `/apps/{id}` page, frame, CSP and Permissions-Policy, sidebar entry, alive poll, Android "Open in browser" card, **and the CSP, Permissions-Policy and frame-attribute goldens** (the PR that widens the headers carries the tests that pin them) | muster | L |
| | 1c: `plugin-app.spec.ts`, routes.golden, cls.spec, nav ledger | muster | M |
| | 1d: the `tasks` capability (decision 11): the capability, the `/plugins` checkbox, pairing, the task routes' check, plugins.md | muster | S |
| | 1e: knock notification (decision 10): `POST`/`DELETE /plugin/v1/notify`, the `app` push event and its toggle, payload v2 `type: "app"`, limits, push.md, **and the new paired-device TLS push sink for e2e** ([knock push spec](#the-knock-push-spec)). **Needs 1a** (the `app` capability) **and 1b** (the `?ref=` page) | muster | L |
| | 1h: **server-installed app plugins** ([design](#core-seam-server-installed-app-plugins); forks 1–10 answered, every recommendation accepted, decision 20): migration 0172 (`plugins.scope`, NULL owner, `plugin_handoffs.server_plugin_id` and `signed_in_at`, `plugin_server_users` with the sign-in time, `plugin_server_tasks`, guarded against `db.RLSOn`), `/admin/apps` install, rotate and uninstall with step-up, the `muster-server app` CLI, the `serverPluginRoutes` door and its ledger, the owner-scoped redeem with pairwise subjects, `Muster-Act-For` and the one `serverGrantLive` predicate, `ViaServerPlugin` at every `ViaPlugin` branch site, per-person notify keys and the per-app ceiling, server tasks through the ledger with their caps, `POST /plugin/v1/owners/check`, the tab for every person with Hide on Account → Apps, no New tab for a server app and its Android card (`open-external`, with the copy-link fallback), the mint's cap and prune keyed on the minting person, `PersonalOwner()`, uninstall withdrawing knocks, plugins.md and auth.md, the isolation tests and `plugin-server-app.spec.ts`. **Needs 1a, 1b, 1d and 1e.** It may split into 1h.1 (store, door, redeem, act-for) and 1h.2 (admin page, CLI, tab, Account → Apps) | muster | L |
| | A1: **`open-external` bridge message** ([the Android path](#the-android-path-for-a-server-app)): the main frame's `{"type": "open-external", "url"}`, accepted only for muster's exact origin and `/apps/<digits>`, opened with `ACTION_VIEW` in the system browser and never in the WebView; the app advertises that it supports it; its unit tests. **A dependency of 1h's Android path**, not of 1h's merge: without it an app shows the copy-link card | muster-android | S |
| **2: plugin MVP (calls)** | 2a: repo from the template, `/enter` and owner sessions, `/self` poll, SQLite, meetings list, New meeting, link. **After 1h:** one server token in place of the comma-separated list ([what changes](#what-changes-in-the-plugins-phase-2a)), S | muster-meetings | M |
| | 2b: LiveKit rooms and tokens, guest page, lobby, admit/deny/remove, end, webhook, screen share, consent notice, knock notify calls, mobile layout | muster-meetings | L |
| | 2c: in-tenant Kubernetes manifests (muster's namespace), the Compose fragment for muster's stack, the subdomain/ingress, the TURN/TLS-only LiveKit config, Egress `cpu_cost`s, and the **`meet-turn` route** ([operator steps](#deployment)), then the owed real-network S1 run | muster-meetings + operator | M |
| | 2d: e2e click path without transcript or recording | muster-meetings | M |
| **3: transcription** | 3a: track Egress ingest (URL token, no pings, S16LE-stereo-48k parsing, sample-count timestamps, egress restart and start retry), `Engine` interface, `utterance` and `fake` engines, transcript panel for owner and guests, archive view and export | muster-meetings | M |
| | 3b: **`images/stt-nemotron/`** in muster (the NeMo cache-aware websocket server, bf16, batched decode, `STT_MAX_STREAMS`, `503`-before-upgrade, pinned image and CI build like `stt-cohere`) **and its measured fit beside `stt-cohere` on the 16 GB GPU** at 1/4/8 streams | muster | L |
| | 3c: the `nemotron` engine in the plugin (the protocol with padding and `flush`, the shared DC-filtered energy VAD, the fallback precedence and `Retry-After`, the boot check, the silent-start check), captions | muster-meetings | M |
| | 3d: `stt-eval` (AMI continuous headset leg, the private dictation leg, all three controls incl. the known-read passage) and its run on the `stt-cohere` GPU host | muster-meetings | M |
| **4: action items** | 4a: `items-eval`, the corpus, the controls | muster-meetings | L |
| | 4b: worker, guards, `extract` and `chat_json` adapters, live panel for owner and guests, the per-meeting **AI action items** switch | muster-meetings | M |
| | 4c: run the evaluation, set the default model, commit the result | muster-meetings | S |
| **5: recording** | 5: room composite Egress, record toggle (on by default) and badge, playback, retention settings (30 / 365 days) and sweeper, bucket lifecycle docs | muster-meetings | M |
| **6: extras** | 6a: Make task; 6b: summary; 6d: ecosystem.md row (6c, the Cohere post-pass, is dropped) | both | S each |
| **7: in-app calls on Android (optional)** | the `app-origins` bridge message and the camera grant | muster, muster-android | M |

Core first because every later PR stands on it, and because it is the only
part that changes muster for everyone. **Order and gates:**

- **1g and 1f ship first**, in either order, each on its own: both close
  gaps that exist today for any sibling host, and neither depends on
  meetings. They **gate go-live**: the plugin may be built and tested
  before them, but `meet.<muster host>` is not added to DNS or
  `MUSTER_PLUGIN_ORIGINS` on a live muster until a release carrying **both**
  is deployed there.
- **1a → 1b → 1c** in sequence; **1d** is independent of all of them; **1e
  needs 1a and 1b**; **1h needs 1a, 1b, 1d and 1e** (its forks are answered, decision 20), and its Android Open button needs **A1** in a released muster-android; before that the card is the copy-link fallback.
- On the plugin side, 2b's knock calls need 1e, and 6a needs 1d. **2a's move
  to one server token** ([what changes](#what-changes-in-the-plugins-phase-2a))
  needs 1h; until it lands, 2a runs on per-person tokens as it does today.

Core gains four PRs over the first revision: 1g (L) and 1f (M) for the
subdomain, 1d (S) and 1e (L, with its push-sink harness) for the 2026-11-20
decisions, about 11–16 engineer-days of core work beyond 1a–1c, plus 3b
(L, 4–6 days) in muster's repository for the Nemotron server image. No plugin PR changes size:
in-tenant hosting simplifies 2c's DNS and certificates, and the review
fixes (removal backstop, consent check, signed media URLs, ingest secret)
fit inside 2b, 3a and 5. Dropping 6c removes one S. Decision 19 (Nemotron) adds one PR: the Nemotron server image (3b, L, in muster, because no packaged websocket server ships with the model), so phase 3 is now M + L + M + M. The plugin's phases 2–5
can each ship alone: a call with no transcript is already useful.

### Phase 0 spikes

Each answers one **UNVERIFIED** claim above, with a go/no-go.

| # | question | go if |
|---|---|---|
| S1 | Do media and TURN work through the relay, from a phone on a mobile network and from a network that blocks UDP? | both connect; relay-over-TLS round-trip p95 under 300 ms (decision 17 leaves no public UDP path) |
| S2 | Does track Egress to a websocket deliver usable PCM (format, rate, gaps) for 30 minutes? | yes, or the Go-SDK participant fallback is chosen |
| S3 | Egress cost, measured beside its reservation: one 8-person, 1-hour meeting with room composite **and** 8 track egresses (and the same unrecorded, track egresses only); CPU, memory, and whether jobs are refused at the default `cpu_cost`s | the cluster fits one recorded and one unrecorded meeting at once, at default or justified lowered costs; otherwise S2's Go-SDK participant replaces track egress |
| S4 | Kyutai `stt-1b-en_fr` on the GPU host: memory beside `stt-cohere`, 8 concurrent streams, WER on the AMI subset | meets the [STT bar](#stt-evaluation) |
| S5 | In Chrome, Firefox and Safari: camera, microphone and `getDisplayMedia` inside the sandboxed cross-origin frame with the `allow` and `Permissions-Policy` above; Android WebView `getDisplayMedia` | all three desktop browsers work (Safari's screen share is the likeliest gap); the Android result decides whether phase 7 is worth it |
| S6 | One live request each: `scaledown/extract` with `provider.options.scaledown.entities`, and `deepseek/deepseek-v4.1-flash-1017` with a strict JSON schema | both answer in the documented shape |
| S7 | The headers a cookie-authenticated `POST` actually carries (`fetch`, htmx and a native form) from Android's WebView (the muster app), the installed PWA, and Safari with strict privacy settings | under 1g's rule each is admitted: `Sec-Fetch-Site: same-origin`, or, where that header is absent, muster's own `Origin`, or a same-origin `Referer`. Any client that falls through to "none of the three" is a no-go for 1g as written |

### Phase 0 results

All seven spikes have run, on throwaway infrastructure (nothing shipped and
nothing outward-facing changed), against this page as it stood at commit
`b834602f6`. Versions measured: LiveKit server 1.13.11, Egress on the LiveKit
Go SDK 2.18.2, `moshi-server` 0.6.4, Chromium 156, Firefox 157, WebKit 27.2.
The design above already carries each change; this table is the index.

| spike | verdict | what changed in this design | still owed |
|---|---|---|---|
| S1 media and TURN | works on a LAN for the UDP mux, TCP, TURN/UDP and TURN/TLS; a self-signed TURN certificate fails in Chromium; LiveKit always advertises `turns:…:443` | the public path is TURN/TLS on 443 only, by one SNI-passthrough route with a publicly trusted certificate (decision 17); local relay allocations derived, UNVERIFIED ([media](#media-livekit-turn-egress)) | the real-network run (phone on a mobile network, UDP-blocked laptop) once the route is live ([deployment](#deployment), step 6) |
| S2 track Egress PCM | **confirmed** for 31 min: 1920 B / 10 ms, S16LE stereo 48 kHz, identical channels | URL token (Egress sends no header), no pings, restart failed egresses, timestamp by sample count | mute/unmute and publisher reconnect (needs a browser publisher); Egress logged `audiodecoder … corrupted stream` about every 4 s per track with the test tool's looping sample while bytes kept flowing: re-check with a real browser publisher |
| S3 Egress cost | the reservation model was wrong: recorded 8-person ≈ 1.41 cores avg / 1.69 peak / 1.56 GiB; ≈ 0.02 core per track; cost held only 15 s | `track_cpu_cost` 0.1, `room_composite_cpu_cost` 2, a start retry, `MEETINGS_MAX_RECORDINGS` and the sizing rule (a second composite was refused at default cost on 6 CPUs); the Go-SDK fallback dropped as a cost argument | a 1-hour run with real browser publishers and screen share; `enable_template_sdk` (the GStreamer compositor) unmeasured |
| S4 streaming STT | **superseded** by the dictation benchmark behind decision 19 (99 recordings, 8 live streams): Nemotron 19.0% agreement WER, 2 empties, end-of-stream flush p95 0.02 s; faster-whisper turbo 20.4%; Kyutai 31.1% with 8 empties. S4's own Kyutai figures (3.6 GB at batch 8, hard stream cap) no longer apply | Nemotron on a NeMo websocket server on the `stt-cohere` GPU host (decisions 18, 19), stream cap with `503`, `utterance` fallback, faster-whisper turbo as the fallback model | bf16 memory fit beside `stt-cohere` (3b); AMI continuous-channel WER and the three controls (3d) |
| S5 frame media | the first revision's header was **refuted** in Chromium (no video, no screen share); with `self` it works; Firefox passes at the policy gate; Android WebView has no `getDisplayMedia` (documented) | `self` kept on every feature; e2e asserts tracks, not "no error" ([the frame](#the-frame-exactly)) | real Safari on macOS; actual capture in Firefox (headless has no screen source) |
| S6 Extract and DeepSeek | **confirmed** shapes under `data_collection: deny`; Extract `text` can be a rewrite; DeepSeek is $0.30 / $1.20 per M and served by ~50 providers | map by offset; provider pin; price corrected; a 10-window smoke (0.91 / 0.91 against 1.00 / 1.00) | the full [action-item evaluation](#action-item-evaluation), under the pinned provider |
| S7 cookie `POST` headers | **confirmed** on the three desktop engines: `Sec-Fetch-Site: same-origin` everywhere, native form `Origin: null`; a sibling `POST` carries the cookie | none needed: 1g's order already matches | the Android WebView, the installed PWA, Safari with tracking protections (a 1g go-live check) |

## Test plan

Per [testing.md](../testing.md): every guard below names the mutation that
must turn it **red**, and is watched red before it counts. A guard pinning an
invariant the code never violated is labelled an invariant guard.

### Muster core tests

Go, in `internal/plugins`, `internal/api` and `internal/ui`:

- **Capability at mint.** `app` without origin, entry URL, label or icon is
  refused; a label of 25 characters, with `<`, with two spaces, or with a
  bidi control is refused; an icon key not in the list is refused; a sixth
  `app` plugin is refused; pairing with `app` is refused. *Red if:* the
  check constraint or the validator is removed (each case its own mutation).
- **Purpose from the target.** A mint with `{"app": {}}` and a request body
  naming `"purpose": "view"` stores `app`. *Red if:* the purpose is read from
  the request.
- **Capability per target.** An `app` target for a `viewer`-only plugin and
  a `mirror` target for an `app`-only plugin are both refused; an `app`-only
  token can redeem its own `app` code and is refused another plugin's.
  *Red if:* the redeem route keeps admitting `CapViewer` only (the app
  plugin's redeem goes red), or admits any capability (the negative goes red).
- **Retired origin.** After the admin removes the pattern, the mint is `403`,
  the tab is not drawn and `/apps/{id}` is `404`.
- **The tab's gates.** A table over each gate closed alone (plugins off,
  origins unset, token revoked, owner disabled, origin retired, no `app`)
  draws no tab, with the all-open row drawing exactly one: the positive
  control that makes the zeros mean something.
- **`/apps/{id}` access.** Another owner's plugin id, a revoked plugin and an
  unknown id are each `404`, with identical bodies.
- **CSP, exactly.** `/apps/{id}`'s CSP is a pinned whole string with
  `frame-src https://meet.example.test` (the entry URL's origin, not the
  pattern). A **golden over every route's CSP** shows no other page changed.
  *Red if:* `frame-src` uses the pattern, adds `'self'`, or leaks to another
  page.
- **Permissions-Policy, exactly.** `/apps/{id}`'s header is a pinned whole
  string; every other route's is exactly `microphone=(self)` (a golden over
  every route). *Red if:* a feature is added, `self` is dropped from any
  feature (spike S5: `self` must be present), or the header leaks to another
  page.
- **The frame, exactly.** The rendered `sandbox` attribute equals
  `PluginChipSandbox` and the `allow` attribute is a pinned whole string with
  the exact origin. *Red if:* `allow-top-navigation` is added, or the origin
  becomes a wildcard.
- **421 still holds** for a `Host` under the meetings zone (an invariant
  guard: the existing test, extended with a new host).
- **Alive poll.** `GET /ui/apps/{id}/alive` is `204` for a live plugin and
  `404` after revoke.
- **Host-only cookies (decision 8's premise; lands in PR 1f).** A golden over every
  `Set-Cookie` muster-server emits across the sign-in, step-up, GoTrue,
  known-device and push-device paths: each has no `Domain` attribute and is
  `HttpOnly`. *Red if:* a `Domain` is added to any one of the seven
  literals, sets and clears (one mutation each). An invariant guard: no cookie violates it today.
- **`__Host-` names (PR 1f).** With `MUSTER_PUBLIC_URL` https, every
  `Set-Cookie` muster emits from the seven literals is named `__Host-…`, is
  `Secure`, has `Path=/` and no `Domain` (pinned whole strings). *Red if:* one
  literal keeps its old name, or gains a `Domain`. A request carrying only an
  old-named valid session is treated as signed out and its response expires
  the old name (red if the old name is still read).
- **The name is per deployment (Y1).** A table over configurations: https
  URL → prefixed; https URL with `MUSTER_SECURE_COOKIES=0` → unprefixed;
  plain-http and loopback URLs → unprefixed. For each, a sign-in `POST`
  (with a loopback `Origin`) and the following `GET` with **no** `Origin` and
  no `Referer` (what `no-referrer` makes a page load) use the **same** name,
  and the `GET` is signed in. *Red if:* the name is derived from
  `loopbackHTTPPage(r)` (the `GET` goes signed out: the loop the per-request
  rule would cause). The table includes the Compose case
  (`MUSTER_SECURE_COOKIES=1`, `http://localhost` URL → unprefixed), and keys
  on the resolved `secureCookies` value (red if the rule reads the literal
  variable). **Boot refusal:** unprefixed names with a plugin origin that is
  a subdomain of muster's host, and with one that is a **same-site sibling**
  (`https://meet.example.com` beside `https://app.example.com`), each refuse
  to start; one on another registrable domain starts (the positive control).
  *Red if:* the comparison is "under muster's host" (the sibling case starts).
  **Prefixed over loopback:** on an https deployment, a sign-in `POST` as a
  loopback plain-http request is answered with the "sign in at
  `<MUSTER_PUBLIC_URL>`" page and no `Set-Cookie`; the same `POST` over https
  signs in (the positive control). Playwright, in Chromium and
  WebKit, on the loopback http install (`http://localhost`): sign in, reload,
  open two more pages, still signed in. Playwright: a browser with an old-named session cookie lands on
  `/login`, signs in once and is signed in afterwards; a cookie
  `__Host-muster_session` set by a page on a sibling host with
  `Domain=<host>` is refused by Chromium and muster sees no session (the
  browser-side control that the prefix does what this design relies on).
- **The central origin check (PR 1g).**
  - **Route ledger:** every route registered on the mux with a method other
    than `GET`/`HEAD`/`OPTIONS`, enumerated from the mux, is classified
    `cookie`, `bearer-only` or `unauthenticated` (with a reason). The test
    fails on an unclassified route and when the set grows or shrinks.
  - **Negative control, every `cookie` route:** a valid session cookie with
    `Sec-Fetch-Site: same-site`; with `Sec-Fetch-Site: none`; with no
    `Sec-Fetch-Site` and `Origin: https://meet.example.test` (a sibling of the
    test host); with no `Sec-Fetch-Site` and `Origin: null`; and with no header
    at all, is each `403` **before the handler** (a spy handler records no
    call). Run for every `cookie` route, not a sample: today's
    unguarded `POST /tasks`, `/tasks/merge`, `/agents/{id}/start` and
    `/chief/provision` are named in it and are red at `5fd6f7632`.
  - **Positive control, every `cookie` route:** `Sec-Fetch-Site:
    same-origin` **with `Origin: null`** (muster's own native form under
    `no-referrer`) reaches the handler; so does no `Sec-Fetch-Site` with
    muster's own `Origin`, and neither with a same-origin `Referer`. *Red if:*
    the rule reads `Origin` before `Sec-Fetch-Site` (the form case goes red).
  - **Bearer-only routes:** each refuses a cookie-only request with a
    sibling `Origin` (it is not admitted by the cookie at all).
  - **Method-less patterns** (`/connect/…`, the LLM prefix) are in the
    ledger and get the `POST` cases.
  - **Bearer requests unaffected:** an `mst_` token with a sibling `Origin`
    reaches the handler; a request with both a cookie and a bearer is judged
    by the credential that admitted it.
  - **GoTrue mode:** the same three cases on the GoTrue session path.
  - **Both headers absent, cookie present:** refused.
  - The browser suite runs whole on the PR (every form and htmx write is a
    real-browser positive control).
- **`tasks` capability.** A `tasks`-only token files and comments on
  `POST /plugin/v1/tasks`, and is `403` on `POST /plugin/v1/messages`, the
  draft routes, chief instructions and the task-write queue; `route_to_chief`
  on it is `400`; a `messages`-only token still files tasks (backward
  compatibility). *Red if:* the task route keeps checking `messages` only
  (the `tasks` case goes red), or `tasks` is admitted by the messages route.
- **Knock notification.** With a fake push sender: `kind: "knock"` sends one
  push to the owner whose title is the tab label and whose body is muster's
  pinned text, for `count` 1 and 3; an extra `name` field or any other `kind`
  is `400`; a non-`app` token is `403`; a second send for the same ref within
  30 s and the 31st in an hour are `429`; the tag is `app-<id>-<ref>`; the tap
  URL is `/apps/<id>?ref=<ref>`; `DELETE` withdraws it; the toggle off answers
  `204` and sends nothing. *Red if:* the body is taken from the request, the
  rate cap is removed, or the capability check is removed (one each). A
  fixture tab label distinct from every word of the pinned body, so a
  mutant hardcoding the label cannot pass.
- **The `ref` hint.** `/apps/{id}?ref=abc` mints a code whose redeemed target
  is `{"app": {"ref": "abc"}}`; a `ref` with `/`, `..` or 65 characters is
  dropped, not passed. *Red if:* the pattern check is removed.
- **Ledgers.** The nav ledger lists the new document; `routes.golden` lists
  `GET /apps/{id}` and `GET /ui/apps/{id}/alive`.

Playwright, `e2e/tests/plugin-app.spec.ts`, with a **stub app plugin** (a
tiny server on a test origin under `MUSTER_PLUGIN_ORIGINS`, reusing the
previews specs' fixture in `e2e/lib/env.ts`) whose page redeems the code,
calls `getUserMedia({audio, video})` and `getDisplayMedia()`, and reports
each result to the test:

- the tab appears, is the last section link, and opens `/apps/{id}` full
  width at 390×844 and 1280×800;
- **positive:** inside the frame (Chromium with
  `--use-fake-device-for-media-stream --use-fake-ui-for-media-stream
  --auto-select-desktop-capture-source=…`) `getUserMedia({audio, video})`
  returns a stream with **one live audio and one live video track**, and
  `getDisplayMedia` **resolves** to a video track. "No error" is not the
  assertion: under the broken header of the first revision (S5) the call
  succeeded with the video track silently missing. `document.featurePolicy
  .allowsFeature()` is true for `camera`, `microphone` and `display-capture`
  in the frame; the redeem answered purpose `app`;
- **negative controls:** the same stub framed from a page without the `allow`
  attribute (a test-only route) is denied all three; one with
  `display-capture` dropped from `allow` gets `NotAllowedError` from
  `getDisplayMedia` only; and one whose header omits `self` (the first
  revision's `camera=("<O>")`) gets no video track. So the positive above is
  not the fake-UI flags granting everything, and a regression to the broken
  header goes red;
- revoking the plugin on `/plugins` in a second tab removes the frame within
  35 s and the stub reports its tracks ended;
- with `window.musterApp` injected, no frame is drawn, the **Open** card is,
  and its tap navigates top-level to a freshly minted URL;
- `cls.spec.ts` measures the page load.

### Plugin unit tests

In `muster-meetings`, Go, fakes from `internal/mustertest` (extended with
redeem, self and tasks) and a fake LiveKit server API:

- **Knock limits:** the 21st waiting knock on a meeting and the 11th knock
  from one address in 10 minutes are `429`; the 20th and 10th pass (the
  boundary's positive side). *Red if:* either cap is removed.
- **Join code:** 22 characters, base64url, distinct across 10,000 draws,
  stored only hashed. *Red if:* a shorter random source is used.
- **Lobby state machine:** a table of every (state, event) pair: waiting →
  admitted / denied / ended; admitted → removed / left / ended; nothing leaves
  `ended`; a removed ticket cannot wait again. *Red if:* any transition guard
  is removed (one mutation per row).
- **No token before admission:** the guest token mint is reachable only from
  the admit path; a `wait` on a waiting ticket never carries one. *Red if:*
  the admission check is dropped.
- **Grants, exactly:** the guest grant is a pinned structure with
  `roomAdmin=false`, `canPublishData=false`, `hidden=false`, the meeting's
  own room. *Red if:* any is flipped or the room comes from the request.
- **Ownership:** every repository query for owner A returns nothing of owner
  B's (a meeting, a transcript, a recording URL, an item). *Red if:* the
  repository's owner filter is removed.
- **Ended link:** `GET /j/{code}`, `knock` and `wait` answer `410` after end
  and after 24 hours (fake clock). *Red if:* the state check is removed.
- **Webhook:** a valid signature passes; a tampered body, a wrong secret and
  no header are `401`. *Red if:* the body hash is not compared.
- **Session revocation:** a `401` from the fake `/self` deletes the owner's
  sessions and ends their meetings within one poll. *Red if:* the poll's
  handler is a no-op.
- **Notice at knock (round 2):** a knock whose `notice` matches the current
  switches gets a ticket; one sent with `{"record": false}` after the owner
  turned recording on is `409` with the current state, and the guest page
  re-shows the notice. *Red if:* the knock ignores `notice`.
- **Reconcile:** with the webhook suppressed, an identity not admitted that
  appears in the fake `ListParticipants` is removed within one 15 s tick.
  *Red if:* the loop is removed.
- **Defaults and toggles:** a new meeting has `record` and `ai` on; either
  can be turned off at any time; turning either back on is refused while a
  guest is **waiting**, while one is **admitted**, and after one has left,
  and allowed before any knock (the positive control). *Red if:* a default
  flips, or the check looks only at admitted guests (the waiting case goes
  red), or it is removed (one each).
- **Notice is current:** a guest knocks with both on, the owner turns
  recording off, the guest is admitted: the admitted page's badge and the
  event stream say "Transcribing · AI", not "Recording". *Red if:* the page
  renders the state cached at knock time.
- **Removed guest cannot rejoin (Y2):** against the fake LiveKit API, a
  removal calls `RemoveParticipant`; a `participant_joined` webhook for a
  revoked, denied or unknown identity calls `RemoveParticipant` at once, and
  for an admitted one does not (the positive control); guest token `exp` is
  2 minutes after mint. *Red if:* the webhook check is removed, or the
  removal skips the LiveKit call (one each). The integration test below does
  it against real LiveKit.
- **Signed media URLs (Y4):** `/media/…` with a valid signature streams with
  `Range` (a `206` for `bytes=0-99`); an expired, tampered, or other
  meeting's signature is `403`; the URL is minted only for the owner's own
  meeting. *Red if:* the expiry or the path is left out of the HMAC.
- **Ingest token:** an upgrade to `/stt/ingest/<track>/<token>` with a
  wrong, expired or other track's token is `401`, and with the right one is
  accepted (no header is ever required, since Egress sends none). The access
  log line for it contains `/stt/ingest/<track>` and not the token. *Red
  if:* the check is removed, or the token is logged.
- **The ingest never pings:** an Egress-like client that only writes, never
  reads, stays connected for 120 s (a fake clock may shorten it). *Red if:*
  the server's ping/pong timeout is left at a library default (the stream
  dies at its timeout, as S2's did at 40 s).
- **Real bytes:** a recorded fixture of Egress' own frames (1920 B, S16LE
  stereo 48 kHz, identical channels) parses to the expected mono samples and
  resamples to 16 kHz with the expected length, sent as 80 ms (1280-sample)
  frames. A fixture whose
  channels differ is still read from the first channel only.
- **Timestamps by sample count:** a fixture delivered with a 7 s stall and
  then a burst yields segment times equal to the same fixture delivered
  evenly. *Red if:* times come from arrival.
- **Egress restart and start retry:** an `egress_failed` webhook for an STT
  track starts a new track egress and stores a gap marker that clears when
  audio flows; a refused start is retried with backoff until accepted. *Red
  if:* either handler is a no-op.
- **STT at capacity:** a fake Nemotron server that answers the ninth
  upgrade `503` sends that participant to the `utterance` engine and shows
  "Captions delayed"; the first eight stay on `nemotron`; when a slot frees
  the participant returns within one 30 s retry. *Red if:* the fallback is
  removed (the ninth participant gets no transcript).
- **The Nemotron protocol:** against a fake server speaking the protocol,
  partials and finals become segments with sample-count times; an `end` text
  frame with the true `samples` is sent when the track ends, after a last
  frame zero-padded to 1280 samples, and the final after it is stored with
  its end time at the true sample count, not the padded one. *Red if:* times
  come from arrival, or the padding is counted.
- **Pause finalisation:** a fixture of speech, 1 s of silence, speech (and
  the same with a DC offset added) makes the plugin send one `flush` about
  700 ms into the silence, and the first utterance's `final` is stored
  before the second begins. *Red if:* the VAD's flush is removed (the first
  final arrives only at `end`), or DC is not removed (the offset variant
  never flushes).
- **Fallback precedence:** a `503` at open goes to `utterance` at once, and
  the retry waits `max(30 s, Retry-After)` (a `Retry-After: 60` waits 60 s);
  a drop after the upgrade reconnects up to 3 times before falling back,
  storing a gap marker; with Nemotron down for every participant and a
  `stt-cohere` fake that answers slowly, at most 4 transcriptions are in
  flight, a participant's queue holds 30 s, and overflow drops the oldest
  with a gap marker and "Transcript paused". *Red if:* each rule is removed
  (one mutation each).
- **Boot:** `nemotron` without `MEETINGS_STT_URL`/`MEETINGS_STT_TOKEN`
  refuses to start.
- **The Nemotron server itself** (in muster's `images/stt-nemotron/`, like
  `stt-cohere`'s `test_app.py`): a missing or wrong bearer is `401`; the
  `STT_MAX_STREAMS + 1`th upgrade is `503` with `Retry-After` and is never
  upgraded; a frame of any size but 2560 bytes gets the `bad_frame` error
  frame and a 1003 close; a `flush` yields a `final` for the text since the
  previous one; a constant-signal stream yields no text; a `.nemo` whose
  `preprocessor.sample_rate` is not 16000 refuses to start.
- **Silent start:** a track with speech energy and no words for 20 s raises
  the flag; a silent track does not (the positive and negative cases).
- **No ticket in URLs or logs:** every route that takes a ticket refuses it
  in the query string, and a captured access log of a full meeting contains
  no ticket or join code.
- **AI off sends nothing:** with **AI action items** off, the fake
  OpenRouter receives zero requests over a whole scripted meeting, while the
  same meeting with it on sends at least one (the positive control), and the
  summary is not requested either. *Red if:* the worker ignores the switch.
- **No audio to OpenRouter:** every request body the fake OpenRouter receives
  is JSON text with no `audio`/`input_audio` part and no base64 longer than
  200 characters. An invariant guard.
- **Guest event stream:** an admitted guest's stream carries transcript
  segments and action items; a waiting, denied or removed ticket's carries
  neither; no guest route accepts Make task or Dismiss. *Red if:* the
  admission check on the stream is removed.
- **Knock push timing:** the plugin calls `notify` on a knock only when no
  owner session holds that meeting's event stream, and calls `DELETE` when
  the lobby empties. *Red if:* the presence check is removed.
- **Action-item guards:** out-of-window citations dropped, duplicates by text
  and by citation dropped, more than 5 per window cut, no call when there is
  no new segment. *Red if:* each guard is removed (one mutation each). The
  Jaccard threshold is tested at 0.69 and 0.71 with fixtures whose texts are
  pairwise distinct, so a hardcoded result cannot pass both.
- **ScaleDown adapter:** spans map to the right segment ids by offset,
  including a span crossing two segments, and an offset past the end is
  dropped. A fixture whose `text` differs from `input[start:end]` (the
  rewrite S6 saw) shows `input[start:end]`. *Red if:* the adapter uses
  `text`. The request body is the pinned S6 shape (entities as a record);
  a list is never sent.
- **Retention sweeper:** with a fake clock and a fake store, objects past
  `purge_recording_at` are deleted, objects one second short are not, and a
  "forever" meeting is never touched. *Red if:* the comparison's direction
  flips.
- **Logs:** a test meeting's transcript text, guest name and code appear in
  no log line (a capture of every log line of the run). Positive control: a
  log call deliberately given the text is caught.
- **Make task:** the request body is the pinned JSON for a fixture item; a
  `503` from the fake shows the not-set-up message and does not retry.

### Plugin integration tests

In CI, with **real** LiveKit, Redis, Egress and MinIO in containers
(`livekit/livekit-server --dev`, `livekit/egress`, `redis`, `minio`), and the
`fake` STT engine and a fake OpenRouter (a local HTTP server answering
scripted completions):

- **Media path without a browser:** two participants join with LiveKit's CLI
  (`lk room join --publish <fixture.ogg>`), so a real track reaches the real
  SFU; the plugin starts a track Egress for each, PCM arrives at the ingest,
  the fake engine emits its script, segments are stored and appear on the
  SSE stream with the right speakers.
- **Two recordings on a 6-CPU Egress:** with Egress limited to 6 CPUs and
  the deployed costs (`room_composite_cpu_cost` 2, `track_cpu_cost` 0.1),
  two recorded 8-person meetings started 60 s apart both record. *Red if:*
  the composite cost is left at its default 4 (the second is refused, as S3
  measured). A third meeting over `MEETINGS_MAX_RECORDINGS` shows
  "Recording unavailable" and its notice drops "recorded".
- **Recording:** with **Record** on, room composite Egress writes an object;
  its size is non-zero and `ffprobe` reads a duration within 10% of the
  meeting's.
- **Removed guest cannot rejoin, against real LiveKit:** a guest is admitted
  and connected, the owner removes them, and the guest client reconnects with
  the token it still holds: it is disconnected within 2 s of
  `participant_joined`, and an admitted control guest who reconnects is not.
- **Lifecycle:** `room_finished` after the empty timeout (shortened to 10 s
  in the test config) ends the meeting and kills the link.
- **Restart:** killing and restarting the plugin mid-meeting resumes
  segments from the same tracks and stores a gap marker.
- **Controls:** a participant publishing silence produces no segment while a
  second, speaking participant in the same room does (so "no segment" is not
  an ingest that never ran).

The real STT engine and the real models are **not** in CI (no GPU, no paid
key). That is what the two evaluation harnesses are for, and the
[live leg](#the-live-leg) below.

### End-to-end: the click path

Playwright in `muster-meetings`, against a muster release image with a
seeded database, the plugin, and the integration containers above. Two
browser contexts: **owner** (signed in to muster) and **guest** (fresh
context, no cookies). Chromium flags: `--use-fake-device-for-media-stream`,
`--use-fake-ui-for-media-stream`, `--use-file-for-fake-audio-capture=<wav>`
(the guest's microphone plays a fixture: a synthesised voice saying "I will
send the budget to Sam by Friday"), and
`--auto-select-desktop-capture-source=<title>` for screen share (headless
support **UNVERIFIED**; if absent this step runs headed under Xvfb).

1. Owner opens muster, taps **Meetings**: the tab's page loads in the frame.
2. **New meeting**; the link is shown; **Copy link** puts it on the
   clipboard (read back with the clipboard permission granted to the test).
3. Guest opens the link: the consent notice is visible before the name field;
   types "Ada"; **Ask to join**; sees "Waiting".
4. Owner sees "Ada (guest) wants to join"; **Admit**.
5. Both see two participants; each side's remote `<video>` has
   `videoWidth > 0` and the remote audio track is live.
6. Owner taps **Share screen**: the guest sees a track with source
   `screen_share`.
7. The guest's fixture audio produces the transcript line under "Ada" in
   both panels (fake engine in the default leg).
8. An action item "send the budget to Sam" appears in the owner's panel
   **and the guest's** within 15 s of the line (fake OpenRouter in the
   default leg); the guest's has no Make task button.
9. (The knock push is tested in its own spec, [below](#the-knock-push-spec).)
   **Make task**: muster's API shows a task tagged
   `meetings:<meeting>-<item>`; a second tap files nothing new.
10. **End meeting**: the guest sees "This meeting has ended"; reloading the
    guest link answers `410`.
11. The past meeting is listed with Ada, its transcript (the line, under
    "Ada"), the action item marked "Task #n", and a summary. **Play** asks
    `POST /api/meetings/{id}/recording-url`, the `<video>` gets that
    `/media/…` URL and plays to a non-zero `currentTime`; a seek to the
    middle issues a `Range` request; **Download** fetches the same URL. The
    page reports no CSP violation (a `securitypolicyviolation` listener).

#### The knock push spec

The click path cannot test the push: there the owner holds the meeting's
event stream, and the plugin sends no push while it does. A separate spec:

1. The owner creates a meeting, then leaves its page (the Meetings list or
   another muster page), so no owner session holds the stream.
2. A guest knocks.
3. The push arrives at a **test push sink** and decrypts to muster's text
   ("Someone is waiting to join your meeting", the tab label as title) with
   no guest name; a second knock within 30 s sends none; the owner opening
   the meeting withdraws it.

**This is new harness work** (in muster's e2e, size M, part of 1e). muster
accepts a browser subscription only on the public push services, and
`MUSTER_PUSH_EXTRA_HOSTS` admits an extra host **for paired phones only**,
on `https` and the **default port** ([push.md](../push.md#which-push-services)).
So the sink is a **paired device**: the spec pairs a test device through
the device routes, registers a UnifiedPush-style endpoint on a TLS sink
listening on 443 under a test host name (a container with a test CA that
muster trusts through `SSL_CERT_FILE`), holds the device's key pair, and
decrypts payload v2. Nothing in muster's e2e does this today:
`push.spec.ts` stubs `pushManager` and has no receiver.

*As built (1e):* `e2e/pushsink` mints the test CA at start and serves the
endpoint `https://push.example.test/up/<topic>` over TLS. Rather than a
container bound to 443 and an `/etc/hosts` entry (both need root), muster is
started with `HTTPS_PROXY` at the sink's own CONNECT proxy, which accepts only
`push.example.test:443`; the TLS handshake (SNI, chain via `SSL_CERT_FILE`)
runs end to end inside the tunnel. It runs in the ordinary e2e leg with no
extra CI step.

**Defaults:** the guest's notice at step 3 names recording, transcription
and the AI service, because all three are on by default; a second meeting
with both switches off shows only "transcribed", its fake OpenRouter receives
nothing, and its past meeting has no recording and no summary.

**Negative paths, same harness:** a second guest is denied and sees "You
weren't admitted"; a guest calling the owner's `/api/meetings/{id}/token` with
their ticket gets `401`; the guest's context cannot open the past meeting's
transcript or recording URL; the owner's frame loses media within 35 s of the
token being revoked; at 390×844 the call, the controls and the lobby row are
all on screen without scrolling.

#### The live leg

The same spec, run by hand or nightly with `MEETINGS_E2E_LIVE=1`, against the
real STT engine on the GPU host and the chosen OpenRouter model: steps 7 and
8 then assert on the real outputs (the line contains "budget" and "Friday";
an item cites that line). It costs a few cents and needs the key, so it is
not a merge gate; its last result is shown in the plugin README.

### Mutation and negative controls for the security gates

The gates that keep a stranger out, each with the mutation that must turn a
named test red, run as a scripted sweep in the plugin's CI (a mutant per
gate, applied, tested, reverted; one known-caught mutant in every batch as the
positive control):

| gate | mutation | must turn red |
|---|---|---|
| lobby | mint a guest token on knock | "no token before admission" |
| guest grant | `roomAdmin=true` | "grants, exactly" |
| ownership | drop the owner filter | "ownership" and the e2e negative |
| ended link | skip the state check | "ended link" |
| webhook | skip the body hash | "webhook" |
| revocation | ignore `/self` 401 | "session revocation" |
| core frame | add `allow-top-navigation`, widen `allow` to `*` | "the frame, exactly" |
| core CSP / Permissions-Policy | emit on every page | the two goldens |
| core purpose | read purpose from the request | "purpose from the target" |
| core cookies | add `Domain` to the session cookie | "host-only cookies" and "`__Host-` names" |
| core cookie names | read the old name on https | "`__Host-` names" |
| core `tasks` | admit `tasks` on `POST /plugin/v1/messages` | "`tasks` capability" |
| core knock | take the push body from the request | "knock notification" |
| core knock | drop the `app` capability check; drop the 429 cap | "knock notification" (one each) |
| core `ref` | drop the `ref` pattern check | "the `ref` hint" |
| core redeem | admit only `CapViewer` on redeem; admit any capability | "capability per target" (one each) |
| core tab | draw the tab with each gate removed in turn | "the tab's gates" (one per gate) |
| core `/apps` | skip the owner check on `/apps/{id}` | "`/apps/{id}` access" |
| core alive poll | never remove the frame on `404` | `plugin-app.spec.ts` revoke case |
| core `tasks` | admit `tasks` on the draft routes, on chief instructions | "`tasks` capability" (one each) |
| core CSRF (1g) | remove the middleware; exempt one route class; accept `Origin: null`; accept both-absent | "the central origin check" (one each) |
| core CSRF ledger | register a new `POST` route without classifying it | "route ledger" |
| core cookie name | derive the name from the request (`loopbackHTTPPage`) | "the name is per deployment" |
| guest stream | serve the event stream to a waiting ticket | "guest event stream" |
| join code | shorten the code to 64 bits | "join code" |
| knock limits | remove the per-meeting or per-address knock cap | a knock-limit case (one each) |
| rejoin | remove the `participant_joined` check | "removed guest cannot rejoin" and the integration case |
| consent | check only admitted guests | "defaults and toggles" (waiting case) |
| media URL | leave the expiry out of the HMAC; leave the path out | "signed media URLs" (one each) |
| recording URL | mint `recording-url` for another owner's meeting | "signed media URLs" and "ownership" |
| notice is current | render the notice cached at knock time | "notice is current" |
| notice at knock | ignore the knock's `notice` | "notice at knock" |
| ticket in query | accept `?ticket=` | "no ticket in URLs or logs" |
| boot refusal | compare "under muster's host" instead of registrable domain; drop the refusal | "the name is per deployment" boot cases (one each) |
| cookie name rule | read the literal `MUSTER_SECURE_COOKIES` instead of the resolved value; prefix over loopback http | "the name is per deployment" (one each) |
| core CSRF order | read `Origin` before `Sec-Fetch-Site`; admit `Sec-Fetch-Site: none` | "the central origin check" positive form case; negative `none` case |
| core CSRF ledger | label a `cookie` route `bearer-only`; skip method-less patterns | "route ledger" bearer-only and method-less cases |
| reconcile | remove the `ListParticipants` loop | "reconcile" |
| ingest | remove the token check; log the full ingest URL | "ingest token" (one each) |
| ingest pings | enable the library's default ping/pong timeout | "the ingest never pings" |
| ingest time | timestamp by arrival | "timestamps by sample count" |
| egress restart | ignore `egress_failed` for STT tracks; drop the start retry | "egress restart and start retry" (one each) |
| STT capacity | drop the `utterance` fallback | "STT at capacity" |
| STT server cap | accept the upgrade over `STT_MAX_STREAMS` and close it later | "the Nemotron server itself" |
| STT server auth | drop the bearer check | "the Nemotron server itself" (401 case) |
| STT server frames | accept a short frame | "the Nemotron server itself" (`bad_frame` case) |
| STT return | never retry Nemotron after a fallback; ignore `Retry-After` | "STT at capacity" (30 s return), "fallback precedence" (one each) |
| STT endpointing | drop the VAD `flush`; skip DC removal | "pause finalisation" (one each) |
| STT padding | count the zero padding in the last timestamp | "the Nemotron protocol" |
| STT protocol time | timestamp segments by arrival | "the Nemotron protocol" |
| extract text | display `text` instead of `input[start:end]` | "ScaleDown adapter" |
| egress costs | leave `room_composite_cpu_cost` at 4; drop `MEETINGS_MAX_RECORDINGS` | "two recordings on a 6-CPU Egress" (one each) |
| frame header | drop `self` from `camera` or `display-capture` | `plugin-app.spec.ts` positive (video track, resolved `getDisplayMedia`) and the Permissions-Policy golden |
| AI switch | ignore it in the worker | "AI off sends nothing" and the e2e defaults case |

## Open questions

1. **DeepSeek `:batch`?** `deepseek/deepseek-v4.1-flash-1017:batch` costs
   $0.036 / M in and $0.15 / M out, against $0.30 / $1.20 at peak for the
   default variant (S6). Its latency was not tested, and action items run on
   10 s windows. Worth measuring it in the action-item evaluation as a third
   candidate, or keep the default variant only?
2. ~~**Server-installed app plugins (core PR 1h):** ten forks.~~ Answered:
   every recommendation in [1h forks for the operator](#1h-forks-for-the-operator)
   accepted (decision 20).

## Decisions

Decisions 1–5 are from the operator's brief. Decisions 17–18 followed the
phase-0 spikes, and 19 the dictation benchmark that superseded S4. Decision 20 is the operator's acceptance of every 1h recommendation. Decisions 6–16 are the operator's answers
to the open questions, given **2026-11-20**. All are binding on this
design.

1. **The tab is a new core capability, `app`,** generic, entered through the
   viewer hand-off, with a frame allowed camera, microphone and screen
   capture.
2. **Media is self-hosted LiveKit,** with Egress recording to S3-compatible
   storage, screen share, and TURN exposed as one public UDP port and TLS
   TURN.
3. **Transcription uses the existing STT service if it streams.** It does
   not; this page recommends a streaming model and keeps the existing service
   for utterance mode. Transcription is per track, not
   diarized.
4. **Action items use OpenRouter:** ScaleDown Extract first, a DeepSeek model
   as the fallback, decided by an evaluation, switchable by config, in a
   rolling incremental window shown live.
5. **Guests join by link with a typed name, into a lobby,** admitted by the
   owner with one click; the link dies when the meeting ends; no guest
   accounts.
6. **(2026-11-20, operator) Guests see the live transcript and the action
   items.** Everyone in the call sees both. The archive stays the owner's.
7. **(2026-11-20, operator) Transcript text, never audio, goes to
   OpenRouter** for action items and the summary. This is on by default, with a
   per-meeting switch to turn it off.
8. **(2026-11-20, operator) Hosting is in the muster tenant,** beside the muster
   deployment (same namespace or Compose stack). It runs on a subdomain of the
   hosted muster's own host (`meet.<muster host>`), added to
   `MUSTER_PLUGIN_ORIGINS`. That is a different origin from muster, never the
   same one (muster's `421`, and the `allow-same-origin` frame). The earlier
   recommendation of a separate domain is dropped. The cookie check found
   every muster cookie host-only. Review then found that a same-site
   subdomain can still forge cookie-authenticated writes muster does not
   origin-check; core PR 1g closes that before go-live
   ([origins](#origins-and-hosts)).
9. **(2026-11-20, operator) Recording is on by default,** with a per-meeting
   switch to turn it off and a visible notice to every participant while it is
   on. Retention defaults are 30 days for recordings and 365 days for
   transcripts.
10. **(2026-11-20, operator) A core "someone is knocking" notification**
    uses muster's existing push path
    ([design](#core-seam-knock-notification)).
11. **(2026-11-20, operator) A narrow `tasks` plugin capability** serves Make
    task instead of `messages` ([design](#core-seam-the-tasks-capability)).
12. **(2026-11-20, operator) The owner session lasts 8 hours.** Revocation reaches an open
    page through the 30 s alive poll and the 60 s `/self` poll.
13. **(2026-11-20, operator) The 8-participant cap stands** (see 9).
14. **(2026-11-20, operator) No team-owned meeting tabs in v1;** they are
    later work.
15. **(2026-11-20, operator) v1 is English only.**
16. **(2026-11-20, operator) muster's cookies are renamed with the
    `__Host-` prefix before meetings ships.** It is one small core PR (1f)
    with its tests, and everyone signs in again once. It must be released and
    deployed before `meet.<muster host>` goes live
    ([design](#core-seam-__host--cookie-names)).
17. **(operator, after the phase-0 spikes) The public media path is TURN over
    TLS on 443 only,** through one SNI-passthrough route for
    `meet-turn.<muster host>`, with a publicly trusted certificate. No UDP is
    opened on the public node in v1 ([media](#media-livekit-turn-egress),
    [operator steps](#deployment)).
18. **(operator, after the phase-0 spikes) The streaming STT runs on the GPU
    host that already runs `stt-cohere`,** beside it
    ([choice](#choosing-the-streaming-stt-model)).
19. **(operator) The streaming STT model is NVIDIA Nemotron Speech Streaming
    en 0.6B,** not Kyutai; the Kyutai deployment is being retired. Chosen on
    the dictation benchmark in [choosing the model](#choosing-the-streaming-stt-model).
    faster-whisper large-v3-turbo is the named fallback model.
20. **(operator) Core PR 1h, server-installed app plugins: every
    recommendation in [1h forks for the operator](#1h-forks-for-the-operator)
    is accepted,** forks 1–10. That is: a `plugins` row with
    `scope = 'server'` and no owner; a pairwise subject and the display
    name; an 8-hour grant cut by the minting sign-in's cutoffs; Hide on
    Account → Apps; a fresh step-up for install and rotate; the admin page
    and CLI, with no bearer API; Rotate keeps subjects and uninstall is
    final; one server token for meetings; a personal plugin at a server
    app's entry URL refused; and no top-level entry, with `open-external`
    on Android.
