# Design: the chief dispatches every agent

**Status: phase 1 is built** (`agent_dispatch` for pods, #428,
`internal/api/agent_dispatch.go`). **Phase 3 is built for claude** (host spawn,
the trust entry, held targets and their cards, forwarding; #513 then #516: see
"As built" under [Phase 3](#phase-3-host-spawn-for-claude-with-the-trust-entry-and-forwarding)).
**Phase 2 is built** (the machine route `POST /api/agents`, `muster agent
create`, and the UI deletion: see "As built" under
[Phase 2](#phase-2-delete-the-dispatch-ui)). **Phase 4 is proposed.** The owner made
the nine decisions in [Owner decisions](#owner-decisions): four up front, and
five more in answer to this page's first draft. This revision adds nine more:
10-13, the owner's rules for **mirrored dispatch**: when "mirrored" means a
host, which runtime runs there, and what the chief asks when a host or a model
is missing ([2.8](#28-mirrored-dispatch-choosing-host-runtime-and-model));
and 14-18, the owner's answers to this revision's questions. It also rebuilds phase 4 on the opencode launcher that #486 shipped
([3.6](#36-the-commands-and-prompt-delivery)). Everything else on this page is
the recommended design. The owner answered this revision's five questions as
decisions 14-18. What is still open is a set of measurements
([7](#7-what-is-still-unverified)). Decision 19, starting an agent from one of
the owner's own sessions in two verbs, is built
([2.9](#29-held-dispatches-from-the-owners-sessions)).

**What this revision changed, and why it is not a new page.** Mirrored
dispatch *is* this page's host spawn (section 3, phases 3 and 4): the same
target strings, the same flag, the same `host_dispatches` row. A parallel page
would restate that protocol and drift from it, so the changes are made in
place:

- decisions 10-18, and [2.8](#28-mirrored-dispatch-choosing-host-runtime-and-model)
  (the routing rules and the two ask cards, reusing PR A's `ask_owner` card);
- `model` is now accepted for `host/<host>/opencode`, as an OpenRouter model
  ([2.2](#22-parameters));
- the opencode spawn uses `muster-mirror opencode` (#486) and an API send, in
  place of the wrapper and `--prompt` ([3.6](#36-the-commands-and-prompt-delivery));
- the live agent card from PR A shows host dispatches ([3.7](#37-linking-the-spawned-window-to-the-dispatch-and-the-task));
- host-side controls that hold **against a compromised server**: a repository
  allow-list, a spawn rate limit and an append-only audit log
  ([5.5](#55-a-compromised-server)). This reopens round-0 disposition D3 (no
  host spawn cap), and says why.

**PR A** is the separate, in-flight change that adds the chief's `ask_owner`
question card, the cluster target rules and the live dispatched-agent card.
This page assumes it has merged, and names the contract it relies on from it in
[2.8](#28-mirrored-dispatch-choosing-host-runtime-and-model).

**What changes.** Today an agent is started in one of two ways: by clicking
through the dispatch form in the web UI, or by the chief calling `task_dispatch`
on an existing task. This page proposes that the chief becomes the only way to
start one. The owner asks in the chief's chat, and the chief calls a single MCP
tool. That tool can start either of two things:

- an agent in a cluster pod, using one of today's pod runtimes;
- a Claude Code or opencode session in a new tmux window on one of the owner's
  hosts, which the existing mirror then picks up and shows in muster.

The dispatch form, its "Save for later" button and the sidebar's **+** button
are deleted.

**Citations.** Where this page says how muster works today, it cites `file:line`
at commit `4ec696cf`, and the code is the authority
([decisions.md](../decisions.md)). A claim that nothing confirmed is marked
**UNVERIFIED**. Hosts are written `<host>`, users `<owner>`, and repositories
`<owner>/<repo>`.

**Related designs.** This page builds on two others:

- [host-sessions.md](host-sessions.md) is the proposed, unbuilt design for a
  human starting a tmux session from the UI. Its tmux and environment mechanics
  are reused here. Its trigger (a browser form behind step-up) and its naming
  (a new session per spawn) are replaced, as
  [Relation to host-sessions.md](#relation-to-host-sessionsmd) explains.
- [multi-tenant.md](multi-tenant.md) supplies the tenant model.

## Summary

- **One tool, `agent_dispatch`, which replaces `task_dispatch`.**
  - `target` is required and has no default. A call without one is refused with
    a sentence that tells the chief to ask its owner.
  - `task_id` is optional, so the chief can start an agent on a task or from a
    prompt alone.
  - `worker_pool` starts a pool agent on `pod/claude-code`. It is the only
    other way, besides the form, to create one, so the form can go.
  - `request_key` makes a call idempotent per owner for 24 hours. It is
    required when there is no task. Reusing a key with different parameters is
    refused.
  - A call with no target is refused, and the refusal carries the targets that
    exist now, plus the budget. That one answer makes the chief ask, and tells
    it what to offer. There is no separate discovery tool.
- **Targets** are strings:
  - `pod/claude-code` and `pod/gateway` are today's two pod kinds
    (`internal/agents/kind.go:20-21`);
  - `host/<host>/claude` and `host/<host>/opencode` are tmux spawns on a host
    whose mirror advertises spawning ("mirrored", decision 10). opencode takes
    an OpenRouter `model` (decision 11). A host or a model the owner did not
    name is asked for with an `ask_owner` card, and the start is bound to the
    owner's tap ([2.8](#28-mirrored-dispatch-choosing-host-runtime-and-model)).
- **Budget: unchanged, and it now covers host spawns too.** The ledger, the
  caps (5 live and 10 a day) and the "every answer carries the budget" rule all
  stay. A reservation for a dispatch without a task names a fresh ledger
  object, not a task. A host spawn counts as live from its own record until its
  window ends.
- **The gate for a dispatch with no task, and for any host spawn, is the
  owner's own typed turn.** This is the check `owner_asked` already runs to
  release a gated task: `ownerTurnFor`, `internal/api/chief_owner_release.go:307`.
  It is mandatory for these two cases. `owner_asked: true` also always runs it,
  on any task. So a plugin message, a task event, or a turn the chief started
  itself can never start an agent with no task, start a process on a host, or
  get outside text accepted as the owner's wishes.
- **Every target's repository must be a connected one** (`connectedRepo`).
  That includes host targets, whose worktrees are pre-trusted and whose
  repository settings therefore apply.
- **The host spawn protocol** adds a new mirror flag, `-agent-spawn`, which is
  off by default and needs `-interactive`. It copies the pattern
  `-resume-spawn` already uses: a capability in `hello`, a request/reply pair on
  the hub's id space, and an argv built by the host behind the constant
  `exec "$@"` login-shell wrapper. The host:
  - creates a **fresh git worktree** of the named repository from
    `origin/<base>`, never touching any checkout the owner has open;
  - opens a **window in the `agents` session** on its own tmux server;
  - starts plain `claude` or `opencode` in their normal permission modes. For
    claude the prompt is handed over in a `0600` file that a constant wrapper
    reads, so neither tmux nor a shell ever parses it. For opencode the window
    runs `muster-mirror opencode` (#486) on a session the host created with the
    chosen OpenRouter model, and the prompt is posted through opencode's API
    once the pane is bound;
  - enforces its own repository allow-list, spawn rate limit and audit log,
    which hold even against a compromised server ([5.5](#55-a-compromised-server));
  - picks the session id itself, so muster can link the mirrored window to the
    dispatch record without guessing;
  - for claude, writes the folder-trust entry for the worktree it just created,
    and for nothing else, so the session does not stop at Claude Code's trust
    prompt.
- **`task_forward` reaches host-spawned agents.** For claude it goes through
  the existing paste relay, and for opencode through opencode's own API. It is
  addressed by the dispatch record and checked on the host against the window's
  `@muster-dispatch` tag. It can only ever reach a window the chief started on
  that task, never a window the owner opened.
- **The UI deletion** removes the sidebar **+**, the home page's
  "Dispatch an agent" button, the dispatch form (both its task-less and its
  task-card forms), "Save for later", `GET /ui/agents/new`,
  `GET /ui/agents/notes` and `POST /agents`. There are **no draft data to
  migrate**: "Save for later" creates an ordinary agent row with status
  `stopped`, and that row stays startable from the agent's page.
- **Phases.** The order is changed from the owner's list:
  1. chief pod dispatch, with or without a task, the required target,
     `worker_pool` and `request_key`;
  2. **delete the UI**;
  3. host spawn for claude, with the trust entry and forwarding, and
     mirrored dispatch (the routing rules and the two cards);
  4. host spawn for opencode with an OpenRouter model, through #486's
     launcher and API send, with forwarding through the same API.

  The reason is in [Phases](#6-phases).

## Owner decisions

These are binding, and this page does not reopen them.

1. **Clarify.** The dispatch tool takes a required `target` with no default. A
   target is either a cluster pod (one of today's pod runtimes), or a tmux host
   plus a runtime of `claude` or `opencode`. The tool refuses a call without a
   target, so the chief has to ask. The refusal is structural, not a prompt
   instruction.

   **Amended by a later owner decision.** The chief resolves the target from
   what the owner said, and the tool still never invents one:
   - "with Claude", or any Claude model → `pod/claude-code` (no model);
   - any other model the owner names → `pod/gateway`, with that model as its
     OpenRouter id (`openrouter/<provider>/<model>`);
   - no model named → the chief **asks** with an `ask_owner` question card:
     Claude, or an OpenRouter model (a few sensible ones, plus the card's own
     "Other" box). It never picks one;
   - "mirrored" → `mirrored`, which is **refused** until mirrored dispatch
     ships, with a sentence saying so. It never falls back to the cluster;
   - not mirrored → the cluster.

   The no-target refusal says to ask with `ask_owner`, and lists each target as
   a choice in the owner's words — "Claude Code (your Claude account)",
   "Gateway (OpenRouter model: <model>)" — not as a raw `pod/` name alone. The
   mapping is stated in the tool's description and in the chief's standing
   instructions (both runtimes); the description still names no target string,
   for the same reason it carries no cap number.
2. **tmux spawn.** The agent opens as a new window in a dedicated `agents` tmux
   session on that host's default tmux server. It runs in a fresh git worktree
   of the named repository, never in a checkout the owner has open. The
   existing mirror then picks it up.
3. **Host safety.**
   - A new muster-mirror flag, off by default. The owner's host configuration
     turns it on for the owner's default mirror only.
   - Spawned agents count toward the chief's existing live and daily caps.
   - Claude Code starts in its **normal** permission mode, so its prompts reach
     the owner as asks (Claude's waiting state, already shipped).
   - **Amended (operator, #522 round 0, Q1 (a): "opencode should run with no
     asking").** opencode starts in its own **allow-by-default** mode and
     **does not ask**: muster enforces no ask-on-shell for it. Measured, with no
     opencode configuration it ran a shell command without a permission
     request ([7](#7-what-is-still-unverified)). Its limits are the owner's opencode **deny rules and guard plugin**, the host's
     repository allow-list (`-agent-spawn-repos`), and `connectedRepo`.
     An ask opencode does raise (a question, or a rule of the owner's that
     asks) still reaches the Attention view through #416.
4. **UI.**
   - Delete the dispatch UI: the sidebar **+**, the dispatch form and "Save for
     later".
   - Allow a dispatch with no task, where the chief starts an agent from a
     prompt alone, as well as a dispatch on an existing task.

The owner's answers to the first draft's open questions:

5. **Worker pools.** `agent_dispatch` gains a `worker_pool` option, under the
   same target, budget and `owner_asked` rules. This unblocks the UI deletion
   ([2.2](#22-parameters), [4](#4-replacing-the-dispatch-ui-a-machine-route-a-cli-verb-then-the-deletion)).
6. **Default tmux server only: accepted.** The owner's default mirror runs on
   each host's default tmux server, so a mirror on any other server refuses to
   spawn ([3.8](#38-read-only-and-tenant-hosts)).
7. **Claude Code's folder trust.** The host writes the trust entry into Claude
   Code's configuration before launching. It is scoped to worktrees the mirror
   itself has just created under its own agents directory, and never touches
   anything else ([3.6.1](#361-claude-codes-folder-trust-entry)).
8. **Retries.** A `request_key` idempotency key: a repeated call with the same
   key returns the first dispatch's result instead of starting another. The
   key's lifetime, its scope (per owner) and the refusal of a reused key with
   different parameters are in [2.6.1](#261-request_key-idempotency).
9. **Forwarding into host-spawned agents.** This is in scope. `task_forward`
   reaches a host-spawned claude window through the existing paste relay, in
   phase 3, and a host-spawned opencode window through opencode's API, in phase
   4. Addressing and gating are in [3.10](#310-forwarding-into-a-host-spawned-agent).

The owner's rules for mirrored dispatch, given after phase 1 shipped:

10. **"Mirrored" picks a host; its absence picks the cluster.** A request that
    says "mirrored" starts the agent on one of the owner's hosts
    (`host/<host>/<runtime>`). A request that does not say it starts a cluster
    agent (`pod/…`, under PR A's cluster target rules).
11. **The runtime follows the model named.** "Mirrored … with Claude" runs
    **Claude Code** (`host/<host>/claude`). "Mirrored … with" any other model
    runs **opencode with that model through OpenRouter**
    (`host/<host>/opencode`, `model: <openrouter model id>`). OpenRouter is
    already configured as a provider in opencode on the hosts, so muster passes
    only the model ([3.6](#36-the-commands-and-prompt-delivery)).
12. **No host named: ask with a card.** The chief asks with an `ask_owner`
    card whose options are the hosts that are online now and accept dispatch
    (`hostSpawnable`, [3.8](#38-read-only-and-tenant-hosts)).
13. **No model named: ask with a card.** The options are Claude (Claude Code)
    or an OpenRouter model (opencode).

Decisions 12 and 13 are decision 1 applied to the two parts of a host target:
muster never chooses one the owner did not name.

The owner's answers to this revision's questions:

14. **"Mirrored" is the chief's reading, with no word check.** Mapping the
    request to a host target is a prose rule in the chief's instructions.
    muster does not refuse a host target for lacking the word "mirror"; the
    card and the live agent card show where an agent goes before and after it
    starts ([2.8](#28-mirrored-dispatch-choosing-host-runtime-and-model)).
15. **The model card offers a configured list plus a typed id.** The list is
    muster configuration, `MUSTER_CHIEF_DISPATCH_MODELS`, which the owner edits.
    Its default is a small list of OpenRouter ids, and the card always also
    takes a typed OpenRouter model id
    ([2.8](#28-mirrored-dispatch-choosing-host-runtime-and-model), "The model
    list").
16. **The host repository allow-list is empty by default.**
    `-agent-spawn-repos` names nothing until the owner lists repositories for
    each host in that host's configuration (their dotfiles), so a host spawns
    nothing until then ([3.2](#32-the-flag), [5.5](#55-a-compromised-server)).
17. **No Stop button in phases 3-4.** The live agent card tells the owner to
    close the window on `<host>`. There is no kill message (D7)
    ([3.7](#the-live-agent-card)).
18. **"Mirrored with Claude `<model>`" uses Claude Code's default model.**
    `model` stays refused on `host/<host>/claude` (defaulted, per the
    recommendation).

The owner's decision on starting an agent from one of their own sessions,
given after a mirrored session asked to start one and had no way to:

19. **Two verbs over MCP, no tap.** *"Dispatch and then another verb to approve
    the dispatch so I don't need to click anything."* The owner's mirrored
    sessions get `dispatch_request` (hold a start) and `dispatch_approve` (start
    exactly the held one), and nothing else gets them
    ([2.9](#29-held-dispatches-from-the-owners-sessions)).

---

## 1. Current state

### 1.1 Path A: the UI dispatch form

**Entry points (the parts that get deleted).**

The sidebar **+** button:
- It is `navDeployButton` (`internal/ui/nav_agents.go:175-192`), with id
  `NavDeployID = "sidebar-deploy"` (`:149`).
- `navAgentsHead` (`:156-168`) draws it when `Features.Dispatch` is set
  (`internal/ui/features.go:53`), and otherwise draws the "not set up" hint,
  `dispatchOffHint` (`internal/ui/agents.go:438,444-452`).
- `Features.Dispatch` is set at `internal/api/server.go:758` from
  `dispatchServed()`, which is `s.ext.Provisioner != nil`
  (`internal/api/ext.go:760`).
- The provisioner is chosen by `MUSTER_AGENT_PROVISIONER`
  (`cmd/muster-server/config.go:326`; it defaults to `none`).

The home page's empty state:
- It has a second button, "Dispatch an agent", with
  `HomeDispatchID = "home-deploy"` (`internal/ui/onboarding.go:35,65-76`).
- It opens the same form.

The task card:
- Its **Dispatch** button is `noteDispatchButton`
  (`internal/ui/notes.go:1784`, drawn at `:1105`).
- It opens the same form preselected to the task (`?note=<id>`).

**Routes.** All are registered in `registerAgentRoutes`
(`internal/api/agents.go:41-189`):

| route | line | handler |
|---|---|---|
| `GET /ui/agents/new` (`?note=&label=`) | 149 | `handleAgentNewModal` `:265-292` |
| `GET /ui/agents/repos` | 150 | `handleAgentRepoOptions` `:389` (**stays**: the task forms use it) |
| `GET /ui/agents/notes` | 151 | `handleAgentNoteOptions` `:404` |
| `GET /api/openrouter/models` | 152 | model search (**stays**: the task forms use it) |
| `POST /agents` | 153 | `handleAgentCreate` `:712-845`, behind `requireSession` + `requireLifecycleProvisioner` (`ext.go:731`) |

**The form.**
- `DispatchModalBodyFor` (`internal/ui/agents.go:486-575`) is the form with no
  task. Its **Dispatch** button posts `action=dispatch` (`:535`), and
  **Save for later** posts `action=save` (`:566-570`).
- `DispatchModalTaskBody` (`:649-722`) is the task card's version. It has
  Dispatch only.
- The fields read are at `internal/api/agents.go:718-760`: `action`, `repo`,
  `repo_branch`, `model`, `note_text` (a prompt for a dispatch with no task),
  `note_id`, `kind`, `cc_account`, `cc_failover` and `worker_pool`.

**What path A enforces.**
- A browser session.
- For `action=dispatch` only, `KickoffUndeliverableReason()`, which answers 409
  (`:737-748`).
- A `note_id` read under row-level security. A `gate:` tag refuses a dispatch
  with 409 (`:768`).
- Then `createAndDispatchAgent` (`:1043-1138`):
  - kind and account (`resolveKindAccount`/`resolveKind`, `:945-997`);
  - the worker pool (`:1050-1058`);
  - `connectedRepo`, **only for kinds that run under a pod supervisor**
    (`:1065`);
  - the model credential (`:1076`);
  - `Agents.Create`, then `Provisioner.Dispatch(ctx, id, kickoff)` in the
    background (`:1132`).

**What path A does not enforce.** It has **no cap, no budget, no reservation,
no one-start-per-task rule and no owner-answer rule**, and `POST /agents` has
no `requireSameOrigin`. It is the uncapped path.

**"Save for later" is not a drafts feature.**
- `action=save` creates the same `agents` row, which
  `Adapter.Dispatch(..., kickoff=false)` sets to `stopped` without provisioning
  anything (`internal/agentprovision/agentprovision.go:620-641`).
- The owner later brings it up with **Start** (`POST /agents/{id}/start`,
  `Adapter.Start` `:704`, from the agent page's ⋯ menu,
  `internal/ui/agent_actions.go:169`).
- There is no drafts table, route or reader. The stopped-agent reaper skips
  saved rows that never started (`internal/agents/reap_stopped.go:47`,
  `internal/api/agent_reap.go:34`, `internal/db/migrations/0045_agent_stopped_at.sql:16,29,32`).
- Everything else named "draft" is a different feature and stays:
  - plugin reply drafts (`internal/plugins/drafts.go`, migrations 0015/0016);
  - attention ask drafts (`internal/api/attention.go:1116`, migration 0025);
  - composer drafts (`internal/ui/composer_draft.go`).

### 1.2 Path B: the chief's `task_dispatch`

**Schema.**
- The tool is `taskDispatchTool = "task_dispatch"`
  (`internal/api/task_routing.go:65`). Its definition (`:245-264`) is listed
  only while `chiefDispatchOn` holds (`:224`).
- Parameters:
  - `task_id`, required;
  - `repo`;
  - `kind`, an enum of `claude-code` and `gateway`;
  - `owner_asked`;
  - `instructions`, at most 1 KiB.
- There is no `model` parameter.
- The description (`:102-113`) states no cap numbers, and a test pins it
  (`TestTaskDispatchDescriptionCarriesNoCapNumbers`).

**Call chain.** `dispatchAgentTool` (`internal/api/agent.go:621`) →
`dispatchTaskRoutingTool` (`task_routing.go:271`) → `toolTaskDispatch` (`:599`).

**Checks, in order.** Each check has its own refusal sentence.

1. Chief only, and the chief is on (`agent.go:627-634`).
2. The deployment flag (`:279-280`, `chiefDispatchOffText` `:79`). It needs
   `MUSTER_TASK_ROUTING` and a non-empty `MUSTER_CHIEF_DISPATCH_OWNERS`:
   - declared at `cmd/muster-server/config.go:212`;
   - parsed at `:1513-1518` by `parseOwnerIDs` (`:1174-1188`);
   - wired at `cmd/muster-server/main.go:602-604`.
3. The owner's opt-in, and whether the deployment can start agents at all:
   `chiefDispatchDeploymentRefusal`, `internal/api/task_actions.go:80-91`.
   **From this check on, every refusal carries the budget** (`:611`).
4. `instructions` must be at most 1 KiB (`:613-616`), **and** must be the
   owner's verbatim answer to an attention ask linked to this task
   (`ownerAnswer`, `:906-922`).
5. The kind: by default `claude-code` if it is enabled, otherwise `gateway`
   (`chiefDefaultKind` `:162`; `resolveKind`, `agents.go:945-963`).
6. The effective repository must be one of the owner's connected GitHub
   repositories (`connectedRepo`, `:926-944`). This applies to **every** kind,
   because a claude-code session starts pre-trusted in that checkout (`:637-646`).
7. `owner_asked`, on an open task that carries the chief's own gate. The check
   is `verifyGateRelease` and `ownerTurnFor`
   (`internal/api/chief_owner_release.go:307,356`), with the refusals at
   `:193-216`.
8. Under the owner's advisory lock, `ReserveChiefDispatch`
   (`internal/plugins/chief_dispatch.go:214`):
   - **one start per task**: a repeat answers `already_dispatched`
     (`task_routing.go:743-797`);
   - the `check()` closure (`:662-696`), which refuses when the task is not the
     owner's, is gated, is not open, already has a live agent
     (`chiefDispatchTaskBlock`, `task_actions.go:115-136`), or names a
     different repository;
   - the **live cap** of 5, then the **daily cap** of 10
     (`chief_dispatch.go:47-49,260-271`).
9. The provisioner is re-checked (`:824-829`), then
   `createAndDispatchAgent(..., Kickoff: true)` runs (`:830`). If the create is
   refused, the reservation is released (`:841`).

**What a successful call writes.**
- Audit rows `task.chief_dispatch` (the reservation, whose detail holds the
  kind and the instructions) and `task.chief_dispatch_agent`, both with object
  `note:<id>`.
- The `agents` row, linked through `agents.note_id`.
- If a gate was released: the gate's removal, a comment and a push.
- The task moves to `in_progress` (`:848-873`).

**What path B never does.** It never starts anything on a host. It only starts
pod agents.

**The ledger.**
- It is `audit_log`, not a table of its own (`chief_dispatch.go:41-61`).
- `chiefBudget` (`:326-383`) counts live agents as either of:
  - agents with a `task.chief_dispatch_agent` row whose `agents.status` is
    `pending`, `provisioning` or `running`;
  - reservations from the last 10 minutes with no agent row and no release row.
- `today` is every reservation in the last 24 hours.
- The owner destroying the agent releases the task's one start
  (`ReleaseChiefDispatch` `:466`, from `internal/api/agent_destroy_tool.go:162-166`).

**Where the chief's text names the tool.** Each of these is pinned by a test:
- the task-event text `TaskEventTools` (`internal/plugins/task_events.go:211`);
- the chief instructions (`internal/agentspec/instructions.go:463-471,604-612`);
- `docs/chief.md:117-146`;
- the guards section of `docs/plugins.md`.

### 1.3 The one host-side spawn that exists: `-resume-spawn`

This is the precedent the host protocol below copies.

**Flags.**
- `-resume-spawn` (`MUSTER_MIRROR_RESUME_SPAWN`, `cmd/muster-mirror/main.go:189`)
  only works together with `-interactive` (`:176`).
- `-resume-session` defaults to `muster-resume` (`:190`).
- `-resume-claude` defaults to `claude` (`:191`).
- The startup log for these is at `:380-385`.

**The wire** (`internal/mirror/resume.go`):
- `spawn-resume` (`:34`), from muster to the host: `{id, gen: <session id>}`.
- `resume-spawned` (`:37`), from the host back: `{id, session, window}` or
  `{id, error, refusal}`. The refusal kinds are at `:44-57`.
- The capability is `hello.resumeSpawn` (`internal/mirror/wire.go:408-413`).
  The hub stores it only as `resumeSpawn && interactive` on a v2 host
  (`internal/mirror/hub.go:717`).

**Server side.** `Hub.SpawnResume` (`resume.go:107-146`):
- looks the host up by `(owner, host)`;
- takes the id from the connection's shared `hc.next` counter;
- registers a reply channel and waits up to 15 s (`:71`).

The reply is routed at `hub.go:979-981`. A disconnect fails every waiter
(`:1090-1093`). Today the only caller is the mail resume
(`internal/api/mail_resume.go:139`).

**Host side** (`internal/mirrorhost/resume.go`):
- The checks are at `:138-250`.
- An in-memory rate limit allows one resume per session per 10 minutes and 6
  per host per hour (`:61-64`).
- The argv is `[bin, "--resume", sid]` (`:321`), wrapped by `loginArgv`
  (`:412-414`) into `<shell> -l -c 'exec "$@"' bin bin --resume sid`. The
  script is a constant (`:405`), and the shell must be a known POSIX shell
  (`:419-442`).
- The tmux call is `new-window -t =<session>:`, or `new-session -s <session>`
  with `-d -P -F '#{window_id}' -n … -c <cwd>` (`:351-388`).
- That call runs with `resumeEnv()`: the daemon's environment minus `TMUX`,
  `TMUX_PANE` and `PATH`, so the window takes the tmux server's PATH
  (`:390-401`).
- It runs on the daemon's own tmux server (`internal/mirrorhost/tmux.go:52-60`).
- It nudges a listing (`:278`).

**It has never run in production.** `-resume-spawn` has never been enabled on
the owner's hosts (per the owner). Its only exercise is its own test suite, and
the host spawn below copies an unproven pattern
([3.1](#31-wire-messages-and-capability)).

**Read-only hosts.** There is no `-read-only` flag. A host without
`-interactive` advertises false, and its host side refuses with `unsupported`
(`:287`).

### 1.4 How a mirrored window gets linked today

- Runtimes are found by walking the pane's process tree
  (`internal/mirrorhost/runtime.go:39-45,268-307`).
- **Claude Code.** Its session id comes from the pane map, then from
  `<config>/sessions/<pid>.json`, then from a guess from the newest transcript
  (`internal/mirrorhost/claude.go:532-757`). It travels as
  `Window.claudeSession` (`internal/mirror/wire.go:567`).
- **opencode.** Its session is matched from the pane title against the server's
  root sessions (`internal/opencodehost/title.go:10-34`). That is a heuristic,
  which travels as `Window.agentSession` (`wire.go:605`).
- **No environment variable, tmux option or table links a window to a
  dispatch or a task.** The only tmux user option the mirror reads is
  `@muster-color` (`internal/mirrorhost/perwindow.go:161`). `session_routes`
  (migrations 0021 and 0053) is the relay for plugin messages and tasks, not an
  identity link.

### 1.5 Every consumer of what gets deleted

**Go code.** These are deleted:
- `navDeployButton`, `NavDeployID`, `HomeDispatchID`;
- `DispatchModalBody(For)`, `DispatchModalTaskBody`, `RenderDispatchModal`
  (`internal/ui/render_phase2.go:56-74`), `DispatchUnavailableBody` (`:462`);
- `dispatchCloseOnOwnSubmit`, `dispatchFailureToast`, `dispatchCloseButton`,
  `TaskDispatchView`, `dispatchConfirmRow`, `RenderNoteOptions`, `noteOptions`
  (`internal/ui/agents.go:581-826`);
- `noteDispatchButton`;
- `handleAgentNewModal`, `handleAgentNoteOptions`, `handleAgentCreate`;
- the inline `agentDirty`/`DIRTY` script entries for the form
  (`internal/ui/components.go:2702-2718`).

`Features.Dispatch` stays only if something still reads it after the deletion.
Otherwise it goes too.

**Go tests that change with the deletion:**
- in `internal/api/`:
  - `dispatch_offer_test.go:68`
  - `kickoff_preflight_test.go:177-253,324`
  - `kinds_test.go:74-233`
  - `worker_pool_form_test.go:21-84`
  - `isolation_credentials_test.go:69-123`
  - `credentials_test.go:143,161`
  - `github_app_identity_test.go:114-116`
  - `nostore_ledger_test.go:269-271`
  - `provisioner_seam_test.go:93,136,143-144,916,1001`
  - `isolation_test.go:246-259,514-527,1239`
  - `principal_test.go:239-262`
  - the route golden files `testdata/routes.golden:123-125,179` and
    `testdata/routes.partition.tsv:221-223,256`
- in `internal/ui/`:
  - `agent_actions_test.go:173-178`
  - `onboarding_test.go:45-68`
  - `worker_pool_test.go:24-59`
  - `claude_code_polish_test.go:52`
  - `mobile_test.go:21`
  - `removed_surfaces_test.go:82` (gains entries)
  - `kinds_removed_test.go:16`
  - `tags_test.go:313-332`
  - `textplain/sinks_test.go:105-132`
- in `internal/agentprovision/`:
  - `kickoff_refusal_test.go:450`, which pins the text "Save for later, then
    Start"

**Text that tells the owner to use the form.**
`KickoffRefusalSummary` (`internal/agentprovision/agentprovision.go:235-239`)
says *Use "Save for later", then Start the agent*. It has to be reworded.

**e2e** (`e2e/`):
- The helper `e2e/lib/dispatch.ts` (`openDispatchSheet`, which clicks
  `#sidebar-deploy`).
- **16 specs use the form**, most of them only as the **fixture path that
  creates an agent** (they click `button[value="save"]`):
  - `agent-composer`
  - `agent-page-actions`
  - `agents-switcher`
  - `byo-credentials`
  - `claude-code-polish`
  - `cls`
  - `dictation`
  - `kinds`
  - `layout`
  - `onboarding`
  - `pickers`
  - `provisioning-indicator`
  - `sidenav`
  - `terminal`
  - `terminal-overflow`
  - `view-swap`
- Measured with `find e2e -name '*.spec.ts' | xargs grep -l` over
  `value="save"`, `sidebar-deploy`, `home-deploy` and `/ui/agents/new`, plus a
  second grep for users of `lib/dispatch` (which adds only `pickers`). The
  union is 16 specs.

**Docs that describe the form:**
- `README.md:138-140`
- `INSTALL.md:982`
- `docs/provisioner.md:9-11,40,252,290,370`
- `docs/jobs.md:232-251,567`
- `docs/tasks.md:36`
- `docs/design/host-sessions.md:6`

`docs/jobs.md:251` says that **the form is the only way to create a
worker-pool agent** ("Neither the `muster` CLI nor the chief's `task_dispatch`
can create an agent in a pool"). Decision 5 gives `agent_dispatch` a
`worker_pool` option, so the form is no longer needed for that
([2.2](#22-parameters)), and that sentence is rewritten in phase 1.

**CLI** (`cmd/muster`): none. It calls `/api/agents` and the task routes, never
`/ui/agents/new` or `POST /agents`.

**plugins/**: none.

**The Android app** (the `muster-android` repository, read through `gh api`
at its default branch): **none.**
- It is a WebView shell, so it reaches the form only by loading the web page.
- Its only native agent action is `AGENT_SEND`
  (`/api/device/actions/agents/{name}/send`, `ActionId.kt:27`).
- Its "draft" actions (`ActionId.kt:25-26`) are plugin reply drafts.
- `/agents/...` appears only in path matching (`PullGate.kt:142,153`).

The app needs no change, and no release.

**What stays, because other code shares it:**
- `createAndDispatchAgent`, `dispatchParams`, `resolveKind*`,
  `resolveRepoBranch`, `dispatchDisplayName`, `ErrDispatchRefused`,
  `advanceTaskToInProgress`. Other callers use them: `chief_provision.go:132`,
  `chief_replace.go:334`, `officers.go:392`, `task_routing.go:830`.
- `kindChoices` (`internal/api/agents.go:296-310`). The target list in
  `agent_dispatch`'s no-target refusal reuses it.
- The model combobox, the repository and model option routes, and
  `advancedDisclosure`. The task create and edit forms use them
  (`internal/ui/notes.go:1950-2036,2209`).
- The agent card, status and badge helpers (`internal/ui/agents.go:21-390`).
- `workerPoolChip` and `CredentialsPath` (`internal/ui/kinds.go:32,224`).
- Every agent page route: `GET /agents/{name}`, its header, status, chat,
  task, start, stop, name, model, sessions and delete routes, plus
  `GET /ui/nav/agents` and `GET /api/agents`.

---

## 2. The new tool surface

### 2.1 One new tool, `agent_dispatch`, replacing `task_dispatch`

| option | for | against |
|---|---|---|
| extend `task_dispatch` | no new name | its name says "task"; its `kind` has a default, which decision 1 forbids; its description is pinned and remembered by the chief's resumed conversation |
| **new `agent_dispatch`, with `task_dispatch` removed** | one tool and one set of guards; the new name is what the chief re-learns | the instruction and task-event texts and their tests change once |
| keep both | — | two copies of every guard ("one rule, one place" says no); two budgets to keep in step |

**Recommended: `agent_dispatch`.** `task_dispatch` comes off the listing in the
same release. `dispatchTaskRoutingTool` re-checks names at the call, because a
gateway can call a name it was never offered (`task_routing.go:277-280`). So a
call to the old name answers with a fixed refusal: `"task_dispatch is replaced
by agent_dispatch, which needs a target: ask your owner where to run it"`.

The core of `toolTaskDispatch` becomes `toolAgentDispatch`. It is the same
function, with the target added and the task made optional. Every existing
guard keeps its order and its sentence.

### 2.2 Parameters

| parameter | required | meaning |
|---|---|---|
| `target` | **yes** | see [targets](#23-targets). No default, and no fallback to an enum |
| `prompt` | yes without `task_id`; optional with one | the work. With a task, it plays today's `instructions` role ([below](#the-prompt)) |
| `task_id` | no | an existing task of the owner's. Without it, the dispatch has no task |
| `repo` | yes for a host target; otherwise as today | `<owner>/<repo>`. **For every target, pod or host, the server checks it with `connectedRepo` before it reserves anything** ([5.1](#51-who-can-trigger-a-spawn)). For a host, the host then resolves it to one of its own clones when the spawn runs, and its refusal names the repository |
| `base` | no | for a host: the branch the worktree is cut from. The default is the remote's default branch (`origin/HEAD`). For a pod: today's `repo_branch` |
| `model` | for `host/<host>/opencode`: **yes** (or the card asks, [2.8](#28-mirrored-dispatch-choosing-host-runtime-and-model)); `pod/gateway`: optional; otherwise refused | `pod/gateway` takes a model today (`buildRuntimeConfig`, `internal/agentspec/agentspec.go:1078`). **`host/<host>/opencode` takes an OpenRouter model id** (decision 11), such as `<vendor>/<model>`, which muster sends as `openrouter/<vendor>/<model>`. Its shape is `^[a-z0-9][a-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._:-]*$`, at most 128 bytes, otherwise `"model %q is not an OpenRouter model id (<vendor>/<model>)"`; whether the host's opencode knows it is the host's check (`bad_model`, [3.1](#31-wire-messages-and-capability)). Refused for `pod/claude-code` and `host/<host>/claude` with today's sentence ("this target takes its model from its own configuration"): claude-code has no per-agent model (`claudeCodeNoModel`, `internal/api/agents.go:933`), and a host's `claude` uses the owner's own settings |
| `owner_asked` | see [gating](#25-gating-when-there-is-no-task-to-hang-it-on) | **`true` always runs `ownerTurnFor`, whatever the target and whether or not there is a task**. It is required for a dispatch with no task and for every host target |
| `worker_pool` | no | decision 5: make the agent a machine-job worker in the named pool. See [below](#worker-pool-agents) |
| `request_key` | **yes without `task_id`**; optional with one | decision 8: the call's idempotency key. See [2.6.1](#261-request_key-idempotency) |

**Unknown fields are refused, which is new.** The arguments are decoded with
`json.Decoder.DisallowUnknownFields`. Any field not in the table above is
refused with `"arguments are {target, prompt?, task_id?, repo?, base?, model?,
worker_pool?, request_key?, owner_asked?}"`. Today's decode
(`task_routing.go:285-294`) ignores unknown fields and refuses only a missing
or invalid `task_id`. So this is a change, and it gets its own test.

#### Worker-pool agents

`worker_pool` carries the form's rules over unchanged. The same code checks
it (`createAndDispatchAgent`, `internal/api/agents.go:1050-1058`), and
migration 0041's `agents_worker_pool_check` also refuses a bad row.

- **The target must be `pod/claude-code`.** Any other target is refused with
  today's sentence: `"worker_pool %q: only a claude-code agent can be a
  machine-job worker"`.
- **The name must be 1-63 characters of `[a-z0-9-]`, not starting with `-`**
  (`agents.ValidWorkerPool`). A bad name is refused with today's sentence.
- **It takes no `task_id`.** A pool worker serves machine jobs, not a task.
  The refusal is `"a worker_pool agent serves machine jobs and takes no
  task_id"`.
- **The `prompt` is optional.** Without one, no first turn is claimed: the
  agent is created and then started, which is what the form's "Save for later,
  then Start" did (`Adapter.Start`, `internal/agentprovision/agentprovision.go:704`).
- **The deployment refusal still applies to a pool worker without a prompt.**
  `chiefDispatchDeploymentRefusal` (`internal/api/task_actions.go:80-91`) runs
  first for every `agent_dispatch` call. On a deployment that cannot deliver a
  first turn, that includes `KickoffUndeliverableReason`. So such a deployment
  refuses even a pool worker that would claim no first turn, with today's
  `"this deployment cannot start an agent now: <reason>"`. That keeps one
  refusal order for every call. A deployment in that state creates pool
  workers through the machine route instead ([4](#4-replacing-the-dispatch-ui-a-machine-route-a-cli-verb-then-the-deletion)).
- **The same target, budget and gating rules as any other dispatch with no
  task** (decision 5): `owner_asked` is verified, `request_key` is required,
  and the agent spends one unit of the daily cap and holds one live slot. Pool
  workers are long-lived, so a pool of 3 holds 3 of the 5 live slots for as
  long as it runs. That is the cost of decision 5, stated here so nobody
  discovers it later.
- `docs/jobs.md:251` becomes: "the chief's `agent_dispatch` with
  `worker_pool` creates one, and so does `muster agent create --worker-pool`".

#### The prompt

- It is cleaned the way `cleanInstructions` cleans text today (`:174-191`).
  Every control, format and invisible code point is removed, line breaks are
  kept, and the limit is checked on the input as given.
- The limit rises from 1 KiB to **8 KiB**, in bytes, measured on that input. A
  host spawn hands the prompt to the runtime through a file
  ([3.6](#36-the-commands-and-prompt-delivery)), so no argv or tmux limit is
  involved.
- **Who may write it:**
  - **With `owner_asked: true`:** `ownerTurnFor` must pass first. That means
    the turn the chief is in is a message its owner typed. This holds on any
    task, gated or not, and with no task at all. Only then is the prompt
    accepted as the chief's relay of that message. The prompt is fenced and
    marked as relayed by the chief (`renderChiefInstructions`, `:200-215`), and
    muster's run rules close the message and take precedence
    (`chiefDispatchRules`, `:142-145`). With no task, the rule about commenting
    on the task becomes "report back in the chief's chat with `chief_ask`, or
    through the pull request".
  - **Without `owner_asked`**, which is possible only on a task: today's rule
    holds. The prompt must be the owner's verbatim answer to an attention ask
    about this task (`ownerAnswer`, `task_routing.go:617-628`).
  - Either way, a turn that a plugin message, a task event or mail triggered
    cannot get outside text accepted as the owner's wishes. Passing
    `owner_asked: true` on such a turn is refused by `ownerTurnFor`, and
    leaving it out puts the prompt under `ownerAnswer`.

### 2.3 Targets

A target is a string, checked against the targets that exist **at the moment
of the call**:

| target | starts | needs |
|---|---|---|
| `pod/claude-code` | a Claude Code pod, as today | the kind enabled (`MUSTER_AGENT_KINDS`) and the owner's Claude account pool |
| `pod/gateway` | a gateway pod, as today | the kind enabled |
| `host/<host>/claude` | `claude` in a new window of `agents` on `<host>` | the host online, `-interactive` on, and its hello advertising `agentSpawn` with `claude` |
| `host/<host>/opencode` | `opencode` the same way | the same, with `opencode` |

**Why a string and not an object.** A string is one field, so a call cannot
fill in half of an object. Its whole grammar is two regular expressions:

- `^pod/(claude-code|gateway)$`
- `^host/(<host name>)/(claude|opencode)$`

The host-name part uses the hub's existing host-name validation.

**A held target.** For mirrored dispatch, the host part and the runtime part
may each be `?`: `host/?/claude`, `host/?/opencode`, `host/<host>/?`,
`host/?/?`. `?` is never a valid host name, so it cannot collide with one. A
held target never starts anything: it records the request and asks the owner
for the blank ([2.8](#28-mirrored-dispatch-choosing-host-runtime-and-model)).
`host/<host>/opencode` without a `model` is held the same way.

**Why the tool listing has no enum.** The set of hosts changes from minute to
minute, so an enum written into the listing goes stale. The chief would also
remember it, which is exactly the failure that took cap numbers out of the
description (`:95-101`).

### 2.4 Discovery: the refusal is the list

There is **no separate discovery tool** (round-0 disposition D1). When
`agent_dispatch` is called with no target, or with a target that does not
exist now, the refusal carries the targets that do exist, together with the
budget:

```json
{
  "error": "target is required and has no default, and your owner named no model: ask them with ask_owner which to use — Claude, or an OpenRouter model (offer two or three sensible ones) — using the labels in choices below, then call again with the target and model their answer names. Never choose one they did not name.",
  "targets": {
    "pods": ["pod/claude-code", "pod/gateway"],
    "hosts": [{"host": "<host>", "targets": ["host/<host>/claude", "host/<host>/opencode"]}],
    "choices": [
      {"target": "pod/claude-code", "label": "Claude Code (your Claude account)", "when": "your owner said Claude, or named a Claude model; pass no model"},
      {"target": "pod/gateway", "label": "Gateway (OpenRouter model: <model>)", "when": "your owner named any other model; pass it as model, as its OpenRouter id: openrouter/<provider>/<model>"}
    ]
  },
  "budget": {"live": 1, "live_cap": 5, "today": 3, "daily_cap": 10, "live_agents": ["…"]}
}
```

That one answer does two jobs: it makes the chief ask its owner, and it tells
the chief what to offer when it asks.

**Where each part comes from.**
- **`pods`:** the enabled kinds that the owner can use, from `kindChoices`
  (`internal/api/agents.go:296-310`). Today that function feeds only the form
  being deleted.
- **`hosts`:** the hub's `HostView` for the owner, keyed by `(owner, host)`,
  filtered by `hostSpawnable` ([3.8](#38-read-only-and-tenant-hosts)). Only the
  owner's own hosts are listed, and only those that can spawn now. Another
  user's host, an offline host, a read-only host, or a mirror connected to
  another muster server (another tenant) never appears. Repositories are not
  listed (D6): the host checks the repository when the spawn runs
  ([3.1](#31-wire-messages-and-capability)).
- **`budget`:** the same `ChiefBudget`. `live_agents` gains a `target` field,
  and `task` is `0` for a dispatch with no task.

**The other target refusals.** Each is its own sentence, so a test and the
chief can tell them apart. Each carries the same `targets` and `budget`:
- `"target %q is not one of: pod/claude-code, pod/gateway,
  host/<host>/claude, host/<host>/opencode"`
- `"pod kind %q is not enabled on this deployment (MUSTER_AGENT_KINDS)"`. This
  is today's `resolveKind` sentence.
- `"host %q cannot start agents now (offline, read-only, or its muster-mirror
  lacks -agent-spawn); nothing was queued"`. A host spawn is never queued: an
  intent from an hour ago is not an intent now (host-sessions, "Lifecycle").
- `"host %q does not offer %s"`, for a runtime the host's hello does not list.

### 2.5 Gating, when there is no task to hang it on

Today the gate lives on the task. A `gate:` tag holds the agent back, and only
`owner_asked` in the owner's own turn releases a gate the chief filed itself.
Today that check runs **only** on a task the chief gated
(`task_routing.go:717-726`). A dispatch with no task has no tag to carry the
gate.

**Proposal: `owner_asked: true` always runs `ownerTurnFor`, on any target,
with or without a task. It is required for a dispatch with no task, and for
every host target.**

- `ownerTurnFor` (`chief_owner_release.go:307`) proves that the turn running on
  the chief is a message its owner **typed** in the muster chat.
  - It accepts only `authz.ViaSession`. `ownerTypedBy` (`:95-101`) excludes
    API tokens, the hook token, plugins, relays, routes and sweeps.
  - It requires that no other turn of the chief's is running at the same time
    (`chiefReleaseOutsideTurn`, `chiefReleaseOtherCLIBusy`, `:200-209`).
- It is checked before anything is reserved, and checked again under the
  reservation's lock, as the gate release is today (`task_routing.go:670-685`).
- **The change from today:** `owner_asked: true` on an open, ungated task is
  no longer silently accepted. It runs the same check, and if that check
  refuses, the call is refused. This is what stops a turn triggered by a
  plugin message from passing `owner_asked: true` to get 8 KiB of outside text
  fenced as the owner's wishes ([2.2](#the-prompt)).
- **The result:** a plugin message, a task event, mail, or a turn the chief
  started itself can **never** start an agent with no task, start anything on
  a host, or speak for the owner. The one path still open without the owner's
  turn is a pod dispatch on an existing, ungated task that has no
  `owner_asked` and no prompt other than an `ownerAnswer`. That is exactly
  what `task_dispatch` allows today.
- **A chief that runs as a gateway can never pass `owner_asked`.**
  `ownerTurnFor` refuses any session that is not Claude Code
  (`chiefReleaseNotOverMCP`, `:212`). Such a chief can only dispatch pods on
  ungated tasks. Every other dispatch needs the machine route
  ([4](#4-replacing-the-dispatch-ui-a-machine-route-a-cli-verb-then-the-deletion)).

| dispatch | `owner_asked` | `ownerTurnFor` runs |
|---|---|---|
| pod, on an open, ungated task, without `owner_asked` | absent | no (as today); any prompt must be an `ownerAnswer` |
| pod, on an open, ungated task, with `owner_asked: true` | given | **yes (new)** |
| pod, on a task gated by the chief's `task_create_gated` | required (as today) | yes, and the gate is released (as today) |
| pod, with no task | **required** | **yes** |
| host, on a task or not | **required** | **yes** ([5.1](#51-who-can-trigger-a-spawn)) |
| any, on a task gated by the **owner** | — | refused, as today (`chiefReleaseNotChiefsGate`) |

**Refusals.**
- `"a dispatch with no task needs owner_asked: true, in your answer to a
  message your owner typed in your chat; otherwise file it with
  task_create_gated and use task_route_attention"`
- `"a host target needs owner_asked: true, in your answer to a message your
  owner typed in your chat"`
- **On a task the chief gated**, the owner's-turn refusals are today's,
  unchanged (`:193-216`).
- **On an ungated task, or with no task**, today's sentences would be wrong.
  Each one talks about releasing "the gate", and most end in "use
  task_route_attention", which needs a task. So every `chiefRelease*` sentence
  that `ownerTurnFor` can return gets two siblings. The check that fails is
  the same; only the words differ.
  - The **no task** sentence is below.
  - The **ungated task** sentence is the no-task one, followed by `; or use
    task_route_attention`. Only the ungated-task variant ends that way, because
    `task_route_attention` takes a task.

  | today's constant (`chief_owner_release.go:193-216`), task the chief gated | sibling, **no task** |
  |---|---|
  | `chiefReleaseNoOwnerTurn` | `"owner_asked: you are not answering a message your owner typed in your muster chat right now, so muster cannot take this as their request (a plugin message, a relayed answer, a note from muster, a turn you started yourself or anything typed in your terminal never counts); ask them in the chat"` |
  | `chiefReleaseNotOwnersTurn` | `"owner_asked: the message you are answering was not typed by your owner, so muster cannot take this as their request"` |
  | `chiefReleaseOutsideTurn` | `"owner_asked: muster is also running a turn for you that your owner did not type, so muster cannot tell which of them asked for this; ask your owner again in the chat once it is over"` |
  | `chiefReleaseOtherCLIBusy` | `"owner_asked: another of your sessions is working at the same time as your owner's turn, so muster cannot tell that this call comes from your owner's; ask your owner again in the chat once only their turn is running"` |
  | `chiefReleaseUnaccountedCLI` | `"owner_asked: a Claude Code process in your pod is running without a session record, so muster cannot tell whether it is working at the same time as your owner's turn; ask your owner again in the chat"` |
  | `chiefReleaseCLIsUnknown` | `"owner_asked: muster could not read which of your sessions are working (your pod did not answer, or its image is too old to say), so nothing was started; ask your owner to start it with muster agent create"` |
  | `chiefReleaseNotOverMCP` | `"owner_asked: this session is not a Claude Code session, and only a Claude Code session can show muster that your owner typed this; ask your owner to start it with muster agent create"` |
  | `chiefReleaseUnverifiable` | `"owner_asked: your owner's turn could not be read, so nothing was started; try again later"` |
  | `chiefReleaseChanged` | `"owner_asked: your owner's turn changed while the start was being checked; call again"` |
  | `chiefReleaseNotChiefsGate` | — : it applies only to a gated task |

  The constants are named `ownerAskedNoTask<Name>` and
  `ownerAskedUngated<Name>`, for example `ownerAskedNoTaskNoOwnerTurn`.
  `ownerTurnFor` keeps returning today's constants. A single mapping function,
  `ownerAskedRefusal(why string, task taskKind)`, where `taskKind` is gated,
  ungated or none, turns them into the right sentence. That is the one place
  the three sets meet, and a test walks every row of the table through it.

### 2.6 Budget and accounting for a dispatch with no task

The ledger stays in `audit_log`, with the same three actions, and the same
`chiefBudget` count decides the caps. What changes is the object, and a third
source of live slots:

- **On a task:** the object is `note:<id>`, as today. One start per task, a
  release when the owner destroys the agent, and `already_dispatched`: all as
  today.
- **With no task:** the object is `dispatch:<id>`, where `<id>` is 16 random
  bytes in hex (`crypto/rand`), minted by the server before the transaction.
  Every such dispatch is therefore its own object, written by the ordinary
  `audit()` insert.
  - `audit_log.id` is not used for this. It is `GENERATED ALWAYS`, and users
    have no UPDATE policy on the table (migrations 0006:327 and 0009:68-79).
    So the row id is not known before the insert, and the row cannot be
    rewritten after it. A random object needs neither.
  - "One start per task" has nothing to bind to, and **does not apply**. A
    retry is caught instead by its `request_key`
    ([2.6.1](#261-request_key-idempotency)), which is required for exactly
    this reason.
- **`ReserveChiefDispatch(ctx, taskID, …)`** becomes
  `ReserveChiefDispatch(ctx, object, …)`. For a `dispatch:` object, the
  prior-start branch (`chief_dispatch.go:231-249`) is skipped.
  `ReservationID` is still read back by object, as it is today (`:280-281`).
  `noteObjectID` returns `0` for a `dispatch:` object (`:386-392`), and that
  `0` is what `live_agents[].task` shows.

**Live slots: one source of truth per kind of dispatch.** `chiefBudget`'s
`live` is the sum of three disjoint sets:

| source | counts | holds a slot while |
|---|---|---|
| A. recorded pod agents (`task.chief_dispatch_agent` rows with `agent_id`) | pod dispatches whose agent row exists | `agents.status` is `pending`, `provisioning` or `running` (today's rule, `chief_dispatch.go:328-334`) |
| B. pod reservations with no agent row yet | pod dispatches still being created | the 10-minute `pendingAgentWindow` (today's rule, `:354-360`), **now restricted to reservations whose `detail.target` does not start with `host/`**. That way a reservation written before this change, whose detail holds only `{"kind": …}`, still counts |
| C. `host_dispatches` rows | **every** host dispatch, and nothing else | `status` is `pending` or `running` |

- **A host dispatch is counted only in C.** Its `host_dispatches` row is
  inserted **inside the reservation's transaction**, under the owner's
  advisory lock (`chief_dispatch.go:221`), in the same commit as the
  `task.chief_dispatch` row. So there is no moment when the reservation is
  counted but the row is not.
- **The API change that makes this possible.** Today
  `ReserveChiefDispatch(ctx, taskID, detail, check)` (`chief_dispatch.go:214`)
  hands `check` no transaction. `ReleaseChiefDispatch` (`:466`) opens its own
  `inTx`. So:
  - `host_dispatches` lives in **`plugins.PGStore`**, beside the ledger, as a
    new file `internal/plugins/host_dispatches.go`. Its row is written with the
    same `tx`.
  - `ReserveChiefDispatch` gains a hook,
    `onReserved func(tx pgx.Tx, reservationID int64) error`.
    - It runs inside the transaction, after the reservation row is written and
      before the commit. `reservationID` is that row's id, read back by object
      as `ReservationID` is today (`:280-281`).
    - A host dispatch passes a hook that inserts its `pending` row,
      **recording `reservation_id`** ([3.7](#37-linking-the-spawned-window-to-the-dispatch-and-the-task)).
    - A hook error rolls back the whole reservation.
  - **Releasing a host dispatch is keyed by its reservation, not by an agent.**
    Today's two release bodies cannot release one:
    - `ReleaseChiefDispatch` (`:466-497`) releases only when
      `latestStartAgent`'s `agent_id` equals the destroyed agent. A host row
      that is `pending`, refused or counted down has no agent row at all, so
      that release would do nothing, and the task's start would stay spent
      forever.
    - A running host row's agent row has the detail `{host_dispatch_id}`, so
      `(detail->>'agent_id')::bigint` is NULL. `latestStartAgent` (`:420-432`)
      scans it into an `int64`, and pgx refuses to scan NULL into one, so the
      surrounding transaction would fail.
  - So there are three changes:
    1. **`latestStartAgent` returns which kind of start it found.** It scans
       both `(detail->>'agent_id')::bigint` and
       `(detail->>'host_dispatch_id')::bigint` into `*int64`, and returns
       `startAgent{AgentID *int64; HostDispatchID *int64}`. Every caller
       branches on that:
       - `ReserveChiefDispatch`'s already-dispatched answer names the agent or
         the host dispatch;
       - `ReleaseChiefDispatch`, the pod destroy path, writes nothing when the
         latest start is a host dispatch. Destroying some pod on the task
         does not release a host start;
       - `ChiefStartedAgent` returns `ok = false` for a host start. A
         completed task never stops a host window ([3.7](#37-linking-the-spawned-window-to-the-dispatch-and-the-task));
       - `ReleaseRefusedChiefDispatch` treats an agent row of either kind as
         "an agent was started".
    2. **A new `releaseHostDispatchTx(ctx, tx, owner, taskID, reservationID)`**
       runs under the owner's advisory lock, which `EndHostDispatch` already
       holds:
       - when `reservationID` is the task's latest **unreleased**
         reservation (`chiefStartRows`), it writes
         `ActionChiefDispatchReleased` with
         `{host_dispatch_id, reservation}`, **whether or not an agent row
         exists**. That is `ReleaseRefusedChiefDispatch`'s keying, by
         reservation, extended to the case where the dispatch did produce a
         window;
       - when a newer reservation exists, or this one is already released, it
         writes nothing, because that start is no longer this dispatch's.
    3. **`EndHostDispatch(ctx, id, why)`** sets `ended` and, for a row with a
       task, calls `releaseHostDispatchTx` with the row's `reservation_id`,
       **in the same transaction**. Ending a dispatch and releasing its task's
       start commit together, or not at all.

       `ReleaseChiefDispatch`'s pod body is unchanged, apart from the branch in
       change 1.
  - B skips host reservations, so there is no double count while the
    10-minute window is open.
  - C does not depend on that window, so nothing drops out after 10 minutes.
  - A host dispatch's `task.chief_dispatch_agent` row (detail
    `{host_dispatch_id}`) is written for the audit trail only, and A does not
    count it.
- **When a host slot is freed.** `host_dispatches.status` alone decides it,
  and the one ledger in [3.7](#37-linking-the-spawned-window-to-the-dispatch-and-the-task)
  bounds who writes it. `ended` is written only by `EndHostDispatch`, which
  only the reconciler and `agent_destroy` call. An offline host's dispatches stay `pending` or
  `running`, so they keep their slots (fail-closed), until the sweep there ends
  them. The cost is a slot held while a laptop sleeps. The alternative would
  let the chief exceed the cap whenever a host disconnects.

**One predicate for "a live agent on this task".** Today these read only the
`agents` table:
- `chiefDispatchTaskBlock` ("already has an agent working it",
  `task_actions.go:115-136`);
- `agentsOnTask` (`task_routing.go:399-412`), and through it:
  - the already-dispatched answer (`:753`);
  - `routeTaskNotice`, which decides whether a task notice goes to a working
    agent or to the chief (`:1015`);
  - mention routing (`routeMention`, `:1088`, then `actOnMention`, `:1151`).
    It delivers only to a pod (`*agents.Agent`). **A mention on a task that
    only a host agent works is not dropped, and is not pasted into the
    window.** It goes to the chief as a task notice, through the same
    `handTaskToChief` (`:1043`) that an unrouted notice takes. The chief can
    then `task_forward` it, so the paste goes through the gated path in
    [3.10](#310-forwarding-into-a-host-spawned-agent). A mention that names a
    pod is unchanged;
  - `agent_destroy`'s check for whether a task in progress still has anyone
    on it (`agent_destroy_tool.go:195`).

All of them get one shared helper, `liveOnTask`, which also reads
`host_dispatches` rows for the task whose status is `running`. Without it, a
host-spawned agent on a task would let a pod start beside it, and a notice or
mention would skip it. Destroying a pod would also move a task back from
in-progress while a host agent was still working it. `task_forward` uses the
same helper to find its target, so it reaches a host-spawned window too
(decision 9, [3.10](#310-forwarding-into-a-host-spawned-agent)).

#### 2.6.1 `request_key`: idempotency

Decision 8. A chief that times out and calls again must not start a second
agent.

- **Shape.** `^[A-Za-z0-9._:-]{1,64}$`. Anything else is refused:
  `"request_key must be 1-64 of [A-Za-z0-9._:-]"`.
- **When it is required.** It is required with no `task_id`, because that is
  the only dispatch with no other idempotency. It is optional with a task, and
  when given it has the same meaning; the task's own one-start rule still
  applies after it. The description tells the chief to mint one key per
  request from its owner and to reuse it on every retry of that request.
- **Scope: per owner.** A key is looked up among the owner's own reservations
  only (`user_id = owner`, on top of row-level security, as every ledger read
  is, `chief_dispatch.go:225-228`). Two owners can use the same key without
  meeting.
- **Lifetime: 24 hours**, the same `ChiefDispatchWindow` the daily cap uses
  (`chief_dispatch.go:49`). A key matches only reservations made within that
  window. After it, the same key starts a new dispatch. The audit rows
  themselves are kept, as all audit rows are.
- **Where it lives.** In the reservation's own audit row: `detail.request_key`,
  and `detail.request_hash`, the SHA-256 of the call's normalised parameters as
  canonical JSON. Those parameters are `target`, `task_id`, `repo` lowercased,
  `base`, `model`, `worker_pool`, the prompt after cleaning, and `owner_asked`.
  There is no new table. A partial index, in migration
  `0072_chief_dispatch_request_key.sql`, keeps the lookup cheap:
  `CREATE INDEX … ON audit_log (user_id, (detail->>'request_key')) WHERE
  action = 'task.chief_dispatch' AND detail ? 'request_key'`.
- **When it is checked.** First, under the owner's advisory lock
  (`chief_dispatch.go:221`), before the task's prior-start check, before
  `check()` and before the caps. Two concurrent calls with one key are
  therefore serialised: the second one finds the first's reservation.
- **Same key, same parameters: a replay.** The call returns the first
  dispatch's result **as it stands now**: `{replayed: true, agent_id |
  host_dispatch_id, agent | window, status, budget}`, with `status` read at the
  call (for example `starting` while the first call's agent is still being
  created). A replay starts nothing and spends nothing. It runs no owner-turn
  check, because it only returns what the first call already answered.
- **Same key, different parameters: refused** (the owner's recommendation,
  adopted). The sentence is `"request_key %q was used in the last 24 hours for
  a different dispatch: use a new key for a new request"`. It spends nothing.
  The other choice, returning the first result, would let a changed target or
  prompt silently answer as if it had run.
- **A first call that failed.**
  - A call refused **before** its reservation writes nothing, so its key is
    not recorded and a retry is judged afresh. That is the right outcome,
    because nothing ran.
  - A reservation released after its create was refused
    (`ReleaseRefusedChiefDispatch`) also does not hold its key. The marker is
    the release row's `detail.refused_reservation = <reservation id>`
    (`chief_dispatch.go:528`), and the key lookup skips any reservation named
    by one. **A refused host spawn frees its key the same way.**
    `EndHostDispatch` with a refusal writes the release row with
    `{host_dispatch_id, refused_reservation: <reservation_id>}`. Every other
    way a host dispatch ends writes `{host_dispatch_id, reservation:
    <reservation_id>}`, which does **not** free the key, because that dispatch
    may have run. It made
    nothing, so a retry tries again.
  - A reservation whose create failed **after** it was spent (fail-closed,
    `task_routing.go:846`) does hold its key. The replay answers
    `{replayed: true, started: false, next: "the first call with this key
    reserved a start that failed; use a new request_key to try again"}`.
    Retrying it therefore takes a deliberate new key, which spends a new unit.

### 2.7 The result

On success the tool returns `{target, task_id?, agent_id | host_dispatch_id,
agent | window, next, budget}`. For a host spawn, it also returns
`{host, session: "agents", window: "@N", worktree, branch}`, so the chief can
tell its owner where the agent is running.

`next` for a host spawn reads: *"Started in a new window of the agents session
on `<host>`. It asks your owner before using tools, and those asks reach them
on muster. Do not start another."* For opencode the result also carries
`model`.

### 2.8 Mirrored dispatch: choosing host, runtime and model

Decisions 10-13 say how the owner's words map to a target. The mapping itself
is the chief's reading of a sentence, so it lives in the chief's instructions
and `agent_dispatch`'s description. That part is **prose**, and it is stated
as prose here. The owner chose this over a deterministic word check
(decision 14). What muster enforces **structurally** is the rest:
a host target only ever starts in a turn the owner typed, or on the owner's own
tap on a card that such a turn raised, and it starts **exactly** what the owner
named or tapped.

**The mapping.**

| the owner says | target | `model` |
|---|---|---|
| no "mirrored" | `pod/…`, under PR A's cluster target rules | as for pods |
| "mirrored on `<host>` with Claude" | `host/<host>/claude` | none |
| "mirrored on `<host>` with `<model>`" | `host/<host>/opencode` | the OpenRouter id of `<model>` |
| "mirrored with Claude", no host | `host/?/claude` (held: host card) | none |
| "mirrored with `<model>`", no host | `host/?/opencode` (held: host card) | the id |
| "mirrored on `<host>`", no model | `host/<host>/?` (held: model card) | none |
| "mirrored", nothing else | `host/?/?` (held: host card, then model card) | none |

- **"Claude" always means Claude Code**, even when a Claude model is also
  reachable through OpenRouter. The owner's rule is about the word, so a
  request to run a Claude model under opencode has to name the OpenRouter id
  in full (`anthropic/…`).
- **Turning a model's name into an OpenRouter id** is the chief's job. When
  the chief is not sure of the exact id, it holds the model as blank and lets
  the card ask, rather than guessing. A wrong id is refused by the host
  (`bad_model`) before anything starts.
- **`repo` is not carded.** A host target still needs `repo`
  ([2.2](#22-parameters)). When the owner did not name one and the request
  has no task, the chief asks in the chat, as today.

**The held dispatch.** A call with a held target runs every guard a host
dispatch runs up to the reservation, in the same order: the opt-in, the
target's shape, `request_key` (required for a held call, with a task or
without, because it is what ties the completing call to the hold), the
prompt, `connectedRepo`, and **`ownerTurnFor`** (`owner_asked: true` is
required). Then, instead of reserving, it:

1. computes the options:
   - **host card:** the owner's `hostSpawnable` hosts, filtered to those whose
     hello offers the runtime when the runtime is known, most recently
     dispatched-to first;
   - **model card:** `Claude (Claude Code)` when the host offers `claude`,
     then the models of `MUSTER_CHIEF_DISPATCH_MODELS` when the host offers
     `opencode`, then "another OpenRouter model (type its id)" (decision 15,
     "The model list" below);
2. refuses, with no card, when a blank has **no** option: `"no host can start
   agents now (none of your hosts is online with -agent-spawn); nothing was
   asked or queued"`, or `"host %q offers neither claude nor opencode now"`;
3. writes an audit row, `chief.dispatch_held`, as the owner. Its detail is
   `{request_key, held_hash, blanks: ["host"|"runtime"|"model"], options,
   ask_id, owner_message}`:
   - `held_hash` is `request_hash` ([2.6.1](#261-request_key-idempotency))
     computed with each blank written as `?`, so the prompt, the repository,
     the base and the task are fixed **now**, in the owner's turn;
   - `owner_message` is the id of the owner's typed message that
     `ownerTurnFor` returned;
4. raises **one** `ask_owner` card (PR A) for the first blank: the host card
   when the host is blank, otherwise the model card. Its stored options are
   the labels above;
5. answers `{held: true, ask_id, blanks, next}`, where `next` is: *"muster
   asked your owner which host to run this on. When their answer arrives,
   call agent_dispatch again with the same request_key, the same arguments,
   and the target they chose; do not choose one yourself."* A held call spends
   nothing and holds no live slot.

A held row is not a reservation, so [2.6.1](#261-request_key-idempotency)'s
reuse rule does not see it. A second held call with the same key and the same
`held_hash` answers the first card again (`replayed: true`) and raises no
second card. The same key with a different `held_hash` is refused with the
reuse sentence.

**Completing it.** The answer reaches the chief as a turn its owner did not
type, so `ownerTurnFor` refuses it. That is why the hold exists. The chief
calls `agent_dispatch` again with the same `request_key`, the same arguments
and the blank filled in. Under the owner's reservation lock, where the key is
already looked up ([2.6.1](#261-request_key-idempotency)), the call is
accepted **in place of `ownerTurnFor`** only when all of these hold:

- a `chief.dispatch_held` row of this owner's has this key, is at most **15
  minutes** old, and is not yet consumed (no reservation names it);
- the call's `request_hash`, with the held blanks written as `?`, equals the
  row's `held_hash`. So the chief can change nothing but the blanks;
- each filled blank equals the owner's answer to that blank's card. The answer
  must be:
  - one of the card's stored options, or, for the model card's free-text
    option, an id that passes `model`'s shape;
  - recorded through a door that proves the owner tapped or typed it: the
    signed-in Attention view (`ViaSession`) or the phone (`ViaDevice`). An
    answer through `attention_answer` from a mirrored session, or any other
    principal, does not count. This is the same door rule as #485's
    release, and they share one predicate (`attention.IsOwnerDoor`);
- a second blank (`host/?/?`) has its own card, raised by the completing call
  for the first blank. That call writes a new `chief.dispatch_held` row with
  the first blank now filled, and the second blank's card. A row is consumed
  by the reservation that names it, so the same tap can never start two
  agents.

When any of these fails, the call is refused with its own sentence, so the
chief and a test can tell them apart:

| refusal | sentence |
|---|---|
| no held row, or older than 15 minutes | `"request_key %q holds no dispatch waiting for your owner's answer (or it is more than 15 minutes old): ask them again in the chat"` |
| arguments changed | `"request_key %q was held with different arguments: only the blank your owner answered may change"` |
| filled blank is not the owner's answer | `"your owner chose %s for this dispatch: call agent_dispatch with exactly that"` (the sibling of `agentDispatchNotTheOwnersTarget`) |
| answer not through an owner door | `"your owner's answer to that card did not come from muster's Attention view or their phone, so muster cannot take it as theirs"` |
| card not answered yet | `"your owner has not answered the card yet: wait for their answer"` |

Once accepted, the call continues as a host dispatch: the reservation, with
`detail.held = <row id>` (which consumes the row), `onReserved`, and the spawn
([3](#3-the-host-spawn-protocol)). Every `agent_dispatch` guard after the
owner's turn still runs, the caps included.

**If the owner answers by typing** in the chat instead ("on `<host>`"), that
turn **is** an owner's typed turn. The chief can then call `agent_dispatch`
with the full target and `owner_asked: true`, and `ownerTurnFor` passes as
usual. The held row then simply expires.

**Why muster raises the card, and not the chief.** The chief could call
`ask_owner` itself. But then nothing would tie the answer to the request it
belongs to, and the chief's next turn could not show muster that the owner
asked for the start. The hold ties three things together, under one key: the
owner's typed request, the card it raised, and the owner's tap. That is the
same reason #485 binds a "Release & dispatch" tap to the target
(`ownersChosenTarget`, `internal/api/agent_dispatch.go:371`).

**What this needs from PR A.** The design relies on these properties of
`ask_owner`, and the phase 3 PR checks each one against PR A's merged code
before it builds on it:

1. a card is an attention ask with **stored options** (and, for the model
   card, a free-text answer, decision 15) and an id, and its
   answer is recorded with the door it came through (`AnsweredVia`, as for
   today's asks);
2. muster's own code can raise one for the chief (not only the chief's tool
   call), linked to the chief's chat;
3. the answer is delivered to the chief as a turn, so the chief can act on it;
4. the live dispatched-agent card can be keyed by something other than a pod
   agent row ([3.7](#37-linking-the-spawned-window-to-the-dispatch-and-the-task)).

If PR A's `ask_owner` instead keeps the chief waiting **inside** the owner's
typed turn (an `attention_wait` loop), `ownerTurnFor` already passes on the
completing call, and the hold is only a record. The binding rules above still
apply, so neither shape opens anything the other closes.

**Checked against PR A's merged code (#496, at `91a52662`): properties 1, 2
and 4 did not hold; the stored-card change (the PR before phase 3) makes all
four hold.** The owner chose to extend `ask_owner` rather than drop the hold.

| property | at #496 | now |
|---|---|---|
| 1. an attention ask with stored options, an id, and `AnsweredVia` | **no**: the card was the chief's stored tool call, its answer never stored | **holds.** `ask_owner` files an attention ask raised by the chief (`raiseOwnerCard`, `internal/api/owner_ask.go`), with the option labels as its stored options; its result names it (`ask_id`). An answer records its door: `browser` or `device`, stamped by `attention.Answer` from the answering principal (migration 0092), or `mcp`. `AnswerSource.OwnerDoor()` is the binding's door rule read back from the row; `attention.IsOwnerDoor` is the same rule for the answering principal (one mapping, `answerVia`), which #485's release uses too. An owner card moves only through those doors: `attention_answer` over MCP is refused (`ErrNotOwnerDoor`). The tap is bound to the exact option by `Ask.ChosenOption()`: the answer is exactly one of the row's stored options. The "Other" box is kept |
| 2. muster's own code can raise one for the chief | **no** | **holds for the Attention view and the phone; the chat card comes with phase 3.** `raiseOwnerCard(ctx, chief, question, options)` is the one raise, for the tool and for server code; it pushes like any ask (`askFiled`), so the card is on the Attention view and the phone. With this PR alone, a card raised by server code has **no chat card**: the chat draws a card only from an `ask_owner` tool call's result. Phase 3 (#516) adds the chat card for a held dispatch's card |
| 3. the answer reaches the chief as a turn | yes | **holds, by two paths.** An answer on the Attention view or phone is delivered as a turn (the attention delivery, "your owner answered attention #N"). A tap in the chat is still the owner's next typed message, which also settles the card in session (`askerAddressed`) |
| 4. the live agent card can be keyed by other than a pod agent row | **no** | **holds.** `ReadDispatchResult` reads `host_dispatch_id`. A card keyed `host-<id>` is served by `/ui/dispatch-card?host_dispatch=<id>` from `Server.hostDispatchCardSource`, which phase 3 wires to `host_dispatches` |

Two consequences for the binding below. A tap in the chat is the owner's typed
turn, so a completing call made in that turn passes `ownerTurnFor` and needs no
hold (the "owner types instead" path). An answer settled in session therefore
never counts as `OwnerDoor()`. Only an answer recorded through the Attention
view or the phone completes a hold from a turn the owner did not type.

**The model list** (decision 15). `MUSTER_CHIEF_DISPATCH_MODELS` is a
comma-separated list of OpenRouter model ids, read at server start, each
checked against `model`'s shape ([2.2](#22-parameters)); a malformed entry
stops the server with its name, as other `MUSTER_*` settings do. It is **editable
configuration**, not a recommendation baked into code. Its default is four
coding models, each confirmed present in OpenRouter's public model list
(`GET https://openrouter.ai/api/v1/models`) when this was written:

```text
qwen/qwen3-coder,moonshotai/kimi-k2-0905,z-ai/glm-4.6,deepseek/deepseek-v3.2
```

- An empty value offers no listed models, only Claude and the typed id.
- The list says nothing about whether a **host's** opencode can run a model:
  the host still checks each one against its own catalog when the spawn runs
  (`bad_model`, [3.6](#36-the-commands-and-prompt-delivery)), so a stale entry
  costs a refusal, never a wrong start.
- **The typed id is always offered.** It needs PR A's card to take a free-text
  answer. If PR A's card does not, phase 4 adds a free-text option to it, so
  this becomes a fifth property checked against PR A before building. A typed
  id binds a completing call like a tapped option does: the owner typed it
  through an owner door, and it must pass `model`'s shape.

**The phone.** The phone draws at most two options beside Dismiss
(`AskPush.buttons()`, per #485). A card with more options shows the first two
there, ordered as above, and every option in muster's chat and Attention view.

---

### 2.9 Held dispatches from the owner's sessions

**Built** (decision 19; `internal/api/dispatch_hold_tools.go`,
`internal/plugins/dispatch_hold.go`). The gap: a mirrored Claude Code session
whose owner asked it to start an agent had no path. `chief_ask` only relays,
and the chief refuses a start that is not from a turn its owner typed
(`ownerTurnFor`), which is the rule that keeps outside text from starting
agents.

- **`dispatch_request`** runs `agent_dispatch`'s checks that need no chief,
  through the same predicates: the opt-in and the deployment
  (`chiefDispatchDeploymentRefusal`), the target (`podKindOf`; mirrored and host
  targets refused as there), the prompt cap, the task (`chiefDispatchTaskBlock`),
  the repository (`connectedRepo`), the kind (`resolveKind`), the budget (the
  caps' own predicates), and the model. It then files a **hold** and starts
  nothing. It takes no idempotency key (Round 0 dropped it): a repeated
  request files a second hold, which starts nothing and lapses in 15 minutes,
  and approval's single use under the lock already prevents a double start.
- **The model** goes through `resolveDispatchModel`, the dispatch-outcomes
  change's one model check: a catalogue id or a display name naming one model
  resolves to `openrouter/<id>`, and the hold carries the resolved model (and
  the value as given).
- **`dispatch_approve`** reserves on the chief's ledger, naming the hold, then
  runs `startReserved`, the post-reservation path `agent_dispatch` now shares:
  `createAndDispatchAgent`, the ledger's agent row, and the release of a refused
  start. Then it writes an approval audit row and pushes the owner once. The
  chief's chat gets nothing: the reservation already puts the agent in the
  chief's budget (`live_agents`). The start is outcome-tracked
  (`TrackOutcome`, with the owner's chief as `DispatchedBy`), so it gets the
  silence watchdog and the waiting, failed and done notifications.
- **Who.** Both verbs are listed only on `POST /mirror/mcp`, dispatched there
  before the tools a gateway agent can name, and each handler refuses any
  caller that is not a mirrored session acting through a mirror host's token
  (`authz.ViaMirrorHost`). A hold is read as its owner only, so another owner's
  session finds nothing to approve.
- **Single use and expiry** are decided under the owner's reservation lock: a
  hold is open while it is at most 15 minutes old, by the database's clock, and
  no reservation names it. The reservation that names it is its consumption, so
  two approvals of one hold start one agent.

**The store is shared with the card path.** A hold is one `chief.dispatch_held`
audit row whose detail is the request as it will start, consumed by the
reservation whose `detail.held` names it. That is the store this page's held
dispatch for a card tap ([2.8](#28-mirrored-dispatch-choosing-host-runtime-and-model))
describes. The MCP door files its holds with `door: "mcp"`. A card hold would
file the same action with its own door and its blanks, and consume it the same
way. There is no new table and no migration.

**Residual risk, accepted with the decision.** A prompt-injected model in one
of the owner's mirrored sessions can call both verbs. What bounds it: the door
is the owner's own sessions only; each request and approval is an audit row
naming the host and the session; each start pushes the owner; the chief's caps
apply; and the started agent opens a draft
pull request at most.

---

## 3. The host spawn protocol

### 3.1 Wire messages and capability

These messages copy `-resume-spawn` ([1.3](#13-the-one-host-side-spawn-that-exists--resume-spawn)).
There is no protocol version bump. An older muster ignores a hello field it
does not know, and its read loop has no `default` case, so it also ignores a
message type it does not know (host-sessions, "The wire";
`internal/mirror/hub.go:800-914`).

- **`hello.agentSpawn: ["claude", "opencode"]`**: the runtimes this host can
  start. These are the runtimes whose binaries resolve on the tmux server's
  PATH when the mirror starts. **`opencode` is listed only when the host also
  sends to opencode** (`hello.openCodeSend`: `-opencode-send` and a token,
  #486), because the spawn posts the first prompt through that same API
  ([3.6](#36-the-commands-and-prompt-delivery)). The hub stores the list only
  for a v2 host with `interactive` set, as `hub.go:717` does for
  `resumeSpawn`. Nothing else is advertised (D6): there is no separate offer
  message, no repository list and no model list.
- **`agent-spawn`** (muster → host): `{id, dispatch, runtime, repo, base,
  prompt, name, model}`. `model` is set only for `opencode`, as
  `openrouter/<vendor>/<model>`; the host refuses it on `claude`.
  - `id` comes from the connection's id space (`hc.next`).
  - `dispatch` is the `host_dispatches` row id.
  - `runtime` is `claude` or `opencode`.
  - `repo` is `<owner>/<repo>`. **The host resolves it when the spawn runs:**
    exactly one git clone directly under its roots must have an `origin` URL
    that parses as that GitHub repository. Otherwise it refuses with `no_repo`,
    and the refusal names the repository.
  - `base` is a branch name. Empty means the remote's default branch.
  - `prompt` arrives already cleaned and wrapped by muster
    ([2.2](#22-parameters)).
  - `name` is the short display label: `#<task> <title>`, or the prompt's
    first words. It is used only for the tmux window name, after
    [3.4](#34-the-tmux-agents-session-and-window-names) sanitises it.
  - The host decodes every message into the one shared `mirror.Msg` struct, so
    a field `Msg` does not have is dropped by the decoder, as it is for every
    other message type. The host does not refuse unknown fields. It acts only
    on the named fields above, and validates each one.
- **`agent-spawned`** (host → muster):
  - on success: `{id, dispatch, session, window, pane, worktree, branch,
    agentSession}`;
  - on failure: `{id, dispatch, refusal: <code>, detail}`.
  - `agentSession` is the runtime's session id, which the host chose
    ([3.6](#36-the-commands-and-prompt-delivery)).
- **Listing:** each `Window` gains `dispatch` (an int64, omitted when empty),
  read from the window's `@muster-dispatch` option. An older muster ignores
  it.

There is **no kill message** (D7). Closing the tmux window, or the agent
exiting, ends the dispatch, and the listing reports it
([3.7](#37-linking-the-spawned-window-to-the-dispatch-and-the-task)).

**Server side.** `Hub.SpawnAgent` copies `Hub.SpawnResume`: the owner-scoped
lookup, an id from `hc.next`, a reply channel and a timeout. The timeout is
**150 s, not 15 s**, because a `git fetch` comes first. The host's own spawn
deadline is **120 s**, measured from when it receives the request, queue wait
included, so a reply normally arrives inside the wait
([3.7](#37-linking-the-spawned-window-to-the-dispatch-and-the-task), "Three
deadlines").

A timeout after the write means **unconfirmed**, not failed, as it does for a
paste (`pasteTo`, `internal/mirror/transcript.go:705-740`).
- The row stays `pending` and keeps its live slot.
- A late reply is applied through the late path, keyed by `dispatch`.
- A listing that carries the window's `dispatch` id makes the row `running`.
- If neither arrives, the reconciler counts the row down once it is more than
  180 s old.

**Refusal codes.** Five codes cover everything the host can refuse
(`bad_model`, the `no_repo` cause `not on this host's allow-list`, and the
`unavailable` causes `rate limited` and `audit log unwritable` are this
revision's; [5.5](#55-a-compromised-server)). Each code
names the one thing the chief can do about it. `detail` is a fixed sentence
for the owner and the logs; the chief acts only on the code.

| code | covers | the chief's sentence, and what it can do |
|---|---|---|
| `unavailable` | the host's own preconditions: no `-agent-spawn`, no `-interactive`, not the default tmux server, no tmux server running, the runtime binary not found; for opencode, no `-opencode-send`, no token, the service unreachable, or the TUI's version differs from the service's (the launcher's own refusal, #486); the host's spawn rate limit; the audit log cannot be written | `"host %q cannot start %s agents now: <detail>"`. Offer your owner another target |
| `no_repo` | no clone of `<owner>/<repo>` under the host's roots, or more than one; or the repository is not on the host's `-agent-spawn-repos` allow-list | `"repo %s has no single clone on host %q: <detail>"`. Ask your owner which repository, or which host |
| `bad_base` | `base` is not a valid branch name, or does not exist on the remote | `"branch %q does not exist on %s's remote"`. Ask your owner which branch |
| `bad_model` | opencode only: `model` is not `openrouter/<vendor>/<model>`, the host's opencode has no `openrouter` provider, or that provider does not list the model (`GET /api/model`) | `"host %q's opencode has no OpenRouter model %q"`. Ask your owner which model |
| `failed` | anything after work started: the fetch, the worktree, tmux, the tag, the landing check, the trust entry ([3.6.1](#361-claude-codes-folder-trust-entry)), or opencode's prompt. The host removes whatever it created | `"the agent could not be started on %q: <detail>; tell your owner"`. Do not retry with the same `request_key` |

`not_listed` is not a refusal. It means the window exists but has not yet
appeared in a listing on this connection. The row stays `pending`, and the
next listing settles it. A request muster built wrongly, with an unknown
runtime or a prompt outside the limits, is a muster bug. The host refuses it as
`failed`, with a detail that says so.

**Note: the pattern copied here has no production mileage.** `-resume-spawn`
has never been enabled on the owner's hosts (per the owner). Its tests are the
only exercise it has had, so phase 3's first live run is also the first real
run of the pattern ([6](#6-phases)).

### 3.2 The flag

The flags follow the mirror's naming convention, `-kebab` plus
`MUSTER_MIRROR_<SNAKE>` (`cmd/muster-mirror/main.go:93-115`):

| flag | env | default | meaning |
|---|---|---|---|
| **`-agent-spawn`** | `MUSTER_MIRROR_AGENT_SPAWN` | **false** | let the chief start agents on this host. It has no effect without `-interactive`, and the mirror logs that it has none, as it does for `-resume-spawn` (`:380-385`) |
| `-agent-spawn-roots` | `MUSTER_MIRROR_AGENT_SPAWN_ROOTS` | `~/src` | colon-separated directories whose direct children are the clones worktrees may be cut from |
| `-agent-spawn-dir` | `MUSTER_MIRROR_AGENT_SPAWN_DIR` | `$XDG_DATA_HOME/muster-mirror/agents` | where the worktrees go ([3.3](#33-worktrees)) |
| **`-agent-spawn-repos`** | `MUSTER_MIRROR_AGENT_SPAWN_REPOS` | **empty: no repository** | comma-separated `<owner>/<repo>` names this host will cut a worktree of, or `<owner>/*` for every repository of that owner (the repository part exactly `*`; owners and names compare case-insensitively). With a wildcard, `connectedRepo` (server) and the clones under the roots (host) are the per-repository bound within that owner; the host is sent the connected repository's own name. A repository not listed is refused `no_repo` (detail `not on this host's allow-list`) whatever muster sends ([5.5](#55-a-compromised-server)). Empty means `-agent-spawn` is on but nothing can be spawned, and the mirror logs that at startup |
| `-agent-spawn-rate` | `MUSTER_MIRROR_AGENT_SPAWN_RATE` | `3/10m,10/24h` | the host's own spawn rate limit ([5.5](#55-a-compromised-server)) |
| `-agent-spawn-log` | `MUSTER_MIRROR_AGENT_SPAWN_LOG` | `$XDG_STATE_HOME/muster-mirror/agent-spawn.jsonl` | the append-only audit log ([5.5](#55-a-compromised-server)) |

**Revised: the host now keeps a spawn rate limit of its own.** Round-0
disposition D3 said the server's budget, 5 live and 10 a day, was the only
limit on spawns, and that still holds for a server working as designed
([5.3](#53-caps)). But a server's budget is no limit at all once the server is
the thing that misbehaves, which is the threat [5.5](#55-a-compromised-server)
is about. Its daily part equals the server's daily cap, so a server that keeps
its budget never meets that part. Its 10-minute part can refuse a burst the
server would allow (a fourth spawn within ten minutes); that is the price of
bounding a burst, and the refusal says so (`unavailable`, detail `rate
limited: 3 spawns in the last 10 minutes`).

The owner's host configuration sets `-agent-spawn` on the owner's default
mirror unit **only**. It never sets it on a `muster-mirror@<tenant>` unit
([multi-tenant.md](multi-tenant.md)).

### 3.3 Worktrees

**Where.** `<agent-spawn-dir>/<owner>__<repo>/<dispatch>`, for example
`~/.local/share/muster-mirror/agents/<owner>__<repo>/417`.
- The path is outside every clone and every root. An agent's worktree is never
  inside a checkout the owner works in, so the owner's own tools that walk a
  checkout (search, file watchers, a build) never see it.
- The dispatch id makes the path unique, and shows which dispatch it belongs
  to. Ids come from a sequence, so **a path is never reused**.

**Branch.** `muster/<dispatch>-<slug>`. The slug is at most 40 characters of
`[a-z0-9-]`, made from the window name. The dispatch id makes the branch name
unique. An existing branch of that name is a leftover, so the spawn refuses
(`failed`), and never reuses or resets that branch.

**Base.** `origin/<base>` after a fetch. `<base>` is the request's `base`, or
the remote's default branch when that is empty
(`git symbolic-ref refs/remotes/origin/HEAD`). It is never the clone's local
branch and never its `HEAD`, so a clone that is behind or dirty has no effect
on what the agent starts from.

**Commands.** Each is an argv run through `exec.CommandContext`, never a shell
string. `<clone>` is resolved from `repo` when the spawn runs
([3.1](#31-wire-messages-and-capability)), with `filepath.EvalSymlinks`, and
it must still be under a root.

```text
git -C <clone> fetch --no-tags origin <base>
git -C <clone> rev-parse --verify --quiet refs/remotes/origin/<base>^{commit}
git -C <clone> worktree add --no-track -b muster/<dispatch>-<slug> <dir> refs/remotes/origin/<base>
```

- **`base` is validated before it reaches git.** It must pass
  `git check-ref-format --branch`, must not start with `-`, and must have no
  `..` segment (otherwise `bad_base`). That closes argument injection into
  `fetch`.
- **The owner's checkout is never touched.** `fetch` writes refs and objects.
  `worktree add` writes a new directory, plus an entry in the clone's
  `.git/worktrees/`. Neither changes the clone's working tree, its index or
  its checked-out branch. **A dirty clone is therefore not a refusal.** The
  only shared write is the branch name, and the dispatch id makes that unique.
- **What the worktree does not carry.** It gets no submodules, and no
  untracked files such as a gitignored `.envrc` or `.env`. The host
  initialises submodules only when the repository has a `.gitmodules` file
  (`git -C <dir> submodule update --init --recursive`). It runs that step
  under its own budget, inside the spawn deadline
  ([3.7](#37-linking-the-spawned-window-to-the-dispatch-and-the-task), "the
  order of the steps"). A submodule failure, including one caused by that
  budget running out, is a warning in the result, not a refusal. The host never copies
  untracked files from the clone: their content is the owner's, possibly
  credentials, and the agent did not ask for them.
- **Concurrency.** Spawns on one host run one at a time, behind a mutex. The
  120 s spawn deadline starts when the request is **received**, not when it
  takes the mutex. So a spawn that queues behind a slow one runs out of time,
  and is refused before it can start an agent
  ([3.7](#37-linking-the-spawned-window-to-the-dispatch-and-the-task)).

**Cleanup.** A worktree is never force-removed.

- **When it is cleaned up.** When the window ends: the owner closes it, or the
  runtime exits and then the pane's shell exits. Then
  `git -C <clone> worktree remove <dir>` runs **without `--force`**. A
  worktree with uncommitted changes therefore refuses to go, and stays.
- **Branches.** A branch with commits the remote lacks is kept, and so is its
  worktree. That means `git rev-list --count @{push}..HEAD` > 0, or no
  upstream at all while the branch has commits beyond the base. A branch with
  no new commits is deleted along with its worktree, with `git branch -d`,
  never `-D`.
- **A sweep** runs at mirror start, and every hour after that. It applies the
  same rule to every directory under `<agent-spawn-dir>` whose dispatch id
  names no live tagged window, so a host crash leaves no worktree unaccounted
  for. It walks only `<owner>__<repo>/<dispatch>` directories. Prompt files
  are elsewhere ([3.6](#36-the-commands-and-prompt-delivery)), and a separate
  hourly pass deletes any prompt file there that is more than an hour old. The
  host
  logs each worktree it keeps, with the reason. A kept worktree stays on disk
  until the owner removes it by hand.
- **The trust entry is not removed** (D5). It is keyed by a path that is never
  reused, so once its directory is gone it is inert
  ([3.6.1](#361-claude-codes-folder-trust-entry)).

### 3.4 The tmux `agents` session and window names

- **Session `agents`**, on the mirror's own tmux server, which for the default
  mirror is the default server ([3.8](#38-read-only-and-tenant-hosts)).
  - If `has-session -t =agents` succeeds, the host uses `new-window -t
    =agents:`.
  - Otherwise it uses `new-session -d -s agents`, and on "duplicate session"
    it retries as `new-window`. This is `resume.go:351-388`, with the session
    name fixed.
  - The tmux server must already be running. If it is not, the spawn is
    refused as `unavailable` (detail `no tmux server`), and the host compares the server's pid
    before and after the call (host-sessions "Environment"). This way a mirror
    unit with a bare PATH never starts the owner's tmux server.
- **Window name:** `<cc|oc>-<dispatch>-<slug>`, for example `cc-417-fix-login`.
  - It is set with `-n`, with `automatic-rename off` on that window only
    (`set-option -w -t <window id> automatic-rename off`).
  - The slug is `[a-z0-9-]{0,24}`. That makes the name safe against tmux's
    trailing-`;` command splitting (host-sessions "The command").
- **Identity:** the window option `@muster-dispatch <dispatch>`.
  - It is set by a separate `set-option -w -t <window id>` call after the
    window exists. A failure kills the window and reports `failed` (detail `tag not set`)
    (host-sessions measured that tmux command lists are not all-or-nothing).
  - It is a **window** option, not host-sessions' session option, because many
    agents share one `agents` session.
  - It is read with `show-options -w -t <window id> -qv @muster-dispatch`,
    without `-A`, so a global value is never inherited. The host refuses to
    spawn while a global `@muster-dispatch` is set.
- **Landing check:** after creation,
  `display-message -p -t <pane> '#{pane_current_path}'` must equal the
  worktree directory. Otherwise the host kills the window and reports
  `failed` (detail `landed outside the worktree`). (tmux returns 0 for a missing `-c` path and opens in the home
  directory instead.)
- **Listing before reply:** `agent-spawned` is written only after this
  connection has sent a listing that contains the window. This is
  host-sessions' `listWindow` guarantee, which also states why `RefreshNow` is
  not enough. Otherwise the host writes `not_listed`, and the server keeps the
  row `pending`.

### 3.5 Environment

- **What it copies from `resumeEnv()`** (`internal/mirrorhost/resume.go:390-401`):
  the daemon's environment minus `TMUX`, `TMUX_PANE` and `PATH`, so the window
  takes the tmux server's PATH. It runs under the login shell that
  `loginArgv` picks (`:412-442`), which reads the owner's profile.
- **What it does not add.** The `update-environment` copy from host-sessions
  ("Environment") is **not** done. A `new-window` into an existing `agents`
  session inherits that session's environment, which avoids the problem
  host-sessions had to solve by copying values. An `agents` session that
  `new-session` creates from the unit gets only the unit's values. That
  matches how `-resume-spawn` already behaves.
- **Nothing from muster** reaches the environment.
- **MCP.** A spawned `claude` reaches muster's MCP through the owner's
  user-scope `mcpServers.muster` entry, as any hand-started session does
  (host-sessions "The MCP server for mirrored sessions"). No secret is
  exported for it.

### 3.6 The commands, and prompt delivery

The host builds the argv itself, from constants plus values it created: the
session id it chose, and the path of the prompt file it wrote. muster sends no
flag, no path and no binary. The spawn has **no permission flag of any kind**,
so both programs start in their normal modes (decision 3). See
[5.1](#51-who-can-trigger-a-spawn) for what a repository's own settings can
still change.

**Why the prompt never goes into a tmux argument.** tmux splits a command on
an argument that ends in `;`. The `;` is removed, and the next word starts a
new tmux command, so a prompt word such as `tail;` would be cut. That was
measured on tmux 3.7c (host-sessions, "The command"). tmux(1) describes `;` as
its command separator and `\;` as a literal semicolon, but this design does not
rely on escaping: the prompt never becomes a word tmux parses. Instead:

1. The host writes the prompt to `$XDG_RUNTIME_DIR/muster-mirror/prompts/<dispatch>`.
   - The directory has mode `0700`. The file is created with
     `O_CREAT|O_EXCL`, mode `0600`.
   - This is outside `<agent-spawn-dir>` and its dispatch namespace, so the
     worktree sweep never meets it ([3.3](#33-worktrees)).
   - It is outside the worktree too, so it can never be committed.
   - `$XDG_RUNTIME_DIR` is the user's per-login runtime directory, so a
     leftover file is gone by the next boot at the latest.
   - With `XDG_RUNTIME_DIR` unset, `-agent-spawn` reports `unavailable`
     (detail `no XDG_RUNTIME_DIR for prompt files`).
2. The tmux command holds two kinds of word, and each kind has its own rule:
   - **Host-built words** are checked against `[A-Za-z0-9/._=@:-]` at the
     moment they are built. Those characters cannot form a trailing `;`. They
     are:
     - the paths: the shell, the binary, the prompt file and the worktree;
     - the names: the session `agents` and the window name;
     - the ids: the session id.

     The mirror also checks at startup that `<agent-spawn-dir>` and the prompt
     directory contain only those characters. Otherwise it reports
     `-agent-spawn` as `unavailable` (detail `a spawn path has characters tmux
     would parse`).
   - **Constant words** are fixed in the source and never built from input.
     They are claude's wrapper script, opencode's `exec "$@"` wrapper, the
     words `opencode` and `-s`, the flags (`--session-id`, `--`), and tmux's
     own options (`-d`, `-P`, `-F '#{window_id}'`, `-n`, `-c`, `-t`). The rule
     for them is that **none may end in `;`**. A unit test enumerates every
     constant word the spawn uses and asserts that. claude's wrapper contains
     `;` inside it, but ends in `"$p"`.
3. A **constant** wrapper script reads the file, deletes it, and execs the
   runtime with the prompt as one final argument. The prompt reaches the shell
   only as the value of a variable inside double quotes, so the shell never
   parses it.

The prompt file and its wrapper are **claude's only**. opencode's prompt never
goes through argv at all: it is posted through opencode's API once the window
is bound (below). The claude wrapper is a constant in the source, passed as the
`-c` argument:

```sh
# claude
p=$(cat -- "$1" && printf x) || exit 97; p=${p%x}; rm -f -- "$1"; shift; exec "$@" "$p"
```

(An earlier draft of this page had a second wrapper that appended
`--prompt=$p` for opencode. It is gone, with the reasons under **opencode**
below.)

- The `printf x` sentinel keeps trailing newlines, which `$(…)` would
  otherwise strip.
- A read that fails exits with status 97. The window then ends, and with it
  the dispatch ([3.7](#37-linking-the-spawned-window-to-the-dispatch-and-the-task)).
  The host logs why.
- A prompt file left behind by a crash is removed by the hourly sweep
  ([3.3](#33-worktrees)).

**claude:**

```text
<shell> -l -c <claude wrapper> <shell> <prompt-file> <claude-abs> --session-id <uuid> --
```

- `<claude-abs>` is the `claude` binary, resolved against the tmux server's
  PATH and passed as an absolute path, so a profile that changes PATH cannot
  swap the binary (host-sessions, "The command").
- `--session-id <uuid>` (Claude Code 2.1.289 `--help`: "Use a specific session
  ID for the conversation") is a fresh UUIDv4 that **the host mints**. That is
  what links the window to the dispatch without the transcript guess
  ([3.7](#37-linking-the-spawned-window-to-the-dispatch-and-the-task)).
- The wrapper appends the prompt after `--`, as the positional `[prompt]`
  (`Usage: claude [options] [command] [prompt]`).
- **The subcommand hazard.** Claude's usage puts `[command]` before
  `[prompt]`, so a prompt equal to a subcommand name (`update`, `mcp`,
  `doctor`, …) could be parsed as that subcommand. Two guards close it:
  - `--`. **Measured: it does not close it.** `claude --session-id <uuid> --
    update` runs the `update` subcommand on 2.1.289 ([7](#7-what-is-still-unverified)).
    `--` stays in the argv, but it is not a guard.
  - Structurally, **muster's prompt always begins with its own fixed
    preface**, `muster: your owner's chief started you …`. That is never a
    subcommand name and never starts with `-`, so this guard holds whatever
    `--` does.

**opencode:**

**This revision replaces the earlier opencode design** (a wrapper appending
`--prompt=<prompt>` to `opencode --session <id>`). Since then #486 shipped
`muster-mirror opencode`, the launcher that binds a pane to an opencode session
deterministically, and the API send behind `-opencode-send`. Three of that
draft's UNVERIFIED items are now settled by #486, in source and against a real
`opencode serve` (v2.0.32+6ec5682, `internal/opencodehost/send.go:18-31`):

- a session id the caller chooses is accepted, in upstream's own `ses_` format
  (`opencodehost.NewSessionID`), and `POST /api/session {id, location:
  {directory}}` creates it;
- the send route is `POST /api/session/{id}/prompt {id, text, delivery}`, and
  a retry with the same message id lands at most once;
- a pane started by the launcher is bound to its session through the launcher
  record (`SourceLauncher`), so no title heuristic is involved.

So the opencode spawn reuses all of that and adds only the model.

**How opencode takes a model, at the pinned commit.** muster's opencode pin is
upstream <https://github.com/anomalyco/opencode> at `6ec5682`
([opencode-first-class.md](opencode-first-class.md)). There,
`packages/cli/src/commands/commands.ts` gives:

- **the TUI, bare `opencode`, has no model flag.** Its parameters are the
  server flags (`--standalone`, `--server`), the permission flags (`--auto`),
  `[directory]`, `-c/--continue`, `-s/--session` and `--prompt`, and nothing
  else (`commands.ts:38-58`);
- **`-m, --model` exists only on `opencode run`** ("Model to use in the format
  provider/model#variant", `:398-402`) **and `opencode mini`** ("Model to use
  in the format provider/model", `:366-370`). Neither is the TUI the mirror
  views;
- **the API takes it on the session instead.** `POST /api/session` accepts
  `model: Model.Ref` (`packages/protocol/src/groups/session.ts:142-152`),
  which is `{id, providerID, variant?}`
  (`packages/schema/src/model.ts:19-23`). `Model.Ref.parse` splits a string
  at its **first** `/` (`model.ts:27-36`), so `openrouter/<vendor>/<model>`
  is `{providerID: "openrouter", id: "<vendor>/<model>"}`. `POST
  /api/session/{id}/model` switches it later (`session.ts:267-269`).

So muster passes the model **in the session create**, not as a flag. A flag
would have needed `opencode run`, which is not the TUI and would leave nothing
for the mirror to view or send to.

**The provider id.** opencode's OpenRouter plugin keys on its package
(`@opencode/ai/providers/openrouter`, `packages/core/src/plugin/provider/openrouter.ts`),
not on a fixed id. The id a host's configuration gives it is, by models.dev's
convention, `openrouter`. The host checks rather than assumes: before creating
the session, it reads `GET /api/model` (`packages/protocol/src/groups/model.ts:10-24`)
and requires an entry whose `providerID` is `openrouter` and whose `id` is the
model. Otherwise it refuses `bad_model`. ⚠ That route describes its answer as
a snapshot that "may precede initial plugin settlement". So a list with **no
`openrouter` models at all** is refused as `unavailable` (detail `opencode's
model list is not ready (it lists no OpenRouter model)`), never as `bad_model`,
so the chief does not tell the owner a model is wrong when the list is merely
early. (Measured: the first answer after a restart was already complete, and a
service with no OpenRouter key lists none at all, [7](#7-what-is-still-unverified).)

**The steps** (the host, inside its spawn deadline,
[3.7](#37-linking-the-spawned-window-to-the-dispatch-and-the-task)):

1. the worktree, as for claude ([3.3](#33-worktrees));
2. the model check above;
3. **create the session**: `Client.CreateSession(ctx, id, worktree, model)`,
   with `id = opencodehost.NewSessionID(now)`. `CreateSession` gains its
   `model` argument. Its doc comment names the launcher as its only caller
   (`internal/opencodehost/send.go:88-90`); the spawn becomes the second, and a
   caller ledger pins exactly those two;
4. **open the window**, running the launcher on that session:

   ```text
   <shell> -l -c 'exec "$@"' <shell> <muster-mirror-abs> opencode -s <id>
   ```

   - `<muster-mirror-abs>` is the running mirror's own binary
     (`os.Executable()`, resolved with `EvalSymlinks`), so the launcher and
     the daemon are always the same build.
   - The launcher finds the session already exists as a root session, binds
     the pane to it, writes its record and execs `opencode --session <id>`
     (`cmd/muster-mirror/opencode_launch.go:263-298`). Its version check runs
     too: a TUI whose version differs from the service's is refused there,
     and the window ends. The host turns that into `unavailable` (detail
     `opencode TUI and service versions differ`) by checking the same versions
     itself before step 3, so the refusal comes before any window.
   - The working directory is the worktree, through tmux's `-c`. No
     positional directory is passed: the session's `location.directory`
     already is the worktree.
   - **There is no prompt in this argv**, and no prompt file. Every word is a
     constant or a host-built value under the character rule above;
5. **wait for the bind**: the listing must carry the window with its
   `@muster-dispatch` tag, `AgentSend` set, and `AgentSession` equal to `id`;
6. **post the prompt**: `Client.Prompt(ctx, id, messageID, prompt)`, with
   `delivery: "queue"`. `messageID` is derived from the dispatch id (`msg_`,
   the dispatch row's creation time, and an HMAC of the dispatch id), so a
   retry of the post, by this host or after a mirror restart, lands at most
   once ([opencode-first-class.md](opencode-first-class.md), "Delivery
   confirmation"). An answer that is lost after the request was written is
   retried **once** with the same id: the stored record coming back is the
   confirmation;
7. reply `agent-spawned`.

**Why the prompt goes after the bind, not in the create.** Posting first would
start the agent's run in the shared service before any window exists. If the
window then failed, an agent would be working, and spending OpenRouter credit,
with nothing for the owner to see or close. Posting last means a spawn that
fails at any step before 6 has run nothing: the host deletes the session it
created (`DELETE /api/session/:sessionID`, `session.remove`,
`packages/protocol/src/groups/session.ts:221-227`, through a new
`Client.RemoveSession`, which joins the package's write ledger in
`ledger_test.go`) and removes the worktree.

**A post that fails after the window exists.** The window is kept, with its
empty session, and the spawn is refused `failed`, detail `prompt not
accepted`. The reconciler ends the dispatch on the refusal, and the window is
then an orphan, which raises the owner's notice
([3.7](#37-linking-the-spawned-window-to-the-dispatch-and-the-task)). The host
does not kill it: the owner may already be typing in it.

**The gates on this send.** It is not a browser send, so the step-up check that
gates the Rendered view's composer does not apply. What authorises it is the
same as what authorises the spawn: `ownerTurnFor`, or the owner's tap on a held
dispatch's card ([2.8](#28-mirrored-dispatch-choosing-host-runtime-and-model)),
on the server; and on the host, `-interactive`, `-agent-spawn`,
`-opencode-send`, the token, and the window's own tag. The host's other send
checks ([opencode-first-class.md](opencode-first-class.md), "Host side") hold
trivially for a session created a moment ago: it is launcher-bound, idle, and
has no ask.

**The shared background service.** The launcher attaches the TUI to the host's
running `opencode serve --service`, as every launcher-bound pane does. Whether
that service runs the session's tools in the session's `location.directory`
(the worktree) is **UNVERIFIED**, and phase 4 measures it first. If it does
not, the spawn is not shipped for opencode until it does: `--standalone` would
leave the pane unbound (`opencode_launch.go:245-246`), and an unbound pane can
be neither forwarded to nor sent to.

**Why, for claude, a file read by a constant wrapper, and not an argv word or a paste.**

| delivery | tmux parses it | shell parses it | verdict |
|---|---|---|---|
| **a `0600` file, read by the constant wrapper** | **no** | **no**: it is a quoted variable's value | **recommended** |
| one argv word to tmux | **yes**: a trailing `;` ends the tmux command | no | ✗ |
| an `-e VAR=…` environment value | **yes**: it is a tmux argument too | no | ✗, for the same reason |
| a paste through the send queue, after `idle` | no | no | ✗ for claude, because it races the CLI's startup; impossible for opencode, because the host refuses pastes into it |
| opencode's HTTP API, after the bind | no | no | **opencode's delivery** (above); claude has no such API |

**Cost: other processes can read it.** Once the runtime has started, the
prompt is one of its arguments, readable at `/proc/<pid>/cmdline` by every
process of the same Unix user for the life of the process. Until then it is in
the `0600` file. On these hosts that user is the owner, who can already read
the transcript the prompt lands in. A prompt is not a secret here, so this
cost is accepted and stated rather than engineered around.

#### 3.6.1 Claude Code's folder-trust entry

A fresh worktree is a directory Claude Code has never seen, so `claude` would
stop at "Do you trust the files in this folder?" before reading its prompt.
Decision 7: the host writes the trust entry itself, just before the launch, for
the worktree it has just created and for nothing else.

**The file and the key.**

| | value | confirmed how |
|---|---|---|
| file | `~/.claude.json`, the owner's user-scope Claude Code state file | **confirmed on a host running Claude Code 2.1.289**: the file exists, and holds the key below |
| file, when `CLAUDE_CONFIG_DIR` is set | `$CLAUDE_CONFIG_DIR/.claude.json` | **UNVERIFIED**. Measured in phase 3 by launching with `CLAUDE_CONFIG_DIR` pointing at a scratch directory and checking which file gains the project entry |
| key | `projects["<absolute path>"].hasTrustDialogAccepted = true` | **the key name is confirmed**: on that host, `projects` has 134 entries, and every entry with a trust key uses exactly this one (97 `true`, 37 `false`). **UNVERIFIED:** that writing it **alone** suppresses the dialog, and how the path must be spelled (the real path after symlinks, no trailing slash). Measured first in phase 3 |

Which config directory the mirror uses is decided the same way the resume
path decides it today: muster-mirror's own `$CLAUDE_CONFIG_DIR`, else `~`
(`ClaudeLocator.configDir()`, `internal/mirrorhost/claude.go:239-248`; see
`docs/mirroring.md`, "Resuming an ended session", for why the mirror's env
file must carry the owner's `CLAUDE_CONFIG_DIR` if they set one).

**The guard: `trustSpawnedWorktree(dir)`**, the only function in muster that
writes Claude Code's configuration. It refuses unless every check holds,
re-checked at the moment of the write, not when the plan was made:

1. `dir` is absolute and clean, and `filepath.EvalSymlinks(dir) == dir`.
2. `dir` is **exactly two levels** below `<agent-spawn-dir>`, as
   `<agent-spawn-dir>/<owner>__<repo>/<dispatch>`. The `<dispatch>` part is the
   id of the spawn now in progress. `<agent-spawn-dir>` itself, a repository
   level directory, and anything outside are refused. This includes `~`, any
   root, and any clone.
3. `<agent-spawn-dir>` is not under any `-agent-spawn-roots` root and contains
   none, which is checked at startup. Then a clone can never be a child of it.
4. **This spawn created `dir`.** It did not exist before this spawn's `git
   worktree add`, which the spawn records; and `git -C <clone> worktree list
   --porcelain` lists `dir` now.
5. The runtime is `claude`. opencode has no trust prompt, and nothing is ever
   written for it.

**The write changes one value, and checks that it did.**
- The host reads the file and decodes it into `map[string]json.RawMessage`
  twice: once at the top level, and once for `projects`.
- It sets `projects[dir]` to `{"hasTrustDialogAccepted": true}`. An existing
  entry for a fresh `dir` is a refusal: it cannot occur, so it means something
  is wrong.
- It encodes the result with a `json.Encoder` that has `SetEscapeHTML(false)`
  and `SetIndent("", "  ")`.
- **The result is semantically equal to the original, not byte-identical.**
  - Go writes map keys in sorted order, so key order may change.
  - Go re-compacts each `RawMessage` value, so whitespace inside a value and
    the file's indentation may change.
  - Every value must still decode to the same thing. With HTML escaping off,
    no `<`, `>` or `&` inside a string is rewritten.

  Claude Code reads the file as JSON, so this costs nothing that it can see.
  That claim is checked by the live pair below.
- It writes the result the way Claude Code itself does: to a temporary file in
  the same directory, mode `0600`, then `rename` over the original. Claude
  Code's own writes leave `.claude.json.tmp.<pid>.<hex>` files beside it, which
  is the same pattern.
- Then it **verifies, and never restores**. It re-reads the file and compares
  it with what it wrote, decoded and compared semantically. If they differ, or
  the file does not parse, the spawn is refused as `failed`, with the detail
  `trust entry not written`, and the host writes nothing more. A mismatch most
  likely means Claude Code wrote the file in between. Restoring the old bytes
  would then revert that write, which is worse than a refused spawn. The
  window is not created yet, so there is nothing to kill.
- A file that does not exist, or does not parse when first read, is refused in
  the same way. It is never created or repaired.

**The race this cannot remove, stated.** Claude Code has no lock on this file.
Every running `claude` process rewrites it whole, by temporary file and rename.
So two failures are possible:
- Claude Code rewrites the file from an older copy, and **our entry is lost**.
  The dialog then appears in the window. That is the safe direction: the
  dispatch waits for the owner, and nothing is granted.
- Our read-modify-write overwrites, or is overwritten by, a Claude Code write
  in the few milliseconds between our read and our rename. A setting Claude
  Code wrote in that gap would be lost.

The second risk is the same one any two running `claude` processes already
take with each other. The write is kept as short as possible: read, decode,
set, write, rename, with no other work in between. A read-back mismatch is
refused rather than repaired. The phase 3 test measures how often the entry is
lost under a concurrent writer.

**No removal** (round-0 disposition D5). The entry stays after its worktree
is gone. An entry like that is **inert**: it names a path that no longer
exists, and the path is never reused. Dispatch ids come from a sequence, and
the path holds the id ([3.3](#33-worktrees)), so no later directory can
inherit the trust. A kept worktree keeps its entry, so the owner can resume
the session there.

**Tests** (phase 3; host suite, with `HOME` and `CLAUDE_CONFIG_DIR` set to
scratch directories):
- **Exactly one value changes.** Start from a realistic fixture: a file with
  the real file's *shape*, holding many projects, MCP entries, unknown keys and
  strings containing `<`, `>` and `&`, all with invented values. After the
  write, decode both files: they must be semantically equal everywhere except
  `projects[dir]`. Every `<`, `>` and `&` must come through unescaped.
- **Each refusal** gets its own case and detail:
  - a symlinked `dir`;
  - `dir` equal to `<agent-spawn-dir>`;
  - a repository-level directory;
  - a path one level deeper;
  - a sibling dispatch this spawn did not create;
  - a path under a root;
  - `~`;
  - an opencode spawn;
  - a missing file;
  - an unparseable file;
  - an existing entry for `dir`.

  In each case **the file's bytes are unchanged** (checked with `cmp`), and no
  window is created.
- **A read-back mismatch does not restore.** A test seam swaps in a different
  valid file between the rename and the read-back, the way a concurrent Claude
  Code write would. The spawn is refused with `failed` and the detail
  `trust entry not written`, and **the swapped-in file is left exactly as it
  was** (checked with `cmp`).
- **Mutation checks.** Each mutant must turn its own case red, with that
  refusal's detail or a semantic diff, and not with some other error:
  - drop check 2 (the depth);
  - drop check 4 (created by this spawn);
  - remove `SetEscapeHTML(false)`: the `<>&` case goes red;
  - restore the old bytes on a mismatch: the no-restore case goes red.
  - Keep one mutant in the batch that is known to be caught, as the positive
    control.
- **Live, before shipping** (the UNVERIFIED rows above): launch `claude` in a
  fresh worktree with the entry written, and see it reach the prompt with no
  dialog. Without the entry, the dialog appears. That pair is the control.
  Repeat with `CLAUDE_CONFIG_DIR` set.

### 3.7 Linking the spawned window to the dispatch and the task

**A new table, `host_dispatches`** (migration `00NN_host_dispatches.sql`: the next free number when phase 3 is built; `0073`, which an earlier draft named, has since been taken),
under row-level security on `owner_id`:

| column | type | |
|---|---|---|
| `id` | bigserial | the `dispatch` on the wire, in `@muster-dispatch`, and in the worktree path |
| `owner_id` | bigint | RLS |
| `host` | text | |
| `runtime` | text | `claude` or `opencode` |
| `model` | text, null | opencode's `openrouter/<vendor>/<model>`; null for claude |
| `held_id` | bigint, null | the `chief.dispatch_held` audit row this dispatch completed, when the owner's tap chose a blank ([2.8](#28-mirrored-dispatch-choosing-host-runtime-and-model)) |
| `repo`, `base`, `branch`, `worktree` | text | `branch` and `worktree` come from the reply |
| `task_id` | bigint, null | `REFERENCES notes ON DELETE SET NULL`, as `agents.note_id` does |
| `reservation_id` | bigint, not null | the id of the `task.chief_dispatch` audit row that reserved this dispatch, written by the `onReserved` hook. Ending the dispatch releases the task's start by this id (`releaseHostDispatchTx`, [2.6](#26-budget-and-accounting-for-a-dispatch-with-no-task)) |
| `agent_session` | text, null | the id the host minted |
| `tmux_window` | text, null | `@N` |
| `status` | text | `pending`, `running` or `ended` (below) |
| `missing_listings` | smallint, default 0 | consecutive listings from the connected host that lacked the window |
| `host_seen_at` | timestamptz | the last time a listing from this row's host was received; set to `now()` at insert |
| `refusal` | text, null | the code and detail, when a spawn was refused |
| `created_at`, `ended_at` | timestamptz | |

**Three statuses.** The chief needs to know two things about a host dispatch:
whether it holds a live slot, and whether `task_forward` can reach it.
`status` is the **only** thing that decides both
([2.6](#26-budget-and-accounting-for-a-dispatch-with-no-task)).

| status | meaning | live slot | `task_forward` |
|---|---|---|---|
| `pending` | asked for, and not yet seen in a listing: the spawn is in flight (the host gives up at 120 s), its reply timed out unconfirmed (after 150 s), or the host is offline. Once more than 180 s old, it is counted down by missing listings | yes | no |
| `running` | its window is in the host's listing, carrying its `dispatch` | yes | yes ([3.10](#310-forwarding-into-a-host-spawned-agent)) |
| `ended` | refused (`refusal` says why), its window gone, or its host not seen for 24 hours | no | no |

**Why a table, when host-sessions slice 1 had none.** That slice had no
server-side caps and no tasks. Here the live cap needs a count that survives a
muster restart, and the task needs something to own its link. The ledger rows
alone cannot say which window is still alive.

**Who writes the row.** There is one seam ledger, with three writers:
- **The `pending` insert**, through the `onReserved` hook, inside the
  reservation ([2.6](#26-budget-and-accounting-for-a-dispatch-with-no-task)).
- **`UpdateHostDispatch`**, which writes the window and session fields,
  `running`, `missing_listings` and `host_seen_at`, **only `WHERE status <>
  'ended'`**, so a late reply or listing can never revive an ended row. **Its only caller is the
  host-dispatch reconciler** (`internal/api/host_dispatch_reconcile.go`, new).
- **`EndHostDispatch`**, which is **the only writer of `ended`** and the only
  caller of `releaseHostDispatchTx`. It is new, so it takes the owner's
  advisory lock itself (`pg_advisory_xact_lock(hashtextextended(
  'muster.chief_dispatch:' || owner, 0))`, the same key as
  `chief_dispatch.go:221`) at the start of its transaction, before it reads
  the row. Its release is therefore one decision with any reservation of that
  owner's. **Its only callers are the reconciler and
  `agent_destroy`** (the owner's recovery, below).

A fourth, narrow writer exists only for the orphan notice:
`SetHostDispatchOrphanNotice` writes only `orphan_notice_id`, and only on an
`ended` row. Its only caller is the reconciler (below).

A seam test pins all four. It is an asserted ledger over the Go source of
`internal/api` and `internal/plugins`: the SQL that writes `host_dispatches`
appears only in the hook's insert and those three store functions, and each
has exactly the callers named here. The test fails if either set grows or
shrinks.

**The reconciler's inputs.**
- **The spawn reply, from either of two paths.**
  - **The waiter.** `Hub.SpawnAgent` waits up to 150 s for the reply on its
    request id, as `SpawnResume` does.
  - **The late path.** It does **not** go through the waiter. Every
    `agent-spawned` frame carries its `dispatch` id
    ([3.1](#31-wire-messages-and-capability)). The hub's read loop hands every
    such frame to a second new hook,
    `Hub.SetSpawnReplyNotify(func(owner int64, host string, m Msg))`, whether
    or not a waiter is still registered for its request id. Once a waiter
    times out it is gone, and the hub drops the frame for it, as `pasteTo`'s
    bookkeeping drops a late paste reply. The hook still delivers the frame.
  - **How a reply is matched**, by either path. The reconciler acts as the
    **hook's `owner`**, the owner of the connection the frame arrived on, and
    never as an owner named inside the frame. It loads the row by `dispatch`
    under that principal, so row-level security hides any other owner's row.
    It then applies the reply only if `row.owner_id == owner` **and**
    `row.host == host`, the connection's host. Otherwise it drops the reply
    and logs it. A host can therefore settle only its own owner's dispatches,
    and only those addressed to it.
  - A reply counts as one event however many times it arrives. A reply for a
    row that is already `ended` changes nothing: that case is the orphan
    case, below.
- **Each listing.** The hub calls a new hook,
  `Hub.SetListingNotify(func(owner int64, host string, windows []Window))`.
  It works like `SetWaitingNotify` (`internal/mirror/hub.go:278-282`): it is
  called on its own goroutine after the hub has applied the listing.
- **A timer**, for the sweep.

**Its principal.** Each write runs as **the row's owner**, which is the hook's
`owner`.
- It builds that principal the way the stopped-agent reaper does
  (`reapPrincipal`, `internal/api/agent_reap.go:86`), with a new
  `authz.ViaHostDispatch`. The reaper uses `authz.ViaReap`; a separate `Via`
  keeps the two apart in the logs.
- The new value joins the `Via` ledger (`internal/authz/ledger_test.go:271`).
- Row-level security, and the user-principal checks in the store, therefore
  hold exactly as they do for the owner's own calls.

**Three deadlines, in order.** Each one is shorter than the next. The ordering
needs only one thing: the host's deadline must sit below the server's wait
minus the time a frame takes to cross the WebSocket. The gaps here are 30 s,
which is generous for that. Ordering alone cannot cover a frame delayed past
the gaps, or a host whose clock jumps. **The orphan path below is the backstop
for those**: it surfaces such a window instead of leaving it untracked.

| deadline | value | who enforces it | what happens past it |
|---|---|---|---|
| the host's spawn deadline | **120 s**, from the moment the host **receives** `agent-spawn`, so the time spent queued behind the host's spawn mutex counts | host | the host refuses (`failed`, detail `deadline passed`) and **never starts the agent** |
| the server's reply wait | 150 s | `Hub.SpawnAgent` | the row stays `pending`; a later reply still lands, through the late path |
| the pending countdown | 180 s (the 150 s wait plus a 30 s margin), then two missing listings | reconciler | the row ends, and the task's start is released |

**The values are a starting point, not a measurement.** No fetch, worktree or
submodule time has been measured on the owner's repositories, and a large
repository or one with many submodules can be slow. So the values are set
well clear of that: 120 s on the host, a 150 s wait, and 180 s before the
countdown, instead of something tighter. **Phase 3's first step measures**
fetch, worktree and submodule time on the owner's real repositories, cold and
warm. If the slowest exceeds about half the host deadline, all three values
are raised together, keeping the same 30 s gaps, before the phase ships
([6](#6-phases)).

**How the host enforces its deadline: the order of the steps.** The order
matters because a spawn must never be refused by a step that ran out of time
only because an earlier step used it up:
1. **Fetch**, then **worktree add**. Each runs under the remaining time as a
   context deadline. If either fails, the spawn is refused (`failed`), and
   there is nothing to undo except the worktree.
2. **Submodules**, only when `.gitmodules` exists. This step runs under its
   **own budget**: the remaining time minus a **15 s reserve** for steps 3 to
   5. When the budget runs out, the step is killed and recorded as a
   **warning** (`submodules incomplete`) in the reply. **The spawn is not
   refused.** The agent starts with whatever was checked out, and the warning
   tells the chief and the owner. So a submodule step that the deadline cut
   short never becomes a refusal, and the reserve leaves the time the later
   steps need.
3. **Trust write** (claude only), then **the prompt file**.
4. **The deadline check, immediately before `tmux new-window`.** This is the
   point of no return. Past the deadline the spawn is refused here, and the
   host removes the worktree and the prompt file it created.
5. **`new-window`**, then the bounded local tmux calls: the tag, the landing
   check, and the listing.

So a spawn queued behind a 130 s fetch is refused at the latest at its
deadline check, and never runs.

**The reconciler's writes:**
1. **Reserve.** `toolAgentDispatch` inserts the row as `pending` through the
   `onReserved` hook, recording `reservation_id`. Then the server sends
   `agent-spawn`.
2. **Refused**, by either reply path: `EndHostDispatch(id, refusal)`. The
   task's start is released by `reservation_id`, in the same transaction.
3. **Spawned**, by either reply path: `UpdateHostDispatch` records the window,
   the session id, the branch and the worktree, and writes the audit-only
   `task.chief_dispatch_agent` row, `{host_dispatch_id}`.
4. **A listing from the row's host.** The reconciler sets
   `host_seen_at = now()`. Then:
   - if the listing carries a window whose `dispatch` is the row's id, and
     whose runtime matches the row's: `running`, and `missing_listings = 0`;
   - if it does not, and the row is `running`: `missing_listings + 1`;
   - if it does not, and the row is `pending` and **more than 180 s old**:
     `missing_listings + 1` as well. A younger `pending` row is not counted,
     because its window may not exist yet;
   - **at 2, `EndHostDispatch(id, "window gone")`.** It takes two listings,
     so a single transient miss cannot free a slot.

   So a spawn that produced no window frees its slot and its task's start
   about 180 s after it was asked for, plus two listing intervals. That covers
   a fetch that ran long and failed, a mirror that crashed or restarted
   mid-spawn, and a reply that never came. An offline host sends no listings,
   so its rows are left to step 5.
5. **The sweep**, every 10 minutes. It ends every `pending` or `running` row
   whose `host_seen_at` is more than **24 hours** old, with
   `EndHostDispatch(id, "host not seen")`.
   - It reads only the table, so it gives the same answer after a muster
     restart. It never relies on the hub's in-memory view of offline hosts
     (`offlineRetention`, `internal/mirror/hub.go:59`).
   - It finds the owners with open rows the same way the reaper finds owners
     (`agent_reap.go:92`), then acts as each one.
6. **Release** happens only inside `EndHostDispatch`, by `reservation_id`
   ([2.6](#26-budget-and-accounting-for-a-dispatch-with-no-task)). That covers
   every way a dispatch ends:
   - the window closed;
   - the agent exited;
   - the spawn was refused, or produced no window;
   - the host was never seen again;
   - the owner ended it.

**A window whose dispatch has already ended is never left running silently.**
This is the backstop the deadlines rely on. The ordered deadlines make it
rare, but they cannot rule it out: a frame can be delayed past their gaps, a
host clock can jump, tmux can stall, or a bug can slip through. When a listing
carries a window whose `@muster-dispatch` tag names a row of this owner's that
is `ended`:
- **The reconciler does not adopt it.** Making it `running` again would put
  an agent back that the budget no longer counts.
- **It raises an attention notice for the owner.** It is an attention row of
  `KindNotice`, because 0071's CHECK allows only `ask` and `notice`
  (`internal/attention/attention.go:142-157`), and a notice is what muster
  raises on its own, with no answer. Each dispatch gets **at most one**:
  migration `00NN` gives `host_dispatches` a column `orphan_notice_id bigint
  null`, and the notice is raised only while that column is empty. Raising it
  writes the notice's id into the column, in the same transaction. The notice
  names the host, the window and the dispatch: *"an agent window on `<host>` (`agents:<window>`,
  dispatch N) is still running, but muster ended its dispatch (`<why>`): it is
  not counted or tracked. Close the window on that host."* It reaches the
  owner's phone the way every attention item does.
- **It logs at error level**, so an operator who watches the server log sees
  it too.
- **The notice is withdrawn** once a listing no longer carries that window. A
  notice is "never answered, only dismissed, withdrawn or expired"
  (`attention.go:150-153`). The column keeps the id, so the same dispatch
  never raises a second notice.
- That column is written by its own store method,
  `SetHostDispatchOrphanNotice`, which writes only `orphan_notice_id` and
  only on an `ended` row. Its only caller is the reconciler. It is part of the
  same seam ledger.
- A tag naming a dispatch id for which this owner has **no row at all** is
  logged at error level, but raises no notice. There is no row to deduplicate
  on, and such a tag is foreign or forged, not something muster lost track of.

**The owner's recovery: `agent_destroy` takes a host dispatch.** The tool
already exists, and only the owner's own mirrored sessions can call it
(`agent_destroy_tool.go:57`; the chief cannot). It gains a third way to name
its target, `host_dispatch` (an id), beside `name` and `id`.
- On a `pending` or `running` row of the owner's, it calls
  `EndHostDispatch(id, "ended by owner")`, which frees the slot and releases
  the task's start at once.
- It does **not** close the tmux window (D7). The answer says so: `"ended host
  dispatch N; its window, if any, is still open on <host>: close it there"`.
- **A host-spawned agent is refused by it**, and by every other owner tool,
  when it calls the tool as itself ([5.1](#51-who-can-trigger-a-spawn),
  "Owner tools refuse spawned agents"). That is a mitigation, not a boundary:
  see 5.1.

**What the owner sees in muster.** The window is an ordinary mirrored window,
with everything that already ships: the rendered view, the waiting state,
Claude's asks, and opencode's permission and question asks (#416). In
addition:
- the window and its sidebar row show a chip, **"chief · #<task>"** (or
  **"chief"** with no task), read from the row through `dispatch`;
- the task page lists it among the agents working the task, through
  `liveOnTask`;
- the chief's `live_agents` names it as `{host, window, task}`;
- **the live dispatched-agent card (PR A)** shows it in the chief's chat, as
  for a pod ([below](#the-live-agent-card)).

#### The live agent card

PR A adds a live card for each agent the chief dispatched. A host dispatch
gets one too, keyed by the **`host_dispatches` row id**, not by an agent row,
because a host dispatch has none until the window exists, and possibly never.
The card reads the row, and the hub's listing for the row's window:

| row state | the card shows |
|---|---|
| `pending`, no reply yet | "Starting on `<host>`…", the runtime, and for opencode the model |
| `pending`, reply timed out | "Started on `<host>`? muster has not seen the window yet" |
| `running` | the window `agents:<window>` on `<host>`, the runtime and model, the agent's status from the listing (`busy`, `idle`, `waiting`), and a link to the window page |
| `ended`, refused | the refusal's sentence ([3.1](#31-wire-messages-and-capability)) |
| `ended`, otherwise | "Ended (`<why>`)"; for a kept worktree, its path and branch |

- **How the card finds the window.** Through the row's `host`, `tmux_window`
  and `agent_session`, exactly as forwarding does
  ([3.10](#310-forwarding-into-a-host-spawned-agent)), so the card and the
  forward cannot disagree about which window is the agent.
- **The session key.** Once the reply lands, the row holds `agent_session`,
  which the host minted. The window's session key
  (`Window.SessionKey`, `internal/mirror/wire.go:681-690`) follows from it:
  the UUID itself for claude, and `opencode:<ses_…>` for opencode. Anything
  muster keys by session (a focus, a route, an ask) therefore links to the
  card's row from the moment the reply lands, without a transcript guess.
- **The card has no Stop button** for a host dispatch (D7, below). It shows
  "close the window on `<host>` to stop it" instead (decision 17).
- The card updates on the same events that redraw the sidebar's window rows,
  so it adds no polling.

**Ending it.** There is **no Kill button and no kill message** (D7). The owner
closes the tmux window, from a terminal or through Live keys, and the listing
ends the dispatch. When a dispatch on a task ends, the task's one start is
released (`ReleaseChiefDispatch`), as the owner destroying a pod does today.
This holds whether the owner closed the window or the agent exited, because
there is no other owner action to wait for. A row stuck for any other reason is
ended by the owner's `agent_destroy` with `host_dispatch` (above).

**When the task becomes complete.** Today a pod is *stopped* when its task
becomes complete (`stopChiefAgentOnComplete`). **A host window is not closed
automatically.** It is a process on the owner's workstation, and the owner may
be typing into it. Its slot is freed only when the window ends.

### 3.8 Read-only and tenant hosts

**The host refuses** in all of these cases, whatever muster sends. Each is
`unavailable`, with its own detail:
- no `-interactive`: detail `not interactive`;
- no `-agent-spawn`: detail `agent spawn is off`;
- **a mirror on any tmux server but the default one**: detail `not the
  default tmux server`.

**What "the default tmux server" means, exactly.** The rule is: **refuse
whenever `-tmux-socket` (`-L`) or `-tmux-socket-path` (`-S`) is set, unless it
resolves to the default socket.**
- **The default socket** is `realpath(${TMUX_TMPDIR:-/tmp})/tmux-<uid>/default`,
  computed from the environment `tmuxEnv` passes to tmux
  (`internal/mirrorhost/tmux.go:87-98`). `tmuxEnv` keeps `TMUX_TMPDIR`
  because it decides where the default socket lives (`tmux.go:87`), so a
  default server under a custom `TMUX_TMPDIR` is still the default server.
- **No flag set:** the default server, accepted.
- **`-L default`,** or an `-S` path whose `realpath` equals the default
  socket: the default server, accepted.
- **Anything else:** refused. That covers `-L work`, and any `-S` path that
  is not the default socket even when its basename is `default`, such as a
  `default` socket in another directory.
- **Where this comes from.** It is multi-tenant.md's own classification: the
  default mirror runs on the default tmux server, and each tenant's mirror
  runs on its own named server, `muster-mirror@<tenant>` → `tmux -L <tenant>`
  (multi-tenant.md:439-461). Any server other than the default is therefore,
  by that design, a tenant's.
- The check runs at startup, and again at each spawn.

**Why the last rule is structural.** In multi-tenant.md, a non-default tmux
server is exactly what marks a tenant's mirror
(`muster-mirror@<tenant>` → `tmux -L <tenant>`). So even if a tenant unit's
environment set `-agent-spawn` by mistake, that mirror would still refuse.
Decision 2 says "the host's DEFAULT tmux server", so this costs nothing the
owner asked for, and the owner confirmed it (decision 6): their default mirror
runs on each host's default tmux server.

**The server cannot cross tenants.** The hub keys hosts by `(owner, host)`.
- A mirror connected to a different muster server (another tenant) is not in
  this server's hub at all.
- A host owned by a different user on this server is under that user's owner
  id, and the chief's no-target refusal never lists it.

A spawn can therefore reach only a host whose mirror token belongs to the
chief's owner (`internal/mirror/hub.go:653`).

**Listed and refused** use the same predicate, `hostSpawnable(view)`. The
no-target refusal's list and the check at the call both use it, so the list
and the refusal cannot disagree.

### 3.9 Host-side limits

**Revised.** Round-0 disposition D3 kept no host spawn limit. This revision
adds one, `-agent-spawn-rate` (3 per 10 minutes, 10 per 24 hours), together
with the repository allow-list and the audit log, because they are the only
limits that hold if the server itself misbehaves
([3.2](#32-the-flag), [5.5](#55-a-compromised-server)). The host also keeps its
limit on forwards ([3.10](#310-forwarding-into-a-host-spawned-agent)).

### 3.10 Forwarding into a host-spawned agent

Decision 9. `task_forward` today reaches the newest running **pod** agent on a
task (`toolTaskForward`, `internal/api/task_routing.go:532-564`, through
`forwardTarget`, which `task_route_attention`'s offer shares). It now reaches a
host-spawned window on the task as well.

**Addressing: by the dispatch record, never by a window name.**
- `forwardTarget` is built on `liveOnTask` ([2.6](#26-budget-and-accounting-for-a-dispatch-with-no-task)).
  Its candidates are today's pod agents plus the `host_dispatches` rows for the
  task with status `running`, and it keeps today's rule: the newest one that
  can be reached.
- A host row can be reached only when all of these hold **in the hub's state
  at the call**:
  - its host is connected, v2, `agentSpawn && interactive`;
  - its latest listing has a window whose `dispatch` equals the row's id and
    whose id equals the row's `tmux_window`;
  - that window's runtime is the row's runtime;
  - its session id (`claudeSession` or `agentSession`) equals the row's
    `agent_session`.

  If any of these fails, the row is not a candidate. The tool then answers
  today's "no running agent works task N" sentence, with `task_dispatch`
  reworded to `agent_dispatch`.
- The paste names the dispatch, through a new paste option,
  `mirror.ForDispatch(dispatch)` (below). That option only stamps a
  `dispatch` field onto the paste message, and
  **the host refuses a paste carrying `dispatch` unless that window's own
  `@muster-dispatch` (read with `show-options -w`, without `-A`) equals it**
  (`not_muster_window`). So a forward can never land in a window the owner
  opened, even if the hub's state is stale or wrong. **This host-side tag check
  is the real security bound on forwarding.** Everything on the server decides
  only whether and when to ask.
- A dispatch with no task has no task events, so `task_forward` never reaches
  it. That is by construction, not a separate rule.

**What is sent.** The same text a pod gets: `renderTaskForward`
(`task_routing.go:435-440`). That is muster's line naming the task and telling
the agent to re-read it, then the chief's note, fenced as data and bounded by
`taskRouteNoteMax` (2000 bytes, `:71`). The chief writes no free text into the
window beyond that fenced note.

**claude: a chief-forward row in the send queue.** This reuses the send
queue's existing pattern for messages the chief causes, rather than adding a
new paste path.
- **The precedent.** The send queue already delivers rows that no browser
  sign-in authorises: chief nudges and relayed answers (`it.ChiefNudgeID`),
  and routed messages (`it.RouteID`). Each kind passes its own gate. Then
  `internal/api` sets the auth time to the moment of delivery, with
  `ctx = stepup.WithAuthTime(ctx, s.stepUpNow())`
  (`internal/api/send_queue.go:884,892,900`), and the row is pasted through
  `PasteWindow` like any other queued send.
- **The new kind.** A forward adds a send-queue row with a new field,
  `ChiefForwardDispatch int64` (column `chief_forward_dispatch`, in the
  migration with `host_dispatches`). The row targets the dispatch's
  `(host, session, window)`, and its body is `renderTaskForward`'s text.
  - Its gate, `sendQueueChiefForwardGate`, runs at delivery. It re-reads the
    `host_dispatches` row as the owner and passes only while:
    - the row is `running`;
    - its window and session id still match the hub's listing;
    - its task is still the forwarded task.
  - When the gate passes, the branch sets `WithAuthTime(ctx, s.stepUpNow())`,
    as the nudge and route branches do.
  - The paste then goes through `PasteWindow`, carrying `ExpectTurn` (as every
    dispatcher send does, `send_queue.go:849-851`) and
    `mirror.ForDispatch(dispatch)`.
- **What stays unchanged.**
  - `PasteWindow`, including its step-up check (`transcript.go:435`), which
    sees the delivery-time auth like every other queued row.
  - `SanitizePaste` and the Claude prompt guard's server half.
  - `TestEveryServerPasteGoesThroughPasteWindow`
    (`internal/mirror/paste_ledger_test.go`).
  - There is no `ChiefForward` option and no context marker.
- **Idle only, without new code.** The dispatcher sends a row only once its
  window has been idle for `IdleFor`, and it treats an unknown state as not
  idle (`internal/sendqueue/dispatcher.go:17-71`). So a `waiting` window (its
  permission prompt is on screen), a `busy` one, or one that has not reported
  a status yet is never pasted into.
  - `ExpectTurn` refuses a paste whose turn moved after the decision, and the
    row goes back to the queue.
  - **A chief-forward row has its own age limit of 10 minutes**, far shorter
    than the queue's general `AuthorizedFor`. `sendQueueChiefForwardGate`
    checks `now() - CreatedAt` first. Past 10 minutes the row ends failed,
    with the detail `"Not sent: this forward is more than 10 minutes old, and
    the task may have changed since; the chief can forward it again."` A
    forward describes the task's state when it was written, so an old one
    would be stale.
  - `task_forward` answers `"queued for the agent on task N; muster delivers
    it once that agent is idle"`.
- **What bounds it.** The gate decides when to ask. The **host's tag check is
  the bound**: the paste carries `dispatch`, and the host refuses it unless
  the window's own `@muster-dispatch` equals it. A ledger also bounds who can
  create such a row. It scans the Go source of `internal/api` for every
  `sendqueue.Item` composite literal or assignment that sets
  `ChiefForwardDispatch`, and asserts that `toolTaskForward` is the only one.
  It fails if that set grows or shrinks.

**opencode: the same queue row, delivered through #486's API send** (phase 4).
An earlier draft of this page proposed a separate wire pair,
`opencode-send`/`opencode-sent`, and left the send route UNVERIFIED. #486
removed the need for both: a paste whose `Msg.Runtime` is `opencode` already
goes to the host's gated API send instead of the keyboard
(`gatedSend`, [opencode-first-class.md](opencode-first-class.md), "Host
side"), as `POST /api/session/{id}/prompt` with `delivery: "queue"`. So a
forward to an opencode window is **the same chief-forward send-queue row** as
for claude, delivered through `PasteWindow` with `mirror.ForDispatch`, and:
- `lookupAgent` admits the window because it is `AgentSend` (launcher-bound)
  on a host that advertised `openCodeSend`; a spawned window always is
  ([3.6](#36-the-commands-and-prompt-delivery));
- the host runs its tag check (`@muster-dispatch` equals `dispatch`) **before**
  `gatedSend`, exactly as before `typeInto` for claude. One check, placed
  ahead of both delivery paths in `hostSide.paste`;
- `gatedSend`'s own checks then apply unchanged: the window is bound to the
  session muster named, it is idle with the status time muster decided on, and
  no permission or question ask is pending under it;
- the message id comes from the queue row's nonce, as for every queued
  opencode send, so a retried delivery lands at most once.

There is no new wire message and no second implementation. The host's paste
guard, which refuses keystrokes into opencode panes, is untouched.

**Gating, all together.**

| check | where |
|---|---|
| the caller is the owner's chief, task routing is on | today's `dispatchTaskRoutingTool` gates (`task_routing.go:271-283`) |
| the task is the owner's | `ownersTask` (`:308`) |
| the target is a chief-dispatched window **on this task** | `forwardTarget` over `host_dispatches` |
| the window is still that dispatch's | hub state, then the host's own tag check |
| it is a send-queue row, delivered through `PasteWindow` with `ExpectTurn`, which keeps `SanitizePaste`, `gatedSendSwitch` and the existing paste seam ledger | `sendQueueChiefForwardGate`, then `WithAuthTime(ctx, s.stepUpNow())`, as the nudge and route rows do (`send_queue.go:884,892,900`); only `toolTaskForward` creates such a row |
| **the bound:** the window's own `@muster-dispatch` equals the paste's `dispatch` | host |
| claude: idle for `IdleFor` (the dispatcher's rule); opencode: no pending ask, not active | send-queue dispatcher / host (opencode) |
| the text is muster's fenced forward, at most 2000 bytes of note | `renderTaskForward` |
| at most **12 forwards per window per hour** | host, in memory, like the resume limits (`internal/mirrorhost/resume.go:61-64`). This is a guard against paste loops: nothing else bounds how often a chief can type into a window. |

**No `owner_asked` is needed for a forward**, as today for pods: task events
are what `task_forward` exists to relay, and they arrive from plugins rather
than from the owner. The bound is the target: a forward can only reach an
agent the chief itself started on that task, which already holds the task's
text, and which runs in normal permission mode, so any tool use it is talked
into still asks the owner.

The answer is today's, with `forwarded_to` becoming `{host, window,
dispatch}` for a host target.

### 3.11 Failure and cleanup, at a glance

Every case below is designed in the sections it links to. This table is the
index.

| what goes wrong | what the chief hears | slot and task start | what is left on the host |
|---|---|---|---|
| the host is offline, or lacks `-agent-spawn` | refused at the call, with the target list; **nothing is queued** ([2.4](#24-discovery-the-refusal-is-the-list)) | nothing spent | nothing |
| no host can spawn, on a held `host/?/…` | `"no host can start agents now …"`; **no card** ([2.8](#28-mirrored-dispatch-choosing-host-runtime-and-model)) | nothing spent | nothing |
| the owner never answers the card | nothing; a later call with the key answers "holds no dispatch … more than 15 minutes old" | nothing spent | nothing |
| the host refuses before the window (`unavailable`, `no_repo`, `bad_base`, `bad_model`, or `failed` before `new-window`) | the code's sentence ([3.1](#31-wire-messages-and-capability)) | released with the refusal; the `request_key` is freed | the worktree and the opencode session it created are removed |
| the window opens, then a later step fails (tag, landing check, trust write) | `failed` | released | the window is killed, the worktree removed |
| opencode: the first prompt is not accepted | `failed`, `prompt not accepted` | released | the window **stays** (the owner may be typing); muster raises the orphan notice ([3.7](#37-linking-the-spawned-window-to-the-dispatch-and-the-task)) |
| the reply never arrives | nothing more; the row stays `pending` | held until 180 s plus two listings without the window, then released ([3.7](#37-linking-the-spawned-window-to-the-dispatch-and-the-task)) | whatever was made; a window that does exist raises the orphan notice |
| the window never reports (exists, but never appears in a listing) | the same as above | the same | the window, flagged by the orphan notice once its tag shows up; the hourly sweep keeps its worktree if it holds work |
| the host goes offline mid-run | `live_agents` still lists it | held (fail-closed) until the 24-hour sweep | the agent keeps running on the host |
| the agent exits, or the owner closes the window | the card shows "Ended" | released after two listings | a clean worktree is removed; one with work is kept ([3.3](#33-worktrees)) |
| the owner wants it stopped from muster | — | `agent_destroy` with `host_dispatch` ends the record and frees the slot at once | **the window keeps running**: no kill message (D7, decision 17) |

### Relation to host-sessions.md

| host-sessions.md (proposed, unbuilt) | this page |
|---|---|
| trigger: a browser form, behind step-up | the chief's tool, in the owner's typed turn ([5.1](#51-who-can-trigger-a-spawn)) |
| a new session per spawn, from `sessionPool`/`m-<hex>` | one `agents` session, a window per spawn (decision 2) |
| directories: children of roots, chosen by key | a fresh worktree from `origin/<base>` (decision 2); host-sessions had it as its optional phase 5 |
| `spawn.json` profiles with a fixed argv, and the bypass table | runtimes fixed in code; no argv from any configuration file, so there is nothing to bypass |
| no initial prompt in slice 1 | the prompt is the point; it goes in through a `0600` file read by a constant wrapper |
| opencode terminal-only | opencode has a rendered view and asks now (#416, `docs/design/opencode.md`) |
| no table | `host_dispatches`, for the caps and the task link |
| reused | the tmux-server pid check, the separate tag call and its rollback, the landing check, listing before reply, the absolute `argv[0]`, the `exec "$@"` wrapper, no environment from muster |

If this page is accepted, host-sessions.md gets a status line saying that its
UI trigger is superseded, and that its mechanics live on here.

---

## 4. Replacing the dispatch UI: a machine route, a CLI verb, then the deletion

**What goes, with nothing in its place** (D8). These are the items in
[1.5](#15-every-consumer-of-what-gets-deleted)'s "deleted" list:
- the sidebar **+**, and the "not set up" hint drawn when there is no **+**
  (`dispatchOffHint`, `DispatchOffHintAttr`);
- the home page's "Dispatch an agent";
- the task card's **Dispatch**;
- both forms, and "Save for later";
- `GET /ui/agents/new`, `GET /ui/agents/notes` and `POST /agents`, together
  with their handlers;
- `Features.Dispatch`, because nothing reads it any more.

Their entries come out of `routes.golden` and `routes.partition.tsv`. They go
into `removed_surfaces_test.go`, which then pins that each route answers 404.

**Phase 2 deletes the UI only. A machine route replaces the form's
function.** Once the form is gone, the chief would otherwise be the only way to
create an agent. For several kinds of deployment that is no way at all. muster
is a public project, so those deployments matter:
- a user who is not in `MUSTER_CHIEF_DISPATCH_OWNERS`;
- a deployment that runs without task routing (`MUSTER_TASK_ROUTING` off), so
  it has no `agent_dispatch`;
- a chief that runs as a gateway. `ownerTurnFor` refuses it
  (`chiefReleaseNotOverMCP`), so it can make no task-less dispatch, no host
  dispatch and no pool worker ([2.5](#25-gating-when-there-is-no-task-to-hang-it-on));
- a deployment where `KickoffUndeliverableReason` is set. It refuses every
  `agent_dispatch` ([2.2](#worker-pool-agents)), yet "create stopped, then
  Start" still works there.

So phase 2 **first** adds an authenticated machine route and a CLI verb (there
is neither today: the CLI has `muster agent messages|task|ls|resolve`, and the
API has only `GET /api/agents` and `POST /api/agents/{name}/messages`), and
**then** deletes the UI:
- **`POST /api/agents`**, on the machine tier, behind `requireArmedHookToken`
  (`internal/api/agents.go:148`, `internal/api/auth.go:588`). Who may call it:
  - **Only a personal `mst_` token with a new scope, `agent-create`.** Such a
    token acts as its owner (`authz.ViaToken`,
    `internal/api/api_tokens.go:25-31`). The scope is added to
    `tokenScopeRoutes` (`api_tokens.go:157`) with exactly this one pattern.
  - **A full-scope token does not reach it.** Today `tokenScopeAdmits` lets
    `ScopeFull` reach every route (`api_tokens.go:165`). It gains one
    exception: a pattern listed in a new `explicitScopeRoutes` set (this route
    only) is admitted **only** to the scope that names it. So the ordinary CLI
    token, which is a full token, cannot create agents. The owner has to mint
    an `agent-create` token deliberately, at `/account/tokens`.
  - Migration 0068's `api_tokens_scope_check` is widened to allow the new
    value. An image from before this change treats an unknown scope as
    reaching nothing (`api_tokens.go:161-165`), which is the safe direction if
    it is rolled back.
  - **The refusal sentences.** `ScopedTokenRefusal` (`api_tokens.go:146-147`)
    is written for the summary scope: it names a "bar-summary token" and tells
    the user to mint a full token. That would be wrong both ways here, so the
    one sentence becomes a function of the token's scope and the route:
    - **any token whose scope is not `agent-create`**, full or summary, on
      `POST /api/agents`: `"creating agents needs a token of scope
      agent-create: mint one at /account/tokens (Create agents). No other
      token reaches this route, so that the CLI's ordinary token cannot create
      agents"`. A summary token here gets this sentence, **not** today's
      "mint a full-access token", which would be wrong: a full token is
      refused here too;
    - an **agent-create** token on any other route: `"an agent-create token
      reaches only POST /api/agents; use a full-access token for anything
      else"`;
    - a **summary** token on any other route: today's sentence, unchanged.
  - **The other scope changes:**
    - `apitokens.ScopeAgentCreate = "agent-create"`;
    - `CleanScope` (`internal/apitokens/apitokens.go:254-257`) accepts the
      new scope;
    - the mint form at `/account/tokens` (`api_tokens.go:359`) gains a third
      radio, **Create agents**, with the line "reaches only POST /api/agents,
      and nothing else";
    - **the token list's scope label.** Today any scope that is not full is
      labelled bar-summary (`if t.Scope != apitokens.ScopeFull { scope =
      scopeLabelSummary }`, `api_tokens.go:374-377`). That becomes a switch on
      the scope, with a new `scopeLabelAgentCreate = "create agents"`, so an
      `agent-create` token is never shown as a bar token.
  - **The handler refuses a `ViaHook` principal** (`authz.ViaHook`,
    `internal/authz/authz.go:56-58`): the shared hook secret, which acts as
    user 1 whoever holds it. It also refuses every `Via` other than
    `ViaToken`.
- It takes the form's fields as JSON: `kind`, `repo`, `repo_branch`, `model`,
  `prompt`, `note_id`, `cc_account`, `cc_failover`, `worker_pool`, and
  `start` (`true` to dispatch, `false` for what "Save for later" did).
- It calls `createAndDispatchAgent` with the form's checks, unchanged: kind and
  account, worker pool, `connectedRepo`, the model credential, the gate tag on
  `note_id`, and `KickoffUndeliverableReason` when `start` is true. The form
  had no caps and no budget, and neither does this route. It is the owner
  acting with their own credential, not the chief.
- **`muster agent create`** in the CLI wraps it, with flags named after the
  fields. It is a new verb, so the verb ledger (`make verb-ledger`) gains one
  entry. It reads its token from `MUSTER_AGENT_CREATE_TOKEN`, never from the
  CLI's ordinary token, so the full token in the CLI's env file never reaches
  this route.
- `KickoffRefusalSummary` then points at `muster agent create --start=false`
  followed by Start ([below](#text-that-must-change-in-the-same-pr)).

**What stays.**
- The agent list in the sidebar, and every agent page, with Start, Stop,
  model, sessions and Delete.
- `POST /agents/{id}/start`, which still starts any `stopped` row.
- The repository and model option routes, which the task forms use.

**Migrations:** only the widened `api_tokens_scope_check` for the new token
scope. Nothing changes for draft data:
- There is no draft data. Rows that "Save for later" created are `stopped`
  agents. They stay, and they can still be started from their pages.
- The reaper's skip for saved rows that never started
  (`internal/agents/reap_stopped.go:47`) stays for as long as such rows can
  exist.

#### Text that must change in the same PR

- `KickoffRefusalSummary` becomes *"…create it with `muster agent create
  --start=false`, then Start it from its page, which brings the instance up
  without claiming a first turn"*.
- Its test (`kickoff_refusal_test.go:450`).
- The docs listed in [1.5](#15-every-consumer-of-what-gets-deleted).

**e2e: a fixture path to replace the form.**
- About 15 specs used **Save for later** only to create an agent. They get
  `e2e/lib/seed.ts`, which calls `POST /api/agents` with `start: false` and a
  test user's `mst_` token. That creates exactly the row `action=save`
  produced, and it exercises the new route on every run.
- The specs that tested the form itself (`kinds`, `byo-credentials`,
  `pickers`, `onboarding`, and the dispatch parts of `agent-page-actions`) are
  deleted, or rewritten to assert that the elements are absent.

**Rollback.** One revert of the PR, with no schema change. The chief's tool
does not depend on the form.

**Worker pools keep a creator.** The form was the only creator of a
worker-pool agent (`docs/jobs.md:251`). Decision 5 moves that to the
`worker_pool` option of `agent_dispatch` ([2.2](#worker-pool-agents)), which
ships in phase 1, before this deletion. `worker_pool_form_test.go` becomes a
test of that option through the tool, with the same refusals.

---

## 5. Security

### 5.1 Who can trigger a spawn

| layer | check |
|---|---|
| server | the caller is the owner's chief, and the chief is on (`agent.go:627-634`) |
| server | `MUSTER_CHIEF_DISPATCH_OWNERS` names the owner (the existing opt-in) |
| server | `owner_asked: true`, **verified by `ownerTurnFor`**. The turn is a message the owner typed in a signed-in browser session, and no other turn of the chief's is running. It is re-checked under the reservation's lock |
| server | **or**, for a held mirrored dispatch, the owner's own tap on the card that such a turn raised: same arguments (`held_hash`), the tapped option, an owner door (`ViaSession` or `ViaDevice`), at most 15 minutes old, used once ([2.8](#28-mirrored-dispatch-choosing-host-runtime-and-model)) |
| server | the host is the owner's, connected, v2, `interactive`, and its hello lists the runtime (`hostSpawnable`) |
| server | **`connectedRepo(repo)`** (`task_routing.go:926-944`) passes, for **every** host target, before anything is reserved. This is the same check, and the same refusal sentences, a pod gets |
| server | the caps hold |
| host | `-interactive`, `-agent-spawn`, and the default tmux server; for opencode, `-opencode-send` and a token |
| agent | **claude:** normal permission mode, so tool use the settings in force do not allow is an ask on muster. **opencode: no ask** (decision 3, amended): it runs allow-by-default, bounded by the owner's opencode deny rules and guard plugin, the repository allow-list on the host, and `connectedRepo` above |
| host | the repository is on `-agent-spawn-repos` (by its exact name, or `<owner>/*` for its owner; case-insensitive); the spawn rate limit holds; the audit log's first line is written ([5.5](#55-a-compromised-server)) |
| host | the runtime is one of two constants; `repo` resolves to exactly one clone under a root; `base` passes `check-ref-format`; the tmux server already exists |

**`ownerTurnFor` is the gate, and there is no extra step-up for host spawns**
(D2). The owner typing the request in a signed-in session is what authorises
the spawn. The cost, stated: whoever can type into the owner's muster chat
from a signed-in browser session can ask the chief for a host spawn. That is
the same reach that session already has to the chief itself.

**Owner tools refuse spawned agents.** Host spawns create a new kind of
mirrored session: one that muster started, running a prompt the chief
relayed, in normal permission mode. Today `mirroredSession()`
(`internal/api/attention_tools.go:162-163`) admits **any** listed session, and
it is the caller rule for every tool that acts **with the owner's authority**:

| tool | file:line |
|---|---|
| `agent_destroy` | `agent_destroy_tool.go:78` |
| `chief_restart` | `chief_restart_tool.go:154` |
| `attention_answer` | `attention_tools.go:268` |
| connector request decide (two tools) | `connector_decide_tools.go:166,221` |
| plugin message route (two tools) | `plugin_message_route_tools.go:171,211` |
| plugin draft decide (two tools) | `plugin_draft_tools.go:140,179` |
| `route_reply` | `route_reply_tool.go:68` |

Without a change, a host-spawned agent could therefore answer the owner's
asks, approve plugin drafts, end its own dispatch, or restart the chief.

**So all ten sites switch to one new predicate, `ownersOwnSession()`.**
`mirroredSession()`'s **only caller** becomes `ownersOwnSession()`. It
holds when `mirroredSession()` holds **and** the caller's own window, found
from its pane headers in the hub's listing, carries **no `dispatch` tag**. If
that window cannot be found or read, the caller is refused. That is the
closed direction.
- The refusal is: `"this tool acts with your owner's authority, and a session
  muster started for the chief cannot use it; ask your owner"`.
- A spawned agent keeps the tools that do not act for the owner, such as
  `attention_ask`, `task_get` and `task_comment`.
- `route_reply` is included, so a spawned agent cannot answer a route.
  **`route_to_session` excludes such windows as targets, too.** Its target
  check, `resolveRouteTarget` (`internal/api/session_routes.go:190`), refuses
  a window whose listing carries a `dispatch` tag, with a new reason,
  `dispatched_window`: `"that window runs an agent the chief started; use
  task_forward for its task"`. The listings the chief picks targets from
  (`sessions_overview`, `focus_search`) mark such a window `chief_dispatch:
  N`, so the chief can see why.
- **The ledger:** `mirroredSession()`'s only caller is `ownersOwnSession()`.
  It is asserted over `internal/api`'s Go source, and fails when anything else
  calls it.

**What this does and does not stop.** It is a **mitigation, bounded by normal
permission mode**, not an isolation boundary:
- **The pane is chosen by the client.** The mirror's MCP proxy maps the pane
  the caller names in `X-Muster-Pane`, which Claude Code fills in from its own
  `$TMUX_PANE` (`internal/mirrorhost/mcpproxy.go:46-54`). `route_reply`'s own
  comment says the same: two sessions of one host "can name each other's
  panes" (`route_reply_tool.go:31-37`).
- **So an agent with a shell can step around it.** It could:
  - send another pane's id;
  - clear its own window's option, with `tmux set-option -w -u
    @muster-dispatch`;
  - read the mirror's MCP secret and call the proxy as any pane.
- **What stands in the way** is normal permission mode
  ([5.2](#52-argv-and-prompt-injection)). Each of those is a shell command
  that the owner's settings do not allow by default, so it reaches the owner
  as an ask. An owner, or a repository's settings (which `connectedRepo`
  bounds), that allows arbitrary shell also allows these.
- **What it does stop** is the plain case: an agent that simply calls
  `attention_answer`, or `agent_destroy` on its own dispatch, as itself.

**`POST /api/agents` and what still bounds it.** The route creates pods only,
never a host spawn. Through it, a holder of an `agent-create` token can create
pre-trusted pods with no caps, no `ownerTurnFor` and no `request_key`. That is
what the form allowed.
- **Who can hold one.** The bound is who holds such a token. It is not the
  CLI's ordinary token, because full tokens are refused there
  ([4](#4-replacing-the-dispatch-ui-a-machine-route-a-cli-verb-then-the-deletion)).
  Any process running as the owner's Unix user, including a host-spawned agent,
  can read that user's files. So an `agent-create` token that sits in a file
  on such a host reaches whatever that process reaches.
- **The advice in the docs:** mint it only where it is needed, keep it out of
  any env file a spawned agent's shell inherits, and revoke it when it is not
  in use.
- **What still bounds each pod it creates:** `connectedRepo`, the kinds the
  deployment enables, the owner's Claude account pool, and revocation, which
  refuses the token on its next request.

**No HTTP route or token can spawn on a host.** There is no UI form, no `mst_`
route, no hook-token route and no CLI verb for it. The only caller of `Hub.SpawnAgent` is
`toolAgentDispatch`. A seam test pins that, the way `provisioner_seam_test.go`
pins the provisioner's callers: it asserts the ledger of callers, and fails
when that set grows or shrinks.

**The repository must be a connected one, for every host target.** A
repository can come from outside text: the chief may be reading a plugin
message when it picks `repo`, and a task's repository can have been written by
an agent (`task_routing.go:637-646`). A spawned agent then runs inside that
repository, under that repository's own configuration:
- **claude:** the worktree is pre-trusted (below), so the repository's
  `.claude/settings.json` applies in full. Its hooks run, and its
  `permissions.defaultMode` and allow rules can **override "normal permission
  mode"**. A repository that sets `defaultMode: bypassPermissions`, for
  example, starts an agent that asks nothing.
- **opencode:** it has no trust step at all, so a repository's own
  `opencode.json` (its permission policy, tools and plugins) applies directly,
  from the first command.

`createAndDispatchAgent` already requires a pre-trusted session's repository to
be connected "from every door" (`internal/api/agents.go:1059-1065`). Host
spawns are a new door, so the server runs `connectedRepo` on `repo` for every
host target, claude and opencode alike, before it reserves anything. **That
rule is the bound** on what a repository's own settings can do to "normal
permission mode": only a repository the owner connected to muster can supply
them. The host's own roots are a second bound: the repository must also be
cloned there. They are not the first bound, because a clone under a root may
be one the owner never connected.

**Writes to Claude Code's configuration.** The host writes exactly one value,
`projects[<worktree>].hasTrustDialogAccepted`. It writes it only for a
worktree it created during the same spawn, under its own agents directory, and
verifies the write afterwards. It never writes anything else
([3.6.1](#361-claude-codes-folder-trust-entry)).

That entry grants trust: it waives the dialog that would otherwise ask whether
the repository's own `.claude/settings.json`, hooks included, may run. So it is
limited to repositories that are both **connected** (`connectedRepo`, above)
and held by the host (its own clones, under its own roots), on a branch cut
from that repository's own remote. That is the same bound as a claude-code
pod, which also starts pre-trusted in its checkout (`task_routing.go:637-646`).

The cost of decision 7: a connected repository that the owner has never
trusted in a checkout of their own becomes trusted here, once it is cloned
under a root.

**Forwards** into host windows are muster's fenced text. They go only into a
window the chief started on that task, which the host confirms by its tag,
only when the agent is idle or has no pending ask, and at most 12 per window
per hour ([3.10](#310-forwarding-into-a-host-spawned-agent)).

### 5.2 argv and prompt injection

- **Shell.** Every call is an argv. The only shell code is two constant
  wrapper scripts: claude's prompt-file reader, and opencode's bare
  `exec "$@"` ([3.6](#36-the-commands-and-prompt-delivery)).
  claude's reads the prompt from its `0600` file into a variable, deletes the
  file, and execs the runtime with that variable, in double quotes, as the
  last argument. opencode's sees no prompt at all. The shell never parses the
  prompt.
- **tmux.** The prompt never reaches tmux. tmux would split a command at any
  argument ending in `;`, so the prompt travels in a `0600` file instead, and
  a constant wrapper reads it ([3.6](#36-the-commands-and-prompt-delivery)).
  Every word tmux does see is a constant or a host-built value checked against
  `[A-Za-z0-9/._=@:-]`, and the window name and branch slug are `[a-z0-9-]`.
  None of them can end in `;`.
- **git.** `base` is validated, the repository is resolved by the host from
  its own roots, and the paths are built by the host.
- **The CLIs' own flag parsing.** For claude, `--` plus the fixed preface mean
  the prompt can never be a flag or a subcommand ([3.6](#36-the-commands-and-prompt-delivery)).
  For opencode, the prompt is never an argument: it is the `text` of an API
  post ([3.6](#36-the-commands-and-prompt-delivery)).
- **Prompt injection into the agent** is the real surface. The chief reads
  outside text (plugin messages, task bodies), and its prompt becomes the
  agent's first message. The bounds:
  - **Who:** a host spawn only ever runs in a turn the owner typed, so outside
    text alone cannot trigger one ([2.5](#25-gating-when-there-is-no-task-to-hang-it-on)).
  - **How it is framed:** the prompt is fenced as relayed by the chief, and
    muster's rules close the message and win (draft PRs only; never mark a
    task complete; text from outside is data).
  - **What it can do:**
    - **claude** starts in **normal permission mode**, so every tool use that
      the settings in force do not already allow becomes an ask on muster.
      That is a mitigation, not a boundary. The settings in force are the
      owner's own plus the repository's `.claude/settings.json`, which can
      allow more.
    - **opencode does not ask** (decision 3, amended): it runs in its own
      allow-by-default mode, and no ask stands between a relayed prompt and
      its tools. Its limits are the owner's opencode **deny rules and guard plugin**, the host's
      repository allow-list (`-agent-spawn-repos`), and `connectedRepo`.
      A repository's own `opencode.json` can loosen opencode's rules further.
      Both the spawned agent and the chief are told so (`spawnPermission*`,
      `internal/api/host_dispatch.go`); neither is told it asks.
    - The connected-repository rule above limits which repositories can
      supply either runtime's settings.

### 5.3 Caps

**The server's budget is the spawn limit for a server working as designed**:
the chief's 5 live and 10 per day. Host spawns count against both, and an
offline host's spawns stay counted. **The host now keeps a rate limit of its
own** as well (revising D3), for the case where the server is not working as
designed ([5.5](#55-a-compromised-server)).

**The one host-side limit is 12 forwards per window per hour**
([3.10](#310-forwarding-into-a-host-spawned-agent)). It exists because a paste
loop, such as a chief forwarding its own agent's echoes back to it, would
otherwise type into the window without bound, and nothing else limits
forwards.

### 5.4 Kill switches

These switches exist already, or come with the feature. There is no
separate server switch for host targets (D4).

| switch | effect | how fast |
|---|---|---|
| empty `MUSTER_CHIEF_DISPATCH_OWNERS` | refuses every `agent_dispatch`, on pods and hosts | restart |
| the chief toggle (`chiefActing`, `internal/api/chief_toggle.go:138-150`) | the chief stops acting | immediate |
| drop `-agent-spawn` (or `-interactive`) on the host | that host refuses every spawn | unit restart |
| revoke the host's mirror token | the connection ends, and every spawn and forward path to that host with it | ≤ 30 s (`internal/mirror/service.go:22-25`) |
| close the tmux window | ends that agent; its worktree stays if it holds work | immediate |
| empty `-agent-spawn-repos` on the host | that host refuses every spawn as `no_repo`, while `-interactive` keeps working | unit restart |

### 5.5 A compromised server

Sections 5.1-5.3 bound what the **chief** can do through a server that works as
designed. This section asks what bounds a **server that does not**: one whose
process, database or deploy an attacker controls. Such a server can send any
`agent-spawn` frame it likes to every host connected with the owner's token.
Every server-side check above (`ownerTurnFor`, the held-card binding,
`connectedRepo`, the budget, the caps) is then gone. Only what the host
enforces itself remains.

**What such a server can already do today, without this feature.** With
`-interactive` on, the server can type into an idle Claude Code pane (step-up
is checked on the server, `transcript.go:435`), and it can answer the asks in
those panes, which includes approving a tool use. So "normal permission mode"
is **not** a bound against a compromised server: the server is the one that
relays the owner's answers. This is the baseline, and it is stated so that the
increment below is measured against it, not against zero.

**What `-agent-spawn` adds to that.** The server can start new processes:

- in repositories where no session is open, at any hour;
- running opencode against an OpenRouter account, which costs money;
- with Claude Code's trust entry written for each new worktree, so the
  repository's own `.claude/settings.json` applies.

**The host-side bounds.** Each holds whatever the server sends:

| bound | what it limits | where |
|---|---|---|
| **opt-in** | `-agent-spawn` is off by default, needs `-interactive`, and works only on the default tmux server. The owner's host configuration turns it on for the default mirror unit only | [3.2](#32-the-flag), [3.8](#38-read-only-and-tenant-hosts) |
| **repository allow-list** | `-agent-spawn-repos`, **empty by default**: only repositories the owner listed **on the host** can be spawned in, and only from one clone under a root. An entry `<owner>/*` (the operator's choice, Q1) admits every repository of that owner, matched by equality on the owner, never a prefix. Under this threat a wildcard bounds the OWNER only: within that owner, the bound is which repositories have a clone directly under a root, and `connectedRepo` is a server check that bounds nothing here. List exact names on a host whose roots hold clones you would not spawn in | [3.2](#32-the-flag), [3.3](#33-worktrees) |
| **directory allow-list** | worktrees go only under `-agent-spawn-dir`, the trust entry is written only for a directory exactly two levels below it that this spawn created, and the prompt file only under `$XDG_RUNTIME_DIR` | [3.3](#33-worktrees), [3.6.1](#361-claude-codes-folder-trust-entry) |
| **fixed programs and argv** | the runtime is one of two constants; the host builds every argv word from constants and values it validated; no flag, path or binary comes from the server; the model travels as a JSON field, never an argument, and must be in the host's own OpenRouter catalog | [3.6](#36-the-commands-and-prompt-delivery) |
| **no permission flags** | neither runtime is ever started with a permission-skipping flag. (A repository's own settings can still allow more; the repository allow-list limits which repositories those are) | [3.6](#36-the-commands-and-prompt-delivery), [5.1](#51-who-can-trigger-a-spawn) |
| **rate limit** | `-agent-spawn-rate`, default 3 per 10 minutes and 10 per 24 hours, counted from the audit log below, so a mirror restart does not reset it | [3.2](#32-the-flag) |
| **audit log** | every spawn request, refused or not, is recorded on the host before any work starts | below |
| **forward limit** | 12 forwards per window per hour, and only into windows whose own tag matches | [3.10](#310-forwarding-into-a-host-spawned-agent) |

**The audit log.** `-agent-spawn-log`, a JSON-lines file, mode `0600`, opened
with `O_APPEND`:

- **Two lines per request.** One when the request arrives, **before** the
  rate check and before any git or tmux call: `{at, dispatch, server,
  runtime, repo, base, model, name, prompt_sha256, prompt_bytes, prompt}`.
  One when the request ends: `{at, dispatch, result, detail, window, session,
  worktree, branch}`.
  - The prompt is logged in full. It is at most 8 KiB, it is not a secret on
    these hosts ([3.6](#36-the-commands-and-prompt-delivery), "Cost"), and
    it is the one thing the owner needs to see to judge what a server asked
    for.
  - `server` is the muster URL the mirror is connected to, so a log shared by
    two tenants' mirrors could still be told apart. (Only the default mirror
    spawns, [3.8](#38-read-only-and-tenant-hosts).)
- **Fail closed.** If the first line cannot be written, the spawn is refused
  `unavailable` (detail `audit log unwritable`) and nothing runs. The rate
  limit is counted from the same file, so it cannot be bypassed by a log
  that is failing.
- **A copy goes to the mirror's own log** (stderr, which the user's service
  manager keeps), one line per request. The file is the record the owner
  reads; the service log is the copy that a process running as the owner
  cannot quietly rewrite.
- **It is evidence against the server, not against the agent.** A spawned
  agent runs as the same Unix user, so it can edit the file. The service log
  copy is what survives that.
- `muster-mirror agent-spawn-log [-n N]` prints the last entries, so the
  owner can check without knowing the path.

**What is still open to a compromised server, stated.** Up to 10 spawns a
day, each in a repository the owner allow-listed on that host, each
self-approving its own asks through the server, each logged. That is the
blast radius this design accepts for the feature. An owner who does not accept
it leaves `-agent-spawn` off on that host, which is the default.

## 6. Phases

The order is changed from the owner's list: **the UI deletion moves to phase
2.** The reasons:

- **It depends only on phase 1.** The form never started anything on a host,
  so deleting it after phase 1 takes nothing away that the chief cannot do.
- **The form is the uncapped path.** Until it goes, two dispatch paths exist,
  with different guards (no caps, no budget, `connectedRepo` only for some
  kinds). Removing it early leaves one predicate sooner.
- **Host spawn is the longest and riskiest work.** It has several UNVERIFIED
  items (the trust entry's exact effect, and opencode's model selection and
  working directory, [7](#7-what-is-still-unverified)). It also copies a pattern, `-resume-spawn`, that
  has never run on the owner's hosts. The deletion should not wait behind it.

If the owner wants the form kept until host spawning works, the original order
still works: every phase ships on its own.

### Phase 1: `agent_dispatch` for pods, with or without a task, the required target, `worker_pool` and `request_key`

**Shipped in #428.** The text below is the design it was built from, kept for its tests and reasoning.

**What ships:**
- `agent_dispatch`, whose no-target refusal carries the targets and the budget
  (D1);
- `task_dispatch` removed, and its name answering the "replaced" refusal;
- `ReserveChiefDispatch` keyed by object, with `dispatch:<32 hex>` objects
  ([2.6](#26-budget-and-accounting-for-a-dispatch-with-no-task));
- `owner_asked` required with no task, and **`owner_asked: true` always
  running `ownerTurnFor`** ([2.5](#25-gating-when-there-is-no-task-to-hang-it-on));
- argument decoding with `DisallowUnknownFields`;
- the 8 KiB prompt;
- `liveOnTask`, used at every call site listed in
  [2.6](#26-budget-and-accounting-for-a-dispatch-with-no-task);
- **`worker_pool`** (decision 5), with the form's checks
  ([2.2](#worker-pool-agents));
- **`request_key`** (decision 8), with migration
  `0072_chief_dispatch_request_key.sql` (the partial index)
  ([2.6.1](#261-request_key-idempotency));
- updated text in the chief instructions, `TaskEventTools`, the
  `task_forward` refusal at `task_routing.go:545`, `docs/chief.md`,
  `docs/plugins.md` and `docs/jobs.md:251`.

**Tests** (the Postgres suite, in the style of
`internal/api/task_routing_pg_test.go`, **each guard with its own refusal**):
- **No target:** the exact refusal sentence, nothing reserved, and the budget
  unchanged. The refusal's `targets` lists exactly the enabled pod kinds, and,
  with a fake hub, exactly the owner's spawnable hosts. A second owner's host,
  an offline host and a read-only host are each absent. Each malformed target
  gets its own sentence, carrying the same list.
- **No task:**
  - without `owner_asked`: refused;
  - with `owner_asked`, but on a plugin-message turn: refused with
    `ownerAskedNoTaskNoOwnerTurn`. A plugin turn has no owner-turn marker, so
    `ownerTurnFor` returns `chiefReleaseNoOwnerTurn`
    (`chief_owner_release.go:320-321`), and the mapping turns it into the
    sentence without the gate;
  - without `request_key`: refused;
  - with both, in the owner's typed turn: an agent starts, the ledger row's
    object matches `^dispatch:[0-9a-f]{32}$`, and `today` goes up by 1.
- **`owner_asked` on an ungated task (R1):** an open, ungated task, with
  `owner_asked: true` and an 8 KiB prompt, on a **plugin-message turn**, is
  refused with `ownerAskedUngatedNoOwnerTurn`, the sentence that does not
  mention a gate. It is not `chiefReleaseNotOwnersTurn`: a plugin turn has no owner-turn
  marker at all, so the check fails at `chief_owner_release.go:320-321`.
  Nothing is reserved, and no agent row exists. The same call in the owner's typed turn starts the agent, with
  the prompt fenced as relayed by the chief. The same task with no
  `owner_asked` and the same prompt is refused by `ownerAnswer`, as today.
- **The sibling refusals:** the same failure on a task the chief gated answers
  today's `chiefReleaseNoOwnerTurn`, on an ungated task answers
  `ownerAskedUngatedNoOwnerTurn`, and with no task answers
  `ownerAskedNoTaskNoOwnerTurn`, which does not mention `task_route_attention`.
  A table-driven test runs every row of [2.5](#25-gating-when-there-is-no-task-to-hang-it-on)'s
  table through `ownerAskedRefusal`, for all three task kinds.
- **Unknown fields:** a call with a field outside the table (`kind`, the old
  name, and `foo`) is refused with the arguments sentence. Each is its own
  case, and nothing is reserved.
- **Two task-less dispatches with different keys:** two agents, and `live`
  goes up by 2. At `live_cap` the third is refused with the live-cap sentence.
- **`request_key`:**
  - the **same key and the same parameters** called twice answers
    `replayed: true` with the first call's `agent_id`, and spends nothing
    (`today` and the ledger row count are unchanged);
  - the **same key with any one parameter changed** is refused with the
    reuse sentence. That covers each of target, task, repo, base, model,
    worker_pool, prompt and owner_asked, each its own case, using pairwise
    distinct fixture values;
  - **two concurrent calls with one key** start one agent;
  - **the same key used by a second owner** starts that owner's own agent;
  - **a key older than 24 hours** (the row's `at` moved back) starts a new
    one;
  - **a reservation released after a refused create** does not hold its key,
    and a spent failed one replays `started: false`.
- **`worker_pool`:**
  - on `pod/gateway`, or with a `task_id`: refused, each with its sentence;
  - a name outside the pattern: refused;
  - a valid one creates and starts a `claude-code` agent in the pool, with no
    first turn, holding a live slot;
  - without a prompt, on a deployment whose `KickoffUndeliverableReason` is
    set: refused with `"this deployment cannot start an agent now: <reason>"`
    ([2.2](#worker-pool-agents)).
- **Every `task_dispatch` guard, re-run through `agent_dispatch` with a
  `task_id`**, with today's sentences unchanged. This is the existing table,
  ported.
- **`task_dispatch` called by name:** the "replaced" refusal.
- **`model` on `pod/claude-code`:** refused. **`model` on `pod/gateway`:**
  accepted.
- **The description carries no cap number** (the existing pin, moved).

**Mutation checks.** Rebuild and re-run for each mutant, and keep one mutant
known to be caught in the batch as a positive control:
- Default the target to `pod/claude-code`: the no-target test goes red **with
  the target refusal**, not with another error — and so do the no-model test
  (`TestAgentDispatchWithNoModelAsksTheOwnerInTheirWords`: the refusal must
  tell the chief to ask with `ask_owner` and list the choices in the owner's
  words) and the browser case where "dispatch" with no model must draw an
  `ask_owner` card offering Claude and OpenRouter models.
- Drop the `hostSpawnable` filter from the list: the read-only-host case goes
  red.
- Drop the `ownerTurnFor` call for task-less dispatches: the plugin-turn case
  goes red.
- Run `ownerTurnFor` only for gated tasks, which is today's condition: the R1
  case goes red with a started agent.
- Decode without `DisallowUnknownFields`: the unknown-field cases go red.
- Count `dispatch:` objects under `note:` instead: the live count of the
  two-in-a-row case goes red.
- Leave the prompt out of `request_hash`: the changed-prompt reuse case goes
  red.
- Look the key up outside the advisory lock: the concurrent case goes red.
  This is a race, so the test runs it many times and the mutant has to fail at
  least once.

**Acceptance:**
- **Live, on the owner's deployment:**
  - "start an agent to do X" in the chief's chat produces a question asking
    where to run it;
  - answering "a claude-code pod" starts a pod with no task, which shows in the
    sidebar and in `live_agents`;
  - a retried call with the same key answers `replayed`;
  - "start a worker in pool `<name>`" creates a pool agent that picks up a
    machine job.
- The same request from a plugin message is refused.

### Phase 2: delete the dispatch UI

**What ships, in this order, in one PR:**
1. `POST /api/agents` and `muster agent create` ([4](#4-replacing-the-dispatch-ui-a-machine-route-a-cli-verb-then-the-deletion)),
   so a deployment without the chief can still create agents;
2. then the UI deletion, in full.

Phase 1's `worker_pool` makes the deletion possible for chief deployments
(decision 5), and the machine route makes it possible for every other kind.

**Tests:**
- `POST /api/agents`:
  - an `agent-create` token creates an agent as its owner, with both
    `start: true` and `start: false`;
  - **a full-scope token is refused**, with the full-token sentence from
    [4](#4-replacing-the-dispatch-ui-a-machine-route-a-cli-verb-then-the-deletion),
    while the same token still reaches every other machine route. Mutation check:
    drop the `explicitScopeRoutes` exception, and this case goes red;
  - an `agent-create` token is refused on every other route, with the
    agent-create sentence. A summary token still gets today's sentence;
  - `CleanScope("agent-create")` is accepted, and the mint form offers
    **Create agents**;
  - the shared hook secret (`ViaHook`) is refused by the handler;
  - each of the form's refusals answers with its sentence: kind, account,
    worker pool, `connectedRepo`, the model credential, a gated `note_id`, and
    `KickoffUndeliverableReason` with `start: true`. The table is ported from
    `kinds_test.go`, `worker_pool_form_test.go` and
    `kickoff_preflight_test.go`;
  - `start: false` works where `KickoffUndeliverableReason` is set.
- `muster agent create` drives the route, and reads only
  `MUSTER_AGENT_CREATE_TOKEN`. With only the CLI's ordinary full token set, it
  fails with the scope refusal. `make verb-ledger` gains the verb.
- `removed_surfaces_test.go`: the three routes answer 404, and the elements
  `#sidebar-deploy` and `#home-deploy` are absent.
- The route golden files are updated.
- The e2e suite is green with `lib/seed.ts` in place of the form.
- A Playwright check that the sidebar has no **+** and no "not set up" hint,
  and that the task card has no **Dispatch** (D8: nothing replaces them).

**Acceptance:**
- `git grep -n -e sidebar-deploy -e home-deploy -e 'value="save"' -e /ui/agents/new -- internal cmd e2e web ':!internal/ui/removed_surfaces_test.go'`
  finds nothing. It is scoped to code paths, so this doc and the other docs,
  which name these strings, are not matched. **A positive control runs
  first:** the same grep at the parent commit lists **16 files under `e2e/`**
  (15 specs plus `e2e/lib/dispatch.ts`) **and 19 under `internal/`**. Measured
  at `4ec696cf` with `git grep -l`.
- On a deployment with no chief, `muster agent create` creates and starts an
  agent.
- A stopped agent that "Save for later" created before the upgrade still
  starts from its page.

#### As built (phase 2)

Built as specified, in one PR. Where it differs from the text above, and why:

- **`POST /agents` answers 405, not 404.** `GET /agents` (home) is still
  registered on that path, so the mux answers Method Not Allowed with an
  `Allow` that names no POST. `GET /ui/agents/new` and `GET /ui/agents/notes`
  answer 404. `internal/api/dispatch_ui_removed_test.go` pins all three (in the
  api package, where the routes are; `internal/ui/removed_surfaces_test.go`
  pins the absent elements).
- **A `note_id` that names no readable task is a 404 (`no such task`).** The
  form silently dropped such a value and dispatched without the task; for a
  machine caller that would create an agent on the wrong work.
- **`muster agent create` with only the ordinary token sends nothing.** It
  refuses locally (exit 2) with the scope sentence, rather than sending a
  request that would carry no credential, so the full token never reaches the
  route.
- **The acceptance grep** finds, outside `removed_surfaces_test.go`, only
  absence assertions (the Playwright checks this section asks for, and the
  api test of the removed routes) and comments naming what was removed.
- **`e2e/lib/seed.ts`** mints its token with `e2e/mktoken` (through the real
  token store, as the user) and re-mints once on a 401, because some specs
  reset the accounts tables. `agent-outcomes.spec.ts` posted to `POST /agents`
  directly and was moved to the new route as well.

### Phase 3: host spawn for claude, with the trust entry and forwarding

**What ships:**
- `-agent-spawn` and its sibling flags;
- `hello.agentSpawn`, `agent-spawn`, `agent-spawned`;
- worktrees, the `agents` session, `@muster-dispatch`;
- **`trustSpawnedWorktree`** ([3.6.1](#361-claude-codes-folder-trust-entry));
- the prompt file and claude's constant wrapper ([3.6](#36-the-commands-and-prompt-delivery));
- `connectedRepo` for every host target ([5.1](#51-who-can-trigger-a-spawn));
- the host-dispatch reconciler, with `Hub.SetListingNotify`,
  `Hub.SetSpawnReplyNotify` (the late-reply path), the countdown for `pending`
  rows past 180 s, and the 24-hour sweep;
- `host_dispatches` in `plugins.PGStore`, `ReserveChiefDispatch`'s
  `onReserved` hook (with `reservation_id`), `latestStartAgent`'s nullable
  two-kind scan, `releaseHostDispatchTx`, and `EndHostDispatch`
  ([2.6](#26-budget-and-accounting-for-a-dispatch-with-no-task),
  [3.7](#37-linking-the-spawned-window-to-the-dispatch-and-the-task));
- `agent_destroy` taking a `host_dispatch` id, as the owner's recovery;
- `host_dispatches.reservation_id`, and release keyed by it
  (`releaseHostDispatchTx`);
- the host's 120 s spawn deadline, which counts queue wait;
- the orphan `KindNotice`, with `orphan_notice_id` and `SetHostDispatchOrphanNotice`;
- `ownersOwnSession()` at all ten owner-tool sites
  ([5.1](#51-who-can-trigger-a-spawn));
- the 10-minute age limit on chief-forward rows;
- mentions on tasks that only a host agent works, handed to the chief as a
  notice;
- `host_dispatches` (migration `00NN`);
- `Hub.SpawnAgent`;
- the `host/<host>/claude` target;
- the chip on the window page;
- the cleanup sweep;
- **forwarding to claude windows**: `forwardTarget` over `liveOnTask`; the
  chief-forward send-queue row with `sendQueueChiefForwardGate`; `PasteWindow`
  with `ExpectTurn` and `mirror.ForDispatch`; the host's tag check; and the
  12-per-hour limit
  ([3.10](#310-forwarding-into-a-host-spawned-agent));
- **mirrored dispatch** (decisions 10-18,
  [2.8](#28-mirrored-dispatch-choosing-host-runtime-and-model)), for claude:
  - the held targets `host/?/claude`, `host/?/?` and `host/<host>/?`, the
    `chief.dispatch_held` audit row, the host card and the model card through
    PR A's `ask_owner`, and the completing call's binding to the owner's tap;
  - `host_dispatches.held_id`;
  - the mapping table in the chief's instructions and in `agent_dispatch`'s
    description, with the whole-paragraph pin and section digests updated as
    for #482;
  - the model card offers only `Claude (Claude Code)` until phase 4 ships, when
  the configured list and the typed id join it;
- **the live agent card for host dispatches**, keyed by the row id
  ([3.7](#the-live-agent-card));
- **the host-side controls of [5.5](#55-a-compromised-server)**:
  `-agent-spawn-repos` (empty by default), `-agent-spawn-rate`, the audit log
  with its fail-closed first line, and `muster-mirror agent-spawn-log`.

**Prerequisite: PR A merged.** Before building on it, the phase 3 PR checks
each of the four properties [2.8](#28-mirrored-dispatch-choosing-host-runtime-and-model)
needs from `ask_owner` against PR A's merged code, and records the answer in
the PR. A property that does not hold is a design change here first, not a
workaround in code.

**This is the first real run of the spawn pattern.** `-resume-spawn` has never
been enabled on the owner's hosts, so nothing in production has yet exercised
`hello` advertising, the hub's spawn request and reply, the login-shell
`exec "$@"` wrapper, or `new-window` into a fixed session. Budget for the first
live run to find what the tests could not.

**First, measure.** Record each result in the PR:
- **the spawn's own time, before anything else.** Measure fetch, worktree add
  and submodule time on the owner's real repositories, cold and warm. If the
  slowest exceeds about 60 s, half the 120 s host deadline, raise the three
  values in [3.7](#37-linking-the-spawned-window-to-the-dispatch-and-the-task)
  together, keeping their 30 s gaps;
- with the trust entry written, `claude` in a fresh worktree reaches its
  prompt with no dialog, and without the entry the dialog appears. That pair
  is the control. Also record the path spelling the key needs, and which file
  is used when `CLAUDE_CONFIG_DIR` is set;
- whether `claude --session-id <uuid> -- update` starts a session whose
  prompt is `update`. Check it with no stubs, on a scratch config directory;
- that `--session-id` shows up as the pane's `claudeSession` without the
  guess.

**Tests:**
- **Host, against a real tmux server.** The mirrorhost suite already drives
  tmux. Use stub `claude`/`opencode` binaries that record their argv.
  - **The prompt arrives verbatim**, through the file and the wrapper. Each
    of these is its own case, and the stub must see exactly
    `[--session-id, <uuid>, --, <prompt>]`, with the prompt byte-identical to
    what was written:
    - an inner `;` (`a;b`);
    - a trailing `;` (`tail;`, the case tmux would split);
    - `\;`;
    - `$(id)` and backticks;
    - single and double quotes;
    - a leading `-`;
    - a leading and a trailing newline;
    - 8 KiB of text.

    In every case the prompt file is gone once the runtime starts. A wrapper
    whose read fails exits 97, and the dispatch ends.
  - **The spawn deadline counts queue wait.** A second spawn is queued
    behind a first that a stub `git` holds for 130 s. The second is refused
    with `failed`, detail `deadline passed`. No `new-window` runs for it, and
    its worktree is removed. A spawn that reaches the deadline check before
    `new-window` while past the deadline is refused there.
  - **A submodule step cut short by its budget is a warning, not a
    refusal.** A stub `git submodule` sleeps past the budget. The agent still
    starts, before the deadline, and the reply carries `submodules
    incomplete`.
  - **Host-built words pass the character check.** An `<agent-spawn-dir>`, or
    a prompt directory, containing a `;` makes `-agent-spawn` `unavailable` at
    startup.
  - **Constant words do not end in `;`.** A unit test enumerates every
    constant word the spawn passes to tmux, including both wrapper scripts and
    `-F '#{window_id}'`, and asserts that none ends in `;`.
  - **Prompt files** are written under `$XDG_RUNTIME_DIR/muster-mirror/prompts/`,
    never under `<agent-spawn-dir>`. The worktree sweep never touches that
    directory, and the hourly prompt pass deletes a file there older than an
    hour. With `XDG_RUNTIME_DIR` unset, the result is `unavailable`.
  - Each of the four refusal codes gets its own case, and so does each cause
    it covers, asserting its `detail`. That covers a repository with no clone,
    one with two clones, and the case where the refusal names the
    repository.
  - A dirty clone spawns, and the clone's `git status --porcelain` and `HEAD`
    are byte-identical before and after.
  - A leftover branch with the dispatch's name gives `failed`, and the branch
    is untouched.
  - A `base` of `--upload-pack=x` is refused before git runs.
  - **The default tmux server.** Each of these is its own case:
    - `-tmux-socket work` is refused with `unavailable`, detail `not the
      default tmux server`;
    - a `-tmux-socket-path` to a socket named `default` in another directory
      is refused the same way;
    - no flag, `-L default`, and an `-S` path that `realpath`s to
      `${TMUX_TMPDIR:-/tmp}/tmux-<uid>/default` are each accepted;
    - a custom `TMUX_TMPDIR` is honoured.
  - `@muster-dispatch` is set on the window. A global value blocks the spawn.
  - Cleanup, after the test closes the window: a clean worktree is removed, a
    dirty one stays, and an unpushed branch stays. The sweep after a simulated
    crash finds each one. A kept worktree's trust entry is still present, and
    a removed worktree's entry is still present too (D5: it is inert).
  - **The trust entry**: every case and mutant listed in
    [3.6.1](#361-claude-codes-folder-trust-entry).
  - **A forward**:
    - a paste carrying `dispatch` into a window whose tag differs, or that has
      no tag, is refused (`not_muster_window`), and the stub sees no input;
    - a matching one is delivered once;
    - the 13th forward in an hour is refused.
- **Ordering.** `agent-spawned` comes only after a listing on the same
  connection, with the three cases from host-sessions. Mutation-check it by
  moving the reply before the listing.
- **Server.** The PG suite:
  - a host target without `owner_asked` is refused;
  - a host target whose `repo` is not connected is refused by `connectedRepo`,
    with its sentence, **before** anything is reserved. That holds for claude
    and for opencode. A repo the host does hold, but the owner never
    connected, is the case that matters;
  - a plugin-message turn is refused (`ownerTurnFor`, the only gate: D2);
  - an offline host is refused, and **nothing is queued**;
  - another owner's host is never listed, and is refused;
  - a host spawn counts in `live` while it is `pending` (including a reply
    that timed out, and an offline host) and while it is `running`. An
    `ended` one does not count;
  - **the reconciler**, through `UpdateHostDispatch`, and `EndHostDispatch` (the only writer of `ended`):
    - closing the window takes the row to `ended` after two listings, and not
      after one, and releases the task's one start, writing as the owner
      (`ViaHostDispatch`);
    - a `pending` row younger than 180 s is not counted down;
    - **a `pending` row older than 180 s and absent from two listings ends**,
      and releases its task's start in the same transaction;
    - **a late reply after the timeout is applied.** The waiter has timed out
      and the row is `pending`. Then an `agent-spawned` frame for its
      `dispatch` arrives, and the row records the window. A late refusal ends
      the row. A duplicate reply changes nothing. Neither goes through the
      waiter;
    - `agent_destroy` with `host_dispatch` ends a `pending` row and a
      `running` row of the owner's, and releases their tasks' starts. It
      refuses another owner's row, and it is refused for the chief;
  - **every way a host dispatch on a task ends releases the task's start, and
    a new `agent_dispatch` on that task then succeeds.** Each is its own case:
    - a refused spawn (no agent row);
    - a `pending` row counted down (no agent row);
    - a `running` row whose window closed (agent row `{host_dispatch_id}`);
    - a row ended by `agent_destroy`;
    - a row ended by the sweep.

    In every case, `ActionChiefDispatchReleased` names the row's
    `reservation_id`. A release by an older `reservation_id`, after a newer
    reservation exists, writes nothing;
  - **`latestStartAgent` on a host start** returns `HostDispatchID` and no
    error. Today's `int64` scan fails on the NULL `agent_id`, so this is
    pinned. Destroying a pod on a task whose latest start is a host dispatch
    releases nothing. A completed task stops no host window;
  - **late reply matching:** a reply for owner A's dispatch, arriving on
    owner B's connection, changes nothing and is logged. So does a reply for
    host X's dispatch arriving on host Y's connection, for the same owner;
  - **orphan window:** a listing carrying a `dispatch` whose row is `ended`
    raises exactly one `KindNotice` attention row, and records its id in
    `orphan_notice_id`. A second listing raises no second notice. The row
    does not become `running`, because `UpdateHostDispatch`'s condition
    refuses. The notice is withdrawn once the window is gone. A tag naming a
    dispatch the owner has no row for is logged, and raises no notice;
  - **`route_to_session` excludes dispatched windows:** a target window whose
    listing carries a `dispatch` tag is refused with `dispatched_window`, and
    the same window without the tag is accepted;
  - **a refused host spawn frees its `request_key`:** after a refusal, the
    same key and the same parameters start a new dispatch. After a dispatch
    that ran and then ended, the same key replays the first result;
  - **the token list** labels an `agent-create` token "create agents", not
    bar-summary. A summary token on `POST /api/agents` gets the agent-create
    sentence;
  - **owner tools refuse spawned agents:** for each of the ten tools, a call
    from a window that carries a `dispatch` tag is refused with the
    owner-authority sentence. A call from an untagged window of the owner's
    works. A caller whose window cannot be found in the listing is refused.
    Mutation check: revert one site to `mirroredSession()`, and that tool's
    case goes red;
    - a host whose `host_seen_at` is 25 hours old has its rows ended by the
      sweep. That still holds after a muster restart, with the hub empty;
    - a reservation for a host target is counted once in `live`: in source C
      and not in B, during the 10-minute window and after it;
  - with 5 live dispatches, a host spawn is refused with the live-cap
    sentence, before anything reaches the host;
  - `liveOnTask` blocks a pod beside a host agent on the same task;
  - a mention on a task that only a host agent works reaches the chief as a
    task notice, and is not pasted;
  - `task_forward`:
    - it queues a chief-forward row for a host window on the task, and the
      dispatcher delivers it once the window is idle;
    - a row for a `waiting`, `busy` or unreported window is not delivered
      until the window is idle;
    - a row more than 10 minutes old fails with the stale-forward detail, and
      is never pasted;
    - the gate refuses a row whose dispatch has ended, or whose window or
      session no longer matches;
    - it never targets a host window whose listing lacks the row's `dispatch`,
      or whose session id differs;
    - it never targets a task-less dispatch;
- **Seam ledgers.**
  - The only caller of `Hub.SpawnAgent` is `toolAgentDispatch`.
  - The only code that sets `ChiefForwardDispatch` on a `sendqueue.Item` is
    `toolTaskForward`. The ledger scans `internal/api`'s Go source.
  - **The one `host_dispatches` ledger** ([3.7](#37-linking-the-spawned-window-to-the-dispatch-and-the-task)):
    - the `pending` insert is made only through `onReserved`;
    - `UpdateHostDispatch` is called only by the reconciler;
    - `EndHostDispatch` is the only writer of `ended`, and is called only by
      the reconciler and `agent_destroy`. So `status` is written by the
      reconciler (through `UpdateHostDispatch` and `EndHostDispatch`) and by
      `agent_destroy` (through `EndHostDispatch`), and by nothing else;
    - `SetHostDispatchOrphanNotice` is called only by the reconciler;
    - `UpdateHostDispatch` on an `ended` row changes nothing (its
      `status <> 'ended'` condition).
  - `mirroredSession()`'s only caller is `ownersOwnSession()`.
  - `TestEveryServerPasteGoesThroughPasteWindow` is **unchanged and green**.
- **Mirrored dispatch: the held card and its binding** (PG suite, with a fake
  hub and PR A's `ask_owner` store; [2.8](#28-mirrored-dispatch-choosing-host-runtime-and-model)).
  Fixtures use two spawnable hosts and one offline host with pairwise distinct
  names, so a mutant that picks "the first host" or "the only host" is seen:
  - `host/?/claude` in the owner's typed turn writes one `chief.dispatch_held`
    row, raises one card whose options are exactly the two spawnable hosts
    that offer `claude`, reserves nothing, and answers `held: true`. The
    offline host is absent. The same call again answers `replayed` and raises
    no second card;
  - with **no** spawnable host, the call is refused with the no-host sentence,
    and **no card is raised and no row written** (counted: 0 cards, 0 rows,
    with the one-host case as the positive control: 1 card, 1 row);
  - the same held call on a **plugin-message turn** is refused by
    `ownerTurnFor`: no row, no card;
  - **completing**: the owner taps host B through `ViaSession`, then through
    `ViaDevice` (two cases). The completing call with `host/B/claude`, the
    same key and the same arguments starts the dispatch; `held_id` is set;
    `today` goes up by exactly 1;
  - each completion refusal is its own case with its own sentence: target
    host A after a tap on B; the prompt, the repo, the base or the task
    changed (four cases); the card not yet answered; the answer recorded
    through `attention_answer` from a mirrored session; the row 16 minutes
    old; a second completion after the first (the row is consumed, so one
    tap starts one agent);
  - `host/?/?`: the host tap writes a second held row with the host filled and
    raises the model card, whose options in phase 3 are exactly `Claude
    (Claude Code)`; tapping it completes as `host/B/claude`;
  - **the owner types instead of tapping**: a full target with `owner_asked`
    in that typed turn starts the dispatch through `ownerTurnFor`, and the
    held row is left unconsumed.
- **Browser (e2e), claude**: the claude half of phase 4's browser spec
  ships here, on `cmd/muster-mirror/e2emirror.go`'s fake host: a held
  `host/?/claude` call, the host card tapped in the Attention view, the window
  in the sidebar with the chief chip, the live agent card `running`, and the
  fake Claude pane's first prompt record byte-identical to the prompt.
- **Mutation checks for the binding**: accept the completing call without
  comparing `held_hash` (the changed-prompt case goes red with a started
  dispatch); accept any door (the mirrored-session case goes red); skip the
  consumed check (the second-completion case goes red with two dispatches);
  build the host options without `hostSpawnable` (the offline host appears).
- **Host-side controls** ([5.5](#55-a-compromised-server); mirrorhost suite,
  real tmux, stub runtimes):
  - with `-agent-spawn-repos` empty, every spawn is refused `no_repo`, detail
    `not on this host's allow-list`, before any git call (a stub `git`
    records zero invocations). With the repository listed, the same request
    spawns (the positive control);
  - the fourth spawn within 10 minutes is refused `unavailable`, detail `rate
    limited: …`, and so is the eleventh in 24 hours. **The limit survives a
    mirror restart**: a fresh host process reading the same audit log refuses
    the fourth;
  - every request writes its first audit line **before** the rate check and
    any git or tmux call, and its second line with the result; a refused
    request has both lines. The lines are counted, not grepped;
  - with the audit log unwritable (a read-only directory), the spawn is
    refused `unavailable`, detail `audit log unwritable`, and the stub `git`
    and stub runtime record zero invocations;
  - each of these runs with a **server that sends anything**: the request is
    built by the test, not by muster's server, so the host's checks are what
    is under test.
- **Mutation checks for forwarding:**
  - drop the host's tag check: the mismatched-tag case goes red **with a
    delivered paste**, not with another error;
  - drop `sendQueueChiefForwardGate`'s `running` check: the ended-dispatch
    case goes red with a delivered paste;
  - drop `mirror.ForDispatch` from the paste: the mismatched-tag case goes
    red, because the host no longer receives a `dispatch` to check;
  - drop the `pending` age countdown: the stuck-`pending` case goes red with a
    slot still held;
  - make `releaseHostDispatchTx` require an agent row, which is today's
    behaviour: the refused and counted-down release cases go red, and the new
    dispatch on the task is refused `already_dispatched`;
  - start the host deadline at mutex acquisition rather than receipt: the
    queued-spawn case goes red with a started window;
  - deliver late replies only through the waiter: the late-reply case goes
    red.

**Acceptance:**
- **Live, on the owner's default mirror:**
  - "fix the typo in the README of `<owner>/<repo>`" → the chief asks for the
    target → "on `<host>` with claude" → a window `cc-<n>-…` appears in
    `agents` on `<host>`, in a new worktree, and in muster with the chief
    chip, **with no trust dialog**;
  - its first tool use arrives on muster as an ask, and answering it from the
    phone continues the session;
  - a comment on the task, routed by the chief with `task_forward`, appears in
    that window as muster's fenced forward;
  - closing the window in tmux ends the dispatch in muster. A clean worktree
    is removed and an edited one is kept.
  - **"start a mirrored agent with Claude to fix the typo in the README of
    `<owner>/<repo>`"**, naming no host → a host card listing the hosts that
    can spawn now → a tap on the phone → the window appears on that host, and
    the live agent card in the chief's chat shows it `running`;
  - the same request **without** "mirrored" starts a cluster agent (PR A's
    rules), and no host card appears;
  - `muster-mirror agent-spawn-log` on the host shows both lines of each
    request.

#### As built (#513, #516)

Where the build differs from the plan above, and what it leaves for later:

- **The card is #513's stored `ask_owner`** (an attention ask raised by the
  chief, `raiseOwnerCard`). The owner's tap in the CHAT is their typed message,
  which `ownerTurnFor` already admits; a tap on the Attention view or the phone
  is recorded with its door (`browser`, `device`, migration 0090) and binds the
  completing call (`completeHeld`). `held_id` stores the hold's id
  (`held:<hex>`), not an audit row number.
- **`held_hash` is computed with `owner_asked` written as false**: the
  completing call is, by design, made in a turn the owner did not type.
- **A gated task is refused for a host target** ("a host target does not
  release a gate"), instead of releasing the chief's gate as the pod path
  does. Releasing on a host start is not built.
- **`ownersOwnSession` covers 12 tools**: the ten above plus #501's
  `dispatch_request` and `dispatch_approve`, which also act with the owner's
  authority. Its window is the one the MCP door bound the call to
  (`toolCaller.mirrorWindow`); a per-session host (no windows) cannot be tagged.
- **`task_forward` tries a pod first**, as before, and reaches a host-spawned
  window only when no pod on the task is reachable: not one "newest across both
  kinds" order.
- **The chip** is on the sidebar row only, and says "chief": the listing
  carries the dispatch, not the task. The window page and the task number are
  not built.
- **Ending a host dispatch reopens its task** (as a pod's destroy does) when
  nobody else works it, so the released start can be used.
- **A mention on a task only a host agent works is not handed to the chief as a
  task notice.** Read in the code (`routeTaskNoticeEvent` → `actOnMention`):
  with no reachable pod it takes the existing no-agent path, an attention ask
  about the task raised as the chief. It is never pasted into the window, which
  is the safety property; the chief-notice route is not built, and no test pins
  either.
- **Not built in phase 3:** the e2e browser spec; the host-side "listing before
  reply" step is built, but its tests call the spawner without a connection.
- **The sweep's dirty-tree check is defence in depth**: removing a worktree
  without force refuses a dirty tree too (its mutant survives).


**Delta audit of #516.** The trust write takes Claude Code's own lock, a
`<file>.lock` directory (proper-lockfile, with its 10-second stale rule),
around the read-modify-rename. It waits at most 5 s; on failure no entry is
written and the window shows the trust dialog. It keeps the file's mode and
refuses a symlink. The rate limit counts `admit` lines, written under the
spawner's lock. Every audit field is capped, and the reader skips an oversize
or corrupt line. `git` is resolved once at startup, and no git means nothing is
offered. A clone's worktrees count as the clone (by `--git-common-dir`). Each
held blank is filled only by its own card's tap. A tap-completed start's retry
with the same `request_key` replays it. The spawn prompt's preface and rules
are never cut. A host-spawned window's spawn prompt is never the owner's
authority for a chief nudge. A chief forward is muster's own message for the
nudge, the send queue and the ask log, and `send_queue_one_origin` covers it.
### Phase 4: host spawn for opencode with an OpenRouter model, and forwarding through its API

**Rebuilt on #486.** The earlier phase 4 measured `--session`, `--prompt` and
the send route first. #486 settled all three, so this phase reuses the
launcher and the API send, and adds the model
([3.6](#36-the-commands-and-prompt-delivery)).

**What ships:**
- the `host/<host>/opencode` target, with `model` required
  ([2.2](#22-parameters)) and its shape check;
- `hello.agentSpawn` listing `opencode` only with `openCodeSend`
  ([3.1](#31-wire-messages-and-capability));
- `agent-spawn.model`, the `bad_model` refusal, and the host's model check
  against `GET /api/model`;
- `Client.CreateSession` taking a `model` (`Model.Ref`), and
  `Client.RemoveSession` for a spawn that fails before its prompt;
- the spawn's steps: create the session, open the window on `muster-mirror
  opencode -s <id>`, wait for the bind, post the prompt with a message id
  derived from the dispatch;
- forwarding to opencode windows through the same chief-forward row and #486's
  `gatedSend`, with the host's tag check placed before it
  ([3.10](#310-forwarding-into-a-host-spawned-agent));
- the model card's OpenRouter options
  ([2.8](#28-mirrored-dispatch-choosing-host-runtime-and-model)), from the
  `MUSTER_CHIEF_DISPATCH_MODELS` and a typed id (decision 15);
- the live agent card showing the model.

**First, measure** (against a real `opencode serve`, v2.0.32+6ec5682, in a
scratch home with an OpenRouter provider configured, as #486 did for the
send):
- `POST /api/session {id, location, model: {providerID: "openrouter", id}}`
  creates a session whose first turn runs on that model (read back from the
  assistant message's `model`, [opencode-first-class.md](opencode-first-class.md),
  "Model, tokens and cost");
- the provider id the host's configuration gives OpenRouter, and how
  `GET /api/model` lists its models, including right after a service restart
  (the "snapshot may precede plugin settlement" case);
- that the shared service runs the session's tools in its
  `location.directory`, the worktree. If it does not, opencode spawning does
  not ship ([3.6](#36-the-commands-and-prompt-delivery));
- that `muster-mirror opencode -s <id>` on a session the host just created
  binds the pane (`SourceLauncher`) on the first listing after it starts.

**Tests:**
- **Host, against the fake opencode server** (`internal/opencodehost/opencodetest`).
  The fake gains: the `model` field of `POST /api/session` recorded per
  session; a `GET /api/model` answer set by the test; and `DELETE
  /api/session/{id}`. Each is its own case:
  - the create body carries `{providerID: "openrouter", id: "<vendor>/<model>"}`
    exactly, for a model id containing a second `/` (`a/b/c` stays
    `providerID: "a"`, `id: "b/c"`, as `Model.Ref.parse` reads it);
  - a model the fake's list lacks is refused `bad_model`, and **no session is
    created and no window opened** (the fake's create count is 0, with the
    listed-model case as the positive control: 1);
  - a list with no `openrouter` models at all is refused `unavailable`,
    detail `opencode's model list is not ready`, not `bad_model`;
  - **the prompt is posted after the bind, never before**: the fake records
    the order of create, the bind (the launcher record appearing) and the
    prompt post. Mutation check: post before waiting for the bind, and this
    case goes red on the order;
  - the prompt arrives byte-identical, through the API, for the same cases as
    phase 3 (`;`, `tail;`, `\;`, newlines, quotes, `$(id)`, a leading `-`,
    8 KiB). The window's argv holds no prompt word (the stub records argv);
  - a post whose answer is lost (`DelayPromptAnswers` past the client
    timeout) is retried once with the **same** message id, and the fake
    holds exactly one user message;
  - a spawn that fails before the post deletes the session it created (the
    fake's session count returns to its start) and removes the worktree;
  - a post refused with 400 reports `failed`, detail `prompt not accepted`,
    and the window is kept;
  - the launcher's version mismatch (`SetVersion`) is refused `unavailable`
    before any session is created.
- **Forwarding**: a chief-forward row to a spawned opencode window is
  delivered once through `gatedSend`; it is held while a permission or
  question ask is pending and while the session is busy; a row whose
  `dispatch` differs from the window's tag is refused by the host before
  `gatedSend` runs (the fake's prompt count stays 0). Mutation check: move the
  tag check after `gatedSend`, and the mismatched case goes red with a
  delivered message.
- **Server (PG suite)**: `model` is required for `host/<host>/opencode`
  (or held for the card), refused for `host/<host>/claude` and
  `pod/claude-code` with today's sentence, and part of `request_hash`; a
  model outside the shape is refused before anything is reserved; the model
  card offers `Claude (Claude Code)` only when the host offers `claude`, and
  `MUSTER_CHIEF_DISPATCH_MODELS` and the typed id only when it offers
  `opencode`; a malformed list entry stops the server at start, naming it.
- **Browser (e2e), both runtimes.** The fake host binary gains `agent-spawn`:
  - `cmd/muster-mirror/e2emirror.go` (the fake tmux) answers an `agent-spawn`
    for `claude` by adding a window in an `agents` session, tagged with the
    dispatch, whose pane is a fake Claude pane (the existing
    `MUSTER_MIRROR_E2E_CLAUDE` transcript mechanism) with the delivered
    prompt as its first prompt record;
  - `cmd/muster-mirror/e2eopencode.go` answers one for `opencode` by creating
    the session on its fake server with the model, binding the window through
    its existing `bind` op, and posting the prompt to the fake, which records
    it;
  - a spec drives `agent_dispatch` over the chief's MCP route (no model in the
    loop), with a held `host/?/opencode` call, taps the host card in the
    Attention view, and asserts: the window appears in the sidebar with the
    chief chip; the live agent card shows `running` and the model; the fake
    recorded exactly one prompt, byte-identical. The same spec with no
    spawnable host asserts that **no** card is drawn (the positive control is
    the first case's card).

**Acceptance:**
- **Live:** "start a mirrored agent with `<model>` to fix the typo in the
  README of `<owner>/<repo>`" → the host card → a tap → an `oc-<n>-…` window
  on that host, running `<model>` through OpenRouter, with the prompt as its
  first message;
- "start a mirrored agent on `<host>`", naming no model → the model card,
  offering Claude, the configured list, and a typed id;
- **amended (decision 3):** it runs **without asking** before it uses tools,
  inside the owner's opencode deny rules and guard plugin; the acceptance is
  that its tool use is NOT held for an ask. An ask it does raise (a question,
  or a rule of the owner's that asks) appears in muster's Attention view
  (#416), and an answer from there continues the session. Claude Code still
  asks;
- a routed task comment reaches it through the API.

#### As built (phase 4)

Where the build differs from the plan above, and what it leaves for later:

- **The host checks everything about opencode before any git call**, not after
  the worktree: `-opencode-send` and the token (before the rate limit, with the
  request's own fields), then the service (`GET /api/info`), the TUI's version
  (`opencode --version` on the tmux server's PATH; **fail closed** when it cannot
  be read, unlike the launcher, which starts anyway), the launcher binary, and
  the model against `GET /api/model`. A refusal there leaves nothing to undo.
  The version rule is one function now (`opencodehost.NormalizeVersion`), shared
  with the launcher.
- **The first prompt's message id** is `mirror.OpenCodeMessageID` of
  `agent-spawn/<dispatch>/<session>`: derived from the dispatch and the session
  the host created, not from the dispatch row's creation time (the host does
  not know it). A lost answer is retried once with the same id.
- **The bind** is read from the host's own listing (`WindowLister`): the
  window's pane `Send` with `Gen` equal to the session, within 20 s. The host
  marks its session list stale when it creates a session, so the bind is not
  delayed by the list's 30 s cache.
- **`MUSTER_CHIEF_DISPATCH_MODELS=none`** lists no model; an empty or unset
  value is the default list (the server's environment reader cannot tell an
  empty value from an unset one). At most 8 entries (the card holds Claude, the
  list and the typed id, 10 at most).
- **The model card's labels** are `Claude (Claude Code)`, `<id> (opencode,
  OpenRouter)` and `another OpenRouter model (type its id)`. A typed id binds
  only on a model card that offered that option, and only in the model's shape;
  **tapping** the typed-id option is refused with its own sentence (no id came
  with it). A refusal for the wrong completion names the call to make
  (`host/<host>/opencode with model <id>`).
- **A model with the runtime blank** (`host/<host>/?` plus `model`) is refused:
  the card asks for both.
- **The live card** says `opencode · <id> (OpenRouter)`.
- **Round-1 audit fixes.** The window runs `<muster-mirror> opencode
  --opencode-bin <abs> -s <id>`: the launcher execs exactly the opencode binary
  whose version the host checked, and with that flag it **refuses** a TUI whose
  version it cannot read or that differs, or a service it cannot reach (a
  person's own start still goes on). A model the chief sends with a host tap of
  `host/?/?` is dropped from the re-filed hold (the model card chooses it), so
  the card stays completable. A definite refusal of the first prompt (400, 404,
  409, 401) is not retried; two posts with no answer are `failed` with the
  detail `prompt not confirmed: opencode did not answer, so it may have been
  delivered` (the wire has no unconfirmed spawn result). A model the host's
  opencode lists with `enabled: false` is not a model it offers.
- **Not built in phase 4:** the e2e browser spec (both runtimes); it was not
  built in phase 3 either. The host and server halves are covered by the tmux
  and PG suites below, and the live acceptance is the owner's.

### Rollout

Each phase ships off. Nothing a host does changes until the owner turns it on,
on that host.

1. **Server first.** A server with phase 3 or 4 lists no host target until a
   host's hello advertises `agentSpawn`, and an older host never does. So the
   server can ship any time; the chief's answers are unchanged until a host
   opts in.
2. **The host binary second.** The owner's host configuration (the flake that
   pins `muster-mirror` on each host) moves its pin to the release that holds
   the phase. With `-agent-spawn` unset, which is the default, the new binary
   behaves exactly as the old one: the hello carries no `agentSpawn`.
3. **Opt in, one host, one repository.** On one host's **default** mirror unit
   only: `-agent-spawn`, plus `-agent-spawn-repos` naming one repository.
   Run the phase's live acceptance there. For phase 4 the unit also needs
   `-opencode-send` (from #486) and opencode's OpenRouter provider configured,
   which the hosts already have.
4. **Widen** the allow-list, then the hosts, one change at a time, each
   through the host configuration and its own unit restart.

**Rolling back** is the same switch in reverse, and needs no server change:
drop `-agent-spawn` (or empty `-agent-spawn-repos`) and restart the unit. A
spawned window that is still running is unaffected and keeps its worktree; the
server ends its dispatch through the usual listing path once it closes
([5.4](#54-kill-switches)).

---

## 7. What is still UNVERIFIED

What remains are measurements, each scheduled as the first item of its phase,
with the branch to take already written. #486 settled the three opencode items
an earlier draft listed here (the session-id format, the send route, and
whether `--prompt` submits, which no longer matters because the prompt goes
through the API).

| claim | where | measured in |
|---|---|---|
| ~~`projects[<path>].hasTrustDialogAccepted` alone suppresses the dialog, and how the path is spelled~~ **MEASURED** (Claude Code 2.1.289, real binary, no stubs, scratch `HOME` and `CLAUDE_CONFIG_DIR`, on a dedicated `tmux -L` server, with onboarding marked done): the key **alone** suppresses it (prompt reached, no dialog); with no entry the dialog appears (the control). The key must be the **real path, with no trailing slash**: a trailing slash shows the dialog; a key spelled through a symlink shows it whether claude is started through the symlink or the real path; a real-path key with claude started through a symlink passes, because claude resolves its cwd. **A trusted parent directory does not cover a child** (the dialog appears) | [3.6.1](#361-claude-codes-folder-trust-entry) | phase 3 |
| ~~the file is `$CLAUDE_CONFIG_DIR/.claude.json` when `CLAUDE_CONFIG_DIR` is set~~ **MEASURED**, same setup: with `CLAUDE_CONFIG_DIR` set, claude wrote `$CLAUDE_CONFIG_DIR/.claude.json` (plus `backups/`, `sessions/`) and nothing under `$HOME` | [3.6.1](#361-claude-codes-folder-trust-entry) | phase 3 |
| ~~claude treats the word after `--` as the prompt, not a subcommand~~ **MEASURED FALSE**: `claude --session-id <uuid> -- update` **ran the `update` subcommand** and installed a new native build (into the scratch `HOME`). So `--` is **not** a guard against the subcommand hazard; **only the fixed preface is**, and it must stay load-bearing and tested (a prompt that does not start with it must be refused on the host, not merely never sent). A non-subcommand prompt after `--`, with `;`, `$(id)` and both quote kinds, arrived byte-identical as the first user message | [3.6](#36-the-commands-and-prompt-delivery) | phase 3 |
| **MEASURED:** `--session-id <uuid>` is written to `<config>/sessions/<pid>.json` as `sessionId`, which is the mirror's second lookup (before the transcript guess), and names the transcript file `<uuid>.jsonl`. The mirror's own code path reading it was not run | [3.6](#36-the-commands-and-prompt-delivery) | phase 3 |
| **MEASURED:** the spawn's own time. On two of the owner's repositories (about 2.5k and 3.2k tracked files, neither with `.gitmodules`), from scratch clones: a cold full clone took 2.5 s and 2.9 s, a warm `fetch --no-tags origin <base>` 0.7 s each, and `worktree add` 0.1-0.2 s. That is far below half the 120 s deadline, so the three deadlines stay. Submodule time was not measurable: neither repository has submodules | [3.7](#37-linking-the-spawned-window-to-the-dispatch-and-the-task) | phase 3 |
| ~~PR A's `ask_owner` has the four properties [2.8](#28-mirrored-dispatch-choosing-host-runtime-and-model) relies on~~ **CHECKED: 1, 2 and 4 did not hold; the stored-card change makes all four hold** (table in [2.8](#28-mirrored-dispatch-choosing-host-runtime-and-model)) | [2.8](#28-mirrored-dispatch-choosing-host-runtime-and-model) | phase 3, before building |
| ~~`POST /api/session` with `model` runs the session's turns on that OpenRouter model~~ **MEASURED** (opencode v2.0.32+6ec5682, a real `opencode serve --service` on its own port, scratch `HOME`/XDG dirs, the OpenRouter key passed as `OPENROUTER_API_KEY`, on a dedicated `tmux -L` server): a session created with `model: {providerID: "openrouter", id: "deepseek/deepseek-v4.1-flash"}` answered with every assistant message's `model` naming exactly that; the control, a second session on `qwen/qwen3-coder`, named qwen. **opencode does not check the model**: a create on `nope/not-a-model` answered 200 with that model. So the host's check against `GET /api/model` is the only bound on the model, and the spawn makes it before anything is created | [3.6](#36-the-commands-and-prompt-delivery) | phase 4 |
| ~~the provider id the hosts' configuration uses; how `GET /api/model` lists OpenRouter models, including just after a service restart~~ **MEASURED**: OpenRouter's models are listed under `providerID: "openrouter"`, `id: "<vendor>/<model>"` (365 of 376 entries on the scratch service; a read-only `GET /api/model` on one of the owner's hosts gave the same provider id and count). After a restart, the **first** answer the service gave (about 0.7-0.9 s after start, three restarts) already listed all 365: the "snapshot may precede plugin settlement" case was not seen. **A service with no OpenRouter credential lists no `openrouter` model at all**, so "none listed" cannot tell "not ready" from "no key": the host refuses it `unavailable`, detail `opencode's model list is not ready (it lists no OpenRouter model)`, never `bad_model` | [3.6](#36-the-commands-and-prompt-delivery) | phase 4 |
| ~~the shared service runs a session's tools in its `location.directory`~~ **MEASURED, holds**: with the service's own cwd the scratch home, a session in directory A ran its shell tool (`pwd > where.txt`) in A and wrote the file there; the control, a session in directory B, did so in B. ⚠ With no opencode configuration at all, opencode ran that shell command **without asking** (no permission request was raised): "normal permission mode" for opencode is whatever the owner's opencode configuration and the repository's `opencode.json` say ([5.2](#52-argv-and-prompt-injection)) | [3.6](#36-the-commands-and-prompt-delivery) | phase 4 |
| ~~`muster-mirror opencode -s <id>` binds a pane on a session the host created a moment before~~ **MEASURED, holds**: a long-running source (the daemon's code, warm, stream open), then `POST /api/session` and a tmux window on `muster-mirror opencode -s <id>`: the first listing (+45 ms) still showed the pane's shell; the **second** (+340 ms) showed it bound (`SourceLauncher`, `Send`, the session) and idle. With the event stream not yet open the bind still came on the second listing, with no status (so the spawn's own prompt, posted directly, does not depend on it; a later forward's idle gate does) | [3.6](#36-the-commands-and-prompt-delivery) | phase 4 |
| `DELETE /api/session/{id}` | [3.6](#36-the-commands-and-prompt-delivery) | **MEASURED**: 204, then 404 `SessionNotFoundError` for the same id again |

**Read in source, not measured:** at the pinned `6ec5682`, the opencode TUI
has no `-m/--model` flag; only `opencode run` and `opencode mini` have one
(`packages/cli/src/commands/commands.ts:38-58, 366-370, 398-402`). That is why
the model goes in the session create.
