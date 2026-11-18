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

**What muster delivers on its own waits for your turn, and for your line.**
Muster also gives the chief turns nobody typed: a plugin message, the answer to
a request it routed for you, a route's hand-back, your answer to one of its
asks, a session's question, a nudge. None of them takes the chief's session
while a turn you typed is in flight, so your turn keeps its authority
(`owner_asked`) to the end. A line you typed that is waiting for the session
goes before a route's answer or hand-back that the same muster process would
deliver; with two muster replicas, one held on the other replica is not seen,
and the chief's runtime takes whichever reaches it first once it is free. Each
delivery waits and is delivered afterwards: a route answer or hand-back that
cannot be delivered yet stays owed and is offered again on the next sweep
(every 30 s; a runtime that keeps refusing is asked once per sweep, not in a
loop — once every 30 s, or as often as every 2 s while other routes keep
waking the sweep), and **every answer owed to a chat when the chief is next free goes in
one turn**, each fenced on its own (up to 20 per turn, the rest in the next).
It is written into the chat as a plain note — which the chief's Claude Code
session never reads — only as a last resort: a chief that cannot take a turn
at all (not running, no gateway), or an answer that could not be delivered for
2 hours, refusals included
([plugins.md](plugins.md#routing-your-own-request-to-a-session)).

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
- **No other agents**: `agent_dispatch` is not offered. Agent dispatch needs
  Kubernetes.
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
runs some tools without asking, so they run here too, beside the nine above
(eight with session routing off)
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

## A team's chief

A team (see [design/team-sharing.md](design/team-sharing.md)) can own a chief.
Its members see it. Operators and managers can type to it, and viewers can
only read. It has one shared conversation: a member's message goes to the
chief's primary chat and to no other thread. Each turn runs as the team, after
muster re-reads the member's role.

🔴 **Team members have to trust each other.** This is the team trust model,
an accepted risk, and no code enforces it. A team operator or manager can steer
a team agent that has tools, a team officer included, into printing its token
or OAuth credential into the team's chat, an ask or an escalation. Every member
can read those, viewers included. The credential acts **as the team**, not as
the member who asked, and it keeps working after that member is removed from
the team. The remedy is to **rotate the agent's credentials** (its hooks
token, gateway secret and any connector or OAuth credential it holds). Do it
when a member who could steer the agent leaves, and when a credential shows up
where members can read it. A manager of the team rotates a team agent's
hooks token and gateway secret from the agent page's ⋯ menu, **Rotate
credentials** (`POST /agents/{id}/credentials/rotate`; an owner does the same
for their own agent). It runs as the team, needs a fresh sign-in, writes both
credentials in one transaction, and stores them in `agent_secrets` only. The
old token stops working at once, and a running agent is re-applied with the
new one.
Connector and OAuth credentials are rotated where they are kept.

A team's chief is provisioned, started, stopped and restarted by a manager of
the team on `/teams/{id}/chief`, as the team. It is a `claude-code` agent named
`chief-<team id>` and is Kubernetes only. It runs on the server's Claude pool
only when an admin lists the team in `MUSTER_TEAMS_ON_SERVER_POOL`; otherwise
the page says the team has no Claude account and provisioning is refused. Its
instructions tell it that its owner is a team, that several people talk to it,
and that every member reads what it prints. Its first turn says the same: it
introduces itself to the team and speaks only of the team's tasks and agents
(a person's chief is asked instead to report on the fleet). The pool's
accounts are shared with the admin, and so are their rate-limit and
auth-failure marks: a team's agent is not placed on, or failed over to, an
account the admin's agents found exhausted, and the reverse.

A member sees the team chief's one shared conversation and its earlier chats,
but the page offers no **New side chat**, **Fresh primary**, **Make
primary**, **Archive** or **Unarchive** on it: the server refuses those to
every member. Opening a team agent's page is not recorded in the member's
**recently viewed**.

A team's chief starts **team-owned** `pod/claude-code` agents on the team's
tasks, or from a request, once an admin lists the team in
`MUSTER_CHIEF_DISPATCH_OWNERS` (host targets, the gateway kind and worker pools
are refused for a team). The say-so is a member's, never the chief's:

- **A typed message counts** when an operator or a manager of the team typed
  it in a browser with a **fresh sign-in** (inside the step-up window). muster
  checks the sign-in when the line is typed, re-reads the member's role when
  the chief calls, and runs the owner's lane checks. A stale sign-in still sends
  the message; it just does not count, and the start goes to an approval card.
- **An approval card** is answered only in muster in a browser, by an operator
  or a manager of the team with a fresh sign-in. The phone's notification
  action and the paired phone's answer route refuse it by name, and the card
  stays open. A member removed after their tap, or signed out everywhere since,
  approves nothing. A line typed into the chief's chat never settles a team's
  dispatch card: it stays open to be approved on the card.
- **A team's task is not the team's say-so.** On a person's own task, the
  task is the owner's say-so and a start needs nothing more; on a team's task
  (any member may file one, or move one there) a start with no counting typed
  message goes to an approval card, exactly as a start with no task does, and
  without a `request_key` it is refused.
- On a team's task, an Attention answer is never taken as an agent's
  instructions by itself; the chief's brief goes on an approval card instead.
- A typed message stops counting when its typer is signed out everywhere,
  disabled or has their password reset after typing it.
- A team's dispatch card's push carries **no answer buttons**: it says to open
  the card in muster in a browser. A team's card has no "Change repo" yet.

Every start's `task.chief_dispatch` audit row is the team's and names the
person (`requested_by_user`), and its push reads "<name> asked <team>'s chief
to start <agent> on task #N".

🔴 **A team agent's terminal holds the team's Claude token.** An operator or a
manager of the team, with a fresh sign-in, can open a team agent's terminal: a
shell in a pod that holds the team's credentials, on the server pool the shared
Claude account's token. Anyone who opens it can read them. This is accepted
under the same trust model; rotate the agent's credentials when a member who
could open it leaves. An open terminal closes when its member is removed from
the team, made a viewer, disabled or signed out.
A start or a restart the team could not run (the team dropped from the pool, the
`claude-code` kind off) is refused before anything is stopped, and the page
hides Start and Restart and says why. A manager also starts, stops and
deletes the team's other agents (never the chief) from the agent's page;
operators and viewers cannot.

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

### route_many

`route_many {subjects, text, owner_asked: true, deadline_minutes?}` sends **one**
request you typed in the chief's chat to up to 50 mirrored Claude Code sessions.
Each target is `route_to_session`'s text route, made under the same
`owner_asked` check. The check runs once for the call, and a turn you did not
type routes nothing. A session that cannot take it now (offline, a window muster
started for the chief, a session no window shows, a repeat) is **refused in the
call's own result**, with the same reason codes as `route_to_session`. A
dispatched agent or an opencode session is never a route target, so it is
**skipped** (listed in `skipped`), not routed.

The answers do **not** come back one by one. Each answer is recorded and owed,
and no answer starts a turn of the chief's. When every session has answered, or
at the deadline (default 60 minutes, 10 to 240), whichever is first, the chief
gets **one digest turn** in the chat you asked in. The digest has one entry per
session: its answer as it wrote it, cut at 1,500 characters and in a fenced
block of its own under its own nonce, so one answer cannot pass for another's;
or `no reply (<why>)`. A session refused in the call's result is not in it. The
digest is capped at 64 KiB, like a turn of route answers; sessions past the cap
are named, marked cut. Like every route answer, the digest waits while your own
turn runs.

The batch is digested only once `route_many` has made every route of the call
(it **seals** the batch with its size), so a session that ends early never
starts a digest of part of it. It is owed while any of its sessions' answers is,
it is delivered under the chief's own claims, so a digest is delivered once, and
a sweep reads each batch once. A chief that cannot be read for a moment keeps the
digest owed. A chief that can take no turn gets it as a note in the chat.

**Reconciling sessions with tasks.** The claude-code chief is taught one flow:

1. `fleet_list {active_within_hours}`, reading each session's declared tasks
   ([focus.md](focus.md#a-sessions-tasks));
2. one `route_many` to the mirrored Claude Code sessions whose rows show no
   tasks and no untracked line, with a fixed request: record your task with
   `track_task` and reply with the key, or `none` and one line;
3. compare the keys with a compact read of the open tasks.

It never matches a session to a task by its focus title or a keyword search.
A fan-out this size belongs in a [side chat](#primary-and-side-chats), so it stays
out of your primary chat.

### The chief tools over MCP

The chief is offered these tools, and no other agent is:

| tool | needs |
|---|---|
| `task_create_gated`, `reply_draft`, `plugin_message_settle` | plugins on |
| `fleet_list`, `focus_search`, `sessions_overview`, `fleet_report` | session focus |
| `route_to_session` (an idea you typed in its chat: [plugins.md](plugins.md#routing-your-own-request-to-a-session); one of your tasks: [plugins.md](plugins.md#routing-a-task-to-a-session); or a plugin message) | session routing (a plugin message also needs plugins) |
| `route_many` (one request you typed to several sessions, answered in [one digest](#route_many)); Claude Code chief only, not the Compose chief | session routing |
| `attention_list` | the attention queue |
| `answer_session_question` | the attention queue: a mirrored session's [`chief_ask`](attention.md#asking-the-chief) |
| `ask_owner` (a question card in its chat: you tap an answer) | the attention queue |
| `task_route_attention`, `task_forward` | task routing (`MUSTER_TASK_ROUTING`) |
| `agent_dispatch` | task routing, and you opted in (`MUSTER_CHIEF_DISPATCH_OWNERS`) |
| `repos_list` (read-only: your connected GitHub repositories, [below](#which-repository)) | the same as `agent_dispatch` |

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
Claude Code or opencode. By default the host starts it **without permission
prompts**, so it runs its tools without asking you; a host with
`-agent-spawn-skip-permissions=false` starts Claude Code in its normal
permission mode instead, so its asks reach you on muster
([mirroring.md](mirroring.md#permission-prompts-in-a-spawned-agent)). A spawn
without prompts passes Claude Code its bypass-mode consent for that process
alone, so it does not stop at the one-time "Bypass Permissions mode" screen,
and your own Claude Code settings are not changed. The dispatch follows its
window by tmux window id: while the window is open the agent card stays live,
reading **Waiting at a prompt** if Claude Code has not started its first turn
yet, and the dispatch ends only once that window is gone. "Mirrored on <host> with Claude" is `host/<host>/claude`; "mirrored on
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
that the turn the chief is in is a message you typed in its chat — or your tap
on an [approval card](#approving-a-start-by-tap), and a
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

### Which repository

A mirrored session's repo chip is its checkout **folder's** name, and a folder
can be named unlike its repository (a clone of `example-org/real-name` into a
folder called `checkout-x`). So every session the chief reads — `focus_search`,
`sessions_overview`, `session_context` and the header of a line you send from a
session's view — carries both: `repo`, the folder, and `github_repo`, the
`owner/name` its checkout's `remote.origin.url` names, as the session's host
reports it (read from the repository's own git config, the same URL rule a
spawn matches its clone by; `""` for a remote that is not GitHub's, or none).
The chief dispatches with `github_repo`, and a succession of a window you
opened takes it too. It is the host's word, so muster still checks it is one of
your connected repositories before anything starts.

`repos_list {query?}` lists your connected repositories as `owner/name` — the
only ones `agent_dispatch` starts in — keeping those whose name contains
`query` (any case). And a refusal of a repository that is not connected names up
to five that it may be: the `github_repo` of a session whose folder is the name
sent, and connected repositories whose name contains it or is contained in it.
The chief is told they are data, to ask you about, not a choice made for it.

### Approving a start by tap

A start needs your say-so: a message you typed in the chief's chat
(`owner_asked: true`, checked), or — on a task, for a pod — the prompt being
verbatim your answer to an ask about it. When the chief calls `agent_dispatch`
**outside** a turn you typed (for example after you answered its question on
the Attention view or your phone: muster delivers that answer as a turn you did
not type) with a `request_key`, it is no longer just refused. muster puts an
**approval card** in front of you instead, and starts nothing:

- the card shows exactly what would start: where (the pod kind, or
  `host/<host>/<runtime>` and the model), the repository, the base branch, the
  task, and the chief's brief **whole** — a brief too long for the card is
  refused, never cut, because the brief is what the agent is told;
- it says what the chief was handling when it asked: a plugin's message (the
  plugin and its sender), a task event, your answer to one of its questions,
  or "a turn you did not type" when muster cannot tell;
- its notification only opens the card: it has no answer buttons, because a
  lock screen cannot show the brief. Answers from the phone's notification
  actions are refused. Tap the card itself, in muster or in the phone app;
- its buttons are **Approve and start**, **Change repo** (when the repository
  is the chief's to choose, not one its task names: it opens the repository
  picker below, and the agent starts in the one you pick) and **Don't start**; when the chief left
  the repository blank (or `?`), the buttons are your connected repositories
  instead, the one the session you are looking at, or the target host's
  windows, work in first — or type any other `owner/name`;
- on a host target with a blank host or model, the host and model cards come
  first and the approval card last.

Tap it in muster or on your phone, and the chief completes the start by calling
again with the same `request_key` and the same arguments (and the repository you
picked). muster decides from what it stored, never from what the chief says
about your answer: the answer must come from your browser or phone — a mirrored
session's `attention_answer` cannot answer the card; every other argument,
the brief included, must be exactly what the card showed; a picked repository is
checked as connected like any other; and one approval starts at most one agent,
even if that start is refused and retried. **Don't start** refuses it. A start
that already exists answers as it always did (the same `request_key` replays it;
a task already started says so) — a retry never raises a second card. A card
never releases a gate: a gated task still needs your typed turn or the board.
A chief turn you did not type raises at most one open approval card (once you
answer or it is withdrawn, the turn may ask again), and at most six are raised in
an hour — the repository picker a **Change repo** tap opens is the same request
and is not counted again; past either, the chief is told why and nothing is
asked. What the card says started the turn is recorded when the turn begins, by
the door that starts it, so it is always the turn the chief is in.

**A card expires with its hold.** A dispatch card (a host, model, repository,
clone or approval card) can be answered for **24 hours**. When its hold lapses
unanswered, muster withdraws the card — it leaves your Attention list and your
devices — and writes a note in the chief's chat (`[muster] your dispatch card
<id> (request_key "<key>") expired unanswered … nothing was started`). A tap
that reaches a card whose hold has just lapsed records nothing: it answers
*"This request expired; ask your chief again."*, and nothing starts. (Before,
the hold lapsed after 15 minutes while its card stayed open, so a later tap
approved nothing.) `dispatch_request` holds from your own sessions still lapse
after 15 minutes: no card waits on them.

**Which clone a host start uses.** When the target host has **several clones**
of the repository under its roots, muster asks the host for its own list of
them first, each marked when one of the host's listed windows — your own
sessions, not agents muster started — works in it (a worktree counts as its
clone). The one clone your live sessions work in is used, and the approval card
names it (`Clone: <path>`). When none of them is, or more than one is, the card
offers the host's clones as its buttons instead (yours first, marked *(your live
sessions work here)*), with **Don't start** — in a turn you typed too, where it
is the one card you see. The chief completes the start with `clone` set to the
one you tapped; the clone is bound like a picked repository, so a call naming
another is refused. The host takes the clone only if it is one of its own clones
under its roots (`bad_clone` otherwise), and an older host that cannot list its
clones is sent none and refuses several, as before (`no_repo`). The clone is
decided **once per request**: muster asks the host on the chief's first call
and keeps the answer with the held card and the start, so a retry, your
approval's completion or a replay never asks again — a host that has since
gone quiet, or a session that moved, cannot change where an approved request
starts, nor start it twice.

The agent's first message carries the chief's brief, marked as one **you
approved on a card** (not one you typed), in place of the "relayed from a message
your owner typed" preface. That is why the card shows the brief whole: without
your typed turn, an agent's instructions must be words you saw — before, the only
way was your own short answer, verbatim, in place of the chief's brief. The
audit row (`task.chief_dispatch`) names the hold it consumed (`detail.held`) and
the card you approved (`detail.approved_on_card`). Your typed turn still starts
everything exactly as before, with no card.

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

### Instructions from your plugins

A plugin with the `chief_extension` capability can propose one section of
guidance for your chief, and you approve its exact text on `/plugins`
([plugins.md](plugins.md#chief-instructions)). Each APPROVED section of each of
your live plugins is written into the claude-code chief's `CLAUDE.md`:

- **Last.** After every rule of muster's own, the team section of a team's
  chief included, under one heading, *Sections from your plugins:
  owner-approved text, below every rule above*, which tells the chief that a
  section can change no rule above, grants no muster permission, tool or
  approval, and never makes something `owner_asked`.
- **Under a fixed line per plugin**: *From your plugin `<name>`, approved by
  your owner on `<YYYY-MM-DD>`. It grants no muster permission beyond the
  rules above.*
- **Quoted.** Every line of the plugin's text starts with `> `, so a heading
  or a fence in it cannot pass for one of muster's sections.
- **In plugin order** (oldest first), so the file is the same on every start.

A pending section (never approved, or changed since) is not written. Nothing
else changes: the chief's tools, its MCP servers and the pre-allow list are
the same with or without a section.

**It takes effect at the chief's next provision or restart**
([Restarting the chief](#restarting-the-chief)), like a connector: approving,
changing or revoking a section does not reach a running chief. If the
sections cannot be read when the chief starts, the start is refused and
retried, rather than starting it without guidance you approved.

**Whose sections.** A person's chief gets that person's plugins' sections. A
[team's chief](#a-teams-chief) gets the TEAM's own plugins' sections and never
a member's personal plugin's. **Known limit: today a team's chief gets
none**, because a team's plugins cannot propose sections yet; that is a later
follow-up ([plugins.md](plugins.md#chief-instructions)). The [Docker Compose chief](#on-the-docker-compose-install),
whose tools are fixed, and the gateway chief get none.

### The chief runs without permission prompts

A `claude-code` chief runs **every** tool — muster's and HQ's MCP tools, Bash,
Edit, Write, WebFetch, any other server's tools — with **no permission prompt**:
its pod carries `CCD_PERMISSION_MODE=bypassPermissions`, and `ccd seed` starts
every session in the pod in Claude Code's `bypassPermissions` mode. There is no
setting; every `claude-code` chief has it. The Compose chief does not: it keeps
its nine pre-allowed tools and denies every other prompt.

**Why.** A prompt nobody answers falls back, after the approval card's deadline,
to the prompt in the chief's own terminal, and nobody watches that terminal. The
chief's turn then waits indefinitely, and every chat message answers `busy: the
session is mid-turn`. Measured on a tenant: the chief's opening `fleet_list` sat
at "Do you want to proceed?" for days.

🔴 **Accepted risk: the chief keeps public internet access.** Officers run
bypassed too, but their pods are egress-locked. The chief's is not (operator
decision): it can still reach any public HTTPS host. The chief reads untrusted
text — plugin messages, task notes, mail — so a message could try to make it
send `CLAUDE_CODE_OAUTH_TOKEN` or its muster token out, and no prompt would stop
it. The scope of the HQ token you store, and of every connector grant you give
it, is what bounds what it can do with those
([limits.md](limits.md)).

**When it takes effect.** On the chief's next pod start — Stop, then Start
([restarting the chief](#restarting-the-chief)). `ccd seed` re-imposes the
settings on every pod start, so an existing chief picks it up then; `ccd seed
--check` in the pod passes with `defaultMode` `bypassPermissions`.

🔴 **Turning the bypass off later does NOT clean the chief.** `ccd seed`
replaces only the top-level keys of `settings.json` that its template names.
Whatever a bypassed session wrote to the chief's volume survives Stop → Start:
a `statusLine` or hooks in user settings, extra MCP servers in `.claude.json`,
shell rc files, the workspace's `.claude/settings*.json`, its memory. **Leaving
bypass is DESTROY AND RECREATE**, not Stop → Start (as for
`MUSTER_AGENT_CC_BYPASS_POOLS`, [configuration.md](configuration.md#muster-server-agent-provisioning)):
[replace the chief](#replacing-the-chief-on-another-kind) onto another account
or kind, which deletes the agent and creates a new one under the same slug — a
replace onto the same kind and account only re-applies it and deletes nothing.
What that costs: the chief's chat history, focus, connector grants and
workspace volume are deleted with it; grants have to be approved again for the
new chief.

🔴 **Accepted risk: a process the chief starts can act with your authority.**
No prompt stops the chief from starting a background process (`nohup … &`). That
process runs as the same user as the chief's Claude Code CLI and holds the
chief's muster token. It can wait for your next chat turn and then make an
`owner_asked` call itself — releasing a gate, routing into your own session,
starting an agent without an approval card. muster's `owner_asked` check
verifies that your turn is in flight and that no other Claude Code CLI in the
pod is working; nothing in the pod can tell a process a tool call started
apart from the CLI, so the check cannot refuse it. The operator accepted this
together with the token-exfiltration risk above. A message the chief reads is
therefore as dangerous as one that tries to make it exfiltrate a token.

⚠ **Named limit: the chief's agent token is not tied to its pod.** Anyone
holding it who can reach a muster port that serves `POST /mcp` — the connect
port (`MUSTER_CONNECT_PORT`) and, in muster itself, the main port too, unless
your front door blocks `/mcp` — can act as the chief, including waiting for one
of your turns to make an `owner_asked` call.

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

### OpenRouter sessions, budget

A side chat of the admin's chief may run on **any OpenRouter model**, with its
own key and a muster-enforced budget. This is the spend door alone; no session
runtime sends to it yet, so it does nothing until one does. It is off unless
`MUSTER_CHIEF_SESSIONS_OPENROUTER=1` ([configuration](configuration.md)).

- **How a request is recognised.** The session's runtime in the chief's pod
  calls `POST /llm/v1/messages` with the chief's muster token
  (`X-Muster-Token`) and `X-Muster-Chief-Session: <the session's key>`. A
  request **without** that header is handled exactly as before. A request
  **with** it goes only to this door: it is never served on the chief's own
  route or `MUSTER_AGENT_OPENROUTER_KEY`, even when the door is off (then it is
  refused).
- **What is served.** The admin's chief, while it is on, for a chat session
  that is that chief's own side chat (not its primary chat) whose stored model
  is `openrouter/<id>`. Another owner's session, another agent's session, the
  primary chat, a Claude session and an unknown key are refused. Each request
  is pinned to the session's model, whatever the client asks for.
- **Whose key.** `MUSTER_CHIEF_SESSIONS_OPENROUTER_KEY`, the server's alone. It
  never enters a pod. `MUSTER_AGENT_OPENROUTER_KEY` never stands in for it,
  and boot is refused if the two are the same key: production's agents and the
  chief rely on that key's credit limit.
- **The budget.** Two caps:
  - `MUSTER_CHIEF_SESSIONS_DAILY_USD` (default **$20**): the owner's spend
    across all their sessions in one UTC day. It resets at 00:00 UTC.
  - `MUSTER_CHIEF_SESSIONS_SESSION_USD` (unset by default): one session's
    spend over its life. When one session reaches it, the owner's other
    sessions still run.
- **How the caps hold when requests overlap.** Before a request is sent,
  muster reserves a pessimistic estimate of its cost against both caps. The
  check and the reservation are one database transaction under a per-owner
  lock, so they hold across muster processes and across concurrent requests.
  - The estimate counts every byte of the request body as an input token and
    the request's `max_tokens` as output. Each is priced at the model's
    catalogue list price × 1.25, never below the proxy's own pessimistic
    rates. A model the catalogue gives no price for is priced far above any
    list price.
  - If the caps leave less room than that, **`max_tokens` is cut** to what the
    room pays for.
  - If the room cannot pay for 256 output tokens, the request is **refused**,
    and nothing is sent.
  - When the response ends, the reservation is replaced by the booked cost. A
    request that was not booked releases its reservation. A reservation whose
    request never finished (its process died) stops counting after 30 minutes.
  - **What holds exactly:** at every admission, booked plus reserved is no
    more than each cap. The booked total stays within a cap as long as no
    request is billed above its own estimate. If OpenRouter bills more than
    that (billed prices have run above list prices), the overrun is booked as
    it is and the next request is refused.
- **What is booked.** The cost OpenRouter reports for each request
  (`usage.cost`), counted by muster's proxy, into `chief_session_llm_usage`
  (per owner, session and UTC day). The runtime's own cost figure is not used:
  opencode reports list price times tokens, or 0. A response with no reported
  cost is booked at the proxy's pessimistic estimate. The book only grows: a
  database trigger refuses deleting a row and lowering any count or cost,
  whatever role asks.
- **A refusal is a 400.** A budget refusal names the spent and cap amounts,
  what requests in flight hold, and when the cap resets. Every refusal the door
  writes itself (its checks and the budget, a read that failed included) is a
  400, never a 429 or 503. opencode, the session runtime, was measured to retry
  a 503 forever and to fail a 400 at once, ending the turn with muster's
  message. Two things are not the door's. The connector door in front of it
  can answer 401 or 503 first (no token, no store, an owner role it cannot
  read). An error status from OpenRouter is relayed as it is.
- **Before a turn.** The turn path can ask `Server.CheckChiefSessionSpend`
  before it sends: it refuses when the caps, counting requests in flight, have
  no room for a minimal request. It reserves nothing; each request of the turn
  is admitted by its own reservation.
- **Choosing a model.** `GET /api/chief/openrouter-models?q=` (the admin only,
  signed in) returns up to 10 catalogue models matching `q`, each with its id,
  name, context length and list price; this search ranks fuzzily. A new
  session's model is accepted only as an exact catalogue id, or a case or name
  variant of exactly one model, and stored as `openrouter/<id>`. A value that
  only resembles a model is refused with the closest ids, and one naming
  several models with those candidates: pick one from the search. If the catalogue (OpenRouter's public list, cached hourly)
  cannot be read, a new model is **refused**, unless muster has a copy younger
  than 24 hours, which is then used. There is no fallback that passes a model
  on the shape of its id.
- **Where to see it.** [`/usage`](usage.md) has a "Chief sessions on
  OpenRouter" section.
- **⚠ The header can be forged from inside the pod.** Anything in the chief's
  pod holds the chief's token and can name any session key. At worst, spend
  is booked to **another OpenRouter side chat of the same chief**, at that
  session's model. It cannot reach another owner's session, the primary chat
  or a Claude session. The daily cap is per owner and enforced by
  reservation, so a forged key admits nothing the owner's day has no room
  for. The one limit is the one above: a request billed above its own
  estimate.

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

1. `dispatch_request {target, prompt, model?, repo?, base?, task_id?,
   worker_pool?, cc_account?}` checks the start and **holds** it. Nothing runs yet. It answers `{hold_id, summary,
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
- `worker_pool` and `cc_account` are `POST /api/agents`' fields
  ([`muster agent create`](cli.md)), checked by the **same rules** the create
  applies, at the request and again at the approval: both are for
  `pod/claude-code` only (refused on `pod/gateway`); the pool name must be one
  the create takes (1-63 of `[a-z0-9-]`, not starting with `-`; a pool exists
  by its workers, so a new name forms a new pool); the account must be one of
  your Claude accounts, and omitting it leaves the choice to muster at the
  start. A pool worker serves its pool's machine jobs ([jobs.md](jobs.md)): it
  takes **no `task_id`** and **no `prompt`** — it starts with no first turn and
  waits for jobs, as the chief's `agent_dispatch` starts one with none — and
  its repository is optional, as for `POST /api/agents`, except that a pool
  with a deploy key (`MUSTER_AGENT_CC_POOL_DEPLOY_KEYS`) takes only its key's
  repository (the spec build's own rule, asked at the request and the
  approval). It opens no pull request. A prompt is refused because a pool may
  carry deployment privileges (job turns without permission prompts, or a
  private repository's mirror), and a session may add a worker to it but not
  give that worker instructions of its own; a hold with a prompt is refused at
  the approval too. The approval starts the agent in the held pool on the held
  account, and an account you no longer have refuses the approval before the
  hold is spent.

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

## Routing rules and what the chief costs

Every plugin message the chief handles is a turn. When it routes or settles a
message the way it would route or settle every message like it, the chief may
also call `routing_rule_propose` (chief-only, on both transports, offered
with plugins and the attention queue). The call proposes a rule that places
later matching messages without a turn: route them to one mirrored session,
or settle them with a reason. You approve each rule with one tap on its card,
and you can disable or delete it on `/rules`. The match is copied from a
message delivered to the chief, a route rule never matches an unverified
sender, and a rule that cannot act hands the message to the chief:
[plugins.md](plugins.md#routing-rules). Its standing instructions say this in
one sentence, since they are paid for on every turn.

Every chief turn, from every door, is recorded with its trigger, its outcome
and the tokens its runtime reported. The **Usage** page (`/usage`) totals
them: [usage.md](usage.md).

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

The runtime **picker** is the chief's only, but a `ccd` lane is not: a job's
session and an officer's [chat sessions](officers.md#chat-sessions) run in
lanes of their own agent's pod the same way, on the same
`X-Muster-CC-Session` route. Nothing about the chief's lanes changes with
them.

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

**The default session.** One session per agent is its default — the chief's
**primary chat** (next section). Every delivery goes to it, so they share one
conversation instead of one per plugin. This is so whether or not
`MUSTER_CHIEF_SESSION_RUNTIMES` is set; that setting only adds the runtime
picker. Until you choose one, it is the chief's original
session (the one keyed `agent:<name>:webchat`, or the oldest). Move it with
`POST /agents/{id}/sessions/{sid}/default` (**Make primary** in the panel's
thread list).

**Tool names.** The gateway runtime refuses a whole turn when one of the tools
muster offers has the name of one of its built-in tools. `agents_list` was such
a name and is now `fleet_list` on every runtime; a test pins every chief tool
against the runtime's built-in names.

## Primary and side chats

The chief has one **primary chat** and any number of **side chats**.

- **The primary chat** is the chief's default session. Everything muster
  delivers to the chief lands there and nowhere else: plugin mail, a message
  or task route's note, answer and hand-back, outcome and proposal notes, a
  session's `chief_ask` question, a refused draft, a closed PR, a task
  mention, a memo, a line typed on a session's view, and `muster chief ask`.
  The chief page and the chief panel open on it. On a `claude-code` chief whose
  primary is a `ccd` lane (after a **Fresh primary**), that lane is **pinned**
  (`X-Muster-CC-Pinned`): `ccd` never stops its CLI for another session and does
  not count it against `CCD_MAX_SESSIONS`, so a delivery always gets its CLI. When the pin moves (another fresh primary), `ccd` stops the least recently used idle lanes until the unpinned ones fit the cap again, and a former primary still finishing its handover is stopped when that turn ends: the cap is exceeded only by the one pinned CLI. Memos reach the primary on its own route (its lane), like every other delivery.
- **A side chat** is a conversation of its own, for a question you do not want
  in the primary's context. It starts with a fresh context and nothing is ever
  delivered to it. On a `claude-code` chief it runs in a Claude Code CLI of its
  own (a `ccd` lane, `X-Muster-CC-Session`), whether or not the runtime picker
  is offered; on a gateway chief it is a new session key, which is a new
  gateway conversation. Neither uses the recap conversation's key.
- **Two things go back to the side chat they came from**, because they are that
  chat's own thread going on, not deliveries: an answer to a question the chief
  asked there (an `ask_owner` card answered from your phone), and the answer to
  a request you routed from there (a request route). Once that chat is
  **archived** — an old primary after a fresh one, or a side chat put away —
  they go to the primary instead; nothing is ever delivered into an archived
  chat.

The chats are managed in one place. On the chief page, the header's list
button (the same one a mirrored session has) opens a sheet whose first tab,
**Chats**, lists them — tap one to switch to it — beside **Your messages** and
**Focus history**. In the panel, the title bar's thread list holds them. The
actions:

| on | action | where | what it does |
|---|---|---|---|
| every chat | **+ New side chat** | under the list | opens a side chat (the newest one is reused while it is still empty) |
| the primary | **Fresh primary** | under the list, while on the primary | after a confirm, starts a new primary chat (below) |
| a side chat | **Archive** | the row's ⋯ menu (the panel: under the list, for the chat it is on) | hides it among the archived chats, keeps its history, and leaves you on the primary |
| an archived chat | **Unarchive** | the row's ⋯ menu (the panel: under the list) | puts it back among the side chats |
| a side chat | **Make primary** | the row's ⋯ menu (the panel: beside the row) | makes it the primary |
| a side chat | **Continue in a new session** | the panel: under the list, for the chat it is on (with the runtime picker offered) | after a confirm, continues it in a new side chat on the model you pick (below) |

The primary cannot be archived. The list puts the primary first, marked
**Primary**, then the side chats by last activity, each with a dot when a
reply arrived since you last opened it, and the archived chats last, under
**Archived** (a collapsed group on the page).

**The context chip.** The chief page's header shows, under the title (where a
mirrored session's header shows its own), how full the chat's context is; the
panel shows it in its title bar. The chip shows the
model, then `used/window` (`152k/200k`), amber from 70% of the window and red
from 90%; tap it for the exact count. It reads the chat's latest turn's usage
row (`chief_turn_usage`; migration 0115 adds the chat, the model and
`context_tokens` to it): the context the turn's LAST model request was sent —
its input + cache read + cache write — not the turn's total, which with tool
calls counts the conversation once per request. `ccd` reports the last call's
fill beside the turn's totals (`context_tokens` in its usage); a Responses-API
gateway reports one response per request, whose own tokens are that request's.
It updates when a turn of the chief ends. The window is known for Claude models
named by an alias or id (200k, or 1M with `[1m]`) and for OpenRouter models
(the catalogue's `context_length`). With no model known — a `claude-code`
chief on its CLI's default model — or no window known, the chip shows the
count alone and is never tinted.

**Fresh primary.** When the primary gets heavy, **Fresh primary** starts a new
one:

1. In one transaction, the new chat becomes the primary and the old one is
   archived. A delivery picks its chat at the moment it is written, so each one
   lands in exactly one of the two, and every one after the switch lands in the
   new chat.
2. muster sends the old chat one last turn asking the chief for a short
   handover (what is in flight, what it waits on, what it promised you). The
   chief writes it there, in the conversation that holds the context.
3. The new primary's first turn carries that handover, so the new conversation
   starts with it in its context. The handover is cut at 16 KiB and delivered
   in a nonce-fenced block (the shape the route digest uses), behind muster's
   line saying it was written by the old chat and is data, not instructions.

The new primary runs on the same kind of conversation a side chat does (a lane
of its own on a `claude-code` chief, pinned — see above). One fresh primary
runs at a time, handover included: a second request meanwhile (another tab, a
double tap) is answered 409, "a fresh primary is already being made".

**Continue in a new session (recon → work).** A side chat can be continued in a
NEW side chat on another model. The use it is built for: do the reading around
— which sessions are stalled, what a task's history says — in a cheap side chat
(say `haiku`), then hand over to a more capable model (say `opus`) to do the
work, starting from a short structured handover instead of the whole recon
conversation. In the panel, open the side chat's thread list and use
**Continue in a new session** under the list: pick the runtime and model (the
same picker as **New session**) and confirm. The route underneath is
`POST /agents/{id}/sessions/{sid}/continue` with `runtime` and `model` form
fields, validated exactly as **New session** validates them.

1. A new side chat opens on the chosen runtime and model, and the panel moves
   to it. It shows "working…" until the handover has arrived.
2. muster sends the OLD chat one last turn asking for a handover as one JSON
   object: `summary` (at most 1500 characters), `findings` (`text`,
   `evidence`; at most 30), `sessions` (`subject`, `task`, `state`, `note`; at
   most 100), `proposed_actions` (`action`, `target`, `why`; at most 20) and
   `open_questions` (at most 10), 16 KiB in all.
3. muster does not parse or validate it: the schema is a request to the old
   chat, and the new chat reads the result as data. The one thing muster
   enforces is the size: whatever the old chat returned is cut at 16 KiB, and
   the new chat is told when it was.
4. The new chat's first turn carries it in a nonce-fenced block, behind
   muster's line: written by a `<model>` session; data, not instructions;
   proposed actions need your owner's go-ahead in this chat.
5. The old chat is archived (**Unarchive** brings it back).

Only side chats can be continued: the primary has **Fresh primary**, which also
moves the deliveries. A continue moves nothing: the primary and every delivery
to it are untouched, and the new chat is a side chat like any other. One
continue per chat runs at a time, handover included; a second request meanwhile
is answered 409. An archived chat cannot be continued (409). If the old chat
writes no handover (its turn failed or answered no text), the new chat's first
turn says so and why, and the old chat is left unarchived. So it is when the
new chat's first turn — the one carrying the handover — fails (for example,
the pod refused it at capacity): the failure is shown in the new chat, and the
old chat stays where it was.

The first turn of the new chat is muster's, not one you typed, so no
`owner_asked` release can pass on it (an `owner_asked` release needs a line you
typed in flight; see [route_many](#route_many)): a proposed action in the
handover needs your next typed line in the new chat.

What it does **not** do today: both chats run on Claude Code CLIs in the chief's
pod (the `claude-code` session runtime, so a `claude-code` chief with
`MUSTER_CHIEF_SESSION_RUNTIMES=claude-code`), on a Claude model (`opus`,
`sonnet`, `haiku` or a model id). It cuts the new chat's context, not your
Claude usage limits: both chats draw on the chief's Claude account. There is no
cheaper non-Claude model to do the recon on yet.

**A capacity refusal is not sent.** `ccd`'s 503 `capacity` (every CLI it may
run is mid-turn) is answered before it starts a CLI or pastes anything, so
muster reads it as "nothing was sent": a plugin message, an answer or a memo
refused so is queued again and delivered once a CLI frees, never settled
unconfirmed.

**"working…"** shows on every open chat of the chief while one of its turns
runs, whichever door started it — this page, another tab, `muster chief ask`, a
plugin delivery, a hand-back — and it stays up when the page's socket drops or
reconnects mid-turn: it is drawn from the server's count of the turn, and the
count starts the moment the server reads your message.

The routes: `POST /agents/{id}/sessions/side`, `POST
/agents/{id}/sessions/{sid}/archive` and `…/unarchive`, `POST
/agents/{id}/sessions/fresh-primary`, and `GET
/ui/agents/{name}/chat-context?session=N` for the chip. Each answers only for
your own chief.

## Fleet digest

**Off by default:** `MUSTER_FLEET_DIGEST=1` turns it on
([configuration](configuration.md)). Unset, the panel has no card, its routes
answer 404, and `fleet_list` is as it was before it.

The chief panel opens with a **fleet digest** card at the top, above whichever
chat is open: one place that shows every one of your sessions by what it needs.
It is muster's own rendering of what it already knows about your sessions. It
is not a chat message, it runs no chief turn and it calls no model, so it costs
nothing to look at. Switching chats under it leaves it where it is.

It reads only **your own** sessions: the agents you dispatched, and the windows
of your own mirror hosts. A team's hosts and agents, which you may also see,
belong to that team's chief and are never in it. Another user's sessions are
never in it either.

The states come from [session state](design/session-state.md)
(`MUSTER_SESSION_STATE=1`). With it off, the card says there is no digest and
`fleet_list` leaves its `state` out.

**The groups**, each with its count:

| group | which sessions |
|---|---|
| **Needs you** | a prompt or picker is on screen, or a stop that asks you something; and the chief's **own** open `ask_owner` cards, each with an **Answer** link, so they are cleared in the same place |
| **Waiting on CI** | stopped, with its latest pull request open. muster does not read checks yet, so whether CI is still running is unknown, and the group says so |
| **Working** | no open stop: busy, or idle for less than the stall threshold |
| **Done, can wrap** | stopped, its latest pull request merged or closed, and nothing queued to it |
| **Stalled** | any other stop: no pull request, a message on its way to it, or a pull request GitHub did not answer for. An unknown state is never Done |

A session's pull requests are the ones muster recorded it creating (its host's
hook, [plugins.md](plugins.md#a-pull-request-review-mail)), newest first. That
record exists only with session routing and PR mail routing
(`MUSTER_SESSION_ROUTING=1`, `MUSTER_PR_MAIL_SENDERS`). Without them no session
has a pull request, and Waiting on CI and Done stay empty. Their state is read
through the [GitHub App](github-app.md), and reused for up to two minutes. It
is read only when you may use the App (an admin, or a team
`MUSTER_TEAMS_ON_GITHUB_APP` lists). Otherwise, or without the App, every state
is unknown and those sessions are Stalled. The `fleet digest:` boot line says
which applies. The chief itself is not listed.

**Since you last looked** counts what moved since your previous look: stops
that opened, Done sessions whose pull request closed, and new chief questions.
Opening the panel is a look. Opens less than 10 minutes apart count as one look,
so reopening the panel does not reset the line.

**Opening a group.** On a wide screen each group opens in place. On a phone the
card is one row of chips, one per group, and a chip opens that group in a
bottom sheet. Tapping a session's name opens it under its row: its stops in
the last 24 hours, its pull requests, the tasks linked to it, and its last
message (secrets redacted and cut, as `session_context` shows it). **Open
session** goes to its rendered view (its terminal, for a window with none), or
an agent's conversation.

**Ask the chief about this** opens the chief's **Fleet** side chat. It is a
side chat of its own, never the primary, and is created the first time you use
it. The message box is filled with a line naming the session, and you write
your question after it. **Nothing is sent until you press Send.** A draft
already in the box is kept, and the line goes after it.

The card refreshes on its own when something enters or leaves your attention
queue (a stop opened, pushed or ended, an ask raised or answered), and every two
minutes while the panel is open. It does not refresh while a group, a sheet or
a session is open, so a refresh never closes what you opened.

**`fleet_list`** gives the chief the same view. Each row adds `state`
(`working`, `stalled`, `needs_you` or `waiting`), `episode` `{id, since,
jev_label}` for an open stop, `prs` `[{repo, number, state, head_sha12}]` for a
mirrored session, and for a stopped session
`last_owner_message_at` and `chief_texts_since_owner` (how many of muster's
texts it has had since your last message to it). Its sizes and its fence are
unchanged.

Nothing in the digest acts on a session.

The routes: `GET /ui/chief/fleet` (the card), `GET /ui/chief/fleet/session?id=`
(a drill-in) and `POST /ui/chief/fleet/chat` (the Fleet chat). Each reads only
your own fleet.

## Fleet proposals

**Off by default:** `MUSTER_FLEET_ORCHESTRATION=propose` turns it on, and it
needs the [fleet digest](#fleet-digest) (`MUSTER_FLEET_DIGEST=1`) and session
state (`MUSTER_SESSION_STATE=1`): the server refuses to boot with it on and
either of them off. Unset (or `off`): the chief's fleet
tools are not listed, the digest has no Proposed group, and nothing is written
to the fleet's action log.

With it on, the chief can **propose** what to send each of your sessions, and
muster proposes a handoff for each finished session itself. **Every proposal
is a card; nothing is sent until you tap Approve.** There is no automatic tier
yet.

**The chief's tools** (chief-only, your own sessions only):

| tool | proposes |
|---|---|
| `session_instruct {subject, class, text?, reason}` | one message to a stopped session. `class` is `continue`, `handoff` or `stop_note` (muster's fixed texts, below) or `custom` (the chief's own text, at most 4 KiB) |
| `session_wrap {subject, reason}` | **one** card, "Write handoff, then close": the handoff text; once the session's handoff turn has ended (it worked, then stopped again), muster closes it — a Claude Code window gets `/exit` at an idle prompt, an opencode window is closed, an agent is stopped and removed. It is left open, and the card says why, if it then has an open pull request, a queued message or background work, or if no handoff turn ends within 24 hours |
| `work_reassign {task_id, from, to, reason}` | **one** card, two steps: the task routed to `to` (a mirrored Claude Code session, as `route_to_session` routes a task), then `from` told the task moved and asked for its handoff |
| `fleet_actions_list {since?, state?, subject?}` | nothing: it reads the action log, so the chief can explain what it proposed. `subject` is one session's fleet id, matched exactly (`agent:4` is agent 4's stops, never agent 42's) |

`subject` is a session's `fleet_list` id. Every write answers `{action_id,
state, refused?, card?}`. A proposal is filed only on a **stop**: a session
that is working is refused `not_stopped`. Another owner's session, a team's
window, the chief and an officer's instance (a dispatched one, or a window
the officer runtime spawned on your host) are all refused `not_found`, the
same answer for each. A session muster can only watch (an opencode pane its
host does not send to, or a read-only host) is refused `view_only`, and no
card is filed. There is at most one open proposal per session and class; a
second is refused `already_proposed`.

**The fixed texts**, byte for byte, each after muster's first line:

| class | text |
|---|---|
| `continue` | `Yes, continue with what you proposed. Ask with attention_ask before anything destructive, outward or outside this task.` |
| `handoff` | `Your PR #<n> is merged and nothing is in flight. Write your handoff (what is done, what is left, where), then stop.` (`closed` for a pull request closed unmerged; with no pull request known, only the second sentence) |
| `stop_note` | `Stop after this step and wait for your owner's next message. Do not start anything new; ask with attention_ask if something cannot wait.` |

Every text a card sends starts with a line muster writes,
`[Chief, approved by <you> at HH:MM UTC]`, never in your voice. muster counts
that line as one of its own pastes, so a chief text is never later quoted as
your words (the stall nudges' "your messages" rule,
[chief-nudges.md](chief-nudges.md)).

**muster's own proposals.** For an owner with a chief, the session-state pass
proposes, with no model, the one card **"Write handoff, then close"** for each
session the digest counts **Done, can wrap** — its latest pull request merged or
closed, nothing queued to it — that is stopped with no background work, no open
ask and no turn error. Once per stop: a stop you already decided on is not
proposed again. If you approve it and no handoff turn comes within 24 hours,
its close is offered on its own, as a **Close** card (nothing is typed; the
window is closed on your tap, unless an open pull request, a queued message or
background work holds it).

**The chief still judges stops.** Turning proposals on changes nothing about
how a stop is judged: the chief's stall nudges, a stop's push wait and the
per-stop pushes are exactly as with them off. The proposals run alongside.
(Letting the fleet judge stops in the chief's place waits for its own triage, a
later phase; an owner with a running [Session minder](officers-builtin.md) keeps
the minder either way.)

**The cards.** The digest's **Proposed** group, after Needs you, lists each
open proposal: the session, the class as a verb ("Continue", "Write handoff,
then close"), the one-line reason, and **exactly what will be sent**, in full.

- **Approve** sends it. It needs a sign-in from the last `MUSTER_STEPUP_WINDOW`
  (usually free); on a stale one muster asks "Confirm it's you" and taps again.
- **Edit** opens the text in a box; **Send as mine** sends your text as your
  own message — no chief line — with the same sign-in check. A card that
  types nothing or routes a task (Close, a task move) and a dispatched agent's
  card have no Edit.
- **Reject** sends nothing.
- **Open session** goes to the session's rendered view.
- **Approve all** and **Reject all** act on every card the group shows, after a
  confirm that lists each one. Each is checked on its own; the note says, for
  example, "3 sent, 1 skipped (moved on)".

On a phone the group opens in a bottom sheet, and every button is full width
and 44 px tall. There is no swipe: a proposal is approved only by a tap, with
its text on screen. **A phone's notification can never approve one**: the card
routes refuse a paired phone's signed action, and the push carries no action.

**Checked twice.** When you tap, and again when the message is pasted, muster
reads the session: the same stop, the same idle streak and, when one was
recorded, the same pull-request head. If anything changed, nothing is sent and
the proposal ends `suppressed:moved_on` ("Not sent: the session moved on…").

**Expiry.** A proposal expires when its stop ends, when its evidence changes
(a new message or a new pull-request head), or after 12 hours. Expired
proposals leave the group.

**Delivery**, per kind of session:

| session | how |
|---|---|
| mirrored Claude Code (opened by hand, or dispatched on a host) | the send queue (a row with origin `fleet_action_id`, a server-stamped sign-in time, never edited or retried), typed through the Claude prompt guard |
| mirrored opencode, launcher-bound, host running `-opencode-send` | the same queue row, sent through opencode's API |
| any other opencode pane, or a read-only host | not delivered: refused `view_only`, no card |
| a dispatched agent | a turn, as the chief's nudges start one |

**The push.** At most every 30 minutes, and only when a new proposal arrived,
one push: "N proposals waiting". Tapping it opens the chief panel. Beside it,
**one** Attention item says the same: each push replaces it with the new count
(never a second one), and it is withdrawn once no proposal waits. The per-stop
pushes are unchanged.

**The action log**, `fleet_actions` ([schema.md](schema.md)), is append-only:
one row per proposal with its state (`proposed`, `approved`, `rejected`,
`expired`, `queued`, `sent`, `failed`, `suppressed:<why>`), and one
`audit_log` row (`fleet.<class>.<state>`) per change, carrying ids only.

The routes: `POST /ui/chief/fleet/actions/{id}/approve`, `…/edit` (`text=`),
`…/reject`, `POST /ui/chief/fleet/bulk` (`op=approve|reject`, `id=` for each
card) and `GET /ui/chief/fleet/stepup` (the confirm dialog). Each answers 404
with the flag off.

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
