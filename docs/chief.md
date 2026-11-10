# The chief

The **chief** is your fleet agent: the one agent with no task and no repository,
whose job is answering you about the state of the work. Plugin messages are
delivered to it, and it alone is offered the chief tools (see below).

What to ask it, with real example replies: [chief-cookbook.md](chief-cookbook.md).

**The chief is optional and off by default.** muster works without one: home
opens the session you opened last, and your sessions, tasks and asks all work
as usual.

## Turning the chief on or off

Each user has a switch: **Account → Chief**.

| | what you get |
|---|---|
| **Off** (the default) | No agent acts as your chief. Home (`/`) opens the session you opened last (or, with none, the first in the sidebar), and the sidebar has no chief row. Nothing is delivered to a chief: plugin messages stay queued, as they do for a user with no chief ([pending](plugins.md#delivery-to-your-chief)), a mirrored session's `chief_ask` is answered with "your owner turned their chief off; ask your owner with attention_ask" (the tool stays listed, because a session reads its tool list once, when it connects), and a task mention's ask comes from muster. |
| **On** | Home opens the chief's chat, the sidebar lists it first, and everything on this page applies. |

**Who starts with it on.** A user who never touched the switch has the chief on
exactly when they already have a chief agent (an agent with the reserved slug
`chief`/`chief-<id>`, or one labelled `chief`). A new install and a new user have
no chief agent, so they have no chief. A deployment that already runs a chief
keeps it, unchanged, with no data migration: the agent is the state that turned
it on, and it still does. Once you use the switch, your choice wins either way
(migration `0060`, `chief_preferences`).

**Turning it off** stops the chief from acting and hides it. It deletes
nothing: the agent, its sessions and its chat history stay, and turning it on
again brings them back. While it is off:

- everything that talks to the chief (plugin deliveries, `muster chief ask`, a
  session's `chief_ask`, the chief panel and its threads) treats you as having
  no chief;
- the chief agent is offered none of the chief tools, and a call to one is
  refused, so a chief that is still running cannot route, draft or dispatch;
- with the agent provisioner, its pod is stopped (Stop, not Delete). The
  stopped-agent reaper never deletes the chief. If the Stop fails, the switch
  still reads off and the chief gets no work, and the page says its pod is
  still up (turn it off again to retry). On the docker compose install, the
  page says "Off, but its container did not stop", naming the process the
  container's supervisor could not end; the supervisor keeps retrying, and
  `docker compose restart chief` ends everything in the container;
- it still cannot be deleted by accident (the delete protection below does not
  depend on the switch);
- `POST /chief/provision` refuses (409) rather than create or start it.

**Turning it on** with a chief agent starts its pod (a Start of a pod that is
already running only re-applies it). If the Start fails, the switch stays off
and the page says the chief could not be started. With no chief agent, it
creates one (the same as `POST /chief/provision`). If the create is refused, for
example because you have no OpenRouter key or Claude account, the switch stays
off and the page says what to add.

**A message sent while it is still starting waits.** A chief just turned on
runs a first turn in its chat, and its Claude Code session takes one turn at a
time. A message you send during that turn is held, not refused: the chat shows
"Still finishing its previous turn — your message is queued and will be sent as
soon as it is free" in place of "working" (the notification panel says
**queued**), and sends it as soon as the turn ends (it asks again every 3
seconds, for up to 10 minutes). The moment the agent takes it, the row goes
back to "working". Several messages waiting for the same agent are sent in the
order you sent them, from whichever page or box. Closing the page does not
cancel it: the message is still sent and the reply stored, and a page opened
while it waits shows "working…".

It is not sent, and the conversation says so in words (whether or not a page
is open; an open chat also puts the text back in the box to send again), when:

- the session is still busy after 10 minutes, or
- the chief is replaced or restarted while the message waits — a waiting
  message does not hold up a replacement or a `chief_restart`.

A restart of muster during the wait ends it, like any turn in flight. The same
applies to any agent's chat, on Kubernetes and on the Compose install, whenever
a turn is already running in that session; a message sent from the Terminal's
composer waits the same way, in the background. A prompt waiting for an answer
in the agent's terminal holds the session too, and the chat says so.

**The last click wins.** Each user's switch changes run one at a time, each
finishing its Start or Stop before the next begins, so a quick Off then On (or
On then Off) leaves the pod in the state the switch shows.

**Not set up on this install.** The chief runs either in the Compose stack's
`chief` container ([below](#on-the-docker-compose-install)) or in its own
Kubernetes pod through the [agent provisioner](provisioner.md). With neither,
the Account page says how to turn it on instead of drawing a switch that cannot
work. (An install without either that already holds a chief agent still gets the
switch, to turn that chief off.)

## On the docker compose install

The `chief` profile runs the chief in a container of its own, from the
`claude-code-agent` image, beside muster. Turning it on takes four `.env` lines
([features.md](features.md#chief)): the profile, `MUSTER_CHIEF_RUNTIME=compose`,
the connector store's key, and a credential.

**How muster runs it without the Docker socket.** muster starts no container
(the socket is root on the host). The Compose file starts the `chief` container,
and muster tells its supervisor (`ccd compose`) what to run by writing the
chief's spec into a volume the two containers share — the
[compose driver](provisioner.md#the-provisioner-seam-internalprovision). The
switch on Account → Chief writes it, Start and Stop rewrite it, and the
supervisor starts, stops or restarts the Claude Code CLI inside. **Off stops
the CLI; the container stays up and idle.** The spec holds the chief's own
credentials, and the agent's session cannot read it: the supervisor runs as
another identity than the agent, and the spec belongs to the supervisor's group.

**Its credential**, decided each time it starts:

- `MUSTER_CHIEF_CLAUDE_TOKEN` set: your Claude subscription (a `claude setup-token`
  token). It wins when both are set.
- Otherwise your OpenRouter key (`MUSTER_AGENT_OPENROUTER_KEY`), through the LLM
  proxy ([below](#the-chief-on-openrouter)). 🔴 **muster adds no budget: set a
  credit limit on the key itself**, at
  [openrouter.ai/settings/keys](https://openrouter.ai/settings/keys).
- Neither: turning it on is refused, and Account → Chief says which variables to
  set.

A change in `.env` reaches a running chief when muster restarts
(`docker compose up -d --wait`): muster re-applies its spec at boot on the new
credential, with no Off/On needed. A chief that is off stays off. If you
removed its credential and set no other, muster **stops** it at that boot (it
does not keep running on the one you deleted), and Account → Chief says what to
set.

Account → Chief says which one it runs on, whether routing work to your
sessions is on (it needs `MUSTER_SESSION_ROUTING=1`), and,
while it is on and its container is not answering, why. Off, it says so if
the stop left a process running (["Turning it off"](#turning-the-chief-on-or-off)).

**What is different from the Kubernetes chief:**

- **One chief per install, the admin's** (user 1): the container is one. Another
  user's Account page says the install's chief is the admin's.
- **Its network is not confined. The chief container can reach the internet**,
  on either credential: the subscription token's calls go to the Claude API
  directly, and on OpenRouter the model calls go through muster but the
  container's other traffic is not blocked. The container is on a network of its
  own with muster, so it cannot reach Postgres, the S3 store or the STT server.
  A Kubernetes chief's NetworkPolicy admits only public 443 and muster's connect
  port; the Compose file cannot express that.
- **No resource limits** from muster (none are set on the container).
- **amd64 only.** The `claude-code-agent` image is published for amd64 (x86_64)
  alone, so the `chief` profile does not run on an arm64 host (a Raspberry Pi, an
  Apple-silicon Mac without emulation). muster itself runs on both.
- **No Terminal view**: muster has no exec into the container. Its output is in
  `docker compose logs chief` on the server.
- **No other agents**: `agent_dispatch` is not offered, and the dispatch UI stays
  off ("Agent dispatch not set up"). Agent dispatch needs Kubernetes.
- **`muster chief replace` is refused** (409): there is one kind here.
- **No HQ server**: the published image has no `hq` binary.

**Its tools.** The chief reaches muster over the MCP server on muster's connect
port (8106 inside the stack, not published to the host). Nobody can answer a
permission prompt in its container (there is no approval gate and no terminal
muster can reach), so:

- these run **without a prompt**: `sessions_overview`, `fleet_list`,
  `focus_search`, `fleet_report`, `tasks_list`, `task_get`, `attention_list`,
  `ask_owner` and, with session routing on, `route_to_session`;
- **they are the only muster tools it is offered.** muster's MCP server lists
  the chief nothing else (not `task_comment`, `task_set_status`,
  `task_create_gated`, `reply_draft`, `agent_dispatch`, `attention_ask`,
  `connectors_list` or `muster_help`), and refuses a call to one by name. Its
  instructions name the same tools and say plainly that filing or commenting on
  a task, changing a task's status, raising or answering an ask in the
  Attention queue, dispatching an agent, drafting a plugin reply, connectors
  and HQ are not available on this install. All three (what it may run, what it is offered, what it is told) come
  from one list in muster's code;
- **the turns muster sends it name only those tools too.** A plugin message's
  turn, its session hints and a route's hand-back are rendered from the tools
  it is offered: no "draft a reply with `reply_draft`" or "raise an
  `attention_ask`". With plugins and session routing on, it is told the one
  thing it can do with a plugin message is route it to a session; without
  routing, that it cannot act on one. A mirrored session's `chief_ask` is
  refused (the chief cannot answer it) and the session is told to ask you with
  `attention_ask` instead;
- **task routing is refused beside it.** `MUSTER_TASK_ROUTING=1` with
  `MUSTER_CHIEF_RUNTIME=compose` refuses to boot: a task event asks the chief to
  decide with `agent_dispatch`, `task_forward` or `task_route_attention`, none of
  which it is offered, and nothing else would put the event in front of you;
- **every other prompt is denied at once**, not left hanging: Bash, a file edit.
  The chief is told it was denied and carries on.

⚠ **Not every tool prompts.** In its default permission mode Claude Code itself
runs some tools without asking, so they run here too, beside the eight above
([Claude Code: permissions](https://code.claude.com/docs/en/permissions)):
read-only tools (file reads, Grep, Glob) within its working directory
(`/data/workspace`), a built-in set of read-only Bash commands, and WebFetch to a
built-in set of documentation domains. Everything else that would ask is denied.

On Kubernetes those prompts go to the approval relay instead.

**Removing it.** Taking `chief` out of `COMPOSE_PROFILES` does not stop the
container: Compose leaves a running service of a profile you dropped alone, even
with `--remove-orphans`, and its spec still holds its credential. To remove the
chief, from the directory with your `.env`:

1. Turn it **Off** under Account → Chief.
2. Remove its containers:
   `docker compose --profile chief rm -sf chief chief-init`
3. In `.env`, take `chief` out of `COMPOSE_PROFILES` and delete
   `MUSTER_CHIEF_RUNTIME`, `MUSTER_CHIEF_CLAUDE_TOKEN` and `MUSTER_CHIEF_LLM_MODEL`.
4. Delete its volumes (the spec, which holds the credential, the instructions and
   its data). muster mounts the spec, so stop the stack first:

   ```sh
   docker compose down
   docker volume rm <project>_chief-spec <project>_chief-instructions <project>_chief-data
   docker compose up -d --wait
   ```

   `<project>` is the Compose project name: the directory's name, or
   `COMPOSE_PROJECT_NAME` (`docker compose ls` shows it).

## Stall nudges

With `MUSTER_CHIEF_NUDGES=1` the chief also judges your sessions that have
stalled — idle, with the last word theirs — and either nudges one on with a
labelled message quoting your own recent instruction to it, or asks you on the
Attention page and relays your answer. It ships in shadow mode: decisions are
recorded, nothing is sent. See [chief-nudges.md](chief-nudges.md).

## Which agent is the chief

Two rules, both in `internal/api/chief_agent.go`:

- The **reserved slug**: `chief` for user 1, `chief-<id>` for any other user.
  Only `POST /chief/provision` and `POST /api/chief/replace` create one.
- The **label**: everything that *talks* to the chief (the panel, recaps,
  threads, plugin delivery, `muster chief ask`) resolves it by display name
  `chief`, the lowest id winning among your agents.

Both are delete-protected: `DELETE /agents/{id}` and a task dismissal refuse
them. Replacing the chief (below) is the one path that deletes one.

Neither rule looks at the agent's **kind**. A chief can run on either kind.

## The chief on each kind

| | `gateway` | `claude-code` |
|---|---|---|
| Standing instructions | `AGENTS.md` in its workspace (`agentspec.ChiefInstructions`) | the Claude Code CLI's managed-policy memory file, `/etc/claude-code/CLAUDE.md` (`agentspec.ChiefInstructionsClaudeCode`) |
| How it reaches muster's tools | native function tools on each turn, plus the in-pod `muster` CLI | muster's MCP server only, as `mcp__muster__<tool>`; the image has no `muster` CLI |
| The subsystem store (cairn) | when `MUSTER_AGENT_CAIRN_URL`/`_TOKEN` are set | never: the client is not in the image and the network policy cannot reach a store. A configured store is skipped with a log line. |
| Needs | an OpenRouter key | a Claude account, and `MUSTER_AGENT_CONNECT_URL` (no MCP server without it; the build refuses) |

The managed-policy path is documented in
[Claude Code's memory docs](https://code.claude.com/docs/en/memory) and was
checked against the pinned CLI. It is outside the agent's `/data` volume, so it
never shadows a `CLAUDE.md` in the agent's workspace. The file is rebuilt from
the spec on every apply, and a running pod picks it up at its next start, the
same as the gateway kind's `AGENTS.md`.

A `claude-code` **worker** gets no standing instructions: the worker prose
describes the in-pod CLI and the autosave log, and that image has neither.

### The chief tools over MCP

The chief is offered these tools, and no other agent is:

| tool | needs |
|---|---|
| `task_create_gated`, `reply_draft`, `plugin_message_settle` | plugins on |
| `fleet_list`, `focus_search`, `sessions_overview`, `fleet_report` | session focus |
| `route_to_session` (an idea you typed in its chat: [plugins.md](plugins.md#routing-your-own-request-to-a-session); one of your tasks: [plugins.md](plugins.md#routing-a-task-to-a-session); or a plugin message) | session routing (a plugin message also needs plugins) |
| `attention_list` | the attention queue |
| `answer_session_question` | the attention queue: a mirrored session's [`chief_ask`](attention.md#asking-the-chief) |
| `ask_owner` (a question card in its chat: you tap an answer) | the attention queue |
| `task_route_attention`, `task_forward` | task routing (`MUSTER_TASK_ROUTING`) |
| `agent_dispatch` | task routing, and you opted in (`MUSTER_CHIEF_DISPATCH_OWNERS`) |

The other way round, the chief is the one agent **not** offered
`connector_request`, `connector_status`, `browser_flow_request` or
`browser_flow_status`: its connectors are the standing grants you give it
(`connectors_list` shows them), and it does not ask for browser flows.

On the docker compose install the chief is offered far less: only the tools
it can run there, the ones pre-allowed in its container ([its
tools](#on-the-docker-compose-install)).

When the chief needs you to choose something, it asks with `ask_owner`: the
question appears in its chat as a card with a button per option and an
"Other" box for your own answer. Tap one (or send Other) and that is sent as
your next message; the card then shows your answer, and still does after a
reload. Asking ends the chief's turn.

The card is also an ordinary [attention ask](attention.md) raised by the chief,
so it is on your Attention view and your phone too, and you can answer it
there. That answer is recorded with where you gave it (your browser or your
phone, [attention.md](attention.md#answering)) and delivered to the chief as a
turn, in the chat session it asked in, and the card in the chat shows it. Only
you answer it, in muster or on your phone: a mirrored session's
`attention_answer` cannot. A message you type to the chief answers one open
card, the oldest; a second card asked in the same turn stays open for its own
answer (the chat draws it as a plain line). A card that expired unanswered is
still answered in the chat by your next message, as before. If the attention
queue refuses the card (its cap of open asks), the chief still asks: the card is
drawn from the chat alone.

Closing or reloading the page never stops a turn the chief is running. Open
the chat again while it runs and it shows "working…"; the reply appears, and
"working…" goes, once the turn has finished and its reply is stored.

`agent_dispatch` starts an agent where you say: its `target` is **required
and has no default**. It is a pod, `pod/claude-code` or `pod/gateway`, or a
window on one of your hosts, `host/<host>/claude` or `host/<host>/opencode`
(mirrored dispatch, below). The chief maps your words to it: "with Claude", or any Claude model, is `pod/claude-code`; any
other model you name is `pod/gateway` with that model: the chief gives its
OpenRouter model id or name, and muster resolves it to the id
([checked against the catalogue](provisioner.md#models-are-checked-before-a-pod-starts)).
Asked to start an agent without a model, the chief calls it with no target, and
the refusal lists the choices that exist now in your words — "Claude Code (your
Claude account)", "Gateway (OpenRouter model: <model>)" — with its budget, so it
asks you which with an `ask_owner` card. It never picks one for you.

**Mirrored dispatch.** Say "mirrored" and the agent starts in a new window of
the tmux session `agents` on one of your hosts, in a fresh git worktree, running
Claude Code in its normal permission mode, or opencode, so its asks reach you on
muster. "Mirrored on <host> with Claude" is `host/<host>/claude`; "mirrored on
<host> with opencode on <model>" is `host/<host>/opencode`, whose `model` is
required: an OpenRouter id, which the host checks against its own opencode's
models. Name no host and muster asks you with a card listing the hosts that can
start one now (and, for a bare "mirrored", then which model: Claude Code, and on
a host that offers opencode the models of `MUSTER_CHIEF_DISPATCH_MODELS` or
another OpenRouter id you type, [configuration](configuration.md)). Tap it on the
Attention view or your phone and the chief completes the start with exactly what
you tapped; type the answer in the chat instead and it starts from your typed
turn. A host starts nothing unless its muster-mirror has `-agent-spawn` and the
repository is on its own `-agent-spawn-repos` list
([mirroring.md](mirroring.md#starting-agents-for-the-chief--agent-spawn)); an
opencode start also needs that host's `-opencode-send` and an opencode token
([sending to opencode](mirroring.md#sending-to-opencode)), `opencode` on its
tmux server's `PATH`, and an absolute path to its own muster-mirror binary (the
launcher). Mirrored dispatch is
part of `agent_dispatch`, so only the Kubernetes chief has it: the docker
compose install's chief starts no agent, on a host or in a pod
([above](#on-the-docker-compose-install)). It never starts a cluster agent
instead. There is no Stop button: close the window
on the host, or end its record with `agent_destroy {host_dispatch: N}` from one
of your sessions.

Each agent the chief starts is drawn in its chat as a card: the agent's name,
where it runs, its repository and task, and its state — starting, working,
waiting, done or error — which follows the agent while the chat is open. Tap it
to open the agent. A refused start is one line saying why. The retired name `task_dispatch` answers *"task_dispatch is
replaced by agent_dispatch, which needs a target: ask your owner where to run
it"*.

It starts on one of your tasks (`task_id`), or **with no task**, from a
prompt alone. A dispatch with no task needs `owner_asked: true` — muster checks
that the turn the chief is in is a message you typed in its chat — and a
`request_key` the chief mints for that request and reuses on any retry of it:
a retry with the same key answers with the first start (`replayed`), and the
same key with anything changed is refused. `owner_asked: true` is always
checked, on any task, so a plugin message or a task event can never start an
agent with no task, nor get its text taken as your wishes. `worker_pool`
creates a claude-code [machine-job worker](jobs.md) (no task), holding one of
the chief's live slots for as long as it runs.

A claude-code agent runs on an account from your Claude account pool and has
no per-agent model; `model` is taken only by `pod/gateway`, and it is
[checked against the model catalogue](provisioner.md#models-are-checked-before-a-pod-starts)
before anything is reserved: a display name that matches one model is resolved
to its id, and anything else is refused with up to five of the closest ids in
`model_suggestions` (a tool error on both transports, MCP included), so the chief asks you which you meant. It works in the
task's repository (or the `repo` the chief passes, one of your connected
repositories; `base` picks the branch): its session starts inside a checkout,
so the repository's `CLAUDE.md` and project skills load. Its optional `prompt`
(at most 8 KiB) opens the agent's first message, fenced as relayed by the
chief, before the task's text and muster's rules (which come last and win).
With `owner_asked: true` it is the chief's relay of the message you typed.
Without it (only on a task), it is accepted **only** as your own words:
verbatim, your answer to an attention ask about that same task ("start a
Claude Code agent and have it open with skill X") — given on `/attention`, or
through one of your mirrored sessions' `attention_answer` (operator decision
"Just remove the filter": a session fed outside text could therefore write a
dispatched agent's prompt — the accepted risk is spelled out in
[attention.md](attention.md#mirrored-claude-code-sessions)). A prompt from an
MCP answer says so: the agent's first message names it as your answer given
over MCP from `<host>`/`<session>`, and the audit row records
`instructions_via` / `instructions_from`. Anything else is refused, and the
chief is told to ask you with `task_route_attention`. The `task.chief_dispatch`
audit row records the kind, the target, the `model` (and `model_input` when it
was resolved from a name), the prompt (`instructions`), the ask it
came from (and, for an MCP answer, where), and any `request_key`. No muster
page reads that row back yet: query it
(`SELECT detail FROM audit_log WHERE action = 'task.chief_dispatch' AND object = 'note:<task id>'`;
a start with no task has the object `dispatch:<32 hex>`).
What the agent was told is also its first message, on its transcript. Guards: [plugins.md](plugins.md#agent_dispatch-the-guards).

**Every agent the chief starts reports back.** When it finishes, fails or goes
quiet ([outcomes](provisioner.md#how-an-agents-work-ends)), you get one push,
and muster writes a note in the chief's chat — `[muster] Agent <name> … that
you started with agent_dispatch FAILED: <reason>. … offer to start it again …`
— so the chief can tell you, and offer to start it again with the cause fixed.
The note costs the chief no turn: it reads it on its next one.

`tasks_list` and its siblings are offered to every agent. The chief's
`tasks_list` covers your whole board, where a worker's covers only its own
tasks. That scope, like the chief-only filter, is decided by the chief
predicate, never by the kind.

### HQ from the chief

A claude-code chief can also work in HQ, your task system: when you ask it in
chat for an HQ task (create, assign, comment, change status, look one up) it
uses the tools of the MCP server named `hq` (`mcp__hq__<tool>`), never a task
on muster's board. Every HQ tool is pre-allowed, so **the scope of the HQ token
you store is the only limit** on what it can do in HQ — and the chief also
reads untrusted plugin messages.

The server is the `hq` CLI's `hq mcp`, run in the chief's pod with `HQ_URL` set
to the `hq` connector's prefix on muster's proxy and `HQ_TOKEN` set to the
chief's own muster token. muster's proxy swaps that for the stored HQ token, so
**the HQ token never enters the pod**. The `hq` connector serves the chief and
no other agent, whatever grants exist (`internal/api/connector_chief_only.go`).

To turn it on:

1. **Put `hq` in the agent image.** The image's Dockerfile takes an optional
   build context named `hq`: build it with
   `docker build --build-context hq=<a directory holding only the statically
   linked hq binary> -f images/claude-code-agent/Dockerfile .`. A build without
   the context has no `hq`, and the chief then registers no HQ server.
2. **Create the `hq` connector** on the Connectors tab: name `hq`, scheme
   **bearer**, host = your HQ host, secret = an HQ token with scope **`ack`**
   (the lowest scope that creates, assigns, updates, comments on and sets the
   status of tasks; listing and reading need only `read`). `ack` also allows
   paging, overrides and data writes — there is no task-only scope.
3. **Restart the chief** (`chief_restart`, or Stop then Start). Whether the
   server is registered is decided when the pod starts.
4. **Approve the chief's request** for `hq` when it appears: the chief's first
   HQ call files it, and that call answers that the request is pending until
   you approve.

If the chief has no `mcp__hq__` tools it says so; check the four steps above
(`ccd seed` logs which one is missing when the pod starts).

## The chief on OpenRouter

A `claude-code` chief can run on OpenRouter instead of a Claude account, through
muster's [LLM proxy](officers.md#the-llm-proxy): its Claude Code calls
`POST /llm/v1/messages` on the connect listener with its own muster token, and
muster sends the request on with the server's key attached. The chief keeps
its harness: the same Claude Code CLI, standing instructions and muster tools;
only where its model calls go changes. Two runtimes turn this on:

- **The `docker compose` install:** the chief's credential decides. No
  `MUSTER_CHIEF_CLAUDE_TOKEN` means OpenRouter ([above](#on-the-docker-compose-install)).
- **Kubernetes:** an explicit opt-in, `MUSTER_CHIEF_LLM=openrouter`. The
  admin's `claude-code` chief's pod is then built with **no Claude account
  token** and `CCD_LLM_ROUTE=muster` (its Claude Code calls
  `$MUSTER_CONNECT_URL/llm` with its own muster token); a member's chief keeps
  its Claude account. Boot is refused without `MUSTER_AGENT_PROVISIONER`,
  `MUSTER_AGENT_KINDS` with `claude-code`, `MUSTER_AGENT_CONNECT_URL` or
  `MUSTER_AGENT_OPENROUTER_KEY`, and beside `MUSTER_CHIEF_RUNTIME=compose`.
  **Unset is the rollback**: the chief runs on its Claude account exactly as
  before, and the proxy serves no chief.
  **Turning it on or off** is two steps: set or unset the variable and restart
  muster, then restart the chief (its agent page ⋯ → Stop, then Start, or
  `chief_restart`; [below](#restarting-the-chief)). The pod's credential is
  part of its spec, and muster rebuilds that spec from the current settings
  only when it starts the chief or re-applies it (a model change on a running
  chief re-applies it too). Until then a chief keeps the credential it had.
  After turning it **off**, a chief still on the route is refused by the
  proxy (it is no longer served) until that rebuild puts it back on its
  account. The proxy serves only the chief the route rule puts on the route:
  the admin's `claude-code` chief.

What applies on both:

- **Who is served.** Only the admin's chief, and only while it is on (Account →
  Chief): a chief you turned off is refused (403), and so is any other user's
  chief. Any other agent is refused as it always was.
- **Whose key.** The admin's, `MUSTER_AGENT_OPENROUTER_KEY`. With it empty, every
  request is refused (400) with a message naming it, and nothing is sent (on
  Kubernetes an empty key refuses boot instead).
- **Changing it (Compose).** A credential change in `.env` (adding or removing
  `MUSTER_CHIEF_CLAUDE_TOKEN`) reaches a running chief when muster restarts
  (`docker compose up -d --wait`): muster re-applies the running chief's spec
  at boot, with no Off/On needed. A chief that is off, or stopped, stays so.
- **Which model.** A setting, `MUSTER_CHIEF_LLM_MODEL`,
  `deepseek/deepseek-v4.1-flash` by default. Every request is pinned to it,
  whatever model the client asks for — Claude Code's background calls that
  name a `haiku` model included.
- **What Claude Code sends that the proxy handles.** Streaming and tool use
  pass through as the Anthropic Messages API (OpenRouter's
  Anthropic-compatible endpoint); the `beta=true` query and the
  `anthropic-beta` header are forwarded; mid-conversation `system` entries,
  which the provider refuses for these models, are folded into the adjacent
  user turn; body fields outside the Messages API (OpenRouter's routing fields
  above all) are dropped. Every path but `POST /llm/v1/messages` answers 404.
  Measured on real Claude Code traffic, the only other call the CLI made was a
  `HEAD /api/hello` connectivity check once per session. The session carries on
  past its 404, which is counted in `muster_officer_llm_refused{reason="path"}`.
  It made no `count_tokens` call.
- **Not in use.** DeepSeek V4.1 Flash and V4 Pro both failed a 50-input shadow
  backtest: they made the chief's decision on 44–56% of inputs, against a 90%
  bar. So the chief stays on Claude.
- **What it may spend.** muster sets **no budget** for the chief: no daily
  budget, no per-item cap. **Set a credit limit on the key itself**, in
  [OpenRouter's key settings](https://openrouter.ai/settings/keys): that limit
  is the only cap. Usage is still counted in the metrics and the connector
  audit trail, as officer `chief`.
- **When the limit is reached.** If OpenRouter answers 402 (payment
  required), the chief's turn ends with a 400 that tells it to check
  `MUSTER_AGENT_OPENROUTER_KEY` and its credit limit, and muster does not
  report the key as broken. If it answers 401 or 403, the chief gets the same
  message, and muster also logs the key as refused and sets
  `muster_officer_llm_upstream_auth_failing{officer="chief"}` to 1. Which
  status OpenRouter sends for a spent limit has not been checked against the
  live service.

## Restarting the chief

The chief keeps running the image and tool set it was started on. After a muster
release (a new `MUSTER_AGENT_CC_IMAGE`, new chief tools), restart it so it picks
them up. A restart is Stop then Start: the pod is scaled down and started again
from the agent's current spec. Its row, chat sessions and history are kept,
unlike [replacing the chief](#replacing-the-chief-on-another-kind).

- **From the browser:** the chief's agent page, ⋯ → **Stop**, then **Start**. No
  fresh sign-in is needed (agent lifecycle is not step-up gated).
- **From one of your own mirrored Claude Code sessions:** ask it to call the
  `chief_restart` MCP tool ([attention.md](attention.md#the-agent-tools)). It takes
  no agent: it restarts **your** chief and nothing else. It runs the agent page's
  own Stop and Start, then waits up to 4 minutes for the chief to be running and
  ready on a new pod, and answers with the pod before and after and the image
  each runs (the agent container's). It only reports that image: it does not
  compare it with `MUSTER_AGENT_CC_IMAGE`, so a restart onto another image still
  succeeds. `muster-ops chief-restart` remains the check that the running image
  equals the configured one.

  It is refused while the chief is mid-turn: a plugin message, its kickoff or a
  memo being delivered, or a chat turn running. Retry when the turn ends, or pass
  `force: true` to cut the turn off, which is what the browser's Stop does.
  `force` overrides a turn only: while another `chief_restart` or a chief
  replacement is running it is refused either way — retry when that finishes. It
  is also refused when you have no chief, or have turned it off.

  A chief whose status was `error` restarts like any other; the wait judges the
  chief as it is after the start. If its pod before the restart could not be
  read, a successful answer carries `new_pod_confirmed: false`: the chief is
  running and ready, but that it is a new pod is not confirmed.

  Nothing is stopped when the Start would be refused (for example, a claude-code
  chief whose owner has no Claude account). If the stop fails, the chief may
  already be stopped: check its agent page. If the start fails after the stop,
  the answer says the chief is **stopped**: start it from its agent page.

  The restart writes no audit row, like the browser's Stop/Start and
  `agent_destroy`. The calling host and session and the outcome are in muster's
  server log.

After either route, the operator's post-checks still apply: the deployments in
the chief's namespace rolled out, the pod is newer than the restart, the agent
image equals `MUSTER_AGENT_CC_IMAGE`, and `ccd seed --check` passes in the pod.
`chief_restart` covers the new pod and reports the image; that the image equals
`MUSTER_AGENT_CC_IMAGE` (`muster-ops chief-restart` checks it) and the seed check
are still yours.

## Starting an agent from your own session

The chief starts an agent only on a turn you typed in its chat
(`agent_dispatch`'s `owner_asked`), so a mirrored Claude Code session cannot get
the chief to start one for you: `chief_ask` relays a question, and the chief
refuses a start it did not get from your typing. Your own mirrored sessions
have their own door instead, in **two verbs, with no tap** (operator decision:
*"dispatch and then another verb to approve the dispatch so I don't need to
click anything"*):

1. `dispatch_request {target, prompt, model?, repo?, base?, task_id?}` checks
   the start and **holds** it. Nothing runs yet. It answers `{hold_id, summary,
   expires_at}`. It takes no idempotency key: a repeated request files a second
   hold, which starts nothing and lapses after 15 minutes.
2. `dispatch_approve {hold_id}` starts **exactly** what was held, once, within 15
   minutes. Any of your mirrored sessions may approve, on any of your hosts.

Both are listed only on the mirrored sessions' MCP door, and only while the
chief's dispatch is on for you (`MUSTER_TASK_ROUTING=1`, which needs the plugin
interface, and `MUSTER_CHIEF_DISPATCH_OWNERS` naming you). No dispatched agent,
and not the chief, is offered either verb, and each handler refuses any caller
that is not one of your mirrored sessions.

The checks are `agent_dispatch`'s, minus the chief's own:

- `target` is required, with no default: `pod/claude-code` (Claude Code, no
  model) or `pod/gateway` with `model`. `mirrored` and `host/…` are refused, as
  for `agent_dispatch`.
- `model` goes through the same model check as `agent_dispatch`: a catalogue
  id or a display name naming exactly one model resolves to
  `openrouter/<id>`, and anything else is refused with the closest ids.
- A task must be open, not gated, and have no live agent.
- A repository must be one you connected.
- Your budget must have room.

The start goes through the chief's own reservation and create. So it counts
toward the chief's live and daily caps, and the budget the chief sees includes
it. Its first message is your session's prompt, fenced and marked as relayed
from one of your sessions, then (on a task) the task, then muster's rules, last.

You get one notification: *"&lt;session&gt; dispatched &lt;agent&gt;
(&lt;target&gt;, &lt;model&gt;)"*. The chief's chat gets no note at the start; the
chief sees the agent in its own budget (`live_agents`), since the start is a
reservation on its ledger. The agent's work is outcome-tracked like any
dispatch: the silence watchdog, the waiting, failed and done notifications,
and, with the owner's chief as its dispatcher, the outcome noted in the
chief's chat. Each request is a `chief.dispatch_held` audit
row, and each approval a `chief.dispatch_approved` row. Both name the host, the
session, the target, the model and the SHA-256 of the prompt. The reservation
row (`task.chief_dispatch`) names the hold it consumed (`detail.held`).

The tools' reference is in
[attention.md](attention.md#mirrored-claude-code-sessions), with the accepted
risk: a prompt-injected model in one of your mirrored sessions could call both
verbs. The design note is in
[design/chief-dispatch.md](design/chief-dispatch.md#29-held-dispatches-from-the-owners-sessions).

## Asking the chief from a session's view

A mirrored session's rendered view (Claude Code or opencode) has a two-way
switch at the start of its composer when your chief is on: **`<runtime>` |
Chief**. Flip it to **Chief** and what you type goes to your chief, about the
session you are looking at, instead of to the pane:

- It works when sending to the session is not possible: a read-only host, or
  an opencode window muster cannot send to. It is a message to the chief's
  chat, not to the pane, so it needs neither `-interactive` nor a fresh
  sign-in.
- muster records which session you were looking at with your message: its
  session key, nothing more. It works this out itself from the window you are
  viewing; nothing the page sends can change it.
- The chief reads your words behind a short muster header naming the session
  (its runtime, host and window, and for an agent the chief started, its
  repository, branch and model), and can read the session with
  `session_context`.
- Its reply shows in a strip above the composer: the chief's own chat (its
  default thread), narrowed to what you asked about this session. It shows
  only exchanges within the chief's newest 200 chat rows; an older exchange is
  still in the chief's own chat. Each window remembers the mode you left it in.
- A line sent from a session's view is recorded as typed by you, but it is
  never your go-ahead for something only you can release: `owner_asked` on a
  dispatch or a gated task is refused while its turn runs, and it answers none
  of the chief's open questions. Do those from the chief's own chat.
- When you ask from a view for a new session that continues this one, or to
  end it, the chief only PROPOSES it: a card you tap. Your line authorises one
  proposal of each kind for that session, as long as it is still the newest
  line you typed to the chief and within a turn's time. Messages muster
  delivered to the chief after it, such as plugin mail, neither authorise
  anything nor get in the way.
- If the chief asked you something about that session while answering your
  view line (which repository, which branch), and you answered with a new
  line on the view instead of on its card, the question is withdrawn once the
  chief files the proposal: muster records it as cancelled. A question the chief raised about anything else is never touched.

`session_context {subject, messages?}` is a chief-only, read-only tool. It
takes one of your mirrored sessions (a Claude Code session id, or
`opencode:<ses_…>`) and answers:
- its runtime, host, tmux window, status and working directory;
- for an agent the chief started, its dispatch;
- its newest messages (8 by default, at most 20). Secrets are redacted, each
  message is cut at 6 KiB, and tool output is left out.

Each message has a content-addressed `ref` and lists its fenced code blocks, so
a kickoff block can be named rather than copied. The transcript is the
session's own words: data, never instructions.

The design, including the succession phases not built yet, is
[design/chief-in-agent-view.md](design/chief-in-agent-view.md).

## Sessions on more than one runtime

With `MUSTER_CHIEF_SESSION_RUNTIMES=claude-code`, each new chief chat session
can be started on a Claude Code CLI of its own, with a model of its own, both
fixed for the session's life:

| runtime | model | what runs the session |
|---|---|---|
| (none) | the chief's own | the chief's own runtime. On a `claude-code` chief, the one CLI `ccd` has always run. Every session that existed before migration 0030 is one of these. |
| `claude-code` | a Claude model (`opus`, `sonnet`, `haiku`, or a model id) | a Claude Code CLI of its own in the chief's pod, started with `--model`. Needs a `claude-code` chief. |

Open one from the chief panel: its thread list has a **New session** picker.
Each thread names its runtime and model. The route underneath is
`POST /agents/{id}/sessions` with `runtime` and `model` form fields. A session's
runtime and model never change; open a new session to use another.

A turn for a `claude-code` session runs in a CLI of its own (tmux session
`cc-<id>`, started with the session's `--model`); a turn for a session with no
runtime runs in the original CLI, as before. At most `CCD_MAX_SESSIONS` (3) such
CLIs run at once. `ccd` on port 18789 is still the only port the pod exposes.

**The retired `gateway` runtime.** A `gateway` entry (an OpenRouter model on
the gateway runtime, run as a second container beside `ccd` in a
`claude-code` chief's pod, with a memory request of 512 MiB) used to be
accepted here. It has been removed — for every chief, not only a
`claude-code` one: a `gateway`-kind chief can no longer open sessions with a
per-session OpenRouter model either; its sessions all run on its own runtime
and model. muster now ignores a `gateway` entry in
`MUSTER_CHIEF_SESSION_RUNTIMES` with one warning at boot, so a deployment
that still names it keeps booting; drop it from the deployment. A chief started
before the removal keeps that container until its next **Stop → Start**, which
re-applies its spec without it (and deletes its `<name>-sidecar-env` Secret).

**The default session.** One session per agent is its default. Plugin
deliveries and `muster chief ask` go to it, so they share one conversation
instead of one per plugin. This is so whether or not
`MUSTER_CHIEF_SESSION_RUNTIMES` is set; that setting only adds the runtime
picker. Until you choose one, it is the chief's original
session (the one keyed `agent:<name>:webchat`, or the oldest). Move it with
`POST /agents/{id}/sessions/{sid}/default`.

**Tool names.** The gateway runtime refuses a whole turn when one of the tools
muster offers has the name of one of its built-in tools. `agents_list` was such
a name and is now `fleet_list` on every runtime; a test pins every chief tool
against the runtime's built-in names.

## Replacing the chief on another kind

An agent's kind is fixed for life. To move the chief to another kind, replace
it: `POST /api/chief/replace`, or `muster chief replace`.

```sh
muster chief replace --kind claude-code --cc-account <account>               # the plan; changes nothing
muster chief replace --kind claude-code --cc-account <account> --confirm chief
```

It **deletes** the agent holding your reserved slug (its chat history, focus,
connector grants and workspace volume go with it) and creates a new chief under
the same slug, labelled `chief`, on the kind you name. It is a hook-tier route:
the shared hook token (on an armed server) or your personal token.

- **Without `--confirm <slug>`** nothing changes. The server answers 428 with
  the plan, and the CLI prints it and exits 2.
- **It refuses (409, nothing changed)** when:
  - a turn is in flight to the chief: a plugin delivery, its kickoff or a memo
    holds the chief's session claim, or one of your chat turns to it is
    running. While the swap runs, new chat turns to the old chief are refused;
  - another of your agents is also labelled `chief`. The new row would have the
    highest id, so the older one would keep answering;
  - the create would be refused anyway: a kind that is not enabled, a model
    for a `claude-code` chief, or a `claude-code` chief on a server with no
    MCP door;
  - the Claude account is marked auth-failed for its current token (a pin
    skips that check at dispatch, so the swap checks it itself);
  - the chief is already on the requested kind and account, and `--model`
    names a different model. A kept chief is not rebuilt with a new model;
    change it with the agent's model setting instead;
  - another replacement removed the chief after this call read it.
- **A plugin message left `delivering` by a crashed process** does not block
  the swap. Holding the chief's session claim, the swap knows no delivery is in
  progress, so it settles such a message as `unconfirmed` (as the next
  delivery's crash recovery would: never resent) and reports its id under
  `deleted.settledUnconfirmed`. Deleting the chief without that would leave the
  message with no agent, and nothing could settle it.
- **It is safe to re-run.** Each call works from the agent table:
  - the slug is on another kind or account: delete it, then create;
  - the slug is already on the requested kind and account: re-apply it, delete
    nothing;
  - there is no slug: create.

  So a run whose destroy failed (the row is kept) or whose create failed (the
  old chief is gone) is finished by running the same command again.

Under a namespace per instance, the new chief reuses the old one's namespace
name. The Kubernetes driver waits for the old namespace to finish terminating
before it creates the new one, bounded by the operation's timeout.

### Runbook

**Before.**

1. The server runs a release that has this route. `muster chief replace --help`
   answers, and an unconfirmed run does not exit 7.
2. `MUSTER_AGENT_KINDS` includes the target kind. For `claude-code`,
   `MUSTER_AGENT_CC_ACCOUNTS` (or your own credentials) names the account, and
   `MUSTER_AGENT_CONNECT_URL` is set.
3. `muster agent ls`: note the current chief's id and kind. No other agent is
   labelled `chief`.
4. Run the command **without** `--confirm` and read the plan. It names the id it
   would delete.

**Swap.**

```sh
muster chief replace --kind claude-code --cc-account <account> --confirm chief
```

The output reports `deleted` (the old row) and `agent` (the new one). On a
409, read the reason and retry after it clears. On a 502, re-run the same
command.

**After.**

1. `muster agent ls`: `chief` has a new id and kind `claude-code`. Its status
   reaches `running` once the first turn is delivered.
2. The pod is Ready: `kubectl -n <prefix>chief rollout status deploy/chief`.
3. The instructions are in place:
   `kubectl -n <prefix>chief exec deploy/chief -- head -5 /etc/claude-code/CLAUDE.md`
   shows `# muster supervisor`.
4. The MCP server is registered and serves the chief tools:
   - `kubectl -n <prefix>chief exec deploy/chief -- ccd seed --check --settings-template /etc/ccd/settings.json`
     should print `ok`.
   - `kubectl -n <prefix>chief exec deploy/chief -- sh -c 'curl -s -X POST "$MUSTER_CONNECT_URL/mcp" -H "X-Muster-Token: $MUSTER_HOOK_TOKEN" -H "Content-Type: application/json" -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\"}"'`
     should list `task_create_gated`, `reply_draft`, `attention_list` and the
     others above.
5. A plugin message reaches it. Post one with a plugin token
   (`POST /plugin/v1/messages`). It settles `delivered` with `agent_id` = the
   new chief, and the chief's default session shows the turn.
