# Design: BoxLite as a runtime for muster

**Status: proposed.** Nothing on this page is built. It is a design proposal
for review: two shapes for running muster's agents in
[BoxLite](https://docs.boxlite.ai) microVMs, each with its user flow,
architecture, security, failure modes, cost and phased slices, and
[open questions](#9-open-questions-for-the-operator) with a recommendation
each. The [recommendation](#10-recommendation) is to build
[shape B](#5-shape-b-a-cloud-computer) first.

Where this page says how muster works today, it cites `file:line` at muster
commit `be827a86` (origin/main when written), and the code is the authority
([decisions.md](../decisions.md)). A claim about BoxLite cites its public
documentation as published for v0.13.1; BoxLite releases often (that was the latest
when this was written), so re-read it before building. A claim nothing confirmed is
marked **UNVERIFIED**. Every figure in the [cost model](#7-cost-model) is
marked **measured**, **documented** (quoted from BoxLite's public price list)
or **estimated**. Hostnames are placeholders: muster is
`muster.example.com`.

## Contents

- [Summary](#summary)
- [1. What BoxLite offers, from its public docs](#1-what-boxlite-offers-from-its-public-docs)
- [2. What muster assumes today](#2-what-muster-assumes-today)
- [3. Goals and non-goals](#3-goals-and-non-goals)
- [4. Shape A: agents run in BoxLite](#4-shape-a-agents-run-in-boxlite)
- [5. Shape B: a cloud computer](#5-shape-b-a-cloud-computer)
- [6. Security, compared with the Kubernetes driver](#6-security-compared-with-the-kubernetes-driver)
- [7. Cost model](#7-cost-model)
- [8. Phased slices](#8-phased-slices)
- [9. Open questions for the operator](#9-open-questions-for-the-operator)
- [10. Recommendation](#10-recommendation)
- [Appendix: a local spike](#appendix-a-local-spike)

## Summary

- **Shape A, agents in BoxLite.** A new provisioner driver,
  `MUSTER_AGENT_PROVISIONER=boxlite`, starts `claude-code` agents as BoxLite
  boxes in the user's own BoxLite account, the way the `kubernetes` driver
  starts pods. Every lifecycle call has a BoxLite counterpart (the
  [mapping](#the-driver-mapped-method-by-method)). Two things are hard: muster
  must **reach into** each box (turns, exec, terminal), and each box must
  **reach muster**, so a self-hosted muster behind Tailscale needs a public
  path both ways, and BoxLite refuses a tunnel to a private box, so each box
  must be made public; and muster-server must hold the user's BoxLite API key,
  which BoxLite documents as **fleet-wide with no narrower scope**.
- **Shape B, a cloud computer.** One box runs Claude Code in tmux plus
  `muster-mirror`, and appears in muster as one more of the user's computers.
  It needs **no change to the mirror wire**: a host already dials out over
  one WebSocket and muster never dials in
  ([internal/mirror/wire.go:16-22](../../internal/mirror/wire.go)). The chief
  starts agents on it with the existing host spawn
  ([mirroring.md](../mirroring.md#starting-agents-for-the-chief--agent-spawn)).
  One-tap creation is the only new muster code.
- **Where it beats today's driver.** BoxLite filters egress **by host name**
  and can keep a credential **out of the VM** (a placeholder the host-side
  proxy swaps for the real value). muster's Kubernetes driver does neither
  today ([limits.md:34-58](../limits.md)).
- **Where it falls short.** No restart policy, no webhooks, no REST logs
  endpoint, a 15-minute idle auto-stop that ignores work inside the box, and
  an org-wide API key. Each is an [ask](#what-wed-need-from-boxlite).
- **Beside the managed network.** Shape B's box is a remote machine, so it
  reaches a self-hosted server exactly as phase 3 of
  [managed-network.md](managed-network.md#phase-3-machines-through-the-relay)
  plans for any machine: through the relay with the `muster-mirror/1` ALPN.
  Until then: Tailscale in the box, or a server with a public HTTPS URL.

## 1. What BoxLite offers, from its public docs

| fact | source |
|---|---|
| Two products: an **embeddable library** (no daemon, no root) and **BoxLite Cloud**, a managed pool behind the same SDK with a URL and an API key; an optional local REST server, `boxlite serve` | [getting-started](https://docs.boxlite.ai/getting-started/index), [cloud vs open source](https://docs.boxlite.ai/cloud/vs-opensource) |
| Hardware isolation: KVM on Linux, Hypervisor.framework on macOS (Apple Silicon), through a libkrun fork; user-mode networking | [networking](https://docs.boxlite.ai/architecture/networking), the GitHub repository |
| SDKs: Python, Node, Rust (the core), Go (a CGO binding), C; a CLI | [installation](https://docs.boxlite.ai/getting-started/installation) |
| Licence: runtime and SDKs Apache-2.0; the cloud stack (`apps/`) AGPL-3.0 | the repository's `NOTICE` |
| Cloud auth: a bearer API key, "a fleet-level credential… There is no narrower key to issue", no expiry by default | [api-keys](https://docs.boxlite.ai/cloud/api-keys) |
| Lifecycle: create, start, stop, remove, get-or-create by name; a crashed main command **stops the box** (no restart policy) | [lifecycle](https://docs.boxlite.ai/manage-sandbox/lifecycle), the Python reference |
| Exec: streamed stdout/stderr, stdin, exit code, kill, signal; a PTY with resize; over REST a WebSocket attach carrying stdin, stdout, stderr and resize/signal/exit, one attach at a time | [pseudo-terminal](https://docs.boxlite.ai/agent-tools/pseudo-terminal), the OpenAPI spec |
| Logs: `boxlite logs -f` in the CLI; **no REST logs endpoint found** in the OpenAPI spec | [CLI reference](https://docs.boxlite.ai/reference/cli) |
| Cloud idle: `auto_stop` 900 s by default, measured from outside the box ("work running inside the box does not count — a long job can be stopped mid-run"); `auto_resume` on; a stopped box keeps its disk but **not process memory** | [box lifecycle](https://docs.boxlite.ai/cloud/box-lifecycle) |
| Persistence on Cloud: the root disk across stop and start; managed volumes on the Team plan only; host bind mounts "accepted and ignored"; checkpoints (disk and config, no memory) instead of snapshots | [volumes](https://docs.boxlite.ai/cloud/volumes), [checkpoints](https://docs.boxlite.ai/cloud/checkpoints) |
| Egress: on or off; `allow_net` takes exact hosts or `*.` wildcards, other names resolve to a sinkhole; fixed at creation on Cloud | [network access](https://docs.boxlite.ai/manage-sandbox/network-access), [network policy](https://docs.boxlite.ai/cloud/network-policy) |
| Ingress on Cloud: private by default; a box can be made public, get per-port preview URLs or signed preview URLs; "inbound.allow_net is not supported yet". A tunnel (`tunnel(port)` in the SDK, `POST /v1/boxes/{id}/network/tunnel` over REST) carries any TCP, verified for HTTP and WebSocket; each prepared tunnel carries one connection, and preparing one is cheap; **a tunnel to a private box is refused**, so the box must have inbound enabled, which makes it public (anyone holding a URL to it reaches it without a key) | [network policy](https://docs.boxlite.ai/cloud/network-policy), [tunnels](https://docs.boxlite.ai/cloud/tunnels) |
| Secrets: a placeholder in the box that the host-side HTTPS proxy replaces with the real value for allowed hosts only ("real values never enter the VM"); "not an authorization boundary" | [secrets and security](https://docs.boxlite.ai/manage-sandbox/secrets-and-security), [cloud secrets](https://docs.boxlite.ai/cloud/secrets) |
| Images on Cloud: BoxLite maintains four images, `base`, `python`, `node` and `sandbaseai-hermes`; the docs say any image on a public registry boots, and a private image boots once its registry login is registered; the product FAQ says only the aliases `base`, `python`, `node`, and that a Docker reference is rejected. **The two disagree** | [images](https://docs.boxlite.ai/cloud/images), [private images](https://docs.boxlite.ai/cloud/private-images), the product site's FAQ |
| Webhooks or events for box state: **none found** | the OpenAPI spec |
| Regions: **none documented** | — |
| Plan ceilings per box: pay-as-you-go 1 vCPU / 2 GiB / 10 GiB and 2 running at once; Starter 2 / 4 / 20 and 4; Team 16 / 32 / 100 and 1,000 | [plans](https://docs.boxlite.ai/cloud/plans) |
| Sizes: presets Small 1 / 1 GiB / 10 GiB, Medium 2 / 4 / 20, Large 4 / 8 / 50, or a custom size: fractional CPU, memory in whole MiB from 128 MiB, chosen independently, within the plan's ceiling | [box sizes](https://docs.boxlite.ai/cloud/box-sizes) |
| Checkpoints "do not… support boxes with volumes or Secrets" | [cloud vs open source](https://docs.boxlite.ai/cloud/vs-opensource), [checkpoints](https://docs.boxlite.ai/cloud/checkpoints) |

## 2. What muster assumes today

| fact | where |
|---|---|
| The lifecycle switch is `MUSTER_AGENT_PROVISIONER`: `none`, `noop`, `kubernetes` | [provisioner.md:18-26](../provisioner.md) |
| The driver interface, `Provisioner`: `Driver`, `Capabilities`, `Create`, `Update`, `Scale`, `Destroy`, `List`, `Get`, `Endpoint`, `StreamLogs`; `Exec`, `ExecContainer`, `ExecTTY` optional and gated by `Capabilities.Exec` | [internal/provision/provision.go:107-301](../../internal/provision/provision.go) |
| Four driver rules: a blind driver errors, never returns an empty set; `Destroy` is nil only if the instance is gone; `Create` is idempotent and refuses a divergent spec; capabilities are branched on in one place | [provisioner.md:778-801](../provisioner.md), [internal/provision/provisiontest/contract.go:76](../../internal/provision/provisiontest/contract.go) |
| Isolation is an ordered enum, `none` < `process` < `container` < `namespace`; nothing reads it yet | [internal/provision/capabilities.go:10-31](../../internal/provision/capabilities.go), [:61-68](../../internal/provision/capabilities.go) |
| The adapter's lifecycle: `Dispatch` (`Create`), `Start` (`Update` when the kind runs the pod supervisor or a credential resolver is wired, otherwise `Scale` 1; muster-server always wires one, so there every kind starts with `Update` alone, and `Scale` 1 happens only in a wiring with no resolver), `Stop` (`Scale` 0), `Destroy`, `StreamLogs`, `AttachTerminal` (`ExecTTY`, then a second `Exec` to release the client) | [internal/agentprovision/agentprovision.go:677](../../internal/agentprovision/agentprovision.go), [:784-848](../../internal/agentprovision/agentprovision.go), [:875-896](../../internal/agentprovision/agentprovision.go), [:932-947](../../internal/agentprovision/agentprovision.go), [:1024](../../internal/agentprovision/agentprovision.go), [:1046-1095](../../internal/agentprovision/agentprovision.go), [cmd/muster-server/provisioner.go:262](../../cmd/muster-server/provisioner.go) |
| A `claude-code` agent: `ccd` serves turns on port 18789; the Claude subscription token is `CLAUDE_CODE_OAUTH_TOKEN` in the agent's own Secret; API-key variables are refused | [internal/agentspec/agentspec.go:149](../../internal/agentspec/agentspec.go), [internal/agentspec/claudecode.go:43](../../internal/agentspec/claudecode.go), [images/claude-code-agent/credentials.sh:52](../../images/claude-code-agent/credentials.sh) |
| Its budget: 250m CPU and 512Mi requested, 3Gi memory limit; a `/data` volume, 10Gi by default | [internal/agentspec/claudecode.go:63](../../internal/agentspec/claudecode.go), [provisioner.md:259-260](../provisioner.md) |
| Its network on Kubernetes: ingress only from muster; egress to cluster DNS, TCP 443 on public IPv4, and muster's connect port; **no egress by name** | [provisioner.md:259-268](../provisioner.md), [limits.md:34-58](../limits.md), [internal/provision/types.go:337-404](../../internal/provision/types.go) |
| GitHub credentials never enter the pod: git goes through muster's connector proxy | [provisioner.md:310-314](../provisioner.md) |
| The compose mode of the agent image: `ccd compose` reads a spec file every 2 s, supervises the CLI as uid 1000, keeps the spec's secrets unreadable by it, and restarts the child with backoff | [provisioner.md:354-427](../provisioner.md) |
| The silence watchdog and the stopped-agent reaper run in muster. The watchdog reads muster's agent rows. The reaper reads the rows (agents stored `stopped` for 14 days), then the driver's `List`, skipping any agent whose instance is not stopped and destroying nothing if `List` fails; it never stops a running one. An agent with no instance at all counts as stopped | [provisioner.md:600-628](../provisioner.md), [provisioner.md:683-745](../provisioner.md), [internal/api/agent_watchdog.go:79](../../internal/api/agent_watchdog.go), [internal/api/agent_reap.go:101-135](../../internal/api/agent_reap.go), [internal/agentprovision/agentprovision.go:1019-1020](../../internal/agentprovision/agentprovision.go), [internal/agents/status.go:32-36](../../internal/agents/status.go) |
| A host dials out over one WebSocket; muster holds no address, key or inbound route to it; at most 64 hosts connected to one server at once, counted across all owners when a host connects | [internal/mirror/wire.go:16-22](../../internal/mirror/wire.go), [internal/mirror/wire.go:125](../../internal/mirror/wire.go) |
| `muster-mirror` refuses plain http to another machine | [internal/mirrorhost/client.go:183-189](../../internal/mirrorhost/client.go) |
| The chief's dispatch targets are `pod/claude-code` and `host/<host>/<runtime>` | [internal/api/agent_dispatch.go:255](../../internal/api/agent_dispatch.go), [:297](../../internal/api/agent_dispatch.go) |

## 3. Goals and non-goals

### Goals

- Agents that keep working when the user's laptop is closed, without the user
  running a Kubernetes cluster.
- Everything driven from the phone: start a computer, watch it, answer it,
  stop it.
- The chief can start work on a BoxLite-backed machine like any other.
- Credentials stay where they are today or move further from the agent,
  never closer.

### Non-goals

- muster reselling compute. The user's own BoxLite account pays BoxLite.
- Replacing the Kubernetes or compose drivers.
- A new isolation model for mirrored hosts. A cloud computer is the user's
  machine, as trusted as their laptop.

## 4. Shape A: agents run in BoxLite

### User flow

On the phone, in plain words:

1. **Account → Agents → Run agents in the cloud.** "muster can start your
   agents on BoxLite, a service that runs each one in its own small virtual
   computer. You pay BoxLite for the time they run."
2. "Connect your BoxLite account": paste a key from BoxLite's console (with a
   link straight to the page that makes one). muster checks it at once and
   shows the plan's limits: "Your plan runs up to 4 agents at a time."
   (Basic, a `claude-code` agent's budget, needs the Starter plan, which
   runs 4 at once; [§7](#7-cost-model).)
3. From then on, the chief starts agents there: "Start an agent on the
   upload bug" works as before, and the agent's card says **In the cloud**.
4. The card shows what it costs while it runs ("about 6¢ an hour") and a
   **Stop** button. A stopped agent costs almost nothing and keeps its work.

### Architecture

```mermaid
flowchart LR
  phone["Phone"] --> server["muster-server"]
  server -->|"BoxLite API (user's key)"| api["BoxLite Cloud API"]
  api --> box["box: claude-code agent image<br/>ccd compose → ccd → Claude Code"]
  server -.->|"turns, exec, terminal<br/>(tunnel or attach)"| box
  box -.->|"API, connect port, MCP<br/>(public HTTPS)"| server
  box -.->|"allow_net"| anthropic["model API"]
```

- **The driver**, `internal/provision/boxlite`, speaks BoxLite's REST API
  with the standard library (the Go SDK is a CGO binding, and
  `internal/provision` imports nothing outside the standard library,
  [provisioner.md:765-767](../provisioner.md)).
- **Inside the box: the compose mode, not the Kubernetes mode.** The agent
  image's `ccd compose` already does what a box lacks: it supervises the CLI
  and restarts it with backoff (BoxLite has no restart policy), runs it as
  uid 1000 so it cannot read its own spec's secrets, and takes its
  configuration as a file. The driver writes `spec.json` and the managed
  files with BoxLite's files API instead of a shared volume, and the box's
  main command is `ccd compose`, which never exits on its own.
- **muster → box.** Turns go to `ccd` on port 18789; terminal and exec use
  BoxLite's exec and its WebSocket attach. Turns need a tunnel to 18789
  (`POST /v1/boxes/{id}/network/tunnel`, documented to carry HTTP and
  WebSocket). Each prepared tunnel carries one connection, so the driver
  prepares one per turn; that is cheap. ⚠ **A tunnel needs the box to have
  inbound enabled, which makes it public:** anyone holding a URL to the box
  reaches it without a BoxLite key. So with a tunnel, as with a preview URL,
  `ccd` is on the internet behind only its bearer. A tunnel to a private box
  is an [ask](#what-wed-need-from-boxlite).
- **box → muster.** The agent calls `MUSTER_AGENT_API_URL` and
  `MUSTER_AGENT_CONNECT_URL` (connectors, MCP, approvals). From a public
  cloud these must be public HTTPS names. A tailnet-only server has none, so
  shape A needs either a public URL or the managed network; and whether the
  managed network's tunnel listener would serve the agent route families is
  a question for that design ([which routes the tunnel listener serves](managed-network.md#which-routes-the-tunnel-listener-serves)).

### The driver, mapped method by method

| muster call | driver method | BoxLite | notes |
|---|---|---|---|
| — | `Driver()` | — | `"boxlite"` |
| — | `Capabilities()` | — | see the next table |
| Dispatch | `Create` | `get_or_create` by name, then files PUT of `spec.json` | Idempotence by name; the spec's fingerprint must be stored with the box. Whether a box carries labels or metadata to hold it and an ownership mark is **UNVERIFIED**; without them, muster's own row is the record |
| Start | `Update` (muster-server always wires a credential resolver, so the adapter calls `Update` alone for every kind, and `Update` must also start a stopped box); `Scale(1)` only in a wiring with no resolver | files PUT (new generation), then start if stopped | Image, size and egress are fixed at creation, so an `Update` that changes them is a **recreate**, which loses the disk unless something carries it (see [the disk and the secrets](#the-disk-across-a-recreate-and-the-secrets)) |
| Stop | `Scale(0)` | stop | Disk kept, memory not: the next start resumes the conversation by id from the transcript, as a restarted pod does |
| Delete, reaper | `Destroy` | remove (`force`) | Ownership is checked before anything is removed (rule 2): a box under the name that muster did not create is `ErrNotManaged` |
| status, reaper, agent list | `List`, `Get` | list and get boxes | An unreachable API, or a key that cannot see the account, is an error (`ErrBlind`), never an empty list (rule 1): the reaper reads `List` before it destroys, and an agent with no instance at all counts as stopped, so an empty list from a blind driver would make every due agent reapable. No webhooks, so state is polled |
| turns | `Endpoint` | tunnel to 18789 | see muster → box above |
| Logs | `StreamLogs` | **no REST logs endpoint**; an exec of `tail -F` on `ccd`'s log file | **UNVERIFIED** that the console log is reachable any other way over REST |
| Terminal | `ExecTTY`, then `Exec` | exec with a PTY, attach WebSocket, resize; a second exec releases the tmux client | BoxLite allows one attach per execution, which matches one terminal per exec |
| (key holder) | `ExecContainer` | — | not offered: one VM, one container (below) |

### Capabilities it would declare

| capability | value | why |
|---|---|---|
| `Isolation` | a new level above `namespace`, say `vm` | a microVM per agent with its own kernel; the enum is ordered and has no such level yet, and nothing reads it ([capabilities.go:61-68](../../internal/provision/capabilities.go)) |
| `Secrets` | true | the compose-mode spec file, 0640 in a group the CLI's uid is not in, as on the compose install |
| `Files` | true | the files API |
| `Persistence` | true | the root disk survives stop and start |
| `ResourceLimits` | true | vCPU and memory are set at creation |
| `Scale` | false | 0 and 1 only |
| `Exec` | true | exec, PTY, attach |
| `NetworkIsolation` | **only with a new `Network` field** | BoxLite allows by **host name**, while `Network` speaks ports and address blocks ([types.go:337-404](../../internal/provision/types.go)). "TCP 443 on any public address" cannot be said in `allow_net`, and saying nothing opens every port. So the driver refuses `Isolate` until `Network` gains a host allowlist, which is the control [limits.md:34-38](../limits.md) says muster lacks |
| `Sidecar`, `KeyHolder`, `ImagePullSecret` | false | one container per box; refused rather than dropped (a private-repository pool's deploy key is therefore refused on this driver) |

### Credentials and where they live

| credential | today on Kubernetes | in a box |
|---|---|---|
| the user's BoxLite API key | — | muster's encrypted connector store, as a member's own credential ([auth.md](../auth.md)); **fleet-wide**: anyone holding it can run, read and delete every box in the account |
| Claude subscription token | env var from the agent's Secret | a BoxLite **secret placeholder** scoped to the model API's host: the real value never enters the VM. **UNVERIFIED** that Claude Code accepts a placeholder token (it may inspect it locally); if not, the compose spec file, as today |
| the agent's muster token and turn bearer | the agent's Secret | the compose spec file, unreadable by the CLI's uid; the muster token could also be a placeholder scoped to muster's host |
| GitHub | never in the pod; muster's connector proxy | unchanged: git goes through the connect URL |

### Repository checkout, persistence and the watchdog

- **Checkout:** unchanged. `ccd` clones into `/data/workspace/<name>` through
  muster's proxy ([provisioner.md:302-314](../provisioner.md)); `/data` is the
  box's root disk.
- **Persistence between turns:** the box keeps running between turns; across
  a Stop it keeps its disk. The driver must create every box with
  `auto_stop` **off**, because BoxLite's idle clock ignores work inside the
  box and would stop an agent mid-turn. With `auto_stop` off, **nothing stops
  a forgotten running box except the user**, or, for an agent the chief
  started, its task becoming `complete`
  ([provisioner.md:681](../provisioner.md)). The stopped-agent reaper does not:
  it destroys agents that have already been stopped for 14 days and never
  stops a running one ([provisioner.md:685-688](../provisioner.md),
  `ListReapableStopped` in [agent_reap.go:101](../../internal/api/agent_reap.go)).
  So the card must show that it is running and what it costs, and muster
  must push after 24 hours with no session activity
  ([question 3](#9-open-questions-for-the-operator)).
- **The silence watchdog** is muster's and does not change. A box BoxLite
  stopped underneath an agent reads as stopped in `List`, its turn fails as a
  connection to a restarting instance (not a failure,
  [provisioner.md:529-538](../provisioner.md)), and the watchdog marks it
  `stuck` after 30 minutes. The driver should report the BoxLite reason in
  `Instance.Reason` so the push says why.

### The disk across a recreate, and the secrets

Two plans in this shape collide. A recreate (a size or egress change) would
carry the agent's disk by a checkpoint, and slice [A3](#shape-a-after) would
keep the Claude token out of the VM as a secret placeholder. But BoxLite's
checkpoints do not support boxes with Secrets (or with volumes)
([cloud vs open source](https://docs.boxlite.ai/cloud/vs-opensource)), so a
box can have one or the other, not both.

**Pick the placeholder, drop the checkpoint.** Keeping the token out of the
VM serves a goal of this design (credentials move further from the agent);
a checkpoint only saves the user from re-cloning after a resize, which is
rare. So: a size or egress change on a box with work in it is refused unless
the user confirms that unpushed work is lost; on the Team plan, `/data` can
be a managed volume, which outlives a recreate without a checkpoint. If A3
finds that Claude Code refuses a placeholder, the token stays in the compose
spec file and checkpoints become available again.

### Egress

Recommended allowlist for an agent box, all by name: muster's public host,
the model API, and `github.com` (anonymous fetches for jobs at a commit,
[provisioner.md:338-347](../provisioner.md)). Everything else resolves to the
sinkhole. That is the officer instance's lock
([officers.md](../officers.md#the-instances-network)) without needing a
cluster with an FQDN-aware network plugin. An agent that must install
packages needs its registries added, by the user, by name.

### Failure modes

| failure | what the user sees | handling |
|---|---|---|
| BoxLite API unreachable | cards keep their last status | `ErrBlind`; nothing rewritten (rule 1) |
| plan's concurrency ceiling reached | "Your BoxLite plan runs 4 agents at once; stop one or upgrade" | refuse at dispatch with BoxLite's error, before a row is created |
| key revoked or expired | "muster can no longer reach your BoxLite account" | every call `ErrBlind` with the auth reason; a banner on Account |
| box crashed | agent `stuck`, then Start | `ccd compose` restarts the CLI; only a crash of `ccd compose` itself stops the box |
| BoxLite stopped the box (idle, plan) | `stuck` with the reason | `auto_stop` off at create; report the reason |
| muster unreachable from the box | kickoff never delivered → `failed` after 15 min | the doctor line checks the public URL from outside before the first dispatch |
| a recreate on `Update` | the agent's files are gone | refuse a size or egress change on a box with work in it unless the user confirms; on Team, a managed volume for `/data` (below) |

### What shape A would need changed in muster

- the driver and its contract run (`provisiontest.RunContract` against a fake
  BoxLite API, with blind and restricted variants);
- a `vm` isolation level;
- a host-name allowlist in `provision.Network`, rendered by this driver and
  refused by drivers that cannot enforce it;
- the compose-mode spec delivered by a files API rather than a shared volume;
- a BoxLite credential kind in the connector store.

## 5. Shape B: a cloud computer

### User flow

1. **Computers → Add a computer → A computer in the cloud.** "Get a computer
   that stays on, so your agents keep working while you sleep."
2. Connect the BoxLite account (as in shape A, once), pick a size ("Basic:
   about $41 a month if it is always on"), tap **Create**. (Basic, 0.25 vCPU
   / 3 GiB / 10 GiB, is this design's name, not one of BoxLite's presets; it
   needs BoxLite's Starter plan, [§7](#7-cost-model).)
3. Within a few seconds the computer appears in the list, **online**.
4. Open it. The first time, it asks you to sign in to Claude, in its
   terminal, the same way as on a laptop.
5. From then on it is one of your computers: its sessions are listed, you
   read and answer them from your phone, and the chief can start work on it
   ("start an agent on the upload bug on your cloud computer").
6. Its ⋯ menu has **Turn off** (keeps everything, costs a little for
   storage), **Turn on**, and **Delete** (two steps).

### Architecture

```mermaid
flowchart LR
  phone["Phone"] --> server["muster-server"]
  server -->|"create, start, stop, delete<br/>(user's BoxLite key, only for these)"| api["BoxLite Cloud API"]
  api --> box["box: tmux · Claude Code · muster-mirror"]
  box -->|"one outbound WebSocket, mirror token"| server
  box -.->|"allow_net"| out["model API · GitHub · package registries"]
```

- **The image**, `images/cloud-computer` (new): tmux, Claude Code, git, `gh`,
  and `muster-mirror` started with `-interactive -agent-spawn`
  ([mirroring.md](../mirroring.md#starting-agents-for-the-chief--agent-spawn)).
  Its main command is a small supervisor that restarts `muster-mirror` and
  keeps the tmux server up, because BoxLite stops a box whose main command
  exits.
- **Connecting back is outbound only, today's wire.** `muster-mirror` dials
  `wss://muster.example.com/mirror/connect` with its mirror token
  ([mirroring.md:90-120](../mirroring.md)); terminals and rendered views are
  multiplexed down that one connection. muster needs **no** inbound route to
  the box, so the box stays private and needs no tunnel, preview URL or
  public port.
- **Reaching a self-hosted server,** in order of preference:
  1. **The managed network** once its phase 3 ships: the box is a remote
     machine, connecting through the relay with the `muster-mirror/1` ALPN
     ([managed-network.md](managed-network.md#phase-3-machines-through-the-relay)).
     Nothing in this design changes the relay.
  2. **A server with a public HTTPS URL** (behind its own proxy).
  3. **Tailscale inside the box**, joined with an ephemeral, tagged auth key.
     **UNVERIFIED** that `tailscaled` runs in a box: without a TUN device it
     needs userspace networking, and `muster-mirror` would then dial through
     its proxy.
- **One-tap creation** is the only new muster code: muster mints a mirror
  token for a new host name, then calls BoxLite to create a box from the
  image, by digest, with `auto_stop` off, the egress allowlist, and the token
  and server URL in its environment. BoxLite's API key is used for create,
  start, stop and delete only; a running computer needs nothing from muster
  but its own token.
- **The chief** dispatches to `host/<box>/claude`
  ([agent_dispatch.go:297](../../internal/api/agent_dispatch.go)), under the
  host's own spawn rate limit and repository allowlist.

### Credentials

A cloud computer is **the user's machine**: what is in it is what they put
there. The Claude sign-in, a `gh` login and the user's git identity live in
the box, as on a laptop. That is a different promise from shape A, where the
agent never holds a GitHub credential, and the UI must not blur the two.

- **The mirror token** could be a BoxLite secret placeholder scoped to
  muster's host, so it never enters the VM. ⚠ That requires BoxLite's proxy
  to terminate TLS to muster to substitute the header, so BoxLite would see
  the mirror tunnel in the clear. Against the managed network's
  end-to-end, blind-relay decision
  ([managed-network.md](managed-network.md#operator-decisions), decision 1),
  that is the wrong trade: **keep the token in the box** (0600, as on any
  host) and the tunnel end to end.
- **The BoxLite key** is in muster's connector store, as in shape A, and is
  never sent into the box.

### Failure modes

| failure | what the user sees | handling |
|---|---|---|
| box stopped by BoxLite | the computer goes **offline**, as a laptop does | `auto_stop` off at create; show the reason from BoxLite's status on the ⋯ menu |
| `muster-mirror` crashes | offline for seconds | the image's supervisor restarts it; it reconnects with backoff (1 s to 60 s) |
| server unreachable | offline | the doctor line; nothing is lost in the box |
| token revoked | offline; the computer stays in BoxLite | the ⋯ menu offers **Reconnect**, which mints a new token and writes it with the files API |
| BoxLite key revoked | the computer keeps working; Turn on/off and Delete fail | a banner on Account; turning it off in BoxLite's own console still works |
| 64 hosts connected to the server | the box is created but its mirror is refused when it connects, so it shows **offline** with the reason | the existing ceiling, counted across all owners at connect time ([wire.go:125](../../internal/mirror/wire.go)); creation cannot reserve a place, so the ⋯ menu says why and offers **Turn off** |

## 6. Security, compared with the Kubernetes driver

| property | Kubernetes driver today | shape A | shape B |
|---|---|---|---|
| isolation | a container per agent, a namespace each; shares the node's kernel | a microVM per agent, own kernel | a microVM per computer; every session in it shares it |
| egress | public TCP 443 by address range; no name filter; depends on the cluster's network plugin | allowlist by name, enforced by BoxLite | allowlist by name, chosen by the user |
| model token | in the pod's environment | a placeholder, if Claude Code accepts one (**UNVERIFIED**) | in the box, as on a laptop |
| GitHub | never in the pod | never in the box | in the box, the user's own |
| muster's credential for the backend | a ServiceAccount, narrowed by RoleBindings | the user's BoxLite key: **whole account** | the same key, used for four calls only |
| reach into the agent | cluster network, ingress only from muster | a BoxLite tunnel, which needs the box public: `ccd`'s bearer is the only guard | none: the box dials out; the box stays private |
| who else sees traffic | the cluster | BoxLite, for every substituted secret (its proxy terminates TLS for those hosts) | BoxLite only if a placeholder is used; otherwise the tunnel is end to end |
| prompt-injected agent's reach | any public HTTPS host | the allowlisted names | the allowlisted names, plus anything the user signed in to |

Two points matter most:

- **BoxLite's key is the weakest link in both shapes.** It is fleet-wide and
  does not expire by default. muster-server holding it means a muster
  compromise is a BoxLite-account compromise. Shape B uses it far less, and
  could drop it entirely (the user creates the box in BoxLite's console and
  pastes a one-line setup), at the cost of the one-tap flow.
- **A secret placeholder is not an authorization boundary** (BoxLite says so):
  code in the box can still use the credential against the allowed host. It
  keeps the token from being copied out, not from being used.

## 7. Cost model

| figure | value | how obtained |
|---|---|---|
| vCPU | $0.0504 per vCPU-hour | **documented**, [pricing](https://docs.boxlite.ai/cloud/pricing) |
| memory | $0.0144 per GiB-hour | **documented** |
| disk | $0.00018 per GiB-hour; a stopped box pays disk only | **documented** |
| network, API calls, boot time | no charge; no minimum duration | **documented** |
| BoxLite's Small preset, 1 vCPU / 1 GiB / 10 GiB | $0.0666/h; $47.95 per 30 days on, $1.30 stopped | **documented**, [box costs](https://docs.boxlite.ai/cloud/box-costs) |
| BoxLite's Medium preset, 2 / 4 / 20 | $0.1620/h; $116.64 per 30 days on, $2.59 stopped; needs the Starter plan or above, since pay-as-you-go caps a box at 1 vCPU / 2 GiB / 10 GiB | **documented**, [plans](https://docs.boxlite.ai/cloud/plans) |
| a `claude-code` agent's budget today | 3Gi memory limit, 250m CPU requested | read from code ([claudecode.go:63](../../internal/agentspec/claudecode.go)) |
| "Basic": one agent at that budget, 0.25 vCPU / 3 GiB / 10 GiB | $0.0576/h; ~$41 per 30 days always on | **estimated** from the unit rates; custom sizes are **documented** ([box sizes](https://docs.boxlite.ai/cloud/box-sizes): fractional CPU, memory in whole MiB). 3 GiB exceeds pay-as-you-go's 2 GiB, so it needs Starter |
| one agent, 1 / 2 / 10 (pay-as-you-go's largest) | ~$0.081/h; ~$58 per 30 days always on | **estimated**; whether Claude Code with a repository's tooling fits in 2 GiB is **UNVERIFIED** |
| one agent working 8 h a day, stopped otherwise | ~$15-20 a month | **estimated**: 240 running hours at the two rates above ($13.82, $19.44), plus the 480 stopped hours' disk (10 GiB, ~$0.86) |
| the Starter plan | $5 fee, with $20 of included quota, then the wallet; the docs name no billing interval or renewal schedule | **documented**, [plans](https://docs.boxlite.ai/cloud/plans) |
| a cloud computer, Medium, always on | ~$117 a month | **documented** size (Starter plan), **estimated** usage |
| model usage | not included | the user's own Claude subscription in both shapes |

## 8. Phased slices

Each slice is tested before the next.

### Shape B first

- **B0, spike (no product change).** Boot a box on BoxLite Cloud from a
  candidate image, start `muster-mirror` against a test server with a public
  URL, and measure the time from the create call to the host being listed.
  Check the egress allowlist with a **negative control** (a host not on the
  list must fail to resolve) and a positive one. Check whether `auto_stop=0`
  holds overnight. *Closing check:* a results page like
  [previews-spike.md](previews-spike.md) with the measured numbers.
- **B1, the image and a manual guide.** `images/cloud-computer`, published
  by digest, and a guide that creates the box with BoxLite's own CLI.
  *Closing check:* CI builds the image, runs it under the open-source
  BoxLite runtime (on a runner with KVM; **UNVERIFIED** that the CI runners
  have it, otherwise under Docker) against the compose stack's muster, and
  asserts the host's windows appear in the host list.
- **B2, one tap.** Account → Computers → **Add a computer in the cloud**: the
  BoxLite credential kind, token minting, create, and the computer's ⋯ menu
  (on, off, delete, reconnect). *Closing check:* an e2e test against a fake
  BoxLite API (`httptest`) asserting the create request's image digest,
  egress list and `auto_stop`, plus a negative control (a refused key leaves
  no host row and no token).
- **B3, through the managed network.** Depends on that design's phase 3.
  *Closing check:* a cloud computer with no public route to the server
  connects through the relay and is listed.

### Shape A after

- **A0, the driver against a fake.** `internal/provision/boxlite` passing
  `provisiontest.RunContract` with its blind and restricted drivers.
  *Closing check:* the contract green, and red with `Destroy` changed to
  return nil when the box survives.
- **A1, a live agent.** A `claude-code` agent dispatched to BoxLite, its
  kickoff delivered, its terminal attached, stopped and started with its
  checkout intact. *Closing check:* `make test-liveenv` against a BoxLite
  account, recorded.
- **A2, egress by name.** The host allowlist in `provision.Network`, refused
  by drivers that cannot enforce it. *Closing check:* from an agent box, a
  name off the list fails and the model API succeeds; the Kubernetes driver
  refuses a spec asking for it.
- **A3, the token out of the VM.** The Claude token as a placeholder, if B0
  or A1 shows Claude Code accepts one. *Closing check:* the token's value is
  absent from the box (`/proc/*/environ` and the disk) while a turn
  succeeds.

### What we'd need from BoxLite

1. **A scoped API key**: create, stop and delete only boxes it created (or
   carrying a label), with an expiry. This is the largest risk in both
   shapes.
2. **Labels or metadata on a box**, to hold muster's ownership mark and spec
   fingerprint.
3. **A restart policy**, or a documented guarantee about the main command.
4. **An idle clock that counts work inside the box**, or a documented
   always-on mode.
5. **Webhooks** for box state, so muster need not poll.
6. **A REST logs endpoint.**
7. **Images on Cloud:** confirm any public OCI image by digest boots (the docs
   and the FAQ disagree).
8. **A tunnel to a private box**, authorized by the API key, or an inbound
   allowlist, so muster can reach a box's port without making the box
   public. (The REST tunnel exists; it refuses a private box.)
9. **Secrets:** state which TLS terminations the proxy does and allow named
   hosts to bypass it.
10. **Regions and data location.**
11. **A connect flow** (OAuth or similar) so a non-technical user never
    copies an API key.

## 9. Open questions for the operator

1. **Which shape first?** **Recommendation: B** ([§10](#10-recommendation)).
   **Decided: B first.**
2. **Hold the user's BoxLite key in muster, or not?** **Recommendation:
   yes, for B's four lifecycle calls only**, with a plain warning on the
   connect page that the key reaches the whole BoxLite account. Revisit once
   a scoped key exists (ask 1).
3. **Is `auto_stop` off the default?** **Recommendation: yes**, and two
   things are **required** with it, not optional: the running cost on the
   card, and a push after 24 hours with no session activity. With `auto_stop`
   off nothing else stops a forgotten running box (the reaper only destroys
   boxes already stopped for 14 days). Agents that stop mid-turn are worse
   than a visible bill.
4. **Shape B's default size?** **Recommendation: the smallest that runs
   Claude Code with a repository's tooling**, measured in B0; until then
   Basic, a `claude-code` agent's budget today (0.25 vCPU / 3 GiB / 10 GiB,
   ~$41 for 30 days on, Starter plan), or 1 vCPU / 2 GiB on pay-as-you-go.
5. **Tailscale inside the box before the managed network ships?**
   **Recommendation: no.** Support public-URL servers in B1-B2 and wait for
   the relay. Tailscale in a box is another credential and an unverified
   network setup.
6. **Shape A's egress default?** **Recommendation: the name allowlist** in
   [§ egress](#egress), not open egress, from the first release.
7. **Whose image?** **Recommendation: muster's own, by digest**, never a
   BoxLite alias, so the image is the same one CI tests.

## 10. Recommendation

**Build shape B first.**

- **It reuses a wire that exists.** A host dialling out over one WebSocket is
  muster's core path; a cloud computer is one more host. Shape A needs muster
  to reach into each box and each box to reach muster's agent routes.
- **It needs almost nothing from BoxLite.** Boxes stay private, there are no
  tunnels, logs or webhooks to depend on, and the BoxLite key is used four
  ways rather than for every turn.
- **It meets the goals in [§3](#3-goals-and-non-goals).**
- **It sits beside the managed network**, as one of its remote machines.
- **Shape A is better isolation**, and egress by name would close a gap
  muster has today. It comes next, on the driver seam, once the asks that
  bite it hardest (a scoped key, a restart policy, labels, logs, a tunnel to a
  private box) are answered.

## Appendix: a local spike

The open-source runtime was run on one Linux x86_64 workstation with KVM,
through the Python SDK (`pip install boxlite`, v0.13.1), with `alpine:latest`.
No account was needed. These numbers describe that machine and the local
runtime only; **BoxLite Cloud was not measured**.

| measurement | samples | median |
|---|---|---|
| image pull, cold | 1127, 7322, 1038 ms | 1127 ms |
| first box after the pull, create and start to the first output | 5669, 18103, 3505 ms | 5669 ms |
| a new box, image cached (create, start, exec) | per-run medians 1159, 1001, 1176, 1022 ms | ~1.0-1.2 s |
| a stopped box started again (start, exec) | per-run medians 516, 350, 482, 452 ms | ~0.35-0.5 s |
| stop | 111-275 ms | — |
| `exec echo` in a running box | first 16-23 ms, then 3-5 ms | ~3-4 ms |

**Measured** besides: a file written before a stop was there after the
start. Under an allowlist, a DNS query to a public resolver and an HTTP
request to an address literal both failed, while both worked with egress
open; the allowed-name positive case could not be checked, because name
resolution inside the box failed on this workstation in every mode (most
likely its local resolver setup, not BoxLite: **UNVERIFIED**). On a
distribution whose dynamic loader is not at the standard path, three of the
runtime's bundled helper binaries need their interpreter patched before a box
starts.
