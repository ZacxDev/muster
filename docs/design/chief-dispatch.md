# Design: the chief dispatches every agent

**Status: proposed. Nothing here is implemented.** The owner made the four
decisions in [Owner decisions](#owner-decisions). Everything else on this page is
the recommended design, and the questions it could not settle are listed in
[Open questions](#7-open-questions).

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
  - A read-only companion tool, `agent_targets`, lists the targets the chief can
    use right now, together with the chief's budget.
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
  For these two cases it is mandatory, so a plugin message, a task event, or a
  turn the chief started itself can never start an agent with no task or a
  process on a host.
- **The host spawn protocol** adds a new mirror flag, `-agent-spawn`, which is
  off by default and needs `-interactive`. It copies the pattern
  `-resume-spawn` already uses: a capability in `hello`, a request/reply pair on
  the hub's id space, and an argv built by the host behind the constant
  `exec "$@"` login-shell wrapper. The host:
  - creates a **fresh git worktree** of the named repository from
    `origin/<base>`, never touching any checkout the owner has open;
  - opens a **window in the `agents` session** on its own tmux server;
  - starts plain `claude` or `opencode` in their normal permission modes, with
    the prompt passed **as one argv word**, so no shell ever sees it;
  - picks the session id itself, so muster can link the mirrored window to the
    dispatch record without guessing.
- **The UI deletion** removes the sidebar **+**, the home page's
  "Dispatch an agent" button, the dispatch form (both its task-less and its
  task-card forms), "Save for later", `GET /ui/agents/new`,
  `GET /ui/agents/notes` and `POST /agents`. There are **no draft data to
  migrate**: "Save for later" creates an ordinary agent row with status
  `stopped`, and that row stays startable from the agent's page.
- **Phases.** The order is changed from the owner's list:
  1. chief pod dispatch, with or without a task, and the required target;
  2. **delete the UI**;
  3. host spawn for claude;
  4. host spawn for opencode.

  The reason is in [Phases](#6-phases).

## Owner decisions

These are binding, and this page does not reopen them.

1. **Clarify.** The dispatch tool takes a required `target` with no default. A
   target is either a cluster pod (one of today's pod runtimes), or a tmux host
   plus a runtime of `claude` or `opencode`. The tool refuses a call without a
   target, so the chief has to ask. The refusal is structural, not a prompt
   instruction.
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
can create an agent in a pool"). See [Open questions](#7-open-questions), Q1.

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
- `kindChoices` (`internal/api/agents.go:296-310`). It is reused by
  `agent_targets`.
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
| `target` | **yes** | see [targets](#23-targets). No default, no enum fallback |
| `prompt` | yes without `task_id`; optional with one | the work. With a task it plays today's `instructions` role (below) |
| `task_id` | no | an existing task of the owner's. Without it, the dispatch has no task |
| `repo` | yes for a host target; otherwise as today | `<owner>/<repo>`. For a pod it is checked by `connectedRepo`, as today. For a host it must also be in that host's latest offer |
| `base` | no | for a host: the branch the worktree is cut from. The default is the remote's default branch (`origin/HEAD`). For a pod: today's `repo_branch` |
| `model` | no | **only** for `pod/gateway`, which takes a model today (`buildRuntimeConfig`, `internal/agentspec/agentspec.go:1078`). Refused for every other target ("this target takes its model from its own configuration"): claude-code has no per-agent model (`claudeCodeNoModel`, `internal/api/agents.go:933`), and the host runtimes take theirs from the owner's own settings |
| `owner_asked` | see [gating](#25-gating-when-there-is-no-task-to-hang-it-on) | meaning unchanged; **required** for a dispatch with no task and for every host target |

Unknown fields are refused (`"arguments are {target, prompt?, task_id?, repo?,
base?, model?, owner_asked?}"`), as today's argument refusal does (`:293-294`).

**The prompt.**
- It is cleaned the way `cleanInstructions` cleans text today (`:174-191`):
  every control, format and invisible code point is removed, line breaks are
  kept, and the limit is checked on the input as given.
- The limit rises from 1 KiB to **8 KiB**, measured in bytes on that input.
  That is still far below Linux's per-argument limit of 128 KiB
  (`MAX_ARG_STRLEN`), which matters because a host spawn delivers the prompt
  as one argv word ([3.6](#36-the-commands-and-prompt-delivery)).
- **Who may write it:**
  - **With a task, and without `owner_asked`:** today's rule holds. The prompt
    must be the owner's verbatim answer to an attention ask about this task
    (`ownerAnswer`).
  - **With `owner_asked`**, which is always the case without a task: the
    prompt is the chief's relay of the owner's message, written in the owner's
    turn. It is fenced and marked as relayed by the chief
    (`renderChiefInstructions`, `:200-215`), and muster's run rules close the
    message and take precedence (`chiefDispatchRules` `:142-145`). With no task,
    the rule about commenting on the task becomes "report back in the chief's
    chat with `chief_ask`, or by the pull request".

### 2.3 Targets

A target is a string, checked against what `agent_targets` would list **at the
moment of the call**:

| target | starts | needs |
|---|---|---|
| `pod/claude-code` | a Claude Code pod, as today | the kind enabled (`MUSTER_AGENT_KINDS`), the owner's Claude account pool |
| `pod/gateway` | a gateway pod, as today | the kind enabled |
| `host/<host>/claude` | `claude` in a new window of `agents` on `<host>` | the host online, its hello advertising `agentSpawn` with `claude`, `-interactive` on |
| `host/<host>/opencode` | `opencode` the same way | the same, with `opencode` |

**Why a string and not an object.** A string is one field, so a call cannot
fill half of an object. Its whole grammar is two regular expressions:

- `^pod/(claude-code|gateway)$`
- `^host/(<host name>)/(claude|opencode)$`

The host-name part uses the hub's existing host-name validation.

**Why the listing carries no enum.** The set of hosts changes minute to
minute, and an enum written into the listing goes stale. The chief would also
remember it, which is exactly the failure that led to taking cap numbers out of
the description (`:95-101`).

**Refusals.** Each is its own sentence, so a test and the chief can tell them
apart:

- **No target:** `"target is required and has no default: ask your owner where
  to run this (a cluster pod, or a host and claude or opencode). Never choose
  one they did not name. agent_targets lists what exists now."`
- `"target %q is not one of: pod/claude-code, pod/gateway, host/<host>/claude,
  host/<host>/opencode"`
- `"pod kind %q is not enabled on this deployment (MUSTER_AGENT_KINDS)"`. This
  is today's `resolveKind` sentence.
- `"host %q is not connected now; nothing was queued"`. A host spawn is never
  queued: an intent from an hour ago is not an intent now (host-sessions
  "Lifecycle").
- `"host %q does not start agents (its muster-mirror needs -interactive and
  -agent-spawn)"`
- `"host %q does not offer %s"` (a runtime the host's offer lacks)
- `"repo %s is not on host %q (it offers: …the first 20…)"`
- `"host targets are off on this deployment (MUSTER_CHIEF_HOST_DISPATCH_OFF)"`

### 2.4 Discovery: `agent_targets`

This is a new chief-only, read-only tool. It takes no arguments and returns:

```json
{
  "pods": [
    {"target": "pod/claude-code", "available": true},
    {"target": "pod/gateway", "available": false, "why": "not enabled on this deployment"}
  ],
  "hosts": [
    {"host": "<host>", "online": true,
     "targets": ["host/<host>/claude", "host/<host>/opencode"],
     "repos": ["<owner>/<repo>", "…"], "repos_truncated": false},
    {"host": "<host-2>", "online": false, "why": "offline since 14:02"}
  ],
  "budget": {"live": 1, "live_cap": 5, "today": 3, "daily_cap": 10, "live_agents": ["…"]}
}
```

**Where each part comes from.**
- **Pods:** `kindChoices` (`internal/api/agents.go:296-310`), which today feeds
  only the form being deleted.
- **Hosts:** the hub's `HostView` for the owner, keyed by `(owner, host)`.
  These are the owner's hosts only, so another user's host, or a mirror
  connected to another muster server (another tenant), can never appear.
- **Repos:** the host's latest `agent-spawn-offer` ([3.1](#31-wire-messages-and-capability)),
  capped at 200 entries, with `repos_truncated` set when the host has more.
- **Budget:** the same `ChiefBudget`. `live_agents` gains a `target` field, and
  `task` becomes `0` for a dispatch with no task.

**Use in a refusal.** The refusal for a missing target names this tool but does
not inline the list. The chief needs the list in order to *ask*, and it gets
it with one more call. Inlining the list would invite it to pick.

### 2.5 Gating, when there is no task to hang it on

Today the gate lives on the task. A `gate:` tag holds the agent back, and only
`owner_asked` in the owner's own turn releases a gate the chief filed itself. A
dispatch with no task has no tag to carry that gate.

**Proposal: for a dispatch with no task, and for every host target,
`owner_asked: true` is required and is verified with `ownerTurnFor`, the
release check that already exists.**

- `ownerTurnFor` (`chief_owner_release.go:307`) proves that the turn running on
  the chief is a message **typed by its owner** in the muster chat.
  - It accepts only `authz.ViaSession`. `ownerTypedBy` (`:95-101`) excludes
    API tokens, the hook token, plugins, relays, routes and sweeps.
  - It requires that no other turn of the chief's is running at the same time
    (`chiefReleaseOutsideTurn`, `chiefReleaseOtherCLIBusy`, `:200-209`).
- It is re-checked under the reservation's lock, as the gate release is today
  (`task_routing.go:670-685`).
- The result is that a plugin message, a task event, mail, or a turn the chief
  started itself **can never** start an agent with no task, or start anything
  on a host. Only a pod dispatch on an existing, ungated task still works
  without the owner's turn, exactly as `task_dispatch` works today.

| dispatch | `owner_asked` | the owner's turn is checked |
|---|---|---|
| pod, on an open, ungated task | optional (as today) | no (as today) |
| pod, on a task gated by the chief's `task_create_gated` | required (as today) | yes, and the gate is released (as today) |
| pod, with no task | **required** | **yes** |
| host, on a task or not | **required** | **yes**, plus the step-up rule in [5.1](#51-who-can-trigger-a-spawn) |
| any, on a task gated by the **owner** | — | refused, as today (`chiefReleaseNotChiefsGate`) |

**Refusals.**
- `"a dispatch with no task needs owner_asked: true, in your answer to a
  message your owner typed in your chat; otherwise file it with
  task_create_gated and use task_route_attention"`
- `"a host target needs owner_asked: true, in your answer to a message your
  owner typed in your chat"`
- The owner's-turn refusals that exist today, unchanged (`:193-216`).

### 2.6 Budget and accounting for a dispatch with no task

The ledger stays in `audit_log` with the same three actions, and the same
`chiefBudget` count decides the caps. Only the object changes:

- **On a task:** the object is `note:<id>`, as today. One start per task, a
  release when the owner destroys the agent, and `already_dispatched`: all as
  today.
- **With no task:** the object is `dispatch:<reservation id>`. It is minted
  under the lock, so every such dispatch is its own object. "One start per
  task" has nothing to bind to, and **it does not apply**. A repeated call
  starts a second agent, which costs one more unit from both caps. That is the
  bound. Idempotency for dispatches with no task is not proposed (see
  [Open questions](#7-open-questions), Q4).
- **`ReserveChiefDispatch(ctx, taskID, …)`** becomes
  `ReserveChiefDispatch(ctx, object, …)`. For a `dispatch:` object, the
  prior-start branch (`chief_dispatch.go:231-249`) is skipped, and the
  reservation id is written into the object. The pending-agent query is
  already keyed by object (`:354-360`), so a dispatch with no task holds its
  pending live slot exactly as one on a task does. `noteObjectID` returns `0`
  for it (`:386-392`), and that `0` is what `live_agents[].task` shows.
- **A host spawn counts as live** from its `host_dispatches` row
  ([3.7](#37-linking-the-spawned-window-to-the-dispatch-and-the-task)) while
  its status is `requested`, `spawned` or `unconfirmed`. `chiefBudget` gains a
  third source next to the two it has: rows whose
  `task.chief_dispatch_agent` detail names a `host_dispatch_id` instead of an
  `agent_id`.
  - An offline host's spawns **stay live** (fail-closed) until the hub forgets
    the host, which happens after 24 hours offline (`internal/mirror/hub.go:59`).
    Their status then becomes `host_lost`.
  - The cost of this is a slot held while a laptop sleeps. The alternative
    would let the chief exceed the cap whenever a host disconnects.

**One predicate for "a live agent on this task".** `chiefDispatchTaskBlock`
("already has an agent working it", `task_actions.go:115-136`) and
`agentsOnTask` (`task_routing.go:399-412`) read only the `agents` table.
Each gets one shared helper, `liveOnTask`, that also reads `host_dispatches`.
Otherwise a host-spawned agent on a task would let a pod be started beside it.
`task_forward` to a host-spawned window is **not** in scope: it would be a
mirror send, which carries its own step-up and its own refusal of opencode
(see Q5).

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

These copy `-resume-spawn` ([1.3](#13-the-one-host-side-spawn-that-exists--resume-spawn)).
There is no protocol version bump: an older muster ignores an unknown hello
field, and its read loop has no `default` case, so it also ignores an unknown
message type (host-sessions "The wire"; `internal/mirror/hub.go:800-914`).

- **`hello.agentSpawn: true`**: this host starts agents. The hub stores it only
  as `agentSpawn && interactive` on a v2 host, as `hub.go:717` does for
  `resumeSpawn`.
- **`agent-spawn-offer`** (host → muster, after `welcome` and whenever the
  offer changes): `{runtimes: ["claude","opencode"], repos: ["<owner>/<repo>",
  …], maxLive}`, or `{disabled: "<reason>"}`. This is host-sessions'
  `spawn-offer` without profiles or directory keys.
  - The repos are **derived by the host**: every git clone directly under its
    configured roots whose `origin` URL parses as a GitHub
    `<owner>/<repo>`.
  - The offer has its own message, not a hello field, because the hub reads
    hello only once per connection (`hub.go:650`).
- **`agent-spawn`** (muster → host): `{id, dispatch, runtime, repo, base,
  prompt, name}`.
  - `id` is from the connection's id space (`hc.next`).
  - `dispatch` is the `host_dispatches` row id.
  - `runtime` is `claude` or `opencode`.
  - `repo` must be in the host's own offer.
  - `base` is a branch name; empty means the remote's default branch.
  - `prompt` is already cleaned and wrapped by muster
    ([2.2](#22-parameters)).
  - `name` is the short display label, `#<task> <title>` or the prompt's first
    words. It is used only for the tmux window name, after
    [3.4](#34-the-tmux-agents-session-and-window-names)'s sanitising.
  - Unknown fields are refused.
- **`agent-spawned`** (host → muster):
  - on success: `{id, dispatch, session, window, pane, worktree, branch,
    agentSession}`;
  - on failure: `{id, dispatch, error, refusal: <code>}`.
  - `agentSession` is the runtime's session id, which the host chose
    ([3.6](#36-the-commands-and-prompt-delivery)).
- **`agent-kill`** / **`agent-killed`**: `{id, dispatch}` and `{id, dispatch,
  worktree: "removed"|"kept_dirty"|"kept_unpushed"}`, or an error. This is the
  owner's Kill, from the mirrored window's page, behind step-up
  ([4](#4-the-ui-deletion-plan)).
- **Listing:** each `Window` gains `dispatch` (an int64, omitted when empty),
  read from the window's `@muster-dispatch` option. An older muster ignores
  it.

**Server side.** `Hub.SpawnAgent` copies `Hub.SpawnResume`: the owner-scoped
lookup, an id from `hc.next`, a reply channel and a timeout. The timeout is
**60 s, not 15 s**, because a `git fetch` comes first.

A timeout after the write is **unconfirmed**, not failed, as for a paste
(`internal/mirror/transcript.go:648-683`):
- the row becomes `unconfirmed` and **keeps its live slot**;
- a later listing that carries the window's `dispatch` id settles it to
  `spawned`.

**Refusal codes.** They are fixed strings, and the server maps each to one
sentence for the chief:

| code | meaning |
|---|---|
| `spawn_disabled` | no `-agent-spawn` |
| `not_interactive` | no `-interactive` |
| `not_default_server` | see [3.8](#38-read-only-and-tenant-hosts) |
| `unknown_runtime` | the runtime is not in the offer |
| `unknown_field` | the request carries a field the host does not know |
| `no_repo` | no clone of that repository under the roots |
| `ambiguous_repo` | two or more clones of it under the roots |
| `fetch_failed` | `git fetch` failed |
| `unknown_base` | the branch does not exist on the remote |
| `branch_taken` | the branch name already exists |
| `worktree_failed` | `git worktree add` failed |
| `bad_prompt` | empty, over 8 KiB, or a NUL |
| `max_live` | the host's live cap is reached |
| `limited` | the host's hourly limit is reached |
| `no_tmux_server` | no tmux server is running |
| `exec_not_found` | the runtime binary is not on the tmux server's PATH |
| `tmux_failed` | the tmux call failed |
| `tag_failed` | the `@muster-dispatch` tag could not be set |
| `cwd_mismatch` | the pane did not land in the worktree |
| `not_listed` | the window did not appear in a listing on this connection |
| `prompt_failed` | opencode only, on the API-delivery branch: the prompt was not accepted; the window is kept |

### 3.2 The flag

The flag follows the mirror's naming convention, `-kebab` plus
`MUSTER_MIRROR_<SNAKE>` (`cmd/muster-mirror/main.go:93-115`):

| flag | env | default | meaning |
|---|---|---|---|
| **`-agent-spawn`** | `MUSTER_MIRROR_AGENT_SPAWN` | **false** | let the chief start agents here. It has no effect without `-interactive`, and it logs that it has none, as `-resume-spawn` does (`:380-385`) |
| `-agent-spawn-roots` | `MUSTER_MIRROR_AGENT_SPAWN_ROOTS` | `~/src` | colon-separated directories whose direct children are the clones it may cut worktrees from |
| `-agent-spawn-dir` | `MUSTER_MIRROR_AGENT_SPAWN_DIR` | `$XDG_DATA_HOME/muster-mirror/agents` | where the worktrees go ([3.3](#33-worktrees)) |
| `-agent-spawn-max-live` | `MUSTER_MIRROR_AGENT_SPAWN_MAX_LIVE` | `4` | the host's own live cap, counted from tmux |

The owner's host configuration sets `-agent-spawn` on the owner's default
mirror unit **only**. It never sets it on a `muster-mirror@<tenant>` unit
([multi-tenant.md](multi-tenant.md)).

### 3.3 Worktrees

**Where.** `<agent-spawn-dir>/<owner>__<repo>/<dispatch>`, for example
`~/.local/share/muster-mirror/agents/<owner>__<repo>/417`.
- It is outside every clone and every root. An agent's worktree is then never
  inside a checkout the owner works in, and the owner's own tools that walk a
  checkout (search, file watchers, a build) never see it.
- The dispatch id makes the path unique, and lets anyone see from the path
  which dispatch it belongs to.

**Branch.** `muster/<dispatch>-<slug>`.
- The slug is at most 40 characters of `[a-z0-9-]`, made from the window name.
- An existing branch of that name is refused with `branch_taken`. It is never
  reused or reset.

**Base.** `origin/<base>` after a fetch, where `<base>` is the request's
`base` or, when that is empty, the remote's default branch (`git
symbolic-ref refs/remotes/origin/HEAD`). It is never the clone's local branch
and never its `HEAD`, so a clone that is behind or dirty has no effect on what
the agent starts from.

**Commands.** Each is an argv run through `exec.CommandContext`, never a shell
string. `<clone>` is the clone the offer named, resolved again at spawn time
with `filepath.EvalSymlinks`, and it must still be under a root.

```text
git -C <clone> fetch --no-tags origin <base>
git -C <clone> rev-parse --verify --quiet refs/remotes/origin/<base>^{commit}
git -C <clone> worktree add --no-track -b muster/<dispatch>-<slug> <dir> refs/remotes/origin/<base>
```

- **`base` is validated before it reaches git.** It must pass
  `git check-ref-format --branch`, must not start with `-`, and must have no
  `..` segment. That closes argument injection into `fetch`.
- **The owner's checkout is never touched.** `fetch` writes refs and objects,
  and `worktree add` writes a new directory plus an entry in the clone's
  `.git/worktrees/`. Neither changes the clone's working tree, its index or
  its checked-out branch. **A dirty clone is therefore not a refusal.** The one
  shared write is the branch name, which `branch_taken` guards.
- **What the worktree does not carry.** It gets no submodules, and no
  untracked files such as a gitignored `.envrc` or `.env`. The host
  initialises submodules only when the repository has a `.gitmodules`
  (`git -C <dir> submodule update --init --recursive`). Their failure is a
  warning in the result, not a refusal. The host never copies untracked files
  from the clone: their content is the owner's, possibly credentials, and the
  agent did not ask for them.
- **Concurrency.** Spawns on one host run one at a time, behind a mutex.

**Cleanup.** A worktree is never force-removed.

- **When it is cleaned up.** The window ends: Kill, the owner closing it, or
  the runtime exiting and the pane's shell then exiting. Then
  `git -C <clone> worktree remove <dir>` runs, **without `--force`**, so a
  worktree with uncommitted changes refuses to go and is reported
  `kept_dirty`.
- **Branches.** A branch with commits the remote lacks
  (`git rev-list --count @{push}..HEAD` > 0, or no upstream at all while the
  branch has commits beyond the base) is kept, and so is its worktree, as
  `kept_unpushed`. A branch with no new commits is deleted along with its
  worktree (`git branch -d`, never `-D`).
- **A sweep** runs at mirror start, and every hour after. It applies the same
  rule to every directory under `<agent-spawn-dir>` whose dispatch id names no
  live tagged window. So a host crash leaves no worktree that the sweep cannot
  account for. A kept worktree stays listed in muster on the dispatch row
  until the owner removes it by hand.

### 3.4 The tmux `agents` session and window names

- **Session `agents`**, on the mirror's own tmux server, which for the default
  mirror is the default server ([3.8](#38-read-only-and-tenant-hosts)).
  - If `has-session -t =agents` succeeds, the host uses `new-window -t
    =agents:`.
  - Otherwise it uses `new-session -d -s agents`, and on "duplicate session"
    it retries as `new-window`. This is `resume.go:351-388`, with the session
    name fixed.
  - The tmux server must already be running. If it is not, the spawn is
    refused with `no_tmux_server`, and the host compares the server's pid
    before and after the call (host-sessions "Environment"). This way a mirror
    unit with a bare PATH never starts the owner's tmux server.
- **Window name:** `<cc|oc>-<dispatch>-<slug>`, for example `cc-417-fix-login`.
  - It is set with `-n`, with `automatic-rename off` on that window only
    (`set-option -w -t <window id> automatic-rename off`).
  - The slug is `[a-z0-9-]{0,24}`. That makes the name safe against tmux's
    trailing-`;` command splitting (host-sessions "The command").
- **Identity:** the window option `@muster-dispatch <dispatch>`.
  - It is set by a separate `set-option -w -t <window id>` call after the
    window exists. A failure kills the window and reports `tag_failed`
    (host-sessions measured that tmux command lists are not all-or-nothing).
  - It is a **window** option, not host-sessions' session option, because many
    agents share one `agents` session.
  - It is read with `show-options -w -t <window id> -qv @muster-dispatch`,
    without `-A`, so a global value is never inherited. The host refuses to
    spawn while a global `@muster-dispatch` is set.
- **Landing check:** after creation,
  `display-message -p -t <pane> '#{pane_current_path}'` must equal the
  worktree directory. Otherwise the host kills the window and reports
  `cwd_mismatch`. (tmux returns 0 for a missing `-c` path and opens in the home
  directory instead.)
- **Listing before reply:** `agent-spawned` is written only after this
  connection has sent a listing that contains the window. This is
  host-sessions' `listWindow` guarantee, which also states why `RefreshNow` is
  not enough. Otherwise the host writes `not_listed`, and the server keeps the
  row `unconfirmed`.

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

The host builds the argv itself, from constants plus three values: the session
id it chose, the worktree path it created, and the prompt. muster sends no
flag, no path and no binary. The spawn has **no permission flag of any kind**,
so both programs start in their normal modes (decision 3).

**claude:**

```text
<shell> -l -c 'exec "$@"' <claude-abs> <claude-abs> --session-id <uuid> -- <prompt>
```

- `<claude-abs>` is the `claude` binary, resolved against the tmux server's
  PATH and passed as an absolute path. A profile that changes PATH therefore
  cannot swap the binary (host-sessions "The command").
- `--session-id <uuid>` (Claude Code 2.1.289 `--help`: "Use a specific session
  ID for the conversation") is a fresh UUIDv4 **the host mints**. That gives
  the link to the dispatch without the transcript guess ([3.7](#37-linking-the-spawned-window-to-the-dispatch-and-the-task)).
- The prompt is the positional `[prompt]` (`Usage: claude [options] [command]
  [prompt]`). It goes in as **one argv word** after `--`.
- **The subcommand hazard.** Claude's usage puts `[command]` before
  `[prompt]`. So a prompt equal to a subcommand name (`update`, `mcp`,
  `doctor`, …) could be parsed as that subcommand. Two guards close it:
  - `--`. Whether Claude's parser treats the word after `--` as the prompt is
    **UNVERIFIED**, and phase 3 measures it with a stub.
  - Structurally, **muster's prompt always begins with its own fixed preface**,
    `muster: your owner's chief started you …`. That preface is never a
    subcommand name and never starts with `-`. This guard holds whatever `--`
    does.

**opencode:**

```text
<shell> -l -c 'exec "$@"' <opencode-abs> <opencode-abs> --session <id> --prompt <prompt> <worktree>
```

- `--session <id>`: "Session ID to continue, or to create if it does not
  exist" (`opencode --help`, v2.0.32). The host mints the id, so the mirror
  links the window **deterministically**. That is the "launcher record that
  names the session deterministically" which the title heuristic says a send
  needs (`internal/opencodehost/title.go:10-34`).
  - **UNVERIFIED:** which id formats opencode accepts for a session that does
    not exist yet.
  - Phase 4's first item measures it. The fallback is to omit `--session` and
    link through the pane title against the server's newest root session
    created after the spawn, and to say so.
- `--prompt <prompt>`.
  - **UNVERIFIED:** whether it submits on launch or only fills the input
    (host-sessions noted the same).
  - If it only fills the input, the prompt goes instead through opencode's own
    HTTP API, which the mirror already holds a token for:
    `POST /api/session/{id}/message` (`internal/opencodehost/client.go`).
    **Never through keystrokes.** The host refuses to paste into opencode
    (`docs/mirroring.md`, "muster does not send to opencode yet").
- `<worktree>` is the positional `[<directory>]`.
- **The shared background service.** Bare `opencode` attaches to a running
  `opencode serve --service` if there is one, and its tools then run in that
  service's environment (host-sessions "The opencode profile"). The spawn
  keeps that default, to match a hand-started opencode. Whether the service
  then runs the session's tools in `<worktree>` is **UNVERIFIED**, and is
  measured in phase 4 before shipping. If it does not, the spawn adds
  `--standalone`.

**Why argv, and not a file or a paste.**

| delivery | shell sees it | timing | failure is visible | verdict |
|---|---|---|---|---|
| **one argv word, behind `exec "$@"`** | **never**: the script is a constant, and the prompt is `$N` | at exec, atomic | exec fails, and the host reports it | **recommended** |
| a file, then `$(cat file)` | yes: needs a shell expansion, or a flag neither CLI has for a user prompt | at exec | — | ✗ |
| a paste through the send queue, after `idle` | no | races the CLI's startup and trust dialog | "unconfirmed" | ✗ for claude; impossible for opencode (the host refuses pastes into it) |
| opencode's HTTP API | no | after the session exists | an HTTP status | the fallback for opencode only |

**Cost of argv: other processes can read it.** The argv is readable at
`/proc/<pid>/cmdline` by every process of the same Unix user, for the life of
the process. On these hosts, that is the owner's own user, which can already
read the transcript the prompt lands in. A prompt is not a secret here. This
cost is accepted and stated rather than engineered around.

**Claude Code's directory trust prompt.** A fresh worktree is a directory
Claude Code has never seen, so `claude` may stop at "Do you trust the files in
this folder?" before it reads the prompt.
- **UNVERIFIED:** whether trust accepted for a parent directory covers the
  children under `<agent-spawn-dir>`. If it does, the owner accepts trust once,
  for that directory.
- If it does not, the window shows the dialog in its terminal and the dispatch
  sits there.
- Phase 3 measures this first. The host must **not** write trust into the
  owner's `~/.claude.json` itself: that is the owner's configuration, and it
  is a permission decision. See [Open questions](#7-open-questions), Q3.

### 3.7 Linking the spawned window to the dispatch and the task

**A new table, `host_dispatches`** (migration `0072_host_dispatches.sql`),
under row-level security on `owner_id`:

| column | type | |
|---|---|---|
| `id` | bigserial | the `dispatch` on the wire, in `@muster-dispatch` and in the worktree path |
| `owner_id` | bigint | RLS |
| `host` | text | |
| `runtime` | text | `claude` or `opencode` |
| `repo`, `base`, `branch`, `worktree` | text | `branch` and `worktree` come from the reply |
| `task_id` | bigint, null | `REFERENCES notes ON DELETE SET NULL`, as `agents.note_id` does |
| `agent_session` | text, null | the id the host minted |
| `tmux_window` | text, null | `@N` |
| `status` | text | `requested`, `spawned`, `unconfirmed`, `refused`, `ended`, `host_lost` |
| `refusal`, `worktree_state` | text, null | |
| `created_at`, `ended_at` | timestamptz | |

**Why a table, when host-sessions slice 1 had none.** That slice had no caps
on the server and no tasks. Here the live cap needs a count that survives a
muster restart, and the task needs an owner of its link. The ledger rows alone
cannot say which window is still alive.

**The flow.**
1. The reservation commits. Then the server inserts the row (`requested`) and
   sends `agent-spawn` carrying `dispatch: <row id>`.
2. `agent-spawned` sets `spawned` together with the window, session id,
   branch and worktree. It also writes a `task.chief_dispatch_agent` audit row
   whose detail is `{host_dispatch_id}`, which is what `chiefBudget` counts.
3. Any listing whose window carries `dispatch` settles an `unconfirmed` row.
4. When a connected host's listing no longer has the window for two
   consecutive listings, or its runtime is gone from the window, the row is
   set `ended`. The worktree state follows from the host's cleanup report.
   Two listings, because one transient missing listing must not free a slot.

**What the owner sees in muster.** The window is an ordinary mirrored window,
with everything that already ships: the rendered view, waiting, Claude's asks,
and opencode's permission and question asks (#416). In addition:
- the window and its sidebar row show a chip, **"chief · #<task>"** (or
  **"chief"** with no task), taken from the row through `dispatch`;
- the task page lists it under the agents working the task, through
  `liveOnTask`;
- the chief's `live_agents` names it as `{host, window, task}`.

**Kill.** The owner can **Kill** it from the window's page, behind step-up.
The host checks that the window's own `@muster-dispatch` equals the request
before running `kill-window` (host-sessions "The host does not trust muster").
Kill also releases the task's one start, as the owner's destroy of a pod does
(`ReleaseChiefDispatch`).

**When the task becomes complete.** Today a pod is *stopped* when its task
becomes complete (`stopChiefAgentOnComplete`). **A host window is not killed
automatically.** It is a process on the owner's workstation that the owner may
be typing into. Its slot is freed only when the window ends.

### 3.8 Read-only and tenant hosts

**The host refuses** in all of these cases, whatever muster sends:
- no `-interactive`: `not_interactive`;
- no `-agent-spawn`: `spawn_disabled`;
- **a mirror on any tmux server but the default one**: `not_default_server`.
  That is `-tmux-socket` or `-tmux-socket-path` set
  (`internal/mirrorhost/tmux.go:52-60`).

**Why the last rule is structural.** In multi-tenant.md, a non-default tmux
server is exactly what marks a tenant's mirror
(`muster-mirror@<tenant>` → `tmux -L <tenant>`). So even if a tenant unit's
environment set `-agent-spawn` by mistake, that mirror would still refuse.
Decision 2 says "the host's DEFAULT tmux server", so this costs nothing the
owner asked for. See [Open questions](#7-open-questions), Q2.

**The server cannot cross tenants.** The hub keys hosts by `(owner, host)`.
- A mirror connected to a different muster server (another tenant) is not in
  this server's hub at all.
- A host owned by a different user on this server is under that user's owner
  id, and the chief's `agent_targets` never lists it.

A spawn can therefore reach only a host whose mirror token belongs to the
chief's owner (`internal/mirror/hub.go:653`).

**Not offered and refused** are the same predicate, `hostSpawnable(view)`. It
is used both by `agent_targets` and at the call, so the offer and the refusal
cannot disagree.

### 3.9 Host-side limits

The host keeps its own limits, because a host does not trust muster
(host-sessions):
- **`maxLive`** (`-agent-spawn-max-live`, default 4): windows tagged
  `@muster-dispatch` whose runtime is still running, counted from tmux at
  spawn time. The count therefore holds across a mirror restart.
- **6 spawns an hour**: kept in memory, like `resumeHostPerHour`
  (`internal/mirrorhost/resume.go:61-64`).

Both are below the server's caps. They bound a compromised or buggy server,
not the chief.

### Relation to host-sessions.md

| host-sessions.md (proposed, unbuilt) | this page |
|---|---|
| trigger: a browser form, behind step-up | the chief's tool, in the owner's typed turn, plus step-up ([5.1](#51-who-can-trigger-a-spawn)) |
| a new session per spawn, from `sessionPool`/`m-<hex>` | one `agents` session, a window per spawn (decision 2) |
| directories: children of roots, chosen by key | a fresh worktree from `origin/<base>` (decision 2); host-sessions had it as its optional phase 5 |
| `spawn.json` profiles with a fixed argv, and the bypass table | runtimes fixed in code; no argv from any configuration file, so there is nothing to bypass |
| no initial prompt in slice 1 | the prompt is the point; it goes in argv |
| opencode terminal-only | opencode has a rendered view and asks now (#416, `docs/design/opencode.md`) |
| no table | `host_dispatches`, for the caps and the task link |
| reused | the tmux-server pid check, the separate tag call and its rollback, the landing check, listing before reply, the absolute `argv[0]`, the `exec "$@"` wrapper, no environment from muster |

If this page is accepted, host-sessions.md gets a status line saying that its
UI trigger is superseded, and that its mechanics live on here.

---

## 4. The UI deletion plan

**What goes.** The items in [1.5](#15-every-consumer-of-what-gets-deleted)'s
"deleted" list:
- the sidebar **+**;
- the home page's "Dispatch an agent";
- the task card's **Dispatch**;
- both forms and "Save for later";
- `GET /ui/agents/new`, `GET /ui/agents/notes` and `POST /agents`, plus their
  handlers.

Their entries come out of `routes.golden` and `routes.partition.tsv`, and go
into `removed_surfaces_test.go`, which then pins that each route answers 404.

**What replaces each entry point.**
- **Sidebar and home page:** the space where **+** was says "Ask your chief to
  start an agent" and links to the chief's chat. `Features.Dispatch` and
  `dispatchOffHint` are kept only if that hint still needs the "not set up"
  variant, which is the case when no provisioner is wired **and** no host can
  spawn.
- **Task card:** its **Dispatch** button becomes **Ask chief to start**. It
  opens the chief's chat with the line *"Start an agent on task #N"*
  prefilled. The owner presses Send, and that typed turn is what
  `owner_asked` checks. The chief must still ask for the target.

**What stays.**
- The agent list in the sidebar, and every agent page, with Start, Stop,
  model, sessions and Delete.
- `POST /agents/{id}/start`, which still starts any `stopped` row.
- The repository and model option routes, which the task forms use.

**Migrations: none.**
- There is no draft data. Rows that "Save for later" created are `stopped`
  agents, they stay, and they can still be started from their pages.
- The reaper's skip for saved rows that never started
  (`internal/agents/reap_stopped.go:47`) stays, as long as such rows can
  exist.

**Text that must change in the same PR:**
- `KickoffRefusalSummary` becomes *"…start the agent from its page with Start,
  which brings the instance up without claiming a first turn"*. That works
  only for existing rows. For new work the sentence names the chief.
- Its test (`kickoff_refusal_test.go:450`).
- The docs listed in [1.5](#15-every-consumer-of-what-gets-deleted).

**e2e: a fixture path to replace the form.**
- About 15 specs used **Save for later** only to create an agent. They get
  `e2e/lib/seed.ts`, which inserts a `stopped` agent row directly into the e2e
  database (`MUSTER_E2E_DATABASE_URL`, which the suite already requires). That
  row is exactly what `action=save` produced.
- The specs that tested the form itself (`kinds`, `byo-credentials`,
  `pickers`, `onboarding`, and the dispatch parts of `agent-page-actions`) are
  deleted or rewritten to assert the new hint.

**Rollback.** One revert of the PR, with no schema change. The chief's tool
does not depend on the form.

**Before deleting it, settle the worker-pool question.** The form is today
the only creator of a worker-pool agent (`docs/jobs.md:251`). Q1 asks whether
`agent_dispatch` gains a `worker_pool` field (a `pod/claude-code` option), or
whether worker pools lose their creator.

---

## 5. Security

### 5.1 Who can trigger a spawn

| layer | check |
|---|---|
| server | the caller is the owner's chief, and the chief is on (`agent.go:627-634`) |
| server | `MUSTER_CHIEF_DISPATCH_OWNERS` names the owner (the existing opt-in) |
| server | `MUSTER_CHIEF_HOST_DISPATCH_OFF` is not set (new; [5.4](#54-kill-switches)) |
| server | `owner_asked: true`, **verified**: the turn is a message the owner typed in a signed-in browser session, with no other turn of the chief's running (`ownerTurnFor`), re-checked under the lock |
| server | **step-up: the browser session that typed that turn was inside `MUSTER_STEPUP_WINDOW` when it typed it.** This is recorded on the owner-turn marker (migration 0069) when the turn starts |
| server | the host is the owner's, connected, v2, `agentSpawn && interactive` (`hostSpawnable`) |
| server | the repository is in the host's latest offer; the caps hold |
| host | `-interactive`, `-agent-spawn`, the default tmux server |
| host | the runtime is one of two constants; the repo is in its own offer, re-resolved under a root; `base` passes `check-ref-format` |
| host | `maxLive` and the hourly limit; the tmux server already exists |

**Why step-up.** On an `-interactive` host, sends and Live keys are already
behind the 12-hour step-up (host-sessions "Step-up, compared with the send
gate"). A spawn must not be the weaker door. Whether an ordinary chief chat
send already requires step-up is **UNVERIFIED** (`internal/api/composer.go`
gates the composer's sends, and the chief chat's path is not traced here). So
this page records the freshness explicitly rather than inherit it.

**No HTTP route or token can spawn.** There is no UI form, no `mst_` route, no
hook-token route and no CLI verb. The only caller of `Hub.SpawnAgent` is
`toolAgentDispatch`. That is pinned by a seam test, the way
`provisioner_seam_test.go` pins the provisioner's callers: an asserted ledger
of callers that fails when the set grows or shrinks.

### 5.2 argv and prompt injection

- **Shell.** None. Every call is an argv. The only script is the constant
  `exec "$@"` (`internal/mirrorhost/resume.go:405`), and the prompt is a
  positional parameter, never interpolated.
- **tmux.** The prompt never reaches tmux as a command word, because it sits
  after `--` inside the shell's argv. The window name and the branch slug are
  `[a-z0-9-]`, with no trailing `;`.
- **git.** `base` is validated; the repository comes from the host's own list;
  the paths are host-built.
- **The CLIs' own flag parsing.** For claude: `--`, plus the fixed preface, so
  the prompt can never be a flag or a subcommand ([3.6](#36-the-commands-and-prompt-delivery)).
  For opencode: the prompt is the value of `--prompt`, a single word.
- **Prompt injection into the agent** is the real surface. The chief reads
  outside text (plugin messages, task bodies), and its prompt becomes the
  agent's first message. The bounds are:
  - **Who:** a host spawn only ever runs in a turn the owner typed, so outside
    text alone cannot trigger one ([2.5](#25-gating-when-there-is-no-task-to-hang-it-on)).
  - **How framed:** the prompt is fenced as relayed by the chief, and muster's
    rules close the message and take precedence (draft PRs only; never mark a
    task complete; text from outside is data).
  - **What it can do:** the agent runs in **normal permission mode**, so every
    tool use that the owner's own settings do not already allow becomes an ask
    on muster. That is a mitigation, not a boundary: an owner who allows
    everything in their settings allows it here too, and opencode's permission
    policy lives in its config (host-sessions "The opencode profile").

### 5.3 Caps

There are two independent layers:
- **The server:** the chief's 5 live and 10 per day. Host spawns now count
  against them, and an offline host's spawns stay counted.
- **The host:** `maxLive` 4 and 6 per hour.

A server bug or a compromised server is bounded by the host's limits. A
runaway chief is bounded by the server's.

### 5.4 Kill switches

| switch | effect | how fast |
|---|---|---|
| `MUSTER_CHIEF_HOST_DISPATCH_OFF=1` (new, server; like `MUSTER_MAIL_RESUME_OFF`, `cmd/muster-server/config.go:203`) | refuses every host target; pods unaffected | restart |
| empty `MUSTER_CHIEF_DISPATCH_OWNERS` | refuses every `agent_dispatch` | restart |
| the chief toggle (`chiefActing`, `internal/api/chief_toggle.go:138-150`) | the chief stops acting | immediate |
| drop `-agent-spawn` on the host | the host refuses | unit restart |
| revoke the host's mirror token | the connection ends, and with it every spawn path | ≤ 30 s (`internal/mirror/service.go:22-25`) |
| Kill on the window | ends that agent, keeps its worktree if it has work | immediate |

---

## 6. Phases

The order is changed from the owner's list: **the UI deletion moves to phase
2.** The reasons:

- **It depends only on phase 1.** The form never started anything on a host,
  so deleting it after phase 1 takes nothing away that the chief cannot do.
- **The form is the uncapped path.** Until it goes, two dispatch paths exist,
  with different guards (no caps, no budget, `connectedRepo` only for some
  kinds). Removing it early leaves one predicate sooner.
- **Host spawn is the longest and riskiest work.** It has several UNVERIFIED
  items (claude's trust dialog, opencode's `--session` and `--prompt`). The
  deletion should not wait behind it.

If the owner wants the form kept until host spawning works, the original order
still works: every phase ships on its own.

### Phase 1: `agent_dispatch` for pods, with or without a task, and the required target

**What ships:**
- `agent_dispatch` and `agent_targets`;
- `task_dispatch` removed, and its name answering the "replaced" refusal;
- `ReserveChiefDispatch` keyed by object, with `dispatch:<n>` objects;
- `owner_asked` required with no task;
- the 8 KiB prompt;
- `liveOnTask`;
- updated text in the chief instructions, `TaskEventTools`,
  `docs/chief.md` and `docs/plugins.md`.

**Tests** (the Postgres suite, `internal/api/task_routing_pg_test.go`'s style,
**each guard with its own refusal**):
- **No target:** the exact refusal sentence, nothing reserved, and the budget
  unchanged. Each malformed target gets its sentence.
- **No task, and no `owner_asked`:** refused. **No task, `owner_asked`, but a
  plugin-message turn:** refused with `chiefReleaseNotOwnersTurn`. **No task,
  in the owner's typed turn:** an agent starts, the ledger row's object is
  `dispatch:<n>`, and `today` goes up by 1.
- **Two dispatches with no task in a row:** two agents, and `live` goes up by
  2. At `live_cap` the third is refused with the live-cap sentence.
- **Every `task_dispatch` guard, re-run through `agent_dispatch` with a
  `task_id`**, with today's sentences unchanged. This is the existing table,
  ported.
- **`task_dispatch` called by name:** the "replaced" refusal.
- **`model` on `pod/claude-code`:** refused. **`model` on `pod/gateway`:**
  accepted.
- **The description carries no cap number** (the existing pin, moved).

**Mutation checks** (rebuild and re-run per mutant, and keep one mutant known
to be caught in the batch as a positive control):
- Default the target to `pod/claude-code`: the no-target test goes red **with
  the target refusal**, not with another error.
- Drop the `ownerTurnFor` call for dispatches with no task: the plugin-turn
  case goes red.
- Count `dispatch:` objects under `note:` instead: the live count of the
  two-in-a-row case goes red.

**Acceptance:**
- **Live, on the owner's deployment:** "start an agent to do X" in the chief's
  chat produces a question asking where to run it. Answering "a claude-code
  pod" starts a pod with no task, which shows in the sidebar and in
  `live_agents`.
- The same request from a plugin message is refused.

### Phase 2: delete the dispatch UI

**What ships:** [section 4](#4-the-ui-deletion-plan) in full, after Q1 is
decided.

**Tests:**
- `removed_surfaces_test.go`: the three routes answer 404, and the elements
  `#sidebar-deploy` and `#home-deploy` are absent.
- The route golden files are updated.
- The e2e suite is green with `lib/seed.ts` in place of the form.
- A Playwright check that the sidebar shows the "Ask your chief" hint.
- The task card's **Ask chief to start** opens the chief's chat with the
  prefilled line.

**Acceptance:**
- `git grep -n -e sidebar-deploy -e home-deploy -e 'value="save"' -e /ui/agents/new`
  finds nothing outside `removed_surfaces_test.go`. A positive control runs
  first: the same grep at the parent commit finds the 16 specs.
- A stopped agent that "Save for later" created before the upgrade still
  starts from its page.

### Phase 3: host spawn, claude

**What ships:**
- `-agent-spawn` and its sibling flags;
- `agent-spawn-offer`, `agent-spawn`, `agent-spawned`, `agent-kill`;
- worktrees, the `agents` session, `@muster-dispatch`;
- `host_dispatches` (migration 0072);
- `Hub.SpawnAgent`;
- the `host/<host>/claude` target;
- the step-up freshness on the owner-turn marker;
- `MUSTER_CHIEF_HOST_DISPATCH_OFF`;
- the chip and the Kill on the window page;
- the cleanup sweep.

**First, measure** (each result recorded in the PR):
- the trust dialog for a directory under an already-trusted parent ([3.6](#36-the-commands-and-prompt-delivery));
- whether `claude --session-id <uuid> -- update` starts a session with the
  prompt `update` (a stub-free check on a scratch config dir);
- that `--session-id` shows up as the pane's `claudeSession` without the guess.

**Tests:**
- **Host, against a real tmux server.** The mirrorhost suite already drives
  tmux. Use stub `claude`/`opencode` binaries that record their argv.
  - **The argv arrives verbatim**, with a prompt holding `$(id)`, `;`, a
    trailing `;`, a leading `-`, newlines and 8 KiB of text. The stub must
    see exactly `[--session-id, <uuid>, --, <prompt>]`.
  - Each refusal code gets its own case.
  - A dirty clone spawns, and the clone's `git status --porcelain` and `HEAD`
    are byte-identical before and after.
  - `branch_taken`.
  - A `base` of `--upload-pack=x` is refused before git runs.
  - A tenant-style mirror (`-tmux-socket x`) refuses with
    `not_default_server`.
  - `@muster-dispatch` is set on the window. A global value blocks the spawn.
  - Kill refuses a window with no tag, or with another tag.
  - Cleanup: a clean worktree is removed, a dirty one is `kept_dirty`, an
    unpushed branch is `kept_unpushed`, and the sweep after a simulated crash
    finds each one.
  - `maxLive` is counted from tmux across a mirror restart.
- **Ordering.** `agent-spawned` comes only after a listing on the same
  connection, with the three cases from host-sessions. Mutation-check it by
  moving the reply before the listing.
- **Server.** The PG suite:
  - a host target without `owner_asked` is refused;
  - an owner's turn with stale step-up is refused;
  - an offline host is refused and **nothing is queued**;
  - another owner's host is never listed and is refused;
  - a host spawn counts in `live`, an unconfirmed one too, an offline host's
    stays counted, and an `ended` one does not;
  - `liveOnTask` blocks a pod beside a host agent on the same task.
- **Seam ledger.** The only caller of `Hub.SpawnAgent` is `toolAgentDispatch`.

**Acceptance:**
- **Live, on the owner's default mirror:** "fix the typo in the README of
  `<owner>/<repo>`" → the chief asks for the target → "on `<host>` with
  claude" → a window `cc-<n>-…` appears in `agents` on `<host>`, in a new
  worktree, and in muster with the chief chip.
- Its first tool use arrives on muster as an ask, and answering it from the
  phone continues the session.
- Kill removes the window. A clean worktree is gone, and an edited one is
  kept.

### Phase 4: host spawn, opencode

**What ships:** the `host/<host>/opencode` target, the opencode argv, the
session-id link, and the prompt delivery decided by the measurement.

**First, measure:**
- `--session <id>` with a host-minted id for a session that does not exist;
- whether `--prompt` submits;
- whether the shared service runs the session's tools in `<worktree>`.

Each answer picks a branch already written in [3.6](#36-the-commands-and-prompt-delivery).

**Tests:**
- The opencode argv arrives verbatim.
- The window's `agentSession` equals the minted id.
- If the API fallback is chosen: the prompt reaches `POST
  /api/session/{id}/message` once, and an HTTP failure reports
  `prompt_failed` with the window kept.

**Acceptance:**
- **Live:** the same click path as phase 3, with opencode.
- Its permission ask appears in muster's Attention view (#416), and an answer
  from there continues the session.

---

## 7. Open questions

1. **Worker pools.** Today the form is the only creator of a worker-pool agent
   (`docs/jobs.md:251`). Should `agent_dispatch` take a `worker_pool` option on
   `pod/claude-code`? Or do worker pools get created some other way (for
   example a `muster` CLI verb)? Or are they retired? **Phase 2 is blocked on
   this answer.** Recommendation: a `worker_pool` field on the pod target, and
   the same pool checks the form ran (`internal/api/agents.go:1050-1058`).
2. **Default tmux server only?** This page makes the host refuse
   `-agent-spawn` on any non-default tmux server, which structurally excludes
   tenant mirrors. Is there a host where the owner's own mirror runs on a
   named socket? If so, the alternative is an explicit
   `-agent-spawn-i-am-the-owner` flag, which is weaker.
3. **Claude Code's trust dialog in a fresh worktree.** If phase 3's
   measurement shows that trust on `<agent-spawn-dir>` does not cover its
   children, which do you choose? (a) Accept the dialog in the window once per
   dispatch. (b) Let the host mark each new worktree as trusted in your Claude
   Code configuration, which is a permission decision made by muster on your
   behalf. (c) Cut worktrees inside each clone's own already-trusted
   directory, `<clone>/.muster-agents/<n>`, gitignored, which is closer to
   "a checkout you have open". The recommendation is (a), until measured.
4. **Idempotency for dispatches with no task.** A chief that retries after a
   timeout starts a second agent, which is bounded by the caps and visible in
   `live_agents`. Is that acceptable? Or should the tool take a
   `request_key`, so a retry of the same key answers with the first start?
   The recommendation is to accept it for now.
5. **`task_forward` to a host agent.** Should the chief be able to forward a
   later task event into a host-spawned window? That would be a mirror send
   (step-up, and today it refuses opencode), and it is out of scope here.
   Should it be scoped next?
