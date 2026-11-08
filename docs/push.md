# Web push and the installable app

## Web push

muster sends Web Push notifications under its own VAPID identity. To enable them,
set the three variables together. Setting only some of them refuses to boot.

```sh
muster-server vapid generate     # prints MUSTER_VAPID_PUBLIC_KEY=… and MUSTER_VAPID_PRIVATE_KEY=…
export MUSTER_VAPID_SUBJECT='mailto:you@example.com'   # or an https: URL
```

⚠ Generating a new keypair strands every existing subscription, so devices have
to enable notifications again.

When push is off:

- no Enable button is drawn;
- `/api/push/vapid-public-key` answers 503;
- nothing is sent.

**Account → Notifications** (on `/account`) lists your subscribed devices. Each
one has a "Send a test notification" button and a Remove button. Each event type
has its own toggle, and all are on by default:

| event | when |
|---|---|
| `review` | A task becomes ready for review. The title says who moved it. `· agent <name>` means an agent did (`· agent` when the write names no agent, as a mirrored session's `task_set_status` does). `· machine` means `PATCH /api/tasks/{id}/status` (`muster task status`) by a caller muster cannot tell is an agent: the shared hook token, a personal token, or a service caller that is not one of your agents. A service caller that *is* one of your agents is named `· agent <name>`. No label means you did it yourself on the card. On a [plugin](plugins.md#task-references) task reference, a write is queued and the task moves when the upstream confirms it, so the title names the write through the upstream, e.g. `Task #42 ready for review · agent worker-7 via hq`. Your own queued write is still unlabelled. `· upstream (hq)` means the upstream itself changed. The namespace is the reference key's, before the colon. When the task leaves review, the notification is withdrawn. |
| `connector` | An agent requests access to a connector. |
| `agent` | A task's agent comes up. |
| `agent_outcome` | An agent you dispatched **finished** — only when it says so (`<agent> finished: <summary>` from `agent_report_done`, or `… moved its task to complete`; a move to ready for review is told by the `review` push alone) — is **waiting for you** (`<agent> is waiting for you: <the last line of its reply>`: its turn ended without saying it finished, e.g. a question), **failed** (`<agent> failed: <reason>`: a turn of it failed, and its status is now `error`) or went **quiet** (`<agent> is stuck: no reply since kickoff` / `… silent for 30 min`). One notification per change of its outcome, tagged `agent-<id>-outcome` so a newer one replaces the older on the device; a tap opens the agent's page. See [provisioner.md](provisioner.md#how-an-agents-work-ends). |
| `created` | A machine (an agent, the CLI) created tasks. |
| `comment` | A machine commented on a task. |
| `plugin` | Your chief drafted a reply to a [plugin](plugins.md) message, and it awaits your approval. The notification's text is muster's own: it never quotes the message or the draft. A tap opens the draft's card on the Asks page's Reply drafts tab (`/attention?tab=drafts#draft-<id>`). The same toggle carries the notice that your chief [routed a plugin message or a task to a session](plugins.md#you-are-told-where-the-chief-routed-it) on its own: one per route, `Chief routed it to <tmux place>`, its body the plugin's label and the message's subject (or the task's number and title), tagged `route-<route id>`; a tap opens the session (or the task). Information only: nothing to answer. |
| `attention` | Something new in your [attention queue](attention.md): an agent asked you a question (the notification shows the asker and the question's first line), or a mirrored Claude Code session started waiting at a prompt. One notification per item; it is withdrawn when the item is answered, dismissed, expires or stops waiting. A tap opens the item's session when it has one (a waiting window's or a held question's rendered view, an asking agent's page, an asking mirrored session's window) and its item on `/attention` otherwise, decided when tapped (`/attention/open/<key>`; [attention.md](attention.md)). A decided reply draft's `plugin` notification is withdrawn the same way. With `MUSTER_SESSION_STATE=1`, a waiting window or a stalled session is pushed instead as its **episode**: one notification per stop, titled by its window as every attention text names it, `<label> · tmux <session>:<window index> @ <host> needs you` (or `is stalled`; [attention.md](attention.md#how-a-window-is-named)), resolved when sent, its body only the state and idle time — never the session's text, except for a question muster can answer (below), whose push carries that question in full — tagged `attention-episode-<id>`, and withdrawn when the stop ends. A waiting window's episode is **not pushed at once**: it is held for 60 seconds and sent once they have passed unless the stop ended first (a window whose host drops meanwhile is still sent, once muster has known the host offline for 60 seconds too), so a prompt answered at the terminal never buzzes ([attention.md](attention.md#stalled--waiting-session-state)). **This holds for a question muster can answer too** (a held AskUserQuestion, an opencode prompt; since the follow-up to #463): its card shows at once, and its push — the question's own full-text notification, `<where> is asking`, tagged `attention-episode-<id>` — is sent only if it is still open after 60 seconds; a restart never re-sends one it already sent. Without session state, or for a window muster cannot key, the waiting push (`attention-session-<hash>`) is held by the same 60 seconds. |

**The task's reference and creator.** A `review`, `agent`, `created` or
`comment` notification about a task ends its body with the task's external
reference and who created it, in parentheses: `Rotate the build key (hq
868ktt574 · by A. Person)`. The batched "N new tasks" and "N new comments"
notifications do the same for each task they list. The title is unchanged,
because a phone cuts it short. The fragment is appended after the body is cut
to its length cap, so a long title or comment can run past that cap by up to
the fragment's length (at most about 110 characters); in a batched
notification the fragments count toward the summary's cap, so fewer tasks are
listed by name.

- **The reference** is `<namespace> <id>`: a [plugin](plugins.md#task-references)
  task reference's key, or the key tag of a task a plugin filed with `create`
  (or a `clickup:<id>` tag).
- **The creator** is, for a task reference, the upstream's creator when the
  plugin's snapshot sends one (`reference.creator`). For any other task it is
  who muster recorded filing it: `chief` (your chief's `task_create_gated`;
  `agent <name>` if muster could not confirm at filing that the filer is your
  chief) or `plugin <label>` (a plugin's `create`). `POST /api/tasks` (the
  CLI's task create, hook scripts) records no one: it takes the shared hook
  token or a personal token, which name no caller, and an `X-Muster-Actor`
  header on it is ignored. Such a task, and any other with nothing recorded,
  names the producer that filed it, as the card names it (`Task drafter`,
  `Claude Code`, …). A task you filed yourself on the board names no creator:
  the notification is yours, so it would only tell you that you made it. Tasks
  created before this was recorded, and a `POST /api/tasks` with no
  `X-Muster-Source`, name no creator either.

Either part is left out when unknown, and both when neither is known. The
notification never adds the task's description or a comment's text beyond
what it already carried.

The same section has **Show tmux sessions** (off unless you
turn it on): a notification that names a mirrored session by its human name
then adds the window's codename (its tmux address when the host sent none)
and host after it, `Billing migration · mango:1 @ atlas` ([focus.md](focus.md#show-tmux-sessions)). It is
drawn with or without Web Push, since it also names sessions on the Asks
list, in the attention summary and on the session switcher's pills and dock
rows; its POST is `/account/notifications/tmux`
(`on=1|0`).

Push routes are session-authenticated and same-origin:
`/api/push/{vapid-public-key,subscribe,unsubscribe}` and
`/account/push/{test,preferences,{id}/remove}`.

On iOS, a page must be installed to the Home Screen before it can subscribe. The
page tells you this.

### Which push services

A subscription's endpoint is a URL that muster later POSTs to, so muster accepts
only endpoints on a known push service, on `https` and the default port:

- the browser push services: FCM, Mozilla autopush, Apple, and Windows (WNS).
  These are the only hosts a **browser** subscription (Enable, in the header)
  may name.
- your own hosts, listed in `MUSTER_PUSH_EXTRA_HOSTS`, for **paired phones
  only**: a self-hosted ntfy that delivers UnifiedPush to the
  [Android app](design/android.md). Each matches by its exact name only, with no
  subdomains or wildcards. It is empty by default. A browser can never subscribe
  on one of these hosts.

**The list is checked again at every send.** A device whose host is no longer
allowed, for example because you took it out of `MUSTER_PUSH_EXTRA_HOSTS`, is
sent nothing and removed, and the send is counted as
`muster_push_delivery_total{result="disallowed"}`.

**muster never follows a redirect on a push send.** A push service that answers
with a 3xx counts as a failed send, and the device's failure count goes up. This
applies to every push service. Without it, a listed host could redirect a send to
a host that is not on the list.

muster does not check which address a listed host resolves to. An in-cluster
ntfy that resolves to a private address works. `MUSTER_DIAL_DENY_PRIVATE` does
not apply to push sends.

⚠ If your ntfy host is behind a CDN, its bot filter may refuse requests with an
unusual `User-Agent`. muster sends Go's default one (`Go-http-client/1.1`).
A phone's registration fails with an error when muster's test push to it is
refused, so a CDN rule that blocks muster shows up there.

### Paired phones (payload v2)

A phone paired with the [Android app](design/android.md) registers its
UnifiedPush endpoint through the device routes ([auth.md](auth.md#paired-devices)).
It receives the same events as a browser, under the same toggles and rate cap,
as **payload v2**:

```json
{"v": 2, "type": "review", "id": "42", "tag": "task-42",
 "title": "<WHO>", "body": "<WHERE>", "url": "/tasks/42"}
```

- `title` and `body` are the browser notification's own lines. For most
  events that is WHO and WHERE, a waiting session included; an ask's and a held
  question's lines carry the question and its options (operator decision "Full
  text always", [attention.md](attention.md#what-is-in-it)). They are cut to
  100 and 200 characters — the Android app's own limits, so it cuts nothing
  more — and never in the middle of a character a reader sees as one (an
  accent, an emoji, a flag).
- An attention **ask** adds `ask`: `{"id": <ask id>, "options": [...],
  "suggestions": [...]}`, the chips the app draws — at most three of each,
  each whole (one longer than 140 characters is left out, never cut). When the
  suggested replies arrive after the first push, the same tag is pushed again
  with `"update": true`: replace the notification without alerting again. The
  app as shipped (muster-android `43d9b90`) reads neither `ask` nor `update`,
  so it alerts again on that re-push (operator-accepted until it does). A chip
  a secret scan would redact is left out, never sent redacted. A payload that
  would not fit drops the suggestions, then the options, never the question.
- An ask also carries `actions`, the notification's buttons: an `ask_answer`
  per option and per suggestion (naming its chip by index in `ask`, whose
  text is the answer it sends), an inline-reply
  `ask_answer` (`input: true`), and `ask_dismiss` — each confirmed on the phone
  with its step-up key and sent as a signed action
  ([design/android.md](design/android.md#the-signed-ask-actions-as-built)).
  A chip dropped to fit, or left out as secret-shaped, takes its button with
  it. The app as shipped does not draw them yet.
- A [notice](attention.md#notices) (a live chief nudge) is an `attention` push
  with **no** `ask` and **no** `actions`: its title names where the session is
  (`<where> may be waiting on you`, [attention.md](attention.md#how-a-window-is-named)), its body the session's
  focus title (with its age past a day, `· 2d`: [focus.md](focus.md#how-old-a-focus-is)) and the chief's message, its `url` the tap route. Every Android
  app version draws buttons only from `ask`, so on any of them, including one
  that predates notices, it is a plain notification on the asks channel whose
  tap opens the session.
- **A phone's push is exactly 4096 bytes.** muster pads every encrypted push to
  one 4096-byte record, so a payload v2 may be at most 3993 bytes
  (4096 − 86 header − 16 tag − 1 delimiter); the encryption library itself
  refuses anything larger. So muster never sends ntfy a message over its
  default 4096-byte limit, which ntfy would accept with HTTP 200 and silently
  cut short, leaving the phone unable to decrypt it (that matters for senders
  that do not pad to one record). muster checks the size before sending, so an
  over-budget payload is **refused, not sent**: it is logged
  (`push: REFUSED …`), counted as `muster_push_delivery_total{result="oversize"}`,
  and does not count as the phone failing.
- A withdrawal is `{"v": 2, "type": "resolved", "id": …, "tag": …}` and shows
  nothing. Phones always get it; Safari browsers are still spared it.
- Which payload a device gets depends on the kind recorded with its
  subscription, not on its push host.
- There are no `actions`, `conv` or `state` fields yet. They come with the
  phases that add the routes and the widgets that use them.
- ⚠ **ntfy does not check VAPID** on UnifiedPush (`up*`) topics, so anyone who
  learns a phone's endpoint can publish to it. The app's rule of dropping every
  message it cannot decrypt is the **only** check that a message came from
  muster, and it is load-bearing.
- muster sets no Web Push `Topic` header, and ntfy ignores one anyway, so a
  push that was not delivered is not replaced by a newer one.

## Installable app (PWA)

- **The manifest** (`/manifest.webmanifest`) declares a standalone display, any
  and maskable icons at 192 and 512 px, and two narrow screenshots. Neither the
  manifest nor `/sw.js` needs a session.
- **An Install button** appears when the browser offers installation, and never
  inside an installed app.
- **The service worker** (`/sw.js`) is stamped with the build version, so each
  deploy counts as an update. An open page offers the update instead of taking
  it. The worker serves an offline shell, but only while you are offline.
- **The share target:** sharing a link or text to muster opens a pre-filled
  new-task sheet. Nothing is saved until you submit it.
- **Shortcuts:** New task, Tasks, Agents.
- **The badge:** the app icon shows the ready-for-review count, kept current over
  the page's event stream.
