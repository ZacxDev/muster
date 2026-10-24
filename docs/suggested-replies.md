# Suggested replies

When a mirrored Claude Code session finishes a turn and hands control back to
you (it asks a question, offers options, or says "should I…"), muster can
offer up to three short replies for the composer, written by a model from the
conversation and from what you asked for. They are labelled **AI-suggested
replies**.

🔴 **A suggestion is never sent.** Tapping one fills the composer (below a
draft you already started, never over it); you edit it and press Send, which
keeps its own [step-up check](sending-and-step-up.md). Nothing in the
suggestion path can reach a pane.

This page covers the server side (generation, grounding, the routes). The chip
row in the composer is a follow-up.

## When a set is generated

Only for a mirrored Claude Code session **with a rendered view open**, and only
at the end of a turn: when the host's busy marker falls while the view is open,
or when a view opens on a session that is already idle. A session nobody is
looking at never generates.

Then, every one of these must hold, or nothing is generated (and nothing is
served — the same checks run again whenever the composer asks):

- suggestions are on: not `MUSTER_SUGGEST=off`, not turned off by you, not
  [paused](#auto-pause);
- the window is a Claude Code pane that names its session (not Hermes, not a
  plain terminal) and has a rendered view, on a host that is interactive (the
  composer is enabled);
- the window's own status is **idle** — not busy, not **waiting** (a permission
  prompt or a question: text pasted there would answer the prompt), not
  unknown;
- no question card (AskUserQuestion) is open for the session;
- no attention ask raised by the session is open — including one you hid on
  this device with "Hide until next ask": hidden is not answered;
- the final reply hands control back. This pre-gate is a cheap text check (a
  `?` near the end, an option list beside a choice word, or "want me to",
  "should I", "your call", "let me know"…). It is a heuristic, and it only
  decides whether a call is worth making.

A set is served only for the turn it was made for: once the window's status
changes, the old set is no longer offered. The window's status can trail the
transcript by a few seconds, so a turn end waits up to 12 s (two of the host's
window-list refreshes) for the turn's own idle status — not the idle from
before the turn. A permission prompt or a question ends the wait at once, and
a turn that moves on while the checks run generates nothing.

## What is sent to the model

The same rules as [session focus](focus.md#the-summarizer), to the same
provider, on the session owner's OpenRouter key:

| entry | sent |
|---|---|
| the session's focus title and description | yes, in a block marked **untrusted** |
| your prompts | the newest 40, first line each (240 bytes), numbered `A<n>` by their place in the whole conversation |
| the last 8 entries before the final reply | prompts and replies as written, 4 KiB each |
| the final reply | whole, up to 8 KiB; past that its **end** is kept (where the options and the question are) |
| tool calls | the tool's **name**, plus the file path when the call names one |
| tool arguments, tool output, thinking, errors | never |

Every field goes through the secret redactor (`internal/redact`) on the
server before it leaves, whatever the host already did. The whole input is at
most 24 KiB.

## How a suggestion stays grounded

The model must answer strict JSON: up to three items, each with its `text`
(one line, at most 140 characters), its `kind` — `pick` (one of the agent's
options), `gap` (one of your asks the reply did not address), `verify` (a check
the agent skipped), `decide` (approve or decline a proposed action, with its
condition), `redirect` (narrow or stop) — the ids it `cites` (`R`, the final
reply, or an ask `A<n>`) and a `quote` copied from what it cites.

Every item then passes a deterministic filter, and is dropped unless:

- every cited id exists in the input, and the quote is verbatim (whitespace
  aside) in a cited source and at least 6 bytes long;
- a `pick` and a `decide` cite `R` and quote it — an option or an action must
  be the **agent's own**, so text quoted from elsewhere cannot steer an
  approval; a `gap` cites one of your asks;
- the text shares a word with what it cites, and every identifier-like term in
  it (a `--flag`, a path, a number) appears in the input — a term from
  nowhere, or from text that was never sent (tool output), is dropped;
- it does not start with `/`, `!` or `#` (a command, bash mode or a memory
  note in Claude Code), spans one line, holds no URL and no code fence, and the redactor leaves it
  unchanged;
- (heuristics, flagged as such) it is not a stock phrase ("continue", "looks
  good", "ok"…), and a `decide` carries its condition ("but", "only if",
  "first", "until", "don't"…).

No survivors means no chips, which is a correct outcome.

⚠ **Residual risk.** The structural defences against prompt injection are that
tool output is never sent, nothing is ever sent automatically, and a tap only
fills. A model that invents an action in plain words that happen to share a
word with the reply can still pass the filter; the chip is then a draft you
read before sending.

## Cost and caching

A set is cached **in memory only**, by the fingerprint of the exact input and
by the turn it answers: the same input, or the same turn, never calls the
model twice, and a new turn calls it exactly once. Two views of the same turn
share one call, even when one of them resumed mid-conversation and saw less
(a resumed view never generates from the partial turn it resumed into; it
does from the first whole turn after it). A set's text is
dropped after an hour (only its fingerprint is kept, so an unchanged session
is not paid for again). **No suggestion text is ever written to the
database.**

Calls are capped at **120 per user per hour** and 1,000 in all, two at a time,
separately from focus.

Measured on the synthetic eval set (below) with `openai/gpt-6-luna` at
reasoning effort `low`, 64 calls: about 780 tokens in and 170 out per
generation, **$0.00016 per generation**, latency **p50 2.3 s, p95 4.5 s**.

## Auto-pause

The composer reports, as counts only, each set it showed and each chip that
was tapped (`POST /mirror/view/suggest/event`). A user shown at least 200 sets
of whom fewer than 5% were tapped is paused until they turn suggestions back
on. The tallies are in memory, so a restart clears them.

## Turning it off

- `MUSTER_SUGGEST=off` stops everything ([configuration](configuration.md)).
- Each user's own toggle: `POST /account/suggest` with `on=0` or `on=1`
  (stored in `suggest_preferences`, migration `0047`; on is the default). Off
  also drops that user's cached sets, and a call already running is not
  kept. A toggle that cannot be read as its owner reads as off.

## Routes

| route | what |
|---|---|
| `GET /mirror/view/suggest?host=&session=[&window=]` | the window's current set as JSON: `{"ai": true, "label": "AI-suggested replies", "suggestions": [{"text", "kind"}], "fingerprint", "reason"}`. It never generates. `reason` says, in fixed words, why there are none |
| `POST /mirror/view/suggest/event` | `host`, `session`, `window`, `event=shown\|filled`: counts for the auto-pause; no text is accepted |
| `POST /account/suggest` | `on=1\|0`: the caller's own toggle |

The rendered view's socket sends `{"type": "suggest"}` when a set for the
current turn is ready; the page then fetches it.

## The eval set

`internal/suggest/testdata/eval/fixtures.yaml` holds 36 **synthetic**
conversations (32 that hand back, 4 that do not), several of them
injection-shaped, each with the properties a useful set must have: cite your
ask, include the option's label, never contain the injected action.

- `go test ./internal/suggest/` (every CI run, no key) builds each fixture's
  input exactly as the server does, checks the pre-gate verdict and that no
  tool output or argument reaches the input, and runs each fixture's canned
  answer through the filter.
- `go run ./cmd/muster-suggest-eval -live` (by hand only; it spends the key in
  `MUSTER_AGENT_OPENROUTER_KEY`) runs the real model on every fixture and
  prints each property's pass rate, the latency and the cost. Two runs of all
  32 hand-back fixtures with the default model: 63 of 64 fixture runs met
  every property.
