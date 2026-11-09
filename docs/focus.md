# Session focus

Every session — a dispatched agent or a mirrored Claude Code window — can carry
a **focus**: a short title (stored whole) and one to three sentences saying what
it is working on now. The search palette (the magnifier in the sidebar or on the
bottom bar, or `/`) finds a session by its focus title and shows it beside the
session's name, but ranks matches by the **name** first, so typing a name still
puts that session first. A session's page lists its focus over time behind the
header's focus-history button ([below](#focus-history)). (The Agents list page,
whose cards were headed by their focus, is retired.)

## How old a focus is

A focus title says what the session was doing **when it was written**. An idle
session keeps its last title for days, so wherever a focus title is shown — the
second line of a row in the switcher dock, the second line of a search palette
result, and the bold focus line of a row in the Asks list — its age follows it
once it is **older than a day**, in whole days: `Bisecting the flaky tunnel
test · 2d`. A title **older than four days** is also marked stale and drawn
muted (italic, faded). A newer title shows no age. The age is labelled for a
pointer and a screen reader ("focus title set 2d ago"), since an Asks card
also shows how long it has waited in the same shape.

A chief **notice** on the Asks list shows the session's title as the chief saw
it when it decided to nudge. It gets that title's age while the session's
stored focus still has that title; once the session has a newer title, the
notice's older one shows no age. The same holds for the notice's **push**: its
body leads with the title and, past a day, its age (`Bisecting the flaky tunnel
test · 2d`), worked out when the push is sent. A title set within the day
carries none, as everywhere. The age is never cut off: a title long enough to
crowd it out is cut instead (with `…`), so the title and its age take at most
half of the 200-character body and the chief's message keeps the rest.

The chief's `fleet_list`, `sessions_overview` and `focus_search` (its current
focus search) rows, and its `attention_list` items, carry the same age beside
each focus title: `focus_set_at` (when it was written, RFC 3339, like every
other time in those answers), and `focus_age` (`"2d"`) past a day, from the same
rule. They carry no stale flag: the age says it, and in those tools "stale"
already means a failed refresh (`focus_problem`). `focus_set_at` is
there so the chief can tell a title set an hour ago from one set yesterday,
which the whole-day age does not show. A session with no title carries neither.
A [chief nudge](chief-nudges.md)'s stall check is given the title's age in words
on the line after it (`focus title set 2d ago`), and the
[attention summary](attention.md)'s `why` for a notice is its push's first line,
age included.

The age is the time the focus was last written. It is worked out by the server
when the page (or the switcher, the list, the push or the tool's answer) is
made, from one rule (`internal/ui`'s `FocusAgeOf`) on one clock, the server's; the dock and the palette draw what the server
decided and hold no rule of their own, so an age is as old as the page showing
it. A refresh that fails keeps the old title and its time, so a session whose
refresh keeps failing ages like an idle one. In the dock and the palette the
title is cut to fit one line, and the age after it never is.

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

A mirrored session is keyed by the conversation, not the window, so its focus
survives tmux reissuing window ids and follows a conversation into another
window. `muster-mirror` reports the id per window (`claudeSession` in its
window list). An older `muster-mirror` does not, and its windows simply have no
focus. Only Claude Code windows, and opencode windows whose host names their
session, have one.

The transcript **path** is not sent to the server. The window list is one
WebSocket message, and its largest case already fills 96% of a frame; muster
reads a transcript through the host's own transcript stream instead (below).

## A session's name

A session is headed by a **name** that does not move, with its focus as the
line under it. The focus changes with the work; the name is how you find the
same session tomorrow. One rule decides it (`internal/sessionname`), and the
first of these that a session has wins:

| rank | name | where it comes from |
|---|---|---|
| 1 | **manual** | the name you gave it: **Rename…** in the session's ⋯ menu (the bar on every session page), or an agent's own ⋯ menu. Nothing automatic ever replaces it. An empty name hands the session back to the rows below. |
| 2 | **system** | a name muster pinned for a role: the chief, an officer's instance. The rest of muster finds the role by it, so the automatic name never replaces it. |
| 3 | **dispatch** | an agent dispatched from a task: `#42 · Fix the login bug`. It is the agent's stable name: it never changes on its own, and only a manual rename replaces it. |
| 4 | **auto** | the session's **first good focus title**, frozen ([below](#the-name-bar)). Set once and never changed by a later focus. (A task-dispatched agent's auto name is frozen too, but never shown: its task name ranks above it.) |
| 5 | fallback | the session's technical id: an agent's slug, a mirrored window's host label (`mango:1`) or tmux address. |

A mirrored window's **host label** (`muster-mirror -labels-file`, drawn as
`<codename>:<window index>`) ranks as a fallback, not a name: it is the
operator's codename for a tmux session, shared by every conversation that
window ever holds. Once the conversation has an auto or manual name, the label
moves to the row's tooltip and the search palette (it is still what the
palette matches first). An agent's display name ranks by who set it
(`agents.display_name_source`, migration `0090`): `manual`, `system` and
`dispatch` above the auto name, `default` (the slug, copied) as the fallback. Existing rows were classified once by the migration: a chief
(slug `chief` or `chief-<owner id>`) is `system`, the slug copied is `default`, `#<id>` or `#<id> · …` is `dispatch`, `officer <id> #<n>`
is `system`, and any other display name is `manual`, since a person set it.

