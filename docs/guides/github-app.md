# Guide: the GitHub App

One guide per optional dependency; the list is in
[docs/README.md](../README.md#set-it-up-the-core-first-then-one-feature-at-a-time).
Words used here are in the [glossary](../glossary.md).

## What it adds

muster's GitHub identity for its admins, and, for agents, tokens that reach
**one repository** each, minted per request and never shown to the agent.
[github-app.md](../github-app.md).

## What it needs

- **A GitHub App you register** and install on your account or organization.
- **The whole agent-facing [connector store](connectors.md):**
  `MUSTER_CONNECTOR_ENCRYPTION_KEY`, `MUSTER_AGENT_CONNECT_URL` and
  `MUSTER_CONNECT_PORT`, all three
  ([configuration.md](../configuration.md#muster-server-optional-features)
  for their values). Agents reach GitHub only through muster's proxy, and
  their grants live in that store; with the App and any of the three missing,
  muster refuses to boot. The key alone (members' own credentials) is not
  enough.
- The tokens are for [dispatched agents](../features.md#dispatch), which need
  Kubernetes (the [Kubernetes guide](kubernetes.md)).

## Turn it on

1. Register the App and install it:
   [github-app.md, "Register the App"](../github-app.md#register-the-app) and
   ["Install it on the organization"](../github-app.md#install-it-on-the-organization).
2. Set `MUSTER_GITHUB_APP_ID`, `MUSTER_GITHUB_APP_PRIVATE_KEY` and
   `MUSTER_GITHUB_APP_ACCOUNTS`, and optionally `MUSTER_GITHUB_APP_ACCESS=read`
   for read-only tokens:
   [github-app.md, "Configure muster"](../github-app.md#configure-muster)
   (how to write the key, how to find each account's id, the Kubernetes
   Secret).
3. Restart muster (`docker compose up -d --wait`, or the chart's upgrade).

## Check it

- `muster doctor`: in its feature list (on stderr), the `github-app` line
  starts with `ON`: `muster doctor 2>&1 | grep -E '^ +ON +github-app( |$)'`
  ([cli.md](../cli.md#muster-doctor)).
- muster's log says the App authenticates and names the accounts it is
  installed on; every other line it can print, and what each means:
  [github-app.md, "Check it"](../github-app.md#check-it).

## Turn it off

Remove the `MUSTER_GITHUB_APP_*` lines and restart muster. Agents then get no
GitHub token; the connector store and everything else in it stay as they are.
To revoke the App's access on GitHub's side as well, uninstall it there.
