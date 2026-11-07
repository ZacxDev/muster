# Design: hosted muster, self-serve tenants

**Status: proposal. Nothing here is built.** This page scopes a new product
arc: an organization signs up, pays, and gets its own muster deployment at
`https://<org>.<tenant-domain>`, provisioned automatically on the operator's
cluster and added to the muster Android app, with a website, a portal and an
MCP server so an agent can do the whole setup. The open-source README then
offers it as the alternative to self-hosting.

Conventions, as in [multi-tenant.md](multi-tenant.md): a claim about muster
cites the code or a doc in this repository at commit `86f2cda9`; a claim about
the hosting cluster was **measured read-only while this page was written** and
says so; anything else is marked **UNVERIFIED**. Hostnames are
placeholders: `<product-domain>`, `<tenant-domain>`, `<personal-domain>` (the
operator's existing personal domain, which fronts their own services today).
The cluster's private topology (addresses, node names, internal hostnames)
stays out of this page on purpose: this is a public repository and
`tests/leakscan.py` holds it to that.

Here a **tenant** means what it means in multi-tenant.md: one separate muster
deployment at its own origin. Hosting does not make muster-server multi-tenant
either. It runs N ordinary single-tenant deployments for N customers.

## Summary: the recommendations

| # | question | recommendation |
|---|---|---|
| 1 | where the pieces live | A new **private** repository, `muster-hosted`: one Go binary serving the marketing site, the portal, the Stripe webhook, the control plane and the MCP server. muster itself gains only generic switches (below) and a README section. |
| 2 | provisioning | **GitOps into a dedicated tenants repository** (not the operator's infra repository), reconciled by Flux under an impersonated ServiceAccount that can touch only tenant namespaces. One commit per tenant change; a Flux webhook makes it ~1–2 min. A CRD controller is the later step if volume ever needs it. |
| 3 | isolation | 🔴 **No cluster agents for hosted tenants.** Provisioner `none`; no chief pod, no officers, no worker pools. Hosted tenants run the server, mirrored sessions on their own machines, tasks, asks, push, dictation by URL, focus and suggestions on their own key. Every tenant pod: own namespace, Pod Security `restricted`, no ServiceAccount token, ResourceQuota and LimitRange, a Cilium egress allowlist by name, private ranges denied, a tainted node pool. |
| 4 | data | **One Postgres per tenant** (with its GoTrue schema in it), on NVMe, with a nightly `pg_dump` to a **dedicated hosted object-store tenant**, a weekly automated restore drill, export on demand, deletion 30 days after cancel. |
| 5 | credentials | **BYO model keys only** (Claude setup-token, OpenRouter key), stored in the tenant's own muster connector store, never by the control plane. No metered resale. Platform secrets per tenant are SOPS-encrypted in the tenants repository with an age key only the cluster holds. |
| 6 | billing | Stripe **Checkout** (subscription mode) + the **Customer Portal**, state driven only by verified webhooks, deduplicated by event id, mapped through the Subscription id. `active → past_due (7 days) → suspended (read-only, 14 days) → deleted (30 days after)`. Stripe Tax only once registrations exist. |
| 7 | identity | The portal account (with a verified email) owns the org. The tenant's first muster admin is created by the provisioner's **claim link**, shown once in the portal. Email needs a **transactional provider**: the self-hosted mail stack is inbound-only. |
| 8 | capacity | ~120 m CPU / ~300 MiB requested per tenant (measured footprint is lower). **Cap the MVP at 25 tenants.** Say plainly on the site: residential uplink, single control-plane node, best effort, no SLA. |
| 9 | upgrades | Two channels pinned in the tenants repository: `canary` (the operator's own dogfood tenant plus opt-ins), then `stable` after a soak. A pre-migration `pg_dump` per tenant gates each bump. |
| 10 | legal | ToS, privacy policy, DPA, data location, a support address and an incident process **before the first stranger pays**. |
| 11 | phases | Phase 0 tests demand by hand (no code). Phase 1 is a thin end-to-end MVP. Phase 2 hardens. The arc closes on a mechanical check (below). |

## The premise, stated so it can be attacked

This is worth building only if **all** of these hold:

1. **Someone will pay for muster without running it.** muster's pitch today is
   "self-hosted, on your own machines, over your own Tailscale network"
   (README). A hosted tenant inverts that: the operator's server sees every
   mirrored session, and, if a host runs `-interactive`, can type into it
   ([limits.md](../limits.md#mirroring): an interactive mirror "is a shell on
   that host"). The buyer must want convenience more than that property.
2. **The hard part of self-hosting is the server, not the hosts.** Hosted
   removes Docker Compose and Postgres from the customer's setup. It does not
   remove `muster-mirror` and tmux on every machine. If customers bounce off
   the mirror install, hosting does not help them.
3. **A single operator can carry strangers' uptime and data** on a
   residential, single-control-plane cluster, at a price that pays for the
   support time.
4. **Without cluster agents it is still worth paying for.** Recommendation 3
   removes dispatch, the chief and officers, which are muster's most
   differentiated features.

**The cheapest test of demand (phase 0, no code):** add a short "Hosted
(early access)" paragraph to the README and the site, linking a form (org name,
email, team size, "would you pay $X/month"), and provision the first 3–5
yeses **by hand** from the same tenant overlay the automation will later
write, billed by a Stripe Payment Link. The arc continues only if at least
three organizations that are not the operator's pay for a full month. That
also produces the overlay, the backup job and the runbook the automation then
just repeats.

## Part 1: what exists today

### How muster is deployed and reached

- **One server, one replica, one Postgres.** muster-server is one Go binary;
  some state lives only in memory, so it must run one replica
  ([limits.md](../limits.md#deployment)). A tenant is therefore one pod plus
  its database, and an upgrade is a short reconnect for its mirrors.
- **The repository ships no Kubernetes manifests** (limits.md), only
  `Dockerfile` and the Compose file. The operator's own deployment is
  hand-written manifests in their infra repository: a muster Deployment, a
  GoTrue Deployment, a Postgres Deployment on a 5 GiB PVC, a nightly
  `pg_dump`-to-S3 CronJob, RBAC for dispatch, and a NodePort. That set is the
  starting point for a tenant overlay.
- **gotrue mode is what a public deployment uses** ([auth.md](../auth.md)): its
  own GoTrue, two listeners (machine tokens on the main port; browser sessions
  on `MUSTER_BROWSER_PORT`), and `MUSTER_PUBLIC_URL`. The operator's GoTrue runs
  with signup disabled and `GOTRUE_MAILER_AUTOCONFIRM=true`: it sends no mail.
- **The public path** is Cloudflare (proxied, origin-pull TLS) → a Traefik
  IngressRoute on an edge VPS → a mesh relay → the cluster. The operator's own
  muster uses this path with per-path rules on the edge (machine-only paths
  such as `/mcp`, `/agent/`, `/connect/` denied at the edge; the sign-in paths
  rate-limited). Each exposed service today is a hand-added relay port and
  IngressRoute.
- **Hosts dial out.** `muster-mirror` opens one outbound WebSocket to
  `<url>/mirror/connect` (multi-tenant.md, part 1), so a hosted tenant needs no
  inbound path to the customer's machines, only a public `wss://` origin.

### What hosted inherits from multi-tenant.md

Each client already holds several servers apart:

- **CLI and mirror** (phases 1 and 2, built): a tenant is an env file; the
  mirror runs `muster-mirror@<tenant>` per server, routed by tmux server.
- **Android** (muster-android 0.2.0, `e25e507`, multi-tenant #18): "Add
  server" by `https` origin, then sign-in in that tenant's WebView, then
  pairing from Account → Devices.
- **Measured: the app handles no links today.** Its manifest declares one
  `MAIN`/`LAUNCHER` intent filter and no `VIEW` filter, and muster serves
  `assetlinks.json` with `get_login_creds` only, "not `handle_all_urls`, since
  the app handles no links" ([auth.md](../auth.md#passkeys-in-the-android-app)).
  So a QR code or deep link that pre-fills Add server is an **app change**
  (section 1.6).

### The cluster, measured read-only

| measure | value |
|---|---|
| nodes | 6: four on the LAN (one control-plane with the GPU, three workers) and two cloud **CI-burst** nodes with public addresses, tainted `ci-burst` |
| LAN allocatable | 47.8 CPU, 138 GiB memory |
| LAN **requested** | 40.7 CPU (85%), 91.2 GiB (66%); two workers at 97–99% CPU and 91–92% memory requested |
| LAN **used** (`kubectl top`) | 17.6 CPU (37%), 35.6 GiB (26%) |
| request headroom | ~7.1 CPU and ~46.9 GiB, most of it (~5.1 CPU, ~33 GiB) on the control-plane node |
| pods / namespaces | 545 / 59 |
| RuntimeClasses | `nvidia` only: **no gVisor, no Kata** |
| network plugin | Cilium 1.17 with the L7 proxy on, so `CiliumNetworkPolicy` `toFQDNs` is available |
| admission | Kyverno installed with **no ClusterPolicy**; no ValidatingAdmissionPolicy |
| Pod Security labels | `privileged` on system namespaces, `baseline` on one; nothing `restricted` |
| TLS | cert-manager with a Let's Encrypt DNS-01 ClusterIssuer |
| storage | all **node-local**: `local-path` (the OS disk), OpenEBS hostpath classes on HDDs and per-node NVMe. Nothing replicated: a dead node's volumes are gone until restored from backup |
| Postgres | one instance per app, no shared server, no operator; 2 of 12 instances had a backup CronJob as of the last count |

The operator's own muster (on another cluster), **measured the same day** as
the per-tenant footprint: muster 7 m CPU / 43 MiB, GoTrue 1 m / 12 MiB,
Postgres 11 m / 187 MiB, a 49 MB database after six weeks of daily use. Its
manifests request 120 m CPU and 288 MiB across the three. Its three dispatched
agent pods used 195, 238 and 409 MiB each while idle.

## Part 2: the design

### 1. Components and where each lives

**Recommendation: one new private repository, `muster-hosted`, one Go binary,
one Postgres.** It holds the website, the portal, the Stripe webhook, the
control plane and the MCP server. Reasons:

- They share one database (orgs, tenants, subscriptions, API keys, the event
  log) and one state machine; splitting them buys network hops and nothing
  else at this size.
- The code encodes the operator's infrastructure (the tenants repository, the
  object store, the edge) and holds billing keys. None of it belongs in an
  open-source repository whose leak gate exists to keep exactly that out.
- muster stays one product with one install story; hosting is a deployment of
  it, not a fork.

What **does** go into muster, because self-hosters need it too:

| change in muster | why |
|---|---|
| a README section "Hosted alternative" and a user-facing `docs/hosted.md` | the operator's ask; content in 1.5 |
| `MUSTER_DIAL_DENY_PRIVATE=1`: the postgres connector, and any other admin-configured outbound dial, refuses private, loopback, link-local and CGNAT addresses | **measured gap:** "muster-server dials whatever host it names, with no private-address rule" ([connectors.md](../connectors.md#postgres-connectors)). On a hosted tenant the admin is a stranger, so this is SSRF into the operator's network. The NetworkPolicy (section 3) is the real wall; this is the second one |
| a boot check refusing `MUSTER_AGENT_PROVISIONER` other than `none` when a `MUSTER_HOSTED=1` profile is set (and refusing the chief's Compose runtime with it) | makes "no cluster agents" a refusal at boot, not a convention |
| `muster-server invite --claim-admin` usable from a one-shot Job (it may already be; **UNVERIFIED**) | the admin handover (section 7) |
| Android: a verified App Link for Add server (1.6) | in muster-android, not here |

#### 1.1 The marketing website

Static pages served by `muster-hosted` at `https://<product-domain>`: what
hosted is, what it is not (no cluster agents; the operator can see your
sessions), price, the reliability statement (section 8), the legal pages, a
status link, and "self-host instead" linking the README. No JavaScript
framework; the existing screenshots from `docs/img/`.

#### 1.2 The portal

`https://app.<product-domain>`:

- **Sign up / sign in**: email and password with **verified email** before
  checkout, passkeys later. A separate identity store from every tenant's
  GoTrue: the portal account owns billing; muster users live in the tenant.
- **Create org**: a slug `[a-z][a-z0-9-]{2,30}`, checked against a reserved
  list (`app`, `www`, `api`, `mcp`, `status`, `admin`, `mail`, `docs`, `auth`,
  `login`, …) and a confusables check, then **Stripe Checkout**.
- **Tenant page**: state (provisioning / active / past due / suspended /
  deleting), the origin, the muster version and channel, the last backup's
  time and size, the claim link while unclaimed, the QR code for the app, and
  the mirror setup commands for this origin.
- **Billing**: a button to the Stripe Customer Portal (payment method, plan,
  invoices, cancel).
- **Export**: request a download of the latest dump plus attachments
  (section 4.4).
- **Delete**: schedule deletion (immediate read-only; data gone after the
  grace period; cancellable until then).
- **API keys** for the MCP server (1.4).

#### 1.3 The provisioning control plane

A loop inside `muster-hosted`, driven by rows, not requests. Each tenant row
has a desired state (from billing and the user) and an observed state (from
the cluster, read-only). The loop renders the tenant's overlay and commits it
to the tenants repository (section 2); it never writes to the cluster
directly. It observes by reading the tenant's `/health` and `/readyz` and the
Flux Kustomization's status with a read-only kubeconfig.

#### 1.4 The MCP server

`https://mcp.<product-domain>`, streamable HTTP, served by the same binary.

| tool | does | notes |
|---|---|---|
| `org_get` | the caller's org, plan, billing state | read |
| `tenant_create` | start provisioning | needs an active subscription; otherwise returns a Checkout URL **for the human to open**; an agent never completes a payment |
| `tenant_status` | state, origin, version, last backup, unclaimed claim link (for the org owner's key only) | read |
| `tenant_setup_mirror` | the exact host commands for this origin, and how to mint a mirror token in the tenant | read; never returns a mirror token itself: those are minted in the tenant by its admin |
| `tenant_rotate` | rotate a platform secret (`hook`, `service`, `vapid`, `upload_signing`), or reissue an unclaimed claim link | each rotation is a commit and a restart; returns when the tenant is ready again |
| `tenant_export` | queue an export; returns a time-limited download link | |
| `tenant_delete` | **schedule** deletion | two-step: the first call returns a confirmation code naming the org; the second call must echo it. The tenant goes read-only at once and is purged after the grace period, cancellable from the portal |
| `tenant_delete_cancel` | cancel a scheduled deletion | |

**Auth: API keys first, OAuth second.** MVP: portal-minted keys,
`mhk_<random>`, stored as sha256, shown once, scoped `read` or `manage`, per
org, revocable, sent as `Authorization: Bearer`. It mirrors muster's own
personal-token model ([auth.md](../auth.md#machine-credentials)) and works in
every MCP client today. Phase 2: OAuth 2.1 with the portal as the
authorization server, as the MCP authorization spec describes, for clients that
add remote MCP servers by URL and expect a browser consent. **UNVERIFIED:**
which clients the operator's users actually run; measure before building OAuth.

Nothing an agent can call through this server makes money move or deletes
data without a second, deliberate step.

#### 1.5 The README "hosted alternative" section

A short section after Quickstart, written once the MVP is live:

- one sentence: hosted muster runs the server for you at
  `https://<org>.<tenant-domain>`; you still install `muster-mirror` and tmux on
  each machine;
- 🔴 the trade, plainly: on hosted, sessions are read by a server the operator
  runs, not on your tailnet; an `-interactive` mirror lets that server type into
  your machine; dispatched agents, the chief and officers are not available;
- the reliability statement (section 8) in one line;
- a link to the site, and "self-hosting stays free and fully featured".

The README's first claim ("Self-hosted, on your own machines, over your own
Tailscale network") stays true of muster; the section must not soften it.

#### 1.6 Getting a new tenant into the app

Today, by hand: open the app, Add server, type `https://<org>.<tenant-domain>`,
sign in, pair from Account → Devices. Works with 0.2.0 unchanged.

**Recommendation: a QR code on the tenant page encoding a verified App Link,**
`https://app.<product-domain>/a/add?origin=https%3A%2F%2F<org>.<tenant-domain>`.

- The app adds a `VIEW` intent filter with `autoVerify` for that host and path
  only, and the portal serves its `assetlinks.json` with `handle_all_urls` for
  the app's signing certificate. A tenant origin's own `assetlinks.json` stays
  `get_login_creds` only.
- The link **pre-fills** Add server; the user still confirms, signs in and
  pairs. No credential is ever in the link.
- The app accepts only an `https` origin, and shows it in full before adding.
  Restricting it to `*.<tenant-domain>` is tempting but would make the feature
  hosted-only; showing the origin and requiring confirmation is the guard.
- Without the app installed, the link opens the portal page with instructions
  and a Play Store link.

A custom scheme (`muster://add?origin=…`) is simpler but any app can claim it;
the App Link is the one that cannot be hijacked.

### 2. Provisioning mechanism

| | GitOps, a commit per tenant | `MusterTenant` CRD + controller | muster-native operator |
|---|---|---|---|
| fits "commit = live deploy" | yes, it *is* that model | no: state in etcd, Git no longer the record | no |
| audit trail | every change is a signed, reviewable commit; `git log -- tenants/<org>` is the tenant's history | Kubernetes events and the controller's log; needs its own audit table | muster's audit table, mixed with product data |
| new code | renderer + git push + a Flux status reader (small) | a controller, a CRD, its RBAC, upgrades of both (large) | muster-server would need cluster-admin-like rights to create namespaces, databases and buckets for *other* deployments, which inverts its trust model |
| time to provision | Flux webhook receiver + reconcile ≈ 1–2 min (**UNVERIFIED** on this cluster); poll fallback = the interval | seconds | seconds |
| teardown | delete the directory; Flux prunes | delete the object; finalizers | — |
| failure mode | a bad render fails one Kustomization; others keep reconciling | a controller bug touches every tenant at once | — |
| disaster recovery | the tenants repository *is* the inventory: re-bootstrap Flux, restore dumps | needs an etcd backup or a re-export | — |

**Recommendation: GitOps, into a dedicated repository.**

- **Not the operator's infra repository.** A push credential to that
  repository can deploy anything anywhere in the operator's clusters. The
  control plane gets a deploy key to `muster-hosted-tenants` only.
- **Flux reconciles it under a confined identity.** A `GitRepository` and one
  `Kustomization` per tenant (or one with per-tenant paths) with
  `serviceAccountName` set to a ServiceAccount whose RBAC is limited to
  namespaces carrying the hosted label, plus `targetNamespace`. A Kyverno
  ClusterPolicy (Kyverno is installed; it has no policies yet) refuses, from
  that ServiceAccount, any object outside `mh-*` namespaces, any
  cluster-scoped object other than the namespace itself, any image not on the
  allow-list, and any pod that fails the tenant pod rules (section 3).
- **The render is a kustomize overlay over a versioned base**
  (`base/tenant@vN`): namespace, quota, limits, NetworkPolicies, Postgres
  StatefulSet, GoTrue, muster, Service, the backup CronJob, the restore-drill
  CronJob, and a SOPS-encrypted Secret. The overlay sets the slug, the channel
  and the secrets, nothing else. The same base is what phase 0 uses by hand.
- **Secrets**: the control plane generates them and encrypts them with the
  cluster's **age public key** before committing; it never holds the private
  key, so a stolen control plane cannot decrypt any tenant's secrets from Git.
- **Edge routing is static, not per tenant**: one wildcard IngressRoute on the
  edge for `HostRegexp` `*.<tenant-domain>` to one relay port, and one
  wildcard route in the cluster's Traefik to a small Host-based router (or one
  IngressRoute per tenant inside its overlay). Adding a tenant touches no edge
  file.
- **Concurrency**: commits are serialised by the control plane (one writer,
  rebase-and-retry on a rejected push).
- **When to switch**: a CRD controller becomes worth it if provisioning time is
  a measured conversion problem, or past ~100 tenants when one repository of
  overlays gets unwieldy. The renderer is reusable either way.

### 3. Tenant isolation on a shared personal cluster (the critical section)

🔴 **The risk.** A muster deployment can dispatch agents that run arbitrary
code: `claude-code` agents run a model with shell access, against any
repository, with egress to the public internet
([limits.md](../limits.md#agents): "the agent can still resolve names and send
anything it holds to any public HTTPS host"). On hosted, the person choosing
that code is a stranger who paid by card. The pods would run on the operator's
personal cluster, beside the operator's object store, password manager, mail
inbox, databases and cluster credentials, on nodes on the operator's home LAN.

Two facts measured above make the existing agent controls weaker here than on
a self-hoster's cluster:

- muster's agent NetworkPolicy allows TCP 443 to **public** IPv4 addresses and
  relies on everything else being private (limits.md). This cluster has two
  nodes with **public** addresses, and its edge VPS is public too, so "public
  443" includes the operator's own infrastructure.
- There is no sandbox runtime (no gVisor or Kata RuntimeClass), and the
  workers are already at 97–99% of requested CPU.

The options:

| control | what it buys | what it costs | verdict |
|---|---|---|---|
| **no cluster agents for hosted tenants** (host mirrors and BYO only) | removes arbitrary code from the cluster entirely; what remains is muster-server, GoTrue and Postgres, code the operator ships | the most differentiated features: dispatch, the chief, officers, worker pools, jobs | **default** |
| gVisor (`runsc`) RuntimeClass | a user-space kernel between the agent and the node; blocks most kernel exploits | an OS-level runtime install on the nodes and a reboot; syscall overhead; some tools (git, Node) slower or broken (**UNVERIFIED** for the claude-code image) | phase 3, only if agents are offered |
| Kata Containers | a micro-VM per pod | nested virtualisation or bare-metal KVM on the nodes (**UNVERIFIED**), more memory per pod | not at this scale |
| a dedicated node | a kernel escape lands on a node with nothing else on it | a node; at home it is still on the LAN; in the cloud it is off the LAN but in the same cluster and API | **yes** for hosted pods: a tainted node pool, preferably a cloud node like the CI-burst ones, so hosted traffic and a compromise both stay off the home network |
| Cilium egress allowlist by name (`toFQDNs`) | closes "any public 443": only the hosts the tenant needs | the list must be kept; DNS proxy in the path | **yes**, for every tenant pod |
| namespace per tenant + ResourceQuota + LimitRange | blast radius and noisy-neighbour bounds | none | **yes** |
| Pod Security `restricted` | no root, no host paths, no privilege, seccomp | Postgres image must run non-root (the official image can) | **yes** |
| no ServiceAccount token (`automountServiceAccountToken: false`) | a compromised pod cannot talk to the API server | muster-server then cannot dispatch, which the default already rules out | **yes** |
| image allow-list (Kyverno) | only digests the operator built or pinned can run in `mh-*` | a policy to keep | **yes** |

**Recommendation (the default):**

- `MUSTER_AGENT_PROVISIONER=none` on every hosted tenant, enforced by the
  `MUSTER_HOSTED=1` boot check (section 1). No chief (it is an agent), no
  officers, no worker pools, no jobs.
- One namespace per tenant, `mh-<org>`, with Pod Security `restricted`
  enforced, a ResourceQuota (e.g. 1.5 CPU, 1.5 GiB, 3 PVCs, 10 GiB storage,
  10 pods) and a LimitRange.
- Every pod: no ServiceAccount token, non-root, read-only root filesystem
  where the image allows, scheduled only on the hosted node pool by taint and
  affinity.
- NetworkPolicy, default deny both ways, then:
  - ingress to muster only from the cluster's Traefik; to GoTrue only from
    muster; to Postgres only from muster, GoTrue and the backup job;
  - egress from muster: DNS, its own Postgres and GoTrue, the hosted object
    store, and by name the hosts features need (OpenRouter, web-push services,
    the operator's push relay if offered, GitHub's API if the GitHub connector
    is offered, the configured speech-to-text);
  - egress to every private, link-local and CGNAT range and to the operator's
    public node and edge addresses **denied** explicitly, so a later rule
    cannot reopen them.
- `MUSTER_DIAL_DENY_PRIVATE=1` (section 1) as the second wall behind that.
- Images pulled from the public `ghcr.io/zacxdev/muster` by digest, never the
  operator's LAN registry (hosted pods then need no registry credential and
  no LAN route).

**What this rules out, plainly:** dispatching agents from muster, the chief,
officers, worker pools and the jobs API, browser flows that need a pod, and
any connector to a database the customer runs on a private network. A hosted
customer who wants agents runs them on their own machines in tmux, where
muster mirrors them like any other session: that is muster's core path, and it
puts the arbitrary code on the customer's hardware.

**What it does not remove:** muster-server itself is internet-facing code with
a stranger as its admin. A bug in it is now a bug on the operator's cluster.
The quota, the egress policy, the tainted node and the absent API token are
what bound that.

**Phase 3 (only if demand is measured):** agents for hosted tenants on a
dedicated cloud node, under gVisor, egress allowlisted by name with no
public-443 rule (the officer instance's model in
[limits.md](../limits.md#agents)), BYO Claude token only, a per-tenant cap on
concurrent agents. Its own design page first.

### 4. Data

#### 4.1 Postgres: one per tenant

| | one Postgres per tenant | shared server, database per tenant | shared database, rows by tenant (RLS) |
|---|---|---|---|
| muster changes | none | none | **large**: muster's RLS is by `owner_id` *within* a deployment ([auth.md](../auth.md#users-roles-and-invites)); a tenant column would touch every table and query |
| blast radius | one tenant | every tenant on a bad upgrade, a full disk, a crash | every tenant, and one RLS bug leaks across orgs |
| export / delete | `pg_dump` / drop the PVC | `pg_dump -d` / `DROP DATABASE` | a per-tenant export query per table |
| memory | ~190 MiB measured each | one ~1 GiB server for dozens | least |
| Postgres version per tenant | yes (canary) | no | no |

**Recommendation: one per tenant**, a StatefulSet on an NVMe class (not
`local-path`, which is the OS disk), GoTrue's `auth` schema in the same
database as today. At the 25-tenant cap that is ~5 GiB of requests, inside the
measured headroom. A shared server with a database per tenant is the fallback
if memory, not CPU, ever binds.

#### 4.2 Backups that ship with each tenant

- **In the base, not an add-on.** Every tenant gets its backup CronJob in the
  same overlay as its database. A tenant without a backup cannot exist, which
  closes the "2 of 12" pattern for this product by construction.
- Nightly `pg_dump -Fc`, encrypted with the tenant's backup key (age), to
  `s3://hosted-backups/<org>/db/<date>.dump`; 14 daily plus 8 weekly kept;
  object lock if the store supports it (**UNVERIFIED** on the operator's object
  store version).
- **A dedicated hosted object-store tenant**, on its own disk if one is free,
  with its own root credential. The control plane gets only a credential that
  can create per-tenant users and buckets under the hosted prefix. Reason: on
  this cluster the primary object store's credential is shared by unrelated
  services (the operator's ops notes), and a hosted control plane must never
  hold a key that reaches the operator's own buckets.
- **Off-site copy.** All storage is node-local on one site; a second copy of
  each night's dumps to a cloud bucket (**UNVERIFIED** provider and cost,
  likely cents per tenant per month) is what makes "your data survives a house
  fire" true. Without it the site must not claim it.
- **Restore drills, automated**: a weekly CronJob per tenant (or one that
  rotates through tenants) restores the latest dump into a throwaway Postgres,
  runs `muster-migrate` against it, and checks row counts against the dump's
  table of contents. A failed drill pages the operator. The tenant page shows
  "last verified restore". A backup nobody restored is a claim, not a backup.

#### 4.3 File storage

Attachments ([uploads.md](../uploads.md)) are optional in muster. Hosted turns
them on: one bucket per tenant (`mh-<org>-uploads`) on the hosted object
store, an access key scoped to that bucket with exactly the permissions
`MUSTER_S3_ACCESS_KEY` lists ([configuration.md](../configuration.md)), a
per-tenant `MUSTER_UPLOAD_SIGNING_KEY`, and a bucket quota matching the plan.
muster already serves downloads itself with a hardened content type, CSP and
`nosniff`, and never hands out presigned URLs (uploads.md), so no bucket is
public.

#### 4.4 Export and deletion

- **Export** at any time: the latest dump (or a fresh one) plus a tarball of
  the uploads bucket, as a link valid 24 h. Self-hosters restore it with the
  Compose install, which is the honest exit ("leave for self-hosting").
- **On cancel**: the subscription runs to the end of the period, then the
  tenant is **suspended** (section 6), then deleted 30 days later. Export works
  throughout.
- **Lapsed payment**: `past_due` 7 days, `suspended` 14 days, then scheduled
  deletion with 30 days of retention, emailed at each step.
- **Deletion** is a commit removing the overlay (Flux prunes the namespace;
  the PVC's reclaim policy is `Delete` for hosted classes, or the control plane
  deletes the retained volume), then the control plane deletes the buckets and
  users. Backups age out under the retention above and are deleted at most 30
  days after that; the privacy policy states the total. The tenant row keeps a
  tombstone (slug, dates, Stripe ids) so the slug is not reissued for 90 days.

### 5. Credentials

**Recommendation: bring your own model keys. No metered resale.**

- **Cost and abuse.** muster's model use is open-ended (focus every 15 min per
  session by default, suggestions, the chief). Reselling it means pricing
  tokens, metering them, capping them, and owning the bill when a stolen card
  runs a key hot. BYO puts the bill, the rate limits and the abuse controls on
  the provider's account, where they already exist.
- **It matches muster's own decision.** Accounts already work this way:
  "members bring their own Claude and OpenRouter credentials" stored at
  `/account/credentials` ([auth.md](../auth.md#users-roles-and-invites)); the
  operator chose BYO over caps when accounts shipped.
- **Terms.** A Claude subscription token is personal; reselling access to it,
  or pooling customers' tokens, is not something this product should do. With
  no agents in the default (section 3), the only model calls are muster-server's
  own (focus, suggestions), on the tenant's OpenRouter key.
- **Where they live.** In the tenant's own connector store
  (`MUSTER_CONNECTOR_ENCRYPTION_KEY`, one per tenant), entered by the tenant's
  users in muster. The control plane never sees them and the portal never asks.

**Platform secrets per tenant** (hook token, service token, VAPID keypair,
GoTrue JWT secret, connector key, upload signing key, Postgres password,
bucket key, backup age key) are generated by the control plane and committed
SOPS-encrypted to the tenant's overlay (section 2). SOPS over Sealed Secrets:
the operator already runs SOPS with Flux, so there is no new controller, and
encryption needs only the public key. The backup age **private** key is held
outside the cluster (the operator's password manager), so a cluster compromise
cannot read old dumps, and a restore is a deliberate act.

### 6. Billing

Following Stripe's own guidance for subscriptions:

- **Checkout Session** in `subscription` mode for signup, with
  `client_reference_id` set to the org id; dynamic payment methods (no
  `payment_method_types`). **Customer Portal** for everything after.
- **A restricted API key** (`rk_…`) with only the permissions the portal uses,
  in the cluster's secret store; a webhook signing secret per endpoint;
  separate **sandboxes** for development and CI.
- **State comes only from verified webhooks**, never from the success page:
  `checkout.session.completed`, `customer.subscription.created|updated|deleted`,
  `invoice.paid`, `invoice.payment_failed`, plus `charge.dispute.created`,
  `charge.refunded` and `radar.early_fraud_warning.created`.
- **Idempotency.** Each event's id is inserted into a `stripe_events` table
  under a unique constraint before it is acted on; a duplicate is
  acknowledged and skipped. Events can arrive out of order, so the handler
  re-reads the Subscription from the API and maps **its** status, rather than
  trusting the event's ordering. Events are mapped to an org through the
  Subscription and Customer ids stored at checkout, not through metadata.
  Outgoing Stripe calls carry an `Idempotency-Key` derived from the action and
  the org.

**The tenant state machine:**

| state | entered on | the tenant | leaves |
|---|---|---|---|
| `provisioning` | subscription `active` or `trialing` | being created | → `active` when `/readyz` is 200 |
| `active` | provisioned and paid | normal | → `past_due` |
| `past_due` | subscription `past_due` (`invoice.payment_failed`) | **normal**, banner in the portal, email; Stripe's retries run | → `active` on `invoice.paid`; → `suspended` after 7 days |
| `suspended` | 7 days past due, subscription `unpaid`/`canceled`, a dispute, or an early-fraud warning | **read-only** (below) | → `active` on payment; → `deleting` after 14 days |
| `deleting` | 14 days suspended, or the owner's delete | read-only; export still works | → purged after 30 days; cancellable until then |

**What suspension does to a running tenant:** muster has no read-only mode
today. Options, cheapest first: (a) scale muster to 0 and serve a static
"suspended, export from the portal" page from the edge (simple, but mirrors
disconnect and the app shows an error); (b) a `MUSTER_READ_ONLY=1` switch in
muster that refuses every write and every send to a host while still serving
pages (a muster change, and a useful one for self-hosters during a migration).
**Recommendation: (a) for the MVP, (b) in phase 2.** Data is never touched
by suspension.

**Tax.** Selling to other countries makes the operator liable for VAT/GST
from the first sale in many of them. Stripe Tax calculates it, but only where a
**registration** exists: turning on `automatic_tax` with none collects nothing
while looking on. So: register where required first (the operator's home
jurisdiction at least), then turn on `automatic_tax`; or sell only where the
operator is registered at launch. Stripe's fees (card processing, Billing,
Tax) are percentages of revenue; current rates are **UNVERIFIED** here.

### 7. Auth and identity

- **The portal account** is a person with a verified email who owns the org
  and its billing. It is not a muster user.
- **The tenant's first admin.** After the first `/readyz` 200, the control
  plane runs a one-shot Job in the tenant namespace with the muster image:
  `muster-server invite --claim-admin`, which prints "a single-use link, valid
  for 24 hours. Opening it binds a new GoTrue login to user 1"
  ([auth.md](../auth.md#setting-up-the-first-admin)). The portal stores the
  link encrypted and shows it **once** to the owner, with "issue a new link"
  while user 1 is unclaimed (`tenant_rotate` too). After that the tenant's admin
  invites members in muster as today; the portal has no role in it. Tenant
  GoTrue keeps signup disabled.
- **Two passwords, on purpose.** The portal and the tenant are separate
  identities. SSO from portal to tenant is phase 3 at the earliest; it would
  make the portal a key to every tenant.
- **The operator's access to a tenant**: none by default. muster has no
  break-glass role; the operator can reach the database through the cluster.
  The privacy policy must say so, and every operator access to a tenant's data
  is logged in the control plane with a reason.

**Email: the self-hosted mail stack cannot send it.** From the operator's
own runbook (not re-probed for this page): the stack is **inbound** (MX to an
edge relay to a receiver that stores into Postgres); the edge VPS's provider
blocks outbound port 25 for its account, and so does the home ISP, so its relay
cannot deliver to arbitrary recipients; the only outbound path in use is the
operator's personal mailbox over its provider's submission port, which must
not be a product's sender. **Recommendation: a transactional email provider**
(Postmark, Amazon SES or Resend; a few dollars a month at this volume,
**UNVERIFIED**), on a `mail.<product-domain>` sending subdomain with SPF, DKIM
and DMARC, used by the portal and, through GoTrue's SMTP settings, by each
tenant's GoTrue for password resets (today's GoTrue sends nothing:
`GOTRUE_MAILER_AUTOCONFIRM=true`).

**Abuse and signup fraud:**

- no tenant before a successful payment, so a fake signup costs a card
  authorisation, not a deployment;
- a captcha (Cloudflare Turnstile) on signup, a rate limit per address and per
  email domain;
- Stripe Radar on by default, and a dispute or early-fraud warning suspends
  the tenant (section 6);
- with no agents, a hosted tenant cannot mine, scan or send mail from the
  cluster: the egress allowlist is the backstop for what muster-server itself
  could be made to do;
- an acceptable-use policy, and a kill switch per tenant in the control plane.

### 8. Capacity and reliability

**Per-tenant cost.** Requests: muster 50 m / 96 MiB, GoTrue 20 m / 64 MiB,
Postgres 50 m / 128 MiB, so **~120 m CPU and ~290 MiB**, as the operator's
manifests set it, against a measured use well under that (part 1). muster
serves `GET /metrics` (`internal/api/server.go:583`). Cookies muster sets
carry no `Domain` attribute, so they are host-only.
Limits roughly 1 CPU / 512 MiB for muster and Postgres each. Storage: 5 GiB
for Postgres (a heavy single-operator database is 49 MB after six weeks) plus
the uploads bucket quota.

**How many.** By requests, today's LAN headroom (~7.1 CPU, ~46.9 GiB) fits
~58 tenants by CPU and ~160 by memory, but most of it is on the control-plane
node, and the workers are full. **Recommendation: cap the MVP at 25 tenants**
(~3 CPU, ~7.3 GiB), enforced by the control plane, and put hosted pods on
their own node pool rather than in the leftover headroom. A dedicated cloud
node of the CI-burst size (8 CPU, 32 GiB allocatable, measured on the two that
exist) would hold the cap several times over at a monthly cost the
subscription revenue must cover (**UNVERIFIED** price).

**Reliability, stated for the website and the terms:**

> Hosted muster runs on a small cluster operated by one person. The control
> plane is a single node on a residential internet connection, reached through
> a cloud relay. Expect outages of minutes to hours when the power, the
> connection or a node fails, and planned maintenance with notice. There is no
> SLA. Your data is backed up nightly and restores are tested weekly; a backup
> can be up to 24 hours old. If that is not enough, self-host muster: it is
> the same software.

That is the honest statement. "99.9%" is not available from this setup and
must not appear anywhere.

**Status page.** An external probe (a free uptime monitor outside the
operator's network, or a small job on the edge VPS) hitting each tenant's
`/health` and the portal, published at `status.<product-domain>` hosted
**off** the cluster, so it is up when the cluster is down.

**Monitoring and alerting per tenant**, with the operator's existing
Prometheus: muster serves `/metrics` (the operator's edge denies it publicly,
which is right); a PodMonitor per tenant from the base; alerts for `/readyz`
down > 5 min, a backup older than 26 h, a failed restore drill, a PVC > 80%,
a pod restarting, and a tenant at its quota. One alert route to the operator's
phone. Tenants are labelled by org so one bad tenant is one alert, not 25.

### 9. Upgrades

`muster-ops release` today builds the image, rewrites the operator's own pins,
and `verify` polls `/health`, the rollout and the banner before tagging
(`muster-ops --help`). For hosted:

- **Two channels**, each one pin in the tenants repository's base:
  `channels/canary` and `channels/stable`. Each tenant overlay names one. The
  operator's own dogfood tenant and opted-in customers are `canary`.
- **Release:** `muster-ops release` (unchanged) cuts the version for the
  operator's deployment; a new `hosted-release --version V --channel canary`
  bumps the canary pin. Before the bump, the control plane runs a pre-upgrade
  `pg_dump` for every tenant on the channel and refuses to proceed if any
  failed. After the commit, it waits for every canary tenant's `/health` to
  report V and `/readyz` 200, with a timeout.
- **Promotion:** after a 24 h soak with no tenant alert, the same command bumps
  `stable`, in batches of 5 with the same checks between batches, stopping on
  the first failure.
- **Migrations:** each tenant runs `muster-migrate` as an init container, as the
  operator's deployment does. A migration that fails leaves that one tenant on
  the old pod (the rollout's `maxUnavailable: 0`) and pages. Rollback is the
  pin back plus, if the migration ran, a restore from the pre-upgrade dump;
  [schema.md](../schema.md) is the authority on which migrations are
  reversible.
- **Version skew:** tenants on a channel move together; across channels they
  differ by at most one release window. Clients already tolerate skew: the CLI
  prints a skew note and never fails on it (multi-tenant.md), and a mirror
  talks protocol versions in its `welcome`. A customer on stable whose mirror
  binary is newer is the common case and must keep working; the release check
  should include "the previous mirror release still connects".

### 10. Legal and operational prerequisites

Before the first stranger pays (phase 0 included):

- **Terms of service**: no SLA, acceptable use, suspension and deletion
  timelines, the operator's right to suspend for abuse, liability capped at
  fees paid. **UNVERIFIED**: whether the operator sells as an individual or a
  company; a company is strongly advisable before taking strangers' data.
- **Privacy policy**: what the server stores (task text, session focus, ask
  log, transcript-derived summaries, attachments), that the operator can
  technically read it, sub-processors (Cloudflare, Stripe, the email provider,
  the off-site backup store, OpenRouter on the tenant's own key), retention.
- **DPA** for business customers, with the sub-processor list; GDPR applies as
  soon as an EU org signs up.
- **Data location**: the cluster's country, the edge VPS's country, the
  off-site backup's region, and Cloudflare's global network, stated.
- **Support**: one address (`support@<product-domain>`) and a stated response
  time ("best effort, usually within two working days").
- **Incident process**: the status page, an email to affected owners for any
  outage over an hour and for any suspected data exposure, and a breach
  notification timeline matching GDPR's 72 hours.

### 11. Domain, TLS and DNS

| option | hostnames | TLS on Cloudflare | cost | verdict |
|---|---|---|---|---|
| **a new product domain** | `<product-domain>`, `app.`, `mcp.`, `status.`; tenants `<org>.<tenant-domain>` | the free Universal certificate covers the apex and **one** wildcard level, so `<org>.<domain>` is covered free | one registration, about $10–15 a year for a `.com`/`.dev` at cost (**UNVERIFIED**) | **recommended** |
| under the personal domain, `<org>.<personal-domain>` | tenants share the namespace with the operator's own services | free | $0 | 🔴 **no** (below) |
| under the personal domain, `<org>.muster.<personal-domain>` | a second level | the free certificate does not cover a second wildcard level; it needs a paid edge certificate (Advanced Certificate Manager, about $10 a month, **UNVERIFIED**) or Total TLS | ~$120 a year | no (below) |

🔴 **Why not the personal domain, measured:** the operator's single sign-on
session cookie (Authelia's) is scoped to the personal domain itself, so it is
sent to every host under it. Tenant servers there would receive the
operator's SSO cookie on every visit from the operator's browser, and any
page a tenant could make serve script could set cookies for the parent domain.
Strangers' deployments must not live under the domain that guards the
operator's own services.

**Recommendation:** one new domain for the MVP; tenants at
`<org>.<tenant-domain>` where `<tenant-domain>` is that same domain, portal at
`app.`, the reserved-name list in 1.2. In phase 2, move tenants to a **second**
registrable domain (as `github.io` is to `github.com`) and request its entry
on the Public Suffix List, so a tenant origin can never set a cookie the
portal reads. Cookies muster sets are host-only today, which keeps tenants from
reading each other's.

**DNS and TLS:** Cloudflare, as today: a proxied wildcard `*` record and the
apex to the edge; Cloudflare's edge certificate to visitors; an Origin CA
wildcard certificate (free, long-lived) plus authenticated origin pulls
between Cloudflare and the edge, as the operator's existing muster route does.
No per-tenant DNS record or certificate, so provisioning touches neither.
WebAuthn's RP id is each tenant's own host (`GOTRUE_WEBAUTHN_RP_ID`), and the
Android origin hash goes in each tenant's `GOTRUE_WEBAUTHN_RP_ORIGINS` from
the base.

**UNVERIFIED:** Cloudflare's limits on long-lived WebSockets (every mirror
holds one) and on the free plan's request rate for this traffic shape; the
operator's own muster already runs its mirrors over this path, which is
evidence for one tenant, not for 25.

## Part 3: phases

### Phase 0: demand, by hand (no code)

- The legal pages (section 10) and the reliability statement, published.
- A README paragraph and a one-page site with the form.
- `muster-hosted-tenants` repository with `base/tenant@v1` and the Flux
  wiring under the confined ServiceAccount and the Kyverno policy.
- The first 3–5 tenants provisioned by hand from the base, billed by a Stripe
  Payment Link.

**Acceptance:** a tenant created from the base by hand serves `/readyz` 200 at
`https://<org>.<tenant-domain>`; a host's `muster-mirror` connects to it; the
Android app 0.2.0 adds it and pairs; its first backup is in the bucket; a
restore drill of that backup passes; from a pod in a tenant namespace a TCP
connect to a LAN address, to the cluster API and to a non-allowlisted public
host each fails, while the same probe to an allowlisted host succeeds (the
positive control); the namespace rejects a privileged pod. **Gate to phase 1:**
three paying organizations other than the operator's after one month.

### Phase 1: the thin self-serve MVP

- `muster-hosted`: signup with email verification, org, Checkout, the webhook
  state machine, the control plane writing overlays, the tenant page with the
  claim link and setup commands, the Customer Portal link, delete (scheduled),
  export (dump only), the 25-tenant cap.
- The MCP server with API keys and `org_get`, `tenant_create`,
  `tenant_status`, `tenant_setup_mirror`, `tenant_delete`,
  `tenant_delete_cancel`.
- muster: `MUSTER_HOSTED=1` boot refusals, `MUSTER_DIAL_DENY_PRIVATE=1`, the
  README section, `docs/hosted.md`.
- Suspension by scale-to-zero; transactional email; the status page;
  per-tenant alerts; canary and stable channels.

**Acceptance:** an automated end-to-end run against Stripe sandboxes signs up a
new email, verifies it, pays with a test card, and within 5 minutes gets a
tenant whose `/readyz` is 200 and whose claim link creates user 1; the same run
over MCP with an API key reaches the same state; a test-clock run moves a
tenant through `past_due` → `suspended` → `deleting` → purged, and afterwards
the namespace, the buckets and the overlay are gone and the slug is
tombstoned; a replayed webhook event changes nothing; a release goes to canary
then stable with the pre-upgrade dumps present.

### Phase 2: hardening

- Tenants on a second registrable domain plus a PSL request; tenant pods on a
  dedicated (cloud) node pool; off-site backup copies; `MUSTER_READ_ONLY=1`
  suspension; export with attachments; the App Link and QR in muster-android;
  OAuth for the MCP server if clients need it; Stripe Tax once registered;
  `tenant_rotate` and `tenant_export` over MCP.

**Acceptance:** a weekly restore drill has passed for every tenant for four
consecutive weeks; a QR scan on a phone with the app pre-fills Add server with
the tenant's origin and nothing else; a suspended tenant serves its pages and
refuses a write (409) and a send; a node drain of the hosted pool reschedules
every tenant and each is back to `/readyz` 200 within 10 minutes (with
node-local volumes this needs the restore path, which is the point of the
test).

### Phase 3 (optional, its own design page): agents for hosted tenants

gVisor, a dedicated cloud node, egress by name with no public-443 rule, BYO
Claude token, per-tenant agent caps. Only if phase 1 customers ask for it in
numbers.

### Closing condition for the arc

The arc is done when **all** of these are true, each checked by a command:

1. the phase-1 end-to-end run (sandbox signup → active tenant → claim → delete
   → purged) passes in CI on `muster-hosted`'s default branch;
2. the README on muster's `main` contains the "Hosted alternative" section
   linking the live site, and the site's `/` answers 200;
3. the Stripe live-mode subscription list holds at least **three active
   subscriptions whose customers are not the operator's** (or, if the phase-0
   gate fails, the arc closes as **declined** with that count recorded, and the
   hosted paragraph is removed from the README);
4. every active tenant has a successful restore drill younger than 8 days
   (the control plane's own report, exit 0).

## Part 4: open questions for the operator

1. **Is the premise right?** Will you sell a muster with no dispatched agents,
   no chief and no officers (section 3), given those are its most
   differentiated features? If not, hosted means phase 3's sandbox work first,
   and a much larger arc.
2. **Price and plan shape.** One flat price per org, or per seat? A trial or
   not? (It sets the Checkout config and the fraud exposure.)
3. **The product domain.** Which name to register (section 11).
4. **Who sells.** An individual or a company, and in which jurisdiction; this
   decides the ToS party, tax registration and whether international sales are
   open at launch.
5. **Hardware.** Put hosted tenants on a rented cloud node joined to the
   cluster (recommended; a monthly cost), or on the LAN workers (free, but on
   the home network and they are full)?
6. **Off-site backups.** Accept a cloud bucket as a sub-processor for tenant
   dumps, or state on the site that backups are on-site only?
7. **Operator access.** Is "the operator can technically read your data, and
   every access is logged" acceptable as the privacy position, or do you want
   to invest in a stronger one (for example, no operator database access
   without the customer's approval)?
8. **The phase-0 gate.** Is "three paying orgs after one month" the bar, or
   another number?