The name is **stored**, not derived: focus history is pruned after 30 days, so
"the oldest title" would move. The auto and manual names are columns of the
session's focus row (`agent_focus.name_auto`, `name_manual`, migration
`0090`), which lives as long as the session: an agent's dies with the agent; a
mirrored one is pruned with its focus a retention after it was last seen, but a
row carrying a name (auto or manual) is kept 90 days longer. A mirrored session's
manual name follows its **conversation** (the key its focus has), so it moves
with the conversation to another window, and a window that later holds a
different conversation is not called by it. A window with no conversation
muster can see (a shell, an older `muster-mirror`) cannot be renamed. An
agent's manual name stays its display name, so the chief is still found by its
display name.

### Where the name is used

Every surface that names a session asks the same rule (`internal/api/names.go`
holds its only callers; `TestTheSessionNameCallerLedger` lists every surface):

- **the owner's views**: the sidebar row (the name, then the live focus and
  its age), the switcher's pills and dock, the search palette, the agent page,
  the Rendered view and the terminal page (header and `<title>`), the
  notifications panel, a dispatch card in the chief's chat, and the Asks list;
- **pushes and attention**: an ask's title (`<name> is asking`), a chief
  notice's, a stalled or waiting session's, the Asks row, `attention_list`'s
  `name` and the attention summary's `title`;
- **the chief's tools**: `fleet_list` and `sessions_overview` rows (`name`),
  `focus_search` hits (`name`), `fleet_report`, the route candidates and
  `route_to_session`'s text, `task_forward`'s label, the connector requests it
  decides, a mirrored session's ask label, and the [stall check](chief-nudges.md)'s
  prompt, which names the session (`session: <name>`) above its focus title.
  (An agent's ask is stored with the display name it had when it was filed —
  a record, not a live name — and is named by the rule when the item is
  shown.)

A named session's **technical ids** — an agent's slug, a window's codename,
tmux address and host — are off those main surfaces: they are the row's,
pill's and header's tooltip, the Asks row's tooltip, the Rendered view's full
title, and still what the palette finds the session by (its codename first).
The chief keeps them where it needs an address: every row's `id`
(`agent:<id>`, `mirror:<session key>`), a mirrored row's `where`, and
`fleet_list`'s `code` (an agent's slug). A waiting session's push
**body** still says where it waits. A session with no human name yet is
called by its technical id everywhere, as before.

### Show tmux sessions

A per-user switch, **Account → Notifications → Show tmux sessions** ("Adds
each session's tmux codename (like mango:1 @ atlas) to notifications and the
session lists"), off for every user until they turn it on (stored in
`notify_preferences`, migration 0094; no row is off). On, a session called by
its human name is also shown by the window as muster named it before sessions
had names — the host's label for it and the host, or, when the host sent no
label, its tmux session and window index and the host:

    Billing migration · mango:1 @ atlas
    Billing migration · scratch3:5 @ atlas

