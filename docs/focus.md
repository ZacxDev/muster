# Session focus

Every session — a dispatched agent or a mirrored Claude Code window — can carry
a **focus**: a short title (stored whole) and one to three sentences saying what
it is working on now. The search palette (the magnifier in the sidebar or on the
bottom bar, or `/`) finds a session by its focus title and shows it beside the
session's name, but ranks matches by the **name** first, so typing a name still
puts that session first. A session's page lists its focus over time behind the
header's focus-history button ([below](#focus-history)). (The Agents list page,
whose cards were headed by their focus, is retired.)

Your **chief** reads focus to decide which session should handle something
(`fleet_list`, `focus_search`). It never computes focus itself.

Focus needs a database. It is stored in `agent_focus` (migration `0020`), one
row per session, owned and under row-level security. Its history is in
`agent_focus_history` (migration `0022`); see [focus history](#focus-history).

## What a session is

| kind | keyed by | notes |
|---|---|---|
| `agent` | the agent's id | deleted with the agent |
| `mirror` | the **Claude Code session id** of the conversation in the window | not the tmux window id |
| `mirror` | `hermes:` and the **Hermes session id** (the conversation's lineage root) of a Hermes window | only a Hermes window whose host names its session |

A mirrored session is keyed by the conversation, not the window, so its focus
survives tmux reissuing window ids and follows a conversation into another
window. `muster-mirror` reports the id per window (`claudeSession` in its
window list). An older `muster-mirror` does not, and its windows simply have no
focus. Only Claude Code windows, and Hermes windows whose host names their
session, have one.

The transcript **path** is not sent to the server. The window list is one
WebSocket message, and its largest case already fills 96% of a frame; muster
reads a transcript through the host's own transcript stream instead (below).

## How focus is written

Only muster writes it, server-side, in a periodic pass (`MUSTER_FOCUS_INTERVAL`,
default `15m`), which for each session:
- check the session is one muster can see now — an agent of yours, or a
  Claude session an online window shows. One it cannot see is refused and
  **no row is written**, so naming arbitrary ids cannot grow the table;
- take a **bounded tail** of the session's transcript: at most 40 entries,
  1 KiB each, 16 KiB in all (what each entry carries is below). A dispatched
  agent's tail is its newest chat. A mirrored window's is the backfill the
  host sends when muster opens its transcript stream (the one the rendered
  view opens), closed again at once — and only if that stream is still the
  conversation asked for: after a `/clear` or a restart the window holds
  another conversation, and nothing is stored;
- **skip the summarizer when the tail has not changed**: the tail's sha256
  is stored with the focus, and an equal one makes no call;
- otherwise ask the summarizer (below) for a title and a description, clean
  them (one line, no markup or control characters, three sentences; a title is
  kept whole, cut only past a 300-character storage guard — migration 0023 —
  while the prompt asks for 3 to 8 words), and store them;
- on any failure, keep the previous focus and record **why** beside it, as a
  short fixed reason: `no OpenRouter key`, `the OpenRouter key could not be
  read` (a credential-store failure, not a missing key), `rate-limited`,
  `summarizer error (HTTP <status>)`, `the summarizer's answer was
  unusable`, `the summarizer could not be reached`. Nothing from the transcript, the key or the provider's error
  text is stored or shown. The card's popover says it ("No focus: …", or
  "Last refresh failed: …" on a focus that is now stale, whose title is also
  marked `stale`), and so does
  `fleet_list` (`focus_problem`). The next successful refresh clears it.

A pass only looks at sessions that were active since their focus was
written, at most 20 per user, and skips disabled users. Calls are capped at
60 per user and 300 in all per hour, two at a time.

### The working directory

With the focus, the pass stores the session's **working directory** as its
transcript last named it (`agent_focus.cwd`, migration `0065`). Claude Code
writes a `cwd` on every transcript record; `muster-mirror` passes the one on
each human prompt along with the prompt (and drops one the secret redactor
would rewrite, or one over 512 bytes), and the newest prompt in the tail
decides. It is read for one thing only: the [fleet report](plugins.md#a-fleet-report-request)'s
mail reply describes a session only when that directory is under
`MUSTER_FLEET_REPORT_PATHS`. It is never shown, never searched and never sent.

It is **untrusted** — it arrives beside text a model wrote — so it is kept only
in a narrow shape: absolute, cleaned (`filepath.Clean`), at most 512 bytes, no
control or invisible characters. A tail whose newest prompt names a malformed
directory makes it **unknown** (an older one never stands in: the session may
have moved); a tail that names none — no prompt in its 40 entries — keeps the
one stored. Unknown is what a dispatched agent, a Hermes window, a host older
than this change, and a session whose focus has not been written since the
upgrade have; the report leaves all of them out. The directory lives on the
focus row, so an ended session keeps it until that row is swept (below):
longer than any report window, unless `MUSTER_FOCUS_RETENTION` is set under 4
days.

**Retention.** A sweep runs at startup and then hourly. It runs whether or not
the refresh pass does: with no summarizer, or with `MUSTER_FOCUS_INTERVAL=0`.
It marks every enabled user's visible mirrored sessions as seen, then deletes
every user's mirrored focus not seen for `MUSTER_FOCUS_RETENTION` (default a
week), and every focus history row older than 30 days. A disabled user's sessions are never marked, so their focus ages out.
An agent's focus dies with the agent. `focus_search` never returns a mirrored
session that no online window shows; the store leaves such sessions out before
it applies its result limit.

## Focus history

Each time a session's focus title or description **changes**, the new focus is
also appended to `agent_focus_history`, in the same transaction as the focus
itself. A refresh that produces the same title and description as the newest
history row adds nothing, so a session that is summarized again without moving
does not fill its history.

You can scroll back through it:
- on a mirrored session's rendered view, the "Your messages" sheet (the list
  icon in the header) has two tabs, **Your messages** and **Focus history**.
  The arrow keys move between them, and the tab you pick is remembered for that
  window in the browser;
