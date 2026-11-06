# What to ask the chief

Prompts that work with the [chief](chief.md), what it does with each, and a real
reply. Type them in the chief's chat (home opens it once the chief is on).

**Every example reply below is real model output**, copied from the page:
the [Compose chief](chief.md#on-the-docker-compose-install) of release
v0.159.0 on an OpenRouter key, on its default model
(`deepseek/deepseek-v4.1-flash`), with `MUSTER_SESSION_ROUTING=1`. **The fleet
is synthetic**: two invented hosts (`atlas`, `nomad`) running muster's
browser-test fake mirror (no tmux, no Claude Code in any window), invented
transcripts, focus titles and tasks. Long replies are cut where `[…]` says so;
nothing else is changed. Each was a new chat. Your replies will differ: it is a
model, and on the examples here it sometimes miscounted or misdescribed its own
tools (see [the limits](#what-it-cannot-do-on-the-compose-install)).

| you want | ask | it calls |
|---|---|---|
| [everything being worked on](#everything-being-worked-on) | "What's everyone working on right now?" | `sessions_overview`, `fleet_list` |
| [who is on what](#who-is-on-what) | "Who's on the billing migration?" | `focus_search` |
| [to hand work to the session on it](#hand-a-request-to-the-session-on-it) | "Send this to whoever's on X: …" | `focus_search`, `route_to_session` |
| [what needs you](#what-needs-me) | "What needs me right now?" | `attention_list` (often with `fleet_list`, `tasks_list`) |
| [what happened while you were away](#what-happened-while-i-was-away) | "What got worked on in the last day? Give me a standup." | `fleet_report` |
| [the task board](#the-task-board) | "What's on the task board? What does task 2 say?" | `tasks_list`, `task_get` |
| [a task routed to the session on it](#route-a-task-to-the-session-on-it) | "Route task 1 to whoever is on billing." | `route_to_session` |

What the tools read, in short: `sessions_overview` is each live session (a
dispatched agent, or a mirrored Claude Code window active in the last 24 hours)
with its status, current [focus](focus.md), recent focus titles, anything it
has waiting on you, and its last few user and assistant messages, secrets
redacted and tool output left out. `fleet_list` is every session with its
focus title and age. `focus_search` is full-text search over focus, current or
over past hours. `fleet_report` covers every session with focus activity in a
window of up to 4 days, ended ones included. `attention_list` is everything
waiting on you. **All of it goes to the chief's model**: on OpenRouter, to
OpenRouter; on a Claude subscription token, to the Claude API.

## Everything being worked on

> What's everyone working on right now?

It reads `fleet_list` and `sessions_overview` and groups the sessions by state.

```text
Fresh read at 20:29Z. Six windows, three working, three parked, one dark.

Actively working (2)

cedar:1 tmux cedar:1 @ atlas — acme-api — flaky upload test; found it races the fake clock, pinning it now.
lynx:1 tmux lynx:1 @ atlas — acme-billing — migrating billing off the nightly poll onto Stripe webhooks; signature check in, retry handler next.

Blocked on you (3) — all stopped mid-task waiting for an answer:

fern:1 tmux fern:1 @ atlas — acme-api — rate limiter is written; needs to know per API key or per client IP.
moss:1 tmux moss:1 @ nomad — acme-web — new welcome email drafted; wants permission to send a test to the staging mailing list.
wren:1 tmux wren:1 @ nomad — Postgres driver upgrade; needs pool size 10 or 25. Parked since Nov 2.
[…]
```

(It lists eight sessions after saying "Six windows": check its counts against
the list.)

## Who is on what

> Who's on the billing migration?

It searches the sessions' focus with `focus_search` and names the session, its
host and what it is doing.

```text
lynx:1 — tmux lynx:1 @ atlas, working in acme-billing.

[…] It's replacing the nightly invoice poll with webhook handlers; signature check is done and it's on the retry handler now. Status is busy, last active 20:29Z — nothing has stalled and nothing is waiting on you from that window.

Two adjacent things that are not the migration, so you don't conflate them:

orchid:1 tmux orchid:1 @ nomad was on "Fixing billing invoice rounding" earlier today (17:28Z) but has since moved to the 2.4 release notes.
[…]
```

## Hand a request to the session on it

> Send this to whoever's on the S3 uploader: please also log each retry with
> its attempt number and the delay before it.

It finds the session (`focus_search`) and calls `route_to_session` with your
request as `text` and `owner_asked: true`. It routes only when one session is
clearly on it; otherwise it asks you which, naming the candidates. muster then
pastes the request into that Claude Code window through its send queue once
the window is idle, with your own message quoted beside the chief's relay, and
the session answers with `route_reply`
([plugins.md](plugins.md#routing-your-own-request-to-a-session)).

```text
One match, no ambiguity. Routing it now.

It's on its way to heron:1 at tmux heron:1 @ nomad — the only session on the S3 uploader, and it was idle, so the request will land as soon as the window settles.

I passed it as your request: log each retry with its attempt number and the delay preceding it, rather than only the final outcome. heron:1's answer will come back to me in this chat and I'll relay it to you.
```

The session's answer comes back to the same chat. In this example the answer
was scripted (the fake window has no Claude Code in it to write one), posted
to `POST /api/routes/{id}/reply`, the route `muster route reply` uses; the
paste, the hand-back and the chief's relay are real:

```text
heron:1 answered — done. Each retry now logs its attempt number and the backoff delay ahead of it, in the form upload retry 2/5 in 400ms. It says the tests pass.

That's the session's own report; I have no repo here, so I'm passing it on rather than vouching for it. […]
```

Needs `MUSTER_SESSION_ROUTING=1`, and works only from a message you typed in
the chief's chat: a plugin message or a turn the chief started cannot route a
request.

## What needs me

> What needs me right now?

It reads `attention_list` (agents' questions, sessions waiting at a prompt,
reply drafts awaiting approval) and orders what it finds.

```text
Three things, all of them stuck questions sitting in windows:

**1. wren:1 @ nomad — tmux wren:1 @ nomad — "Which pool size do you want: 10 or 25?"
Waiting since Nov 2 — four days.** This is the one that actually needs you. […] Answer this first.

**2. moss:1 @ nomad — tmux moss:1 @ nomad — "The new welcome email is ready. Can I send a test to the staging mailing list?"**
Waiting ~20 min. This is an authorization, not a design call — sending to a mailing list is outward-facing, so it genuinely needs your yes. It can't proceed without you.

**3. fern:1 @ atlas — tmux fern:1 @ atlas — "Should the limit be per API key or per client IP?"**
[…]
```

It cannot answer them for you on the Compose install; answer in the session
or on the Asks page. It offered to route your answers back into each window,
which `route_to_session` can do as a typed request.

## What happened while I was away

> What got worked on in the last day? Give me a standup.

It calls `fleet_report` over the last day (ended sessions included) and the
board with `tasks_list`.

```text
Standup — 2026-11-05 20:00Z → 2026-11-06 20:15Z

6 windows had focus activity, across both hosts (atlas, nomad). No pull-request data: this muster doesn't record PR authorship, so I can't tell you what actually landed or what's sitting in review — treat the below as what was worked on, not what shipped.

acme-billing — lynx:1 @ atlas
Audited the payment retry logic (16:08), then moved onto the main event: migrating billing off the nightly poll onto Stripe webhooks (19:23). Signature verification is in; the retry handler is in progress. Still busy — the biggest single piece of work in the window.
[…]
Two windows had no activity in this window:

wren:1 @ nomad — Postgres driver upgrade. Focus unchanged since Nov 1; blocked on the pool-size question since Nov 2. Four days of nothing, and it's the longest-standing blocker in the fleet.
[…]
```

`fleet_report` covers at most 4 days per call. It lists the pull requests a
session created or pushed only where muster records them.

## The task board

> What's on the task board? What does task 2 say?

`tasks_list` covers your whole board for the chief; `task_get` reads one task
with its comments.

```text
Two tasks on muster's board, both open, both created minutes ago (~20:08Z) and not touched since — no status change, no comments, nothing has claimed them.

Task 2 — Rotate the staging TLS certificate

> The staging certificate expires on the 20th. Rotate it and update the runbook.

Directory: /work/acme-api
Status: open — never updated since creation (20:08:25Z)
[…]
```

## Route a task to the session on it

> Route task 1 to whoever is on billing.

`route_to_session` with `task_id`: muster pastes the task (id, status, title
and body) into the session, and its answer is added to the task as a comment
([plugins.md](plugins.md#routing-a-task-to-a-session)). A route that is not
answered in 30 minutes comes back to the chief.

```text
Routed. lynx:1 @ atlas (tmux lynx:1 @ atlas) — the only window in /work/acme-billing, so it's the one "on billing" by repo.

Task 1 went across as the task itself, not a paraphrase, so lynx gets the title, body and directory intact. It queues until the window is idle, so it won't cut into the migration mid-edit. The answer comes back as a comment on task 1 rather than into this chat […]
```

## What it cannot do on the Compose install

On the Compose install the chief runs with exactly eight tools pre-approved
(`sessions_overview`, `fleet_list`, `focus_search`, `fleet_report`,
`tasks_list`, `task_get`, `attention_list` and, with session routing on,
`route_to_session`) and **every other prompt is denied at once**
([chief.md](chief.md#on-the-docker-compose-install)). So it reports and
routes; it does not:

- **file a task.** Asked "File this idea as a task: add a dark mode to the
  invoice PDFs.", it answered:

  ```text
  I can't file it — I have no task-creation tool in this session.
  ```

  and then, wrongly, that it could still comment on tasks and change their
  status: `task_comment` and `task_set_status` are among the denied prompts. A
  model can misstate its own tools; the allowlist above is what holds. File the
  task yourself, or have it route the idea to a session.
- comment on a task, answer an ask for you, draft a reply, or start an agent
  (`agent_dispatch` needs Kubernetes).
- route to anything but a mirrored Claude Code window that can take a paste:
  not an offline or read-only host, not a Codex, opencode or shell window.

The chief is the admin's alone on this install (one per install), its
container's network is not confined, and on OpenRouter muster sets no budget:
put a credit limit on the key. [chief.md](chief.md) has the rest.
