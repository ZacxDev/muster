# Kubernetes: the Helm chart

`charts/muster` installs muster on a Kubernetes cluster: the server, its
Postgres, and, each off until you turn it on, the agent plane (agents in their
own namespaces), the chief, officers, push, attachments, dictation and GoTrue
sign-in.

**Previews are not in the chart yet.** The preview gateway (`muster-preview`,
[previews.md](previews.md)) needs a wildcard DNS zone, a wildcard certificate
and an edge that overwrites the client-address header; the chart does not run
it. Run it yourself, set `plugins: true`, and pass the server
`MUSTER_PLUGIN_ORIGINS` through `server.extraEnv`.

It is the advanced path. The Docker Compose stack (`deploy/docker-compose.yml`,
[INSTALL.md](../INSTALL.md)) is the basic one and runs on one machine with no
cluster. Choose the chart when you want what only Kubernetes gives muster:
**dispatched agents** ([provisioner.md](provisioner.md)), each in its own
namespace behind its own NetworkPolicy, a per-user chief on Kubernetes, and pod
officers ([officers.md](officers.md)). The two read the same environment
variables ([configuration.md](configuration.md)); the chart's values map onto
them, and `server.extraEnv` takes any variable it has no value for.

## Install

The chart is published with every release as an asset,
`muster-<X.Y.Z>.tgz`, whose `version` and `appVersion` are the release's: its
default image is `ghcr.io/zacxdev/muster:<X.Y.Z>`. It is not published to an
OCI registry.

```sh
helm install muster \
  https://github.com/ZacxDev/muster/releases/download/v<X.Y.Z>/muster-<X.Y.Z>.tgz \
  --namespace muster --create-namespace -f my-values.yaml
```

From a clone, the chart directory carries placeholder versions, so give the
image tag of a release:

```sh
helm install muster ./charts/muster --namespace muster --create-namespace \
  --set image.tag=<X.Y.Z> -f my-values.yaml
```

With no values file you get the core: the server (one replica, Recreate), a
bundled Postgres with a 5 Gi volume, password sign-in, mirrored sessions, and
a ClusterIP Service. The install's notes print how to read the generated
password and how to run `muster doctor --api-url <your URL>` against it.

A minimal `my-values.yaml` for a cluster with an ingress controller and
cert-manager:

```yaml
publicUrl: https://muster.example.com
ingress:
  enabled: true
  className: nginx
  host: muster.example.com
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt
  tls:
    secretName: muster-tls
```

Every value is documented in [`charts/muster/values.yaml`](../charts/muster/values.yaml)
with the variable it sets. `values.schema.json` refuses a value the chart does
not know, so a misspelt key fails the install instead of doing nothing.

## Secrets

Every secret the chart can make, it makes **once**, on the first install, into
the Secret `<release>-muster-generated`:

- the UI password and the hook token;
- the Postgres password, and the `DATABASE_URL` built from it;
- the VAPID pair (a P-256 key; muster checks at boot that the private half
  derives the public one);
- the connector key, the upload signing key, the bundled store's secret key,
  the STT token, and the GoTrue JWT secret and its database password, when
  their feature is on.

On every later `helm upgrade` the chart reads that Secret back (`lookup`) and
keeps each value it holds, including the values of a feature you turned off,
so turning push or the connector store off and on again does not mint a new
VAPID pair (every subscription would stop) or a new connector key (every
stored credential would become unreadable). The Secret is annotated
`helm.sh/resource-policy: keep`, so `helm uninstall` leaves it, together with
the Postgres volume, and a reinstall finds both.

To read one: `kubectl -n muster get secret muster-generated -o
jsonpath='{.data.MUSTER_UI_PASSWORD}' | base64 -d`.

To rotate one, delete its key from the Secret and run `helm upgrade`; the next
render makes a new value. The server's Deployment does not roll on a Secret
change, so then `kubectl -n muster rollout restart deploy/muster`. Two keys
need more:

