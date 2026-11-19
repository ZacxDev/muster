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
| `agent_outcome` | An agent you dispatched **finished** — only when it says so (`<agent> finished: <summary>` from `agent_report_done`, or `… moved its task to complete`; a move to ready for review is told by the `review` push alone) — is **waiting for you** (`<agent> is waiting for you: <the last line of its reply>`: its turn ended without saying it finished, e.g. a question), **failed** (`<agent> failed: <reason>`: a turn of it failed, and its status is now `error`) or went **quiet** (`<agent> is stuck: no reply since kickoff` / `… silent for 30 min`). One notification per change of its outcome, tagged `agent-<id>-outcome` so a newer one replaces the older on the device; a tap opens the agent's page. See [provisioner.md](provisioner.md#how-an-agents-work-ends). An agent the chief started **on one of your hosts** pushes under the same toggle, tagged `host-dispatch-<id>-outcome`: `<runtime> on <host> finished: <summary>` (Claude's `agent_report_done`, or an opencode reply whose last line is `MUSTER-DONE: <summary>`) or, for opencode, `… is waiting for you: <the reply's last line>`; a tap opens the host's agents session. |
| `created` | A machine (an agent, the CLI) created tasks. |
| `comment` | A machine commented on a task, or a member commented on a team task ([below](#team-notifications)). |
| `plugin` | Your chief drafted a reply to a [plugin](plugins.md) message, and it awaits your approval. The notification's text is muster's own: it never quotes the message or the draft. A tap opens the draft's card on the Asks page's Drafts tab (`/attention?tab=drafts#draft-<id>`). An officer's outbound draft is titled `A message awaits your approval`, its body naming the officer, and opens its card the same way ([below](#officer-notifications-decisions-only)). The same toggle carries the notice that your chief [routed a plugin message or a task to a session](plugins.md#you-are-told-where-the-chief-routed-it) on its own: one per route, `Chief routed it to <tmux place>`, its body the plugin's label and the message's subject (or the task's number and title), tagged `route-<route id>`; a tap opens the session (or the task). Information only: nothing to answer. |
| `attention` | Something new in your [attention queue](attention.md): an agent asked you a question (the notification shows the asker and the question's first line), or a mirrored Claude Code session started waiting at a prompt. One notification per item; it is withdrawn when the item is answered, dismissed, expires or stops waiting. A tap opens the item's session when it has one (a waiting window's or a held question's rendered view, an asking agent's page, an asking mirrored session's window) and its item on `/attention` otherwise, decided when tapped (`/attention/open/<key>`; [attention.md](attention.md)). A decided reply draft's `plugin` notification is withdrawn the same way. With `MUSTER_SESSION_STATE=1`, a waiting window or a stalled session is pushed instead as its **episode**: one notification per stop, titled by its window as every attention text names it, `<label> · tmux <session>:<window index> @ <host> needs you` (or `is stalled`; [attention.md](attention.md#how-a-window-is-named)), resolved when sent, its body only the state and idle time — never the session's text, except for a question muster can answer (below), whose push carries that question in full — tagged `attention-episode-<id>`, and withdrawn when the stop ends. A waiting window's episode is **not pushed at once**: it is held for 60 seconds and sent once they have passed unless the stop ended first (a window whose host drops meanwhile is still sent, once muster has known the host offline for 60 seconds too), so a prompt answered at the terminal never buzzes ([attention.md](attention.md#stalled--waiting-session-state)). **This holds for a question muster can answer too** (a held AskUserQuestion, an opencode prompt; since the follow-up to #463): its card shows at once, and its push — the question's own full-text notification, `<where> is asking`, tagged `attention-episode-<id>` — is sent only if it is still open after 60 seconds; a restart never re-sends one it already sent. Without session state, or for a window muster cannot key, the waiting push (`attention-session-<hash>`) is held by the same 60 seconds. |
| `page` | Someone sent a response through one of your [pages](pages.md)' links: at most one notification per page a minute. |
| `page_published` | An agent published a new page for you (`page_create`): `Page ready: <title>`, its body `From agent <name>.` or `From session "<name>".`, tagged `page-<id>-ready`; a tap opens your view of it. |
| `page_shared` | An agent shared one of your pages without asking (your own opt-in, [pages.md](pages.md#when-an-agent-shares)): its own toggle, so muting `page_published` never hides that a page went public. |
| `morning_report` | Your [morning report](features.md#morning-report): once a day at the server's report time, only on a day something happened overnight. Titled `While you were away`, its body counts only (`1 question for you · 3 merged · 2 stuck`), never a session's, a task's or a question's text; tagged `morning-report`, so the next day's replaces it. A tap opens that day's report (`/morning/<date>`), where its open questions are answered in place. Sent at most once per day. |
| `page_updated` | An agent published a new **version** of one of your pages (`page_update`): `Page updated: <title> (v<N>)`, its body `From agent <name>.` or `From session "<name>".`; a tap opens that version in your view of the page (`/pages/<id>/view?v=<N>`). One tag per page (`page-<id>-updated`), so a newer version replaces the older on the device. At most 6 per 10 minutes from one agent or session ([the sender's cap](#an-agent-notifies-you)); the rest are dropped and logged. Its own toggle, apart from `page_published`, so you can mute an iterating agent's revisions and still hear of a new page. |
| `agent_notify` | One of your agents sent you a notification with `notify_owner` ([below](#an-agent-notifies-you)). |

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

Under it is **Start notification titles with the tmux name** (off unless you
turn it on; independent of the switch above): every push title about a tmux
window then starts with the window's codename and host, so a phone that cuts
the end of a title still shows it — `mango:1@laptop · Billing migration needs
you`, `mango:1@laptop · A session is waiting on you`, `agents:3@laptop ·
Claude Code is waiting for you: …`
([focus.md](focus.md#tmux-name-first-in-push-titles)). It changes push titles
only, on the browser and on the paired phone alike; its POST is
`/account/notifications/codename` (`on=1|0`).

### An agent notifies you

Any of your agents can send you a notification of its own with the MCP tool
`notify_owner {title, body?, url?}`: your mirrored Claude Code and opencode
sessions (`POST /mirror/mcp`), your chief and your dispatched agents (their
gateway turns and `POST /mcp`), and an agent the chief started on one of your
hosts. It is offered while muster's own push is on (the `push` feature), and
never on a [team host](#team-notifications): a team has no one owner to
notify, and a team-owned agent's call is refused.

- **Who sent it.** The body always starts with the sender as muster knows the
  caller, never as the caller describes itself: `From agent <name>
  (dispatched)`, `From your chief`, `From session "<name>"` for a mirrored
  session, and `From session "<name>" (spawned on a host)` for an agent the
  chief started on a host. Every kind has its own fixed marker. A session's
  name is quoted (its own `"` become `'`) and cut to 60 characters, because
  a session's name can come from its own transcript: a session named
  "Your chief" still reads `From session "Your chief"`. The title is the
  agent's own, so read the `From` line to tell who sent it. Title and body are flattened to plain text, the
  title cut to 80 characters and the body to 200. One tag per sender, so a
  sender's newer notification replaces its older one on the device.
- **What a tap opens.** `url` is either a muster path (`/tasks/42`,
  `/pages/7/view`) or an `https` address on a host listed in
  `MUSTER_NOTIFY_LINK_HOSTS` ([configuration.md](configuration.md)), matched
  exactly (no subdomains), with no user name, password or port. Anything
  else is refused with a sentence: `http:`, `javascript:`, `data:`, another
  host, `//host`, `/\host`, the same written with `%2F` or `%5C`, `.` and `..`
  segments, spaces, control and non-ASCII characters. With no `url`, a
  dispatched agent's notification opens its own page, and any other opens
  muster's home. An external address is never put in a notification as the
  tap target: the notification opens muster's own `/notify/open?to=<address>`
  (signed in), which checks the address against the **current** list again
  and then redirects with no referrer. A host you take off the list stops
  working at once, also for notifications already sent. The browser opens it
  in a new window, never in an open muster tab. The Android app opens it as
  any muster path; what its shell does with the redirect is the app's own.
- **Limits.** Each sender may send 6 per 10 minutes. That cap is on top of
  your own cap (30 notifications per 10 minutes, all events together) and
  the **An agent sent you a notification** toggle in Account → Notifications,
  which is on by default.
- **What the agent is told.** The tool waits for the send and says what
  happened: `delivered` (a push service accepted it for that many devices,
  which is not proof you read it), `suppressed` with the reason
  (`sender_cap`, `owner_cap`, `muted`, `officer` for an [officer](#officer-notifications-decisions-only)), `no_device`, `failed`, or
  `not_configured`.
- **The record.** Every call writes one `audit_log` row (`agent.notify`): the
  sender and its kind, the title, the link, and the outcome. **Account →
  Notifications** lists your 20 newest. Only you read them; they are not on
  the admins' activity list.

### Every dropped notification is logged

A notification that reaches nobody leaves one line in muster's log, at the
drop, naming its tag, its event, the owner and why:

```
push: dropped tag=page-12-ready event=page_published owner=3: no device is subscribed
```

The reasons: the event is muted, the owner's cap is reached (every drop, not
only the first), the sender's cap is reached, no device is subscribed, every
device refused or failed it, a team with no operator or manager whose
notifications are on, and a notification that names no owner. On a server with
no push at all, every notification is dropped, so that is logged once, for the
first.

### Officer notifications: decisions only

An [officer](officers.md) pushes you **only when you have something to
decide**, and a tap opens the card you decide it on. Everything else an officer
does is on its page, the sidebar and the Asks page when you look; none of it
pushes. One table in the server classifies every push an officer can cause
(`internal/api/officer_push.go`), and one function names each decision's card,
so a later change can move a card without touching each push. A test pins
every push call site in the server as either routed through that table or
named, with a reason, as one no officer reaches: a new push path fails it
until it is classified.

| what the officer did | pushes? | a tap opens |
|---|---|---|
| asked you a question (`officer_ask_person`) | yes (`attention`) | the ask's card (`/attention/open/ask-<id>`: its own page while open) |
| escalated an item to you | yes (`attention`) | the escalation's card, as above |
| filed an outbound draft its plugin passed | yes (`plugin`) | the draft's card (`/attention?tab=drafts#draft-<id>`) |
| filed a day's follows and likes | yes (`plugin`), one per batch | the batch's card (`/attention?tab=drafts#outreach-<…>`) |
| asked for a connector | yes (`connector`) | the request's card (`/connectors#connector-request-<id>`, or the team's page) |
| its plugin refused a draft | no: the draft went back to the officer; its "awaits your approval" notification is withdrawn | — |
| its daily budget was reached (it is held for the day) | yes (`attention`), once per day | the officer's page (`/officers/<id>?owner=<owner>`), where the hold is lifted or the day's cap raised |
| its judgement pool is held by a refused cap escalation | yes (`attention`), once per hold; withdrawn when the hold ends | the officer's page, as above |
| its spend could not be read | no: muster's own failure, in its log | — |
| a host instance has not claimed its item | no: its notice is on the Asks page | — |
| published a page, or shared one under your opt-in to unasked shares | no: the page lists its links | — |
| published a new version of a page (`page_update`) | no: the page shows it | — |
| called `notify_owner` | no: the tool answers `suppressed` (reason `officer`); to ask you something it uses `officer_ask_person` | — |

What an officer's instance does through the tools every agent has is pushed as
any agent's is: a generic `attention_ask` or `page_share_request` (an ask, a
decision, opening its card), a task it moves to ready for review (the task), a
host step-up confirmation, and a host officer's window waiting at a prompt.
Each dropped officer push logs one line, `push: officer <path> is information
only: not pushed (owner <id>, tag <tag>)`.

### Team notifications

A notification owned by a [team](auth.md#teams) reaches the team's
**operators and managers**, never its viewers. muster decides who at send
time, from the team's current members: someone who has left, a disabled
person, and a member who turned the team's switch off get nothing. This
covers every team-owned push: a team officer's asks, escalations, drafts
and outreach batches; a team's reply drafts; its task notifications; and a
member's comment on a team task (below).

- **Each member is decided on their own.** For each recipient muster applies
  that person's own event toggles, their own rate cap, their own **Start
  notification titles with the tmux name** switch, and the router's admin
  rule. A busy team spends each member's cap, not one shared one.
- **The title names the team.** It starts with the team's name and ` · `, as
  in `platform · Ana is asking`. The prefix goes on before the title is
  plained, so it survives a long title.
- **Withdrawals.** When a team's item is answered, decided or ends, its
  notification is withdrawn from the team's current operators and managers,
  whatever their switch says. A person who is removed from the team or
  downgraded to viewer is sent, once the change has saved, a withdrawal for
  each of the team's open items: asks, pending drafts and outreach batches,
  tasks in review, and officer cap-hold notices. A person **disabled** on the
  server's pages (Admin → Users) is sent the same withdrawals **before** the
  disable, on their browsers and their paired phones alike.
  - **Limits.** A disable through `muster-server accounts` (the CLI) sends
    **no** withdrawal: that process has no push service. The disable then
    deletes the person's paired phones, so their team notifications stay on
    those phones (a tap opens nothing: the phone's credential is gone), and
    on their browsers until each item's next change.
  - A notification already on its way when the person leaves can land
    just after its withdrawal, and then stays.
- **The switch.** **Account → Teams** (`/account/teams`) lists your teams,
  your role in each, and one switch per team: **Notifications from this
  team**, on by default. It is yours; a manager cannot set it for you. Off
  means no push from that team at all. Its items stay on the Asks page. The
  POST is `/account/teams/{id}/notify` (`on=1|0`); a team you are not in
  answers 404.
- **A comment on a team task.** A member's comment on a team task, typed on
  the task's page, pushes the team's other operators and managers under the
  `comment` toggle, at once, titled `New comment on task #N`, its body the
  commenter's name and the comment. The person who wrote it is not pushed.
  A comment on your own task still pushes no one. An agent's comment
  (`/agent/task/comment`, an agent's task tool) pushes no one, on a team
  task as on yours. The machine route `POST /api/tasks/{id}/comments`
  (`muster task comment`) takes the hook token or a personal token, which
  carry no team, so it never reaches a team task: it answers 404 there and
  pushes no one.
- **Open pages refresh.** A change to a team's item (an ask, a draft or an
  outreach batch arriving or being decided) also refreshes the open
  Attention and ask pages of the team's current operators and managers,
  whatever their switch says: a refresh is not a push.
- **A dropped team push is counted.** If muster cannot read who a
  notification's owner is, it sends it to the owner alone; for a team, which
  has no devices, that is nobody. It logs `push: read owner <id>'s team
  recipients: …` and counts in `muster_push_suppressed_total`:
  `reason="team_read_failed"` when the owner is known to be a team (a
  dropped team push), and `reason="owner_kind_unread"` when muster had not
  yet read the owner's kind (a person's push did go; a team's did not).
- A waiting or done push for a session on a **team host** is the team's
  episode, pushed to its operators and managers once each, through the same
  fan-out (never a viewer). No stall classifier runs for a team's session.

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
- `url` is always a path on muster, never another origin's address: an
  agent's link to an allowed external host is sent as muster's own
  `/notify/open?to=<address>` ([above](#an-agent-notifies-you)). The browser's
  service worker likewise opens only a path on its own origin from any
  notification, and anything else (another origin, `//host`, `/\host`,
  `javascript:`) opens muster's home.
- An attention **ask** adds `ask`: `{"id": <ask id>, "options": [...],
  "suggestions": [...]}`, the chips the app draws — at most three of each,
  each whole (one longer than 140 characters is left out, never cut). When the
  suggested replies arrive after the first push, the same tag is pushed again
  with `"update": true`: replace the notification without alerting again. The
  app reads both (muster-android #16, `7af3250b7`, first in v0.1.5; still so
  at v0.4.0): it draws the options as buttons plus Dismiss, at most three in
  all, never a suggestion (`AskPush.buttons()`, `MAX_BUTTONS = 3`), and the
  re-push does not alert again (`onlyAlertOnce(p) = p.update`). A chip
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
  and maskable icons at 192 and 512 px (muster's mark, [brand.md](brand.md)),
  and two narrow screenshots. A notification's small badge is the mark in
  white, `badge-72.png`. Neither the
  manifest nor `/sw.js` needs a session. On a server with a [logo](branding.md) its icons are the logo's
  192 and 512 px squares (purpose `any`, no maskable icon), and every
  Web Push notification's `icon` is the logo's too. The Android payload (v2)
  carries no icon; the app reads the logo from `GET /api/instance`.
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

## Several servers (the server switcher)

If you use more than one muster server (a separate deployment at its own
address, with its own account), the sidebar's top row switches between them.
It is the web app's counterpart of the Android app's switcher; the design and
what it cannot match is [design/web-server-switcher.md](design/web-server-switcher.md).

- **The row** shows the server you are on: its colour and its label. On a
  phone it is in the menu (the button at the top left). Tap it, or press Enter
  on it, for the list of servers; the one you are on is marked **In view**.
  Escape closes the list.
- **Add server** asks for the other server's address
  (`https://muster.example.com`; `http://` only for `localhost` or
  `127.0.0.1`). Only the address's origin is kept: a path or anything after
  `?` is dropped. An address with a user name or password in it is refused.
  At most 8 servers, this one included.
- **Switching** opens the other server in this window. You sign in there as
  usual; each server keeps its own sign-in, and its own installed app if you
  installed it.
- **The way back:** a switch tells the other server where it came from (in
  the address after `#`, never sent to the server), and that server offers to
  add it to its own list, also after you sign in there. It asks; it never adds
  it by itself. Because any link can carry the same request, the offer says
  "A link asked", shows the server's full address, and starts on **Not now**.
  Add it only if it is a muster server you use.
- **Labels and logos:** another server is labelled by its address until you
  give it a label of your own. Its display name is not shown, because this
  page never reads another server's data. Its [logo](branding.md) is drawn
  straight from that server (an image of its `/brand/icon-192.png`, with no
  referrer), and only while the list is open. A server with no logo, or one
  that is down or behind a sign-in proxy that does not let `/brand/` through
  ([branding.md](branding.md#behind-an-edge-that-signs-people-in)), shows its
  colour and label. The server you are on shows its own name.
- **Manage servers** renames a server's label (empty: its address again) and
  removes a server from the list. Removing does not sign you out
  of it or turn off its notifications: do that on that server.
- **Notifications:** turn them on on each server (Web Push belongs to one
  address). Each server's notifications then arrive with its own icon, and a
  tap opens that server.
- **Where the list is kept:** in this browser, for this server's address
  (`localStorage`, key `muster-servers`): the addresses and your labels,
  nothing else. It is not synced to other browsers or
  devices, each server has its own list, and anyone who uses this browser
  profile on this server sees it. On iOS an app added to the Home Screen keeps
  a list separate from Safari's.
- **What another server receives** from this one: nothing but the image
  request for its public `/brand/icon-192.png`, with no referrer, and the
  navigation when you switch, which carries no referrer and no path of this
  server. No script on this page reads anything from another server.
  muster's pages may *draw* exactly that one path from another address (the
  CSP's `img-src`). `connect-src` stays `'self'`, and `/api/instance` and
  `/brand/` send no CORS header. muster's sign-in cookie is
  `SameSite=Lax`, so a cross-site image request carries none. Between two
  servers on sibling subdomains of one site, it may carry the other server's
  own cookie, to that same server's public image route.
- **Use a separate host name for each server.** Cookies are not port-scoped,
  so two muster servers on different ports of one host share one
  `muster_session` cookie jar and see each other's sign-in cookie on every
  request, with or without the switcher (it adds nothing beyond ordinary
  navigation between them).
- **A server addressed by IP** may show no logo, only its colour and label:
  the CSP's wildcard host does not match IP addresses in browsers that follow
  the spec (`localhost` and `127.0.0.1` are listed explicitly).
