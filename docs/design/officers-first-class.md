# Officers as a first-class feature (pluggable backend)

**Status: slices 0–4 built (slices 3–4 not wired)** (this revision supersedes the draft of
muster#503). Each slice below lands as its own pull request; slices 0–4 change
nothing an existing HQ officer sees or does.

[officers.md](../officers.md) is the as-built reference: today an officer's
identity, charter, queue, leases and memory live in an **external backend**
(HQ, a team console) that muster calls over HTTPS with a per-officer token,
and muster runs the officer's instances. This design makes the officer
contract muster's own, keeps HQ working unchanged as **one implementation of
it**, and adds a **built-in backend** in muster's own Postgres so a deployment
with no external console — or one that must not depend on one — can run
officers.

Symbols are named rather than line-numbered: the surveyed code moves weekly.
Surveyed at `origin/main` `56c33a5c`.

## Why

A deployment's own officers must be able to run in **its own** muster, with
its own state, and never on somebody else's console. Today they cannot: muster
has no place to keep an officer type, a charter, an item or a schedule, so
every officer is a tenant of some HQ. The built-in backend removes that
dependency; HQ stays fully supported for the officers that belong there.

🔴 Nothing in this work calls HQ's write tools (officer, officer-type or
schedule administration). HQ is talked to only through the officer API it
already serves to its officers.

## Goal and non-goals

**Goal.**

- muster owns and documents an **officer-backend contract**
  (`officer-backend/v1`): the item queue, leases, claim / claim-next / brief /
  heartbeat / complete / release / the on-off switch, tiers, memory, and
  capability negotiation; optional verbs behind capabilities.
- muster ships a **built-in backend** implementing the contract in its own
  Postgres, escalating through muster's own Attention surface.
- HQ is one implementation of the contract with no renames on its side.

**Non-goals.** Moving any deployment's domain triggers (alert baselines,
release ingestion) into muster; changing the runtime (wake/scale, instance
pods, tool hosting, read budgets, the LLM proxy, ship gates); a hosted
multi-tenant backend.

## What exists on main

| | lives in |
|---|---|
| officer types, charters, officers, the item queue, leases, memory, triggers, escalation delivery, shadow flag, schedules, asks to people, observability relay, cost and usage ledgers | HQ |
| wake loop and scaling, instance pods and accounts, MCP officer tools, kube reads, LLM proxy and budgets, ship gates, transcript forwarding, the judgement session cap (migration 0082) | muster |

muster's whole backend dependency was **one concrete type**, `officers.Client`
(`internal/officers/hq.go` plus a verb per file: `asks.go`, `item_asks.go`,
`gap_tasks.go`, `tasks.go`, `pr_context.go`, `pools.go`, `obs.go`), reached
through `ClientHQ.Client(o)` from **25 routes**:

