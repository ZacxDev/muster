# Guide: connectors (the encryption key and the connect URL)

One guide per optional dependency; the list is in
[docs/README.md](../README.md#set-it-up-the-core-first-then-one-feature-at-a-time).
Words used here are in the [glossary](../glossary.md).

## What it adds

A sealed store of credentials, in two steps:

- **The key alone:** each member's own Claude token and OpenRouter key, stored
  on **Account → Credentials**. The [chief](../features.md#chief) needs this
  much.
- **The key and the connect URL:** HTTP and read-only Postgres credentials that
  agents use through muster's proxy without ever seeing the secret, with your
  approval per request. [connectors.md](../connectors.md).

## What it needs

- **A key of 32 random bytes**, as 64 hex digits or as base64:
  `openssl rand -hex 32` or `openssl rand -base64 32`. 🔴 **Keep it.** It seals
  every stored secret; without the same key they cannot be read.
- **For agents' connectors:** agents that reach muster's connect listener,
  `MUSTER_CONNECT_PORT`, at the address `MUSTER_AGENT_CONNECT_URL`. That is
  [dispatched agents](../features.md#dispatch) on Kubernetes (the
  [Kubernetes guide](kubernetes.md)), the Compose chief, which sets both itself
  ([configuration.md](../configuration.md#muster-server-agent-provisioning),
  `MUSTER_CHIEF_RUNTIME`), or an officer on a host
  ([officers-builtin.md](../officers-builtin.md)).

## Turn it on

1. In the hub's `.env`: `MUSTER_CONNECTOR_ENCRYPTION_KEY=<the key>`. With an
   agent, `env_secret` from [INSTALL.md, "Optional features"](../../INSTALL.md#step-6-optional-features)
   writes it without printing it.
2. For agents' connectors outside the Compose chief, also
   `MUSTER_CONNECT_PORT` and `MUSTER_AGENT_CONNECT_URL`
   ([connectors.md](../connectors.md#configuration)).
3. Optionally `MUSTER_DIAL_DENY_PRIVATE=1`, so a Postgres connector cannot dial
   loopback or private addresses
   ([connectors.md](../connectors.md#refusing-private-addresses)).
4. `docker compose up -d --wait`.

## Check it

- `muster doctor`: the feature list reads `ON connectors`
  ([cli.md](../cli.md#muster-doctor)).
- `docker compose logs muster | grep 'connector store:'` reads
  `MEMBERS' CREDENTIALS ONLY` with the key alone, and `ENABLED` with the connect
  URL too; then the Connectors tab appears.

## Turn it off

Remove the lines from `.env` and run `docker compose up -d --wait`. Every
connector route then answers 503 and members cannot store credentials. What
was stored stays in the database, sealed with that key: keep the key if you
may turn the store on again. Features that need the store (the chief, the
[GitHub App](github-app.md)) refuse or turn off without it.
