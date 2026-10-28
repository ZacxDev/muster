# Officers

An **officer** is a standing agent with a job description instead of a task: a
release captain for one service, a triage officer for one repository. Its
durable identity, its work queue, its charter and the lock on each item live in
**HQ** (an external server that speaks the officers API below). muster runs the
officer's **instances**: Claude Code pods it wakes when HQ has work, gives the
officer's tools, and scales to zero when the queue is quiet.

| | lives in |
|---|---|
| the officer (id, type, area, charter, max instances) | HQ |
| the queue of items, and who holds each one | HQ (Postgres; one claim at a time, under a lease) |
| the officer's HQ token | muster-server's environment — never an instance's pod |
| instances (pods, volumes, Claude accounts) | muster, one agent per instance |

Officers **watch, triage, record and dispatch**. They do not merge, deploy or
release: those stay with humans. An instance holds no production credential.

The code: `internal/officers` (configuration, HQ client, the wake loop's
policy, webhook verification, the first message), bound to muster in
`internal/api/officers.go` (instance agents, the MCP tools, the webhook route,
delete protection).

## Configuration

```sh
MUSTER_AGENT_KINDS=gateway,claude-code        # officers run on claude-code
MUSTER_OFFICERS='[
  {"id": "release:example-web",
   "hq_url": "https://hq.example.com/api/v1",
   "max_instances": 1,
   "idle_scale_to_zero_after": "30m",
   "poll_interval": "1m"}
]'
MUSTER_OFFICER_TOKEN_RELEASE_EXAMPLE_WEB=…     # from a Secret; see below
MUSTER_OFFICER_WEBHOOK_SECRET=whsec_…          # optional: the push wake path
```

| field | default | |
|---|---|---|
| `id` | required | HQ's officer id, `<type>:<area slug>`: lowercase letters, digits and single hyphens on each side of the colon. HQ allows 54 characters; muster refuses an id whose instance agent name (`officer-<type>-<slug>-<max>`) would pass 48, because that name becomes a namespace. |
| `hq_url` | required | HQ's API base, including `/api/v1`. No credentials in the URL. |
| `max_instances` | `1` | 1–8. Each instance is a pod, a volume and a Claude account for life. |
| `idle_scale_to_zero_after` | `30m` | How long the officer must be idle before its instances are stopped. |
| `poll_interval` | `1m` | How often HQ's open items are read when nothing else wakes the loop. At least `10s`. |
| `owner` | `1` | The muster user the instance agents belong to. |
| `cc_account` | the pool's choice | Pin the Claude account instances run on. |
| `kind` | `claude-code` | Only `claude-code` is accepted. |

