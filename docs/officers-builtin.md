# Officers on the built-in backend

An **officer** is a Claude Code agent with standing instructions (its
*charter*) and a queue of work. muster wakes it when something lands in the
queue — a schedule, **Run now**, a plugin's message — runs one or more
*instances* of it, in a pod or as a Claude Code window on one of your
machines, and stops them when the queue is quiet. It asks you before
anything leaves.

On the **built-in backend** its charter, queue and leases live in muster's
own Postgres, so it needs nothing beyond muster. (An officer can instead live
in an external HQ: [officers.md](officers.md), also the reference for
instances, tools, pods and egress; the contract both backends implement is
[design/officers-first-class.md](design/officers-first-class.md).) It gets
the six queue tools (brief, items, claim, heartbeat, complete, release),
tiers, the investigation block, item cost, asks and escalations through
muster's Attention view, and its usage booked per day
([`officer_llm_usage`](officers.md#a-builtin-officers-usage-officer_llm_usage)).
HQ's ledgers, derived asks, gap tasks, task reads, PR context, ship gates and
release captain are not offered to it, nor mentioned in its first message.

**Start with [the Officers page](#the-officers-page).** Everything below
[How a built-in officer works](#how-a-built-in-officer-works) is reference;
[the last part](#reference-muster_officers-the-cli-and-the-admin-api-advanced)
covers declaring officers in `MUSTER_OFFICERS`, the `muster officer` CLI and
the admin API.

## The Officers page

**Officers** in the sidebar (under **More**; shown whenever muster has a
database) opens `/officers`: your officers, and each team's you belong to, one row each with its id, where it
runs, its status — **Live**, **Shadow**, **Paused**, or **Not running** with
the reason — and how many items and asks are open on it, how many decisions
wait on you (*2 waiting on you*) and how many of its [What it did](#an-officers-page)
entries are new since you last opened its page (*3 new*). An officer
`MUSTER_OFFICERS` declares is listed **read-only**.

At the top, **Needs you** gathers every officer of yours (and your teams')
that has a decision waiting on you, with how many, each a link to that
officer's own Needs-you section.

Under the list, **Where officers can run here** reads the server's own state:
pods (available only with the Kubernetes agent plane, else why not), each of
your mirrored machines (*ready*, or *not ready* and why: not connected,
cannot close windows, Claude Code missing or not logged in), and where a new
officer would run now — the same answer as `GET /api/officers/runtime-options`.
No machine listed means none is mirrored: run `muster-mirror` with
`-interactive -agent-spawn` on a machine you own.

**In the sidebar**, an **Officers** group lists every officer you can see —
yours and your teams' (each labelled with its team) — one row each, awake or
asleep, directly below your chief and the sessions waiting on you, and above
the rest of your sessions, your teams' groups, **Superseded** and **Idle**. A row says
whether the officer is running or asleep and how many items are queued for
it; with a `daily_budget_usd` ([Daily budget](#daily-budget-and-routine-batching)) it also shows the day's spend against it
(`$6.40 / $10`) and **held** once the cap has stopped it for the day (nothing
when it has no budget, and no figure when the spend could not be read), and
**N new** while some of its work finished since you last opened its page
(opening the page clears it; each person's own). An officer's routine work
pushes you nothing: this count is how you notice it. It
opens the running instance's chat, else the officer's page. An instance that
waits on you is listed with the other waiting sessions at the top (just above
the group), and its officer's row says **↑ waiting**; a team officer's waiting
instance is in its team's group below, and the row says **↓ waiting**. A host officer's row says neither running
nor asleep (its instances are mirrored windows) and opens its page.

### New officer

**New officer** (a team's manager also sees **New team officer**) is a
wizard, one step at a time:

1. **Start from** — a [template](#templates): *Blank officer* (the default),
   *Session minder*, *Marketing (you post by hand)* or *QA*. Next with
   another one chosen redraws the wizard with its starting values.
2. **Name and instructions** — its name, its id (made from the name,
   e.g. `release-notes:main`; you can type another) and its
   standing instructions. A team's manager also chooses whose officer it is:
   changing it redraws the rest for that owner, keeping what was typed.
3. **Where it runs** — *Let muster choose* (the default: a pod when this
   muster has the Kubernetes agent plane, else one of your machines that is
   ready; what it picks right now is shown), *In a pod*, or *On one of my
   machines* with a machine picker marking each *ready* or *not ready* and
   why. A choice this install cannot take is disabled with its reason. A
   repository is optional: without one, each instance starts in an empty
   scratch directory of its own.
4. **When it works** — *On a schedule* (every hour; every day, weekdays or
   Mondays at 09:00 UTC; or your own five-field cron, UTC), or *Only when I
   give it work* (Run now, or an item a plugin or the CLI queues). One of the
   two is required: an officer nothing wakes is refused. A schedule is saved
   as the officer's `pass` schedule, filing `scheduled.pass` items.
5. **What it may do** — it starts with its instructions, its queue and
   asking you. Each extra is opt-in: *draft posts and replies for your
   approval* ([outbound drafts](#outbound-drafts-through-a-bound-plugin)),
   *keep a record of the people it hears from*
   ([contacts](#people-and-outreach)), *mind my stopped sessions* (the
   [Session minder](#templates)), connectors it may ask for (you approve each
   request), public repositories it reads (pods only; read with no approval:
   [standing read](#letting-it-read-public-repositories-read_grant-standing)),
   and who else an escalation asks, picked from a list
   ([who that can be](#who-an-escalation-can-ask)). The step ends with what a
   machine officer can reach ([below](#-before-you-declare-one-on-a-real-deployment)).
6. **Review** — your answers, then **Start in Shadow** (the default) or
   **Live**, and **Create officer**.

The create goes through the same check as the API, so a refused answer is
shown beside its field and nothing is stored. Once created it runs with no
restart.

**Shadow** means its escalations are recorded and ask nobody; it still works
its queue, and a question it puts to you with `officer_ask_person` still
reaches you. Watch it, then **Switch to Live**: its escalations then ask you
(and whoever you picked) at once.

### Templates

A template only fills in the wizard: every value it sets — name,
instructions, schedule, permissions, where it runs (each starts at *Let
muster choose*) — is yours to change before you create it, and none adds a
repository, a connector or a host.

- **Blank officer** — nothing filled in.
- **Session minder** — minds your stopped sessions in place of the chief's
  stall nudges; it starts as *Only when I give it work*, and muster feeds it.
  Its permission *Mind my stopped sessions* (`"capabilities":
  ["session_minder"]` in its type's policy) is what muster acts on: while an
  officer of yours with it is switched on **and runs** (the roster resolved
  it somewhere), the chief's nudge sweep skips you entirely (no chief call,
  no nudge, and a stop's push no longer waits for the chief), and each stop
  the sweep would have judged — idle past `MUSTER_CHIEF_NUDGE_IDLE`, no
  background work, no turn error, no open ask — becomes **one**
  `session.stalled` item, subject `episode:<id>`, whose payload says where the
  session is and its state, never what it wrote. A stop is fed once. Pause or
  delete the officer and the nudges return on the next sweep. 🔴 It does
  **not** filter your notifications: every stop's push still reaches you; it
  asks about the stops that look like they need you and closes the rest. 🔴
  It needs session state (`MUSTER_SESSION_STATE=1`); without it, or with
  nowhere to run, it suppresses nothing. 🔴 A minder that runs **on one of
  your machines** (`"runtime": "host"`, or auto placed on a host) counts only
  while that machine's mirror is **connected**: while it is offline nothing is
  minding, so the chief's nudges come back on the next sweep and the minder is
  fed nothing until it reconnects. Each stop still has one judge: a stop
  the minder already acted on (claimed, waiting on you, done) is left to
  it; a stop still open in its queue is closed there (`no_action`) before
  the chief judges it; and a stop the chief judged is never fed to the
  minder when it comes back. Only one minder is used per owner,
  the first by id; a team officer is refused the permission (a team's pass
  runs no chief). It cannot type into a session; it asks you.
- **Marketing (you post by hand)** — drafts posts on a weekly schedule
  (Mondays 09:00 UTC) and sends each draft to you as a question; the venue
  is manual, so you post it yourself. It is given no outbound-drafts
  permission and its instructions forbid posting by any other means (on a
  machine they are what keeps it from posting — see the
  [warning](#-before-you-declare-one-on-a-real-deployment)). Write your
  venues into the last line of its instructions.
- **QA** — tests one recent change per weekday pass (09:00 UTC) in the
  repository you give it, reports with steps to reproduce, and escalates a
  defect; it never pushes or merges. Without a repository it asks you once
  which one to test.

### An officer's page

An officer's page is where you deal with it. From the top:

**Needs you** — everything from this officer that waits on you, each one
decided right there:

- **Its posts and replies** to approve: the exact text and every image and
  video, with **Approve and send** and **Reject** (with an optional reason,
  which the officer is told). It is the same card as on the Asks page's
  Drafts tab, posting to the same place: an approval binds the exact text
  and media you were shown (their SHA-256), so an approval of anything else
  is refused, and it needs a recent sign-in as there.
- **Its outreach batch** of the day: each follow or like with the person's
  profile (what their plugin reported — labelled **untrusted**), why the
  officer chose them, and a tick; **Approve ticked, reject the rest**. As on
  the Asks page, every entry you leave unticked is rejected "not selected".
- **Its questions** to you, answered with the same form as the Asks page's.
- **Refusals that wait on you**: a draft its plugin (or muster) refused that
  the officer has not redrafted — for example one refused because the
  venue's posting window was not open — with the plugin's own words for
  why. **Re-file** it in an hour, in 6 hours, at the next UTC midnight or at a
  time you choose (UTC) — or, when the plugin said when it could go (its
  refusal's [`retry_at`](plugins.md#retry_at-when-a-refused-draft-could-go)),
  at that time, which is then the default: the officer gets the same work back then, and not
  before, as your own item (a spent daily budget does not hold it back), and
  you approve the draft it files then. **Drop it**: the officer never
  redrafts it. A refusal you rejected yourself is not listed (that was your
  decision), nor one older than 7 days.
- **Its spend and run controls**: one bar, **$X of $Y today**, against its
  daily budget. It turns **amber from 80%** and **red once held** (the budget
  has stopped it, until the next UTC midnight). From 80% its **owner** also
  sees **Raise today's limit to $M** — one more day's worth of its
  `daily_budget_usd`, for the current UTC day only: it goes back at the next
  00:00 UTC by itself (the raise is stored against that day,
  `officer_cap_raises`, so nothing has to clean it up). The next wake pass
  reads it, and a held officer resumes the work the budget held back. At
  most 3 raises a day. Only the officer's own owner can raise it, from their
  own signed-in browser: a team's officer (whose owner is the team) is not
  raised here, and no token — an officer's, an agent's, a personal API
  token — can. Each raise is in the audit log (`officer.cap_raise`: who,
  which officer, the cap before and after, and until when). It raises the
  officer's daily budget, not an [LLM proxy](officers.md#the-llm-proxy)
  pool's `llm.daily_budget_usd`. **Spending details** under the bar holds
  the limit, when it resets, a raise, and the last 7 days' requests and
  estimated cost. Then **Run now** and **Pause** / **Resume**. Run now is how
  you wake a held officer: work you give it yourself gets past the cap.

The page is laid out for a phone first: a question's answer box has a line
of its own, the card's full width, with the microphone, **Answer** and
**Dismiss** on the row under it (the Asks page draws the same form), and
every button is at least 44px tall.

**Instructions** — the officer's charter (its type's: when several officers
share the type, the section says how many, and a change reaches all of
them), its version (**Version N**), and which version the officer last read.
An officer still working from an older version says so: *It is still working
from version M*. **Read or change them** opens the editor, with two buttons:

- **Save** — the instructions are stored; the version goes up only when the
  text changed. A running officer switches to them the next time it picks up
  or finishes work (its `officer_claim` and `officer_complete` are refused
  until it reads the new version, as below in
  [Charter changes reach running instances](#charter-changes-reach-running-instances)).
- **Save and refresh** — the same save, then every running instance is
  stopped and started again in a **new conversation** that begins by reading
  the new instructions and its brief. muster forgets which version each
  instance read (`officer_defs.charter_briefed`), and an instance's first turn
  after a start, with nothing recorded, asks its pod's `ccd` for a new
  conversation (the `X-Muster-CC-New-Conversation` header: the CLI's
  `/clear`; the old transcript is kept). Once it reads its charter again,
  later starts resume that new conversation. An officer that is not running
  is not started; its next start is a new conversation. A machine officer's
  start is always a new window and conversation. The restart carries on if you close the page
  mid-way. If a copy could not be stopped or started again, the page says
  how many of each (the instructions are saved either way).

Instructions are at most 32,768 characters (the editor stops there). A
refused or failed save shows your text again with the reason, never the
stored instructions.

Each manager's save of a team officer's instructions is in the team's audit
(`officer.charter.put`).

#### Show session

Each item in **Needs you** — a post or reply to approve, an outreach batch, a
question, a refusal — has a **Show session** button. It opens the conversation
of the officer's copy that made it, scrolled to the moment it did, with a
marker: *Below: the turn where it raised this*. A refusal opens the session
that wrote the draft it refused.

muster records the moment when the item is made (migration `0185`,
`origin_*`), from the tool call itself. It notes which of the officer's
running copies made the call, the conversation that copy was in, and the
newest message of that conversation at the call. An officer's tool calls are
not messages of their own, so the marker sits at the start of the turn that
made the item. The record comes from the officer's own sign-in, never from
anything the officer wrote in the call. An item made before this, or one with
no conversation recorded, opens that copy's conversation as a whole. The
button only ever opens one of the item's own officer's copies, for the people
who can see that officer; the database refuses a record naming anyone else's.
An escalation sent to someone in `escalate_to` carries no such link: only the
owner's own copy of it does.

Every decision there is made only by your own signed-in browser: an
officer's instance (or its machine's window) holds a token, never your
sign-in, so nothing the officer does can approve its own work.

**Chat with it** — talk to the officer like any agent: ask what it is
doing, what waits on you, or to get something ready for you. It answers in
the chat (reload to see a reply that is still coming). Each person has
their own chat with each officer (it follows the officer to whichever of its
instances is running); a team's managers never read each
other's. Details in [Chat with it](#chat-with-it).

**What it did** — its finished work, newest first, in plain words: each
item's kind and how it ended, with the officer's own summary; what it sent
(*Posted on X*, the text, and **See the post** where muster can build the
address — an X post's id); what your [chief](chief.md#your-officers) did to
it (*Your chief paused it*, *Your chief sent it a message*, with the
message); and today's spend. Work finished since your last
visit is marked **new**. The ids muster keeps for an entry (item, draft, the
post's reference) are behind its ⓘ.

Below those: where it runs and why it is not running when it is not, its
instances (a machine instance's window opens its mirrored view), its raw
queue and its schedules (add and delete them there). The page does not
repeat the machine-user warning (the wizard shows it where you choose a
machine), and has no Shadow / Live button: Shadow is set on Edit, through the
admin API or in `MUSTER_OFFICERS`. It shows no "last pass": the wake loop's memory of
one is per replica and forgotten by a restart, so it is not drawn until a
shared record of passes exists. Its other actions:

- **Run now** — puts one `manual.run` item in its queue, and it wakes for it
  within seconds; a second tap while it waits adds none.
- **Pause** / **Resume** — paused, its instances stop and none starts,
  whatever its queue holds.
- **Edit settings** — the wizard's steps 2, 3, 5 and 6 on one form (schedules are on
  the page). It keeps a machine officer on its machine when that machine is
  not connected right now (drawn selected, marked offline).
- **Delete** — type its id to confirm. It goes with its queue, schedules and
  people, and its instances are stopped.

Each writes only what it changes: a mode switch writes the mode, a pause the
switch, and an Edit neither (the mode only when you changed it on the form),
so none undoes another made while a page was open. An officer
`MUSTER_OFFICERS` declares is read-only here — Edit, the mode, schedules and
Delete are changed in the environment with a restart — but its Needs you,
Run now and Pause / Resume work as for any other: they write its queue and
its switch (its def's `enabled`, the one `muster officer def put --enabled`
sets), never its declaration. A team's viewers and operators read a team
officer's page — its machine instances' windows and its Needs you
included; only a manager decides, re-files, runs or changes it, and Postgres
refuses anyone else again. A manager's writes to a team officer (create,
edit, mode, pause, Run now, schedules, re-file, drop, delete) are in the
team's audit.

### Chat with it

The **Chat with it** section of an officer's page is there for its owner
(and a manager of its team) when it runs in pods. Your message goes to one
of its instances as a turn of **your** chat: a Claude Code session of its
own beside the instance's queue work (the same kind of lane an HQ chat runs
in), so the chat never waits for an item and an item never waits for the
chat. When none of its instances is running, muster starts one for the
message. A turn that cannot be taken yet (its instance starting, its chat
busy) is retried for up to 5 minutes.

In the chat the officer reads and acts on **its own state** with four tools
(nothing else of anyone's — each reads only its own officer's rows):

| tool | what it does |
|---|---|
| `officer_my_state` `{section?}` | its open questions to you, its pending drafts (exact text and media) and outreach batch, the refusals waiting on you, today's spend against its budget, its schedules |
| `officer_confirm_card` `{draft_id}` or `{batch_id}` | asks muster to show you a **confirm card** for one of its pending drafts or its open batch |
| `officer_refusal_refile` `{item_id, when?, at?}` | asks you, with a confirm card, to **Re-file** a refusal (by default at the time its plugin said it can go) |
| `officer_refusal_drop` `{item_id}` | asks you, with a confirm card, to **Drop** a refusal |

The last three work only inside your chat with **that** officer, and they
only **ask**: each puts a card in your chat, and nothing happens until you
tap it. (The chat's session holds a secret muster gave it, but every process
in the officer's pod can read that secret, so muster never lets a call that
holds it act for you.) Your tap on a Re-file or Drop card makes your page's
own choice, in the team's audit for a team officer — except that a re-file
asked for in a chat comes back as the officer's **own** work, so a spent
daily budget holds it back like any of its work. Its queue work is refused
in the chat, as in any chat session.

🔴 **A confirm card is yours to decide, never the officer's.** It shows the
exact text and every image and video (or each follow and like of the
batch), with **Approve and send** and **Reject**. Approving needs a recent
sign-in, as every approval does (and so does rejecting a batch card, which
rejects every follow and like on it), and decides **exactly what the card
showed**: the card keeps the SHA-256 of the text and of each medium (a batch
card, the batch's own SHA-256 and its entries), and the approval is made on
those — so if the draft or the batch changed after the card was drawn,
nothing is decided and the card says to ask for a new one. No officer tool
approves anything, and every door that decides a draft, a batch or a card
refuses an officer's own credential or tool call.

**A chat is never held by the budget.** A held officer, or one past its
daily budget, still answers. Its chat's model spend is counted toward that
budget like any of its work (`officer_llm_usage`), and each turn books it
as it ends.

**Your chief can chat with it too.** Its `chief_officer_message` sends a
turn of **your** chat, drawn here as *Your chief*; its
`chief_officer_confirm_card` puts a confirm card here for you to decide —
the chief never approves anything ([chief.md](chief.md#your-officers)).

⚠ Limits: an officer that runs in a window on one of your machines
(`"runtime": "host"`) is chatted with in that window, not here; a reply
still coming shows as "Answering…" only on the replica that took the
message.

## ⚠ Before you declare one on a real deployment

Every outward action an officer takes **through muster** passes an Attention
approval that shows the exact content: the officer files an **outbound
draft** (`officer_post_draft`, only with that permission), its owner approves
the exact text and media, and a plugin on the owner's own machine posts it
([Outbound drafts](#outbound-drafts-through-a-bound-plugin)). This holds for
pod and machine officers alike; on a machine, owner-authority decisions made
through that machine's MCP door also need your paired phone
([the step-up gate](#officers-in-the-compose-stack-host-sessions)). Deciding
in muster's own web UI (Reply drafts, Connectors, Attention) is unchanged.

- **In a pod** that approval is also the only way out: its other ways out of
  its pod are muster's tools, the connectors its owner approves one request
  at a time, and any PUBLIC repositories it is declared to read
  ([standing read](#letting-it-read-public-repositories-read_grant-standing)).
  Do not approve a connector that can write to the outside world for one,
  and give it no `repo` with write access.
- 🔴 **On one of your machines** the approval is the **sanctioned path, not
  an enforcement boundary.** The instance runs as the machine's user, with
  that user's credentials and network: a marketing officer there COULD post
  directly (a browser session, a CLI token, an API key in that user's files),
  and its `repo` is a pushable checkout with that user's git credentials. Its
  charter is what keeps it on the sanctioned path
  ([the security model](#officers-in-the-compose-stack-host-sessions)).

The empty roster (no officer declared) exposes nothing.

## How a built-in officer works

What follows holds for every built-in officer, whether the Officers page,
the CLI or `MUSTER_OFFICERS` declared it.

### Asks and escalations, through Attention

- **An instance's question** — `officer_ask_person{item_id, to: "owner",
  question, options?, deadline_seconds?}` — becomes an Attention ask to the
  officer's owner (the Attention view and their phone), raised by muster
  itself, labelled `officer <id>`. Asking ends the instance's lease: the item
  is `waiting`. When the owner answers (or dismisses it), muster's sweep puts
  the answer on the item's `asks[]` and re-opens it once no ask on it is open;
  the next claim reads the answer. An ask that **expires** unanswered (24 h by
  default) escalates the item to the owner. `to` is not a routing input here
  and images are not served.
- **An escalation** — `officer_complete{outcome: "escalated"}`, an expired ask,
  or the wall-time cap — writes, in the same transaction, one entry per
  recipient (the owner and each `--escalate-to` user). The sweep raises each
  one still inside the officer's reach when it sends
  ([Who an escalation can ask](#who-an-escalation-can-ask))
  as an Attention **ask** — the officer asking a human to act — exactly once,
  even with two replicas. **An answer flows back to the officer**: the item
  re-opens as a judgement item with the answer on its `asks[]`, the next
  instance claims it, and the other recipients' asks are withdrawn.
  Dismissing it (or letting it expire) leaves the item escalated. A
  **Shadow** officer records its escalations and asks nobody; its
  `officer_ask_person` questions are raised either way.

#### What the owner is shown

The ask's question is the officer's own words, under muster's line naming the
officer and the item, and the card labels it **The officer's note**. For an
item a plugin message became (`plugin.*`), muster attaches the item's context
itself, read from the item and the stored message when it raises the ask —
never from the officer's prose (`attention_asks.item_context`, migration
0129): the officer, item id, kind and subject, and the source message in its own
box, marked untrusted: plugin and message id, venue and thread, the sender
(UNVERIFIED unless the plugin verified it), the body (sanitised; cut at 1500
characters or 30 lines with a visible "Cut here"), and its `url` as text,
never a link. A message muster cannot read is shown as unread. Only the
owner sees the message: an escalation's other recipients get the officer and
item line (row-level security). `attention_list` returns it as the item's
`context`; the push stays `Officer <id> · item <n> · <the note's first
line>` — no outside sender's words on a lock screen.

#### Who an escalation can ask

An escalation asks only **people who can already see the officer**, and
the server enforces it on every door that writes `escalate_to` — the wizard,
Edit, `POST`/`PUT /api/officers/declared`, `PUT /api/officers/defs/{officer}`
and the team officers page:

- a **team** officer: the team itself (one ask any member answers) or its
  members — not another team, even one its manager operates, and not the
  members of the manager's other teams;
- **your own** officer: you (always asked) or the people on the teams you
  are in.

The picker offers exactly that set. Any other id is refused with one
answer, whether it names someone or no one. An id stored before this rule is
**not asked**; the officer's next Edit shows it ticked and marked, and
refuses the save until it is unticked.

The same reach is checked again **when an escalation is sent**, read at that
moment: the sweep raises an escalation only to a recipient inside the
officer's reach *now*. A recipient outside it — an id stored before the rule,
or a member who has left the team since the officer was saved — is dropped:
their entry on the item is given up (never retried), and muster logs one
line per dropped recipient naming the officer, the item and the user id
(never the item's text):

```
officers: <officer>: item <n>: the escalation to user <id> is dropped: that user cannot see the officer
```

The owner (the team, for a team officer) is always in the reach, so an
officer whose every `--escalate-to` id is out of reach still asks its owner.
If the reach cannot be read (a database error), nothing is raised to that
recipient on that sweep and the next sweep tries again. This holds for every
built-in officer, however declared and wherever it runs: all of them escalate
through the one sweep. (An HQ-backed officer has no `escalate_to` in muster:
HQ notifies its owner.)

### Schedules (cron, UTC)

The wizard's schedule and the ones added on an officer's page are
`scheduled.pass` schedules; the CLI names its own kind and subject:

```sh
muster officer schedule put marketing:example weekly-digest \
    --cron "0 9 * * 1" --kind digest.weekly --subject weekly --priority normal
muster officer schedule ls marketing:example
muster officer schedule delete marketing:example weekly-digest
```

Five fields (minute hour day-of-month month day-of-week), evaluated in
**UTC**; `*`, numbers, ranges `a-b`, lists and steps `*/n`; day-of-week 0–6 (0
or 7 Sunday); when both day fields are restricted a day matches if either
does. No names (`MON`) or `@daily`. muster checks every 30 seconds:

- **once**: a fire is a compare-and-set on the schedule's `next_fire_at`, so of
  two replicas (or a restarted muster) exactly one enqueues, in the same
  transaction as the advance;
- **missed fires collapse to one**: after downtime a schedule fires once, then
  at its next future time;
- `--skip-if-live` (default): a fire while an item of that kind and subject
  is still live enqueues nothing; `--skip-if-live=false` makes every fire its
  own item (`<subject>@<fire minute>`);
- a switched-off officer is not fed, and its schedules still advance (no
  backlog on switching it back on).

### Outbound drafts through a bound plugin

1. Grant the capability: tick *Draft posts and replies for your approval*
   in the wizard or Edit, or
   `muster officer type put marketing --name "Marketing" --charter-file ./charter.md --policy '{"capabilities":["outbound_drafts"]}'`.
2. Mint (or [pair](plugins.md#pairing-a-plugin-without-handling-its-token)) a
   plugin with `messages` and `draft_checks`, and bind it:
   `deliver_to = officer:marketing:example`
   ([plugins.md](plugins.md#a-plugin-bound-to-an-officer)). The binding is
   refused unless the officer is defined for the same owner.
3. The plugin's messages (`kind`: `plan`, `reply`, `snapshot`, `note`) arrive
   as `plugin.<kind>` items with the message fenced as untrusted; an
   `engagement` records a person and becomes their item of the day
   ([People and outreach](#people-and-outreach)).
4. The officer files `officer_post_draft{plugin, venue, target, body, media?,
   item_id?}`. The plugin checks the draft and uploads each medium's bytes;
   the owner sees the exact text, images and videos on the Drafts tab,
   and approval binds the body and every medium's SHA-256. The plugin claims
   it, re-hashes its files, posts, reads the post back and reports the result.
5. With `item_id`, the item **waits on the draft**: posted, muster completes
   it; refused, rejected (with an optional reason), expired or withdrawn,
   muster queues a `draft.refused` item whose `detail` says why; unconfirmed
   (it may have been posted), the item is escalated to the owner. The rules
   in full are the tool's `item_id` row in
   [officers.md](officers.md#outbound-drafts-officer_post_draft) and
   [plugins.md](plugins.md#the-result). Put what the officer should do with a
   `draft.refused` item (read `detail`, redraft with its own `item_id`) in its
   charter.

### People and outreach

A plugin bound to the officer can report **engagement** with its owner's
posts — replies (nested ones too), quotes, mentions, likes, reposts, new
followers, inbound DMs and blocks — one event per message
([plugins.md](plugins.md#engagement-kind-engagement)). muster keeps the
**people** in its own Postgres, scoped to the officer (owner and officer),
never in a repository. A [team](auth.md#teams) officer's people are its team's: its operators and managers read them, its managers write them, nobody else:

- **One row per person** per venue and handle (`officer_contacts`, migration
  0132): what the plugin said of them (display name, profile URL, a short bio —
  the person's own words, untrusted), first seen, last engaged, last
  contacted, the marks **do-not-contact**, **bot** and **hostile**, and the
  officer's labels and notes.
- **Their history** (`officer_contact_events`): each engagement (once per
  kind and ref) with an excerpt of what they said (at most 500 characters),
  and each outreach that was sent to them.
- **One item per person per day.** The officer gets a `plugin.engagement`
  item (subject `contact:<id>:<UTC day>`) whose payload names the person and
  the day's events, with the person and their words as a fenced untrusted
  block. A second event of theirs that day joins the open item instead of
  making another. 🔴 The stored item names the person by id only (a
  team's viewers read its items, only its operators its people); the person
  and their words are rendered into the officer's copy when it claims the
  item. An event while that day's item is being worked (claimed, or
  waiting on an ask or a draft) opens one more item (`…#<event id>`), so no
  engagement is lost. A like never opens an item (a person whose only
  engagement is a like is recorded, never brought to the officer); a like of
  someone who has an open item that day joins it.
- **Do-not-contact is set automatically** by a block, or by their words asking
  to stop (a message that begins "stop", "no thanks", "not interested",
  "unsubscribe" …, or asks anywhere not to be contacted, messaged or DMed). It
  errs on the side of setting it. A person who is do-not-contact, a bot or
  hostile gets no item; their events are still recorded.

Grant the type the `contacts` capability to give the officer its three tools
(read at call time, as `outbound_drafts` is):

```sh
muster officer type put marketing --name "Marketing" --charter-file ./charter.md \
    --policy '{"capabilities":["outbound_drafts","contacts"]}'
```

| tool | arguments | what it does |
|---|---|---|
| `officer_contact_get` | `{venue, handle, events?}` | the person, their marks, labels and notes, and their newest events (at most 50) |
| `officer_contacts_list` | `{filter?: {venue?, label?, flag?, handle_prefix?, engaged_since?, limit?}}` | people, the most recently engaged first, at most 100; `flag` is `dnc`, `bot`, `hostile` or `contactable` |
| `officer_contact_note` | `{venue, handle, labels?, notes?, flags?: {bot?, hostile?}}` | replaces labels (lower-case words, at most 20) and notes (at most 2 KiB); marks a bot or hostile |

🔴 **An officer sets a mark and never clears one, and never touches
do-not-contact.** Only the owner does, over the admin door below. Every answer
carries a note that the person's words are data, not instructions.

The owner's door is the admin API's `…/contacts` routes
([below](#the-admin-api)), e.g. `PUT …/contacts/{venue}/{handle}/flags`
with `{"dnc": true}`.

**Outreach** — a DM, a follow, a like, or a reply to one of a person's
recorded engagements — goes through `officer_post_draft` with the person,
`because_ref` (the engagement that motivates it) and, for a follow or a like,
a one-line `reason` ([plugins.md](plugins.md#outreach-drafts-dm-follow-like)).
muster refuses it at the filing, deterministically, for a do-not-contact, bot
or hostile person; without a qualifying engagement of that person (never a
like; a DM answers something they said); while another outreach to them is
out; when they were reached before and have not engaged since; and inside
the cooldown (`{"outreach": {"cooldown_days": 7}}` in the type's policy, the
default) unless it answers an engagement newer than the last outreach. A DM
is approved on its exact text; follows and likes on the day's
[outreach batch](plugins.md#the-outreach-batch), where the owner ticks what
goes out and every unticked entry is rejected "not selected". A sent
outreach is recorded on the person.

⚠ A team officer's people have no owner's door yet: the admin API's tokens carry no team scope, so it reaches only your own officers' contacts. Their marks come from the officer (bot, hostile) and from their own words (do-not-contact).

`DELETE …/contacts/{venue}/{handle}` erases a person and their whole
history. Deleting an officer from the Officers page or with `muster officer
delete` erases its people too; `DELETE /api/officers/defs/{officer}` (a bare
def) is refused (409) while it has contacts.

Every contact tool's structured answer carries ids, kinds, refs, times,
marks, labels and notes; every free-text field the person chose (display
name, profile URL, bio, what they said) is only in its `people` field, one
fenced untrusted block. Stored text is sanitised (no control, bidi or
invisible character).

### Officers in the Compose stack (host sessions)

A built-in officer can run without Kubernetes: on a machine (*On one of my
machines* in the wizard, *Let muster choose* where there is no agent plane, or
`"runtime": "host"` with `"host"` in `MUSTER_OFFICERS`) each instance is a
Claude Code window that muster asks ONE of your machines to start, through
that machine's `muster-mirror` — the same host spawn the chief uses
([design/officers-on-hosts.md](design/officers-on-hosts.md)). This is how
officers run in the Compose stack, with the Compose chief
(`MUSTER_CHIEF_RUNTIME=compose`) or without it: the chief runs beside host
officers, and refuses only an officer that runs in pods, naming it.

On that machine `muster-mirror` must run with `-interactive -agent-spawn`
(and the officer's `repo`, if it names one, on `-agent-spawn-repos` with one
clone of it under `-agent-spawn-roots`), and be current enough to close
windows and to accept an officer's first message (a muster-mirror from this
version). Its
user-scope `mcpServers.muster` entry ([attention.md](attention.md#mirrored-claude-code-sessions))
is how an instance reaches its officer tools.

- **No repository.** An officer with nothing to do with a repository (a
  session minder, a marketing officer) declares none: each instance then starts
  in a private scratch directory of its own on the host,
  `<-agent-spawn-dir>/scratch/<n>`, removed once its window is gone. Any host
  with `-agent-spawn` does this; nothing more to set, and no allow-listed
  repository is needed.
- **Claude Code on the host.** muster-mirror runs `claude` from the tmux
  server's global PATH, which may not reach `~/.local/bin`: set
  `-claude-path` (`MUSTER_MIRROR_CLAUDE_PATH`) to `command -v claude`'s answer,
  and log in to Claude Code once as that user, or a window stops at its login
  screen. The host reports its allow-list, scratch support and claude's state
  whenever they change; `auto` never picks a host whose claude is missing or
  not logged in, the Officers page and `GET /api/officers/runtime-options`
  show each host's state, and `muster doctor`'s `officers` check names what is
  in each officer's way, with the fix.

What happens:

- **Wake.** An item in its queue makes the wake loop start instance 1: muster
  records the spawn and the host opens a window in its `agents` tmux session,
  in a fresh worktree of `repo` (or its scratch directory), running Claude
  Code with the officer's first message (behind the preface `muster: your owner's officer runtime started
  you`), in normal permission mode unless the host opted in to
  `-agent-spawn-skip-permissions`.
- **Its agent row.** Each instance has an agent row of its own name that
  nothing starts or stops (no provisioner, no reaper; it is not shown in the
  agent list — the instance is its mirrored window). Its outbound drafts and
  connector grants are filed against it. It is created by the first start and
  kept when the window closes, so a draft or a grant outlives the window.
- **Tools.** The instance is offered the six queue tools,
  `officer_ask_person`, `officer_post_draft` (with the `outbound_drafts`
  grant), and — when it declares `connectors` and the agent connector store
  is configured — `connectors_list`, `connector_request`, `connector_status`
  and `postgres_query`, through the mirror door (without the store the
  connectors feature reports MISCONFIGURED, naming the officer). Every call is
  bound to the Claude session and window muster recorded for that spawn and
  runs as the instance's agent row; any other session gets a tool error. A
  bound instance gets ONLY these (and `attention_ask`). Every mirrored
  session's tool list shows them while a host officer is declared (a list
  carries no session); only the instance can use them.
- **The step-up gate.** The instance could still name ANOTHER of your windows
  to the machine's MCP door (the header is the caller's to set) and arrive as
  that session. So on this machine every owner-authority tool
  (`plugin_draft_decide`, `plugin_outbound_batch_decide`,
  `connector_request_decide`, `attention_answer`, `agent_destroy`,
  `dispatch_approve`, `route_reply`, `plugin_message_route`,
  `plugin_request_decide`) files a confirmation card instead of deciding
  (`chief_restart` does not: it runs at once, on every machine; see
  [mirroring.md](mirroring.md#chief_restart-is-not-gated)); it
  runs once, exactly as asked, when you answer **Confirm** on your paired
  phone (a signed action: a fresh step-up, single use, bound to that one
  decision); a browser or MCP answer of the card is refused. Without a paired
  phone, decide in muster's web UI.
  The gate holds while a host officer is declared on the machine, and while
  any officer window muster started there is still listed (a paused, deleted
  or moved officer's window finishing its turn). A window is gone for good
  once two listings in a row have not listed it: a window id the host later
  reissues to another window does not gate the machine again. If muster
  cannot read its record of officer windows, the gate holds.
- **Connectors.** Each `connector_request` is decided by its owner, per
  request, never from a shared approval — as for a pod. Not `github`: it works
  through an HTTP proxy with an agent token no host session holds; the
  instance reads its repository from its worktree.
- **More work while it is up.** muster pastes the re-prompt into its window,
  only while the host reports it idle.
- **Scale to zero.** After `idle_scale_to_zero_after` with nothing to do,
  muster closes the window — and asks only while the host reports it **idle**
  in the recorded conversation (an instance still finishing its last turn is
  left for the next idle pass): the host types `/exit` at the empty idle
  prompt and closes the window if Claude Code has not exited 30 s later; a
  window that is NOT at an empty idle prompt when the close arrives (a turn
  started, a person being asked, text in the box) is closed **at once**, with
  no grace. The host then removes the worktree if it is clean. If the
  recorded window starts another conversation (a `/clear` in it), muster
  follows it at its next poll. muster
  closes only the window it recorded for that instance — the one exception to
  "muster never closes a host-spawned window", and declaring `"runtime":
  "host"` is the consent to it ([decisions.md](decisions.md#a-host-officers-window-is-the-one-window-muster-closes-by-itself)).
- **Moving it.** A database-declared officer's old instances are removed by
  the roster load that moves it (an Edit onto another runtime or machine, or
  an `auto` move). Changing a `MUSTER_OFFICERS` officer's `host`, or moving it
  between pods and a host, leaves its instance rows on the old side; its next
  start is refused naming the command that clears each: `muster agent rm
  <instance>` (a host row needs no provisioner; drafts and grants on it go
  with it).
- **Host offline.** The officer changes nothing until the host is back: its
  items wait.
- **An instance that never claims.** An instance that has not claimed its
  item 10 minutes after it started — its window listed, work open — raises ONE
  notice on the Attention view, naming the likely cause the host reported
  ("Claude Code is not logged in on `<host>`", or a window where Claude Code
  started no conversation: its first-run, login or folder-trust screen) and the
  window to open. It is withdrawn the moment the instance claims (or its window
  goes), and never raised twice for one start.
- **Why it started.** Every start's log line says why: `first start`, `woken
  for new work after muster scaled it to zero (idle 30m0s)`, or `restart:` and
  what ended the last one (its window gone, its last start refused or never
  answered).

Refused at boot, each named: `backend` other than `builtin`, no `host`,
and `tiers`, `model`, `image`, `cc_account`, `cc_failover`, `read_repos` (so a
[standing read](#letting-it-read-public-repositories-read_grant-standing) too:
it is served through muster's proxy with the agent token a host session never
holds), `llm`, `kube` (plus what every builtin officer refuses). `connectors` and
[outbound drafts](#outbound-drafts-through-a-bound-plugin) work on a host.

🔴 **The security model is the machine's, not a pod's.** An instance runs as
the user `muster-mirror` runs as: that user's files, git and SSH credentials,
network and other tmux windows; no egress lock and no read-only GitHub token
(see the [warning](#-before-you-declare-one-on-a-real-deployment) — the first
message telling it not to push or post is prose, not a gate). The host's own
allow-list, its ceiling on
officer spawns (`-agent-spawn-officer-rate`, default 20 per 10 minutes and 400
a day, all officers together; the chief's own `-agent-spawn-rate` no longer
counts them) and audit log still bound what muster can make it do. Under that
ceiling muster limits each officer by its own budget, sized from its idle
window, poll and shortest schedule (a 10-minute schedule gets 150 starts a day;
the defaults 53), and refuses a start past it, saying so — so one officer's
restart loop cannot spend the others' share. Declare one only on a machine whose
user you would let run that charter unattended. The full list is in the
[design](design/officers-on-hosts.md#security-what-changes-against-a-pod).

**Check:** the boot log's `officers: <id> on: runtime host on <machine> …` line,
and the ON line's `S on hosts` count.

#### A team's host officer (team hosts)

A [team](auth.md#teams) may own a `"runtime": "host"` officer (team sharing
2b.5). Its `host` is a **team host**: a `muster-mirror` connected with the
team's own mirror token ([mirroring.md](mirroring.md#team-hosts)), set up as
above (`-interactive -agent-spawn`, and the officer's `repo`, if it names
one, on `-agent-spawn-repos`).

- **Only the team's host.** It spawns, closes and lists windows only on the
  team's host of that name — never a member's personal host of the same
  name, even when that is the only one online (the officer waits, as for any
  offline host). muster does not check the host at boot.
- **Its rows are the team's.** Its agent and spawn rows are owned by the
  team; every member, viewer and up, sees them on `/officers/spawns`. Its
  drafts wait for a manager's approval with a fresh sign-in, as every team
  officer's do.
- **Nothing is decided from the machine.** A team host's sessions are offered
  no owner-authority tool, so the step-up gate never files a card for a team
  (a team has no phone). An instance gets its officer tools and
  `attention_ask`; its asks are the team's.
- 🔴 **No `connectors`.** A team's host officer that declares `connectors` is
  refused: at boot for `MUSTER_OFFICERS`, and at the roster load for a
  database declaration (including a `"runtime": "auto"` one placed on a
  host), which then says why it does not run. A
  team's connector requests would have no one to decide them (decision G3 in
  [design/team-sharing.md](design/team-sharing.md#g-operator-decided-2026-11-14)).
  Run it in pods, or without connectors.
- It can do whatever the team host's Unix user can, which every operator
  there can already do.

### Declared in the database (no restart)

Every officer the Officers page creates is declared in muster's database:
one record holds its type and charter, its def, and where it runs. The CLI
(`muster officer create|pause|resume|delete|ls`) and the API below write the
same record. Each write runs at once, with no restart: it swaps the live
roster, and every replica re-reads it on its wake loop's refresh (15 s).

```bash
muster officer create scout:main --charter-file charter.md --runtime auto
muster officer pause scout:main     # its instances stop; none starts
muster officer resume scout:main
muster officer delete scout:main    # its queue, schedules and instances go too
```

- It runs on the **built-in backend**, untiered, and holds no secret: no
  `tiers`, `ship`, `ci_logs`, `transcript`, `llm`, `kube`, `image` or token.
  Those stay `MUSTER_OFFICERS`-only.
- `runtime` is `pod`, `host` or `auto` (the default; the wizard's three
  choices). When `auto` finds neither a pod nor a qualifying host it does not
  run, and its `reason` says why. Its `read_repos` are always a
  [standing public read](#letting-it-read-public-repositories-read_grant-standing).
- 🔴 **An `auto` officer on a host stays there.** Its placement is stored in
  the database (the host, and when that host last qualified), so a restart
  and every replica read the same answer. It keeps that host while the host
  qualifies, and for **10 minutes** after it stops qualifying (disconnected,
  or an older muster-mirror): a blip does not move it. Its passes meanwhile
  see the host offline and change nothing. Only past those 10 minutes, and
  only to another host that qualifies, does it move — a compare-and-swap on
  the stored placement, so of two replicas deciding a move one wins and the
  other takes its answer — and the load that moves it removes the old host's
  instance agents. With nowhere else to go it stays. An officer placed before
  the placement was stored is placed where its instance agents run.
- ⚠ **A move can cut a window mid-turn.** The old host's window is asked to
  close only when idle, as every stop is, so a window still working its last
  turn is left open; but its instance agent row is removed at once, so that
  window's officer tools stop answering for the rest of its turn. Its machine
  stays step-up gated while the host lists the window. A move happens only
  after the host has failed to qualify for 10 minutes (usually: it is
  disconnected, so the window cannot be reached anyway) or when you replace
  the officer onto another runtime or host.
- A `host` officer must name one of **your** mirrored hosts (by the mirror
  token's host name); any other is refused on the `host` field.
- It is validated by the same checks as a `MUSTER_OFFICERS` entry, and a
  refusal names each field (`{"error", "fields": [{field, error}]}`).
- Its instance names are built on a **server-allocated slug**
  (`instance_slug`, e.g. `scout-main-k3x9q2`), unique across owners, so two
  people may each have a `scout:main`. A slug never passes from one owner to
  another, is never handed out while its instance agents exist, and says
  nothing about who else declared the id.
- 🔴 **Its type is its own**, keyed by its slug and written and deleted with
  it: it never touches the type its id's prefix names (`scout` for
  `scout:main`), which an env officer may run on.
- 🔴 **An env officer wins.** An officer `MUSTER_OFFICERS` declares for you
  is listed `managed_by: env` and is read-only here (409); a database row of
  the same id does not run beside it.
- A **replace** (`PUT`) is the whole record, and must state `mode` and
  `paused`: a body that leaves either out is refused on that field, so a
  shadow officer is never flipped live, nor a paused one resumed, by
  omission. Every other field left out takes its default, as on a create.
- A team's officers run from the database as a person's do (a row a team
  owns is loaded; a team host officer runs on the team's host, and one that
  declares `connectors` is refused: [A team's host officer](#a-teams-host-officer-team-hosts)),
  and a team's MANAGER writes them on the [Officers page](#the-officers-page)
  (the JSON API's tokens carry no team, so it stays the caller's own).

| method | path | body / query |
|---|---|---|
| GET / POST | `/api/officers/declared` | POST `{id, name?, charter?, policy?, runtime?, host?, repo?, connectors?, read_repos?, max_instances?, idle_scale_to_zero_after?, poll_interval?, model?, mode?, escalate_to?, paused?}` |
| GET / PUT / DELETE | `/api/officers/declared/{officer}` | PUT: the whole record, as POST, with `mode` and `paused` required (its id and slug never change) |
| POST | `/api/officers/declared/{officer}/pause`, `…/resume` | |
| GET | `/api/officers/runtime-options` | `?repo=`: pod available or not, each of your hosts (spawnable, closable, repo_allowed, scratch, claude, login, qualifies, reason), what `auto` picks |

#### Two replicas: the 15 s window

Each replica keeps its own copy of the roster and re-reads the database on
its wake loop's refresh, every 15 s. A write answers from the replica that
took it, which swaps its roster at once; another replica acts on the old
roster until its next refresh:

- **create**: the other replica does not run the new officer yet (no
  instance starts there) for up to 15 s.
- **pause**: the other replica's passes read the officer's switch from the
  database on every pass, so a paused officer's instances are stopped and
  none starts there either; nothing waits for the refresh.
- **delete**: the other replica may still hold the officer for up to 15 s;
  its passes act on an emptied queue and start nothing. A start already
  running there asks the database again once its instance exists (and before
  a host spawn is sent) and removes the instance when the officer is gone, so
  no orphan is left (with the database unreadable the instance is left, and
  the start says it could not remove it).
- **auto host placement** is the database's, not a replica's: each replica
  reads the stored placement. A replica that sees the placed host qualify
  records so (at most once a minute), so a replica that cannot see that host
  (a host is connected to one replica's hub) still keeps the officer there; a
  move is a compare-and-swap, so replicas never keep different hosts.

## Reference: `MUSTER_OFFICERS`, the CLI and the admin API (advanced)

The Officers page is a front end to the same core as everything here. An
officer can also be declared in the `MUSTER_OFFICERS` environment variable
(a GitOps roster): it then runs like any other, is listed on the Officers
page **read-only**, and is changed only in the environment, with a restart.
The `muster officer` CLI and the admin API act on the store directly, as
the caller.

### Turning the runtime on

With a database (`DATABASE_URL`) the built-in store and the officer runtime
are on whether or not `MUSTER_OFFICERS` is set, so an officer
[declared in the database](#declared-in-the-database-no-restart) runs with no
restart. On a muster where nobody has declared one, the wake loop's roster
refresh (every 15 s) makes **one index probe** and reads nothing more until
one exists; the Attention sweep and the schedule runner run and find nothing;
the chief is offered the `officer_spawn_share` tool and its
[officer tools](chief.md#your-officers); and the Features page
reads Officers **on**. For a GitOps roster set `MUSTER_OFFICERS` (`"[]"`
declares none). The boot log with none declared:

```
officers: ON — MUSTER_OFFICERS declares none; officers declared in the database (`muster officer create`) run with no restart
officers: built-in store ON — admin API at /api/officers (`muster officer`), Attention sweep every 15s, schedules every 30s (UTC cron)
```

With no database the store is off and every `/api/officers/…` route answers
404.

🔴 **A builtin officer's queue is in muster's database.** If that database
was not backed up because nothing in it mattered before, back it up now.

### Declaring an officer in `MUSTER_OFFICERS`

```yaml
- name: MUSTER_OFFICERS
  value: >-
    [{"id": "marketing:example", "backend": "builtin", "max_instances": 1,
      "idle_scale_to_zero_after": "30m"}]
```

On a machine, as in the Compose stack (which forwards `MUSTER_OFFICERS`):

```sh
MUSTER_OFFICERS=[{"id":"marketing:example","backend":"builtin","runtime":"host","host":"laptop"}]
```

`runtime` here is `pod` (the default) or `host`; `auto` is for a
database-declared officer only. Such an officer is listed on the Officers
page read-only, and a database row of the same id does not run beside it.
The type and def it runs on are written with the CLI
([below](#types-and-charters-muster-officer-type-def)).

A builtin officer takes the runtime fields every officer takes (`max_instances`,
`tiers`, `model`, `image`, `repo`, `read_repos`, `connectors`, `llm`, `kube`,
idle and poll intervals, `owner`). It is refused at boot, naming the field,
if it declares `hq_url`, a `MUSTER_OFFICER_TOKEN_<NAME>`, `ship`, `ci_logs` or
`transcript` — those only mean something with an HQ. Its `owner` (default 1)
is the user whose store it is read from.

At boot, one line per officer names its backend, what the backend serves and
the tools each pool is offered, then whether the store defines it:

```
officers: marketing:example backend builtin (officer-backend/v1; serves asks,cost,investigation,tiers,usage); tools officer_brief,…,officer_ask_person
officers: marketing:example: defined in the built-in store and switched on
```

An officer declared here but not defined in the store answers 404 to the wake
loop, which treats it as a configuration fault and keeps its instances at
zero until you define it.

### Types and charters (`muster officer type`, `def`)

A charter is the officer's standing instructions. It lives **only** in
muster's database: write it from a file you keep privately, never commit it
to a repository the deployment publishes. The CLI reads it from a file or
stdin, never argv.

```sh
# the type and its charter (a changed charter bumps charter_version)
muster officer type put marketing --name "Marketing" --charter-file ./charter.md

# the officer: <type>:<area>; the type is the part before the colon
muster officer def put marketing:example                 # enabled, live
muster officer def put marketing:example --mode shadow   # escalations recorded, never notified
muster officer def put marketing:example --enabled=false # switched off
muster officer def put marketing:example --escalate-to 2,3

muster officer type ls          # no charters in the list
muster officer type get marketing
muster officer def ls           # "declared": true once MUSTER_OFFICERS runs it
```

Every call acts as the caller: a personal token (`mst_…`) acts as its owner,
the shared hook token as the operator (on an armed server only). One user
never sees another's types, officers or items (row-level security, and every
statement names the caller as owner).

`PUT` replaces: a flag you leave out takes its default. `delete` is refused
(409) while something still names the row: a type an officer uses, an officer
with items or schedules.

#### Charter changes reach running instances

An instance is one long Claude Code conversation, resumed across
scale-to-zero, and its charter reaches it only through `officer_brief`. So
muster records, per instance, the `charter_version` its last `officer_brief`
answered (`officer_defs.charter_briefed`, migration 0140; written by the same
statement that reads the charter it hands over), and holds the instance to the
type's **current** version:

- `officer_claim` (claim-next and by id) and `officer_complete` are refused
  `409 charter_changed` while the instance's recorded version is older than
  its type's `charter_version`, with `charter_version` (the type's) and
  `briefed_charter_version` (the instance's) in the body. Nothing is claimed
  or completed. The refusal tells it to call `officer_brief`, follow the
  charter it returns, and retry.
- An instance with no record — never briefed, or briefed before 0140 — is
  briefed version 0, so its first claim is refused the same way.
- An item it already holds stays its: `officer_heartbeat` and
  `officer_release` are not gated, and the complete goes through once it has
  read the new charter.
- The record is keyed by the instance's name, so it outlives scale-to-zero
  and a restart: a charter put while it was scaled to zero is refused on its
  first claim after the wake.
- `muster officer type put` with an unchanged charter keeps the version, so it
  does not force a re-brief.

Only the instance's own tools are gated; muster's own completions (the
wall-time cap) are not. An HQ officer is not gated this way: HQ holds its
charters, and the contract gives muster no read of the current
`charter_version` short of a whole brief (`officers.CharterGated`).

### Pod egress to the model

An officer instance's pod is egress-locked: no public address and no DNS,
only muster's connector door, so the operator lets it reach Anthropic — by
name on a cluster that filters by name (a Cilium `toFQDNs` rule), or by
address with `MUSTER_OFFICER_EGRESS_CIDRS` and `MUSTER_OFFICER_EGRESS_DNS` on
one that cannot (flannel with the k3s NetworkPolicy controller). Both, with
what the address form costs (unfiltered DNS is a channel), are in
[officers.md](officers.md#the-instances-network) and
[its section on clusters without FQDN policies](officers.md#on-a-cluster-without-fqdn-policies-muster_officer_egress_).

### Letting it read public repositories (`read_grant: "standing"`)

An officer that answers questions about a product needs its docs and source.
When those are in PUBLIC repositories, a standing read lets every instance
read them from its first turn, with no `github` request for its owner to
approve on each scale-up. The wizard's *Public repositories it reads* is
always one; in `MUSTER_OFFICERS` it is spelled out:

```json
{"id": "marketing:example", "backend": "builtin",
 "read_repos": ["example-org/product"], "read_grant": "standing", "read_at": "release"}
```

muster serves the reads itself, anonymously (no credential), reads only, and
only after GitHub confirms each repository is public. `read_at: "release"`
points the instance at the tag of the muster release the server runs. Pods
only, builtin officers only. The rules in full:
[officers.md](officers.md#a-standing-public-read-read_grant-standing).

### Feeding its queue

```sh
muster officer item enqueue marketing:example --kind release.post --subject v1.2.0 \
    --payload '{"tag":"v1.2.0"}' --priority high
muster officer item ls marketing:example --status open
muster officer item resolve marketing:example 42 --outcome no_action --summary "superseded"
```

From a **mirrored Claude Code session**, `officer_item_enqueue` (its MCP door)
makes the same owner item and records that session as the item's requester,
so the owner's approval or rejection of a draft the officer files for it — and
that draft's send outcome — is pasted into the session
([attention.md](attention.md#officer-items-a-session-enqueues)); the owner's
rejection of such a draft then goes to that session only, and the officer does
not redraft it (unless the session's line is never delivered: then it comes
back to the officer as a `draft.refused` item). `muster officer item enqueue` run inside a mirrored Claude Code
session records that session the same way, by calling `officer_item_enqueue`
through the machine's muster-mirror MCP endpoint; anywhere else (or when the
session cannot be resolved, which it says on stderr) it names no session and
the item wakes none.

What it costs, per UTC day (an estimate at list price for a Claude Code
instance — [officers.md](officers.md#a-builtin-officers-usage-officer_llm_usage)):

```sh
muster officer usage marketing:example --days 7
```

An enqueue that matches a live item (open, claimed or waiting) of the same
kind and subject returns that item (`created: false`). The wake loop starts
an instance for claimable work within its poll interval. `resolve` is refused
(409) while an instance holds the item under a live lease.

### Daily budget and routine batching

Optional fields on a builtin officer's `MUSTER_OFFICERS` entry only (a
database-declared officer has none; on an HQ officer they are refused at
boot, and `daily_budget_usd` is refused on a `"runtime": "host"` officer,
whose spend is not booked and whose windows hold the hook token that
enqueues as the owner):

```json
{"id": "marketing:example", "backend": "builtin", "max_instances": 1,
 "daily_budget_usd": 10,
 "routine_kinds": ["plugin.note", "plugin.snapshot", "plugin.engagement"],
 "routine_wake_at": "09:00"}
```

- **`daily_budget_usd`** (0–1000, 0 = no cap): the cap on what the officer
  spends per UTC day, measured against its own `officer_llm_usage` rows for the
  day, every pool and instance summed (what `muster officer usage` shows). Once
  the day's spend reaches it, the wake loop stops waking the officer and the
  claim refuses it new work (409 `over_budget`; claim-next skips such items)
  until the next UTC midnight. **Only an item the owner enqueued, whose kind is
  not routine, gets through**: one enqueued through the admin API
  (`muster officer item enqueue`, which also makes a matching live item the
  owner's) or the follow-up of a draft the owner rejected
  (`officer_items.origin = 'owner'`). Items from plugins, schedules and
  muster's own follow-ups wait for the next day. The owner is told once per
  UTC day, on the attention channel, when the cap is reached, and likewise
  when the spend cannot be read (treated as spent for that pass).
- The cap stops **wakes**; it does not end a turn. An instance holding a claim
  when the cap is reached finishes it, and the spend is only as fresh as the
  usage collector's last push (every 10 minutes), so the day's total can end up
  above the cap.
- **`routine_kinds`** with **`routine_wake_at`** (`"HH:MM"`, UTC; each needs
  the other): an item of a listed kind (matched exactly, never by pattern) is
  enqueued with `not_before` at the next `routine_wake_at`, unless it names a
  `not_before` of its own. It neither wakes the officer nor can be claimed
  before then, and every routine item queued during the day comes due at the
  same minute, so one wake works them all. **A routine kind never bypasses the
  budget**, whoever enqueued it.
- **Rolling back:** an older muster refuses these fields as unknown at boot:
  remove them **before** rolling the image back; rolling forward, deploy the
  image first, then add them.
- Whatever these fields say, the wake loop does not wake an officer for an
  item whose `not_before` is still ahead (`--not-before` included).

### Switching it off

**Pause** on its page (or `muster officer pause`) for a database-declared
officer; for one `MUSTER_OFFICERS` declares, **Pause** on its page too, or
`muster officer def put marketing:example --enabled=false` (the same switch). Either way the wake loop stops its
instances and starts none, whatever its queue holds, until it is switched on
again. Removing it from `MUSTER_OFFICERS` also stops muster running it; its
store rows stay.

### The admin API

| method | path | body / query |
|---|---|---|
| GET | `/api/officers/types` | |
| GET / PUT / DELETE | `/api/officers/types/{key}` | PUT `{name, charter, policy?}` |
| GET | `/api/officers/defs` | |
| PUT / DELETE | `/api/officers/defs/{officer}` | PUT `{enabled?, mode?, escalate_to?}` |
| GET / POST | `/api/officers/defs/{officer}/items` | GET `?status=&limit=`; POST `{kind, subject_key, payload?, tier?, priority?, not_before?}` |
| POST | `/api/officers/defs/{officer}/items/{id}/resolve` | `{outcome, summary}` |
| GET | `/api/officers/defs/{officer}/schedules` | |
| GET | `/api/officers/defs/{officer}/usage` | `?days=` (default 30, at most 90): the officer's model spend per UTC day, pool and instance (`officer_llm_usage`), newest first, with `total_cost_usd` and `prices_as_of` |
| PUT / DELETE | `/api/officers/defs/{officer}/schedules/{name}` | PUT `{cron, kind, subject_key, payload?, tier?, priority?, skip_if_live?, enabled?}` |
| GET | `/api/officers/defs/{officer}/contacts` | `?venue=&label=&flag=&handle_prefix=&limit=` |
| GET | `/api/officers/defs/{officer}/contacts/{venue}/{handle}` | the person and their newest events |
| PUT | `/api/officers/defs/{officer}/contacts/{venue}/{handle}/flags` | `{dnc?, bot?, hostile?}`: the owner's marks (`dnc: true` records reason `owner`) |
| DELETE | `/api/officers/defs/{officer}/contacts/{venue}/{handle}` | erases the person and their history |

All behind the hook tier (`requireArmedHookToken`); 404 while the store is off.
