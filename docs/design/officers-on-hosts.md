# Officers on hosts: officer instances as host-spawned sessions

**Status: v1 built in this pull request.** Operator decision (2026-11-13):
officers run in the Docker Compose stack as **host-spawned Claude Code
sessions** — a tmux window on one opted-in machine, started through that
machine's `muster-mirror` — rather than through a Docker-socket provisioner.

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
| instance identity | the agent row's name `officer-<slug>-<n>` | the same name, recorded on the spawn row; the window is bound by muster's record (below), never by a tag |
| first message | the kickoff, delivered by the kickoff deliverer through ccd | the same kickoff (`KickoffText`), sent as the spawn's prompt behind `mirror.OfficerSpawnPreface` |
| a new turn for an idle instance (`Reprompt`) | ccd's gateway, under the session claim | a paste into its window, only while the host reports it idle (`AgentState`), stamped with the spawn id (`mirror.ForDispatch`) |
| scale to zero (`Stop`) | provisioner Stop (agent and volume kept) | `agent-close` on that window: `/exit` at an idle prompt, the window closed after the grace otherwise; the host then removes the worktree if it is clean |
| up / down (`Instances`) | the agent row and the provisioner's instances | the latest spawn row per instance, against the host's live listing: a recorded window the host lists with that spawn's id is up; a row still waiting for its reply is up for 180 s (the chief's pending countdown); anything else is down |
| officer MCP tools | the agent door (`/mcp`), bound by the agent's own token | the mirror door (`/mirror/mcp`), bound by the caller's (host, Claude session) to a spawn row and then to the caller's window as listed — the rule `agent_report_done` uses (`callerHostDispatch`) |

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
   host's MCP endpoint. muster binds each call to the spawn row.
4. **More work while up.** `Decide`'s `Reprompt` pastes the re-prompt into the
   window, only when the host reports the session idle; otherwise the pass gets
   `ErrInstanceBusy` and tries again.
5. **Idle.** After `idle_scale_to_zero_after` with nothing claimable and
   nothing held, `Stop` closes the window through `agent-close`. A session that
   does not exit within the grace has its window closed.
6. **Host offline.** `Instances` fails blind — "cannot see" is not "nothing is
   up" — so the pass changes nothing and backs off, as an unreadable fleet does
   for a pod officer.

### Tokens and tools

- **No token reaches the session.** A builtin officer has no token at all; the
  spawn carries only the kickoff. The window gets exactly what every
  host-spawned window gets: the host's user-scope `mcpServers.muster` entry and
  its per-host loopback secret (chief-dispatch §3.5). muster adds nothing to
  its environment.
- **The tools are the queue tools and `officer_ask_person`** (`officer_brief`,
  `officer_items`, `officer_claim`, `officer_heartbeat`, `officer_complete`,
  `officer_release`, `officer_ask_person`). `officers.ToolOffered` says so for
  every host officer, so the kickoff, the listing, the boot line and the
  dispatch agree.
- **The mirror door lists them to every session of the owner's hosts while a
  host officer is declared** — `tools/list` carries no session (the same
  reason `agent_report_done` is listed for every session). A call from a
  session that is not a host officer's instance gets a tool error naming why.

## What works on the host runtime, and what is refused at boot

Works: the built-in backend's queue, leases, the switch, schedules, asks to
the owner through Attention, escalations, `max_instances` (one window each),
`idle_scale_to_zero_after`, `poll_interval`, `owner`, `repo` (now the
worktree's repository).

Refused at boot, each naming the field and why (`officers.parseRuntime`):

| field | why it is refused with `runtime: "host"` |
|---|---|
| `backend` other than `builtin` | v1. An HQ officer's usage, transcript and pools ledgers read the instance through its pod's ccd; on a host they would be silently empty |
| `host` missing, or not a mirror host name | there is nowhere to start it |
| `repo` missing | the host cuts every spawned window's worktree from a clone of a named repository; there is no repository-less spawn |
| `tiers` | each pool runs its own model, and the host spawn of Claude Code takes no model |
| `model` | the same: the host's Claude Code runs its own default |
| `image`, `cc_account`, `cc_failover` | pod and account-pool settings; the host runs its own `claude` on its own login |
| `connectors`, `read_repos` | connectors are granted per agent row, and a host instance has none; it reads its repository from its worktree |
| `llm`, `kube` | the LLM proxy and kube reads authenticate the pod's agent token |
| `ship`, `ci_logs`, `transcript` | already refused for every builtin officer |
| `runtime` other than `pod` or `host` | unknown runtime |

And at `EnableOfficers`: a host officer with no mirror hub is refused. A
roster whose officers are all on hosts no longer needs the agent store, a
provisioner or the `claude-code` kind; a pod officer beside it still does.

Not offered on a host (absent from the tool list and the kickoff, never
silently broken): `officer_post_draft` (an outbound draft is filed against the
instance's agent row, `plugins.FileOutbound`), the obs tools (the built-in
backend serves none anyway), and every ship, kube, task and gap tool.

## Security: what changes against a pod

State it plainly; the operator chose this knowing it.

- **It runs as the user on that machine.** No pod, no namespace, no
  NetworkPolicy, no egress lock. The session can read every file that user
  can, use their git and SSH credentials, reach anything their network can,
  and type into their other tmux windows. A pod officer's whole isolation story
  ([officers-builtin.md](../officers-builtin.md#-before-you-declare-one-on-a-real-deployment))
  does not apply.
- **Its repository access is the user's, not a read-only token.** A pod
  officer's GitHub access is a read-only App token minted per request; a host
  officer works in a real worktree and can push with whatever credentials the
  user's git has. The kickoff tells it not to push; that is prose, not a gate.
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

## Open questions

1. **muster closes an officer's window on its own.** chief-dispatch decision
   17 / D7 says muster never closes a host-spawned window: the owner closes it.
   Scale-to-zero needs a stop, so v1 closes an **officer** window (and only
   one: the window muster recorded for that instance, running the conversation
   it recorded) through the existing `agent-close` message, without the
   owner's tap. Recommended and built: declaring `runtime: "host"` is the
   owner's opt-in to that. The alternative — never close; the instance exits
   itself when the queue is empty — leaves scale-to-zero to the model's
   obedience, which is not a mechanism.
2. **`officer_post_draft` on a host.** It needs an agent row id
   (`plugins.FileOutbound`). Options: let an outbound draft be filed by an
   officer instance name with no row, or give host instances a row that no
   provisioner acts on. Neither is built; v1 does not offer the tool, and an
   officer that needs outbound drafts stays on pods.
3. **HQ officers on hosts.** The queue tools would work (the token stays in
   muster-server), but HQ's usage, transcript and pools ledgers would read
   empty. v1 refuses `backend: hq` with `runtime: host`.

## Tests

- config: every refusal above, each naming its field; `runtime` defaults to
  pod; a host officer needs no provisioner, a mixed roster still does.
- the wake loop over a host-spawn fake: a queue item starts instance 1 (one
  spawn, prompt behind the officer preface), the pending row counts up, idle
  past the window closes it, a host offline changes nothing.
- tool wiring: a call from the instance's own session and window is served; the
  same session in another window, another session, and a non-officer session
  are refused; the listing has the host tools only.
- seam: one `Reconciler`, one fleet, both runtimes driven by the same `Decide`;
  `dispatchOfficerTool` reached from exactly the agent door and the mirror
  door's host binding; `SpawnAgent` and `CloseAgent` callers widened by exactly
  the officer fleet.
