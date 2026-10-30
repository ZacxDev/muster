# The machine job API

An external orchestrator submits a one-shot **job** — run this prompt on this
agent, optionally at this commit, and give me the result — and polls for its
outcome. A job runs as one turn of an existing, running `claude-code` agent: one
it names, or one muster chooses from a named **worker pool**.

> **Who a `sha` job is safe for: commits by trusted members only.** A `sha`
> job is isolated from its commit's agent configuration, and when it runs on a
> **pool** worker, that worker holds no credential that could write to the
> repository (a job naming an agent gets no such guarantee: see below). It is
> NOT isolated from the commit's code. That is enough for reviewing commits written
> by people you already trust with your repository — a teammate's pull
> request. It is **not** enough for a third party's commit (a pull request from
> a fork, an outside contributor): keep those away from the job API until
> workers also get execution isolation, such as an egress policy that stops a
> turn from sending what it can read.
>
> **Isolated:**
>
> - **The commit's agent configuration.** The job runs in a git worktree of its
>   own, outside the agent's checkout, in a fresh Claude Code session whose CLI
>   starts in that worktree. Before the turn, these paths are removed and the
>   repository's default branch's copies (`origin/HEAD`) put back where it has
>   them — or, with `config_ref`, that commit's copies, which must be on the
>   default branch: every `.claude/` directory at any depth (settings with their hooks —
>   commands the CLI runs — agents, and skills, which the CLI also finds in
>   subdirectories); the root `CLAUDE.md`, `CLAUDE.local.md` and `AGENTS.md`
>   (the instructions it loads at launch, `CLAUDE.local.md` being its personal
>   memory file); the root `.mcp.json` (MCP servers, whose commands it starts).
>   The script reads every path list NUL-separated and as bytes (in the C
>   locale), so a name with bytes outside ASCII — valid UTF-8 or not — is
>   checked and restored like any other, whatever locale the agent's instance
>   runs with. With the real CLI, a commit whose `.claude/settings.json` defines a
>   SessionStart hook and whose `.mcp.json` defines a server runs neither in its
>   job, and runs both without this step
>   ([`realcli_test.go`](../internal/agentprovision/realcli_test.go), in CI's
>   image job).
> - **Write access to the repository, on a pool worker.** The commit is
>   fetched anonymously (or, for a pool with a
>   [read-only deploy key](#private-repositories-a-pools-deploy-key), by the
>   pod's key holder with that key and nothing else), as
>   pull request `pr`'s head from `https://github.com/<repo>` — not through
>   muster's credentialed proxy, whatever git routing the pod has (in CI, with
>   that routing in force, a plain fetch reaches the proxy and the job's fetch
>   does not) — and the job runs only at that head or one of its ancestors. A
>   pool worker holds no approved grant on any connector that reaches
>   github.com: such a grant is never approved for it, and a worker holding one
>   anyway is skipped ([Worker pools](#worker-pools)). So a pool job's turn has
>   no credential to push, comment or merge with.
> - **A runtime that ignores the worktree.** ccd answers every such turn with
>   the directory its CLI worked in (`X-Muster-CC-Workdir-Used`). When the
>   answer is missing — a ccd from before job worktrees, which would have run
>   the turn in the agent's own checkout — or names another directory, the
>   turn's reply is discarded and the job fails `workdir_unconfirmed`.
>
> **Not isolated:** the turn runs in the worker's pod with the worker's Claude
> account, its network (any public HTTPS host — or, on an
> [egress-locked pool](#a-pools-egress-lock), what the operator's cluster
> policy allows) and its volume. Every other file
> in the commit is the commit's — `CLAUDE.md` / `AGENTS.md` below the root
> (instructions the CLI reads as it works there), build scripts, tests,
> dependencies — and the turn may run them if the prompt leads it there. The
> commit's code can therefore read what the pod holds (the worker's Claude
> credential, the volume) and send it anywhere on the public internet. A named
> agent (not a pool member) may also hold a GitHub grant, and the commit's code
> could use it. And the default branch's restored `CLAUDE.md` may `@import`
> other files by path; if the CLI follows such an import into the worktree, it
> reads the COMMIT's copy of that file. Both are accepted for commits by
> trusted members.

## Auth

Both routes are on the service tier: `MUSTER_SERVICE_TOKEN` as
`Authorization: Bearer …` (or `X-Muster-Token`), plus an `X-Muster-Actor`
header naming the caller ([auth.md](auth.md#machine-credentials)). The actor is
recorded on the job as who requested it.

- No token, or a wrong one: **401**.
- `MUSTER_SERVICE_TOKEN` unset on the server: **503** with `"unarmed": true`.
  The tier fails closed.
- The tier acts as user 1, so a job can name only user 1's agents and pools.

## Submitting a job

```
POST /api/jobs
{
  "agent": "keen-wren",            // an existing claude-code agent — or:
  "worker_pool": "reviewers",      // a pool to choose one from (exactly one of the two)
  "prompt": "Review the change …", // required, at most 64 KiB
  "repo": "owner/name",            // optional: must be the agent's repository (required with worker_pool and sha)
  "sha": "0123456789abcdef0123456789abcdef01234567", // optional: a full 40-hex commit id
  "pr": 42,                        // required with sha (and only with it): the pull request the commit is on
  "config_ref": "89abcdef0123456789abcdef0123456789abcdef", // optional, only with sha: the default-branch commit the agent configuration comes from
  "base_sha": "abcdefabcdefabcdefabcdefabcdefabcdefabcd", // optional, only with sha: a commit fetched into refs/muster-jobs/<job_id>-base to diff against
  "model": "sonnet",               // optional: the Claude model the job's own session runs on
  "result_path": "out/review.md",  // optional: relative to the job's working directory
  "timeout_s": 1800,               // optional: 1-2100, default 1800
  "idempotency_key": "review-42"   // optional, at most 128 bytes
}
```

**202** `{"job_id": 17, "status": "queued", …}`, the same shape as a read.

Unknown fields are refused, so a misspelt `result_path` is a 400 rather than a
job that returns the wrong thing.

| answer | when |
|---|---|
| 400 | a missing or malformed field — including both or neither of `agent` and `worker_pool`, a `worker_pool` with a `sha` but no `repo`, a `sha` without a `pr` (or a `pr` without a `sha`), a `repo` that is not `owner/name`, and an abbreviated `sha`: only a full 40-character id is accepted; a `config_ref` that is not a full 40-character id, or one without a `sha`; a `base_sha` that is not a full 40-character id, or one without a `sha`; a `model` outside the pattern below |
| 404 | no agent by that name; no agent is a member of that `worker_pool` |
| 409 | the agent is not `claude-code`; `repo` is not the agent's repository; `sha` on an agent with no repository; an `idempotency_key` already used with a different `model`, `config_ref` or `base_sha` (below); a `config_ref` on a pool with a [deploy key](#private-repositories-a-pools-deploy-key) (by the pool's name or one of its workers'), whose jobs get none of the repository's agent configuration |
| 429 | the agent already has 4 jobs queued or running (the per-agent limit); for a `worker_pool`, every member that could otherwise take the job has 4, or runs on a Claude account that is cooling down after a rate limit or already running its limit of concurrent jobs (`error` names each, and when a cooldown ends). Submit again once one finishes or the cooldown ends |
| 501 | `sha` / `result_path` on a deployment with no provisioner |
| 503 | no database, no agent gateway, or the service tier is unarmed; or, for a `worker_pool`, no member can take the job for a reason other than the limits above (`error` says why for each) |

**Idempotency.** A second submission with an `idempotency_key` already used
records nothing and answers the first job, with **200** and
`"deduplicated": true` — before any agent is looked up or worker chosen, so a
retry is answered even when its pool now has no eligible worker.

The key is compared with `model`, `config_ref` and `base_sha` and with nothing
else in the body. If any differs from the first job's — including one sent where the
first had none, or left out where it had one — the answer is **409** with
`"idempotency_mismatch": true`, the first job's fields, and an `error` naming
the first job and each difference
(`idempotency_key "review-42" already names job 17, which has model "opus", not "sonnet"; use a new key for a different job`).
Nothing is recorded or run. Use a new key to run the job with the new values.

### `model`

The Claude model the job's turn runs on. Absent, the CLI's default, as before.

- **Accepted:** an alias (`opus`, `sonnet`, `haiku`) or a full model id,
  optionally with the CLI's `[1m]` suffix: 1-80 characters of `[a-z0-9.-]`,
  starting with a letter or digit, then optionally `[1m]`
  (`^[a-z0-9][a-z0-9.-]{0,79}(\[1m\])?$`). This is the check a Claude Code
  chat session's model gets, and ccd checks it again before it starts a CLI.
  muster does not check that the account can use the model: a model the CLI
  refuses fails the job with ccd's reason.
- **Where it goes:** the job's own Claude Code session is opened with it, so
  ccd starts that session's CLI with `--model`. The agent's main session and
  every other session are not touched. On a deployment whose chat store cannot
  open a session of the job's own, a job with a `model` fails `internal`
  before its turn — the refusal a `sha` job gets there — rather than run in
  the agent's own session.
- **Confirmed, or discarded.** ccd answers a turn from a session CLI of the
  job's own with `X-Muster-CC-Workdir-Used`, and only from one. A `model` job
  whose answer lacks it — an agent image that ignores the session and model
  headers and answers from its main CLI, on that CLI's model — fails
  `model_unconfirmed`, its reply discarded, instead of succeeding with a
  `model` it did not run on. An image whose ccd has sessions but predates that
  header fails the same way: update the agent's image.
- **Read back:** `model` on the job (`""` when none was given).

### `config_ref`

The commit a `sha` job's worktree takes its agent configuration from (the
overlay above), instead of the default branch's tip. Absent, the default
branch's tip (`origin/HEAD`), as before. A caller that recorded the version of
the configuration it reviews with — a hash of a `.claude/agents/` file, say —
sends the commit it read it at, so a merge to the default branch between its
read and the job's start does not change what the job runs with.

- **Accepted:** a full 40-character lowercase hex commit id, on a job with a
  `sha` only (a job without one has no worktree and no overlay).
- **Refused when it is not on the default branch.** The commit must be the
  checkout's `origin/HEAD` or an ancestor of it. If it is not, the default
  branch is fetched anonymously, the same way as the pull request (`HEAD` of
  `https://github.com/<repo>`, into a ref of the job's own, deleted with the
  worktree), and the commit must be its tip or an ancestor. Otherwise the job
  fails `checkout_failed`, and nothing is run. Any other commit — the pull
  request's own head, above all — would hand the commit under review its own
  configuration back.
- **Read back:** `config_ref` on the job (`""` when none was given).

### A deploy-key pool's jobs get none of the repository's configuration

Whether a `sha` job's worktree gets the repository's agent configuration is
decided by the **pool**, on the server, not by the job: there is no field for
it. A job on a worker of a pool with a
[deploy key](#private-repositories-a-pools-deploy-key) always has the overlaid
paths **removed** — every `.claude/` at any depth, the root `CLAUDE.md`,
`CLAUDE.local.md`, `AGENTS.md` and `.mcp.json` — and **nothing restored**,
neither the commit's copies nor the default branch's. The turn then has the
job's prompt and muster's own configuration only: the managed-policy
`CLAUDE.md` an agent's instructions are placed in, and the pod's user settings.
Every other pool, and an agent in no pool, gets the overlay above, as before.

- **A `config_ref` on such a pool is refused (409)** — by the pool's name, or
  naming one of its workers — rather than silently ignored: it names a
  configuration to overlay, and the pool's jobs have none. Nothing is recorded.
  A job that reaches a worker anyway (the configuration changed in between)
  fails `checkout_failed` before anything runs.
- **Not changed by it:** everything else in the commit is still the commit's,
  including `CLAUDE.md` / `AGENTS.md` files below the root (see the note at the
  top). The fetch and the `sha`-on-the-pull-request check are the same.

### `base_sha`

A commit for the turn to compare the pull request against — typically the
pull request's base. The job's worktree script fetches it **with the same
credential path as the pull request's head**: anonymously from
`https://github.com/<repo>`, or, for a pool with a
[deploy key](#private-repositories-a-pools-deploy-key), over SSH with that key.
It is stored at **`refs/muster-jobs/<job_id>-base`** in the agent's checkout,
whose objects and refs the job's worktree shares, so the turn can run
`git diff <base_sha>...HEAD` (or `git log <base_sha>..HEAD`) with no credential
of its own. A lane session's own `git fetch` has no credential on a deploy-key
pool, so this is the way a turn gets a commit that is not already in the
checkout.

- **Accepted:** a full 40-character lowercase hex commit id, on a job with a
  `sha` only. It is not required to be on the default branch or on the pull
  request; it must be a commit GitHub serves by id.
- **Fetched before the worktree is made.** A base that cannot be fetched
  fails the job `checkout_failed`, with an `error` that starts "the job's base
  commit could not be fetched" and names the `base_sha`; nothing is run.
- **Removed with the worktree**, like the job's pull-request ref.
- **Read back:** `base_sha` on the job (`""` when none was given).

## Worker pools

A worker pool is a name carried by long-lived `claude-code` agents. An agent
becomes a member when it is created with `worker_pool` set. Membership is fixed
at creation; there is no pool table and no other configuration — a pool exists
while some agent carries its name (1-63 of `[a-z0-9-]`, not starting with `-`).

### Creating a pool worker

1. Open the dispatch form (the **+** beside **Agents** in the sidebar, or a
   task's **Dispatch**).
2. Pick **Claude Code** as the agent kind. The **Worker pool** field appears
   under the Claude account; it is not shown for any other kind.
3. Enter the pool's name, for example `reviewers`.
4. Leave the Claude account on **Auto** unless you want to pin one; the account
   pool chooses as it does for any agent. Pick the repository the pool's jobs
   will name (with `repo`), which must be a **public** one (see below), or the
   one private repository the pool has a
   [deploy key](#private-repositories-a-pools-deploy-key) for.
5. **Dispatch** (or **Save for later** and Start it). A pool job can run only on
   a `running` worker.

The agent's page shows a **pool** chip, and `GET /api/agents` reports
it as `workerPool`. Repeat with the same name for more workers; each takes its
own account from the pool unless pinned.

The form posts `worker_pool` to `POST /agents` with `kind=claude-code`. With any
other kind, or a name outside the pattern, the server refuses the dispatch with
**409** and the reason, and creates nothing. Neither the `muster` CLI nor the
chief's `task_dispatch` can create an agent in a pool.

**A pool worker holds no GitHub credential.** The one way an agent gets one is
a grant on a connector that reaches github.com — the `github` connector (the
GitHub App's token for the agent's repository, [github-app.md](github-app.md))
or a stored connector with a `github.com` host: approved, it routes all of the
pod's git traffic to github.com through muster's proxy, which adds the
credential. So:

- approving such a grant for a member is **refused**, on the Connectors tab,
  from a mirrored session (`connector_request_decide`), and in the approval
  that comes with entering a credential — including when the request cannot be
  read at that moment. A connector whose hosts muster cannot read counts as
  reaching github.com;
- a member holding such a grant **approved** anyway is not eligible for a job.
  A **pending** one does not count: it can never be approved, and ccd files a
  pending `github` request by itself whenever a clone fails, which would
  otherwise take a worker out of its pool after one failed clone.

An agent joins its pool when it is created, before it can hold any grant, so
with both rules it never holds one. A worker therefore cannot clone a
**private** repository (its clone has no credential, and the access request
ccd files can never be approved): pools serve public repositories, the same
ones a job's anonymous fetch can read — unless the pool is given a read-only
deploy key, below.

### Private repositories: a pool's deploy key

A pool can serve ONE private repository with a **read-only deploy key**, and
nothing else changes: no GitHub grant, no proxy, no token.

1. Make an SSH key pair and add its public half to the repository as a
   **deploy key, without "Allow write access"** (Settings → Deploy keys).
2. Store the private half in a Secret in **muster's own namespace**
   (`MUSTER_AGENT_NETPOL_FROM_NAMESPACE`), key `ssh-privatekey`:
   `kubectl -n <muster's namespace> create secret generic <secret> --type=kubernetes.io/ssh-auth --from-file=ssh-privatekey=<file>`.
   muster copies it into each worker's namespace (below); do not copy it there
   yourself — a Secret of that name muster did not make is refused, never
   overwritten.
3. Set `MUSTER_AGENT_CC_POOL_DEPLOY_KEYS=<pool>=<owner>/<name>:<secret>`
   ([configuration](configuration.md)) and restart muster. The pool is
   [egress-locked](#a-pools-egress-lock): ship its cluster egress policy
   first, or its workers reach nothing public.
4. Create the pool's workers on that repository (Start an existing one again to
   give it the mount).

A worker of such a pool runs a second container beside the agent's, the **key
holder** (`deploy-key`), under its own uid (1001, the image's `muster-keyholder`
user, which ssh needs to exist; the agent's session runs as 1000). The holder, and only the holder, mounts the Secret's one key at
`/etc/muster/deploy-key/ssh-privatekey` and GitHub's pinned host keys at
`/etc/muster/deploy-key/known_hosts`. Every use of the key is the holder's:

- **At the pod's start** it clones the repository from
  `ssh://git@ssh.github.com:443/<owner>/<name>.git` into a bare **mirror** at
  `/var/lib/muster-mirror/repo.git`, retrying every 30 s until it can. That
  directory is an `emptyDir` the holder writes and the agent's container mounts
  **read-only**. ccd is told the mirror's path (`CCD_REPO_MIRROR`) and clones
  the agent's checkout from it — locally, with no credential. ccd's clone and
  muster's job scripts pass
  `--upload-pack='git -c safe.directory=<mirror> upload-pack'` on every git
  that reads the mirror: the mirror belongs to uid 1001, and git 2.39's
  upload-pack refuses another user's repository unless its own command line
  marks it safe. **The session's own git cannot fetch `origin`** (the
  mirror) on such a pool: the clone does not record that option, so a plain
  `git fetch origin` in the checkout fails with "detected dubious
  ownership". That is intended — a job's commits, its `base_sha` included
  (`refs/muster-jobs/<id>-base`), are fetched for it before its turn.
- **For each job**, muster execs into the holder: it probes the key for write
  access and fetches the pull request's head (and the `base_sha`) into the
  mirror; the job's worktree script then fetches them from the mirror, in the
  agent's container. The pool's jobs get none of the repository's agent
  configuration
  ([above](#a-deploy-key-pools-jobs-get-none-of-the-repositorys-configuration)).

**The Secret is copied, not shared.** Each time a worker's spec is applied
(dispatch, Start, reapply) muster reads the Secret in its own namespace and
writes its data and type — nothing else of it, under muster's ownership labels
— to a Secret of the same name in the worker's namespace, before the pod. A
source that is missing, or has no `ssh-privatekey`, fails that dispatch or
Start with an error naming the Secret and the namespace, and no pod is started
(it would otherwise wait in `ContainerCreating` forever). The copy is removed
with the worker's namespace. muster never logs the Secret's data. In the
shared-namespace layout (`MUSTER_AGENT_NAMESPACE_SHARED`) the pool's workers
share one copy, which a worker's Destroy leaves; when that namespace is
muster's own, nothing is copied.

Port 443, because a worker may reach public addresses on 443 only; GitHub
serves the same SSH endpoint there. The ssh command fixes every option
(`-F /dev/null`, no agent, only this identity, only the pinned host keys), and
the holder's gits ignore the system and global configuration and run in the
mirror, whose configuration only the holder can write.

- **Read-only, verified per job as far as muster can.** Before the fetch, the
  holder opens GitHub's receive-pack with the key — a FORCED `git push
  --dry-run` to `refs/heads/muster-read-only-probe`, which sends nothing and
  writes no ref. (Forced, so a branch of that name already on GitHub, at any
  commit, cannot turn the push into a non-fast-forward refused before write
  access is asked.) GitHub refuses that for a read-only deploy key; if it is
  accepted the key can push, and the job fails `checkout_failed` without
  fetching. A probe that fails for any other reason (the network) is not a
  refusal; the fetch then fails on its own. muster cannot see the key's setting
  on GitHub, only how GitHub answers.
- **One repository.** A worker of the pool on another repository is refused at
  dispatch and Start, and a job naming another repository fails
  `checkout_failed` before anything runs — never a silent anonymous fetch.
- **Every other pool is unchanged:** anonymous, public repositories only, no
  key holder.

🔴 **How far the key is exposed.** The key is a file in the **holder's**
container only (mode `0440`, root with the pod's group, mounted through a
`subPath`). No process of the agent's session — ccd, the agent's main CLI,
every lane's tools, whatever a lane's `Bash` runs, bypassed or not — can read
its bytes or use it as a credential:

- the file does not exist in the agent's container at any path: the secret
  volume is mounted into the holder alone, and nothing in the agent's
  container names its path (`CCD_REPO_MIRROR` names the mirror, not the key);
- the holder runs as another uid, non-root, without privilege escalation or
  capabilities, and the pod shares no process namespace, so the session cannot
  reach the holder's `/proc/<pid>/root`, `environ` or memory; the key reaches
  no argv or environment there either (ssh is given its path);
- the session has no channel to the holder: it listens on nothing, and the
  directory they share is read-only on the session's side, so the session can
  neither plant configuration the holder reads nor ask it to fetch. Only muster
  (an exec through the Kubernetes API) runs anything in it.

What remains: the **mirror's contents** — the repository's branches at the
pod's start, and every pull-request head and base fetched for the worker's
jobs — are readable by the session, as the checkout always was. Whoever can
exec into the pod or read the Secret through the Kubernetes API (muster's
ServiceAccount, cluster admins) can read the key; the agent's pod mounts no
ServiceAccount token. Rotating the key: replace the key in muster's namespace,
then Stop → Start each worker (Start copies it again; a `subPath` mount is not
refreshed in a running pod).

### A pool's egress lock

Every pool with a deploy key, and every pool listed in
`MUSTER_AGENT_CC_EGRESS_LOCKED_POOLS` ([configuration](configuration.md)), is
**egress-locked** — the officer lock (docs/officers.md "The instance's
network"), applied to a pool's workers. muster does two things, and only two:

- **It removes egress.** The worker's NetworkPolicy keeps its ingress rule
  (from muster's pods) and, when the connector store is offered, egress to
  muster's connect port — and has **no DNS rule and no "any public address on
  443" rule**. Network plugins union allows, so either rule would widen
  whatever the cluster's policy narrows.
- **It labels the pod**: `muster.agent/pool=<pool>` and
  `muster.agent/egress=locked` (only locked workers carry them; every other
  pod's labels are unchanged). The pod also gets
  `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`, so the CLI does not try its
  optional hosts (telemetry and error-report intake, the auto-updater,
  release notes).

🔴 **The allowlist is the operator's cluster policy, not muster's.** A policy
selecting `muster.agent/egress=locked` is the only thing that lets a locked
worker resolve a name or reach a host. Without one, a locked worker reaches
nothing public and every turn fails — closed and loud. On Cilium, a
`CiliumClusterwideNetworkPolicy` (clusterwide, because muster creates the
worker namespaces itself):

```yaml
# Egress for muster's egress-locked pool workers: DNS for the allowed names
# and cluster-internal names only, and those hosts on 443.
apiVersion: cilium.io/v2
kind: CiliumClusterwideNetworkPolicy
metadata:
  name: muster-egress-locked-workers
spec:
  endpointSelector:
    matchLabels:
      # The driver's ownership pair, so a pod someone else labels
      # egress=locked gets nothing from this policy.
      app.kubernetes.io/managed-by: muster
      app.kubernetes.io/name: muster-agent
      muster.agent/egress: locked
  enableDefaultDeny:
    ingress: false   # ingress stays muster's NetworkPolicy
    egress: true
  egress:
    - toEndpoints:
        - matchLabels:
            k8s:io.kubernetes.pod.namespace: kube-system
            k8s-app: kube-dns
      toPorts:
        - ports:
            - port: "53"
              protocol: UDP
            - port: "53"
              protocol: TCP
          rules:
            dns:
              - matchName: github.com
              - matchName: api.github.com
              - matchName: codeload.github.com
              - matchName: ssh.github.com
              - matchName: objects.githubusercontent.com
              - matchName: api.anthropic.com
              # Your cluster's DNS domain (the kubelet's clusterDomain): the
              # search-path expansions every lookup is tried with first under
              # ndots:5. `**.` is multi-level; `*.` matches one label only.
              - matchPattern: "**.CLUSTER_DOMAIN"
    - toFQDNs:
        - matchName: github.com
        - matchName: api.github.com
        - matchName: codeload.github.com
        - matchName: ssh.github.com          # a deploy-key holder's SSH, on 443
        - matchName: objects.githubusercontent.com
        - matchName: api.anthropic.com
      toPorts:
        - ports:
            - port: "443"
              protocol: TCP
```

Why those hosts: GitHub for the clone, a job's anonymous fetch, the REST API,
archives and raw objects, and SSH over 443 (`ssh.github.com`, the key
holder's); `api.anthropic.com` for the CLI's inference, its WebFetch domain
check and its token use. Add a pool's own hosts (an HQ, say) to both lists; to
give pools different allowlists, select `muster.agent/pool` as well.

⚠ **A worker keeps the policy it was started with.** The lock and the labels
are written when its spec is applied (dispatch, Start, reapply); a worker
started by an older muster keeps its public-443 rule until Stop → Start.
Check with `kubectl -n <worker namespace> get networkpolicy -o yaml`: the
egress list has no `ipBlock` and no port 53.

What it still allows: anything those hosts accept. A session can push what it
reads to a GitHub repository or gist of an attacker's, or call the Anthropic
API with an attacker's key and put data in the request; closing that needs an
egress proxy that checks credentials, which muster does not ship.

A job naming a pool runs on the member chosen when it is submitted. A member is
**eligible** when it is a `claude-code` agent whose status is `running`, works
on the job's `repo` (when it names one), holds no approved grant reaching github.com,
runs on a Claude account that is configured, whose current token is not
marked `auth_failed` and that is not cooling down after a `rate_limited`, has
fewer than 4 jobs queued or running, and — when
`MUSTER_AGENT_CC_MAX_CONCURRENT_JOBS` is set (unset is no cap) — would not
take its account past that many jobs at once. Of the
eligible it prefers, in order:

1. a worker with no job queued or running;
2. then the fewest jobs queued;
3. then the account least recently marked `rate_limited` (never, then the
   oldest);
4. then the agent's name, so the choice is deterministic.

Submissions arriving at the same moment are placed one after another: each
choice sees the jobs the ones before it placed, so six submitted at once to two
idle workers are spread over both rather than sent to one until its limit
refuses them.

**The cooldown.** After a turn on an account answers `rate_limited`, the
account rests: no pool job is placed on any worker using it until the reset
time the CLI stated (`resets 9:45pm (UTC)`, `resets Oct 9, 9am (UTC)`), or,
when the message states none muster can read (no zone, for one),
`MUSTER_AGENT_CC_RATE_LIMIT_COOLDOWN` (default `1h`) after the mark. It is
usable again at exactly that time.

**The per-account cap** is opt-in (`MUSTER_AGENT_CC_MAX_CONCURRENT_JOBS`;
unset, there is none). Jobs on one worker run one after another, so the jobs
an account runs at once are its workers with a job queued or running — in any
pool, or none. An idle worker whose account already has
`MUSTER_AGENT_CC_MAX_CONCURRENT_JOBS` such workers is skipped; a busy worker is
not, because a job placed on it queues behind the one it runs. A job naming an
agent directly is not checked against the cap or the cooldown: naming the
agent is the caller's override.

The job records the pool (`worker_pool`) and the worker (`agent`,
`cc_account`). The choice is made once; a job is not moved to another worker
later, and one worker still runs its jobs one at a time.

## Reading a job

```
GET /api/jobs/{id}
```

```json
{
  "job_id": 17,
  "status": "succeeded",
  "result": "…",
  "agent": "keen-wren",
  "worker_pool": "reviewers",
  "cc_account": "alpha",
  "started_at": "…",
  "finished_at": "…",
  "created_at": "…",
  "repo": "", "sha": "", "result_path": "", "timeout_s": 1800,
  "model": "", "config_ref": "", "base_sha": "",
  "session_id": 412
}
```

`worker_pool` is `""` for a job that named its agent.

| `status` | meaning |
|---|---|
| `queued` | accepted; waiting for the agent (one job runs per agent at a time) |
| `running` | the agent's turn is in progress |
| `succeeded` | `result` holds the outcome, verbatim |
| `failed` | `reason` and `error` say why |
| `timed_out` | the job's `timeout_s` elapsed (or ccd's own turn budget did); `reason`, `error` |

`result` is present only on `succeeded`, and `reason` only on `failed` and
`timed_out`. `error` is on every `failed` and `timed_out` job; on a
`succeeded` one it appears only as a note that does not change the outcome
(a result file or a worktree that could not be removed, below).

**The result** is the agent's final reply text. With `result_path`, it is
instead that file's contents, read after the turn, byte for byte — from the
job's worktree for a `sha` job, from the agent's working directory otherwise.
A file over 1 MiB, or one that is not UTF-8 text, fails the job
(`result_read_failed`) rather than being cut or altered. `result_path` may not
contain a `.git` element (400).

**A `sha` job's worktree is removed after the job**, however it ended —
succeeded, failed, timed out, or muster failing around it — after its result
was read. A removal that fails is noted in `error`
(`could not remove the job's worktree: …`) without changing the status.

**For a job without a `sha`, the result file is deleted after a successful
read, if the repository does not track it.** Left in the agent's working
directory, it would still be there for the next job naming the same
`result_path`, whose turn could then return it as its own if it wrote none;
after a successful read its content is in `result`, byte for byte, so nothing is
lost. It is **kept** in two cases:

- **The read failed** (`result_read_failed` or `timed_out`): the file is then
  the only copy of the agent's output.
- **The repository tracks it** (a `result_path` such as `CHANGELOG.md`):
  deleting it would itself dirty the tree. The job succeeds, and `error` notes
  `the result file <path> is tracked by the repository, so it was left in place, as the turn left it, in the agent's checkout`.

If the delete itself fails — including when git cannot say whether the file is
tracked (git missing, or it refuses the repository), in which case the file is
kept — the job keeps its status and `error` says
`could not delete the result file <path> …`.

**`reason`** is a stable token a caller can branch on:

| `reason` | |
|---|---|
| `rate_limited`, `auth_failed` | ccd's typed failure for the Claude account the agent runs on (`cc_account`). Re-queue on an agent with another account — a pool job submitted again is not given a worker whose account is marked `auth_failed` or is still cooling down after a `rate_limited`, and prefers the account rate-limited longest ago. |
| other ccd types | `busy`, `capacity`, `not_ready`, `empty_reply`, `turn_failed`, … — ccd's vocabulary, passed through unchanged |
| `timeout` | the job's `timeout_s` elapsed (status `timed_out`) |
| `turn_timeout` | ccd's own turn budget elapsed (status `timed_out`) |
| `checkout_failed` | the job's worktree could not be made: the pull request's head could not be fetched anonymously, or with the pool's deploy key (a private repository without one, a key GitHub refuses, no such pull request, no network), the pool's deploy key can push (a dry-run push was accepted), the job's repository is not the one the pool's deploy key is for, the `base_sha` could not be fetched, the `sha` is neither that head nor an ancestor of it, the checkout's own git configuration rewrites the fetch URL or adds a header or credential to it, the default branch (`origin/HEAD`) could not be resolved, the `config_ref` is not on the default branch (or the default branch could not be fetched to look for it), the default branch has a symlink among the overlaid paths (it could point at a file the commit controls), or git failed; `error` says which |
| `workdir_unconfirmed` | a `sha` job's turn succeeded, but the agent's runtime did not confirm it ran in the job's worktree (its image predates job worktrees); the reply was discarded — update the agent's image |
| `model_unconfirmed` | a `model` job's turn succeeded, but the agent's runtime did not confirm it ran in a Claude Code session of the job's own (no `X-Muster-CC-Workdir-Used` on the answer, which ccd sends only from such a session's CLI): an image that ignores the session and model headers runs the turn in the agent's own session on that session's model. The reply was discarded — update the agent's image |
| `result_read_failed` | the turn succeeded but `result_path` could not be returned |
| `empty_reply` | the turn produced no reply text and no `result_path` was asked for |
| `agent_unavailable` | the agent could not be loaded when the job was due to start |
| `interrupted` | nothing finished the job by its deadline, so the reaper failed it: the server running it stopped, crashed or could not record the outcome (see [restarts](#what-it-does-not-do)) |
| `internal` | muster failed around the turn — including a `sha` job, or a job with a `model`, on a deployment whose chat store cannot open a Claude Code session of the job's own, which is refused rather than run in the agent's own session |

## How a job runs

1. The job waits for its agent: jobs on one agent run one at a time.
2. A new chat session is opened on the agent, on the `claude-code` runtime
   and with the job's `model` if it has one, so ccd runs the job in a Claude
   Code CLI of its own (a fresh conversation, started with `--model` when there
   is a model) rather than the agent's main one. The prompt and the reply are stored in
   it, so the job is readable on the agent's page (`session_id`).
3. With `sha`, through the provisioner's exec in the agent's instance:
   - `refs/remotes/origin/HEAD` is resolved (asking `origin` once if the ref is
     missing; the job fails `checkout_failed` if it still cannot be);
   - for a pool with a [deploy key](#private-repositories-a-pools-deploy-key),
     FIRST, through an exec into the pod's key holder: the key is probed for
     write access (a forced dry-run push) and the pull request's head (and the
     `base_sha`) is fetched from `ssh://git@ssh.github.com:443/<repo>.git` into
     the holder's mirror; every fetch below is then from that mirror, a local
     path, with no credential, instead of anonymously from GitHub;
   - pull request `pr`'s head is fetched anonymously from
     `https://github.com/<repo>` (`+refs/pull/<pr>/head`, into a ref of the
     job's own) with git's global and system configuration ignored — where the
     proxy routing of an agent with a GitHub grant lives — and no credential
     helper or extra header; the job fails `checkout_failed` if the checkout's
     own configuration would still rewrite that URL or add a header or
     credential, or if the `sha` is neither the fetched head nor an ancestor
     of it. `repo` is the agent's repository when the request names none;
   - with `base_sha`, that commit is fetched the same way, into
     `refs/muster-jobs/<job_id>-base`;
   - with `config_ref`, the commit is checked to be on the default branch
     (fetching the default branch anonymously when the checkout's
     `origin/HEAD` does not reach it), and becomes the overlay's source;
   - `git worktree add --detach` creates the job's worktree at
     `<the agent's workspace>/.muster-jobs/<job_id>` — beside the agent's
     checkout, on its volume — and the overlay above is applied to it (on a
     pool with a deploy key, the configuration is removed and none restored).

   The agent's checkout, its branch, HEAD and working tree are not touched: an
   agent with uncommitted work is not refused and not changed.
4. The prompt is sent as one turn — the same synchronous call `chief ask` makes
   to ccd's `/v1/responses`. For a `sha` job the turn names the worktree, and
   ccd starts the session's CLI there (it accepts only a job worktree of that
   agent's workspace). The CLI treats a git worktree of the checkout it already
   trusts as trusted, so ccd writes no trust of its own for it. The turn is done
   when ccd answers.
5. With `result_path`, the file is read back through the provisioner's exec.
6. A `sha` job's worktree is removed (`git worktree remove --force`, the
   directory if git left it, `git worktree prune`), and its fetched
   pull-request, default-branch and base refs deleted. For a job without a
   `sha`, the result file is deleted as above.

The job's `timeout_s` covers steps 3–5, not the wait in step 1; the removal in
step 6 has its own bound.

## What it does not do

- **No isolation from the commit's code, and so no third-party commits.** See
  the note at the top.
- **One job at a time per worker.** Each job has its own worktree and its own
  CLI, but one worker's turns share its Claude account and its pod's memory,
  and running several at once on one worker has not been exercised.
- **No survival across a restart.** The turn runs inside the server process,
  and nothing waits for it on shutdown. On a graceful shutdown the server
  fails the jobs it holds itself — queued ones ("nothing was sent to the
  agent") and running ones ("the agent may have done some of the work") —
  with `interrupted`, within 5 seconds; it never touches another server's
  jobs. A job a crashed server leaves unfinished is
  failed `interrupted` by the reaper, which runs at boot and every minute and
  fails a job only once it is past its own deadline: a `running` job at
  `started_at` + `timeout_s` + 5 minutes, a `queued` one at `created_at` + 2h40m
  (the longest a queued job can wait: 4 jobs × (2100 s + 5 minutes)). So a job
  can read `running` for up to that long after its server died, and a job the
  other server of a rolling update is still running is never failed by the new
  one. The same pass ends a job orphaned by a crash in muster or by an outcome
  it could not record (the write is retried 3 times first). The agent may have
  done some of the work, so re-submitting is the caller's call. A server that
  stops or crashes mid-job also leaves that job's worktree on the agent's
  volume (`.muster-jobs/<job_id>`); nothing removes it later.
- **At most 4 jobs queued or running per agent.** A fifth submission naming
  the agent is a 429; a pool skips a worker at the limit, and is a 429 when
  every worker it could otherwise use is at the limit. A repeated
  `idempotency_key` is still answered. A job orphaned by a CRASHED server
  counts toward the 4 until the reaper fails it (a graceful shutdown fails its
  own jobs at once).
- **A pool is chosen from at submission, not re-balanced.** The marks it reads
  are the account pool's: `auth_failed` under the account's current token, and
  the latest `rate_limited`, whose cooldown is judged at submission: a job
  already queued on a worker whose account is then rate-limited is not moved.
- **`result_path` is checked as a string, not resolved.** No `..`, no absolute
  path — but a symlink in the worktree (or the working directory) is followed,
  so the file read can be outside it.
- **A job without a `sha` runs in the agent's own working directory.** Only
  its successfully read, untracked `result_path` file is cleaned up; anything
  else the turn leaves there stays.
- **A timeout does not stop the agent's turn.** It aborts muster's request to
  ccd and ends the job `timed_out`, but the CLI's turn may run on — in a
  worktree that has since been removed — and the next job on that agent can be
  refused `busy`.
- **Not verified end to end against a live muster and cluster.** The handler
  tests run the runner against fakes of the gateway, the chat store and the
  workspace, and against the real adapter's scripts and real git on the test
  host; the real CLI and ccd are exercised together in the image (CI's image
  job), driven directly rather than through a running muster.
