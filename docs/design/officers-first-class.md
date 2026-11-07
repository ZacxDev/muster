# Officers as a first-class feature (pluggable backend)

**Status: proposed.** This is a design to read before any of it is built.
Each phase below lands as its own PR.

[officers.md](../officers.md) is the as-built reference: today an officer's
identity, charter, queue, leases and memory live in an **external backend**
(for example a team console) that muster calls over HTTP, and muster runs the
officer's instances. This proposal makes the officer contract muster's own,
keeps external backends working unchanged, and adds a **built-in backend** in
muster's Postgres so a deployment with no external console can run officers.
When a phase lands, its as-built facts move into officers.md and the reasoning
stays here.

Line numbers are against `origin/main` at `22ffad47`.

## Goal and non-goals

**Goal.**

- muster owns and documents an **officer-backend contract** (`officer-backend/v1`):
  types and charters, the item queue, leases, claim / brief / complete /
  escalate / release / ask, tiers, memory, and capability negotiation.
- muster ships a **built-in backend** implementing that contract in its own
  Postgres, with webhook and schedule triggers and escalation through muster's
  own attention/notify surface.
- An existing external backend becomes one implementation of the contract,
  with no renames on its side: it conforms by addition.

**Non-goals.**

- Moving any deployment's domain triggers (alert baselines, release ingestion,
  incident correlation) into muster. Those stay in the backend that owns the
  domain; the built-in backend offers a generic ingress instead.
- Changing the runtime: wake/scale, instance pods, tool hosting, read budgets,
  the LLM proxy, ship gates and their credentials stay as they are.
- A hosted multi-tenant backend.

## What exists today

| | lives in |
|---|---|
| officer types, charters, officers, the item queue, leases, memory, triggers, escalation delivery, shadow flag, schedules, observability relay | the external backend |
| wake loop and scaling, instance pods and accounts, MCP officer tools, kube reads, LLM proxy and budgets, ship gates, transcript forwarding | muster |

muster's whole backend dependency is **one concrete type**, `officers.Client`
(`internal/officers/hq.go:159-765`, plus `obs.go:74-135` and `asks.go:63-76`),
whose generic `Do(method, path, body)` (`hq.go:164`) has 17 call sites:

