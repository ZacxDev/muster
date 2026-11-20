# Design: meetings — voice, video, screen share, live transcript and action items

**Status: proposed. Nothing here is built.** The operator's decisions are
recorded under [decisions](#decisions). Everything else is the recommended
design. The operator answered the first round of open questions on
2026-11-20 (recorded as decisions 6–16); what is still open is under
[open questions](#open-questions). Where
this page says how muster works today, it cites `file:line` at commit
`5fd6f7632`, and the code is the authority ([decisions.md](../decisions.md)).
A claim nothing confirmed is marked **UNVERIFIED**, and each one has a
[phase 0 spike](#phase-0-spikes) that settles it.

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
  Recommended: **Kyutai `stt-1b-en_fr`** on its own websocket server on the GPU
  host for live captions, with an **utterance mode** on the existing service
  as the zero-new-service fallback and the post-meeting accuracy pass.
- **Action items** come from an LLM on OpenRouter over a rolling window of
  the transcript, every 10 s, emitting only items it has not emitted before.
  The candidate is **`scaledown/extract`**, with **`deepseek/deepseek-v4-flash`**
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
  a path to French (Kyutai) and 14 languages (Cohere) for later.
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
| CSRF check | `internal/api/auth.go:317-331` (`requireSameOrigin`), `auth.go:363` (`isSameOrigin`) | state-changing requests must carry muster's own **origin** (full host), because a sibling subdomain is same-site |
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
mode and the post-meeting pass.

## User flow

### The owner

1. Opens muster and taps **Meetings** in the sidebar. The page fills the
   content area: a **New meeting** button and the list of past meetings,
   newest first.
2. Taps **New meeting**. The meeting is created and the owner is in it: their
   camera preview, a mute and camera toggle, and the link with a **Copy link**
   button. Two switches, both **on** by default: **Record**, and **AI action
   items** ("sends the transcript text, never audio, to an AI service"). Either
   can be turned off at any time; turned off, it cannot be turned back on once
   a guest has been admitted, so nobody is recorded or sent to the AI service
   who knocked while it was off.
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
| Streaming STT | a GPU service on the GPU host | speech to text, one stream per participant |
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

Decision 16. **Must land before `meet.<muster host>` is live.** It is not
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
- **The loopback exception.** On a page muster serves itself over plain http
  on loopback, `Secure` is dropped (`passwordCookieSecure`,
  `internal/api/login.go:237`, for WebKit). A `__Host-` cookie without `Secure` is refused,
  so there the **old name** is kept. The name is chosen by the same predicate
  that chooses `Secure`, in one function (`cookieName(base, secure)`), and the
  reader accepts only the name for the request's mode. An https deployment
  never reads an unprefixed name.
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
- **e2e helpers** that read or set cookies by name (`e2e/lib/signin.ts`) move
  to the new names in the same PR.

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
  Permissions-Policy: microphone=(self "<O>"), camera=("<O>"), display-capture=("<O>"),
                      fullscreen=(self "<O>"), autoplay=(self "<O>"), clipboard-write=(self "<O>")
  ```

  `microphone` keeps `self` so muster's own dictation code path is not
  changed by the page existing. `camera` and `display-capture` do not include
  `self`: muster's own page has no use for them. Every other response keeps
  exactly `microphone=(self)`.
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
its camera and microphone, and Android's WebView has no screen capture
(**UNVERIFIED**: [spike S5](#phase-0-spikes)).

- **v1: open in the browser.** Where `window.musterApp` exists, `/apps/{id}`
  draws no frame. It draws a card, "Meetings opens in your browser", and one
  button, **Open**. The tap mints a hand-off (the same `POST`, at tap time)
  and navigates to the minted URL top-level; the app sends that origin to a
  Custom Tab, where the browser grants camera and microphone the ordinary way
  and the plugin can use a first-party cookie. The code is single use and
  lives 60 s, so the hand-off in a Custom Tab is no wider than **New tab** in
  a browser. No app change is needed for v1.
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
  requests) and pass `requireSameOrigin`. The sandbox protects muster only
  because the frame's origin differs.

**The cookie check: no blocker.** Every cookie muster sets is **host-only**:
none sets a `Domain` attribute, so the browser never sends one to
`meet.<muster host>`, and all are `HttpOnly`, so no script reads them on
muster's own origin either. The places:

| cookie | set at | attributes |
|---|---|---|
| password-mode session (`muster_session`) | `internal/api/login.go:203-216`, `login.go:279-286` | `Path=/`, `HttpOnly`, `SameSite=Lax`, `Secure` (except loopback http), no `Domain` |
| GoTrue access and refresh | `internal/api/accounts.go:879-883` (cleared `:889-892`) | `Path=/`, `HttpOnly`, `SameSite=Lax`, `Secure`, no `Domain` |
| known device | `internal/api/known_device.go:61-68` | the same, no `Domain` |
| push device | `internal/api/push_routes.go:409-414` | the same, no `Domain` |

(Searched `internal/` and `cmd/`, non-test code, at `5fd6f7632`: those five
`http.Cookie` literals are every cookie muster-server sets, none has a
`Domain` field, and no code writes a raw `Set-Cookie` header. The only other
cookie code is the preview gateway's, `internal/previewgw/cookie.go`, a
separate process on its own host. The finding is pinned by a test,
[below](#muster-core-tests), so a later `Domain` turns it red.)

**What host-only does not cover, and why it holds anyway:**

- **Same-site.** `meet.<muster host>` is **same-site** with muster (site is
  the registrable domain). `SameSite=Lax` therefore does not stop the
  browser attaching muster's cookies to a request the meetings page makes to
  muster. What stops it acting is `requireSameOrigin`
  (`internal/api/auth.go:317-331`), which compares the full-host **origin**
  and refuses every state-changing request from another host; its own comment
  names exactly this sibling-subdomain case. A `GET` passes and changes
  nothing. The meetings page's own CSP (`connect-src 'self' …`) does not
  list muster at all, and the plugin calls muster server-to-server only.
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
previews do; one owner is the common case.

### Guests: links, the lobby, admission

- **The link** is `https://meet.<muster host>/j/<code>`, `<code>` 128 random bits
  (22 base64url characters), stored as SHA-256. It is a right to **knock**,
  not to join. It dies when the meeting ends (`410`), and in any case
  24 hours after the meeting was created.
- **Knock:** `POST /j/<code>/knock {"name"}` → `{"ticket"}`, a second random
  128-bit value held in the guest page's memory. The name is trimmed, 1–40
  characters, control and bidi characters refused; it is shown to the owner
  as text, with "(guest)" after it, never as HTML.
- **Wait:** `GET /j/<code>/wait?ticket=…` is an SSE stream that ends with
  `admitted {"livekit_url", "token"}`, `denied`, or `ended`.
- **Admit:** the owner's `POST /api/meetings/{id}/lobby/{pid}/admit` mints the
  guest's LiveKit token and sends it down that guest's wait stream. **A guest
  never holds a LiveKit token before admission**, so the lobby cannot be
  bypassed by talking to LiveKit directly.
- **Limits:** 20 waiting knocks per meeting, 10 knocks per source address per
  10 minutes, 8 admitted participants (owner included) by default. Over the
  limit is `429`, never a silent drop.
- **The owner can remove** an admitted guest (`RemoveParticipant`); a removed
  guest's ticket is dead, so they must knock again.

### LiveKit tokens

| who | grants | TTL |
|---|---|---|
| owner | `roomJoin`, `room=<meeting room>`, `canPublish` (camera, microphone, screen_share, screen_share_audio), `canSubscribe`, `roomAdmin`, `canPublishData` | 10 min to connect |
| guest | `roomJoin`, `room=<meeting room>`, `canPublish` (camera, microphone, screen_share, screen_share_audio), `canSubscribe`. **Not** `roomAdmin`, **not** `canPublishData`, **not** `hidden` | 10 min to connect |
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
| `owners` | `owner_id`, `owner_login` (from `/self`), `token_ref`, settings: `record_default` (on), `ai_default` (on), `retention_days_recording` (30), `retention_days_text` (365) |
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
| `POST /api/meetings/{id}/record {"on"}` · `POST /api/meetings/{id}/ai {"on"}` | off at any time; on again only until the first guest is admitted |
| `GET /api/meetings/{id}/events` | SSE: lobby changes, transcript segments, action items, recording state |
| `POST /api/meetings/{id}/lobby/{pid}/admit` · `/deny` · `DELETE …/participants/{pid}` | lobby and removal |
| `POST /api/meetings/{id}/end` | end it |
| `GET /api/meetings/{id}/transcript?format=json\|txt\|vtt` | the transcript |
| `GET /api/meetings/{id}/recording` | a `302` to a presigned object URL valid 5 minutes, or a stream through the plugin if the store is not reachable from browsers |
| `POST /api/items/{id}/task` · `POST /api/items/{id}/dismiss` | [Make task](#make-this-a-muster-task), dismiss |
| `DELETE /api/meetings/{id}` | delete now: rows and objects |
| `GET/PUT /api/settings` | record and AI defaults, retention |

Guest and machine routes:

| route | does |
|---|---|
| `GET /j/{code}` | the guest page (`410` once ended) |
| `POST /j/{code}/knock` · `GET /j/{code}/wait` | above |
| `GET /j/{code}/events?ticket=…` | SSE for an admitted guest: transcript and items |
| `POST /livekit/webhook` | LiveKit's events; the `Authorization` JWT is verified against the API secret and the body's SHA-256 inside it; anything else `401` |
| `GET /healthz`, `GET /metrics` | on an internal port only |

Every page sets its own strict CSP (`default-src 'self'`, `connect-src 'self'
wss://meet-rtc.<muster host>`, `frame-ancestors <muster's origin>` on owner pages and
`'none'` on guest pages), `Referrer-Policy: no-referrer`, and
`Permissions-Policy: camera=(self), microphone=(self), display-capture=(self)`.

## Media: LiveKit, TURN, Egress

- **LiveKit server**, one node, its config from a mounted file: API key and
  secret from a secret, `room.empty_timeout`, `room.max_participants`, the
  webhook URL `http://muster-meetings:…/livekit/webhook`, Redis address.
- **Ports.** Media on **one UDP port with muxing** (`rtc.udp_port`, e.g.
  `7882/udp`), TCP fallback `7881/tcp`, TURN/UDP `3478/udp`, TURN/TLS `443`
  on its own host name. One UDP port, not a range, because the deployment
  reaches the internet through a relay, and one forwarded port is what a
  relay can carry cleanly.
- **Through the relay.** The relay forwards `7882/udp`, `3478/udp` and
  `7881/tcp` to the SFU, and passes TLS for `meet-turn.<muster host>` and `meet-rtc.<muster host>`
  through by SNI. LiveKit must advertise the relay's **public** address as its
  node IP (`rtc.node_ip`), not its pod address. If the relay can carry only
  TCP, everything still works over TURN/TLS on 443, with more latency
  ([spike S1](#phase-0-spikes) measures both).
- **TURN: LiveKit's built-in TURN, not coturn.** It shares LiveKit's
  credentials (no separate TURN secret to rotate) and is one fewer service.
  Coturn is the fallback only if S1 shows the built-in TURN cannot be
  reached through the relay.
- **Recording: room composite Egress** (a headless Chrome rendering the
  default grid layout) to MP4 in the meetings bucket, key
  `meetings/<owner_id>/<meeting_id>/recording.mp4`. Started when the first
  guest is admitted with **Record** on, stopped at end. About 2–4 CPU and
  2–4 GiB per concurrent recording (**UNVERIFIED** for our hardware; S3).
- **Audio for STT: track Egress to a websocket** (per audio track, raw PCM to
  `ws://muster-meetings:…/stt/ingest/<track>`), started by the plugin on
  every `track_published` webhook for a microphone track. No browser, little
  CPU. **UNVERIFIED** (S2): if the PCM format or reliability is not what the
  docs suggest, the fallback is a server-side participant built on LiveKit's
  Go SDK with an Opus decoder.
- **The object store** is a bucket of its own (`meetings`), with an access
  key scoped to `meetings/*` that only the plugin and Egress hold. muster's
  own upload key is not reused and gains nothing.

## Transcription

### Choosing the streaming STT model

Vendor figures, not measured on our hardware; S4 measures the recommendation.

| model | streaming | latency | WER | serving | licence |
|---|---|---|---|---|---|
| **Kyutai `stt-1b-en_fr`** (recommended) | native (delayed streams), semantic VAD | 0.5 s fixed delay | `stt-2.6b-en`: Open ASR 6.4, AMI 12.17 (no figure published for the 1b) | Rust `moshi-server`, websocket, batches many streams on one GPU (vendor: 64 streams at 3× real time on an L40S) | weights CC-BY-4.0, code MIT/Apache |
| Kyutai `stt-2.6b-en` | native | 2.5 s delay | as above | same | same |
| NVIDIA Nemotron Speech Streaming en 0.6B (fallback) | native, cache-aware RNNT | 80–1120 ms chunks | avg 7.07 at 560 ms | NeMo, Riva/NIM (gRPC); no memory figures published | NVIDIA Open Model License |
| Mistral Voxtral Mini 4B Realtime | native | 0.16–2.4 s | FLEURS 6.73–8.72 (multilingual, not comparable) | vLLM `/v1/realtime`; needs ≥16 GB | Apache-2.0 |
| Parakeet TDT 0.6B v2/v3 | chunked emulation | chunk-dependent | Open ASR 6.05 / 6.34 | NeMo, no bundled server | CC-BY-4.0 |
| faster-whisper + WhisperLive | emulation (re-decodes a rolling buffer) | 1–3 s | ~7.4 (large-v3) | websocket, default 4 clients | MIT |
| Moonshine v2 | native | 50–258 ms (on an Apple M3) | 6.65–12.0 | edge-targeted, no server | permissive, English |
| Cohere Transcribe (existing) | **no** | per utterance | **5.42** | the existing `stt-cohere` | Apache-2.0 |

Sources: [Kyutai repo](https://github.com/kyutai-labs/delayed-streams-modeling),
[stt-2.6b-en card](https://huggingface.co/kyutai/stt-2.6b-en),
[Nemotron card](https://huggingface.co/nvidia/nemotron-speech-streaming-en-0.6b),
[Voxtral Realtime card](https://huggingface.co/mistralai/Voxtral-Mini-4B-Realtime-2602),
[Parakeet v3 card](https://huggingface.co/nvidia/parakeet-tdt-0.6b-v3),
[WhisperLive](https://github.com/collabora/WhisperLive),
[Moonshine v2](https://arxiv.org/html/2602.12241v1),
[LiveKit STT plugins](https://docs.livekit.io/agents/models/stt.md).

**Recommendation: Kyutai `stt-1b-en_fr` on `moshi-server`, on the GPU host.**
It is the only option that streams natively, ships its own websocket server,
and batches many concurrent streams on one GPU, which is exactly the shape of
one stream per participant. The 0.5 s delay makes captions feel live. Its
memory on our GPU, next to `stt-cohere`, is **UNVERIFIED** (S4). Fallback:
Nemotron Speech Streaming behind Riva if S4 fails on accuracy or memory.
Rejected: Whisper emulation (re-decoding costs GPU per stream, and its
maintainers call the LocalAgreement approach outdated); Voxtral Realtime
(16 GB is too much beside the existing service; revisit for multilingual).

### Three engines, one interface

```go
type Engine interface {
    // Open starts one stream for one participant's track.
    Open(ctx context.Context, track TrackRef) (Stream, error)
}
type Stream interface {
    Write(pcm []int16) error            // 16 kHz mono (resampled from Egress' rate)
    Segments() <-chan Segment           // partial and final, with start/end ms
    Close() error
}
```

| engine | `MEETINGS_STT_ENGINE` | output | use |
|---|---|---|---|
| `kyutai` | `kyutai` + `MEETINGS_STT_URL` (`wss://…`) | word-level partials, finals at semantic end of turn | live captions (recommended) |
| `utterance` | `utterance` + the existing `MUSTER_STT_URL`/token pair | one final per utterance, after a pause (≤15 s segments, energy-based endpointing) | zero new service; dev; Compose without a second GPU service |
| `fake` | `fake` | scripted text per participant | tests only; refuses to start unless `MEETINGS_TEST_MODE=1` |

The utterance engine sends each VAD-cut utterance to the existing
`POST /v1/audio/transcriptions`. Captions appear sentence by sentence, about
1–2 s after the speaker pauses. That is good enough for action items, which
run on 10 s windows anyway, but not word-by-word. It shares the GPU and the
threadpool with dictation, so it is capped at 4 in flight.

**The post-meeting pass (phase 6, optional):** after the meeting, each
participant's audio (kept by the track Egress as Opus files) is sent to the
existing Cohere service and the result replaces the live segments as
`pass=final`, aligned by time. This gets Cohere's accuracy for the archive at
no new cost. Off by default; it is the one place long audio meets the
service's 3600 s cap, so a longer meeting is sent in 30-minute parts.

### Segments

- Each segment carries its participant, its start and end in meeting time
  (milliseconds from the room's start), its text, and whether it is final.
- Partials are sent to the SSE streams and **never stored**. Finals are
  stored and sent.
- A silent or muted track sends nothing (the engine's VAD, and a
  constant-signal check for the utterance engine, as `app.py` already does).
- If the STT engine is down, the transcript panel says "Transcript paused"
  and the gap is stored as a marker, so the archive shows where text is
  missing rather than appearing complete.

## Action items

### What ScaleDown Extract is

`scaledown/extract` is a model on OpenRouter from the provider ScaleDown
([OpenRouter page](https://openrouter.ai/scaledown/extract/api), fetched
2026-11-19; also in OpenRouter's `/api/v1/models`). It is **description-driven
entity extraction**: the caller describes the fields it wants in plain
English, and it returns **spans copied from the input**, each with a type, a
confidence and character offsets
([ScaleDown docs](https://docs.scaledown.ai/api-reference/extract-overview)).

- Price: $0.04 per million input tokens, output free (OpenRouter's listing).
- Context: 1M tokens. Listed parameters: `max_tokens` only; no
  `response_format`, `tools` or structured outputs through OpenRouter. The
  fields are passed in `provider.options.scaledown.entities`; the exact
  payload shape through OpenRouter is **UNVERIFIED** (no example found).
- Response: `{"entities": [{"text", "type", "confidence", "start", "end"}]}`.
- OpenRouter publishes no latency for it yet (its field reads `null`); a
  "0.85 s P50" figure in search results was not confirmed by any page.
- Not context compression: that is the separate `scaledown/compress`.

**What that means here.** Extract can only return text that is in the
transcript. "Ada: sure, I'll send the deck Friday" becomes the span "I'll send
the deck Friday", not "Ada to send the deck by Friday". The assignee then
comes from the segment's speaker (which we know from the track), and the due
date from a second entity type. Whether that reads well enough is what the
evaluation decides. It is cheap enough that the cost is nearly zero either
way.

**The fallback: `deepseek/deepseek-v4-flash`** ($0.09 / M in, $0.18 / M out,
per OpenRouter's listing on 2026-11-19), a chat model with `response_format`
(JSON schema) support, which rewrites each item as owner, task, due. Strict
schema enforcement on its route is **UNVERIFIED** until one live request
with `strict: true` (S6). The `~deepseek/deepseek-flash-latest` alias is not
used: a model that moves under a passed evaluation invalidates it.

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
  | `MEETINGS_ITEMS_PROVIDER_PREFS` | `{"data_collection": "deny"}` | passed as OpenRouter's `provider` object, so only providers that do not retain prompts are used |

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
- **Choice:** `scaledown/extract` if it passes every gate; else
  `deepseek/deepseek-v4-flash` if it does; if both pass, the one with the
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
- **Metrics:** WER per channel (Whisper's English normaliser), finalisation
  latency (audio time of a word's end to its final segment), p50/p95, GPU
  memory at 4, 8 and 16 concurrent streams, and whether latency holds at each.
- **Pass bar for live use:** WER ≤ 18% on AMI headset, p95 finalisation
  ≤ 2.5 s, 8 concurrent streams with p95 still ≤ 2.5 s, fits in the GPU memory
  left beside `stt-cohere`.
- **Controls:** a silent channel must produce no text; a channel of a known
  read passage must produce WER under 10%; the harness must report a higher
  WER when fed the wrong channel's reference (it can go red).

## Extras: what is included and why

| extra | included | why |
|---|---|---|
| **(a) Make task** on an action item | **yes**, phase 6 | One click, and the plugin API already does it idempotently; through the new narrow `tasks` capability (decision 11) |
| **(b) Recording/transcribing notice + per-meeting toggles** | **yes, in the MVP**, not as an extra | Recording is on by default (decision 9), so the notice is not optional: it ships with the first recording and the first transcript. Toggles: **Record** and **AI action items**, both on, both off per meeting |
| **(c) Post-meeting summary** | **yes**, phase 6 | One LLM call at meeting end, on the same transcript; cheap, and it is what makes the past-meetings list scannable. Follows the **AI action items** switch: off, no summary |
| **(d) Retention** for recordings and transcripts | **yes**, phase 5 with recording | Recordings are the largest and most sensitive objects; they need an end date from day one. Defaults 30 days (recordings) and 365 days (text), decision 9 |
| Post-meeting Cohere re-transcription | optional, phase 6 | Better archive accuracy using the service that already exists |
| Knock notification to the owner's phone | **yes**, phase 1d (core) + 2b (plugin) | Decision 10: a [core seam](#core-seam-knock-notification) on muster's existing push path, muster-written text |
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
  turning one back on after a guest was admitted is refused, so nobody is
  recorded or sent to the AI service who knocked while it was off. The owner
  sees the same badge.
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
  the same site. muster's cookies are host-only and `HttpOnly`, and its
  state-changing routes check the full-host origin, so the plugin's pages
  can neither read nor use the owner's muster session, and after PR 1f
  ([`__Host-` names](#core-seam-__host--cookie-names)) it cannot plant one
  either.
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
| STT engine down or overloaded | "Transcript paused" | call and recording go on; a gap marker is stored; streams reconnect with backoff |
| OpenRouter 429/5xx | "Action items paused" after 3 windows | backoff; the next window covers the gap; nothing is lost |
| Egress fails to start or dies | "Recording failed" badge for everyone | the meeting goes on; the recording row says failed; the owner sees it on the past meeting |
| Object store full or unreachable | as above | egress fails; transcripts are unaffected (SQLite) |
| TURN unreachable, UDP blocked | the guest sees "Can't connect media" with a retry | TCP and TURN/TLS are tried in turn before that |
| The plugin restarts mid-meeting | a short "Reconnecting" in the panels | the room lives in LiveKit and carries on; on start the plugin rebuilds open meetings from SQLite and `ListParticipants`, re-opens STT streams; speech in the gap is not transcribed (gap marker) |
| LiveKit down | **New meeting** says "Calls are unavailable" | no room can be created; existing calls drop |
| The owner leaves without ending | nothing, at first | the room's 10-minute empty timeout fires `room_finished`; the meeting ends; the link dies |
| The owner's muster token is revoked | the tab goes; an open page loses its frame within 30 s | the plugin, within 60 s, ends that owner's meetings and deletes their sessions |
| muster is down | the tab is unreachable | a live call goes on (it needs only the plugin and LiveKit); guests can still knock and the owner, already in, can admit |
| GPU host also serving dictation | dictation slower | the utterance engine is capped at 4 in flight; `kyutai` runs as its own service |

## Deployment

Generic; the deployment's names and addresses stay out of this repository.
**Everything runs in the muster tenant** (decision 8): the same Kubernetes
namespace as the muster deployment, or the same Compose project, on
subdomains of muster's own host.

**Kubernetes (muster's namespace):**

- `muster-meetings`: a Deployment, 1 replica, `strategy: Recreate` (one
  SQLite writer), a small PVC, its `mpl_` token, the LiveKit key and secret,
  the bucket key and the OpenRouter key from Secrets of its own (muster's
  Secrets are not mounted). It calls muster at muster's in-namespace Service.
  An ingress rule for `meet.<muster host>`, beside muster's own rule for
  `<muster host>`; an internal port for the webhook, STT ingest and metrics.
- LiveKit server: the upstream Helm chart or a plain Deployment, with
  `hostNetwork` or a `LoadBalancer`/`NodePort` Service for `7882/udp`,
  `7881/tcp` and `3478/udp`; TLS for `meet-rtc.<muster host>` and
  `meet-turn.<muster host>` through the ingress or by SNI passthrough at the
  relay.
- LiveKit Egress: a Deployment, sized for the number of concurrent
  recordings. Redis: a single small instance of its own (not muster's, if
  muster has one).
- The streaming STT: a GPU workload on the GPU host, ClusterIP only. Where
  the tenant has no GPU of its own, the `utterance` engine against muster's
  existing STT service is the fallback.
- NetworkPolicies: the plugin may reach muster's Service, LiveKit's API, the
  STT service, the object store and the internet (OpenRouter) only; Egress may
  reach LiveKit, the object store and the plugin's ingest port; STT is
  reachable from the plugin alone; **muster's own pods gain no egress to any
  of these** (muster still calls nothing).

**Docker Compose (muster's stack):** the plugin repository ships a fragment,
`compose.meetings.yml`, used with muster's file
(`docker compose -f docker-compose.yml -f compose.meetings.yml --profile meetings up`),
so its services join muster's project and network: `muster-meetings`,
`livekit`, `egress`, `redis-meetings`, and optionally `stt-kyutai` (NVIDIA
runtime). muster's own file is unchanged. Without a GPU service it runs the
`utterance` engine against muster's `stt-cohere`. Ports published on the
host: the three media ports, and HTTPS for the three subdomains through the
stack's reverse proxy.

**muster's side:** add exactly `https://meet.<muster host>` to
`MUSTER_PLUGIN_ORIGINS`, `meetings` to `MUSTER_PLUGIN_TASK_NAMESPACES`, turn on
web push (for knock notifications), and mint the token on `/plugins` with
**App tab** and **Files tasks** checked.

## Phased plan

Sizes: **S** ≤ 1 day, **M** 2–3 days, **L** 4–6 days, for one engineer
with the test plan's tests included.

| phase | PR | repository | size |
|---|---|---|---|
| **0: spikes** | S1–S6 below; results written into this page | — | M |
| **1: core `app`** | 1a: migration, capability, `/plugins` form, label/icon validation, pairing refusal, docs in plugins.md and configuration.md | muster | M |
| | 1b: hand-off `app` target (with `ref`) and purpose, redeem for `app`, `/apps/{id}` page, frame, CSP and Permissions-Policy, sidebar entry, alive poll, Android "Open in browser" card | muster | L |
| | 1c: `plugin-app.spec.ts`, routes.golden, cls.spec, nav ledger, the host-only cookie golden | muster | M |
| | 1d: the `tasks` capability (new, decision 11): the capability, the `/plugins` checkbox, pairing, the task routes' check, plugins.md | muster | S |
| | 1f: **`__Host-` cookie names (decision 16): must merge and be released before `meet.<muster host>` goes live.** Rename, the loopback exception, expiring the old names, e2e helpers, release note | muster | M |
| | 1e: knock notification (new, decision 10): `POST`/`DELETE /plugin/v1/notify`, the `app` push event and its toggle, payload v2 `type: "app"`, limits, push.md | muster | M |
| **2: plugin MVP (calls)** | 2a: repo from the template, `/enter` and owner sessions, `/self` poll, SQLite, meetings list, New meeting, link | muster-meetings | M |
| | 2b: LiveKit rooms and tokens, guest page, lobby, admit/deny/remove, end, webhook, screen share, consent notice, knock notify calls, mobile layout | muster-meetings | L |
| | 2c: in-tenant Kubernetes manifests (muster's namespace), the Compose fragment for muster's stack, the subdomain/ingress and TURN/relay notes | muster-meetings | M |
| | 2d: e2e click path without transcript or recording | muster-meetings | M |
| **3: transcription** | 3a: track Egress ingest, `Engine` interface, `utterance` and `fake` engines, transcript panel for owner and guests, archive view and export | muster-meetings | M |
| | 3b: `kyutai` engine, partial captions | muster-meetings | M |
| | 3c: `stt-eval` and its run on the GPU host | muster-meetings | M |
| **4: action items** | 4a: `items-eval`, the corpus, the controls | muster-meetings | L |
| | 4b: worker, guards, `extract` and `chat_json` adapters, live panel for owner and guests, the per-meeting **AI action items** switch | muster-meetings | M |
| | 4c: run the evaluation, set the default model, commit the result | muster-meetings | S |
| **5: recording** | 5: room composite Egress, record toggle (on by default) and badge, playback, retention settings (30 / 365 days) and sweeper, bucket lifecycle docs | muster-meetings | M |
| **6: extras** | 6a: Make task; 6b: summary; 6c: Cohere post-pass (optional); 6d: ecosystem.md row | both | S each |
| **7: in-app calls on Android (optional)** | the `app-origins` bridge message and the camera grant | muster, muster-android | M |

Core first because every later PR stands on it, and because it is the only
part that changes muster for everyone. The 2026-11-20 decisions add three core
PRs (1d, S; 1e, M; 1f, M), about 5–7 engineer-days in all. **1f gates
phase 2c's go-live**: the plugin may be developed and tested before 1f, but
`meet.<muster host>` is not added to DNS or `MUSTER_PLUGIN_ORIGINS` on a live
muster until a release carrying 1f is deployed there; no plugin PR changes size
(in-tenant hosting simplifies 2c's DNS and certificates rather than adding
to it). 1d and 1e are independent of 1b and can go in parallel; 2b's knock
calls need 1e, and 6a needs 1d. The plugin's phases 2–5 can each ship
alone: a call with no transcript is already useful.

### Phase 0 spikes

Each answers one **UNVERIFIED** claim above, with a go/no-go.

| # | question | go if |
|---|---|---|
| S1 | Do media and TURN work through the relay, from a phone on a mobile network and from a network that blocks UDP? | both connect; p95 round trip under 300 ms on UDP |
| S2 | Does track Egress to a websocket deliver usable PCM (format, rate, gaps) for 30 minutes? | yes, or the Go-SDK participant fallback is chosen |
| S3 | Room composite Egress CPU and memory for one 8-person, 1-hour recording | fits the cluster with one concurrent recording |
| S4 | Kyutai `stt-1b-en_fr` on the GPU host: memory beside `stt-cohere`, 8 concurrent streams, WER on the AMI subset | meets the [STT bar](#stt-evaluation) |
| S5 | In Chrome, Firefox and Safari: camera, microphone and `getDisplayMedia` inside the sandboxed cross-origin frame with the `allow` and `Permissions-Policy` above; Android WebView `getDisplayMedia` | all three desktop browsers work (Safari's screen share is the likeliest gap); the Android result decides whether phase 7 is worth it |
| S6 | One live request each: `scaledown/extract` with `provider.options.scaledown.entities`, and `deepseek/deepseek-v4-flash` with a strict JSON schema | both answer in the documented shape |

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
  every route). *Red if:* a feature is added, `self` is added to `camera`,
  or the header leaks to another page.
- **The frame, exactly.** The rendered `sandbox` attribute equals
  `PluginChipSandbox` and the `allow` attribute is a pinned whole string with
  the exact origin. *Red if:* `allow-top-navigation` is added, or the origin
  becomes a wildcard.
- **421 still holds** for a `Host` under the meetings zone (an invariant
  guard: the existing test, extended with a new host).
- **Alive poll.** `GET /ui/apps/{id}/alive` is `204` for a live plugin and
  `404` after revoke.
- **Host-only cookies (decision 8's premise).** A golden over every
  `Set-Cookie` muster-server emits across the sign-in, step-up, GoTrue,
  known-device and push-device paths: each has no `Domain` attribute and is
  `HttpOnly`. *Red if:* a `Domain` is added to any one of the five literals
  (one mutation each). An invariant guard: no cookie violates it today.
- **`__Host-` names (PR 1f).** On an https test server, every `Set-Cookie`
  muster emits on the five paths is named `__Host-…`, is `Secure`, has
  `Path=/` and no `Domain` (pinned whole strings). *Red if:* one literal keeps
  its old name, or gains a `Domain`. A request carrying only an old-named
  valid session is treated as signed out and its response expires the old
  name (red if the old name is still read). On the loopback-http page the
  old name is used and read, and the `__Host-` name is not (red if the
  prefix is applied there: WebKit would drop it and loopback sign-in would
  break). Playwright: a browser with an old-named session cookie lands on
  `/login`, signs in once and is signed in afterwards; a cookie
  `__Host-muster_session` set by a page on a sibling host with
  `Domain=<host>` is refused by Chromium and muster sees no session (the
  browser-side control that the prefix does what this design relies on).
- **Sibling subdomain refused.** A `POST` to a state-changing route with a
  valid session cookie and `Origin: https://meet.example.test` (a sibling of
  the test server's host) is refused by `requireSameOrigin`; the same request
  with muster's own origin passes (the positive control). Invariant guard
  for the existing check, now load-bearing for this design.
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
- **positive:** inside the frame all three media calls succeed (Chromium with
  `--use-fake-device-for-media-stream --use-fake-ui-for-media-stream
  --auto-select-desktop-capture-source=…`); the redeem answered purpose
  `app`;
- **negative control:** the same stub framed from a page without the `allow`
  attribute (a test-only route) is denied all three, so the positive above is
  not the fake-UI flags granting everything;
- revoking the plugin on `/plugins` in a second tab removes the frame within
  35 s and the stub reports its tracks ended;
- with `window.musterApp` injected, no frame is drawn, the **Open** card is,
  and its tap navigates top-level to a freshly minted URL;
- `cls.spec.ts` measures the page load.

### Plugin unit tests

In `muster-meetings`, Go, fakes from `internal/mustertest` (extended with
redeem, self and tasks) and a fake LiveKit server API:

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
- **Defaults and toggles:** a new meeting has `record` and `ai` on; either
  can be turned off at any time; turning either back on after a guest was
  admitted is refused. *Red if:* a default flips, or the re-enable check is
  removed (one each).
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
  dropped.
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
- **Recording:** with **Record** on, room composite Egress writes an object;
  its size is non-zero and `ffprobe` reads a duration within 10% of the
  meeting's.
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
   default leg); the guest's has no Make task button. Between steps 3 and 4, the
   knock push reaches the owner while they are not on the meeting page. muster's
   own `push.spec.ts` stubs the browser side and has no receiver, so this
   needs a **new push sink**: a local HTTP server registered as the owner's
   subscription endpoint (admitted by `MUSTER_PUSH_EXTRA_HOSTS`), holding the
   subscription's key pair so it can decrypt the payload. The test asserts
   muster's text and no guest name.
9. **Make task**: muster's API shows a task tagged
   `meetings:<meeting>-<item>`; a second tap files nothing new.
10. **End meeting**: the guest sees "This meeting has ended"; reloading the
    guest link answers `410`.
11. The past meeting is listed with Ada, its transcript (the line, under
    "Ada"), a recording whose `<video>` plays to a non-zero duration, the
    action item marked "Task #n", and a summary.

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
| AI switch | ignore it in the worker | "AI off sends nothing" and the e2e defaults case |

## Open questions

None open. Every question so far has been answered (decisions 6–16).

## Decisions

Decisions 1–5 are from the operator's brief. Decisions 6–16 are the operator's answers
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
   for utterance mode and the post-pass. Transcription is per track, not
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
   every muster cookie host-only, so this is not a blocker
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