- **The VAPID pair:** delete BOTH `MUSTER_VAPID_PUBLIC_KEY` and
  `MUSTER_VAPID_PRIVATE_KEY`. A new pair is made only when the private key is
  missing, and a public key left behind would no longer match it. Every push
  subscription then has to be made again ([push.md](push.md)).
- **`POSTGRES_PASSWORD`:** Postgres keeps the password it was initialised
  with, so a new value in the Secret alone locks muster out. Change the role
  first, then put the SAME value in the Secret (the chart rebuilds
  `DATABASE_URL` from it), then upgrade and restart:

  ```sh
  NEW="$(openssl rand -hex 24)"
  kubectl -n muster exec muster-postgres-0 -- \
    psql -U muster -d muster -v ON_ERROR_STOP=1 -c "ALTER ROLE muster PASSWORD '$NEW'"
  kubectl -n muster patch secret muster-generated --type merge \
    -p "{\"stringData\":{\"POSTGRES_PASSWORD\":\"$NEW\"}}"
  helm upgrade muster <chart> -n muster -f my-values.yaml
  kubectl -n muster rollout restart deploy/muster
  ```

  (CI runs exactly this on kind, and checks the old password is refused.)

Each feature also takes an `existingSecret` naming a Secret of yours, holding
the keys its values comment names; the chart then generates nothing for it.

⚠ **`lookup` needs a live cluster.** `helm template`, `helm install
--dry-run` and tools that render without one (Argo CD) get a new value on
every render. With those, set `secrets.generate: false`: the chart then makes
no Secret at all, and refuses to render while a feature that is on has no
`existingSecret`, naming the value to set. With the bundled GoTrue,
`gotrue.existingSecret` must then also hold `GOTRUE_DB_PASSWORD` and
`GOTRUE_DB_DATABASE_URL`
(`postgres://muster_auth:<password>@<release>-muster-postgres:5432/muster_auth?sslmode=disable&search_path=auth`).
`charts/muster/ci/own-secrets.yaml` is a working example. Flux's
helm-controller runs Helm against the cluster, so `lookup` works there.

## Values per feature

Each row names the values, the variables they set, and the doc that is the
authority on what the feature does. A test
(`internal/modulegate/chart_env_test.go`) fails if the chart sets a variable
that [configuration.md](configuration.md) does not list for the binary that
reads it; another (`tests/chart/boot_config_check.py`) runs the real
`muster-server` on each test value set's environment and fails if it refuses
the configuration.

