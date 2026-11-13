# Officers on the built-in backend

An officer normally lives in an external HQ (see [officers.md](officers.md)):
HQ keeps its type, charter, queue and leases, and muster runs its instances.
The **built-in backend** keeps all of that in muster's own Postgres instead, so
a deployment can run its own officers with no HQ at all. The design and the
contract both backends implement are in
[design/officers-first-class.md](design/officers-first-class.md).

What a builtin officer gets on day 1: the six queue tools (brief, items,
claim, heartbeat, complete, release), tiers, the investigation block, item
cost, **asks to its owner through muster's Attention view**, and escalations its owner can answer back. What it does
not get: HQ's observability, transcripts, usage and pools ledgers, derived
asks, gap tasks, task reads, PR context, the ship gates and the release
captain — it is not offered those tools and its first message does not
mention them.

## ⚠ Before you declare one on a real deployment

Every outward action an officer takes is meant to pass an Attention ask that
shows the exact content (a muster draft the owner approves, carried out by a
relay on the owner's own machine — the design's *Phase-3 gated outward
posting*). **That path is not built yet.** Until it is, a builtin officer's
only ways out of its pod are muster's tools and the connectors its owner
approves one request at a time: do not approve a connector that can write to
the outside world for one, and give it no `repo` with write access. The
empty roster (no officer declared) exposes nothing.

## 1. Turn the runtime on (the empty roster)

Set `MUSTER_OFFICERS` — an empty array is enough:

```yaml
- name: MUSTER_OFFICERS
  value: "[]"
```

With a database (`DATABASE_URL`) this turns the **built-in store** on: the
admin API at `/api/officers/…` (the `muster officer` CLI) and the Attention
sweep. No officer runs yet. The boot log says:

```
officers: ON — empty roster (MUSTER_OFFICERS declares none)
officers: built-in store ON — admin API at /api/officers (`muster officer`), Attention sweep every 15s, schedules every 30s (UTC cron)
```

Unset (or blank), the store is off and every `/api/officers/…` route answers
404.

🔴 **A builtin officer's queue is in muster's database.** If that database
was not backed up because nothing in it mattered before, back it up now.

## 2. Define the officer (types and charters live only in Postgres)

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

## 3. Declare it in `MUSTER_OFFICERS`

```yaml
- name: MUSTER_OFFICERS
  value: >-
    [{"id": "marketing:example", "backend": "builtin", "max_instances": 1,
      "idle_scale_to_zero_after": "30m"}]
```

A builtin officer takes the runtime fields every officer takes (`max_instances`,
`tiers`, `model`, `image`, `repo`, `read_repos`, `connectors`, `llm`, `kube`,
idle and poll intervals, `owner`). It is refused at boot, naming the field,
if it declares `hq_url`, a `MUSTER_OFFICER_TOKEN_<NAME>`, `ship`, `ci_logs` or
`transcript` — those only mean something with an HQ. Its `owner` (default 1)
is the user whose store it is read from.

At boot, one line per officer names its backend, what the backend serves and
the tools each pool is offered, then whether the store defines it:

```
officers: marketing:example backend builtin (officer-backend/v1; serves asks,cost,investigation,tiers); tools officer_brief,…,officer_ask_person
officers: marketing:example: defined in the built-in store and switched on
```

An officer declared here but not defined in the store answers 404 to the wake
loop, which treats it as a configuration fault and keeps its instances at
zero until you define it.

## 4. Feed its queue

```sh
muster officer item enqueue marketing:example --kind release.post --subject v1.2.0 \
    --payload '{"tag":"v1.2.0"}' --priority high
muster officer item ls marketing:example --status open
muster officer item resolve marketing:example 42 --outcome no_action --summary "superseded"
```

An enqueue that matches a live item (open, claimed or waiting) of the same
kind and subject returns that item (`created: false`). The wake loop starts
an instance for claimable work within its poll interval. `resolve` is refused
(409) while an instance holds the item under a live lease.

## 4b. Schedules (cron, UTC)

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

## 5. Asks and escalations, through Attention

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
  as an Attention **ask** — the officer asking a human to act — exactly once,
  even with two replicas. **An answer flows back to the officer**: the item
  re-opens as a judgement item with the answer on its `asks[]`, the next
  instance claims it, and the other recipients' asks are withdrawn.
  Dismissing it (or letting it expire) leaves the item escalated. A
  `--mode shadow` officer records its escalations and asks nobody.

## 6. Switch it off

`muster officer def put marketing:example --enabled=false`: the wake loop
stops its instances and starts none, whatever its queue holds; switch it on
again and it resumes. Removing it from `MUSTER_OFFICERS` also stops muster
running it; its store rows stay.

## The admin API

| method | path | body / query |
|---|---|---|
| GET | `/api/officers/types` | |
| GET / PUT / DELETE | `/api/officers/types/{key}` | PUT `{name, charter, policy?}` |
| GET | `/api/officers/defs` | |
| PUT / DELETE | `/api/officers/defs/{officer}` | PUT `{enabled?, mode?, escalate_to?}` |
| GET / POST | `/api/officers/defs/{officer}/items` | GET `?status=&limit=`; POST `{kind, subject_key, payload?, tier?, priority?, not_before?}` |
| POST | `/api/officers/defs/{officer}/items/{id}/resolve` | `{outcome, summary}` |
| GET | `/api/officers/defs/{officer}/schedules` | |
| PUT / DELETE | `/api/officers/defs/{officer}/schedules/{name}` | PUT `{cron, kind, subject_key, payload?, tier?, priority?, skip_if_live?, enabled?}` |

All behind the hook tier (`requireArmedHookToken`); 404 while the store is off.
