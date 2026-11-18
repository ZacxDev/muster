# Building on muster

muster is meant to be extended. Out of the box it lists every coding agent on
your machines and asks you when one needs you. What you add decides what else
reaches it, and what it can do for you. This page is the map: the six ways to
extend muster, and the list of plugins people have published.

New to it? Build a plugin first: [plugins-quickstart.md](plugins-quickstart.md)
takes about 15 minutes. Then give it a specialised officer:
[officer-plugin-recipe.md](officer-plugin-recipe.md).

## What you can build

| you build | what it is | it lets you | start with |
|---|---|---|---|
| **A plugin** | a separate program you run, holding one narrow token. It calls muster; muster never calls it | bring another system's messages (an inbox, a chat, a forum, a tracker) to your chief or an officer, and send the replies you approve | [plugins-quickstart.md](plugins-quickstart.md), then the reference [plugins.md](plugins.md) |
| **An officer** | a Claude Code agent with standing instructions (a charter) and a queue, on muster's own backend | give one job (support, release notes, QA, marketing) to an agent that works its queue and asks you before anything leaves | [officers-builtin.md](officers-builtin.md) |
| **An officer with its own plugin** | the two together: the plugin feeds the officer's queue and posts the drafts you approve | a specialised assistant for one channel, with your approval on every word it sends | [officer-plugin-recipe.md](officer-plugin-recipe.md) |
| **A connector** | a stored credential that agents use through muster's proxy, without ever seeing it | let agents call an HTTP API (or query a database) you approve once | [connectors.md](connectors.md) |
| **Tools for your sessions** | muster's MCP server, offered to your mirrored Claude Code sessions and to dispatched agents | let a session ask you and wait, answer an ask, decide a draft, route a message, publish a page | [attention.md](attention.md#mirrored-claude-code-sessions), [pages.md](pages.md) |
| **A client of the API** | anything holding a personal API token (`mst_…`) or the `muster` CLI | script muster: tasks, officers, plugins, the ask log, one-shot agent jobs | [cli.md](cli.md), [auth.md](auth.md#machine-credentials), [jobs.md](jobs.md) |

Plugins and officers are the two meant for others to publish. A plugin is a
program in a repository of its own; an officer is a charter and a few choices
on the Officers page, which a plugin's README can carry as a starting point.

### Plugins in more detail

- **Messages in.** A plugin posts each upstream message once
  (`POST /plugin/v1/messages`). muster hands it to your chief, or to the
  officer the plugin is bound to, fenced as untrusted data.
- **Replies out, approved.** Nothing a plugin sends was not approved by you,
  on its exact text, unless you chose to let muster answer someone you trust
  without asking: a sender you allowlisted for that plugin, whose identity the
  plugin verified, gets the answer one of your sessions wrote straight away
  (`MUSTER_ROUTE_RELAY_ALLOW`; [Routing a message to a session](plugins.md#routing-a-message-to-a-session),
  [`sender_verified`](plugins.md#sender_verified)). The plugin collects an
  approved reply (`POST /plugin/v1/drafts/claim`), sends it, and reports what
  happened.
- **Its own rules.** A plugin with `draft_checks` checks each reply before you
  can approve it, so your upstream's limits (a length, no Markdown) are
  enforced in one place, the plugin.
- **More it can do:** one task per upstream record
  ([plugin tasks](plugins.md#post-pluginv1tasks-one-task-per-upstream-record)),
  a section of guidance for your chief that you approve
  ([chief instructions](plugins.md#chief-instructions)), and a page of its own
  beside a session ([viewer plugins](plugins.md#viewer-hand-off)).
- **Pairing.** A plugin can ask for its own token, so nobody copies one by
  hand ([pairing](plugins.md#pairing-a-plugin-without-handling-its-token)).

There is no SDK yet. The protocol is plain HTTP and JSON, and the
[template](https://github.com/ZacxDev/muster-plugin-template) is a small Go
program to copy.

This repository ships two plugins of its own: the ClankMail plugin, signed
agent-to-agent messages ([clankmail.md](clankmail.md)), and the preview
gateway, a viewer plugin ([previews.md](previews.md)).

## Published plugins

Each plugin is its own public repository. muster keeps this list; to add
yours, see [below](#add-your-plugin).

| plugin | what it connects | author | repository | status |
|---|---|---|---|---|
| muster-social | posts an officer's approved drafts to social sites (X, LinkedIn) from your own signed-in browser | ZacxDev | [ZacxDev/muster-social](https://github.com/ZacxDev/muster-social) | early: tested against fixture pages, not yet against the live sites |
| muster-plugin-template | nothing real: an echo round-trip to copy and turn into your own plugin | ZacxDev | [ZacxDev/muster-plugin-template](https://github.com/ZacxDev/muster-plugin-template) | template |
<!-- MORE PLUGINS GO HERE: one row each, in the columns above. Rows for the
     maintainer's other plugins are added here once each repository is public. -->

A listing says the plugin met the bar below when it was added. It is not a
review of its code: read a plugin before you give it a token.

## Add your plugin

Open a pull request against this page that adds one row to the table above:
its name, one line on what it connects, you as the author, its repository and
its status in your own words (`early`, `in use`, `unmaintained`).

The minimum bar, checked when the row is reviewed:

- **A public repository with an open-source licence** (MIT, Apache-2.0 or
  similar), in a `LICENSE` file.
- **A README** that says what the plugin connects, how to run it against a
  muster, what it sends where, and **the muster version you tested it
  against**.
- **A stated token scope.** Which [capabilities](plugins.md#capabilities) it
  needs (`messages`, `draft_checks`, `viewer`, `chief_extension`) and why each
  one, and whether it is meant to be [bound to an officer](plugins.md#a-plugin-bound-to-an-officer).
  Ask for the fewest that work.
- **No telemetry to third parties without saying so.** If the plugin sends
  anything anywhere other than muster and the upstream it connects (usage
  statistics, crash reports, an analytics service), the README says what, to
  whom, and how to turn it off.
- **Approval is never bypassed.** The plugin sends only what it claimed
  through `POST /plugin/v1/drafts/claim`, sends each `draft_id` at most once,
  and sets `sender_verified` only when it authenticated the sender.

A row whose repository disappears, or that stops meeting the bar, is
removed.

## Where to ask

Questions about the protocol, and anything in [plugins.md](plugins.md) that
reads wrong: open an issue in this repository.