| feature | values | variables | doc |
|---|---|---|---|
| sign-in | `auth.mode`, `auth.existingSecret`, `secureCookies` | `MUSTER_AUTH_MODE`, `MUSTER_UI_PASSWORD`, `MUSTER_HOOK_TOKEN`, `MUSTER_SECURE_COOKIES` | [auth.md](auth.md) |
| GoTrue | `auth.mode: gotrue`, `gotrue.*` (with `gotrue.enabled: false`, `gotrue.url` and `gotrue.existingSecret` are required) | `MUSTER_GOTRUE_URL`, `MUSTER_GOTRUE_JWT_SECRET`, `MUSTER_BROWSER_PORT` | [auth.md](auth.md#gotrue) |
| client address | `clientAddress.trustedProxies`, `clientAddress.header` | `MUSTER_TRUSTED_PROXIES`, `MUSTER_CLIENT_IP_HEADER` | [configuration.md](configuration.md#client-addresses-behind-a-proxy) |
| database | `postgres.*`, `externalDatabase.*` | `DATABASE_URL` | [configuration.md](configuration.md#muster-server-core) |
| agent plane | `agents.enabled`, `agents.provisioner`, `agents.gatewayImage`, `agents.namespacePrefix`, `agents.workspace` | `MUSTER_AGENT_PROVISIONER`, `MUSTER_AGENT_IMAGE_REPO`/`_TAG`, `MUSTER_AGENT_API_URL`, `MUSTER_AGENT_NAMESPACE_PREFIX`, `MUSTER_AGENT_NETPOL_FROM_NAMESPACE`/`_FROM_POD_LABELS` | [provisioner.md](provisioner.md) |
| agent chat | `agents.gateway.*` | `MUSTER_AGENT_GATEWAY`, `MUSTER_AGENT_GATEWAY_MODEL`, `MUSTER_AGENT_RUNTIME_CONFIG`/`_INSTALL` | [provisioner.md](provisioner.md#the-two-switches) |
| Claude Code agents | `agents.claudeCode.*` | `MUSTER_AGENT_KINDS`, `MUSTER_AGENT_CC_IMAGE`, `MUSTER_AGENT_CC_ACCOUNTS`, `MUSTER_AGENT_CC_TOKEN_<NAME>` (from `tokensSecret`) | [provisioner.md](provisioner.md#agent-kinds) |
| RBAC | `agents.rbac.*` | `MUSTER_AGENT_NAMESPACE_CLUSTERROLE` (least-privilege) | [below](#the-agent-plane) |
| chief | `chief.*`, `plugins` | `MUSTER_CHIEF_DISPATCH_OWNERS`, `MUSTER_TASK_ROUTING`, `MUSTER_CHIEF_LLM`, `MUSTER_CHIEF_LLM_MODEL`, `MUSTER_PLUGINS` | [chief.md](chief.md) |
| officers | `officers.*` | `MUSTER_OFFICERS`, `MUSTER_OFFICER_TOKEN_<NAME>` (from `tokensSecret`), `MUSTER_OFFICER_EGRESS_CIDRS`, `MUSTER_OFFICER_EGRESS_DNS` | [officers.md](officers.md) |
| connector store | `connectors.*` | `MUSTER_CONNECTOR_ENCRYPTION_KEY`, `MUSTER_CONNECT_PORT`, `MUSTER_AGENT_CONNECT_URL` | [connectors.md](connectors.md) |
| GitHub App | `githubApp.*` | `MUSTER_GITHUB_APP_ID`, `MUSTER_GITHUB_APP_ACCOUNTS`, `MUSTER_GITHUB_APP_ACCESS`, `MUSTER_GITHUB_APP_PRIVATE_KEY` | [github-app.md](github-app.md) |
| OpenRouter | `openrouter.*` | `MUSTER_AGENT_OPENROUTER_KEY` | [configuration.md](configuration.md#muster-server-agent-provisioning) |
| push | `push.*` | `MUSTER_VAPID_PUBLIC_KEY`, `MUSTER_VAPID_PRIVATE_KEY`, `MUSTER_VAPID_SUBJECT` | [push.md](push.md) |
| attachments | `attachments.*`, `s3.*` | `MUSTER_S3_*`, `MUSTER_UPLOAD_SIGNING_KEY`, `MUSTER_UPLOAD_MAX_BYTES` | [uploads.md](uploads.md) |
| dictation | `dictation.*`, `stt.*` | `MUSTER_STT_URL`, `MUSTER_STT_TOKEN`, `MUSTER_STT_MODEL` | [dictation.md](dictation.md) |

`dictation` and `attachments` run the chart's bundled service (the Compose
file's `stt` and `s3` images, configured under `stt.*` and `s3.*`) unless you
give `dictation.url` or `attachments.endpoint`; there is no separate switch for
the bundles. The GPU speech-to-text service (stt-cohere) is not
bundled: run it yourself and set `dictation.url`.

The chart's own test value sets, `charts/muster/ci/*.yaml` (minimal, every
feature, the default RBAC, an external database, every Secret your own), are
working examples.

## The agent plane

`agents.enabled` with `provisioner: kubernetes` makes muster create one
namespace per agent, `<agents.namespacePrefix><agent>`, holding its pod and,
for a `claude-code` agent, a NetworkPolicy that admits only muster's own pods.
The chart sets `MUSTER_AGENT_NETPOL_FROM_NAMESPACE` to the release namespace
and `MUSTER_AGENT_NETPOL_FROM_POD_LABELS` to the server's selector labels, so
that policy names exactly this release's server.

**RBAC.** The chart binds muster's ServiceAccount in one of the two layouts
[provisioner.md](provisioner.md#kubernetes-rbac) describes:

- `agents.rbac.layout: least-privilege`, **the default**: the cluster-wide
  grant is namespaces (`get`, `list`, `create`, `delete`), RoleBinding
  `get`/`create` and `bind` on one ClusterRole; muster binds itself inside
  each agent namespace. A Role in the release namespace grants `get` on
  Secrets there, for a deploy-key pool's and an officer image's pull Secret
  (narrow it to their names with `agents.rbac.sourceSecretNames`).
- `agents.rbac.layout: cluster-wide`, opt-in: the agent-namespace ClusterRole
  bound cluster-wide, plus namespaces (`get`, `create`, `delete`). muster can
  then read Secrets and create Deployments in every namespace. Switching an
  existing install between the layouts: see provisioner.md for the order
  (agents created before get their binding at their next operation).

**The admission policy** (`agents.rbac.admissionPolicy.enabled`, **on by
default**, Kubernetes 1.30 or later) is a ValidatingAdmissionPolicy that
applies to muster's ServiceAccount only. RBAC cannot scope a grant by a
namespace's name; this policy does it for two kinds of request:

- creating or deleting a **namespace** is refused unless its name starts with
  `agents.namespacePrefix`, so muster's cluster-wide `create` and `delete` on
  namespaces cannot touch `kube-system`, your applications' namespaces or its
  own;
- writing or deleting a **RoleBinding** is refused outside a namespace whose
  name starts with the prefix, so the least-privilege layout's cluster-wide
  `create` on RoleBindings cannot bind muster into any other namespace.

What it does **not** restrict:

- any namespace whose name starts with the prefix, including one that muster
  did not create. Two installations whose prefixes overlap (`muster-agent-`
  and `muster-agent-staging-`) can each reach the other's agent namespaces;
  give each its own, non-overlapping prefix;
- reading: `get` and `list` on namespaces stay cluster-wide;
- every other request. In the cluster-wide layout, muster still reads
  Secrets and writes Deployments, Services and the rest in **every**
  namespace: the policy closes only the namespace and RoleBinding gaps, and
  only the least-privilege layout removes the others;
- in the least-privilege layout, the Role on Secrets in the release namespace
  (every Secret there unless `agents.rbac.sourceSecretNames` names them);
- requests by anyone other than muster's ServiceAccount.

Turn it off (`agents.rbac.admissionPolicy.enabled: false`) only on a cluster
older than 1.30, where the API server does not know the kind and the install
fails.

`internal/provision/k8s/chart_rbac_test.go` runs the Kubernetes driver
through an agent's whole life against exactly the RBAC the chart renders, in
both layouts, and fails on any request it refuses. The admission policy is
checked on a real API server: CI's kind job dispatches an agent with it on, and
checks it refuses muster's ServiceAccount a namespace outside the prefix.

**The CNI must enforce NetworkPolicy.** An agent's isolation is its
NetworkPolicy; on a cluster whose network plugin ignores NetworkPolicies
(some default CNIs do) the policy is written and nothing enforces it, and
muster cannot tell. Use a plugin that enforces them (Cilium, Calico, …).

**Egress for officers.** An officer instance's policy allows only muster's
connect port; it reaches `api.anthropic.com` through a cluster policy that can
name a host. On a plugin that cannot (no FQDN policies), `officers.egressCidrs`
(`MUSTER_OFFICER_EGRESS_CIDRS`) is the fallback, with the risk
[officers.md](officers.md#on-a-cluster-without-fqdn-policies-muster_officer_egress_)
states.

## Ingress

`ingress.enabled` renders an Ingress, `httpRoute.enabled` a Gateway API
HTTPRoute; both route to the server's main listener, or, with
`auth.mode: gotrue`, to the browser listener (the only one that serves browser
sessions in that mode). Terminate TLS there and keep `secureCookies` on.

muster holds connections open for as long as a page is: mirrored terminals and
agent chat are WebSockets, and the live updates are server-sent events. Your
controller must pass WebSocket upgrades, must not buffer responses, and must
not time them out:

- **ingress-nginx:** `nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"`,
  `nginx.ingress.kubernetes.io/proxy-send-timeout: "3600"` and
  `nginx.ingress.kubernetes.io/proxy-buffering: "off"`;
- **Traefik:** WebSockets and streaming work with no annotation;
- **HTTPRoute:** set `httpRoute.timeouts` to your implementation's form of
  "no request timeout" (several take `{request: "0s"}`).

muster believes no forwarding header by default: every client address is the
TCP peer, which behind an ingress is the controller's pod, so all clients share
the per-address limits. To tell them apart, set `clientAddress.trustedProxies`
to the controller's own addresses and `clientAddress.header` to the header the
controller writes (`X-Forwarded-For` for ingress-nginx and Traefik). 🔴 Do not
trust the cluster's whole pod range: the agent pods muster provisions connect
to muster-server from it, and an agent inside a trusted range could choose its
own address in every limit and audit row. Do not trust a range a client can
connect from directly either, and do not pick
`CF-Connecting-IP` unless every request reaching the controller came through
Cloudflare ([Client addresses behind a proxy](configuration.md#client-addresses-behind-a-proxy)).

The ingress exposes every route, including `/metrics`, which is
unauthenticated ([architecture.md](architecture.md#listeners)): block it at
the edge if the host is public. In gotrue mode the shared hook and service
tokens are refused on the browser listener; a machine client that uses them
must reach the main port (`service.port`), for example inside the cluster.

## Postgres

The bundled Postgres is one StatefulSet replica on one PVC: there is no
backup and no failover. For anything you keep, either back it up
([schema.md](schema.md)) or run Postgres yourself and set
`postgres.enabled: false` with `externalDatabase.existingSecret` holding
`DATABASE_URL`. Migrations run when the server starts.

Its image is `postgres:16-alpine`, a tag that moves within major version 16
only. **A major-version upgrade (`postgres.image.tag: 17-alpine`) cannot open
the existing data directory**: the pod fails to start. Dump the database
(`pg_dump`) with the old version, start the new one on an empty volume, and
restore into it.

## Upgrade and rollback

```sh
helm upgrade muster <chart> -n muster -f my-values.yaml --set image.tag=<X.Y.Z>
```

The server is replaced with Recreate, so an upgrade is a short outage; its
migrations run at the new pod's start. `helm rollback muster <revision>`
restores the previous manifests and image, but not the database: a migration
the newer version ran stays applied. Check the release notes before rolling
back across one.

`helm uninstall muster` removes everything except the generated Secret, the
Postgres and bundled store volumes, and the speech model cache (marked
`keep`). Delete those by hand to start over.

## What CI proves, and what it does not

- `helm lint` and `helm template` of every test value set, each validated
  with kubeconform against the Kubernetes schemas;
- the env ledger and the RBAC test above, and the real server accepting each
  value set's configuration;
- on a kind cluster, once per RBAC layout, with the admission policy on: the
  minimal install reaches `/readyz`, the generated password signs in, an
  upgrade turning the agent plane on keeps every generated secret, an agent
  created through `POST /api/agents` gets its namespace, pod and
  NetworkPolicy, and destroying it removes the namespace, with no RBAC
  refusal in muster's log; and muster's ServiceAccount is refused a namespace
  outside the prefix, both creating one and deleting one. That agent is a
  stand-in, not a Claude session: CI has no Claude subscription token.
- on a kind cluster, the bundled services running: a sign-in through the
  bundled GoTrue (a wrong password is refused), an attachment uploaded
  through muster that lands in the bundled store and comes back through its
  signed link, two spoken clips (the dictation check's sample among them)
  transcribed by the bundled speech-to-text service into their own words,
  `muster doctor` run as the install's notes print it, an upgrade that turns
  push and the connector store off and on again keeping the same VAPID pair
  and connector key, and the `POSTGRES_PASSWORD` rotation above. That leg
  runs a smaller speech model than the default
  (`Systran/faster-whisper-base.en`), to keep its time down.
