# Officers on hosts: officer instances as host-spawned sessions

**Status: built in this pull request (round 1 after the round-0 audit).**
Operator decisions (2026-11-13):

- officers run in the Docker Compose stack as **host-spawned Claude Code
  sessions** — a tmux window on one opted-in machine, started through that
  machine's `muster-mirror` — rather than through a Docker-socket provisioner;
- **"Add it in this PR"**: each host instance gets an **agent row that nothing
  starts or stops**, so `officer_post_draft` and `connectors` work on a host;
- **"Yes, opt-in is enough"**: declaring `"runtime": "host"` is the owner's
  consent for muster to close that officer's recorded instance windows to
  scale to zero (see [Closing a window](#closing-a-window-the-exception-to-d7)).

Symbols are named rather than line-numbered: the code moves weekly.

## Why

An officer instance today is a pod: `officerFleet` creates an agent row and
the Kubernetes provisioner runs it ([officers.md](../officers.md)). The
Compose stack has no provisioner, so `MUSTER_OFFICERS` is not even forwarded
there and the built-in backend ([officers-builtin.md](../officers-builtin.md))
— whose whole point is "no HQ" — still needs a cluster. muster already starts
Claude Code on its owner's machines: the chief's host spawn
([chief-dispatch.md](chief-dispatch.md) §3) cuts a worktree and opens a window
in the `agents` tmux session, and every window reaches muster's MCP through the
host's loopback endpoint. An officer instance needs nothing more than that.

## The mapping

| officer runtime concept | pod runtime (`runtime: "pod"`, the default) | host runtime (`runtime: "host"`) |
|---|---|---|
| where an instance runs | a Deployment in the cluster | a window in the `agents` tmux session on the machine named by `host` |
| what starts it | `officerFleet.Start` → agent row + provisioner | `officerFleet.Start` → an `officer_host_spawns` row + `agent-spawn` to that host |
| working directory | the pod's workspace, no checkout | a fresh worktree of `repo`, cut by the host from its own clone (chief-dispatch §3.3) |
| instance identity | the agent row's name `officer-<slug>-<n>` | an agent row of the same name that nothing provisions (`agents.runs_on_host`, below), plus the spawn row; the window is bound by muster's record, never by a tag |
| first message | the kickoff, delivered by the kickoff deliverer through ccd | the same kickoff (`KickoffText`), sent as the spawn's prompt behind `mirror.OfficerSpawnPreface` |
| a new turn for an idle instance (`Reprompt`) | ccd's gateway, under the session claim | a paste into its window, only while the host reports it idle (`AgentState`), stamped with the spawn id (`mirror.ForDispatch`) |
| scale to zero (`Stop`) | provisioner Stop (agent and volume kept) | `agent-close` on that window, asked only while the host reports it idle in the recorded conversation: `/exit` at the empty idle prompt, the window closed 30 s later if Claude Code has not exited; a window not at an empty idle prompt when the close arrives is closed at once; the host then removes the worktree if it is clean |
| up / down (`Instances`) | the agent row and the provisioner's instances | the latest spawn row per instance, against the host's live listing: a recorded window the host lists with that spawn's id is up; a row still waiting for its reply is up for 180 s (the chief's pending countdown); anything else is down |
| officer MCP tools | the agent door (`/mcp`), bound by the agent's own token | the mirror door (`/mirror/mcp`), bound on EVERY call by the caller's (host, Claude session) to a spawn row, then to the caller's window as listed — the rule `agent_report_done` uses (`callerHostDispatch`) — then to the instance's agent row |
| outbound drafts, connector grants | filed against the agent row | the same: filed against the instance's agent row |

**The policy is not duplicated.** Both runtimes are one `officers.Fleet`
(`officerFleet`) handed to one `officers.Reconciler`: `Decide`, the backoff,
the switch, the wall-time cap and idle scale-to-zero run unchanged, and
`officerFleet` branches on `Officer.OnHost()` — the one predicate — only to
pick how an instance is moved. The officer tools on the mirror door go through
the same `dispatchOfficerTool` as on the agent door, so `officerToolServed`,
the refusal to name another officer, the claim locks and the backend's own
checks are the same code. A seam test pins both.

### Which machine

A per-officer `host` field names one mirror host of the officer's owner (the
name its mirror token was minted for). That host must be connected, run
`-interactive -agent-spawn`, list the officer's `repo` on its
`-agent-spawn-repos` allow-list, and advertise `agent-close`; each of those is
the host's own setting, so the machine opts in, not muster. One host per
officer in v1: no failover between machines.

