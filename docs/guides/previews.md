# Guide: previews (the preview gateway)

One guide per optional dependency; the list is in
[docs/README.md](../README.md#set-it-up-the-core-first-then-one-feature-at-a-time).
Words used here are in the [glossary](../glossary.md).

## What it adds

A web server an agent runs on one of your machines (a dev server, a docs site,
a Storybook) served to you inside muster or in a new tab, through a tunnel. The
server stays on that machine's loopback; nothing is public, and nothing is
opened until you click **Open** or the agent asks. [previews.md](../previews.md).

## What it needs

This is advanced: it needs infrastructure muster does not ship a deploy file
for.

- **The preview gateway, `muster-preview`, which you run** (it is in muster's
  image; neither the Compose file nor the Helm chart runs it).
- **A preview zone:** a wildcard DNS name with a wildcard certificate, and an
  edge in front of the gateway that keeps `Host`, passes WebSocket upgrades,
  does not buffer, and **overwrites** the client-address header
  ([previews.md](../previews.md#setting-it-up)).
- **Plugins on the server** (`MUSTER_PLUGINS=1`) and the zone on its allowlist
  of viewer origins (`MUSTER_PLUGIN_ORIGINS`):
  [features.md](../features.md#plugin-viewers). How the `plugins` line reads on
  a server without a chief: [features.md](../features.md#plugins).
- **On each machine:** `muster-mirror` with `-preview`, a host token from the
  gateway, and a `previews.json` allowlist of ports; for the agent's
  `preview_*` tools, its MCP endpoint too
  ([agent ask tool](../features.md#agent-ask-tool)).

## Turn it on

Follow [previews.md, "Setting it up"](../previews.md#setting-it-up), in order:
the server's two settings, a viewer plugin token, the gateway, a host token
per machine, each machine's `muster-mirror`, and the agent's MCP entry. Every
variable is in [configuration.md](../configuration.md#muster-preview-the-preview-gateway)
(the gateway) and [configuration.md](../configuration.md#muster-mirror-host-agent)
(the machine).

## Check it

- `muster doctor`: the feature list reads `ON plugin-viewers`
  ([cli.md](../cli.md#muster-doctor)).
- On a machine: `muster-mirror -list -previews` prints the servers it would
  offer.
- A mirrored window running an allowlisted server shows a **Preview** button
  with a count.

## Turn it off

- **One machine:** revoke its host token on the gateway
  (`muster-preview host-token revoke <id>`): at the gateway's next recheck its tunnel ends
  and its previews close ([previews.md](../previews.md#lifecycle)). Then drop
  `-preview` from its `muster-mirror`.
- **Everywhere:** unset `MUSTER_PLUGIN_ORIGINS` on the server, stop the
  gateway, and revoke its plugin token on `/plugins`.
