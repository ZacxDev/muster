# Usage: what the chief costs

Every plugin message your chief handles is a model turn, and turns cost
tokens. The **Usage** page (`/usage`, linked from `/plugins` and `/rules`)
shows how much, where it goes, and how many messages were placed without the
chief. It is on whenever muster has a database. It needs no setting.

## What is recorded

**One row per chief turn** (`chief_turn_usage`, migration `0105`):

| Field | What it is |
|---|---|
| trigger | What started the turn: `plugin` (an inbound plugin message: its plugin, its sender and its id), `task_event` (a task muster handed the chief), `owner_chat` (a line you typed in the chief's chat), `view` (a line you typed on a session's view), `answer` (your answer to its question, delivered), `session_question` (one of your mirrored sessions asked the chief with `chief_ask`), `hand_back` (a route handed back), `machine` (the machine message door, `POST /api/agents/{name}/messages`), `kickoff` (the chief's first turn), or `other`. |
| outcome | What the turn did, from the tools it called: `routed` (route_to_session), `task` (task_create_gated, task_forward, agent_dispatch), `ask` (ask_owner, attention_ask, task_route_attention), `settled` (plugin_message_settle), `rule_proposed` (routing_rule_propose and nothing above), `other`, or `failed`. A turn that also proposed a routing rule is flagged `rule_proposed` too. |
| tokens | Input, output, cache read and cache creation, **as the chief's runtime reported them** for the turn. |
| usage_known | False when the runtime reported no usage. Such a turn shows as unknown, not as free. |

The tokens are the real numbers. Not an estimate:

- A **Claude Code chief** runs its CLI under ccd. When the turn ends, ccd adds
  up the usage the CLI wrote into its transcript, once per API call: the CLI
  repeats a call's usage on every content block it writes. ccd sends the
  total on the turn's `response.completed`. This works the same whether the
  CLI reaches Anthropic directly or reaches OpenRouter through muster's LLM
  proxy ([the chief on OpenRouter](chief.md#the-chief-on-openrouter)): the
  transcript holds what the provider returned either way. Subagent
  (sidechain) calls are not counted.
- A **gateway chief** reports usage in the Responses API's shape. There,
  `input_tokens` includes cached tokens, so muster moves
  `input_tokens_details.cached_tokens` out of input and into cache read.

Both land in the same row. A chief turn that used tools muster runs itself
makes several requests, and their usage is summed.

**One row per inbound plugin message** (`plugin_message_handling`), naming who
handled it:

- one of muster's own paths, with no chief turn: `fleet_report`, `thread`
  (a reply on a thread a session started), `closed_pr` (a review of a merged
  or closed pull request), `pr_route` (a review routed to the session that
  wrote the pull request), `mail_resume`, `task_event_mention`, or the
  [task triage](plugins.md#an-assigned-task-muster-places-itself) of an
  assigned task: `task_forward`, `task_route` or `task_board`;
- `rule`: an approved [routing rule](plugins.md#routing-rules), with its id;
- `chief`: a chief turn. When a rule matched but its session could not take
  the message, the row also names that rule as the fallback.

Rows hold numbers, tags and ids. They never hold a prompt, a reply or a
message body. Both tables are owned, under row-level security: you see only
your own rows. A row outlives the message, the plugin and the chief it
counted, so totals do not change when a plugin is revoked. Nothing prunes the
rows yet: there is one small row per turn and one per message.

## The page

Pick a window: the last 7 days or the last 30. The page shows:

- **Totals.** Plugin messages and the share handled without the chief (by a
  rule or by muster's own paths), split by rule, muster path and chief turn.
  Then the chief turns, their tokens (input, output, cache read, cache
  write), and how many turns reported no usage.
- **By day.** Messages, turns and tokens for each UTC day.
- **Messages by source.** Each plugin's messages, split by who handled them.
- **Chief tokens by source.** The chief's turns and tokens, grouped by what
  started them and, for a plugin, which plugin.
- **Costliest senders.** The ten plugin senders whose messages cost the chief
  the most tokens.
- **Rules.** Each approved rule: its hits in the window and in all, and its
  **estimated tokens avoided**. The page labels this figure as an estimate,
  not a measurement.

The estimate takes a rule's hits in the window and multiplies them by the
average total tokens of the chief's turns for that rule's plugin before the
rule was approved. When the rule matches on a sender, only that sender's
turns count. All of the history before approval is read, even turns older
than the window. Turns with no reported usage are left out. A rule with no
such turns has nothing to estimate from, and the page says so.

⚠ A token total is a count, not a cost. A cache read is billed far below an
input token, so two turns with the same total can cost very different
amounts. The page shows the four kinds apart for that reason. muster records
no prices.

## Using it

1. Look at **Costliest senders** and **Chief tokens by source** to see where
   the chief's tokens go.
2. When the chief keeps placing one sender's messages the same way, it can
   propose a [routing rule](plugins.md#routing-rules). You can also ask it to.
3. After you approve the rule, watch its hits and estimate under **Rules**,
   and watch the share without the chief rise.