| group | verbs (HQ route under the officer's base URL) | callers |
|---|---|---|
| core | items `GET /officers/{id}/items`, brief, claim, claim-next, heartbeat, complete (with `investigation`, `cost_usd`/`cost_from`), release, officer `GET /officers/{id}` (`enabled`) | wake loop (`Reconciler` via the `HQ` adapter), the six queue tools, the one-claim and held-item checks, the ship and shepherd item gates, failover |
| asks | `POST …/items/{item}/asks`, `POST /officers/{id}/ask-images` | `officer_ask_person`, the ask-image upload route |
| derived asks | task-asks, flood-blocks | `officer_task_ask`, `officer_flood_block_propose` |
| gap tasks | `POST …/items/{item}/gap-tasks` | `officer_gap_task` |
| tasks read | `GET /officers/{id}/tasks`, `…/tasks/{task}` | `officer_task_list`, `officer_task_get` |
| PR context | `GET /officers/{id}/prs/{n}/context` | `officer_pr_context` |
| obs | `GET …/obs/queries`, `POST …/obs/query`, `POST …/obs/raw` | the three obs tools |
| transcript | `POST …/items/{item}/transcript` | the transcript forwarder |
| usage, pools | `PUT /officers/{id}/usage`, `PUT /officers/{id}/pools` | the usage collector, the pools reporter |
| ship | `POST` notices (path configurable), `POST /officers/{id}/preflight` | the push gate, merge and tag gates |
| captain | `POST /releases/captain` | `officer_release_evaluation` |

Two properties made this a de-facto contract rather than a designed one:
**pass-through bodies** (brief, claim and items reach the instance as HQ's raw
JSON), and **semantics only in the fake** (`internal/officers/hqtest` is the
best specification of the lease rules there is).

## The contract: `officer-backend/v1`

### In muster: `officers.Backend` (slice 1, as built)

`internal/officers/backend.go`. The core verbs, for one officer:

```go
type Backend interface {
    Officer() Officer
    Capabilities() Capabilities
    Items(ctx, ItemsQuery) (ItemsPage, error)
    AllItems(ctx, status string) ([]Item, error)
    Brief(ctx) ([]byte, error)
    Claim(ctx, item ItemID, instance string, leaseSeconds int, tier string) ([]byte, error)
    ClaimNext(ctx, instance string, leaseSeconds int, tier string) ([]byte, error)
    Heartbeat(ctx, item ItemID, instance string, leaseSeconds int) ([]byte, error)
    Complete(ctx, item ItemID, instance string, done Completion) ([]byte, error)
    Release(ctx, item ItemID, instance string) ([]byte, error)
    Enabled(ctx) (bool, error)
    Scrub(s string) string // remove every credential the backend holds
}
```

Every optional verb group above is its own small interface
(`AsksBackend`, `TaskAsksBackend`, `FloodBlocksBackend`, `GapTasksBackend`,
`TasksBackend`, `PRContextBackend`, `ObsBackend`, `ObsRawBackend`,
`TranscriptBackend`, `UsageBackend`, `PoolsBackend`, `ShipBackend`,
`CaptainBackend`). `officers.Backends` caches one backend per officer and is
the only way any caller reaches one; `officers.Serves[T](b, capability)` hands
out an optional verb only when the backend both **advertises** the capability
and **implements** the interface, else `ErrNotServed`. `*Client` (HQ)
implements all of them. `CompleteDroppingCost` is now a function over any
`Backend`.

**Errors are the contract's three kinds** whatever the implementation: a
decision is `*HQError` (status plus the backend's `{code, …}` body), a failure
its route says to retry is `*RetryableError`, an outage wraps
`ErrHQUnavailable`. Every caller already branches on exactly these, so a
second backend speaks them too (the names stay: renaming 40 call sites buys
nothing).

### Capabilities (slice 2, as built)

`Capabilities{Contract, Verbs}`. The capability names: `tiers`,
`investigation`, `cost` (features of the core verbs), and one per optional
verb group: `asks`, `task-asks`, `flood-blocks`, `gap-tasks`, `tasks-read`,
`pr-context`, `obs`, `obs.raw`, `transcript`, `usage`, `pools`, `ship`,
`x-captain`.

- **HQ advertises every capability.** HQ has no capabilities route yet and
  muster has always offered an HQ officer every tool its shape allows; a route
  an older HQ lacks still answers 404 at call time, exactly as before. When HQ
  adds `GET /officers/{id}/capabilities`, the client reads it instead.
- **`officers.ToolCapability(tool)`** is THE map from an officer tool to the
  capability it needs (`""` for a core tool or one muster serves itself:
  kube reads, CI logs). A ledger test fails on any registered officer tool it
  does not classify, and an unclassified tool is never served.
- **`officers.ToolServed(o, caps, tool)` = the officer's shape offers it
  (`ToolOffered`) AND the backend serves its capability.** The tool listing,
  the session allow-list, dispatch and the ask-image route all ask it
  (`Server.officerToolServed`). A narrower backend's instance is therefore
  never offered, pre-allowed or dispatched a tool its backend cannot serve.
- **The golden** `internal/api/testdata/officer_backend.golden`, recorded on
  unmodified main before the refactor, pins the kickoff, the pre-allowed tools
  and every offered tool definition of ten HQ officer pools (every type pack,
  both tiers, a shipping officer). It is byte-identical after slices 1–2.

Kickoff notes are **not** yet capability-gated: that is slice 5, when the
first narrower backend is wired, and the golden is what lets it change the
kickoff of a builtin officer without touching an HQ one.

### Per-officer backend (as built: the declaration; wiring is slice 5)

`MUSTER_OFFICERS` entries take `"backend": "hq" | "builtin"`, default `hq`.
There is **no global flag**: one deployment may run HQ officers and builtin
officers side by side. A `builtin` officer is **refused at config load, fail
closed**, naming each field when it declares anything that only means
something with an external HQ:

- `hq_url` (its backend is muster itself);
- a token in `MUSTER_OFFICER_TOKEN_<ID>` (an in-process backend holds none —
  muster's per-agent binding is the authorisation);
- `ship` or `ci_logs` (the ship gates need an HQ's notices, preflight and
  switch);
- `transcript` (forwarding goes to an HQ's transcript store).

Until slice 5 lands, a builtin officer that passes all of those is refused
too ("not available in this build yet") rather than run against nothing.

### Semantics (unchanged from the draft; the conformance suite pins them)

| topic | rule |
|---|---|
| item | `{id, officer_id, kind, subject_key, payload, status, tier, priority, not_before?, claimed_by?, claimed_at?, lease_expires_at?, attempts, outcome?, summary?, routine_summary?, investigation?, created_at, updated_at}`; ids are integers or strings |
| dedupe | at most one live item (open, claimed or waiting) per `(officer, kind, subject_key)`; an enqueue that matches a live item returns it |
| status | `open -> claimed -> done / escalated` (outcomes `done`, `no_action`, `escalated`); lease expiry makes a claimed item claimable again (no reaper); `release` -> open; `waiting` while an ask to a person is outstanding |
| lease | `lease_seconds` 30..3600; heartbeat renews; 409 `held` to ANY claim on a live lease (muster turns the holder's own into a heartbeat); 409 `lease_lost` on heartbeat/complete/release without it; 410 on a terminal item; another officer's item is 404 |
| queue order | blocked never; priority `urgent > high > normal > low`; then id. `claim-next` with `tier` takes only that tier; `{item: null}` when none |
| tiers | `routine` / `judgement`, absent reads judgement. A routine holder may complete `needs_judgement`: the item reopens as judgement in place, holder cleared, the summary kept as `routine_summary`; refused (400) on a judgement item and with links |
| claim answer | `{item, lease_expires_at}` (+ `memory` when the backend computes it) |
| brief | `{officer, type{key, charter, charter_version}, open_items, recent_items, generated_at}`; the charter reaches the instance only here |
| errors | `{code, message|error}` with a closed vocabulary: `held, blocked, wrong_tier, lease_lost, gone, invalid, not_found` (+ the optional verbs' own) |

## The built-in backend

### Store (slice 4, as built; not wired)

Migration **0118** `officer_backend` (the next free number on main; the
in-flight shepherd-loop change, muster#617, also numbered its migration 0118,
so whichever merges second renumbers — the contiguity test refuses a gap), four owned tables under FORCED
row-level security in migration 0082's shape (`owner_id` defaults to the
principal, `owner_isolation` policy, re-applied by `db.RLSOn`, undone by
`rollback/rls_off.sql`):

- `officer_types(owner_id, key, name, charter, charter_version, policy jsonb)`
- `officer_defs(owner_id, officer, type_key, enabled, mode, escalate_to)` —
  "defs", because the declared officer list stays `MUSTER_OFFICERS`
- `officer_items(owner_id, officer, kind, subject_key, payload, status, tier,
  priority, not_before, blocked, claimed_by, claimed_at, lease_expires_at,
  attempts, outcome, summary, routine_summary, investigation, cost_usd,
  cost_from, created_at, updated_at)` with a partial UNIQUE index on live
  items `(owner_id, officer, kind, subject_key) WHERE status IN
  ('open','claimed','waiting')` (dedupe) and a queue index
- `officer_schedules(owner_id, officer, name, cron, kind, subject_key,
  payload, skip_if_live, enabled, next_fire_at, last_fired_at)` — the table
  only; firing is slice 8

`internal/officers/builtin`: the Postgres store (enqueue with dedupe; claim
by id under `SELECT … FOR UPDATE`; **claim-next as one `UPDATE … WHERE id =
(SELECT … ORDER BY <queue order> FOR UPDATE SKIP LOCKED LIMIT 1)`**, so two
replicas never hand out one item; heartbeat, complete incl. `needs_judgement`,
release, items paging, brief from the type's charter) and `builtin.Backend`
implementing `officers.Backend` over it with the contract's errors. Every
timestamp comparison takes muster's clock as a parameter, never SQL `now()`,
so the conformance suite can expire a lease without sleeping. Its capabilities
on day 1: `tiers`, `investigation`, `cost` (vocabulary until slice 5 branches on them).

### Conformance (slice 3, as built)

`internal/officers/backendtest`: the lease, queue, tier and refusal rules as
named checks over any `officers.Backend` plus a small `World` (seed an item,
block it, disable an officer, set a charter, advance the clock). It runs in CI
against **HQ's client over `hqtest`** and against **`builtin.Backend` over
Postgres**. Each harness has a **fault-injection knob** that allows a second
claim on a live lease; a test runs the suite with the fault on and requires
the double-claim check to fail — so the suite is shown able to go red against
each backend, not only green. It also asserts the capabilities relationship:
every capability a backend advertises is implemented
(`officers.Unimplemented`). The checks: capabilities; claim under a lease;
a live lease refuses any other claim (the holder's own too); an expired lease
is anyone's and lost to its holder (heartbeat and complete 409); heartbeat
renews only the holder's; complete finishes once (410 on a claim after, 409
on a second complete, 400 on an unknown outcome); release by the holder only;
another officer's or an unknown item is 404; claim-next by priority then age,
never blocked or held, then null; tiers on claim, claim-next and items;
needs_judgement in place; concurrent claim-nexts hand each item out once;
items by status; the switch and the brief.

### Day-1 scope of the built-in backend (slices 5–6)

Core verbs **plus asks plus Attention escalation**:

- **asks**: `officer_ask_person` on a builtin officer becomes a muster
  Attention ask to the officer's owner (the Attention view and phone push);
  the item is `waiting` until it is answered, then re-opens with the answer
  on it and muster wakes an instance — HQ's asks semantics, delivered by
  muster.
- **escalation**: an `escalated` complete notifies the owner (and
  `escalate_to`) through Attention, once per item per recipient, written in
  the same transaction as the complete; shadow mode records without notifying.

Not on day 1: obs, transcripts, usage/pools ledgers, derived asks, gap tasks,
tasks read, PR context, ship, captain. A builtin officer is simply not offered
those tools (`ToolServed`), and its kickoff does not mention them (slice 5).

## Slices

S is up to 300 changed non-test lines, M up to 1,000.

| slice | change | size | closes when |
|---|---|---|---|
| 0 | this revision | S | merged with slices 1–2 |
| 1 | `officers.Backend` + optional interfaces; HQ client implements them; every call site through `Backends`/`Serves` | M | merged, officer tests green in CI on the merged tree |
| 2 | capabilities; `ToolCapability`/`ToolServed` gate listing, allow-list, dispatch; the golden | S | the golden green after the refactor and red on a mutated kickoff or a dropped HQ capability |
| 3 | `backendtest` conformance suite + fault knob, run against HQ (hqtest) | M | green in CI; red with the fault on |
| 4 | migration 0118 + `officers/builtin` store and backend, not wired; suite run against it | M | green in CI against Postgres; red with the fault on; the migration applies on the deployment |
| 5 | **wiring**: `Officer.Backend`, `Backends` routes builtin officers to `builtin.Backend`; capability-gated kickoff notes; drop the claim-next 404 sniff for backends that advertise it; boot line lists each officer's backend and offered tools; types/defs seeded from a declarative file | M | a builtin officer runs end to end on a dev deployment (item → pod → claim → complete) |
| 6 | **Attention asks + escalation** for builtin officers | M | one Attention ask per ask, re-open on answer; one notification per recipient per escalated item, zero in shadow (tests) |
| 7 | **admin REST + CLI `muster officer`**: list/enqueue/resolve items, types, defs | M | the CLI enqueues an item that a builtin officer then claims (test) |
| 8 | **schedules**: cron fire with `skip_if_live`, missed fires collapse to one, once across replicas | M | a schedule fires once across a restart and two replicas (tests) |
| 9 | **egress** for officer instances (below) | S | an instance reaches exactly the admitted CIDRs; DNS only when unfiltered is set (policy test) |
| 10 | **docs**: split officers.md into runtime / contract / built-in backend; a runnable sample | M | a fresh reader runs the sample from the doc alone |

Slices 1–4 carry no behaviour change for an HQ officer and can ship before any
builtin officer exists.

### Slice 9: officer egress

Officer instances are egress-locked today (`agentspec.claudeCodeNetworkFor`:
no public TCP ports, no cluster DNS; only the connect port to muster, the
connector door). Two opt-in, deployment-wide environment variables, read in
`internal/agentspec/claudecode.go` beside `claudeCodeNetworkFor`:

- `MUSTER_OFFICER_EGRESS_CIDRS` — a comma-separated CIDR:port list admitted
  for officer instances (TCP), e.g. Anthropic's API on 443. Empty (the
  default) changes nothing.
- `MUSTER_OFFICER_EGRESS_DNS=unfiltered` — admit cluster DNS for officer
  instances. Off by default; needed only when an admitted endpoint is named
  rather than addressed.

Both refused at boot when malformed; both documented in configuration.md with
the risk stated. What they must be able to admit together is exactly: **DNS,
Anthropic on 443, and the muster connector door** (the in-cluster muster
service, already admitted by the lock). Nothing else — in particular no
operator device: an officer pod never reaches the operator's browser bridge
(below).

### Phase-3 design intent (recorded, not built): gated outward posting

The marketing officer (a builtin officer) drafts posts; it **never reaches
the operator's browser bridge**. The bridge listens only on the operator
laptop's loopback, behind a Host allowlist and a full-power bearer token, and
the laptop's mesh firewall drops traffic from the cluster — which is the
right shape, and stays. Outward posting follows the existing discord-relay
pattern instead:

1. the officer files a **muster draft** holding the exact text and media to
   post;
2. the owner approves (or rejects) it through Attention, as a plugin verdict
   (muster#593);
3. a **laptop-side relay claims only approved drafts from muster** — the
   connection is outbound from the laptop, so nothing in the cluster can reach
   the laptop — and posts each one through the local bridge, exactly as
   approved, then reports the outcome back on the draft.

The officer can draft freely and post nothing on its own authority; the only
path to the outside world is a human's approval of the exact content, carried
out by a process on the operator's own machine.

## Risks

- **muster becomes stateful for builtin officers.** A deployment that ran
  muster's Postgres without backups because the queue lived elsewhere must
  add them before declaring a builtin officer (slice 5 says so at boot).
- **Hidden tools.** A capability a backend forgets to advertise silently
  removes a tool; the boot line (slice 5) and the ledger test are the guards.
- **Two meanings of "shadow"** (a backend's mode and the shipping officer's
  `ship.stage`): name them differently before onboarding a builtin officer.
- **The in-flight shepherd-loop change (muster#617)** adds HQ-only verbs
  (`Shadow`, `PROpened`, `MergeAsk`) and new `*officers.Client` call sites.
  Whichever of the two merges second adapts: the new verbs become optional
  interfaces with a capability, and the call sites take `officers.Backend`.

## Open questions

- Shadow suppressing builtin escalation notifications: yes on day 1 (above).
- One merged config file for builtin officers (types, charters, defs,
  schedules), or `MUSTER_OFFICERS` plus a declarative file: decided in slice 5.
- Escalation recipients beyond muster users (email, webhook): not day 1.