**In notifications** that place follows the name: wherever the attention
queue names a session by its human name — the push title of an agent
question from a mirrored session, of a held question or of a chief notice,
the Asks list row, the attention summary's `title` and `attention_list`'s
`name` — and a stalled or waiting session's push title ("… needs you", "… is
stalled") and its "Stalled / waiting" row. (A waiting window's own push keeps
its fixed title and already says where it waits in its body.)

**In the session lists:** a pill on the switcher bar gets a second, smaller
line under its name, the codename alone (`mango:1`: the pill's host icon
already names the host, and a host name would make the pill wider than its
name; the line is capped and cut with "…"). It sits inside the pill's 32px,
so the bar is as tall as with the switch off. The dock's row gets a muted
line, `mango:1 @ atlas`, under its focus title (the second line when it has
none). The sidebar's row gets the same muted line under its name and focus
title, and so does the command palette's result for it (the search picker,
opened from the bar's magnifier, the sidebar's, or `/`); the palette's
result then leaves out the bare codename it would otherwise draw under a
name with no focus title, since the line starts with it. What the palette
finds an entry by does not change. Each is text in the pill or row, so it is
part of its accessible name. Live refreshes keep it: the bar's and the
sidebar's rows are drawn by the server on every render (the sidebar's on
every SSE or minute's refetch of its partial), and the palette draws its
results from those rows' `data-switcher-tmux` each time it is opened or the
sidebar's rows land.

