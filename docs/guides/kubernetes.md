# Guide: Kubernetes (the agent plane)

One guide per optional dependency; the list is in
[docs/README.md](../README.md#set-it-up-the-core-first-then-one-feature-at-a-time).
Words used here are in the [glossary](../glossary.md).

## What it adds

What only Kubernetes gives muster: **dispatched agents**, each in its own pod
and namespace behind its own NetworkPolicy, started, chatted with and stopped
by muster; a per-user chief on Kubernetes; pod officers; and everything built on
them ([features.md, "Advanced features"](../features.md#advanced-features)).
Your mirrored sessions keep working as before.

## What it needs

- **A Kubernetes cluster, with muster deployed inside it.** The provisioner
  uses in-cluster credentials only, so the Compose stack cannot dispatch
  agents.
- **The Helm chart**, `charts/muster`, published with every release. It
  installs the server, its Postgres and the RBAC, and each feature off until
  you turn it on.
- An agent image for the agents you dispatch
  ([provisioner.md](../provisioner.md)).

## Turn it on

Follow [kubernetes.md](../kubernetes.md): [install the
chart](../kubernetes.md#install), create its [Secrets](../kubernetes.md#secrets),
then turn on [the agent plane](../kubernetes.md#the-agent-plane). The values
that map onto each feature are in
[kubernetes.md, "Values per feature"](../kubernetes.md#values-per-feature); the
same environment variables as the Compose stack, in
[configuration.md](../configuration.md#muster-server-agent-provisioning).

## Check it

- `muster doctor`: in its feature list (on stderr), the `dispatch` line (and
  the `agent-gateway` and `claude-code-agents` lines, for what you turned on)
  starts with `ON`: `muster doctor 2>&1 | grep -E '^ +ON +dispatch( |$)'`
  ([cli.md](../cli.md#muster-doctor)).
- muster's log reads `agent provisioning: LIFECYCLE …`, not `UNWIRED`.
- [What CI proves, and what it does not](../kubernetes.md#what-ci-proves-and-what-it-does-not).

## Turn it off

Turn the agent plane's values off and upgrade the release
([kubernetes.md, "Upgrade and rollback"](../kubernetes.md#upgrade-and-rollback)).
muster keeps running without dispatch, as in the Compose stack.
