# Officers

An **officer** is a standing agent with a job description instead of a task: a
release captain for one service, a triage officer for one repository. Its
durable identity, its work queue, its charter and the lock on each item live in
**HQ** (an external server that speaks the officers API below). muster runs the
officer's **instances**: Claude Code pods it wakes when HQ has work, gives the
officer's tools, and scales to zero when the queue is quiet.

| | lives in |
|---|---|
| the officer (id, type, area, charter, max instances) | HQ |
| the queue of items, and who holds each one | HQ (Postgres; one claim at a time, under a lease) |
| the officer's HQ token | muster-server's environment — never an instance's pod |
| instances (pods, volumes, Claude accounts) | muster, one agent per instance |

Officers **watch, triage, record and dispatch**. They do not merge, deploy or
release: those stay with humans. An instance holds no production credential.
The one exception is a [shipping officer](#shipping-officers), which merges
its own pull requests and tags releases in its one repository — still holding
no credential, and only through two muster tools that check every gate.

The code: `internal/officers` (configuration, HQ client, the wake loop's
policy, the first message), bound to muster in
`internal/api/officers.go` (instance agents, the MCP tools,
delete protection).

## Configuration

```sh
MUSTER_AGENT_KINDS=gateway,claude-code        # officers run on claude-code
# REQUIRED with officers: the connect listener the instances reach their tools on
MUSTER_CONNECTOR_ENCRYPTION_KEY=…              # (the connector store's key)
MUSTER_CONNECT_PORT=8091
MUSTER_AGENT_CONNECT_URL=http://muster.<namespace>.svc:8091
MUSTER_OFFICERS='[
  {"id": "release:example-web",
   "hq_url": "https://hq.example.com/api/v1",
   "max_instances": 1,
   "idle_scale_to_zero_after": "30m",
   "poll_interval": "1m",
   "model": "opus",
   "repo": "example-org/example-web"}
]'
MUSTER_OFFICER_TOKEN_RELEASE_EXAMPLE_WEB=…     # from a Secret; see below
MUSTER_GITHUB_APP_ACCESS=read                  # with the GitHub App: officers only ever read GitHub
```

| field | default | |
|---|---|---|
| `id` | required | HQ's officer id, `<type>:<area slug>`: lowercase letters, digits and single hyphens on each side of the colon. HQ allows 54 characters; muster refuses, at boot, an id whose highest instance agent name (`officer-<type>-<slug>-<max>`) would not fit a namespace under the deployment's `MUSTER_AGENT_NAMESPACE_PREFIX` (63 minus the prefix: 50 under `muster-agent-`). |
| `hq_url` | required | HQ's API base, including `/api/v1`. No credentials in the URL. |
| `max_instances` | `1` | 1–8. Each instance is a pod, a volume and a Claude account (which the [account failover](#account-failover) may change). |
| `idle_scale_to_zero_after` | `30m` | How long the officer must be idle before its instances are stopped. |
| `poll_interval` | `1m` | How often HQ's open items are read when nothing else wakes the loop. At least `10s`. |
| `owner` | `1` | The muster user the instance agents belong to. |
| `cc_account` | the pool's choice | Pin the Claude account instances run on. A pinned instance is not moved to another account when this one hits its limit, unless `cc_failover` is set. |
| `cc_failover` | `false` | With `cc_account`: let a pinned instance fail over to another account anyway ([account failover](#account-failover)). Without `cc_account` it changes nothing: an unpinned instance fails over by default. |
| `model` | the account's default | The model the instances' Claude Code sessions run: an alias (`opus`, `sonnet`) or a full model id (`claude-opus-4-5-20251101`, `opus[1m]`). Checked at boot. Written as the session's settings `model` (below). |
| `repo` | none | The GitHub repository (`owner/name`, the same shape check as a task's repository) the instances may **read** through the `github` connector — see [Reading a repository](#reading-a-repository-repo). Not checked out. A value that is not `owner/name` is a boot refusal naming the officer. |
| `connectors` | none | Connector names, besides `github`, the instances may request — e.g. a [postgres connector](connectors.md#postgres-connectors) for read-only SQL through `postgres_query`. Declaring one grants nothing: each instance's request is approved by hand, never from a shared approval ([below](#reading-a-repository-repo)). `github` itself, and a name that is not a connector name, is a boot refusal. |
| `ship` | none | Lets the officer push, merge its own pull requests and tag releases in `repo`, through muster's gates: [Shipping officers](#shipping-officers). |
| `transcript` | off | `{"enabled": true}` forwards each instance's live session transcript to HQ while it holds a claimed item: [Transcript to HQ](#transcript-to-hq). Strict: any other key is a boot refusal. |

Each officer's token is read from `MUSTER_OFFICER_TOKEN_<NAME>`: the id
upper-cased with every other character written as `_`. A declared officer with
no token, an unknown field, two officers whose agent names would collide, or an
officer on a server without the `claude-code` kind or an agent provisioner, is
a boot refusal that names the problem — and so is **any officer without the
connector door**: an instance reaches its tools only over muster's MCP server,
which is the connect listener, so `MUSTER_AGENT_CONNECT_URL` (and with it
`MUSTER_CONNECT_PORT` and the connector store's key) must be set; without it
instances would boot with no officer tools, silently. The rows are in
[configuration.md](configuration.md#muster-server-officers).

### The release captain

`release:example-web` above is the first officer: HQ produces a
`release.evaluate` item when a release of the service is built and awaiting
approval (with the PR list and each PR's review verdict in the payload) and a
`release.post_report` item when it goes live. Its instance evaluates the
release — risk level, summary, what to monitor — and posts the evaluation to
HQ's captain route with `officer_release_evaluation` (muster attaches the
officer token, which HQ's captain route accepts for a release officer), then
completes the item. muster holds **no copy of the charter**: the instance loads it from HQ
with `officer_brief`, so the versioned charter in HQ is the only one. Approval
of the release itself stays a human's, in HQ.

## Instances

Instance *n* (1-based) of officer `<type>:<area>` is the muster agent named
`officer-<type>-<area>-<n>` (`officer-release-example-web-1`), kind
`claude-code`, with no task and no repository checkout (an officer's `repo`, when
it has one, is read through the `github` connector — [below](#reading-a-repository-repo)).
Instances are interchangeable:
they share the officer's queue and identity, and any of them can take any item.

Names starting `officer-` are reserved. muster's name generator cannot produce
one (its adjective-noun pool has no such word), and the officer runtime is the
only code that creates an agent under a fixed name of that shape. An instance
keeps its namespace, volume and Claude account across stops and starts.

### Account failover

When a turn of an instance fails `rate_limited` or `auth_failed`, muster moves
the instance to another of the owner's Claude accounts that is usable, exactly
as for any `claude-code` agent ([jobs: account
failover](jobs.md#account-failover)) — unless the officer pins `cc_account`
without `cc_failover`, or `MUSTER_AGENT_CC_FAILOVER=off`. The move restarts the
instance's pod (its session restarts; its volume, name and identity are kept),
and, before the pod restarts:

- the instance's **first turn is re-armed** (as when an existing instance is
  started), so once the new pod is ready the kickoff deliverer sends it the
  officer kickoff again and it loads its charter and works the queue;
- every **live claim it held is released**, so the item it was working when
  the limit hit is claimable at once, instead of after its lease lapses, and is
  retried by the restarted instance (or another).

There is no second instance needed and no automatic move back when the first
account resets. With every other account cooling down, nothing moves: the
instance stays on its account and the next turn after the reset works.

### The instance's Claude Code session

The instance runs in its pod's primary Claude Code session, and muster sets
three things in that session's `settings.json` at provision time (the pod's
`ccd seed` writes them from `CCD_PERMISSION_MODE`, `CCD_ALLOW_MCP_TOOLS` and
`CCD_MODEL`, which muster puts on the pod; `internal/podagent/session.go`):

- 🔴 **Every tool allowed: `permissions.defaultMode: "bypassPermissions"`.** An
  officer instance never stops on a permission prompt — not for its own tools,
  not for `Bash`, `WebFetch`, `Write` or any other. muster puts
  `CCD_PERMISSION_MODE=bypassPermissions` on every officer instance's pod, and
  `ccd seed` then sets `defaultMode` to `bypassPermissions`, drops the image's
  `disableBypassPermissionsMode` and records the bypass disclaimer's consent
  (`skipDangerousModePermissionPrompt`), so the CLI starts at its prompt with no
  dialog. Every session the pod's CLI starts runs that way. Why: an officer
  deployment may have no approval gate anyone answers, and an officer reads its
  repository with `git` and `curl` through muster's proxy; a prompt would stall
  it until its turn times out. What bounds an officer is therefore what it can
  *reach*, not what it may run: its MCP toolset (officer tools and the three
  connector tools only), the `github` connector only, every grant decided
  explicitly by its owner and minted read-only (below), and the pod's network
  policy. ⚠ Like a bypass job lane (`MUSTER_AGENT_CC_BYPASS_POOLS`), treat the
  WHOLE POD as running without approvals: anything in it, including the agent's
  own muster token (`MUSTER_HOOK_TOKEN`), is reachable by a command the session
  runs. Any value of `CCD_PERMISSION_MODE` other than unset or exactly
  `bypassPermissions` is refused and the pod does not start. **No other agent
  gets it**: task agents and the chief keep the image's posture (default mode,
  bypass disabled, prompts routed for approval). This needs a
  `claude-code-agent` image built from this version: an older `ccd` ignores the
  variable, and the officer would keep prompting.
- **`permissions.allow`: exactly the officer's own tools, then the connector
  tools** — `mcp__muster__officer_brief`, `…officer_items`, `…officer_claim`,
  `…officer_heartbeat`, `…officer_complete`, `…officer_release`, and for a
  `release:*` officer `…officer_release_evaluation`; then, when the connector
  store is configured (`MUSTER_CONNECTOR_ENCRYPTION_KEY`, so the instance is
  offered them), `…connectors_list`, `…connector_request` and
  `…connector_status` — and `…postgres_query` when the officer declares
  `connectors`. Redundant under bypass and kept: it names the muster
  tools the instance is for, and holds if bypass is ever lifted. The channel
  only takes bare tool names and always writes them as `mcp__muster__<name>`
  (a name holding `__` or anything but `[a-z0-9_]` is refused and the pod does
  not start). The template's `permissions` replace the file's on every pod start
  (`ccd seed`; an in-pod CLI restart by the supervisor does not re-seed), so an
  allow list or mode a session writes for itself does not survive the next pod
  start.
- **`model`**, when the officer sets one. Removing it from the officer's config
  takes effect at the instance's next pod start: seed removes the model it set
  itself (the instance keeps its volume, so it would otherwise persist), back to
  the account default. A model changed by hand inside the pod is left alone.

No other agent gets any of them (the officer runtime is the provisioner's only
source of them, and it answers for a declared officer's instances only).

### The instance's network

🔴 **An officer instance is egress-locked, always.** It is the one agent whose
whole session runs with permissions bypassed while it reads untrusted text
(public pull requests), and its pod holds `CLAUDE_CODE_OAUTH_TOKEN` and
`MUSTER_HOOK_TOKEN`. A claude-code agent's usual policy allows TCP 443 to any
public IPv4 address, so one injected `curl` could send either token anywhere.
So the lock is derived from the bypass (`agentspec.ClaudeCodeSession.EgressLocked`):
there is no setting that gives an officer the bypass without it, and no other
agent gets it.

On the Kubernetes driver, an officer instance's NetworkPolicy is the usual one
**without the public rule and without the DNS rule**:

- ingress: ccd's port (18789), from muster's own pods only (unchanged);
- egress: muster's connect port (`MUSTER_CONNECT_PORT`) on muster's own pods.
  GitHub reads (`git`, REST) and muster's MCP tools both go through that port.
- nothing else — **no name resolution, and not even the Anthropic API**.

The pod also gets `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`, so the CLI does
not try its auto-updater, telemetry, error reporting or feature-flag hosts.

🔴 **The cluster must allow DNS and the Anthropic API itself, by name.** A
NetworkPolicy cannot match a hostname, so muster writes neither rule. Ship a
policy for the officer instance pods that allows:

- DNS to the cluster's DNS pods (UDP/TCP 53) **with names filtered** — on
  Cilium, `rules.dns` on that port, so Cilium's DNS proxy answers only for
  `api.anthropic.com` and the cluster-local names the pod needs (muster's
  connect URL is a cluster-local Service name, and with the usual `ndots:5`
  search path `api.anthropic.com` is first tried with each cluster search
  domain appended, so the rule must match every name under the cluster
  domain at any depth — on Cilium a `**.` pattern; a single `*` matches one
  label only);
- `api.anthropic.com:443` — on Cilium, `toFQDNs`.

On Cilium both fit in one `CiliumClusterwideNetworkPolicy`. Without it every
turn of the officer fails (closed and loud). Network plugins union allows,
which is why both rules had to leave muster's policy: an FQDN allow next to
"any public address on 443" narrows nothing, and a name-filtered DNS rule next
to a plain "port 53 to the DNS pods" allow is widened back to every name.

What the CLI needs with `CLAUDE_CODE_OAUTH_TOKEN`: inference, its WebFetch
domain check and token use all go to `api.anthropic.com`. The docs
(<https://code.claude.com/docs/en/network-config>) also list
`platform.claude.com` for OAuth refresh of claude.ai logins; an env-supplied
`setup-token` is not refreshed, so it is not allowed here — if an officer's
turns fail with an auth error, that host is the first to check. claude.ai MCP
connectors (`mcp-proxy.anthropic.com`) are unreachable by design.

⚠ **An instance keeps the policy it was started with.** The lock is written when
the spec is applied (dispatch, Start). An officer instance created by an older
build keeps its public-443 rule until it is stopped and started — scale its
Deployment in its agent namespace to 0 and the wake loop starts it again on the
current spec. Check with
`kubectl -n <namespace-prefix>officer-<…>-1 get networkpolicy -o yaml`: the
egress list has no `ipBlock` and no port 53.

⚠ **What it still allows.**

- 🔴 **`api.anthropic.com`, at full bandwidth, for anyone with an Anthropic
  account.** The allow is by host name, not by credential: an injected session
  can call the API with an attacker's own API key and put the pod's tokens in
  the request body, where the attacker reads them back from their own account.
  Closing that needs an egress proxy that only forwards requests carrying the
  pod's own credential; muster does not ship one, so this channel is open.
- Anything the session can send to muster's connect surface (its own tools and
  the read-only `github` connector).
- Bypass still means the session can read every secret in its pod.

**DNS tunnelling is closed once the cluster policy above is live**: muster's
policy allows no port 53, so the only resolver path is the cluster's
name-filtered one, and a query for a name the attacker controls is refused. On
a cluster with no such policy the pod resolves nothing at all.

### Reading a repository (`repo`)

The GitHub App connector mints a token only for the **calling agent's own
repository**, and refuses an agent that has none
([github-app.md](github-app.md)). An officer instance is dispatched with no task
and no checkout, so without `repo` an approved `github` grant gives it nothing.

With `"repo": "owner/name"`, that repository is the instance's own for its
connectors. `Server.agentRepo` (`internal/api/officers.go`) is the one place
this is decided: for a declared officer's instance it is the officer's
configured `repo`, for every other agent its row's repository. Everything that
asks "which repository is this agent's" for a connector reads it from there:

- the proxy's caller (`connectors.Caller.Repo`), so the App mints a 1-hour token
  for exactly that one repository; any other repository is reached anonymously,
- the approve card's and `connector_requests_list`'s description of what
  approving grants (and `connector_request_decide`'s no-repository warning),
- the repository a grant is recorded as approved FOR (below),
- the App's problem line on the agent's card.

**`github`, and the connectors it declares, only.** An officer instance may
request and use the `github` connector and the connectors its officer lists in
`connectors` (`officers.MayRequestConnector`), and nothing else. A request for
any other connector its owner holds is refused with an error naming the officer — by `connector_request` and
`POST /agent/connectors/{name}/request` (400), and by the proxy (403, before it
would file an implicit request) — and an officer's request for another
connector filed before this rule cannot be approved (the tab and
`connector_request_decide` refuse it).

**Always read-only** — except for a [shipping officer](#shipping-officers),
whose requests go through its push gate and carry its OWN App's token instead
(nothing below applies to it). An officer's App token is minted with Contents, Pull
requests, Checks and Commit statuses **read** — whatever
[`MUSTER_GITHUB_APP_ACCESS`](github-app.md#read-only-mode) says
(`connectors.Caller.ReadOnly`, set for every officer instance). A deployment in
the default `write` mode keeps minting write tokens for its task agents, and
read tokens for its officers; the approve card and `connector_requests_list`
describe an officer's request as READ ONLY in both modes. A push or a
pull-request write through the proxy is refused by GitHub. ⚠ This limits the
**App**: when `github` is a stored connector (no App, or an owner who is not an
admin), the officer gets whatever that stored token may do — store a read-only
token for an officers deployment.

**No checkout.** The instance row carries no repository: a row repository is
what makes the pod's `ccd` clone it before the CLI starts and start the session
pre-trusted in it (running that repository's `.claude/settings.json`), and would
hold the instance's first turn until the `github` grant is approved. The officer
fetches what it needs through muster's proxy instead, as its first message says:

- **git**: `ccd connect-git github` routes git's `https://github.com/` through
  the proxy (ccd also does it by itself within 15 seconds of the approval), then
  `git clone --depth 50 https://github.com/<owner>/<name>.git`;
- **the REST API**: `curl -sS -H "Authorization: Bearer $MUSTER_HOOK_TOKEN"
  "$MUSTER_CONNECT_URL/connect/github/api.github.com/repos/<owner>/<name>/<path>"`.

**A grant is approved FOR a repository** (migration 0061). Because the
repository is read from the configuration and never stored on the row, changing
or removing `repo` takes effect for existing instances at the next request
(their first message is re-armed with the new text the next time the wake loop
starts them) — and the grant does NOT follow it. The approval records the
instance's repository at the time it was decided; when the officer's `repo` is
then different — another repository, none → some (an officer approved while it
had no `repo`, when the card said "approving grants NOTHING usable"), or some →
none — the grant is put back to **pending** at its next use (the proxy, or the
agent's `connector_status` / `connector_request`), audited as "re-opened: the
agent's repository changed (approved for …, now …)", and the owner is prompted
again, as for a changed credential source (0033). Nothing is minted for the new
repository until the owner approves it.

**Approving the grant.** Nothing is pre-granted: there is no automatic grant
for officers, and none for any other configured agent. The instance asks once
(`connector_request{name: "github"}`), and the officer's `owner` (default user
1, the operator) decides it **explicitly, for that instance**:

- the **Connectors** tab (`/connectors` on the officers muster, signed in as
  the owner): the request card names the instance agent
  (`officer-<type>-<area>-<n>`) and its reason; the `github` connector's entry
  states the token's scope. Press **Approve**. A pending request also sends a
  `connector` push notification when push is configured; or
- `connector_requests_list` then `connector_request_decide {id, decision:
  "approve"}` from one of the owner's own mirrored Claude Code sessions
  ([connectors.md](connectors.md#deciding-a-request-from-your-own-session)).

🔴 **Shared approvals never reach an officer**, in either direction
(`excludedFromSharing`, `internal/api/connector_standing.go`): no standing grant
— not one keyed on a stored connector's name alone, not one on the officer's
repository — approves an officer instance's request, and an officer's approval
is never shared. Each instance's `github` request waits for its own decision.
The App is an admin's: an `owner` who is not an admin is offered the stored
`github` connector instead, and the App mints nothing for them.

## Waking and scaling

One loop runs every officer. Each pass, for one officer:

1. read HQ: its `open` items and its `claimed` items;
2. read muster: which of its instance agents exist and are up;
3. decide;
4. release live claims held by an instance that is down;
5. start or stop instances.

The decision (`officers.Decide`, a pure function):

- **Claimable** is the open items plus claimed items whose lease has expired
  (the contract makes those claimable by anyone), less any item HQ marks
  `blocked: true` (its task waits on unfinished tasks, so HQ refuses the
  claim): a queue of only blocked items wakes nothing and keeps nothing up. An
  HQ that does not send the field counts every item. **Held** is claimed items
  with a live lease held by one of this officer's instances.
- **Busy** (anything claimable or held): want *held-by-up-instances +
  claimable* instances up, at most `max_instances`, and start the
  lowest-numbered down instances to get there. Nothing is stopped while busy.
- **Idle** (nothing claimable, nothing held): once idle has lasted
  `idle_scale_to_zero_after`, every up instance is stopped. A new item resets
  the window.
- **Re-prompt.** An up instance that has finished — not in error, holding no
  live claim, its first turn delivered — is sent a new turn ("new work: call
  `officer_items` …") when there is claimable work that no instance being
  started or recently prompted already covers. Without it an instance whose turn
  ended would never look at the queue again while it stays up, and the officer
  would neither work the new item nor go idle. Rate limit: one prompt per
  instance per 5 minutes (`officers.RepromptAfter`).

  **How it is sent.** As a turn of its own under the instance's **session
  claim** (`ClaimKickoff`), the claim a first turn and a cairn memo hold for a
  whole turn — the memo relay's pattern. If the claim is held, a turn is
  running (an instance works the queue inside one turn, which can last ~37
  minutes, and holds no HQ claim between items): the re-prompt is **not sent**
  and the loop tries again on its next pass, so it lands when that turn ends.
  It never writes the instance's kickoff state (`kicked_off`, `pending_note`,
  `updated_at`). An earlier version re-armed the first turn instead, and when
  the running turn outlasted the kickoff deliverer's 15-minute dwell bound the
  deliverer marked the instance `error` — permanently holding its slot. A turn
  started some other way without the claim (an operator's chat) is refused by
  ccd with `409 busy` before anything is typed, so the re-prompt fails cleanly
  rather than interrupting it. muster re-prompts rather than stopping an
  instance when its turn ends because it has no turn-ended signal for a
  dispatched session, and stop/start would cost a pod cold start per item.

**Up** means the instance's live status is running, provisioning or pending —
or `error`: a crash-looping instance keeps its slot rather than being started
again every poll (which would re-send its first message each time). A create or
start still in flight (the row says provisioning, the backend shows nothing yet)
also counts as up, so it is not started twice. An instance **entering or
leaving error is logged once per change** ("instance N is in error — it holds
its slot and is NOT restarted …"), and `muster_officer_error_slots` counts the
slots so held across all officers: the officer runs below target until the
instance recovers or an operator acts.

**Starting** an instance that does not exist creates it, with its first message
(below) as the kickoff. Starting one that exists **re-arms** its first message
(`agents.PGStore.RearmKickoff`: the pending note is replaced and the kickoff
markers reset, so the ordinary kickoff deliverer sends it once the pod is
ready) and starts it. **Stopping** scales it to zero; its agent and volume stay.

**Wake signal.** Polling HQ every `poll_interval` is the only one: muster has
no webhook receiver (a one-minute poll is fast enough for release work, and it
keeps a public route and an HQ→muster network path out of the design).

**HQ down.** A pass whose HQ read fails with a transport error or a 5xx
changes nothing — no start, no stop, no release — and the officer's next pass
waits `poll_interval` doubled per consecutive failure, capped at 10 minutes.

**HQ refuses the officer.** A 401 or 403 (the token is revoked, wrong or not
this officer's) or a 404 (HQ without the officer API, or no such officer), seen
on **2 consecutive passes** (`officers.FaultStrikes`; one alone is an HQ deploy
or a token cache mid-rollout, and changes nothing), is a **configuration fault,
not an outage**. An outage (5xx, network error) in between breaks the run — 404,
502, 404 confirms nothing — and does not clear a fault already confirmed; only a
successful answer does. It is logged loudly once per change
("HQ refuses this officer …: a CONFIGURATION fault"), counted on
`muster_officer_hq_faults`, and the officer's up instances are **scaled to
zero** (logged as `scaled instance N to zero (HQ configuration fault: 404)`)
rather than holding pods and Claude accounts for ever; the loop keeps
retrying with the same backoff and logs "HQ answers again" when it does. A pass that cannot see muster's own
instances (the provisioner is unreachable) is treated the same way: "I cannot
see" is never read as "nothing is running".

**Restarts.** Every start decision is a function of HQ's queue and muster's
instances, so a restarted muster makes the same decisions. The one thing kept
in memory is when the officer was first seen idle; a restart forgets it and
waits a full idle window again. That can delay a scale-down, never cause one.

**Shutdown.** A re-prompt being sent when muster shuts down is cancelled
with the other in-flight deliveries, releases the instance's session claim,
and is waited for before the process exits.

**More than one muster pod.** The loop is not leader-gated (no background loop
in muster is; see `cmd/muster-server`'s `startBackgroundLoops`). Two pods
starting the same instance is one agent create winning on the unique name and
one logged failure; two stopping one is idempotent.

### A claim held by a stopped instance

When an instance is stopped — by the operator, or because it crashed out —
while it holds a live claim, the next pass sees a held item whose holder is not
up and **releases** it (`POST …/release` with that instance's id), so another
instance can take it at once instead of waiting out the lease. The idle
scale-down cannot stop an instance holding a claim (a held claim is not idle).
An instance in `error` counts as up, so its claim is not released early; it
lapses with its lease. If the release itself fails (HQ down), the lease lapse
is the fallback.

## The tools

An officer's instances are offered these MCP tools on muster's MCP server, and no
other agent is (a third axis beside the chief-only one, enforced in the tool
listing and again at dispatch):

| tool | HQ call |
|---|---|
| `officer_brief` | `GET /officers/{id}/brief` — the charter, open items, recent outcomes, memory scopes |
| `officer_items` `{status?, limit?, cursor?}` | `GET /officers/{id}/items` (default `status=open`) |
| `officer_claim` `{item_id, lease_seconds?}` | `POST …/items/{item}/claim` |
| `officer_heartbeat` `{item_id, lease_seconds?}` | `POST …/items/{item}/heartbeat` |
| `officer_complete` `{item_id, outcome, summary, links?}` | `POST …/items/{item}/complete` |
| `officer_release` `{item_id}` | `POST …/items/{item}/release` |
| `officer_release_evaluation` `{service, tag, evaluation?: {level, summary, monitors?}, report?}` | `POST /releases/captain` — **release officers only** (`release:*`): HQ's captain body, checked locally, with `instance` set by muster. A 409 with code `officer_shadow` means the officer is in **shadow** mode: the full evaluation or report goes in `officer_complete`'s summary instead |
| `officer_merge_pr` `{pr}`, `officer_tag_release` `{item_id?}`, `officer_rollout_status`, `officer_mark_ready` `{pr}` | **shipping officers only** (a `ship` block): GitHub and HQ reads, then a squash merge, a release tag or a draft → ready conversion made by muster — see [Shipping officers](#shipping-officers) |
| `officer_ci_logs` `{pr}` | **shipping officers with a `ci_logs` block only**: no HQ call. GitHub and Kubernetes reads by muster-server; the failed CI tasks of the officer's OWN pull request, with log excerpts — see [`officer_ci_logs {pr}`](#officer_ci_logs-pr) |

`lease_seconds` defaults to 600 (muster always sends it; HQ's own default is
300) and is clamped to 30–3600. **Claiming an item you already hold** is not a
conflict: HQ refuses a claim on any live lease, the caller's own included, and
since an instance's id is its agent name a restarted instance re-claiming its
item would hit that. When HQ's 409 names the calling instance as `held_by`,
`officer_claim` heartbeats with the requested lease instead and answers with
the item, `already_yours: true` and the note "already yours — lease renewed"
(if the lease ran out between the two calls, the claim is retried once). A 409
naming another instance is returned as a refusal. **Claiming a blocked item**
(HQ's 409 `code: "blocked"`, `blocked_by`) is not a refusal either: nothing
was claimed, and the instance gets a plain result — `claimed: false`,
`blocked: true`, `blocked_by` and the note "not claimed: item N is blocked by
…; claim another open item". At most 10 `links`. `outcome` is `done`,
`escalated` or `no_action`; `summary` is at most 24,576 characters (HQ's bound; a shadow officer puts its whole evaluation or report there). HQ's JSON is
returned as is; a 4xx comes back as a refusal carrying HQ's status and body (a
409 on a claim names the holder), and an outage as "try again in a minute".

The `instance` muster sends HQ is the agent's name (`officer-…-<n>`), so the
same instance keeps its claims across a pod restart. For anything that needs a
human, an instance follows its charter: by default it finishes the item with
`officer_complete{outcome: "escalated", summary}` saying who must act and why,
and HQ notifies the officer's owner. It uses `attention_ask` only when its
charter says to — an officers muster usually runs headless, so an attention
ask there reaches nobody. Its MCP instructions and system prompt say the same,
in place of the generic attention paragraph every other agent gets
([attention.md](attention.md#what-agents-are-told)).

### The first message

```
You are officer <id>, instance <n> of <max> (<agent name>). …
1. Call officer_brief. It returns your charter …
2. Work the queue one item at a time: officer_items, officer_claim{item_id},
   officer_heartbeat{item_id} …, officer_complete{item_id, outcome, summary} …
3. Anything that needs a human: follow your charter. By default, finish the
   item with officer_complete{item_id, outcome: "escalated", summary} … — HQ
   notifies the officer's owner — rather than attention_ask … Use attention_ask
   only if your charter explicitly says to.
4. When nothing is open and you hold no claim, stop and wait …
```

An officer with a `repo` is also told that it may read that repository through
muster's `github` connector, the only connector it may use: request it once
with `connector_request`, wait with `connector_status` (the owner decides it by
hand), then read through the proxy either with git (`ccd connect-git github`,
then `git clone https://github.com/<owner>/<name>.git`) or with `curl` on
`$MUSTER_CONNECT_URL/connect/github/api.github.com/repos/<owner>/<name>/…`
carrying `Authorization: Bearer $MUSTER_HOOK_TOKEN`; muster holds a read-only
token, there is no checkout, and it never pushes or merges. Without a `repo` the
text is unchanged.

An officer that declares `connectors` is also told their names, that each is
requested once and decided by hand, and how to query a postgres one:
`postgres_query{connector, sql, params?}`, one statement per call in a
read-only transaction with a timeout and capped rows, reading `pg_catalog` to
check tables and columns (the github sentence then says "the only connector
you may use for it").

(`officers.KickoffText` has the whole text.) It carries no charter and no
credential.

## Shipping officers

An officer with a `ship` block may **write code to its `repo`**: push branches,
open pull requests, merge its own pull requests and tag releases. It holds no
GitHub credential for any of it. muster holds the officer's **own** GitHub App
key, filters every write the instance sends through the connector proxy, and
makes merges and tags itself, in two tools that check every gate at call
time (a third, `officer_mark_ready`, converts its own draft pull request to
ready for review). Nothing the officer can merge changes those gates: they are muster's
code and muster-server's environment, and the officer's App is installed on
its one repository only.

🔴 **Use it only where the repository enforces nothing else, and know that
muster is then the branch protection.** On a private repository without
branch protection or rulesets, any `contents: write` token can push to the
base branch, push any tag and merge with red checks. The design rests on that
token never reaching the instance (below).

The code: `internal/officers/ship.go` (configuration), `internal/officership`
(the push gate, the merge and release gates, the migration lint, the rollout
watch), bound to muster in `internal/api/officer_ship.go`.

### Configuration

A `ship` block in the officer's `MUSTER_OFFICERS` entry, which must also have
`repo`:

```json
{"id": "engineering:example-web", "hq_url": "https://hq.example.com/api/v1", "repo": "example-org/example-web",
 "ship": {
   "stage": "merge",
   "github_app": {"id": 123456, "account": "example-org:7654321"},
   "required_checks": ["ci / build", "ci / e2e", "ci / lint"],
   "review_check": "pr-review",
   "review_app": "example-review-app",
   "migrations": "internal/db/migrations/",
   "sensitive_paths": ["internal/auth/", "*token*", "internal/store/officer*", "Dockerfile*"],
   "notice": {"channel": "ch/example-alerts"},
   "releases_per_day": 6,
   "min_release_gap": "30m",
   "quiet_hours": {"tz": "America/New_York", "start": "22:00", "end": "08:00", "weekends": true},
   "rollout": {"health_url": "https://svc.example.com/healthz", "version_field": "version",
               "ok_field": "db", "ok_value": "ok", "rollback": "pr"}
 }}
```

| field | default | |
|---|---|---|
| `stage` | **required** | `shadow` (push and open pull requests; both gated tools refuse), `merge` (also `officer_merge_pr`), `release` (also `officer_tag_release`; needs `rollout`). There is no default. |
| `base` | `main` | The branch merges land on and releases are cut from. |
| `branch_prefix` | `officer/` | The only branches the instance may push and the only heads a merge may come from. Must end in `/` and must not overlap `base`. |
| `github_app` | **required** | The officer's OWN App: `id`, and `account` as `<login>:<numeric id>` (matched by id; the login must be the repository's owner). Never the deployment's App, so suspending it stops this officer and nothing else. |
| `required_checks` | **required** | 1–20 commit-status contexts (or check-run names) that must be `success` on the head commit. A status's `success` counts whatever its description says (a CI job that reports "success: skipped" passes). Missing is not passed. |
| `review_check` | **required** | The check run that must be `completed`/`success` on the head. `neutral` does not pass. |
| `review_app` | none | When set, only that App's check run of that name counts. |
| `migrations` | none | The directory whose files are linted for destructive DDL, and whose added files make a release need HQ's backup preflight. |
| `sensitive_paths` | none | Patterns whose change makes `officer_merge_pr` post a notice to HQ first (never a block by itself). `dir/` is a prefix, a pattern with `/` matches the whole path, one without matches any path segment. Needs `notice`. |
| `notice` | none | `channel` (the mail channel or address HQ posts the notice to) and `path` (default `/officers/{id}/notices`, under `hq_url`). |
| `releases_per_day`, `min_release_gap` | `6`, `30m` | Officer tags per rolling 24 h, and the least time between two. |
| `quiet_hours` | none | No officer tag in these local hours (`start` > `end` wraps midnight), or on weekends when `weekends` is true. Merges are not affected. |
| `rollout` | none; **required** for `release` | `health_url` (fetched by muster-server), `version_field` (default `version`), optional `ok_field`/`ok_value`, `live_reads` (10), `live_within` (20m), `healthy_for` (15m), `interval` (30s), `rollback` (**required**: `off`, `pr` or `auto`), `rollback_within` (60m). |

Secrets, from muster-server's environment only (never an instance's pod):

- `MUSTER_OFFICER_GITHUB_APP_KEY_<NAME>`: the officer App's PEM private key.
- `MUSTER_OFFICER_ESCALATE_WEBHOOK_<NAME>`: an https webhook **outside HQ**
  (a Discord-style `{"content": …}` endpoint), required with `rollout`. A
  failed rollout escalates there, because the failing service may be HQ.

`<NAME>` is the officer id as for its HQ token (`engineering:example-web` →
`ENGINEERING_EXAMPLE_WEB`).

**The officer's App needs** these repository permissions and no others:
Contents **read and write** (push its branches, create tags, merge), Pull
requests **read and write** (open and edit its pull requests, comment, merge),
Checks **read** and Commit statuses **read** (the gates), and Metadata read
(implicit). No Workflows, Administration, Actions or Issues. muster mints
every token with exactly Contents and Pull requests write, Checks and Commit
statuses read, for the one repository.

### The push gate

Every GitHub request a shipping officer's instance sends through muster's
proxy is decided by its push gate (`officership.PushGate`, a
`connectors.Gate`) **before any credential exists**; an admitted request then
carries the officer App's token, never the deployment App's or a stored
connector's. It is an allowlist:

| forwarded | refused |
|---|---|
| every read (`GET`, `HEAD`) | everything not on the left, including: |
| `git fetch` (`git-upload-pack`) | a push updating **any** ref outside `refs/heads/<prefix>…`, so the base branch and **every tag** (one bad ref refuses the whole push) |
| `git push` (`git-receive-pack`) to the officer's repo whose every command updates `refs/heads/<prefix>…` (create, update, force or delete) | `PUT …/pulls/{n}/merge`, `…/git/refs`, `…/git/tags`, `…/releases`, the contents API, the merges API, GraphQL, every `DELETE` |
| `POST /repos/{repo}/pulls` with head `<prefix>…` and base `base` (`draft: true` allowed) | a signed push, an unreadable push body, a body naming a key twice |
| `PATCH /repos/{repo}/pulls/{n}` of its **own** pull request (`title`, `body`, `state`) | a method-override header, a query string on a write, a path with an empty, `.`, `..` or doubly encoded segment; a `PATCH …/pulls/{n}` body naming `draft` (either value, alone or with other fields, on any pull request — the refusal names [`officer_mark_ready`](#officer_mark_ready-pr)) |
| `POST /repos/{repo}/issues/{n}/comments` on its **own** pull request | any write to another repository |

A refusal is a 403 naming the rule, and the upstream sees nothing.

🔴 **Why the instance cannot go around it.** The instance's pod holds no
GitHub credential, and its network policy reaches only muster's connect port
([The instance's network](#the-instances-network)). On that port the one route
that attaches a GitHub credential is the connector proxy, which resolves the
caller from the agent's own token (an agent cannot rename itself into another
officer) and, for a shipping officer's instance, runs the gate first
(`connectors.Proxy.Serve`). The token is minted in muster-server and injected
on the way out; the instance never sees it. The gate, the gates and the App
key live in muster's code and muster-server's environment, and the officer's
App is not installed on the repository that holds them.

### `officer_merge_pr {pr}`

Squash-merges the officer's own pull request, with GitHub's match-head guard
(`sha` = the head every gate read), only when ALL of these hold, read fresh:

1. `stage` is `merge` or `release`;
2. HQ answers `GET /officers/{id}` with `enabled: true` (HQ unreachable
   refuses: fail closed);
3. the pull request is open and not a draft, into `base` of `repo`, from a
   `<prefix>` branch of `repo`, authored by the officer App's bot
   (`<app slug>[bot]`, read from GitHub);
4. GitHub says it is mergeable, and it is not behind `base` (`behind_by` 0);
5. every `required_checks` entry is success on the head;
6. the newest `review_check` run on the head is completed/success;
7. no human (any author who is not a bot) wrote `/hold` on it — in a
   comment, a review or a review comment. Lift it by editing or deleting the
   comment;
8. no added or changed file under `migrations` holds destructive DDL, and none
   is removed or renamed (the lint, below);
9. if a changed path matches `sensitive_paths`, HQ accepted the notice
   `POST <notice.path>` `{pr, head_sha, paths, summary, channel}` with the
   officer's token. **If that call fails, the merge is refused.**

A refusal names its gate and the gates that passed; nothing is merged. Every
call is logged with its inputs and verdict.

After the merge muster re-reads the merge commit: if its parent is not the
base sha the gates checked (the base moved in between), the result carries
`untested_tree: true` and a warning, and `officer_tag_release` refuses
(`untested_merge`) while the base's head is that merge — its tree is one no CI
run saw. The next up-to-date merge clears it.

**The migration lint** classifies each `DROP` by what it is attached to:
under `ALTER [COLUMN] c` only `DEFAULT`, `NOT NULL`, `IDENTITY` and `EXPRESSION`
pass; as an `ALTER TABLE` action only a constraint the same file re-adds by
name (the CHECK-widening idiom) passes — anything else, whatever its name
(`DROP "x"`, `DROP index`), is a column drop, since `COLUMN` is optional; as a
statement only `DROP INDEX` passes. It also refuses any `DROP … CASCADE`; `RENAME`; `ALTER … TYPE`; `SET NOT NULL`;
`TRUNCATE`; `DELETE` or `UPDATE` with no `WHERE`. Comments are stripped by a
scanner that knows quoting; string literals and dollar-quoted bodies are
scanned, so dynamic SQL in a `DO` block is seen. A destructive migration is
a human's to write and ship.

### `officer_mark_ready {pr}`

Converts the officer's **own draft** pull request to ready for review — the
one draft transition an officer may make. A shipping officer opens test-first
pull requests as drafts (the red-before commit should not look reviewable);
a draft is not reviewed, so `review_check` never runs on it and
`officer_merge_pr` cannot pass. Once CI is green on the draft's head, the
officer calls this tool. Read fresh, ALL of these must hold:

1. the pull request is open, not merged, and **a draft** (an already-ready one
   is refused: there is no ready → draft, and no no-op write);
2. it is into `base` of `repo`, from a `<prefix>` branch of `repo`, authored
   by the officer App's bot — the same own-pull-request gates as
   `officer_merge_pr` and the push gate's own-pull-request writes (one
   rule, `owner.ownPR` in `internal/officership/gates.go`).

Then muster runs GitHub's GraphQL `markPullRequestReadyForReview` mutation on
the node id it read in 1, with the officer App's token, and fails unless
GitHub answers that pull request with `isDraft: false`. A refusal by the
`ready` gate means GitHub did not *confirm* the conversion — a lost reply, or
a 200 carrying both data and errors, can follow one that happened — so the
officer re-reads the pull request's `draft` before calling again; a refusal
by any earlier gate changed nothing. No stage gate (a
`shadow` officer's pull requests are reviewed too) and no HQ gate — it is an
edit of the officer's own pull request, as the push gate's `PATCH` is.

**Why a tool and not a push-gate rule:** GitHub's REST `PATCH …/pulls/{n}`
takes no `draft` field (only `POST …/pulls` does), so forwarding
`{"draft": false}` would not convert anything; GraphQL is the documented way,
and the push gate refuses all of GraphQL. The App's existing Pull requests
write permission covers the mutation.

### `officer_tag_release {item_id?}`

Creates the next release — the highest `vX.Y.Z` tag with its minor bumped and
patch 0 — as an **annotated** tag by the officer's bot on `base`'s head, only
when ALL of these hold:

1. `stage` is `release`, and HQ says the officer is enabled;
2. the highest tag is an ancestor of `base`'s head, and the head is ahead of
   it;
3. no release is in flight: the rollout watch owns none, and `health_url`
   reports the highest tag's version (the previous release is live);
4. fewer than `releases_per_day` officer tags in the last 24 h (counted from
   the annotated tags its bot made, walking annotated tags newest first until
   the first one — anyone's — older than 24 h) and none within
   `min_release_gap`; the base's head is not an untested officer merge
   (above);
5. not inside `quiet_hours`;
6. the required checks passed for the head's **exact tree**: either they are
   success on the head itself, or the head is a pull request's squash merge
   whose head commit has them all success and the same tree (true of every
   squash merge of an up-to-date head). A head merged from a stale branch is
   refused until the next up-to-date merge;
7. no destructive DDL in the release's migrations, and when it adds one, HQ's
   preflight (`POST /officers/{id}/preflight`) reports a WAL archive under 5
   minutes old, no archive failure in the last hour and a base backup under
   26 hours old.

Then it creates the tag object and `refs/tags/<next>`. Its message ends with
the trailer `Muster-Release: release` (or `rollback`), which is how the
rollout watch, resumed after a restart, knows never to roll back a rollback
release. A tag someone else
pushed first makes the ref create fail; the tool says so and nothing is
created. Untagged merges by humans on `base` ship in the officer's release.

### The rollout watch and rollback

After a tag, muster-server watches the release, using only `health_url` and
GitHub (never HQ, which may be the service). It is **live** once `live_reads`
consecutive answers report the tag (within `live_within` of tagging), and
**healthy** once it has answered 200 (and `ok_field` = `ok_value`) for
`healthy_for`; three bad answers in a row is **unhealthy**. A higher version
answering means a newer release superseded it. `officer_rollout_status`
returns the phase, verdict and events; the instance puts the verdict in its
item's completion. A watch in progress at a restart resumes at boot.

On `not_live` or `unhealthy`, per `rollback`:

- `off`: escalate to the webhook; touch nothing.
- `pr`: open a revert pull request from `<prefix>revert-<tag>` (`-2`, `-3`, … if
  that branch exists) that reverts the
  officer's own squash merges in the release, newest first (each a 3-way merge
  GitHub computes, so later commits are kept); escalate with its number.
- `auto`: as `pr`, then merge it through `officer_merge_pr`'s gates (CI and
  review must pass on the revert) and tag it through the release gates minus
  the in-flight, rate, quiet-hours and HQ-enabled gates (the release being
  rolled back is the one in flight, and HQ may be what is down); then watch
  that release. Anything that fails, including a revert that would need a
  human's merge, a conflict or a removed migration file, escalates. The
  revert's MERGE keeps the HQ-enabled and notice gates, so when HQ itself is
  the failing service the automatic merge is refused and the watch escalates
  with the revert pull request open.

The rollback is always a revert plus a **new** tag, never a higher tag on an
old commit (which would leave `base` ahead of production for the next tag to
re-ship). Schema is never reverted. A rollback release is never itself rolled
back (a failure escalates), and a release holding an automatic revert is
never reverted automatically.

### `officer_ci_logs {pr}`

Why CI failed on the officer's own pull request, for a CI that runs as
**Tekton PipelineRuns in a Kubernetes namespace** and posts commit statuses
with no link (the instance has no cluster access and no toolchain, so
otherwise it sees only "red" and escalates). Read-only; muster-server answers
it, with a short-lived token of a dedicated ServiceAccount, minted per call. Declared per officer, beside `ship`
(which it needs):

```json
{"id": "engineering:example-web", "repo": "example-org/example-web", "ship": {…},
 "ci_logs": {"namespace": "example-ci", "pipeline": "example-pr-check",
             "service_account": "muster-ci-logs"}}
```

| field | default | |
|---|---|---|
| `namespace` | **required** | The one namespace the PipelineRuns live in. |
| `pipeline` | **required** | The PipelineRuns' `tekton.dev/pipeline` label. |
| `service_account` | **required** | The dedicated read-only ServiceAccount, in muster-server's OWN namespace, that the reads run as. For each call muster mints it a token with a TokenRequest (`expirationSeconds` 600, the API server's default audience) and builds the reading client from that token alone. 🔴 There is no default, and muster's own token is never used for the reads (its grant is far wider): it only mints. |
| `commit_label` | `pipeline.jquad.rocks/pr.branch.commit` | The PipelineRun label whose value is the pull request's head sha (the pipeline-trigger-operator sets it). |
| `pr_param` | `PR_NUMBER` | The PipelineRun param whose value is the pull request number. |
| `max_bytes` | `16384` | The excerpt text one call returns, at most (2048–65536). |

A `ci_logs` source that cannot be built at boot (not in a cluster) does not
stop the server: the tool answers that it is unavailable,
and muster-server logs why.

What it does, read fresh on every call:

1. **Ownership** — the same rule as `officer_merge_pr`: the head is a
   `<prefix>…` branch of `repo` itself and the author is the officer App's
   bot. Anything else is refused (`head_branch` / `author`) before any
   cluster read.
2. **The run** — it lists PipelineRuns in `namespace` with the selector
   `tekton.dev/pipeline=<pipeline>,<commit_label>=<head sha>` (the head GitHub
   reports NOW, so a push moves the answer to the new run) and keeps those
   whose labels say so and whose `pr_param` is this pull request; the newest
   wins and older retries are counted (`other_runs_for_this_head`).
3. **The failures**: every TaskRun of that run whose `tekton.dev/pipelineRun`
   label names it and whose `Succeeded` condition is `False`. For each, it reads
   the logs of the failed steps' `step-<name>` containers only. When no step is
   marked failed, as on a timeout, it reads every step. It never reads a sidecar.
   Each log is read from its END: the API is asked for the last 20,000 lines,
   and the last 8 MiB of those are kept, starting at a line boundary.
4. **The excerpt**:
   - the failing-test lines (`--- FAIL`, `FAIL …`, `✘`, `N failed`) and the
     error lines (`ERROR`, `SQLSTATE`, `panic:`, `Error:`, `x_test.go:N:` and
     `file.go:N:M:` messages, `timed out`, …), deduplicated with repeats
     counted;
   - then the step's last 15 lines.

   ANSI colour is removed, each line is clipped at 400 bytes, and all excerpts
   together stay within `max_bytes`, split evenly between the failed tasks. A
   task's `truncated` is true when anything was cut: the log read dropped the
   log's start (over 8 MiB, or 20,000 lines), the byte cap dropped lines, or an
   emitted line was clipped.

**Secret safety.** The log is split into lines at `\n` and at a lone `\r` (a
progress line overwritten in place). These lines are withheld whole:

- a token shape: GitHub (`ghp_…`, `gho_…`, `ghu_…`, `ghs_…`, `ghr_…`,
  `github_pat_…`), AWS (`AKIA…`), Slack (`xox…-`), `sk-…`, a JWT (`eyJ….….`);
- framing that carries a credential:
  - an `Authorization:` header, or `Bearer` followed by 12+ token characters;
  - a `Cookie:` or `Set-Cookie:` header;
  - a URL query `sig=`, `signature=`, `x-amz-signature=`, `x-goog-signature=`,
    `token=` or `access_token=`;
  - a `--password`, `--token`, `--secret`, `--api-key`, `--access-token` or
    `--client-secret` flag with a value;
  - `-p VALUE` / `-pVALUE` on a `docker`, `podman`, `helm`, `oras`, `skopeo`
    or `buildah` `login`, or on `mysql`, `mysqladmin`, `mysqldump` or
    `mariadb`;
  - `basic auth: <base64>`;
- every `set -x` trace line (`+ …`, `++ …`). A trace is never the failure's
  message, and it is where the shell prints expanded secrets;
- a credential-named key given a value of 6+ characters that is not a path.
  The credential word has to END the key.
  - The long words are `password`, `passwd`, `secret`, `token`, `api_key`,
    `access_key` and `private_key`. They may follow any prefix:
    `AWS_SECRET_ACCESS_KEY=`, `API_TOKEN=`, `OAUTH_CLIENT_SECRET:`,
    `npm_config__authToken=`.
  - The short words are `pat`, `pass`, `pw`, `auth` and `credential(s)`. They
    must be a whole segment of the key: `GITHUB_PAT=`, `DB_PASS:`,
    `{"auth":"…"}`, `credentials:`.
  - The separator is `=` or `:`, with optional quotes (`"KEY":"value"`).
  - So `file_path:`, `output_path:`, `tokenizer:` and `compat=` are not
    credential keys. Neither is a value that is a path: `Secret:
    default/my-secret` names a Kubernetes Secret, it doesn't hold one;
- an environment assignment at the start of a line: an UPPER-case
  `NAME=value`, or any name after `export`. A lowercase `key=value` on its own
  is a logfmt/slog field and passes (`level=error msg=…`, `exit_code=1`). A
  credential key inside such a line is still withheld, as in `level=error
  password=…`;
- a line that is nothing but 40+ characters of base64;
- every line of a PEM block, from `-----BEGIN …-----` through
  `-----END …-----` (headers included), and to the end of the log when the
  END never came.

A URL's `user:password@`, with or without a user, is masked to `***@`.

This is a pattern filter, not a proof. A secret in a shape not listed here,
such as a bare value on a line of its own that isn't base64, passes. Keeping
secrets out of the CI output is the CI's job, and the byte cap bounds what
any miss can carry.

It never answers empty: `state` is `no_run` (no run for this head yet, or
pruned), `running`, `succeeded` (with "no failure found") or `failed`, and
`note` says what was found. The officer's first message tells it to call this
before escalating a red check.

**RBAC.** The ServiceAccount needs, in `namespace` only, exactly what the
tool calls: `list` on `pipelineruns` and `taskruns` (`tekton.dev`) and `get`
on `pods/log` — nothing else (no `watch`, no Secrets, no `exec`). muster's
own ServiceAccount needs `create` on `serviceaccounts/token` with
`resourceNames: [<service_account>]` in its own namespace, and nothing more
for this. A mint that fails (that grant missing) is a `ci_runs` refusal. No
long-lived token Secret is involved.

### Kill switches

In order of reach. The first two do not depend on HQ.

1. **Suspend the officer's GitHub App.** Stops every push, merge and tag at
   once and touches nothing else (the App is the officer's alone). In the
   UI: the organization's *Settings → GitHub Apps → <the App> → Configure →
   Suspend*. By API, as the App (`$APP_JWT` is a JWT signed with its key):

   ```sh
   curl -X PUT -H "Authorization: Bearer $APP_JWT" -H "Accept: application/vnd.github+json" \
     https://api.github.com/app/installations/<installation-id>/suspended
   # undo: the same with -X DELETE
   ```

2. **Stop it in muster.** Set the officer's `stage` to `shadow` (both gated
   tools refuse at once) or remove its `ship` block, and roll muster-server.
   To stop the instances as well, scale them to zero in a way your deployment
   tool does not immediately revert, e.g. with Flux:

   ```sh
   flux suspend kustomization <muster kustomization>
   kubectl -n <namespace-prefix>officer-<type>-<area>-1 scale deploy --all --replicas=0
   ```

   then remove the officer from `MUSTER_OFFICERS` in git and `flux resume`.

3. **Disable it in HQ.** muster reads `enabled` on every `officer_merge_pr` and
   `officer_tag_release` call, so the next one is refused (the automatic
   rollback's tag is the one exception: it does not ask HQ). HQ's `PUT
   /officers/{id}` replaces the fields it is not given, so repeat every one:

   ```sh
   hq officer put <id> --area '<the area JSON>' --mail-handle <handle> --owner <login> \
     --max-instances <n> --disabled
   ```

None of these undoes a tag already pushed. If a bad release is live, a human
releases the fix.

## Transcript to HQ

An officer declared with `"transcript": {"enabled": true}` has its instances'
live session transcripts forwarded to HQ — the FULL transcript, for HQ's
officer pages — while an instance holds a claimed item. It is off unless an
officer enables it.

```json
{"id": "release:example-web", "hq_url": "https://hq.example.com/api/v1",
 "transcript": {"enabled": true}}
```

**The source** is the instance's own Claude Code transcript (the JSONL the CLI
writes), never the screen. The instance pod's `ccd` serves it incrementally on
`GET /v1/transcript?after=<offset>&session=<id>` (`after=-1&backfill=<bytes>`
for the tail), decoded by `internal/cctranscript` exactly as the rendered view
decodes a mirrored session: complete lines only, a line still being written is
left for the next read. It is the PRIMARY CLI's transcript — the one an
officer's turns run in. muster-server reads it through the agent gateway with
the instance's own turn bearer (`agentgateway.Gateway.Transcript`).

**The sink** is `POST {hq_url}/officers/{id}/items/{item_id}/transcript` with
the officer's token:

```json
{"instance": "officer-release-example-web-1",
 "events": [{"seq": 2192, "at": "2026-11-01T12:01:00Z", "kind": "tool_use", "text": "Bash: go test ./..."}]}
```

HQ answers `{"stored": n, "last_seq": s}`; it skips a `seq` at or below its
highest stored (idempotent). At most 100 events a call and 8 KiB a text.
Before the first events for an item, muster sends one post with
`"events": []` to learn HQ's `last_seq` for it (the PROBE); an HQ that answers
that with 400 is handled (muster then sends without it), but loses the
resume and re-claim handling below.

| transcript entry | `kind` | `text` |
|---|---|---|
| the prompt an instance is given | `user` | the prompt |
| assistant text | `assistant` | the text (thinking is never sent) |
| a tool call | `tool_use` | `ToolName: <target>` — the file, the command, the pattern; a compact argument summary when there is none |
| a tool result | `tool_result` | its output (as `ccd` caps it, 4 KiB), `[error] ` first when the tool failed |
| an API error, a harness notice | `system` | the message |

**When.** `officer_claim` succeeding starts forwarding for that item, from the
claim on (the first read takes the tail and keeps the lines written since the
claim, less a 5 s clock slack; a line with no timestamp there is not sent).
`officer_complete` or `officer_release` succeeding stops it. Every wake-loop
pass also reconciles: an item one of the officer's instances holds under a
live lease that muster is not forwarding is RESUMED, and forwarding stops for
an item no instance holds any more. A resume after a muster restart re-reads a
256 KiB backfill window and sends only what is above HQ's `last_seq` (the
probe); when HQ holds NOTHING for the item, that window is the instance's
earlier work, not this item's, and the resume starts live instead. A resume
of an instance this muster has already forwarded for (it held another item,
completed one) always starts live: what its transcript holds since is another
item's. HQ answering a
decision — 409 `not_claimed` (the item is not this instance's), 409
`officer_disabled`, 404, 401/403 — stops forwarding for that item; a pass does
not restart it until a new claim (or until the item leaves the held list). An
instance that claims another item while forwarding one switches to it.

**Redaction.** Every text goes through, in order: the officer's own token
replaced (`officers.Client.Scrub` — a shape filter cannot know an opaque
token); the CI-log secret filter (`officership.RedactText`, the same
`RedactLines` `officer_ci_logs` uses: a line that looks like it carries a
credential is withheld whole, a PEM block is withheld from BEGIN to END, a URL's
userinfo is masked; it knows GitHub, GitLab `glpat-`, AWS `AKIA`/`ASIA` and
`aws_secret_access_key`, Slack `xox?-`, Stripe `sk_`/`rk_` live/test, Google
`AIza…`, Hugging Face `hf_`, npm `npm_`, PyPI `pypi-`, `sk-`/`sk-proj-`/`sk-ant-`
and JWT shapes, and a kubeconfig's `client-key-data`/`client-certificate-data`);
then a line in which a word cut by a cap (`…` right after it — `ccd` caps long
fields before they reach muster) starts like one of those shapes is withheld
(`officership.RedactCutTail`), and a cut word ending in the officer token's
first bytes has that part replaced (`Client.Scrub`, which runs first); only then the 8 KiB cut, marked `… [truncated N bytes]`. No
cut of muster's own happens before the filters. Transcript text is never
logged.

**`seq`** is the event's position in the transcript FILE: the end offset of
its line × 32768 + its index within the line (15 bits: ccd never serves a
line over 1 MiB, so no line carries that many events, and none is dropped). It strictly increases along the
file, so a restarted muster derives the same `seq` for the same event, and its
resume — which re-reads the backfill window — is deduplicated by HQ's rule
rather than by memory. It stays below 2^53 (a JSON-safe integer) for any file
under 256 GiB (2^53 / 2^15 = 2^38 bytes). A new conversation mid-item (a new file, offsets from zero: an
operator's `/clear`; `ccd` resumes the primary's own conversation by id on a
restart) is shifted above the seqs before it; a muster that restarts INTO such
a conversation has lost that shift, sees HQ answer `stored: 0` with a
`last_seq` above a batch it read live, and re-sequences that batch and the ones
after it above `last_seq` — on a batch's FIRST send only: a re-send answered
`stored: 0` is a lost acknowledgement (HQ committed, then failed to answer)
and is NOT re-sequenced, so it is stored once. What that double edge can lose
is the part of the resume's window HQ skipped as old. A batch whose seqs are
not above HQ's `last_seq` from the probe — a RE-CLAIM of the item by another
instance, whose transcript is its own file with its own offsets — is shifted
above it rather than skipped (HQ's `last_seq` is per item).

**Cadence and back-pressure.** Each forwarding instance is read and posted
every 2 s, in batches of at most 100. An HQ outage (5xx, 429, no answer) backs
off — 2 s doubling to 1 min — and a batch is dropped after 5 failed re-sends.
A 400 drops the batch. A 413 drops that batch and halves the batch size for
the rest of the item. The forwarder never blocks the officer: the officer
tools and the wake loop only record state; reads and posts run on the
forwarder's own loop. Each instance's buffer holds at most 1000 events
waiting for HQ; past that the OLDEST are dropped. Every drop is logged and
counted on `muster_officer_transcript_events_total{result}`: `posted` (what HQ
answered as STORED), `skipped_by_hq` (sent, HQ already held it — a re-send
after a lost answer), `skipped_resume` (a resume's re-read HQ already held, or
earlier work), `dropped_buffer`, `dropped_retries`, `dropped_refused` (400),
`dropped_too_large` (413), `dropped_superseded`, `dropped_unsequenced`.

**Deploy order.** HQ's transcript route must be LIVE before `transcript` is
enabled: an HQ without it answers 404, which muster reads as a decision and
stops forwarding for that item (until its next claim) — every item, quietly.
`MUSTER_OFFICERS` refuses an unknown field, so the
`transcript` key must reach a deployment only with (or after) a muster image
that knows it — the other way round, muster-server refuses to boot. The
instance pods need a `ccd` that serves `GET /v1/transcript`: an older pod
answers 404, the forwarder logs that the transcript cannot be read and retries
every tick, and nothing is sent until the pod runs the new image. An officer
that enables `transcript` on a server whose agent gateway cannot read
transcripts is a boot refusal.

**Rollback order** is the reverse: remove the `transcript` key from
`MUSTER_OFFICERS` (and let muster-server restart on that config) BEFORE rolling
muster-server back to an image that predates it — an older image refuses the
unknown field and does not boot.

## Security model

- **The token never leaves muster-server.** It is read from the server's
  environment into `officers.Officer`, whose token field neither JSON nor `fmt`
  prints, and is spent only by `officers.Client`, in the `Authorization` header
  of requests to that officer's own `hq_url`. HQ errors carry HQ's response,
  never the request. Redirects are not followed, so a redirect cannot carry the
  token elsewhere. An instance's pod spec, env, files and first message carry
  no trace of it (`TestAnOfficerInstancesPodSpecCarriesNoOfficerToken`,
  `TestTheKickoffNamesTheOfficerAndCarriesNoToken`).
- **Per-officer binding.** An officer tool acts for the officer whose instance
  the CALLING agent is — `officers.Registry.ForAgentName` on the agent muster
  resolved from the caller's own per-agent token — and for no other. The tools
  take no officer argument; one naming another officer is refused before HQ is
  reached. An agent cannot rename itself (the slug is immutable), and no muster
  path mints an `officer-` name except this runtime. HQ enforces the same
  boundary again: an officer token may only touch its own officer's items.
- **Delete protection.** A declared officer's instance cannot be deleted by any
  of muster's destroy paths (the card's Delete, a task dismissal, the
  `agent_destroy` tool, the stopped-agent reaper): they ask one predicate,
  `deleteProtection`, which is the chief predicate *or* "an instance of a
  declared officer". The chief predicate itself is unchanged, so an officer is
  never offered the chief's tools. Remove an officer from `MUSTER_OFFICERS` and
  its instances become ordinary, deletable agents.
- **No production access from the pod.** Instances are ordinary `claude-code`
  pods: no service-account token, and egress **narrower** than every other
  claude-code agent: muster's connect port only, with name-filtered DNS and the
  Anthropic API allowed by the cluster, by name
  ([The instance's network](#the-instances-network)). An officer that declares
  a [postgres connector](connectors.md#postgres-connectors) reads that one
  database through muster-server, which holds the password and runs each
  statement read-only; the pod's network is unchanged.

## cairn

**Deferred.** The contract gives officers read access to their cairn scopes and
append-only writes. muster does not provide either yet: a `claude-code` pod has
no cairn client and its network policy cannot reach a store, by design
([chief.md](chief.md)). The planned shape keeps that property — a
per-officer, narrowed cairn credential held by muster-server (like the HQ
token) and an `officer_cairn_recall` tool proxied by muster — rather than
opening pod egress to the store. Until then, `officer_brief`'s `cairn.scopes`
is informational.

## The HQ API muster consumes

From the officers contract, v1 (base `/api/v1`, bearer token of scope
`officer`): `GET /officers/{id}/brief`, `GET /officers/{id}/items`, `POST
/officers/{id}/items/{item}/{claim,heartbeat,complete,release}`, and `POST
/releases/captain` (release officers), with the bodies in the table above. A
shipping officer also uses `GET /officers/{id}` (its `enabled`), `POST
/officers/{id}/notices` (or its configured `notice.path`) and `POST
/officers/{id}/preflight` (`{"wal": {"last_archived_time", "last_failed_time",
"failed_count"}, "last_backup_time"}`). An officer with `transcript` enabled
uses `POST /officers/{id}/items/{item}/transcript`
([Transcript to HQ](#transcript-to-hq)). Where the contract leaves a shape open, muster accepts:

- the items list as HQ sends it, `{"items": […], "next": "…"|null}`; the wake
  loop follows `next` for up to 5 pages of 100;
- item ids as JSON strings or numbers (HQ's are integers).

`internal/officers/hqtest` is the in-process fake the tests run against; it
implements the claim/lease rules as HQ does (409 to ANY claim while the lease
is live — the holder's own included, which `officer_claim` turns into a
heartbeat — an expired lease claimable by anyone, 410 on a finished item, 403
across officers), the captain route's body rules, and the transcript route's
(limits, 409 `not_claimed` / `officer_disabled`, idempotency on `seq`).

## Deploying

What a deployment needs beyond muster's usual claude-code setup:

1. HQ: the officer and its type declared (`PUT /officer-types/release`, `PUT
   /officers/release:example-web` with `max_instances`), and an officer token
   issued for it.
2. A Secret holding that token, exposed to the muster-server Deployment as
   `MUSTER_OFFICER_TOKEN_<NAME>` (not to any agent).
3. `MUSTER_OFFICERS` on muster-server, and `MUSTER_AGENT_KINDS` including
   `claude-code` with an account in the pool.
4. The connector door: `MUSTER_CONNECTOR_ENCRYPTION_KEY`, `MUSTER_CONNECT_PORT`
   and `MUSTER_AGENT_CONNECT_URL` (the server refuses to boot officers without
   it). No approval gate is needed: an officer instance runs with every tool
   allowed (above). Its `github` requests still need the owner's explicit
   decision.
6. A `claude-code-agent` image built from this version or later (its `ccd`
   applies `CCD_PERMISSION_MODE`; an older one ignores it and the officer
   prompts).
7. Egress from muster-server to HQ. Nothing needs to reach muster from HQ.
8. A cluster-side policy for the officer instance pods allowing name-filtered
   DNS and `api.anthropic.com:443` by FQDN
   ([The instance's network](#the-instances-network)). Without it an
   instance's turns all fail.

Checking it: the boot log has one `officers: <id> on: …` line per officer;
after HQ creates an item, `officer-<type>-<area>-1` appears among the agents
within one poll; its transcript opens with the first message above.