The window is the one that holds the conversation when the text is made (for
a stop whose conversation no window holds now, the place stored on the stop,
by its codename when its host still gives one), shown verbatim. It is decided
in one place (`tmuxPlace`, `internal/api/names.go`: notifyName adds it to a
notification, `mirrorCards` puts it on the lists' entries);
`TestEveryNotificationSurfaceAsksForTheTmuxAddress` classifies every surface
on the name ledger as a notification, a list, or one it does not apply to,
and `TestOnlyMirrorCardsSetsTheListsCodename` keeps any other code from
setting the lists' value. In the ui, `TestOnlyTheEntryHelpersDrawTheCodename`
pins what draws it: the one attribute helper (`entryTmuxAttr`, on a pill and
a sidebar row), the pill's and the sidebar row's line, and the two scripts
(the dock's and the palette's) that read the attribute.

Nothing else changes: a session with no human name is already called by its
place (nothing twice), and so is one whose human name IS its codename (a
manual name `mango:1` on the window labelled `mango:1`: no second line, no
`mango:1 · mango:1 @ atlas`); an agent has no tmux window and gets nothing; a
session no window shows now gets no address (muster names no place it cannot
see). Off — or with no database, or when the switch cannot be read — every
text and every pill is byte for byte what it was before the switch existed.

### The name bar

The auto name freezes from the first focus that passes this bar, in the same
database write that stores that focus (`focus.Nameable`, checked in code, not
asked of the model):

- the conversation has had a **real turn**: a prompt from the person that is
  not an opener, answered by a reply. An opener is a prompt made only of
  greeting, "carry on" and acknowledgement words (`hi`, `continue`, `ok go
  on`): a closed vocabulary in `internal/focus/names.go`, matched word for
  word;
- the title is one the summarizer may keep (`FeedableTitle`: within 60
  characters, plain, no markdown);
- it has at least **two words**;
- it is not itself an opener (`Greeting the user`, `Continue previous
  session`, `Awaiting user instructions`).

A title with one word outside that vocabulary passes (`Greeting the billing
team`). Until a focus passes, the session keeps the name below it in the
table.

### When the work moves on

A name is frozen, but the work can move on. When it has, the session's page
(an agent's page, a window's Rendered view) **offers** a rename under its
header — "This session has moved on to "Fixing the Stripe webhook retry"",
then a **Rename to "…"** button, then **Keep the name** as a text link, one
per row (a long offer wraps inside the banner). muster never renames a session
on its own. The offer is decided by `focus.DriftOffer`, from the session's
focus history:

- the **newest three** focus changes (`DriftMinTitles`) written **after the
  name took effect** (a manual mirrored name's `name_manual_at`, an auto
  name's `name_auto_at`, an agent's `display_name_at`, migration `0091`) have
  all **diverged** from the name: the words they share are fewer than half of
  the name's content words AND fewer than half of the title's
  (`DriftMaxOverlap`; filler and opener words do not count, a task name's
  `#<id> · ` lead does not either, and words are compared by their first four
  letters, so "migrating" meets "migration"). Measuring both sides keeps a
  long task name from "diverging" from a short title on the same job. One
  off-topic detour is two changes at most; three in a row is a new job. A
  session renamed a moment ago is not offered a rename on the history it was
  renamed over;
- the **oldest of those three** was written at least **30 minutes** ago
  (`DriftMinAge`, two default refresh intervals): the divergence is sustained,
  not one busy minute;
- the current title passes the [name bar](#the-name-bar)'s title half and is
  not the name itself.

A manual, task (dispatch) or auto name is offered a rename — a task's name
never changes on its own, so this offer is how it moves on — but never a
system name (the chief, an officer) or a technical fallback. **Rename to "…"**
is an ordinary rename, so the new name is manual. **Keep the name** records
the dismissal per user (`agent_focus.drift_dismissed`, migration `0091`): the
offer stays hidden while the focus title is the one it was dismissed at, and
comes back when the title changes.

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
- otherwise ask the summarizer (below) for a title and a description —
  handing it the session's **current title to keep** — check the answer
  ([what is refused](#what-the-summarizer-must-answer)), clean it (one line,
  no control characters, three sentences; a title cut to **60 characters** at
  a word boundary), and store it;
- on any failure, keep the previous focus and record **why** beside it, as a
  short fixed reason: `no OpenRouter key`, `the OpenRouter key could not be
  read` (a credential-store failure, not a missing key), `rate-limited`,
  `summarizer error (HTTP <status>)`, `the summarizer's answer was
  unusable`, `the summarizer ignored the format`, `the summarizer answered in
  markdown`, `the summarizer repeated the session's text`, `the summarizer
  could not be reached`. Nothing from the transcript, the key or the provider's error
  text is stored or shown. `fleet_list` shows it (`focus_problem`); no page
  does (the retired Agents list's card popover did). The next successful
  refresh clears it. A failed refresh leaves the focus's time where it was, so
  its title [ages](#how-old-a-focus-is) like any other.

A pass only looks at sessions that were active since their focus was
written **or since their tail was last read**, at most 20 per user, and skips
disabled users. A session's activity is its transcript's last message (the
time the Agents list shows), or tmux's window activity for a window whose host
reports no last message (an opencode window, an older host) —
**except while a Claude window is busy or waiting on you**: its host then
reports its activity as *now*, so it always looks active. A tail read that
found nothing new (or an empty tail) is remembered, with when it began and the
tail's sha256, so a session whose transcript has not moved since is skipped
without another read. A Claude window that has been **waiting** on you (a
permission prompt, a question) since before that read is skipped too, from its
host's status: Claude Code writes no transcript row a focus tail shows while it
waits. A **busy** window has no such signal, so it is read on every pass.
Skipped sessions do not count toward the 20, so they cannot crowd out
sessions that did move — but 20 or more **busy** windows, which sort first,
still fill the 20 ahead of an idle window whose transcript moved, and that one
waits for a pass with fewer busy windows. The record is kept in memory only:
after a restart, each session costs one extra read on the first pass. A read
that failed is not remembered, so it is retried on the next pass. Calls are
capped at 60 per user and 300 in all per hour, two at a time.

Every pass logs one line of counts, a quiet pass included, so a missing line
means the pass did not run:
`focus: pass: owners=1 candidates=25 skipped=20 deferred=0 updated=3 unchanged=2 empty=0 failed=0 rate-limited=false`.
`skipped` is sessions not read because nothing moved, and `deferred` is
sessions that may have moved but were past the 20 or the rate cap, left for
the next pass. It never carries a title.

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
one stored. Unknown is what a dispatched agent, an opencode window, a host older
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

The model is `MUSTER_FOCUS_MODEL` ([configuration](configuration.md)),
`scaledown/summarize` when unset; any OpenRouter model that keeps to the
format below can replace it.

### What the summarizer must answer

A focus title is a **stable name for the session's task**, not a description
of its latest step. So the prompt asks for a name of 3 to 8 words, at most 60
characters, plain text, in the summarizer's own words — and, when the session
already has a title, it is sent with the transcript (in the instructions, not
as transcript) with the instruction to **keep it word for word unless the
session's overall goal has changed**. A kept title that comes back in another
case or punctuation is stored exactly as before, so the name does not
flicker. (A history row is still written when the description changes:
history keeps every change of title *or* description.)

The answer must be exactly a `Title: …` line and, optionally, a
`Description: …` line. The code enforces what the prompt asks:

- **any other shape is refused**: unlabelled lines, a sentence, labels in
  markdown (`**Title:**`, `- Title:`), an extra line before or after. Nothing
  is read out of an off-format answer: the session keeps its title, and the
  row records `the summarizer ignored the format`;
- **markdown is refused**: a code fence anywhere, or a title that is a
  heading, `**bold**` or a markdown link. The session keeps its title, and the
  row records `the summarizer answered in markdown`. Backticks around a name
  and a leading list marker are removed rather than refused, and `__init__` or
  `C#` are ordinary title text;
- **a reply echoed back is refused**: a title that is mostly the session's
  own replies word for word (at least 6 words, 60% of whose 4-word runs appear
  in the tail's assistant lines). The session keeps its title, and the row
  records `the summarizer repeated the session's text`. A title kept as asked
  is never judged a copy, and neither are your own words: a title that names
  the job as you asked for it is what the prompt wants. A **description** that
  is mostly a reply is dropped, and the title kept. This is a quality guard
  against echoed prose — it is **not** a privacy guard: a short title that
  carries a host name or a token from the transcript passes it;
- **a refused tail is not sent again until it changes**: after an answer is
  refused for its content (format, markdown or a copy), that tail's
  fingerprint is remembered (in memory, so a restart costs one more call), and
  the next pass makes no call for it. A failure to reach the summarizer, or an
  HTTP error, is retried on the next pass;
- **a title longer than 60 characters is cut** at the last whole word, with
  no ellipsis.

Titles stored before these rules (up to the 300-character storage guard of
migration 0023) stay until the next refresh replaces them, and are not sent
back as a title to keep.

🔴 **What leaves for each kind of session** (to OpenRouter and ScaleDown):

| entry | dispatched agent | mirrored Claude Code session |
|---|---|---|
| your prompts | as written | as written |
| replies | as written | as written (thinking never) |
| tool calls | the tool's **name**, plus the file path when the call names one (`file_path`, `notebook_path`, `path`) | the same |
| tool arguments (shell commands, file bodies, URLs, patterns, anything else) | never | never |
| tool output | never | never |
| errors | — | the error text |
| the session's current focus title | when it is one the rules above could produce | the same |

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
| `fleet_list` | chief | every session: id, kind, name (its [stable name](#a-sessions-name)), `code` (an agent's slug), status, a mirrored window's `where` (its exact tmux target, [attention.md](attention.md#how-a-window-is-named)), focus title with how old it is (`focus_set_at`; `focus_age` past a day, [how old a focus is](#how-old-a-focus-is)), and `focus_problem` (the reason) when its focus is missing or stale |
| `focus_search(query, limit, since_hours?)` | chief | full-text search over titles (weighted higher) and descriptions; id, kind, name (the session's stable name, current-focus mode), title (with `focus_set_at`, and `focus_age` past a day) and a 120-character snippet per hit. With `since_hours` (1–24) it searches every focus each session had in that window instead of its current one, and each hit carries when that focus was written |
| `sessions_overview(limit?)` | chief | every live session (running agents, online mirrored windows) active in the last 24 h, newest-active first: id, kind, name, host, a mirrored window's `where`, status, current focus with the same `focus_set_at` and `focus_age` as `fleet_list`, the distinct focus titles of the last 6 h with their times, its pending attention item, and its last three user and assistant messages (300 characters each, secrets redacted, no tool output). A session whose transcript does not arrive within 5 s reads `"transcript": "unavailable (timed out)"`; all the reads share a 12 s budget, and a session it cuts or never reaches reads `"unavailable (overview budget)"`. The answer is capped at 32 KiB and says how many sessions it left out |
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
