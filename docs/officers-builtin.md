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

Every outward action an officer takes **through muster** passes an Attention
approval that shows the exact content: the officer files an **outbound draft**
(`officer_post_draft`), its owner approves the exact text and media, and a
plugin on the owner's own machine posts it ([§ 5b](#5b-outbound-drafts-through-a-bound-plugin)).
This holds for pod and host officers alike. On a host the machine's MCP door
names its caller by a header the caller sets — any process of that user,
the officer included, can call it as one of your sessions there — so while a
host officer is declared on a machine, every owner-authority tool reached
through that machine's door (`plugin_draft_decide`,
`connector_request_decide`, `attention_answer`, `agent_destroy`,
`chief_restart`, `dispatch_approve`, `route_reply`, `plugin_message_route`,
`plugin_request_decide`) decides nothing: it files a **confirmation card**
that you confirm on your **paired phone** (a signed action: a fresh step-up,
single use, bound to that one decision); a browser or MCP answer of the card
is refused. That step-up is what makes a decision from that machine provably
yours. Deciding in muster's own web UI (Reply drafts, Connectors, Attention)
is unchanged. Only an officer whose type grants `outbound_drafts` gets that
tool.

- **On a pod** that approval is also the only way out: its other ways out of
  its pod are muster's tools and the connectors its owner approves one request
  at a time. Do not approve a connector that can write to the outside world for
  one, and give it no `repo` with write access.
