---
name: muster
description: Use an installed muster from a coding-agent session. Check a muster install with `muster doctor`, ask the person a question through muster's Attention queue (it reaches their phone) instead of stopping when they are away, and read or update tasks on muster's board with the `muster` CLI. Use when muster is installed and the person asks to check muster, to ask them on their phone, or to work with muster tasks.
---

# Using muster

This skill assumes muster is already installed. If it is not, use the
`muster-setup` skill instead.

The reference for everything below is muster's documentation; read the linked
page before relying on a detail here:

- the CLI: <https://github.com/ZacxDev/muster/blob/main/docs/cli.md>
- asks and the agent tools: <https://github.com/ZacxDev/muster/blob/main/docs/attention.md>

## Check the install: `muster doctor`

```sh
muster doctor >/dev/null                          # any machine
muster doctor --deploy-env ~/muster/.env >/dev/null   # the machine running the server
```

The report goes to stderr, and stdout is one JSON document, so `>/dev/null`
leaves only the report. Exit status 0 means no check failed; a SKIP never fails
the run. Each failing check prints its fix: follow it and run the doctor again.
`--deploy-env` takes the path of the server's compose `.env`; the path above is
an example.

## Ask the person instead of stopping

When the `muster` MCP server is connected to this session, it offers
`attention_ask` and `attention_wait`. If you need the person's input (a
decision, a missing fact, an approval), do not end your turn and wait in the
chat:

1. Call `attention_ask {question, options?}`. `options` (at most 10) become
   buttons; the person can still answer in free text. It returns an `id`.
2. Call `attention_wait {id}`, and call it again for as long as it answers
   `status: "pending"`.
3. Continue with the answer it returns. If the ask was cancelled or expired,
   say so and stop at that decision.

The question appears in muster's Attention view and as a notification on the
person's phone.

If those tools are not available, the MCP endpoint is not set up for this
session. It is optional and off by default: the host's `muster-mirror` serves
it on a loopback address when `MUSTER_MIRROR_MCP_LISTEN` is set, and Claude Code
needs an MCP server entry for it. The exact steps are in docs/attention.md,
section "Mirrored Claude Code sessions" and its "Client configuration". Do not
set it up unless the person asks: it changes how their sessions are reached.

## Tasks from the CLI

`muster task ls`, `muster task get`, `muster task create`, `muster task comment`
and `muster task status` work on muster's task board. The CLI reads its API URL
and token from `~/.muster/muster.env` (or `MUSTER_API_URL` and `MUSTER_TOKEN`,
or flags). Never print the token.