Each officer's token is read from `MUSTER_OFFICER_TOKEN_<NAME>`: the id
upper-cased with every other character written as `_`. A declared officer with
no token, an unknown field, two officers whose agent names would collide, or an
officer on a server without the `claude-code` kind or an agent provisioner, is
a boot refusal that names the problem. The rows are in
[configuration.md](configuration.md#muster-server-officers).

### The release captain

`release:example-web` above is the first officer: HQ produces a
`release.evaluate` item when a release of the service is built and awaiting
approval (with the PR list and each PR's review verdict in the payload) and a
`release.post_report` item when it goes live. Its instance evaluates the
release — risk level, summary, what to monitor — and posts the evaluation to
HQ's captain route with `officer_release_evaluation` (muster attaches the
officer token, which HQ's captain route accepts for a release officer), then
completes the item. muster holds **no copy of the charter**: the instance loads it from HQ
with `officer_brief`, so the versioned charter in HQ is the only one. Approval
of the release itself stays a human's, in HQ.

## Instances

Instance *n* (1-based) of officer `<type>:<area>` is the muster agent named
`officer-<type>-<area>-<n>` (`officer-release-example-web-1`), kind
`claude-code`, with no task and no repository. Instances are interchangeable:
they share the officer's queue and identity, and any of them can take any item.

Names starting `officer-` are reserved. muster's name generator cannot produce
one (its adjective-noun pool has no such word), and the officer runtime is the
only code that creates an agent under a fixed name of that shape. An instance
keeps its namespace, volume and Claude account across stops and starts.

## Waking and scaling

One loop runs every officer. Each pass, for one officer:

1. read HQ: its `open` items and its `claimed` items;
2. read muster: which of its instance agents exist and are up;
3. decide;
4. release live claims held by an instance that is down;
5. start or stop instances.

The decision (`officers.Decide`, a pure function):

- **Claimable** is the open items plus claimed items whose lease has expired
  (the contract makes those claimable by anyone). **Held** is claimed items
  with a live lease held by one of this officer's instances.
- **Busy** (anything claimable or held): want *held-by-up-instances +
  claimable* instances up, at most `max_instances`, and start the
  lowest-numbered down instances to get there. Nothing is stopped while busy.
- **Idle** (nothing claimable, nothing held): once idle has lasted
  `idle_scale_to_zero_after`, every up instance is stopped. A new item resets
  the window.

**Up** means the instance's live status is running, provisioning or pending —
or `error`: a crash-looping instance keeps its slot rather than being started
again every poll (which would re-send its first message each time). A create or
start still in flight (the row says provisioning, the backend shows nothing yet)
also counts as up, so it is not started twice.

**Starting** an instance that does not exist creates it, with its first message
(below) as the kickoff. Starting one that exists **re-arms** its first message
(`agents.PGStore.RearmKickoff`: the pending note is replaced and the kickoff
markers reset, so the ordinary kickoff deliverer sends it once the pod is
ready) and starts it. **Stopping** scales it to zero; its agent and volume stay.

**Wake signals.** Every `poll_interval`, and at once when HQ's
`officer.item_created` webhook arrives (below). Polling alone is enough.

**HQ down.** A pass whose HQ read fails changes nothing — no start, no stop, no
release — and the officer's next pass waits `poll_interval` doubled per
consecutive failure, capped at 10 minutes. A pass that cannot see muster's own
instances (the provisioner is unreachable) is treated the same way: "I cannot
see" is never read as "nothing is running".

**Restarts.** Every start decision is a function of HQ's queue and muster's
instances, so a restarted muster makes the same decisions. The one thing kept
in memory is when the officer was first seen idle; a restart forgets it and
waits a full idle window again. That can delay a scale-down, never cause one.

**More than one muster pod.** The loop is not leader-gated (no background loop
in muster is; see `cmd/muster-server`'s `startBackgroundLoops`). Two pods
starting the same instance is one agent create winning on the unique name and
one logged failure; two stopping one is idempotent.

### A claim held by a stopped instance

When an instance is stopped — by the operator, or because it crashed out —
while it holds a live claim, the next pass sees a held item whose holder is not
up and **releases** it (`POST …/release` with that instance's id), so another
instance can take it at once instead of waiting out the lease. The idle
scale-down cannot stop an instance holding a claim (a held claim is not idle).
An instance in `error` counts as up, so its claim is not released early; it
lapses with its lease. If the release itself fails (HQ down), the lease lapse
is the fallback.

## The tools

An officer's instances are offered these MCP tools on muster's MCP server, and no
other agent is (a third axis beside the chief-only one, enforced in the tool
listing and again at dispatch):

| tool | HQ call |
|---|---|
| `officer_brief` | `GET /officers/{id}/brief` — the charter, open items, recent outcomes, memory scopes |
| `officer_items` `{status?, limit?, cursor?}` | `GET /officers/{id}/items` (default `status=open`) |
| `officer_claim` `{item_id, lease_seconds?}` | `POST …/items/{item}/claim` |
| `officer_heartbeat` `{item_id, lease_seconds?}` | `POST …/items/{item}/heartbeat` |
| `officer_complete` `{item_id, outcome, summary, links?}` | `POST …/items/{item}/complete` |
| `officer_release` `{item_id}` | `POST …/items/{item}/release` |
| `officer_release_evaluation` `{evaluation}` | `POST /releases/captain` — **release officers only** (`release:*`): the captain route's body, passed through, with `instance` set by muster |

`lease_seconds` defaults to 600 (muster always sends it; HQ's own default is
300) and is clamped to 30–3600. **Claiming an item you already hold** is not a
conflict: HQ refuses a claim on any live lease, the caller's own included, and
since an instance's id is its agent name a restarted instance re-claiming its
item would hit that. When HQ's 409 names the calling instance as `held_by`,
`officer_claim` heartbeats with the requested lease instead and answers with
the item, `already_yours: true` and the note "already yours — lease renewed"
(if the lease ran out between the two calls, the claim is retried once). A 409
naming another instance is returned as a refusal. At most 10 `links`. `outcome` is `done`,
`escalated` or `no_action`; `summary` is at most 4096 bytes. HQ's JSON is
returned as is; a 4xx comes back as a refusal carrying HQ's status and body (a
409 on a claim names the holder), and an outage as "try again in a minute".

The `instance` muster sends HQ is the agent's name (`officer-…-<n>`), so the
same instance keeps its claims across a pod restart. For anything that needs a
human, an instance uses `attention_ask` like every other agent.

### The first message

```
You are officer <id>, instance <n> of <max> (<agent name>). …
1. Call officer_brief. It returns your charter …
2. Work the queue one item at a time: officer_items, officer_claim{item_id},
   officer_heartbeat{item_id} …, officer_complete{item_id, outcome, summary} …
3. Anything that needs a human decision: attention_ask, then attention_wait.
4. When nothing is open and you hold no claim, stop and wait …
```

(`officers.KickoffText` has the whole text.) It carries no charter and no
credential.

## Security model

- **The token never leaves muster-server.** It is read from the server's
  environment into `officers.Officer`, whose token field neither JSON nor `fmt`
  prints, and is spent only by `officers.Client`, in the `Authorization` header
  of requests to that officer's own `hq_url`. HQ errors carry HQ's response,
  never the request. Redirects are not followed, so a redirect cannot carry the
  token elsewhere. An instance's pod spec, env, files and first message carry
  no trace of it (`TestAnOfficerInstancesPodSpecCarriesNoOfficerToken`,
  `TestTheKickoffNamesTheOfficerAndCarriesNoToken`).
- **Per-officer binding.** An officer tool acts for the officer whose instance
  the CALLING agent is — `officers.Registry.ForAgentName` on the agent muster
  resolved from the caller's own per-agent token — and for no other. The tools
  take no officer argument; one naming another officer is refused before HQ is
  reached. An agent cannot rename itself (the slug is immutable), and no muster
  path mints an `officer-` name except this runtime. HQ enforces the same
  boundary again: an officer token may only touch its own officer's items.
- **Delete protection.** A declared officer's instance cannot be deleted by any
  of muster's destroy paths (the card's Delete, a task dismissal, the
  `agent_destroy` tool, the stopped-agent reaper): they ask one predicate,
  `deleteProtection`, which is the chief predicate *or* "an instance of a
  declared officer". The chief predicate itself is unchanged, so an officer is
  never offered the chief's tools. Remove an officer from `MUSTER_OFFICERS` and
  its instances become ordinary, deletable agents.
- **The webhook.** `POST /officers/webhook` asks for no session: the Standard
  Webhooks signature is its authentication (HMAC-SHA256 over
  `<webhook-id>.<webhook-timestamp>.<body>`, any listed `v1` signature, a
  timestamp within 5 minutes). A verified `officer.item_created` for a declared
  officer only nudges the loop, which re-reads HQ before deciding anything, so
  the body is never trusted for more than "look now". A repeated `webhook-id`
  is accepted (204) and ignored. A bad or missing signature is 401; without
  `MUSTER_OFFICER_WEBHOOK_SECRET` the route is 404.
- **No production access.** Instances are ordinary `claude-code` pods: no
  service-account token, egress limited as for every claude-code agent
  ([provisioner.md](provisioner.md)).

## cairn

**Deferred.** The contract gives officers read access to their cairn scopes and
append-only writes. muster does not provide either yet: a `claude-code` pod has
no cairn client and its network policy cannot reach a store, by design
([chief.md](chief.md)). The planned shape keeps that property — a
per-officer, narrowed cairn credential held by muster-server (like the HQ
token) and an `officer_cairn_recall` tool proxied by muster — rather than
opening pod egress to the store. Until then, `officer_brief`'s `cairn.scopes`
is informational.

## The HQ API muster consumes

From the officers contract, v1 (base `/api/v1`, bearer token of scope
`officer`): `GET /officers/{id}/brief`, `GET /officers/{id}/items`, `POST
/officers/{id}/items/{item}/{claim,heartbeat,complete,release}`, and `POST
/releases/captain` (release officers), with the bodies in the table above. Where the contract leaves a shape open, muster accepts:

- the items list as HQ sends it, `{"items": […], "next": "…"|null}` (and
  `next_cursor`, or a bare array); the wake loop follows `next` for up to 5
  pages of 100;
- item ids as JSON strings or numbers;
- a webhook body of the Standard Webhooks form `{"type", "timestamp", "data":
  {"officer_id", "item_id", "kind"}}`.

`internal/officers/hqtest` is the in-process fake the tests run against; it
implements the claim/lease rules (409 to a second claimant while the lease is
live, an expired lease claimable by anyone, 410 on a finished item, 403 across
officers).

## Deploying

What a deployment needs beyond muster's usual claude-code setup:

1. HQ: the officer and its type declared (`PUT /officer-types/release`, `PUT
   /officers/release:example-web` with `max_instances`), and an officer token
   issued for it.
2. A Secret holding that token, exposed to the muster-server Deployment as
   `MUSTER_OFFICER_TOKEN_<NAME>` (not to any agent).
3. `MUSTER_OFFICERS` on muster-server, and `MUSTER_AGENT_KINDS` including
   `claude-code` with an account in the pool.
4. Egress from muster-server to HQ.
5. Optionally, an HQ webhook subscription to `officer.item_created` pointing at
   `https://<muster>/officers/webhook`, with its secret as
   `MUSTER_OFFICER_WEBHOOK_SECRET` — and that path reachable from HQ.

Checking it: the boot log has one `officers: <id> on: …` line per officer;
after HQ creates an item, `officer-<type>-<area>-1` appears among the agents
within one poll; its transcript opens with the first message above.