### The instance's agent row

Every instance has an `agents` row named like a pod instance's, created by its
first start (`ensureHostInstanceRow`) with `runs_on_host` = its host
(migration 0125). It exists so that what a pod instance files against its
row — an outbound draft (the drafts table's trigger requires an agent id) and
a connector grant (granted per agent, decided per request) — works unchanged.

- **Nothing provisions it.** `agentprovision.Adapter` refuses every lifecycle
  call on it (Dispatch, Start, StartIfLive, Stop, Destroy, ReapplyProfiles)
  before any driver call (`refuseHostRow`). The stopped-agent reaper's
  predicate excludes it. `agents_runs_on_host_check` keeps it out of every
  cluster shape: no namespace, no Claude account (so the gateway kind), no
  worker pool.
- **Every consumer that iterates agent rows is classified.** A test finds them
  by TYPE (every call of `agents.Store.List` / `AgentsByNoteIDs` in the
  module) and holds a ledger: each one either skips the row
  (`agents.ProvisionedOnly`, checked in its body) — the loops that act (the
  kickoff deliverer, the status poller, the memo relay, the watchdog, usage,
  stall nudges, focus) and the lists shown to a person (the agent list and
  sidebar, the palette, the chief's fleet and session lists, the fleet report)
  — or only reads it in a way it cannot mislead (lookups by name, task or
  pool). The ledger fails when the set grows or shrinks.
- **Lifecycle: created on the first spawn, KEPT on scale-to-zero, never
  deleted by muster.** A pending outbound draft and the instance's connector
  grants reference the row; deleting it at every scale-to-zero would cascade
  them away while the owner may still be deciding, and recreate a grant the
  owner already decided. Its stored status stays `stopped`: the row is never
  provisioned; the instance's liveness is the window (the spawn row against the
  host's listing). It is not shown in the agent list; the instance is seen as
  its mirrored window.
- **A row of that name that is not this host's** (a pod instance's left from
  before the officer moved, or another host's) is refused at start, naming
  the command that clears it — never adopted or rewritten. A pod officer over
  a host row is refused the same way, before the provisioner.
- **Removing one** (round 1's 🟡2): `destroyAgent` tears a host row down as
  the row alone (`destroyHostRow`, never the provisioner), on every door —
  `muster agent rm <name>` (`DELETE /api/agents/{name}`, which needs no
  provisioner for a host row), `agent_destroy`. The declared officer's OWN
  row (its current runtime and host) stays delete-protected; a row left from
  a move or an undeclared officer is not.
- **A `/clear` in the window** (round 1's 🟢3): the fleet's read follows the
  recorded window's new conversation (`followHostSpawnSession`, the spawn's
  fourth writer), so re-prompt and close work again instead of wedging. The
  tool binding follows it from that read on (within one poll).

### Lifecycle

1. **Wake.** A pass finds claimable items with no instance up; `Decide` says
   `Start(n)`. `officerFleet.Start` inserts the spawn row (its id is drawn from
   the host-dispatch sequence, so it can never equal a chief dispatch's id on
   the wire, in a worktree path or in a branch name) and sends `agent-spawn`
   in the background, so a 150 s host wait never holds the pass. The next pass
   already counts the instance up (pending).
2. **Reply.** The host answers with the window and the Claude session id it
   chose; muster records both on the row. A late reply (after the 150 s wait)
   arrives on the hub's late-reply hook and is recorded the same way. A
   refusal is recorded and the instance reads as down on the next pass, which
   may start it again (the host's own rate limit bounds that).
3. **Work.** The instance calls `officer_brief`, `officer_claim`… through the
   host's MCP endpoint. muster binds each call to the spawn row and the
   instance's agent row.
4. **More work while up.** `Decide`'s `Reprompt` pastes the re-prompt into the
   window, only when the host reports the session idle; otherwise the pass gets
   `ErrInstanceBusy` and tries again.
5. **Idle.** After `idle_scale_to_zero_after` with nothing claimable and
   nothing held, `Stop` closes the window through `agent-close`. A session that
   does not exit within the grace has its window closed. The agent row stays.
6. **Host offline.** `Instances` fails blind — "cannot see" is not "nothing is
   up" — so the pass changes nothing and backs off, as an unreadable fleet does
   for a pod officer.

### Tokens and tools

- **No token reaches the session.** A builtin officer has no token at all; the
  spawn carries only the kickoff. The window gets exactly what every
  host-spawned window gets: the host's user-scope `mcpServers.muster` entry and
  its per-host loopback secret (chief-dispatch §3.5). muster adds nothing to
  its environment.
- **The officer tools are the queue tools, `officer_ask_person` and
  `officer_post_draft`** (the type's `outbound_drafts` grant read at call
  time, as on pods). `officers.ToolOffered` says so for every host officer, so
  the kickoff, the listing, the boot line and the dispatch agree.
- **The connector tools** (`connectors_list`, `connector_request`,
  `connector_status`, `postgres_query`) run as the instance's row through the
  agent door's own handlers: the officer's connector rule, each request decided
  by the owner, never a shared approval. Not `github`: it is used through the
  HTTP proxy with the agent's token, which never reaches a host session, so
  `MayRequestConnector` refuses it on a host (the instance reads its
  repository from its worktree). `postgres_query` runs server-side, so no
  credential reaches the session either.
- **A bound instance is not its owner's session.** The mirror door gives the
  owner's own sessions operator tools (`attention_answer`,
  `connector_request_decide`, `plugin_draft_decide`, `agent_destroy` …). A
  call bound to a host instance runs ONLY its own tools (the officer tools, the
  connector tools, `attention_ask`/`_wait`/`_cancel`); anything else is
  refused naming the instance (`hostOfficerMirrorTool`). **That is not a
  boundary by itself** (round 1's 🟡1): the machine's MCP door names its
  caller by a header the caller sets, so the instance can name another of the
  owner's windows and arrive unbound, as that session. The boundary is the
  step-up gate below.
- **The step-up gate** (operator decision, 2026-11-13; `host_stepup.go`).
  While any host officer of the owner is declared on a machine, every
  owner-authority tool reached through that machine's mirror door — the
  ledger `hostStepUpTools`: `plugin_draft_decide`,
  `connector_request_decide`, `attention_answer`, `agent_destroy`,
  `chief_restart`, `dispatch_approve`, `route_reply`, `plugin_message_route`,
  `plugin_request_decide`; every other mirror tool is classified exempt with a
  reason, and the test fails when a new one is neither — decides nothing. It
  files a **confirmation card** naming the tool and its exact arguments; the
  decision runs once, as the session that asked, only when the owner answers
  **Confirm** from their **paired phone**: a signed action whose challenge is
  single use, lives two minutes and is bound to that card's id and the exact
  answer (`device_actions.go`), so a step-up for one decision cannot be
  replayed for another, and there is no time window an officer could ride. A
  browser answer of the card, an MCP answer (from any machine) and an answer
  after a restart lost the pending decision are refused. Deciding in muster's
  web UI is unchanged; a machine with no host officer is unaffected.
- **The mirror door lists these tools to every session of the owner's hosts
  while a host officer is declared** (the connector tools only when one
  declares a connector) — `tools/list` carries no session (the same reason
  `agent_report_done` is listed for every session; round 0's D2, kept). A call
  from a session that is not a host officer's instance gets a tool error naming
  why.

## What works on the host runtime, and what is refused at boot

Works: the built-in backend's queue, leases, the switch, schedules, asks to
the owner through Attention, escalations, **outbound drafts**
(`officer_post_draft` → the owner's Attention approval of the exact text and
media → the bound plugin posts), **`connectors`** (each request approved by the
owner; `postgres_query`), `max_instances` (one window each),
`idle_scale_to_zero_after`, `poll_interval`, `owner`, `repo` (now a pushable
worktree: see Security).

Refused at boot, each naming the field and why (`officers.parseRuntime`):

| field | why it is refused with `runtime: "host"` |
|---|---|
| `backend` other than `builtin` | v1. An HQ officer's usage, transcript and pools ledgers read the instance through its pod's ccd; on a host they would be silently empty |
| `host` missing, or not a mirror host name | there is nowhere to start it |
| `repo` missing | the host cuts every spawned window's worktree from a clone of a named repository; there is no repository-less spawn |
| `tiers` | each pool runs its own model, and the host spawn of Claude Code takes no model |
| `model` | the same: the host's Claude Code runs its own default |
| `image`, `cc_account`, `cc_failover` | pod and account-pool settings; the host runs its own `claude` on its own login |
| `read_repos` | further repositories are read through the `github` connector's HTTP proxy, which takes the agent's token in the session's environment, and none is put there; read them from a clone on the host |
| `llm` | the LLM proxy routes a pod's Claude Code through it (`CCD_LLM_ROUTE`), and it needs `tiers` |
| `kube` | kube reads are the judgement pool's, and need `tiers` |
| `ship`, `ci_logs`, `transcript` | already refused for every builtin officer |
| `runtime` other than `pod` or `host` | unknown runtime |

And at `EnableOfficers`: a host officer with no mirror hub, no host spawn
store or no agents store is refused, each named. A roster whose officers are
all on hosts no longer needs a provisioner or the `claude-code` kind; a pod
officer beside it still does.

Not offered on a host (absent from the tool list and the kickoff, never
silently broken): the obs tools (the built-in backend serves none anyway), and
every ship, kube, task and gap tool; and the `github` connector.

## Security: what changes against a pod

State it plainly; the operator chose this knowing it.

- **It runs as the user on that machine.** No pod, no namespace, no
  NetworkPolicy, no egress lock. The session can read every file that user
  can, use their git and SSH credentials, reach anything their network can,
  and type into their other tmux windows. A pod officer's whole isolation story
  ([officers-builtin.md](../officers-builtin.md#-before-you-declare-one-on-a-real-deployment))
  does not apply.
- **Its `repo` is a pushable checkout with the user's credentials, not a
  read-only token.** A pod officer's GitHub access is a read-only App token
  minted per request; a host officer works in a real worktree of `repo` and can
  push with whatever credentials the user's git has. The kickoff tells it not
  to push; that is prose, not a gate.
- **muster's approval is the SANCTIONED path, not an enforcement boundary.**
  `officer_post_draft` puts every post behind the owner's Attention approval of
  the exact text and media, and on a host officer's machine the step-up gate
  makes an approval reached through that machine's MCP door provably the
  owner's (their phone's signed confirm), however the caller named itself.
  But the session runs with the machine user's credentials and network: a host marketing officer COULD post directly
  (a browser session, a CLI token, an API key in that user's files). On a pod,
  the missing credentials and the egress lock make the approval the only way
  out; on a host nothing does. The charter is what keeps it on the sanctioned
  path.
- **It runs without permission prompts** by default (the host's
  `-agent-spawn-skip-permissions`, on unless the host turns it off).
- **Its tool identity is a selector bound to muster's record, not a
  credential.** The binding is the caller's Claude session on the caller's host
  (muster recorded both from the host's spawn reply) AND the caller's window
  as listed. Another process running as the same user on that host can type
  into that window and so act as the instance — exactly the reach any process
  of that user already has over that user's sessions.
- **What still bounds a compromised muster-server:** the host's own
  `-agent-spawn-repos` allow-list, its spawn rate limit (`-agent-spawn-rate`,
  default 3 per 10 minutes and 10 a day — it counts officer wakes too; raise it
  on that host if an officer wakes more often) and its append-only audit log;
  the host types a re-prompt only into the window carrying that spawn's id; it
  closes only a window it lists running the conversation muster names.

Declare a host officer only on a machine whose user you would let run that
charter unattended.

## Closing a window: the exception to D7

chief-dispatch decision 17 / D7 says muster never closes a host-spawned window
(the owner closes it), and the 2026-11-09 rule (chief-in-agent-view) says
nothing about one of the owner's sessions is ended without the owner's tap.
**A host officer's scale-to-zero is the one exception** (operator decision
2026-11-13, "Yes, opt-in is enough"): declaring `"runtime": "host"` is the
owner's consent for muster to close **only that officer's recorded instance
windows** — the window muster recorded from the spawn reply, running the
conversation it recorded — through the existing `agent-close`, asked only
while the host reports the window idle (`/exit` at the empty idle prompt,
closed 30 s later if it has not exited; closed at once if it is not at an
empty idle prompt when the close arrives). No other window,
and no chief-dispatched window, is ever closed this way. Recorded in
[decisions.md](../decisions.md#a-host-officers-window-is-the-one-window-muster-closes-by-itself).

## Resolved questions

1. **Closing an officer's window by itself:** yes, by opt-in (above).
2. **`officer_post_draft` on a host:** built — the instance's agent row
   (option b), with the consumers ledger and the adapter's refusal as its
   guards.
3. **HQ officers on hosts:** still refused. The queue tools would work, but
   HQ's usage, transcript and pools ledgers would read empty; the Compose
   target has no HQ.

## Tests

- config: every refusal above, each naming its field; `runtime` defaults to
  pod; a host officer needs no provisioner, a mixed roster still does.
- the wake loop over a host-spawn fake: a queue item starts instance 1 (one
  spawn, prompt behind the officer preface), the pending row counts up, idle
  past the window closes it, a host offline changes nothing.
- tool wiring: a call from the instance's own session and window is served as
  its agent row; the same session in another window, another session, a row on
  another host, a provisioned row under the recorded window, and a non-officer
  session are refused; the instance's operator tools are refused; the listing
  has the host tools only.
- the agent row: created on the first spawn, kept on scale-to-zero, reused,
  never provisioned (the adapter refuses all six lifecycle calls), never
  reaped (the predicate itself, Postgres), no cluster shape (the CHECK); every
  agent-row consumer ledgered by type.
- end to end on Postgres: `officer_post_draft` from a host instance through
  the owner's approval and the plugin's claim and result to a completed item;
  `connector_request` from a host instance files a pending grant for its row.
- the step-up gate (round 2), through the real mirror door on Postgres: an
  impersonating `connector_request_decide`, `attention_answer` and
  `plugin_draft_decide` file cards and decide nothing; the browser and the MCP
  door cannot answer a card; the owner's phone (a device principal) can, and
  the decision then runs once as the session that asked; Refuse runs nothing;
  a signed confirm for card A is refused for card B; a machine with no host
  officer, and the browser's own Asks page, are unaffected; every mirror tool
  is classified gated or exempt, read from `mirrorTools`' source.
- removing a host row with no provisioner (`muster agent rm`), the declared
  officer's own row still protected, a moved officer self-heals after the
  rm; a pod officer over a host row is refused naming the command; a
  `/clear` is followed; a busy or waiting window is not asked to close.
- seam: one `Reconciler`, one fleet, both runtimes driven by the same `Decide`;
  `dispatchOfficerTool` reached from exactly the agent door and the mirror
  door's host binding; `SpawnAgent` and `CloseAgent` callers widened by exactly
  the officer fleet.