| verb | method + path (under the officer's base URL) | site |
|---|---|---|
| items | `GET /officers/{id}/items?status=&tier=&cursor=` | `hq.go:291` |
| brief | `GET /officers/{id}/brief` | `hq.go:335` |
| claim | `POST /officers/{id}/items/{item}/claim` `{instance, lease_seconds, tier}` | `hq.go:366` |
| claim-next | `POST /officers/{id}/claim-next` (same body) | `hq.go:381` |
| heartbeat | `POST …/items/{item}/heartbeat` `{instance, lease_seconds}` | `hq.go:386` |
| complete | `POST …/items/{item}/complete` `{instance, outcome, summary, links, investigation?}` | `hq.go:480` |
| release | `POST …/items/{item}/release` `{instance}` | `hq.go:485` |
| transcript | `POST …/items/{item}/transcript` `{instance, events[]}` | `hq.go:528` |
| asks | `POST …/items/{item}/asks`; `POST /officers/{id}/ask-images` | `asks.go:63,76` |
| obs | `GET /officers/{id}/obs/queries`; `POST …/obs/query`; `POST …/obs/raw` | `obs.go:74,80,135` |
| officer | `GET /officers/{id}` (`enabled`) | `hq.go:710` |
| notice | `POST /officers/{id}/notices` | `hq.go:739` |
| preflight | `POST /officers/{id}/preflight` | `hq.go:756` |
| release captain | `POST /releases/captain` | `hq.go:692` |

Two properties make this a de-facto contract rather than a designed one:

1. **Pass-through bodies.** Brief, claim and items return the backend's raw
   body (`[]byte`) and the MCP tools hand it to the instance unchanged
   (`internal/api/officers.go:1687`). The instance-visible schema is therefore
   whatever the backend emits.
2. **Semantics live only in the fake.** `internal/officers/hqtest/hqtest.go`
   implements the lease rules (409 to any claim on a live lease, the holder's
   own included; an expired lease claimable by anyone; 410 on a finished item;
   403 across officers), the transcript rules and the asks rules. It is the best
   existing specification and becomes the conformance suite below.

A missing route is discovered at call time: claim-next treats any 404 as "an
older backend" (`internal/api/officers.go:1481`), even though 404 also means
an unknown officer.

## The contract: `officer-backend/v1`

HTTPS, JSON, a bearer token per officer (unchanged). The existing paths are
the v1 wire format, so a backend already serving them conforms by adding the
capabilities route.

### Capabilities

`GET /officers/{id}/capabilities`

```json
{
  "contract": "officer-backend/v1",
  "capabilities": ["queue", "tiers", "memory", "events", "investigation",
                   "transcript", "asks", "obs.registry", "obs.raw",
                   "notices", "preflight", "schedules", "triggers.webhook"],
  "extensions": ["x-captain"],
  "limits": {"lease_seconds": [30, 3600], "summary_chars": 24576,
             "links": 10, "brief_bytes": 32768, "transcript_events": 100}
}
```

muster reads it at boot and after a fault. A tool whose capability is absent
is **not offered**, and its kickoff note is not sent; the boot log prints the
offered tool set per officer, so a missing capability is loud. A backend that
does not serve the route is treated as "core only, v1 limits" so current
backends keep working before they add it.

### Core verbs (MUST)

`items`, `brief`, `claim`, `claim-next`, `heartbeat`, `complete`, `release`,
`officer`. These are enough to run an officer.

### Optional verbs (capability-gated)

`transcript`, `asks` (+ images), `obs.*`, `notices`, `preflight`, schedule and
trigger administration. Product-specific verbs (for example a release
evaluation write) are **extensions** (`x-…`), shipped by muster as optional
tool packs that a backend switches on by advertising them.

### Semantics

| topic | rule |
|---|---|
| item | `{id, officer_id, kind, subject_key, memory_key?, payload, status, tier, priority, not_before?, claimed_by?, claimed_at?, lease_expires_at?, attempts, outcome?, summary?, links?, created_at, updated_at}`; ids are integers or strings |
| dedupe | at most one live item (open, claimed or waiting) per `(officer, kind, subject_key)`; an enqueue that matches a live item returns it |
| status | `open -> claimed -> done / escalated` (outcomes `done`, `no_action`, `escalated`); lease expiry makes a claimed item READ open (no reaper needed); `release` -> open; `waiting` while an ask to a person is outstanding; `scheduled` is the read view of an open item before `not_before` |
| lease | `lease_seconds` within `limits.lease_seconds`; heartbeat renews; 409 `held` to any claim on a live lease (muster turns the holder's own into a heartbeat); 409 `lease_lost` on heartbeat/complete/release without it; 410 `gone` on a terminal item; 403 across officers |
| queue order | blocked last; priority `urgent > high > normal > low`; then `not_before`; then id. `claim-next` with `tier` returns only that tier, `{item: null}` when none |
| tiers | `routine` / `judgement`, default judgement (fail toward the stronger model). A routine holder may complete `needs_judgement`: the item reopens at tier judgement, same id, attempts kept, the routine summary carried. A routine item that lapsed twice reads judgement |
| claim answer | `{item, lease_expires_at, memory?, event?}`; `memory = {history[], open_threads[], open_threads_total}` is backend-computed and bounded |
| brief | `{officer, type{key, name, charter, charter_version}, open_items, recent_items, generated_at, truncated?}` within `limits.brief_bytes`. The charter reaches the instance only here; muster holds no copy |
| investigation | optional structured block on complete: `{hypotheses[], eliminated[{hypothesis, evidence, audit_ids}], root_cause, confidence, fix{pr_urls or operator_steps}, not_checked[]}`; a type may require it on `escalated` |
| mode | `mode: live` or `shadow` per officer, with a defined effect on every side-effecting verb: in shadow, escalations and reports are recorded but nobody is notified, and extension writes are refused (409 `officer_shadow`). Today's backends define shadow more narrowly; the contract makes the effect explicit |
| errors | `{code, message}` with a closed vocabulary: `held, blocked, not_due, wrong_tier, waiting, lease_lost, officer_disabled, gone, must_escalate, officer_shadow, not_claimed, not_investigating, unknown_recipient`. 4xx is a decision, 5xx or transport is retryable, a 5xx carrying a declared code is a decision (today's `HQError` / `ErrHQUnavailable` / `RetryableError` split, `hq.go:164-200`) |
| idempotency | complete on a terminal item never double-applies; transcript batches are idempotent on `seq`; trigger deliveries are idempotent on a delivery id within the dedupe window |
| filters | `items` accepts `claimed_by=` so the one-claim checks need not scan every page (`AllItems` stops at 500, `hq.go:313-331`) |
| versioning | additive fields only within v1; muster ignores unknown fields; a breaking change is v2, served side by side |

### In muster: the interface

```go
// internal/officers/backend
type Backend interface {
    Capabilities(ctx context.Context) (Caps, error)
    Officer(ctx context.Context) (OfficerState, error) // enabled, mode
    Items(ctx context.Context, q ItemsQuery) (ItemsPage, error)
    Brief(ctx context.Context) (Brief, error)
    Claim(ctx context.Context, item ItemID, r ClaimReq) (Claimed, error)
    ClaimNext(ctx context.Context, r ClaimReq) (*Claimed, error)
    Heartbeat(ctx context.Context, item ItemID, instance string, lease int) error
    Complete(ctx context.Context, item ItemID, instance string, c Completion) (Item, error)
    Release(ctx context.Context, item ItemID, instance string) error
}

// optional, discovered by capability
type Transcripts interface{ PostTranscript(/* … */) (TranscriptAck, error) }
type Asks interface{ CreateAsk(/* … */); UploadAskImage(/* … */) }
type Obs interface{ ObsQueries(/* … */); ObsQuery(/* … */); ObsRaw(/* … */) }
type Notices interface{ PostNotice(/* … */) error }
type Preflight interface{ Preflight(/* … */) (PreflightAnswer, error) }
```

Two implementations: `backend/http` (today's `Client`, same wire format, any
conforming server) and `backend/builtin` (muster's Postgres). The wake loop
already depends on a thin adapter (`ClientHQ`, `wake.go:110-147`), so the seam
exists. Tools decode backend answers into these types and re-encode them, with
unknown fields preserved under `extra`, so the instance-visible schema becomes
muster's.

## The built-in backend

### Day 1

- **Tables**: `officer_types(key, name, charter, charter_version, policy)`,
  `officers(id, type_key, area, owner, enabled, mode, escalate_to[])`,
  `officer_items(…the item fields…, routine_summary, investigation)`,
  `officer_schedules(officer_id, name, cron, kind, payload, skip_if_live, enabled, next_fire_at)`,
  `officer_notices(item_id, recipient, sent_at)`.
- **Queue**: dedupe index on live items; claim-next as one
  `UPDATE … WHERE id = (SELECT … ORDER BY <queue order> FOR UPDATE SKIP LOCKED LIMIT 1)`;
  tiers, priority, `not_before`, `needs_judgement` and lapse promotion as above.
- **Brief** from the type's charter and the officer's open and recent items.
- **Memory**: history by `memory_key` (a per-kind template in the type policy;
  default `kind:subject_key`) plus open escalations, bounded in bytes.
- **Triggers**:
  - a signed webhook, `POST /officers/{id}/triggers/{name}`, HMAC-SHA256 over the
    body with a per-officer secret, `subject_key` from a template, priority and
    tier from the trigger, idempotent on the delivery id;
  - cron schedules (five fields, UTC) with `skip_if_live`; missed fires collapse to one.
- **Escalation**: an `escalated` complete notifies the owner and `escalate_to`
  through muster's attention/notify surface (the Attention view and phone
  push), once per item per recipient, and is written in the same transaction as
  the complete (a notice never fails a complete). Follow-on escalations for the
  same subject within a window are coalesced into one message.
- **Mode**: shadow records escalations and reports without notifying.
- **Admin**: a declarative `officers.yaml` (types, charters, officers,
  triggers, schedules) applied at boot; resolve-an-escalation from the UI.
- **No token**: an in-process backend needs no per-officer bearer token; the
  officer binding stays muster's existing per-agent check.

### Later

Event grouping (lead and joined items, supersede), investigation enforcement,
asks to people, a transcript store and viewer, an observability source
(Prometheus/Loki URL and a query registry), payload materialisers run at
`not_before`, notices and preflight for shipping officers, cost per item.

### Not in scope

Domain-specific triggers and their gates (alert baselines, release
ingestion), and product-specific extension verbs.

## Migration plan

Every step keeps officers on an external backend working; that backend's wire
format is never renamed. S is up to 300 changed non-test lines, M up to 1,000,
L more.

| step | change | size | closes when |
|---|---|---|---|
| 0 | this design | S | the design PR is approved or edited |
| 1 | extract `officers/backend`; `backend/http` is today's `Client` moved; reconciler, tools and ship gates depend on the interface (pure refactor) | M | merged with the officer tests green in CI, and a deployment on the new image shows every `officers: <id> on:` boot line and a claimed item afterwards |
| 2 | typed decode and normalise brief, claim and items; unknown fields kept in `extra` | M | golden tests on recorded backend answers show identical tool output or only documented additions; a 24 h soak shows no new officer tool errors |
| 3 | conformance suite from the `hqtest` semantics: a Go package that takes a base URL and a token | M | green against the fake in CI; a seeded fault in the fake (a double claim allowed) turns it red |
| 4 | an external backend adds `GET /officers/{id}/capabilities` (done in that backend's own repository) | S | the suite's read-only verbs pass against it with a dedicated test officer |
| 5 | capability-gated tools and kickoff notes; drop the claim-next 404 sniff; move product verbs into extension packs | M | the boot log lists capabilities and the offered tools per officer; a backend without `obs` offers no obs tool (test) |
| 6 | built-in backend core: migrations, queue, leases, tiers, brief, memory, resolve; `"backend": "builtin"`; `officers.yaml` | L | the conformance suite passes against `backend/builtin` in CI; a dev deployment runs one officer end to end (item, pod, claim, complete) |
| 7 | built-in triggers: signed webhook and cron schedules | M | a duplicate delivery makes one item; a schedule fires once across a restart (tests) |
| 8 | built-in escalation and mode | M | one notification per recipient per escalated item; zero in shadow (tests) |
| 9 | docs: split officers.md into runtime, contract and built-in backend; a runnable sample | M | a fresh reader deploys the sample to a local cluster from the doc alone |

Steps 1 to 5 carry no new behaviour for existing deployments and can ship
before the built-in backend exists.

Risks:

- **Schema drift through pass-through** (step 2): charters and prompts quote
  backend field names (`memory`, `event`, `investigation`). Keep the names;
  golden-test them.
- **muster becomes stateful.** A deployment that ran muster's Postgres without
  backups because the queue lived elsewhere must add them before using the
  built-in backend.
- **Two meanings of "shadow"** (a backend's flag and the shipping officer's
  `ship.stage`): name them differently before onboarding anyone.
- **Hidden tools**: a capability the backend forgets to advertise silently
  removes a tool; the per-officer boot line is the guard.
- **Scope creep** into domain triggers: kept out by the generic webhook.

## Onboarding example: "acme"

A generic project with no external backend: one type `ci-triage`, one officer
`ci-triage:acme-api`, a webhook trigger from failed CI runs and a nightly schedule.

`officers.yaml`:

```yaml
types:
  - key: ci-triage
    name: CI triage
    charter_file: charters/ci-triage.md
    policy:
      tiers: {ci-triage.failure: routine, ci-triage.nightly: routine}
      memory_key: "{{kind}}:{{payload.workflow}}"
officers:
  - id: ci-triage:acme-api
    type: ci-triage
    owner: alice
    escalate_to: [bob]
    mode: shadow
    area: {repos: [acme/api]}
    triggers:
      - name: ci-failure
        kind: ci-triage.failure
        subject: "{{payload.workflow}}:{{payload.branch}}"
        dedupe_window: 30m
    schedules:
      - name: nightly
        kind: ci-triage.nightly
        cron: "0 6 * * *"
        skip_if_live: true
```

The charter (`charters/ci-triage.md`) states the mission (classify each failed
run as flake, infrastructure or a real regression), the authority (read the
repository, never push), when to complete `no_action` (a known flake with a
memory hit), `done` (a one-line classification) or `escalated` (a regression on
`main`, with the failing test, the root cause and a suggested owner), and what
to say when a check could not be made.

Runtime (`MUSTER_OFFICERS`, unchanged shape):

```sh
MUSTER_OFFICERS='[{"id":"ci-triage:acme-api","backend":"builtin","repo":"acme/api",
  "tiers":{"routine":{"model":"<provider model>","max_instances":1,"fresh_session_per_item":true},
           "judgement":{"model":"opus","max_instances":1,"max_sessions_per_day":10,"max_item_minutes":30}},
  "llm":{"daily_budget_usd":3}}]'
MUSTER_OFFICER_OPENROUTER_KEY_CI_TRIAGE_ACME_API=…   # routine pool's provider key
MUSTER_OFFICER_TRIGGER_SECRET_CI_TRIAGE_ACME_API=…   # webhook HMAC, shared with CI
```

CI posts `POST /officers/ci-triage:acme-api/triggers/ci-failure` with
`X-Signature: sha256=<hmac>` and `{"workflow":"test","branch":"main","run_url":"…"}`.

The operator provisions: muster with a **backed-up** Postgres; the connect
listener, the agents provisioner, a namespace-confinement admission policy and
the per-instance network policies (as officers.md describes); a Claude account
for the judgement pool and a credit-limited provider key for the routine pool;
a read-only GitHub App on `acme/api`; the trigger secret; `officers.yaml` and
the charter in a ConfigMap. Nothing outside the cluster.

### What the example shows is awkward

- **Per-officer tokens are meaningless in-process.** The built-in backend must
  not require `MUSTER_OFFICER_TOKEN_<ID>`.
- **Two config homes.** `MUSTER_OFFICERS` and `officers.yaml` both name the
  officer and both carry tier facts (which kinds are routine; which model runs
  each pool). For `builtin`, one merged file is simpler; keep the split for
  external backends.
- **Recipients are backend principals.** Escalation targets must be muster
  users for `builtin`; the contract should say "a principal the backend can
  resolve", not a particular login namespace.
- **Instance names derive from the id** and must fit a namespace under the
  prefix (`internal/officers/config.go:316-318`); long project names hit the
  63-character ceiling fast.
- **No observability source** by default, so the charter must not mention the
  obs tools; capability gating (step 5) makes that safe.
- **The kickoff assumes notification** ("the backend notifies your owner");
  that sentence must follow the escalation capability.

## Improvements found while surveying (muster)

- Kube reads spend the per-item read budget before the read and never refund
  on refusal, mint or transport failure, while the obs tools refund
  (`internal/api/officer_kube.go:218-229`, `internal/api/officer_reads.go:99`).
- The one-claim checks scan `AllItems`, which stops at 500 items
  (`hq.go:313-331`, `internal/api/officers.go:1508-1523`); the `claimed_by`
  filter above removes the scan.
- Wake passes run serially with a two-minute timeout each, so one slow backend
  call delays every officer (`wake.go:1089,1106-1112`).
- Officer MCP calls are logged only as `POST /mcp 200`; log the tool, officer,
  instance, item and backend status per call.
- officers.md "The API muster consumes" omits claim-next and `obs/raw`, and the
  Deploying list skips a step number.
- Two glob dialects for paths (`ship.go:499` vs `ship_v2.go:331`) and the
  secret redactors on officer paths (`internal/kuberead/redact.go`, and
  `RedactText` / `Redact` in `internal/officership/cilogs.go:434,516`) should
  each become one.

## Open questions

- Does phase 1 stop after step 5 (contract and conformance), or include the
  built-in backend (steps 6 to 8)?
- Should shadow suppress escalation notifications? The contract above says yes.
- Do product-specific write verbs stay extensions, or does a generic
  "post an evaluation" verb earn a place in the core?
- For `builtin`: one merged config file, or `MUSTER_OFFICERS` plus `officers.yaml`?
- Escalation delivery on day 1: muster users only, or also email and webhook targets?