- on a dispatched agent's page, the target icon in the header opens a
  **Focus history** dialog. That page has no "Your messages" sheet, so the
  history is a dialog of its own rather than a tab.

Both list the newest first, 20 at a time, and load older pages as you scroll
(or with **Load older**). Pages come from `GET /ui/focus/history`, which reads
under your own row-level security and answers 404 for a session that is not
yours (a mirrored session is yours while you hold its history or an online
window of it).

History is kept for **30 days**, whatever happens to the session's focus. A
mirrored session's focus is deleted a week after it was last seen, but its
history stays, and you can still open it. History rows are deleted:
- after 30 days, by the hourly retention sweep (see "Retention" above). The
  row that is a session's current focus is kept past that while the session
  still has a focus, so the history never reads empty beside a focus;
- when an agent is deleted: its history goes with it.

## The summarizer

[ScaleDown](https://docs.scaledown.ai/api-reference/summarize)'s `summarize`
model, through OpenRouter (`scaledown/summarize`). It is called with the
session owner's OpenRouter key, exactly as that owner's gateway agents get it
([configuration](configuration.md)): the environment's for an admin, the
member's own otherwise. An owner with no key gets no summarized focus, and the
error is recorded.

🔴 **What leaves for each kind of session** (to OpenRouter and ScaleDown):

| entry | dispatched agent | mirrored Claude Code session |
|---|---|---|
| your prompts | as written | as written |
| replies | as written | as written (thinking never) |
| tool calls | the tool's **name**, plus the file path when the call names one (`file_path`, `notebook_path`, `path`) | the same |
| tool arguments (shell commands, file bodies, URLs, patterns, anything else) | never | never |
| tool output | never | never |
| errors | — | the error text |

Prompts and replies are sent as they are: whatever you or a session wrote,
including text pasted from elsewhere, goes out. The one exception is a
mirrored session's: its host redacts anything that looks like a secret before
the transcript leaves it ([mirroring](mirroring.md#rendered-and-terminal-views)), so
what reaches the summarizer is redacted too. The prompt asks the summarizer not
to quote commands, secrets or host names, but a dispatched agent's input is not
redacted.

🔴 **Focus is untrusted.** A title or description is a summarizer's output
over a transcript that can carry an outside party's words, so it can repeat an
injected instruction. The summarizer is told to ignore instructions in the
transcript; every focus tool's description and result (`"untrusted": …`) tell
the chief that focus is data, never instructions. Neither makes the text safe
— they only label it.

## The tools

| tool | who | what |
|---|---|---|
| `fleet_list` | chief | every session: id, kind, name, status, a mirrored window's `where` (its exact tmux target, [attention.md](attention.md#how-a-window-is-named)), focus title, and `focus_problem` (the reason) when its focus is missing or stale |
| `focus_search(query, limit, since_hours?)` | chief | full-text search over titles (weighted higher) and descriptions; id, kind, title and a 120-character snippet per hit. With `since_hours` (1–24) it searches every focus each session had in that window instead of its current one, and each hit carries when that focus was written |
| `sessions_overview(limit?)` | chief | every live session (running agents, online mirrored windows) active in the last 24 h, newest-active first: id, kind, name, host, a mirrored window's `where`, status, current focus, the distinct focus titles of the last 6 h with their times, its pending attention item, and its last three user and assistant messages (300 characters each, secrets redacted, no tool output). A session whose transcript does not arrive within 5 s reads `"transcript": "unavailable (timed out)"`; all the reads share a 12 s budget, and a session it cuts or never reaches reads `"unavailable (overview budget)"`. The answer is capped at 32 KiB and says how many sessions it left out |
| `fleet_report(since, until?)` | chief | every session of the owner's — running agents and mirrored windows, **ended ones included** — with focus activity between `since` and `until` (RFC 3339; `until` defaults to now; at most 4 days apart), most recently active first: id, kind, name, host, a listed window's `where` (this door only: the fleet-report reply below never carries it), repo, state (`waiting`: an attention item is pending; `active`: live now; `ended`), first and last seen, the distinct focus titles of the window with their times (at most 20 per session, the newest), and the pull requests it created or pushed. The chief itself is left out. Capped at 32 KiB; `coverage` says how many sessions were left out and what did not answer (an offline host, a store that could not be read). It shows the owner **everything**, hosts included and unfiltered, because its answer never leaves muster; the same report answering a [fleet-report request](plugins.md#a-fleet-report-request) is narrowed to `MUSTER_FLEET_REPORT_PATHS`, with no hosts and secret-shaped strings removed |

All four are on the agent gateway and on `/mcp`. A subject is spelled
`agent:<id>` or `mirror:<claude session id>`, as `fleet_list` and
`focus_search` return it.

The chief uses focus to pick a session for a plugin message it routes; see
[routing a message to a session](plugins.md#routing-a-message-to-a-session).

### Why a time window over history, and not the current focus

A session's focus is rewritten every refresh, so one that has worked a long job
for hours can have a current focus about another subtask of it, and a search of
the current focus misses it. The newest N history rows miss it too: a busy
session writes many rows an hour, so its newest N reach back only as far as its
own churn. A time window is the same span for every session. So the session
hint and `sessions_overview` read **six hours** of history
(`focus.HistoryWindow`), and `focus_search`'s `since_hours` is a window too.
Every one of these reads filters on liveness in SQL before its limit, as the
current-focus search does.

### `fleet_report`: every session in a window, ended ones included

`fleet_report` answers a different question — what was worked on — so it has
**no liveness filter**: a session that ended last night is exactly what a
standup needs. What it shares with the reads above is the rest: it runs under
the caller's principal on tables under forced row-level security, so another
owner's session never appears, and it reads a time window, never the newest N
rows. A session counts as active in the window when either of two rows falls
inside it:

- a focus **history** row (its focus changed), or
- its **current** focus row's last refresh: muster refreshes a focus only when
  the session's transcript moved, so a session that worked all day on one
  unchanged focus is not missed because its last history row is older.

A session with neither in the window — idle, or not summarised because session
focus could not read it — is not listed. The report carries focus **titles**
only: no description, transcript text, command or tool output.
