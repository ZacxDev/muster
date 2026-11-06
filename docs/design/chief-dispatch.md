# Design: the chief dispatches every agent

**Status: proposed. Nothing here is implemented.** The owner made the nine
decisions in [Owner decisions](#owner-decisions): four up front, and five more in
answer to this page's first draft. Everything else on this page is the
recommended design. No owner question remains open. What is still open is a set
of measurements, listed in [7](#7-what-is-still-unverified).

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
    whose mirror advertises spawning.
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
  - starts plain `claude` or `opencode` in their normal permission modes, with
    the prompt handed over in a `0600` file that a constant wrapper reads, so
    neither tmux nor a shell ever parses it;
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
  3. host spawn for claude, with the trust entry and forwarding;
  4. host spawn for opencode, with forwarding through its API.

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
   - any other model the owner names → `pod/gateway`, with that model
     (an OpenRouter model id);
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
   - Claude and opencode start in their **normal** permission modes, so their
     prompts reach the owner as asks. That path has already shipped: Claude's
     waiting state, and opencode's question and permission asks (#416).
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
| `model` | no | **only** for `pod/gateway`, which takes a model today (`buildRuntimeConfig`, `internal/agentspec/agentspec.go:1078`). Refused for every other target ("this target takes its model from its own configuration"): claude-code has no per-agent model (`claudeCodeNoModel`, `internal/api/agents.go:933`), and the host runtimes take theirs from the owner's own settings |
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
      {"target": "pod/gateway", "label": "Gateway (OpenRouter model: <model>)", "when": "your owner named any other model; pass it as model (an OpenRouter model id)"}
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
on muster. Do not start another."*

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
  PATH when the mirror starts. The hub stores the list only for a v2 host with
  `interactive` set, as `hub.go:717` does for `resumeSpawn`. Nothing else is
  advertised (D6): there is no separate offer message and no repository list.
- **`agent-spawn`** (muster → host): `{id, dispatch, runtime, repo, base,
  prompt, name}`.
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

**Refusal codes.** Four codes cover everything the host can refuse. Each code
names the one thing the chief can do about it. `detail` is a fixed sentence
for the owner and the logs; the chief acts only on the code.

| code | covers | the chief's sentence, and what it can do |
|---|---|---|
| `unavailable` | the host's own preconditions: no `-agent-spawn`, no `-interactive`, not the default tmux server, no tmux server running, the runtime binary not found | `"host %q cannot start %s agents now: <detail>"`. Offer your owner another target |
| `no_repo` | no clone of `<owner>/<repo>` under the host's roots, or more than one | `"repo %s has no single clone on host %q: <detail>"`. Ask your owner which repository, or which host |
| `bad_base` | `base` is not a valid branch name, or does not exist on the remote | `"branch %q does not exist on %s's remote"`. Ask your owner which branch |
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

The host has **no spawn cap of its own** (D3). The server's budget, 5 live and
10 a day, is the only limit on spawns ([5.3](#53-caps)).

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
     They are the two wrapper scripts, the flags (`--session-id`, `--`,
     `--session`), and tmux's own options (`-d`, `-P`, `-F '#{window_id}'`,
     `-n`, `-c`, `-t`). The rule for them is that **none may end in `;`**. A
     unit test enumerates every constant word the spawn uses and asserts that.
     The wrapper scripts contain `;` inside them, but each ends in `"$p"` or
     `"--prompt=$p"`.
3. A **constant** wrapper script reads the file, deletes it, and execs the
   runtime with the prompt as one final argument. The prompt reaches the shell
   only as the value of a variable inside double quotes, so the shell never
   parses it.

The two wrapper scripts are constants in the source, one per runtime. Each is
passed as the `-c` argument:

```sh
# claude
p=$(cat -- "$1" && printf x) || exit 97; p=${p%x}; rm -f -- "$1"; shift; exec "$@" "$p"
# opencode
p=$(cat -- "$1" && printf x) || exit 97; p=${p%x}; rm -f -- "$1"; shift; exec "$@" "--prompt=$p"
```

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
  - `--`. Whether Claude's parser treats the word after `--` as the prompt is
    **UNVERIFIED**, and phase 3 measures it.
  - Structurally, **muster's prompt always begins with its own fixed
    preface**, `muster: your owner's chief started you …`. That is never a
    subcommand name and never starts with `-`, so this guard holds whatever
    `--` does.

**opencode:**

```text
<shell> -l -c <opencode wrapper> <shell> <prompt-file> <opencode-abs> --session <id>
```

The wrapper appends `--prompt=<prompt>` as one argument. The working directory
is the worktree, set by tmux's `-c`, so the positional `[<directory>]` is not
passed.

- `--session <id>`: "Session ID to continue, or to create if it does not
  exist" (`opencode --help`, v2.0.32). The host mints the id, so the mirror
  links the window **deterministically**. That is the "launcher record that
  names the session deterministically" which the title heuristic says a send
  needs (`internal/opencodehost/title.go:10-34`).
  - **UNVERIFIED:** which id formats opencode accepts for a session that does
    not exist yet.
  - Phase 4's first item measures it. The fallback is to omit `--session`,
    link through the pane title against the server's newest root session
    created after the spawn, and say so.
- `--prompt=<prompt>` is one word, so a prompt can never be taken for a flag.
  - **UNVERIFIED:** whether it submits on launch or only fills the input
    (host-sessions noted the same).
  - If it only fills the input, the prompt goes through opencode's own HTTP
    API instead, which the mirror already holds a token for. It uses the
    session's send route, through the same `openCodeSend` that forwarding uses
    ([3.10](#310-forwarding-into-a-host-spawned-agent)).
    - **UNVERIFIED:** that route's path and body. Today the mirror only reads
      `GET /api/session/{id}/message` (`internal/opencodehost/client.go:259-268`).
    - **Never through keystrokes.** The host refuses to paste into opencode
      (`docs/mirroring.md`, "muster does not send to opencode yet").
- **The shared background service.** Bare `opencode` attaches to a running
  `opencode serve --service` if one exists, and its tools then run in that
  service's environment (host-sessions, "The opencode profile"). The spawn
  keeps that default, to match a hand-started opencode.
  - Whether the service then runs the session's tools in the worktree is
    **UNVERIFIED**, and is measured in phase 4 before shipping.
  - If it does not, the spawn adds `--standalone`.

**Why a file read by a constant wrapper, and not an argv word or a paste.**

| delivery | tmux parses it | shell parses it | verdict |
|---|---|---|---|
| **a `0600` file, read by the constant wrapper** | **no** | **no**: it is a quoted variable's value | **recommended** |
| one argv word to tmux | **yes**: a trailing `;` ends the tmux command | no | ✗ |
| an `-e VAR=…` environment value | **yes**: it is a tmux argument too | no | ✗, for the same reason |
| a paste through the send queue, after `idle` | no | no | ✗ for claude, because it races the CLI's startup; impossible for opencode, because the host refuses pastes into it |
| opencode's HTTP API | no | no | the fallback for opencode only |

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

**A new table, `host_dispatches`** (migration `0073_host_dispatches.sql`),
under row-level security on `owner_id`:

| column | type | |
|---|---|---|
| `id` | bigserial | the `dispatch` on the wire, in `@muster-dispatch`, and in the worktree path |
| `owner_id` | bigint | RLS |
| `host` | text | |
| `runtime` | text | `claude` or `opencode` |
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
  migration 0073 gives `host_dispatches` a column `orphan_notice_id bigint
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
- the chief's `live_agents` names it as `{host, window, task}`.

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

There are none for spawning (D3). The server's budget is the only limit on
spawns. The one limit the host keeps is on forwards
([3.10](#310-forwarding-into-a-host-spawned-agent)).

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

**opencode: its own API, never keystrokes** (phase 4).
- A new wire pair, `opencode-send {id, dispatch, session, text}` /
  `opencode-sent {id}` or `{id, error}`.
- The host checks, in order:
  - the window's `@muster-dispatch` equals `dispatch`;
  - the pane's opencode session is `session`;
  - the session has **no pending ask**: `GET /api/session/{id}/permission` and
    `/form` both return nothing (`docs/design/opencode.md`, "Asks"), for the
    same reason as `waiting` above;
  - the session is not active (`/api/session/active`,
    `internal/opencodehost/client.go`).
- Then it posts the text to the session's send route with the token the mirror
  already holds. **UNVERIFIED:** that route's path and body at the pinned
  opencode commit. Today the mirror only *reads* `GET
  /api/session/{id}/message` (`client.go:259-268`). The route is read from
  upstream before phase 4, and it is the same call that 3.6's prompt fallback
  needs, so there is one implementation, `openCodeSend`.
- The host's paste guard refusing opencode panes stays as it is. This path
  does not go near it.

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
| server | the host is the owner's, connected, v2, `interactive`, and its hello lists the runtime (`hostSpawnable`) |
| server | **`connectedRepo(repo)`** (`task_routing.go:926-944`) passes, for **every** host target, before anything is reserved. This is the same check, and the same refusal sentences, a pod gets |
| server | the caps hold |
| host | `-interactive`, `-agent-spawn`, and the default tmux server |
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
  wrapper scripts, one per runtime ([3.6](#36-the-commands-and-prompt-delivery)).
  Each reads the prompt from its `0600` file into a variable, deletes the
  file, and execs the runtime with that variable, in double quotes, as the
  last argument. The shell never parses the prompt.
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
  For opencode, the prompt is one word, `--prompt=<prompt>`.
- **Prompt injection into the agent** is the real surface. The chief reads
  outside text (plugin messages, task bodies), and its prompt becomes the
  agent's first message. The bounds:
  - **Who:** a host spawn only ever runs in a turn the owner typed, so outside
    text alone cannot trigger one ([2.5](#25-gating-when-there-is-no-task-to-hang-it-on)).
  - **How it is framed:** the prompt is fenced as relayed by the chief, and
    muster's rules close the message and win (draft PRs only; never mark a
    task complete; text from outside is data).
  - **What it can do:** the agent starts in **normal permission mode**, so
    every tool use that the settings in force do not already allow becomes an
    ask on muster. That is a mitigation, not a boundary. The settings in force
    are the owner's own, plus the repository's: `.claude/settings.json` for
    claude, `opencode.json` for opencode. Either can allow more. The
    connected-repository rule above limits which repositories can do that.

### 5.3 Caps

**The server's budget is the only spawn limit** (D3): the chief's 5 live and
10 per day. Host spawns count against both, and an offline host's spawns stay
counted. The host keeps no spawn cap of its own.

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

## 6. Phases

The order is changed from the owner's list: **the UI deletion moves to phase
2.** The reasons:

- **It depends only on phase 1.** The form never started anything on a host,
  so deleting it after phase 1 takes nothing away that the chief cannot do.
- **The form is the uncapped path.** Until it goes, two dispatch paths exist,
  with different guards (no caps, no budget, `connectedRepo` only for some
  kinds). Removing it early leaves one predicate sooner.
- **Host spawn is the longest and riskiest work.** It has several UNVERIFIED
  items (the trust entry's exact effect, and opencode's `--session`,
  `--prompt` and send route). It also copies a pattern, `-resume-spawn`, that
  has never run on the owner's hosts. The deletion should not wait behind it.

If the owner wants the form kept until host spawning works, the original order
still works: every phase ships on its own.

### Phase 1: `agent_dispatch` for pods, with or without a task, the required target, `worker_pool` and `request_key`

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

### Phase 3: host spawn for claude, with the trust entry and forwarding

**What ships:**
- `-agent-spawn` and its sibling flags;
- `hello.agentSpawn`, `agent-spawn`, `agent-spawned`;
- worktrees, the `agents` session, `@muster-dispatch`;
- **`trustSpawnedWorktree`** ([3.6.1](#361-claude-codes-folder-trust-entry));
- the prompt file and the two constant wrappers ([3.6](#36-the-commands-and-prompt-delivery));
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
- `host_dispatches` (migration 0073);
- `Hub.SpawnAgent`;
- the `host/<host>/claude` target;
- the chip on the window page;
- the cleanup sweep;
- **forwarding to claude windows**: `forwardTarget` over `liveOnTask`; the
  chief-forward send-queue row with `sendQueueChiefForwardGate`; `PasteWindow`
  with `ExpectTurn` and `mirror.ForDispatch`; the host's tag check; and the
  12-per-hour limit
  ([3.10](#310-forwarding-into-a-host-spawned-agent)).

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
    sentence. The server's budget is the only spawn limit (D3);
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

### Phase 4: host spawn for opencode, with forwarding through its API

**What ships:**
- the `host/<host>/opencode` target;
- the opencode argv;
- the session-id link;
- the prompt delivery decided by the measurement;
- `openCodeSend`;
- the `opencode-send` wire pair, and forwarding to opencode windows
  ([3.10](#310-forwarding-into-a-host-spawned-agent)).

**First, measure:**
- `--session <id>` with a host-minted id for a session that does not exist;
- whether `--prompt=<prompt>` submits;
- whether the shared service runs the session's tools in `<worktree>`;
- **the send route's path and body**, read from upstream at the pinned
  commit, then exercised against a real `opencode serve`, as
  `docs/design/opencode.md` did for asks.

Each answer picks a branch already written in
[3.6](#36-the-commands-and-prompt-delivery).

**Tests:**
- The opencode prompt arrives verbatim, as one word `--prompt=<prompt>`,
  through the file and the wrapper, with the same cases as phase 3 (`;`,
  `tail;`, `\;`, newlines, quotes, a leading `-`).
- A host target on opencode with an unconnected `repo` is refused by
  `connectedRepo` ([5.1](#51-who-can-trigger-a-spawn)). opencode has no trust
  step, so this check is the only bound on the repository's own
  `opencode.json`.
- The window's `agentSession` equals the minted id.
- `openCodeSend`, against the recorded fixture server:
  - it delivers once;
  - it refuses while a permission or form ask is pending, and while the
    session is active;
  - it refuses a window whose tag differs;
  - an HTTP failure reports `failed` for a spawn (detail `prompt not
    accepted`), with the window kept,
    or a "not delivered" sentence for a forward.
- **Mutation check:** drop the pending-ask check, and the pending-permission
  case goes red with a delivered message.

**Acceptance:**
- **Live:** the same click path as phase 3, with opencode.
- Its permission ask appears in muster's Attention view (#416), and an answer
  from there continues the session.
- A routed task comment reaches it through the API.

---

## 7. What is still UNVERIFIED

No owner question is open. The owner answered the first draft's five as
decisions 5-9. What remains are measurements, each scheduled as the first item
of its phase, with the branch to take already written:

| claim | where | measured in |
|---|---|---|
| `projects[<path>].hasTrustDialogAccepted` alone suppresses the dialog, and how the path is spelled | [3.6.1](#361-claude-codes-folder-trust-entry) | phase 3 |
| the file is `$CLAUDE_CONFIG_DIR/.claude.json` when `CLAUDE_CONFIG_DIR` is set | [3.6.1](#361-claude-codes-folder-trust-entry) | phase 3 |
| claude treats the word after `--` as the prompt, not a subcommand (the fixed preface holds either way) | [3.6](#36-the-commands-and-prompt-delivery) | phase 3 |
| opencode's `--session <id>` id format; whether `--prompt=<prompt>` is accepted and submits; the shared service's working directory | [3.6](#36-the-commands-and-prompt-delivery) | phase 4 |
| opencode's session send route, its path and body | [3.10](#310-forwarding-into-a-host-spawned-agent) | phase 4 |
