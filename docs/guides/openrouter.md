# Guide: OpenRouter (suggested replies, focus, the chief)

One guide per optional dependency; the list is in
[docs/README.md](../README.md#set-it-up-the-core-first-then-one-feature-at-a-time).
Words used here are in the [glossary](../glossary.md).

## What it adds

One [OpenRouter](https://openrouter.ai) key turns on muster's model calls:

- [**suggested replies**](../features.md#suggested-replies): reply chips when a
  session hands back to you;
- [**focus**](../features.md#focus): a title and summary per session, which
  search and the chief read, and a session's first good title as its name;
- the [**chief**](../features.md#chief) on OpenRouter, when it has no Claude
  subscription token, and the [chief nudges](../features.md#chief-nudges)'
  stall classifier.

What is sent to the model: [suggested-replies.md](../suggested-replies.md#what-is-sent-to-the-model),
[focus.md](../focus.md).

## What it needs

- An OpenRouter API key. 🔴 **muster sets no budget of its own: put a credit
  limit on the key** in OpenRouter's key settings. That limit is the only
  spending cap.
- Members of a multi-user install store their own keys, which needs the
  [connector store](connectors.md).

## Turn it on

1. In the hub's `.env`: `MUSTER_AGENT_OPENROUTER_KEY=<your key>`. With an agent,
   write it from a file so the key never passes through the chat
   ([INSTALL.md, "Optional features"](../../INSTALL.md#step-6-optional-features)).
2. `docker compose up -d --wait`.
3. Suggested replies stay **off for each user until they turn them on**:
   **Account → Suggested replies**.
4. Optional: the models (`MUSTER_SUGGEST_MODEL`, `MUSTER_FOCUS_MODEL`,
   `MUSTER_CHIEF_LLM_MODEL`) and focus's interval. Defaults:
   [configuration.md](../configuration.md#muster-server-core) (the chief's,
   under [agent provisioning](../configuration.md#muster-server-agent-provisioning)).

## Check it

- `muster doctor`: in its feature list (on stderr), the `suggested-replies`
  and `focus` lines each start with `ON`:
  `muster doctor 2>&1 | grep -E '^ +ON +(suggested-replies|focus)( |$)'` prints both
  ([cli.md](../cli.md#muster-doctor)).
- After a focus pass, search (`/`) finds a session by its focus title.
- With suggested replies turned on in Account, chips appear above the box
  when a session finishes a turn with its Rendered view open.

## Turn it off

- **Everything at once:** remove `MUSTER_AGENT_OPENROUTER_KEY` and run
  `docker compose up -d --wait`. No model call is made.
- **One at a time:** `MUSTER_SUGGEST=off` stops suggested replies for everyone;
  `MUSTER_FOCUS_INTERVAL=0` stops the periodic focus pass
  ([configuration.md](../configuration.md#muster-server-core)).