- 🔴 **On a host** (`"runtime": "host"`) the approval is the **sanctioned
  path, not an enforcement boundary.** The instance runs as the machine's
  user, with that user's credentials and network: a host marketing officer
  COULD post directly (a browser session, a CLI token, an API key in that
  user's files), and its `repo` is a pushable checkout with that user's git
  credentials. Its charter is what keeps it on the sanctioned path
  ([below](#officers-in-the-compose-stack-host-sessions)).

The empty roster (no officer declared) exposes nothing.

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

## 2b. Let its instances reach the model (egress)

An officer instance's pod is egress-locked: no public address and no DNS,
only muster's connector door. On a cluster whose policy engine filters by
name (a Cilium `toFQDNs` rule) the operator allows Anthropic there. On one
that cannot (flannel with the k3s NetworkPolicy controller), let muster write
the allow by address:

```yaml
- name: MUSTER_OFFICER_EGRESS_CIDRS        # Anthropic's published API ranges (example: check
  value: "160.79.104.0/23:443,153.61.224.0/22:443"   # https://platform.claude.com/docs/en/api/ip-addresses)
- name: MUSTER_OFFICER_EGRESS_DNS          # the API and muster's connect URL are names
  value: "unfiltered"
```

An officer pod then reaches exactly: cluster DNS, those blocks on 443, and
muster's connector door. IPv4 only; private, shared and reserved ranges and
anything wider than a /16 are refused at boot. Unfiltered DNS is a channel a
prompt-injected session could leak through by name — see
[officers.md](officers.md#on-a-cluster-without-fqdn-policies-muster_officer_egress_)
and [configuration.md](configuration.md) for the risk stated in full.

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

## Officers in the Compose stack (host sessions)

A builtin officer can run without Kubernetes: with `"runtime": "host"` each
instance is a Claude Code window that muster asks ONE of your machines to
start, through that machine's `muster-mirror` — the same host spawn the chief
uses ([design/officers-on-hosts.md](design/officers-on-hosts.md)). This is how
officers run in the Compose stack, which forwards `MUSTER_OFFICERS`.

```sh
# .env on the Compose install
MUSTER_OFFICERS=[{"id":"marketing:example","backend":"builtin","runtime":"host","host":"laptop","repo":"example-org/example-site"}]
```

On the machine named by `host`, `muster-mirror` must run with `-interactive
-agent-spawn` and the officer's `repo` on `-agent-spawn-repos` (one clone of it
under `-agent-spawn-roots`), and be current enough to close windows and to
accept an officer's first message (a muster-mirror from this version). Its
user-scope `mcpServers.muster` entry ([attention.md](attention.md#mirrored-claude-code-sessions))
is how an instance reaches its officer tools.

What happens:

- **Wake.** An item in its queue makes the wake loop start instance 1: muster
  records the spawn and the host opens a window in its `agents` tmux session,
  in a fresh worktree of `repo`, running Claude Code with the officer's first
  message (behind the preface `muster: your owner's officer runtime started
  you`), without permission prompts unless the host turned
  `-agent-spawn-skip-permissions` off.
- **Its agent row.** Each instance has an agent row of its own name that
  nothing starts or stops (no provisioner, no reaper; it is not shown in the
  agent list — the instance is its mirrored window). Its outbound drafts and
  connector grants are filed against it. It is created by the first start and
  kept when the window closes, so a draft or a grant outlives the window.
- **Tools.** The instance is offered `officer_brief`, `officer_items`,
  `officer_claim`, `officer_heartbeat`, `officer_complete`, `officer_release`,
  `officer_ask_person` and `officer_post_draft` (with the type's
  `outbound_drafts` grant), and — when the officer declares `connectors` —
  `connectors_list`, `connector_request`, `connector_status` and
  `postgres_query`, through the mirror door. Every call is bound to the Claude
  session muster recorded for that spawn, in the window it recorded, and runs
  as the instance's agent row; any other session gets a tool error. A bound
  instance gets ONLY these (and `attention_ask`). While a host officer is
  declared, every mirrored session's tool list shows these tools (a list
  carries no session); only the instance can use them.
- **The step-up gate.** The instance could still name ANOTHER of your windows
  to the machine's MCP door (the header is the caller's to set) and arrive as
  that session. So on this machine every owner-authority tool — deciding a
  draft, a connector request or a plugin's pairing, answering an ask,
  destroying an agent, restarting the chief, approving a held start,
  answering or routing a plugin message — files a confirmation card instead
  of deciding; it runs once, exactly as asked, when you answer **Confirm** on
  your paired phone. Without a paired phone, decide in muster's web UI.
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
- **Moving it.** Changing an officer's `host`, or moving it between pods and
  a host, leaves its instance rows on the old side; the officer's next start
  is refused naming the command that clears each: `muster agent rm <instance>`
  (a host row needs no provisioner; drafts and grants on it go with it).
- **Host offline.** The officer changes nothing until the host is back: its
  items wait.

Refused at boot, each named: `backend` other than `builtin`, no `host` or `repo`,
and `tiers`, `model`, `image`, `cc_account`, `cc_failover`, `read_repos`,
`llm`, `kube` (plus what every builtin officer refuses). `connectors` and
[outbound drafts](#5b-outbound-drafts-through-a-bound-plugin) work on a host.

🔴 **The security model is the machine's, not a pod's.** An instance runs as
the user `muster-mirror` runs as: that user's files, git and SSH credentials,
network and other tmux windows; no egress lock and no read-only GitHub token —
its `repo` worktree is a pushable checkout with that user's credentials, and
the first message telling it not to is prose, not a gate. muster's approval of
its drafts is the sanctioned path, not a boundary: it could post directly with
whatever that user can reach. The host's own allow-list, spawn rate
limit (`-agent-spawn-rate`, default 3 per 10 minutes and 10 a day — it counts
each wake, so raise it on that host if the officer wakes more often) and audit
log still bound what muster can make it do. Declare one only on a machine whose
user you would let run that charter unattended. The full list is in the
[design](design/officers-on-hosts.md#security-what-changes-against-a-pod).

**Check:** the boot log's `officers: <id> on: runtime host on <machine> …` line,
and the ON line's `S on hosts` count.

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

## 5b. Outbound drafts through a bound plugin

1. Grant the type the capability:
   `muster officer type put marketing --name "Marketing" --charter-file ./charter.md --policy '{"capabilities":["outbound_drafts"]}'`.
2. Mint (or [pair](plugins.md#pairing-a-plugin-without-handling-its-token)) a
   plugin with `messages` and `draft_checks`, and bind it:
   `deliver_to = officer:marketing:example`
   ([plugins.md](plugins.md#a-plugin-bound-to-an-officer)). The binding is
   refused unless the officer is defined for the same owner.
3. The plugin's messages (`kind`: `plan`, `reply`, `snapshot`, `note`) arrive
   as `plugin.<kind>` items with the message fenced as untrusted.
4. The officer files `officer_post_draft{plugin, venue, target, body, media?,
   item_id?}`. The plugin checks the draft and uploads each medium's bytes;
   the owner sees the exact text, images and videos on the Reply drafts tab,
   and approval binds the body and every medium's SHA-256. The plugin claims
   it, re-hashes its files, posts, reads the post back and reports the result.
5. With `item_id`, the item **waits on the draft** (no holder; the instance
   cannot complete or release it). Posted: muster completes it with the
   post's reference. Refused by the plugin, or rejected by the owner (with an
   optional reason, on the card or `plugin_draft_decide`): muster queues a
   `draft.refused` item whose `detail` says why, and finishes the waiting item
   `no_action` naming it — at most 3 `draft.refused` items wait at once
   ([plugins.md](plugins.md#the-result)). Put what the officer should do with a
   `draft.refused` item (read `detail`, redraft with its own `item_id`) in its
   charter.

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
